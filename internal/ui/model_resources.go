package ui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ---- Images -------------------------------------------------------------

// LoadImages reads the image cache.
func (m *Model) LoadImages() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	m.Images.Update(func(l Listing[rungv.Image]) Listing[rungv.Image] { l.Loading = true; return l })
	var items []rungv.Image
	m.async(func(ctx context.Context) error {
		var err error
		items, err = c.Images(ctx)
		return err
	}, func(err error) {
		l := Listing[rungv.Image]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.Images.Set(l)
	})
}

// OpenPull shows the pull dialog targeting gvx's image cache, with ref filled in.
func (m *Model) OpenPull(ref string) {
	if ggui.Peek(m.Pulling) {
		m.PullOpen.Set(true)
		return
	}
	m.PullTarget.Set(PullTargetGVX)
	m.PullRef.Set(ref)
	m.PullLog.Set("")
	m.PullOpen.Set(true)
}

// Pull downloads the image named in the pull dialog into gvx's cache or the
// Docker Engine, according to PullTarget.
func (m *Model) Pull() {
	ref := strings.TrimSpace(ggui.Peek(m.PullRef))
	if ref == "" || ggui.Peek(m.Pulling) {
		return
	}
	target := ggui.Peek(m.PullTarget)
	m.Pulling.Set(true)
	if target == PullTargetDocker {
		m.PullLog.Set("$ docker pull " + ref + "\n")
	} else {
		m.PullLog.Set("$ rungvx pull " + ref + "\n")
	}
	var mu sync.Mutex
	var pending []string
	flush := func() {
		mu.Lock()
		lines := pending
		pending = nil
		mu.Unlock()
		if len(lines) > 0 {
			m.PullLog.Update(func(s string) string {
				s += strings.Join(lines, "\n") + "\n"
				if len(s) > 32*1024 {
					s = s[len(s)-32*1024:]
				}
				return s
			})
		}
	}
	onLine := func(line string) {
		mu.Lock()
		pending = append(pending, line)
		mu.Unlock()
		m.post(flush)
	}
	if target == PullTargetDocker {
		host := m.dockerBackendHost()
		m.async(func(ctx context.Context) error {
			c, err := dockerapi.New(host)
			if err != nil {
				return err
			}
			defer c.Close()
			return c.PullImage(ctx, ref, onLine)
		}, func(err error) {
			flush()
			m.Pulling.Set(false)
			if err != nil {
				m.PullLog.Update(func(s string) string { return s + "error: " + err.Error() + "\n" })
				m.toastError(tr("Pull failed"), err)
				return
			}
			m.PullOpen.Set(false)
			if ggui.Peek(m.Settings).SandboxEngine {
				m.toastOK(tr("Pulled %s", ref), tr("The image layers are compiled to EROFS and cached."))
			} else {
				m.toastOK(tr("Pulled %s", ref), tr("The image is ready in the Docker Engine."))
			}
			m.LoadDockerImages()
			if ggui.Peek(m.Settings).SandboxEngine {
				m.LoadImages()
				m.LoadDisk()
			}
		})
		return
	}
	c := m.Client()
	m.async(func(ctx context.Context) error {
		return c.Pull(ctx, ref, onLine)
	}, func(err error) {
		flush()
		m.Pulling.Set(false)
		if err != nil {
			m.PullLog.Update(func(s string) string { return s + "error: " + err.Error() + "\n" })
			m.toastError(tr("Pull failed"), err)
			return
		}
		m.PullOpen.Set(false)
		m.toastOK(tr("Pulled %s", ref), tr("The image layers are compiled to EROFS and cached."))
		m.LoadImages()
		m.LoadDisk()
		if ggui.Peek(m.Settings).SandboxEngine {
			m.LoadDockerImages()
		}
	})
}

// InspectImage opens the image explorer on the Inspect tab for a gvx image.
func (m *Model) InspectImage(ref string) {
	for _, img := range ggui.Peek(m.Images).Items {
		if img.Ref() == ref {
			m.ExploreGVXImage(img, ExploreInspect)
			return
		}
	}
	m.ExploreGVXImage(rungv.Image{ID: ref, Repository: ref}, ExploreInspect)
}

// AskRemoveImage confirms and removes an image.
func (m *Model) AskRemoveImage(img rungv.Image) {
	m.AskRemoveImageWithUsers(img, nil)
}

// AskRemoveImageWithUsers confirms and removes a cached gvx image in a single
// dialog, warning when any sandboxes currently reference it.
func (m *Model) AskRemoveImageWithUsers(img rungv.Image, users []string) {
	ref := img.Ref()
	busyKey := "img:" + img.Key()
	if m.isBusy(busyKey) {
		return
	}
	text := tr("The image record is removed and layers no other image uses are deleted (%s).", formatSize(img.Size))
	if len(users) > 0 {
		text = tr("Used by %s. They pull it again at their next start.", strings.Join(users, ", ")) + " " + text
	}
	m.Confirm(tr("Remove %s?", ref), text, tr("Remove image"), func() {
		if m.isBusy(busyKey) {
			return
		}
		m.setBusy(busyKey, true)
		c := m.Client()
		m.async(func(ctx context.Context) error { return c.RemoveImage(ctx, ref, true) }, func(err error) {
			m.setBusy(busyKey, false)
			if err != nil {
				m.toastError(tr("Could not remove %s", ref), err)
			} else {
				m.toastOK(tr("Removed %s", ref), "")
			}
			m.LoadImages()
			m.LoadDisk()
			if ggui.Peek(m.Settings).SandboxEngine {
				m.LoadDockerImages()
			}
		})
	})
}

// AskPruneImages removes dangling images.
func (m *Model) AskPruneImages() {
	const busyKey = "img:prune"
	if m.isBusy(busyKey) {
		return
	}
	m.Confirm(tr("Remove unused images?"), tr("Dangling images and orphaned layer files are deleted."), tr("Prune images"), func() {
		if m.isBusy(busyKey) {
			return
		}
		m.setBusy(busyKey, true)
		c := m.Client()
		var out string
		m.async(func(ctx context.Context) error {
			var err error
			out, err = c.PruneImages(ctx, false)
			return err
		}, func(err error) {
			m.setBusy(busyKey, false)
			if err != nil {
				m.toastError(tr("Prune failed"), err)
			} else {
				m.toastOK(tr("Images pruned"), lastLine(out))
			}
			m.LoadImages()
			m.LoadDisk()
			if ggui.Peek(m.Settings).SandboxEngine {
				m.LoadDockerImages()
			}
		})
	})
}

// ---- Volumes ------------------------------------------------------------

