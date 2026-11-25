package appupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ironpark/ggui/updater"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   "v0.6.0",
			"html_url":   "https://github.com/ironpark/gvx-desktop/releases/tag/v0.6.0",
			"prerelease": false,
			"assets": []map[string]any{
				{"name": "gvx-desktop-0.6.0-macos-arm64.dmg", "browser_download_url": "https://example.com/dmg", "size": 40 << 20},
				{"name": "gvx-desktop-0.6.0-macos-arm64.dmg.sha256", "browser_download_url": "https://example.com/dmg.sha256", "size": 100},
			},
		})
	}))
	defer srv.Close()
	old := latestURL
	latestURL = srv.URL
	defer func() { latestURL = old }()

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v0.6.0" || !strings.HasSuffix(rel.Page, "/v0.6.0") || len(rel.Assets) != 2 {
		t.Fatalf("%+v", rel)
	}
	name, _ := AssetName(rel.Tag, "darwin", "arm64", Bundle)
	if a, ok := rel.Find(name); !ok || a.Size != 40<<20 {
		t.Errorf("the disk image: %+v %v", a, ok)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, current string
		want         bool
	}{
		{"v0.6.0", "0.5.1", true},
		{"v0.5.2", "0.5.1", true},
		{"v1.0.0", "0.99.99", true},
		{"v0.10.0", "0.9.0", true},
		{"v0.5.1", "0.5.1", false}, // the same
		{"0.5.1", "v0.5.1", false},
		{"v0.5.0", "0.5.1", false}, // older
		{"v0.4.9", "0.5.1", false},
		{"v0.6.0", "dev", false}, // a development build is offered nothing
		{"v0.6.0", "", false},
		{"v0.6.0", "0.5", false},
		{"v0.6.0", "(devel)", false},
		{"v0.6.0-rc1", "0.5.1", false}, // a prerelease is not offered
		{"v0.6.0-beta.2", "0.5.1", false},
		{"v0.6.0", "0.6.0-rc1", true}, // the release after its prerelease
		{"v0.6.1", "0.6.0-rc1", true},
		{"v0.5.9", "0.6.0-rc1", false},
		{"v0.6.0+build5", "0.5.1", true}, // build metadata counts for nothing
		{"v0.6.0+build5", "0.6.0", false},
		{"latest", "0.5.1", false},
		{"v0.6", "0.5.1", false},
		{"v0.6.0.1", "0.5.1", false},
		{"v0.6.x", "0.5.1", false},
		{"v-1.0.0", "0.5.1", false},
		{"v0.6.0-", "0.5.1", false},
	} {
		if got := Newer(c.tag, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.tag, c.current, got)
		}
	}
	for v, want := range map[string]bool{"0.5.1": true, "v0.5.1": true, "1.0.0-rc1": true, "dev": false, "": false, "0.5": false} {
		if Comparable(v) != want {
			t.Errorf("Comparable(%q) = %v", v, !want)
		}
	}
}

func TestAssetName(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		m            Method
		want         string // empty: no build
	}{
		{"darwin", "arm64", Bundle, "gvx-desktop-0.6.0-macos-arm64.dmg"},
		{"darwin", "arm64", Homebrew, "gvx-desktop-0.6.0-macos-arm64.dmg"},
		{"darwin", "amd64", Bundle, ""}, // no Intel build
		{"windows", "amd64", Program, "gvx-desktop-0.6.0-windows-amd64.zip"},
		{"windows", "arm64", Program, "gvx-desktop-0.6.0-windows-arm64.zip"},
		{"windows", "386", Program, ""},
		{"linux", "amd64", Program, "gvx-desktop-0.6.0-linux-amd64.tar.gz"},
		{"linux", "arm64", Program, "gvx-desktop-0.6.0-linux-arm64.tar.gz"},
		{"linux", "amd64", Deb, "gvx-desktop_0.6.0_amd64.deb"},
		{"linux", "arm64", Deb, "gvx-desktop_0.6.0_arm64.deb"},
		{"linux", "amd64", RPM, "gvx-desktop-0.6.0-1.x86_64.rpm"},
		{"linux", "arm64", RPM, "gvx-desktop-0.6.0-1.aarch64.rpm"},
		{"linux", "riscv64", Program, ""},
		{"freebsd", "amd64", Program, ""},
	} {
		got, ok := AssetName("v0.6.0", c.goos, c.goarch, c.m)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s/%s %v: %q, %v", c.goos, c.goarch, c.m, got, ok)
		}
	}
}

