package ui

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/localreg"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// LocalRegistryState describes the runtime status and storage metrics of GVX
// Desktop's built-in local OCI image registry.
type LocalRegistryState struct {
	Mode             string // store.LocalRegistryAuto / On / Off
	Running          bool   // built-in server is actively listening
	LegacyActive     bool   // a legacy registry:2 sandbox is active (built-in yields port 5001)
	LegacyPaused     bool   // a legacy registry:2 sandbox is paused
	AutoDemand       bool   // Docker (sandbox engine or docker:dind) or Kubernetes (k3s) is active
	Addr             string // e.g. "127.0.0.1:5001" while running
	Err              string // last start error if binding failed
	TotalBytes       int64
	ReclaimableBytes int64
	ImageCount       int
	BlobCount        int
}

// Equal makes LocalRegistryState comparable for ggui signals.
func (s LocalRegistryState) Equal(o LocalRegistryState) bool {
	return s == o
}

func localRegistryImagesEqual(a, b Listing[localreg.ImageEntry]) bool {
	return a.Err == b.Err && a.Loading == b.Loading && a.Loaded == b.Loaded &&
		slices.EqualFunc(a.Items, b.Items, localreg.ImageEntry.Equal)
}

// filterLocalRegistryImages keeps entries whose repository, tag, digest, or
// platform matches q.
func filterLocalRegistryImages(items []localreg.ImageEntry, q string) []localreg.ImageEntry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return items
	}
	var out []localreg.ImageEntry
	for _, e := range items {
		hay := strings.ToLower(e.Repository + ":" + e.Tag + " " + e.ShortDigest() + " " + e.Digest + " " + strings.Join(e.Platforms, " "))
		if strings.Contains(hay, q) {
			out = append(out, e)
		}
	}
	return out
}

// ensureLocalRegServer lazily constructs the stopped localreg.Server for
// <dataDir>/registry. Returns nil when dataDir is empty.
func (m *Model) ensureLocalRegServer() *localreg.Server {
	m.localRegMu.Lock()
	defer m.localRegMu.Unlock()
	if m.localReg != nil {
		return m.localReg
	}
	if m.dataDir == "" {
		return nil
	}
	port := m.localRegPort
	if port == 0 {
		port = recipes.RegistryPort
	}
	m.localReg = localreg.New(localreg.Config{
		Dir:  filepath.Join(m.dataDir, "registry"),
		Port: port,
		OnChange: func() {
			if m.localRegChangePending.CompareAndSwap(false, true) {
				time.AfterFunc(120*time.Millisecond, func() {
					m.localRegChangePending.Store(false)
					m.post(m.LoadLocalRegistry)
				})
			}
		},
	})
	return m.localReg
}

// ensureLocalRegistryStarted starts the built-in local registry synchronously
// if it is not already running, safe to call from background work such as
// PushToRegistry.
func (m *Model) ensureLocalRegistryStarted() error {
	m.localRegOpMu.Lock()
	defer m.localRegOpMu.Unlock()
	m.localRegMu.Lock()
	closed := m.localRegClosed
	m.localRegMu.Unlock()
	if closed {
		return errors.New("local registry is shutting down")
	}
	srv := m.ensureLocalRegServer()
	if srv == nil {
		return errors.New("local registry storage directory is not configured")
	}
	if !srv.Running() {
		if err := srv.Start(); err != nil {
			m.post(func() {
				m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
					st.Running = false
					st.Addr = ""
					st.Err = err.Error()
					return st
				})
			})
			return err
		}
	}
	addr := srv.Addr()
	m.post(func() {
		m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
			st.Running = true
			st.Addr = addr
			st.Err = ""
			return st
		})
	})
	return nil
}

// followLocalRegistry watches settings, sandbox engine status, and running
// sandboxes to start or stop the built-in local OCI registry automatically
// (yielding port 5001 whenever a legacy registry:2 sandbox is active).
// Call it from Start.
func (m *Model) followLocalRegistry() {
	ggui.Effect(func() ggui.Cleanup {
		set := m.Settings.Get()
		seStatus := m.SandboxEngineStatus.Get()
		rows := m.Rows.Get()
		m.syncLocalRegistryWith(set, seStatus, rows, false)
		return nil
	})
	ggui.Watch(m.ImageView, func(view int) {
		if view == ImagesRegistry {
			m.LoadLocalRegistry()
		}
	})
}

func (m *Model) syncLocalRegistry() {
	m.syncLocalRegistryWith(
		ggui.Peek(m.Settings),
		ggui.Peek(m.SandboxEngineStatus),
		ggui.Peek(m.Rows),
		false,
	)
}

