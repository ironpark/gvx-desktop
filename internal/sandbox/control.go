package sandbox

// Owner: W1 (rungv + sandbox layer). Design: docs/design/gvx-v020.md §4.
//
// The runtime control of a running sandbox over its control socket
// (rungv v0.20). The Control and ExecProcess interfaces, ErrNoControl, and
// the Supervisor methods in this file are frozen seams: the engine (W2)
// and the UI (W3) code against them, with fakes of the two interfaces in
// their own tests. Bodies are W1's.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// ErrNoControl reports a run without a control socket: an older rungv, or
// a run started before the binary was upgraded. State.Control is false.
var ErrNoControl = errors.New("the sandbox was started without a control socket; restart it with rungvx v0.20 or later")

// ErrPaused reports an operation a paused sandbox refuses (exec, cp).
var ErrPaused = errors.New("the sandbox is paused")

// Control drives one running sandbox through its control socket. It is
// bound to one run: once that run ends, every method fails (with
// ErrNotRunning-like errors from rungv: rungv.IsUnreachable).
//
// Lifecycle changes that the supervisor must know of (pause, unpause,
// update, graceful stop) are Supervisor methods, not Control methods.
type Control interface {
	// Target is the run's control socket, for callers that run rungv
	// themselves.
	Target() rungv.Target
	// ExecArgv is the full argument vector, rungv executable first, of
	// rungv exec in this run: what the UI's terminal starts on its own
	// PTY with internal/term (term.Start), as it does kubectl exec -it.
	ExecArgv(o rungv.ExecOptions) []string
	// Exec starts a command in the guest and returns at once. With
	// o.TTY on Unix, rungv exec runs on a PTY of the supervisor's
	// (raw mode, sized to o's Rows/Cols before it starts) so Resize
	// reaches the guest; on Windows it runs on pipes and Resize fails.
	// Exec fails with ErrPaused when the run is paused.
	Exec(ctx context.Context, o ExecOptions) (ExecProcess, error)
	// CopyFrom, CopyTo, StatPath and Export are rungv.Client's, bound to
	// this run.
	CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error)
	CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error
	StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error)
	Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error)
	// Stats takes one sample of the guest's counters.
	Stats(ctx context.Context) (rungv.Stats, error)
	// StreamStats samples every interval until ctx ends or the run does.
	StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error
	// Top lists the guest's processes.
	Top(ctx context.Context) ([]rungv.Process, error)
	// Kill signals guest processes (the first one by default). A SIGKILL
	// of the first process ends the run, which then ends as Failed with
	// rungv's exit code, not as Stopped.
	Kill(ctx context.Context, o rungv.KillOptions) error
}

// ExecOptions are the options of Control.Exec: rungv's, plus the initial
// terminal size. PIDFile is the supervisor's own (it always asks for one).
type ExecOptions struct {
	rungv.ExecOptions
	Rows, Cols uint16 // initial terminal size with TTY; 0 is 24x80
}

// ExecProcess is one rungv exec started by Control.Exec.
type ExecProcess interface {
	// Stdin is the command's standard input; nil without Interactive.
	// Close sends end-of-file: it closes the pipe, or writes EOT (0x04)
	// on a terminal.
	Stdin() io.WriteCloser
	// Stdout is what the command writes; with TTY, the terminal's bytes,
	// standard error included.
	Stdout() io.Reader
	// Stderr is the command's standard error without TTY, and rungv's own
	// messages ("rungv exec: CODE: ..."); nil with TTY (rungv's messages
	// then go to Wait's error).
	Stderr() io.Reader
	// Resize sets the terminal's size; an error without TTY, and on
	// Windows.
	Resize(rows, cols uint16) error
	// PID is the command's guest PID once it started, 0 before (read
	// from rungv's --pid-file). Started is closed when PID is known, or
	// when the exec failed before starting.
	PID() int
	Started() <-chan struct{}
	// Wait waits for the command and returns its exit code. err is
	// non-nil when rungv failed rather than the command: the exec never
	// started (PID() == 0: paused, not found, too many sessions), or the
	// run ended under it. An *rungv.Error carries rungv's message; see
	// rungv.ControlCode.
	Wait() (code int, err error)
	// Signal sends sig to the command's guest process (rungv kill --pid).
	Signal(ctx context.Context, sig string) error
	// Close ends the rungv exec client without signaling the guest (the
	// guest command keeps running detached) and releases the PTY and pipes.
	// Safe to call twice.
	Close() error
}

type runControl struct {
	sup    *Supervisor
	id     string
	proc   *proc
	client *rungv.Client
	target rungv.Target
}

func (c *runControl) Target() rungv.Target { return c.target }

func (c *runControl) ExecArgv(o rungv.ExecOptions) []string {
	args := c.client.ExecArgs(c.target, o)
	out := make([]string, 0, len(args)+1)
	out = append(out, c.client.Path)
	return append(out, args...)
}

func (c *runControl) isPaused() bool {
	c.sup.mu.Lock()
	paused := c.proc.state.Paused
	c.sup.mu.Unlock()
	return paused
}

