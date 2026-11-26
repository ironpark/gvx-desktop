package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// SandboxEngineStatus tracks the in-app sandbox engine's startup lifecycle.
type SandboxEngineStatus struct {
	Opening bool
	Ready   bool
	Err     string
}

// sandboxEngine is the in-app Docker Engine API (*engine.Engine). Tests may
// substitute a fake to verify the switch's Open/Close/SetLocal lifecycle.
type sandboxEngine interface {
	Open(ctx context.Context) error
	Close(timeout time.Duration) error
	Handler() http.Handler
	RunningCount() int
	StopRunning(timeout time.Duration)
	VolumeOwnership() (owned map[string]bool, users map[string][]string)
	ContainerRuntimes() map[string]engine.ContainerRuntimeInfo
	BuilderSocket() string
	BuilderState() engine.BuilderState
	SetAmd64(cfg *engine.Amd64Config) error
	Amd64Socket() string
	Amd64State() engine.Amd64State
	ResetAmd64Disk(ctx context.Context) error
	SetMicroVM(cfg *engine.MicroVMConfig) error
	MicroVMState() engine.MicroVMState
	Features() rungv.Features
	CanControl(ref string) bool
}

// engineFeatures returns what the sandbox engine's rungv supports, subscribing
// to m.RungvFeatures so reactive callers update when rungv detection finishes.
// For the real *engine.Engine it never calls Engine.Features() on the UI thread
// (which would run `rungv run -help` if uncached), using m.RungvFeatures
// computed off-thread in DetectEngine instead.
func (m *Model) engineFeatures() rungv.Features {
	rf := m.RungvFeatures.Get()
	if m.sandboxEngine != nil {
		if _, real := m.sandboxEngine.(*engine.Engine); !real {
			return m.sandboxEngine.Features()
		}
	}
	return rf
}

// peekEngineFeatures is engineFeatures without subscribing to signals.
func (m *Model) peekEngineFeatures() rungv.Features {
	rf := ggui.Peek(m.RungvFeatures)
	if m.sandboxEngine != nil {
		if _, real := m.sandboxEngine.(*engine.Engine); !real {
			return m.sandboxEngine.Features()
		}
	}
	return rf
}

// canControlContainer reports whether container ref in the sandbox engine has
// an active control socket.
func (m *Model) canControlContainer(ref string) bool {
	if m.sandboxEngine == nil {
		return false
	}
	if _, real := m.sandboxEngine.(*engine.Engine); real && m.Offline {
		return ggui.Peek(m.RungvFeatures).Control
	}
	return m.sandboxEngine.CanControl(ref)
}

// initDockerProxy constructs the Model's Docker API proxy and in-app sandbox
// engine, and keeps atomic snapshots of the Docker Engine spec (for request-time
// bind-mount checks) and saved sandbox images (for engine image pruning). While
// the engine sandbox is active, the snapshot reflects the spec it started with;
// otherwise it falls back to the saved spec. Call it from NewModel after m.sup
// and m.Rows are initialized.
func (m *Model) initDockerProxy(dataDir string) {
	ggui.Watch(m.Rows, func(rows []Row) {
		m.syncEngineSpec(rows)
	})
	ggui.Watch(m.Specs, func(specs []sandbox.Spec) {
		m.syncKeepImages(specs)
	})
	m.syncKeepImages(ggui.Peek(m.Specs))
	m.dockerProxy = dockerproxy.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", recipes.DockerBackendSocket())
	})
	m.dockerProxy.Rewrite = m.rewriteContainerCreate
	m.dockerProxy.Check = m.checkContainerCreate
	m.dockerProxy.Changed = func() {
		m.post(func() {
			m.refreshEngineRunning()
			m.refreshEngineVolumes()
			m.refreshEngineContainerRuntimes()
			m.refreshBuilderState()
			m.refreshAmd64State()
			m.refreshMicroVMState()
			m.LoadDocker()
			m.LoadDockerImages()
			if ggui.Peek(m.Settings).SandboxEngine {
				m.LoadImages()
				m.LoadDisk()
			}
		})
	}
	engineDir := filepath.Join(dataDir, "engine")
	m.sandboxEngine = engine.New(engine.Config{
		Dir:              engineDir,
		Runner:           engine.NewRunner(m.Client, engineDir),
		Version:          AppVersion,
		Changed:          m.dockerProxy.NotifyChanged,
		KeepImages:       m.sandboxKeepImages,
		BuilderSocketDir: recipes.EngineSocketDir(),
		DockerSocket:     recipes.DockerSocket(),
		AllowLANPorts:    m.dockerExposeLAN.Load,
		SSHAuthSock:      m.sandboxSSHAuthSock,
		BindSymlinks:     m.dockerBindSymlinks.Load,
	})
	ggui.OnCleanup(m.CloseDockerProxy)
}

