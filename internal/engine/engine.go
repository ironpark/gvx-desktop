// Package engine is the app's own Docker Engine API, the experimental
// sandbox engine: it answers the docker CLI and Compose with no dockerd
// anywhere, and runs every container as a gvx sandbox of its own through a
// Runner, which is the supervisor and rungv. Images are rungv's image
// cache and volumes are rungv's volumes.
//
// See docs/design/sandbox-engine.md. Each file of the package belongs to
// one workstream; the comment at the top of a file names it.
package engine

// Owner: frozen (architect). Ask the coordinator before changing it.

import (
	"context"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// APIVersion is the Docker Engine API version the engine speaks, and
// MinAPIVersion the oldest it accepts. The docker CLI and Compose settle on
// the lower of theirs and APIVersion through /_ping.
const (
	APIVersion    = "1.52"
	MinAPIVersion = "1.24"
)

// Config is what an Engine runs with.
type Config struct {
	// Dir keeps the engine's store (containers.json, volumes.json,
	// networks.json), its container logs, and the supervisor's files.
	Dir string
	// Runner runs the sandboxes and drives rungv. NewRunner makes the real
	// one; the tests use a fake.
	Runner Runner
	// Version is the app's version: /version and /info answer
	// gvx-desktop-<Version>.
	Version string
	// Changed, when set, is called after the engine's state changed by
	// itself or by a request, from any goroutine; the app coalesces the
	// calls (dockerproxy.Proxy.NotifyChanged).
	Changed func()
	// KeepImages, when set, names images that image prune must keep besides
	// the ones containers use: those of the user's own sandboxes, which share
	// rungv's cache.
	KeepImages func() []string
	// BuilderSocketDir is where the lazy BuildKit front socket (buildkit.sock)
	// and backend socket (buildkitd.sock) live. Empty defaults to Dir (or a
	// short temporary directory if Dir would exceed the Unix sun_path limit).
	BuilderSocketDir string
	// BuilderIdleTimeout is how long the BuildKit builder sandbox stays running
	// after its last connection closes. Non-positive defaults to 10 minutes.
	BuilderIdleTimeout time.Duration
	// BuilderVolume names the rungv volume storing BuildKit's cache. Empty
	// defaults to BuilderVolumeName.
	BuilderVolume string
	// Amd64, when set, turns on the amd64 builder from the start; the app
	// changes it later with SetAmd64. See docs/design/amd64-builder.md.
	Amd64 *Amd64Config
	// MicroVM, when set, turns on the per-container micro-VM runtime from the
	// start; the app changes it later with SetMicroVM. See docs/design/amd64-microvm.md.
	MicroVM *MicroVMConfig
	// DockerSocket is the host Docker API Unix socket path (for example
	// ~/.gvx/docker.sock) when served on Unix. With rungv v0.21+
	// (--unix-forward), container bind mounts of /var/run/docker.sock are
	// forwarded to this host socket.
	DockerSocket string
	// SSHAuthSock, when set, returns the host SSH agent Unix socket path to
	// forward when a container bind-mounts /run/host-services/ssh-auth.sock
	// with rungv v0.21+ (--unix-forward). Returning "" disables forwarding.
	// When nil, the engine falls back to os.Getenv("SSH_AUTH_SOCK").
	SSHAuthSock func() string
	// AllowLANPorts, when set and returning true, allows published container
	// ports to bind non-loopback host addresses (such as 0.0.0.0, ::, or an
	// explicit host LAN IP). When nil or returning false, all published ports
	// bind 127.0.0.1 only.
	AllowLANPorts func() bool
	// BindSymlinks, when set and returning true, enables rungvx v0.26+
	// symlinks=contained on existing host directory bind mounts so safe
	// relative symlinks created by containers appear on macOS and Linux hosts.
	BindSymlinks func() bool
	// Logf, when set, traces every request and what the engine does.
	Logf func(format string, args ...any)
}

// Engine serves the Docker Engine API. Its methods are safe for concurrent
// use.
type Engine struct {
	cfg    Config
	runner Runner

	changeMu sync.Mutex
	changeCh chan struct{}

	openMu sync.Mutex
	opened bool

	// The state each workstream keeps, in a type its own file defines.
	ctr containerState // workstream B: containers.go
	str streamState    // workstream A: streams.go
	img imageState     // workstream C: images.go
	vol volumeState    // workstream C: volumes.go
	net networkState   // workstream C: networks.go
	evt eventState     // workstream C: events.go
	bld builderState   // builder.go
	amd amd64State     // amd64.go (the amd64 builder, workstream C)
	mvm microVMState   // microvm.go (per-container micro-VM runtime)
	ovl overlayState   // overlay.go (networks between containers)
	exc execState      // exec.go (rungv v0.20 exec instances)
	hlt healthState    // healthcheck.go (container healthcheck monitors)
}

// New returns an engine over cfg.Runner. It serves nothing until Open.
func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, runner: cfg.Runner, changeCh: make(chan struct{})}
	e.amd.cfg = cfg.Amd64
	e.mvm.cfg = cloneMicroVMConfig(cfg.MicroVM)
	e.runner.SetHandlers(e.onRunnerEvent, e.handleRunnerOutput)
	return e
}