func (c *runControl) CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error) {
	return c.client.CopyFrom(ctx, c.target, guestPath, o)
}

func (c *runControl) CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error {
	return c.client.CopyTo(ctx, c.target, guestDir, r, o)
}

func (c *runControl) StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error) {
	return c.client.StatPath(ctx, c.target, guestPath)
}

func (c *runControl) Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error) {
	return c.client.Export(ctx, c.target, guestPath, upperOnly)
}

func (c *runControl) Stats(ctx context.Context) (rungv.Stats, error) {
	return c.client.Stats(ctx, c.target)
}

func (c *runControl) StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error {
	return c.client.StreamStats(ctx, c.target, interval, fn)
}

func (c *runControl) Top(ctx context.Context) ([]rungv.Process, error) {
	return c.client.Top(ctx, c.target)
}

func (c *runControl) Kill(ctx context.Context, o rungv.KillOptions) error {
	return c.client.Kill(ctx, c.target, o)
}

func (s *Supervisor) clientFor(p *proc) *rungv.Client {
	if s.Client != nil {
		if cur := s.Client(); cur != nil && cur.Path != "" {
			return cur
		}
	}
	return p.client
}

// Control returns the control of the active run of id: false when none is
// active, or the run has no control socket (State.Control is false).
func (s *Supervisor) Control(id string) (Control, bool) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() || !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return nil, false
	}
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	s.mu.Unlock()
	client := s.clientFor(p)
	if client == nil || client.Path == "" {
		return nil, false
	}
	return &runControl{
		sup:    s,
		id:     id,
		proc:   p,
		client: client,
		target: target,
	}, true
}

func (s *Supervisor) controlTarget(id string) (*proc, *rungv.Client, rungv.Target, error) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil, nil, rungv.Target{}, errNotRunning
	}
	if !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return nil, nil, rungv.Target{}, ErrNoControl
	}
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	s.mu.Unlock()
	client := s.clientFor(p)
	if client == nil || client.Path == "" {
		return nil, nil, rungv.Target{}, rungv.ErrNotFound
	}
	return p, client, target, nil
}

// Pause freezes the guest of the active run of id. State.Paused becomes
// true and an EventChanged follows. ErrNoControl without a socket.
func (s *Supervisor) Pause(ctx context.Context, id string) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.Pause(ctx, target); err != nil && rungv.ControlCode(err) != rungv.CodeAlreadyPaused {
		return err
	}
	s.mu.Lock()
	if p.state.Status.Active() {
		p.state.Paused = true
	}
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Unpause thaws the guest of the active run of id.
func (s *Supervisor) Unpause(ctx context.Context, id string) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.Unpause(ctx, target); err != nil && rungv.ControlCode(err) != rungv.CodeNotPaused {
		return err
	}
	s.mu.Lock()
	p.state.Paused = false
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Update changes the resources of the active run of id live. On success
// the active spec (ActiveSpec) takes the new Memory, CPUs and IsolateLAN,
// so a later edit compares against what runs.
func (s *Supervisor) Update(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return rungv.Resources{}, err
	}
	res, err := client.Update(ctx, target, u)
	if err != nil {
		return rungv.Resources{}, err
	}
	s.mu.Lock()
	applyUpdateToSpec(&p.spec, u)
	s.mu.Unlock()

	if summary := formatUpdateSummary(u); summary != "" {
		s.Logs(id).Add(System, "── Updated: "+summary)
	}
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return res, nil
}

func applyUpdateToSpec(spec *Spec, u rungv.Update) {
	if u.Memory != "" {
		if u.Memory == "0" {
			spec.Memory = ""
		} else {
			spec.Memory = u.Memory
		}
	}
	if u.CPUs != "" {
		if u.CPUs == "0" {
			spec.CPUs = ""
		} else {
			spec.CPUs = u.CPUs
		}
	}
	if u.IsolateLAN != nil {
		spec.IsolateLAN = *u.IsolateLAN
	}
	if u.PidsLimit != nil {
		if *u.PidsLimit < 0 {
			spec.PidsLimit = 0
		} else {
			spec.PidsLimit = *u.PidsLimit
		}
	}
	if u.NetTXBPS != nil {
		spec.NetTXBPS = *u.NetTXBPS
	}
	if u.NetRXBPS != nil {
		spec.NetRXBPS = *u.NetRXBPS
	}
}

