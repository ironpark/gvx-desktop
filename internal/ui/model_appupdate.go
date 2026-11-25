package ui

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/updater"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// The app looks for a newer version of itself a while after it starts,
// out of the way of what starting does, and once a day after that, by the
// wall clock, which goes on while the computer sleeps.
const (
	appCheckDelay = 30 * time.Second
	appCheckEvery = 24 * time.Hour
)

// appDownloadTimeout bounds downloading an update of the app.
const appDownloadTimeout = 30 * time.Minute

// AppUpdate is what the app knows of its own newest release, and of the
// update it is getting.
type AppUpdate struct {
	Tag  string // the newest release, v0.6.0…; empty until it is known
	Page string // its notes

	// Install is how the app was installed, which says how it is updated,
	// and Command, for an install a package manager keeps, what updates it
	// from a terminal.
	Install appupdate.Install
	Command string

	Checking    bool
	Err         string // why the last check failed
	Downloading bool
	Percent     int  // of the download
	Ready       bool // downloaded and checked: a restart puts it in place
}

// Available reports a release newer than the app running. A development
// build is offered none.
func (u AppUpdate) Available() bool { return appupdate.Newer(u.Tag, AppVersion) }

// Name is the newest release as people say it: 0.6.0.
func (u AppUpdate) Name() string { return appupdate.Plain(u.Tag) }

// page is where the newest release's notes are, or the releases when
// there is none yet.
func (u AppUpdate) page() string {
	if u.Page != "" {
		return u.Page
	}
	return appupdate.ReleasesURL
}

// Offered reports that the app tells of an update by itself, in the
// banner and the tray: one is available that the user did not skip.
func (u AppUpdate) Offered(s store.Settings) bool { return u.Available() && u.Tag != s.SkipAppVersion }

// initAppUpdater configures the Sparkle 2 ggui/updater Controller when
// appupdate.PublicKey is configured and this is a release build. When not
// active, m.UpdateCtrl stays nil and the app uses the GitHub Releases API +
// .sha256 fallback seamlessly.
func (m *Model) initAppUpdater(dataDir string) {
	in := ggui.Peek(m.AppUpdate).Install
	set := ggui.Peek(m.Settings)
	u, err := appupdate.NewUpdater(dataDir, AppVersion, in,
		appupdate.WithInitialSettings(!set.NoAppUpdateCheck, set.SkipAppVersion),
	)
	if err != nil || u == nil {
		return
	}
	m.attachAppUpdater(u)
}

