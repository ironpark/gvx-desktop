package rungv

// Owner: W1 (rungv + sandbox layer). Design: docs/design/gvx-v020.md §3.
//
// The runtime commands of rungv v0.20 (exec, cp, export, pause, unpause,
// ping, status, stats, top, kill, stop, update, port, events), run as
// subprocesses against a sandbox's control socket, as the app drives rungv
// everywhere else. The signatures and types in this file are frozen seams:
// the engine (W2) and the UI (W3) code against them. Bodies are W1's.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotImplemented is what the skeleton's stubs return until W1 lands.
var ErrNotImplemented = errors.New("not implemented")

// Target addresses the control socket of one running sandbox.
type Target struct {
	// Socket is the -S endpoint: a Unix socket path (also on Windows), or
	// a loopback TCP address.
	Socket string
	// TokenFile is passed as --token-file; "" when the socket needs no
	// token (POSIX Unix sockets authenticate the peer's user ID).
	TokenFile string
}

// Valid reports whether t names a socket.
func (t Target) Valid() bool { return t.Socket != "" }

// Features are what a rungv binary can do beyond v0.19, read from its run
// command's usage (HasRunFlag), so once for each binary.
type Features struct {
	// Control: run takes --control-token-file, so -S/-control-socket
	// serves the gvx-ctrl/2 protocol, and the runtime commands of this
	// file exist (rungv v0.20 and later).
	Control bool
	// DynamicPorts: run takes --dynamic-ports, so `rungv port add` works
	// on a locked-down sandbox started with it.
	DynamicPorts bool
	// KeepBlobs: run/pull/load take -keep-blobs; also gates v0.21 `rungv push`
	// and offline `rungv save`.
	KeepBlobs bool
	// UnixForward: run/restore take --unix-forward.
	UnixForward bool
	// BindAttach: run/restore take --allow-bind-attach for live host bind mounts.
	BindAttach bool
	// VolumeAttach: run/restore take --allow-volume-attach for live POSIX
	// SCM_RIGHTS ext4lite volume attachment under lockdown.
	VolumeAttach bool
	// VolumeOptions: --mount type=volume supports size= and ephemeral (rungvx v0.22+).
	VolumeOptions bool
	// PidsLimit: run takes --pids-limit for root cgroup2 pids.max (rungvx v0.20+).
	PidsLimit bool
	// GPU: run takes -gpu (off|venus|virgl) and -gpu-memory-limit (rungvx v0.28+ -tags ext_gpu).
	GPU bool
	// DynamicPortAddrs: run/restore take --dynamic-ports-addr.
	DynamicPortAddrs bool
	// AutoPublish: run takes -auto-publish.
	AutoPublish bool
	// AutoPublishAddr: run/restore take --auto-publish-addr.
	AutoPublishAddr bool
	// NetQoS: run/restore/update take --net-tx-bps and --net-rx-bps.
	NetQoS bool
	// NATPolicy: run/restore take --nat-policy-file and `rungv net-policy` exists.
	NATPolicy bool
	// RestoreLiveMounts: restore takes --attach-bind and --serve to reattach
	// checkpointed dynamic bind and rpcfs mounts.
	RestoreLiveMounts bool
	// VolumeDirFlag: run/restore/volume/info/system/mount add take -volume-dir
	// (rungvx v0.24+), which overrides -cache-dir and GVX_VOLUME_DIR.
	VolumeDirFlag bool
	// BindSymlinks: --mount type=bind supports symlinks=virtual|contained
	// (rungvx v0.26+).
	BindSymlinks bool
	// CheckpointStream: checkpoint takes -o (gvx-ctrl/2 streamed snapshots
	// without a pre-opened -checkpoint-dir).
	CheckpointStream bool
}

// Features reports what this rungv binary can do. A zero Features (v0.19,
// or a binary that does not answer) means: behave exactly as today.
func (c *Client) Features(ctx context.Context) Features {
	if c == nil || c.Path == "" {
		return Features{}
	}
	runUsage, ok := commandUsage(ctx, &runFlags, "run", c.Path)
	if !ok {
		return Features{}
	}
	hasRunFlag := func(name string) bool { return usageHasFlag(runUsage, name) }
	f := Features{
		Control:          hasRunFlag("control-token-file"),
		DynamicPorts:     hasRunFlag("dynamic-ports"),
		KeepBlobs:        hasRunFlag("keep-blobs"),
		UnixForward:      hasRunFlag("unix-forward"),
		BindAttach:       hasRunFlag("allow-bind-attach"),
		VolumeAttach:     hasRunFlag("allow-volume-attach"),
		VolumeOptions:    strings.Contains(runUsage, "ephemeral"),
		PidsLimit:        hasRunFlag("pids-limit"),
		GPU:              hasRunFlag("gpu"),
		DynamicPortAddrs: hasRunFlag("dynamic-ports-addr"),
		AutoPublish:      hasRunFlag("auto-publish"),
		AutoPublishAddr:  hasRunFlag("auto-publish-addr"),
		NetQoS:           hasRunFlag("net-tx-bps"),
		NATPolicy:        hasRunFlag("nat-policy-file"),
		VolumeDirFlag:    hasRunFlag("volume-dir"),
		BindSymlinks:     strings.Contains(runUsage, "symlinks="),
	}
	if f.Control && f.KeepBlobs {
		if restoreUsage, ok := commandUsage(ctx, &restoreFlags, "restore", c.Path); ok {
			f.RestoreLiveMounts = usageHasFlag(restoreUsage, "attach-bind") && usageHasFlag(restoreUsage, "serve")
		}
	}
	if f.Control {
		f.CheckpointStream = c.HasCheckpointFlag(ctx, "o")
	}
	return f
}

