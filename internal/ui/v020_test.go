package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/term"
)

type fakeExecProcess struct {
	started chan struct{}
}

func newFakeExecProcess() fakeExecProcess {
	ch := make(chan struct{})
	close(ch)
	return fakeExecProcess{started: ch}
}

func (fakeExecProcess) Stdin() io.WriteCloser                { return nil }
func (fakeExecProcess) Stdout() io.Reader                    { return strings.NewReader("") }
func (fakeExecProcess) Stderr() io.Reader                    { return strings.NewReader("") }
func (fakeExecProcess) Resize(uint16, uint16) error          { return nil }
func (fakeExecProcess) PID() int                             { return 42 }
func (f fakeExecProcess) Started() <-chan struct{}           { return f.started }
func (fakeExecProcess) Signal(context.Context, string) error { return nil }
func (fakeExecProcess) Wait() (int, error)                   { return 0, nil }
func (fakeExecProcess) Close() error                         { return nil }

type fakeSandboxControl struct {
	mu         sync.Mutex
	target     rungv.Target
	statsSeq   []rungv.Stats
	statsErr   error
	statsIdx   int
	statsCalls int
	execArgs   []rungv.ExecOptions
	execCalls  []sandbox.ExecOptions
}

func (f *fakeSandboxControl) Target() rungv.Target { return f.target }
func (f *fakeSandboxControl) ExecArgv(o rungv.ExecOptions) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execArgs = append(f.execArgs, o)
	return append([]string{"rungv", "exec", "-S", f.target.Socket, "--"}, o.Argv...)
}
func (f *fakeSandboxControl) Exec(_ context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, o)
	return newFakeExecProcess(), nil
}
func (f *fakeSandboxControl) CopyFrom(context.Context, string, rungv.CopyOptions) (io.ReadCloser, error) {
	return nil, rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) CopyTo(context.Context, string, io.Reader, rungv.CopyOptions) error {
	return rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) StatPath(context.Context, string) (rungv.PathStat, error) {
	return rungv.PathStat{}, rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) Export(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) Stats(context.Context) (rungv.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	if f.statsErr != nil {
		return rungv.Stats{}, f.statsErr
	}
	if len(f.statsSeq) == 0 {
		return rungv.Stats{}, errors.New("no stats")
	}
	i := min(f.statsIdx, len(f.statsSeq)-1)
	f.statsIdx++
	return f.statsSeq[i], nil
}
func (f *fakeSandboxControl) StreamStats(context.Context, time.Duration, func(rungv.Stats)) error {
	return rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) Top(context.Context) ([]rungv.Process, error) {
	return nil, rungv.ErrNotImplemented
}
func (f *fakeSandboxControl) Kill(context.Context, rungv.KillOptions) error {
	return rungv.ErrNotImplemented
}

func TestSandboxShellTargetGating(t *testing.T) {
	SetLanguage(LangEnglish)
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	spec := sandbox.Spec{ID: "sb1", Name: "web", Image: "nginx:alpine"}
	dummyStart := func(uint16, uint16) (term.Program, error) { return nil, nil }

	for _, tc := range []struct {
		name     string
		state    sandbox.State
		features rungv.Features
		offline  bool
		wantWhy  string
		wantText string
		wantKey  bool
	}{
		{
			name:     "stopped",
			state:    sandbox.State{Status: sandbox.Stopped},
			features: rungv.Features{Control: true},
			wantWhy:  "The sandbox is not running",
			wantText: "Start it to open a shell in it.",
		},
		{
			name:     "stopping",
			state:    sandbox.State{Status: sandbox.Stopping, Control: true},
			features: rungv.Features{Control: true},
			wantWhy:  "The sandbox is not running",
			wantText: "Start it to open a shell in it.",
		},
		{
			name:     "paused",
			state:    sandbox.State{Status: sandbox.Running, Control: true, Paused: true},
			features: rungv.Features{Control: true},
			wantWhy:  "The sandbox is paused",
			wantText: "Resume it to open a shell in it.",
		},
		{
			name:     "no control and v0.19 features",
			state:    sandbox.State{Status: sandbox.Running, Control: false},
			features: rungv.Features{Control: false},
			wantWhy:  "Needs rungv v0.20 or later",
			wantText: "Update rungv in Settings to open a shell in a running sandbox, pause it, and change its resources while it runs.",
		},
		{
			name:     "no control and v0.20 features (started before upgrade)",
			state:    sandbox.State{Status: sandbox.Running, Control: false},
			features: rungv.Features{Control: true},
			wantWhy:  "Restart the sandbox to open a shell",
			wantText: "It started before rungv was updated. Restart it to open a shell, pause it, or change its resources while it runs.",
		},
		{
			name:     "running with control live",
			state:    sandbox.State{Status: sandbox.Running, Control: true, StartedAt: t0},
			features: rungv.Features{Control: true},
			wantKey:  true,
		},
		{
			name:     "running with control offline",
			state:    sandbox.State{Status: sandbox.Running, Control: true, StartedAt: t0},
			features: rungv.Features{Control: true},
			offline:  true,
			wantKey:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxShellTarget(Row{Spec: spec, State: tc.state}, tc.features, tc.offline, dummyStart)
			if got.Why != tc.wantWhy || got.WhyText != tc.wantText {
				t.Errorf("Why=(%q, %q), want (%q, %q)", got.Why, got.WhyText, tc.wantWhy, tc.wantText)
			}
			if (got.Key != "") != tc.wantKey {
				t.Errorf("Key=%q, wantKey=%v", got.Key, tc.wantKey)
			}
			if tc.wantKey && got.Start == nil {
				t.Error("expected non-nil Start")
			}
		})
	}

	// Restarting changes StartedAt, which changes Key so the shell reopens.
	k1 := sandboxShellTarget(Row{Spec: spec, State: sandbox.State{Status: sandbox.Running, Control: true, StartedAt: t0}}, rungv.Features{Control: true}, true, nil).Key
	k2 := sandboxShellTarget(Row{Spec: spec, State: sandbox.State{Status: sandbox.Running, Control: true, StartedAt: t0.Add(time.Second)}}, rungv.Features{Control: true}, true, nil).Key
	if k1 == k2 {
		t.Errorf("Key did not change across restarts: %q", k1)
	}
}

