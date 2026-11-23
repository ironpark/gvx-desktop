package rungv

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecArgsVectors(t *testing.T) {
	c := &Client{Path: "/usr/bin/rungv", CacheDir: "/ignored-for-exec"}
	tr := true
	fa := false

	cases := []struct {
		name string
		t    Target
		o    ExecOptions
		want []string
	}{
		{
			name: "minimal",
			t:    Target{Socket: "/tmp/a.sock"},
			o:    ExecOptions{Argv: []string{"echo", "hi"}},
			want: []string{"exec", "-S", "/tmp/a.sock", "--", "echo", "hi"},
		},
		{
			name: "interactive tty disables detach keys and passes token",
			t:    Target{Socket: `C:\tmp\a.sock`, TokenFile: `C:\tmp\a.sock.token`},
			o: ExecOptions{
				Argv:        []string{"/bin/sh"},
				Interactive: true,
				TTY:         true,
				User:        "1000:1000",
				Workdir:     "/work",
				Env:         []string{"A=1", "BARE_KEY_DROPPED", "EMPTY=", "=BAD", "B=two=2"},
				Privileged:  &fa,
				PIDFile:     "/tmp/exec.pid",
			},
			want: []string{
				"exec", "-S", `C:\tmp\a.sock`, "--token-file", `C:\tmp\a.sock.token`,
				"-i", "-t", "--detach-keys=",
				"-u", "1000:1000", "-w", "/work",
				"-e", "A=1", "-e", "EMPTY=", "-e", "B=two=2",
				"--privileged=false", "--pid-file", "/tmp/exec.pid",
				"--", "/bin/sh",
			},
		},
		{
			name: "tty without interactive does not pass detach-keys",
			t:    Target{Socket: "/tmp/a.sock"},
			o:    ExecOptions{Argv: []string{"ls"}, TTY: true, Privileged: &tr},
			want: []string{"exec", "-S", "/tmp/a.sock", "-t", "--privileged=true", "--", "ls"},
		},
		{
			name: "detach",
			t:    Target{Socket: "/tmp/a.sock"},
			o:    ExecOptions{Argv: []string{"sleep", "5"}, Detach: true},
			want: []string{"exec", "-S", "/tmp/a.sock", "-d", "--", "sleep", "5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.ExecArgs(tc.t, tc.o)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExecArgs:\n got  %q\n want %q", got, tc.want)
			}
			// exec is not in cacheFlagCommands: Args must not inject -cache-dir.
			if full := c.Args(got...); !reflect.DeepEqual(full, tc.want) {
				t.Fatalf("Args injected unexpected flags:\n got  %q\n want %q", full, tc.want)
			}
		})
	}
}