func (m *Model) attachAppUpdater(u *updater.Updater) {
	if u == nil {
		return
	}
	autoOpen := false
	cfg := updater.ControllerConfig{
		Quit:                 func() { quitApp() },
		RelaunchArgs:         func() []string { return relaunchArgs(os.Args[1:]) },
		AutoOpenOnBackground: &autoOpen,
		Translate:            tr,
		OnLink:               m.Open,
		ResolveExternal:      m.resolveExternalUpdate,
		BeforeInstall:        m.beforeAppUpdateInstall,
	}
	if m.host != nil {
		cfg.Post = m.host.Post
	}
	ctrl := updater.NewController(u, cfg)
	m.UpdateCtrl = ctrl

	type ctrlSnap struct {
		phase    updater.Phase
		release  *updater.Release
		progress float64
		errText  string
	}
	snap := ggui.Derived(func() ctrlSnap {
		return ctrlSnap{
			phase:    ctrl.Phase.Get(),
			release:  ctrl.Release.Get(),
			progress: ctrl.Progress.Get(),
			errText:  ctrl.ErrText.Get(),
		}
	})
	var prevPhase updater.Phase
	ggui.Watch(snap, func(s ctrlSnap) {
		prev := prevPhase
		prevPhase = s.phase

		switch s.phase {
		case updater.PhaseIdle:
			if prev == updater.PhaseAvailable {
				if skipped := appupdate.Plain(u.SkippedVersion()); skipped != "" {
					tag := "v" + skipped
					if ggui.Peek(m.Settings).SkipAppVersion != tag {
						m.post(func() {
							m.UpdateSettings(func(set *store.Settings) { set.SkipAppVersion = tag })
						})
					}
				}
			}
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading = false, false
				return au
			})
		case updater.PhaseChecking:
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading, au.Err = true, false, ""
				return au
			})
		case updater.PhaseUpToDate:
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading, au.Ready, au.Err = false, false, false, ""
				if au.Tag == "" && appupdate.Comparable(AppVersion) {
					au.Tag = "v" + appupdate.Plain(AppVersion)
				}
				return au
			})
		case updater.PhaseAvailable:
			if s.release == nil {
				return
			}
			ver := s.release.DisplayVersion()
			tag := "v" + appupdate.Plain(ver)
			page := releasePageFor(s.release)
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading, au.Ready, au.Err = false, false, false, ""
				au.Tag = tag
				au.Page = page
				au.Command = au.Install.CommandForVersion(ver)
				return au
			})
			if ggui.Peek(ctrl.ManualCheck) && ggui.Peek(m.Settings).SkipAppVersion == tag {
				m.post(func() {
					m.UpdateSettings(func(set *store.Settings) { set.SkipAppVersion = "" })
				})
			}
		case updater.PhaseDownloading:
			pct := int(math.Round(s.progress * 100))
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading, au.Ready, au.Err = false, true, false, ""
				au.Percent = pct
				return au
			})
		case updater.PhaseReadyToInstall, updater.PhaseInstalling:
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading, au.Ready, au.Err = false, false, true, ""
				au.Percent = 100
				return au
			})
		case updater.PhaseError:
			// During the transition before a release publishes appcast.xml,
			// fall back transparently to GitHub Releases API + .sha256.
			if appupdate.IsFeedNotFound(errors.New(s.errText)) && ggui.Peek(ctrl.ManualCheck) {
				m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
					au.Checking, au.Downloading, au.Err = false, false, ""
					return au
				})
				m.post(func() {
					ctrl.Dismiss()
					m.checkAppUpdateLegacy(false)
				})
				return
			}
			m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
				au.Checking, au.Downloading = false, false
				au.Err = s.errText
				return au
			})
		}
	})

	ggui.Watch(ctrl.AutoCheck, func(enabled bool) {
		if ggui.Peek(m.Settings).NoAppUpdateCheck == enabled {
			m.post(func() {
				m.UpdateSettings(func(set *store.Settings) { set.NoAppUpdateCheck = !enabled })
			})
		}
	})

	ggui.Watch(m.Settings, func(set store.Settings) {
		wantAuto := !set.NoAppUpdateCheck
		if ggui.Peek(ctrl.AutoCheck) != wantAuto {
			ctrl.ToggleAutoCheck(wantAuto)
		}
		wantSkip := appupdate.Plain(set.SkipAppVersion)
		if appupdate.Plain(u.SkippedVersion()) != wantSkip {
			if wantSkip == "" {
				_ = u.ClearSkippedVersion()
			} else {
				_ = u.SkipVersion(wantSkip)
			}
		}
	})
}

func (m *Model) resolveExternalUpdate(rel *updater.Release) *updater.ExternalUpdate {
	in := ggui.Peek(m.AppUpdate).Install
	if in.Method.SelfUpdates() {
		return nil
	}
	ver := ""
	if rel != nil {
		ver = rel.DisplayVersion()
	}
	tag := ""
	if p := appupdate.Plain(ver); p != "" {
		tag = "v" + p
	}
	page := releasePageFor(rel)
	cmd := in.CommandForVersion(ver)
	au := AppUpdate{
		Tag:     tag,
		Page:    page,
		Install: in,
		Command: cmd,
	}
	return &updater.ExternalUpdate{
		Message:     appUpdateText(au),
		Command:     cmd,
		ActionLabel: appUpdateVerb(au),
		OnAction: func() {
			if cmd != "" {
				m.Copy(cmd, appCommandHint(in.Method))
				return
			}
			m.Open(page)
		},
	}
}

func (m *Model) beforeAppUpdateInstall(rel *updater.Release, proceed func()) {
	name := ""
	if rel != nil {
		name = appupdate.Plain(rel.DisplayVersion())
	}
	if name == "" {
		name = ggui.Peek(m.AppUpdate).Name()
	}
	if n := max(m.ActiveCount(), ggui.Peek(m.TotalRunning)); n > 0 {
		if m.UpdateCtrl != nil {
			m.UpdateCtrl.Open.Set(false)
		}
		text := trn(n, "%d sandbox is running and will be stopped: sandboxes live only while the app runs.",
			"%d sandboxes are running and will be stopped: sandboxes live only while the app runs.") + " " +
			tr("GVX Desktop quits, puts %s in place, and opens again.", name)
		m.Confirm(tr("Restart to update?"), text, tr("Restart to update"), func() {
			m.StopAllForUpdate(proceed)
		})
		return
	}
	proceed()
}