// LoadVolumes reads the local volumes and the sandbox engine's volume users.
func (m *Model) LoadVolumes() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	m.Volumes.Update(func(l Listing[rungv.Volume]) Listing[rungv.Volume] { l.Loading = true; return l })
	var items []rungv.Volume
	var engUsers map[string][]string
	m.async(func(ctx context.Context) error {
		var err error
		items, err = c.Volumes(ctx)
		_, engUsers = m.sandboxEngineVolumeOwnership()
		return err
	}, func(err error) {
		l := Listing[rungv.Volume]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.EngineVolumeUsers.Set(engUsers)
		m.Volumes.Set(l)
	})
}

func stringSliceMapEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || !slices.Equal(va, vb) {
			return false
		}
	}
	return true
}

// VolumeUsers names the sandboxes that mount a volume.
func VolumeUsers(rows []Row, name string) []string {
	var out []string
	for _, r := range rows {
		if slices.Contains(r.Spec.Volumes(), name) {
			out = append(out, r.Spec.Name)
		}
	}
	return out
}

// volumeUsersWithEngine names the sandboxes and experimental sandbox-engine
// containers that mount a volume.
func volumeUsersWithEngine(rows []Row, engineUsers map[string][]string, name string) []string {
	out := VolumeUsers(rows, name)
	for _, ctr := range engineUsers[name] {
		if !slices.Contains(out, ctr) {
			out = append(out, ctr)
		}
	}
	return out
}

// prunableVolumes returns the names of rungv volumes that are neither locked in
// use, referenced by any saved user sandbox, nor owned/referenced by the
// experimental sandbox engine.
func prunableVolumes(volumes []rungv.Volume, rows []Row, engineOwned map[string]bool, engineUsers map[string][]string) []string {
	var out []string
	for _, v := range volumes {
		if v.InUse {
			continue
		}
		if len(VolumeUsers(rows, v.Name)) > 0 {
			continue
		}
		if engineOwned[v.Name] || len(engineUsers[v.Name]) > 0 {
			continue
		}
		out = append(out, v.Name)
	}
	return out
}

