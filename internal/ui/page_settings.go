package ui

import (
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// settingRow is one setting: what it is and what it does at the left, its
// control at the right.
func settingRow(title, help string, control ggui.Widget) ggui.Widget {
	text := []ggui.Widget{widget.Text(title).Size(14).Weight(ggui.WeightMedium)}
	if help != "" {
		text = append(text, ui.Caption(help))
	}
	return widget.Row(widget.Expanded(widget.Column(text...).Gap(2)), control).Gap(24).Align(widget.AlignCenter)
}

// settingField is a setting whose control wants the width of the card.
func settingField(title, help string, control ggui.Widget) ggui.Widget {
	return widget.Column(
		widget.Text(title).Size(14).Weight(ggui.WeightMedium),
		control,
		ui.Caption(help),
	).Gap(6).Stretch()
}

// settingsGroup is a titled card of settings with a rule between each two.
func settingsGroup(icon *icons.Glyph, title string, rows ...ggui.Widget) ggui.Widget {
	body := make([]ggui.Widget, 0, 2*len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		if len(body) > 0 {
			body = append(body, ui.Divider())
		}
		body = append(body, r)
	}
	return widget.Column(
		widget.Row(glyph(icon, 15).Color(colorGray), ui.Heading(title).Size(14)).Gap(8).Align(widget.AlignCenter),
		ui.Card(widget.Column(body...).Gap(14).Stretch()),
	).Gap(10).Stretch()
}

// trayPlace is where the system keeps the app's tray icon.
func trayPlace() string {
	if runtime.GOOS == "darwin" {
		return tr("menu bar")
	}
	return tr("notification area")
}

func settingsPage(m *Model) ggui.Widget {
	set := ggui.Peek(m.Settings)
	path := ggui.State(set.RungvPath)
	cache := ggui.State(set.CacheDir)
	volume := ggui.State(set.VolumeDir)
	ggui.Watch(m.Settings, func(s storeSettings) {
		if ggui.Peek(path) != s.RungvPath {
			path.Set(s.RungvPath)
		}
		if ggui.Peek(cache) != s.CacheDir {
			cache.Set(s.CacheDir)
		}
		if ggui.Peek(volume) != s.VolumeDir {
			volume.Set(s.VolumeDir)
		}
	})
	flag := func(get func(storeSettings) bool, put func(*storeSettings, bool)) ggui.Binding[bool] {
		return ggui.Bind(func() bool { return get(m.Settings.Get()) }, func(v bool) { m.UpdateSettings(func(s *storeSettings) { put(s, v) }) })
	}
	theme := ggui.Bind(func() string { return m.Settings.Get().Theme }, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.Theme = v }) })
	language := ggui.Bind(func() string {
		if l := m.Settings.Get().Language; l != LangSystem {
			if _, ok := languageOf(l); ok {
				return l
			}
		}
		return LangSystem
	}, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.Language = v }) })
	tray := flag(func(s storeSettings) bool { return !s.NoTray }, func(s *storeSettings, v bool) { s.NoTray = !v })
	hideOnClose := flag(func(s storeSettings) bool { return s.HideOnClose }, func(s *storeSettings, v bool) { s.HideOnClose = v })
	stopOnQuit := flag(func(s storeSettings) bool { return s.StopOnQuit }, func(s *storeSettings, v bool) { s.StopOnQuit = v })
	checkUpdates := flag(func(s storeSettings) bool { return !s.NoUpdateCheck }, func(s *storeSettings, v bool) { s.NoUpdateCheck = !v })
	timeout := ggui.Bind(func() float64 { return float64(m.Settings.Get().StopTimeout) }, func(v float64) {
		m.UpdateSettings(func(s *storeSettings) { s.StopTimeout = max(1, int(v)) })
	})
	channel := ggui.Bind(func() string {
		if m.Settings.Get().UpdateChannel == ChannelSource {
			return ChannelSource
		}
		return ChannelRelease
	}, m.SetChannel)
	noTray := ggui.Map(m.Settings, func(s storeSettings) bool { return s.NoTray })

	groups := []ggui.Widget{
		headed(pageHeader(tr("Settings"), ui.Caption(tr("Where rungvx lives and how the app treats running sandboxes."))), engineAlertFor(m, true)),
		settingsGroup(lucide.Palette, tr("General"),
			settingRow(tr("Appearance"), tr("Light, dark, or as the system is."),
				ui.ToggleGroup[string](theme).Options([]string{"system", "light", "dark"}).Format(themeName).Icon(themeIcon).Outline().Spacing(0).Name(tr("Appearance"))),
			settingRow(tr("Language"), tr("The language the app is worded in."),
				ui.Select(language).Options(append([]string{LangSystem}, LanguageTags()...)).Format(languageName).MinWidth(180).Name(tr("Language"))),
			settingRow(tr("Start at login"),
				tr("Starts the app when you log in, in the tray without its window when the tray icon is shown. Sandboxes set to start with the app start then."),
				ui.Switch(ggui.Bind(m.Autostart.Get, m.SetAutostart), "").Name(tr("Start at login"))),
		),
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		trayTitle := tr("Show GVX Desktop in the %s", trayPlace())
		groups = append(groups, settingsGroup(lucide.PanelTop, tr("Tray"),
			settingRow(trayTitle,
				tr("An icon with the sandboxes and what they are doing, to start and stop them without the window."),
				ui.Switch(tray, "").Name(trayTitle)),
			settingRow(tr("Keep running when the window closes"),
				tr("Closing the window leaves the app and its sandboxes running behind the icon. Without the icon, closing the window quits."),
				ui.Switch(hideOnClose, "").Name(tr("Keep running when the window closes")).BindDisabled(noTray)),
		))
	}
	groups = append(groups,
		settingsGroup(lucide.Box, tr("Sandboxes"),
			settingRow(tr("Stop running sandboxes on quit without asking"),
				tr("gvx has no daemon: sandboxes run as long as this app does."),
				ui.Switch(stopOnQuit, "").Name(tr("Stop running sandboxes on quit without asking"))),
			settingRow(tr("Stop timeout (seconds)"),
				tr("How long a sandbox gets to exit after an interrupt before it is killed."),
				widget.Box(ui.NumberInput(timeout).Range(1, 300).Step(1).Precision(0).Name(tr("Stop timeout"))).Width(120)),
		),
		dockerSettings(m),
		kubeSettings(m),
		localRegistrySettings(m),
		rungvSettings(m, path, cache, volume),
		appUpdateSettings(m),
		settingsGroup(lucide.Download, tr("rungvx updates"),
			widget.Column(
				settingRow(tr("Channel"),
					tr("Releases are gvx's published builds. From source clones a git repository and builds rungvx on this computer, which needs git and Go."),
					ui.ToggleGroup[string](channel).Options([]string{ChannelRelease, ChannelSource}).Format(channelName).Outline().Spacing(0).
						BindDisabled(ggui.Map(m.Update, func(u Update) bool { return u.Installing })).Name(tr("Channel"))),
				sourceFields(m),
				releaseVersionField(m),
				gpuExtensionField(m),
			).Gap(12).Stretch(),
			releaseRow(m),
			settingRow(tr("Look for a newer rungvx when the app starts"),
				tr("Asks GitHub for the newest release of gvx. Nothing is installed until you say so."),
				ui.Switch(checkUpdates, "").Name(tr("Look for a newer rungvx when the app starts"))),
		),
		aboutCard(m),
	)
	return page(groups...)
}

// rungvSettings configures the rungv binary path, layer cache, volume
// directory, terminal PATH registration, and detected runtime details.
func rungvSettings(m *Model, path, cache, volume *ggui.StateValue[string]) ggui.Widget {
	pathField := settingField(tr("rungvx path"), tr("Leave empty to search $GVX_RUNGVX, $GVX_RUNGV, PATH, ~/.gvx/bin and the app's own folder."), widget.Row(
		widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, ~/.gvx/bin, next to the app)")).Name(tr("rungvx path")).
			OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = strings.TrimSpace(v) }) })),
		ui.Button(tr("Browse…"), func() {
			browseFile(m, path, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = v }) })
		}).Outline(),
	).Gap(8))
	cacheField := settingField(tr("Layer cache directory"), tr("Passed as -cache-dir. Named volumes default to its volumes folder unless a volume directory is set (rungvx v0.24+)."), widget.Row(
		widget.Expanded(ui.TextField(cache).Placeholder(tr("rungvx's default")).Name(tr("Layer cache directory")).
			OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = strings.TrimSpace(v) }) })),
		ui.Button(tr("Browse…"), func() {
			browseFolder(m, cache, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = v }) })
		}).Outline(),
	).Gap(8))
	volumeField := settingField(tr("Volume directory"), tr("Passed as -volume-dir on rungvx v0.24+ (works alongside a custom layer cache directory), or as GVX_VOLUME_DIR on earlier builds. Keep it out of cache folders: macOS empties ~/Library/Caches when the disk runs low."), widget.Row(
		widget.Expanded(ui.TextField(volume).Placeholder(tr("rungvx's default")).Name(tr("Volume directory")).
			OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.VolumeDir = strings.TrimSpace(v) }) })),
		ui.Button(tr("Browse…"), func() {
			browseFolder(m, volume, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.VolumeDir = v }) })
		}).Outline(),
	).Gap(8))
	facts := engineFacts(m)
	showPath := ggui.Map(m.RungvPath, func(s RungvPathState) bool {
		return s.EnginePath != "" || s.BinInPATH
	})
	return ggui.View(showPath, func(visible bool) ggui.Widget {
		rows := []ggui.Widget{pathField, cacheField, volumeField}
		if visible {
			rows = append(rows, rungvPathSettings(m))
		}
		rows = append(rows, facts)
		return settingsGroup(lucide.Cpu, "rungvx", rows...)
	})
}

