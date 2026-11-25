package ui

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	gguiruntime "github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

type extViewSession struct {
	mu        sync.Mutex
	form      *blockForm
	tree      *uiblock.Node
	title     string
	state     string
	err       string
	streamErr string
	chrome    string
	stale     bool
	gone      bool
	rev       *ggui.StateValue[int]
}

func (m *Model) extensionViewSession(extID, viewID string) *extViewSession {
	key := extID + "/" + viewID
	m.extMu.Lock()
	defer m.extMu.Unlock()
	if m.extViews == nil {
		m.extViews = map[string]*extViewSession{}
	}
	sess, ok := m.extViews[key]
	if !ok {
		sess = &extViewSession{
			form: newBlockForm(),
			rev:  ggui.State(0),
		}
		m.extViews[key] = sess
	}
	return sess
}

func (m *Model) clearExtensionViewSession(extID, viewID string) {
	key := extID + "/" + viewID
	m.extMu.Lock()
	sess := m.extViews[key]
	m.extMu.Unlock()
	if sess == nil {
		return
	}
	sess.mu.Lock()
	sess.tree = nil
	sess.err = ""
	sess.streamErr = ""
	sess.gone = false
	sess.stale = false
	sess.mu.Unlock()
	sess.rev.Update(func(v int) int { return v + 1 })
}

func (m *Model) dropExtensionViewSessions(extID string) {
	if m == nil || extID == "" {
		return
	}
	prefix := extID + "/"
	m.extMu.Lock()
	var dropped []*extViewSession
	for k, sess := range m.extViews {
		if strings.HasPrefix(k, prefix) {
			delete(m.extViews, k)
			if sess != nil {
				dropped = append(dropped, sess)
			}
		}
	}
	m.extMu.Unlock()
	for _, sess := range dropped {
		sess.mu.Lock()
		sess.tree = nil
		sess.title = ""
		sess.state = ""
		sess.err = ""
		sess.streamErr = ""
		sess.chrome = ""
		sess.stale = false
		sess.gone = true
		sess.mu.Unlock()
		sess.rev.Update(func(v int) int { return v + 1 })
	}
}

// SetExtensionManager attaches an extension Manager to the model (nil-safe).
// When non-nil, a background snapshot-diff poller keeps UI state synchronized
// even if the manager is mutated externally (e.g. via tests or background jobs).
func (m *Model) SetExtensionManager(mgr *ext.Manager) {
	if m == nil {
		return
	}
	m.setExtensionManager(mgr, false)
}

func (m *Model) setExtensionManager(mgr *ext.Manager, owned bool) {
	m.extLifeMu.Lock()
	defer m.extLifeMu.Unlock()

	m.stopWatchPoller()
	m.extMgrPtr.Store(mgr)
	m.cancelExtensionStream()
	m.extMu.Lock()
	m.extOwnedMgr = owned
	m.extMu.Unlock()

	if m.host != nil {
		m.post(m.refreshExtensions)
	} else {
		m.refreshExtensions()
	}
	if mgr != nil {
		m.startWatchPoller(mgr)
	}
}

func (m *Model) stopWatchPoller() {
	m.extMu.Lock()
	cancel := m.extWatchCancel
	done := m.extWatchDone
	m.extWatchCancel = nil
	m.extWatchDone = nil
	m.extMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *Model) startWatchPoller(mgr *ext.Manager) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.extMu.Lock()
	m.extWatchCancel = cancel
	m.extWatchDone = done
	m.extMu.Unlock()

	go func() {
		defer close(done)
		lastList := mgr.List()
		lastSec := mgr.SecretStoreStatus()
		lastRoutes := mgr.LocalProxyRoutes()
		lastBuild := buildLogCounts(mgr, lastList)

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				curList := mgr.List()
				curSec := mgr.SecretStoreStatus()
				curRoutes := mgr.LocalProxyRoutes()
				curBuild := buildLogCounts(mgr, curList)
				if !extensionsEqual(lastList, curList) ||
					lastSec != curSec ||
					!slices.EqualFunc(lastRoutes, curRoutes, localproxy.Route.Equal) ||
					!maps.Equal(lastBuild, curBuild) {
					lastList = curList
					lastSec = curSec
					lastRoutes = curRoutes
					lastBuild = curBuild
					m.post(m.refreshExtensions)
				}
			}
		}
	}()
}

