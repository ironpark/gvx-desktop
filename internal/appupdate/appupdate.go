// Package appupdate updates GVX Desktop itself from its GitHub releases:
// it reads the newest, tells whether it is newer than the app running,
// tells how the app was installed, and for a copy that is the app's own
// to replace — a macOS bundle, a Windows program, a Linux program from
// the tarball — downloads the release's build, checks it against the
// checksum the release gives it, and puts it in place when the app quits.
// A copy a package manager keeps is that manager's to update: Homebrew,
// Chocolatey, dpkg or rpm.
package appupdate

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui/updater"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// ReleasesURL is where a person reads about the app's releases.
const ReleasesURL = "https://github.com/ironpark/gvx-desktop/releases"

// AppcastURL is the Sparkle 2 appcast feed published with each GitHub release.
const AppcastURL = "https://github.com/ironpark/gvx-desktop/releases/latest/download/appcast.xml"

// PublicKey is the standard base64-encoded 32-byte Ed25519 public key
// (Sparkle SUPublicEDKey) used to verify release enclosures in appcast.xml.
//
// Generate a key pair with `ggui update keygen`, store the private key in the
// repository's GGUI_UPDATE_PRIVATE_KEY secret, and paste the printed public key
// here. Keeping the key in source preserves bit-for-bit reproducible builds
// without CI secrets. While PublicKey is empty (or before the first release
// publishes appcast.xml), the app falls back to GitHub Releases API + .sha256.
var PublicKey = ""

// latestURL answers with the app's newest release; a test points it at a
// server of its own.
var latestURL = "https://api.github.com/repos/ironpark/gvx-desktop/releases/latest"

// appcastURL points to the Sparkle 2 appcast feed; a test points it at a
// server of its own.
var appcastURL = AppcastURL

// Package is the app's name to Homebrew, Chocolatey, dpkg and rpm, and the
// start of its release files' names.
const Package = "gvx-desktop"

// homebrewCask is the app's cask by its full name, with its tap: the name
// it is installed by.
const homebrewCask = "ironpark/tap/" + Package

// homebrewCommand updates the cask, one command a line. brew update comes
// first: on its own, Homebrew refreshes a third-party tap only once a day
// (HOMEBREW_AUTO_UPDATE_SECS), so brew upgrade alone may not know the
// release yet.
const homebrewCommand = "brew update\nbrew upgrade --cask " + homebrewCask

// Release is a published version of the app.
type Release = ghrelease.Release

// Latest asks GitHub for the app's newest release. GitHub leaves releases
// marked as prereleases out.
func Latest(ctx context.Context) (Release, error) { return ghrelease.Latest(ctx, latestURL) }

// version is X.Y.Z, and what follows a hyphen: a prerelease.
type version struct {
	n   [3]uint64
	pre string
}

