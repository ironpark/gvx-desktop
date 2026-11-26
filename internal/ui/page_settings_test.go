package ui

import (
	"path/filepath"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestVolumeDirWarning(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "u")
	caches := filepath.Join(home, "Library", "Caches", "gvx", "volumes")
	support := filepath.Join(home, "Library", "Application Support", "gvx", "volumes")
	mine := filepath.Join(home, "volumes")
	cacheDir := filepath.Join(home, "gvx-cache")
	notApplied := tr("Volume directory not applied")
	inCache := tr("Volumes are in a cache folder")
	for _, c := range []struct {
		name, goos, dir, cache, volume string
		hasVolFlag                     bool
		title, body                    string
	}{
		{"no report", "darwin", "", "", mine, false, "", ""},
		{"default outside caches", "darwin", support, "", "", false, "", ""},
		{"setting applied", "darwin", mine, "", mine, false, "", ""},
		{"setting applied with spaces", "darwin", mine, "", " " + mine + " ", false, "", ""},
		{"v0.24 cache and volume both applied", "darwin", mine, cacheDir, mine, true, "", ""},
		{"rungvx ignores the variable", "darwin", support, "", mine, false, notApplied,
			tr("This rungvx does not read GVX_VOLUME_DIR and keeps volumes in %s. Update rungvx to use the setting.", support)},
		{"cache setting wins before v0.24", "darwin", filepath.Join(cacheDir, "volumes"), cacheDir, mine, false, notApplied,
			tr("A layer cache directory is set, so this rungvx keeps volumes in %s. Clear the layer cache directory setting, or update rungvx to v0.24+ to use -volume-dir.", filepath.Join(cacheDir, "volumes"))},
		{"volume setting names the cache volumes", "darwin", filepath.Join(cacheDir, "volumes"), cacheDir, filepath.Join(cacheDir, "volumes"), false, "", ""},
		{"default in caches", "darwin", caches, "", "", false, inCache,
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Set a volume directory outside it, or update rungvx.")},
		{"cache setting in caches before v0.24", "darwin", caches, filepath.Dir(caches), "", false, inCache,
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Move the layer cache directory out of it.")},
		{"cache setting in caches on v0.24+", "darwin", caches, filepath.Dir(caches), "", true, inCache,
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Set a volume directory outside it, or move the layer cache directory out of it.")},
		{"caches on linux", "linux", caches, "", "", false, "", ""},
	} {
		title, body := volumeDirWarning(c.goos, home, c.dir, c.cache, c.volume, c.hasVolFlag)
		if title != c.title || body != c.body {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, title, body, c.title, c.body)
		}
	}
}

func TestV026BindSymlinksDraftAndSettings(t *testing.T) {
	// Round-trip Sandbox Draft <-> Spec for WorkspaceSymlinks and bind mount symlinks option.
	orig := sandbox.Spec{
		Name:              "dev",
		Image:             "node:22-alpine",
		Workspace:         "~/project",
		WorkspaceRO:       true,
		WorkspaceSymlinks: sandbox.SymlinksContained,
		Mounts: []sandbox.Mount{
			{Type: sandbox.MountBind, Source: "~/src", Target: "/src", ReadOnly: true, Symlinks: sandbox.SymlinksContained},
			{Type: sandbox.MountBind, Source: `C:\work\repo`, Target: "/repo", Symlinks: sandbox.SymlinksContained},
			{Type: sandbox.MountBind, Source: "~/plain", Target: "/plain"},
		},
	}
	draft := DraftFrom(orig)
	if !draft.WorkspaceSymlinks {
		t.Fatal("DraftFrom lost WorkspaceSymlinks")
	}
	got, err := draft.Spec()
	if err != nil {
		t.Fatalf("draft.Spec(): %v", err)
	}
	if got.WorkspaceSymlinks != sandbox.SymlinksContained {
		t.Errorf("WorkspaceSymlinks = %q, want %q", got.WorkspaceSymlinks, sandbox.SymlinksContained)
	}
	if len(got.Mounts) != 3 {
		t.Fatalf("Mounts = %+v, want 3", got.Mounts)
	}
	if got.Mounts[0].Source != "~/src" || got.Mounts[0].Target != "/src" || !got.Mounts[0].ReadOnly || got.Mounts[0].Symlinks != sandbox.SymlinksContained {
		t.Errorf("mount 0 = %+v", got.Mounts[0])
	}
	if got.Mounts[1].Source != `C:\work\repo` || got.Mounts[1].Target != "/repo" || got.Mounts[1].ReadOnly || got.Mounts[1].Symlinks != sandbox.SymlinksContained {
		t.Errorf("mount 1 = %+v", got.Mounts[1])
	}
	if got.Mounts[2].Symlinks != "" {
		t.Errorf("mount 2 Symlinks = %q, want empty", got.Mounts[2].Symlinks)
	}

	// Settings persistence for DockerBindSymlinks.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.DockerBindSymlinks = true
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.DockerBindSymlinks {
		t.Fatal("DockerBindSymlinks did not persist in store.Settings")
	}
}
