package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestHasProgramPrefixForSandboxStderr(t *testing.T) {
	for _, c := range []struct {
		line string
		want bool
	}{
		{"rungv: platform=hvf", true},
		{"rungvx: platform=hvf", true},
		{"rungvx: publishing 127.0.0.1:8080 -> 80/tcp", true},
		{"rungv run: gvx: executable /nope not found", false},
		{"rungvx run: gvx: executable /nope not found", false},
		{"rungvy: something else", false},
		{"error: boom", false},
	} {
		if got := hasProgramPrefix(c.line, ":"); got != c.want {
			t.Errorf("hasProgramPrefix(%q, \":\") = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestSelectAndInstallSpecificRungvRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	dir := t.TempDir()

	rel23 := rungv.Release{Tag: "v0.23.10022026112300", Page: "https://example.com/23"}
	rel22 := rungv.Release{Tag: "v0.22.10002026111600", Page: "https://example.com/22"}
	relPre := rungv.Release{Tag: "v0.24.0-rc1", Page: "https://example.com/24", Prerelease: true}

	oldReleases := fetchRungvReleases
	oldLatest := fetchRungvLatest
	oldInstall := installRungvRelease
	oldSign := signRungvBinary
	t.Cleanup(func() {
		fetchRungvReleases = oldReleases
		fetchRungvLatest = oldLatest
		installRungvRelease = oldInstall
		signRungvBinary = oldSign
	})

	fetchRungvReleases = func(context.Context) ([]rungv.Release, error) {
		return []rungv.Release{relPre, rel23, rel22}, nil
	}
	fetchRungvLatest = func(context.Context) (rungv.Release, error) {
		return rel23, nil
	}
	signRungvBinary = func(context.Context, string) error { return nil }
	var installedTag string
	installRungvRelease = func(_ context.Context, r rungv.Release, target string, progress func(done, total int64)) error {
		installedTag = r.Tag
		if progress != nil {
			progress(50, 100)
			progress(100, 100)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte("#!/bin/sh\necho 'rungv "+r.Tag+"'\n"), 0o755)
	}

	stStore := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	if err := stStore.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 900))
	p.Setup(func() {
		m = NewModel(stStore, dir)
		m.Attach(p)
		m.Engine.Set(Engine{
			Path:    filepath.Join(dir, "rungv"),
			Checked: true,
			Version: rungv.Version{Program: "rungvx", Version: rel23.Tag},
			Info:    rungv.Info{OS: "linux", AvailableBackends: []string{"kvm"}, DefaultBackend: "kvm"},
		})
	})
	defer p.Close()
	p.Frame()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	m.CheckUpdate(true)
	waitFor("releases list loaded", func() bool {
		return len(ggui.Peek(m.Releases)) == 3 && !ggui.Peek(m.Update).Checking
	})

	// Newest stable release (not prerelease) should be chosen as latest.
	if got := ggui.Peek(m.Update).Tag; got != rel23.Tag {
		t.Fatalf("Update.Tag = %q, want %q", got, rel23.Tag)
	}

	// Open Settings and confirm Release version selector appears.
	m.Go(PageSettings)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "Release version") {
		t.Fatalf("Settings missing Release version selector:\n%s", sem)
	}

	// Select an older release (downgrade path).
	m.SetSelectedRelease(rel22.Tag)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "older than v0.23.1002") || !strings.Contains(sem, "Install v0.22.1000") {
		t.Fatalf("Settings downgrade state unexpected:\n%s", sem)
	}

	// Select the currently installed release -> Reinstall state.
	m.SetSelectedRelease(rel23.Tag)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "is currently installed") || !strings.Contains(sem, "Reinstall") {
		t.Fatalf("Settings installed state unexpected:\n%s", sem)
	}

	// Install the older release explicitly and confirm Update.Tag stays at latest.
	m.SetSelectedRelease(rel22.Tag)
	m.InstallRungvRelease(rel22.Tag)
	waitFor("install finished", func() bool {
		return installedTag == rel22.Tag && !ggui.Peek(m.Update).Installing
	})
	if got := ggui.Peek(m.Update).Tag; got != rel23.Tag {
		t.Fatalf("Update.Tag changed after installing older release: got %q, want %q", got, rel23.Tag)
	}

	// Fallback to fetchRungvLatest when fetchRungvReleases fails.
	fetchRungvReleases = func(context.Context) ([]rungv.Release, error) {
		return nil, errors.New("list unavailable")
	}
	m.CheckUpdate(true)
	waitFor("fallback latest loaded", func() bool {
		return len(ggui.Peek(m.Releases)) == 1 && !ggui.Peek(m.Update).Checking
	})
	if got := ggui.Peek(m.SelectedRelease); got != "" {
		t.Fatalf("SelectedRelease = %q after disappearing from release list, want empty", got)
	}

	// Switching to source channel clears selected release.
	m.SelectedRelease.Set(rel23.Tag)
	m.Offline = true
	m.SetChannel(ChannelSource)
	if got := ggui.Peek(m.SelectedRelease); got != "" {
		t.Fatalf("SelectedRelease = %q after switching to source channel, want empty", got)
	}
}