// Open loads the store and brings the containers back as the restart
// policies say: one that ran when the engine last closed is exited now,
// and one that restarts always starts again. Opening twice does nothing.
func (e *Engine) Open(ctx context.Context) error {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	if e.opened {
		return nil
	}
	if err := e.loadVolumes(); err != nil {
		return err
	}
	if err := e.loadNetworks(); err != nil {
		return err
	}
	if err := e.loadContainers(); err != nil {
		return err
	}
	e.sweepRootfs()
	e.sweepTmp()
	e.reconcileEndpoints()
	e.openExecs()
	e.openHealthMonitors()
	e.openStreams()
	if err := e.openBuilder(); err != nil {
		e.closeStreams()
		return err
	}
	if err := e.openAmd64(); err != nil {
		e.closeBuilder(0)
		e.closeStreams()
		return err
	}
	e.openMicroVM()
	e.opened = true
	e.resumeContainers(ctx)
	e.notify()
	return nil
}

// Close stops every running container, waiting up to timeout for each, and
// ends the streams of the clients still attached. The store keeps the
// containers; Open brings them back.
func (e *Engine) Close(timeout time.Duration) error {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	if !e.opened {
		return nil
	}
	e.opened = false
	e.closeAmd64(timeout)
	e.closeBuilder(timeout)
	e.closeHealthMonitors()
	e.closeExecs()
	e.stopContainers(timeout)
	e.closeMicroVM(timeout)
	e.closeOverlay()
	e.closeStreams()
	e.notify()
	return nil
}

// Features are what the engine's rungv can do (docs/design/gvx-v020.md
// §5): with a zero Features (rungv v0.19) exec, cp, pause, top and update
// answer 501 as before. A frozen seam for the UI (W3); body W2's.
func (e *Engine) Features() rungv.Features {
	return e.runner.Features()
}

// CanControl reports whether the container ref runs with a control
// socket, so exec, cp, pause, top, update and guest stats work on it: the
// UI's Terminal tab and Pause action follow it. False when the engine is
// closed, ref is unknown or not running, or rungv is older than v0.20. A
// frozen seam for the UI (W3); body W2's.
func (e *Engine) CanControl(ref string) bool {
	if !e.isOpen() {
		return false
	}
	c, err := e.lookup(ref)
	if err != nil || c.State.Status != StatusRunning {
		return false
	}
	return e.ctrControl(c) != nil
}

// RunningCount is how many containers run or wait to restart while the
// engine is open: what the app counts when it asks before quitting.
func (e *Engine) RunningCount() int {
	if !e.isOpen() {
		return 0
	}
	n := 0
	for _, c := range e.allContainers() {
		if c.State.Running() || c.State.Status == StatusRestarting {
			n++
		}
	}
	return n
}

// StopRunning stops every running or restarting container in parallel, as
// docker stop does, so their restart policies leave them stopped; the
// engine stays open. Close is for quitting.
func (e *Engine) StopRunning(timeout time.Duration) {
	if !e.isOpen() {
		return
	}
	var ids []string
	for _, c := range e.allContainers() {
		if c.State.Running() || c.State.Status == StatusRestarting {
			ids = append(ids, c.ID)
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			_ = e.stopContainerByRef(context.Background(), id, "", timeout, true)
		})
	}
	wg.Wait()
	e.notify()
}