// rungvPathSettings shows whether rungv is reachable in terminals and lets the
// user register or remove ~/.gvx/bin on their terminal PATH.
func rungvPathSettings(m *Model) ggui.Widget {
	return ggui.View(m.RungvPath, func(s RungvPathState) ggui.Widget {
		if s.EnginePath == "" && !s.BinInPATH {
			return widget.Box()
		}
		bin := tildePath(s.BinDir)
		prof := tildePath(s.Profile)
		var help string
		var controls []ggui.Widget

		switch {
		case s.EnginePath == "" && s.BinInPATH:
			help = tr("%s is on your terminal PATH (%s).", bin, prof)
			if s.Managed {
				controls = append(controls, ui.Button(tr("Remove from PATH"), m.RemoveRungvFromPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
			}
		case s.BinInPATH && s.InBinDir:
			if s.PATHBinary != "" && !s.OnPATH {
				help = tr("Available via %s (%s). Another rungvx is earlier on PATH (%s).", bin, prof, tildePath(s.PATHBinary))
			} else if prof != "" {
				help = tr("Available in terminals via %s (%s).", bin, prof)
			} else {
				help = tr("Available in terminals via %s.", bin)
			}
			if s.Managed {
				controls = append(controls, ui.Button(tr("Remove from PATH"), m.RemoveRungvFromPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
			}
		case s.OnPATH:
			help = tr("Available in terminals via %s.", tildePath(s.PATHBinary))
			if s.BinInPATH && s.Managed {
				controls = append(controls, ui.Button(tr("Remove from PATH"), m.RemoveRungvFromPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
			}
		case s.BinInPATH && !s.InBinDir:
			help = tr("%s is already on your terminal PATH. Link this rungvx binary there to run it from terminals.", bin)
			controls = append(controls, ui.Button(tr("Link to ~/.gvx/bin"), m.AddRungvToPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
			if s.Managed {
				controls = append(controls, ui.Button(tr("Remove from PATH"), m.RemoveRungvFromPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
			}
		case s.InBinDir:
			help = tr("Add %s to %s to run rungvx from any new terminal.", bin, prof)
			controls = append(controls, ui.Button(tr("Add to PATH"), m.AddRungvToPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
		default:
			help = tr("Link rungvx into %s and add it to %s to run it from any new terminal.", bin, prof)
			controls = append(controls, ui.Button(tr("Add to PATH"), m.AddRungvToPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy))
		}

		var control ggui.Widget = widget.Box()
		if len(controls) == 1 {
			control = controls[0]
		} else if len(controls) > 1 {
			control = widget.Row(controls...).Gap(8).Align(widget.AlignCenter)
		}
		return settingRow(tr("Terminal PATH"), help, control)
	})
}

// engineFacts is what the rungv in use says of itself, with the buttons
// that look again and sign it.
func engineFacts(m *Model) ggui.Widget {
	return ggui.View(m.Engine, func(e Engine) ggui.Widget {
		again := ui.Button(tr("Check again"), func() { m.DetectEngine(nil) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14))
		if !e.Checked || e.Path == "" {
			if e.Err != "" {
				return widget.Column(ui.Alert(tr("Engine unavailable"), e.Err).Destructive().Icon(icons.Alert), widget.Row(again)).Gap(10).Stretch()
			}
			return widget.Row(again)
		}
		actions := []ggui.Widget{again}
		if e.Info.OS == "darwin" {
			actions = append(actions, tip(ui.Button(tr("Sign rungvx"), m.SignRungv).Outline().Size(ui.ButtonSmall),
				tr("Ad-hoc signs rungvx with the com.apple.security.hypervisor entitlement for the hvf engine.")))
		}
		rows := []ggui.Widget{
			kvText(tr("Binary"), e.Path),
			kvText(tr("Version"), strings.TrimSpace(e.Version.Version+" "+e.Version.Commit)),
			kvText(tr("Built with"), e.Version.Go),
			kvText(tr("Backends"), tr("%s (default %s)", strings.Join(e.Info.AvailableBackends, ", "), e.Info.DefaultBackend)),
			kvText(tr("Host lockdown"), e.Info.HostLockdown),
			kvText(tr("Layer cache"), e.Info.LayerCacheDir),
			kvText(tr("Volumes"), e.Info.VolumeDir),
		}
		rows = append(rows, ggui.View(m.RungvFeatures, func(f rungv.Features) ggui.Widget {
			var extra []ggui.Widget
			if f.GPU || e.GPURenderer != "" {
				var gpuVal string
				switch {
				case f.GPU && e.GPURenderer != "" && e.GPURendererVersion != "":
					gpuVal = tr("enabled (gvx-gpu-renderer %s)", shortVersion(e.GPURendererVersion))
				case f.GPU && e.GPURenderer != "":
					gpuVal = tr("enabled (%s)", tildePath(e.GPURenderer))
				case f.GPU:
					gpuVal = tr("rungvx supports GPU, but gvx-gpu-renderer was not found")
				default:
					gpuVal = tr("standard build (gvx-gpu-renderer at %s)", tildePath(e.GPURenderer))
				}
				extra = append(extra, kvText(tr("GPU"), gpuVal))
			}
			if note := volumeDirNote(e, ggui.Peek(m.Settings), f.VolumeDirFlag); note != nil {
				extra = append(extra, note)
			}
			if len(extra) == 0 {
				return widget.Box()
			}
			return widget.Column(extra...).Gap(8).Stretch()
		}))
		rows = append(rows, widget.Padding(widget.Row(actions...).Gap(8), 6, 0, 0, 0))
		return widget.Column(rows...).Gap(8).Stretch()
	})
}

// volumeDirNote shows the warning of volumeDirWarning for the engine e and the
// settings set, or returns nil when there is none.
func volumeDirNote(e Engine, set storeSettings, hasVolumeDirFlag bool) ggui.Widget {
	home, _ := os.UserHomeDir()
	title, body := volumeDirWarning(e.Info.OS, home, e.Info.VolumeDir, set.CacheDir, set.VolumeDir, hasVolumeDirFlag)
	if title == "" {
		return nil
	}
	return ui.Alert(title, body).Icon(icons.Alert)
}

// volumeDirWarning explains why the named volumes that rungvx reports in dir
// are not where the settings put them, or can be lost: rungvx ignored the
// volume directory setting, or dir lies in a cache folder that the host may
// purge. On rungvx v0.24+ (-volume-dir), a custom volume directory works
// alongside -cache-dir; earlier builds only read GVX_VOLUME_DIR when no
// -cache-dir is given. It returns empty strings when nothing applies.
func volumeDirWarning(goos, home, dir, cacheSetting, volumeSetting string, hasVolumeDirFlag bool) (title, body string) {
	if dir == "" {
		return "", ""
	}
	cacheSet := strings.TrimSpace(cacheSetting) != ""
	if want := rungv.ExpandHome(strings.TrimSpace(volumeSetting)); want != "" && filepath.Clean(want) != filepath.Clean(dir) {
		if cacheSet && !hasVolumeDirFlag {
			return tr("Volume directory not applied"),
				tr("A layer cache directory is set, so this rungvx keeps volumes in %s. Clear the layer cache directory setting, or update rungvx to v0.24+ to use -volume-dir.", dir)
		}
		return tr("Volume directory not applied"),
			tr("This rungvx does not read GVX_VOLUME_DIR and keeps volumes in %s. Update rungvx to use the setting.", dir)
	}
	if rungv.CacheVolumeDir(goos, home, dir) {
		if cacheSet && hasVolumeDirFlag {
			return tr("Volumes are in a cache folder"),
				tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Set a volume directory outside it, or move the layer cache directory out of it.")
		}
		if cacheSet {
			return tr("Volumes are in a cache folder"),
				tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Move the layer cache directory out of it.")
		}
		return tr("Volumes are in a cache folder"),
			tr("macOS empties ~/Library/Caches when the disk runs low, and named volumes there lose their data. Set a volume directory outside it, or update rungvx.")
	}
	return "", ""
}

// aboutCard names the app and its version, and where to read more.
func aboutCard(m *Model) ggui.Widget {
	return ui.Card(widget.Row(
		widget.Image(brandMark()).Size(56, 56),
		widget.Expanded(widget.Column(
			widget.Row(ui.Heading("GVX Desktop").Size(16), ui.Badge(AppVersion).Outline()).Gap(8).Align(widget.AlignCenter),
			ui.Caption(tr("gvx runs unmodified Linux containers on gVisor's user-space kernel, ported to macOS, Windows and Linux.")),
			widget.Padding(widget.Row(
				ui.Button(tr("gvx on GitHub"), func() { m.Open("https://github.com/gfire-sigs/gvx") }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)),
				ui.Button(tr("Releases"), func() { m.Open(rungv.ReleasesURL) }).Outline().Size(ui.ButtonSmall),
				ui.Button(tr("App data folder"), func() { m.Open(m.store.Dir) }).Ghost().Size(ui.ButtonSmall),
			).Gap(8), 6, 0, 0, 0),
		).Gap(4)),
	).Gap(16).Align(widget.AlignCenter))
}

// sourceFields lets the user choose which git repository and branch are cloned
// and built when the From source channel is selected.
func sourceFields(m *Model) ggui.Widget {
	isSource := ggui.Map(m.Settings, func(s storeSettings) bool { return s.UpdateChannel == ChannelSource })
	return ggui.If(isSource, func() ggui.Widget {
		set := ggui.Peek(m.Settings)
		repo := ggui.State(set.SourceRepo)
		branch := ggui.State(set.SourceBranch)
		ggui.Watch(m.Settings, func(s storeSettings) {
			if ggui.Peek(repo) != s.SourceRepo {
				repo.Set(s.SourceRepo)
			}
			if ggui.Peek(branch) != s.SourceBranch {
				branch.Set(s.SourceBranch)
			}
		})
		installing := ggui.Map(m.Update, func(u Update) bool { return u.Installing })
		custom := ggui.Map(m.Settings, func(s storeSettings) bool { return s.SourceRepo != "" || s.SourceBranch != "" })
		commit := func(string) { m.SetSource(ggui.Peek(repo), ggui.Peek(branch)) }
		reset := ggui.If(custom, func() ggui.Widget {
			return ui.Button(tr("Reset"), func() {
				repo.Set("")
				branch.Set("")
				m.SetSource("", "")
			}).Outline().Size(ui.ButtonSmall).BindDisabled(installing)
		})
		return widget.Column(
			widget.Row(
				widget.Expanded(widget.Column(
					ui.Caption(tr("Repository")).Size(12),
					ui.TextField(repo).Placeholder(rungv.SourceURL).Name(tr("Source repository")).
						OnCommit(commit).BindDisabled(installing),
				).Gap(4).Stretch()),
				widget.Box(widget.Column(
					ui.Caption(tr("Branch")).Size(12),
					ui.TextField(branch).Placeholder(rungv.SourceBranch).Name(tr("Source branch")).
						OnCommit(commit).BindDisabled(installing),
				).Gap(4).Stretch()).Width(180),
				widget.Padding(reset, 18, 0, 0, 0),
			).Gap(8).Align(widget.AlignStart),
			ui.Caption(tr("Git URL, GitHub owner/repo, or a local checkout. Leave empty for gfire-sigs/gvx (main).")),
		).Gap(6).Stretch()
	})
}

// releaseVersionField lets the user pick the newest release or a specific
// published version of rungv when the Releases channel is selected.
func releaseVersionField(m *Model) ggui.Widget {
	isRelease := ggui.Map(m.Settings, func(s storeSettings) bool { return s.UpdateChannel != ChannelSource })
	return ggui.If(isRelease, func() ggui.Widget {
		type state struct {
			e    Engine
			u    Update
			rels []rungv.Release
		}
		combined := ggui.Derived(func() state {
			return state{m.Engine.Get(), m.Update.Get(), m.Releases.Get()}
		}).WithEqual(func(a, b state) bool {
			return a.e.Equal(b.e) && a.u == b.u && slices.EqualFunc(a.rels, b.rels, releaseEqual)
		})
		selected := ggui.Bind(m.SelectedRelease.Get, m.SetSelectedRelease)
		return ggui.View(combined, func(s state) ggui.Widget {
			opts := make([]string, 0, len(s.rels)+1)
			opts = append(opts, "")
			pre := make(map[string]bool, len(s.rels))
			for _, r := range s.rels {
				opts = append(opts, r.Tag)
				if r.Prerelease {
					pre[r.Tag] = true
				}
			}
			format := func(tag string) string {
				if tag == "" {
					if s.u.Tag != "" {
						return tr("Latest (%s)", shortVersion(s.u.Tag))
					}
					return tr("Latest")
				}
				label := shortVersion(tag)
				installed := s.e.Checked && s.e.Path != "" && s.e.Version.Version == tag
				switch {
				case installed && pre[tag]:
					return tr("%s (installed, pre-release)", label)
				case installed:
					return tr("%s (installed)", label)
				case pre[tag]:
					return tr("%s (pre-release)", label)
				default:
					return label
				}
			}
			sel := ui.Select(selected).Options(opts).Format(format).MinWidth(240).Name(tr("Release version")).
				Disabled(s.u.Installing || s.u.Checking || len(s.rels) == 0)
			return settingRow(tr("Release version"),
				tr("Install the newest release or choose a specific published version."),
				sel)
		})
	})
}

// gpuExtensionField lets macOS Apple silicon users opt into rungvx's -ext
// build and gvx-gpu-renderer bundle on the Releases channel, showing clear
// status for releases that omit -ext or gvx-gpu-renderer.
func gpuExtensionField(m *Model) ggui.Widget {
	if !gpuExtensionSupported() {
		return widget.Box()
	}
	isRelease := ggui.Map(m.Settings, func(s storeSettings) bool { return s.UpdateChannel != ChannelSource })
	return ggui.If(isRelease, func() ggui.Widget {
		gpu := ggui.Bind(
			func() bool { return m.Settings.Get().RungvGPU },
			func(v bool) { m.UpdateSettings(func(s *storeSettings) { s.RungvGPU = v }) },
		)
		installing := ggui.Map(m.Update, func(u Update) bool { return u.Installing })
		type state struct {
			e    Engine
			f    rungv.Features
			gpu  bool
			u    Update
			rels []rungv.Release
			sel  string
		}
		combined := ggui.Derived(func() state {
			return state{
				e:    m.Engine.Get(),
				f:    m.RungvFeatures.Get(),
				gpu:  m.Settings.Get().RungvGPU,
				u:    m.Update.Get(),
				rels: m.Releases.Get(),
				sel:  m.SelectedRelease.Get(),
			}
		}).WithEqual(func(a, b state) bool {
			return a.e.Equal(b.e) && a.f == b.f && a.gpu == b.gpu && a.u == b.u &&
				a.sel == b.sel && slices.EqualFunc(a.rels, b.rels, releaseEqual)
		})
		details := ggui.View(combined, func(s state) ggui.Widget {
			var lines []ggui.Widget
			var chosen rungv.Release
			if s.sel != "" {
				for _, r := range s.rels {
					if r.Tag == s.sel {
						chosen = r
						break
					}
				}
			} else if len(s.rels) > 0 {
				chosen = newestRelease(s.rels)
			}
			if s.gpu && chosen.Tag != "" {
				switch {
				case !rungv.HasExtBuild(chosen):
					lines = append(lines, ui.Caption(tr("Release %s does not include a GPU extension build; standard rungvx will be installed.", shortVersion(chosen.Tag))).Color(colorAmber))
				case !rungv.HasGPURenderer(chosen):
					lines = append(lines, ui.Caption(tr("Release %s does not include gvx-gpu-renderer; an installed copy or the newest release that has one will be used.", shortVersion(chosen.Tag))))
				}
			}
			if s.e.Checked && s.e.Path != "" {
				busy := s.u.Installing || s.u.Checking
				canInstallExt := chosen.Tag == "" || rungv.HasExtBuild(chosen)
				switch {
				case s.gpu && !s.f.GPU:
					lines = append(lines, widget.Row(
						ui.Button(tr("Reinstall with GPU"), func() { m.InstallRungvRelease(s.sel) }).Outline().Size(ui.ButtonSmall).Disabled(busy || !canInstallExt),
						widget.Expanded(ui.Caption(tr("Installed rungvx is a standard build without GPU support."))),
					).Gap(10).Align(widget.AlignCenter))
				case s.gpu && s.f.GPU && s.e.GPURenderer == "":
					lines = append(lines, widget.Row(
						ui.Button(tr("Reinstall with GPU"), func() { m.InstallRungvRelease(s.sel) }).Outline().Size(ui.ButtonSmall).Disabled(busy || !canInstallExt),
						widget.Expanded(ui.Caption(tr("gvx-gpu-renderer is not installed yet."))),
					).Gap(10).Align(widget.AlignCenter))
				case !s.gpu && s.f.GPU:
					lines = append(lines, widget.Row(
						ui.Button(tr("Reinstall standard"), func() { m.InstallRungvRelease(s.sel) }).Outline().Size(ui.ButtonSmall).Disabled(busy),
						widget.Expanded(ui.Caption(tr("Installed rungvx has GPU support; reinstall to switch back to the standard build."))),
					).Gap(10).Align(widget.AlignCenter))
				case s.gpu && s.f.GPU && s.e.GPURenderer != "":
					if s.e.GPURendererVersion != "" {
						lines = append(lines, ui.Caption(tr("GPU extension ready (gvx-gpu-renderer %s).", shortVersion(s.e.GPURendererVersion))))
					} else {
						lines = append(lines, ui.Caption(tr("GPU extension ready (%s).", tildePath(s.e.GPURenderer))))
					}
				}
			}
			if len(lines) == 0 {
				return widget.Box()
			}
			return widget.Column(lines...).Gap(6).Stretch()
		})
		return widget.Column(
			settingRow(tr("Include GPU extension"),
				tr("Installs the darwin/arm64 -ext build and gvx-gpu-renderer into ~/.gvx/libexec/gvx-gpu-renderer (macOS 26+ Apple silicon). Releases without gvx-gpu-renderer reuse an installed copy or fall back to the newest release that has one."),
				ui.Switch(gpu, "").BindDisabled(installing).Name(tr("Include GPU extension"))),
			details,
		).Gap(8).Stretch()
	})
}

// releaseRow says what the channel's newest gvx is (or the chosen release
// version), and installs it.
func releaseRow(m *Model) ggui.Widget {
	type state struct {
		e   Engine
		u   Update
		sel string
	}
	both := ggui.Derived(func() state {
		return state{m.Engine.Get(), m.Update.Get(), m.SelectedRelease.Get()}
	}).WithEqual(func(a, b state) bool {
		return a.e.Equal(b.e) && a.u == b.u && a.sel == b.sel
	})
	row := ggui.View(both, func(s state) ggui.Widget {
		e, u, sel := s.e, s.u, s.sel
		branch := u.BranchName()
		var text string
		var action ggui.Widget = widget.Box()
		install := tr("Install rungvx")
		if u.Source {
			install = tr("Build and install")
		}
		switch {
		case u.Installing && u.Source:
			text = tr("Building rungvx from %s", branch)
			action = widget.Row(
				installButton(m, u, install).Size(ui.ButtonSmall),
				ui.Button(tr("Cancel"), m.CancelInstallRungv).Outline().Size(ui.ButtonSmall),
			).Gap(6).Align(widget.AlignCenter)
		case u.Installing:
			text = tr("Installing rungvx %s", u.InstallName())
			action = widget.Row(
				installReleaseButton(m, u, sel, install).Size(ui.ButtonSmall),
				ui.Button(tr("Cancel"), m.CancelInstallRungv).Outline().Size(ui.ButtonSmall),
			).Gap(6).Align(widget.AlignCenter)
		case u.Checking:
			text = tr("Checking for the newest…")
		case !u.Known() && u.Err != "":
			text = tr("The newest could not be read: %s", u.Err)
		case !u.Known():
			text = tr("The newest has not been looked up.")
		case !u.Source && sel != "":
			ver := shortVersion(sel)
			switch {
			case e.Path == "":
				text = tr("rungvx %s can be installed into ~/.gvx/bin.", ver)
				action = installReleaseButton(m, u, sel, install).Size(ui.ButtonSmall)
			case e.Version.Version == sel:
				text = tr("rungvx %s is currently installed.", ver)
				action = ui.Button(tr("Reinstall"), func() { m.InstallRungvRelease(sel) }).Outline().Size(ui.ButtonSmall)
			case rungv.Newer(sel, e.Version.Version):
				text = tr("rungvx %s is selected; this computer has %s.", ver, engineVersion(e))
				action = installReleaseButton(m, u, sel, tr("Install %s", ver)).Size(ui.ButtonSmall)
			default:
				text = tr("rungvx %s is selected (older than %s).", ver, engineVersion(e))
				action = installReleaseButton(m, u, sel, tr("Install %s", ver)).Size(ui.ButtonSmall)
			}
		case e.Path == "":
			text = tr("rungvx %s can be installed into ~/.gvx/bin.", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Available(e):
			switch {
			case u.Source && u.Ahead > 0:
				text = tr("%s is %d commits past this computer's %s.", branch, u.Ahead, engineVersion(e))
			case u.Source:
				text = tr("%s is at %s; this computer has %s.", branch, u.Name(), engineVersion(e))
			case u.Newer(e):
				text = tr("rungvx %s is available; this computer has %s.", u.Name(), engineVersion(e))
			default:
				text = tr("The newest release is rungvx %s; this computer has %s.", u.Name(), engineVersion(e))
			}
			action = installButton(m, u, updateVerb(u, e)).Size(ui.ButtonSmall)
		case u.Source:
			text = tr("rungvx is up to date with %s (%s).", branch, u.Name())
			action = ui.Button(tr("Build again"), m.InstallRungv).Outline().Size(ui.ButtonSmall)
		default:
			text = tr("rungvx is up to date: %s is the newest release.", u.Name())
		}
		return widget.Row(
			action,
			ui.Button(tr("Check for updates"), func() { m.CheckUpdate(false) }).Outline().Size(ui.ButtonSmall).
				Disabled(u.Checking || u.Installing),
			widget.Expanded(ui.Caption(text)),
		).Gap(10).Align(widget.AlignCenter)
	})
	// What a build printed stays while it runs, and after it failed.
	log := ggui.If(ggui.Map(m.BuildLog, func(s string) bool { return s != "" }), func() ggui.Widget {
		return ui.CodeBlockOf(m.BuildLog).Height(160).Small().CopyButton(true)
	})
	return widget.Column(row, log).Gap(10).Stretch()
}

// channelName words a channel.
func channelName(c string) string {
	if c == ChannelSource {
		return tr("From source")
	}
	return tr("Releases")
}

// themeIcon pictures an appearance: a screen for the system's, the sun
// and the moon for light and dark.
func themeIcon(theme string) *icons.Widget {
	switch theme {
	case "light":
		return glyph(lucide.Sun, 14)
	case "dark":
		return glyph(lucide.Moon, 14)
	}
	return glyph(lucide.Monitor, 14)
}

// dockerSettings chooses the docker command the app runs and the endpoint the
// app's Docker API proxy listens on.
func dockerSettings(m *Model) ggui.Widget {
	path := ggui.State(ggui.Peek(m.Settings).DockerPath)
	setPath := func(v string) { m.UpdateSettings(func(s *storeSettings) { s.DockerPath = strings.TrimSpace(v) }) }
	found := ui.CaptionOf(ggui.Combine(m.Settings, m.BundledDocker, func(s storeSettings, bd BundledDockerState) string {
		p, ok := m.DockerCLI()
		switch {
		case ok && (dockercli.IsBundledPath(p) || (bd.Available && bd.Using && strings.TrimSpace(s.DockerPath) == "")):
			return tr("Using bundled Docker CLI (%s).", tildePath(p))
		case ok:
			return tr("Using %s.", tildePath(p))
		case strings.TrimSpace(s.DockerPath) != "":
			return tr("Nothing is at that path.")
		}
		return tr("docker was not found. Install the Docker CLI alone; no local engine is needed.")
	}))

	endpoint := ggui.Bind(func() string {
		m.Settings.Get()
		if m.DockerAPI().TCP {
			return DockerTCPEndpoint
		}
		return DockerSocketEndpoint
	}, func(v string) {
		api := m.DockerAPI()
		api.TCP = v == DockerTCPEndpoint
		m.SetDockerAPI(api)
	})
	port := ggui.State(strconv.Itoa(m.DockerAPI().Port))
	ggui.Watch(m.Settings, func(s storeSettings) {
		if ggui.Peek(path) != s.DockerPath {
			path.Set(s.DockerPath)
		}
		wantPort := strconv.Itoa(dockerSettingOf(s).Port)
		if ggui.Peek(port) != wantPort {
			port.Set(wantPort)
		}
	})
	setPort := func(v string) {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > 65535 {
			port.Set(strconv.Itoa(m.DockerAPI().Port)) // not a port: put back what holds
			return
		}
		api := m.DockerAPI()
		api.Port = n
		m.SetDockerAPI(api)
	}
	overTCP := ggui.Map(endpoint, func(v string) bool { return v == DockerTCPEndpoint })
	host := ui.CaptionOf(ggui.Map(m.DockerEndpoint, func(h string) string { return tr("DOCKER_HOST is %s.", h) }))
	proxyErr := ggui.View(ggui.Combine(m.DockerEndpoint, m.DockerProxyErr, func(h, err string) [2]string { return [2]string{h, err} }), func(v [2]string) ggui.Widget {
		if v[1] == "" {
			return widget.Box()
		}
		return widget.Row(
			glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
			widget.Expanded(ui.Caption(tr("Could not listen on %s: %s", v[0], v[1]))),
		).Gap(8).Align(widget.AlignStart)
	})

	exposeLAN := ggui.Bind(m.DockerExposeLAN.Get, m.SetDockerExposeLAN)
	sshAgent := ggui.Bind(m.DockerSSHAgent.Get, m.SetDockerSSHAgent)
	bindSymlinks := ggui.Bind(m.DockerBindSymlinks.Get, m.SetDockerBindSymlinks)

	apiRows := []ggui.Widget{
		widget.Row(
			widget.Expanded(widget.Column(
				widget.Text(tr("Engine API")).Size(14).Weight(ggui.WeightMedium),
				ui.CaptionOf(ggui.Map(m.Settings, func(s storeSettings) string {
					if s.SandboxEngine {
						return tr("How docker reaches GVX Desktop's sandbox engine. Changes take effect immediately.")
					}
					return tr("How the Docker Engine is reached. Changes take effect immediately.")
				})),
			).Gap(2)),
			widget.Row(
				ggui.If(overTCP, func() ggui.Widget {
					return widget.Box(ui.TextField(port).Filter(digits).Name(tr("Docker API port")).OnCommit(setPort)).Width(84)
				}),
				ui.ToggleGroup(endpoint).Options([]string{DockerSocketEndpoint, DockerTCPEndpoint}).Format(dockerEndpointName).Outline().Spacing(0).Name(tr("Engine API")),
			).Gap(8).Align(widget.AlignCenter),
		).Gap(24).Align(widget.AlignCenter),
	}
	if runtime.GOOS == "windows" {
		apiRows = append(apiRows, ui.Caption(tr("A Docker CLI on Windows reaches a Unix socket only from version 29 on. TCP on this computer's loopback address works with any version, on every system.")))
	}
	apiRows = append(apiRows, host, proxyErr)

	modeRow := dockerModeSettings(m)
	cliRow := widget.Column(
		settingField(tr("docker path"), tr("The docker command the app sets a context up with. Leave empty to look where a terminal would."), widget.Row(
			widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, Homebrew, Docker Desktop, bundled)")).Name(tr("docker path")).OnCommit(setPath)),
			ui.Button(tr("Browse…"), func() { browseFile(m, path, setPath) }).Outline(),
		).Gap(8)),
		found,
	).Gap(4).Stretch()
	bundledRow := bundledDockerSettings(m)
	apiCol := widget.Column(apiRows...).Gap(6).Stretch()
	sharedRow := sharedFoldersField(m)
	lanRow := settingRow(
		tr("Publish container ports on local network (0.0.0.0)"),
		tr("Published container ports default to 127.0.0.1 only. Turning this on binds ports on 0.0.0.0, exposing them to other devices on your local network (requires rungvx v0.21+ for Docker Engine)."),
		ui.Switch(exposeLAN, "").Name(tr("Publish container ports on local network (0.0.0.0)")),
	)
	sshRow := settingRow(
		tr("Forward host SSH agent"),
		tr("Relays this machine's SSH_AUTH_SOCK to /run/host-services/ssh-auth.sock for containers that mount it. Requires rungvx v0.21+."),
		ui.Switch(sshAgent, "").Name(tr("Forward host SSH agent")),
	)
	symlinkRow := settingRow(
		tr("Create host symbolic links in bind mounts"),
		tr("Materializes safe relative symbolic links inside container bind mounts on macOS and Linux (symlinks=contained) so host tools and IDEs see them. Requires rungvx v0.26+ in Sandbox Engine mode."),
		ui.Switch(bindSymlinks, "").Name(tr("Create host symbolic links in bind mounts")),
	)
	cfgRow := dockerConfigField(m)

	hasBundled := ggui.Map(m.BundledDocker, func(s BundledDockerState) bool { return s.Available })
	return ggui.View(hasBundled, func(avail bool) ggui.Widget {
		rows := []ggui.Widget{modeRow, cliRow}
		if avail {
			rows = append(rows, bundledRow)
		}
		rows = append(rows, apiCol, sharedRow, lanRow, sshRow, symlinkRow, cfgRow)
		return settingsGroup(dockerIcon, "Docker", rows...)
	})
}

// bundledDockerSettings shows the bundled Docker CLI status and lets the user
// add or remove ~/.gvx/bin on their terminal PATH.
func bundledDockerSettings(m *Model) ggui.Widget {
	return ggui.View(m.BundledDocker, func(s BundledDockerState) ggui.Widget {
		if !s.Available {
			return widget.Box()
		}
		bin := tildePath(s.BinDir)
		prof := tildePath(s.Profile)
		var help string
		var control ggui.Widget = widget.Box()
		if s.InPATH {
			help = tr("Ships %s inside the app (%s). Used automatically when no external docker CLI is installed; %s is on your terminal PATH.", s.ToolsList(), tildePath(s.CLIPath), bin)
			if s.Managed {
				control = ui.Button(tr("Remove from PATH"), m.RemoveBundledDockerFromPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy)
			}
		} else {
			help = tr("Ships %s inside the app (%s). Used automatically when no external docker CLI is installed; add %s to %s to use it in new terminals.", s.ToolsList(), tildePath(s.CLIPath), bin, prof)
			control = ui.Button(tr("Add to PATH"), m.AddBundledDockerToPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy)
		}
		return settingRow(tr("Bundled Docker CLI"), help, control)
	})
}

// dockerConfigField leads to the editor of the Docker Engine's daemon.json.
// In sandbox-engine mode it shows a dimmed note instead: no dockerd runs.
func dockerConfigField(m *Model) ggui.Widget {
	return ggui.View(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func(se bool) ggui.Widget {
		title := widget.Text(tr("Engine configuration")).Size(14).Weight(ggui.WeightMedium)
		if se {
			return widget.Column(
				title.Color(colorGray),
				ui.Caption(tr("Applies only to the Docker Engine sandbox. The sandbox engine has no dockerd to configure.")),
			).Gap(4).Stretch()
		}
		return widget.Row(
			widget.Expanded(widget.Column(
				title,
				ui.Caption(tr("dockerd's daemon.json, kept at %s: registry mirrors, insecure registries, log levels and the like. A running engine applies what dockerd can without a restart.", tildePath(recipes.DockerConfigFile()))),
			).Gap(2)),
			ui.Button(tr("Edit daemon.json…"), m.OpenDockerConfig).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.FileBraces, 14)),
		).Gap(16).Align(widget.AlignCenter)
	})
}

// dockerConfigDialog edits the Docker Engine's daemon.json.
func dockerConfigDialog(m *Model) ggui.Widget {
	errText := ggui.If(ggui.Map(m.DockerConfigErr, func(s string) bool { return s != "" }), func() ggui.Widget {
		return widget.Row(glyph(lucide.TriangleExclamationPoint, 14).Color(colorRed),
			widget.Expanded(ui.CaptionOf(m.DockerConfigErr).Color(colorRed))).Gap(8).Align(widget.AlignStart)
	})
	field := ui.TextField(m.DockerConfigText).NoWrap().Lines(16).Name(tr("daemon.json")).
		OnChange(func(string) { m.DockerConfigErr.Set("") })
	return ui.Dialog(m.DockerConfigOpen, widget.Column(
		ui.Caption(tr("Saved to %s. %s stays among the insecure registries, so the engine can push to the built-in local registry.", tildePath(recipes.DockerConfigFile()), recipes.RegistryHost())),
		field,
		errText,
		ui.Caption(tr("dockerd applies debug, labels, registry-mirrors, insecure-registries, max-concurrent-downloads and a few more on the fly; any other change takes a restart of the engine.")),
	).Gap(10).Stretch()).Title(tr("Docker Engine configuration")).Width(620).Footer(
		ui.Button(tr("Cancel"), func() { m.DockerConfigOpen.Set(false) }).Outline(),
		ui.Button(tr("Save"), m.SaveDockerConfig),
	)
}

// dockerModeToggle returns a compact mode selector switching between the
// default native sandbox engine and docker:dind compatibility mode.
func dockerModeToggle(m *Model, size *ui.ButtonSizeVariant) ggui.Widget {
	mode := ggui.Bind(func() string {
		if m.Settings.Get().SandboxEngine {
			return "sandbox"
		}
		return "compat"
	}, func(v string) {
		m.SwitchSandboxEngine(v == "sandbox")
	})
	return ui.ToggleGroup(mode).Options([]string{"sandbox", "compat"}).Format(func(v string) string {
		if v == "compat" {
			return tr("Compatibility (docker:dind)")
		}
		return tr("Sandbox engine")
	}).Outline().Size(size).Spacing(0).Name(tr("Docker engine mode"))
}

// dockerModeSettings chooses between the default native sandbox engine and
// compatibility mode (docker:dind), with BuildKit builder status when active.
func dockerModeSettings(m *Model) ggui.Widget {
	dockerRunning := ggui.Combine(m.Settings, m.Rows, func(s storeSettings, rows []Row) bool {
		if !s.SandboxEngine {
			return false
		}
		r, ok := m.RecipeSandbox(rows, recipes.Docker)
		return ok && r.State.Status.Active()
	})
	runningNote := ggui.If(dockerRunning, func() ggui.Widget {
		return widget.Row(
			glyph(lucide.Info, 14).Color(colorAmber),
			widget.Expanded(ui.Caption(tr("Your Docker Engine sandbox is still running, but docker no longer reaches it while this mode is on. Stop it manually if you do not need it."))),
		).Gap(8).Align(widget.AlignStart)
	})
	builderStatus := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func() ggui.Widget {
		return widget.Column(
			sandboxBuilderStatus(m),
			widget.Row(
				widget.Padding(glyph(lucide.Info, 13).Color(colorBlue), 2, 0, 0, 0),
				widget.Expanded(ui.Caption(tr("Turning this on routes docker build through buildx (docker buildx install) and selects the gvx-desktop builder for the gvx context and engine endpoint; turning it off puts both back.")).Size(12)),
			).Gap(8).Align(widget.AlignStart),
		).Gap(6).Stretch()
	})
	desc := ui.CaptionOf(ggui.Map(m.Settings, func(s storeSettings) string {
		if s.SandboxEngine {
			return tr("Default: GVX Desktop serves the Docker API directly and runs each container as its own gvx sandbox. Switching to compatibility mode stops running sandbox-engine containers.")
		}
		return tr("Compatibility mode: runs an unmodified dockerd inside a gvx sandbox (docker:dind). Use this for workloads that need dockerd internals or daemon.json.")
	}))
	return widget.Column(
		widget.Row(
			widget.Expanded(widget.Column(
				widget.Text(tr("Engine mode")).Size(14).Weight(ggui.WeightMedium),
				desc,
			).Gap(2)),
			dockerModeToggle(m, ui.ButtonSmall),
		).Gap(24).Align(widget.AlignCenter),
		builderStatus,
		runningNote,
	).Gap(8).Stretch()
}

// amd64BuilderSettings is the shared Apple silicon requirements header plus the
// "amd64 builds with Rosetta" switch, in Labs.
func amd64BuilderSettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().Amd64Builds }, m.SetAmd64Builds)
	disabled := ggui.Combine(m.Settings, m.Amd64Check, func(s storeSettings, c Amd64CheckState) bool {
		if !s.SandboxEngine {
			return true
		}
		if s.Amd64Builds {
			return false
		}
		return !c.OK() || c.Checking || c.Installing
	})
	needsEngine := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return !s.SandboxEngine }), func() ggui.Widget {
		return ui.Caption(tr("Requires the default sandbox engine. Switch off compatibility mode in Settings → Docker."))
	})
	statusBox := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine && s.Amd64Builds }), func() ggui.Widget {
		return amd64BuilderStatus(m)
	})
	return widget.Column(
		amd64Requirements(m),
		needsEngine,
		settingRow(tr("amd64 builds with Rosetta"),
			tr("Builds for --platform linux/amd64 run in a small Linux VM with Rosetta for Linux. The VM starts on the first amd64 build and stops when idle."),
			ui.Switch(enabled, "").Name(tr("amd64 builds with Rosetta")).BindDisabled(disabled)),
		statusBox,
	).Gap(8).Stretch()
}