// pruneVolumes removes every volume not used by a sandbox or owned by the
// sandbox engine one by one, never calling rungv volume prune.
func (m *Model) pruneVolumes(
	ctx context.Context,
	list func(context.Context) ([]rungv.Volume, error),
	remove func(context.Context, string) error,
	rows []Row,
) (int, error) {
	vols, err := list(ctx)
	if err != nil {
		return 0, err
	}
	engineOwned, engineUsers := m.sandboxEngineVolumeOwnership()
	targets := prunableVolumes(vols, rows, engineOwned, engineUsers)
	deleted := 0
	var errs []error
	for _, name := range targets {
		if err := remove(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

// isValidVolumeName checks whether s is a valid rungv volume name: non-empty,
// starts with an alphanumeric character or '_', and contains only letters,
// digits, '-', '_' and '.'.
func isValidVolumeName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s[0] == '-' || s[0] == '.' {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// OpenVolumeDialog resets the volume form and opens the create-volume dialog.
func (m *Model) OpenVolumeDialog() {
	m.VolumeName.Set("")
	m.VolumeOpen.Set(true)
}

// CreateVolume formats the volume named in the dialog.
func (m *Model) CreateVolume() {
	name := strings.TrimSpace(ggui.Peek(m.VolumeName))
	if !isValidVolumeName(name) || m.isBusy("vol:create") {
		return
	}
	m.setBusy("vol:create", true)
	driver := ggui.Peek(m.VolumeDriver)
	c := m.Client()
	m.async(func(ctx context.Context) error { return c.CreateVolume(ctx, name, driver) }, func(err error) {
		m.setBusy("vol:create", false)
		if err != nil {
			m.toastError(tr("Could not create the volume"), err)
			return
		}
		m.VolumeOpen.Set(false)
		m.VolumeName.Set("")
		m.toastOK(tr("Volume created"), tr("Mount it in a sandbox as %s:/path.", name))
		m.LoadVolumes()
		m.LoadDisk()
	})
}

// AskRemoveVolume confirms and deletes a volume.
func (m *Model) AskRemoveVolume(v rungv.Volume) {
	busyKey := "vol:" + v.Name
	if m.isBusy(busyKey) {
		return
	}
	engOwned, engUsers := m.sandboxEngineVolumeOwnership()
	users := volumeUsersWithEngine(ggui.Peek(m.Rows), engUsers, v.Name)
	text := tr("All data in %s (%s) is deleted. This cannot be undone.", v.Name, formatSize(v.Size))
	if len(users) > 0 {
		text += tr(" Used by: %s.", strings.Join(users, ", "))
	} else if engOwned[v.Name] {
		text += " " + tr("This volume is managed by Docker in the sandbox engine.")
	}
	m.Confirm(tr("Delete volume %s?", v.Name), text, tr("Delete volume"), func() {
		if m.isBusy(busyKey) {
			return
		}
		m.setBusy(busyKey, true)
		c := m.Client()
		m.async(func(ctx context.Context) error { return c.RemoveVolume(ctx, v.Name) }, func(err error) {
			m.setBusy(busyKey, false)
			if err != nil {
				m.toastError(tr("Could not delete %s", v.Name), err)
			} else {
				m.toastOK(tr("Deleted %s", v.Name), "")
			}
			m.LoadVolumes()
			m.LoadDisk()
		})
	})
}

// AskPruneVolumes removes unused rungv volumes individually, keeping volumes
// referenced by saved sandboxes and all engine-managed volumes.
func (m *Model) AskPruneVolumes() {
	const busyKey = "vol:prune"
	if m.isBusy(busyKey) {
		return
	}
	m.Confirm(tr("Delete all unused volumes?"),
		tr("Unused volumes that no sandbox or container uses are deleted, with their data. Engine-managed Docker volumes are kept."),
		tr("Prune volumes"), func() {
			if m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			c := m.Client()
			rows := slices.Clone(ggui.Peek(m.Rows))
			var deleted int
			m.async(func(ctx context.Context) error {
				var err error
				deleted, err = m.pruneVolumes(ctx, c.Volumes, c.RemoveVolume, rows)
				return err
			}, func(err error) {
				m.setBusy(busyKey, false)
				if err != nil {
					m.toastError(tr("Prune failed"), err)
				} else if deleted == 0 {
					m.toastOK(tr("Volumes pruned"), tr("No unused volumes to remove."))
				} else {
					m.toastOK(tr("Volumes pruned"), trn(deleted, "Removed %d unused volume.", "Removed %d unused volumes."))
				}
				m.LoadVolumes()
				m.LoadDisk()
			})
		})
}

// LoadDisk reads the disk usage totals.
func (m *Model) LoadDisk() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	var rows []rungv.DiskUsage
	m.async(func(ctx context.Context) error {
		var err error
		rows, err = c.DiskUsage(ctx)
		return err
	}, func(err error) {
		if err == nil {
			m.Disk.Set(rows)
		}
	})
}

// ---- Docker and Kubernetes ----------------------------------------------

// dockerHost is the user-facing DOCKER_HOST served by the app's proxy.
func (m *Model) dockerHost() string { return m.DockerAPI().Host() }

// dockerBackendHost is the app-private backend socket dockerd publishes inside
// the sandbox, used directly by the app's own dockerapi client. In
// sandbox-engine mode the proxy's front endpoint serves the in-app engine.
func (m *Model) dockerBackendHost() string {
	if m.dockerBackend != "" {
		return m.dockerBackend
	}
	if ggui.Peek(m.Settings).SandboxEngine {
		return m.dockerHost()
	}
	return recipes.DockerBackendHost()
}

// Docker endpoints, as the settings name them.
const (
	DockerSocketEndpoint = "socket"
	DockerTCPEndpoint    = "tcp"
)

func dockerSettingOf(set store.Settings) recipes.DockerAPI {
	api := recipes.DefaultDockerAPI()
	switch set.DockerEndpoint {
	case DockerSocketEndpoint:
		api.TCP = false
	case DockerTCPEndpoint:
		api.TCP = true
	}
	if set.DockerPort > 0 {
		api.Port = set.DockerPort
	}
	return api
}

// DockerAPI is the user-facing Docker endpoint the app's proxy listens on,
// configured by settings.
func (m *Model) DockerAPI() recipes.DockerAPI {
	return dockerSettingOf(ggui.Peek(m.Settings))
}

// DockerHost is the DOCKER_HOST value that reaches the Docker Engine.
func (m *Model) DockerHost() string { return m.dockerHost() }

// SetDockerAPI changes the endpoint the app's Docker API proxy listens on and
// saves the setting. The listener re-binds immediately with no engine restart.
func (m *Model) SetDockerAPI(api recipes.DockerAPI) {
	if api.Port <= 0 || api.Port > 65535 {
		api.Port = recipes.DefaultDockerPort
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.DockerEndpoint = DockerSocketEndpoint
		if api.TCP {
			s.DockerEndpoint = DockerTCPEndpoint
		}
		s.DockerPort = api.Port
	})
	if m.dockerProxyStarted {
		m.bindDockerProxy(api)
	}
}

// ServiceLB reports whether the cluster runs k3s's load balancer: as its
// sandbox's command has it when there is one, which is what counts, and
// else as the settings would make one.
func (m *Model) ServiceLB() bool {
	if row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Kubernetes); ok {
		return recipes.ServiceLBOf(row.Spec)
	}
	return ggui.Peek(m.Settings).K3sServiceLB
}

// SetServiceLB turns the cluster's load balancer on or off, in the settings
// and in the cluster's sandbox when there is one, with the node's ports 80
// and 443 published for it. A running cluster keeps what it started with
// until it is restarted, and is told so.
func (m *Model) SetServiceLB(on bool) {
	m.UpdateSettings(func(s *store.Settings) { s.K3sServiceLB = on })
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, spec := range specs {
		if spec.Recipe != recipes.Kubernetes {
			continue
		}
		if recipes.ServiceLBOf(spec) == on {
			return
		}
		specs[i] = recipes.WithServiceLB(spec, on)
		m.saveSpecs(specs)
		if m.sup.State(spec.ID).Status.Active() {
			m.Toasts.Push(ui.Toast(tr("Restart the cluster to apply the change"),
				tr("It keeps running as it started until then.")).Duration(8 * time.Second))
		}
		return
	}
}

// sharedSetting is the folders the settings ask a new engine to share.
func sharedSetting(set store.Settings) []string {
	if set.DockerSharedFolders == nil {
		return recipes.DefaultSharedFolders()
	}
	return set.DockerSharedFolders
}

// sharedFoldersOf is the folders the Docker Engine shares: as its sandbox
// mounts them when there is one, whose mounts are its own, and else as the
// settings would make one.
func (m *Model) sharedFoldersOf(rows []Row, set store.Settings) []string {
	if row, ok := m.RecipeSandbox(rows, recipes.Docker); ok {
		return recipes.SharedFoldersOf(row.Spec)
	}
	return sharedSetting(set)
}

// SharedFolders are the folders of this computer the Docker Engine sees at
// their own paths.
func (m *Model) SharedFolders() []string {
	return m.sharedFoldersOf(ggui.Peek(m.Rows), ggui.Peek(m.Settings))
}

// SetSharedFolders changes the folders the Docker Engine shares, in the
// settings and in the engine's sandbox when there is one. With rungv v0.21+
// live mounts (bind attach on Unix or rpcfs mount serve on Windows), changes
// apply live to a running engine; otherwise the engine keeps what it started
// with until restarted.
func (m *Model) SetSharedFolders(folders []string) {
	folders = append([]string{}, folders...) // empty, not nil: none, not the default
	m.UpdateSettings(func(s *store.Settings) { s.DockerSharedFolders = folders })
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, spec := range specs {
		if spec.Recipe != recipes.Docker {
			continue
		}
		if slices.Equal(recipes.SharedFoldersOf(spec), folders) {
			return
		}
		st := m.sup.State(spec.ID)
		if !st.Status.Active() && m.Offline {
			st = ggui.Peek(m.States)[spec.ID]
		}
		activeSpec, hasActive := m.activeSpec(spec.ID)
		desiredSpec := recipes.WithSharedFolders(spec, folders)
		specs[i] = desiredSpec
		m.saveSpecs(specs)
		if st.Status.Active() {
			if (st.BindAttach || st.LiveMounts) && hasActive {
				add, remove := recipes.SharedFolderDiff(activeSpec, desiredSpec)
				if len(add) > 0 || len(remove) > 0 {
					id := spec.ID
					diff := sandbox.LiveDiff{AddMounts: add, RemoveMounts: remove}
					m.async(func(ctx context.Context) error {
						ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
						defer cancel()
						return m.applyLiveSandbox(ctx, id, diff)
					}, func(err error) {
						if err != nil {
							m.Toasts.Push(ui.Toast(tr("Restart the Docker Engine to share the change"),
								err.Error()).Warning().Duration(8 * time.Second))
							return
						}
						m.refreshStates()
						m.syncEngineSpec(ggui.Peek(m.Rows))
						m.toastOK(tr("Shared folders updated"), tr("Applied to the running Docker Engine without restarting."))
					})
				}
				return
			}
			m.Toasts.Push(ui.Toast(tr("Restart the Docker Engine to share the change"),
				tr("It sees the folders it started with until then.")).Duration(8 * time.Second))
		}
		return
	}
}

// SetDockerExposeLAN toggles opt-in LAN port publishing (0.0.0.0) versus
// loopback-only (127.0.0.1) in settings and in the Docker Engine sandbox spec
// when one exists.
func (m *Model) SetDockerExposeLAN(on bool) {
	before := ggui.Peek(m.Settings).DockerExposeLAN
	m.UpdateSettings(func(s *store.Settings) { s.DockerExposeLAN = on })
	addr := ""
	if on {
		addr = "0.0.0.0"
	}
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, spec := range specs {
		if spec.Recipe != recipes.Docker {
			continue
		}
		updated := spec
		if on && !sandbox.HasAutoPublish(updated) {
			updated = sandbox.WithAutoPublish(updated, true)
		}
		updated = recipes.WithDockerAutoPublishAddr(updated, addr)
		changed := !updated.Equal(spec)
		if changed {
			specs[i] = updated
			m.saveSpecs(specs)
		}
		st := m.sup.State(spec.ID)
		if !st.Status.Active() && m.Offline {
			st = ggui.Peek(m.States)[spec.ID]
		}
		if st.Status.Active() && (changed || before != on) {
			if ggui.Peek(m.Settings).SandboxEngine {
				m.Toasts.Push(ui.Toast(tr("LAN port publishing updated"),
					tr("New sandbox-engine containers use this setting immediately; restart the Docker Engine sandbox if you switch back to it.")).Duration(8 * time.Second))
			} else {
				m.Toasts.Push(ui.Toast(tr("Restart the Docker Engine to apply the change"),
					tr("The new port bind address applies on the next start of the Docker Engine.")).Duration(8 * time.Second))
			}
		}
		return
	}
}

// SetDockerSSHAgent toggles opt-in host SSH agent forwarding in settings and
// in the Docker Engine sandbox spec when one exists.
func (m *Model) SetDockerSSHAgent(on bool) {
	before := ggui.Peek(m.Settings).DockerSSHAgent
	m.UpdateSettings(func(s *store.Settings) { s.DockerSSHAgent = on })
	hostSock := ""
	if on {
		hostSock = strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	}
	dockerRunning := false
	changed := false
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, spec := range specs {
		if spec.Recipe != recipes.Docker {
			continue
		}
		updated := recipes.WithSSHAgentForward(spec, hostSock)
		if !updated.Equal(spec) {
			specs[i] = updated
			m.saveSpecs(specs)
			changed = true
		}
		st := m.sup.State(spec.ID)
		if !st.Status.Active() && m.Offline {
			st = ggui.Peek(m.States)[spec.ID]
		}
		dockerRunning = st.Status.Active()
		break
	}
	if on && hostSock == "" {
		m.Toasts.Push(ui.Toast(tr("SSH_AUTH_SOCK is not set"),
			tr("Start an SSH agent and set SSH_AUTH_SOCK so containers can use /run/host-services/ssh-auth.sock.")).Warning().Duration(8 * time.Second))
		return
	}
	if dockerRunning && (changed || before != on) {
		if ggui.Peek(m.Settings).SandboxEngine {
			m.Toasts.Push(ui.Toast(tr("SSH agent forwarding updated"),
				tr("New sandbox-engine containers use this setting immediately; restart the Docker Engine sandbox if you switch back to it.")).Duration(8 * time.Second))
		} else {
			m.Toasts.Push(ui.Toast(tr("Restart the Docker Engine to apply the change"),
				tr("SSH agent forwarding applies when the Docker Engine starts next.")).Duration(8 * time.Second))
		}
	}
}

// ShareFolder adds a folder to those the Docker Engine shares, or says why
// it cannot.
func (m *Model) ShareFolder(dir string) {
	folders, err := recipes.AddSharedFolder(m.SharedFolders(), dir)
	if err != nil {
		m.toastError(tr("Could not share %s", tildePath(dir)), errors.New(shareProblem(err)))
		return
	}
	m.SetSharedFolders(folders)
}

// UnshareFolder stops sharing a folder with the Docker Engine.
func (m *Model) UnshareFolder(dir string) {
	m.SetSharedFolders(slices.DeleteFunc(m.SharedFolders(), func(f string) bool { return f == dir }))
}

// shareProblem words why a folder cannot be shared.
func shareProblem(err error) string {
	var se *recipes.ShareError
	errors.As(err, &se)
	switch {
	case errors.Is(err, recipes.ErrAlreadyShared) && se != nil:
		return tr("It is shared already, through %s.", tildePath(se.Folder))
	case errors.Is(err, recipes.ErrNotFolder):
		return tr("No folder is at that path.")
	case errors.Is(err, recipes.ErrReserved):
		return tr("The sandbox's own system is at that path, or inside it. Share a folder of your own.")
	case errors.Is(err, recipes.ErrNoGuestPath):
		return tr("Choose a folder by its full path, on a drive of this computer.")
	}
	return err.Error()
}

// LoadDocker lists the containers of the Docker Engine recipe or the
// experimental sandbox engine. Asked for while a listing runs, it lists them
// again after it.
func (m *Model) LoadDocker() {
	if m.Offline {
		return // the containers shown are made up
	}
	if ggui.Peek(m.ContainersAccess) != ContainersOpen {
		m.DockerContainers.Set(Listing[dockerapi.Container]{})
		return
	}
	if ggui.Peek(m.Settings).SandboxEngine {
		if pe := ggui.Peek(m.DockerProxyErr); pe != "" {
			m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Err: pe})
			return
		}
		if seErr := ggui.Peek(m.SandboxEngineStatus).Err; seErr != "" {
			m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Err: seErr})
			return
		}
	}
	if m.dockerLoading {
		m.dockerAgain = true
		return
	}
	m.dockerLoading = true
	host := m.dockerBackendHost()
	var items []dockerapi.Container
	var version dockerapi.Version
	m.DockerContainers.Update(func(l Listing[dockerapi.Container]) Listing[dockerapi.Container] { l.Loading = true; return l })
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if version, err = c.Version(ctx); err != nil {
			return err
		}
		items, err = c.Containers(ctx, true)
		return err
	}, func(err error) {
		m.dockerLoading = false
		if m.dockerAgain {
			m.dockerAgain = false
			m.LoadDocker()
			return
		}
		if ggui.Peek(m.ContainersAccess) != ContainersOpen {
			return // the engine stopped meanwhile, and its list was emptied
		}
		l := Listing[dockerapi.Container]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		} else {
			m.DockerVersion.Set(version)
		}
		m.refreshEngineRunning()
		m.refreshEngineContainerRuntimes()
		m.DockerContainers.Set(l)
	})
}