func (m *Model) syncLocalRegistryWith(set store.Settings, seStatus SandboxEngineStatus, rows []Row, forceRetry bool) {
	mode := set.LocalRegistryMode()
	legacyRow, hasLegacy := m.RecipeSandbox(rows, recipes.Registry)
	legacyActive := hasLegacy && legacyRow.State.Status.Active()
	legacyPaused := legacyActive && legacyRow.State.Paused

	dockerRow, hasDocker := m.RecipeSandbox(rows, recipes.Docker)
	dockerSandboxActive := hasDocker && dockerRow.State.Status.Active()
	kubeRow, hasKube := m.RecipeSandbox(rows, recipes.Kubernetes)
	kubeSandboxActive := hasKube && kubeRow.State.Status.Active()

	autoDemand := (set.SandboxEngine && (seStatus.Ready || seStatus.Opening)) || dockerSandboxActive || kubeSandboxActive
	wantRunning := !legacyActive && (mode == store.LocalRegistryOn || (mode == store.LocalRegistryAuto && autoDemand))

	wasWant := m.localRegWant
	m.localRegWant = wantRunning

	m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
		st.Mode = mode
		st.LegacyActive = legacyActive
		st.LegacyPaused = legacyPaused
		st.AutoDemand = autoDemand
		if !wantRunning || forceRetry || (!wasWant && wantRunning) {
			st.Err = ""
		}
		return st
	})

	if m.Offline || m.quitting {
		return
	}

	srv := m.ensureLocalRegServer()
	if srv == nil {
		return
	}

	running := srv.Running()
	if wantRunning {
		if running && m.localRegInFlight != "stop" {
			addr := srv.Addr()
			m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
				st.Running = true
				st.Addr = addr
				st.Err = ""
				return st
			})
			return
		}
		if m.localRegInFlight == "start" && !forceRetry {
			return
		}
		if !forceRetry && wasWant && ggui.Peek(m.LocalRegistry).Err != "" {
			return
		}
		m.reconcileLocalRegistry(srv, true, "start")
		return
	}

	if !running && m.localRegInFlight != "start" {
		m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
			st.Running = false
			st.Addr = ""
			st.Err = ""
			return st
		})
		return
	}
	if m.localRegInFlight == "stop" {
		return
	}
	m.reconcileLocalRegistry(srv, false, "stop")
}

func (m *Model) reconcileLocalRegistry(srv *localreg.Server, want bool, op string) {
	m.localRegMu.Lock()
	m.localRegDesired = want
	m.localRegMu.Unlock()

	m.localRegSyncGen++
	gen := m.localRegSyncGen
	m.localRegInFlight = op

	var runningAfter bool
	var addrAfter string
	m.async(func(context.Context) error {
		m.localRegOpMu.Lock()
		defer m.localRegOpMu.Unlock()
		m.localRegMu.Lock()
		desired := m.localRegDesired && !m.localRegClosed
		m.localRegMu.Unlock()
		if desired {
			err := srv.Start()
			runningAfter = srv.Running()
			addrAfter = srv.Addr()
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := srv.Stop(ctx)
		runningAfter = srv.Running()
		addrAfter = srv.Addr()
		return err
	}, func(err error) {
		if m.localRegSyncGen != gen {
			return
		}
		m.localRegInFlight = ""
		if m.quitting {
			return
		}
		m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
			st.Running = runningAfter
			st.Addr = addrAfter
			if err != nil {
				st.Err = err.Error()
			} else {
				st.Err = ""
			}
			return st
		})
		m.LoadLocalRegistry()
	})
}

// SetLocalRegistryMode updates Settings.LocalRegistry (auto, on, or off) and
// synchronizes the built-in local registry server.
func (m *Model) SetLocalRegistryMode(mode string) {
	switch mode {
	case store.LocalRegistryAuto, store.LocalRegistryOn, store.LocalRegistryOff:
	default:
		mode = store.LocalRegistryAuto
	}
	if ggui.Peek(m.Settings).LocalRegistryMode() != mode {
		m.UpdateSettings(func(s *store.Settings) {
			s.LocalRegistry = mode
		})
	}
	m.syncLocalRegistry()
}

// RetryLocalRegistry clears the last start error and retries starting the
// built-in local registry.
func (m *Model) RetryLocalRegistry() {
	m.syncLocalRegistryWith(
		ggui.Peek(m.Settings),
		ggui.Peek(m.SandboxEngineStatus),
		ggui.Peek(m.Rows),
		true,
	)
}