// microVMSettings is the "Micro-VM container runtime" switch, default per-VM
// resource fields, and live status card in Labs (Apple silicon macOS).
func microVMSettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().MicroVMContainers }, m.SetMicroVMContainers)
	disabled := ggui.Combine(m.Settings, m.Amd64Check, func(s storeSettings, c Amd64CheckState) bool {
		if !s.SandboxEngine {
			return true
		}
		if s.MicroVMContainers {
			return false
		}
		return !c.MicroVMOK() || c.Checking || c.Installing
	})

	initSet := ggui.Peek(m.Settings)
	cpusField := ggui.State(strconv.Itoa(initSet.MicroVMCPUsOrDefault()))
	memField := ggui.State(strconv.Itoa(initSet.MicroVMMemoryMiBOrDefault()))
	ggui.Watch(m.Settings, func(s storeSettings) {
		wantCPUs := strconv.Itoa(s.MicroVMCPUsOrDefault())
		if ggui.Peek(cpusField) != wantCPUs {
			cpusField.Set(wantCPUs)
		}
		wantMem := strconv.Itoa(s.MicroVMMemoryMiBOrDefault())
		if ggui.Peek(memField) != wantMem {
			memField.Set(wantMem)
		}
	})
	commitResources := func(string) {
		cur := ggui.Peek(m.Settings)
		c, err1 := strconv.Atoi(strings.TrimSpace(ggui.Peek(cpusField)))
		mem, err2 := strconv.Atoi(strings.TrimSpace(ggui.Peek(memField)))
		if err1 != nil || err2 != nil || c <= 0 || mem <= 0 {
			cpusField.Set(strconv.Itoa(cur.MicroVMCPUsOrDefault()))
			memField.Set(strconv.Itoa(cur.MicroVMMemoryMiBOrDefault()))
			return
		}
		m.SetMicroVMResources(c, mem)
		after := ggui.Peek(m.Settings)
		cpusField.Set(strconv.Itoa(after.MicroVMCPUsOrDefault()))
		memField.Set(strconv.Itoa(after.MicroVMMemoryMiBOrDefault()))
	}

	resourcesDisabled := ggui.Map(m.Settings, func(s storeSettings) bool { return !s.SandboxEngine })
	resourcesRow := settingRow(
		tr("Default resources per VM"),
		tr("Per-container defaults when docker run does not pass --cpus or -m. Defaults match apple/container (%d CPUs, %d MiB).", store.DefaultMicroVMCPUs, store.DefaultMicroVMMemoryMiB),
		widget.Row(
			ui.Caption(tr("CPUs")).Size(12),
			widget.Box(ui.TextField(cpusField).Filter(digits).Name(tr("Default micro-VM CPUs")).OnCommit(commitResources).BindDisabled(resourcesDisabled)).Width(64),
			ui.Caption(tr("Memory (MiB)")).Size(12),
			widget.Box(ui.TextField(memField).Filter(digits).Name(tr("Default micro-VM memory (MiB)")).OnCommit(commitResources).BindDisabled(resourcesDisabled)).Width(84),
		).Gap(8).Align(widget.AlignCenter),
	)

	statusBox := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine && s.MicroVMContainers }), func() ggui.Widget {
		return microVMStatus(m)
	})

	return widget.Column(
		settingRow(tr("Micro-VM container runtime"),
			tr("Run linux/amd64 containers automatically with Rosetta, or run linux/arm64 containers on a real Linux kernel with --runtime=microvm. Each container gets an isolated VM and returns all memory when stopped."),
			ui.Switch(enabled, "").Name(tr("Micro-VM container runtime")).BindDisabled(disabled)),
		resourcesRow,
		microVMImagesSection(m, resourcesDisabled),
		statusBox,
	).Gap(10).Stretch()
}