// followDocker keeps the engine's containers current while it runs, for
// the Docker and Containers pages and the menus: often while a page shows
// them, now and then otherwise, and not at all while the engine is stopped.
// The logs of the container the Containers page shows follow with them.
// Call it from Start.
func (m *Model) followDocker() {
	if m.Offline {
		return
	}
	every := ggui.Derived(func() time.Duration {
		if !m.ContainersOpen.Get() {
			return 0
		}
		switch m.Page.Get() {
		case PageDocker, PageContainers, PageNetworks:
			return 3 * time.Second
		}
		return 10 * time.Second
	})
	networksEvery := ggui.Derived(func() time.Duration {
		if m.ContainersOpen.Get() && m.Page.Get() == PageNetworks {
			return 3 * time.Second
		}
		return 0
	})
	m.poll(networksEvery, m.LoadDockerNetworks, m.LoadDockerNetworks)
	logs := ggui.Derived(m.followsContainerLogs)
	m.poll(every, m.LoadDocker, func() {
		m.LoadDocker()
		if ggui.Peek(logs) {
			m.LoadContainerLogs()
		}
	})
	imagesEvery := ggui.Derived(func() time.Duration {
		if !m.ContainersOpen.Get() {
			return 0
		}
		switch m.Page.Get() {
		case PageImages:
			v := m.ImageView.Get()
			if v == ImagesDocker || (m.Settings.Get().SandboxEngine && v != ImagesRegistry) {
				return 3 * time.Second
			}
			return 0
		case PageDocker:
			return 10 * time.Second
		}
		return 0
	})
	m.poll(imagesEvery, m.LoadDockerImages, m.LoadDockerImages)
	id := ggui.Map(m.Container, func(c dockerapi.Container) string { return c.ID })
	shown := ggui.Combine(logs, id, func(on bool, id string) string {
		if !on {
			return ""
		}
		return id
	})
	m.followLogs(shown, id, m.LoadContainerLogs, m.ContainerLogs)
}