// parseVersion reads X.Y.Z, with or without a v, a prerelease after a
// hyphen and build metadata after a plus, which counts for nothing.
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hyphen := strings.Cut(s, "-")
	if hyphen && pre == "" {
		return version{}, false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return version{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return version{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}

// Comparable reports whether the app at version current can tell a newer
// release: one a release build stamped, X.Y.Z, and not a development
// build's "dev".
func Comparable(current string) bool {
	_, ok := parseVersion(current)
	return ok
}

// Newer reports whether the release tagged tag is one to offer the app at
// version current: a release, not a prerelease, later than current. The
// app does not update to a prerelease, nor offer anything to a build whose
// version is not X.Y.Z.
func Newer(tag, current string) bool {
	t, ok := parseVersion(tag)
	if !ok || t.pre != "" {
		return false
	}
	c, ok := parseVersion(current)
	if !ok {
		return false
	}
	for i := range t.n {
		if t.n[i] != c.n[i] {
			return t.n[i] > c.n[i]
		}
	}
	// 1.0.0 is past 1.0.0-rc1.
	return c.pre != ""
}

// Plain is a version as people say it, without its v.
func Plain(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Method is how the app was installed, which says how it is updated.
type Method int

const (
	// Manual is a copy that cannot update itself, or one this package
	// cannot tell the origin of: the release page has the new version.
	Manual Method = iota
	// Bundle is a macOS app bundle in a folder this user may write to: the
	// new bundle goes in its place after the app quits.
	Bundle
	// Program is the program file of a Windows install, or of a Linux one
	// from the tarball, in a folder this user may write to: the new
	// program goes in its place, and the app starts again.
	Program
	// Homebrew is the cask's: brew update, then
	// brew upgrade --cask ironpark/tap/gvx-desktop.
	Homebrew
	// Chocolatey is the package's: choco upgrade gvx-desktop.
	Chocolatey
	// Deb and RPM are the Linux packages', which the system's package
	// manager installs.
	Deb
	RPM
)

// SelfUpdates reports whether the app replaces an install of m itself.
func (m Method) SelfUpdates() bool { return m == Bundle || m == Program }

// Install is how the running app was installed.
type Install struct {
	Method Method
	// Path is what an update replaces: the bundle, or the program. For the
	// other methods it is the program running.
	Path string
	// Tool is what installs an RPM: dnf, zypper or rpm.
	Tool string
	// PreferWindowsSetup is true on Windows for installs created by the Setup
	// installer (install-manifest.json beside the executable) and for existing
	// install.ps1 installs in %LOCALAPPDATA%\Programs\GVX Desktop so their next
	// in-app update transitions smoothly to GVX Desktop-Setup.exe. Portable zip
	// installs in other folders keep false and update from the zip archive.
	PreferWindowsSetup bool
}

// probe is what detect looks at, which a test makes up.
type probe struct {
	goos, goarch string
	exe          string // the program running, links resolved
	getenv       func(string) string
	exists       func(string) bool
	writable     func(dir string) bool
}

// Detect tells how the running app was installed. It reads no more than a
// few file names: it runs no package manager.
func Detect() Install {
	exe, err := os.Executable()
	if err != nil {
		return Install{}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return detect(probe{
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
		exe:    exe,
		getenv: os.Getenv,
		exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		writable: ghrelease.Writable,
	})
}

// detect tells the install of p.exe from where it is:
//
// On macOS the program is in a bundle, X.app/Contents/MacOS. Homebrew's
// cask keeps the bundle in /Applications or ~/Applications, not under the
// Caskroom, but records it there: a Caskroom/gvx-desktop under a Homebrew
// prefix, with the bundle in one of those folders or under the Caskroom
// itself, is a cask's. Otherwise a bundle whose folder this user may write
// to is the app's to replace; one macOS runs from a quarantine's
// translocation, or from a disk image, is not.
//
// On Windows a program under a chocolatey\lib folder, or under
// %ChocolateyInstall%\lib, is the package's; any other .exe in a folder
// this user may write to, as the installer's %LOCALAPPDATA%\Programs is,
// is the app's.
//
// On Linux a program under /usr, but not /usr/local, or under /opt, is a
// package's: a Debian one when dpkg lists it, else an RPM one where rpm is
// installed. Any other program in a folder this user may write to, as the
// tarball unpacked into ~/.local is, is the app's.
//
// Anything else is Manual.
func detect(p probe) Install {
	manual := Install{Method: Manual, Path: p.exe}
	switch p.goos {
	case "darwin":
		bundle, ok := bundleOf(p.exe)
		if !ok {
			return manual
		}
		if p.cask(bundle) {
			return Install{Method: Homebrew, Path: bundle}
		}
		if strings.Contains(bundle, "/AppTranslocation/") || !p.writable(path.Dir(bundle)) {
			return Install{Method: Manual, Path: bundle}
		}
		return Install{Method: Bundle, Path: bundle}
	case "windows":
		lower := strings.ToLower(strings.ReplaceAll(p.exe, "/", `\`))
		if strings.Contains(lower, `\chocolatey\lib\`) {
			return Install{Method: Chocolatey, Path: p.exe}
		}
		if root := strings.ToLower(strings.TrimRight(strings.ReplaceAll(p.getenv("ChocolateyInstall"), "/", `\`), `\`)); root != "" && strings.HasPrefix(lower, root+`\lib\`) {
			return Install{Method: Chocolatey, Path: p.exe}
		}
		dir := windowsDir(p.exe)
		if !strings.HasSuffix(lower, ".exe") || !p.writable(dir) {
			return manual
		}
		return Install{
			Method:             Program,
			Path:               p.exe,
			PreferWindowsSetup: isWindowsSetupInstall(p, dir),
		}
	case "linux":
		if (strings.HasPrefix(p.exe, "/usr/") && !strings.HasPrefix(p.exe, "/usr/local/")) || strings.HasPrefix(p.exe, "/opt/") {
			if p.exists("/var/lib/dpkg/info/"+Package+".list") || p.exists("/var/lib/dpkg/info/"+Package+":"+p.goarch+".list") {
				return Install{Method: Deb, Path: p.exe}
			}
			if p.exists("/usr/bin/rpm") || p.exists("/bin/rpm") {
				tool := "rpm"
				switch {
				case p.exists("/usr/bin/dnf"):
					tool = "dnf"
				case p.exists("/usr/bin/zypper"):
					tool = "zypper"
				}
				return Install{Method: RPM, Path: p.exe, Tool: tool}
			}
			return manual
		}
		// Ways the app is not shipped, whose files are not its to change.
		for _, v := range []string{"APPIMAGE", "SNAP", "FLATPAK_ID"} {
			if p.getenv(v) != "" {
				return manual
			}
		}
		if !p.writable(path.Dir(p.exe)) {
			return manual
		}
		return Install{Method: Program, Path: p.exe}
	}
	return manual
}

// cask reports whether Homebrew's cask installed the bundle.
func (p probe) cask(bundle string) bool {
	if strings.Contains(bundle, "/Caskroom/") {
		return true
	}
	dir := path.Dir(bundle)
	home := strings.TrimRight(p.getenv("HOME"), "/")
	if dir != "/Applications" && (home == "" || dir != home+"/Applications") {
		return false
	}
	for _, prefix := range []string{p.getenv("HOMEBREW_PREFIX"), "/opt/homebrew", "/usr/local"} {
		if prefix != "" && p.exists(strings.TrimRight(prefix, "/")+"/Caskroom/"+Package) {
			return true
		}
	}
	return false
}

// bundleOf is the app bundle a macOS program is in: X.app of
// X.app/Contents/MacOS/program.
func bundleOf(exe string) (string, bool) {
	macos := path.Dir(exe)
	contents := path.Dir(macos)
	bundle := path.Dir(contents)
	if path.Base(macos) != "MacOS" || path.Base(contents) != "Contents" || !strings.HasSuffix(bundle, ".app") {
		return "", false
	}
	return bundle, true
}

// windowsDir is the folder of a Windows path, which this package reads on
// any system.
func windowsDir(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[:i]
	}
	return "."
}

// isWindowsSetupInstall reports whether a writable Windows install in dir
// should update via the Setup installer rather than the portable zip: either it
// was already installed by Setup (install-manifest.json beside the exe) or it
// lives in the standard per-user install.ps1 directory
// (%LOCALAPPDATA%\Programs\GVX Desktop).
func isWindowsSetupInstall(p probe, dir string) bool {
	sep := `\`
	if !strings.Contains(dir, `\`) && strings.Contains(dir, "/") {
		sep = "/"
	}
	if p.exists(strings.TrimRight(dir, `\/`) + sep + "install-manifest.json") {
		return true
	}
	localAppData := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(p.getenv("LOCALAPPDATA")), "/", `\`), `\`)
	if localAppData == "" {
		return false
	}
	normDir := strings.TrimRight(strings.ReplaceAll(dir, "/", `\`), `\`)
	wantDir := localAppData + `\Programs\GVX Desktop`
	return strings.EqualFold(normDir, wantDir)
}

// AssetName is the file of the release tagged tag that installs the app
// on goos and goarch by method m. The app is built for Apple silicon Macs,
// and for amd64 and arm64 PCs on Windows and Linux.
func AssetName(tag, goos, goarch string, m Method) (string, bool) {
	v := Plain(tag)
	if goarch != "arm64" && (goos == "darwin" || goarch != "amd64") {
		return "", false
	}
	switch goos {
	case "darwin":
		return Package + "-" + v + "-macos-arm64.dmg", true
	case "windows":
		return Package + "-" + v + "-windows-" + goarch + ".zip", true
	case "linux":
		switch m {
		case Deb:
			return Package + "_" + v + "_" + goarch + ".deb", true
		case RPM:
			arch := "x86_64"
			if goarch == "arm64" {
				arch = "aarch64"
			}
			return Package + "-" + v + "-1." + arch + ".rpm", true
		}
		return Package + "-" + v + "-linux-" + goarch + ".tar.gz", true
	}
	return "", false
}

// Command is what updates in of the release rel from a terminal, for the
// installs a package manager keeps: brew's or choco's command, or for a
// Linux package, one that downloads the release's package, checks it
// against its checksum and installs it. It is empty for the others, and
// for a release with no package for this machine.
func (in Install) Command(rel Release) string { return in.command(rel, runtime.GOARCH) }

// CommandForVersion builds the terminal update command for version (such as
// "0.9.0" or "v0.9.0") using deterministic GitHub release download URLs, for
// feeds such as appcast.xml that do not enumerate .deb/.rpm enclosures.
func (in Install) CommandForVersion(version string) string {
	return in.commandForVersion(version, runtime.GOARCH)
}

func (in Install) commandForVersion(version, goarch string) string {
	switch in.Method {
	case Homebrew:
		return homebrewCommand
	case Chocolatey:
		return "choco upgrade " + Package + " -y"
	case Deb, RPM:
		plain := Plain(version)
		if plain == "" {
			return ""
		}
		tag := "v" + plain
		name, ok := AssetName(tag, "linux", goarch, in.Method)
		if !ok {
			return ""
		}
		base := ReleasesURL + "/download/" + tag + "/"
		return in.linuxPackageCommand(name, base+name, base+name+".sha256")
	}
	return ""
}

func (in Install) command(rel Release, goarch string) string {
	switch in.Method {
	case Homebrew:
		return homebrewCommand
	case Chocolatey:
		return "choco upgrade " + Package + " -y"
	case Deb, RPM:
		name, ok := AssetName(rel.Tag, "linux", goarch, in.Method)
		if !ok {
			return ""
		}
		pkg, ok1 := rel.Find(name)
		sum, ok2 := rel.Find(name + ".sha256")
		if !ok1 || !ok2 {
			return ""
		}
		return in.linuxPackageCommand(name, pkg.URL, sum.URL)
	}
	return ""
}

func (in Install) linuxPackageCommand(name, pkgURL, sumURL string) string {
	install := "sudo apt install"
	if in.Method == RPM {
		switch in.Tool {
		case "dnf":
			install = "sudo dnf install"
		case "zypper":
			install = "sudo zypper install"
		default:
			install = "sudo rpm -U"
		}
	}
	return `cd "$(mktemp -d)" && curl -fL -O ` + shellQuote(pkgURL) + ` -O ` + shellQuote(sumURL) +
		` && sha256sum -c ` + shellQuote(name+".sha256") + ` && ` + install + ` ./` + shellQuote(name)
}

// UpdaterOption customizes NewUpdater, primarily for tests and first-run
// settings migration.
type UpdaterOption func(*updaterInitOptions)

type updaterInitOptions struct {
	cfg             updater.Config
	seedSettings    bool
	seedAutoCheck   bool
	seedSkipVersion string
}

// WithInitialSettings seeds updater-state.json from GVX Desktop's settings.json
// when updater-state.json does not exist yet, preserving existing users'
// NoAppUpdateCheck and SkipAppVersion preferences across the upgrade.
func WithInitialSettings(autoCheck bool, skipVersion string) UpdaterOption {
	return func(o *updaterInitOptions) {
		o.seedSettings = true
		o.seedAutoCheck = autoCheck
		o.seedSkipVersion = skipVersion
	}
}

// WithPublicKey overrides PublicKey when constructing an Updater.
func WithPublicKey(pubKey string) UpdaterOption {
	return func(o *updaterInitOptions) { o.cfg.PublicKey = pubKey }
}

// WithFeedURL overrides AppcastURL when constructing an Updater.
func WithFeedURL(feedURL string) UpdaterOption {
	return func(o *updaterInitOptions) { o.cfg.FeedURL = feedURL }
}

// WithAllowInsecureHTTP enables plain HTTP URLs (for local tests).
func WithAllowInsecureHTTP(allow bool) UpdaterOption {
	return func(o *updaterInitOptions) { o.cfg.AllowInsecureHTTP = allow }
}

// WithApplier overrides the platform update applier (for tests).
func WithApplier(a updater.Applier) UpdaterOption {
	return func(o *updaterInitOptions) { o.cfg.Applier = a }
}

// WithOSArch overrides the detected OS and architecture (for tests).
func WithOSArch(osName, arch string) UpdaterOption {
	return func(o *updaterInitOptions) {
		o.cfg.OS = osName
		o.cfg.Arch = arch
	}
}

// WithCheckInterval overrides the automatic check interval (for tests).
func WithCheckInterval(d time.Duration) UpdaterOption {
	return func(o *updaterInitOptions) { o.cfg.CheckInterval = d }
}

// NewUpdater constructs a ggui/updater.Updater for GVX Desktop backed by the
// signed Sparkle 2 appcast feed.
//
// It returns (nil, nil) when PublicKey is empty (and not overridden) or when
// currentVersion is not a release version (such as "dev"), allowing callers to
// fall back cleanly to the GitHub Releases API + .sha256 updater.
func NewUpdater(dataDir, currentVersion string, in Install, opts ...UpdaterOption) (*updater.Updater, error) {
	if !Comparable(currentVersion) {
		return nil, nil
	}
	initOpts := updaterInitOptions{
		cfg: updater.Config{
			FeedURL:            appcastURL,
			PublicKey:          PublicKey,
			CurrentVersion:     Plain(currentVersion),
			AppID:              BundleID,
			AppName:            "GVX Desktop",
			StateDir:           dataDir,
			CacheDir:           filepath.Join(dataDir, "updates"),
			PreferWindowsSetup: in.PreferWindowsSetup,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&initOpts)
		}
	}
	if strings.TrimSpace(initOpts.cfg.PublicKey) == "" && !initOpts.cfg.AllowUnsigned {
		return nil, nil
	}

	var firstRun bool
	if initOpts.seedSettings && initOpts.cfg.StateDir != "" {
		_, err := os.Stat(filepath.Join(initOpts.cfg.StateDir, "updater-state.json"))
		firstRun = errors.Is(err, os.ErrNotExist)
	}

	u, err := updater.New(initOpts.cfg)
	if err != nil {
		return nil, err
	}
	if firstRun {
		_ = u.SetAutoCheckEnabled(initOpts.seedAutoCheck)
		if skip := Plain(initOpts.seedSkipVersion); skip != "" {
			_ = u.SkipVersion(skip)
		}
	}
	return u, nil
}

// IsFeedNotFound reports whether err was caused by appcast.xml returning HTTP
// 404 (for example during the transition before a release publishes
// appcast.xml).
func IsFeedNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "returned HTTP 404")
}

// shellQuote quotes s for a POSIX shell, leaving it bare when it needs
// nothing.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
