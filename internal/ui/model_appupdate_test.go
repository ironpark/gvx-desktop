package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/updater"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// appUpdateModel is a model whose app is version 0.5.1, with no network.
func appUpdateModel(t *testing.T) (*Model, *ggui.Probe) {
	t.Helper()
	old := AppVersion
	AppVersion = "0.5.1"
	t.Cleanup(func() { AppVersion = old })
	dir := t.TempDir()
	set := store.DefaultSettings()
	set.Language = LangEnglish
	st := store.Store{Dir: dir}
	st.SaveSettings(set)
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1100, 760))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p
}

func TestAppUpdateBanner(t *testing.T) {
	m, p := appUpdateModel(t)
	const title = "GVX Desktop 0.6.0 is available"
	shows := func(want bool, why string) {
		t.Helper()
		if ok := strings.Contains(p.Semantics().String(), title); ok != want {
			t.Errorf("%s: the banner shows: %v", why, ok)
		}
	}
	shows(false, "nothing known")
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(true, "a newer release")
	if _, ok := p.Find("Update"); !ok {
		t.Error("no Update button")
	}
	m.SkipAppVersion()
	shows(false, "skipped")
	if got := ggui.Peek(m.Settings).SkipAppVersion; got != "v0.6.0" {
		t.Errorf("skipped %q", got)
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.1", Install: appupdate.Install{Method: appupdate.Bundle}})
	if !strings.Contains(p.Semantics().String(), "GVX Desktop 0.6.1 is available") {
		t.Error("a release past the skipped one is not offered")
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.5.1", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(false, "the same version")
	AppVersion = "dev"
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(false, "a development build")
}

// brewCommand is what updates a copy Homebrew installed.
const brewCommand = "brew update\nbrew upgrade --cask ironpark/tap/gvx-desktop"

func TestAppUpdateActionFollowsTheInstall(t *testing.T) {
	m, p := appUpdateModel(t)
	for _, c := range []struct {
		u    AppUpdate
		verb string
	}{
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Bundle}}, "Update"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Program}, Ready: true}, "Restart to update"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Homebrew}, Command: brewCommand}, "Copy command"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Deb}}, "Open the release page"}, // no package to name
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Manual}}, "Open the release page"},
	} {
		c.u.Tag = "v0.6.0"
		if got := appUpdateVerb(c.u); got != c.verb {
			t.Errorf("%v: %q", c.u.Install.Method, got)
		}
		m.AppUpdate.Set(c.u)
		p.Frame()
		if _, ok := p.Find(c.verb); !ok {
			t.Errorf("%v: no %q in the window", c.u.Install.Method, c.verb)
		}
	}
	// Homebrew's command is two lines, which do not fit in the banner's
	// sentence: the banner leaves it to its button, and the settings show it
	// in a block to copy.
	brew := AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Homebrew}, Command: brewCommand}
	if strings.Contains(appUpdateText(brew), "brew update") {
		t.Error("Homebrew's banner puts the command in its sentence")
	}
	m.AppUpdate.Set(brew)
	m.Go(PageSettings)
	extUISettle(t, p, func() bool {
		return strings.Contains(p.Semantics().String(), "brew update\\nbrew upgrade --cask ironpark/tap/gvx-desktop")
	}, "the settings show Homebrew's command")
}

// The restart that puts the update in place asks first, and says the
// sandboxes running stop; without an update downloaded it does nothing.
func TestAskRestartToUpdate(t *testing.T) {
	m, _ := appUpdateModel(t)
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	m.AskRestartToUpdate()
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("asked to restart with nothing downloaded")
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}, Ready: true})
	m.AskRestartToUpdate()
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmLabel) != "Restart to update" || !strings.Contains(ggui.Peek(m.ConfirmText), "0.6.0") {
		t.Errorf("open %v, %q: %q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmLabel), ggui.Peek(m.ConfirmText))
	}
	if !ggui.Peek(m.ConfirmSafe) {
		t.Error("with no sandbox running, restarting destroys nothing")
	}
	// Nothing was staged, so confirming it does not quit.
	quits := 0
	old := quitApp
	quitApp = func() { quits++ }
	t.Cleanup(func() { quitApp = old })
	m.RunConfirmed()
	if quits != 0 {
		t.Error("quit with no update to put in place")
	}
	if err := m.FinishUpdate(); err != nil {
		t.Errorf("FinishUpdate with no update: %v", err)
	}
}

func TestOfflineChecksNothing(t *testing.T) {
	m, _ := appUpdateModel(t)
	m.CheckAppUpdate(false)
	if u := ggui.Peek(m.AppUpdate); u.Checking || u.Tag != "" {
		t.Errorf("an offline model checked: %+v", u)
	}
}

