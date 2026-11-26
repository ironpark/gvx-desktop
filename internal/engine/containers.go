package engine

// Owner: workstream B (containers). Workstream B may add files named
// container_*.go.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	defaultRestartBase  = 100 * time.Millisecond
	defaultRestartMax   = time.Minute
	defaultRestartReset = 10 * time.Second
)

// containerState is what the engine keeps of its containers.
type containerState struct {
	// portMu serializes port probing/allocation and reservation across
	// concurrent startContainer calls. Lock order: portMu before mu.
	portMu sync.Mutex

	mu      sync.Mutex
	byID    map[string]*Container
	closing bool

	// Restart backoff parameters; zero values use the defaults above.
	// Tests may set shorter values on a testEngine.
	restartBase  time.Duration
	restartMax   time.Duration
	restartReset time.Duration
}

func (s *containerState) ensureInit() {
	if s.byID == nil {
		s.byID = map[string]*Container{}
	}
}

func (s *containerState) backoffBase() time.Duration {
	if s.restartBase > 0 {
		return s.restartBase
	}
	return defaultRestartBase
}

func (s *containerState) backoffMax() time.Duration {
	if s.restartMax > 0 {
		return s.restartMax
	}
	return defaultRestartMax
}

func (s *containerState) backoffReset() time.Duration {
	if s.restartReset > 0 {
		return s.restartReset
	}
	return defaultRestartReset
}

// ---- seams: called by the other workstreams; the signatures are frozen ----

// lookup finds a container by full ID, name (with or without the leading
// slash) or unique ID prefix, and returns a copy. Not found is a 404 "No
// such container: ref".
func (e *Engine) lookup(ref string) (Container, error) {
	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	c, err := e.lookupLocked(ref)
	if err != nil {
		return Container{}, err
	}
	return c.clone(), nil
}

func (e *Engine) lookupLocked(ref string) (*Container, error) {
	if ref == "" || len(e.ctr.byID) == 0 {
		return nil, errNotFound("No such container: %s", ref)
	}
	if c, ok := e.ctr.byID[ref]; ok {
		return c, nil
	}
	name := strings.TrimPrefix(ref, "/")
	for _, c := range e.ctr.byID {
		if c.Name == name {
			return c, nil
		}
	}
	var match *Container
	matches := 0
	for id, c := range e.ctr.byID {
		if strings.HasPrefix(id, ref) {
			match = c
			matches++
		}
	}
	if matches == 1 {
		return match, nil
	}
	if matches > 1 {
		return nil, errNotFound("Multiple IDs found with provided prefix: %s", ref)
	}
	return nil, errNotFound("No such container: %s", ref)
}