// followLinks asks docker and kubectl about their contexts at the start,
// whenever a screen that tells of them opens, and when what they should
// point at changes. Call it from Start.
func (m *Model) followLinks() {
	if m.Offline {
		return
	}
	shown := func(page, recipe string) *ggui.DerivedValue[bool] {
		return ggui.Derived(func() bool {
			switch m.Page.Get() {
			case page:
				return true
			case PageSandboxes:
				if m.Detail.Get() != connectTab {
					return false
				}
				id := m.Selected.Get()
				for _, s := range m.Specs.Get() {
					if s.ID == id {
						return s.Recipe == recipe
					}
				}
			}
			return false
		})
	}
	ggui.Watch(shown(PageDocker, recipes.Docker), func(on bool) {
		if on {
			m.CheckLink(m.DockerLink)
		}
	})
	ggui.Watch(shown(PageKubernetes, recipes.Kubernetes), func(on bool) {
		if on {
			m.CheckLink(m.KubeLink)
		}
	})
	// Each runs at once as well, which asks both tools at the start.
	ggui.Watch(m.DockerEndpoint, func(string) { m.CheckLink(m.DockerLink) })
	ggui.Watch(m.KubeconfigReady, func(bool) { m.CheckLink(m.KubeLink) })
}

// DockerAction runs start, stop, restart, pause, unpause or remove on a container.
func (m *Model) DockerAction(action string, ctr dockerapi.Container) {
	busyKey := "ctr:" + ctr.ID
	if m.isBusy(busyKey) {
		return
	}
	m.setBusy(busyKey, true)
	host := m.dockerBackendHost()
	m.async(func(ctx context.Context) error {
		if action == "pause" {
			m.closeTerminalsFor(containerTermTarget(ctr.ID))
		}
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := dockerDo(ctx, c, action, ctr.ID); err != nil {
			return err
		}
		if action == "unpause" {
			m.drainContainerSweeps(ctx, c, ctr.ID)
		}
		return nil
	}, func(err error) {
		m.setBusy(busyKey, false)
		if err != nil {
			m.toastError(containerActionErrorTitle(action, ctr.Name()), err)
		}
		m.LoadDocker()
		if ggui.Peek(m.Page) == PageContainers && ggui.Peek(m.Container).ID == ctr.ID {
			m.LoadContainerLogs() // what it wrote as it stopped or started
		}
	})
}

func containerActionErrorTitle(action, name string) string {
	switch action {
	case "start":
		return tr("Could not start %s", name)
	case "stop":
		return tr("Could not stop %s", name)
	case "restart":
		return tr("Could not restart %s", name)
	case "pause":
		return tr("Could not pause %s", name)
	case "unpause":
		return tr("Could not resume %s", name)
	case "remove":
		return tr("Could not remove %s", name)
	}
	return tr("Could not update %s", name)
}

// dockerDo runs start, stop, restart, pause, unpause or remove on the container id.
func dockerDo(ctx context.Context, c *dockerapi.Client, action, id string) error {
	switch action {
	case "start":
		return c.Start(ctx, id)
	case "stop":
		return c.Stop(ctx, id)
	case "restart":
		return c.Restart(ctx, id)
	case "pause":
		return c.Pause(ctx, id)
	case "unpause":
		return c.Unpause(ctx, id)
	case "remove":
		return c.Remove(ctx, id, true)
	}
	return fmt.Errorf("unknown action %s", action)
}

// AskRemoveContainer confirms and removes a container.
func (m *Model) AskRemoveContainer(ctr dockerapi.Container) {
	m.Confirm(tr("Remove %s?", ctr.Name()), tr("The container is stopped and removed from the engine."), tr("Remove container"), func() { m.DockerAction("remove", ctr) })
}

// DockerCLI finds the docker command: the one the settings name, and
// otherwise where a terminal would find it.
func (m *Model) DockerCLI() (string, bool) {
	return findCLI("docker", ggui.Peek(m.Settings).DockerPath)
}

// CheckKubeconfig looks for the kubeconfig the k3s recipe writes.
func (m *Model) CheckKubeconfig() {
	_, err := os.Stat(recipes.Kubeconfig())
	m.KubeconfigReady.Set(err == nil)
}

// ---- Settings -----------------------------------------------------------

// SetDockerBindSymlinks toggles opt-in contained host symbolic links
// (symlinks=contained, rungvx v0.26+) for Sandbox Engine bind mounts.
func (m *Model) SetDockerBindSymlinks(on bool) {
	m.UpdateSettings(func(s *store.Settings) { s.DockerBindSymlinks = on })
}

// UpdateSettings applies and stores a change of the settings.
func (m *Model) UpdateSettings(fn func(*store.Settings)) {
	s := ggui.Peek(m.Settings)
	before := s
	fn(&s)
	m.dockerExposeLAN.Store(s.DockerExposeLAN)
	m.dockerSSHAgent.Store(s.DockerSSHAgent)
	m.dockerBindSymlinks.Store(s.DockerBindSymlinks)
	m.developerMode.Store(s.DeveloperMode)
	m.sandboxEngineMode.Store(s.SandboxEngine)
	if ggui.Peek(m.DockerExposeLAN) != s.DockerExposeLAN {
		m.DockerExposeLAN.Set(s.DockerExposeLAN)
	}
	if ggui.Peek(m.DockerSSHAgent) != s.DockerSSHAgent {
		m.DockerSSHAgent.Set(s.DockerSSHAgent)
	}
	if ggui.Peek(m.DockerBindSymlinks) != s.DockerBindSymlinks {
		m.DockerBindSymlinks.Set(s.DockerBindSymlinks)
	}
	m.Settings.Set(s)
	if err := m.store.SaveSettings(s); err != nil {
		m.toastError(tr("Could not save settings"), err)
	}
	if s.RungvPath != before.RungvPath || s.CacheDir != before.CacheDir || s.VolumeDir != before.VolumeDir {
		m.DetectEngine(nil)
	}
	if s.DockerPath != before.DockerPath {
		m.CheckLink(m.DockerLink)
		m.refreshBundledDockerState()
		if s.SandboxEngine {
			m.syncSandboxBuilder(true)
			if s.Amd64Builds {
				m.syncAmd64BuildxNode(true)
			}
		}
	}
}

