package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	// BuilderImage is the BuildKit image the sandbox engine runs for builds.
	// Pinned to v0.34.2 (runc 1.5-era): older releases such as v0.17.3 fail
	// every RUN step under gVisor with "BPF_PROG_ATTACH not supported" because
	// gVisor exposes cgroup2 without BPF device programs.
	BuilderImage = "moby/buildkit:v0.34.2"

	// BuilderName is the docker buildx builder instance the app manages.
	BuilderName = "gvx-desktop"

	// BuilderVolumeName is the default rungv volume storing BuildKit's cache.
	BuilderVolumeName = "gvx-buildkit"

	defaultBuilderIdleTimeout = 10 * time.Minute
	builderSandboxID          = "engine-buildkit"
	builderSandboxName        = "gvxd-buildkit"
	builderBuildctlPrefix     = "engine-buildctl-"
	builderGuestPort          = 1234
	builderReadyTimeout       = 60 * time.Second
	builderDialRetryTimeout   = 10 * time.Second
	builderStopTimeout        = 10 * time.Second
	builderMaxUnixSockPath    = 100
)

// Builder status values reported by Engine.BuilderState.
const (
	BuilderIdle     = "idle"
	BuilderStarting = "starting"
	BuilderRunning  = "running"
)

// BuilderState is the current lifecycle state of the engine's BuildKit builder.
type BuilderState struct {
	Status string // BuilderIdle, BuilderStarting, or BuilderRunning
	Err    string // last start error, cleared on next start attempt
}

// builderJob tracks one in-flight buildctl sandbox for legacy POST /build.
type builderJob struct {
	onOutput func(sandbox.Output)
	done     chan sandbox.State
}

// builderState owns the lazy BuildKit front socket, the buildkitd sandbox
// lifecycle, idle stop timer, and one-shot buildctl jobs.
//
// Lock order: startStopMu -> mu. Never call e.notify() while holding either.
type builderState struct {
	startStopMu sync.Mutex

	mu            sync.Mutex
	opened        bool
	closed        bool
	ctx           context.Context
	cancel        context.CancelFunc
	sockDir       string
	tempSockDir   string // created only when cfg.Dir is too long for sun_path
	frontSock     string // endpoint returned by BuilderSocket (Unix path or tcp://...)
	frontSockFile string // Unix socket file path to unlink on Close, empty for TCP
	backendSock   string
	ln            net.Listener
	tracked       map[net.Conn]struct{}

	starting  bool
	running   bool
	startCh   chan struct{}
	startErr  error
	lastErr   string
	readyCh   chan struct{}
	exitCh    chan struct{}
	exitErr   string
	active    int
	idleTimer *time.Timer

	jobs map[string]*builderJob
}

func (e *Engine) builderVolumeName() string {
	if v := strings.TrimSpace(e.cfg.BuilderVolume); v != "" {
		return v
	}
	return BuilderVolumeName
}

func (e *Engine) builderIdleTimeout() time.Duration {
	if e.cfg.BuilderIdleTimeout > 0 {
		return e.cfg.BuilderIdleTimeout
	}
	return defaultBuilderIdleTimeout
}

// BuilderSocket returns the front endpoint buildx connects to (a Unix socket
// path on macOS/Linux, or a tcp://127.0.0.1:<port> URL on Windows), or "" when
// the engine has no socket directory.
func (e *Engine) BuilderSocket() string {
	e.bld.mu.Lock()
	defer e.bld.mu.Unlock()
	if e.bld.frontSock != "" {
		return e.bld.frontSock
	}
	if e.cfg.BuilderSocketDir != "" {
		return defaultBuilderEndpoint(e.cfg.BuilderSocketDir)
	}
	if e.cfg.Dir != "" {
		return defaultBuilderEndpoint(e.cfg.Dir)
	}
	return ""
}

// BuilderState reports whether the BuildKit builder sandbox is idle, starting,
// or running, along with the last startup error if any.
func (e *Engine) BuilderState() BuilderState {
	e.bld.mu.Lock()
	defer e.bld.mu.Unlock()
	switch {
	case e.bld.starting:
		return BuilderState{Status: BuilderStarting}
	case e.bld.running:
		return BuilderState{Status: BuilderRunning}
	default:
		return BuilderState{Status: BuilderIdle, Err: e.bld.lastErr}
	}
}

