package rungv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// ErrNoGPURenderer reports a release with no gvx-gpu-renderer bundle for this
// machine. Not every gvx release publishes gvx-gpu-renderer.
var ErrNoGPURenderer = errors.New("the release has no gvx-gpu-renderer for this system")

// InstallOptions customizes how InstallWithOptions selects a release archive.
type InstallOptions struct {
	// PreferExt selects the darwin/arm64 -ext build (with GPU support) when
	// the release publishes one, falling back to the standard build otherwise.
	PreferExt bool
}

// SupportsGPUExtension reports whether goos/goarch has upstream gvx GPU
// extension (-ext) and gvx-gpu-renderer release artifacts (macOS Apple silicon).
func SupportsGPUExtension(goos, goarch string) bool {
	return goos == "darwin" && goarch == "arm64"
}

// HostSupportsGPUExtension reports whether this machine can use the upstream
// gvx GPU extension builds.
func HostSupportsGPUExtension() bool {
	return SupportsGPUExtension(runtime.GOOS, runtime.GOARCH)
}

// ExtAssetName returns the GPU-enabled -ext archive name for tag on goos/goarch,
// or "" when the platform has no -ext release builds.
func ExtAssetName(tag, goos, goarch string) string {
	if !SupportsGPUExtension(goos, goarch) || tag == "" {
		return ""
	}
	return programX + "-" + tag + "-" + goos + "-" + goarch + "-ext.tar.gz"
}

// GPURendererAssetName returns the gvx-gpu-renderer bundle archive name for tag
// on goos/goarch, or "" when the platform has no renderer release builds.
func GPURendererAssetName(tag, goos, goarch string) string {
	if !SupportsGPUExtension(goos, goarch) || tag == "" {
		return ""
	}
	return "gvx-gpu-renderer-" + tag + "-" + goos + "-" + goarch + ".tar.gz"
}

func extBuildFor(r Release, goos, goarch string) (Asset, bool) {
	name := ExtAssetName(r.Tag, goos, goarch)
	if name == "" {
		return Asset{}, false
	}
	return r.Find(name)
}

func gpuRendererAssetFor(r Release, goos, goarch string) (Asset, bool) {
	name := GPURendererAssetName(r.Tag, goos, goarch)
	if name == "" {
		return Asset{}, false
	}
	return r.Find(name)
}

func hasExtBuildFor(r Release, goos, goarch string) bool {
	if _, ok := extBuildFor(r, goos, goarch); !ok {
		return false
	}
	_, ok := r.Find("SHA256SUMS")
	return ok
}

func hasGPURendererFor(r Release, goos, goarch string) bool {
	if _, ok := gpuRendererAssetFor(r, goos, goarch); !ok {
		return false
	}
	_, ok := r.Find("SHA256SUMS")
	return ok
}

// HasExtBuild reports whether r publishes a verifiable -ext build of rungvx
// for this machine.
func HasExtBuild(r Release) bool {
	return hasExtBuildFor(r, runtime.GOOS, runtime.GOARCH)
}

// HasGPURenderer reports whether r publishes a verifiable gvx-gpu-renderer
// bundle for this machine. Not every release includes one.
func HasGPURenderer(r Release) bool {
	return hasGPURendererFor(r, runtime.GOOS, runtime.GOARCH)
}

func findGPURendererReleaseFor(preferred Release, recent []Release, goos, goarch string) (Release, bool) {
	if hasGPURendererFor(preferred, goos, goarch) {
		return preferred, true
	}
	var best Release
	var found bool
	for _, r := range recent {
		if !r.Prerelease && hasGPURendererFor(r, goos, goarch) {
			if !found || Newer(r.Tag, best.Tag) {
				best = r
				found = true
			}
		}
	}
	if found {
		return best, true
	}
	for _, r := range recent {
		if r.Prerelease && hasGPURendererFor(r, goos, goarch) {
			if !found || Newer(r.Tag, best.Tag) {
				best = r
				found = true
			}
		}
	}
	return best, found
}

// FindGPURendererRelease returns preferred if it includes gvx-gpu-renderer;
// otherwise it returns the newest release in recent (preferring non-prerelease
// releases) that ships gvx-gpu-renderer for this machine.
func FindGPURendererRelease(preferred Release, recent []Release) (Release, bool) {
	return findGPURendererReleaseFor(preferred, recent, runtime.GOOS, runtime.GOARCH)
}