// Copy puts text on the clipboard and says so.
func (m *Model) Copy(text, what string) {
	if m.host == nil {
		return
	}
	m.host.Clipboard().Write(text)
	m.Toasts.Push(ui.Toast(tr("Copied"), what).Duration(2 * time.Second))
}

// Open opens a URL or reveals a path.
func (m *Model) Open(target string) {
	var err error
	if strings.Contains(target, "://") {
		err = runtime.OpenURL(target)
	} else {
		err = runtime.Reveal(target)
	}
	if err != nil {
		m.toastError(tr("Could not open %s", target), err)
	}
}

// PrepareQuit decides what quitting does with running sandboxes and
// sandbox-engine containers: true when the app may close now.
func (m *Model) PrepareQuit() bool {
	m.refreshEngineRunning()
	if m.ActiveCount() == 0 {
		return true
	}
	m.QuitOpen.Set(true)
	return false
}

// StopAll stops every sandbox and every running sandbox-engine container, as
// the user asked: the recipe sandboxes among them are not started again with
// the app.
func (m *Model) StopAll() {
	for _, id := range m.sup.Running() {
		m.setResume(id, false)
	}
	m.stopAll(false, func() {})
}

// StopAllAndThen stops every sandbox (and closes the in-app sandbox engine) to
// quit, then runs then on the UI thread. The recipe sandboxes running start
// again with the app. A quit asked again while one is under way, as Homebrew
// asks until the app is gone, stops nothing more: the first one ends the app.
func (m *Model) StopAllAndThen(then func()) {
	if m.quitting {
		return
	}
	m.quitting = true
	m.stopAll(true, then)
}

// StopAllForUpdate stops running sandboxes and sandbox-engine containers before
// an in-app update applies, without marking the model as quitting or closing
// the sandbox engine so the app remains fully recoverable if Apply fails.
func (m *Model) StopAllForUpdate(then func()) {
	m.stopAll(false, then)
}

func (m *Model) stopAll(closeEngine bool, then func()) {
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	id := m.Toasts.Push(ui.Toast(tr("Stopping sandboxes"), tr("Waiting for the guests to exit…")).Loading())
	m.async(func(context.Context) error {
		var wg sync.WaitGroup
		wg.Go(func() { m.sup.StopAll(timeout) })
		if closeEngine {
			wg.Go(func() { m.CloseSandboxEngine(timeout) })
			wg.Go(func() { m.CloseLocalRegistry() })
			wg.Go(func() { m.CloseLocalProxy() })
			wg.Go(func() { m.CloseMCPServer() })
		} else {
			wg.Go(func() { m.StopSandboxEngineContainers(timeout) })
		}
		wg.Wait()
		return nil
	}, func(error) {
		m.Toasts.Dismiss(id)
		m.refreshEngineRunning()
		if !closeEngine {
			m.LoadDocker()
		}
		then()
	})
}

// ---- Editor draft -------------------------------------------------------

// Draft is the editor's form: lists are edited as text, one entry a line.
type Draft struct {
	ID, Name, Image, Command string
	Entrypoint               string
	SetEntrypoint            bool
	Ports, UnixForwards, Env string
	Volumes, Binds, Tmpfs    string
	Workspace                string
	WorkspaceRO              bool
	WorkspaceSymlinks        bool
	RootfsVolume             string
	Memory, CPUs             string
	Network, WGConf          string
	IsolateLAN               bool
	NetTX, NetRX             string
	NATPolicyFile            string
	DynamicPortAddrs         string
	AutoPublish              bool
	AutoPublishAddr          string
	Backend                  string
	Hostname, User, Workdir  string
	Privileged, HostCACerts  bool
	Pull                     string
	Checkpoint, AutoStart    bool
	ReadyPattern, ExtraArgs  string
	Recipe                   string
	CreatedAt                time.Time
}

// DraftFrom fills the form from a spec.
func DraftFrom(s sandbox.Spec) Draft {
	d := Draft{
		ID: s.ID, Name: s.Name, Image: s.Image, Command: s.Command,
		Entrypoint: s.Entrypoint, SetEntrypoint: s.SetEntrypoint,
		Ports: strings.Join(s.Ports, "\n"), UnixForwards: strings.Join(s.UnixForwards, "\n"), Env: strings.Join(s.Env, "\n"),
		Workspace: s.Workspace, WorkspaceRO: s.WorkspaceRO, WorkspaceSymlinks: s.WorkspaceSymlinks == sandbox.SymlinksContained, RootfsVolume: s.RootfsVolume,
		Memory: s.Memory, CPUs: s.CPUs, Network: s.Network, WGConf: s.WGConf, IsolateLAN: s.IsolateLAN,
		NetTX: formatBandwidthBPS(s.NetTXBPS), NetRX: formatBandwidthBPS(s.NetRXBPS),
		NATPolicyFile: s.NATPolicyFile, DynamicPortAddrs: strings.Join(s.DynamicPortAddrs, "\n"),
		AutoPublish: sandbox.HasAutoPublish(s), AutoPublishAddr: s.AutoPublishAddr,
		Backend: s.Backend, Hostname: s.Hostname, User: s.User, Workdir: s.Workdir,
		Privileged: !s.Unprivileged, HostCACerts: s.HostCACerts, Pull: s.Pull,
		Checkpoint: s.Checkpoint, AutoStart: s.AutoStart, ReadyPattern: s.ReadyPattern,
		ExtraArgs: s.ExtraArgs, Recipe: s.Recipe, CreatedAt: s.CreatedAt,
	}
	if d.Network == "" {
		d.Network = "nat"
	}
	if d.Backend == "" {
		d.Backend = "auto"
	}
	if d.Pull == "" {
		d.Pull = "missing"
	}
	var vols, binds, tmpfs []string
	for _, mt := range s.Mounts {
		switch mt.Type {
		case sandbox.MountVolume:
			vols = append(vols, mountLine(mt.Source, mt.Target, mt.ReadOnly))
		case sandbox.MountBind:
			binds = append(binds, bindMountLine(mt.Source, mt.Target, mt.ReadOnly, mt.Symlinks))
		case sandbox.MountTmpfs:
			line := mt.Target
			var opts []string
			if mt.Size != "" {
				opts = append(opts, "size="+mt.Size)
			}
			if mt.ReadOnly {
				opts = append(opts, "ro")
			}
			if len(opts) > 0 {
				line += ":" + strings.Join(opts, ",")
			}
			tmpfs = append(tmpfs, line)
		}
	}
	d.Volumes, d.Binds, d.Tmpfs = strings.Join(vols, "\n"), strings.Join(binds, "\n"), strings.Join(tmpfs, "\n")
	return d
}