func microVMImagesSection(m *Model, disabled ggui.Readable[bool]) ggui.Widget {
	draft := ggui.State("")
	submit := func() {
		raw := strings.TrimSpace(ggui.Peek(draft))
		if raw == "" {
			return
		}
		m.AddMicroVMImage(raw)
		draft.Set("")
	}
	inputRow := widget.Row(
		widget.Expanded(ui.TextField(draft).
			Placeholder("postgres:16, ghcr.io/org/*").
			Name(tr("Image or pattern")).
			OnSubmit(func(string) { submit() }).
			BindDisabled(disabled)),
		ui.Button(tr("Add"), submit).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.Plus, 14)).BindDisabled(disabled),
	).Gap(8).Align(widget.AlignCenter)

	images := ggui.Map(m.Settings, func(s storeSettings) []string { return s.MicroVMImages }).WithEqual(slices.Equal[[]string])
	list := ggui.View(images, func(items []string) ggui.Widget {
		if len(items) == 0 {
			return ui.Caption(tr(`No images registered yet. Add an image above or choose "Always run in micro-VM" from the Images page.`)).Size(12)
		}
		rows := make([]ggui.Widget, 0, len(items))
		for _, item := range items {
			pat := item
			rows = append(rows, widget.Row(
				glyph(lucide.Cpu, 14).Color(colorGray),
				widget.Expanded(ui.Mono(pat).Size(12).NoWrap().Ellipsis()),
				tip(iconButton(lucide.X, tr("Remove %s from micro-VM images", pat), func() { m.RemoveMicroVMImage(pat) }), tr("Remove")),
			).Gap(8).Align(widget.AlignCenter))
		}
		return widget.Box(widget.Column(rows...).Gap(4).Stretch()).Pad(8, 10).Radius(8).Border(1, colorBorder)
	})

	return widget.Column(
		widget.Text(tr("Images that always run in a micro-VM")).Size(13).Weight(ggui.WeightMedium),
		ui.Caption(tr("Containers from matching images start in a Linux micro-VM automatically without passing --runtime=microvm. Use a repository (postgres), exact tag (postgres:16), or wildcard (ghcr.io/org/*).")).Size(12),
		inputRow,
		list,
	).Gap(6).Stretch()
}