func TestInstallRungvWithGPUHandlesMissingRendererInRelease(t *testing.T) {
	if !rungv.HostSupportsGPUExtension() {
		t.Skip("GPU extension updater tests run on darwin/arm64")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GVX_GPU_RENDERER", "")
	dir := t.TempDir()

	makeRel := func(tag string, withExt, withRenderer bool) rungv.Release {
		assets := []rungv.Asset{
			{Name: rungv.AssetNames(tag, "darwin", "arm64")[0]},
			{Name: "SHA256SUMS"},
		}
		if withExt {
			assets = append(assets, rungv.Asset{Name: rungv.ExtAssetName(tag, "darwin", "arm64")})
		}
		if withRenderer {
			assets = append(assets, rungv.Asset{Name: rungv.GPURendererAssetName(tag, "darwin", "arm64")})
		}
		return rungv.Release{Tag: tag, Page: "https://example.com/" + tag, Assets: assets}
	}

	rel29NoRen := makeRel("v0.29.0", true, false) // has -ext, NO gvx-gpu-renderer
	rel28Full := makeRel("v0.28.0", true, true)   // has -ext AND gvx-gpu-renderer
	rel27NoExt := makeRel("v0.27.0", false, true) // NO -ext

	oldReleases := fetchRungvReleases
	oldLatest := fetchRungvLatest
	oldInstallOpts := installRungvReleaseOpts
	oldInstallRen := installRungvGPURenderer
	oldSign := signRungvBinary
	oldGPUSupp := gpuExtensionSupported
	t.Cleanup(func() {
		fetchRungvReleases = oldReleases
		fetchRungvLatest = oldLatest
		installRungvReleaseOpts = oldInstallOpts
		installRungvGPURenderer = oldInstallRen
		signRungvBinary = oldSign
		gpuExtensionSupported = oldGPUSupp
	})

	gpuExtensionSupported = func() bool { return true }
	fetchRungvReleases = func(context.Context) ([]rungv.Release, error) {
		return []rungv.Release{rel29NoRen, rel28Full, rel27NoExt}, nil
	}
	fetchRungvLatest = func(context.Context) (rungv.Release, error) { return rel29NoRen, nil }
	signRungvBinary = func(context.Context, string) error { return nil }

	var (
		mu              sync.Mutex
		installedBinTag string
		rendererTags    []string
	)
	installRungvReleaseOpts = func(_ context.Context, r rungv.Release, target string, opts rungv.InstallOptions, _ func(done, total int64)) (bool, error) {
		mu.Lock()
		installedBinTag = r.Tag
		mu.Unlock()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(target, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
			return false, err
		}
		return opts.PreferExt && rungv.HasExtBuild(r), nil
	}
	installRungvGPURenderer = func(_ context.Context, r rungv.Release, rungvTarget string, _ func(done, total int64)) (string, error) {
		mu.Lock()
		rendererTags = append(rendererTags, r.Tag)
		mu.Unlock()
		bin := rungv.GPURendererBinary(rungvTarget)
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(rungv.GPURendererDir(rungvTarget), ".version"), []byte(r.Tag+"\n"), 0o644); err != nil {
			return "", err
		}
		return bin, nil
	}
	getBinTag := func() string {
		mu.Lock()
		defer mu.Unlock()
		return installedBinTag
	}
	setBinTag := func(v string) {
		mu.Lock()
		installedBinTag = v
		mu.Unlock()
	}
	getRendererTags := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(rendererTags))
		copy(out, rendererTags)
		return out
	}

	stStore := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.RungvGPU = true
	if err := stStore.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 900))
	p.Setup(func() {
		m = NewModel(stStore, dir)
		m.Attach(p)
		m.Engine.Set(Engine{
			Path:    filepath.Join(home, ".gvx", "bin", rungv.BinaryName()),
			Checked: true,
			Version: rungv.Version{Program: "rungvx", Version: rel27NoExt.Tag},
			Info:    rungv.Info{OS: "darwin", AvailableBackends: []string{"hvf"}, DefaultBackend: "hvf"},
		})
	})
	defer p.Close()
	p.Frame()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	m.CheckUpdate(true)
	waitFor("releases loaded", func() bool {
		return len(ggui.Peek(m.Releases)) == 3 && !ggui.Peek(m.Update).Checking
	})

	// 1. Install v0.29.0 (has -ext, NO renderer) with no renderer installed yet
	//    -> installs v0.29.0 -ext and falls back to v0.28.0 for gvx-gpu-renderer.
	m.InstallRungvRelease(rel29NoRen.Tag)
	waitFor("v0.29.0 installed", func() bool {
		return !ggui.Peek(m.Update).Installing && getBinTag() == rel29NoRen.Tag
	})
	if tags := getRendererTags(); len(tags) != 1 || tags[0] != rel28Full.Tag {
		t.Fatalf("rendererTags = %v, want [%s]", tags, rel28Full.Tag)
	}

	// 2. Reinstall v0.29.0 now that v0.28.0 renderer is already installed
	//    -> keeps existing renderer without re-downloading.
	setBinTag("")
	m.InstallRungvRelease(rel29NoRen.Tag)
	waitFor("v0.29.0 reinstalled", func() bool {
		return !ggui.Peek(m.Update).Installing && getBinTag() == rel29NoRen.Tag
	})
	if tags := getRendererTags(); len(tags) != 1 {
		t.Fatalf("expected existing renderer to be reused without download, got calls %v", tags)
	}

	// 3. Install v0.27.0 (no -ext) -> installs standard rungvx without touching renderer.
	setBinTag("")
	m.InstallRungvRelease(rel27NoExt.Tag)
	waitFor("v0.27.0 installed", func() bool {
		return !ggui.Peek(m.Update).Installing && getBinTag() == rel27NoExt.Tag
	})
	if tags := getRendererTags(); len(tags) != 1 {
		t.Fatalf("expected no renderer install for non-ext release, got calls %v", tags)
	}
}
