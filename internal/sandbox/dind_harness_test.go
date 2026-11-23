//go:build !windows

package sandbox

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// TestDockerEngineHarness runs docker:dind as the Docker Engine recipe does,
// in a temp cache and away from the user's engine (no ~/.gvx socket, no home
// folder), and checks what the app now asks of it: ports dockerd's
// containers publish are known as -auto-publish publishes and unpublishes
// them, and when this computer has the port; dockerd reads daemon.json
// again on SIGHUP; Stop ends dockerd with SIGTERM, as docker stop would.
//
// It pulls docker:dind and busybox and takes a minute or two: set
// GVX_DIND_HARNESS=1 besides GVX_SANDBOX_HARNESS=1.
func TestDockerEngineHarness(t *testing.T) {
	if os.Getenv("GVX_DIND_HARNESS") != "1" {
		t.Skip("set GVX_DIND_HARNESS=1 (with GVX_SANDBOX_HARNESS=1) to run docker:dind")
	}
	client := controlHarnessClient(t)
	if !client.Features(context.Background()).Control {
		t.Skip("rungv has no control socket (v0.19)")
	}
	confDir := t.TempDir()
	conf := filepath.Join(confDir, "daemon.json")
	if err := os.WriteFile(conf, []byte(`{"debug": false}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var (
		evMu   sync.Mutex
		events []Event
	)
	sup := &Supervisor{
		Client:  func() *rungv.Client { return client },
		DataDir: t.TempDir(),
		// As recipes.StopGrace has the Docker Engine end.
		StopGrace: func(Spec, time.Duration) (time.Duration, bool) { return 20 * time.Second, true },
		Notify: func(e Event) {
			evMu.Lock()
			events = append(events, e)
			evMu.Unlock()
		},
	}
	spec := Spec{
		ID:      "gvxd-dind",
		Name:    "gvxd-dind",
		Image:   "docker:dind",
		Command: "--tls=false",
		Env:     []string{"DOCKER_TLS_CERTDIR="},
		Mounts: []Mount{
			{Type: MountVolume, Source: "gvxd-dind", Target: "/var/lib/docker"},
			{Type: MountBind, Source: confDir, Target: "/etc/docker", ReadOnly: true},
		},
		ExtraArgs:    "-auto-publish",
		Recipe:       "docker",
		ReadyPattern: "API listen on",
	}
	if err := sup.Start(spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sup.Stop(spec.ID, 2*time.Second)
		if t.Failed() {
			for _, l := range sup.Logs(spec.ID).Tail(80, Stderr) {
				t.Log(l)
			}
		}
	})
	waitFor(t, "dockerd ready", 5*time.Minute, func() bool {
		st := sup.State(spec.ID)
		if !st.Status.Active() {
			t.Fatalf("ended: %+v", st)
		}
		return st.Status == Running
	})
	ctl, ok := sup.Control(spec.ID)
	if !ok {
		t.Fatal("no control")
	}
	if procs, err := ctl.Top(context.Background()); err == nil {
		for _, p := range procs {
			if p.PID == 1 {
				t.Logf("guest PID 1: %+v", p)
			}
		}
	}
	dockerRun := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := client.ExecCommand(ctx, ctl.Target(), rungv.ExecOptions{Argv: append([]string{"docker"}, args...)})
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	docker := func(args ...string) string {
		t.Helper()
		out, err := dockerRun(args...)
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return out
	}
	// The volume outlives a run when GVX_SANDBOX_CACHE is reused.
	_, _ = dockerRun("rm", "-f", "gvxd-web", "gvxd-busy")

	// SIGHUP: dockerd reads daemon.json again, replaced whole on this
	// computer as the app replaces it.
	seq := sup.Logs(spec.ID).Seq()
	tmp := filepath.Join(confDir, ".daemon.json-x")
	_ = os.WriteFile(tmp, []byte(`{"debug": true, "labels": ["gvxd=1"]}`+"\n"), 0o644)
	if err := os.Rename(tmp, conf); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Kill(context.Background(), rungv.KillOptions{Signal: "SIGHUP"}); err != nil {
		t.Fatal(err)
	}
	var reload []string
	waitFor(t, "dockerd reloads", 15*time.Second, func() bool {
		for _, l := range sup.Logs(spec.ID).Snapshot() {
			if l.Seq > seq && strings.Contains(strings.ToLower(l.Text), "reload") {
				reload = append(reload, l.Text)
				if strings.Contains(l.Text, "Reloaded configuration") {
					return true
				}
			}
		}
		reload = nil
		return false
	})
	t.Logf("reload lines: %q", reload)
	if info := docker("info", "--format", "{{.Debug}} {{.Labels}}"); !strings.Contains(info, "true") || !strings.Contains(info, "gvxd=1") {
		t.Errorf("docker info after reload: %q", info)
	}

	// A broken file: dockerd keeps running and says why.
	seq = sup.Logs(spec.ID).Seq()
	_ = os.WriteFile(tmp, []byte(`{"debug": tru`), 0o644)
	_ = os.Rename(tmp, conf)
	_ = ctl.Kill(context.Background(), rungv.KillOptions{Signal: "SIGHUP"})
	time.Sleep(3 * time.Second)
	for _, l := range sup.Logs(spec.ID).Snapshot() {
		if l.Seq > seq {
			t.Logf("after a broken file: %s", l.Text)
		}
	}

	// Ports: one published as the container listens, one this computer has.
	busy, err := net.Listen("tcp", "127.0.0.1:15696")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	docker("run", "-d", "--name", "gvxd-web", "-p", "15695:8080", "busybox", "httpd", "-f", "-p", "8080", "-h", "/")
	docker("run", "-d", "--name", "gvxd-busy", "-p", "15696:8080", "busybox", "httpd", "-f", "-p", "8080", "-h", "/")
	waitFor(t, "15695 published and 15696 busy", 30*time.Second, func() bool {
		st := sup.State(spec.ID)
		pub := slices.ContainsFunc(st.Ports, func(p PublishedPort) bool {
			return p.Host == "127.0.0.1:15695" && p.Source == "auto" && p.Proto == "tcp"
		})
		return pub && slices.ContainsFunc(st.Busy, func(p PublishedPort) bool { return p.Host == "127.0.0.1:15696" })
	})
	t.Logf("ports %+v busy %v", sup.State(spec.ID).Ports, sup.State(spec.ID).Busy)
	// rungv port ls answers (issue #3: -S before ls failed), and the state
	// is what it lists.
	rules, err := client.Ports(context.Background(), ctl.Target())
	if err != nil {
		t.Fatalf("rungv port ls: %v", err)
	}
	waitFor(t, "ports synced from port ls", 5*time.Second, func() bool {
		return slices.Equal(sup.State(spec.ID).Ports, portsFromRules(rules))
	})
	evMu.Lock()
	sawBusy := slices.ContainsFunc(events, func(e Event) bool { return e.Kind == EventPortBusy && e.Port == "127.0.0.1:15696" })
	evMu.Unlock()
	if !sawBusy {
		t.Error("no EventPortBusy for 15696")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:15695", 5*time.Second)
	if err != nil {
		t.Errorf("dial 15695: %v", err)
	} else {
		conn.Close()
	}
	docker("rm", "-f", "gvxd-web")
	waitFor(t, "15695 unpublished", 30*time.Second, func() bool {
		return !slices.ContainsFunc(sup.State(spec.ID).Ports, func(p PublishedPort) bool { return p.Host == "127.0.0.1:15695" })
	})

	// Stop: dockerd gets SIGTERM, stops its container and ends by itself.
	seq = sup.Logs(spec.ID).Seq()
	start := time.Now()
	if err := sup.Stop(spec.ID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	st := sup.State(spec.ID)
	var tail []string
	for _, l := range sup.Logs(spec.ID).Snapshot() {
		if l.Seq > seq {
			tail = append(tail, l.Text)
		}
	}
	all := strings.Join(tail, "\n")
	t.Logf("stopped in %v as %v (code %d, signal %d)", time.Since(start).Round(100*time.Millisecond), st.Status, st.ExitCode, st.Signal)
	if !strings.Contains(all, "Processing signal 'terminated'") || !strings.Contains(all, "Daemon shutdown complete") {
		t.Errorf("dockerd did not shut down by itself:\n%s", all)
	}
	if st.Status != Stopped {
		t.Errorf("status %v", st.Status)
	}
}