func releasePageFor(rel *updater.Release) string {
	if rel != nil {
		if link := strings.TrimSpace(rel.Item.ReleaseNotesLink); link != "" {
			return link
		}
		if p := appupdate.Plain(rel.DisplayVersion()); p != "" {
			return appupdate.ReleasesURL + "/tag/v" + p
		}
	}
	return appupdate.ReleasesURL
}

// startAppUpdates removes what an earlier update left, and looks for a
// newer app a while after the app starts, and again once a day, while the
// settings let it.
func (m *Model) startAppUpdates() {
	if m.Offline {
		return
	}
	go cleanupAppUpdate(ggui.Peek(m.AppUpdate).Install)
	go func() {
		time.Sleep(appCheckDelay)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			m.post(m.checkAppUpdateDaily)
			<-t.C
		}
	}()
}

// cleanupAppUpdate removes what an update left beside the app. Windows lets
// go of the program an update replaced only once the app that ran it has
// ended, which may be a moment after this one started.
func cleanupAppUpdate(in appupdate.Install) {
	_ = appupdate.Cleanup(in)
	for i := 0; i < 15 && !appupdate.RemoveReplaced(in); i++ {
		time.Sleep(2 * time.Second)
	}
}

// checkAppUpdateDaily looks for a newer app when it has not looked for a
// day, and the settings let it.
func (m *Model) checkAppUpdateDaily() {
	if ggui.Peek(m.Settings).NoAppUpdateCheck || !appupdate.Comparable(AppVersion) {
		return
	}
	if !m.appChecked.IsZero() && time.Now().Round(0).Sub(m.appChecked) < appCheckEvery {
		return
	}
	m.CheckAppUpdate(true)
}

// CheckAppUpdate checks for the app's newest release. When the Sparkle 2
// ggui/updater controller is active, manual checks open updater.Dialog and
// quiet checks run silently in the background (falling back to the GitHub
// Releases API if appcast.xml returns HTTP 404).
func (m *Model) CheckAppUpdate(quiet bool) {
	if m.Offline {
		return
	}
	if m.UpdateCtrl != nil {
		switch ggui.Peek(m.UpdateCtrl.Phase) {
		case updater.PhaseChecking, updater.PhaseDownloading, updater.PhaseInstalling:
			if !quiet {
				m.UpdateCtrl.Open.Set(true)
			}
			return
		}
		m.appChecked = time.Now().Round(0)
		if !quiet {
			m.UpdateCtrl.CheckNow()
			return
		}
		m.checkAppUpdateSilentWithFallback()
		return
	}
	m.checkAppUpdateLegacy(quiet)
}

func (m *Model) checkAppUpdateSilentWithFallback() {
	ctrl := m.UpdateCtrl
	if ctrl == nil {
		return
	}
	u := ctrl.Updater()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rel, ok, err := u.CheckWithOptions(ctx, updater.CheckOptions{Manual: false})
		m.post(func() {
			if err != nil {
				if appupdate.IsFeedNotFound(err) {
					m.checkAppUpdateLegacy(true)
				}
				return
			}
			if !ok || rel == nil {
				m.AppUpdate.Update(func(au AppUpdate) AppUpdate {
					au.Checking, au.Err = false, ""
					if au.Tag == "" && appupdate.Comparable(AppVersion) {
						au.Tag = "v" + appupdate.Plain(AppVersion)
					}
					return au
				})
				return
			}
			switch ggui.Peek(ctrl.Phase) {
			case updater.PhaseChecking, updater.PhaseDownloading, updater.PhaseReadyToInstall, updater.PhaseInstalling:
				return
			}
			ctrl.ManualCheck.Set(false)
			ctrl.Release.Set(rel)
			ctrl.Downloaded.Set(nil)
			ctrl.Progress.Set(0)
			ctrl.ProgressText.Set("")
			ctrl.ErrText.Set("")
			ctrl.Phase.Set(updater.PhaseAvailable)
		})
	}()
}

// fetchLatestRelease asks GitHub for the latest release; tests may replace it
// so transition-fallback tests never reach the network.
var fetchLatestRelease = appupdate.Latest

