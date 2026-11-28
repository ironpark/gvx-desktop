package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestComputeLiveDiffRemount(t *testing.T) {
	stCtrl := State{Status: Running, Control: true}

	// 1. Named volume rw -> ro remounts in place without requiring restart.
	runVol := Spec{
		ID:     "v1",
		Name:   "vol-box",
		Image:  "alpine:3.20",
		Mounts: []Mount{{Type: MountVolume, Source: "pgdata", Target: "/data", ReadOnly: false}},
	}
	edVolRO := runVol.Clone()
	edVolRO.Mounts[0].ReadOnly = true

	diff, live := ComputeLiveDiff(runVol, edVolRO, stCtrl)
	if !live {
		t.Fatal("expected volume rw -> ro to be live via mount.remount")
	}
	if len(diff.RemountMounts) != 1 || diff.RemountMounts[0].Target != "/data" || !diff.RemountMounts[0].ReadOnly {
		t.Fatalf("RemountMounts = %+v, want [/data ro]", diff.RemountMounts)
	}
	if len(diff.RemoveMounts) != 0 || len(diff.AddMounts) != 0 {
		t.Fatalf("expected in-place remount without remove/add, got remove=%v add=%v", diff.RemoveMounts, diff.AddMounts)
	}

	// 2. Named volume ro -> rw still requires restart (backing FD opened O_RDONLY).
	if _, live := ComputeLiveDiff(edVolRO, runVol, stCtrl); live {
		t.Fatal("expected volume ro -> rw to require restart")
	}

	// 3. tmpfs rw <-> ro remounts in place in both directions, preserving data.
	runTmp := Spec{
		ID:     "t1",
		Name:   "tmp-box",
		Image:  "alpine:3.20",
		Mounts: []Mount{{Type: MountTmpfs, Target: "/scratch", Size: "64m", ReadOnly: true}},
	}
	edTmpRW := runTmp.Clone()
	edTmpRW.Mounts[0].ReadOnly = false
	diffTmp, liveTmp := ComputeLiveDiff(runTmp, edTmpRW, stCtrl)
	if !liveTmp || len(diffTmp.RemountMounts) != 1 || diffTmp.RemountMounts[0].ReadOnly {
		t.Fatalf("tmpfs ro -> rw diff = (%+v, %v), want live RemountMounts rw", diffTmp, liveTmp)
	}

	// 4. Bind mount rw -> ro remounts in place even without BindAttach/LiveMounts.
	runBind := Spec{
		ID:     "b1",
		Name:   "bind-box",
		Image:  "alpine:3.20",
		Mounts: []Mount{{Type: MountBind, Source: "/tmp/work", Target: "/work", ReadOnly: false}},
	}
	edBindRO := runBind.Clone()
	edBindRO.Mounts[0].ReadOnly = true
	diffBind, liveBind := ComputeLiveDiff(runBind, edBindRO, stCtrl)
	if !liveBind || len(diffBind.RemountMounts) != 1 || !diffBind.RemountMounts[0].ReadOnly {
		t.Fatalf("bind rw -> ro diff = (%+v, %v), want live RemountMounts ro", diffBind, liveBind)
	}

	// 5. Changing bind Symlinks mode is not equal and does not trigger remount.
	edBindSym := runBind.Clone()
	edBindSym.Mounts[0].Symlinks = SymlinksContained
	if _, live := ComputeLiveDiff(runBind, edBindSym, stCtrl); live {
		t.Fatal("expected bind Symlinks mode change without BindAttach to require restart")
	}

	// 6. Live volume hotplug when VolumeAttach is enabled, plus live PIDs limit update.
	stVol := State{Status: Running, Control: true, VolumeAttach: true}
	edVolAdd := runVol.Clone()
	edVolAdd.PidsLimit = 256
	edVolAdd.Mounts = append(edVolAdd.Mounts, Mount{Type: MountVolume, Source: "extra", Target: "/extra"})
	diffVol, liveVol := ComputeLiveDiff(runVol, edVolAdd, stVol)
	if !liveVol {
		t.Fatal("expected volume attach + PIDs limit change to be live when VolumeAttach=true")
	}
	if diffVol.Update.PidsLimit == nil || *diffVol.Update.PidsLimit != 256 {
		t.Fatalf("diff.Update.PidsLimit = %v, want 256", diffVol.Update.PidsLimit)
	}
	if len(diffVol.AddMounts) != 1 || diffVol.AddMounts[0].Source != "extra" {
		t.Fatalf("diff.AddMounts = %+v, want [extra -> /extra]", diffVol.AddMounts)
	}

	// 7. Ephemeral volume mount cannot be attached live (requires restart).
	edVolEph := runVol.Clone()
	edVolEph.Mounts = append(edVolEph.Mounts, Mount{Type: MountVolume, Target: "/tmpvol", Ephemeral: true})
	if _, live := ComputeLiveDiff(runVol, edVolEph, stVol); live {
		t.Fatal("expected ephemeral volume mount to require restart")
	}
}