func TestDetect(t *testing.T) {
	const home = "/Users/me"
	for _, c := range []struct {
		name     string
		goos     string
		exe      string
		env      map[string]string
		exists   []string
		readOnly []string // folders this user may not write to
		want     Install
	}{
		{name: "mac app in /Applications", goos: "darwin", exe: "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			want: Install{Method: Bundle, Path: "/Applications/GVX Desktop.app"}},
		{name: "mac app in ~/Applications", goos: "darwin", exe: home + "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			want: Install{Method: Bundle, Path: home + "/Applications/GVX Desktop.app"}},
		{name: "mac app from the cask", goos: "darwin", exe: "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			exists: []string{"/opt/homebrew/Caskroom/gvx-desktop"},
			want:   Install{Method: Homebrew, Path: "/Applications/GVX Desktop.app"}},
		{name: "mac app from the cask, Intel prefix, appdir in home", goos: "darwin", exe: home + "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			exists: []string{"/usr/local/Caskroom/gvx-desktop"},
			want:   Install{Method: Homebrew, Path: home + "/Applications/GVX Desktop.app"}},
		{name: "mac app from the cask, custom prefix", goos: "darwin", exe: "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			env: map[string]string{"HOMEBREW_PREFIX": "/opt/brew/"}, exists: []string{"/opt/brew/Caskroom/gvx-desktop"},
			want: Install{Method: Homebrew, Path: "/Applications/GVX Desktop.app"}},
		{name: "mac app under the Caskroom", goos: "darwin", exe: "/opt/homebrew/Caskroom/gvx-desktop/0.5.1/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			want: Install{Method: Homebrew, Path: "/opt/homebrew/Caskroom/gvx-desktop/0.5.1/GVX Desktop.app"}},
		{name: "mac app elsewhere while the cask is installed", goos: "darwin", exe: home + "/Downloads/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			exists: []string{"/opt/homebrew/Caskroom/gvx-desktop"},
			want:   Install{Method: Bundle, Path: home + "/Downloads/GVX Desktop.app"}},
		{name: "mac app another cask", goos: "darwin", exe: "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			exists: []string{"/opt/homebrew/Caskroom/docker"},
			want:   Install{Method: Bundle, Path: "/Applications/GVX Desktop.app"}},
		{name: "mac app in a folder not writable", goos: "darwin", exe: "/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			readOnly: []string{"/Applications"},
			want:     Install{Method: Manual, Path: "/Applications/GVX Desktop.app"}},
		{name: "mac app translocated", goos: "darwin", exe: "/private/var/folders/x/T/AppTranslocation/ABCD/d/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			want: Install{Method: Manual, Path: "/private/var/folders/x/T/AppTranslocation/ABCD/d/GVX Desktop.app"}},
		{name: "mac app on its disk image", goos: "darwin", exe: "/Volumes/GVX Desktop/GVX Desktop.app/Contents/MacOS/gvx-desktop",
			readOnly: []string{"/Volumes/GVX Desktop"},
			want:     Install{Method: Manual, Path: "/Volumes/GVX Desktop/GVX Desktop.app"}},
		{name: "mac program out of a bundle", goos: "darwin", exe: home + "/src/gvx-desktop/bin/gvx-desktop",
			want: Install{Method: Manual, Path: home + "/src/gvx-desktop/bin/gvx-desktop"}},

		{name: "windows installer", goos: "windows", exe: `C:\Users\me\AppData\Local\Programs\GVX Desktop\GVX Desktop.exe`,
			want: Install{Method: Program, Path: `C:\Users\me\AppData\Local\Programs\GVX Desktop\GVX Desktop.exe`}},
		{name: "windows legacy install.ps1 default dir prefers setup", goos: "windows", exe: `C:\Users\me\AppData\Local\Programs\GVX Desktop\GVX Desktop.exe`,
			env:  map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`},
			want: Install{Method: Program, Path: `C:\Users\me\AppData\Local\Programs\GVX Desktop\GVX Desktop.exe`, PreferWindowsSetup: true}},
		{name: "windows custom dir with install-manifest prefers setup", goos: "windows", exe: `D:\Apps\GVX Desktop\GVX Desktop.exe`,
			env:    map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`},
			exists: []string{`D:\Apps\GVX Desktop\install-manifest.json`},
			want:   Install{Method: Program, Path: `D:\Apps\GVX Desktop\GVX Desktop.exe`, PreferWindowsSetup: true}},
		{name: "windows portable zip in custom dir stays portable", goos: "windows", exe: `D:\Portable\GVX Desktop\GVX Desktop.exe`,
			env:  map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`},
			want: Install{Method: Program, Path: `D:\Portable\GVX Desktop\GVX Desktop.exe`, PreferWindowsSetup: false}},
		{name: "windows chocolatey", goos: "windows", exe: `C:\ProgramData\chocolatey\lib\gvx-desktop\tools\GVX Desktop.exe`,
			want: Install{Method: Chocolatey, Path: `C:\ProgramData\chocolatey\lib\gvx-desktop\tools\GVX Desktop.exe`}},
		{name: "windows chocolatey, other case", goos: "windows", exe: `C:\ProgramData\Chocolatey\Lib\gvx-desktop\tools\GVX Desktop.exe`,
			want: Install{Method: Chocolatey, Path: `C:\ProgramData\Chocolatey\Lib\gvx-desktop\tools\GVX Desktop.exe`}},
		{name: "windows chocolatey elsewhere", goos: "windows", exe: `D:\tools\choco\lib\gvx-desktop\tools\GVX Desktop.exe`,
			env:  map[string]string{"ChocolateyInstall": `D:\Tools\Choco\`},
			want: Install{Method: Chocolatey, Path: `D:\tools\choco\lib\gvx-desktop\tools\GVX Desktop.exe`}},
		{name: "windows program files", goos: "windows", exe: `C:\Program Files\GVX Desktop\GVX Desktop.exe`,
			readOnly: []string{`C:\Program Files\GVX Desktop`},
			want:     Install{Method: Manual, Path: `C:\Program Files\GVX Desktop\GVX Desktop.exe`}},

		{name: "linux tarball in ~/.local", goos: "linux", exe: "/home/me/.local/bin/gvx-desktop",
			want: Install{Method: Program, Path: "/home/me/.local/bin/gvx-desktop"}},
		{name: "linux tarball in /usr/local", goos: "linux", exe: "/usr/local/bin/gvx-desktop",
			readOnly: []string{"/usr/local/bin"},
			want:     Install{Method: Manual, Path: "/usr/local/bin/gvx-desktop"}},
		{name: "linux deb", goos: "linux", exe: "/usr/bin/gvx-desktop",
			exists: []string{"/var/lib/dpkg/info/gvx-desktop.list", "/usr/bin/rpm"},
			want:   Install{Method: Deb, Path: "/usr/bin/gvx-desktop"}},
		{name: "linux deb, multiarch", goos: "linux", exe: "/usr/bin/gvx-desktop",
			exists: []string{"/var/lib/dpkg/info/gvx-desktop:amd64.list"},
			want:   Install{Method: Deb, Path: "/usr/bin/gvx-desktop"}},
		{name: "linux rpm with dnf", goos: "linux", exe: "/usr/bin/gvx-desktop",
			exists: []string{"/usr/bin/rpm", "/usr/bin/dnf"},
			want:   Install{Method: RPM, Path: "/usr/bin/gvx-desktop", Tool: "dnf"}},
		{name: "linux rpm with zypper", goos: "linux", exe: "/usr/bin/gvx-desktop",
			exists: []string{"/usr/bin/rpm", "/usr/bin/zypper"},
			want:   Install{Method: RPM, Path: "/usr/bin/gvx-desktop", Tool: "zypper"}},
		{name: "linux rpm alone", goos: "linux", exe: "/usr/bin/gvx-desktop",
			exists: []string{"/bin/rpm"},
			want:   Install{Method: RPM, Path: "/usr/bin/gvx-desktop", Tool: "rpm"}},
		{name: "linux in /usr from nowhere known", goos: "linux", exe: "/usr/bin/gvx-desktop",
			want: Install{Method: Manual, Path: "/usr/bin/gvx-desktop"}},
		{name: "linux in /opt", goos: "linux", exe: "/opt/gvx-desktop/gvx-desktop",
			exists: []string{"/var/lib/dpkg/info/gvx-desktop.list"},
			want:   Install{Method: Deb, Path: "/opt/gvx-desktop/gvx-desktop"}},
		{name: "linux appimage", goos: "linux", exe: "/tmp/.mount_gvx/usr/bin/gvx-desktop",
			env:  map[string]string{"APPIMAGE": "/home/me/gvx.AppImage"},
			want: Install{Method: Manual, Path: "/tmp/.mount_gvx/usr/bin/gvx-desktop"}},

		{name: "another system", goos: "freebsd", exe: "/usr/local/bin/gvx-desktop",
			want: Install{Method: Manual, Path: "/usr/local/bin/gvx-desktop"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"HOME": home}
			for k, v := range c.env {
				env[k] = v
			}
			got := detect(probe{
				goos:   c.goos,
				goarch: "amd64",
				exe:    c.exe,
				getenv: func(k string) string { return env[k] },
				exists: func(p string) bool {
					for _, e := range c.exists {
						if e == p {
							return true
						}
					}
					return false
				},
				writable: func(dir string) bool {
					for _, r := range c.readOnly {
						if r == dir {
							return false
						}
					}
					return true
				},
			})
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestCommand(t *testing.T) {
	rel := Release{Tag: "v0.6.0", Assets: []ghrelease.Asset{
		{Name: "gvx-desktop_0.6.0_amd64.deb", URL: "https://github.com/ironpark/gvx-desktop/releases/download/v0.6.0/gvx-desktop_0.6.0_amd64.deb"},
		{Name: "gvx-desktop_0.6.0_amd64.deb.sha256", URL: "https://github.com/ironpark/gvx-desktop/releases/download/v0.6.0/gvx-desktop_0.6.0_amd64.deb.sha256"},
		{Name: "gvx-desktop-0.6.0-1.x86_64.rpm", URL: "https://example.com/x.rpm"},
		{Name: "gvx-desktop-0.6.0-1.x86_64.rpm.sha256", URL: "https://example.com/x.rpm.sha256"},
	}}
	for _, c := range []struct {
		in   Install
		want string
	}{
		{Install{Method: Homebrew}, "brew update\nbrew upgrade --cask ironpark/tap/gvx-desktop"},
		{Install{Method: Chocolatey}, "choco upgrade gvx-desktop -y"},
		{Install{Method: Deb}, `cd "$(mktemp -d)" && curl -fL -O https://github.com/ironpark/gvx-desktop/releases/download/v0.6.0/gvx-desktop_0.6.0_amd64.deb -O https://github.com/ironpark/gvx-desktop/releases/download/v0.6.0/gvx-desktop_0.6.0_amd64.deb.sha256 && sha256sum -c gvx-desktop_0.6.0_amd64.deb.sha256 && sudo apt install ./gvx-desktop_0.6.0_amd64.deb`},
		{Install{Method: RPM, Tool: "dnf"}, `cd "$(mktemp -d)" && curl -fL -O https://example.com/x.rpm -O https://example.com/x.rpm.sha256 && sha256sum -c gvx-desktop-0.6.0-1.x86_64.rpm.sha256 && sudo dnf install ./gvx-desktop-0.6.0-1.x86_64.rpm`},
		{Install{Method: RPM, Tool: "zypper"}, `cd "$(mktemp -d)" && curl -fL -O https://example.com/x.rpm -O https://example.com/x.rpm.sha256 && sha256sum -c gvx-desktop-0.6.0-1.x86_64.rpm.sha256 && sudo zypper install ./gvx-desktop-0.6.0-1.x86_64.rpm`},
		{Install{Method: RPM, Tool: "rpm"}, `cd "$(mktemp -d)" && curl -fL -O https://example.com/x.rpm -O https://example.com/x.rpm.sha256 && sha256sum -c gvx-desktop-0.6.0-1.x86_64.rpm.sha256 && sudo rpm -U ./gvx-desktop-0.6.0-1.x86_64.rpm`},
		{Install{Method: Bundle}, ""},
		{Install{Method: Program}, ""},
		{Install{Method: Manual}, ""},
	} {
		if got := c.in.command(rel, "amd64"); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// A release without the package, or without its checksum, has no
	// command: the release page stands in for it.
	if got := (Install{Method: Deb}).command(rel, "arm64"); got != "" {
		t.Errorf("no arm64 package: %q", got)
	}
	noSum := Release{Tag: "v0.6.0", Assets: rel.Assets[:1]}
	if got := (Install{Method: Deb}).command(noSum, "amd64"); got != "" {
		t.Errorf("no checksum: %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":               "plain",
		"/Applications/X.app": "/Applications/X.app",
		"GVX Desktop.app":     "'GVX Desktop.app'",
		"it's":                `'it'\''s'`,
		"":                    "''",
		"$(rm -rf ~)":         "'$(rm -rf ~)'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCommandForVersion(t *testing.T) {
	for _, c := range []struct {
		in   Install
		ver  string
		arch string
		want string
	}{
		{Install{Method: Homebrew}, "0.9.0", "arm64", "brew update\nbrew upgrade --cask ironpark/tap/gvx-desktop"},
		{Install{Method: Chocolatey}, "0.9.0", "amd64", "choco upgrade gvx-desktop -y"},
		{Install{Method: Deb}, "0.9.0", "amd64", `cd "$(mktemp -d)" && curl -fL -O https://github.com/ironpark/gvx-desktop/releases/download/v0.9.0/gvx-desktop_0.9.0_amd64.deb -O https://github.com/ironpark/gvx-desktop/releases/download/v0.9.0/gvx-desktop_0.9.0_amd64.deb.sha256 && sha256sum -c gvx-desktop_0.9.0_amd64.deb.sha256 && sudo apt install ./gvx-desktop_0.9.0_amd64.deb`},
		{Install{Method: RPM, Tool: "dnf"}, "v0.9.0", "arm64", `cd "$(mktemp -d)" && curl -fL -O https://github.com/ironpark/gvx-desktop/releases/download/v0.9.0/gvx-desktop-0.9.0-1.aarch64.rpm -O https://github.com/ironpark/gvx-desktop/releases/download/v0.9.0/gvx-desktop-0.9.0-1.aarch64.rpm.sha256 && sha256sum -c gvx-desktop-0.9.0-1.aarch64.rpm.sha256 && sudo dnf install ./gvx-desktop-0.9.0-1.aarch64.rpm`},
		{Install{Method: Bundle}, "0.9.0", "arm64", ""},
		{Install{Method: Manual}, "0.9.0", "amd64", ""},
	} {
		if got := c.in.commandForVersion(c.ver, c.arch); got != c.want {
			t.Errorf("%+v (%s/%s):\n got %s\nwant %s", c.in, c.ver, c.arch, got, c.want)
		}
	}
}

func TestNewUpdaterAndInitialSettingsMigration(t *testing.T) {
	pub, _, err := updater.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	// Empty PublicKey falls back cleanly (returns nil, nil).
	if u, err := NewUpdater(t.TempDir(), "0.8.0", Install{Method: Bundle}); err != nil || u != nil {
		t.Fatalf("empty PublicKey: u=%v err=%v", u, err)
	}

	// Non-release build ("dev") falls back cleanly even when PublicKey is set.
	if u, err := NewUpdater(t.TempDir(), "dev", Install{Method: Bundle}, WithPublicKey(pub)); err != nil || u != nil {
		t.Fatalf("dev build: u=%v err=%v", u, err)
	}

	// First-run seeds updater-state.json from existing settings.json values.
	dir := t.TempDir()
	in := Install{Method: Program, PreferWindowsSetup: true}
	u1, err := NewUpdater(dir, "0.8.0", in,
		WithPublicKey(pub),
		WithInitialSettings(false, "v0.8.1"),
	)
	if err != nil || u1 == nil {
		t.Fatalf("NewUpdater first run: u=%v err=%v", u1, err)
	}
	if !u1.Config().PreferWindowsSetup {
		t.Error("PreferWindowsSetup not propagated to updater.Config")
	}
	if u1.AutoCheckEnabled() {
		t.Error("AutoCheckEnabled = true, want migrated false")
	}
	if got := u1.SkippedVersion(); got != "0.8.1" {
		t.Errorf("SkippedVersion = %q, want 0.8.1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "updater-state.json")); err != nil {
		t.Fatalf("expected updater-state.json: %v", err)
	}

	// Subsequent runs keep existing updater-state.json instead of overwriting it.
	u2, err := NewUpdater(dir, "0.8.0", in,
		WithPublicKey(pub),
		WithInitialSettings(true, "v0.9.9"),
	)
	if err != nil || u2 == nil {
		t.Fatalf("NewUpdater second run: u=%v err=%v", u2, err)
	}
	if u2.AutoCheckEnabled() {
		t.Error("second run overwrote AutoCheckEnabled")
	}
	if got := u2.SkippedVersion(); got != "0.8.1" {
		t.Errorf("second run overwrote SkippedVersion: %q", got)
	}

	if !IsFeedNotFound(errors.New("updater: appcast https://example.com/appcast.xml returned HTTP 404")) {
		t.Error("IsFeedNotFound should match HTTP 404")
	}
	if IsFeedNotFound(errors.New("updater: appcast https://example.com/appcast.xml returned HTTP 500")) {
		t.Error("IsFeedNotFound should not match HTTP 500")
	}
}
