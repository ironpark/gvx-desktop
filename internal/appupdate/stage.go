package appupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// BundleID is the macOS app's identifier, which a new bundle must have.
const BundleID = "io.github.ironpark.gvx-desktop"

// maxDownload bounds what an update downloads, and what it unpacks of
// that: several times a release's size.
const maxDownload = 512 << 20

// stagePrefix starts the name of the folder a new macOS bundle waits in,
// beside the one it replaces.
const stagePrefix = ".gvx-desktop-update-"

// downloadPrefix starts the name of a program's archive while it
// downloads, beside the program.
const downloadPrefix = ".gvx-desktop-download-"

// ErrNoBuild reports a release with no build of the app for this machine.
var ErrNoBuild = errors.New("the release has no build of GVX Desktop for this system")

// ErrNotSelf reports an install the app does not update itself.
var ErrNotSelf = errors.New("this copy of GVX Desktop does not update itself")

// stagedFile is one optional companion binary shipped alongside a Program
// install (the Zig driver shared library and the bundled Docker CLI + plugins)
// that is staged, swapped, rolled back and cleaned up with the main program.
type stagedFile struct {
	target string
	fresh  string
	old    string
	hadOld bool
}

// Staged is an update downloaded, checked against its checksum, and put
// beside the app it replaces, for Prepare and Finish to put in place.
type Staged struct {
	Tag    string // the release's
	Method Method // Bundle or Program
	Target string // the bundle or program the update replaces

	fresh      string       // the new bundle or program, beside Target
	old        string       // a program: where the one it replaces steps aside to
	companions []stagedFile // a program: optional bundled driver library & Docker CLI binaries
	stage      string       // a bundle: the folder fresh is in
	script     string       // a bundle: the helper Prepare wrote
	goos       string
	args       []string
	swapped    bool // a program: fresh is at Target, and the old one at old
}

// Stage downloads the build of rel that installs the app as in has it,
// checks it, and puts it beside the app. Nothing of the app changes yet.
// progress, when not nil, hears how much of the download has arrived.
func Stage(ctx context.Context, rel Release, in Install, progress func(done, total int64)) (*Staged, error) {
	switch in.Method {
	case Bundle:
		return stageBundle(ctx, rel, in.Path, progress)
	case Program:
		return stageProgram(ctx, rel, in.Path, runtime.GOOS, runtime.GOARCH, progress)
	}
	return nil, ErrNotSelf
}

// fetch downloads the file name of rel to w, checked against the checksum
// the release gives it in name.sha256. A file the release has no checksum
// for is not installed.
func fetch(ctx context.Context, rel Release, name string, w io.Writer, progress func(done, total int64)) error {
	a, ok := rel.Find(name)
	if !ok {
		return fmt.Errorf("%w: %s is not in release %s", ErrNoBuild, name, rel.Tag)
	}
	sums, ok := rel.Find(name + ".sha256")
	if !ok {
		return fmt.Errorf("the release has no checksum for %s, so it is not installed", name)
	}
	want, err := ghrelease.Checksum(ctx, sums.URL, name)
	if err != nil {
		return err
	}
	return ghrelease.Download(ctx, a, want, w, maxDownload, progress)
}

// programPaths are where a new program (or companion library) waits beside
// target, and where target steps aside to when the new one takes its place.
func programPaths(target, goos string) (fresh, old string) {
	if goos == "windows" {
		if ext := filepath.Ext(target); ext != "" {
			base := strings.TrimSuffix(target, ext)
			return base + ".new" + ext, base + ".old" + ext
		}
	}
	return target + ".new", target + ".old"
}

// companionSpec describes one optional companion file inside a Program release
// archive and where it installs relative to the main executable.
type companionSpec struct {
	archivePath string      // normalized slash path inside zip/tar
	target      string      // on-disk destination path
	mode        fs.FileMode // file mode when staged
}