func formatUpdateSummary(u rungv.Update) string {
	var parts []string
	if u.Memory != "" {
		parts = append(parts, "memory "+u.Memory)
	}
	if u.CPUs != "" {
		parts = append(parts, "cpus "+u.CPUs)
	}
	if u.CPUQuota != 0 {
		parts = append(parts, fmt.Sprintf("cpu-quota %d", u.CPUQuota))
	}
	if u.CPUPeriod != 0 {
		parts = append(parts, fmt.Sprintf("cpu-period %d", u.CPUPeriod))
	}
	if u.PidsLimit != nil {
		parts = append(parts, fmt.Sprintf("pids-limit %d", *u.PidsLimit))
	}
	if u.IsolateLAN != nil {
		parts = append(parts, fmt.Sprintf("isolate-lan %t", *u.IsolateLAN))
	}
	if u.NetTXBPS != nil {
		parts = append(parts, fmt.Sprintf("net-tx-bps %d", *u.NetTXBPS))
	}
	if u.NetRXBPS != nil {
		parts = append(parts, fmt.Sprintf("net-rx-bps %d", *u.NetRXBPS))
	}
	return strings.Join(parts, ", ")
}

// Shutdown stops the active run of id as docker stop does: signal (""
// is SIGTERM) to the guest's first process through the control socket,
// which kills it after timeout; the sandbox ends with it, as Stopped. A
// paused run is unpaused first. Without a control socket, or when rungv
// stop fails, it is Stop(id, timeout). It returns once the run ended.
func (s *Supervisor) Shutdown(id string, signal string, timeout time.Duration) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil
	}
	if !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return s.Stop(id, timeout)
	}
	p.stopping = true
	p.state.Status = Stopping
	paused := p.state.Paused
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	osProc := p.cmd.Process
	done := p.done
	s.mu.Unlock()

	client := s.clientFor(p)
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	s.Logs(id).Add(System, "── Stopping…")

	fallbackStop := func() error {
		if runtime.GOOS == "windows" {
			_ = osProc.Kill()
		} else if err := osProc.Signal(os.Interrupt); err != nil {
			_ = osProc.Kill()
		}
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			_ = osProc.Kill()
			<-done
			return nil
		}
	}

	if client == nil || client.Path == "" {
		return fallbackStop()
	}

	if paused {
		uCtx, uCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.Unpause(uCtx, target)
		uCancel()
		s.mu.Lock()
		p.state.Paused = false
		s.mu.Unlock()
	}

	grace := max(timeout, 0)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), grace+10*time.Second)
	defer stopCancel()

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- client.StopGuest(stopCtx, target, signal, grace)
	}()

	timer := time.NewTimer(grace + 5*time.Second)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case err := <-stopErrCh:
		select {
		case <-done:
			return nil
		default:
		}
		if err != nil {
			return fallbackStop()
		}
		// StopGuest returned cleanly; wait briefly for the host rungv process to exit.
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return fallbackStop()
		}
	case <-timer.C:
		return fallbackStop()
	}
}

// LiveUpdate compares the spec a sandbox runs with (running) and an edit
// of it. live is true when every difference can be applied to the run with
// Supervisor.Update: only Memory (not to ""), CPUs ("" becomes "0"),
// IsolateLAN, NetTXBPS, and NetRXBPS differ, besides fields that never reach
// rungv (AutoStart). u is then the update to apply; it is zero when nothing
// differs.
func LiveUpdate(running, edited Spec) (u rungv.Update, live bool) {
	a, b := running.Clone(), edited.Clone()
	a.Memory, b.Memory = "", ""
	a.CPUs, b.CPUs = "", ""
	a.PidsLimit, b.PidsLimit = 0, 0
	a.IsolateLAN, b.IsolateLAN = false, false
	a.NetTXBPS, b.NetTXBPS = 0, 0
	a.NetRXBPS, b.NetRXBPS = 0, 0
	a.AutoStart, b.AutoStart = false, false
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	if !a.Equal(b) {
		return rungv.Update{}, false
	}
	isNAT := running.Network == "" || running.Network == "nat"
	if running.Memory != edited.Memory {
		if edited.Memory == "" {
			return rungv.Update{}, false
		}
		u.Memory = edited.Memory
	}
	if running.CPUs != edited.CPUs {
		if edited.CPUs == "" {
			u.CPUs = "0"
		} else {
			u.CPUs = edited.CPUs
		}
	}
	if running.PidsLimit != edited.PidsLimit {
		v := edited.PidsLimit
		u.PidsLimit = &v
	}
	if running.IsolateLAN != edited.IsolateLAN {
		v := edited.IsolateLAN
		u.IsolateLAN = &v
	}
	if running.NetTXBPS != edited.NetTXBPS {
		if !isNAT {
			return rungv.Update{}, false
		}
		v := edited.NetTXBPS
		u.NetTXBPS = &v
	}
	if running.NetRXBPS != edited.NetRXBPS {
		if !isNAT {
			return rungv.Update{}, false
		}
		v := edited.NetRXBPS
		u.NetRXBPS = &v
	}
	return u, true
}

// LiveDiff describes the changes between a running sandbox's active spec and
// an edited spec that can be applied live without restarting the sandbox.
type LiveDiff struct {
	Update        rungv.Update
	NATPolicyFile string
	AddPorts      []string
	RemovePorts   []string
	AddMounts     []Mount
	RemountMounts []Mount
	RemoveMounts  []string
}