// GPURendererDir returns the libexec bundle directory rungvx auto-discovers
// relative to rungvTarget: <dir(rungvTarget)>/../libexec/gvx-gpu-renderer.
func GPURendererDir(rungvTarget string) string {
	rungvTarget = strings.TrimSpace(rungvTarget)
	if rungvTarget == "" {
		return ""
	}
	return filepath.Clean(filepath.Join(filepath.Dir(rungvTarget), "..", "libexec", "gvx-gpu-renderer"))
}

// GPURendererBinary returns the expected gvx-gpu-renderer executable path for
// rungvTarget.
func GPURendererBinary(rungvTarget string) string {
	dir := GPURendererDir(rungvTarget)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "bin", "gvx-gpu-renderer")
}

// LocateGPURenderer searches the same locations as rungvx's GPU client:
//  1. GVX_GPU_RENDERER environment variable
//  2. <dir of rungvPath>/gvx-gpu-renderer/bin/gvx-gpu-renderer
//  3. <dir of rungvPath>/../libexec/gvx-gpu-renderer/bin/gvx-gpu-renderer
//
// Both rungvPath and its symlink target (when different) are checked.
func LocateGPURenderer(rungvPath string) string {
	if env := strings.TrimSpace(os.Getenv("GVX_GPU_RENDERER")); env != "" {
		if isRegularFile(env) {
			return env
		}
	}
	var dirs []string
	addDir := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		d := filepath.Clean(filepath.Dir(p))
		for _, seen := range dirs {
			if seen == d {
				return
			}
		}
		dirs = append(dirs, d)
	}
	addDir(rungvPath)
	if rungvPath != "" {
		if resolved, err := filepath.EvalSymlinks(rungvPath); err == nil {
			addDir(resolved)
		}
	}
	for _, d := range dirs {
		for _, cand := range []string{
			filepath.Join(d, "gvx-gpu-renderer", "bin", "gvx-gpu-renderer"),
			filepath.Clean(filepath.Join(d, "..", "libexec", "gvx-gpu-renderer", "bin", "gvx-gpu-renderer")),
		} {
			if isRegularFile(cand) {
				return cand
			}
		}
	}
	return ""
}

// GPURendererVersion reads the release tag recorded alongside the located
// gvx-gpu-renderer bundle, or "" if none is installed or no marker exists.
func GPURendererVersion(rungvPath string) string {
	bin := LocateGPURenderer(rungvPath)
	if bin == "" {
		return ""
	}
	binDir := filepath.Dir(bin)
	bundleDir := filepath.Dir(binDir)
	// Only read .version from a standard gvx-gpu-renderer/bin layout so an
	// arbitrary GVX_GPU_RENDERER override (e.g. /usr/local/bin/...) never reads
	// an unrelated file like /usr/.version.
	if filepath.Base(binDir) != "bin" || filepath.Base(bundleDir) != "gvx-gpu-renderer" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(bundleDir, ".version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		return false
	}
	return true
}

// InstallWithOptions downloads rungvx from r, verifies it against SHA256SUMS,
// and installs it at target. When opts.PreferExt is true and r publishes a
// -ext archive for this machine, it installs that build (usedExt == true);
// otherwise it falls back to the standard archive (usedExt == false).
func InstallWithOptions(ctx context.Context, r Release, target string, opts InstallOptions, progress func(done, total int64)) (usedExt bool, err error) {
	var a Asset
	var ok bool
	if opts.PreferExt {
		if a, ok = extBuildFor(r, runtime.GOOS, runtime.GOARCH); ok {
			usedExt = true
		}
	}
	if !ok {
		a, ok = build(r)
	}
	if !ok {
		return false, fmt.Errorf("%w (%s/%s)", ErrNoBuild, runtime.GOOS, runtime.GOARCH)
	}
	if err := installBinaryAsset(ctx, r, a, target, progress); err != nil {
		return false, err
	}
	return usedExt, nil
}

