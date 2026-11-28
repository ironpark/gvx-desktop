package rungv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGPUAssetNames(t *testing.T) {
	if got := ExtAssetName("v0.28.1", "darwin", "arm64"); got != "rungvx-v0.28.1-darwin-arm64-ext.tar.gz" {
		t.Fatalf("ExtAssetName(darwin/arm64) = %q", got)
	}
	if got := ExtAssetName("v0.28.1", "linux", "arm64"); got != "" {
		t.Fatalf("ExtAssetName(linux/arm64) = %q, want empty", got)
	}
	if got := GPURendererAssetName("v0.28.1", "darwin", "arm64"); got != "gvx-gpu-renderer-v0.28.1-darwin-arm64.tar.gz" {
		t.Fatalf("GPURendererAssetName(darwin/arm64) = %q", got)
	}
	if got := GPURendererAssetName("v0.28.1", "darwin", "amd64"); got != "" {
		t.Fatalf("GPURendererAssetName(darwin/amd64) = %q, want empty", got)
	}
}

func TestFindGPURendererRelease(t *testing.T) {
	makeRel := func(tag string, pre, withRenderer bool) Release {
		assets := []Asset{
			{Name: "rungvx-" + tag + "-darwin-arm64.tar.gz"},
			{Name: "SHA256SUMS"},
		}
		if withRenderer {
			assets = append(assets, Asset{Name: "gvx-gpu-renderer-" + tag + "-darwin-arm64.tar.gz"})
		}
		return Release{Tag: tag, Prerelease: pre, Assets: assets}
	}

	rel29 := makeRel("v0.29.0", false, false)
	rel29RC := makeRel("v0.29.1-rc1", true, true)
	rel28 := makeRel("v0.28.0", false, true)
	rel27 := makeRel("v0.27.0", false, true)
	rel26 := makeRel("v0.26.0", false, false)

	// 1. Preferred release carries renderer -> picked directly.
	if got, ok := findGPURendererReleaseFor(rel28, []Release{rel29, rel28, rel27}, "darwin", "arm64"); !ok || got.Tag != rel28.Tag {
		t.Fatalf("preferred with renderer = (%q, %v), want %q", got.Tag, ok, rel28.Tag)
	}

	// 2. Preferred release lacks renderer -> falls back to newest stable release with renderer (skipping prerelease).
	if got, ok := findGPURendererReleaseFor(rel29, []Release{rel29RC, rel29, rel28, rel27}, "darwin", "arm64"); !ok || got.Tag != rel28.Tag {
		t.Fatalf("fallback stable = (%q, %v), want %q", got.Tag, ok, rel28.Tag)
	}

	// 3. Only prerelease has renderer -> falls back to prerelease.
	if got, ok := findGPURendererReleaseFor(rel29, []Release{rel29RC, rel29, rel26}, "darwin", "arm64"); !ok || got.Tag != rel29RC.Tag {
		t.Fatalf("fallback prerelease = (%q, %v), want %q", got.Tag, ok, rel29RC.Tag)
	}

	// 4. No release has renderer -> not found.
	if got, ok := findGPURendererReleaseFor(rel29, []Release{rel29, rel26}, "darwin", "arm64"); ok {
		t.Fatalf("expected none, got %q", got.Tag)
	}

	// 5. Unsupported platform -> never found.
	if _, ok := findGPURendererReleaseFor(rel28, []Release{rel28}, "linux", "amd64"); ok {
		t.Fatal("expected unsupported platform to return false")
	}

	// 6. Backport published after a newer minor does not beat higher semver.
	rel27Backport := makeRel("v0.27.2", false, true)
	if got, ok := findGPURendererReleaseFor(rel29, []Release{rel27Backport, rel28}, "darwin", "arm64"); !ok || got.Tag != rel28.Tag {
		t.Fatalf("semver preference = (%q, %v), want %q", got.Tag, ok, rel28.Tag)
	}
}