// IsZero reports whether d carries no live changes.
func (d LiveDiff) IsZero() bool {
	return d.Update.IsZero() &&
		d.NATPolicyFile == "" &&
		len(d.AddPorts) == 0 &&
		len(d.RemovePorts) == 0 &&
		len(d.AddMounts) == 0 &&
		len(d.RemountMounts) == 0 &&
		len(d.RemoveMounts) == 0
}

// ComputeLiveDiff compares running and edited against the capabilities the
// running sandbox actually started with (st). It returns (diff, true) when
// every difference can be applied live via Supervisor.ApplyLive (with diff
// zero when nothing runtime-visible changed), or (zero, false) when a restart
// is required.
func ComputeLiveDiff(running, edited Spec, st State) (LiveDiff, bool) {
	a, b := running.Clone(), edited.Clone()
	a.Memory, b.Memory = "", ""
	a.CPUs, b.CPUs = "", ""
	a.PidsLimit, b.PidsLimit = 0, 0
	a.IsolateLAN, b.IsolateLAN = false, false
	a.NetTXBPS, b.NetTXBPS = 0, 0
	a.NetRXBPS, b.NetRXBPS = 0, 0
	a.NATPolicyFile, b.NATPolicyFile = "", ""
	a.Ports, b.Ports = nil, nil
	a.Mounts, b.Mounts = nil, nil
	a.AutoStart, b.AutoStart = false, false
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	if !a.Equal(b) {
		return LiveDiff{}, false
	}

	var diff LiveDiff
	isNAT := running.Network == "" || running.Network == "nat"

	if running.Memory != edited.Memory {
		if !st.Control || edited.Memory == "" {
			return LiveDiff{}, false
		}
		diff.Update.Memory = edited.Memory
	}
	if running.CPUs != edited.CPUs {
		if !st.Control {
			return LiveDiff{}, false
		}
		if edited.CPUs == "" {
			diff.Update.CPUs = "0"
		} else {
			diff.Update.CPUs = edited.CPUs
		}
	}
	if running.PidsLimit != edited.PidsLimit {
		if !st.Control {
			return LiveDiff{}, false
		}
		v := edited.PidsLimit
		diff.Update.PidsLimit = &v
	}
	if running.IsolateLAN != edited.IsolateLAN {
		if !st.Control || !isNAT {
			return LiveDiff{}, false
		}
		v := edited.IsolateLAN
		diff.Update.IsolateLAN = &v
	}
	if running.NetTXBPS != edited.NetTXBPS {
		if !st.Control || !st.NetQoS || !isNAT {
			return LiveDiff{}, false
		}
		v := edited.NetTXBPS
		diff.Update.NetTXBPS = &v
	}
	if running.NetRXBPS != edited.NetRXBPS {
		if !st.Control || !st.NetQoS || !isNAT {
			return LiveDiff{}, false
		}
		v := edited.NetRXBPS
		diff.Update.NetRXBPS = &v
	}

	runPol := strings.TrimSpace(running.NATPolicyFile)
	edPol := strings.TrimSpace(edited.NATPolicyFile)
	if runPol != edPol {
		if edPol == "" || !st.Control || !st.NATPolicy || !isNAT {
			return LiveDiff{}, false
		}
		diff.NATPolicyFile = edPol
	}

	addPorts, removePorts := diffStrings(running.Ports, edited.Ports)
	if len(addPorts) > 0 || len(removePorts) > 0 {
		if !st.Control || !st.DynamicPorts || !isNAT {
			return LiveDiff{}, false
		}
		for _, p := range addPorts {
			if !portSpecAllowedLive(p, running.DynamicPortAddrs) {
				return LiveDiff{}, false
			}
		}
		for _, p := range removePorts {
			if !portSpecAllowedLive(p, running.DynamicPortAddrs) {
				return LiveDiff{}, false
			}
		}
		diff.AddPorts = addPorts
		diff.RemovePorts = removePorts
	}

	runMounts := make(map[string]Mount, len(running.Mounts))
	for _, m := range running.Mounts {
		runMounts[m.Target] = m
	}
	edMounts := make(map[string]Mount, len(edited.Mounts))
	for _, m := range edited.Mounts {
		edMounts[m.Target] = m
	}
	mountAllowedLive := func(m Mount) bool {
		switch m.Type {
		case MountTmpfs:
			return st.Control
		case MountBind:
			return st.Control && (st.BindAttach || st.LiveMounts) && m.Symlinks == ""
		case MountVolume:
			return st.Control && st.VolumeAttach && !m.Ephemeral && m.Size == ""
		default:
			return false
		}
	}
	for _, m := range running.Mounts {
		em, exists := edMounts[m.Target]
		if !exists {
			if !mountAllowedLive(m) {
				return LiveDiff{}, false
			}
			diff.RemoveMounts = append(diff.RemoveMounts, m.Target)
		} else if !mountsEqual(m, em) {
			if st.Control && mountsSameExceptReadOnly(m, em) && (em.ReadOnly || m.Type == MountTmpfs) {
				diff.RemountMounts = append(diff.RemountMounts, em)
				continue
			}
			if !mountAllowedLive(m) || !mountAllowedLive(em) {
				return LiveDiff{}, false
			}
			diff.RemoveMounts = append(diff.RemoveMounts, m.Target)
			diff.AddMounts = append(diff.AddMounts, em)
		}
	}
	for _, m := range edited.Mounts {
		if _, exists := runMounts[m.Target]; !exists {
			if !mountAllowedLive(m) {
				return LiveDiff{}, false
			}
			diff.AddMounts = append(diff.AddMounts, m)
		}
	}

	return diff, true
}