func buildLogCounts(mgr *ext.Manager, list []ext.ExtensionInfo) map[string]int {
	var out map[string]int
	for _, info := range list {
		if info.State == ext.StateBuilding {
			if out == nil {
				out = map[string]int{}
			}
			out[info.ID] = len(mgr.BuildLogs(info.ID))
		}
	}
	return out
}

func (m *Model) stopExtensionWatchers() {
	if m == nil {
		return
	}
	m.extLifeMu.Lock()
	defer m.extLifeMu.Unlock()

	m.stopWatchPoller()
	m.cancelExtensionStream()
	m.extMu.Lock()
	owned := m.extOwnedMgr
	m.extOwnedMgr = false
	m.extMu.Unlock()
	if owned {
		if mgr := m.extMgrPtr.Load(); mgr != nil {
			_ = mgr.Close(2 * time.Second)
		}
	}
}

// CloseExtensions cancels view subscriptions/watchers and gracefully closes the
// attached extension Manager within timeout. Safe on nil Model and idempotent.
func (m *Model) CloseExtensions(timeout time.Duration) {
	if m == nil {
		return
	}
	m.extLifeMu.Lock()
	defer m.extLifeMu.Unlock()

	m.stopWatchPoller()
	m.cancelExtensionStream()
	if mgr := m.extMgrPtr.Load(); mgr != nil {
		_ = mgr.Close(timeout)
	}
	m.extMu.Lock()
	m.extOwnedMgr = false
	m.extMu.Unlock()
}

// SetExtensionsEnabled turns the Labs Extensions feature on or off, persisting
// the setting and starting or stopping the owned extension Manager accordingly.
func (m *Model) SetExtensionsEnabled(enabled bool) {
	if m == nil {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.Extensions = enabled
	})
	if enabled {
		if m.extMgrPtr.Load() == nil {
			m.initExtensions()
		}
		return
	}
	cur := ggui.Peek(m.Page)
	if cur == PageExtensions || strings.HasPrefix(cur, PageExtensionPrefix) {
		if LabsSupported() {
			m.Go(PageLabs)
		} else {
			m.Go(PageDashboard)
		}
	}
	m.stopOwnedExtensions()
}

func (m *Model) stopOwnedExtensions() {
	if m == nil {
		return
	}
	m.extLifeMu.Lock()
	m.extMu.Lock()
	owned := m.extOwnedMgr
	m.extMu.Unlock()
	if !owned {
		m.extLifeMu.Unlock()
		return
	}
	m.stopWatchPoller()
	m.cancelExtensionStream()
	mgr := m.extMgrPtr.Swap(nil)
	m.extMu.Lock()
	m.extOwnedMgr = false
	m.extMu.Unlock()
	m.extLifeMu.Unlock()

	if m.host != nil {
		m.post(m.refreshExtensions)
	} else {
		m.refreshExtensions()
	}
	if mgr != nil {
		go func() {
			_ = mgr.Close(5 * time.Second)
		}()
	}
}