// isBuilderImageProtected reports whether ref/id/tags refer to the builder's
// image while the builder is starting or running.
func (e *Engine) isBuilderImageProtected(id string, repoTags, repoDigests []string) bool {
	e.bld.mu.Lock()
	active := e.bld.starting || e.bld.running
	e.bld.mu.Unlock()
	if !active {
		return false
	}
	return imageMatchesRef(BuilderImage, id, repoTags, repoDigests)
}

// openBuilder binds the lazy front Unix socket and starts accepting
// connections without launching buildkitd yet.
func (e *Engine) openBuilder() error {
	dir := strings.TrimSpace(e.cfg.BuilderSocketDir)
	tempDir := ""
	if dir == "" {
		dir = strings.TrimSpace(e.cfg.Dir)
		if dir != "" && len(filepath.Join(dir, "buildkitd.sock")) > builderMaxUnixSockPath {
			// Unix socket paths are limited to ~104 bytes on macOS; fall back to
			// a short temp directory when cfg.Dir is long (e.g. Go test TempDir).
			short, err := os.MkdirTemp("", "gvxd-bk-")
			if err != nil {
				return err
			}
			dir = short
			tempDir = short
		}
	}
	if dir == "" {
		return nil
	}

	backendSock := filepath.Join(dir, "buildkitd.sock")
	ln, frontSock, frontSockFile, err := listenBuilderFront(dir)
	if err != nil {
		if tempDir != "" {
			_ = os.RemoveAll(tempDir)
		}
		return fmt.Errorf("listen builder socket: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.bld.mu.Lock()
	e.bld.opened = true
	e.bld.closed = false
	e.bld.ctx = ctx
	e.bld.cancel = cancel
	e.bld.sockDir = dir
	e.bld.tempSockDir = tempDir
	e.bld.frontSock = frontSock
	e.bld.frontSockFile = frontSockFile
	e.bld.backendSock = backendSock
	e.bld.ln = ln
	e.bld.tracked = map[net.Conn]struct{}{}
	e.bld.starting = false
	e.bld.running = false
	e.bld.startCh = nil
	e.bld.startErr = nil
	e.bld.lastErr = ""
	e.bld.active = 0
	if e.bld.jobs == nil {
		e.bld.jobs = map[string]*builderJob{}
	}
	e.bld.mu.Unlock()

	go e.acceptBuilder(ln)
	return nil
}

// closeBuilder tears down the front socket, closes active proxy connections,
// cancels the idle timer, and stops the buildkitd sandbox if active.
func (e *Engine) closeBuilder(timeout time.Duration) {
	e.bld.mu.Lock()
	if !e.bld.opened {
		e.bld.mu.Unlock()
		return
	}
	e.bld.opened = false
	e.bld.closed = true
	cancel := e.bld.cancel
	e.bld.cancel = nil
	if e.bld.idleTimer != nil {
		e.bld.idleTimer.Stop()
		e.bld.idleTimer = nil
	}
	ln := e.bld.ln
	e.bld.ln = nil
	frontSockFile := e.bld.frontSockFile
	e.bld.frontSock = ""
	e.bld.frontSockFile = ""
	backendSock := e.bld.backendSock
	tempDir := e.bld.tempSockDir
	e.bld.tempSockDir = ""
	conns := make([]net.Conn, 0, len(e.bld.tracked))
	for c := range e.bld.tracked {
		conns = append(conns, c)
	}
	clear(e.bld.tracked)
	jobIDs := make([]string, 0, len(e.bld.jobs))
	for id := range e.bld.jobs {
		jobIDs = append(jobIDs, id)
	}
	e.bld.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if frontSockFile != "" {
		_ = os.Remove(frontSockFile)
	}
	for _, c := range conns {
		_ = c.Close()
	}
	if timeout <= 0 {
		timeout = builderStopTimeout
	}
	for _, id := range jobIDs {
		_ = e.runner.Stop(id, timeout)
		e.runner.Forget(id)
	}

	e.bld.startStopMu.Lock()
	e.bld.mu.Lock()
	wasRunning := e.bld.running
	e.bld.running = false
	e.bld.starting = false
	e.bld.mu.Unlock()
	if wasRunning || e.runner.State(builderSandboxID).Status.Active() {
		_ = e.runner.Stop(builderSandboxID, timeout)
		e.runner.Forget(builderSandboxID)
	}
	if backendSock != "" {
		_ = os.Remove(backendSock)
	}
	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
	e.bld.startStopMu.Unlock()
}

func (e *Engine) acceptBuilder(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go e.serveBuilderConn(conn)
	}
}

func (e *Engine) trackBuilderConns(conns ...net.Conn) bool {
	e.bld.mu.Lock()
	closed := e.bld.closed
	if !closed {
		for _, c := range conns {
			e.bld.tracked[c] = struct{}{}
		}
	}
	e.bld.mu.Unlock()
	if closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	return true
}

func (e *Engine) untrackBuilderConns(conns ...net.Conn) {
	e.bld.mu.Lock()
	for _, c := range conns {
		delete(e.bld.tracked, c)
	}
	e.bld.mu.Unlock()
}

func (e *Engine) serveBuilderConn(clientConn net.Conn) {
	if !e.trackBuilderConns(clientConn) {
		return
	}
	defer func() {
		e.untrackBuilderConns(clientConn)
		_ = clientConn.Close()
	}()

	e.bld.mu.Lock()
	ctx := e.bld.ctx
	e.bld.mu.Unlock()
	if ctx == nil {
		return
	}

	backendSock, release, err := e.acquireBuilder(ctx)
	if err != nil {
		e.logf("engine: builder start failed: %v", err)
		return
	}
	defer release()

	backendConn, err := dialBuilderBackend(ctx, backendSock, builderDialRetryTimeout)
	if err != nil {
		e.logf("engine: dial builder backend %s: %v", backendSock, err)
		return
	}
	if !e.trackBuilderConns(backendConn) {
		return
	}
	defer func() {
		e.untrackBuilderConns(backendConn)
		_ = backendConn.Close()
	}()

	pipeHalfClose(clientConn, backendConn)
}

// acquireBuilder holds a reference on the builder so it stays running, lazily
// starting buildkitd on the first caller. Concurrent first callers share a
// single start attempt. The returned release func must be called once done.
func (e *Engine) acquireBuilder(ctx context.Context) (backendSock string, release func(), err error) {
	e.bld.mu.Lock()
	if e.bld.closed || !e.bld.opened {
		e.bld.mu.Unlock()
		return "", nil, net.ErrClosed
	}
	e.bld.active++
	if e.bld.idleTimer != nil {
		e.bld.idleTimer.Stop()
		e.bld.idleTimer = nil
	}
	if e.bld.running {
		sock := e.bld.backendSock
		e.bld.mu.Unlock()
		return sock, e.releaseBuilder, nil
	}
	var ch chan struct{}
	needStart := false
	if e.bld.starting {
		ch = e.bld.startCh
	} else {
		ch = make(chan struct{})
		e.bld.starting = true
		e.bld.startCh = ch
		e.bld.startErr = nil
		e.bld.lastErr = ""
		needStart = true
	}
	e.bld.mu.Unlock()

	if needStart {
		e.notify()
		go e.runBuilderStart(ch)
	}

	select {
	case <-ch:
	case <-ctx.Done():
		e.releaseBuilder()
		return "", nil, ctx.Err()
	}

	e.bld.mu.Lock()
	startErr := e.bld.startErr
	running := e.bld.running
	closed := e.bld.closed
	sock := e.bld.backendSock
	e.bld.mu.Unlock()

	if closed {
		e.releaseBuilder()
		return "", nil, net.ErrClosed
	}
	if startErr != nil {
		e.releaseBuilder()
		return "", nil, startErr
	}
	if !running {
		e.releaseBuilder()
		return "", nil, errors.New("builder stopped unexpectedly")
	}
	return sock, e.releaseBuilder, nil
}

func (e *Engine) releaseBuilder() {
	e.bld.mu.Lock()
	if e.bld.active > 0 {
		e.bld.active--
	}
	if e.bld.active == 0 && e.bld.running && !e.bld.closed {
		if e.bld.idleTimer != nil {
			e.bld.idleTimer.Stop()
		}
		e.bld.idleTimer = time.AfterFunc(e.builderIdleTimeout(), e.onBuilderIdle)
	}
	e.bld.mu.Unlock()
}

func (e *Engine) onBuilderIdle() {
	e.bld.startStopMu.Lock()
	e.bld.mu.Lock()
	if e.bld.active > 0 || !e.bld.running || e.bld.closed {
		e.bld.mu.Unlock()
		e.bld.startStopMu.Unlock()
		return
	}
	e.bld.running = false
	e.bld.idleTimer = nil
	backendSock := e.bld.backendSock
	e.bld.mu.Unlock()

	e.logf("engine: stopping idle buildkit builder")
	_ = e.runner.Stop(builderSandboxID, builderStopTimeout)
	e.runner.Forget(builderSandboxID)
	if backendSock != "" {
		_ = os.Remove(backendSock)
	}
	e.bld.startStopMu.Unlock()
	e.notify()
}

func (e *Engine) runBuilderStart(ch chan struct{}) {
	e.bld.startStopMu.Lock()

	e.bld.mu.Lock()
	closed := e.bld.closed
	ctx := e.bld.ctx
	backendSock := e.bld.backendSock
	e.bld.mu.Unlock()

	var err error
	if closed || ctx == nil {
		err = net.ErrClosed
	} else {
		err = e.startBuilderSandboxLocked(ctx, backendSock)
	}

	e.bld.mu.Lock()
	e.bld.starting = false
	e.bld.startErr = err
	if err == nil && !e.bld.closed {
		e.bld.running = true
		e.bld.lastErr = ""
		if e.bld.active == 0 {
			if e.bld.idleTimer != nil {
				e.bld.idleTimer.Stop()
			}
			e.bld.idleTimer = time.AfterFunc(e.builderIdleTimeout(), e.onBuilderIdle)
		}
	} else {
		e.bld.running = false
		if err != nil && !errors.Is(err, net.ErrClosed) {
			e.bld.lastErr = err.Error()
		}
	}
	close(ch)
	e.bld.mu.Unlock()

	e.bld.startStopMu.Unlock()
	e.notify()
}

// startBuilderSandboxLocked ensures the builder cache volume and image exist,
// starts buildkitd, and waits until it is ready to accept connections.
// Caller holds e.bld.startStopMu.
func (e *Engine) startBuilderSandboxLocked(ctx context.Context, backendSock string) error {
	volName := e.builderVolumeName()
	if err := e.ensureBuilderVolume(ctx, volName); err != nil {
		return fmt.Errorf("prepare builder volume: %w", err)
	}
	if err := e.ensureBuilderImage(ctx); err != nil {
		return fmt.Errorf("pull builder image %s: %w", BuilderImage, err)
	}
	if err := removeStaleBuilderSocket(backendSock); err != nil {
		return fmt.Errorf("prepare builder socket: %w", err)
	}

	readyCh := make(chan struct{})
	exitCh := make(chan struct{})
	e.bld.mu.Lock()
	e.bld.readyCh = readyCh
	e.bld.exitCh = exitCh
	e.bld.exitErr = ""
	e.bld.mu.Unlock()

	spec := sandbox.Spec{
		ID:           builderSandboxID,
		Name:         builderSandboxName,
		Image:        BuilderImage,
		Pull:         "never",
		Command:      rungv.FormatArgs([]string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", builderGuestPort)}),
		Ports:        []string{fmt.Sprintf("unix:%s:%d", backendSock, builderGuestPort)},
		Mounts:       []sandbox.Mount{{Type: sandbox.MountVolume, Source: volName, Target: "/var/lib/buildkit"}},
		ReadyPattern: "running server on",
	}
	if err := e.runner.Start(spec); err != nil {
		return err
	}

	// Fake runner marks ReadyAt synchronously in Start; real supervisor fires
	// EventReady once buildkitd logs "running server on".
	if st := e.runner.State(builderSandboxID); !st.ReadyAt.IsZero() {
		select {
		case <-readyCh:
		default:
			e.bld.mu.Lock()
			if e.bld.readyCh == readyCh {
				close(readyCh)
				e.bld.readyCh = nil
			}
			e.bld.mu.Unlock()
		}
	}

	timer := time.NewTimer(builderReadyTimeout)
	defer timer.Stop()

	select {
	case <-readyCh:
	case <-exitCh:
		e.bld.mu.Lock()
		msg := e.bld.exitErr
		e.bld.mu.Unlock()
		if msg == "" {
			msg = "builder exited before becoming ready"
		}
		return errors.New(msg)
	case <-ctx.Done():
		_ = e.runner.Stop(builderSandboxID, builderStopTimeout)
		e.runner.Forget(builderSandboxID)
		return ctx.Err()
	case <-timer.C:
		_ = e.runner.Stop(builderSandboxID, builderStopTimeout)
		e.runner.Forget(builderSandboxID)
		return errors.New("timed out waiting for buildkitd to start")
	}

	// Confirm the backend socket accepts connections before declaring ready.
	conn, err := dialBuilderBackend(ctx, backendSock, builderDialRetryTimeout)
	if err != nil {
		_ = e.runner.Stop(builderSandboxID, builderStopTimeout)
		e.runner.Forget(builderSandboxID)
		return fmt.Errorf("connect to buildkitd: %w", err)
	}
	_ = conn.Close()
	return nil
}

func (e *Engine) ensureBuilderVolume(ctx context.Context, name string) error {
	vols, err := e.runner.Volumes(ctx)
	if err == nil {
		for _, v := range vols {
			if v.Name == name {
				return nil
			}
		}
	}
	if err := e.runner.CreateVolume(ctx, name, "ext4lite"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		return err
	}
	return nil
}

func (e *Engine) ensureBuilderImage(ctx context.Context) error {
	if imgs, err := e.runner.InspectImages(ctx, BuilderImage); err == nil && len(imgs) > 0 {
		return nil
	}
	e.logf("engine: pulling %s for sandbox builder", BuilderImage)
	return e.runner.Pull(ctx, BuilderImage, "", func(line string) {
		e.logf("engine: pull %s: %s", BuilderImage, line)
	})
}

func dialBuilderBackend(ctx context.Context, sockPath string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		c, err := d.DialContext(ctx, "unix", sockPath)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// builderEvent handles supervisor events for the internal buildkitd sandbox
// and one-shot buildctl runs. Returns true when ev belonged to the builder so
// container and stream handlers ignore it.
func (e *Engine) builderEvent(ev sandbox.Event) bool {
	if ev.ID == builderSandboxID {
		notify := false
		e.bld.mu.Lock()
		switch ev.Kind {
		case sandbox.EventReady:
			if e.bld.readyCh != nil {
				close(e.bld.readyCh)
				e.bld.readyCh = nil
			}
		case sandbox.EventExited:
			if e.bld.running {
				e.bld.running = false
				notify = true
			}
			if e.bld.idleTimer != nil {
				e.bld.idleTimer.Stop()
				e.bld.idleTimer = nil
			}
			if e.bld.exitCh != nil {
				msg := ev.State.Error
				if msg == "" && ev.State.ExitCode != 0 {
					msg = fmt.Sprintf("buildkitd exited with code %d", ev.State.ExitCode)
				}
				e.bld.exitErr = msg
				close(e.bld.exitCh)
				e.bld.exitCh = nil
			}
		}
		e.bld.mu.Unlock()
		if notify {
			e.notify()
		}
		return true
	}

	if strings.HasPrefix(ev.ID, builderBuildctlPrefix) {
		if ev.Kind == sandbox.EventExited {
			e.bld.mu.Lock()
			job := e.bld.jobs[ev.ID]
			e.bld.mu.Unlock()
			if job != nil {
				select {
				case job.done <- ev.State:
				default:
				}
			}
		}
		return true
	}
	return false
}

// builderOutput routes guest stdout/stderr of internal builder sandboxes.
// Returns true when id is internal so container log files are not created.
func (e *Engine) builderOutput(id string, o sandbox.Output) bool {
	if id == builderSandboxID {
		return true
	}
	if strings.HasPrefix(id, builderBuildctlPrefix) {
		e.bld.mu.Lock()
		job := e.bld.jobs[id]
		e.bld.mu.Unlock()
		if job != nil && job.onOutput != nil {
			job.onOutput(o)
		}
		return true
	}
	return false
}

func (e *Engine) registerBuildJob(id string, onOutput func(sandbox.Output)) *builderJob {
	job := &builderJob{
		onOutput: onOutput,
		done:     make(chan sandbox.State, 1),
	}
	e.bld.mu.Lock()
	if e.bld.jobs == nil {
		e.bld.jobs = map[string]*builderJob{}
	}
	e.bld.jobs[id] = job
	e.bld.mu.Unlock()
	return job
}

func (e *Engine) unregisterBuildJob(id string) {
	e.bld.mu.Lock()
	delete(e.bld.jobs, id)
	e.bld.mu.Unlock()
}

// pipeHalfClose copies bytes bidirectionally between client and backend,
// propagating half-closes cleanly and waking if the client disconnects.
func pipeHalfClose(client, backend net.Conn) {
	closeWrite := func(c net.Conn) {
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		_ = c.Close()
	}

	backendDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(backend, client)
		if err != nil {
			_ = backend.Close()
			return
		}
		closeWrite(backend)
		if attachWaitPeerClose(client, backendDone) {
			_ = backend.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend)
		closeWrite(client)
		close(backendDone)
		_ = client.Close()
		_ = backend.Close()
	}()
	wg.Wait()
}