// checkAppUpdateLegacy asks GitHub for the app's newest release via the
// Releases API. Used when PublicKey is not configured, on development builds,
// or before the first release publishes appcast.xml.
func (m *Model) checkAppUpdateLegacy(quiet bool) {
	if u := ggui.Peek(m.AppUpdate); u.Checking || u.Downloading || m.Offline {
		return
	}
	m.appChecked = time.Now().Round(0) // the wall clock alone
	m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Checking = true; return u })
	var rel appupdate.Release
	m.async(func(ctx context.Context) error {
		var err error
		rel, err = fetchLatestRelease(ctx)
		return err
	}, func(err error) {
		if err != nil {
			m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Checking, u.Err = false, err.Error(); return u })
			if !quiet {
				m.toastError(tr("Could not check for updates"), err)
			}
			return
		}
		m.appRelease = rel
		if s := m.appStaged; s != nil && s.Tag != rel.Tag {
			// What was downloaded is not the newest any more.
			m.appStaged = nil
			go s.Discard()
		}
		m.AppUpdate.Update(func(u AppUpdate) AppUpdate {
			u.Checking, u.Err = false, ""
			u.Tag, u.Page = rel.Tag, rel.Page
			u.Command = u.Install.Command(rel)
			u.Ready = m.appStaged != nil
			return u
		})
		if quiet {
			return
		}
		u := ggui.Peek(m.AppUpdate)
		switch {
		case !appupdate.Comparable(AppVersion):
			m.toastOK(tr("GVX Desktop %s is the newest release", u.Name()), tr("This build (%s) is not a release, and does not update itself.", AppVersion))
		case u.Available():
			if ggui.Peek(m.Settings).SkipAppVersion == u.Tag {
				m.UpdateSettings(func(s *store.Settings) { s.SkipAppVersion = "" })
			}
			m.Toasts.Push(ui.Toast(tr("GVX Desktop %s is available", u.Name()), tr("This is version %s.", AppVersion)).Success().
				Duration(10*time.Second).Action(appUpdateVerb(u), m.UpdateApp))
		default:
			m.toastOK(tr("GVX Desktop is up to date"), tr("%s is the newest version.", AppVersion))
		}
	})
}

// UpdateApp does what updates the app as it was installed: it opens the
// updater dialog / downloads the new version, or once that is done asks to
// restart into it; copies the command a package manager updates it with; or
// opens the release page.
func (m *Model) UpdateApp() {
	u := ggui.Peek(m.AppUpdate)
	if u.Checking || u.Downloading || !u.Available() {
		return
	}
	if m.UpdateCtrl != nil && ggui.Peek(m.UpdateCtrl.Release) != nil && u.Install.Method.SelfUpdates() {
		switch ggui.Peek(m.UpdateCtrl.Phase) {
		case updater.PhaseReadyToInstall:
			m.UpdateCtrl.Install()
		default:
			m.UpdateCtrl.Open.Set(true)
		}
		return
	}
	switch {
	case u.Install.Method.SelfUpdates() && u.Ready:
		m.AskRestartToUpdate()
	case u.Install.Method.SelfUpdates():
		m.downloadAppUpdate()
	case u.Command != "":
		m.Copy(u.Command, appCommandHint(u.Install.Method))
	default:
		m.Open(u.page())
	}
}

// downloadAppUpdate downloads the newest release beside the app, checked
// against its checksum, for a restart to put in place.
func (m *Model) downloadAppUpdate() {
	u := ggui.Peek(m.AppUpdate)
	rel, in, name := m.appRelease, u.Install, u.Name()
	m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Downloading, u.Percent, u.Ready = true, 0, false; return u })
	title := tr("Downloading GVX Desktop %s", name)
	toast := m.Toasts.Push(ui.Toast(title, tr("It goes in place when the app restarts.")).Loading())
	var staged *appupdate.Staged
	work := func(ctx context.Context) error {
		shown := -1
		var err error
		staged, err = appupdate.Stage(ctx, rel, in, func(done, total int64) {
			if total <= 0 {
				return
			}
			if p := int(done * 100 / total); p != shown {
				shown = p
				m.post(func() {
					m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Percent = p; return u })
					m.Toasts.Update(toast, ui.Toast(title, tr("%d%% downloaded. It goes in place when the app restarts.", p)).Loading())
				})
			}
		})
		return err
	}
	done := func(err error) {
		m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Downloading, u.Percent, u.Ready = false, 0, err == nil; return u })
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Could not update GVX Desktop"), err.Error()).Error().Duration(10*time.Second))
			return
		}
		m.appStaged = staged
		m.Toasts.Update(toast, ui.Toast(tr("GVX Desktop %s is ready", name), tr("Restart the app to finish the update.")).Success().
			Duration(12*time.Second).Action(tr("Restart"), m.AskRestartToUpdate))
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), appDownloadTimeout)
		defer cancel()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