func (m *Model) initExtensions() {
	if m.extMgrPtr.Load() != nil {
		m.refreshExtensions()
		return
	}
	if m.Offline || m.dataDir == "" {
		return
	}

	var (
		kc     keychain.Keychain
		kcNote string
	)
	if strings.EqualFold(strings.TrimSpace(os.Getenv(keychain.EnvVar)), "file") {
		kc = nil
		kcNote = "GVX_KEYCHAIN=file"
	} else if testing.Testing() {
		kc = &keychain.Fake{}
	} else {
		var err error
		kc, err = keychain.System()
		if err != nil {
			kc = nil
			kcNote = err.Error()
		}
	}

	mgr, err := ext.NewManager(ext.Config{
		DataDir:             m.dataDir,
		Keychain:            kc,
		KeychainNote:        kcNote,
		DeveloperMode:       m.developerMode.Load,
		HostInfo:            m.snapshotHostInfo,
		Supervisor:          m.sup,
		DockerBackendDial:   m.extensionDockerDial,
		DockerEngineHandler: m.extensionDockerEngineHandler,
		ImageBuilder:        ext.NewEngineImageBuilder(m.extensionBuildKitHandler),
		ImagePuller:         modelImagePuller{m: m},
		UIHost:              modelUIHost{m: m},
	})
	if err != nil {
		return
	}
	m.setExtensionManager(mgr, true)
	m.async(func(context.Context) error {
		// Use a long-lived background context for Manager.Start: Manager stores
		// this context as its lifetime baseCtx until Close() is called, whereas
		// m.async cancels its callback ctx as soon as Start returns.
		return mgr.Start(context.Background())
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Extensions failed to start"), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) followExtensions() {
	ggui.Effect(func() ggui.Cleanup {
		set := m.Settings.Get()
		eng := m.Engine.Get()
		_ = m.SandboxEngineStatus.Get()
		_ = m.Rows.Get()
		cOpen := m.ContainersOpen.Get()
		m.developerMode.Store(set.DeveloperMode)
		m.sandboxEngineMode.Store(set.SandboxEngine)
		hi := m.updateHostInfoSnapshot(set, eng, cOpen)

		if set.Extensions {
			if m.extMgrPtr.Load() == nil {
				m.initExtensions()
			}
		} else {
			m.stopOwnedExtensions()
		}

		if mgr := m.extMgrPtr.Load(); mgr != nil {
			if hi.Theme != "" && hi.Theme != m.extThemeSent {
				m.extThemeSent = hi.Theme
				mgr.NotifyHostEvent("host.theme", map[string]string{"theme": hi.Theme})
			}
			if hi.Locale != "" && hi.Locale != m.extLocaleSent {
				m.extLocaleSent = hi.Locale
				mgr.NotifyHostEvent("host.locale", map[string]string{"locale": hi.Locale})
			}
			dockerState := "stopped"
			if hi.DockerReady {
				dockerState = "ready"
			} else if set.SandboxEngine && ggui.Peek(m.SandboxEngineStatus).Opening {
				dockerState = "starting"
			}
			if dockerState != m.extDockerStateSent {
				m.extDockerStateSent = dockerState
				mgr.NotifyHostEvent("docker.state", map[string]any{
					"state": dockerState,
					"mode":  hi.DockerMode,
					"ready": hi.DockerReady,
				})
			}
		}
		return nil
	})
}

func (m *Model) updateHostInfoSnapshot(set store.Settings, eng Engine, dockerReady bool) ext.HostInfo {
	theme := "light"
	switch set.Theme {
	case "dark":
		theme = "dark"
	case "light":
		theme = "light"
	default:
		if ggui.Peek(ggui.SystemDark()) {
			theme = "dark"
		}
	}
	dockerMode := "off"
	if set.SandboxEngine {
		dockerMode = "sandbox"
	} else {
		for _, s := range ggui.Peek(m.Specs) {
			if s.Recipe == recipes.Docker {
				dockerMode = "dind"
				break
			}
		}
	}
	hi := ext.HostInfo{
		AppVersion:   AppVersion,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Locale:       ResolveLanguage(set.Language),
		Theme:        theme,
		DockerMode:   dockerMode,
		DockerReady:  dockerReady,
		RungvVersion: shortVersion(eng.Version.Version),
	}
	m.hostInfoSnap.Store(&hi)
	return hi
}

func (m *Model) snapshotHostInfo() ext.HostInfo {
	if p := m.hostInfoSnap.Load(); p != nil {
		return *p
	}
	return ext.HostInfo{
		AppVersion: AppVersion,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Locale:     LangEnglish,
		Theme:      "light",
		DockerMode: "off",
	}
}

func (m *Model) extensionDockerDial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", recipes.DockerBackendSocket())
}

func (m *Model) extensionDockerEngineHandler() http.Handler {
	if !m.sandboxEngineMode.Load() || m.sandboxEngine == nil {
		return nil
	}
	return m.sandboxEngine.Handler()
}

func (m *Model) extensionBuildKitHandler() http.Handler {
	if m.sandboxEngineMode.Load() && m.sandboxEngine != nil {
		return m.sandboxEngine.Handler()
	}
	trp := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return m.extensionDockerDial(ctx)
		},
	}
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "docker"
		},
		Transport: trp,
	}
}

type modelImagePuller struct{ m *Model }