func (m *Model) sandboxSSHAuthSock() string {
	if !m.dockerSSHAgent.Load() {
		return ""
	}
	return os.Getenv("SSH_AUTH_SOCK")
}

func (m *Model) syncKeepImages(specs []sandbox.Spec) {
	imgs := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Image != "" {
			imgs = append(imgs, s.Image)
		}
	}
	m.keepImages.Store(&imgs)
}

func (m *Model) sandboxKeepImages() []string {
	var out []string
	seen := map[string]bool{}
	add := func(img string) {
		if img != "" && !seen[img] {
			seen[img] = true
			out = append(out, img)
		}
	}
	if p := m.keepImages.Load(); p != nil {
		for _, img := range *p {
			add(img)
		}
	}
	if mgr := m.extMgrPtr.Load(); mgr != nil {
		for _, img := range mgr.KeepImages() {
			add(img)
		}
	}
	return out
}

// OpenDocker opens the Docker page; in compatibility mode it also ensures the
// docker:dind sandbox is set up and started, while in sandbox-engine mode it
// retries opening the engine if a previous attempt failed.
func (m *Model) OpenDocker() {
	if ggui.Peek(m.Settings).SandboxEngine {
		if st := ggui.Peek(m.SandboxEngineStatus); st.Err != "" && !st.Opening {
			m.RetrySandboxEngine()
		}
		m.Go(PageDocker)
		return
	}
	m.CreateFromRecipe(recipes.Docker)
	m.Go(PageDocker)
}

// RetrySandboxEngine re-attempts binding the Docker API proxy (if it failed)
// and opening the in-app sandbox engine.
func (m *Model) RetrySandboxEngine() {
	if m.Offline || !ggui.Peek(m.Settings).SandboxEngine {
		return
	}
	if ggui.Peek(m.DockerProxyErr) != "" && m.dockerProxy != nil {
		m.bindDockerProxy(m.DockerAPI())
	}
	m.openSandboxEngine()
}

// SwitchSandboxEngine switches Docker engine mode from the UI, confirming first
// when switching to compatibility mode would stop running containers.
func (m *Model) SwitchSandboxEngine(on bool) {
	if ggui.Peek(m.Settings).SandboxEngine == on {
		return
	}
	if !on {
		if n := max(m.sandboxEngineRunningCount(), ggui.Peek(m.EngineRunning)); n > 0 {
			m.Confirm(
				tr("Switch to compatibility mode?"),
				trn(n,
					"%d running container in the sandbox engine will be stopped. Your images and volumes stay.",
					"%d running containers in the sandbox engine will be stopped. Your images and volumes stay."),
				tr("Stop and switch"),
				func() { m.SetSandboxEngine(false) },
			)
			return
		}
	}
	m.SetSandboxEngine(on)
}

// SetSandboxEngine turns the in-app sandbox engine on or off.
// Turning it on routes the Docker API proxy to the engine handler immediately
// (answering 503 until Open completes) and opens the engine asynchronously;
// turning it off routes the proxy back to the Docker Engine sandbox immediately
// and stops the engine's containers asynchronously.
func (m *Model) SetSandboxEngine(on bool) {
	if ggui.Peek(m.Settings).SandboxEngine == on {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = on })
	m.DockerVersion.Set(dockerapi.Version{})
	if m.Offline {
		return
	}
	if on {
		m.openSandboxEngine()
	} else {
		m.closeSandboxEngine()
	}
}