func TestContainerShellTargetGating(t *testing.T) {
	SetLanguage(LangEnglish)
	dummyStart := func(uint16, uint16) (term.Program, error) { return nil, nil }
	ctr := func(state string) dockerapi.Container {
		return dockerapi.Container{ID: "c1", Names: []string{"/web"}, State: state}
	}

	for _, tc := range []struct {
		name       string
		state      string
		se         bool
		features   rungv.Features
		canControl bool
		offline    bool
		wantWhy    string
		wantText   string
		wantKey    bool
	}{
		{
			name:     "dind stopped",
			state:    "exited",
			wantWhy:  "The container is not running",
			wantText: "Start it to open a shell in it.",
		},
		{
			name:     "dind paused",
			state:    "paused",
			wantWhy:  "The container is paused",
			wantText: "Resume it to open a shell in it.",
		},
		{
			name:    "dind running",
			state:   "running",
			wantKey: true,
		},
		{
			name:     "engine mode v0.19 running",
			state:    "running",
			se:       true,
			features: rungv.Features{Control: false},
			wantWhy:  "Needs rungv v0.20 or later",
			wantText: "Container exec in the experimental sandbox engine needs rungv v0.20 or later. Update rungv in Settings, then restart the container.",
		},
		{
			name:     "engine mode v0.20 paused",
			state:    "paused",
			se:       true,
			features: rungv.Features{Control: true},
			wantWhy:  "The container is paused",
			wantText: "Resume it to open a shell in it.",
		},
		{
			name:     "engine mode v0.20 stopped",
			state:    "exited",
			se:       true,
			features: rungv.Features{Control: true},
			wantWhy:  "The container is not running",
			wantText: "Start it to open a shell in it.",
		},
		{
			name:       "engine mode v0.20 old run without control",
			state:      "running",
			se:         true,
			features:   rungv.Features{Control: true},
			canControl: false,
			wantWhy:    "Restart the container to open a shell",
			wantText:   "It started before rungv was updated to v0.20.",
		},
		{
			name:       "engine mode v0.20 running with control",
			state:      "running",
			se:         true,
			features:   rungv.Features{Control: true},
			canControl: true,
			wantKey:    true,
		},
		{
			name:       "engine mode v0.20 offline",
			state:      "running",
			se:         true,
			features:   rungv.Features{Control: true},
			canControl: true,
			offline:    true,
			wantKey:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := containerShellTarget(ctr(tc.state), tc.se, tc.features, tc.canControl, tc.offline, dummyStart)
			if got.Why != tc.wantWhy || got.WhyText != tc.wantText {
				t.Errorf("Why=(%q, %q), want (%q, %q)", got.Why, got.WhyText, tc.wantWhy, tc.wantText)
			}
			if (got.Key != "") != tc.wantKey {
				t.Errorf("Key=%q, wantKey=%v", got.Key, tc.wantKey)
			}
		})
	}
}

func TestPauseActionVisibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   sandbox.State
		want string
	}{
		{"stopped", sandbox.State{Status: sandbox.Stopped}, ""},
		{"stopping", sandbox.State{Status: sandbox.Stopping, Control: true}, ""},
		{"running no control", sandbox.State{Status: sandbox.Running, Control: false}, ""},
		{"running with control", sandbox.State{Status: sandbox.Running, Control: true}, "pause"},
		{"paused", sandbox.State{Status: sandbox.Running, Control: true, Paused: true}, "resume"},
	} {
		if got := sandboxPauseAction(tc.st); got != tc.want {
			t.Errorf("sandboxPauseAction(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name       string
		state      string
		se         bool
		canControl bool
		want       string
	}{
		{"dind running", "running", false, false, "pause"},
		{"dind paused", "paused", false, false, "unpause"},
		{"dind exited", "exited", false, false, ""},
		{"engine running no control", "running", true, false, ""},
		{"engine running with control", "running", true, true, "pause"},
		{"engine paused with control", "paused", true, true, "unpause"},
		{"engine paused no control", "paused", true, false, ""},
	} {
		if got := containerPauseAction(dockerapi.Container{State: tc.state}, tc.se, tc.canControl); got != tc.want {
			t.Errorf("containerPauseAction(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEditorLiveChoice(t *testing.T) {
	SetLanguage(LangEnglish)
	base := sandbox.Spec{ID: "sb1", Name: "web", Image: "nginx:alpine", Memory: "512m", CPUs: "1"}

	// Stopped sandbox: old caption, Save and start.
	a := editorActionOf(sandbox.State{Status: sandbox.Stopped}, base, false, base)
	if got := editorPrimaryLabel(a); got != "Save and start" {
		t.Errorf("stopped primary = %q", got)
	}
	if got := editorCaption(a); got != "Ports and mounts are opened before the guest starts and stay fixed while it runs." {
		t.Errorf("stopped caption = %q", got)
	}

	// Running without Control: old caption, Save and restart.
	edMem := base.Clone()
	edMem.Memory = "1g"
	a = editorActionOf(sandbox.State{Status: sandbox.Running, Control: false}, base, true, edMem)
	if got := editorPrimaryLabel(a); got != "Save and restart" {
		t.Errorf("no control primary = %q", got)
	}
	if got := editorCaption(a); got != "Ports and mounts are opened before the guest starts and stay fixed while it runs." {
		t.Errorf("no control caption = %q", got)
	}

	// Running with Control, no diff (or only AutoStart changed): Save, no restart.
	a = editorActionOf(sandbox.State{Status: sandbox.Running, Control: true}, base, true, base)
	if got := editorPrimaryLabel(a); got != "Save" {
		t.Errorf("no diff primary = %q, want Save", got)
	}
	if got := editorCaption(a); got != "Memory, CPUs and LAN isolation apply to a running sandbox at once. Ports, mounts and the other settings take a restart." {
		t.Errorf("control caption = %q", got)
	}
	edAuto := base.Clone()
	edAuto.AutoStart = true
	a = editorActionOf(sandbox.State{Status: sandbox.Running, Control: true}, base, true, edAuto)
	if got := editorPrimaryLabel(a); got != "Save" {
		t.Errorf("AutoStart-only primary = %q, want Save", got)
	}

	// Running with Control, live diff (memory + CPUs + IsolateLAN): Save and apply.
	edLive := base.Clone()
	edLive.Memory = "1g"
	edLive.CPUs = "2"
	edLive.IsolateLAN = true
	a = editorActionOf(sandbox.State{Status: sandbox.Running, Control: true}, base, true, edLive)
	if got := editorPrimaryLabel(a); got != "Save and apply" {
		t.Errorf("live diff primary = %q", got)
	}

	// Running with Control, non-live diff (ports changed): Save and restart.
	edPort := edLive.Clone()
	edPort.Ports = []string{"8081:80"}
	a = editorActionOf(sandbox.State{Status: sandbox.Running, Control: true}, base, true, edPort)
	if got := editorPrimaryLabel(a); got != "Save and restart" {
		t.Errorf("non-live diff primary = %q", got)
	}
}

func TestGuestCPUPercent(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	mk := func(ts time.Time, userNS, sysNS int64) rungv.Stats {
		var s rungv.Stats
		s.Timestamp = ts
		s.CPU.UserNanos = userNS
		s.CPU.SystemNanos = sysNS
		return s
	}

	if _, ok := guestCPUPercent(rungv.Stats{}, mk(t0, 100, 0)); ok {
		t.Error("zero prev timestamp should not be ok")
	}
	if _, ok := guestCPUPercent(mk(t0, 100, 0), mk(t0, 200, 0)); ok {
		t.Error("zero wall delta should not be ok")
	}
	if _, ok := guestCPUPercent(mk(t0, 500, 0), mk(t0.Add(time.Second), 400, 0)); ok {
		t.Error("negative CPU delta should not be ok")
	}
	// 0.3s user + 0.2s sys = 0.5s CPU over 1s wall = 50%
	pct, ok := guestCPUPercent(mk(t0, 1e9, 0), mk(t0.Add(time.Second), 13e8, 2e8))
	if !ok || math.Abs(pct-50.0) > 0.01 {
		t.Errorf("50%% case: got (%v, %v)", pct, ok)
	}
	// 2.0s CPU over 1s wall = 200% (2 busy cores)
	pct, ok = guestCPUPercent(mk(t0, 0, 0), mk(t0.Add(time.Second), 15e8, 5e8))
	if !ok || math.Abs(pct-200.0) > 0.01 {
		t.Errorf("200%% case: got (%v, %v)", pct, ok)
	}
}

func TestLoadSandboxUsageGuestStatsAndFallback(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	web := sandbox.Spec{ID: "w1", Name: "web", Image: "nginx:alpine", Memory: "512m"}
	db := sandbox.Spec{ID: "db1", Name: "db", Image: "postgres:17", Memory: "1g"}
	if err := st.SaveSandboxes([]sandbox.Spec{web, db}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var s1, s2 rungv.Stats
	s1.Timestamp = t0
	s1.CPU.UserNanos = 1e9
	s1.Memory.TotalBytes = 40 << 20
	s1.Memory.LimitBytes = 256 << 20
	s2.Timestamp = t0.Add(time.Second)
	s2.CPU.UserNanos = 12e8
	s2.CPU.SystemNanos = 5e7 // +0.25s over 1s = 25%
	s2.Memory.TotalBytes = 48 << 20
	s2.Memory.LimitBytes = 256 << 20

	webCtl := &fakeSandboxControl{statsSeq: []rungv.Stats{s1, s2}}
	dbCtl := &fakeSandboxControl{statsErr: errors.New("socket gone")}
	m.controlOf = func(id string) (sandbox.Control, bool) {
		switch id {
		case web.ID:
			return webCtl, true
		case db.ID:
			return dbCtl, true
		}
		return nil, false
	}
	m.States.Set(map[string]sandbox.State{
		web.ID: {Status: sandbox.Running, PID: os.Getpid(), Control: true},
		db.ID:  {Status: sandbox.Running, PID: os.Getpid(), Control: true},
	})
	m.Page.Set(PageSandboxes)
	m.LoadSandboxUsage()
	settle(t, p, func() bool {
		u := ggui.Peek(m.SandboxUsage)
		return len(u) == 2
	}, "guest and fallback sandbox usage")

	wu := ggui.Peek(m.SandboxUsage)[web.ID]
	if !wu.HasCPU || math.Abs(wu.CPU-25.0) > 0.01 {
		t.Errorf("web guest CPU = (%v, %v), want 25%%", wu.CPU, wu.HasCPU)
	}
	if wu.Memory != 48<<20 || wu.Limit != 256<<20 {
		t.Errorf("web guest memory/limit = (%d, %d), want (%d, %d)", wu.Memory, wu.Limit, 48<<20, 256<<20)
	}
	// db guest stats errored -> fell back to procstat on os.Getpid(), with
	// initial procstat sample before the 500ms sleep so CPU% is known on tick 1.
	du := ggui.Peek(m.SandboxUsage)[db.ID]
	if du.Memory <= 0 || du.Limit != 1<<30 || !du.HasCPU {
		t.Errorf("db fallback usage = %+v, want Memory>0, Limit=1GiB, HasCPU=true", du)
	}

	// Second tick: db must NOT be re-classified as freshCtl (only 1 Stats call,
	// no 500ms sleep), and fallback still produces CPU%.
	dbCtl.mu.Lock()
	callsBefore := dbCtl.statsCalls
	dbCtl.mu.Unlock()
	start := time.Now()
	m.LoadSandboxUsage()
	settle(t, p, func() bool { return !m.sandboxUsageLoading }, "second sandbox usage tick")
	elapsed := time.Since(start)
	dbCtl.mu.Lock()
	callsDelta := dbCtl.statsCalls - callsBefore
	dbCtl.mu.Unlock()
	if callsDelta != 1 {
		t.Errorf("db second tick called Stats %d times, want 1 (no freshCtl retry loop)", callsDelta)
	}
	if elapsed >= 450*time.Millisecond {
		t.Errorf("db second tick slept %v, expected no freshCtl 500ms sleep", elapsed)
	}
	du2 := ggui.Peek(m.SandboxUsage)[db.ID]
	if !du2.HasCPU || du2.Memory <= 0 {
		t.Errorf("db second tick fallback usage = %+v, want HasCPU=true and Memory>0", du2)
	}
}

func TestSaveDraftLiveApply(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	web := sandbox.Spec{ID: "w1", Name: "web", Image: "nginx:alpine", Memory: "512m", CPUs: "1"}
	if err := st.SaveSandboxes([]sandbox.Spec{web}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true}})
	})
	defer p.Close()
	p.Frame()

	var mu sync.Mutex
	var applied []rungv.Update
	var updateErr error
	m.updateSandboxFn = func(_ context.Context, id string, u rungv.Update) (rungv.Resources, error) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, u)
		return rungv.Resources{}, updateErr
	}

	// Live edit + SaveDraft(true) ("Save and apply"): saves and calls updateSandbox.
	m.EditSandbox(web.ID)
	m.Draft.Update(func(d Draft) Draft { d.Memory = "1g"; d.IsolateLAN = true; return d })
	if !m.SaveDraft(true) {
		t.Fatal("SaveDraft(true) failed")
	}
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 1
	}, "live update call")
	mu.Lock()
	u := applied[0]
	mu.Unlock()
	if u.Memory != "1g" || u.IsolateLAN == nil || !*u.IsolateLAN {
		t.Errorf("applied update = %+v, want Memory=1g IsolateLAN=true", u)
	}
	if spec, ok := m.spec(web.ID); !ok || spec.Memory != "1g" || !spec.IsolateLAN {
		t.Errorf("saved spec = %+v", spec)
	}

	// Failure still saves spec and surfaces error toast.
	// (saveSpecs calls refreshStates which reads the empty real supervisor, so
	// restore the simulated running+control state for the second edit.)
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true}})
	mu.Lock()
	updateErr = errors.New("memory below current use")
	mu.Unlock()
	m.EditSandbox(web.ID)
	m.Draft.Update(func(d Draft) Draft { d.Memory = "64m"; return d })
	if !m.SaveDraft(false) {
		t.Fatal("SaveDraft(false) failed")
	}
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(applied) == 2
	}, "second live update call")
	if spec, ok := m.spec(web.ID); !ok || spec.Memory != "64m" {
		t.Errorf("spec should remain saved on update error: %+v", spec)
	}

	// AutoStart-only change (zero live diff) with SaveDraft(true): saves
	// without calling updateSandboxFn and without restarting.
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true}})
	m.EditSandbox(web.ID)
	m.Draft.Update(func(d Draft) Draft { d.AutoStart = true; return d })
	if !m.SaveDraft(true) {
		t.Fatal("SaveDraft(true) on zero live diff failed")
	}
	p.Frame()
	mu.Lock()
	gotApplied := len(applied)
	mu.Unlock()
	if gotApplied != 2 {
		t.Errorf("zero live diff should not call updateSandboxFn, got %d calls", gotApplied)
	}
	if spec, ok := m.spec(web.ID); !ok || !spec.AutoStart {
		t.Errorf("AutoStart should be saved: %+v", spec)
	}
}