func mountsEqual(a, b Mount) bool {
	if a.Type != b.Type || a.Target != b.Target || a.ReadOnly != b.ReadOnly || a.Size != b.Size || a.Ephemeral != b.Ephemeral || a.Symlinks != b.Symlinks {
		return false
	}
	if a.Type == MountBind {
		return rungv.ExpandHome(a.Source) == rungv.ExpandHome(b.Source)
	}
	return a.Source == b.Source
}

func mountsSameExceptReadOnly(a, b Mount) bool {
	if a.ReadOnly == b.ReadOnly {
		return false
	}
	b.ReadOnly = a.ReadOnly
	return mountsEqual(a, b)
}

func diffStrings(before, after []string) (added, removed []string) {
	beforeCounts := make(map[string]int, len(before))
	for _, raw := range before {
		if s := strings.TrimSpace(raw); s != "" {
			beforeCounts[s]++
		}
	}
	afterCounts := make(map[string]int, len(after))
	for _, raw := range after {
		if s := strings.TrimSpace(raw); s != "" {
			afterCounts[s]++
		}
	}
	seenRemoved := map[string]int{}
	for _, raw := range before {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if seenRemoved[s] < beforeCounts[s]-afterCounts[s] {
			removed = append(removed, s)
			seenRemoved[s]++
		}
	}
	seenAdded := map[string]int{}
	for _, raw := range after {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if seenAdded[s] < afterCounts[s]-beforeCounts[s] {
			added = append(added, s)
			seenAdded[s]++
		}
	}
	return added, removed
}