// microVMStatus shows the live micro-VM container runtime status: running
// micro-VM count, VM assets v2 download progress, Rosetta hint when Rosetta is
// not installed, and any runtime error.
func microVMStatus(m *Model) ggui.Widget {
	type viewState struct {
		st    engine.MicroVMState
		check Amd64CheckState
	}
	combined := ggui.Combine(m.MicroVMState, m.Amd64Check, func(st engine.MicroVMState, c Amd64CheckState) viewState {
		return viewState{st: st, check: c}
	})
	return ggui.View(combined, func(v viewState) ggui.Widget {
		st := v.st
		var rows []ggui.Widget

		var stateDot ggui.Widget
		var stateLabel string
		switch {
		case st.RunningVMs > 0:
			stateDot = dot(colorGreen, 7)
			stateLabel = trn(st.RunningVMs, "%d micro-VM running", "%d micro-VMs running")
		case st.Downloading:
			stateDot = dot(colorAmber, 7)
			stateLabel = tr("Micro-VM runtime: idle")
		default:
			stateDot = dot(colorGray, 7)
			stateLabel = tr("Micro-VM runtime: idle")
		}

		var assetLine string
		switch {
		case st.Downloading && st.Total > 0:
			assetLine = tr("Downloading the VM, %s of %s", formatBytes(st.Done), formatBytes(st.Total))
		case st.Downloading:
			assetLine = tr("Downloading the VM…")
		case st.Assets == "local":
			assetLine = tr("VM assets: local override")
		case st.Assets != "":
			assetLine = tr("VM assets: v%s downloaded", st.Assets)
		default:
			assetLine = tr("VM assets download on the first micro-VM container.")
		}

		if st.Downloading {
			rows = append(rows, widget.Row(
				stateDot,
				widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
			).Gap(8).Align(widget.AlignCenter))
			rows = append(rows, widget.Row(
				ui.Spinner().Size(12),
				widget.Expanded(ui.Caption(assetLine).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
			if st.Total > 0 {
				frac := float64(st.Done) / float64(st.Total)
				rows = append(rows, widget.Box(ui.Progress(ggui.Const(min(1.0, max(0.0, frac)))).Height(4)))
			}
		} else {
			rows = append(rows, widget.Row(
				stateDot,
				widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
				widget.Expanded(ui.Caption(assetLine).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
		}

		if v.check.Checked && v.check.Err == "" && v.check.Result.Rosetta != vmhelper.RosettaInstalled {
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 13).Color(colorBlue),
				widget.Expanded(ui.Caption(tr("arm64 containers with --runtime=microvm are ready; install Rosetta to also run linux/amd64 containers.")).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		}

		if st.Err != "" {
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Micro-VM runtime error: %s", st.Err)).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		}

		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10).Radius(8).Border(1, colorBorder)
	})
}

// amd64Requirements shows the host requirements check for the amd64 builder VM
// (Apple silicon, macOS 13+, virtualization entitlement, Rosetta), offering
// Rosetta installation when missing.
func amd64Requirements(m *Model) ggui.Widget {
	return ggui.View(m.Amd64Check, func(c Amd64CheckState) ggui.Widget {
		if c.Checking && !c.Checked {
			return widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Checking requirements…")).Size(12)).Gap(8).Align(widget.AlignCenter)
		}
		if !c.Checked {
			return widget.Row(
				widget.Expanded(ui.Caption(tr("Requirements have not been checked.")).Size(12)),
				ui.Button(tr("Check again"), m.CheckAmd64).Outline().Size(ui.ButtonExtraSmall),
			).Gap(8).Align(widget.AlignCenter)
		}
		if c.Err != "" {
			return widget.Row(
				glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Could not check requirements: %s", c.Err)).Size(12)),
				ui.Button(tr("Check again"), m.CheckAmd64).Outline().Size(ui.ButtonExtraSmall).Disabled(c.Checking),
			).Gap(8).Align(widget.AlignCenter)
		}
		res := c.Result
		if !res.AppleSilicon {
			return widget.Row(
				glyph(lucide.Info, 14).Color(colorGray),
				widget.Expanded(ui.Caption(tr("Not available on this computer: requires Apple silicon (macOS 13 or later).")).Size(12)),
			).Gap(8).Align(widget.AlignStart)
		}
		if c.OK() {
			osLabel := tr("macOS 13+")
			if res.MacOS != "" {
				osLabel = tr("macOS %s", res.MacOS)
			}
			return widget.Row(
				glyph(lucide.CircleCheck, 14).Color(colorGreen),
				widget.Expanded(ui.Caption(tr("Apple silicon")+" · "+osLabel+" · "+tr("Rosetta installed")).Size(12)),
			).Gap(8).Align(widget.AlignCenter)
		}

		reqRow := func(ok bool, text string) ggui.Widget {
			icon := glyph(lucide.CircleCheck, 13).Color(colorGreen)
			if !ok {
				icon = glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber)
			}
			return widget.Row(icon, widget.Expanded(ui.Caption(text).Size(12))).Gap(8).Align(widget.AlignStart)
		}

		var rows []ggui.Widget
		rows = append(rows, reqRow(true, tr("Apple silicon")))
		switch {
		case res.MacOS != "" && macOSAtLeast13(res.MacOS):
			rows = append(rows, reqRow(true, tr("macOS %s", res.MacOS)))
		case res.MacOS != "":
			rows = append(rows, reqRow(false, tr("macOS 13 or later is required (this Mac has %s).", res.MacOS)))
		case res.Rosetta == vmhelper.RosettaNotSupported:
			rows = append(rows, reqRow(false, tr("macOS 13 or later is required.")))
		default:
			rows = append(rows, reqRow(true, tr("macOS 13+")))
		}
		if !res.Virtualization {
			rows = append(rows, reqRow(false, tr("Virtualization is unavailable (missing the virtualization entitlement).")))
		}
		switch res.Rosetta {
		case vmhelper.RosettaInstalled:
			rows = append(rows, reqRow(true, tr("Rosetta installed")))
		case vmhelper.RosettaNotInstalled:
			var installBtn ggui.Widget
			if c.Installing {
				installBtn = widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Installing Rosetta…")).Size(12)).Gap(6).Align(widget.AlignCenter)
			} else {
				installBtn = ui.Button(tr("Install Rosetta"), m.InstallRosetta).Outline().Size(ui.ButtonExtraSmall).Disabled(c.Checking)
			}
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Rosetta is not installed.")).Size(12)),
				installBtn,
			).Gap(8).Align(widget.AlignCenter))
			rows = append(rows, widget.Row(
				widget.Expanded(ui.Caption(tr("Or run in a terminal: %s", "softwareupdate --install-rosetta")).Size(12)),
				iconButton(lucide.Copy, tr("Copy %s", "softwareupdate --install-rosetta"), func() {
					m.Copy("softwareupdate --install-rosetta", "softwareupdate --install-rosetta")
				}),
			).Gap(8).Align(widget.AlignCenter))
		default:
			rows = append(rows, reqRow(false, tr("Rosetta for Linux is not supported on this Mac.")))
		}
		if res.Err != "" {
			rows = append(rows, reqRow(false, res.Err))
		}
		rows = append(rows, widget.Row(
			ui.Button(tr("Check again"), m.CheckAmd64).Outline().Size(ui.ButtonExtraSmall).Disabled(c.Checking || c.Installing),
		))
		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10).Radius(8).Border(1, colorBorder)
	})
}

// amd64BuilderStatus shows the amd64 builder VM's state, asset download
// progress, persistent build cache disk size, reset button, and buildx routing.
func amd64BuilderStatus(m *Model) ggui.Widget {
	type viewState struct {
		amd       engine.Amd64State
		bx        BuildxStatus
		resetting bool
	}
	combined := ggui.Derived(func() viewState {
		return viewState{
			amd:       m.Amd64State.Get(),
			bx:        m.BuildxStatus.Get(),
			resetting: m.Amd64Resetting.Get(),
		}
	})
	return ggui.View(combined, func(v viewState) ggui.Widget {
		st := v.amd
		var rows []ggui.Widget

		// 1. VM state (idle / starting / running / stopping).
		var stateDot ggui.Widget
		var stateLabel string
		switch st.Status {
		case engine.BuilderRunning:
			stateDot = dot(colorGreen, 7)
			stateLabel = tr("amd64 builder: running")
		case engine.BuilderStarting:
			stateDot = dot(colorAmber, 7)
			stateLabel = tr("amd64 builder: starting…")
		case engine.Amd64Stopping:
			stateDot = dot(colorAmber, 7)
			stateLabel = tr("amd64 builder: stopping…")
		default:
			stateDot = dot(colorGray, 7)
			stateLabel = tr("amd64 builder: idle")
		}

		// 2. VM assets state or download progress.
		var assetLine string
		switch {
		case st.Downloading && st.Total > 0:
			assetLine = tr("Downloading the VM, %s of %s", formatBytes(st.Done), formatBytes(st.Total))
		case st.Downloading:
			assetLine = tr("Downloading the VM…")
		case st.Assets == "local":
			assetLine = tr("VM assets: local override")
		case st.Assets != "":
			assetLine = tr("VM assets: v%s downloaded", st.Assets)
		default:
			assetLine = tr("VM assets download on the first amd64 build.")
		}
		if st.Downloading {
			rows = append(rows, widget.Row(
				stateDot,
				widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
			).Gap(8).Align(widget.AlignCenter))
			dlParts := []ggui.Widget{
				ui.Spinner().Size(12),
				widget.Expanded(ui.Caption(assetLine).Size(12)),
			}
			rows = append(rows, widget.Row(dlParts...).Gap(8).Align(widget.AlignCenter))
			if st.Total > 0 {
				frac := float64(st.Done) / float64(st.Total)
				rows = append(rows, widget.Box(ui.Progress(ggui.Const(min(1.0, max(0.0, frac)))).Height(4)))
			}
		} else {
			rows = append(rows, widget.Row(
				stateDot,
				widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
				widget.Expanded(ui.Caption(assetLine).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
		}

		// 3. Last error (including ErrNotPublished).
		if st.Err != "" {
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("amd64 builder error: %s", st.Err)).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		}

		// 4. Persistent disk size and Reset button.
		diskLabel := tr("Build cache disk: not created yet")
		if st.DiskBytes > 0 {
			diskLabel = tr("Build cache disk: %s", formatBytes(st.DiskBytes))
		}
		rows = append(rows, widget.Row(
			glyph(lucide.HardDrive, 13).Color(colorGray),
			widget.Expanded(ui.Caption(diskLabel).Size(12)),
			ui.Button(tr("Reset amd64 build cache"), m.AskResetAmd64Disk).Outline().Size(ui.ButtonExtraSmall).Disabled(v.resetting),
		).Gap(8).Align(widget.AlignCenter))

		// 5. buildx amd64 node status.
		if v.bx.Amd64Node {
			rows = append(rows, widget.Row(
				glyph(lucide.CircleCheck, 14).Color(colorGreen),
				widget.Expanded(ui.Caption(tr("docker buildx sends linux/amd64 builds to the amd64 node.")).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
		}

		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10).Radius(8).Border(1, colorBorder)
	})
}

// sandboxBuilderStatus shows the lazy BuildKit builder state (idle/running)
// and the buildx gvx-desktop builder status line in Settings → Docker.
func sandboxBuilderStatus(m *Model) ggui.Widget {
	type viewState struct {
		builder engine.BuilderState
		bx      BuildxStatus
	}
	combined := ggui.Combine(m.BuilderState, m.BuildxStatus, func(b engine.BuilderState, bx BuildxStatus) viewState {
		return viewState{builder: b, bx: bx}
	})
	return ggui.View(combined, func(v viewState) ggui.Widget {
		var rows []ggui.Widget

		// 1. Builder sandbox state (idle / starting / running).
		var stateDot ggui.Widget
		var stateLabel string
		switch v.builder.Status {
		case engine.BuilderRunning:
			stateDot = dot(colorGreen, 7)
			stateLabel = tr("Image builder: running")
		case engine.BuilderStarting:
			stateDot = dot(colorAmber, 7)
			stateLabel = tr("Image builder: starting…")
		default:
			stateDot = dot(colorGray, 7)
			stateLabel = tr("Image builder: idle")
		}
		rows = append(rows, widget.Row(
			stateDot,
			widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
			widget.Expanded(ui.Caption(tr("Starts automatically on the first build and stops when idle.")).Size(12)),
		).Gap(8).Align(widget.AlignCenter))
		if v.builder.Err != "" {
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Image builder error: %s", v.builder.Err)).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		}

		// 2. "Use the gvx-desktop builder" status line.
		bx := v.bx
		switch {
		case bx.Selected:
			rows = append(rows, widget.Row(
				glyph(lucide.CircleCheck, 14).Color(colorGreen),
				widget.Expanded(ui.Caption(tr("docker build uses the gvx-desktop builder for the gvx context.")).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
		case bx.NoCLI:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("docker was not found. Install the Docker CLI with the buildx plugin to use docker build.")).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		case bx.NoBuildx:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("The docker buildx plugin was not found. Install docker-buildx to use docker build with the gvx-desktop builder.")).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		case bx.NoCtx:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorBlue),
				widget.Expanded(ui.Caption(tr("The gvx-desktop builder is ready. Set up the gvx context, or run: docker buildx use gvx-desktop")).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		case bx.Err != "":
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Could not configure the gvx-desktop builder: %s", bx.Err)).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		default:
			rows = append(rows, widget.Row(
				glyph(lucide.Wrench, 14).Color(colorGray),
				widget.Expanded(ui.Caption(tr("Use the gvx-desktop builder")).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		}

		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10).Radius(8).Border(1, colorBorder)
	})
}

// sharedFoldersField lists the folders the Docker Engine shares at their
// own paths, with a button that removes each and one that adds another.
// In sandbox-engine mode it shows a dimmed note instead because rungv mounts
// host bind paths directly.
func sharedFoldersField(m *Model) ggui.Widget {
	return ggui.View(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func(se bool) ggui.Widget {
		if se {
			return widget.Column(
				widget.Text(tr("Shared folders")).Size(14).Weight(ggui.WeightMedium).Color(colorGray),
				ui.Caption(tr("Applies only to the Docker Engine sandbox. In sandbox-engine mode, bind mounts access host paths directly.")),
			).Gap(4).Stretch()
		}
		folders := ggui.Combine(m.Rows, m.Settings, m.sharedFoldersOf).WithEqual(slices.Equal[[]string])
		pick := ggui.State("")
		list := ggui.View(folders, func(list []string) ggui.Widget {
			add := ui.Button(tr("Add folder…"), func() { browseFolder(m, pick, m.ShareFolder) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.FolderPlus, 14))
			if len(list) == 0 {
				home := rungv.ExpandHome("~")
				return widget.Column(
					widget.Row(glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
						widget.Expanded(ui.Caption(tr("No folder is shared: bind mounts of folders on this computer are rejected."))),
					).Gap(8).Align(widget.AlignStart),
					widget.Row(ui.Button(tr("Share your home folder"), func() { m.ShareFolder(home) }).Size(ui.ButtonSmall), add).Gap(8),
				).Gap(8).Stretch()
			}
			rows := make([]ggui.Widget, 0, len(list)+1)
			for _, f := range list {
				rows = append(rows, widget.Row(
					glyph(lucide.Folder, 14).Color(colorGray),
					widget.Expanded(ui.Mono(tildePath(f)).Size(12).NoWrap().Ellipsis()),
					tip(iconButton(lucide.X, tr("Stop sharing %s", f), func() { m.UnshareFolder(f) }), tr("Stop sharing")),
				).Gap(8).Align(widget.AlignCenter))
			}
			rows = append(rows, widget.Row(add))
			return widget.Column(rows...).Gap(6).Stretch()
		})
		caption := ui.CaptionOf(ggui.Map(m.RungvFeatures, func(f rungv.Features) string {
			if f.BindAttach || f.KeepBlobs {
				return tr("The Docker Engine sees these folders at the same paths, so docker run -v and the bind mounts of a compose file work with what is in them. rungvx keeps the owners of files in a .gvx_meta file at the top of each. Changes apply live to a running engine without restarting.")
			}
			return tr("The Docker Engine sees these folders at the same paths, so docker run -v and the bind mounts of a compose file work with what is in them. rungvx keeps the owners of files in a .gvx_meta file at the top of each. The engine sees a change when it is restarted.")
		}))
		help := []ggui.Widget{list, caption}
		if runtime.GOOS == "windows" {
			help = append(help, widget.Row(glyph(lucide.Info, 14).Color(colorBlue),
				widget.Expanded(ui.Caption(tr("A folder such as C:\\Users\\me is at /c/Users/me in the engine. Paths written the Windows way in docker run -v and in compose files are converted on the way there."))),
			).Gap(8).Align(widget.AlignStart))
		}
		return widget.Column(
			widget.Text(tr("Shared folders")).Size(14).Weight(ggui.WeightMedium),
			widget.Column(help...).Gap(6).Stretch(),
		).Gap(6).Stretch()
	})
}

// kubeSettings chooses what the Kubernetes recipe's cluster runs.
func kubeSettings(m *Model) ggui.Widget {
	serviceLB := ggui.Bind(func() bool {
		m.Settings.Get() // follows the setting; the sandbox's own command is what counts
		m.Rows.Get()
		return m.ServiceLB()
	}, m.SetServiceLB)
	return settingsGroup(lucide.ShipWheel, "Kubernetes",
		settingRow(tr("Service load balancer"),
			tr("Runs k3s's ServiceLB, so a LoadBalancer service gets an address instead of staying pending. The node's ports 80 and 443 reach this computer at 127.0.0.1:%d and :%d. A running cluster changes when it is restarted.",
				recipes.ServiceLBHTTPPort, recipes.ServiceLBHTTPSPort),
			ui.Switch(serviceLB, "").Name(tr("Service load balancer"))),
	)
}

func localRegistryModeName(mode string) string {
	switch mode {
	case store.LocalRegistryOn:
		return tr("Always on")
	case store.LocalRegistryOff:
		return tr("Off")
	default:
		return tr("Auto")
	}
}

func localRegistryShortModeName(mode string) string {
	switch mode {
	case store.LocalRegistryOn:
		return tr("On")
	case store.LocalRegistryOff:
		return tr("Off")
	default:
		return tr("Auto")
	}
}

// localRegistrySettings configures GVX Desktop's built-in local OCI image
// registry mode and displays its status and storage usage.
func localRegistrySettings(m *Model) ggui.Widget {
	mode := ggui.Bind(func() string {
		return m.Settings.Get().LocalRegistryMode()
	}, m.SetLocalRegistryMode)
	modeRow := settingRow(
		tr("Built-in OCI registry"),
		tr("Auto runs the local registry on %s whenever Docker or Kubernetes is active. Pods pull from %s.", recipes.RegistryLocal(), recipes.RegistryHost()),
		ui.ToggleGroup(mode).
			Options([]string{store.LocalRegistryAuto, store.LocalRegistryOn, store.LocalRegistryOff}).
			Format(localRegistryModeName).
			Outline().Spacing(0).
			Name(tr("Built-in OCI registry")),
	)
	type statusView struct {
		st        LocalRegistryState
		mode      string
		busyPrune bool
	}
	sv := ggui.Derived(func() statusView {
		return statusView{
			st:        m.LocalRegistry.Get(),
			mode:      m.Settings.Get().LocalRegistryMode(),
			busyPrune: m.BusyOps.Get()["lreg:prune"],
		}
	})
	statusRow := ggui.View(sv, func(v statusView) ggui.Widget {
		st := v.st
		var dotColor color.Color = colorGray
		stateText := tr("Stopped · starts automatically with Docker or Kubernetes")
		switch {
		case st.LegacyActive:
			dotColor = colorAmber
			if st.LegacyPaused {
				stateText = tr("Legacy registry sandbox (paused)")
			} else {
				stateText = tr("Served by legacy registry sandbox on %s.", recipes.RegistryLocal())
			}
		case st.Err != "":
			dotColor = colorRed
			stateText = tr("Could not start on %s: %s", recipes.RegistryLocal(), st.Err)
		case st.Running:
			dotColor = colorGreen
			stateText = tr("Running on %s", recipes.RegistryLocal())
		case v.mode == store.LocalRegistryOff:
			dotColor = colorGray
			stateText = tr("Off")
		}
		storageText := tr("%d images · %s on disk", st.ImageCount, formatBytes(st.TotalBytes))
		if st.ReclaimableBytes > 0 {
			storageText = tr("%d images · %s on disk · %s reclaimable", st.ImageCount, formatBytes(st.TotalBytes), formatBytes(st.ReclaimableBytes))
		}
		var actions []ggui.Widget
		if st.Err != "" {
			actions = append(actions, ui.Button(tr("Retry"), m.RetryLocalRegistry).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14)))
		}
		if st.ReclaimableBytes > 0 {
			actions = append(actions, ui.Button(tr("Prune"), m.AskPruneLocalRegistry).Outline().Size(ui.ButtonSmall).Disabled(v.busyPrune))
		}
		actions = append(actions, ui.Button(tr("Open in Images"), m.OpenRegistry).Outline().Size(ui.ButtonSmall))
		return widget.Row(
			widget.Expanded(widget.Column(
				widget.Row(dot(dotColor, 7), widget.Expanded(widget.Text(stateText).Size(13).Weight(ggui.WeightMedium))).Gap(8).Align(widget.AlignCenter),
				ui.Caption(storageText).Size(12),
			).Gap(3).Stretch()),
			widget.Row(actions...).Gap(8).Align(widget.AlignCenter),
		).Gap(16).Align(widget.AlignCenter)
	})
	return settingsGroup(lucide.Warehouse, tr("Local registry"), modeRow, statusRow)
}

// dockerEndpointName words an endpoint of the Docker Engine's API.
func dockerEndpointName(v string) string {
	if v == DockerTCPEndpoint {
		return "TCP"
	}
	return tr("Unix socket")
}