// LoadLocalRegistry reads the images and disk usage stored in the built-in
// local registry directory. Works whether the HTTP server is running or stopped.
func (m *Model) LoadLocalRegistry() {
	if m.Offline {
		return
	}
	srv := m.ensureLocalRegServer()
	if srv == nil {
		return
	}
	if m.localRegLoading {
		m.localRegAgain = true
		return
	}
	m.localRegLoading = true
	m.LocalRegistryImages.Update(func(l Listing[localreg.ImageEntry]) Listing[localreg.ImageEntry] {
		l.Loading = true
		return l
	})
	var items []localreg.ImageEntry
	var stats localreg.DiskStats
	m.async(func(context.Context) error {
		var err error
		items, err = srv.List()
		if err != nil {
			return err
		}
		stats, err = srv.DiskUsage()
		return err
	}, func(err error) {
		m.localRegLoading = false
		if m.localRegAgain {
			m.localRegAgain = false
			m.LoadLocalRegistry()
			return
		}
		l := Listing[localreg.ImageEntry]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		} else {
			m.LocalRegistry.Update(func(st LocalRegistryState) LocalRegistryState {
				st.TotalBytes = stats.TotalBytes
				st.ReclaimableBytes = stats.ReclaimableBytes
				st.ImageCount = stats.ImageCount
				st.BlobCount = stats.BlobCount
				return st
			})
		}
		m.LocalRegistryImages.Set(l)
	})
}

// AskDeleteLocalRegistryImage confirms and removes a tagged image from the
// built-in local registry, reclaiming any blobs that become unreferenced.
func (m *Model) AskDeleteLocalRegistryImage(img localreg.ImageEntry) {
	key := img.Key()
	busyKey := "lreg:" + key
	if m.isBusy(busyKey) {
		return
	}
	m.Confirm(
		tr("Delete %s?", key),
		tr("The tag is removed from the local registry (%s) and any unused layers are reclaimed.", formatBytes(img.Size)),
		tr("Delete"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			srv := m.ensureLocalRegServer()
			if srv == nil {
				return
			}
			m.setBusy(busyKey, true)
			var res localreg.GCResult
			m.async(func(context.Context) error {
				var err error
				res, err = srv.DeleteTag(img.Repository, img.Tag)
				return err
			}, func(err error) {
				m.setBusy(busyKey, false)
				if err != nil {
					m.toastError(tr("Could not delete %s", key), err)
				} else if res.ReclaimedBytes > 0 {
					m.toastOK(tr("Deleted %s", key), tr("Reclaimed %s.", formatBytes(res.ReclaimedBytes)))
				} else {
					m.toastOK(tr("Deleted %s", key), "")
				}
				m.LoadLocalRegistry()
			})
		},
	)
}

// AskPruneLocalRegistry confirms and runs garbage collection on the built-in
// local registry to remove unreferenced manifests and blobs.
func (m *Model) AskPruneLocalRegistry() {
	const busyKey = "lreg:prune"
	if m.isBusy(busyKey) {
		return
	}
	reclaimable := ggui.Peek(m.LocalRegistry).ReclaimableBytes
	desc := tr("Unreferenced manifests and blobs in the local registry are deleted.")
	if reclaimable > 0 {
		desc = tr("Unreferenced manifests and blobs in the local registry are deleted (%s reclaimable).", formatBytes(reclaimable))
	}
	m.Confirm(
		tr("Prune local registry?"),
		desc,
		tr("Prune"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			srv := m.ensureLocalRegServer()
			if srv == nil {
				return
			}
			m.setBusy(busyKey, true)
			var res localreg.GCResult
			m.async(func(context.Context) error {
				var err error
				res, err = srv.GC()
				return err
			}, func(err error) {
				m.setBusy(busyKey, false)
				if err != nil {
					m.toastError(tr("Prune failed"), err)
				} else {
					m.toastOK(tr("Local registry pruned"), tr("Reclaimed %s.", formatBytes(res.ReclaimedBytes)))
				}
				m.LoadLocalRegistry()
			})
		},
	)
}

// CloseLocalRegistry stops the built-in local registry cleanly on shutdown.
func (m *Model) CloseLocalRegistry() {
	m.localRegMu.Lock()
	m.localRegClosed = true
	m.localRegDesired = false
	m.localRegMu.Unlock()

	m.localRegOpMu.Lock()
	defer m.localRegOpMu.Unlock()
	m.localRegMu.Lock()
	srv := m.localReg
	m.localRegMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
}

// OpenRegistry opens the Images page on the Local Registry tab.
func (m *Model) OpenRegistry() {
	m.GoImages(ImagesRegistry)
	m.LoadLocalRegistry()
}
