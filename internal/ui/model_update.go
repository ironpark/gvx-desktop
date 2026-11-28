package ui

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// Test hooks for rungv release lookup and installation.
var (
	fetchRungvReleases      = rungv.Releases
	fetchRungvLatest        = rungv.Latest
	installRungvRelease     = rungv.Install
	installRungvReleaseOpts = rungv.InstallWithOptions
	installRungvGPURenderer = rungv.InstallGPURenderer
	signRungvBinary         = rungv.Sign
	gpuExtensionSupported   = rungv.HostSupportsGPUExtension
)

func releaseEqual(a, b rungv.Release) bool {
	return a.Tag == b.Tag && a.Page == b.Page && a.Prerelease == b.Prerelease && len(a.Assets) == len(b.Assets)
}

// The channels rungv is updated from: the releases of gvx, or the head of
// its main branch, built here from source.
const (
	ChannelRelease = "release"
	ChannelSource  = "source"
)

// buildTimeout bounds a build from source, which compiles gVisor the first
// time and takes minutes.
const buildTimeout = 45 * time.Minute

// Update is what the app knows of the newest gvx on its channel, and of
// the one it is installing.
type Update struct {
	Source bool // the channel: built from source, not a release

	Tag  string // the newest release, v0.17.1…; empty until one is known
	Page string // where its notes or commits are

	Repo    string // display name of the source repository
	Branch  string // branch or tag built from source; empty means rungv.SourceBranch
	Commit  string // the head of Branch; empty until it is known
	Subject string // the first line of its message
	Ahead   int    // commits Branch is past the rungv in use; -1 when unknown

	Checking      bool
	Err           string // why the last check failed
	Installing    bool
	InstallingTag string // release tag currently being installed, when chosen explicitly
	Percent       int    // of the download, while installing a release
	Phase         string // what a build from source is doing
}

// BranchName is the branch or tag name of a source update.
func (u Update) BranchName() string {
	if u.Branch != "" {
		return u.Branch
	}
	return rungv.SourceBranch
}

// Known reports that the newest gvx of the channel has been looked up.
func (u Update) Known() bool {
	if u.Source {
		return u.Commit != ""
	}
	return u.Tag != ""
}

// Name is what the newest gvx of the channel is called.
func (u Update) Name() string {
	if u.Source {
		return commitName(u.BranchName(), u.Commit)
	}
	return shortVersion(u.Tag)
}

// InstallName is the version currently being installed (a chosen release tag
// when set, otherwise the channel's newest).
func (u Update) InstallName() string {
	if !u.Source && u.InstallingTag != "" {
		return shortVersion(u.InstallingTag)
	}
	return u.Name()
}

// commitName names a build from source by its branch and commit.
func commitName(branch, sha string) string {
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if branch == "" {
		branch = rungv.SourceBranch
	}
	return branch + "@" + sha
}

// shortCommit trims a commit hash to seven characters.
func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Available reports that the rungv of e is not the channel's newest: the
// channel is what the rungv in use is measured against, so that changing
// it offers the other channel's newest, be that an older one, as the last
// release is to a build of main.
func (u Update) Available(e Engine) bool {
	if !e.Checked || e.Path == "" || !u.Known() {
		return false
	}
	if u.Source {
		return e.Version.Commit != u.Commit
	}
	return e.Version.Version != u.Tag
}

// Newer reports that the channel's newest is past the rungv of e, rather
// than only other than it.
func (u Update) Newer(e Engine) bool {
	if u.Source {
		return u.Ahead != 0
	}
	return rungv.Newer(u.Tag, e.Version.Version)
}

// Channel is the channel the settings choose.
func (m *Model) Channel() string {
	if ggui.Peek(m.Settings).UpdateChannel == ChannelSource {
		return ChannelSource
	}
	return ChannelRelease
}

// Source is the git repository and branch configured for building rungv
// from source.
func (m *Model) Source() rungv.Source {
	s := ggui.Peek(m.Settings)
	return rungv.Source{Repo: s.SourceRepo, Branch: s.SourceBranch}
}