// companionSpecs returns the optional companion binaries (the Zig graphics
// driver shared library and the bundled Docker CLI + plugins) for a Program
// install at target on goos.
func companionSpecs(target, goos string) []companionSpec {
	dir := filepath.Dir(target)
	switch goos {
	case "windows":
		return []companionSpec{
			{archivePath: "ggfx_driver.dll", target: filepath.Join(dir, "ggfx_driver.dll"), mode: 0o644},
			{archivePath: "bin/docker.exe", target: filepath.Join(dir, "bin", "docker.exe"), mode: 0o755},
			{archivePath: "cli-plugins/docker-buildx.exe", target: filepath.Join(dir, "cli-plugins", "docker-buildx.exe"), mode: 0o755},
			{archivePath: "cli-plugins/docker-compose.exe", target: filepath.Join(dir, "cli-plugins", "docker-compose.exe"), mode: 0o755},
		}
	case "linux":
		libDir := filepath.Join(dir, "..", "lib", Package)
		return []companionSpec{
			{archivePath: "usr/lib/" + Package + "/libggfx_driver.so", target: filepath.Join(libDir, "libggfx_driver.so"), mode: 0o644},
			{archivePath: "usr/lib/" + Package + "/bin/docker", target: filepath.Join(libDir, "bin", "docker"), mode: 0o755},
			{archivePath: "usr/lib/" + Package + "/cli-plugins/docker-buildx", target: filepath.Join(libDir, "cli-plugins", "docker-buildx"), mode: 0o755},
			{archivePath: "usr/lib/" + Package + "/cli-plugins/docker-compose", target: filepath.Join(libDir, "cli-plugins", "docker-compose"), mode: 0o755},
		}
	default:
		return nil
	}
}