// VolumeOwnership names the volumes the engine made, and, by volume, the
// names of the containers that mount it. It reads the engine's state while
// it is open and its store files while it is closed, so the app can keep
// the engine's volumes safe in either mode.
func (e *Engine) VolumeOwnership() (owned map[string]bool, users map[string][]string) {
	owned = map[string]bool{}
	users = map[string][]string{}
	// Always protect the engine's BuildKit cache volume from UI volume pruning,
	// whether the sandbox engine is currently open or closed.
	bvol := e.builderVolumeName()
	owned[bvol] = true
	users[bvol] = []string{BuilderName}
	addUsers := func(c Container) {
		add := func(vol string) {
			if vol != "" && !slices.Contains(users[vol], c.Name) {
				users[vol] = append(users[vol], c.Name)
			}
		}
		for _, mt := range c.Mounts {
			if mt.Type == "volume" {
				add(mt.Name)
			}
		}
		for _, v := range c.AnonymousVolumes {
			add(v)
		}
	}
	if e.isOpen() {
		e.vol.mu.Lock()
		for name := range e.vol.byName {
			owned[name] = true
		}
		e.vol.mu.Unlock()
		for _, c := range e.allContainers() {
			addUsers(c)
		}
		return owned, users
	}
	if e.cfg.Dir == "" {
		return owned, users
	}
	var vf volumesFile
	if loadFile(e.path("volumes.json"), &vf) == nil {
		for _, v := range vf.Volumes {
			if v != nil && v.Name != "" {
				owned[v.Name] = true
			}
		}
	}
	var cf containerStoreFile
	if loadFile(e.path("containers.json"), &cf) == nil {
		for _, c := range cf.Containers {
			addUsers(c)
		}
	}
	return owned, users
}

// Handler is the engine's http.Handler, for dockerproxy.Proxy.SetLocal or
// a listener of its own. It answers 503 while the engine is not open.
func (e *Engine) Handler() http.Handler { return http.HandlerFunc(e.serveHTTP) }

func (e *Engine) isOpen() bool {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	return e.opened
}

// notify tells the waiters on changes, and Config.Changed, that the
// engine's state changed. Whoever changes a container, image, volume or
// network calls it once the change is in place, holding no lock.
func (e *Engine) notify() {
	e.changeMu.Lock()
	close(e.changeCh)
	e.changeCh = make(chan struct{})
	e.changeMu.Unlock()
	if e.cfg.Changed != nil {
		go e.cfg.Changed()
	}
}

// changes returns a channel that closes at the next notify. Take it before
// looking at the state, then wait on it:
//
//	for {
//		ch := e.changes()
//		if done() { return }
//		select { case <-ch: case <-ctx.Done(): return }
//	}
func (e *Engine) changes() <-chan struct{} {
	e.changeMu.Lock()
	defer e.changeMu.Unlock()
	return e.changeCh
}

func (e *Engine) logf(format string, args ...any) {
	if e.cfg.Logf != nil {
		e.cfg.Logf(format, args...)
	}
}

func (e *Engine) allowLANPorts() bool {
	return e.cfg.AllowLANPorts != nil && e.cfg.AllowLANPorts()
}

func (e *Engine) bindSymlinks() bool {
	return e.cfg.BindSymlinks != nil && e.cfg.BindSymlinks()
}

func (e *Engine) sshAuthSock() string {
	if e.cfg.SSHAuthSock != nil {
		return e.cfg.SSHAuthSock()
	}
	return os.Getenv("SSH_AUTH_SOCK")
}

// onRunnerEvent receives the runner's events: internal builder events are
// handled separately; container events run before stream events so waiters
// woken by streams see the updated container state.
func (e *Engine) onRunnerEvent(ev sandbox.Event) {
	if e.builderEvent(ev) {
		return
	}
	e.containerEvent(ev)
	e.streamsEvent(ev)
}

func (e *Engine) handleRunnerOutput(id string, o sandbox.Output) {
	if e.builderOutput(id, o) {
		return
	}
	e.onRunnerOutput(id, o)
}