// allContainers returns a copy of every container, oldest first.
func (e *Engine) allContainers() []Container {
	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	out := make([]Container, 0, len(e.ctr.byID))
	for _, c := range e.ctr.byID {
		out = append(out, c.clone())
	}
	slices.SortFunc(out, func(a, b Container) int {
		if d := a.Created.Compare(b.Created); d != 0 {
			return d
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// saveContainersLocked writes containers.json atomically. Caller holds e.ctr.mu.
func (e *Engine) saveContainersLocked() error {
	if e.cfg.Dir == "" {
		return nil
	}
	list := make([]Container, 0, len(e.ctr.byID))
	for _, c := range e.ctr.byID {
		list = append(list, c.clone())
	}
	slices.SortFunc(list, func(a, b Container) int {
		if d := a.Created.Compare(b.Created); d != 0 {
			return d
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return saveFile(e.path("containers.json"), containerStoreFile{
		Version:    storeVersion,
		Containers: list,
	})
}

// containerEvent takes the runner's events for the containers' sandboxes:
// a run that ended updates the state, emits die, and applies AutoRemove and
// the restart policy. It ignores IDs that are not containers'.
func (e *Engine) containerEvent(ev sandbox.Event) {
	switch ev.Kind {
	case sandbox.EventChanged, sandbox.EventReady:
		changed := false
		e.ctr.mu.Lock()
		if c := e.ctr.byID[ev.ID]; c != nil && c.State.Status == StatusRunning {
			if ev.State.PID != 0 && c.State.Pid != ev.State.PID {
				c.State.Pid = ev.State.PID
				changed = true
			}
			if !ev.State.StartedAt.IsZero() && c.State.StartedAt.IsZero() {
				c.State.StartedAt = ev.State.StartedAt.UTC()
				changed = true
			}
			if ev.State.Status.Active() && c.State.Paused != ev.State.Paused {
				c.State.Paused = ev.State.Paused
				changed = true
			}
		}
		e.ctr.mu.Unlock()
		if changed {
			e.notify()
		}
		return

	case sandbox.EventExited:
		var (
			found         bool
			needStartEmit bool
			startAttrs    map[string]string
			dieAttrs      map[string]string
			autoRemove    bool
			anonVols      []string
			baseAttrs     map[string]string
		)
		e.ctr.mu.Lock()
		c := e.ctr.byID[ev.ID]
		if c != nil {
			found = true
			// Leave the overlay before the record stops running: a start
			// may follow as soon as it does (restart, a restart policy), and
			// its join must not be undone by this exit.
			e.leaveOverlay(ev.ID)
			exitCode := ev.State.ExitCode
			if exitCode == -1 && ev.State.Signal != 0 {
				exitCode = 128 + ev.State.Signal
			}
			finishedAt := ev.State.ExitedAt.UTC()
			if finishedAt.IsZero() {
				finishedAt = time.Now().UTC()
			}
			var runDur time.Duration
			if !c.State.StartedAt.IsZero() {
				runDur = finishedAt.Sub(c.State.StartedAt)
			}

			c.State.ExitCode = exitCode
			c.State.Pid = 0
			c.State.Paused = false
			c.State.FinishedAt = finishedAt
			c.State.Exits++
			if c.State.Health != nil {
				c.State.Health.Status = HealthUnhealthy
			}

			if c.startEmittedGen < c.runGen {
				c.startEmittedGen = c.runGen
				needStartEmit = true
				startAttrs = containerAttrs(c)
			}
			baseAttrs = containerAttrs(c)
			dieAttrs = containerAttrs(c)
			dieAttrs["exitCode"] = strconv.Itoa(exitCode)

			wasRemoving := c.State.Status == StatusRemoving
			closing := e.ctr.closing
			stoppedByUser := c.State.StoppedByUser

			switch {
			case wasRemoving:
				// Removal handler owns the remaining cleanup.
			case c.HostConfig.AutoRemove && !closing:
				c.State.Status = StatusRemoving
				autoRemove = true
				anonVols = slices.Clone(c.AnonymousVolumes)
			case stoppedByUser || closing:
				c.State.Status = StatusExited
			default:
				if !e.scheduleRestartLocked(c, runDur, false) {
					c.State.Status = StatusExited
				}
			}

			if err := e.saveContainersLocked(); err != nil {
				e.logf("engine: save containers.json: %v", err)
			}
		}
		e.ctr.mu.Unlock()
		if testHookExitRecorded != nil {
			testHookExitRecorded()
		}

		if !found {
			return
		}
		e.stopHealthMonitor(ev.ID)
		e.onContainerExitedExecs(ev.ID)
		if needStartEmit {
			e.emit(Event{Type: "container", Action: "start", ID: ev.ID, Attributes: startAttrs})
		}
		e.emit(Event{Type: "container", Action: "die", ID: ev.ID, Attributes: dieAttrs})
		e.notify()
		if autoRemove {
			go e.removeContainerCleanup(context.Background(), ev.ID, false, true, anonVols, baseAttrs)
		}
	}
}

// testHookExitRecorded, when set, runs after an exit is recorded and
// before the rest of its handling; a test widens the window with it.
var testHookExitRecorded func()

func shouldRestart(p RestartPolicy, exitCode, restartCount int) bool {
	switch p.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		if exitCode == 0 {
			return false
		}
		return p.MaximumRetryCount == 0 || restartCount < p.MaximumRetryCount
	default:
		return false
	}
}

// scheduleRestartLocked arms the next backoff timer if c's restart policy
// applies. Caller holds e.ctr.mu.
func (e *Engine) scheduleRestartLocked(c *Container, runDur time.Duration, forceBackoff bool) bool {
	if !shouldRestart(c.HostConfig.RestartPolicy, c.State.ExitCode, c.RestartCount) {
		return false
	}
	if !forceBackoff && runDur >= e.ctr.backoffReset() {
		c.backoff = 0
	}
	delay := c.backoff * 2
	if delay < e.ctr.backoffBase() {
		delay = e.ctr.backoffBase()
	}
	if delay > e.ctr.backoffMax() {
		delay = e.ctr.backoffMax()
	}
	c.backoff = delay
	c.State.Status = StatusRestarting
	c.restartGen++
	rGen := c.restartGen
	if c.restartTimer != nil {
		c.restartTimer.Stop()
	}
	id := c.ID
	c.restartTimer = time.AfterFunc(delay, func() {
		e.doRestart(id, rGen)
	})
	return true
}

func (e *Engine) doRestart(id string, rGen uint64) {
	e.ctr.mu.Lock()
	c := e.ctr.byID[id]
	if c == nil || e.ctr.closing || c.restartGen != rGen || c.State.Status != StatusRestarting || c.State.StoppedByUser {
		e.ctr.mu.Unlock()
		return
	}
	c.RestartCount++
	c.restartTimer = nil
	if err := e.saveContainersLocked(); err != nil {
		e.logf("engine: save containers.json: %v", err)
	}
	e.ctr.mu.Unlock()

	if err := e.startContainer(id); err != nil {
		e.logf("engine: restart %s: %v", id[:min(12, len(id))], err)
		notify := false
		e.ctr.mu.Lock()
		c := e.ctr.byID[id]
		if c != nil && !e.ctr.closing && !c.State.StoppedByUser &&
			c.State.Status != StatusRemoving && c.State.Status != StatusRunning && c.restartTimer == nil {
			c.State.Error = err.Error()
			if !e.scheduleRestartLocked(c, 0, true) {
				c.State.Status = StatusExited
			}
			_ = e.saveContainersLocked()
			notify = true
		}
		e.ctr.mu.Unlock()
		if notify {
			e.notify()
		}
	}
}

// loadContainers reads containers.json. A container that ran when the
// engine closed is exited now.
func (e *Engine) loadContainers() error {
	var f containerStoreFile
	if e.cfg.Dir != "" {
		if err := loadFile(e.path("containers.json"), &f); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	dirty := false

	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	e.ctr.byID = make(map[string]*Container, len(f.Containers))
	e.ctr.closing = false

	for i := range f.Containers {
		c := f.Containers[i]
		if len(c.AnonymousVolumes) > 0 {
			anonSet := make(map[string]bool, len(c.AnonymousVolumes))
			for _, v := range c.AnonymousVolumes {
				anonSet[v] = true
			}
			for j := range c.Mounts {
				if c.Mounts[j].Type == "volume" && anonSet[c.Mounts[j].Name] {
					c.Mounts[j].Anonymous = true
				}
			}
		}
		switch c.State.Status {
		case StatusRunning, StatusRestarting, StatusRemoving:
			c.State.Status = StatusExited
			c.State.Pid = 0
			if !c.State.StoppedByUser {
				c.State.ExitCode = 255
			}
			c.State.FinishedAt = now
			if c.State.Health != nil {
				c.State.Health.Status = HealthUnhealthy
			}
			dirty = true
		}
		cCopy := c
		e.ctr.byID[c.ID] = &cCopy
	}
	if dirty {
		return e.saveContainersLocked()
	}
	return nil
}

// resumeContainers starts the containers whose restart policy says so
// after a start of the engine.
func (e *Engine) resumeContainers(ctx context.Context) {
	var toStart []string
	e.ctr.mu.Lock()
	e.ctr.closing = false
	for _, c := range e.ctr.byID {
		switch c.HostConfig.RestartPolicy.Name {
		case "always":
			toStart = append(toStart, c.ID)
		case "unless-stopped":
			if !c.State.StoppedByUser {
				toStart = append(toStart, c.ID)
			}
		}
	}
	e.ctr.mu.Unlock()

	slices.Sort(toStart)
	for _, id := range toStart {
		if ctx.Err() != nil {
			return
		}
		if err := e.startContainer(id); err != nil {
			e.logf("engine: resume %s: %v", id[:min(12, len(id))], err)
		}
	}
}

// stopContainers stops every running container in parallel, waiting up to
// timeout for each, and records them as stopped by the engine, not by the
// user.
func (e *Engine) stopContainers(timeout time.Duration) {
	type stopTarget struct {
		id        string
		sig       string
		isMicroVM bool
	}
	var running []stopTarget
	dirty := false

	e.ctr.mu.Lock()
	e.ctr.closing = true
	for _, c := range e.ctr.byID {
		c.restartGen++
		if c.restartTimer != nil {
			c.restartTimer.Stop()
			c.restartTimer = nil
		}
		if c.State.Status == StatusRunning {
			sig := strings.TrimSpace(c.Config.StopSignal)
			if sig == "" {
				sig = "SIGTERM"
			}
			running = append(running, stopTarget{id: c.ID, sig: sig, isMicroVM: c.isMicroVM()})
		} else if c.State.Status == StatusRestarting {
			c.State.Status = StatusExited
			dirty = true
		}
	}
	if dirty {
		_ = e.saveContainersLocked()
	}
	e.ctr.mu.Unlock()

	var wg sync.WaitGroup
	for _, item := range running {
		wg.Add(1)
		go func(item stopTarget) {
			defer wg.Done()
			if err := e.ctrShutdown(item.id, item.isMicroVM, item.sig, timeout); err != nil {
				e.logf("engine: stop %s: %v", item.id[:min(12, len(item.id))], err)
			}
		}(item)
	}
	wg.Wait()

	// Wait briefly for EventExited callbacks to settle, then ensure any
	// remaining running records are marked exited without setting StoppedByUser.
	deadline := time.After(200 * time.Millisecond)
	for {
		ch := e.changes()
		stillRunning := false
		e.ctr.mu.Lock()
		for _, item := range running {
			if c := e.ctr.byID[item.id]; c != nil && c.State.Status == StatusRunning {
				stillRunning = true
				break
			}
		}
		e.ctr.mu.Unlock()
		if !stillRunning {
			break
		}
		select {
		case <-ch:
		case <-deadline:
			e.ctr.mu.Lock()
			now := time.Now().UTC()
			for _, item := range running {
				if c := e.ctr.byID[item.id]; c != nil && c.State.Status == StatusRunning {
					c.State.Status = StatusExited
					c.State.Pid = 0
					c.State.Paused = false
					c.State.FinishedAt = now
				}
			}
			_ = e.saveContainersLocked()
			e.ctr.mu.Unlock()
			return
		}
	}
}

// ---- internal lifecycle helpers ----

func containerAttrs(c *Container) map[string]string {
	attrs := make(map[string]string, len(c.Config.Labels)+2)
	maps.Copy(attrs, c.Config.Labels)
	attrs["name"] = c.Name
	attrs["image"] = c.Image
	return attrs
}

// usedPortsLocked returns all host ports currently bound by running containers
// other than skipID. Caller holds e.ctr.mu.
func (e *Engine) usedPortsLocked(skipID string) []hostPortBind {
	var used []hostPortBind
	for id, c := range e.ctr.byID {
		if id == skipID || c.State.Status != StatusRunning {
			continue
		}
		for containerPort, bindings := range c.Ports {
			_, proto, ok := strings.Cut(containerPort, "/")
			if !ok || proto == "" {
				proto = "tcp"
			}
			for _, b := range bindings {
				port, err := strconv.Atoi(b.HostPort)
				if err != nil || port <= 0 {
					continue
				}
				ip, err := netip.ParseAddr(strings.TrimSpace(b.HostIP))
				if err != nil {
					ip = netip.MustParseAddr("127.0.0.1")
				}
				used = append(used, hostPortBind{ip: ip, port: port, proto: proto})
			}
		}
	}
	return used
}

// startContainer resolves ports, starts the sandbox via runner.Start, updates
// state and emits the start event.
func (e *Engine) startContainer(id string) error {
	allowLAN := e.allowLANPorts()

	// Serialize port probing and reservation across concurrent starts so two
	// containers cannot claim the same fixed or dynamic host port.
	e.ctr.portMu.Lock()

	e.ctr.mu.Lock()
	c, ok := e.ctr.byID[id]
	if !ok {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errNotFound("No such container: %s", id)
	}
	if c.State.Status == StatusRunning {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errNotModified
	}
	if c.State.Status == StatusRemoving {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errConflict("container %s is being removed", c.ID)
	}
	// Cancel any pending restart backoff timer.
	c.restartGen++
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	usedByOthers := e.usedPortsLocked(c.ID)
	snapshot := c.clone()
	prevStatus := c.State.Status
	prevStartedAt := c.State.StartedAt
	prevFinishedAt := c.State.FinishedAt
	prevPorts := c.Ports
	prevHealth := c.State.Health.clone()
	e.ctr.mu.Unlock()

	resolvedPorts, specPorts, err := resolveStartPorts(&snapshot, usedByOthers, allowLAN)
	if err != nil {
		e.ctr.portMu.Unlock()
		e.handleStartFailure(id, 0, prevStatus, prevStartedAt, prevFinishedAt, prevPorts, prevHealth, err)
		return err
	}

	hc, _ := parseHealthConfig(snapshot.Config.Healthcheck)
	runHealth := hc.enabled() && (snapshot.isMicroVM() || e.runner.Features().Control)

	spec := snapshot.Spec.Clone()
	spec.Name = snapshot.Name
	spec.Ports = specPorts
	if !snapshot.isMicroVM() && snapshot.HostConfig.PidsLimit != nil && *snapshot.HostConfig.PidsLimit > 0 && e.hasRunFlag("pids-limit") {
		if extra, err := rungv.SplitArgs(spec.ExtraArgs); err == nil {
			extra = append(extra, "-pids-limit", strconv.FormatInt(*snapshot.HostConfig.PidsLimit, 10))
			spec.ExtraArgs = rungv.FormatArgs(extra)
		}
	}

	// Mark running and record reserved ports BEFORE releasing portMu and
	// before calling runner.Start, so concurrent starts see these ports and
	// an instantaneous EventExited cannot be overwritten.
	startedAt := time.Now().UTC()
	e.ctr.mu.Lock()
	c, ok = e.ctr.byID[id]
	if !ok {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errNotFound("No such container: %s", id)
	}
	if c.State.Status == StatusRunning {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errNotModified
	}
	if c.State.Status == StatusRemoving {
		e.ctr.mu.Unlock()
		e.ctr.portMu.Unlock()
		return errConflict("container %s is being removed", c.ID)
	}
	c.runGen++
	gen := c.runGen
	c.State.Status = StatusRunning
	c.State.Paused = false
	c.State.StartedAt = startedAt
	c.State.FinishedAt = time.Time{}
	c.State.Error = ""
	c.State.StoppedByUser = false
	if runHealth {
		c.State.Health = &ContainerHealth{
			Status: HealthStarting,
			Log:    []HealthcheckResult{},
		}
	} else {
		c.State.Health = nil
	}
	c.Ports = resolvedPorts
	e.ctr.mu.Unlock()

	e.ctr.portMu.Unlock()

	if snapshot.isMicroVM() {
		if err := e.microVMStart(&snapshot, gen, resolvedPorts); err != nil {
			e.handleStartFailure(id, gen, prevStatus, prevStartedAt, prevFinishedAt, prevPorts, prevHealth, err)
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "volume") && (strings.Contains(msg, "in use") || strings.Contains(msg, "locked")) {
				return errConflict("%s", err.Error())
			}
			return err
		}
	} else {
		spec = e.joinOverlay(&snapshot, spec)
		if err := e.runner.Start(spec); err != nil {
			e.leaveOverlay(id)
			e.handleStartFailure(id, gen, prevStatus, prevStartedAt, prevFinishedAt, prevPorts, prevHealth, err)
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "volume") && (strings.Contains(msg, "in use") || strings.Contains(msg, "locked")) {
				return errConflict("%s", err.Error())
			}
			return err
		}
	}

	if snapshot.Config.Tty && snapshot.HostConfig.ConsoleSize[0] > 0 && snapshot.HostConfig.ConsoleSize[1] > 0 {
		_ = e.ctrResize(snapshot, uint16(snapshot.HostConfig.ConsoleSize[0]), uint16(snapshot.HostConfig.ConsoleSize[1]))
	}

	st := e.ctrState(snapshot)
	var (
		emitStart       bool
		stillRunningGen bool
		attrs           map[string]string
	)
	e.ctr.mu.Lock()
	if c := e.ctr.byID[id]; c != nil && c.runGen == gen {
		if c.State.Status == StatusRunning {
			stillRunningGen = true
			if st.PID != 0 {
				c.State.Pid = st.PID
			}
			if !st.StartedAt.IsZero() {
				startedAt = st.StartedAt.UTC()
				c.State.StartedAt = startedAt
			}
		}
		if c.startEmittedGen < gen {
			c.startEmittedGen = gen
			emitStart = true
			attrs = containerAttrs(c)
		}
		_ = e.saveContainersLocked()
	}
	e.ctr.mu.Unlock()

	if runHealth && stillRunningGen {
		e.startHealthMonitor(id, gen, hc, startedAt)
	}
	if emitStart {
		e.emit(Event{Type: "container", Action: "start", ID: id, Attributes: attrs})
	}
	e.notify()
	return nil
}

// handleStartFailure records startErr on the container and, when AutoRemove
// is set, removes the container and its anonymous volumes so wait?condition=removed
// and attach do not hang.
func (e *Engine) handleStartFailure(id string, gen uint64, prevStatus string, prevStartedAt, prevFinishedAt time.Time, prevPorts map[string][]PortBinding, prevHealth *ContainerHealth, startErr error) {
	e.stopHealthMonitor(id)
	var (
		autoRemove bool
		anonVols   []string
		attrs      map[string]string
		updated    bool
	)
	e.ctr.mu.Lock()
	if c := e.ctr.byID[id]; c != nil && c.State.Status != StatusRemoving && (gen == 0 || c.runGen == gen) {
		updated = true
		c.State.Error = startErr.Error()
		c.State.Pid = 0
		c.Ports = prevPorts
		c.State.Health = prevHealth
		if c.HostConfig.AutoRemove && !e.ctr.closing {
			c.State.Status = StatusRemoving
			c.State.StoppedByUser = true
			anonVols = slices.Clone(c.AnonymousVolumes)
			attrs = containerAttrs(c)
			autoRemove = true
		} else {
			if prevStatus == StatusCreated {
				c.State.Status = StatusCreated
			} else {
				c.State.Status = StatusExited
			}
			c.State.StartedAt = prevStartedAt
			c.State.FinishedAt = prevFinishedAt
		}
		_ = e.saveContainersLocked()
	}
	e.ctr.mu.Unlock()
	if !updated {
		return
	}
	e.notify()
	if autoRemove {
		e.removeContainerCleanup(context.Background(), id, false, true, anonVols, attrs)
	}
}

// stopContainerByRef stops a container by ref with signal and timeout. It
// returns errNotModified if the container is already stopped.
func (e *Engine) stopContainerByRef(ctx context.Context, ref string, signal string, timeout time.Duration, hasExplicitTimeout bool) error {
	e.ctr.mu.Lock()
	c, err := e.lookupLocked(ref)
	if err != nil {
		e.ctr.mu.Unlock()
		return err
	}
	id := c.ID
	sig := strings.TrimSpace(signal)
	if sig == "" {
		sig = strings.TrimSpace(c.Config.StopSignal)
	}
	if sig == "" {
		sig = "SIGTERM"
	}
	if !hasExplicitTimeout {
		if c.Config.StopTimeout != nil {
			if *c.Config.StopTimeout < 0 {
				timeout = 24 * time.Hour
			} else {
				timeout = time.Duration(*c.Config.StopTimeout) * time.Second
			}
		} else {
			timeout = 10 * time.Second
		}
	}
	c.restartGen++
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	if c.State.Status == StatusRestarting {
		c.State.Status = StatusExited
		c.State.StoppedByUser = true
		attrs := containerAttrs(c)
		_ = e.saveContainersLocked()
		e.ctr.mu.Unlock()
		e.emit(Event{Type: "container", Action: "stop", ID: id, Attributes: attrs})
		e.notify()
		return nil
	}
	if c.State.Status != StatusRunning {
		e.ctr.mu.Unlock()
		return errNotModified
	}
	c.State.StoppedByUser = true
	isMicroVM := c.isMicroVM()
	attrs := containerAttrs(c)
	_ = e.saveContainersLocked()
	e.ctr.mu.Unlock()

	if err := e.ctrShutdown(id, isMicroVM, sig, timeout); err != nil {
		return err
	}
	e.waitNotRunning(ctx, id)
	e.emit(Event{Type: "container", Action: "stop", ID: id, Attributes: attrs})
	return nil
}

// waitNotRunning waits briefly for EventExited to transition id out of
// Running after runner.Stop returns.
func (e *Engine) waitNotRunning(ctx context.Context, id string) {
	deadline := time.After(time.Second)
	for {
		ch := e.changes()
		e.ctr.mu.Lock()
		c := e.ctr.byID[id]
		done := c == nil || c.State.Status != StatusRunning
		e.ctr.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		case <-deadline:
			return
		}
	}
}

// removeContainerCleanup finishes removing a container whose status is already
// StatusRemoving.
func (e *Engine) removeContainerCleanup(ctx context.Context, id string, stopFirst, removeVols bool, anonVols []string, attrs map[string]string) {
	isMicroVM := e.isMicroVMContainer(id)
	e.stopHealthMonitor(id)
	if stopFirst {
		_ = e.ctrShutdown(id, isMicroVM, "SIGKILL", 0)
		e.waitNotRunning(ctx, id)
	}
	e.dropExecsForContainer(id)
	e.ctrForget(id, isMicroVM)
	e.dropLogs(id)
	e.leaveOverlay(id)
	e.disconnectNetworks(id)
	e.syncOverlay()

	e.ctr.mu.Lock()
	removed := e.ctr.byID[id]
	delete(e.ctr.byID, id)
	_ = e.saveContainersLocked()
	e.ctr.mu.Unlock()
	if removed != nil {
		if removed.isMicroVM() {
			e.removeMicroVMRootfs(removed.ID)
		} else {
			e.removeRootfs(*removed) // a crash before this leaves it to sweepRootfs
		}
	}

	if removeVols {
		for _, v := range anonVols {
			_ = e.removeVolume(ctx, v, false)
		}
	}

	e.emit(Event{Type: "container", Action: "destroy", ID: id, Attributes: attrs})
	e.notify()
}

// ---- handlers ----

func (e *Engine) containerCreate(w http.ResponseWriter, r *http.Request) {
	var req containerCreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		writeError(w, r, errInvalid("No image specified"))
		return
	}

	reqName := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("name")), "/")
	if strings.HasPrefix(reqName, "buildx_buildkit_") || isBuildkitImage(req.Image) {
		writeError(w, r, errBuildDriverNotSupported())
		return
	}
	if r.URL.Query().Get("name") != "" && !validContainerName(reqName) {
		writeError(w, r, errInvalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", reqName))
		return
	}

	// Quick pre-check for name conflict before touching volumes/networks.
	if reqName != "" {
		e.ctr.mu.Lock()
		for _, existing := range e.ctr.byID {
			if existing.Name == reqName {
				id := existing.ID
				e.ctr.mu.Unlock()
				writeError(w, r, nameConflictErr(reqName, id))
				return
			}
		}
		e.ctr.mu.Unlock()
	}

	// Resolve runtime and image according to the §3.2 matrix (400 on invalid /
	// disabled runtime or refused foreign arch; 404 on uncached allowed image
	// so Docker CLI auto-pulls).
	reqRuntime := strings.TrimSpace(req.HostConfig.Runtime)
	if reqRuntime == "" && req.Labels != nil {
		reqRuntime = strings.TrimSpace(req.Labels["gvx.runtime"])
	}
	img, targetRuntime, targetPlatform, err := e.resolveCreateImage(r.Context(), req.Image, r.URL.Query().Get("platform"), reqRuntime)
	if err != nil {
		writeError(w, r, err)
		return
	}
	isMicroVM := targetRuntime == RuntimeMicroVM

	id := randomHex64()
	cfg, argv, warnings, err := mergeContainerConfig(id, &req, img)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if hc, _ := parseHealthConfig(cfg.Healthcheck); hc.enabled() && !isMicroVM && !e.runner.Features().Control {
		warnings = append(warnings, "Healthcheck requires rungv 0.20 and is ignored by GVX Desktop's experimental sandbox engine")
	}

	hc := req.HostConfig
	if isMicroVM {
		hc.Runtime = RuntimeMicroVM
	}
	if err := validateRestartPolicy(hc); err != nil {
		writeError(w, r, err)
		return
	}

	// Validate ExtraHosts; netinit adds them to /etc/hosts at each start.
	if _, err := extraHostLines(hc.ExtraHosts); err != nil {
		writeError(w, r, errInvalid("%v", err))
		return
	}

	// Determine target networks and validate they exist.
	type netTarget struct {
		ref     string
		aliases []string
		ipv4    string
	}
	var targets []netTarget
	seenNet := map[string]bool{}
	addNet := func(ref string, ep *endpointConfig) {
		if ref == "" || ref == "default" {
			ref = "bridge"
		}
		if seenNet[ref] {
			return
		}
		seenNet[ref] = true
		var aliases []string
		if ep != nil {
			aliases = ep.Aliases
		}
		targets = append(targets, netTarget{ref: ref, aliases: slices.Clone(aliases), ipv4: ep.ipv4()})
	}

	netMode := strings.TrimSpace(hc.NetworkMode)
	if strings.HasPrefix(netMode, "container:") {
		writeError(w, r, errInvalid("container network mode is not supported"))
		return
	}
	if netMode == "host" {
		warnings = append(warnings, "Host networking is not supported; running on the default NAT network instead")
	}
	var endpoints map[string]*endpointConfig
	if req.NetworkingConfig != nil {
		endpoints = req.NetworkingConfig.EndpointsConfig
	}
	switch netMode {
	case "", "default":
		if len(endpoints) == 0 {
			hc.NetworkMode = "bridge"
			addNet("bridge", nil)
		} else {
			keys := slices.Sorted(maps.Keys(endpoints))
			if len(keys) == 1 && keys[0] != "" && keys[0] != "default" {
				hc.NetworkMode = keys[0]
			} else {
				hc.NetworkMode = "bridge"
			}
			for _, k := range keys {
				addNet(k, endpoints[k])
			}
		}
	default:
		hc.NetworkMode = netMode
		addNet(netMode, endpoints[netMode])
		for _, k := range slices.Sorted(maps.Keys(endpoints)) {
			if k == netMode {
				continue
			}
			addNet(k, endpoints[k])
		}
	}

	type resolvedNet struct {
		net     Network
		aliases []string
		ipv4    string
	}
	resolvedNets := make([]resolvedNet, 0, len(targets))
	for _, t := range targets {
		n, err := e.network(t.ref)
		if err != nil {
			writeError(w, r, err)
			return
		}
		resolvedNets = append(resolvedNets, resolvedNet{net: n, aliases: t.aliases, ipv4: t.ipv4})
	}

	// Validate ports after normalizing NetworkMode.
	portWarnings, err := validatePortBindings(&hc, e.allowLANPorts())
	if err != nil {
		writeError(w, r, err)
		return
	}
	warnings = append(warnings, portWarnings...)
	if isMicroVM {
		hasUDP := false
		for k := range hc.PortBindings {
			if strings.HasSuffix(k, "/udp") {
				hasUDP = true
				break
			}
		}
		if !hasUDP && hc.PublishAllPorts {
			for k := range cfg.ExposedPorts {
				if strings.HasSuffix(k, "/udp") {
					hasUDP = true
					break
				}
			}
		}
		if hasUDP {
			warnings = append(warnings, "UDP port publishing is not supported by the micro-VM runtime yet; only TCP ports will be forwarded")
		}
	}

	// Plan and validate mounts before creating any volumes.
	feat := e.runner.Features()
	mountOpts := mountPlanOptions{
		unixForward:  !isMicroVM && feat.UnixForward,
		dockerSocket: e.cfg.DockerSocket,
		sshAuthSock:  e.sshAuthSock(),
		bindSymlinks: !isMicroVM && e.bindSymlinks() && feat.BindSymlinks,
	}
	if len(hc.VolumesFrom) > 0 {
		inherited, err := resolveVolumesFrom(hc.VolumesFrom, e.lookup, mountOpts)
		if err != nil {
			writeError(w, r, err)
			return
		}
		mountOpts.volumesFrom = inherited
	}
	planned, err := planMountsWithOptions(hc, cfg, mountOpts)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Validate spec early (using a temporary name if unnamed).
	specImage := img.ResolvedRef
	if specImage == "" {
		specImage = cfg.Image
	}
	tmpName := reqName
	if tmpName == "" {
		tmpName = "container"
	}
	tmpMounts := make([]sandbox.Mount, 0, len(planned))
	var tmpForwards []string
	for _, pm := range planned {
		if pm.unixForward != "" {
			tmpForwards = append(tmpForwards, pm.unixForward)
		} else {
			tmpMounts = append(tmpMounts, pm.spec)
		}
	}
	if _, err := buildSandboxSpecWithForwards(id, tmpName, specImage, cfg, hc, argv, tmpMounts, tmpForwards); err != nil {
		writeError(w, r, err)
		return
	}

	// Materialize volumes (calls workstream C seam, holding no lock).
	specMounts, unixForwards, mountPoints, anonVols, err := e.materializeMounts(r.Context(), planned)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cleanupAnon := func() {
		for _, v := range anonVols {
			_ = e.removeVolume(r.Context(), v, true)
		}
	}

	// Reserve the container name and record under lock.
	created := time.Now().UTC()
	e.ctr.mu.Lock()
	e.ctr.ensureInit()
	name := reqName
	if name != "" {
		for _, existing := range e.ctr.byID {
			if existing.Name == name {
				existingID := existing.ID
				e.ctr.mu.Unlock()
				cleanupAnon()
				writeError(w, r, nameConflictErr(name, existingID))
				return
			}
		}
	} else {
		name = generateContainerName(func(cand string) bool {
			for _, existing := range e.ctr.byID {
				if existing.Name == cand {
					return true
				}
			}
			return false
		})
	}
	spec, err := buildSandboxSpecWithForwards(id, name, specImage, cfg, hc, argv, specMounts, unixForwards)
	if err != nil {
		e.ctr.mu.Unlock()
		cleanupAnon()
		writeError(w, r, err)
		return
	}
	spec.CreatedAt = created
	if !isMicroVM && e.keepsRootfs(hc) {
		// rungv makes the volume at the first start.
		if err := os.MkdirAll(e.rootfsDir(), 0o700); err != nil {
			e.ctr.mu.Unlock()
			cleanupAnon()
			writeError(w, r, err)
			return
		}
		spec.RootfsVolume = e.rootfsPath(id)
	}

	storedRuntime := ""
	if isMicroVM {
		storedRuntime = RuntimeMicroVM
	}
	c := &Container{
		ID:               id,
		Name:             name,
		Created:          created,
		Image:            req.Image,
		ImageID:          img.ID,
		Config:           cfg,
		HostConfig:       hc,
		Networks:         map[string]EndpointSettings{},
		Mounts:           mountPoints,
		AnonymousVolumes: anonVols,
		Spec:             spec,
		Platform:         targetPlatform,
		Runtime:          storedRuntime,
		State: ContainerState{
			Status: StatusCreated,
		},
	}
	e.ctr.byID[id] = c
	e.ctr.mu.Unlock()

	// Connect to networks (workstream C seam, holding no lock).
	netSettings := make(map[string]EndpointSettings, len(resolvedNets))
	for _, rn := range resolvedNets {
		ep, err := e.connectNetwork(rn.net.Name, id, rn.aliases, rn.ipv4)
		if err != nil {
			e.disconnectNetworks(id)
			e.ctr.mu.Lock()
			delete(e.ctr.byID, id)
			e.ctr.mu.Unlock()
			cleanupAnon()
			writeError(w, r, err)
			return
		}
		ep.EndpointID = randomHex64()
		ep.DNSNames = buildDNSNames(name, id[:12], rn.aliases)
		if ep.IPAddress != "" {
			ep.MacAddress = containerMAC(id)
		}
		netSettings[rn.net.Name] = ep
	}
	if !isMicroVM {
		for _, rn := range resolvedNets {
			if w := overlayWarning(rn.net); w != "" {
				warnings = append(warnings, w)
				break
			}
		}
	}

	e.ctr.mu.Lock()
	if cur := e.ctr.byID[id]; cur != nil {
		cur.Networks = netSettings
		if err := e.saveContainersLocked(); err != nil {
			e.logf("engine: save containers.json: %v", err)
		}
	}
	attrs := containerAttrs(c)
	e.ctr.mu.Unlock()

	e.emit(Event{Type: "container", Action: "create", ID: id, Attributes: attrs})
	e.notify()

	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusCreated, containerCreateResponse{ID: id, Warnings: warnings})
}

func nameConflictErr(name, existingID string) error {
	return errConflict(
		`Conflict. The container name "/%s" is already in use by container "%s". You have to remove (or rename) that container to be able to reuse that name.`,
		name, existingID,
	)
}

func isBuildkitImage(ref string) bool {
	s := strings.TrimSpace(ref)
	s = strings.TrimPrefix(s, "docker.io/")
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s == "moby/buildkit"
}

func buildDNSNames(name, shortID string, aliases []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(name)
	for _, a := range aliases {
		add(a)
	}
	add(shortID)
	return out
}

func containerMAC(id string) string {
	if len(id) < 8 {
		return "02:42:0a:00:02:0f"
	}
	return fmt.Sprintf("02:42:%s:%s:%s:%s", id[0:2], id[2:4], id[4:6], id[6:8])
}

func (e *Engine) containerSizeRw(c Container) int64 {
	if c.isMicroVM() {
		p := e.microVMRootfsPath(c.ID)
		if p == "" {
			return 0
		}
		return allocatedFileBytes(p)
	}
	if !e.ownsRootfs(c.ID, c.Spec.RootfsVolume) {
		return 0
	}
	return allocatedFileBytes(c.Spec.RootfsVolume)
}

func (e *Engine) containerList(w http.ResponseWriter, r *http.Request) {
	all := queryBool(r, "all")
	showSize := queryBool(r, "size")
	limit, err := queryInt(r, "limit", 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	nameRegexes := make([]*regexp.Regexp, 0, len(f["name"]))
	for _, pat := range f["name"] {
		re, err := regexp.Compile(pat)
		if err != nil {
			writeError(w, r, errInvalid("invalid name filter %q: %v", pat, err))
			return
		}
		nameRegexes = append(nameRegexes, re)
	}

	var beforeID, sinceID string
	if vals := f["before"]; len(vals) > 0 {
		anchor, err := e.lookup(vals[0])
		if err != nil {
			writeError(w, r, err)
			return
		}
		beforeID = anchor.ID
	}
	if vals := f["since"]; len(vals) > 0 {
		anchor, err := e.lookup(vals[0])
		if err != nil {
			writeError(w, r, err)
			return
		}
		sinceID = anchor.ID
	}

	showAll := all || limit > 0 || f.has("status") || f.has("exited")

	containers := e.allContainers()
	// Newest first.
	slices.Reverse(containers)

	beforeIdx, sinceIdx := -1, -1
	if beforeID != "" || sinceID != "" {
		for idx, c := range containers {
			if c.ID == beforeID {
				beforeIdx = idx
			}
			if c.ID == sinceID {
				sinceIdx = idx
			}
		}
	}

	var imgSizes map[string]int64
	if showSize {
		if imgs, err := e.buildImageSummaries(r.Context()); err == nil {
			imgSizes = make(map[string]int64, len(imgs))
			for _, img := range imgs {
				imgSizes[img.ID] = img.Size
			}
		}
	}

	now := time.Now().UTC()
	out := make([]containerListItem, 0, len(containers))
	for idx, c := range containers {
		if beforeIdx >= 0 && idx <= beforeIdx {
			continue
		}
		if sinceIdx >= 0 && idx >= sinceIdx {
			continue
		}
		c.State.Paused = e.isContainerPaused(c)
		if !showAll && !c.State.Running() {
			continue
		}
		if !matchContainerFilters(c, f, nameRegexes) {
			continue
		}
		item := formatContainerListItem(c, now)
		if showSize {
			rw := e.containerSizeRw(c)
			item.SizeRw = rw
			item.SizeRootFs = imgSizes[c.ImageID] + rw
		}
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func matchContainerFilters(c Container, f filters, nameRegexes []*regexp.Regexp) bool {
	if !f.matchLabels(c.Config.Labels) {
		return false
	}
	if !f.matchAny("id", func(v string) bool { return strings.HasPrefix(c.ID, v) }) {
		return false
	}
	if len(nameRegexes) > 0 {
		matched := false
		slashName := "/" + c.Name
		for _, re := range nameRegexes {
			if re.MatchString(slashName) || re.MatchString(c.Name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	effectiveStatus := c.State.Status
	if c.State.Status == StatusRunning && c.State.Paused {
		effectiveStatus = StatusPaused
	}
	if !f.matchAny("status", func(v string) bool { return effectiveStatus == v }) {
		return false
	}
	if !f.matchAny("exited", func(v string) bool {
		return c.State.Status == StatusExited && strconv.Itoa(c.State.ExitCode) == v
	}) {
		return false
	}
	if !f.matchAny("ancestor", func(v string) bool {
		if c.Image == v || c.ImageID == v || strings.HasPrefix(c.ImageID, v) {
			return true
		}
		if strings.TrimPrefix(c.ImageID, "sha256:") == strings.TrimPrefix(v, "sha256:") {
			return true
		}
		if !strings.Contains(v, ":") && c.Image == v+":latest" {
			return true
		}
		if !strings.Contains(c.Image, ":") && v == c.Image+":latest" {
			return true
		}
		return false
	}) {
		return false
	}
	if !f.matchAny("network", func(v string) bool {
		for name, ep := range c.Networks {
			if name == v || ep.NetworkID == v || strings.HasPrefix(ep.NetworkID, v) {
				return true
			}
		}
		return false
	}) {
		return false
	}
	if !f.matchAny("volume", func(v string) bool {
		for _, m := range c.Mounts {
			if m.Name == v || m.Source == v || m.Destination == v {
				return true
			}
		}
		return false
	}) {
		return false
	}
	if !f.matchAny("health", func(v string) bool {
		cur := HealthNone
		if c.State.Health != nil && c.State.Health.Status != "" {
			cur = c.State.Health.Status
		}
		return cur == v
	}) {
		return false
	}
	return true
}

func formatContainerListItem(c Container, now time.Time) containerListItem {
	argv := make([]string, 0, len(c.Config.Entrypoint)+len(c.Config.Cmd))
	argv = append(argv, c.Config.Entrypoint...)
	argv = append(argv, c.Config.Cmd...)

	labels := c.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	networks := c.Networks
	if networks == nil {
		networks = map[string]EndpointSettings{}
	}
	mounts := c.Mounts
	if mounts == nil {
		mounts = []MountPoint{}
	}

	ports := []containerListPort{}
	if c.State.Status == StatusRunning {
		published := map[string]bool{}
		for containerPort, bindings := range c.Ports {
			priv, proto := splitPortProto(containerPort)
			for _, b := range bindings {
				pub, _ := strconv.Atoi(b.HostPort)
				ports = append(ports, containerListPort{
					IP:          b.HostIP,
					PrivatePort: uint16(priv),
					PublicPort:  uint16(pub),
					Type:        proto,
				})
				published[containerPort] = true
			}
		}
		for ep := range c.Config.ExposedPorts {
			if published[ep] {
				continue
			}
			priv, proto := splitPortProto(ep)
			ports = append(ports, containerListPort{
				PrivatePort: uint16(priv),
				Type:        proto,
			})
		}
		slices.SortFunc(ports, func(a, b containerListPort) int {
			if d := cmp.Compare(a.PrivatePort, b.PrivatePort); d != 0 {
				return d
			}
			if d := cmp.Compare(a.Type, b.Type); d != 0 {
				return d
			}
			if d := cmp.Compare(a.PublicPort, b.PublicPort); d != 0 {
				return d
			}
			return cmp.Compare(a.IP, b.IP)
		})
	}

	stateWord := c.State.Status
	if c.State.Status == StatusRunning && c.State.Paused {
		stateWord = StatusPaused
	}
	return containerListItem{
		ID:      c.ID,
		Names:   []string{"/" + c.Name},
		Image:   c.Image,
		ImageID: c.ImageID,
		Command: strings.Join(argv, " "),
		Created: c.Created.Unix(),
		Ports:   ports,
		Labels:  labels,
		State:   stateWord,
		Status:  containerStatusText(c.State, now),
		HostConfig: listHostConfig{
			NetworkMode: c.HostConfig.NetworkMode,
		},
		NetworkSettings: listNetworkSettings{
			Networks: networks,
		},
		Mounts: mounts,
	}
}

func splitPortProto(s string) (int, string) {
	pStr, proto, ok := strings.Cut(s, "/")
	if !ok || proto == "" {
		proto = "tcp"
	}
	n, _ := strconv.Atoi(pStr)
	return n, proto
}

func (e *Engine) containerInspect(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	c, err := e.lookup(ref)
	if err != nil {
		if statusOf(err) == http.StatusNotFound && strings.HasPrefix(strings.TrimPrefix(ref, "/"), "buildx_buildkit_") {
			writeError(w, r, errBuildDriverNotSupported())
			return
		}
		writeError(w, r, err)
		return
	}

	argv := make([]string, 0, len(c.Config.Entrypoint)+len(c.Config.Cmd))
	argv = append(argv, c.Config.Entrypoint...)
	argv = append(argv, c.Config.Cmd...)
	pathStr := ""
	args := []string{}
	if len(argv) > 0 {
		pathStr = argv[0]
		if len(argv) > 1 {
			args = argv[1:]
		}
	}

	if c.Config.Labels == nil {
		c.Config.Labels = map[string]string{}
	}
	mounts := c.Mounts
	if mounts == nil {
		mounts = []MountPoint{}
	}
	networks := c.Networks
	if networks == nil {
		networks = map[string]EndpointSettings{}
	}

	inspectPorts := map[string][]PortBinding{}
	gateway, ipAddr, macAddr := "", "", ""
	prefixLen := 0
	if c.State.Status == StatusRunning {
		for k, v := range c.Ports {
			inspectPorts[k] = slices.Clone(v)
		}
		for ep := range c.Config.ExposedPorts {
			if _, ok := inspectPorts[ep]; !ok {
				inspectPorts[ep] = nil
			}
		}
		for _, k := range slices.Sorted(maps.Keys(networks)) {
			if k == "none" {
				continue
			}
			ep := networks[k]
			gateway = ep.Gateway
			ipAddr = ep.IPAddress
			prefixLen = ep.IPPrefixLen
			macAddr = ep.MacAddress
			break
		}
	}

	logPath := ""
	if e.cfg.Dir != "" {
		logPath = e.path("logs", c.ID+".log")
	}

	paused := e.isContainerPaused(c)
	stateWord := c.State.Status
	if paused {
		stateWord = StatusPaused
	}

	var sizeRw, sizeRootFs *int64
	if queryBool(r, "size") {
		rw := e.containerSizeRw(c)
		rootFS := rw
		if img, _, err := e.lookupImage(r.Context(), c.ImageID); err == nil {
			rootFS += img.Size
		}
		sizeRw = &rw
		sizeRootFs = &rootFS
	}

	driver := "gvx"
	inspectHC := c.HostConfig
	if c.isMicroVM() {
		driver = "gvx-microvm"
		inspectHC.Runtime = RuntimeMicroVM
	}

	resp := containerInspectResponse{
		ID:      c.ID,
		Created: formatInspectTime(c.Created),
		Path:    pathStr,
		Args:    args,
		State: inspectState{
			Status:     stateWord,
			Running:    c.State.Running(),
			Paused:     paused,
			Restarting: c.State.Status == StatusRestarting,
			OOMKilled:  c.State.OOMKilled,
			Dead:       c.State.Status == StatusDead,
			Pid:        c.State.Pid,
			ExitCode:   c.State.ExitCode,
			Error:      c.State.Error,
			StartedAt:  formatInspectTime(c.State.StartedAt),
			FinishedAt: formatInspectTime(c.State.FinishedAt),
			Health:     c.State.Health.clone(),
		},
		Image:        c.ImageID,
		LogPath:      logPath,
		Name:         "/" + c.Name,
		RestartCount: c.RestartCount,
		Driver:       driver,
		Platform:     "linux",
		ExecIDs:      e.execIDsForContainer(c.ID),
		HostConfig:   inspectHC,
		GraphDriver:  inspectGraphDriver{Name: driver},
		SizeRw:       sizeRw,
		SizeRootFs:   sizeRootFs,
		Mounts:       mounts,
		Config:       c.Config,
		NetworkSettings: inspectNetworkSettings{
			Ports:       inspectPorts,
			Gateway:     gateway,
			IPAddress:   ipAddr,
			IPPrefixLen: prefixLen,
			MacAddress:  macAddr,
			Networks:    networks,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) containerStart(w http.ResponseWriter, r *http.Request) {
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := e.startContainer(c.ID); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerStop(w http.ResponseWriter, r *http.Request) {
	sigRaw := strings.TrimSpace(r.URL.Query().Get("signal"))
	if sigRaw != "" {
		if _, err := parseSignal(sigRaw); err != nil {
			writeError(w, r, err)
			return
		}
	}
	hasT := strings.TrimSpace(r.URL.Query().Get("t")) != ""
	tSec, err := queryInt(r, "t", 10)
	if err != nil {
		writeError(w, r, err)
		return
	}
	timeout := time.Duration(tSec) * time.Second
	if tSec < 0 {
		timeout = 24 * time.Hour
	}
	if err := e.stopContainerByRef(r.Context(), r.PathValue("id"), sigRaw, timeout, hasT); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerRestart(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	c, err := e.lookup(ref)
	if err != nil {
		writeError(w, r, err)
		return
	}
	sigRaw := strings.TrimSpace(r.URL.Query().Get("signal"))
	if sigRaw != "" {
		if _, err := parseSignal(sigRaw); err != nil {
			writeError(w, r, err)
			return
		}
	}
	hasT := strings.TrimSpace(r.URL.Query().Get("t")) != ""
	tSec, err := queryInt(r, "t", 10)
	if err != nil {
		writeError(w, r, err)
		return
	}
	timeout := time.Duration(tSec) * time.Second
	if tSec < 0 {
		timeout = 24 * time.Hour
	}
	if err := e.stopContainerByRef(r.Context(), c.ID, sigRaw, timeout, hasT); err != nil && err != errNotModified {
		writeError(w, r, err)
		return
	}
	if err := e.startContainer(c.ID); err != nil {
		writeError(w, r, err)
		return
	}
	e.ctr.mu.Lock()
	var attrs map[string]string
	if cur := e.ctr.byID[c.ID]; cur != nil {
		attrs = containerAttrs(cur)
	}
	e.ctr.mu.Unlock()
	if attrs != nil {
		e.emit(Event{Type: "container", Action: "restart", ID: c.ID, Attributes: attrs})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerKill(w http.ResponseWriter, r *http.Request) {
	sig, err := parseSignal(r.URL.Query().Get("signal"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	ref := r.PathValue("id")
	e.ctr.mu.Lock()
	c, err := e.lookupLocked(ref)
	if err != nil {
		e.ctr.mu.Unlock()
		writeError(w, r, err)
		return
	}
	if c.State.Status == StatusRestarting {
		id := c.ID
		c.restartGen++
		if c.restartTimer != nil {
			c.restartTimer.Stop()
			c.restartTimer = nil
		}
		c.State.Status = StatusExited
		c.State.StoppedByUser = true
		attrs := containerAttrs(c)
		attrs["signal"] = strconv.Itoa(int(sig))
		_ = e.saveContainersLocked()
		e.ctr.mu.Unlock()
		e.emit(Event{Type: "container", Action: "kill", ID: id, Attributes: attrs})
		e.notify()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if c.State.Status != StatusRunning {
		id := c.ID
		e.ctr.mu.Unlock()
		writeError(w, r, errConflict("Cannot kill container: %s: container %s is not running", ref, id))
		return
	}
	id := c.ID
	wasPaused := c.State.Paused
	c.restartGen++
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	c.State.StoppedByUser = true
	cCopy := c.clone()
	attrs := containerAttrs(c)
	attrs["signal"] = strconv.Itoa(int(sig))
	_ = e.saveContainersLocked()
	e.ctr.mu.Unlock()

	if wasPaused || e.ctrState(cCopy).Paused {
		if err := e.ctrUnpause(r.Context(), cCopy); err == nil {
			e.ctr.mu.Lock()
			if cur := e.ctr.byID[id]; cur != nil {
				cur.State.Paused = false
			}
			e.ctr.mu.Unlock()
		}
	}

	if err := e.ctrSignal(cCopy, sig); err != nil {
		writeError(w, r, errConflict("Cannot kill container: %s: %v", ref, err))
		return
	}
	e.emit(Event{Type: "container", Action: "kill", ID: id, Attributes: attrs})
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerRename(w http.ResponseWriter, r *http.Request) {
	newName := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("name")), "/")
	if !validContainerName(newName) {
		writeError(w, r, errInvalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", newName))
		return
	}
	ref := r.PathValue("id")
	e.ctr.mu.Lock()
	c, err := e.lookupLocked(ref)
	if err != nil {
		e.ctr.mu.Unlock()
		writeError(w, r, err)
		return
	}
	if c.Name == newName {
		e.ctr.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, other := range e.ctr.byID {
		if other.ID != c.ID && other.Name == newName {
			otherID := other.ID
			e.ctr.mu.Unlock()
			writeError(w, r, nameConflictErr(newName, otherID))
			return
		}
	}
	oldName := c.Name
	c.Name = newName
	c.Spec.Name = newName
	for netName, ep := range c.Networks {
		ep.DNSNames = buildDNSNames(newName, shortHex(c.ID), ep.Aliases)
		c.Networks[netName] = ep
	}
	id := c.ID
	attrs := containerAttrs(c)
	attrs["oldName"] = "/" + oldName
	if err := e.saveContainersLocked(); err != nil {
		e.logf("engine: save containers.json: %v", err)
	}
	e.ctr.mu.Unlock()

	e.syncOverlay()
	e.emit(Event{Type: "container", Action: "rename", ID: id, Attributes: attrs})
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerDelete(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	force := queryBool(r, "force")
	removeVols := queryBool(r, "v")

	e.ctr.mu.Lock()
	c, err := e.lookupLocked(ref)
	if err != nil {
		e.ctr.mu.Unlock()
		writeError(w, r, err)
		return
	}
	if c.State.Status == StatusRemoving {
		e.ctr.mu.Unlock()
		writeError(w, r, errConflict("removal of container %s is already in progress", ref))
		return
	}
	if (c.State.Running() || c.State.Status == StatusRestarting) && !force {
		id := c.ID
		e.ctr.mu.Unlock()
		writeError(w, r, errConflict("You cannot remove a running container %s. Stop the container before attempting removal or force remove", id))
		return
	}
	c.restartGen++
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	wasRunning := c.State.Status == StatusRunning
	c.State.Status = StatusRemoving
	c.State.StoppedByUser = true
	id := c.ID
	anonVols := slices.Clone(c.AnonymousVolumes)
	attrs := containerAttrs(c)
	_ = e.saveContainersLocked()
	e.ctr.mu.Unlock()

	e.removeContainerCleanup(r.Context(), id, wasRunning, removeVols, anonVols, attrs)
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerPrune(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var untilTime time.Time
	if vals := f["until"]; len(vals) > 0 {
		untilTime, err = parseUntilFilter(vals[0], time.Now().UTC())
		if err != nil {
			writeError(w, r, err)
			return
		}
	}

	type candidate struct {
		id          string
		anonVols    []string
		attrs       map[string]string
		rootfsBytes int64
	}
	var toRemove []candidate

	e.ctr.mu.Lock()
	for _, c := range e.ctr.byID {
		if c.State.Running() || c.State.Status == StatusRestarting || c.State.Status == StatusRemoving {
			continue
		}
		if !f.matchLabels(c.Config.Labels) {
			continue
		}
		if !untilTime.IsZero() && !c.Created.Before(untilTime) {
			continue
		}
		c.State.Status = StatusRemoving
		c.State.StoppedByUser = true
		toRemove = append(toRemove, candidate{
			id:          c.ID,
			anonVols:    slices.Clone(c.AnonymousVolumes),
			attrs:       containerAttrs(c),
			rootfsBytes: e.containerSizeRw(*c),
		})
	}
	if len(toRemove) > 0 {
		_ = e.saveContainersLocked()
	}
	e.ctr.mu.Unlock()

	deleted := make([]string, 0, len(toRemove))
	var reclaimed uint64
	for _, cand := range toRemove {
		e.removeContainerCleanup(r.Context(), cand.id, false, true, cand.anonVols, cand.attrs)
		deleted = append(deleted, cand.id)
		if cand.rootfsBytes > 0 {
			reclaimed += uint64(cand.rootfsBytes)
		}
	}
	slices.Sort(deleted)
	writeJSON(w, http.StatusOK, containerPruneResponse{
		ContainersDeleted: deleted,
		SpaceReclaimed:    reclaimed,
	})
}

func parseUntilFilter(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(sec, 0).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, errInvalid("invalid until filter: %q", raw)
}
