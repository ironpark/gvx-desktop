package ui

import (
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
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
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
	for i, r := range rows {
		if i > 0 {
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
	ggui.Watch(m.Settings, func(s storeSettings) {
		if ggui.Peek(path) != s.RungvPath {
			path.Set(s.RungvPath)
		}
		if ggui.Peek(cache) != s.CacheDir {
			cache.Set(s.CacheDir)
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
		headed(pageHeader(tr("Settings"), ui.Caption(tr("Where rungv lives and how the app treats running sandboxes."))), engineAlert(m), appUpdateAlert(m)),
		settingsGroup(lucide.Palette, tr("General"),
			settingRow(tr("Appearance"), tr("Light, dark, or as the system is."),
				ui.ToggleGroup[string](theme).Options([]string{"system", "light", "dark"}).Format(themeName).Icon(themeIcon).Outline().Spacing(0).Name(tr("Appearance"))),
			settingRow(tr("Language"), tr("The language the app is worded in."),
				ui.Select(language).Options(append([]string{LangSystem}, LanguageTags()...)).Format(languageName).MinWidth(180).Name(tr("Language"))),
			settingRow(tr("Start at login"),
				tr("Starts the app when you log in, in the tray without its window when the tray icon is shown. Sandboxes set to start with the app start then."),
				ui.Switch(ggui.Bind(m.Autostart.Get, m.SetAutostart), "")),
		),
		appUpdateSettings(m),
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		groups = append(groups, settingsGroup(lucide.PanelTop, tr("Tray"),
			settingRow(tr("Show GVX Desktop in the %s", trayPlace()),
				tr("An icon with the sandboxes and what they are doing, to start and stop them without the window."),
				ui.Switch(tray, "")),
			settingRow(tr("Keep running when the window closes"),
				tr("Closing the window leaves the app and its sandboxes running behind the icon. Without the icon, closing the window quits."),
				ui.Switch(hideOnClose, "").BindDisabled(noTray)),
		))
	}
	groups = append(groups,
		settingsGroup(lucide.Box, tr("Sandboxes"),
			settingRow(tr("Stop running sandboxes on quit without asking"),
				tr("gvx has no daemon: sandboxes run as long as this app does."),
				ui.Switch(stopOnQuit, "")),
			settingRow(tr("Stop timeout (seconds)"),
				tr("How long a sandbox gets to exit after an interrupt before it is killed."),
				widget.Box(ui.NumberInput(timeout).Range(1, 300).Step(1).Precision(0).Name(tr("Stop timeout"))).Width(120)),
		),
		dockerSettings(m),
		kubeSettings(m),
		settingsGroup(lucide.Cpu, tr("Engine"),
			settingField(tr("rungv path"), tr("Leave empty to search $GVX_RUNGV, PATH, ~/.gvx/bin and the app's own folder."), widget.Row(
				widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, ~/.gvx/bin, next to the app)")).Name(tr("rungv path")).
					OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = strings.TrimSpace(v) }) })),
				ui.Button(tr("Browse…"), func() {
					browseFile(m, path, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = v }) })
				}).Outline(),
			).Gap(8)),
			settingField(tr("Layer cache directory"), tr("Passed as -cache-dir. Named volumes live in a volumes folder beside it."), widget.Row(
				widget.Expanded(ui.TextField(cache).Placeholder(tr("rungv's default")).Name(tr("Layer cache directory")).
					OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = strings.TrimSpace(v) }) })),
				ui.Button(tr("Browse…"), func() {
					browseFolder(m, cache, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = v }) })
				}).Outline(),
			).Gap(8)),
			engineFacts(m),
		),
		settingsGroup(lucide.Download, tr("rungv updates"),
			widget.Column(
				settingRow(tr("Channel"),
					tr("Releases are gvx's published builds. From source clones a git repository and builds rungv on this computer, which needs git and Go."),
					ui.ToggleGroup[string](channel).Options([]string{ChannelRelease, ChannelSource}).Format(channelName).Outline().Spacing(0).
						BindDisabled(ggui.Map(m.Update, func(u Update) bool { return u.Installing })).Name(tr("Channel"))),
				sourceFields(m),
			).Gap(12).Stretch(),
			releaseRow(m),
			settingRow(tr("Look for a newer rungv when the app starts"),
				tr("Asks GitHub for the newest release of gvx. Nothing is installed until you say so."),
				ui.Switch(checkUpdates, "")),
		),
		aboutCard(m),
	)
	return page(groups...)
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
			actions = append(actions, tip(ui.Button(tr("Sign rungv"), m.SignRungv).Outline().Size(ui.ButtonSmall),
				tr("Ad-hoc signs rungv with the com.apple.security.hypervisor entitlement for the hvf engine.")))
		}
		return widget.Column(
			kvText(tr("Binary"), e.Path),
			kvText(tr("Version"), strings.TrimSpace(e.Version.Version+" "+e.Version.Commit)),
			kvText(tr("Built with"), e.Version.Go),
			kvText(tr("Backends"), tr("%s (default %s)", strings.Join(e.Info.AvailableBackends, ", "), e.Info.DefaultBackend)),
			kvText(tr("Host lockdown"), e.Info.HostLockdown),
			kvText(tr("Layer cache"), e.Info.LayerCacheDir),
			kvText(tr("Volumes"), e.Info.VolumeDir),
			widget.Padding(widget.Row(actions...).Gap(8), 6, 0, 0, 0),
		).Gap(8).Stretch()
	})
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