func TestSpecValidationNewCLIFlags(t *testing.T) {
	badRO := Spec{ID: "x", Name: "x", Image: "alpine", ReadOnlyRoot: true, RootfsVolume: "rootv"}
	if err := badRO.Validate(); err == nil {
		t.Fatal("expected ReadOnlyRoot + RootfsVolume to fail validation")
	}
	badGPU := Spec{ID: "x", Name: "x", Image: "alpine", GPUMemoryLimit: "512m"}
	if err := badGPU.Validate(); err == nil {
		t.Fatal("expected GPUMemoryLimit without GPU to fail validation")
	}
	badEphRO := Spec{
		ID: "x", Name: "x", Image: "alpine",
		Mounts: []Mount{{Type: MountVolume, Target: "/s", Ephemeral: true, ReadOnly: true}},
	}
	if err := badEphRO.Validate(); err == nil {
		t.Fatal("expected ephemeral read-only volume to fail validation")
	}
	okSpec := Spec{
		ID: "x", Name: "x", Image: "alpine",
		PidsLimit: 128, ReadOnlyRoot: true, GPU: "virgl", GPUMemoryLimit: "1g",
		EnvFiles: []string{"~/.env"},
		Mounts:   []Mount{{Type: MountVolume, Target: "/s", Size: "512m", Ephemeral: true}},
	}
	if err := okSpec.Validate(); err != nil {
		t.Fatalf("expected valid spec, got %v", err)
	}
	if vols := okSpec.Volumes(); len(vols) != 0 {
		t.Fatalf("ephemeral volume should not appear in Spec.Volumes(), got %v", vols)
	}
}