// AskRestartToUpdate asks before the app quits to put the downloaded
// update in place, saying what that does to the sandboxes running.
func (m *Model) AskRestartToUpdate() {
	u := ggui.Peek(m.AppUpdate)
	if !u.Ready {
		return
	}
	if m.UpdateCtrl != nil && ggui.Peek(m.UpdateCtrl.Downloaded) != nil {
		m.UpdateCtrl.Install()
		return
	}
	text := tr("GVX Desktop quits, puts %s in place, and opens again.", u.Name())
	if n := max(m.ActiveCount(), ggui.Peek(m.TotalRunning)); n > 0 {
		text = trn(n, "%d sandbox is running and will be stopped: sandboxes live only while the app runs.",
			"%d sandboxes are running and will be stopped: sandboxes live only while the app runs.") + " " + text
		m.Confirm(tr("Restart to update?"), text, tr("Restart to update"), m.restartToUpdate)
		return
	}
	m.Offer(tr("Restart to update?"), text, tr("Restart to update"), m.restartToUpdate)
}

// restartToUpdate readies the update to go in, and quits as the quit
// dialog does, stopping the sandboxes first; main finishes the update
// once the app has ended, with FinishUpdate.
func (m *Model) restartToUpdate() {
	s := m.appStaged
	if s == nil {
		return
	}
	args := relaunchArgs(os.Args[1:])
	m.async(func(context.Context) error { return s.Prepare(args) }, func(err error) {
		if err != nil {
			m.toastError(tr("Could not put the update in place"), err)
			return
		}
		m.appStaged, m.appRestart = nil, s
		if m.ActiveCount() > 0 {
			m.StopAllAndThen(func() { quitApp() })
			return
		}
		quitApp()
	})
}

// FinishUpdate starts the updated app when the user restarted to update,
// and otherwise removes an update downloaded and not put in place. main
// calls it last, once the app has ended everything it ran.
func (m *Model) FinishUpdate() error {
	if s := m.appRestart; s != nil {
		return s.Finish()
	}
	if s := m.appStaged; s != nil {
		return s.Discard()
	}
	return nil
}

// SkipAppVersion stops the app from telling of its newest release, until
// a newer one comes or the user checks for updates.
func (m *Model) SkipAppVersion() {
	u := ggui.Peek(m.AppUpdate)
	if u.Tag == "" {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.SkipAppVersion = u.Tag })
	if m.UpdateCtrl != nil {
		m.UpdateCtrl.Skip()
	}
	m.Toasts.Push(ui.Toast(tr("Skipped GVX Desktop %s", u.Name()), tr("Check for updates in Settings to see it again.")).Info())
}

// relaunchArgs are the arguments the updated app starts with: this run's,
// but for the flag a start at login passes, as the user is there.
func relaunchArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); strings.HasPrefix(a, "-") && name == HiddenFlag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// appUpdateVerb is what the button that updates the app says.
func appUpdateVerb(u AppUpdate) string {
	switch {
	case u.Install.Method.SelfUpdates() && u.Ready:
		return tr("Restart to update")
	case u.Install.Method.SelfUpdates():
		return tr("Update")
	case u.Command != "":
		return tr("Copy command")
	}
	return tr("Open the release page")
}

// appCommandHint says where to run the command a package manager updates
// the app with.
func appCommandHint(m appupdate.Method) string {
	switch m {
	case appupdate.Homebrew:
		return tr("Run it in a terminal, then open the app again.")
	case appupdate.Chocolatey:
		return tr("Quit the app, then run it in a terminal opened as administrator.")
	}
	return tr("Run it in a terminal, then open the app again.")
}

// appUpdateText says how the newest release gets onto this computer, as
// the app was installed.
func appUpdateText(u AppUpdate) string {
	this := tr("This is version %s.", AppVersion) + " "
	switch u.Install.Method {
	case appupdate.Bundle, appupdate.Program:
		if u.Ready {
			return this + tr("The update is downloaded: it goes in place when the app restarts.")
		}
		return this + tr("It downloads now and goes in place when the app restarts.")
	case appupdate.Homebrew:
		return this + tr("Homebrew installed this copy: run the command in a terminal, then open the app again.")
	case appupdate.Chocolatey:
		return this + tr("Chocolatey installed this copy: quit the app and update it from a terminal opened as administrator.")
	case appupdate.Deb, appupdate.RPM:
		if u.Command == "" {
			return this + tr("The system's package manager installed this copy: install the new package from the release page.")
		}
		return this + tr("The system's package manager installed this copy: the command downloads the new package, checks it and installs it.")
	}
	return this + tr("This copy cannot replace itself where it is: download the new version from the release page.")
}