// releaseRow says what the channel's newest gvx is, and installs it.
func releaseRow(m *Model) ggui.Widget {
	type state struct {
		e Engine
		u Update
	}
	both := ggui.Combine(m.Engine, m.Update, func(e Engine, u Update) state { return state{e, u} })
	row := ggui.View(both, func(s state) ggui.Widget {
		e, u := s.e, s.u
		branch := u.BranchName()
		var text string
		var action ggui.Widget = widget.Box()
		install := tr("Install rungv")
		if u.Source {
			install = tr("Build and install")
		}
		switch {
		case u.Installing && u.Source:
			text = tr("Building rungv from %s", branch)
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Installing:
			text = tr("Installing rungv %s", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Checking:
			text = tr("Checking for the newest…")
		case !u.Known() && u.Err != "":
			text = tr("The newest could not be read: %s", u.Err)
		case !u.Known():
			text = tr("The newest has not been looked up.")
		case e.Path == "":
			text = tr("rungv %s can be installed into ~/.gvx/bin.", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Available(e):
			switch {
			case u.Source && u.Ahead > 0:
				text = tr("%s is %d commits past this computer's %s.", branch, u.Ahead, engineVersion(e))
			case u.Source:
				text = tr("%s is at %s; this computer has %s.", branch, u.Name(), engineVersion(e))
			case u.Newer(e):
				text = tr("rungv %s is available; this computer has %s.", u.Name(), engineVersion(e))
			default:
				text = tr("The newest release is rungv %s; this computer has %s.", u.Name(), engineVersion(e))
			}
			action = installButton(m, u, updateVerb(u, e)).Size(ui.ButtonSmall)
		case u.Source:
			text = tr("rungv is up to date with %s (%s).", branch, u.Name())
			action = ui.Button(tr("Build again"), m.InstallRungv).Outline().Size(ui.ButtonSmall)
		default:
			text = tr("rungv is up to date: %s is the newest release.", u.Name())
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
	found := ui.CaptionOf(ggui.Map(m.Settings, func(s storeSettings) string {
		p, ok := m.DockerCLI()
		switch {
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

	return settingsGroup(lucide.Container, "Docker",
		widget.Column(
			settingField(tr("docker path"), tr("The docker command the app sets a context up with. Leave empty to look where a terminal would."), widget.Row(
				widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, Homebrew, Docker Desktop)")).Name(tr("docker path")).OnCommit(setPath)),
				ui.Button(tr("Browse…"), func() { browseFile(m, path, setPath) }).Outline(),
			).Gap(8)),
			found,
		).Gap(4).Stretch(),
		widget.Column(
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
			ui.Caption(tr("A Docker CLI on Windows reaches a Unix socket only from version 29 on. TCP on this computer's loopback address works with any version, on every system.")),
			host,
			proxyErr,
		).Gap(6).Stretch(),
		sharedFoldersField(m),
		dockerConfigField(m),
		sandboxEngineSettings(m),
	)
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
		ui.Caption(tr("Saved to %s. %s stays among the insecure registries, so the engine can push to the image registry recipe.", tildePath(recipes.DockerConfigFile()), recipes.RegistryHost())),
		field,
		errText,
		ui.Caption(tr("dockerd applies debug, labels, registry-mirrors, insecure-registries, max-concurrent-downloads and a few more on the fly; any other change takes a restart of the engine.")),
	).Gap(10).Stretch()).Title(tr("Docker Engine configuration")).Width(620).Footer(
		ui.Button(tr("Cancel"), func() { m.DockerConfigOpen.Set(false) }).Outline(),
		ui.Button(tr("Save"), m.SaveDockerConfig),
	)
}

// sandboxEngineSettings is the Experimental subsection under Settings → Docker.
func sandboxEngineSettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().SandboxEngine }, m.SetSandboxEngine)
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
		return sandboxBuilderStatus(m)
	})
	notes := ggui.View(m.RungvFeatures, func(f rungv.Features) ggui.Widget {
		var rows []ggui.Widget
		if f.Control {
			rows = append(rows,
				widget.Row(
					widget.Padding(glyph(lucide.Check, 13).Color(colorGreen), 2, 0, 0, 0),
					widget.Expanded(ui.Caption(tr("Works: docker run (-d, -i, -t, --rm), exec, cp, pause, top, update, stats, build (BuildKit and Compose), ps, logs, stop, start, rm, ports on 127.0.0.1, volumes, bind mounts, pull/images, networks between containers, and Compose with several services.")).Size(12)),
				).Gap(8).Align(widget.AlignStart),
				widget.Row(
					widget.Padding(glyph(lucide.CircleMinus, 13).Color(colorGray), 2, 0, 0, 0),
					widget.Expanded(ui.Caption(tr("Not yet supported: commit, export and diff.")).Size(12)),
				).Gap(8).Align(widget.AlignStart),
			)
		} else {
			rows = append(rows,
				widget.Row(
					widget.Padding(glyph(lucide.Check, 13).Color(colorGreen), 2, 0, 0, 0),
					widget.Expanded(ui.Caption(tr("Works: docker run (-d, -i, -t, --rm), build (BuildKit and Compose), ps, logs, stop, start, rm, ports on 127.0.0.1, volumes, bind mounts, pull/images, networks between containers, and Compose with several services.")).Size(12)),
				).Gap(8).Align(widget.AlignStart),
				widget.Row(
					widget.Padding(glyph(lucide.CircleMinus, 13).Color(colorGray), 2, 0, 0, 0),
					widget.Expanded(ui.Caption(tr("Not yet supported: exec and cp.")).Size(12)),
				).Gap(8).Align(widget.AlignStart),
				widget.Row(
					widget.Padding(glyph(lucide.Info, 13).Color(colorAmber), 2, 0, 0, 0),
					widget.Expanded(ui.Caption(tr("Update rungv to v0.20 or later for exec, cp, pause, top and update.")).Size(12)),
				).Gap(8).Align(widget.AlignStart),
			)
		}
		rows = append(rows, widget.Row(
			widget.Padding(glyph(lucide.Info, 13).Color(colorBlue), 2, 0, 0, 0),
			widget.Expanded(ui.Caption(tr("Turning this on routes docker build through buildx (docker buildx install) and selects the gvx-desktop builder for the gvx context and engine endpoint; turning it off puts both back.")).Size(12)),
		).Gap(8).Align(widget.AlignStart))
		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10, 12).Radius(8).Fill(colorSurfaceInset).Border(1, colorBorder)
	})
	return widget.Column(
		widget.Row(
			glyph(lucide.FlaskConical, 14).Color(colorAmber),
			widget.Text(tr("Experimental")).Size(14).Weight(ggui.WeightMedium),
		).Gap(6).Align(widget.AlignCenter),
		settingRow(tr("Run containers as sandboxes, without a Docker Engine"),
			tr("GVX Desktop answers the Docker API directly and runs each container as its own gvx sandbox. Takes effect immediately; turning it off stops those containers."),
			ui.Switch(enabled, "")),
		notes,
		builderStatus,
		runningNote,
		widget.Padding(ui.Divider(), 6, 0),
		amd64BuilderSettings(m),
	).Gap(8).Stretch()
}