func TestControlCodeAndIsUnreachable(t *testing.T) {
	codeCases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"non-rungv", errors.New("rungv exec: NOT_FOUND: nope"), ""},
		{"already paused", &Error{Stderr: "rungv exec: ALREADY_PAUSED: cannot exec while sandbox is paused\n"}, CodeAlreadyPaused},
		{"unauthorized", &Error{Code: 126, Stderr: "rungv ping: UNAUTHORIZED: unauthorized control token"}, CodeUnauthorized},
		{"forbidden", &Error{Code: 126, Stderr: "rungv port add: FORBIDDEN_BY_POLICY: method \"port.add\" is disabled"}, CodeForbiddenByPolicy},
		{"lockdown", &Error{Code: 126, Stderr: "rungv mount add: LOCKDOWN_DENIED: bind mount refused"}, CodeLockdownDenied},
		{"not found after warning", &Error{Stderr: "rungv cp: warning: skipped fifo\nrungv cp: NOT_FOUND: guest path \"/nope\" does not exist\n"}, CodeNotFound},
		{"limit exceeded", &Error{Stderr: "rungv exec: LIMIT_EXCEEDED: too many exec sessions"}, CodeLimitExceeded},
		{"connect refused is not a code", &Error{Stderr: "rungv ping: connect control socket unix:///tmp/x.sock: dial unix /tmp/x.sock: connect: connection refused"}, ""},
		{"usage error", &Error{Code: 2, Stderr: "rungv update: specify at least one setting to update"}, ""},
	}
	for _, tc := range codeCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControlCode(tc.err); got != tc.want {
				t.Errorf("ControlCode = %q, want %q", got, tc.want)
			}
		})
	}

	unreachCases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("connection refused"), false},
		{"connection refused", &Error{Stderr: "rungv ping: connect control socket unix:///tmp/x.sock: dial unix /tmp/x.sock: connect: connection refused"}, true},
		{"no such file", &Error{Stderr: "rungv status: dial unix /tmp/x.sock: connect: no such file or directory"}, true},
		{"windows refused", &Error{Stderr: `rungv ping: connect control socket unix://C:\x.sock: connectex: No connection could be made because the target machine actively refused it.`}, true},
		{"protocol error NOT_FOUND is reachable", &Error{Stderr: "rungv cp: NOT_FOUND: guest path \"/a\" does not exist: no such file or directory"}, false},
		{"protocol error ALREADY_PAUSED is reachable", &Error{Stderr: "rungv exec: ALREADY_PAUSED: cannot exec"}, false},
		{"other rungv error", &Error{Stderr: "rungv update: specify at least one setting"}, false},
	}
	for _, tc := range unreachCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnreachable(tc.err); got != tc.want {
				t.Errorf("IsUnreachable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeCapturedFixtures(t *testing.T) {
	rawStats, err := os.ReadFile(filepath.Join("testdata", "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st Stats
	if err := json.Unmarshal(rawStats, &st); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if st.CPUNanos() != 15000000 || st.Memory.TotalBytes != 188416 || st.Memory.LimitBytes != 536870912 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if len(st.Network) != 2 || st.Network[1].Interface != "eth0" || st.Network[1].RxBytes != 4096 {
		t.Fatalf("unexpected network: %+v", st.Network)
	}
	if len(st.BlockIO) != 1 || st.BlockIO[0].ReadSectors != 88 || st.PIDs != 2 || st.PIDsLimit != 64 {
		t.Fatalf("unexpected block/pids: %+v", st)
	}

	rawTop, err := os.ReadFile(filepath.Join("testdata", "top.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	procs, err := decodeLines[Process](rawTop)
	if err != nil {
		t.Fatalf("decode top: %v", err)
	}
	if len(procs) != 2 || procs[0].PID != 1 || procs[0].Comm != "sleep" || procs[1].UID != 1000 || !reflect.DeepEqual(procs[1].Cmdline, []string{"sh", "-c", "echo hi"}) {
		t.Fatalf("unexpected top: %+v", procs)
	}

	rawStatus, err := os.ReadFile(filepath.Join("testdata", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(rawStatus, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status.ID != "gvxd-test-1" || status.Platform != "hvf" || !status.LockdownApplied ||
		status.Resources.MemoryBytes != 536870912 || !status.Resources.IsolateLAN ||
		len(status.Ports) != 1 || status.Ports[0].HostPort != 18090 ||
		status.ActiveExecSessions != 1 || status.PIDCount != 3 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestFeaturesAndForget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { ForgetFeatures(bin) })

	v19 := "#!/bin/sh\ncat <<'EOF'\nUsage: rungv run [flags] IMAGE\n  -control-socket string\n    \tsocket\nEOF\nexit 2\n"
	if err := os.WriteFile(bin, []byte(v19), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Client{Path: bin}
	f := c.Features(context.Background())
	if f.Control || f.DynamicPorts {
		t.Fatalf("v0.19 features = %+v, want zero", f)
	}

	// Replace with v0.20 script without calling ForgetFeatures: cache stays v0.19.
	v20 := "#!/bin/sh\ncat <<'EOF'\nUsage: rungv run [flags] IMAGE\n  -control-socket string\n  -control-token-file string\n  -dynamic-ports\nEOF\nexit 2\n"
	if err := os.WriteFile(bin, []byte(v20), 0o755); err != nil {
		t.Fatal(err)
	}
	if cached := c.Features(context.Background()); cached.Control {
		t.Fatalf("expected cached v0.19 features before ForgetFeatures, got %+v", cached)
	}

	ForgetFeatures(bin)
	f2 := c.Features(context.Background())
	if !f2.Control || !f2.DynamicPorts {
		t.Fatalf("v0.20 features after ForgetFeatures = %+v, want both true", f2)
	}
}

func writeTarBytes(t *testing.T, name string, mode int64, body []byte, typeflag byte, linkname string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(body)),
		ModTime:  time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		Typeflag: typeflag,
		Linkname: linkname,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClientControlCommandsWithStub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	stdinDump := filepath.Join(dir, "stdin.tar")
	fileTar := filepath.Join(dir, "file.tar")
	dirTar := filepath.Join(dir, "dir.tar")
	if err := os.WriteFile(fileTar, writeTarBytes(t, "hello.txt", 0o644, []byte("hello world"), tar.TypeReg, ""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirTar, writeTarBytes(t, "etc/", 0o755, nil, tar.TypeDir, ""), 0o644); err != nil {
		t.Fatal(err)
	}

	statsJSON, err := os.ReadFile(filepath.Join("testdata", "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	topNDJSON, err := os.ReadFile(filepath.Join("testdata", "top.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	statusJSON, err := os.ReadFile(filepath.Join("testdata", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	statsPath := filepath.Join(dir, "stats.json")
	topPath := filepath.Join(dir, "top.ndjson")
	statusPath := filepath.Join(dir, "status.json")
	_ = os.WriteFile(statsPath, statsJSON, 0o644)
	_ = os.WriteFile(topPath, topNDJSON, 0o644)
	_ = os.WriteFile(statusPath, statusJSON, 0o644)

	bin := filepath.Join(dir, "rungv")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + argsLog + `"
cmd="$1"
shift
case "$cmd" in
  pause|unpause|kill|stop)
    exit 0
    ;;
  ping)
    echo '{"version":2,"sandbox_id":"sb1","state":"running","uptime_ns":1000,"capabilities":["exec","exec.tty"]}'
    ;;
  status)
    cat "` + statusPath + `"
    ;;
  stats)
    case "$*" in
      *--no-stream*) cat "` + statsPath + `" ;;
      *) cat "` + statsPath + `"; cat "` + statsPath + `" ;;
    esac
    ;;
  top)
    cat "` + topPath + `"
    ;;
  update)
    echo '{"memory_bytes":268435456,"cpu_millis":1500,"pids_limit":64,"isolate_lan":true}'
    ;;
  events)
    echo '{"timestamp":"2026-11-22T17:27:45Z","type":"lifecycle","action":"pause"}'
    echo '{"timestamp":"2026-11-22T17:27:46Z","type":"lifecycle","action":"unpause"}'
    ;;
  cp)
    case "$*" in
      *:/missing*)
        echo 'rungv cp: NOT_FOUND: guest path "/missing" does not exist' >&2
        exit 1
        ;;
      *:/etc*)
        cat "` + dirTar + `"
        ;;
      *:/hello.txt*)
        cat "` + fileTar + `"
        ;;
      *:/\ *)
        cat "` + dirTar + `"
        ;;
      *)
        cat > "` + stdinDump + `"
        ;;
    esac
    ;;
  export)
    cat "` + fileTar + `"
    ;;
  port)
    # rungv port dispatches on its subcommand before it parses any flag.
    if [ "$1" != ls ]; then
      echo "rungv port: unknown subcommand \"$1\" (want ls, add, or rm)" >&2
      exit 2
    fi
    echo '{"proto":"tcp","host_ip":"127.0.0.1","host_port":18093,"guest_ip":"10.0.2.15","guest_port":8080,"source":"dynamic"}'
    echo '{"proto":"tcp","host_socket":"/tmp/d.sock","guest_ip":"10.0.2.15","guest_port":2375,"source":"static"}'
    ;;
  *)
    echo "unexpected cmd $cmd" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Client{Path: bin, CacheDir: "/should-not-appear"}
	target := Target{Socket: "/tmp/s.sock", TokenFile: "/tmp/s.token"}
	ctx := context.Background()

	if err := c.Pause(ctx, target); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c.Unpause(ctx, target); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if err := c.Kill(ctx, target, KillOptions{Signal: "SIGTERM", PID: 7}); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := c.StopGuest(ctx, target, "SIGTERM", 2*time.Second); err != nil {
		t.Fatalf("StopGuest: %v", err)
	}
	p, err := c.Ping(ctx, target, 3*time.Second)
	if err != nil || p.Version != 2 || !p.Has("exec.tty") || p.Has("nope") {
		t.Fatalf("Ping: %+v %v", p, err)
	}
	st, err := c.Status(ctx, target)
	if err != nil || st.ID != "gvxd-test-1" {
		t.Fatalf("Status: %+v %v", st, err)
	}
	ss, err := c.Stats(ctx, target)
	if err != nil || ss.CPUNanos() != 15000000 {
		t.Fatalf("Stats: %+v %v", ss, err)
	}
	var streamed int
	if err := c.StreamStats(ctx, target, 500*time.Millisecond, func(Stats) { streamed++ }); err != nil || streamed != 2 {
		t.Fatalf("StreamStats: count=%d err=%v", streamed, err)
	}
	procs, err := c.Top(ctx, target)
	if err != nil || len(procs) != 2 {
		t.Fatalf("Top: %+v %v", procs, err)
	}
	// Zero Update must be refused locally without running rungv.
	if _, err := c.Update(ctx, target, Update{}); err == nil {
		t.Fatal("zero Update accepted")
	}
	pids := int64(64)
	iso := true
	res, err := c.Update(ctx, target, Update{Memory: "256m", CPUs: "1.5", PidsLimit: &pids, IsolateLAN: &iso})
	if err != nil || res.MemoryBytes != 268435456 || !res.IsolateLAN {
		t.Fatalf("Update: %+v %v", res, err)
	}
	ports, err := c.Ports(ctx, target)
	if err != nil || len(ports) != 2 || ports[0].HostPort != 18093 || ports[0].Source != "dynamic" || ports[1].HostSocket != "/tmp/d.sock" {
		t.Fatalf("Ports: %+v %v", ports, err)
	}
	var evs []Event
	if err := c.Events(ctx, target, func(e Event) { evs = append(evs, e) }); err != nil || len(evs) != 2 || evs[0].Action != "pause" {
		t.Fatalf("Events: %+v %v", evs, err)
	}

	// CopyFrom missing path fails immediately in CopyFrom with NOT_FOUND.
	if rc, err := c.CopyFrom(ctx, target, "/missing", CopyOptions{}); err == nil || ControlCode(err) != CodeNotFound {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatalf("CopyFrom missing: err=%v code=%q", err, ControlCode(err))
	}

	// CopyFrom file succeeds.
	rc, err := c.CopyFrom(ctx, target, "/hello.txt", CopyOptions{Archive: true, FollowLink: true})
	if err != nil {
		t.Fatalf("CopyFrom file: %v", err)
	}
	gotTar, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(gotTar) == 0 {
		t.Fatal("empty tar")
	}

	// StatPath file, dir, and root (root uses ping, not cp :/ -).
	ps, err := c.StatPath(ctx, target, "/hello.txt")
	if err != nil || ps.Name != "hello.txt" || ps.Size != 11 || os.FileMode(ps.Mode).IsDir() {
		t.Fatalf("StatPath file: %+v %v", ps, err)
	}
	psd, err := c.StatPath(ctx, target, "/etc")
	if err != nil || psd.Name != "etc" || !os.FileMode(psd.Mode).IsDir() {
		t.Fatalf("StatPath dir: %+v %v", psd, err)
	}
	beforeRoot, _ := os.ReadFile(argsLog)
	psr, err := c.StatPath(ctx, target, "/")
	if err != nil || psr.Name != "/" || !os.FileMode(psr.Mode).IsDir() {
		t.Fatalf("StatPath /: %+v %v", psr, err)
	}
	afterRoot, _ := os.ReadFile(argsLog)
	rootCmds := strings.TrimPrefix(string(afterRoot), string(beforeRoot))
	if !strings.Contains(rootCmds, "ping ") || strings.Contains(rootCmds, "cp ") {
		t.Fatalf("StatPath(/) should use ping, not cp; logged: %q", rootCmds)
	}

	// CopyTo sends stdin archive.
	payload := writeTarBytes(t, "up.txt", 0o600, []byte("upload"), tar.TypeReg, "")
	if err := c.CopyTo(ctx, target, "/tmp", bytes.NewReader(payload), CopyOptions{Archive: true, NoOverwriteDir: true, User: "1000"}); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	dumped, err := os.ReadFile(stdinDump)
	if err != nil || !bytes.Equal(dumped, payload) {
		t.Fatalf("CopyTo stdin mismatch: len=%d err=%v", len(dumped), err)
	}

	// Export streams tar.
	erc, err := c.Export(ctx, target, "/tmp", true)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	eb, err := io.ReadAll(erc)
	if err != nil || len(eb) == 0 {
		t.Fatalf("Export read: len=%d err=%v", len(eb), err)
	}
	if err := erc.Close(); err != nil {
		t.Fatalf("Export close: %v", err)
	}

	// Verify -cache-dir was never passed to any control command.
	logBytes, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBytes), "-cache-dir") {
		t.Fatalf("control command received -cache-dir:\n%s", logBytes)
	}
}