func (m *Model) openSandboxEngine() {
	if m.Offline || m.sandboxEngine == nil || m.dockerProxy == nil {
		return
	}
	m.sandboxEngineGen++
	gen := m.sandboxEngineGen
	eng := m.sandboxEngine
	set := ggui.Peek(m.Settings)
	run := m.runCLI
	sockPath := eng.BuilderSocket()
	dockerHost := m.dockerHost()
	var amd64Cfg *engine.Amd64Config
	if set.Amd64Builds {
		var err error
		if amd64Cfg, err = m.amd64Config(); err != nil {
			log.Printf("amd64 builder config: %v", err)
		}
	}
	var microVMCfg *engine.MicroVMConfig
	if set.MicroVMContainers {
		var err error
		if microVMCfg, err = m.microVMConfig(set); err != nil {
			log.Printf("micro-VM runtime config: %v", err)
		}
	}
	// Route traffic to the in-app engine right away so requests and LoadDocker
	// never hit the dind backend while Open is in flight; Handler answers 503
	// until Open finishes.
	m.SandboxEngineStatus.Set(SandboxEngineStatus{Opening: true})
	m.dockerProxy.SetLocal(eng.Handler())
	var (
		bxStatus         BuildxStatus
		bxChecked        bool
		bxPrevSave       string
		bxSavePrev       bool
		bxInstalledAlias bool
	)
	m.async(func(ctx context.Context) error {
		if err := eng.SetAmd64(amd64Cfg); err != nil {
			return err
		}
		if err := eng.SetMicroVM(microVMCfg); err != nil {
			return err
		}
		// Configure buildx before eng.Open binds buildkit.sock so buildx
		// inspect/create probes do not lazily launch buildkitd at startup.
		if sockPath != "" {
			bxChecked = true
			if cli, ok := findCLI("docker", set.DockerPath); !ok {
				bxStatus = BuildxStatus{Checked: true, NoCLI: true}
			} else {
				bxCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				bxStatus, bxPrevSave, bxSavePrev, bxInstalledAlias = configureBuildx(bxCtx, cli, run, sockPath, dockerHost, set.DockerBuilderBefore)
				cancel()
			}
		}
		if err := eng.Open(ctx); err != nil {
			return err
		}
		// If Open bound a different endpoint (e.g. Windows loopback TCP fell
		// back from the preferred port to an ephemeral port), reconcile buildx
		// in probeSafe mode so it updates the endpoint without waking buildkitd.
		if actualSock := eng.BuilderSocket(); actualSock != "" && actualSock != sockPath {
			if cli, ok := findCLI("docker", set.DockerPath); ok {
				bxChecked = true
				bxCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				st2, prev2, save2, inst2 := configureBuildxWithAmd64(
					bxCtx, cli, run, actualSock, dockerHost, set.DockerBuilderBefore,
					false, "", true,
				)
				cancel()
				bxStatus = st2
				if save2 && prev2 != "" && !bxSavePrev {
					bxPrevSave, bxSavePrev = prev2, true
				}
				if inst2 {
					bxInstalledAlias = true
				}
			}
		}
		return nil
	}, func(err error) {
		if (bxSavePrev && bxPrevSave != "") || bxInstalledAlias {
			m.UpdateSettings(func(s *store.Settings) {
				if bxSavePrev && bxPrevSave != "" {
					s.DockerBuilderBefore = bxPrevSave
				}
				if bxInstalledAlias {
					s.DockerBuilderAliasInstalled = true
				}
			})
		}
		if !ggui.Peek(m.Settings).SandboxEngine {
			// Setting was turned off while Open was in flight: make sure the
			// engine is closed if Open succeeded and undo any buildx changes.
			if err == nil {
				timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
				go func() { _ = eng.Close(timeout) }()
			}
			if bxInstalledAlias || (bxSavePrev && bxPrevSave != "") {
				m.syncSandboxBuilder(false)
			}
			if set.Amd64Builds {
				m.syncAmd64BuildxNode(false)
			}
			return
		}
		if m.sandboxEngineGen != gen {
			// Setting is still on under a newer open generation; do not close
			// the engine or surface stale errors.
			return
		}
		if err != nil {
			m.SandboxEngineStatus.Set(SandboxEngineStatus{Err: err.Error()})
			m.toastError(tr("Could not start the sandbox engine"), err)
			return
		}
		m.SandboxEngineStatus.Set(SandboxEngineStatus{Ready: true})
		if bxChecked {
			m.BuildxStatus.Set(bxStatus)
		}
		m.dockerProxy.SetLocal(eng.Handler())
		m.refreshEngineRunning()
		m.refreshEngineVolumes()
		m.refreshEngineContainerRuntimes()
		m.refreshBuilderState()
		m.refreshAmd64State()
		m.refreshMicroVMState()
		if ggui.Peek(m.Settings).Amd64Builds {
			m.syncAmd64BuildxNode(true)
		}
		m.LoadDocker()
		m.LoadDockerImages()
		m.LoadDockerNetworks()
	})
}