func (p modelImagePuller) Pull(ctx context.Context, ref string, line func(string)) error {
	return p.m.Client().Pull(ctx, ref, line)
}

type modelUIHost struct{ m *Model }

func (h modelUIHost) PickFolder(ctx context.Context, _ string) (string, bool, error) {
	if h.m.host == nil {
		return "", false, errors.New("desktop UI host is not attached")
	}
	dir, err := h.m.host.Dialogs().PickFolder(gguiruntime.FileDialog{Title: tr("Choose a folder")})
	if err != nil {
		return "", false, err
	}
	if dir == "" {
		return "", false, nil
	}
	return dir, true, nil
}

func (h modelUIHost) OpenTerminal(targetType, targetID, _ string) error {
	switch targetType {
	case "sandbox":
		h.m.post(func() { h.m.ShowSandbox(targetID, SandboxTerminalTab) })
		return nil
	case "container":
		h.m.post(func() { h.m.ShowContainer(targetID, containerTerminalTab) })
		return nil
	default:
		return errors.New("unsupported terminal target")
	}
}

func (h modelUIHost) OpenURL(rawURL string) error {
	return gguiruntime.OpenURL(rawURL)
}

func (h modelUIHost) CopyText(text string) error {
	if h.m.host == nil {
		return errors.New("desktop UI host is not attached")
	}
	h.m.host.Clipboard().Write(text)
	return nil
}

func (h modelUIHost) ShowToast(level, message string) {
	h.m.post(func() {
		if h.m.quitting {
			return
		}
		toast := ui.Toast(message, "")
		switch level {
		case "success":
			toast = toast.Success()
		case "warning":
			toast = toast.Warning()
		case "error":
			toast = toast.Error()
		default:
			toast = toast.Info()
		}
		h.m.Toasts.Push(toast)
	})
}

func (h modelUIHost) Changed() {
	h.m.post(h.m.refreshExtensions)
}

func (m *Model) refreshExtensions() {
	if m == nil || m.Extensions == nil {
		return
	}
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		m.Extensions.Set(nil)
		if m.ExtensionSecrets != nil {
			m.ExtensionSecrets.Set(ext.SecretStoreStatus{})
		}
		m.syncLocalProxy(false)
		return
	}
	list := mgr.List()
	m.Extensions.Set(list)
	if m.ExtensionSecrets != nil {
		m.ExtensionSecrets.Set(mgr.SecretStoreStatus())
	}
	if m.ExtBuildLogsTick != nil {
		for _, info := range list {
			if info.State == ext.StateBuilding {
				m.ExtBuildLogsTick.Update(func(v int) int { return v + 1 })
				break
			}
		}
	}
	m.syncLocalProxy(false)
}

func (m *Model) extensionLocalProxyRoutes() []localproxy.Route {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return nil
	}
	return mgr.LocalProxyRoutes()
}

func (m *Model) cancelExtensionStream() {
	if m == nil {
		return
	}
	m.extStreamMu.Lock()
	cancel := m.extStreamCancel
	m.extStreamCancel = nil
	m.extStreamKey = ""
	m.extStreamMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Model) ensureExtensionStream(extID, viewID, locale string) {
	key := extID + "/" + viewID + "\x00" + locale
	m.extStreamMu.Lock()
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		m.extStreamMu.Unlock()
		return
	}
	if m.extStreamKey == key && m.extStreamCancel != nil {
		m.extStreamMu.Unlock()
		return
	}
	prevCancel := m.extStreamCancel
	ctx, cancel := context.WithCancel(context.Background())
	m.extStreamKey = key
	m.extStreamCancel = cancel
	m.extStreamMu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}

	sess := m.extensionViewSession(extID, viewID)
	go func() {
		ch, err := mgr.SubscribeView(ctx, extID, viewID, locale)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.post(func() {
				sess.mu.Lock()
				sess.streamErr = err.Error()
				sess.mu.Unlock()
				sess.rev.Update(func(v int) int { return v + 1 })
			})
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case upd, ok := <-ch:
				if !ok {
					return
				}
				u := upd
				m.post(func() {
					if ctx.Err() != nil {
						return
					}
					sess.mu.Lock()
					if u.Tree != nil {
						sess.tree = u.Tree
						sess.streamErr = ""
					} else if len(u.Ops) > 0 && sess.tree != nil {
						next, applyErr := uiblock.Apply(sess.tree, u.Ops)
						if applyErr != nil {
							sess.streamErr = applyErr.Error()
						} else {
							sess.tree = next
							sess.streamErr = ""
						}
					}
					if u.Title != "" {
						sess.title = u.Title
					}
					if u.State != "" {
						sess.state = u.State
					}
					sess.err = u.Error
					if u.Chrome != "" {
						sess.chrome = u.Chrome
					}
					sess.stale = u.Stale
					sess.gone = u.Gone
					sess.mu.Unlock()
					sess.rev.Update(func(v int) int { return v + 1 })
				})
			}
		}
	}()
}

