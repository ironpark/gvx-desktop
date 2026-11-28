package rungv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestCheckpointArgs(t *testing.T) {
	target := Target{Socket: "/tmp/s.sock", TokenFile: "/tmp/s.sock.token"}

	gotDir, err := CheckpointArgs(target, CheckpointOptions{Dir: "/tmp/b", LeaveRunning: true})
	if err != nil {
		t.Fatalf("CheckpointArgs(Dir) error = %v", err)
	}
	wantDir := []string{"checkpoint", "-control-socket", "/tmp/s.sock", "--token-file", "/tmp/s.sock.token", "-checkpoint-dir", "/tmp/b", "-leave-running"}
	if !slices.Equal(gotDir, wantDir) {
		t.Fatalf("CheckpointArgs(Dir) = %q, want %q", gotDir, wantDir)
	}

	gotStream, err := CheckpointArgs(target, CheckpointOptions{Output: "/tmp/out", LeaveRunning: true, Compress: true, Force: true})
	if err != nil {
		t.Fatalf("CheckpointArgs(Output) error = %v", err)
	}
	wantStream := []string{"checkpoint", "-control-socket", "/tmp/s.sock", "--token-file", "/tmp/s.sock.token", "-o", "/tmp/out", "-z", "-leave-running", "--force"}
	if !slices.Equal(gotStream, wantStream) {
		t.Fatalf("CheckpointArgs(Output) = %q, want %q", gotStream, wantStream)
	}

	if _, err := CheckpointArgs(Target{}, CheckpointOptions{Output: "/tmp/out"}); err == nil {
		t.Fatal("expected error for empty socket")
	}
	if _, err := CheckpointArgs(target, CheckpointOptions{Dir: "/a", Output: "/b"}); err == nil {
		t.Fatal("expected error when both Dir and Output are set")
	}
	if _, err := CheckpointArgs(target, CheckpointOptions{Dir: "/a", Compress: true}); err == nil {
		t.Fatal("expected error when Compress is set without Output")
	}
}

func TestPortForwardAndDmesgArgs(t *testing.T) {
	c := &Client{Path: "rungvx"}
	target := Target{Socket: "/tmp/s.sock", TokenFile: "/tmp/t"}

	pf := c.PortForwardArgs(target, []string{"18080:80", " ", "127.0.0.1:9000:9000"}, PortForwardOptions{
		Address: "127.0.0.1",
		GuestIP: "10.0.2.15",
	})
	wantPF := []string{"port-forward", "-S", "/tmp/s.sock", "--token-file", "/tmp/t", "--address", "127.0.0.1", "--guest-ip", "10.0.2.15", "--", "18080:80", "127.0.0.1:9000:9000"}
	if !slices.Equal(pf, wantPF) {
		t.Fatalf("PortForwardArgs = %q, want %q", pf, wantPF)
	}

	dm := c.DmesgArgs(target, DmesgOptions{Follow: true, Tail: 50, Level: 4, Clear: true})
	wantDM := []string{"dmesg", "-S", "/tmp/s.sock", "--token-file", "/tmp/t", "--follow", "--tail", "50", "--level", "4", "--clear"}
	if !slices.Equal(dm, wantDM) {
		t.Fatalf("DmesgArgs = %q, want %q", dm, wantDM)
	}
}

func TestCtrl2CommandsExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary")
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "rungvx")
	script := `#!/bin/sh
echo "$@" >> "` + logFile + `"
if [ "$1" = "dmesg" ]; then
  echo "[    0.000000] <6> gVisor booted"
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Path: bin}
	target := Target{Socket: "/tmp/s.sock"}
	ctx := context.Background()

	if err := c.Remount(ctx, target, "/data", true); err != nil {
		t.Fatalf("Remount(ro) = %v", err)
	}
	if err := c.Remount(ctx, target, "/data", false); err != nil {
		t.Fatalf("Remount(rw) = %v", err)
	}
	if err := c.Remount(ctx, target, "-bad", true); err == nil {
		t.Fatal("expected error for leading-dash mount target")
	}

	out, err := c.Dmesg(ctx, target, DmesgOptions{Tail: 20, Level: 6})
	if err != nil {
		t.Fatalf("Dmesg = %v", err)
	}
	if !strings.Contains(out, "gVisor booted") {
		t.Fatalf("Dmesg output = %q, want kernel log line", out)
	}
	if _, err := c.Dmesg(ctx, target, DmesgOptions{Follow: true}); err == nil {
		t.Fatal("expected error when Follow=true is passed to unary Dmesg")
	}

	if err := c.SetLogLevel(ctx, target, "DEBUG"); err != nil {
		t.Fatalf("SetLogLevel(DEBUG) = %v", err)
	}
	if err := c.SetLogLevel(ctx, target, "trace"); err == nil {
		t.Fatal("expected error for invalid log level")
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	wantLines := []string{
		"mount remount -S /tmp/s.sock --ro -- /data",
		"mount remount -S /tmp/s.sock --rw -- /data",
		"dmesg -S /tmp/s.sock --tail 20 --level 6",
		"debug -S /tmp/s.sock --log-level debug",
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("recorded invocations:\n got: %q\nwant: %q", lines, wantLines)
	}
}