// ForgetFeatures drops what is cached of the binary at path (its run,
// restore, and checkpoint usage), after an install or a source build replaced it.
func ForgetFeatures(path string) {
	if path != "" {
		runFlags.Delete(path)
		restoreFlags.Delete(path)
		checkpointFlags.Delete(path)
	}
}

func (c *Client) checkTarget(t Target) error {
	if c == nil || c.Path == "" {
		return ErrNotFound
	}
	if !t.Valid() {
		return errors.New("rungvx: control target has no socket")
	}
	return nil
}

// targetArgs starts the arguments of a runtime command: its words, as
// "port ls" (a command group takes its subcommand before any flag), then
// the flags that name the sandbox.
func targetArgs(cmd string, t Target) []string {
	args := make([]string, 0, 8)
	args = append(args, strings.Fields(cmd)...)
	if t.Socket != "" {
		args = append(args, "-S", t.Socket)
	}
	if t.TokenFile != "" {
		args = append(args, "--token-file", t.TokenFile)
	}
	return args
}

func cmdExitError(args []string, stderr string, err error) *Error {
	e := &Error{Args: args, Stderr: stderr, Err: err}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		e.Code = exit.ExitCode()
	}
	return e
}

// ---- exec ---------------------------------------------------------------

// ExecOptions are the flags of rungv exec.
type ExecOptions struct {
	Argv    []string // the command and its arguments; required
	Env     []string // KEY=VALUE entries (-e); never a bare KEY, which would copy the app's environment
	User    string   // -u USER[:GROUP]; "" is rungv's default
	Workdir string   // -w
	// Interactive (-i) forwards stdin; without it the guest's stdin is
	// closed, as docker exec does.
	Interactive bool
	// TTY (-t) gives the command a terminal in the guest. With
	// Interactive, ExecArgs also passes --detach-keys= (off): the caller
	// handles detaching, if at all.
	TTY bool
	// Detach (-d) starts the command and returns at once; rungv prints the
	// guest PID. Not with Interactive or TTY.
	Detach bool
	// Privileged, when set, passes --privileged=<v>; nil keeps rungv's
	// default (true).
	Privileged *bool
	// PIDFile, when set, is --pid-file: rungv writes the guest PID there
	// once the command started.
	PIDFile string
}

// ExecArgs is the argument vector of rungv exec against t, without the
// rungv executable: "exec -S <sock> [--token-file F] [flags] -- argv...".
func (c *Client) ExecArgs(t Target, o ExecOptions) []string {
	args := targetArgs("exec", t)
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Interactive && o.TTY {
		args = append(args, "--detach-keys=")
	}
	if o.Detach {
		args = append(args, "-d")
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	if o.Workdir != "" {
		args = append(args, "-w", o.Workdir)
	}
	for _, e := range o.Env {
		if k, _, ok := strings.Cut(e, "="); ok && k != "" {
			args = append(args, "-e", e)
		}
	}
	if o.Privileged != nil {
		if *o.Privileged {
			args = append(args, "--privileged=true")
		} else {
			args = append(args, "--privileged=false")
		}
	}
	if o.PIDFile != "" {
		args = append(args, "--pid-file", o.PIDFile)
	}
	args = append(args, "--")
	args = append(args, o.Argv...)
	return args
}

// ExecCommand builds the exec.Cmd of rungv exec; the caller wires its
// stdio (a pipe, or a PTY slave for TTY) and starts it.
func (c *Client) ExecCommand(ctx context.Context, t Target, o ExecOptions) *exec.Cmd {
	return c.Command(ctx, c.ExecArgs(t, o)...)
}

// ---- cp / export --------------------------------------------------------

// CopyOptions are the flags of rungv cp.
type CopyOptions struct {
	Archive        bool   // -a: keep mode, ownership and times (docker cp -a, copyUIDGID=1)
	FollowLink     bool   // -L
	NoOverwriteDir bool   // --no-overwrite-dir (docker noOverwriteDirNonDir)
	User           string // --user UID[:GID] for host-to-guest copies without Archive
}

type streamReadCloser struct {
	parentCtx context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	args      []string
	stdout    io.ReadCloser
	reader    io.Reader
	stderr    *bytes.Buffer

	mu        sync.Mutex
	eof       bool
	closeOnce sync.Once
	closeErr  error
}

func (s *streamReadCloser) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if errors.Is(err, io.EOF) {
		s.mu.Lock()
		s.eof = true
		s.mu.Unlock()
	}
	return n, err
}

func (s *streamReadCloser) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		eof := s.eof
		s.mu.Unlock()

		aborted := !eof
		if aborted {
			_ = s.stdout.Close()
			s.cancel()
		}
		waitErr := s.cmd.Wait()
		s.cancel()

		var err error
		if waitErr != nil {
			if aborted && s.parentCtx.Err() == nil && strings.TrimSpace(s.stderr.String()) == "" {
				// Killed by an intentional early Close (e.g., StatPath after reading
				// the first tar header) without any rungv error on stderr.
				err = nil
			} else if s.parentCtx.Err() != nil && strings.TrimSpace(s.stderr.String()) == "" {
				err = s.parentCtx.Err()
			} else {
				err = cmdExitError(s.args, s.stderr.String(), waitErr)
			}
		}
		s.closeErr = err
	})
	return s.closeErr
}