// SetSource updates the git repository and branch built from source, forgets
// what was known of the previous one, and looks the new head up when on the
// source channel.
func (m *Model) SetSource(repo, branch string) {
	if ggui.Peek(m.Update).Installing {
		return
	}
	repo = strings.TrimSpace(repo)
	if strings.TrimSuffix(repo, ".git") == rungv.SourceURL || repo == "gfire-sigs/gvx" {
		repo = ""
	}
	branch = strings.TrimSpace(branch)
	if branch == rungv.SourceBranch {
		branch = ""
	}
	cur := ggui.Peek(m.Settings)
	if repo == cur.SourceRepo && branch == cur.SourceBranch {
		return
	}
	m.UpdateSettings(func(s *storeSettings) {
		s.SourceRepo = repo
		s.SourceBranch = branch
	})
	if m.Channel() == ChannelSource {
		src := m.Source()
		m.Update.Set(Update{Source: true, Repo: src.DisplayRepo(), Branch: src.BranchName()})
		m.BuildLog.Set("")
		m.CheckUpdate(true)
	}
}

// SetChannel changes the channel, forgets what was known of the other, and
// looks the new one up.
func (m *Model) SetChannel(channel string) {
	if channel == m.Channel() || ggui.Peek(m.Update).Installing {
		return
	}
	m.UpdateSettings(func(s *storeSettings) { s.UpdateChannel = channel })
	src := m.Source()
	m.Update.Set(Update{Source: channel == ChannelSource, Repo: src.DisplayRepo(), Branch: src.BranchName()})
	m.SelectedRelease.Set("")
	m.BuildLog.Set("")
	m.CheckUpdate(true)
}

// SetSelectedRelease picks a specific published release tag to install in
// Settings (empty string follows the newest release).
func (m *Model) SetSelectedRelease(tag string) {
	if ggui.Peek(m.Update).Installing {
		return
	}
	if tag != "" {
		if _, ok := m.releaseByTag(tag); !ok {
			return
		}
	}
	m.SelectedRelease.Set(tag)
}

func (m *Model) releaseByTag(tag string) (rungv.Release, bool) {
	for _, r := range ggui.Peek(m.Releases) {
		if r.Tag == tag {
			return r, true
		}
	}
	return rungv.Release{}, false
}

// CheckUpdate asks for the newest gvx of the channel. A quiet check, as
// the one at launch, says nothing of its own; one the user asked for tells
// them what it found.
func (m *Model) CheckUpdate(quiet bool) {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing || m.Offline {
		return
	}
	source := m.Channel() == ChannelSource
	src := m.Source()
	installed := ggui.Peek(m.Engine).Version.Commit
	m.Update.Update(func(u Update) Update {
		u.Source, u.Checking, u.Repo, u.Branch = source, true, src.DisplayRepo(), src.BranchName()
		return u
	})
	var rel rungv.Release
	var rels []rungv.Release
	var head rungv.Head
	m.async(func(ctx context.Context) error {
		var err error
		if source {
			head, err = rungv.SourceHead(ctx, src, installed)
			return err
		}
		rels, err = fetchRungvReleases(ctx)
		if err == nil && len(rels) > 0 {
			rel = newestRelease(rels)
			return nil
		}
		latest, lerr := fetchRungvLatest(ctx)
		if lerr != nil {
			if err != nil {
				return err
			}
			return lerr
		}
		rel = latest
		rels = []rungv.Release{latest}
		return nil
	}, func(err error) {
		if source != (m.Channel() == ChannelSource) || (source && m.Source() != src) {
			return // the channel or source changed while this was asked
		}
		if err != nil {
			m.Update.Update(func(u Update) Update { u.Checking, u.Err = false, err.Error(); return u })
			if !quiet {
				m.toastError(tr("Could not check for a new rungvx"), err)
			}
			return
		}
		u := Update{
			Source:  source,
			Tag:     rel.Tag,
			Page:    rel.Page,
			Repo:    src.DisplayRepo(),
			Branch:  src.BranchName(),
			Commit:  head.SHA,
			Subject: head.Subject,
			Ahead:   head.Ahead,
		}
		if source {
			u.Page = src.CommitsURL()
		} else {
			m.release = rel
			m.Releases.Set(rels)
			if sel := ggui.Peek(m.SelectedRelease); sel != "" {
				if !slices.ContainsFunc(rels, func(r rungv.Release) bool { return r.Tag == sel }) {
					m.SelectedRelease.Set("")
				}
			}
		}
		m.Update.Set(u)
		if quiet {
			return
		}
		switch e := ggui.Peek(m.Engine); {
		case e.Path == "":
			m.toastOK(tr("rungvx %s can be installed", u.Name()), "")
		case u.Available(e):
			m.toastOK(updateTitle(u, e), tr("This computer has %s.", engineVersion(e)))
		default:
			m.toastOK(tr("rungvx is up to date"), engineVersion(e))
		}
	})
}

