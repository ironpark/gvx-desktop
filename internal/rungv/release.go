package rungv

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// ReleasesURL is where a person reads about the releases of gvx.
const ReleasesURL = "https://github.com/gfire-sigs/gvx/releases"

// latestURL answers with the newest release of gvx; a test points it at a
// server of its own.
var latestURL = "https://api.github.com/repos/gfire-sigs/gvx/releases/latest"

// releasesURL lists recent releases of gvx; a test points it at a server of
// its own.
var releasesURL = "https://api.github.com/repos/gfire-sigs/gvx/releases?per_page=30"

// maxArchive bounds what Install reads, several times a release's size.
const maxArchive = 512 << 20

// Asset is one file of a release.
type Asset = ghrelease.Asset

// Release is a published version of gvx.
type Release = ghrelease.Release

// ErrNoBuild reports a release with no rungv for this machine.
var ErrNoBuild = errors.New("the release has no rungvx for this system")

func get(ctx context.Context, url string) (*http.Response, error) { return ghrelease.Get(ctx, url) }

// Latest asks GitHub for the newest release of gvx.
func Latest(ctx context.Context) (Release, error) { return ghrelease.Latest(ctx, latestURL) }

// Releases asks GitHub for recent published releases of gvx that include a
// verifiable build for this system, newest first.
func Releases(ctx context.Context) ([]Release, error) {
	all, err := ghrelease.List(ctx, releasesURL)
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(all))
	for _, r := range all {
		if HasBuild(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// HasBuild reports whether r has both a rungv archive for this system and a
// SHA256SUMS file to verify it against.
func HasBuild(r Release) bool {
	if _, ok := build(r); !ok {
		return false
	}
	_, ok := r.Find("SHA256SUMS")
	return ok
}

// AssetNames are the archives of tag that may hold rungv for goos and
// goarch: rungvx-<tag>-<goos>-<goarch> since gvx v0.23, rungv-… before.
func AssetNames(tag, goos, goarch string) []string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	rest := "-" + tag + "-" + goos + "-" + goarch + ext
	return []string{programX + rest, program + rest}
}

// build is the archive of r for this machine.
func build(r Release) (Asset, bool) {
	for _, name := range AssetNames(r.Tag, runtime.GOOS, runtime.GOARCH) {
		if a, ok := r.Find(name); ok {
			return a, true
		}
	}
	return Asset{}, false
}

// Newer reports whether version a is later than b. Versions are compared
// number by number, v0.17.2 after v0.9.10; one that is not numbers, such as
// (devel), is earlier than any that is.
func Newer(a, b string) bool { return ghrelease.Newer(a, b) }

// InstallDir is where Install puts rungv when it has no better place:
// ~/.gvx/bin, one of the places Locate looks.
func InstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gvx", "bin"), nil
}

// InstallTarget is the file an update of the rungv at current should
// replace: current itself when it is a plain file in a directory this user
// may write to, and otherwise, as for a link a package manager keeps or
// when there is no rungv yet, the one in InstallDir. replaces reports the
// first.
func InstallTarget(current string) (target string, replaces bool, err error) {
	if current != "" {
		if st, err := os.Lstat(current); err == nil && st.Mode().IsRegular() && ghrelease.Writable(filepath.Dir(current)) {
			return current, true, nil
		}
	}
	dir, err := InstallDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, BinaryName()), false, nil
}

// Install downloads the rungv of r for this machine, checks it against the
// release's SHA256SUMS, and puts it at target, replacing what is there. A
// rungv that is running keeps running the file it started from. progress,
// when not nil, hears how much of the archive has arrived. On macOS the
// caller still signs the binary, with Sign.
func Install(ctx context.Context, r Release, target string, progress func(done, total int64)) error {
	_, err := InstallWithOptions(ctx, r, target, InstallOptions{}, progress)
	return err
}

func installBinaryAsset(ctx context.Context, r Release, a Asset, target string, progress func(done, total int64)) error {
	sums, ok := r.Find("SHA256SUMS")
	if !ok {
		return errors.New("the release has no SHA256SUMS to check the download against")
	}
	want, err := ghrelease.Checksum(ctx, sums.URL, a.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	archive, err := os.CreateTemp(filepath.Dir(target), ".rungv-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := ghrelease.Download(ctx, a, want, archive, maxArchive, progress); err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}

	fresh, err := os.CreateTemp(filepath.Dir(target), ".rungv-new-*")
	if err != nil {
		return err
	}
	defer os.Remove(fresh.Name())
	if strings.HasSuffix(a.Name, ".zip") {
		err = unzipBinary(archive, fresh)
	} else {
		err = untarBinary(archive, fresh)
	}
	if cerr := fresh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(fresh.Name(), 0o755); err != nil {
		return err
	}
	if err := replaceFile(fresh.Name(), target); err != nil {
		return err
	}
	ForgetFeatures(target)
	if abs, err := filepath.Abs(target); err == nil && abs != target {
		ForgetFeatures(abs)
	}
	return nil
}

// replaceFile moves fresh onto target. Windows does not let a running
// program's file be written over, but lets it be renamed: the old one steps
// aside first.
func replaceFile(fresh, target string) error {
	if runtime.GOOS == "windows" {
		old := target + ".old"
		os.Remove(old)
		if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(fresh, target)
}

// isBinary reports whether an archive's entry is rungv itself, which lies
// one directory down: rungvx, or rungv in a release before gvx v0.23.
func isBinary(name string) bool {
	return slices.Contains(binaryNames(), path.Base(filepath.ToSlash(name)))
}

func untarBinary(archive io.Reader, out io.Writer) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive holds no rungvx")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && isBinary(h.Name) {
			n, err := io.Copy(out, io.LimitReader(tr, maxArchive+1))
			if err != nil {
				return err
			}
			if n > maxArchive {
				return errors.New("rungvx binary exceeds size limit")
			}
			return gz.Close()
		}
	}
}

func unzipBinary(archive *os.File, out io.Writer) error {
	st, err := archive.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(archive, st.Size())
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isBinary(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		n, err := io.Copy(out, io.LimitReader(rc, maxArchive+1))
		if err != nil {
			return err
		}
		if n > maxArchive {
			return errors.New("rungvx binary exceeds size limit")
		}
		return nil
	}
	return errors.New("the archive holds no rungvx")
}