func (c *Client) startTarStream(ctx context.Context, args []string) (io.ReadCloser, error) {
	childCtx, cancel := context.WithCancel(ctx)
	cmd := c.Command(childCtx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	// Wait for the first byte or an immediate failure (e.g., NOT_FOUND,
	// ALREADY_PAUSED, UNAUTHORIZED) so callers get the error from CopyFrom /
	// Export directly instead of an empty tar stream.
	var first [512]byte
	n, rerr := stdout.Read(first[:])
	if n == 0 {
		waitErr := cmd.Wait()
		cancel()
		if waitErr != nil {
			if ctx.Err() != nil && strings.TrimSpace(stderr.String()) == "" {
				return nil, ctx.Err()
			}
			return nil, cmdExitError(args, stderr.String(), waitErr)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, rerr
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	peek := make([]byte, n)
	copy(peek, first[:n])
	reader := io.MultiReader(bytes.NewReader(peek), stdout)
	eof := errors.Is(rerr, io.EOF)
	if eof {
		reader = bytes.NewReader(peek)
	}
	return &streamReadCloser{
		parentCtx: ctx,
		cancel:    cancel,
		cmd:       cmd,
		args:      args,
		stdout:    stdout,
		reader:    reader,
		stderr:    &stderr,
		eof:       eof,
	}, nil
}

// CopyFrom streams guestPath out of the sandbox as a tar archive whose
// first entry is the path's base name (rungv cp :PATH -), as docker's
// GET /archive does. Closing the reader ends the copy; Close returns the
// command's error (an *Error, NOT_FOUND for a missing path).
func (c *Client) CopyFrom(ctx context.Context, t Target, guestPath string, o CopyOptions) (io.ReadCloser, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	args := targetArgs("cp", t)
	if o.Archive {
		args = append(args, "-a")
	}
	if o.FollowLink {
		args = append(args, "-L")
	}
	args = append(args, ":"+guestPath, "-")
	return c.startTarStream(ctx, args)
}

// CopyTo extracts the tar archive read from r into the guest directory
// guestDir (rungv cp - :DIR), as docker's PUT /archive does.
func (c *Client) CopyTo(ctx context.Context, t Target, guestDir string, r io.Reader, o CopyOptions) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	args := targetArgs("cp", t)
	if o.Archive {
		args = append(args, "-a")
	}
	if o.FollowLink {
		args = append(args, "-L")
	}
	if o.NoOverwriteDir {
		args = append(args, "--no-overwrite-dir")
	}
	if o.User != "" {
		args = append(args, "--user", o.User)
	}
	args = append(args, "-", ":"+guestDir)
	cmd := c.Command(ctx, args...)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return cmdExitError(args, stderr.String(), err)
	}
	return nil
}

// PathStat is a guest path as docker's X-Docker-Container-Path-Stat header
// describes it.
type PathStat struct {
	Name       string
	Size       int64
	Mode       uint32 // Go os.FileMode bits, as docker sends them
	Mtime      time.Time
	LinkTarget string
}

// StatPath describes guestPath: the first header of CopyFrom's archive,
// read and then abandoned (rungv has no stat command).
func (c *Client) StatPath(ctx context.Context, t Target, guestPath string) (PathStat, error) {
	if err := c.checkTarget(t); err != nil {
		return PathStat{}, err
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	if cleaned == "/" {
		// Exporting "/" omits a root header and would walk the rootfs; Ping
		// verifies the sandbox answers without starting a tar stream.
		if _, err := c.Ping(ctx, t, 0); err != nil {
			return PathStat{}, err
		}
		return PathStat{
			Name: "/",
			Mode: uint32(os.ModeDir | 0o755),
		}, nil
	}
	rc, err := c.CopyFrom(ctx, t, cleaned, CopyOptions{})
	if err != nil {
		return PathStat{}, err
	}
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	cerr := rc.Close()
	if err != nil {
		if cerr != nil {
			return PathStat{}, cerr
		}
		return PathStat{}, err
	}
	name := strings.TrimSuffix(hdr.Name, "/")
	if name == "" {
		name = path.Base(cleaned)
	}
	return PathStat{
		Name:       name,
		Size:       hdr.Size,
		Mode:       uint32(hdr.FileInfo().Mode()),
		Mtime:      hdr.ModTime,
		LinkTarget: hdr.Linkname,
	}, nil
}

// Export streams the guest's filesystem under guestPath ("" for /) as a
// tar archive (rungv export -o -). upperOnly keeps only what the run
// changed (--upper-only), whiteouts included.
func (c *Client) Export(ctx context.Context, t Target, guestPath string, upperOnly bool) (io.ReadCloser, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	args := targetArgs("export", t)
	args = append(args, "-o", "-")
	if upperOnly {
		args = append(args, "--upper-only")
	}
	if guestPath != "" {
		args = append(args, guestPath)
	}
	return c.startTarStream(ctx, args)
}

// ---- lifecycle ----------------------------------------------------------

// Pause freezes the guest (rungv pause; host forwarders quiesce too).
func (c *Client) Pause(ctx context.Context, t Target) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	_, err := c.output(ctx, targetArgs("pause", t)...)
	return err
}

// Unpause thaws a paused guest.
func (c *Client) Unpause(ctx context.Context, t Target) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	_, err := c.output(ctx, targetArgs("unpause", t)...)
	return err
}

// KillOptions are the flags of rungv kill.
type KillOptions struct {
	Signal string // name (SIGTERM, TERM) or number; "" is SIGKILL
	PID    int    // guest PID; 0 is the guest's first process
	All    bool   // every guest process
}

