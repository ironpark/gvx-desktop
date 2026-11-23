package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// Status is where a sandbox is in its life.
type Status int

const (
	Stopped  Status = iota // never run, or stopped by the user
	Starting               // the process runs; the guest is not ready yet
	Running                // the guest is ready
	Stopping               // asked to stop
	Exited                 // the guest ended by itself with code 0
	Failed                 // the guest or rungv ended with an error
)

func (s Status) String() string {
	switch s {
	case Starting:
		return "Starting"
	case Running:
		return "Running"
	case Stopping:
		return "Stopping"
	case Exited:
		return "Exited"
	case Failed:
		return "Failed"
	}
	return "Stopped"
}

// Active reports whether a process is alive in this status.
func (s Status) Active() bool { return s == Starting || s == Running || s == Stopping }

// PublishedPort is a host endpoint rungv bound for the sandbox.
type PublishedPort struct {
	Host  string // 127.0.0.1:8080, or a socket path
	Guest int
	Proto string // tcp, udp
	// Source is what published it: static (-p at start), auto (rungv's
	// -auto-publish, as the guest listened on it), or dynamic (port add).
	Source string
}

// URL is an address a browser can open, or "" for a socket or UDP port.
func (p PublishedPort) URL() string {
	if p.Proto != "tcp" || strings.HasPrefix(p.Host, "/") || strings.Contains(p.Host, `\`) {
		return ""
	}
	host := p.Host
	if after, ok := strings.CutPrefix(host, "0.0.0.0:"); ok {
		host = "127.0.0.1:" + after
	}
	return "http://" + host
}

// State is a snapshot of a sandbox's runtime state.
type State struct {
	Status    Status
	PID       int
	StartedAt time.Time
	ReadyAt   time.Time
	ExitedAt  time.Time
	ExitCode  int
	Error     string // the cause of a failure
	Advice    string // what to do about it
	Platform  string // the engine rungv picked: hvf, whpx, systrap
	Ports     []PublishedPort
	Restored  string // the checkpoint bundle this run resumed, if any
	LogSeq    int64
	// Signal is the signal that ended the process, 0 when it exited by
	// itself; ExitCode is then -1.
	Signal int
	// Control reports a run with a control socket (rungv v0.20): exec,
	// cp, pause, stats, top, update and a graceful stop work on it
	// (Supervisor.Control). Owner: W1.
	Control bool
	// Paused reports a run whose guest Supervisor.Pause froze; Status
	// stays Running. Owner: W1.
	Paused bool
	// Busy are the host ports -auto-publish could not take, because
	// something on this computer has them, until the guest lets the port
	// go or it is published after all.
	Busy []PublishedPort
}

// Equal makes State comparable for signals.
func (s State) Equal(o State) bool {
	if !slices.Equal(s.Ports, o.Ports) || !slices.Equal(s.Busy, o.Busy) {
		return false
	}
	return s.Status == o.Status && s.PID == o.PID && s.StartedAt.Equal(o.StartedAt) &&
		s.ReadyAt.Equal(o.ReadyAt) && s.ExitedAt.Equal(o.ExitedAt) && s.ExitCode == o.ExitCode &&
		s.Error == o.Error && s.Advice == o.Advice && s.Platform == o.Platform &&
		s.Restored == o.Restored && s.LogSeq == o.LogSeq && s.Signal == o.Signal &&
		s.Control == o.Control && s.Paused == o.Paused
}

// Event is a change the supervisor reports.
type Event struct {
	ID    string
	Kind  EventKind
	State State
	Port  string // the host address rungv could not publish, for EventPortBusy
}

// EventKind classifies an Event.
type EventKind int

const (
	EventChanged  EventKind = iota // state or logs changed
	EventReady                     // the guest became ready
	EventExited                    // the process ended
	EventPortBusy                  // rungv could not publish a port: Port names it
)

// Supervisor runs sandboxes as rungv child processes. It is the daemon gvx
// does not have: it owns each process, keeps its output, and tells the UI
// what changed. Its methods are safe for concurrent use.
type Supervisor struct {
	// Client returns the rungv client to start processes with.
	Client func() *rungv.Client
	// DataDir holds control sockets and checkpoint bundles.
	DataDir string
	// Notify receives events from the supervisor's goroutines.
	Notify func(Event)
	// Prepare, when set, writes what a spec reads from this computer as it
	// starts, before rungv runs: the files a recipe gives its guest.
	Prepare func(Spec) error
	// StopGrace, when set, says which sandboxes Stop ends as docker stop
	// ends a container (Shutdown with SIGTERM) and how long they get,
	// given the stop timeout: those whose daemon keeps state. The others,
	// and runs without a control socket, end with an interrupt.
	StopGrace func(spec Spec, timeout time.Duration) (grace time.Duration, ok bool)
	// Output, when set, receives what the guest of every run writes, as it
	// comes and without rungv's own lines, from the goroutines reading it.
	// It must not block. The log buffers get the same output either way.
	Output func(id string, o Output)

	mu    sync.Mutex
	procs map[string]*proc
	logs  map[string]*LogBuffer
}

type proc struct {
	cmd         *exec.Cmd
	client      *rungv.Client
	state       State
	spec        Spec
	paths       Paths
	socketIsTCP bool
	done        chan struct{}
	stopping    bool
	interactive bool
	// portSync is set while syncPorts runs for the proc, portAgain when
	// it is to read the ports once more as it ends.
	portSync, portAgain bool

	stdinMu     sync.Mutex
	stdinW      *os.File
	stdinClosed bool
	term        *term
	closed      bool
}

func (p *proc) closeStdio() {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.stdinW != nil && !p.stdinClosed {
		p.stdinClosed = true
		_ = p.stdinW.Close()
	}
	if p.term != nil {
		_ = p.term.close()
	}
}

func (s *Supervisor) notify(e Event) {
	if s.Notify != nil {
		s.Notify(e)
	}
}

// Logs returns the log buffer of a sandbox, creating it.
func (s *Supervisor) Logs(id string) *LogBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logsLocked(id)
}

func (s *Supervisor) logsLocked(id string) *LogBuffer {
	if s.logs == nil {
		s.logs = map[string]*LogBuffer{}
	}
	b := s.logs[id]
	if b == nil {
		b = NewLogBuffer(5000)
		s.logs[id] = b
	}
	return b
}

// State returns the state of a sandbox.
func (s *Supervisor) State(id string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil {
		st := p.state
		st.Ports = append([]PublishedPort(nil), st.Ports...)
		st.Busy = append([]PublishedPort(nil), st.Busy...)
		st.LogSeq = s.logsLocked(id).Seq()
		return st
	}
	return State{LogSeq: s.logsLocked(id).Seq()}
}

// ActiveSpec returns a copy of the spec the active process for id started with,
// and false when no process for id is active.
func (s *Supervisor) ActiveSpec(id string) (Spec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil && p.state.Status.Active() {
		return p.spec.Clone(), true
	}
	return Spec{}, false
}

// Running lists the IDs of sandboxes with a live process.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, p := range s.procs {
		if p.state.Status.Active() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// SnapshotRoot is the directory holding the checkpoint bundles of a sandbox.
func (s *Supervisor) SnapshotRoot(id string) string {
	return filepath.Join(s.DataDir, "snapshots", id)
}

// maxSocketPath keeps a socket path inside sockaddr_un on every host.
const maxSocketPath = 100

// sockName returns the file stem for a sandbox's control socket: the ID
// directly when short (such as user 8-hex IDs), or the first 16 hex digits of
// sha256(id) for longer IDs (such as 64-hex engine container IDs).
func sockName(id string) string {
	if len(id) > 0 && len(id) <= 16 && !strings.ContainsAny(id, `/\:`) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func joinForOS(goos string, elem ...string) string {
	if goos == "windows" {
		parts := make([]string, 0, len(elem))
		for _, e := range elem {
			if e == "" {
				continue
			}
			e = strings.TrimRight(e, `\/`)
			if len(parts) > 0 {
				e = strings.TrimLeft(e, `\/`)
			}
			if e != "" {
				parts = append(parts, e)
			}
		}
		return strings.Join(parts, `\`)
	}
	return filepath.Join(elem...)
}

type pathsEnv struct {
	goos           string
	dataDir        string
	euid           int
	tempDir        string
	mkdir          func(dir string) error
	ensurePrivate  func(dir string) error
	removeExisting func(sock, token string)
}

func computePaths(env pathsEnv, spec Spec, feat rungv.Features, snapshotDir string) (Paths, error) {
	if !feat.Control && !spec.Checkpoint {
		return Paths{}, nil
	}
	var ckptDir string
	if spec.Checkpoint {
		ckptDir = snapshotDir
		if env.mkdir != nil {
			if err := env.mkdir(ckptDir); err != nil {
				return Paths{}, err
			}
		}
	}
	if !feat.Control && env.goos == "windows" {
		// v0.19 checkpoint on Windows uses loopback TCP.
		return Paths{ControlSocket: "127.0.0.1:0", CheckpointDir: ckptDir}, nil
	}

	name := sockName(spec.ID)
	var sock string
	if env.dataDir != "" {
		runDir := joinForOS(env.goos, env.dataDir, "run")
		cand := joinForOS(env.goos, runDir, name+".sock")
		if len(cand) <= maxSocketPath {
			if env.mkdir != nil {
				if err := env.mkdir(runDir); err != nil {
					return Paths{}, err
				}
			}
			sock = cand
		}
	}
	if sock == "" {
		var short string
		if env.goos == "windows" {
			short = joinForOS(env.goos, env.tempDir, "gvxd")
			if env.mkdir != nil {
				if err := env.mkdir(short); err != nil {
					return Paths{}, err
				}
			}
		} else {
			short = fmt.Sprintf("/tmp/gvxd-%d", env.euid)
			if env.ensurePrivate != nil {
				if err := env.ensurePrivate(short); err != nil {
					return Paths{}, err
				}
			}
		}
		sock = joinForOS(env.goos, short, name+".sock")
	}

	var tokenFile string
	if feat.Control && env.goos == "windows" {
		tokenFile = sock + ".token"
	}
	if env.removeExisting != nil {
		env.removeExisting(sock, tokenFile)
	}
	return Paths{
		ControlSocket: sock,
		TokenFile:     tokenFile,
		Control:       feat.Control,
		CheckpointDir: ckptDir,
	}, nil
}

// pathsFor allocates the control socket, optional token file, and (for
// Checkpoint specs) a fresh bundle directory.
func (s *Supervisor) pathsFor(spec Spec, feat rungv.Features) (Paths, error) {
	snapDir := ""
	if spec.Checkpoint {
		snapDir = filepath.Join(s.SnapshotRoot(spec.ID), time.Now().Format("20060102-150405"))
	}
	return computePaths(pathsEnv{
		goos:    runtime.GOOS,
		dataDir: s.DataDir,
		euid:    currentEUID(),
		tempDir: os.TempDir(),
		mkdir: func(dir string) error {
			return os.MkdirAll(dir, 0o700)
		},
		ensurePrivate: ensurePrivateSocketDir,
		removeExisting: func(sock, token string) {
			removeSocketFile(sock)
			removeTokenFile(token)
		},
	}, spec, feat, snapDir)
}

// Start runs the spec. It fails when the sandbox already runs.
func (s *Supervisor) Start(spec Spec) error {
	return s.launch(spec, "")
}

// Restore resumes the checkpoint bundle in dir under the spec's sandbox.
func (s *Supervisor) Restore(spec Spec, dir string) error {
	return s.launch(spec, dir)
}

func (s *Supervisor) launch(spec Spec, restoreDir string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	client := s.Client()
	if client == nil || client.Path == "" {
		return rungv.ErrNotFound
	}
	s.mu.Lock()
	if p := s.procs[spec.ID]; p != nil && p.state.Status.Active() {
		s.mu.Unlock()
		return fmt.Errorf("%s is already running", spec.Name)
	}
	if other := s.volumeHolderLocked(spec); other != "" {
		s.mu.Unlock()
		return fmt.Errorf("a volume of %s is in use by the running sandbox %s", spec.Name, other)
	}
	s.mu.Unlock()

	if err := spec.Prepare(); err != nil {
		return err
	}
	if s.Prepare != nil {
		if err := s.Prepare(spec); err != nil {
			return err
		}
	}
	feat := client.Features(context.Background())
	paths, err := s.pathsFor(spec, feat)
	if err != nil {
		return err
	}
	if HasAutoPublish(spec) && !client.HasRunFlag(context.Background(), "auto-publish") {
		// An older rungv would refuse the run: it starts without, and
		// reaches this computer through the ports it publishes.
		spec = WithAutoPublish(spec, false)
	}
	var args []string
	if restoreDir != "" {
		// The resumed sandbox keeps the bundle directory it was restored
		// from pre-opened, so its next snapshot rewrites that bundle.
		if paths.CheckpointDir != "" {
			_ = os.Remove(paths.CheckpointDir)
			paths.CheckpointDir = restoreDir
		}
		args = spec.RestoreArgs(restoreDir, paths)
	} else {
		args = spec.RunArgs(paths)
	}

	cmd := client.Command(context.Background(), args...)
	cmd.Env = os.Environ()
	// A pipe of its own rather than StderrPipe: on Windows a run with a
	// terminal is started without exec.Cmd.Start (startCmd).
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stderr = stderrW
	var (
		stdout          io.Reader
		stdinW          *os.File
		ptyTerm         *term
		closeAfterStart func()
		closeOnFail     func()
	)
	if spec.TTY {
		var closeSlave func()
		ptyTerm, stdout, closeSlave, err = openPTY(cmd)
		if err != nil {
			return err
		}
		closeAfterStart = closeSlave
		closeOnFail = func() {
			closeSlave()
			_ = ptyTerm.close()
		}
	} else {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return err
		}
		// The guest reads rungv's standard input; give it a pipe that
		// stays open so non-interactive shells and servers do not see EOF
		// and quit. Spec.Interactive exposes stdinW through Supervisor.Stdin.
		stdinR, w, err := os.Pipe()
		if err != nil {
			return err
		}
		stdinW = w
		cmd.Stdin = stdinR
		closeAfterStart = func() { _ = stdinR.Close() }
		closeOnFail = func() {
			_ = stdinR.Close()
			_ = stdinW.Close()
		}
	}

	logs := s.Logs(spec.ID)
	what := "Starting"
	if restoreDir != "" {
		what = "Restoring " + filepath.Base(restoreDir)
	}
	logs.Add(System, fmt.Sprintf("── %s %s · %s", what, spec.Name, time.Now().Format("2006-01-02 15:04:05")))
	logs.Add(System, "$ rungv "+rungv.FormatArgs(redactArgs(args)))

	if err := startCmd(context.Background(), cmd, ptyTerm); err != nil {
		closeOnFail()
		_ = stderr.Close()
		_ = stderrW.Close()
		logs.Add(System, "failed to start: "+err.Error())
		return err
	}
	closeAfterStart()
	_ = stderrW.Close()

	p := &proc{
		cmd:         cmd,
		client:      client,
		spec:        spec.Clone(),
		paths:       paths,
		socketIsTCP: paths.ControlSocket == "127.0.0.1:0",
		done:        make(chan struct{}),
		interactive: spec.Interactive,
		stdinW:      stdinW,
		term:        ptyTerm,
		state: State{
			Status:    Starting,
			PID:       cmd.Process.Pid,
			StartedAt: time.Now(),
			Restored:  restoreDir,
			Control:   paths.Control,
		},
	}
	if spec.ReadyPattern == "" {
		p.state.Status = Running
		p.state.ReadyAt = p.state.StartedAt
	}
	s.mu.Lock()
	if s.procs == nil {
		s.procs = map[string]*proc{}
	}
	s.procs[spec.ID] = p
	s.mu.Unlock()
	s.notify(Event{ID: spec.ID, Kind: EventChanged, State: s.State(spec.ID)})

	var wg sync.WaitGroup
	wg.Add(2)
	go s.pump(spec.ID, p, stdout, Stdout, &wg)
	go s.pump(spec.ID, p, stderr, Stderr, &wg)
	go func() {
		wg.Wait()
		_ = stderr.Close()
		err := waitCmd(cmd, ptyTerm)
		p.closeStdio()
		s.exited(spec.ID, p, err)
	}()
	return nil
}

// volumeHolderLocked names a running sandbox that mounts a volume of spec.
func (s *Supervisor) volumeHolderLocked(spec Spec) string {
	want := map[string]bool{}
	for _, v := range spec.Volumes() {
		want[v] = true
	}
	if len(want) == 0 {
		return ""
	}
	for id, p := range s.procs {
		if id == spec.ID || !p.state.Status.Active() {
			continue
		}
		for _, v := range p.spec.Volumes() {
			if want[v] {
				return p.spec.Name
			}
		}
	}
	return ""
}

func (s *Supervisor) pump(id string, p *proc, r io.Reader, stream Stream, wg *sync.WaitGroup) {
	defer wg.Done()
	logs := s.Logs(id)
	var split lineSplitter
	var filter rungvFilter
	emitLine := func(line string) {
		logs.Add(stream, line)
		ev := Event{ID: id, Kind: EventChanged}
		s.mu.Lock()
		if stream == Stderr {
			if busy, ok := p.state.portLine(line); ok {
				if busy != "" {
					ev.Kind, ev.Port = EventPortBusy, busy
				}
				if p.state.Status != Starting { // the sync at ready covers those
					s.syncPortsLocked(id, p)
				}
			} else if v, ok := strings.CutPrefix(line, "rungv: platform="); ok {
				p.state.Platform = strings.TrimSpace(v)
			}
		}
		if p.state.Status == Starting && p.spec.ReadyPattern != "" && strings.Contains(line, p.spec.ReadyPattern) {
			p.state.Status = Running
			p.state.ReadyAt = time.Now()
			ev.Kind = EventReady
			s.syncPortsLocked(id, p)
		}
		s.mu.Unlock()
		ev.State = s.State(id)
		s.notify(ev)
	}
	emitOutput := func(data []byte) {
		if s.Output == nil || len(data) == 0 {
			return
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		s.Output(id, Output{Stream: stream, Data: cp})
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if s.Output != nil {
				if stream == Stderr {
					emitOutput(filter.write(chunk))
				} else {
					emitOutput(chunk)
				}
			}
			split.write(chunk, emitLine)
		}
		if err != nil {
			// On Linux, reading the PTY master returns EIO once the slave closes.
			if stream == Stdout && p.term != nil && isPTYEIO(err) {
				break
			}
			break
		}
	}
	if s.Output != nil && stream == Stderr {
		emitOutput(filter.flush())
	}
	split.flush(emitLine)
}

func (s *Supervisor) exited(id string, p *proc, err error) {
	logs := s.Logs(id)
	s.mu.Lock()
	code := 0
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	p.state.ExitCode = code
	p.state.Signal = waitSignal(p.cmd.ProcessState)
	p.state.ExitedAt = time.Now()
	p.state.PID = 0
	p.state.Paused = false
	switch {
	case p.stopping:
		p.state.Status = Stopped
	case err == nil && code == 0:
		p.state.Status = Exited
	default:
		p.state.Status = Failed
		tail := logs.Tail(8, Stderr)
		msg := ""
		for _, t := range slices.Backward(tail) {
			if t := strings.TrimSpace(t); t != "" && !publishRe.MatchString(t) {
				msg = t
				break
			}
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		p.state.Error = msg
		p.state.Advice = rungv.Diagnose(strings.Join(tail, "\n"), code)
	}
	if p.paths.ControlSocket != "" && !p.socketIsTCP {
		removeSocketFile(p.paths.ControlSocket)
	}
	if p.paths.TokenFile != "" {
		removeTokenFile(p.paths.TokenFile)
	}
	// rungv opens the bundle files before the lockdown; drop the bundle
	// directory of a run that never wrote a snapshot into it.
	if p.paths.CheckpointDir != "" && p.state.Restored == "" && !bundleComplete(p.paths.CheckpointDir) {
		_ = os.RemoveAll(p.paths.CheckpointDir)
	}
	st := p.state
	close(p.done)
	s.mu.Unlock()

	switch st.Status {
	case Stopped:
		logs.Add(System, "── Stopped")
	case Exited:
		logs.Add(System, "── Exited with code 0")
	default:
		logs.Add(System, fmt.Sprintf("── Failed with code %d", st.ExitCode))
	}
	s.notify(Event{ID: id, Kind: EventExited, State: s.State(id)})
}

// Stop asks a sandbox to end with an interrupt, as Ctrl-C in a terminal
// does, and kills it when it has not ended after timeout. A sandbox
// StopGrace picks, with a control socket, ends as docker stop ends a
// container instead (Shutdown): its daemon gets SIGTERM and time to stop
// what it runs and save its state.
func (s *Supervisor) Stop(id string, timeout time.Duration) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil
	}
	if s.StopGrace != nil && p.state.Control && p.paths.ControlSocket != "" {
		if grace, ok := s.StopGrace(p.spec, timeout); ok {
			s.mu.Unlock()
			return s.Shutdown(id, "SIGTERM", grace)
		}
	}
	p.stopping = true
	p.state.Status = Stopping
	proc := p.cmd.Process
	done := p.done
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	s.Logs(id).Add(System, "── Stopping…")

	if runtime.GOOS == "windows" {
		_ = proc.Kill()
	} else if err := proc.Signal(os.Interrupt); err != nil {
		_ = proc.Kill()
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		_ = proc.Kill()
		<-done
		return nil
	}
}

// StopAll stops every running sandbox in parallel.
func (s *Supervisor) StopAll(timeout time.Duration) {
	var wg sync.WaitGroup
	for _, id := range s.Running() {
		wg.Go(func() {
			_ = s.Stop(id, timeout)
		})
	}
	wg.Wait()
}

// Forget drops what the supervisor keeps of a stopped sandbox.
func (s *Supervisor) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil && p.state.Status.Active() {
		return
	}
	delete(s.procs, id)
	delete(s.logs, id)
}

// ErrNoCheckpoint reports a sandbox started without checkpoint support.
var ErrNoCheckpoint = errors.New("the sandbox was started without snapshot support; enable it in the sandbox settings and restart")

// Checkpoint writes a snapshot of a running sandbox. Without leaveRunning
// the sandbox stops once the bundle is written. It returns the bundle
// directory.
func (s *Supervisor) Checkpoint(ctx context.Context, id string, leaveRunning bool) (string, error) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return "", errors.New("the sandbox is not running")
	}
	if p.paths.ControlSocket == "" || p.paths.CheckpointDir == "" {
		s.mu.Unlock()
		return "", ErrNoCheckpoint
	}
	if p.state.Paused {
		s.mu.Unlock()
		return "", errors.New("Resume the sandbox first")
	}
	paths := p.paths
	if !leaveRunning {
		p.stopping = true
	}
	s.mu.Unlock()
	s.Logs(id).Add(System, "── Writing snapshot to "+paths.CheckpointDir)
	target := rungv.Target{Socket: paths.ControlSocket, TokenFile: paths.TokenFile}
	err := s.Client().CheckpointTarget(ctx, target, paths.CheckpointDir, leaveRunning)
	if err != nil {
		s.mu.Lock()
		p.stopping = false
		s.mu.Unlock()
		s.Logs(id).Add(System, "snapshot failed: "+err.Error())
		return "", err
	}
	s.Logs(id).Add(System, "── Snapshot written")
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return paths.CheckpointDir, nil
}

// redactArgs returns a copy of args with sensitive values (GVX_NET=... and
// --token/-token values) replaced before logging a command line.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		a := out[i]
		switch {
		case a == "--token" || a == "-token":
			if i+1 < len(out) {
				out[i+1] = "<redacted>"
				i++
			}
		case strings.HasPrefix(a, "--token="):
			out[i] = "--token=<redacted>"
		case strings.HasPrefix(a, "-token="):
			out[i] = "-token=<redacted>"
		case a == "-e" || a == "--env" || a == "-env":
			if i+1 < len(out) && strings.HasPrefix(out[i+1], "GVX_NET=") && len(out[i+1]) > len("GVX_NET=") {
				out[i+1] = "GVX_NET=<redacted>"
				i++
			}
		case strings.HasPrefix(a, "-e=GVX_NET=") && len(a) > len("-e=GVX_NET="):
			out[i] = "-e=GVX_NET=<redacted>"
		case strings.HasPrefix(a, "--env=GVX_NET=") && len(a) > len("--env=GVX_NET="):
			out[i] = "--env=GVX_NET=<redacted>"
		case strings.HasPrefix(a, "-env=GVX_NET=") && len(a) > len("-env=GVX_NET="):
			out[i] = "-env=GVX_NET=<redacted>"
		case strings.HasPrefix(a, "GVX_NET=") && len(a) > len("GVX_NET="):
			out[i] = "GVX_NET=<redacted>"
		}
	}
	return out
}

// Snapshot is a checkpoint bundle on disk.
type Snapshot struct {
	SandboxID string
	Dir       string
	Name      string
	Time      time.Time
	Size      int64
}

// Snapshots lists the complete bundles of a sandbox, newest first.
func (s *Supervisor) Snapshots(id string) []Snapshot {
	root := s.SnapshotRoot(id)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !bundleComplete(dir) {
			continue
		}
		snap := Snapshot{SandboxID: id, Dir: dir, Name: e.Name()}
		if t, err := time.ParseInLocation("20060102-150405", e.Name(), time.Local); err == nil {
			snap.Time = t
		} else if info, err := e.Info(); err == nil {
			snap.Time = info.ModTime()
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				snap.Size += info.Size()
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// bundleComplete reports whether dir holds a written checkpoint bundle:
// rungv creates the files empty when the run starts, and fills them on a
// checkpoint.
func bundleComplete(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "manifest.json"))
	return err == nil && st.Size() > 0
}