// ---- Extension UI actions -----------------------------------------------

func (m *Model) SendExtensionViewEvent(extID, viewID string, ev uiblock.Event) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	if ev.Locale == "" {
		ev.Locale = ggui.Peek(m.Lang)
	}
	m.async(func(context.Context) error {
		return mgr.SendViewEvent(extID, viewID, ev)
	}, func(err error) {
		if err != nil && !isIgnorableNoChangeHandlerError(ev, err) {
			m.toastError(tr("Extension action failed"), err)
		}
	})
}

func isIgnorableNoChangeHandlerError(ev uiblock.Event, err error) bool {
	if err == nil || ev.Kind != uiblock.EventChange {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "extension HTTP 400 (invalid_event):") &&
		strings.Contains(msg, "has no change handler")
}

func (m *Model) ReloadExtensionView(extID, viewID string) {
	m.cancelExtensionStream()
	m.clearExtensionViewSession(extID, viewID)
	m.refreshExtensions()
	if page := ggui.Peek(m.Page); page == ExtensionPage(extID, viewID) {
		m.ensureExtensionStream(extID, viewID, ggui.Peek(m.Lang))
	}
}

func (m *Model) RunExtensionCommand(extID, cmdID string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	locale := ggui.Peek(m.Lang)
	m.async(func(context.Context) error {
		return mgr.RunCommand(extID, cmdID, locale)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Command failed"), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) OpenExtensionWebUI(info ext.ExtensionInfo) {
	if info.WebUI == nil {
		return
	}
	lp := ggui.Peek(m.LocalProxy)
	port := lp.HTTPSPort
	if port <= 0 {
		port = store.DefaultLocalProxyHTTPSPort
	}
	host := localproxy.BuildHost(info.ID)
	url := localproxy.FormatURL("https", host, port) + info.WebUI.Path
	m.Open(url)
}

func (m *Model) InstallGitExtension(repo, ref string, onDone func(error)) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		if onDone != nil {
			onDone(errors.New("extension manager unavailable"))
		}
		return
	}
	m.async(func(context.Context) error {
		_, err := mgr.InstallGit(strings.TrimSpace(repo), strings.TrimSpace(ref))
		return err
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Install failed"), err)
		} else {
			m.toastOK(tr("Extension installed"), tr("Review permissions and allow it to build."))
		}
		m.refreshExtensions()
		if onDone != nil {
			onDone(err)
		}
	})
}

func (m *Model) InstallLocalExtension(dir string, onDone func(error)) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		if onDone != nil {
			onDone(errors.New("extension manager unavailable"))
		}
		return
	}
	m.async(func(context.Context) error {
		_, err := mgr.InstallLocal(strings.TrimSpace(dir))
		return err
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Install failed"), err)
		} else {
			m.toastOK(tr("Extension installed"), tr("Review permissions and allow it to build."))
		}
		m.refreshExtensions()
		if onDone != nil {
			onDone(err)
		}
	})
}

func (m *Model) InstallImageExtension(ref string, onDone func(error)) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		if onDone != nil {
			onDone(errors.New("extension manager unavailable"))
		}
		return
	}
	m.async(func(context.Context) error {
		_, err := mgr.InstallImage(strings.TrimSpace(ref))
		return err
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Install failed"), err)
		} else {
			m.toastOK(tr("Extension installed"), tr("Review permissions and allow it to build."))
		}
		m.refreshExtensions()
		if onDone != nil {
			onDone(err)
		}
	})
}