func mountLine(src, dst string, ro bool) string {
	l := src + ":" + dst
	if ro {
		l += ":ro"
	}
	return l
}

func bindMountLine(src, dst string, ro bool, symlinks string) string {
	l := src + ":" + dst
	var opts []string
	if ro {
		opts = append(opts, "ro")
	}
	if symlinks != "" {
		opts = append(opts, "symlinks="+symlinks)
	}
	if len(opts) > 0 {
		l += ":" + strings.Join(opts, ",")
	}
	return l
}

func parseBindMount(line string) (src, dst string, ro bool, symlinks string, err error) {
	i := strings.LastIndex(line, ":/")
	if i <= 0 {
		return "", "", false, "", fmt.Errorf("mount %q: write HOST:/guest/path[:ro][,symlinks=contained]", line)
	}
	src = line[:i]
	rest := line[i+1:]
	dst, opts, hasOpts := strings.Cut(rest, ":")
	if !hasOpts {
		return src, dst, false, "", nil
	}
	for opt := range strings.SplitSeq(opts, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		switch {
		case opt == "ro":
			ro = true
		case opt == "rw":
			ro = false
		case strings.HasPrefix(opt, "symlinks="):
			mode := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(opt, "symlinks=")))
			if mode != sandbox.SymlinksVirtual && mode != sandbox.SymlinksContained {
				return "", "", false, "", fmt.Errorf("mount %q: unsupported symlinks mode %q (use virtual or contained)", line, mode)
			}
			symlinks = mode
		default:
			return "", "", false, "", fmt.Errorf("mount %q: unknown bind option %q (use ro, rw, or symlinks=virtual|contained)", line, opt)
		}
	}
	return src, dst, ro, symlinks, nil
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// splitAddrList splits newline- or comma-separated IP literals, ignoring empty
// items and comment lines starting with '#'.
func splitAddrList(s string) []string {
	var out []string
	for _, line := range lines(s) {
		for item := range strings.SplitSeq(line, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

func validateBareIPAddr(raw string) error {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if addr.Zone() != "" {
		return errors.New("IP address must not include a zone")
	}
	return nil
}

// parseMount splits SRC:/DST[:ro|rw], keeping colons in SRC such as a
// Windows drive letter.
func parseMount(line string) (src, dst string, ro bool, err error) {
	if rest, ok := strings.CutSuffix(line, ":ro"); ok {
		line, ro = rest, true
	} else if rest, ok := strings.CutSuffix(line, ":rw"); ok {
		line = rest
	}
	i := strings.LastIndex(line, ":/")
	if i <= 0 {
		return "", "", false, fmt.Errorf("mount %q: write SOURCE:/guest/path[:ro]", line)
	}
	return line[:i], line[i+1:], ro, nil
}

// parseBandwidthBPS parses a human-friendly NAT bandwidth limit into bits per
// second (0 = unlimited). Accepts plain bits/s or decimal SI suffixes k/m/g/t
// (e.g. "500k", "100m", "1.5g", "100Mbps").
func parseBandwidthBPS(s string) (uint64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, nil
	}
	lower := strings.ToLower(raw)
	i := 0
	dots := 0
	for i < len(lower) {
		ch := lower[i]
		if ch >= '0' && ch <= '9' {
			i++
		} else if ch == '.' && dots == 0 {
			dots++
			i++
		} else {
			break
		}
	}
	numStr := lower[:i]
	unit := strings.TrimSpace(lower[i:])
	if numStr == "" || numStr == "." {
		return 0, fmt.Errorf("bandwidth limit %q: write bits/s with an optional k, m or g unit (for example 100m or 1g)", s)
	}
	var mult uint64
	switch unit {
	case "", "b", "bps", "bit", "bits", "b/s", "bit/s", "bits/s":
		mult = 1
	case "k", "kb", "kbps", "kbit", "kbits", "k/s", "kbit/s":
		mult = 1_000
	case "m", "mb", "mbps", "mbit", "mbits", "m/s", "mbit/s":
		mult = 1_000_000
	case "g", "gb", "gbps", "gbit", "gbits", "g/s", "gbit/s":
		mult = 1_000_000_000
	case "t", "tb", "tbps", "tbit", "tbits", "t/s", "tbit/s":
		mult = 1_000_000_000_000
	default:
		return 0, fmt.Errorf("bandwidth limit %q: unknown unit %q (use k, m or g)", s, strings.TrimSpace(raw[i:]))
	}
	if dots == 0 {
		v, err := strconv.ParseUint(numStr, 10, 64)
		if err != nil || v > math.MaxUint64/mult {
			return 0, fmt.Errorf("bandwidth limit %q is out of range", s)
		}
		return v * mult, nil
	}
	v, err := strconv.ParseFloat(numStr, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("bandwidth limit %q: invalid number", s)
	}
	prod := v * float64(mult)
	if prod >= float64(math.MaxUint64) || (v > 0 && prod < 1) {
		return 0, fmt.Errorf("bandwidth limit %q is out of range", s)
	}
	return uint64(math.Round(prod)), nil
}

// formatBandwidthBPS formats a bits-per-second limit as a compact string for
// the editor ("100m", "500k", "1.5g", or plain bits/s); 0 formats as "".
func formatBandwidthBPS(bps uint64) string {
	if bps == 0 {
		return ""
	}
	const (
		k = 1_000
		m = 1_000_000
		g = 1_000_000_000
	)
	switch {
	case bps%g == 0:
		return strconv.FormatUint(bps/g, 10) + "g"
	case bps >= g && bps%(g/10) == 0:
		return fmt.Sprintf("%d.%dg", bps/g, (bps%g)/(g/10))
	case bps%m == 0:
		return strconv.FormatUint(bps/m, 10) + "m"
	case bps >= m && bps%(m/10) == 0:
		return fmt.Sprintf("%d.%dm", bps/m, (bps%m)/(m/10))
	case bps%k == 0:
		return strconv.FormatUint(bps/k, 10) + "k"
	default:
		return strconv.FormatUint(bps, 10)
	}
}

// Spec converts the form to a sandbox spec.
func (d Draft) Spec() (sandbox.Spec, error) {
	isNAT := d.Network == "" || d.Network == "nat"
	var (
		tx, rx      uint64
		natPolicy   string
		autoPubAddr string
		dynAddrs    []string
	)
	if isNAT {
		var err error
		if tx, err = parseBandwidthBPS(d.NetTX); err != nil {
			return sandbox.Spec{}, err
		}
		if rx, err = parseBandwidthBPS(d.NetRX); err != nil {
			return sandbox.Spec{}, err
		}
		natPolicy = strings.TrimSpace(d.NATPolicyFile)
		autoPubAddr = strings.TrimSpace(d.AutoPublishAddr)
		if autoPubAddr != "" {
			if err := validateBareIPAddr(autoPubAddr); err != nil {
				return sandbox.Spec{}, fmt.Errorf("Auto-publish address %q: must be a valid IP literal.", autoPubAddr)
			}
		}
		dynAddrs = splitAddrList(d.DynamicPortAddrs)
		for _, addr := range dynAddrs {
			if err := validateBareIPAddr(addr); err != nil {
				return sandbox.Spec{}, fmt.Errorf("Dynamic port bind address %q: must be a valid IP literal.", addr)
			}
		}
	}
	s := sandbox.Spec{
		ID: d.ID, Name: strings.TrimSpace(d.Name), Image: strings.TrimSpace(d.Image),
		Command: strings.TrimSpace(d.Command), Entrypoint: strings.TrimSpace(d.Entrypoint), SetEntrypoint: d.SetEntrypoint,
		Ports: lines(d.Ports), UnixForwards: lines(d.UnixForwards), Env: lines(d.Env),
		Workspace: strings.TrimSpace(d.Workspace), WorkspaceRO: d.WorkspaceRO, RootfsVolume: strings.TrimSpace(d.RootfsVolume),
		Memory: strings.TrimSpace(d.Memory), CPUs: strings.TrimSpace(d.CPUs), WGConf: strings.TrimSpace(d.WGConf), IsolateLAN: d.IsolateLAN,
		NetTXBPS: tx, NetRXBPS: rx, NATPolicyFile: natPolicy,
		DynamicPortAddrs: dynAddrs, AutoPublishAddr: autoPubAddr,
		Hostname: strings.TrimSpace(d.Hostname), User: strings.TrimSpace(d.User), Workdir: strings.TrimSpace(d.Workdir),
		Unprivileged: !d.Privileged, HostCACerts: d.HostCACerts,
		Checkpoint: d.Checkpoint, AutoStart: d.AutoStart, ReadyPattern: strings.TrimSpace(d.ReadyPattern),
		ExtraArgs: strings.TrimSpace(d.ExtraArgs), Recipe: d.Recipe, CreatedAt: d.CreatedAt,
	}
	if !isNAT {
		s.Network = d.Network
		if sandbox.HasAutoPublish(s) {
			s = sandbox.WithAutoPublish(s, false)
		}
	} else if d.AutoPublish != sandbox.HasAutoPublish(s) {
		s = sandbox.WithAutoPublish(s, d.AutoPublish)
	}
	if d.Backend != "auto" {
		s.Backend = d.Backend
	}
	if d.Pull != "missing" {
		s.Pull = d.Pull
	}
	if s.Workspace != "" && d.WorkspaceSymlinks {
		s.WorkspaceSymlinks = sandbox.SymlinksContained
	}
	for _, l := range lines(d.Volumes) {
		src, dst, ro, err := parseMount(l)
		if err != nil {
			return s, err
		}
		s.Mounts = append(s.Mounts, sandbox.Mount{Type: sandbox.MountVolume, Source: src, Target: dst, ReadOnly: ro})
	}
	for _, l := range lines(d.Binds) {
		src, dst, ro, sym, err := parseBindMount(l)
		if err != nil {
			return s, err
		}
		s.Mounts = append(s.Mounts, sandbox.Mount{Type: sandbox.MountBind, Source: src, Target: dst, ReadOnly: ro, Symlinks: sym})
	}
	for _, l := range lines(d.Tmpfs) {
		target, opts, _ := strings.Cut(l, ":")
		mt := sandbox.Mount{Type: sandbox.MountTmpfs, Target: target}
		for o := range strings.SplitSeq(opts, ",") {
			switch {
			case strings.HasPrefix(o, "size="):
				mt.Size = strings.TrimPrefix(o, "size=")
			case o == "ro":
				mt.ReadOnly = true
			}
		}
		s.Mounts = append(s.Mounts, mt)
	}
	return s, nil
}

// ---- Helpers ------------------------------------------------------------

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func removeAll(dir string) error { return os.RemoveAll(dir) }

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatBytes formats a storage byte count (images, volumes, snapshots, files)
// with SI decimal units: integer B, up to 1 decimal for KB/MB, and up to 2
// decimals for GB and larger.
func formatBytes(n int64) string {
	if n < 0 {
		return "-" + formatBytes(-n)
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	val := float64(n) / unit
	idx := 0
	for val >= unit && idx < len(units)-1 {
		val /= unit
		idx++
	}
	prec := 1
	if idx >= 2 { // GB and larger
		prec = 2
	}
	return trimFloat(val, prec) + " " + units[idx]
}

// formatSize normalizes a human-readable size string reported by rungv (such as
// "1.711GB", "419.4MB", "9.155MB", "0B", or "113.4MB (32%)") into consistent
// spacing and precision: integer B, up to 1 decimal for KB/MB, and up to 2
// decimals for GB and larger.
func formatSize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	suffix := ""
	if i := strings.IndexByte(s, '('); i >= 0 {
		suffix = " " + strings.TrimSpace(s[i:])
		s = strings.TrimSpace(s[:i])
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+') {
		i++
	}
	if i == 0 {
		return s + suffix
	}
	val, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return s + suffix
	}
	unit := strings.TrimSpace(s[i:])
	var norm string
	var prec int
	switch strings.ToLower(unit) {
	case "b", "bytes":
		norm, prec = "B", 0
	case "kb":
		norm, prec = "KB", 1
	case "kib":
		norm, prec = "KiB", 1
	case "mb":
		norm, prec = "MB", 1
	case "mib":
		norm, prec = "MiB", 1
	case "gb":
		norm, prec = "GB", 2
	case "gib":
		norm, prec = "GiB", 2
	case "tb":
		norm, prec = "TB", 2
	case "tib":
		norm, prec = "TiB", 2
	case "pb":
		norm, prec = "PB", 2
	case "pib":
		norm, prec = "PiB", 2
	default:
		return s + suffix
	}
	return trimFloat(val, prec) + " " + norm + suffix
}

func trimFloat(v float64, prec int) string {
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if prec > 0 && strings.ContainsRune(s, '.') {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return tr("just now")
	case d < time.Minute:
		return fmt.Sprintf(tr("%ds ago"), int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(tr("%dm ago"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("%dh ago"), int(d.Hours()))
	}
	return t.Format(tr("Jan 2 15:04"))
}

func uptime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf(tr("%ds"), int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(tr("%dm %ds"), int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("%dh %dm"), int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf(tr("%dd %dh"), int(d.Hours())/24, int(d.Hours())%24)
}
