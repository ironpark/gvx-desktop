package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestHealthConfigMergeAndValidation(t *testing.T) {
	imgRaw := json.RawMessage(`{"Test":["CMD","pg_isready"],"Interval":10000000000,"Timeout":5000000000,"StartPeriod":2000000000,"StartInterval":1000000000,"Retries":4}`)

	// 1. Inherit image healthcheck when request omits it.
	mergedRaw, hc, err := mergeHealthConfig(imgRaw, nil)
	if err != nil || !hc.enabled() || !slices.Equal(hc.Test, []string{"CMD", "pg_isready"}) || hc.Retries != 4 {
		t.Fatalf("inherit = %s %+v err=%v", mergedRaw, hc, err)
	}

	// 2. Partial override keeps image Test & other fields.
	_, hc, err = mergeHealthConfig(imgRaw, json.RawMessage(`{"Interval":2000000000}`))
	if err != nil || !hc.enabled() || !slices.Equal(hc.Test, []string{"CMD", "pg_isready"}) || hc.Interval != 2*time.Second || hc.Retries != 4 {
		t.Fatalf("partial override = %+v err=%v", hc, err)
	}

	// 3. Test: ["NONE"] disables inherited image healthcheck.
	_, hc, err = mergeHealthConfig(imgRaw, json.RawMessage(`{"Test":["NONE"]}`))
	if err != nil || hc.enabled() || !slices.Equal(hc.Test, []string{"NONE"}) {
		t.Fatalf("NONE override = %+v err=%v", hc, err)
	}

	// 4. Invalid type or negative duration rejected with 400.
	for _, bad := range []string{
		`{"Test":["BOGUS","true"]}`,
		`{"Test":["CMD","true"],"Interval":-1}`,
		`{"Test":["CMD","true"],"Timeout":-1}`,
		`{"Test":["CMD","true"],"StartPeriod":-1}`,
		`{"Test":["CMD","true"],"StartInterval":-1}`,
		`{"Test":["CMD","true"],"Retries":-1}`,
	} {
		if _, _, err := mergeHealthConfig(nil, json.RawMessage(bad)); statusOf(err) != http.StatusBadRequest {
			t.Errorf("mergeHealthConfig(%s) = %v, want 400", bad, err)
		}
	}
}