func makeGPURendererTar(t *testing.T, entries []struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir || tf == tar.TypeSymlink {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestInstallWithOptionsExtAndFallback(t *testing.T) {
	if !HostSupportsGPUExtension() {
		t.Skip("GPU -ext release archives are darwin/arm64 only")
	}
	const tag = "v0.28.10002026112300"
	stdName := AssetNames(tag, runtime.GOOS, runtime.GOARCH)[0]
	extName := ExtAssetName(tag, runtime.GOOS, runtime.GOARCH)
	stdArchive := archiveOf(t, stdName, []byte("standard-rungvx"))
	extArchive := archiveOf(t, extName, []byte("ext-rungvx"))

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/std", func(w http.ResponseWriter, r *http.Request) { w.Write(stdArchive) })
	mux.HandleFunc("/ext", func(w http.ResponseWriter, r *http.Request) { w.Write(extArchive) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sha256Hex(stdArchive) + "  " + stdName + "\n" + sha256Hex(extArchive) + "  " + extName + "\n"))
	})

	relWithExt := Release{
		Tag: tag,
		Assets: []Asset{
			{Name: stdName, URL: srv.URL + "/std", Size: int64(len(stdArchive))},
			{Name: extName, URL: srv.URL + "/ext", Size: int64(len(extArchive))},
			{Name: "SHA256SUMS", URL: srv.URL + "/sums"},
		},
	}
	relWithoutExt := Release{
		Tag: tag,
		Assets: []Asset{
			{Name: stdName, URL: srv.URL + "/std", Size: int64(len(stdArchive))},
			{Name: "SHA256SUMS", URL: srv.URL + "/sums"},
		},
	}

	target := filepath.Join(t.TempDir(), "bin", BinaryName())

	// PreferExt=true with -ext asset -> installs -ext binary.
	usedExt, err := InstallWithOptions(context.Background(), relWithExt, target, InstallOptions{PreferExt: true}, nil)
	if err != nil || !usedExt {
		t.Fatalf("InstallWithOptions(PreferExt, withExt) = (%v, %v), want (true, nil)", usedExt, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "ext-rungvx" {
		t.Fatalf("installed binary = %q, want ext-rungvx", got)
	}

	// PreferExt=true without -ext asset -> gracefully falls back to standard binary.
	usedExt, err = InstallWithOptions(context.Background(), relWithoutExt, target, InstallOptions{PreferExt: true}, nil)
	if err != nil || usedExt {
		t.Fatalf("InstallWithOptions(PreferExt, withoutExt) = (%v, %v), want (false, nil)", usedExt, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "standard-rungvx" {
		t.Fatalf("installed binary = %q, want standard-rungvx", got)
	}
}

func TestInstallAndLocateGPURenderer(t *testing.T) {
	if !HostSupportsGPUExtension() {
		t.Skip("gvx-gpu-renderer release archives are darwin/arm64 only")
	}
	t.Setenv("GVX_GPU_RENDERER", "")

	const tag = "v0.28.10002026112300"
	renName := GPURendererAssetName(tag, runtime.GOOS, runtime.GOARCH)
	renArchive := makeGPURendererTar(t, []struct {
		name     string
		typeflag byte
		body     string
		linkname string
		mode     int64
	}{
		{name: "gvx-gpu-renderer/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "gvx-gpu-renderer/BUILDINFO", body: "tag=" + tag},
		{name: "gvx-gpu-renderer/bin/gvx-gpu-renderer", body: "#!/bin/sh\necho ok\n", mode: 0o755},
		{name: "gvx-gpu-renderer/lib/libvulkan.dylib", body: "dylib-bytes", mode: 0o644},
		{name: "gvx-gpu-renderer/lib/libvulkan.1.dylib", typeflag: tar.TypeSymlink, linkname: "libvulkan.dylib"},
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/ren", func(w http.ResponseWriter, r *http.Request) { w.Write(renArchive) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sha256Hex(renArchive) + "  " + renName + "\n"))
	})

	rel := Release{
		Tag: tag,
		Assets: []Asset{
			{Name: renName, URL: srv.URL + "/ren", Size: int64(len(renArchive))},
			{Name: "SHA256SUMS", URL: srv.URL + "/sums"},
		},
	}

	root := t.TempDir()
	rungvTarget := filepath.Join(root, "bin", BinaryName())
	if err := os.MkdirAll(filepath.Dir(rungvTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rungvTarget, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	binPath, err := InstallGPURenderer(context.Background(), rel, rungvTarget, nil)
	if err != nil {
		t.Fatalf("InstallGPURenderer = %v", err)
	}
	wantBin := filepath.Join(root, "libexec", "gvx-gpu-renderer", "bin", "gvx-gpu-renderer")
	if binPath != wantBin {
		t.Fatalf("binPath = %q, want %q", binPath, wantBin)
	}
	if got := LocateGPURenderer(rungvTarget); got != wantBin {
		t.Fatalf("LocateGPURenderer = %q, want %q", got, wantBin)
	}
	if ver := GPURendererVersion(rungvTarget); ver != tag {
		t.Fatalf("GPURendererVersion = %q, want %q", ver, tag)
	}
	if dylib, err := os.ReadFile(filepath.Join(root, "libexec", "gvx-gpu-renderer", "lib", "libvulkan.dylib")); err != nil || string(dylib) != "dylib-bytes" {
		t.Fatalf("libvulkan.dylib = (%q, %v)", dylib, err)
	}
	if targetLink, err := os.Readlink(filepath.Join(root, "libexec", "gvx-gpu-renderer", "lib", "libvulkan.1.dylib")); err != nil || targetLink != "libvulkan.dylib" {
		t.Fatalf("symlink = (%q, %v)", targetLink, err)
	}

	// Release without renderer returns ErrNoGPURenderer without removing installed copy.
	emptyRel := Release{Tag: "v0.29.0", Assets: []Asset{{Name: "SHA256SUMS", URL: srv.URL + "/sums"}}}
	if _, err := InstallGPURenderer(context.Background(), emptyRel, rungvTarget, nil); !errors.Is(err, ErrNoGPURenderer) {
		t.Fatalf("InstallGPURenderer(emptyRel) = %v, want ErrNoGPURenderer", err)
	}
	if got := LocateGPURenderer(rungvTarget); got != wantBin {
		t.Fatalf("existing renderer was disturbed: got %q", got)
	}
}

func TestExtractGPURendererTarRejectsTraversal(t *testing.T) {
	badTar := makeGPURendererTar(t, []struct {
		name     string
		typeflag byte
		body     string
		linkname string
		mode     int64
	}{
		{name: "../evil", body: "boom"},
	})
	if err := extractGPURendererTar(bytes.NewReader(badTar), t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected traversal error, got %v", err)
	}

	badSym := makeGPURendererTar(t, []struct {
		name     string
		typeflag byte
		body     string
		linkname string
		mode     int64
	}{
		{name: "gvx-gpu-renderer/lib/escape", typeflag: tar.TypeSymlink, linkname: "../../../etc/passwd"},
	})
	if err := extractGPURendererTar(bytes.NewReader(badSym), t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected symlink escape error, got %v", err)
	}

	// Symlink-chain traversal: a/b/c -> ../.. followed by a/b/c/esc -> ../..
	chainTar := makeGPURendererTar(t, []struct {
		name     string
		typeflag byte
		body     string
		linkname string
		mode     int64
	}{
		{name: "gvx-gpu-renderer/a/b/c", typeflag: tar.TypeSymlink, linkname: "../.."},
		{name: "gvx-gpu-renderer/a/b/c/esc", typeflag: tar.TypeSymlink, linkname: "../.."},
		{name: "gvx-gpu-renderer/a/b/c/esc/evil", body: "escaped"},
	})
	if err := extractGPURendererTar(bytes.NewReader(chainTar), t.TempDir()); err == nil {
		t.Fatal("expected symlink-chain traversal to be rejected")
	}
}

func TestGPURendererVersionIgnoresArbitraryEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	// Create /tmp/.../usr/local/bin/gvx-gpu-renderer and /tmp/.../usr/.version
	bin := filepath.Join(tmp, "usr", "local", "bin", "gvx-gpu-renderer")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "usr", "local", ".version"), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GVX_GPU_RENDERER", bin)
	if got := GPURendererVersion(""); got != "" {
		t.Fatalf("GPURendererVersion = %q, want empty for non-bundle GVX_GPU_RENDERER path", got)
	}
}

func TestCommandUsageDoesNotCacheExecFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0644 non-executable test is unix-only")
	}
	bin := filepath.Join(t.TempDir(), "rungvx")
	// Initially non-executable (EACCES) -> must NOT cache empty usage.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '  -gpu string'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ForgetFeatures(bin) })
	c := &Client{Path: bin}
	if c.HasRunFlag(context.Background(), "gpu") {
		t.Fatal("non-executable binary should report gpu=false")
	}
	// Make executable (simulating post-chmod/post-sign) -> must re-probe without ForgetFeatures.
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if !c.HasRunFlag(context.Background(), "gpu") {
		t.Fatal("expected HasRunFlag to re-probe after earlier EACCES failure")
	}
}