// newestRelease picks the first non-prerelease entry in rels, or rels[0] if
// every listed release is a prerelease.
func newestRelease(rels []rungv.Release) rungv.Release {
	for _, r := range rels {
		if !r.Prerelease {
			return r
		}
	}
	return rels[0]
}

// InstallRungv installs the newest rungv of the channel — a release
// downloaded, or built from source — in place of the one in use, or
// in ~/.gvx/bin when there is none or it is not this app's to replace.
// A rungvx in ~/.gvx/bin also gets that name there (linkRungvx).
// Sandboxes that are running keep the rungv they started with.
func (m *Model) InstallRungv() {
	m.installRungv(m.Channel() == ChannelSource, "")
}

// InstallRungvRelease installs a specific published release tag (or the newest
// release when tag is empty).
func (m *Model) InstallRungvRelease(tag string) {
	m.installRungv(false, tag)
}

func (m *Model) installRungv(source bool, tag string) {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing {
		return
	}
	var rel rungv.Release
	if !source {
		if tag != "" {
			var ok bool
			if rel, ok = m.releaseByTag(tag); !ok {
				return
			}
		} else {
			rel = m.release
		}
	}
	src := m.Source()
	m.Update.Update(func(u Update) Update {
		u.Source, u.Repo, u.Branch = source, src.DisplayRepo(), src.BranchName()
		u.Installing, u.InstallingTag, u.Percent, u.Phase, u.Err = true, rel.Tag, 0, "", ""
		return u
	})
	current := ggui.Peek(m.Engine).Path
	wantGPU := !source && ggui.Peek(m.Settings).RungvGPU && gpuExtensionSupported()
	recentRels := slices.Clone(ggui.Peek(m.Releases))
	var (
		target               string
		built                string
		usedExt              bool
		rendererTag          string
		rendererReused       bool
		rendererFromFallback bool
		rendererMissing      bool
		rendererErr          error
	)
	work := func(ctx context.Context) error {
		var err error
		if target, _, err = rungv.InstallTarget(current); err != nil {
			return err
		}
		if source {
			built, err = m.buildFromSource(ctx, src, target)
		} else {
			if rel.Tag == "" {
				if rel, err = fetchRungvLatest(ctx); err != nil {
					return err
				}
				m.post(func() { m.Update.Update(func(u Update) Update { u.InstallingTag = rel.Tag; return u }) })
			}
			shown := -1
			onProgress := func(done, total int64) {
				if total <= 0 {
					return
				}
				if p := int(done * 100 / total); p != shown {
					shown = p
					m.post(func() { m.Update.Update(func(u Update) Update { u.Percent = p; return u }) })
				}
			}
			if wantGPU {
				usedExt, err = installRungvReleaseOpts(ctx, rel, target, rungv.InstallOptions{PreferExt: true}, onProgress)
			} else {
				err = installRungvRelease(ctx, rel, target, onProgress)
			}
		}
		if err != nil {
			return err
		}
		if runtime.GOOS == "darwin" {
			// A rungv without the entitlement cannot reach the hypervisor.
			if err := signRungvBinary(ctx, target); err != nil {
				return errors.New(tr("rungvx was installed, but signing it failed: ") + err.Error())
			}
		}
		if wantGPU && usedExt {
			installRenderer := func(cand rungv.Release, phase string) error {
				shown := -1
				m.post(func() {
					m.Update.Update(func(u Update) Update { u.Phase, u.Percent = phase, 0; return u })
				})
				_, rerr := installRungvGPURenderer(ctx, cand, target, func(done, total int64) {
					if total <= 0 {
						return
					}
					if p := int(done * 100 / total); p != shown {
						shown = p
						m.post(func() { m.Update.Update(func(u Update) Update { u.Percent = p; return u }) })
					}
				})
				return rerr
			}

			if rungv.HasGPURenderer(rel) {
				if rerr := installRenderer(rel, tr("Installing GPU renderer…")); rerr != nil {
					if errors.Is(rerr, context.Canceled) {
						return rerr
					}
					rendererErr = rerr
				} else {
					rendererTag = rel.Tag
				}
			} else {
				// Not every gvx release publishes gvx-gpu-renderer. Preserve an
				// already-installed renderer unless a newer release carries one,
				// or fall back to the newest release that publishes it.
				if len(recentRels) == 0 {
					if fetched, ferr := fetchRungvReleases(ctx); ferr == nil {
						recentRels = fetched
					}
				}
				cand, found := rungv.FindGPURendererRelease(rel, recentRels)
				existing := rungv.LocateGPURenderer(target)
				existingVer := rungv.GPURendererVersion(target)
				switch {
				case existing != "" && (!found || existingVer == "" || !rungv.Newer(cand.Tag, existingVer)):
					rendererReused = true
					rendererTag = existingVer
				case found:
					if rerr := installRenderer(cand, tr("Installing GPU renderer…")); rerr != nil {
						if errors.Is(rerr, context.Canceled) {
							return rerr
						}
						if existing != "" {
							rendererReused = true
							rendererTag = existingVer
						} else {
							rendererErr = rerr
						}
					} else {
						rendererTag = cand.Tag
						rendererFromFallback = true
					}
				default:
					rendererMissing = true
				}
			}
		}
		// A link that cannot be made leaves the install as it is: Add to
		// PATH makes it again and says why.
		_ = linkRungvx(ctx)
		return nil
	}
	done := func(err error) {
		m.Update.Update(func(u Update) Update {
			u.Installing, u.InstallingTag, u.Percent, u.Phase = false, "", 0, ""
			switch {
			case source && built != "":
				u.Commit, u.Ahead, u.Repo, u.Branch, u.Page = built, 0, src.DisplayRepo(), src.BranchName(), src.CommitsURL()
			case !source && rel.Tag != "" && (tag == "" || u.Tag == ""):
				u.Tag, u.Page = rel.Tag, rel.Page
			}
			return u
		})
		if !source && rel.Tag != "" && (tag == "" || m.release.Tag == "") {
			m.release = rel
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				m.Toasts.Push(ui.Toast(tr("Build canceled"), tr("The rungvx installation was canceled.")).Info())
				return
			}
			if errors.Is(err, rungv.ErrNoToolchain) {
				err = errors.New(tr("Building from source needs git and Go on this computer: ") + err.Error())
			}
			m.toastError(tr("Could not install rungvx"), err)
			if target != "" {
				m.DetectEngine(nil)
			}
			return
		}
		// Another rungv found before this one would hide it: name it.
		set := ggui.Peek(m.Settings)
		if found, _ := rungv.Locate(set.RungvPath); found != target {
			m.UpdateSettings(func(s *storeSettings) { s.RungvPath = target })
		}
		name := shortVersion(rel.Tag)
		if source {
			name = commitName(src.BranchName(), built)
			m.BuildLog.Set("")
		}
		switch {
		case wantGPU && !usedExt:
			m.toastOK(tr("rungvx %s installed", name), tr("This release has no GPU extension build; installed standard rungvx."))
		case wantGPU && usedExt && rendererErr != nil:
			m.Toasts.Push(ui.Toast(tr("rungvx %s installed (GPU renderer failed)", name), rendererErr.Error()).Warning())
		case wantGPU && usedExt && rendererMissing:
			m.Toasts.Push(ui.Toast(tr("rungvx %s installed (GPU)", name), tr("No recent release includes gvx-gpu-renderer; install it manually to use -gpu.")).Warning())
		case wantGPU && usedExt && rendererReused && rendererTag != "":
			m.toastOK(tr("rungvx %s installed (GPU)", name), tr("Kept existing gvx-gpu-renderer (%s); this release does not include one.", shortVersion(rendererTag)))
		case wantGPU && usedExt && rendererReused:
			m.toastOK(tr("rungvx %s installed (GPU)", name), tr("Kept existing gvx-gpu-renderer; this release does not include one."))
		case wantGPU && usedExt && rendererFromFallback:
			m.toastOK(tr("rungvx %s installed (GPU)", name), tr("Installed gvx-gpu-renderer from %s (not included in %s).", shortVersion(rendererTag), name))
		case wantGPU && usedExt:
			m.toastOK(tr("rungvx %s installed (GPU)", name), tr("Installed with gvx-gpu-renderer in %s.", tildePath(rungv.GPURendererDir(target))))
		default:
			m.toastOK(tr("rungvx %s installed", name), target)
		}
		m.DetectEngine(nil)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		m.buildCancelMu.Lock()
		m.buildCancel = cancel
		m.buildCancelMu.Unlock()
		defer func() {
			m.buildCancelMu.Lock()
			m.buildCancel = nil
			m.buildCancelMu.Unlock()
			cancel()
		}()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

// CancelInstallRungv aborts an in-progress rungv download or source build.
func (m *Model) CancelInstallRungv() {
	m.buildCancelMu.Lock()
	cancel := m.buildCancel
	m.buildCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// buildFromSource builds src into target, telling the screen what it is
// doing and keeping the end of what git and Go print in BuildLog.
func (m *Model) buildFromSource(ctx context.Context, src rungv.Source, target string) (string, error) {
	var tail []string
	var last time.Time
	flush := func() {
		text := strings.Join(tail, "\n")
		m.post(func() { m.BuildLog.Set(text) })
	}
	phase := func(p rungv.Phase) {
		text := ""
		switch p {
		case rungv.PhaseFetch:
			text = tr("Fetching the source…")
		case rungv.PhaseBuild:
			text = tr("Building… this takes minutes")
		case rungv.PhaseInstall:
			text = tr("Installing…")
		}
		m.post(func() { m.Update.Update(func(u Update) Update { u.Phase = text; return u }) })
	}
	line := func(s string) {
		if strings.TrimSpace(s) == "" {
			return
		}
		tail = append(tail, s)
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		// git prints progress many times a second; the screen need not.
		if now := time.Now(); now.Sub(last) > 150*time.Millisecond {
			last = now
			flush()
		}
	}
	commit, err := rungv.BuildFromSource(ctx, src, filepath.Join(m.store.Dir, "gvx-src"), target, phase, line)
	if err != nil {
		tail = append(tail, "error: "+err.Error())
	}
	flush()
	return commit, err
}

// isSourceBuild reports a version as Go gives a build between tags, which
// ends in the time and the hash of its commit: a build from source, not a
// release.
func isSourceBuild(version string) bool {
	i := strings.LastIndexByte(version, '-')
	return i > 0 && len(version)-i-1 == 12 && strings.Count(version, "-") >= 2
}

// engineVersion is the version of the rungv in use as people say it: a
// release's without its build stamp, a build from source by its commit.
func engineVersion(e Engine) string {
	if isSourceBuild(e.Version.Version) && e.Version.Commit != "" {
		return shortCommit(e.Version.Commit)
	}
	return shortVersion(e.Version.Version)
}