func TestV020UIScreensAndGating(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	web := sandbox.Spec{ID: "w1", Name: "web", Image: "nginx:alpine", Memory: "512m", Checkpoint: true}
	if err := st.SaveSandboxes([]sandbox.Spec{web}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	fake := &fakeSandboxEngine{}
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 1400))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.sandboxEngine = fake
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}

	// 1. Sandboxes page: Terminal tab states & Pause/Resume/Snapshots.
	m.ShowSandbox(web.ID, sandboxTerminalTab)
	p.Frame()
	if !shows("The sandbox is not running") {
		t.Errorf("stopped sandbox terminal missing message:\n%s", p.Semantics())
	}

	// Running without Control, v0.19 features -> Needs rungv v0.20 or later.
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running}})
	p.Frame()
	if !shows("Needs rungv v0.20 or later") {
		t.Errorf("v0.19 sandbox terminal missing message:\n%s", p.Semantics())
	}

	// Running without Control, v0.20 features -> Restart the sandbox to open a shell.
	m.RungvFeatures.Set(rungv.Features{Control: true})
	p.Frame()
	if !shows("Restart the sandbox to open a shell") {
		t.Errorf("pre-upgrade sandbox terminal missing message:\n%s", p.Semantics())
	}

	// Running with Control -> Pause button visible, terminal starts canned shell.
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true, StartedAt: time.Now()}})
	p.Frame()
	if !shows("Pause") {
		t.Errorf("running+control sandbox missing Pause button:\n%s", p.Semantics())
	}

	// Paused -> Paused status, Resume button, Terminal explains paused, Snapshots disabled.
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true, Paused: true, StartedAt: time.Now()}})
	p.Frame()
	if !shows("Paused") || !shows("Resume") || !shows("The sandbox is paused") {
		t.Errorf("paused sandbox missing expected UI:\n%s", p.Semantics())
	}
	m.Detail.Set(3) // Snapshots tab
	p.Frame()
	snapBtn, ok := p.Semantics().Find(ggui.RoleButton, "Snapshot and stop")
	if !ok || !snapBtn.Disabled {
		t.Errorf("Snapshot and stop should be disabled while paused (found=%v disabled=%v)", ok, snapBtn.Disabled)
	}

	// 2. Editor sheet: live caption and Save and apply.
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true}})
	m.EditSandbox(web.ID)
	m.Draft.Update(func(d Draft) Draft { d.Memory = "1g"; return d })
	p.Frame()
	if !shows("Memory, CPUs and LAN isolation apply to a running sandbox at once.") || !shows("Save and apply") {
		t.Errorf("editor live mode missing caption or Save and apply:\n%s", p.Semantics())
	}
	m.Draft.Update(func(d Draft) Draft { d.Ports = "8080:80"; return d })
	p.Frame()
	if !shows("Save and restart") {
		t.Errorf("editor non-live diff should show Save and restart:\n%s", p.Semantics())
	}
	m.EditorOpen.Set(false)
	p.Frame()

	// 3. Settings -> Docker -> Experimental notes for v0.20 vs v0.19.
	m.Go(PageSettings)
	p.Frame()
	if !shows("Not yet supported: commit, export and diff.") {
		t.Errorf("Settings v0.20 notes missing:\n%s", p.Semantics())
	}
	m.RungvFeatures.Set(rungv.Features{})
	p.Frame()
	if !shows("Update rungv to v0.20 or later for exec, cp, pause, top and update.") {
		t.Errorf("Settings v0.19 upgrade note missing:\n%s", p.Semantics())
	}

	// 4. Containers page in sandbox-engine mode: Terminal gating with fakeSandboxEngine.
	m.SetSandboxEngine(true)
	ctr := dockerapi.Container{ID: "c1", Names: []string{"/web"}, Image: "nginx:alpine", State: "running"}
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{ctr}})
	m.Go(PageContainers)
	m.ContainerDetail.Set(containerTerminalTab)
	p.Frame()
	if !shows("Needs rungv v0.20 or later") {
		t.Errorf("Containers engine mode v0.19 missing message:\n%s", p.Semantics())
	}

	fake.mu.Lock()
	fake.features = rungv.Features{Control: true}
	fake.canControl = map[string]bool{}
	fake.mu.Unlock()
	m.RungvFeatures.Set(rungv.Features{Control: true})
	p.Frame()
	if !shows("Restart the container to open a shell") {
		t.Errorf("Containers engine mode !CanControl missing message:\n%s", p.Semantics())
	}

	ctr.State = "paused"
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{ctr}})
	p.Frame()
	if !shows("The container is paused") {
		t.Errorf("Containers paused missing message:\n%s", p.Semantics())
	}
}