// stageProgram downloads the archive of rel for goos and goarch beside
// target, and unpacks its program (and any bundled driver library / Docker CLI
// binaries present) there, at programPaths' fresh.
func stageProgram(ctx context.Context, rel Release, target, goos, goarch string, progress func(done, total int64)) (*Staged, error) {
	name, ok := AssetName(rel.Tag, goos, goarch, Program)
	if !ok {
		return nil, fmt.Errorf("%w (%s/%s)", ErrNoBuild, goos, goarch)
	}
	archive, err := os.CreateTemp(filepath.Dir(target), downloadPrefix+"*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := fetch(ctx, rel, name, archive, progress); err != nil {
		return nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	fresh, old := programPaths(target, goos)
	specs := companionSpecs(target, goos)
	var companions []stagedFile
	if goos == "windows" {
		companions, err = unzipProgram(archive, fresh, specs)
	} else {
		companions, err = untarProgram(archive, fresh, specs)
	}
	if err == nil {
		err = checkProgram(fresh, goos)
	}
	if err == nil {
		err = os.Chmod(fresh, 0o755)
	}
	for _, c := range companions {
		if err == nil {
			err = checkProgram(c.fresh, goos)
		}
	}
	if err != nil {
		os.Remove(fresh)
		for _, c := range companions {
			os.Remove(c.fresh)
		}
		return nil, err
	}
	return &Staged{Tag: rel.Tag, Method: Program, Target: target, fresh: fresh, old: old, companions: companions, goos: goos}, nil
}

func writeStagedFile(dst string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	err = copyCapped(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// unzipProgram copies the program (and any bundled companions) out of a
// Windows release's zip.
func unzipProgram(archive *os.File, exeDst string, specs []companionSpec) ([]stagedFile, error) {
	st, err := archive.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(archive, st.Size())
	if err != nil {
		return nil, err
	}
	cleanup := func(staged []stagedFile) {
		for _, c := range staged {
			os.Remove(c.fresh)
		}
	}
	var (
		gotExe bool
		staged []stagedFile
		seen   = make(map[string]bool, len(specs))
	)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(f.Name, `\`, "/"), "./"))
		matched := false
		for _, sp := range specs {
			if !seen[sp.archivePath] && strings.EqualFold(clean, sp.archivePath) {
				matched = true
				cFresh, cOld := programPaths(sp.target, "windows")
				rc, err := f.Open()
				if err != nil {
					cleanup(staged)
					return nil, err
				}
				err = writeStagedFile(cFresh, rc, sp.mode)
				rc.Close()
				if err != nil {
					cleanup(staged)
					return nil, err
				}
				seen[sp.archivePath] = true
				staged = append(staged, stagedFile{target: sp.target, fresh: cFresh, old: cOld})
				break
			}
		}
		if matched {
			continue
		}
		if !gotExe && !strings.Contains(clean, "/") && strings.EqualFold(path.Ext(clean), ".exe") {
			rc, err := f.Open()
			if err != nil {
				cleanup(staged)
				return nil, err
			}
			err = writeStagedFile(exeDst, rc, 0o755)
			rc.Close()
			if err != nil {
				cleanup(staged)
				return nil, err
			}
			gotExe = true
		}
	}
	if !gotExe {
		cleanup(staged)
		return nil, errors.New("the archive holds no program")
	}
	return staged, nil
}

// untarProgram copies the program (and any bundled companions) out of a Linux
// release's tarball, which lays out /usr: usr/bin/gvx-desktop and
// usr/lib/gvx-desktop/...
func untarProgram(archive io.Reader, exeDst string, specs []companionSpec) ([]stagedFile, error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	wantExe := "usr/bin/" + Package
	byPath := make(map[string]companionSpec, len(specs))
	for _, sp := range specs {
		byPath[sp.archivePath] = sp
	}
	cleanup := func(staged []stagedFile) {
		for _, c := range staged {
			os.Remove(c.fresh)
		}
	}
	var (
		gotExe bool
		staged []stagedFile
		seen   = make(map[string]bool, len(specs))
	)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup(staged)
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if clean == wantExe {
			if err := writeStagedFile(exeDst, tr, 0o755); err != nil {
				cleanup(staged)
				return nil, err
			}
			gotExe = true
			continue
		}
		if sp, ok := byPath[clean]; ok && !seen[clean] {
			cFresh, cOld := programPaths(sp.target, "linux")
			if err := writeStagedFile(cFresh, tr, sp.mode); err != nil {
				cleanup(staged)
				return nil, err
			}
			seen[clean] = true
			staged = append(staged, stagedFile{target: sp.target, fresh: cFresh, old: cOld})
		}
	}
	if !gotExe {
		cleanup(staged)
		return nil, errors.New("the archive holds no program")
	}
	return staged, nil
}

func copyCapped(out io.Writer, in io.Reader) error {
	n, err := io.Copy(out, io.LimitReader(in, maxDownload+1))
	if err == nil && n > maxDownload {
		err = ghrelease.ErrTooLarge
	}
	return err
}

// checkProgram makes sure what was unpacked is a program for goos at all:
// a Windows .exe starts MZ, a Linux one is ELF.
func checkProgram(p, goos string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	magic := []byte("\x7fELF")
	if goos == "windows" {
		magic = []byte("MZ")
	}
	if !bytes.HasPrefix(head, magic) {
		return fmt.Errorf("the release's program is not one for %s", goos)
	}
	return nil
}

// swap puts fresh in target's place, moving target to old first: a
// running program's file may be renamed, though Windows lets it be neither
// written over nor removed. When fresh does not go in, target comes back.
func swap(target, fresh, old string) error {
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("an earlier update left %s, which cannot be removed: %w", old, err)
	}
	if err := os.Rename(target, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, target); err != nil {
		if rerr := os.Rename(old, target); rerr != nil {
			return fmt.Errorf("%w; putting the old program back failed too, and it is at %s: %v", err, old, rerr)
		}
		return err
	}
	return nil
}

// unswap undoes swap: the old program goes back in target's place, and
// the new one back to fresh.
func unswap(target, fresh, old string) error {
	if err := os.Rename(target, fresh); err != nil {
		return err
	}
	if err := os.Rename(old, target); err != nil {
		os.Rename(fresh, target) // the new one at least, rather than none
		return err
	}
	return nil
}

// swapOptional puts fresh in target's place like swap, allowing target to not
// exist yet (for example when upgrading from a build without a companion
// driver library to one with one).
func swapOptional(target, fresh, old string) (hadOld bool, err error) {
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("an earlier update left %s, which cannot be removed: %w", old, err)
	}
	if err := os.Rename(target, old); err == nil {
		hadOld = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.Rename(fresh, target); err != nil {
		if hadOld {
			if rerr := os.Rename(old, target); rerr != nil {
				return hadOld, fmt.Errorf("%w; putting the old file back failed too, and it is at %s: %v", err, old, rerr)
			}
		}
		return false, err
	}
	return hadOld, nil
}

// unswapOptional undoes swapOptional.
func unswapOptional(target, fresh, old string, hadOld bool) error {
	if err := os.Rename(target, fresh); err != nil {
		return err
	}
	if !hadOld {
		return nil
	}
	if err := os.Rename(old, target); err != nil {
		os.Rename(fresh, target)
		return err
	}
	return nil
}

// Prepare readies the update to go in when the app quits, which starts it
// with args. A program goes in place now, the one running stepping aside,
// and back should the new one not go in. A bundle stays where it waits:
// the helper Finish starts moves it once the app has quit.
func (s *Staged) Prepare(args []string) error {
	s.args = args
	switch s.Method {
	case Program:
		if s.swapped {
			return nil
		}
		swappedCompanions := 0
		rollbackCompanions := func() {
			for i := swappedCompanions - 1; i >= 0; i-- {
				c := &s.companions[i]
				_ = unswapOptional(c.target, c.fresh, c.old, c.hadOld)
				c.hadOld = false
			}
		}
		for i := range s.companions {
			c := &s.companions[i]
			hadOld, err := swapOptional(c.target, c.fresh, c.old)
			if err != nil {
				rollbackCompanions()
				return err
			}
			c.hadOld = hadOld
			swappedCompanions++
		}
		if err := swap(s.Target, s.fresh, s.old); err != nil {
			rollbackCompanions()
			return err
		}
		s.swapped = true
		return nil
	case Bundle:
		s.script = filepath.Join(s.stage, "finish.sh")
		return os.WriteFile(s.script, []byte(helperScript(os.Getpid(), s.Target, s.fresh, s.stage, "/usr/bin/open", args)), 0o700)
	}
	return ErrNotSelf
}

// Finish starts the updated app. Call it last, once the app has ended all
// it runs. A program is started from its place; should it not start, the
// old one goes back and starts instead. For a bundle, the helper starts,
// and waits for this process to end before it moves anything.
func (s *Staged) Finish() error {
	switch s.Method {
	case Program:
		if !s.swapped {
			return errors.New("the update was not put in place")
		}
		if err := start(s.Target, s.args); err != nil {
			uerr := unswap(s.Target, s.fresh, s.old)
			for i := len(s.companions) - 1; i >= 0; i-- {
				c := s.companions[i]
				if luerr := unswapOptional(c.target, c.fresh, c.old, c.hadOld); luerr != nil && uerr == nil {
					uerr = luerr
				}
			}
			if uerr != nil {
				return fmt.Errorf("the new program did not start: %w; putting the old one back failed: %v", err, uerr)
			}
			if serr := start(s.Target, s.args); serr != nil {
				return fmt.Errorf("the new program did not start: %w; the old one, put back, did not either: %v", err, serr)
			}
			return fmt.Errorf("the new program did not start, and the old one was put back: %w", err)
		}
		if s.goos != "windows" {
			os.Remove(s.old) // a Unix program's file may go while it runs
			for _, c := range s.companions {
				if c.hadOld {
					os.Remove(c.old)
				}
			}
		}
		return nil
	case Bundle:
		if s.script == "" {
			return errors.New("the update was not prepared")
		}
		cmd := exec.Command("/bin/sh", s.script)
		if log, err := os.Create(filepath.Join(os.TempDir(), "gvx-desktop-update.log")); err == nil {
			cmd.Stdout, cmd.Stderr = log, log
			defer log.Close()
		}
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	return ErrNotSelf
}

// Discard removes an update that is not to go in.
func (s *Staged) Discard() error {
	switch s.Method {
	case Program:
		if s.swapped {
			return nil // it is in place; the old one goes at the next start
		}
		var errs []error
		if err := os.Remove(s.fresh); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, c := range s.companions {
			if err := os.Remove(c.fresh); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	case Bundle:
		return os.RemoveAll(s.stage)
	}
	return nil
}

// start runs program with args, apart from this process, which is about
// to end.
func start(program string, args []string) error {
	cmd := exec.Command(program, args...)
	cmd.Dir = filepath.Dir(program)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Cleanup removes what an update left beside the app as in has it: the
// program it replaced, which Windows does not let go of while it runs, a
// new program that never went in, a download cut short, or a bundle's
// folder. Call it when the app starts, before any update is staged.
func Cleanup(in Install) error { return cleanup(in, runtime.GOOS) }

func cleanup(in Install, goos string) error {
	var errs []error
	remove := func(p string, all bool) {
		rm := os.Remove
		if all {
			rm = os.RemoveAll
		}
		if err := rm(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	switch in.Method {
	case Program:
		fresh, old := programPaths(in.Path, goos)
		remove(fresh, false)
		remove(old, false)
		for _, sp := range companionSpecs(in.Path, goos) {
			cFresh, cOld := programPaths(sp.target, goos)
			remove(cFresh, false)
			remove(cOld, false)
		}
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(in.Path), downloadPrefix+"*"))
		for _, p := range left {
			remove(p, false)
		}
	case Bundle:
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(in.Path), stagePrefix+"*"))
		for _, p := range left {
			remove(p, true)
		}
	}
	return errors.Join(errs...)
}

// RemoveReplaced removes the program (and companion binaries) an update
// replaced, which Windows lets go of once the old app has ended: it reports
// whether they are gone.
func RemoveReplaced(in Install) bool {
	if in.Method != Program {
		return true
	}
	_, old := programPaths(in.Path, runtime.GOOS)
	err := os.Remove(old)
	ok := err == nil || errors.Is(err, fs.ErrNotExist)
	for _, sp := range companionSpecs(in.Path, runtime.GOOS) {
		_, cOld := programPaths(sp.target, runtime.GOOS)
		cerr := os.Remove(cOld)
		ok = ok && (cerr == nil || errors.Is(cerr, fs.ErrNotExist))
	}
	return ok
}

// stageBundle downloads the disk image of rel, copies the app out of it
// into a folder beside bundle, on the same volume, and checks its
// signature and identifier.
func stageBundle(ctx context.Context, rel Release, bundle string, progress func(done, total int64)) (*Staged, error) {
	name, ok := AssetName(rel.Tag, "darwin", runtime.GOARCH, Bundle)
	if !ok {
		return nil, fmt.Errorf("%w (darwin/%s)", ErrNoBuild, runtime.GOARCH)
	}
	tmp, err := os.MkdirTemp("", "gvx-desktop-update-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dmg := filepath.Join(tmp, name)
	f, err := os.Create(dmg)
	if err != nil {
		return nil, err
	}
	err = fetch(ctx, rel, name, f, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(bundle), stagePrefix)
	if err != nil {
		return nil, err
	}
	fresh := filepath.Join(stage, filepath.Base(bundle))
	if err := copyFromImage(ctx, dmg, filepath.Join(tmp, "mnt"), fresh); err != nil {
		os.RemoveAll(stage)
		return nil, err
	}
	if err := verifyBundle(ctx, fresh); err != nil {
		os.RemoveAll(stage)
		return nil, err
	}
	return &Staged{Tag: rel.Tag, Method: Bundle, Target: bundle, fresh: fresh, stage: stage, goos: "darwin"}, nil
}

// copyFromImage mounts the disk image dmg at mnt, read-only and out of the
// Finder's sight, copies the app in it to dst, and unmounts it.
func copyFromImage(ctx context.Context, dmg, mnt, dst string) error {
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if _, err := run(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg); err != nil {
		return err
	}
	defer func() {
		if _, err := run(context.Background(), "/usr/bin/hdiutil", "detach", mnt); err != nil {
			run(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mnt)
		}
	}()
	app, err := findApp(mnt)
	if err != nil {
		return err
	}
	_, err = run(ctx, "/usr/bin/ditto", app, dst)
	return err
}

// findApp is the one app bundle at the top of a mounted disk image, which
// also holds a link to /Applications.
func findApp(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var apps []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			apps = append(apps, filepath.Join(dir, e.Name()))
		}
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("the disk image holds %d apps, not one", len(apps))
	}
	return apps[0], nil
}

// verifyBundle checks that app is sealed by its signature, an ad hoc one
// or a Developer ID's, and is this app.
func verifyBundle(ctx context.Context, app string) error {
	if _, err := run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", app); err != nil {
		return fmt.Errorf("the new app's signature does not hold: %w", err)
	}
	id, err := run(ctx, "/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return err
	}
	if id = strings.TrimSpace(id); id != BundleID {
		return fmt.Errorf("the new app is %s, not %s", id, BundleID)
	}
	return nil
}

// run runs a command and returns what it printed, which an error carries.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", filepath.Base(name), err, msg)
		}
		return "", fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return out.String(), nil
}

// helperWait is how long the helper waits for the app to end, in half
// seconds: five minutes, more than stopping its sandboxes takes.
const helperWait = 600

// helperScript is the shell script that finishes a bundle's update: it
// waits for the process pid, the app, to end; moves the bundle app into
// the stage and the new bundle fresh into its place, the old one going
// back if the new one does not; removes the stage, the old bundle with it;
// and opens the app with open, the new one or the old, with args.
func helperScript(pid int, app, fresh, stage, open string, args []string) string {
	launch := shellQuote(open) + ` "$app"`
	if len(args) > 0 {
		launch += " --args"
		for _, a := range args {
			launch += " " + shellQuote(a)
		}
	}
	return `#!/bin/sh
# GVX Desktop wrote this to finish its update once it has quit: the new app
# goes in place of the old, which stays until the new one is in, and opens.
pid=` + strconv.Itoa(pid) + `
app=` + shellQuote(app) + `
new=` + shellQuote(fresh) + `
stage=` + shellQuote(stage) + `
old="$stage/previous.app"
waited=0
while kill -0 "$pid" 2>/dev/null; do
	if [ "$waited" -ge ` + strconv.Itoa(helperWait) + ` ]; then
		echo "the app did not quit; the update is left in $stage" >&2
		exit 1
	fi
	sleep 0.5
	waited=$((waited + 1))
done
if mv "$app" "$old"; then
	if ! mv "$new" "$app"; then
		echo "the new app did not go in place; the old one goes back" >&2
		mv "$old" "$app"
	fi
else
	echo "the old app could not be moved aside; it stays" >&2
fi
if [ -d "$app" ]; then
	rm -rf "$stage"
fi
` + launch + `
`
}