// Kill sends a signal inside the guest.
func (c *Client) Kill(ctx context.Context, t Target, o KillOptions) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	args := targetArgs("kill", t)
	if o.Signal != "" {
		args = append(args, "-s", o.Signal)
	}
	if o.PID > 0 {
		args = append(args, "--pid", strconv.Itoa(o.PID))
	}
	if o.All {
		args = append(args, "--all")
	}
	_, err := c.output(ctx, args...)
	return err
}

// StopGuest sends signal ("" is SIGTERM) to the guest's first process and
// SIGKILL after grace (rungv stop -s -t). The sandbox ends when that
// process does; StopGuest returns when rungv stop does.
func (c *Client) StopGuest(ctx context.Context, t Target, signal string, grace time.Duration) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	if grace < 0 {
		grace = 0
	}
	args := targetArgs("stop", t)
	if signal != "" {
		args = append(args, "-s", signal)
	}
	args = append(args, "-t", grace.String())
	_, err := c.output(ctx, args...)
	return err
}

// ---- inspection ---------------------------------------------------------

// Ping is rungv ping -f json.
type Ping struct {
	Version      int      `json:"version"`
	SandboxID    string   `json:"sandbox_id"`
	State        string   `json:"state"` // starting, running, paused, ...
	UptimeNanos  int64    `json:"uptime_ns"`
	Capabilities []string `json:"capabilities"`
}

// Has reports whether the sandbox grants capability name (such as "exec",
// "exec.tty", "port.dynamic_loopback").
func (p Ping) Has(name string) bool {
	return slices.Contains(p.Capabilities, name)
}

// Ping asks the sandbox for its state and capabilities, retrying to
// connect for up to wait (0: once).
func (c *Client) Ping(ctx context.Context, t Target, wait time.Duration) (Ping, error) {
	if err := c.checkTarget(t); err != nil {
		return Ping{}, err
	}
	args := targetArgs("ping", t)
	args = append(args, "-f", "json")
	if wait > 0 {
		args = append(args, "--wait", wait.String())
	}
	out, err := c.output(ctx, args...)
	if err != nil {
		return Ping{}, err
	}
	var p Ping
	if err := json.Unmarshal(out, &p); err != nil {
		return Ping{}, err
	}
	return p, nil
}

// Resources is the resource snapshot rungv update and status report.
type Resources struct {
	MemoryBytes   int64  `json:"memory_bytes"`
	CPUMillis     int64  `json:"cpu_millis"`
	CPUQuotaUSec  int64  `json:"cpu_quota_usec"`
	CPUPeriodUSec int64  `json:"cpu_period_usec"`
	GuestCPUs     int    `json:"guest_cpus"`
	PidsLimit     int64  `json:"pids_limit"`
	IsolateLAN    bool   `json:"isolate_lan"`
	NetTXBPS      uint64 `json:"net_tx_bps"`
	NetRXBPS      uint64 `json:"net_rx_bps"`
}

// PortRule is one host port of the sandbox (rungv port ls / status).
type PortRule struct {
	Proto      string `json:"proto"`
	HostIP     string `json:"host_ip,omitempty"`
	HostPort   uint16 `json:"host_port,omitempty"`
	HostSocket string `json:"host_socket,omitempty"`
	GuestIP    string `json:"guest_ip"`
	GuestPort  uint16 `json:"guest_port"`
	Source     string `json:"source"` // static, auto, dynamic
}

// Ports lists the host ports the sandbox has now, whatever published
// them (rungv port ls -f json): -p at start, -auto-publish, or port add.
func (c *Client) Ports(ctx context.Context, t Target) ([]PortRule, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	out, err := c.output(ctx, append(targetArgs("port ls", t), "-f", "json")...)
	if err != nil {
		return nil, err
	}
	return decodeLines[PortRule](out)
}

// AddPorts dynamically publishes one or more host-to-guest port rules on a
// running sandbox (rungv port add -S ... [--replace] -f json -- SPEC...).
func (c *Client) AddPorts(ctx context.Context, t Target, specs []string, replace bool) ([]PortRule, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, errors.New("rungvx port add: no port specs specified")
	}
	cleaned := make([]string, 0, len(specs))
	for _, spec := range specs {
		s := strings.TrimSpace(spec)
		if s == "" {
			return nil, errors.New("rungvx port add: empty port spec")
		}
		if err := checkNoLeadingDash("port spec", s); err != nil {
			return nil, err
		}
		cleaned = append(cleaned, s)
	}
	args := targetArgs("port add", t)
	if replace {
		args = append(args, "--replace")
	}
	args = append(args, "-f", "json", "--")
	args = append(args, cleaned...)
	out, err := c.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	return decodeLines[PortRule](out)
}

// RemovePorts closes one or more published port rules on a running sandbox
// (rungv port rm -S ... [--force] -- SPEC...).
func (c *Client) RemovePorts(ctx context.Context, t Target, specs []string, force bool) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	if len(specs) == 0 {
		return errors.New("rungvx port rm: no port specs specified")
	}
	cleaned := make([]string, 0, len(specs))
	for _, spec := range specs {
		s := strings.TrimSpace(spec)
		if s == "" {
			return errors.New("rungvx port rm: empty port spec")
		}
		if err := checkNoLeadingDash("port spec", s); err != nil {
			return err
		}
		cleaned = append(cleaned, s)
	}
	args := targetArgs("port rm", t)
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--")
	args = append(args, cleaned...)
	_, err := c.output(ctx, args...)
	return err
}