type stubTermProg struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
	onStop func()
	once   sync.Once
}

func newStubTermProg(onStop func()) *stubTermProg {
	r, w := io.Pipe()
	return &stubTermProg{r: r, w: w, closed: make(chan struct{}), onStop: onStop}
}

func (s *stubTermProg) Read(p []byte) (int, error)                  { return s.r.Read(p) }
func (s *stubTermProg) Write(p []byte) (int, error)                 { return len(p), nil }
func (s *stubTermProg) Resize(uint16, uint16, uint16, uint16) error { return nil }
func (s *stubTermProg) EndOutput(time.Duration)                     {}
func (s *stubTermProg) Kill()                                       { _ = s.Close() }
func (s *stubTermProg) Wait() int                                   { <-s.closed; return 0 }
func (s *stubTermProg) Close() error {
	s.once.Do(func() {
		if s.onStop != nil {
			s.onStop()
		}
		_ = s.w.Close()
		_ = s.r.Close()
		close(s.closed)
	})
	return nil
}

func TestPauseClosesTerminalAndDrainsSweeps(t *testing.T) {
	// 1. Sandbox pause closes active terminals BEFORE calling pauseSandboxFn,
	// and ResumeSandbox drains deferred sweep marks.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	web := sandbox.Spec{ID: "w1", Name: "web", Image: "nginx:alpine"}
	if err := st.SaveSandboxes([]sandbox.Spec{web}); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running, Control: true}})
	})
	defer p.Close()
	p.Frame()

	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	prog := newStubTermProg(func() { record("close") })
	sess, err := term.New(prog, term.Options{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	m.openTerminal(sess, sandboxTermTarget(web.ID))

	m.pauseSandboxFn = func(context.Context, string) error {
		record("pause")
		return nil
	}
	m.PauseSandbox(web.ID)
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 2
	}, "sandbox pause ordering")
	mu.Lock()
	gotEvents := append([]string(nil), events...)
	mu.Unlock()
	if len(gotEvents) != 2 || gotEvents[0] != "close" || gotEvents[1] != "pause" {
		t.Fatalf("sandbox pause events = %v, want [close pause]", gotEvents)
	}

	// Deferred sweep recorded while paused is drained on ResumeSandbox.
	ctl := &fakeSandboxControl{target: rungv.Target{Socket: "/tmp/s.sock"}}
	m.controlOf = func(id string) (sandbox.Control, bool) {
		if id == web.ID {
			return ctl, true
		}
		return nil, false
	}
	m.recordPendingSweep(sandboxTermTarget(web.ID), "gvx-mark-123")
	resumed := false
	m.unpauseSandboxFn = func(context.Context, string) error {
		mu.Lock()
		resumed = true
		mu.Unlock()
		return nil
	}
	m.ResumeSandbox(web.ID)
	settle(t, p, func() bool {
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		return len(ctl.execCalls) == 1
	}, "sandbox pending sweep drain")
	mu.Lock()
	wasResumed := resumed
	mu.Unlock()
	if !wasResumed {
		t.Error("expected unpauseSandboxFn to be called")
	}
	ctl.mu.Lock()
	sweepArgv := strings.Join(ctl.execCalls[0].Argv, " ")
	ctl.mu.Unlock()
	if !strings.Contains(sweepArgv, "gvx-mark-123") {
		t.Errorf("sweep argv = %q, want to contain gvx-mark-123", sweepArgv)
	}

	// 2. Container pause closes container terminals BEFORE POST /pause, and
	// unpause drains deferred container sweeps via Docker exec.
	var cmu sync.Mutex
	var cevents []string
	crecord := func(e string) {
		cmu.Lock()
		cevents = append(cevents, e)
		cmu.Unlock()
	}
	cm, cp, _ := usageModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/containers/c1/pause":
			crecord("pause")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/c1/unpause":
			crecord("unpause")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/c1/exec":
			var body struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.Contains(strings.Join(body.Cmd, " "), "ctr-mark-456") {
				crecord("sweep-create")
			}
			_, _ = w.Write([]byte(`{"Id":"exec1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/exec/exec1/start":
			crecord("sweep-start")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		}
	})
	cprog := newStubTermProg(func() { crecord("close") })
	csess, err := term.New(cprog, term.Options{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	cm.openTerminal(csess, containerTermTarget("c1"))
	ctr := dockerapi.Container{ID: "c1", Names: []string{"/web"}, State: "running"}
	cm.DockerAction("pause", ctr)
	settle(t, cp, func() bool {
		cmu.Lock()
		defer cmu.Unlock()
		return len(cevents) >= 2
	}, "container pause ordering")
	cmu.Lock()
	gotCEvents := append([]string(nil), cevents...)
	cmu.Unlock()
	if len(gotCEvents) < 2 || gotCEvents[0] != "close" || gotCEvents[1] != "pause" {
		t.Fatalf("container pause events = %v, want [close pause ...]", gotCEvents)
	}

	cm.recordPendingSweep(containerTermTarget("c1"), "ctr-mark-456")
	cm.DockerAction("unpause", ctr)
	settle(t, cp, func() bool {
		cmu.Lock()
		defer cmu.Unlock()
		return strings.Contains(strings.Join(cevents, ","), "sweep-start")
	}, "container pending sweep drain")
}

func TestEngineFeaturesNoUIBlockAndDetectCleanup(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Real *engine.Engine must read m.RungvFeatures and never call Features() on UI thread.
	m.sandboxEngine = &engine.Engine{}
	m.RungvFeatures.Set(rungv.Features{Control: true})
	if !m.engineFeatures().Control || !m.peekEngineFeatures().Control {
		t.Errorf("real *engine.Engine should use m.RungvFeatures without calling Features()")
	}

	// DetectEngine with a broken binary clears stale m.client and m.RungvFeatures.
	m.client = &rungv.Client{Path: "/stale/rungv"}
	m.RungvFeatures.Set(rungv.Features{Control: true})
	badPath := filepath.Join(dir, "broken-rungv")
	if err := os.WriteFile(badPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Settings.Update(func(s store.Settings) store.Settings {
		s.RungvPath = badPath
		return s
	})
	done := make(chan struct{}, 1)
	m.DetectEngine(func(bool) { done <- struct{}{} })
	settle(t, p, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, "DetectEngine invalid path")
	if m.client != nil {
		t.Errorf("DetectEngine left stale m.client = %+v", m.client)
	}
	if ggui.Peek(m.RungvFeatures).Control {
		t.Error("DetectEngine left stale RungvFeatures.Control = true")
	}
	if ggui.Peek(m.Engine).Err == "" {
		t.Error("DetectEngine expected non-empty Engine.Err")
	}
}

func TestPausedRecipesGateAndPollingStop(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	dockerSpec := recipes.DockerEngine()
	k3sSpec := recipes.KubernetesNode()
	regSpec := recipes.ImageRegistry()
	if err := st.SaveSandboxes([]sandbox.Spec{dockerSpec, k3sSpec, regSpec}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	dockerHits := 0
	var hitMu sync.Mutex
	srv := newLocalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hitMu.Lock()
		dockerHits++
		hitMu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 1400))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.dockerBackend = srv
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{
			dockerSpec.ID: {Status: sandbox.Running, Control: true},
			k3sSpec.ID:    {Status: sandbox.Running, Control: true},
			regSpec.ID:    {Status: sandbox.Running, Control: true},
		})
	})
	defer p.Close()
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}

	// Open Containers and seed data, then pause Docker Engine.
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{{ID: "c1", Names: []string{"/web"}, State: "running"}}})
	m.Go(PageContainers)
	p.Frame()
	// Wait for any in-flight LoadDocker triggered by Go(PageContainers) while running,
	// then pause the recipe sandboxes and reset the backend hit counter.
	settle(t, p, func() bool { return !m.dockerLoading }, "in-flight load before pause")

	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, Paused: true},
		k3sSpec.ID:    {Status: sandbox.Running, Control: true, Paused: true},
		regSpec.ID:    {Status: sandbox.Running, Control: true, Paused: true},
	})
	p.Frame()
	hitMu.Lock()
	dockerHits = 0
	hitMu.Unlock()

	if got := ggui.Peek(m.ContainersAccess); got != ContainersPaused {
		t.Fatalf("ContainersAccess = %v, want ContainersPaused", got)
	}
	if ggui.Peek(m.ContainersOpen) {
		t.Error("ContainersOpen should be false while Docker Engine is paused")
	}
	if got := ggui.Peek(m.PodsAccess); got != PodsPaused {
		t.Fatalf("PodsAccess = %v, want PodsPaused", got)
	}
	if ggui.Peek(m.PodsOpen) {
		t.Error("PodsOpen should be false while Kubernetes is paused")
	}
	if d := ggui.Untrack(func() time.Duration { return m.recipePageInterval(recipes.Docker, PageContainers) }); d != 0 {
		t.Errorf("recipePageInterval while paused = %v, want 0", d)
	}

	// LoadDocker and LoadPods must stop polling and clear listings rather than hit backend.
	m.LoadDocker()
	m.LoadPods()
	p.Frame()
	hitMu.Lock()
	hits := dockerHits
	hitMu.Unlock()
	if hits != 0 {
		t.Errorf("LoadDocker hit backend %d times while paused, want 0", hits)
	}
	if len(ggui.Peek(m.DockerContainers).Items) != 0 {
		t.Error("LoadDocker should clear containers while paused")
	}
	if ggui.Untrack(m.wantsPodUsage) {
		t.Error("wantsPodUsage should be false while Kubernetes is paused")
	}

	// Containers page shows paused gate (with Resume button, not red error).
	if !shows("The Docker Engine is paused") || !shows("Resume Docker Engine") {
		t.Errorf("Containers paused gate missing expected text:\n%s", p.Semantics())
	}
	if shows("Could not reach") {
		t.Errorf("Containers paused gate should not show error:\n%s", p.Semantics())
	}

	// Pods page shows paused gate.
	m.Page.Set(PagePods)
	p.Frame()
	if !shows("Kubernetes is paused") || !shows("Resume Kubernetes") {
		t.Errorf("Pods paused gate missing expected text:\n%s", p.Semantics())
	}

	// Docker recipe page hero shows Paused + Resume.
	m.Go(PageDocker)
	p.Frame()
	if !shows("Paused") || !shows("Resume") {
		t.Errorf("Docker recipe hero missing Paused/Resume:\n%s", p.Semantics())
	}

	// Dashboard recipe cards show Paused + Resume.
	m.Go(PageDashboard)
	p.Frame()
	if !shows("Paused") || !shows("Resume") {
		t.Errorf("Dashboard paused recipe card missing Paused/Resume:\n%s", p.Semantics())
	}

	// Sandboxes Connect tab for Docker does not claim dockerd is listening.
	m.ShowSandbox(dockerSpec.ID, connectTab)
	p.Frame()
	if !shows("The Docker Engine is paused.") || shows("dockerd is listening") {
		t.Errorf("Docker Connect tab wrong while paused:\n%s", p.Semantics())
	}

	// Registry Connect tab disables "List images" while paused; PushToRegistry offers Resume.
	m.ShowSandbox(regSpec.ID, connectTab)
	p.Frame()
	listBtn, ok := p.Semantics().Find(ggui.RoleButton, "List images")
	if !ok || !listBtn.Disabled {
		t.Errorf("Registry List images should be disabled while paused (found=%v disabled=%v)", ok, listBtn.Disabled)
	}
	m.PushToRegistry(DockerImageRow{ID: "img1", Repository: "app", Tag: "dev"})
	if got := ggui.Peek(m.ConfirmTitle); got != "Resume the image registry?" {
		t.Errorf("PushToRegistry confirm title = %q, want Resume the image registry?", got)
	}

	// Menu state & RecipeMenu show Paused + Resume instead of Running.
	ms := ggui.Peek(m.Menus)
	if !ms.Docker.Paused || len(ms.Containers) != 0 {
		t.Errorf("MenuState.Docker = %+v containers=%d, want Paused=true containers=0", ms.Docker, len(ms.Containers))
	}
	items := m.RecipeMenu(recipes.Docker, func() {})
	var labels []string
	for _, it := range items {
		labels = append(labels, it.Label)
	}
	joined := strings.Join(labels, " | ")
	if !strings.Contains(joined, "Paused") || !strings.Contains(joined, "Resume Docker Engine") {
		t.Errorf("RecipeMenu(Docker) while paused = %v", labels)
	}
}

func newLocalTestServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	// Reuse usageModel's pattern via httptest.
	m, _, _ := usageModel(t, h)
	return m.dockerBackend
}

func TestPausedContainersActionsMenuAndUsage(t *testing.T) {
	SetLanguage(LangEnglish)
	members := []dockerapi.Container{
		{ID: "c-run", Names: []string{"/web"}, State: "running"},
		{ID: "c-pause", Names: []string{"/worker"}, State: "paused"},
		{ID: "c-exit", Names: []string{"/job"}, State: "exited"},
	}

	// projectTargets: start targets paused + exited; stop targets running + paused.
	startTargets := projectTargets("start", members)
	if len(startTargets) != 2 || startTargets[0].ID != "c-pause" || startTargets[1].ID != "c-exit" {
		t.Fatalf("projectTargets(start) = %+v", startTargets)
	}
	stopTargets := projectTargets("stop", members)
	if len(stopTargets) != 2 || stopTargets[0].ID != "c-run" || stopTargets[1].ID != "c-pause" {
		t.Fatalf("projectTargets(stop) = %+v", stopTargets)
	}

	// ProjectAction("start") unpauses paused containers and starts exited ones;
	// LoadContainerUsage samples paused containers' memory.
	var mu sync.Mutex
	var calls []string
	m, p, _ := usageModel(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/containers/c-pause/stats" {
			_, _ = w.Write([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000,"online_cpus":2},"memory_stats":{"usage":4194304,"limit":1073741824}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	m.ProjectAction("start", "shop", members)
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		joined := strings.Join(calls, ",")
		return strings.Contains(joined, "POST /containers/c-pause/unpause") &&
			strings.Contains(joined, "POST /containers/c-exit/start")
	}, "ProjectAction start unpauses paused")
	mu.Lock()
	if strings.Contains(strings.Join(calls, ","), "POST /containers/c-pause/start") {
		t.Errorf("ProjectAction(start) called /start on paused container: %v", calls)
	}
	mu.Unlock()

	// containerItems paused branch: "● worker · Paused", Resume + Stop (not Start).
	menuItems := containerItems(m, MenuState{Listed: true, Containers: members[:2]}, func() {})
	var pausedItem ggui.MenuItem
	for _, it := range menuItems {
		if it.Label == "● worker · Paused" {
			pausedItem = it
			break
		}
	}
	if pausedItem.Label != "● worker · Paused" {
		t.Fatalf("paused menu item not found in %+v", menuItems)
	}
	var sub []string
	for _, s := range pausedItem.Items {
		sub = append(sub, s.Label)
	}
	subJoined := strings.Join(sub, ",")
	if !strings.Contains(subJoined, "Resume") || !strings.Contains(subJoined, "Stop") || strings.Contains(subJoined, "Start") {
		t.Errorf("paused container submenu = %v, want Resume + Stop (no Start)", sub)
	}

	// LoadContainerUsage includes paused container memory.
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{members[1]}})
	m.Go(PageContainers)
	p.Frame()
	m.LoadContainerUsage()
	settle(t, p, func() bool {
		u, ok := ggui.Peek(m.ContainerUsage)["c-pause"]
		return ok && u.Memory == 4194304
	}, "paused container usage memory")
}

func TestPausedExcludedFromRunningAndSearchableInPalette(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	s1 := sandbox.Spec{ID: "s1", Name: "web", Image: "nginx:alpine"}
	s2 := sandbox.Spec{ID: "s2", Name: "worker", Image: "alpine:3.20"}
	if err := st.SaveSandboxes([]sandbox.Spec{s1, s2}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{
			s1.ID: {Status: sandbox.Running, Control: true},
			s2.ID: {Status: sandbox.Running, Control: true, Paused: true},
		})
	})
	defer p.Close()
	p.Frame()

	if got := ggui.Peek(m.Running); got != 1 {
		t.Errorf("Running = %d, want 1 (paused excluded)", got)
	}
	if got := ggui.Peek(m.Active); got != 2 {
		t.Errorf("Active = %d, want 2 (paused included)", got)
	}
	if got := ggui.Peek(m.TotalRunning); got != 2 {
		t.Errorf("TotalRunning = %d, want 2 (quit warns for paused too)", got)
	}

	// Even with only a paused sandbox active, sandboxUsageShown stays true.
	m.States.Set(map[string]sandbox.State{
		s2.ID: {Status: sandbox.Running, Control: true, Paused: true},
	})
	m.Page.Set(PageSandboxes)
	p.Frame()
	if got := ggui.Peek(m.Running); got != 0 {
		t.Errorf("Running with only paused = %d, want 0", got)
	}
	if !ggui.Untrack(m.sandboxUsageShown) {
		t.Error("sandboxUsageShown should be true while a paused sandbox is active")
	}

	// Palette finds the paused sandbox when searching "paused".
	m.PaletteOpen.Set(true)
	m.PaletteQuery.Set("paused")
	p.Frame()
	found := false
	for _, n := range p.Semantics().All() {
		if strings.Contains(n.Name, "worker") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("palette query 'paused' did not surface paused sandbox 'worker':\n%s", p.Semantics())
	}
}
