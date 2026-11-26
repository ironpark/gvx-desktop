package rungv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestFeaturesAndVolumeDirFlagV026(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rungvx")
	argsLog := filepath.Join(dir, "args.log")
	t.Cleanup(func() { ForgetFeatures(bin) })

	script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
cat <<'EOF'
Usage: rungvx run [flags] IMAGE
  -allow-bind-attach
  -auto-publish-addr string
  -cache-dir string
  -control-socket string
  -control-token-file string
  -dynamic-ports
  -dynamic-ports-addr value
  -keep-blobs
  -mount value
    	Mount with Docker syntax: type=bind|volume|tmpfs|overlay,src=...,dst=...[,readonly][,driver=ext4lite|hostfs][,symlinks=virtual|contained]
  -nat-policy-file string
  -net-rx-bps uint
  -net-tx-bps uint
  -unix-forward value
  -volume-dir string
    	Named volume directory (overrides -cache-dir and GVX_VOLUME_DIR)
EOF
exit 2
fi
printf '%s\n' "$@" > "` + argsLog + `"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := &Client{Path: bin, CacheDir: "/tmp/cache", VolumeDir: "/data/vols"}

	f := c.Features(ctx)
	if !f.VolumeDirFlag {
		t.Fatalf("Features().VolumeDirFlag = false, want true (%+v)", f)
	}
	if !f.BindSymlinks {
		t.Fatalf("Features().BindSymlinks = false, want true (%+v)", f)
	}

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "run gets both cache-dir and volume-dir",
			args: []string{"run", "alpine:3.20"},
			want: []string{bin, "run", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols", "alpine:3.20"},
		},
		{
			name: "restore gets both cache-dir and volume-dir",
			args: []string{"restore", "-checkpoint-dir", "/ck"},
			want: []string{bin, "restore", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols", "-checkpoint-dir", "/ck"},
		},
		{
			name: "volume ls gets flags after subcommand",
			args: []string{"volume", "ls", "--format", "json"},
			want: []string{bin, "volume", "ls", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols", "--format", "json"},
		},
		{
			name: "system df gets flags after subcommand",
			args: []string{"system", "df", "--format", "json"},
			want: []string{bin, "system", "df", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols", "--format", "json"},
		},
		{
			name: "info gets both flags",
			args: []string{"info", "--format", "json"},
			want: []string{bin, "info", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols", "--format", "json"},
		},
		{
			name: "pull only gets cache-dir, never volume-dir",
			args: []string{"pull", "alpine:3.20"},
			want: []string{bin, "pull", "-cache-dir", "/tmp/cache", "alpine:3.20"},
		},
	}
	for _, tc := range cases {
		cmd := c.Command(ctx, tc.args...)
		if !slices.Equal(cmd.Args, tc.want) {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, cmd.Args, tc.want)
		}
		if !slices.Contains(cmd.Env, VolumeDirEnv+"=/data/vols") {
			t.Errorf("%s: missing %s in Env", tc.name, VolumeDirEnv)
		}
	}

	// AddMount passes -volume-dir for volume mounts, never for bind mounts.
	target := Target{Socket: "/tmp/s.sock"}
	if err := c.AddMount(ctx, target, MountAddOptions{Type: "volume", Source: "pgdata", Target: "/data"}); err != nil {
		t.Fatalf("AddMount volume: %v", err)
	}
	wantVolMount := []string{"mount", "add", "-S", "/tmp/s.sock", "--type", "volume", "--src", "pgdata", "--dst", "/data", "-cache-dir", "/tmp/cache", "-volume-dir", "/data/vols"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantVolMount) {
		t.Errorf("AddMount volume args =\n got  %q\n want %q", got, wantVolMount)
	}
	if err := c.AddMount(ctx, target, MountAddOptions{Type: "bind", Source: "/host/dir", Target: "/guest/dir"}); err != nil {
		t.Fatalf("AddMount bind: %v", err)
	}
	wantBindMount := []string{"mount", "add", "-S", "/tmp/s.sock", "--type", "bind", "--src", "/host/dir", "--dst", "/guest/dir"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantBindMount) {
		t.Errorf("AddMount bind args =\n got  %q\n want %q", got, wantBindMount)
	}
}

func TestVolumeDirFlagSkippedOnOlderRungv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rungv-old")
	t.Cleanup(func() { ForgetFeatures(bin) })

	script := `#!/bin/sh
cat <<'EOF'
Usage: rungv run [flags] IMAGE
  -cache-dir string
  -mount value
    	Mount with Docker syntax: type=bind|volume|tmpfs,src=...,dst=...
EOF
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := &Client{Path: bin, CacheDir: "/tmp/cache", VolumeDir: "/data/vols"}
	f := c.Features(ctx)
	if f.VolumeDirFlag || f.BindSymlinks {
		t.Fatalf("old binary features = %+v, want VolumeDirFlag=false BindSymlinks=false", f)
	}
	cmd := c.Command(ctx, "volume", "ls")
	want := []string{bin, "volume", "ls", "-cache-dir", "/tmp/cache"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("old binary args = %q, want %q", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, VolumeDirEnv+"=/data/vols") {
		t.Errorf("old binary still sets %s env", VolumeDirEnv)
	}
}