func (m *Model) AllowExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.AllowAndBuild(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not start %s", id), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) DenyExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.Deny(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not deny %s", id), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) RevokeExtensionPermissions(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.RevokePermissions(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not revoke permissions"), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) EnableExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.Enable(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not enable %s", id), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) DisableExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.Disable(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not disable %s", id), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) RebuildExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.Rebuild(id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Rebuild failed"), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) UpdateExtension(id string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	var updated, needsApproval bool
	m.async(func(context.Context) error {
		var err error
		updated, needsApproval, err = mgr.Update(id)
		return err
	}, func(err error) {
		switch {
		case err != nil:
			m.toastError(tr("Update failed"), err)
		case !updated:
			m.toastOK(tr("Already up to date"), id)
		case needsApproval:
			m.toastOK(tr("Extension updated"), tr("Review new permissions and allow it to build."))
		default:
			m.toastOK(tr("Extension updated"), id)
		}
		m.refreshExtensions()
	})
}

func (m *Model) UninstallExtension(id string, purgeData bool) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	if cur := ggui.Peek(m.Page); strings.HasPrefix(cur, PageExtensionPrefix+id+"/") {
		m.Go(PageExtensions)
	}
	m.extStreamMu.Lock()
	activeForID := strings.HasPrefix(m.extStreamKey, id+"/")
	m.extStreamMu.Unlock()
	if activeForID {
		m.cancelExtensionStream()
	}
	m.async(func(context.Context) error {
		return mgr.Uninstall(id, purgeData)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Uninstall failed"), err)
		} else {
			m.dropExtensionViewSessions(id)
			m.toastOK(tr("Extension uninstalled"), id)
		}
		m.refreshExtensions()
	})
}

func (m *Model) RemoveExtensionFolder(id, dir string) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.RemoveAllowedFolder(id, dir)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not remove folder"), err)
		}
		m.refreshExtensions()
	})
}

func (m *Model) SetExtensionSetting(id, key string, val any) {
	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		return
	}
	m.async(func(context.Context) error {
		return mgr.SetSetting(id, key, val)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not save setting"), err)
		}
		m.refreshExtensions()
	})
}

// ---- Equality helpers for signals ---------------------------------------

func extensionsEqual(a, b []ext.ExtensionInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !extensionInfoEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func extensionInfoEqual(a, b ext.ExtensionInfo) bool {
	if a.ID != b.ID ||
		a.Name != b.Name ||
		a.Description != b.Description ||
		a.Icon != b.Icon ||
		a.Version != b.Version ||
		a.Publisher != b.Publisher ||
		a.Bundled != b.Bundled ||
		a.State != b.State ||
		a.Error != b.Error ||
		a.Badge != b.Badge ||
		a.Source != b.Source ||
		a.RuntimeType != b.RuntimeType ||
		a.Unsandboxed != b.Unsandboxed ||
		a.StorageUsed != b.StorageUsed ||
		a.StorageQuota != b.StorageQuota ||
		!slices.Equal(a.Permissions, b.Permissions) ||
		!slices.Equal(a.Granted, b.Granted) ||
		!slices.Equal(a.Denied, b.Denied) ||
		!slices.Equal(a.Asked, b.Asked) ||
		!slices.Equal(a.AllowedFolders, b.AllowedFolders) ||
		!slices.Equal(a.Views, b.Views) ||
		!slices.Equal(a.Commands, b.Commands) ||
		!toolsEqual(a.Tools, b.Tools) ||
		!settingsSummaryEqual(a.Settings, b.Settings) ||
		!webUIEqual(a.WebUI, b.WebUI) ||
		!a.I18nEqual(b) {
		return false
	}
	return true
}

func toolsEqual(a, b []ext.ToolInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Description != b[i].Description || !slices.Equal(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

func settingsSummaryEqual(a, b []ext.SettingSummary) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key ||
			a[i].Type != b[i].Type ||
			a[i].Label != b[i].Label ||
			a[i].Description != b[i].Description ||
			a[i].Set != b[i].Set ||
			a[i].Value != b[i].Value ||
			a[i].Default != b[i].Default ||
			!slices.Equal(a[i].Options, b[i].Options) {
			return false
		}
	}
	return true
}

func webUIEqual(a, b *ext.WebUIInfo) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