func (m *Model) closeSandboxEngine() {
	m.SandboxEngineStatus.Set(SandboxEngineStatus{})
	if m.dockerProxy != nil {
		m.dockerProxy.SetLocal(nil)
	}
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.sandboxEngineGen++
	eng := m.sandboxEngine
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	m.async(func(context.Context) error {
		return eng.Close(timeout)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not stop the sandbox engine"), err)
		}
		// Restore buildx after eng.Close removes buildkit.sock so buildx
		// inspect does not wake buildkitd while shutting down.
		if !ggui.Peek(m.Settings).SandboxEngine {
			hadAmd64Node := ggui.Peek(m.BuildxStatus).Amd64Node
			m.syncSandboxBuilder(false)
			if ggui.Peek(m.Settings).Amd64Builds || hadAmd64Node {
				m.syncAmd64BuildxNode(false)
			}
		}
		m.refreshEngineRunning()
		m.refreshEngineVolumes()
		if !ggui.Peek(m.Settings).SandboxEngine {
			m.EngineContainerRuntimes.Set(nil)
		} else {
			m.refreshEngineContainerRuntimes()
		}
		m.refreshBuilderState()
		m.refreshAmd64State()
		m.refreshMicroVMState()
		m.LoadDocker()
		m.LoadDockerImages()
		m.LoadDockerNetworks()
	})
}

// CloseSandboxEngine detaches the proxy from the in-app engine and stops all
// running engine containers synchronously. Called on app quit.
func (m *Model) CloseSandboxEngine(timeout time.Duration) {
	if m.dockerProxy != nil {
		m.dockerProxy.SetLocal(nil)
	}
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	_ = m.sandboxEngine.Close(timeout)
}

// StopSandboxEngineContainers stops all running containers in the in-app
// sandbox engine while keeping the engine open.
func (m *Model) StopSandboxEngineContainers(timeout time.Duration) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.sandboxEngine.StopRunning(timeout)
}

func (m *Model) sandboxEngineRunningCount() int {
	if m.Offline || m.sandboxEngine == nil {
		return 0
	}
	return m.sandboxEngine.RunningCount()
}

func (m *Model) refreshEngineRunning() {
	m.EngineRunning.Set(m.sandboxEngineRunningCount())
}

func (m *Model) refreshBuilderState() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.BuilderState.Set(m.sandboxEngine.BuilderState())
}

// ActiveCount reports the total number of running user sandboxes plus running
// experimental sandbox-engine containers.
func (m *Model) ActiveCount() int {
	return len(m.sup.Running()) + m.sandboxEngineRunningCount()
}

func (m *Model) sandboxEngineVolumeOwnership() (map[string]bool, map[string][]string) {
	if m.sandboxEngine == nil {
		return nil, nil
	}
	return m.sandboxEngine.VolumeOwnership()
}

func (m *Model) refreshEngineVolumes() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	eng := m.sandboxEngine
	var users map[string][]string
	m.async(func(context.Context) error {
		_, users = eng.VolumeOwnership()
		return nil
	}, func(error) {
		m.EngineVolumeUsers.Set(users)
	})
}

func (m *Model) refreshEngineContainerRuntimes() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	if !ggui.Peek(m.Settings).SandboxEngine {
		m.EngineContainerRuntimes.Set(nil)
		return
	}
	eng := m.sandboxEngine
	var runtimes map[string]engine.ContainerRuntimeInfo
	m.async(func(context.Context) error {
		runtimes = eng.ContainerRuntimes()
		return nil
	}, func(error) {
		if !ggui.Peek(m.Settings).SandboxEngine {
			m.EngineContainerRuntimes.Set(nil)
			return
		}
		m.EngineContainerRuntimes.Set(runtimes)
	})
}

func (m *Model) syncEngineSpec(rows []Row) {
	for _, r := range rows {
		if r.Spec.Recipe != recipes.Docker {
			continue
		}
		live := r.State.Status.Active() && (r.State.BindAttach || r.State.LiveMounts)
		if m.sup != nil {
			if st := m.sup.State(r.Spec.ID); st.Status.Active() {
				live = st.BindAttach || st.LiveMounts
			}
			if active, ok := m.sup.ActiveSpec(r.Spec.ID); ok {
				m.engineSpec.Store(&active)
				m.engineLiveMounts.Store(live)
				return
			}
		}
		c := r.Spec.Clone()
		m.engineSpec.Store(&c)
		m.engineLiveMounts.Store(live)
		return
	}
	m.engineSpec.Store(nil)
	m.engineLiveMounts.Store(false)
}

// startDockerProxy binds the Docker API proxy listener for the configured
// endpoint. It is skipped when Offline is set. When the experimental sandbox
// engine setting is on, the proxy routes to its handler immediately before
// listening so requests never leak to the dind backend while Open finishes.
func (m *Model) startDockerProxy() {
	if m.Offline || m.dockerProxy == nil || m.dockerProxyStarted {
		return
	}
	m.dockerProxyStarted = true
	if ggui.Peek(m.Settings).SandboxEngine && m.sandboxEngine != nil {
		m.dockerProxy.SetLocal(m.sandboxEngine.Handler())
	}
	m.bindDockerProxy(m.DockerAPI())
}