// amd64BuilderSettings is the "amd64 builds with Rosetta" subsection under
// Settings → Docker → Experimental.
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
		return ui.Caption(tr("Needs the sandbox engine above."))
	})
	statusBox := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine && s.Amd64Builds }), func() ggui.Widget {
		return amd64BuilderStatus(m)
	})
	return widget.Column(
		settingRow(tr("amd64 builds with Rosetta"),
			tr("Builds for --platform linux/amd64 run in a small Linux VM with Rosetta for Linux. The images can be loaded and pushed, not run yet. The VM starts on the first amd64 build and stops when idle."),
			ui.Switch(enabled, "").Name(tr("amd64 builds with Rosetta")).BindDisabled(disabled)),
		needsEngine,
		amd64Requirements(m),
		statusBox,
	).Gap(6).Stretch()
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
		help := []ggui.Widget{list, ui.Caption(tr("The Docker Engine sees these folders at the same paths, so docker run -v and the bind mounts of a compose file work with what is in them. rungv keeps the owners of files in a .gvx_meta file at the top of each. The engine sees a change when it is restarted."))}
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
			ui.Switch(serviceLB, "")),
	)
}

// dockerEndpointName words an endpoint of the Docker Engine's API.
func dockerEndpointName(v string) string {
	if v == DockerTCPEndpoint {
		return "TCP"
	}
	return tr("Unix socket")
}
