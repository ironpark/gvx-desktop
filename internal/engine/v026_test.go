package engine

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestBindSymlinksInSandboxEngine(t *testing.T) {
	hostDir := t.TempDir()
	hostFile := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(hostFile, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingDir := filepath.Join(t.TempDir(), "will-be-created")

	allow := true
	te := newTestEngine(t)
	te.cfg.BindSymlinks = func() bool { return allow }
	te.fake.features = rungv.Features{BindSymlinks: true}
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	create := func(name string, binds []string, mounts []map[string]any) Container {
		t.Helper()
		hc := map[string]any{}
		if len(binds) > 0 {
			hc["Binds"] = binds
		}
		if len(mounts) > 0 {
			hc["Mounts"] = mounts
		}
		st, body := te.do(t, http.MethodPost, "/v1.52/containers/create?name="+name, map[string]any{
			"Image":      "alpine:3.20",
			"HostConfig": hc,
		})
		if st != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", name, st, body)
		}
		c, err := te.lookup(name)
		if err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		return c
	}

	// Enabled + v0.26+: existing directory gets symlinks=contained, while regular
	// file and non-existent path stay default so rungvx never rejects them.
	c1 := create("sym-on", []string{
		hostDir + ":/work",
		hostFile + ":/etc/app.conf:ro",
		missingDir + ":/newdir",
	}, nil)
	byTarget := map[string]sandbox.Mount{}
	for _, m := range c1.Spec.Mounts {
		byTarget[m.Target] = m
	}
	if got := byTarget["/work"].Symlinks; got != sandbox.SymlinksContained {
		t.Errorf("dir bind Symlinks = %q, want %q", got, sandbox.SymlinksContained)
	}
	if got := byTarget["/etc/app.conf"].Symlinks; got != "" {
		t.Errorf("file bind Symlinks = %q, want empty", got)
	}
	if got := byTarget["/newdir"].Symlinks; got != "" {
		t.Errorf("missing dir bind Symlinks = %q, want empty", got)
	}

	// HostConfig.Mounts type=bind directory also gets symlinks=contained.
	c2 := create("sym-mount", nil, []map[string]any{
		{"Type": "bind", "Source": hostDir, "Target": "/src", "ReadOnly": true},
	})
	if len(c2.Spec.Mounts) != 1 || c2.Spec.Mounts[0].Symlinks != sandbox.SymlinksContained {
		t.Errorf("HostConfig.Mounts bind = %+v, want SymlinksContained", c2.Spec.Mounts)
	}

	// Disabled setting -> stays empty.
	allow = false
	c3 := create("sym-off", []string{hostDir + ":/work"}, nil)
	if len(c3.Spec.Mounts) != 1 || c3.Spec.Mounts[0].Symlinks != "" {
		t.Errorf("setting off bind = %+v, want empty Symlinks", c3.Spec.Mounts)
	}

	// Enabled setting on pre-v0.26 rungvx -> gracefully stays empty.
	allow = true
	te.fake.features = rungv.Features{}
	c4 := create("sym-old-rungvx", []string{hostDir + ":/work"}, nil)
	if len(c4.Spec.Mounts) != 1 || c4.Spec.Mounts[0].Symlinks != "" {
		t.Errorf("old rungvx bind = %+v, want empty Symlinks", c4.Spec.Mounts)
	}
}