// bindDockerProxy binds or moves the Docker API proxy listener to api and
// records any listener error in DockerProxyErr and a warning toast.
func (m *Model) bindDockerProxy(api recipes.DockerAPI) {
	if m.Offline || m.dockerProxy == nil {
		return
	}
	network, addr := api.ListenAddr()
	if err := m.dockerProxy.Listen(network, addr); err != nil {
		m.DockerProxyErr.Set(err.Error())
		m.Toasts.Push(ui.Toast(tr("Could not listen for Docker"), err.Error()).Warning().Duration(8 * time.Second))
		return
	}
	m.DockerProxyErr.Set("")
}

// CloseDockerProxy stops the Docker API proxy listener and closes active
// connections.
func (m *Model) CloseDockerProxy() {
	if m.dockerProxy != nil {
		_ = m.dockerProxy.Close()
	}
}

func (m *Model) checkContainerCreate(_ *http.Request, body []byte) error {
	sp := m.engineSpec.Load()
	if sp == nil {
		return nil
	}
	spec := *sp
	live := m.engineLiveMounts.Load()
	if m.sup != nil {
		if active, ok := m.sup.ActiveSpec(spec.ID); ok {
			spec = active
			st := m.sup.State(spec.ID)
			live = st.BindAttach || st.LiveMounts
		}
	}
	return checkCreateBindsLive(body, spec, live)
}

// checkCreateBinds rejects a POST /containers/create body that bind-mounts a
// host path outside the folders shared with engine. Unparsable bodies pass
// through untouched so dockerd returns its own error.
func checkCreateBinds(body []byte, engine sandbox.Spec) error {
	return checkCreateBindsLive(body, engine, false)
}

func checkCreateBindsLive(body []byte, engine sandbox.Spec, liveMounts bool) error {
	var req struct {
		HostConfig *struct {
			Binds  []string `json:"Binds"`
			Mounts []struct {
				Type   string `json:"Type"`
				Source string `json:"Source"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.HostConfig == nil {
		return nil
	}
	for _, b := range req.HostConfig.Binds {
		src, _, ok := strings.Cut(b, ":")
		if !ok || !strings.HasPrefix(src, "/") {
			continue
		}
		if recipes.UnsharedBind(src, engine) {
			return unsharedBindError(src, liveMounts)
		}
	}
	for _, m := range req.HostConfig.Mounts {
		if !strings.EqualFold(m.Type, "bind") || !strings.HasPrefix(m.Source, "/") {
			continue
		}
		if recipes.UnsharedBind(m.Source, engine) {
			return unsharedBindError(m.Source, liveMounts)
		}
	}
	return nil
}

func unsharedBindError(path string, liveMounts bool) error {
	if liveMounts {
		return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker and run this again.", path)
	}
	return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again.", path)
}

// rewriteContainerCreate changes a container's creation on its way to the
// engine: on Windows the folders docker names the Windows way are named as
// the engine has them.
func (m *Model) rewriteContainerCreate(_ *http.Request, body []byte) []byte {
	if runtime.GOOS != "windows" {
		return nil
	}
	return windowsBinds(body)
}

// windowsSource splits a bind at the colon after its source, C:\\src:/dst
// into C:\\src and :/dst, the drive's colon aside; whether the source is a
// Windows folder is recipes.WindowsGuestPath's to say.
func windowsSource(bind string) (source, rest string) {
	if len(bind) > 2 {
		if i := strings.IndexByte(bind[2:], ':'); i >= 0 {
			return bind[:2+i], bind[2+i:]
		}
	}
	return bind, ""
}

// windowsBinds returns a POST /containers/create body with the bind mount
// sources written the Windows way, C:\Users\me\src, as the engine has
// them, /c/Users/me/src; nil when there is none, or the body is not what
// it should be, which is dockerd's to say.
func windowsBinds(body []byte) []byte {
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // a memory limit must come out as it went in
	if err := dec.Decode(&req); err != nil {
		return nil
	}
	host, _ := req["HostConfig"].(map[string]any)
	if host == nil {
		return nil
	}
	changed := false
	if binds, _ := host["Binds"].([]any); binds != nil {
		for i, b := range binds {
			s, _ := b.(string)
			src, rest := windowsSource(s)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				binds[i], changed = g+rest, true
			}
		}
	}
	if mounts, _ := host["Mounts"].([]any); mounts != nil {
		for _, mt := range mounts {
			mount, _ := mt.(map[string]any)
			if typ, _ := mount["Type"].(string); !strings.EqualFold(typ, "bind") {
				continue
			}
			src, _ := mount["Source"].(string)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				mount["Source"], changed = g, true
			}
		}
	}
	if !changed {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}