func TestHealthcheckLifecycleTransitionsAndEvents(t *testing.T) {
	fake := newFakeRunner()
	fake.features = rungv.Features{Control: true}
	fake.Script = func(p *fakeProc) int { return <-p.end }
	fake.addImage("app:1", ImageConfig{Cmd: []string{"/bin/sleep", "60"}})

	var probeStep atomic.Int32
	var seenArgv []string
	var seenEnv []string
	ctl := &fakeControl{
		execFn: func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
			step := probeStep.Add(1)
			if step == 1 {
				seenArgv = slices.Clone(o.Argv)
				seenEnv = slices.Clone(o.Env)
			}
			// 1: fail (starting), 2: pass (healthy), 3: fail (streak 1), 4: fail (streak 2 -> unhealthy), 5+: pass (healthy)
			return newFakeExecProc(100+int(step), func(p *fakeExecProc) (int, error) {
				switch step {
				case 1:
					_, _ = p.stderrW.Write([]byte("not ready yet\n"))
					return 1, nil
				case 2:
					_, _ = p.stdoutW.Write([]byte("ok\n"))
					return 0, nil
				case 3:
					_, _ = p.stderrW.Write([]byte("fail-1\n"))
					return 1, nil
				case 4:
					_, _ = p.stderrW.Write([]byte("fail-2\n"))
					return 1, nil
				default:
					_, _ = p.stdoutW.Write([]byte("recovered\n"))
					return 0, nil
				}
			}), nil
		},
	}

	te := reopenTestEngine(t, t.TempDir(), fake)

	// Subscribe to health_status events via HTTP /events before starting.
	evCtx, evCancel := context.WithCancel(context.Background())
	defer evCancel()
	evReq, _ := http.NewRequestWithContext(evCtx, http.MethodGet, te.srv.URL+`/v1.52/events?filters={"event":{"health_status":true}}`, nil)
	evResp, err := te.srv.Client().Do(evReq)
	if err != nil {
		t.Fatal(err)
	}
	defer evResp.Body.Close()

	evCh := make(chan string, 16)
	go func() {
		dec := json.NewDecoder(evResp.Body)
		for {
			var msg eventMessage
			if err := dec.Decode(&msg); err != nil {
				return
			}
			evCh <- msg.Action
		}
	}()

	st, body := te.do(t, http.MethodPost, "/containers/create?name=hc-app", map[string]any{
		"Image": "app:1",
		"Env":   []string{"APP_ENV=prod"},
		"Healthcheck": map[string]any{
			"Test":     []string{"CMD-SHELL", "check-health"},
			"Interval": int64(25 * time.Millisecond),
			"Timeout":  int64(2 * time.Second),
			"Retries":  2,
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)
	if len(cr.Warnings) != 0 {
		t.Fatalf("unexpected warnings with Control enabled: %v", cr.Warnings)
	}

	fake.mu.Lock()
	if fake.controls == nil {
		fake.controls = map[string]sandbox.Control{}
	}
	fake.controls[cr.ID] = ctl
	fake.mu.Unlock()
	te.Engine.markStartedWithNetinit(cr.ID)

	if st, body := te.do(t, http.MethodPost, "/containers/hc-app/start", nil); st != http.StatusNoContent {
		t.Fatalf("start = %d %s", st, body)
	}

	// Wait for healthy -> unhealthy -> healthy transitions via /events!
	wantEvents := []string{"health_status: healthy", "health_status: unhealthy", "health_status: healthy"}
	for i, want := range wantEvents {
		select {
		case got := <-evCh:
			if got != want {
				t.Fatalf("event[%d] = %q, want %q", i, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event[%d] %q (probes=%d)", i, want, probeStep.Load())
		}
	}

	if !slices.Equal(seenArgv, []string{"/bin/sh", "-c", "check-health"}) {
		t.Errorf("probe argv = %v", seenArgv)
	}
	if !slices.Contains(seenEnv, "APP_ENV=prod") || !slices.Contains(seenEnv, guestcfg.EnvVar+"=") {
		t.Errorf("probe env missing APP_ENV or blanked GVX_NET: %v", seenEnv)
	}

	// Inspect shows healthy status and capped log (<= 5 entries).
	st, body = te.do(t, http.MethodGet, "/containers/hc-app/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect = %d %s", st, body)
	}
	var ins containerInspectResponse
	_ = json.Unmarshal(body, &ins)
	if ins.State.Health == nil || ins.State.Health.Status != HealthHealthy || ins.State.Health.FailingStreak != 0 {
		t.Fatalf("inspect Health = %+v, want healthy streak 0", ins.State.Health)
	}
	if len(ins.State.Health.Log) == 0 || len(ins.State.Health.Log) > maxHealthLogEntries {
		t.Fatalf("inspect Health.Log len = %d", len(ins.State.Health.Log))
	}

	// docker ps shows "(healthy)" and --filter health=healthy matches.
	st, body = te.do(t, http.MethodGet, `/containers/json?filters={"health":{"healthy":true}}`, nil)
	if st != http.StatusOK {
		t.Fatalf("list health=healthy = %d %s", st, body)
	}
	var items []containerListItem
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || !strings.Contains(items[0].Status, "(healthy)") {
		t.Fatalf("ps health=healthy got %+v", items)
	}

	// Stop marks health unhealthy so stopped containers never report healthy.
	if st, body := te.do(t, http.MethodPost, "/containers/hc-app/stop?t=1", nil); st != http.StatusNoContent {
		t.Fatalf("stop = %d %s", st, body)
	}
	if st, body = te.do(t, http.MethodGet, "/containers/hc-app/json", nil); st != http.StatusOK {
		t.Fatalf("inspect stopped = %d %s", st, body)
	}
	var insStopped containerInspectResponse
	_ = json.Unmarshal(body, &insStopped)
	if insStopped.State.Health == nil || insStopped.State.Health.Status != HealthUnhealthy {
		t.Fatalf("stopped Health = %+v, want unhealthy", insStopped.State.Health)
	}
}

func TestHealthcheckStartPeriodAndTimeoutAndPausedSkip(t *testing.T) {
	fake := newFakeRunner()
	fake.features = rungv.Features{Control: true}
	fake.Script = func(p *fakeProc) int { return <-p.end }
	fake.addImage("app:1", ImageConfig{Cmd: []string{"/bin/sleep", "60"}})

	var calls atomic.Int32
	ctl := &fakeControl{
		execFn: func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
			n := calls.Add(1)
			return newFakeExecProc(200+int(n), func(p *fakeExecProc) (int, error) {
				if n <= 2 {
					// Fail quickly during start_period -> must stay "starting", streak 0.
					return 1, nil
				}
				if n == 3 {
					// Hang past timeout -> must be killed and recorded as ExitCode -1.
					<-ctx.Done()
					return 137, ctx.Err()
				}
				return 0, nil
			}), nil
		},
	}

	te := reopenTestEngine(t, t.TempDir(), fake)

	st, body := te.do(t, http.MethodPost, "/containers/create?name=hc-sp", map[string]any{
		"Image": "app:1",
		"Healthcheck": map[string]any{
			"Test":          []string{"CMD", "probe"},
			"StartPeriod":   int64(120 * time.Millisecond),
			"StartInterval": int64(30 * time.Millisecond),
			"Interval":      int64(40 * time.Millisecond),
			"Timeout":       int64(60 * time.Millisecond),
			"Retries":       1,
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)
	fake.mu.Lock()
	if fake.controls == nil {
		fake.controls = map[string]sandbox.Control{}
	}
	fake.controls[cr.ID] = ctl
	fake.mu.Unlock()

	if st, body := te.do(t, http.MethodPost, "/containers/hc-sp/start", nil); st != http.StatusNoContent {
		t.Fatalf("start = %d %s", st, body)
	}

	// Wait until probe 2 has completed while still in start_period: status must remain starting with streak 0.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, _ := te.Engine.lookup("hc-sp")
		if c.State.Health != nil && len(c.State.Health.Log) >= 2 {
			if c.State.Health.Status != HealthStarting || c.State.Health.FailingStreak != 0 {
				t.Fatalf("during start_period: got %+v, want starting streak 0", c.State.Health)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Eventually probe 3 times out (ExitCode -1) and probe 4 succeeds (healthy).
	sawTimeout := false
	sawHealthy := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, _ := te.Engine.lookup("hc-sp")
		if c.State.Health != nil {
			for _, entry := range c.State.Health.Log {
				if entry.ExitCode == -1 && strings.Contains(entry.Output, "timeout") {
					sawTimeout = true
				}
			}
			if c.State.Health.Status == HealthHealthy {
				sawHealthy = true
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !sawTimeout || !sawHealthy {
		c, _ := te.Engine.lookup("hc-sp")
		t.Fatalf("sawTimeout=%v sawHealthy=%v health=%+v", sawTimeout, sawHealthy, c.State.Health)
	}

	// Pausing the container stops new probes from running.
	if st, body := te.do(t, http.MethodPost, "/containers/hc-sp/pause", nil); st != http.StatusNoContent {
		t.Fatalf("pause = %d %s", st, body)
	}
	time.Sleep(60 * time.Millisecond)
	countPaused := calls.Load()
	time.Sleep(120 * time.Millisecond)
	if after := calls.Load(); after != countPaused {
		t.Errorf("healthcheck probes ran while paused: before=%d after=%d", countPaused, after)
	}
}

func TestHealthcheckWithoutControlWarnsAndCommitChange(t *testing.T) {
	// Without Control (rungv v0.19), create warns and start leaves State.Health nil.
	fake := newFakeRunner()
	fake.features = rungv.Features{Control: false}
	fake.Script = func(p *fakeProc) int { return <-p.end }
	fake.addImage("app:1", ImageConfig{Cmd: []string{"/bin/sleep", "60"}})
	te := reopenTestEngine(t, t.TempDir(), fake)

	st, body := te.do(t, http.MethodPost, "/containers/create?name=hc-v19", map[string]any{
		"Image":       "app:1",
		"Healthcheck": map[string]any{"Test": []string{"CMD", "true"}},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)
	if len(cr.Warnings) == 0 || !strings.Contains(cr.Warnings[0], "Healthcheck") {
		t.Fatalf("expected v0.19 healthcheck warning, got %v", cr.Warnings)
	}

	// parseHealthcheckChange supports NONE and option flags + CMD.
	rawNone, err := parseHealthcheckChange("NONE")
	if err != nil || !strings.Contains(string(rawNone), `"NONE"`) {
		t.Fatalf("parseHealthcheckChange(NONE) = %s err=%v", rawNone, err)
	}
	rawCmd, err := parseHealthcheckChange(`--interval=5s --timeout=2s --start-period=10s --start-interval=1s --retries=4 CMD curl -f http://localhost/`)
	if err != nil {
		t.Fatalf("parseHealthcheckChange(CMD) err=%v", err)
	}
	hc, err := parseHealthConfig(rawCmd)
	if err != nil || !hc.enabled() || hc.Interval != 5*time.Second || hc.Timeout != 2*time.Second ||
		hc.StartPeriod != 10*time.Second || hc.StartInterval != time.Second || hc.Retries != 4 ||
		!slices.Equal(hc.Test, []string{"CMD-SHELL", "curl -f http://localhost/"}) {
		t.Fatalf("parsed commit healthcheck = %+v err=%v", hc, err)
	}
	rawExec, err := parseHealthcheckChange(`--retries=2 CMD ["pg_isready","-U","app"]`)
	if err != nil {
		t.Fatalf("parseHealthcheckChange(exec CMD) err=%v", err)
	}
	hcExec, _ := parseHealthConfig(rawExec)
	if !slices.Equal(hcExec.Test, []string{"CMD", "pg_isready", "-U", "app"}) || hcExec.Retries != 2 {
		t.Fatalf("parsed exec healthcheck = %+v", hcExec)
	}
}
