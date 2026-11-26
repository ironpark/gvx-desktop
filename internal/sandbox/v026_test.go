package sandbox

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestBindSymlinksValidationAndArgs(t *testing.T) {
	base := Spec{Name: "dev", Image: "node:22-alpine"}

	// Workspace with SymlinksContained emits --mount type=bind,...,symlinks=contained.
	ws := base
	ws.Workspace = "/tmp/proj"
	ws.WorkspaceRO = true
	ws.WorkspaceSymlinks = SymlinksContained
	if err := ws.Validate(); err != nil {
		t.Fatalf("workspace contained validate: %v", err)
	}
	if !ws.UsesBindSymlinks() {
		t.Fatal("UsesBindSymlinks = false for WorkspaceSymlinks=contained")
	}
	wantRun := []string{"run", "--name", "dev", "--mount", "type=bind,src=/tmp/proj,dst=/workspace,readonly,symlinks=contained", "node:22-alpine"}
	if got := ws.RunArgs(Paths{}); !reflect.DeepEqual(got, wantRun) {
		t.Fatalf("workspace RunArgs =\n got  %q\n want %q", got, wantRun)
	}
	wantRestore := []string{"restore", "-checkpoint-dir", "/ck", "--name", "dev", "--mount", "type=bind,src=/tmp/proj,dst=/workspace,readonly,symlinks=contained"}
	if got := ws.RestoreArgs("/ck", Paths{}); !reflect.DeepEqual(got, wantRestore) {
		t.Fatalf("workspace RestoreArgs =\n got  %q\n want %q", got, wantRestore)
	}

	// Bind mount with symlinks=contained emits --mount; default stays -v.
	bm := base
	bm.Mounts = []Mount{
		{Type: MountBind, Source: "/tmp/a", Target: "/a", Symlinks: SymlinksContained},
		{Type: MountBind, Source: "/tmp/b", Target: "/b", ReadOnly: true},
	}
	if err := bm.Validate(); err != nil {
		t.Fatalf("bind mount validate: %v", err)
	}
	wantBindRun := []string{
		"run", "--name", "dev",
		"--mount", "type=bind,src=/tmp/a,dst=/a,symlinks=contained",
		"-v", "/tmp/b:/b:ro",
		"node:22-alpine",
	}
	if got := bm.RunArgs(Paths{}); !reflect.DeepEqual(got, wantBindRun) {
		t.Fatalf("bind mount RunArgs =\n got  %q\n want %q", got, wantBindRun)
	}

	// Feature gate checks rungvx v0.26+.
	if err := checkSpecFeatures(bm, rungv.Features{}); err == nil || !strings.Contains(err.Error(), "v0.26") {
		t.Fatalf("checkSpecFeatures without BindSymlinks = %v, want v0.26 error", err)
	}
	if err := checkSpecFeatures(bm, rungv.Features{BindSymlinks: true}); err != nil {
		t.Fatalf("checkSpecFeatures with BindSymlinks = %v", err)
	}

	// Invalid cases.
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{
			name: "invalid workspace symlink mode",
			spec: Spec{Name: "dev", Image: "alpine", Workspace: "/tmp/p", WorkspaceSymlinks: "host"},
		},
		{
			name: "workspace path with comma when contained",
			spec: Spec{Name: "dev", Image: "alpine", Workspace: "/tmp/a,b", WorkspaceSymlinks: SymlinksContained},
		},
		{
			name: "symlinks on volume mount",
			spec: Spec{Name: "dev", Image: "alpine", Mounts: []Mount{{Type: MountVolume, Source: "data", Target: "/data", Symlinks: SymlinksContained}}},
		},
		{
			name: "bind mount comma when symlinks set",
			spec: Spec{Name: "dev", Image: "alpine", Mounts: []Mount{{Type: MountBind, Source: "/tmp/a,b", Target: "/a", Symlinks: SymlinksContained}}},
		},
	} {
		if err := tc.spec.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}