func portSpecAllowedLive(spec string, dynamicAddrs []string) bool {
	p := strings.TrimSpace(spec)
	if p == "" || strings.HasPrefix(p, "unix:") {
		return false
	}
	m := portRe.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	ipStr := m[1]
	if ipStr == "" {
		return true
	}
	addr, err := parseBareIPAddr(strings.Trim(ipStr, "[]"))
	if err != nil {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, raw := range dynamicAddrs {
		if allowed, err := parseBareIPAddr(raw); err == nil && allowed == addr {
			return true
		}
	}
	return false
}

func mountAddOptions(m Mount) rungv.MountAddOptions {
	src := m.Source
	if m.Type == MountBind || (m.Type == MountVolume && strings.ContainsAny(m.Source, `/\`)) {
		src = rungv.ExpandHome(m.Source)
	}
	return rungv.MountAddOptions{
		Type:     string(m.Type),
		Source:   src,
		Target:   m.Target,
		ReadOnly: m.ReadOnly,
		Size:     m.Size,
	}
}

func removePortsFromSpec(ports []string, remove []string) []string {
	rmCounts := make(map[string]int, len(remove))
	for _, r := range remove {
		rmCounts[strings.TrimSpace(r)]++
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		trimmed := strings.TrimSpace(p)
		if rmCounts[trimmed] > 0 {
			rmCounts[trimmed]--
			continue
		}
		out = append(out, p)
	}
	return out
}

func addPortsToSpec(ports []string, add []string) []string {
	out := append([]string(nil), ports...)
	for _, a := range add {
		s := strings.TrimSpace(a)
		if s == "" {
			continue
		}
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.TrimSpace(existing) == s }) {
			out = append(out, s)
		}
	}
	return out
}

func upsertMountInSpec(mounts []Mount, m Mount) []Mount {
	out := append([]Mount(nil), mounts...)
	for i, existing := range out {
		if existing.Target == m.Target {
			out[i] = m
			return out
		}
	}
	return append(out, m)
}

func removeMountFromSpec(mounts []Mount, target string) []Mount {
	want := path.Clean(strings.TrimSpace(target))
	return slices.DeleteFunc(append([]Mount(nil), mounts...), func(m Mount) bool {
		return m.Target == target || (want != "" && path.Clean(strings.TrimSpace(m.Target)) == want)
	})
}

const mountServeSilentGrace = 200 * time.Millisecond

type serveProc struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	exitErr  error
	stderr   string
}

func (sp *serveProc) stop() {
	sp.stopOnce.Do(func() {
		sp.cancel()
		select {
		case <-sp.done:
			return
		case <-time.After(3 * time.Second):
		}
		if sp.cmd != nil && sp.cmd.Process != nil {
			_ = sp.cmd.Process.Kill()
		}
		<-sp.done
	})
}

func (s *Supervisor) stopAllServes(p *proc) {
	s.mu.Lock()
	p.servesClosed = true
	var list []*serveProc
	if len(p.serves) > 0 {
		list = make([]*serveProc, 0, len(p.serves))
		for _, sp := range p.serves {
			list = append(list, sp)
		}
		p.serves = nil
	}
	s.mu.Unlock()
	if len(list) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, sp := range list {
		wg.Go(func() {
			sp.stop()
		})
	}
	wg.Wait()
}

func (s *Supervisor) startServeMount(ctx context.Context, id string, p *proc, client *rungv.Client, target rungv.Target, m Mount) error {
	key := path.Clean(strings.TrimSpace(m.Target))
	hostPath := rungv.ExpandHome(strings.TrimSpace(m.Source))

	s.mu.Lock()
	active := s.procs[id] == p && p.state.Status.Active() && !p.servesClosed
	s.mu.Unlock()
	if !active {
		return errNotRunning
	}

	args := client.MountServeArgs(target, hostPath, key, m.ReadOnly)
	serveCtx, serveCancel := context.WithCancel(context.Background())
	cmd := client.MountServeCommand(serveCtx, target, hostPath, key, m.ReadOnly)
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
	}
	cmd.WaitDelay = 2 * time.Second

	outR, outW, err := os.Pipe()
	if err != nil {
		serveCancel()
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		serveCancel()
		_ = outR.Close()
		_ = outW.Close()
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		serveCancel()
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return err
	}
	_ = outW.Close()
	_ = errW.Close()

	sp := &serveProc{
		cmd:    cmd,
		cancel: serveCancel,
		done:   make(chan struct{}),
	}

	readyCh := make(chan struct{})
	var readyOnce sync.Once
	var ioWG sync.WaitGroup
	var errBuf bytes.Buffer

	ioWG.Add(2)
	go func() {
		defer ioWG.Done()
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 4*1024), 64*1024)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				readyOnce.Do(func() { close(readyCh) })
			}
		}
	}()
	go func() {
		defer ioWG.Done()
		// Keep up to 4 KiB of stderr for error reporting, then drain the rest.
		_, _ = io.Copy(&errBuf, io.LimitReader(errR, 4096))
		_, _ = io.Copy(io.Discard, errR)
	}()

	go func() {
		waitErr := cmd.Wait()
		ioWG.Wait()
		_ = outR.Close()
		_ = errR.Close()
		sp.exitErr = waitErr
		sp.stderr = errBuf.String()
		close(sp.done)
		var dropped bool
		s.mu.Lock()
		if p.serves[key] == sp {
			delete(p.serves, key)
			if !p.servesClosed && s.procs[id] == p && p.state.Status.Active() {
				p.spec.Mounts = removeMountFromSpec(p.spec.Mounts, m.Target)
				dropped = true
			}
		}
		s.mu.Unlock()
		if dropped {
			if msg := strings.TrimSpace(sp.stderr); msg != "" {
				s.Logs(id).Add(System, fmt.Sprintf("── Live mount %s ended: %s", m.Target, msg))
			} else if waitErr != nil {
				s.Logs(id).Add(System, fmt.Sprintf("── Live mount %s ended: %v", m.Target, waitErr))
			}
			s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
		}
	}()

	grace := time.NewTimer(mountServeSilentGrace)
	defer grace.Stop()

	exitToErr := func() error {
		if sp.exitErr != nil {
			re := &rungv.Error{Args: args, Stderr: sp.stderr, Err: sp.exitErr}
			if exit, ok := errors.AsType[*exec.ExitError](sp.exitErr); ok {
				re.Code = exit.ExitCode()
			}
			return re
		}
		if msg := strings.TrimSpace(sp.stderr); msg != "" {
			return errors.New(msg)
		}
		return errors.New("rungvx mount serve exited unexpectedly")
	}

	select {
	case <-readyCh:
	case <-sp.done:
		return exitToErr()
	case <-ctx.Done():
		sp.stop()
		return ctx.Err()
	case <-grace.C:
	}

	var prev *serveProc
	s.mu.Lock()
	select {
	case <-sp.done:
		s.mu.Unlock()
		return exitToErr()
	default:
	}
	if s.procs[id] != p || !p.state.Status.Active() || p.servesClosed {
		s.mu.Unlock()
		sp.stop()
		return errNotRunning
	}
	if p.serves == nil {
		p.serves = map[string]*serveProc{}
	}
	prev = p.serves[key]
	p.serves[key] = sp
	p.spec.Mounts = upsertMountInSpec(p.spec.Mounts, m)
	s.mu.Unlock()

	if prev != nil && prev != sp {
		prev.stop()
	}
	return nil
}

func (s *Supervisor) attachMount(ctx context.Context, id string, p *proc, client *rungv.Client, target rungv.Target, m Mount) error {
	if err := m.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	bindAttach := p.state.BindAttach
	liveMounts := p.state.LiveMounts
	s.mu.Unlock()

	if m.Type == MountBind && !bindAttach && liveMounts {
		return s.startServeMount(ctx, id, p, client, target, m)
	}
	if err := client.AddMount(ctx, target, mountAddOptions(m)); err != nil {
		return err
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		p.spec.Mounts = upsertMountInSpec(p.spec.Mounts, m)
	}
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) detachMount(ctx context.Context, id string, p *proc, client *rungv.Client, target rungv.Target, guestTarget string, lazy bool) error {
	key := path.Clean(strings.TrimSpace(guestTarget))
	s.mu.Lock()
	sp := p.serves[key]
	s.mu.Unlock()

	err := client.RemoveMount(ctx, target, guestTarget, lazy)
	if err != nil {
		if sp == nil || rungv.ControlCode(err) != rungv.CodeNotFound {
			return err
		}
	}
	if sp != nil {
		s.mu.Lock()
		if p.serves[key] == sp {
			delete(p.serves, key)
		}
		s.mu.Unlock()
		sp.stop()
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		p.spec.Mounts = removeMountFromSpec(p.spec.Mounts, guestTarget)
	}
	s.mu.Unlock()
	return nil
}

// AddPorts dynamically publishes specs on the active run of id, updates
// ActiveSpec, syncs published ports, and emits EventChanged.
func (s *Supervisor) AddPorts(ctx context.Context, id string, specs []string, replace bool) ([]rungv.PortRule, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return nil, err
	}
	rules, err := client.AddPorts(ctx, target, specs, replace)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		p.spec.Ports = addPortsToSpec(p.spec.Ports, specs)
		s.syncPortsLocked(id, p)
	}
	s.mu.Unlock()
	s.Logs(id).Add(System, "── Updated: added ports "+strings.Join(specs, ", "))
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return rules, nil
}

// RemovePorts closes published port specs on the active run of id, updates
// ActiveSpec, syncs published ports, and emits EventChanged.
func (s *Supervisor) RemovePorts(ctx context.Context, id string, specs []string, force bool) error {
	if len(specs) == 0 {
		return nil
	}
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.RemovePorts(ctx, target, specs, force); err != nil {
		return err
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		p.spec.Ports = removePortsFromSpec(p.spec.Ports, specs)
		s.syncPortsLocked(id, p)
	}
	s.mu.Unlock()
	s.Logs(id).Add(System, "── Updated: removed ports "+strings.Join(specs, ", "))
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Mounts lists active mounts inside the running sandbox of id.
func (s *Supervisor) Mounts(ctx context.Context, id string) ([]rungv.MountInfo, error) {
	_, client, target, err := s.controlTarget(id)
	if err != nil {
		return nil, err
	}
	return client.Mounts(ctx, target)
}

// AddMount attaches m inside the active run of id, updates ActiveSpec, and
// emits EventChanged.
func (s *Supervisor) AddMount(ctx context.Context, id string, m Mount) error {
	if err := m.validate(); err != nil {
		return err
	}
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := s.attachMount(ctx, id, p, client, target, m); err != nil {
		return err
	}
	s.Logs(id).Add(System, "── Updated: mounted "+m.Target)
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Remount toggles an existing mount point between read-only and read-write
// inside the active run of id (`rungvx mount remount`), updates ActiveSpec,
// and emits EventChanged.
func (s *Supervisor) Remount(ctx context.Context, id string, guestTarget string, readOnly bool) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.Remount(ctx, target, guestTarget, readOnly); err != nil {
		return err
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		want := path.Clean(strings.TrimSpace(guestTarget))
		if want == "/workspace" && p.spec.Workspace != "" {
			p.spec.WorkspaceRO = readOnly
		}
		for i := range p.spec.Mounts {
			if p.spec.Mounts[i].Target == guestTarget || (want != "" && path.Clean(strings.TrimSpace(p.spec.Mounts[i].Target)) == want) {
				p.spec.Mounts[i].ReadOnly = readOnly
				break
			}
		}
	}
	s.mu.Unlock()
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	s.Logs(id).Add(System, fmt.Sprintf("── Updated: remounted %s (%s)", guestTarget, mode))
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Dmesg reads buffered guest kernel log records from the active run of id
// (`rungvx dmesg`, gvx-ctrl/2 kernel.log).
func (s *Supervisor) Dmesg(ctx context.Context, id string, opts rungv.DmesgOptions) (string, error) {
	_, client, target, err := s.controlTarget(id)
	if err != nil {
		return "", err
	}
	return client.Dmesg(ctx, target, opts)
}

// SetLogLevel dynamically changes the Sentry debug log level of the active run
// of id (`rungvx debug --log-level`, gvx-ctrl/2 debug.log_level).
func (s *Supervisor) SetLogLevel(ctx context.Context, id string, level string) error {
	_, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.SetLogLevel(ctx, target, level); err != nil {
		return err
	}
	s.Logs(id).Add(System, "── Updated: log-level "+strings.ToLower(strings.TrimSpace(level)))
	return nil
}

// RemoveMount unmounts guestTarget inside the active run of id, updates
// ActiveSpec, and emits EventChanged.
func (s *Supervisor) RemoveMount(ctx context.Context, id string, guestTarget string, lazy bool) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := s.detachMount(ctx, id, p, client, target, guestTarget, lazy); err != nil {
		return err
	}
	s.Logs(id).Add(System, "── Updated: unmounted "+guestTarget)
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// NATPolicy reads the active host NAT firewall policy JSON of id.
func (s *Supervisor) NATPolicy(ctx context.Context, id string) ([]byte, error) {
	_, client, target, err := s.controlTarget(id)
	if err != nil {
		return nil, err
	}
	return client.NATPolicy(ctx, target)
}

// ReplaceNATPolicy replaces the host NAT firewall policy of the active run of
// id from policyFile, updates ActiveSpec, and emits EventChanged.
func (s *Supervisor) ReplaceNATPolicy(ctx context.Context, id string, policyFile string) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(policyFile)
	if err := client.ReplaceNATPolicy(ctx, target, rungv.ExpandHome(trimmed)); err != nil {
		return err
	}
	s.mu.Lock()
	if s.procs[id] == p && p.state.Status.Active() {
		p.spec.NATPolicyFile = trimmed
	}
	s.mu.Unlock()
	s.Logs(id).Add(System, "── Updated: nat-policy "+trimmed)
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// ApplyLive applies every change in diff to the active run of id without
// holding s.mu across rungv calls. ActiveSpec is updated incrementally as each
// step succeeds, ports are synced if any port changed, and a summary log line
// plus EventChanged follow.
func (s *Supervisor) ApplyLive(ctx context.Context, id string, diff LiveDiff) error {
	if diff.IsZero() {
		return nil
	}
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}

	var applied []string
	portsChanged := false
	finish := func() {
		if portsChanged {
			s.mu.Lock()
			if s.procs[id] == p && p.state.Status.Active() {
				s.syncPortsLocked(id, p)
			}
			s.mu.Unlock()
		}
		if len(applied) > 0 {
			s.Logs(id).Add(System, "── Updated: "+strings.Join(applied, ", "))
			s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
		}
	}

	if !diff.Update.IsZero() {
		if _, err := client.Update(ctx, target, diff.Update); err != nil {
			finish()
			return err
		}
		s.mu.Lock()
		if s.procs[id] == p && p.state.Status.Active() {
			applyUpdateToSpec(&p.spec, diff.Update)
		}
		s.mu.Unlock()
		if summary := formatUpdateSummary(diff.Update); summary != "" {
			applied = append(applied, summary)
		}
	}

	if diff.NATPolicyFile != "" {
		pol := strings.TrimSpace(diff.NATPolicyFile)
		if err := client.ReplaceNATPolicy(ctx, target, rungv.ExpandHome(pol)); err != nil {
			finish()
			return err
		}
		s.mu.Lock()
		if s.procs[id] == p && p.state.Status.Active() {
			p.spec.NATPolicyFile = pol
		}
		s.mu.Unlock()
		applied = append(applied, "nat-policy "+pol)
	}

	if len(diff.RemovePorts) > 0 {
		if err := client.RemovePorts(ctx, target, diff.RemovePorts, true); err != nil {
			finish()
			return err
		}
		s.mu.Lock()
		if s.procs[id] == p && p.state.Status.Active() {
			p.spec.Ports = removePortsFromSpec(p.spec.Ports, diff.RemovePorts)
		}
		s.mu.Unlock()
		portsChanged = true
		applied = append(applied, "removed ports "+strings.Join(diff.RemovePorts, ", "))
	}

	if len(diff.AddPorts) > 0 {
		if _, err := client.AddPorts(ctx, target, diff.AddPorts, false); err != nil {
			finish()
			return err
		}
		s.mu.Lock()
		if s.procs[id] == p && p.state.Status.Active() {
			p.spec.Ports = addPortsToSpec(p.spec.Ports, diff.AddPorts)
		}
		s.mu.Unlock()
		portsChanged = true
		applied = append(applied, "added ports "+strings.Join(diff.AddPorts, ", "))
	}

	for _, m := range diff.RemountMounts {
		if err := client.Remount(ctx, target, m.Target, m.ReadOnly); err != nil {
			finish()
			return err
		}
		s.mu.Lock()
		if s.procs[id] == p && p.state.Status.Active() {
			p.spec.Mounts = upsertMountInSpec(p.spec.Mounts, m)
		}
		s.mu.Unlock()
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		applied = append(applied, fmt.Sprintf("remounted %s (%s)", m.Target, mode))
	}

	for _, rmTarget := range diff.RemoveMounts {
		if err := s.detachMount(ctx, id, p, client, target, rmTarget, false); err != nil {
			finish()
			return err
		}
		applied = append(applied, "unmounted "+rmTarget)
	}

	for _, m := range diff.AddMounts {
		if err := s.attachMount(ctx, id, p, client, target, m); err != nil {
			finish()
			return err
		}
		applied = append(applied, "mounted "+m.Target)
	}

	finish()
	return nil
}