// MountInfo is one active guest mount entry (rungv mount ls -f json).
type MountInfo struct {
	Target   string `json:"target"`
	Type     string `json:"type"`   // tmpfs, hostfs, ext4lite, overlay, rpcfs, erofs
	Source   string `json:"source"` // volume name, host path, or label
	ReadOnly bool   `json:"read_only"`
	Mutable  bool   `json:"mutable"` // whether host backing allows remount rw
}

// MountAddOptions are the flags of rungv mount add.
type MountAddOptions struct {
	Type     string // tmpfs, volume, or bind
	Source   string // host path, named volume, or .ext4 image path
	Target   string // guest mount destination path
	ReadOnly bool   // --ro
	Size     string // --size (for tmpfs, e.g. 64m)
}

// Mounts lists active mounts inside the running sandbox (rungv mount ls -f json).
func (c *Client) Mounts(ctx context.Context, t Target) ([]MountInfo, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	out, err := c.output(ctx, append(targetArgs("mount ls", t), "-f", "json")...)
	if err != nil {
		return nil, err
	}
	return decodeLines[MountInfo](out)
}

// AddMount attaches a new filesystem inside a running sandbox (rungv mount add).
func (c *Client) AddMount(ctx context.Context, t Target, o MountAddOptions) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(o.Type))
	dst := strings.TrimSpace(o.Target)
	src := strings.TrimSpace(o.Source)
	size := strings.TrimSpace(o.Size)
	if kind == "" {
		return errors.New("rungvx mount add: mount type is required")
	}
	if dst == "" {
		return errors.New("rungvx mount add: target path is required")
	}
	if err := checkNoLeadingDash("mount type", kind); err != nil {
		return err
	}
	if err := checkNoLeadingDash("mount target", dst); err != nil {
		return err
	}
	if src != "" {
		if err := checkNoLeadingDash("mount source", src); err != nil {
			return err
		}
	}
	if size != "" {
		if err := checkNoLeadingDash("mount size", size); err != nil {
			return err
		}
	}
	args := targetArgs("mount add", t)
	args = append(args, "--type", kind)
	if src != "" {
		args = append(args, "--src", src)
	}
	args = append(args, "--dst", dst)
	if o.ReadOnly {
		args = append(args, "--ro")
	}
	if size != "" {
		args = append(args, "--size", size)
	}
	if kind == "volume" {
		if c.CacheDir != "" {
			args = append(args, "-cache-dir", c.CacheDir)
		}
		if c.VolumeDir != "" && c.HasRunFlag(ctx, "volume-dir") {
			args = append(args, "-volume-dir", c.VolumeDir)
		}
	}
	_, err := c.output(ctx, args...)
	return err
}

// Remount toggles an existing managed mount point between read-only and
// read-write inside a running sandbox (rungvx mount remount).
func (c *Client) Remount(ctx context.Context, t Target, target string, readOnly bool) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	dst := strings.TrimSpace(target)
	if dst == "" {
		return errors.New("rungvx mount remount: target path is required")
	}
	if err := checkNoLeadingDash("mount target", dst); err != nil {
		return err
	}
	args := targetArgs("mount remount", t)
	if readOnly {
		args = append(args, "--ro")
	} else {
		args = append(args, "--rw")
	}
	args = append(args, "--", dst)
	_, err := c.output(ctx, args...)
	return err
}

// RemoveMount unmounts target inside a running sandbox (rungv mount rm).
// When lazy is true, --detach (MNT_DETACH) is passed.
func (c *Client) RemoveMount(ctx context.Context, t Target, target string, lazy bool) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	dst := strings.TrimSpace(target)
	if dst == "" {
		return errors.New("rungvx mount rm: target path is required")
	}
	if err := checkNoLeadingDash("mount target", dst); err != nil {
		return err
	}
	args := targetArgs("mount rm", t)
	if lazy {
		args = append(args, "--detach")
	}
	args = append(args, "--", dst)
	_, err := c.output(ctx, args...)
	return err
}

// MountServeArgs is the argument vector of rungv mount serve against t,
// without the rungv executable:
// "mount serve -S <sock> [--token-file F] [--ro] -- HOST_PATH:GUEST_PATH".
func (c *Client) MountServeArgs(t Target, hostPath, guestTarget string, readOnly bool) []string {
	args := targetArgs("mount serve", t)
	if readOnly {
		args = append(args, "--ro")
	}
	args = append(args, "--", strings.TrimSpace(hostPath)+":"+strings.TrimSpace(guestTarget))
	return args
}

// MountServeCommand builds the long-lived exec.Cmd of rungv mount serve
// (rpcfs proxy mount) against t. The caller manages its lifecycle.
func (c *Client) MountServeCommand(ctx context.Context, t Target, hostPath, guestTarget string, readOnly bool) *exec.Cmd {
	return c.Command(ctx, c.MountServeArgs(t, hostPath, guestTarget, readOnly)...)
}

// UnixForwardArgs is the argument vector of rungv unix-forward against t,
// without the rungv executable:
// "unix-forward -S <sock> [--token-file F] [--replace] -- SPEC...".
func (c *Client) UnixForwardArgs(t Target, specs []string, replace bool) []string {
	args := targetArgs("unix-forward", t)
	if replace {
		args = append(args, "--replace")
	}
	cleaned := make([]string, 0, len(specs))
	for _, spec := range specs {
		if s := strings.TrimSpace(spec); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) > 0 {
		args = append(args, "--")
		args = append(args, cleaned...)
	}
	return args
}