func TestRelaunchArgs(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{nil, nil},
		{[]string{"-hidden"}, nil},
		{[]string{"--hidden=true", "-data-dir", "/x"}, []string{"-data-dir", "/x"}},
		{[]string{"-data-dir=/hidden"}, []string{"-data-dir=/hidden"}},
		{[]string{"hidden"}, []string{"hidden"}},
	} {
		if got := relaunchArgs(c.in); !slices.Equal(got, c.want) {
			t.Errorf("relaunchArgs(%q) = %q", c.in, got)
		}
	}
}

type testApplier struct {
	applied atomic.Int32
}

func (a *testApplier) Apply(_ context.Context, _ *updater.DownloadedUpdate, opts updater.ApplyOptions) error {
	a.applied.Add(1)
	if opts.QuitFunc != nil {
		opts.QuitFunc()
	}
	return nil
}

func TestAppUpdateControllerSignedFeedAndHooks(t *testing.T) {
	pub, priv, err := updater.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("signed release payload")
	sig, err := updater.SignBytes(priv, payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	shaHex := hex.EncodeToString(sum[:])

	feedOS := runtime.GOOS
	if feedOS == "darwin" {
		feedOS = "macos"
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/appcast.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>GVX Desktop</title>
    <item>
      <title>GVX Desktop 0.9.0</title>
      <sparkle:version>0.9.0</sparkle:version>
      <sparkle:shortVersionString>0.9.0</sparkle:shortVersionString>
      <sparkle:releaseNotesLink>https://github.com/ironpark/gvx-desktop/releases/tag/v0.9.0</sparkle:releaseNotesLink>
      <description><![CDATA[### Highlights
- Windows setup installer
]]></description>
      <enclosure url="%s/gvx-desktop-0.9.0.zip"
        sparkle:os="%s" sparkle:arch="%s"
        sparkle:edSignature="%s" sparkle:sha256="%s"
        length="%d" type="application/octet-stream" />
    </item>
  </channel>
</rss>`, srv.URL, feedOS, runtime.GOARCH, sig, shaHex, len(payload))
		case "/gvx-desktop-0.9.0.zip":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldVer := AppVersion
	AppVersion = "0.8.0"
	t.Cleanup(func() { AppVersion = oldVer })

	oldQuit := quitApp
	var quits atomic.Int32
	quitApp = func() { quits.Add(1) }
	t.Cleanup(func() { quitApp = oldQuit })

	dir := t.TempDir()
	set := store.DefaultSettings()
	set.Language = LangEnglish
	st := store.Store{Dir: dir}
	_ = st.SaveSettings(set)
	t.Cleanup(func() { SetLanguage(LangEnglish) })

	ap := &testApplier{}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1100, 760))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.AppUpdate.Set(AppUpdate{Install: appupdate.Install{Method: appupdate.Bundle}})
		u, uerr := appupdate.NewUpdater(dir, AppVersion, appupdate.Install{Method: appupdate.Bundle},
			appupdate.WithPublicKey(pub),
			appupdate.WithFeedURL(srv.URL+"/appcast.xml"),
			appupdate.WithAllowInsecureHTTP(true),
			appupdate.WithApplier(ap),
		)
		if uerr != nil || u == nil {
			t.Fatalf("NewUpdater: u=%v err=%v", u, uerr)
		}
		m.attachAppUpdater(u)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", msg)
	}

	// 1. Quiet check surfaces the Dashboard banner WITHOUT popping the modal dialog.
	m.CheckAppUpdate(true)
	waitFor(func() bool { return ggui.Peek(m.UpdateCtrl.Phase) == updater.PhaseAvailable }, "silent check available")
	if ggui.Peek(m.UpdateCtrl.Open) {
		t.Error("AutoOpenOnBackground=false: silent check should not open dialog")
	}
	if au := ggui.Peek(m.AppUpdate); au.Tag != "v0.9.0" || !au.Available() {
		t.Errorf("AppUpdate not synced from UpdateCtrl: %+v", au)
	}
	if !strings.Contains(p.Semantics().String(), "GVX Desktop 0.9.0 is available") {
		t.Error("Dashboard banner should show 0.9.0 after silent check")
	}

	// 2. External package manager resolution (Homebrew) shows copyable brew command in updater.Dialog.
	m.AppUpdate.Update(func(au AppUpdate) AppUpdate { au.Install = appupdate.Install{Method: appupdate.Homebrew}; return au })
	m.UpdateCtrl.CheckNow()
	waitFor(func() bool { return ggui.Peek(m.UpdateCtrl.Phase) == updater.PhaseAvailable }, "homebrew manual check available")
	p.Frame()
	sem := p.Semantics().String()
	if !strings.Contains(sem, "brew upgrade --cask ironpark/tap/gvx-desktop") {
		t.Errorf("Homebrew updater dialog should show brew command:\n%s", sem)
	}
	if !strings.Contains(sem, "Copy command") {
		t.Errorf("Homebrew updater dialog should show Copy command button:\n%s", sem)
	}

	// 3. Switch back to self-updating Bundle, download, and test BeforeInstall gate when sandboxes are running.
	m.AppUpdate.Update(func(au AppUpdate) AppUpdate { au.Install = appupdate.Install{Method: appupdate.Bundle}; return au })
	m.UpdateCtrl.CheckNow()
	waitFor(func() bool { return ggui.Peek(m.UpdateCtrl.Phase) == updater.PhaseAvailable }, "bundle manual check available")
	m.UpdateCtrl.Download()
	waitFor(func() bool { return ggui.Peek(m.UpdateCtrl.Phase) == updater.PhaseReadyToInstall }, "download ready")
	if !ggui.Peek(m.AppUpdate).Ready {
		t.Error("AppUpdate.Ready should be true once verified")
	}

	// Simulate a running sandbox-engine container so BeforeInstall prompts for confirmation first.
	m.EngineRunning.Set(1)
	p.Frame()
	m.UpdateCtrl.Install()
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("BeforeInstall should ask confirmation when sandboxes are running")
	}
	if ap.applied.Load() != 0 {
		t.Fatal("Apply ran before user confirmed stopping sandboxes")
	}
	// Cancel confirmation -> stays in PhaseReadyToInstall; clear running count and install directly.
	m.ConfirmOpen.Set(false)
	m.EngineRunning.Set(0)
	p.Frame()
	m.UpdateCtrl.Install()
	waitFor(func() bool { return ap.applied.Load() == 1 && quits.Load() == 1 }, "apply and quit")
}

func TestAppUpdate404FallsBackToGitHubReleases(t *testing.T) {
	pub, _, err := updater.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	oldVer := AppVersion
	AppVersion = "0.8.0"
	t.Cleanup(func() { AppVersion = oldVer })

	var legacyCalls atomic.Int32
	oldFetch := fetchLatestRelease
	fetchLatestRelease = func(context.Context) (appupdate.Release, error) {
		legacyCalls.Add(1)
		return appupdate.Release{
			Tag:  "v0.8.5",
			Page: "https://github.com/ironpark/gvx-desktop/releases/tag/v0.8.5",
		}, nil
	}
	t.Cleanup(func() { fetchLatestRelease = oldFetch })

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	t.Cleanup(func() { SetLanguage(LangEnglish) })

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1100, 760))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.AppUpdate.Set(AppUpdate{Install: appupdate.Install{Method: appupdate.Bundle}})
		u, uerr := appupdate.NewUpdater(dir, AppVersion, appupdate.Install{Method: appupdate.Bundle},
			appupdate.WithPublicKey(pub),
			appupdate.WithFeedURL(srv.URL+"/appcast.xml"),
			appupdate.WithAllowInsecureHTTP(true),
		)
		if uerr != nil || u == nil {
			t.Fatalf("NewUpdater: u=%v err=%v", u, uerr)
		}
		m.attachAppUpdater(u)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Manual check against 404 appcast.xml dismisses the updater modal and
	// seamlessly falls back to the GitHub Releases API.
	m.CheckAppUpdate(false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		if legacyCalls.Load() > 0 && ggui.Peek(m.AppUpdate).Tag == "v0.8.5" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if legacyCalls.Load() == 0 {
		t.Fatal("expected HTTP 404 appcast to fall back to GitHub Releases API")
	}
	if ggui.Peek(m.UpdateCtrl.Open) {
		t.Error("updater dialog should dismiss when falling back from 404")
	}
	if got := ggui.Peek(m.AppUpdate).Tag; got != "v0.8.5" {
		t.Errorf("AppUpdate.Tag = %q, want v0.8.5", got)
	}

	// Quiet check against 404 appcast.xml also falls back silently.
	m.AppUpdate.Update(func(au AppUpdate) AppUpdate { au.Tag = ""; return au })
	before := legacyCalls.Load()
	m.CheckAppUpdate(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		if legacyCalls.Load() > before && ggui.Peek(m.AppUpdate).Tag == "v0.8.5" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if legacyCalls.Load() <= before {
		t.Fatal("expected quiet HTTP 404 appcast to fall back to GitHub Releases API")
	}
}