// InstallGPURenderer downloads r's gvx-gpu-renderer bundle for this machine,
// verifies it against r's SHA256SUMS, and atomically installs it into
// GPURendererDir(rungvTarget) so rungvx discovers it automatically.
func InstallGPURenderer(ctx context.Context, r Release, rungvTarget string, progress func(done, total int64)) (string, error) {
	a, ok := gpuRendererAssetFor(r, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("%w (%s/%s)", ErrNoGPURenderer, runtime.GOOS, runtime.GOARCH)
	}
	sums, ok := r.Find("SHA256SUMS")
	if !ok {
		return "", errors.New("the release has no SHA256SUMS to check the download against")
	}
	want, err := ghrelease.Checksum(ctx, sums.URL, a.Name)
	if err != nil {
		return "", err
	}
	destDir := GPURendererDir(rungvTarget)
	if destDir == "" {
		return "", errors.New("empty rungvx install target")
	}
	libexecDir := filepath.Dir(destDir)
	if err := os.MkdirAll(libexecDir, 0o755); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(libexecDir, ".gvx-gpu-renderer-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := ghrelease.Download(ctx, a, want, archive, maxArchive, progress); err != nil {
		return "", err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	stageDir, err := os.MkdirTemp(libexecDir, ".gvx-gpu-renderer-new-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stageDir)
	if err := extractGPURendererTar(archive, stageDir); err != nil {
		return "", err
	}
	binPath := filepath.Join(stageDir, "bin", "gvx-gpu-renderer")
	if st, err := os.Lstat(binPath); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("the archive holds no bin/gvx-gpu-renderer")
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", err
	}
	if r.Tag != "" {
		if err := os.WriteFile(filepath.Join(stageDir, ".version"), []byte(r.Tag+"\n"), 0o644); err != nil {
			return "", err
		}
	}

	if err := replaceDir(stageDir, destDir); err != nil {
		return "", err
	}
	return filepath.Join(destDir, "bin", "gvx-gpu-renderer"), nil
}

// replaceDir atomically swaps stageDir into destDir while preserving an
// existing installation if the swap fails.
func replaceDir(stageDir, destDir string) error {
	backup := destDir + ".old"
	_ = os.RemoveAll(backup)
	hadOld := false
	if _, err := os.Lstat(destDir); err == nil {
		if err := os.Rename(destDir, backup); err != nil {
			return err
		}
		hadOld = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stageDir, destDir); err != nil {
		if hadOld {
			_ = os.Rename(backup, destDir)
		}
		return err
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func rejectSymlinkParents(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." || dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if st, err := root.Lstat(cur); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive entry traverses symlink parent: %q", cur)
			}
			if !st.IsDir() {
				return fmt.Errorf("archive entry parent is not a directory: %q", cur)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func extractGPURendererTar(archive io.Reader, destDir string) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()

	root, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer root.Close()

	tr := tar.NewReader(gz)
	var totalBytes int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return gz.Close()
		}
		if err != nil {
			return err
		}
		rel, ok, err := cleanGPURendererEntry(h.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := rejectSymlinkParents(root, rel); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			mode := os.FileMode(h.Mode) & 0o777
			if mode == 0 {
				mode = 0o755
			}
			if st, err := root.Lstat(rel); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive directory collides with symlink: %q", h.Name)
			}
			if err := root.MkdirAll(rel, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if parent := path.Dir(rel); parent != "." && parent != "" {
				if err := root.MkdirAll(parent, 0o755); err != nil {
					return err
				}
			}
			if st, err := root.Lstat(rel); err == nil && st.Mode()&os.ModeSymlink != 0 {
				if err := root.Remove(rel); err != nil {
					return err
				}
			}
			mode := os.FileMode(h.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			if strings.HasPrefix(rel, "bin/") {
				mode |= 0o755
			}
			remaining := maxArchive - totalBytes
			if remaining < 0 {
				return errors.New("gpu renderer archive exceeds size limit")
			}
			f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, remaining+1))
			closeErr := f.Close()
			totalBytes += n
			if totalBytes > maxArchive {
				return errors.New("gpu renderer archive exceeds size limit")
			}
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) || strings.HasPrefix(filepath.ToSlash(h.Linkname), "/") {
				return fmt.Errorf("archive symlink uses absolute target: %q", h.Linkname)
			}
			resolved := path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(h.Linkname)))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("archive symlink escapes destination: %q -> %q", h.Name, h.Linkname)
			}
			if parent := path.Dir(rel); parent != "." && parent != "" {
				if err := root.MkdirAll(parent, 0o755); err != nil {
					return err
				}
			}
			_ = root.Remove(rel)
			if err := root.Symlink(h.Linkname, rel); err != nil {
				return err
			}
		}
	}
}

func cleanGPURendererEntry(name string) (rel string, keep bool, err error) {
	s := strings.TrimPrefix(filepath.ToSlash(name), "./")
	if strings.HasPrefix(s, "/") {
		return "", false, fmt.Errorf("archive entry has absolute path: %q", name)
	}
	clean := path.Clean(s)
	if clean == "." || clean == "" {
		return "", false, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("archive entry escapes destination: %q", name)
	}
	if clean == "gvx-gpu-renderer" {
		return "", false, nil
	}
	if rest, ok := strings.CutPrefix(clean, "gvx-gpu-renderer/"); ok {
		if rest == "" || rest == "." {
			return "", false, nil
		}
		return rest, true, nil
	}
	return clean, true, nil
}