// UnixForwardCommand builds the long-lived exec.Cmd of rungv unix-forward
// against t. The caller manages its lifecycle.
func (c *Client) UnixForwardCommand(ctx context.Context, t Target, specs []string, replace bool) *exec.Cmd {
	return c.Command(ctx, c.UnixForwardArgs(t, specs, replace)...)
}

// NATPolicy reads the active host NAT firewall policy JSON of a running sandbox
// (rungv net-policy get). Returns "null" when no policy is installed.
func (c *Client) NATPolicy(ctx context.Context, t Target) ([]byte, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	return c.output(ctx, targetArgs("net-policy get", t)...)
}

// ReplaceNATPolicy replaces the host NAT firewall policy of a running sandbox
// from policyFile (.json, .yaml, or .yml) via rungv net-policy replace --file.
func (c *Client) ReplaceNATPolicy(ctx context.Context, t Target, policyFile string) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	f := strings.TrimSpace(policyFile)
	if f == "" {
		return errors.New("rungvx net-policy replace: policy file is required")
	}
	if err := checkNoLeadingDash("policy file", f); err != nil {
		return err
	}
	args := append(targetArgs("net-policy replace", t), "--file", f)
	_, err := c.output(ctx, args...)
	return err
}

// Status is the part of rungv status -f json the app reads.
type Status struct {
	ID                 string     `json:"id"`
	State              string     `json:"state"`
	Platform           string     `json:"platform"`
	LockdownApplied    bool       `json:"lockdown_applied"`
	Resources          Resources  `json:"resources"`
	MemoryUsedBytes    uint64     `json:"memory_used_bytes"`
	Ports              []PortRule `json:"ports,omitempty"`
	ActiveExecSessions int        `json:"active_exec_sessions"`
	PIDCount           int        `json:"pid_count"`
	Capabilities       []string   `json:"capabilities"`
	StartedAt          time.Time  `json:"started_at"`
}

// Status runs rungv status -f json.
func (c *Client) Status(ctx context.Context, t Target) (Status, error) {
	if err := c.checkTarget(t); err != nil {
		return Status{}, err
	}
	args := append(targetArgs("status", t), "-f", "json")
	out, err := c.output(ctx, args...)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Stats is one sample of rungv stats -f json: the guest's own counters.
type Stats struct {
	Timestamp time.Time `json:"timestamp"`
	CPU       struct {
		UserNanos      int64  `json:"user_ns"`
		SystemNanos    int64  `json:"sys_ns"`
		Periods        uint64 `json:"periods,omitempty"`
		Throttled      uint64 `json:"throttled,omitempty"`
		ThrottledNanos int64  `json:"throttled_ns,omitempty"`
	} `json:"cpu"`
	Memory struct {
		TotalBytes uint64 `json:"total_bytes"`
		AnonBytes  uint64 `json:"anon_bytes"`
		FileBytes  uint64 `json:"file_bytes"`
		TmpfsBytes uint64 `json:"tmpfs_bytes"`
		LimitBytes uint64 `json:"limit_bytes"`
	} `json:"memory"`
	Network []struct {
		Interface string `json:"interface"`
		RxBytes   uint64 `json:"rx_bytes"`
		TxBytes   uint64 `json:"tx_bytes"`
		RxPackets uint64 `json:"rx_packets"`
		TxPackets uint64 `json:"tx_packets"`
	} `json:"network,omitempty"`
	BlockIO []struct {
		Disk         string `json:"disk"`
		ReadOps      uint64 `json:"read_ops"`
		WriteOps     uint64 `json:"write_ops"`
		ReadSectors  uint64 `json:"read_sectors"`
		WriteSectors uint64 `json:"write_sectors"`
	} `json:"block_io,omitempty"`
	PIDs      int   `json:"pids"`
	PIDsLimit int64 `json:"pids_limit"`
}

// CPUNanos is the guest's CPU time, user and system.
func (s Stats) CPUNanos() int64 { return s.CPU.UserNanos + s.CPU.SystemNanos }

// Stats takes one sample (rungv stats --no-stream -f json).
func (c *Client) Stats(ctx context.Context, t Target) (Stats, error) {
	if err := c.checkTarget(t); err != nil {
		return Stats{}, err
	}
	args := append(targetArgs("stats", t), "--no-stream", "-f", "json")
	out, err := c.output(ctx, args...)
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return Stats{}, err
	}
	return s, nil
}