func TestSupervisorCtrl2StreamedCheckpointAndRuntimeOps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake rungvx")
	}
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "cmd.log")
	bin := filepath.Join(tmp, "rungvx")
	script := `#!/bin/sh
cmd="$1"
if [ "$cmd" = "run" ] && [ "$2" = "-help" ]; then
  cat <<'EOF'
Usage: rungvx run [flags] IMAGE
  -control-socket string
  -control-token-file string
  -dynamic-ports
EOF
  exit 2
fi
echo "$@" >> "` + logPath + `"
case "$cmd" in
  run)
    echo "READY"
    trap 'exit 0' INT TERM
    while true; do sleep 1; done
    ;;
  checkpoint)
    out=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "-o" ] || [ "$prev" = "-checkpoint-dir" ]; then
        out="$a"
      fi
      prev="$a"
    done
    if [ -n "$out" ]; then
      mkdir -p "$out"
      echo '{"version":3}' > "$out/manifest.json"
    fi
    exit 0
    ;;
  dmesg)
    echo "[    0.001000] <6> sentry ready"
    exit 0
    ;;
  mount|debug)
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(bin) })

	c := &rungv.Client{Path: bin}
	sup := &Supervisor{
		Client:  func() *rungv.Client { return c },
		DataDir: filepath.Join(tmp, "data"),
	}
	spec := Spec{
		ID:           "c2test01",
		Name:         "ctrl2-box",
		Image:        "alpine:3.20",
		Checkpoint:   false, // NOT enabled at start — streamed via gvx-ctrl/2!
		ReadyPattern: "READY",
		Mounts:       []Mount{{Type: MountVolume, Source: "vol1", Target: "/data", ReadOnly: false}},
	}
	if err := sup.Start(spec); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(spec.ID, 2*time.Second) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := sup.State(spec.ID); st.Status == Running && st.Control {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := sup.State(spec.ID); st.Status != Running || !st.Control {
		t.Fatalf("sandbox state = %+v, want Running with Control=true", st)
	}

	ctx := context.Background()

	// 1. Streamed checkpoint on a sandbox with Spec.Checkpoint == false.
	dir1, err := sup.Checkpoint(ctx, spec.ID, true)
	if err != nil {
		t.Fatalf("first streamed Checkpoint = %v", err)
	}
	// 2. Second streamed checkpoint in same second produces a distinct dir (no overwrite).
	dir2, err := sup.Checkpoint(ctx, spec.ID, true)
	if err != nil {
		t.Fatalf("second streamed Checkpoint = %v", err)
	}
	if dir1 == dir2 {
		t.Fatalf("expected distinct snapshot dirs, both were %q", dir1)
	}
	snaps := sup.Snapshots(spec.ID)
	if len(snaps) != 2 {
		t.Fatalf("Snapshots count = %d, want 2 (%+v)", len(snaps), snaps)
	}

	// 3. Live in-place remount of volume mount via ApplyLive.
	edited := spec.Clone()
	edited.Mounts[0].ReadOnly = true
	activeSpec, ok := sup.ActiveSpec(spec.ID)
	if !ok {
		t.Fatal("expected ActiveSpec to be found for running sandbox")
	}
	diff, live := ComputeLiveDiff(activeSpec, edited, sup.State(spec.ID))
	if !live {
		t.Fatal("expected volume rw -> ro to be live")
	}
	if err := sup.ApplyLive(ctx, spec.ID, diff); err != nil {
		t.Fatalf("ApplyLive(remount) = %v", err)
	}
	afterSpec, ok := sup.ActiveSpec(spec.ID)
	if !ok || !afterSpec.Mounts[0].ReadOnly {
		t.Fatalf("expected ActiveSpec mount ReadOnly=true after ApplyLive remount, got (%+v, %v)", afterSpec.Mounts, ok)
	}

	// 4. Guest kernel logs (dmesg) and runtime Sentry log level.
	kmsg, err := sup.Dmesg(ctx, spec.ID, rungv.DmesgOptions{Tail: 10})
	if err != nil {
		t.Fatalf("Dmesg = %v", err)
	}
	if !strings.Contains(kmsg, "sentry ready") {
		t.Fatalf("Dmesg = %q, want sentry ready", kmsg)
	}
	if err := sup.SetLogLevel(ctx, spec.ID, "info"); err != nil {
		t.Fatalf("SetLogLevel = %v", err)
	}

	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(rawLog)
	if !strings.Contains(logText, "checkpoint -control-socket ") || !strings.Contains(logText, " -o "+dir1+" -z -leave-running") {
		t.Fatalf("expected streamed checkpoint -o -z in log:\n%s", logText)
	}
	if !strings.Contains(logText, "mount remount -S ") || !strings.Contains(logText, "--ro -- /data") {
		t.Fatalf("expected mount remount --ro -- /data in log:\n%s", logText)
	}
	if !strings.Contains(logText, "debug -S ") || !strings.Contains(logText, "--log-level info") {
		t.Fatalf("expected debug --log-level info in log:\n%s", logText)
	}
}