func streamNDJSON[T any](ctx context.Context, c *Client, args []string, fn func(T)) error {
	cmd := c.Command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var decodeErr error
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var item T
		if err := json.Unmarshal(line, &item); err != nil {
			if decodeErr == nil {
				decodeErr = err
			}
			continue
		}
		if fn != nil {
			fn(item)
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if waitErr != nil {
		if ctx.Err() != nil && strings.TrimSpace(stderr.String()) == "" {
			return ctx.Err()
		}
		return cmdExitError(args, stderr.String(), waitErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanErr != nil {
		return scanErr
	}
	return decodeErr
}

// StreamStats hands a sample to fn every interval from one long-lived
// rungv stats -f json, until ctx ends or the sandbox does.
func (c *Client) StreamStats(ctx context.Context, t Target, interval time.Duration, fn func(Stats)) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	args := append(targetArgs("stats", t), "-f", "json")
	if interval > 0 {
		args = append(args, "--interval", interval.String())
	}
	return streamNDJSON(ctx, c, args, fn)
}

// Process is one guest process of rungv top -f json.
type Process struct {
	PID      int32    `json:"pid"`
	PPID     int32    `json:"ppid"`
	UID      uint32   `json:"uid"`
	GID      uint32   `json:"gid"`
	State    string   `json:"state"`
	Threads  int      `json:"threads"`
	CPUNanos int64    `json:"cpu_ns"`
	RSSBytes uint64   `json:"rss_bytes"`
	Comm     string   `json:"comm"`
	Cmdline  []string `json:"cmdline,omitempty"`
}

// Top lists the guest's processes.
func (c *Client) Top(ctx context.Context, t Target) ([]Process, error) {
	if err := c.checkTarget(t); err != nil {
		return nil, err
	}
	args := append(targetArgs("top", t), "-f", "json")
	out, err := c.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	return decodeLines[Process](out)
}

// ---- update -------------------------------------------------------------

// Update is what rungv update changes; a zero field is left alone.
type Update struct {
	Memory      string  // -m: 512m, 2g, or "0" for unlimited; rungv's units
	ForceMemory bool    // --force-memory: allow going below current use
	CPUs        string  // --cpus: 1.5, or "0" for unlimited
	CPUQuota    int64   // --cpu-quota USEC (-1 unlimited); with CPUPeriod, not with CPUs
	CPUPeriod   int64   // --cpu-period USEC
	PidsLimit   *int64  // --pids-limit (0 or -1 unlimited)
	IsolateLAN  *bool   // --isolate-lan=<v>
	NetTXBPS    *uint64 // --net-tx-bps BPS (0 for unlimited)
	NetRXBPS    *uint64 // --net-rx-bps BPS (0 for unlimited)
}

// IsZero reports whether u changes nothing.
func (u Update) IsZero() bool {
	return u.Memory == "" && u.CPUs == "" && u.CPUQuota == 0 && u.CPUPeriod == 0 &&
		u.PidsLimit == nil && u.IsolateLAN == nil && u.NetTXBPS == nil && u.NetRXBPS == nil
}

// Update applies u to the running sandbox and returns the resulting
// resources (rungv update -f json).
func (c *Client) Update(ctx context.Context, t Target, u Update) (Resources, error) {
	if err := c.checkTarget(t); err != nil {
		return Resources{}, err
	}
	if u.IsZero() {
		return Resources{}, errors.New("rungvx update: no settings specified")
	}
	args := targetArgs("update", t)
	if u.Memory != "" {
		args = append(args, "-m", u.Memory)
	}
	if u.ForceMemory {
		args = append(args, "--force-memory")
	}
	if u.CPUs != "" {
		args = append(args, "--cpus", u.CPUs)
	}
	if u.CPUQuota != 0 {
		args = append(args, "--cpu-quota", strconv.FormatInt(u.CPUQuota, 10))
	}
	if u.CPUPeriod != 0 {
		args = append(args, "--cpu-period", strconv.FormatInt(u.CPUPeriod, 10))
	}
	if u.PidsLimit != nil {
		args = append(args, "--pids-limit", strconv.FormatInt(*u.PidsLimit, 10))
	}
	if u.IsolateLAN != nil {
		args = append(args, fmt.Sprintf("--isolate-lan=%t", *u.IsolateLAN))
	}
	if u.NetTXBPS != nil {
		args = append(args, "--net-tx-bps", strconv.FormatUint(*u.NetTXBPS, 10))
	}
	if u.NetRXBPS != nil {
		args = append(args, "--net-rx-bps", strconv.FormatUint(*u.NetRXBPS, 10))
	}
	args = append(args, "-f", "json")
	out, err := c.output(ctx, args...)
	if err != nil {
		return Resources{}, err
	}
	var res Resources
	if err := json.Unmarshal(out, &res); err != nil {
		return Resources{}, err
	}
	return res, nil
}

// ---- events (P2) --------------------------------------------------------

// Event is one line of rungv events -f json.
type Event struct {
	Timestamp  time.Time         `json:"timestamp"`
	Type       string            `json:"type"`   // exec, port, mount, sandbox, lifecycle, resource, oom
	Action     string            `json:"action"` // start, exit, add, remove, pause, unpause, exited, ...
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Events hands every event of the sandbox to fn until ctx ends or the
// sandbox exits.
func (c *Client) Events(ctx context.Context, t Target, fn func(Event)) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	args := append(targetArgs("events", t), "-f", "json")
	return streamNDJSON(ctx, c, args, fn)
}

// ---- port-forward, dmesg, debug -----------------------------------------

// PortForwardOptions configures `rungvx port-forward` (gvx-ctrl/2 net.dial).
type PortForwardOptions struct {
	Address string // --address (default 127.0.0.1)
	GuestIP string // --guest-ip (optional guest target IP)
}

// PortForwardArgs builds the argument vector for `rungvx port-forward`
// without the executable name.
func (c *Client) PortForwardArgs(t Target, specs []string, o PortForwardOptions) []string {
	args := targetArgs("port-forward", t)
	if addr := strings.TrimSpace(o.Address); addr != "" {
		args = append(args, "--address", addr)
	}
	if gip := strings.TrimSpace(o.GuestIP); gip != "" {
		args = append(args, "--guest-ip", gip)
	}
	cleaned := make([]string, 0, len(specs))
	for _, s := range specs {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) > 0 {
		args = append(args, "--")
		args = append(args, cleaned...)
	}
	return args
}

// PortForwardCommand builds the long-lived exec.Cmd of `rungvx port-forward`
// against t. It works on any gvx-ctrl/2 sandbox without requiring --dynamic-ports.
func (c *Client) PortForwardCommand(ctx context.Context, t Target, specs []string, o PortForwardOptions) *exec.Cmd {
	return c.Command(ctx, c.PortForwardArgs(t, specs, o)...)
}

// DmesgOptions configures `rungvx dmesg` (gvx-ctrl/2 kernel.log).
type DmesgOptions struct {
	Follow bool // -f / --follow
	Tail   int  // --tail N (0 = all buffered entries)
	Level  int  // --level 1..8 (0 = all levels)
	Clear  bool // --clear (clear ring after reading)
}

// DmesgArgs builds the argument vector for `rungvx dmesg` without the
// executable name.
func (c *Client) DmesgArgs(t Target, o DmesgOptions) []string {
	args := targetArgs("dmesg", t)
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if o.Level >= 1 && o.Level <= 8 {
		args = append(args, "--level", strconv.Itoa(o.Level))
	}
	if o.Clear {
		args = append(args, "--clear")
	}
	return args
}

// DmesgCommand builds the exec.Cmd for `rungvx dmesg`.
func (c *Client) DmesgCommand(ctx context.Context, t Target, o DmesgOptions) *exec.Cmd {
	return c.Command(ctx, c.DmesgArgs(t, o)...)
}

// Dmesg reads buffered guest kernel log entries from a running sandbox
// (`rungvx dmesg`, gvx-ctrl/2 kernel.log). Follow must be false.
func (c *Client) Dmesg(ctx context.Context, t Target, o DmesgOptions) (string, error) {
	if err := c.checkTarget(t); err != nil {
		return "", err
	}
	if o.Follow {
		return "", errors.New("rungvx dmesg: use DmesgCommand for streaming follow mode")
	}
	if o.Tail < 0 {
		return "", errors.New("rungvx dmesg: tail must be non-negative")
	}
	if o.Level < 0 || o.Level > 8 {
		return "", errors.New("rungvx dmesg: level must be between 0 and 8")
	}
	out, err := c.output(ctx, c.DmesgArgs(t, o)...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// SetLogLevel dynamically changes the Sentry debug log verbosity of a running
// sandbox (`rungvx debug --log-level`, gvx-ctrl/2 debug.log_level).
// Valid levels are "warning", "info", and "debug".
func (c *Client) SetLogLevel(ctx context.Context, t Target, level string) error {
	if err := c.checkTarget(t); err != nil {
		return err
	}
	lvl := strings.ToLower(strings.TrimSpace(level))
	switch lvl {
	case "warning", "info", "debug":
	default:
		return fmt.Errorf("rungvx debug: invalid log level %q (want warning, info, or debug)", level)
	}
	args := append(targetArgs("debug", t), "--log-level", lvl)
	_, err := c.output(ctx, args...)
	return err
}

// ---- errors -------------------------------------------------------------

// Control protocol error codes rungv prints as "rungv <cmd>: CODE: message"
// (rungvx <cmd>: … since gvx v0.23).
const (
	CodeInvalidArgument   = "INVALID_ARGUMENT"
	CodeUnauthorized      = "UNAUTHORIZED"        // exit 126
	CodeForbiddenByPolicy = "FORBIDDEN_BY_POLICY" // exit 126
	CodeLockdownDenied    = "LOCKDOWN_DENIED"     // exit 126
	CodeUnsupported       = "UNSUPPORTED"
	CodeAlreadyPaused     = "ALREADY_PAUSED"
	CodeNotPaused         = "NOT_PAUSED"
	CodeNotFound          = "NOT_FOUND"
	CodeAlreadyExists     = "ALREADY_EXISTS"
	CodeBusy              = "BUSY"
	CodeLimitExceeded     = "LIMIT_EXCEEDED" // too many exec sessions or connections
	CodeExecActive        = "EXEC_ACTIVE"
	CodeSandboxExiting    = "SANDBOX_EXITING"
	CodeInternal          = "INTERNAL"
)

func lastNonEmptyLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}

func isControlCodeToken(s string) bool {
	if s == "" {
		return false
	}
	hasUpper := false
	for i := range len(s) {
		b := s[i]
		if b >= 'A' && b <= 'Z' {
			hasUpper = true
			continue
		}
		if (b >= '0' && b <= '9') || b == '_' {
			continue
		}
		return false
	}
	return hasUpper
}

// ControlCode is the protocol error code of a failed runtime command, ""
// when err is not one (or names none).
func ControlCode(err error) string {
	var re *Error
	if !errors.As(err, &re) {
		return ""
	}
	line, ok := CutProgram(lastNonEmptyLine(re.Stderr), " ")
	if !ok {
		return ""
	}
	_, rest, ok := strings.Cut(line, ": ")
	if !ok {
		return ""
	}
	code, _, _ := strings.Cut(rest, ": ")
	code = strings.TrimSpace(code)
	if !isControlCodeToken(code) {
		return ""
	}
	return code
}

// IsUnreachable reports whether err says the control socket has no
// sandbox behind it: connection refused, or no such file. The sandbox has
// ended, or is ending.
func IsUnreachable(err error) bool {
	var re *Error
	if !errors.As(err, &re) {
		return false
	}
	if ControlCode(err) != "" {
		return false
	}
	msg := strings.ToLower(lastNonEmptyLine(re.Stderr))
	if msg == "" && re.Err != nil {
		msg = strings.ToLower(re.Err.Error())
	}
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "connect:") ||
		strings.Contains(msg, "connect control socket") ||
		strings.Contains(msg, "actively refused") ||
		strings.Contains(msg, "cannot find the file specified")
}
