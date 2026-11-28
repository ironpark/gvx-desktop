package ui

import (
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// recipeStatus is the state of a preset's sandbox, or ok false when it
// has not been set up.
func recipeRow(m *Model, key string) ggui.Readable[Row] {
	return ggui.Map(m.Rows, func(rows []Row) Row {
		r, _ := m.RecipeSandbox(rows, key)
		return r
	}).WithEqual(Row.Equal)
}

// recipeHero is the card at the top of a recipe page: what it is, its
// state and its main action.
func recipeHero(m *Model, key string, row ggui.Readable[Row], title, what string, icon *icons.Glyph) ggui.Widget {
	return ggui.View(row, func(r Row) ggui.Widget {
		var actions []ggui.Widget
		var state ggui.Widget
		switch {
		case r.Spec.ID == "":
			state = ui.Caption(tr("Not set up yet."))
			actions = append(actions, ui.Button(tr("Set up and start"), func() { m.CreateFromRecipe(key) }).Icon(glyph(lucide.Play, 14)))
		case r.State.Status.Active() && r.State.Paused:
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button(tr("Logs"), func() { m.ShowSandbox(r.Spec.ID, 0) }).Outline().Icon(glyph(lucide.ScrollText, 14)),
				ui.Button(tr("Resume"), func() { m.ResumeSandbox(r.Spec.ID) }).Icon(glyph(lucide.Play, 14)),
				ui.Button(tr("Stop"), func() { m.StopSandbox(r.Spec.ID) }).Outline().Icon(glyph(lucide.Square, 14)).Disabled(r.State.Status == sandbox.Stopping),
			)
		case r.State.Status.Active():
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button(tr("Logs"), func() { m.ShowSandbox(r.Spec.ID, 0) }).Outline().Icon(glyph(lucide.ScrollText, 14)),
			)
			if r.State.Control && r.State.Status != sandbox.Stopping {
				actions = append(actions, ui.Button(tr("Pause"), func() { m.PauseSandbox(r.Spec.ID) }).Outline().Icon(glyph(lucide.Pause, 14)))
			}
			actions = append(actions,
				ui.Button(tr("Stop"), func() { m.StopSandbox(r.Spec.ID) }).Outline().Icon(glyph(lucide.Square, 14)).Disabled(r.State.Status == sandbox.Stopping),
			)
		default:
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button(tr("Settings"), func() { m.EditSandbox(r.Spec.ID) }).Outline(),
				ui.Button(tr("Start"), func() { m.StartSandbox(r.Spec.ID) }).Icon(glyph(lucide.Play, 14)),
			)
		}
		uptimeText := widget.Box()
		if r.State.Status.Active() {
			started := r.State.StartedAt
			uptimeText = widget.Box(ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string { return tr("up %s", uptime(started, now)) })).Size(12))
		}
		return ui.Card(widget.Row(
			widget.Box(glyph(icon, 26).Color(colorBlue)).Pad(12).Radius(12).Fill(rgbA(0x3b82f6, 0x22)),
			widget.Expanded(widget.Column(
				widget.Row(ui.Heading(title).Size(18), state, uptimeText).Gap(12).Align(widget.AlignCenter),
				ui.Caption(what),
			).Gap(6)),
			widget.Row(actions...).Gap(8),
		).Gap(16).Align(widget.AlignCenter))
	})
}

// ---- Docker -------------------------------------------------------------

func dockerPage(m *Model) ggui.Widget {
	row := recipeRow(m, recipes.Docker)
	sandboxMode := ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine })

	version := ui.CaptionOf(ggui.Derived(func() string {
		open, se, v := m.ContainersOpen.Get(), sandboxMode.Get(), m.DockerVersion.Get()
		if open && v.Version != "" {
			if se {
				return tr("Sandbox Engine %s (API %s) · %s/%s · kernel %s", v.Version, v.APIVersion, v.OS, v.Arch, v.KernelVersion)
			}
			return tr("Docker Engine %s (API %s) · %s/%s · kernel %s", v.Version, v.APIVersion, v.OS, v.Arch, v.KernelVersion)
		}
		if se {
			return tr("Containers run directly as gvx sandboxes; the docker CLI talks to GVX Desktop.")
		}
		return tr("Compatibility mode: containers run inside a docker:dind sandbox.")
	}))

	header := pageHeader("Docker", version,
		dockerModeToggle(m, ui.ButtonDefault),
		tip(iconButton(lucide.RefreshCw, tr("Refresh containers"), func() {
			m.LoadDocker()
			m.LoadDockerImages()
			m.LoadDockerNetworks()
			m.CheckLink(m.DockerLink)
		}), tr("Refresh")),
		ui.Button(tr("Settings"), func() { m.Go(PageSettings) }).Outline().Icon(glyph(lucide.Settings, 16)),
	)

	body := ggui.View(sandboxMode, func(se bool) ggui.Widget {
		if se {
			return sandboxDockerBody(m, row)
		}
		return compatDockerBody(m, row)
	})

	return page(header, body)
}

// sandboxDockerBody is the default Docker page: native sandbox engine status,
// BuildKit builder status, quick resource metrics, CLI setup, and notes.
func sandboxDockerBody(m *Model, dindRow ggui.Readable[Row]) ggui.Widget {
	dindActive := ggui.Map(dindRow, func(r Row) string {
		if r.Spec.ID != "" && r.State.Status.Active() {
			return r.Spec.ID
		}
		return ""
	})
	dindNotice := ggui.View(dindActive, func(id string) ggui.Widget {
		if id == "" {
			return widget.Box()
		}
		return noticeBanner(lucide.Info, 0xf59e0b, false,
			tr("Compatibility docker:dind sandbox is still running"),
			tr("Your Docker Engine sandbox is still running, but docker no longer reaches it while this mode is on. Stop it manually if you do not need it."),
			ui.Button(tr("Stop"), func() { m.StopSandbox(id) }).Outline(),
		)
	})

	type heroState struct {
		engine   Engine
		status   SandboxEngineStatus
		proxyErr string
	}
	hs := ggui.Derived(func() heroState {
		return heroState{
			engine:   m.Engine.Get(),
			status:   m.SandboxEngineStatus.Get(),
			proxyErr: m.DockerProxyErr.Get(),
		}
	})
	hero := ggui.View(hs, func(s heroState) ggui.Widget {
		state := stateLabel(colorGreen, tr("Running"), false, "")
		var actions []ggui.Widget
		switch {
		case s.engine.Checked && !s.engine.Ready():
			state = stateLabel(colorRed, tr("Engine unavailable"), false, s.engine.Err)
			actions = append(actions, ui.Button(tr("Retry"), func() { m.DetectEngine(nil) }).Outline().Icon(glyph(lucide.RefreshCw, 14)))
		case s.status.Opening:
			state = stateLabel(colorAmber, tr("Starting"), true, "")
		case s.status.Err != "":
			state = stateLabel(colorRed, tr("Failed"), false, s.status.Err)
			actions = append(actions, ui.Button(tr("Retry"), m.RetrySandboxEngine).Outline().Icon(glyph(lucide.RefreshCw, 14)))
		case s.proxyErr != "":
			state = stateLabel(colorAmber, tr("Listener failed"), false, s.proxyErr)
			actions = append(actions, ui.Button(tr("Retry"), m.RetrySandboxEngine).Outline().Icon(glyph(lucide.RefreshCw, 14)))
		default:
			actions = append(actions,
				ui.Button(tr("Pull image"), func() { m.OpenDockerPull("") }).Outline().Icon(glyph(lucide.Download, 14)),
				ui.Button(tr("Open Containers"), func() { m.Go(PageContainers) }).Icon(glyph(lucide.Container, 14)),
			)
		}
		hostLine := ui.CaptionOf(ggui.Map(m.DockerEndpoint, func(h string) string {
			return tr("Each container runs as its own unprivileged gvx sandbox. The Docker API is served on %s.", displayDockerHost(h))
		}))
		top := widget.Row(
			widget.Box(glyph(dockerIcon, 26).Color(colorBlue)).Pad(12).Radius(12).Fill(rgbA(0x3b82f6, 0x22)),
			widget.Expanded(widget.Column(
				widget.Row(ui.Heading(tr("Sandbox engine")).Size(18), state).Gap(12).Align(widget.AlignCenter),
				hostLine,
			).Gap(6)),
			widget.Row(actions...).Gap(8),
		).Gap(16).Align(widget.AlignCenter)
		return ui.Card(widget.Column(
			top,
			sandboxBuilderStatus(m),
			ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.Amd64Builds }), func() ggui.Widget {
				return amd64BuilderStatus(m)
			}),
			ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.MicroVMContainers }), func() ggui.Widget {
				return microVMStatus(m)
			}),
		).Gap(12).Stretch())
	})

	return widget.Column(
		dindNotice,
		hero,
		dockerMetricsRow(m),
		widget.Responsive(900,
			widget.Row(widget.Flex(dockerCLICard(m), 1), widget.Flex(sandboxEngineNotesCard(m), 1)).Gap(16).Stretch(),
			widget.Column(dockerCLICard(m), sandboxEngineNotesCard(m)).Gap(16).Stretch(),
		),
	).Gap(16).Stretch()
}

// compatDockerBody is the Docker page in compatibility mode (docker:dind).
func compatDockerBody(m *Model, row ggui.Readable[Row]) ggui.Widget {
	compatBanner := noticeBanner(lucide.Info, 0x3b82f6, false,
		tr("Compatibility mode (docker:dind)"),
		tr("Containers run inside an unmodified dockerd sandbox. Switch back to the default sandbox engine to run each container directly as a gvx sandbox."),
		ui.Button(tr("Use sandbox engine"), func() { m.SwitchSandboxEngine(true) }).Outline(),
	)
	compatHero := ggui.View(m.DockerEndpoint, func(h string) ggui.Widget {
		return recipeHero(m, recipes.Docker, row, "docker:dind",
			tr("An unmodified dockerd in one sandbox. Images and containers persist in the gvx-docker volume; the API is published on %s.", displayDockerHost(h)),
			dockerIcon)
	})
	return widget.Column(
		compatBanner,
		compatHero,
		dockerMetricsRow(m),
		widget.Responsive(900,
			widget.Row(widget.Flex(dockerCLICard(m), 1), widget.Flex(dockerNotesCard(m), 1)).Gap(16).Stretch(),
			widget.Column(dockerCLICard(m), dockerNotesCard(m)).Gap(16).Stretch(),
		),
	).Gap(16).Stretch()
}

// dockerMetricsRow shows Containers, Networks, and Images overview cards in
// the same style as the Dashboard metrics strip.
func dockerMetricsRow(m *Model) ggui.Widget {
	big := func(r ggui.Readable[string]) ggui.Widget { return ui.TitleOf(r).Size(24) }
	pair := func(count, sub ggui.Readable[string]) ggui.Widget {
		return widget.Row(
			big(count),
			ui.CaptionOf(sub).Size(12).NoWrap().Ellipsis(),
		).Gap(8).Align(widget.AlignBaseline)
	}
	ctrRunning := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return "—"
		}
		l := m.DockerContainers.Get()
		if l.Err != "" {
			return "!"
		}
		if !l.Loaded {
			return "…"
		}
		return itoa(m.RunningContainers.Get())
	})
	ctrSub := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return m.ContainersAccess.Get().Reason()
		}
		l := m.DockerContainers.Get()
		if l.Err != "" {
			return l.Err
		}
		if !l.Loaded {
			return ""
		}
		return tr("of %s", itoa(len(l.Items)))
	})
	netCount := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return "—"
		}
		l := m.DockerNetworks.Get()
		if l.Err != "" {
			return "!"
		}
		if !l.Loaded {
			return "…"
		}
		return itoa(len(l.Items))
	})
	netSub := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return ""
		}
		l := m.DockerNetworks.Get()
		if l.Err != "" {
			return l.Err
		}
		if !l.Loaded {
			return ""
		}
		user := 0
		for _, n := range l.Items {
			if !n.Builtin() {
				user++
			}
		}
		return trn(user, "%d custom", "%d custom")
	})
	imgCount := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return "—"
		}
		l := m.DockerImages.Get()
		if l.Err != "" {
			return "!"
		}
		if !l.Loaded {
			return "…"
		}
		return itoa(len(l.Items))
	})
	imgSub := ggui.Derived(func() string {
		if !m.ContainersOpen.Get() {
			return ""
		}
		l := m.DockerImages.Get()
		if l.Err != "" {
			return l.Err
		}
		if !l.Loaded || len(l.Items) == 0 {
			return ""
		}
		return formatBytes(dockerImagesTotalSize(l.Items))
	})
	return ggui.View(m.ContainersOpen, func(open bool) ggui.Widget {
		var openCtr, openNet func()
		if open {
			openCtr = func() { m.Go(PageContainers) }
			openNet = func() { m.Go(PageNetworks) }
		}
		cards := []ggui.Widget{
			metric(tr("Containers"), pair(ctrRunning, ctrSub), lucide.Container, openCtr),
			metric(tr("Networks"), pair(netCount, netSub), lucide.Network, openNet),
			metric(tr("Images"), pair(imgCount, imgSub), lucide.Layers, func() { m.GoImages(ImagesDocker) }),
		}
		return widget.Responsive(760, widget.Grid(3, cards...).Gap(12), widget.Column(cards...).Gap(12).Stretch())
	})
}

func sandboxEngineNotesCard(m *Model) ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return widget.Column(widget.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	featuresNote := ggui.View(m.RungvFeatures, func(f rungv.Features) ggui.Widget {
		var text string
		switch {
		case f.Control && f.KeepBlobs:
			text = tr("Supports docker run, exec, cp, pause, top, update, stats (with BLOCK I/O), diff, export, commit, build (BuildKit and Compose), push, offline save, networks, and host Unix socket bind mounts (including /var/run/docker.sock).")
		case f.Control:
			text = tr("Works: docker run (-d, -i, -t, --rm), exec, cp, pause, top, update, stats, build (BuildKit and Compose), ps, logs, stop, start, rm, ports on 127.0.0.1, volumes, bind mounts, pull/images, networks between containers, and Compose with several services.")
		default:
			text = tr("Update rungvx to v0.20 or later for exec, cp, diff, export, commit, pause, top and update.")
		}
		return note(tr("Capabilities"), text)
	})
	return ui.Card(
		ui.CardHeader(tr("Good to know"), tr("How containers run in the default sandbox engine.")),
		ui.CardContent(widget.Column(
			note(tr("Host bind mounts"), tr("Host paths in docker run -v and Compose bind mounts mount directly into each container's sandbox — no shared-folder list is needed.")),
			note(tr("Ports"), tr("Published ports default to 127.0.0.1 only. Opt in to local network (0.0.0.0) publishing in Settings → Docker.")),
			note(tr("Architecture"), tr("The engine runs images for this CPU (linux/%s) natively.", goarch())),
			featuresNote,
		).Gap(10).Stretch()),
	)
}

// displayDockerHost shortens a unix:// host path under home with ~ for compact
// prose display, without altering the real DOCKER_HOST used in CLI commands.
func displayDockerHost(host string) string {
	if p, ok := strings.CutPrefix(host, "unix://"); ok {
		return "unix://" + tildePath(p)
	}
	return host
}

func dockerCLICard(m *Model) ggui.Widget {
	return ui.Card(
		ui.CardHeader(tr("Use it from a terminal"), tr("docker on this machine reaches the engine through a context named gvx.")),
		ui.CardContent(widget.Column(
			bundledDockerBanner(m),
			linkPanel(m, m.DockerLink, func(v LinkView) []ggui.Widget {
				return dockerCommands(m, v, copyable(m, "docker run --rm -p 9000:80 nginx:alpine", tr("example")))
			}),
		).Gap(8).Stretch()),
	)
}

func bundledDockerBanner(m *Model) ggui.Widget {
	return ggui.View(m.BundledDocker, func(s BundledDockerState) ggui.Widget {
		if !s.Available || !s.Using {
			return widget.Box()
		}
		bin := tildePath(s.BinDir)
		prof := tildePath(s.Profile)
		title := widget.Text(tr("Bundled Docker CLI (%s)", s.ToolsList())).Size(13).Weight(ggui.WeightMedium)
		if s.InPATH {
			return widget.Box(widget.Row(
				widget.Padding(glyph(lucide.CircleCheck, 16).Color(colorGreen), 1, 0, 0, 0),
				widget.Expanded(widget.Column(
					title,
					ui.Caption(tr("Available in terminals via %s.", bin)).Size(12),
				).Gap(3).Stretch()),
			).Gap(10).Align(widget.AlignStart)).Pad(12).Radius(8).Border(1, colorBorder)
		}
		return widget.Box(widget.Row(
			widget.Padding(glyph(lucide.Terminal, 16).Color(colorBlue), 1, 0, 0, 0),
			widget.Expanded(widget.Column(
				title,
				ui.Caption(tr("Add %s to %s to run docker from any new terminal.", bin, prof)).Size(12),
				widget.Padding(widget.Row(
					ui.Button(tr("Add to PATH"), m.AddBundledDockerToPATH).Outline().Size(ui.ButtonSmall).Disabled(s.Busy),
				), 4, 0, 0, 0),
			).Gap(3).Stretch()),
		).Gap(10).Align(widget.AlignStart)).Pad(12).Radius(8).Border(1, colorBorder)
	})
}

// dockerCommands are the commands that set the gvx context up by hand, with
// extra after the one that selects it.
func dockerCommands(m *Model, v LinkView, extra ...ggui.Widget) []ggui.Widget {
	verb := "create"
	if v.C.Exists {
		verb = "update"
	}
	var out []ggui.Widget
	if bd := ggui.Peek(m.BundledDocker); bd.Available && bd.Using && !bd.InPATH {
		out = append(out, copyable(m, dockercli.ManualPathCommand(), tr("PATH")))
	}
	out = append(out,
		copyable(m, "docker context "+verb+" gvx --docker host="+v.Want, "docker context "+verb),
		copyable(m, "docker context use gvx", "docker context use"),
	)
	out = append(out, extra...)
	return append(out, copyable(m, "docker context use "+v.Back, tr("switch back")))
}

// linkPanel tells whether a command-line tool on this machine reaches a
// recipe's engine, and offers the one action that would make it so; the
// commands that do the same by hand are folded away under it.
func linkPanel(m *Model, l *Link, commands func(LinkView) []ggui.Widget) ggui.Widget {
	type linkPanelState struct {
		v  LinkView
		bd BundledDockerState
	}
	combined := ggui.Combine(l.View, m.BundledDocker, func(v LinkView, bd BundledDockerState) linkPanelState {
		return linkPanelState{v: v, bd: bd}
	})
	status := ggui.View(combined, func(s linkPanelState) ggui.Widget {
		v := s.v
		mark, title, detail := linkWordsFor(l, v, s.bd)
		text := []ggui.Widget{widget.Text(title).Size(13).Weight(ggui.WeightMedium)}
		if detail != "" {
			text = append(text, ui.Caption(detail).Size(12))
		}
		if v.C.Env != "" && v.Status > ContextFailed {
			text = append(text, widget.Row(glyph(lucide.Info, 12).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("%s is set in the environment the app started from. Where it is set, %s follows it, not this context.", v.C.Env, l.Tool)).Size(12)),
			).Gap(6).Align(widget.AlignStart))
		}
		if b := linkButton(m, l, v); b != nil {
			text = append(text, widget.Padding(widget.Row(b), 4, 0, 0, 0))
		}
		return widget.Row(
			widget.Padding(mark, 1, 0, 0, 0),
			widget.Expanded(widget.Column(text...).Gap(3).Stretch()),
			tip(iconButton(lucide.RefreshCw, tr("Check %s again", l.Tool), func() { m.CheckLink(l) }), tr("Check again")),
		).Gap(10).Align(widget.AlignStart)
	})
	cmds := ggui.View(combined, func(s linkPanelState) ggui.Widget {
		return widget.Padding(widget.Column(commands(s.v)...).Gap(8).Stretch(), 4, 0, 0, 0)
	})
	return widget.Column(
		widget.Box(status).Pad(12).Radius(8).Border(1, colorBorder),
		ui.Collapsible(m.LinkCommandsOpen, tr("Commands to type instead"), cmds),
	).Gap(8).Stretch()
}

// linkWordsFor returns a context's mark, its state in a few words, and what
// those leave to say.
func linkWordsFor(l *Link, v LinkView, bd BundledDockerState) (ggui.Widget, string, string) {
	mark := func(g *icons.Glyph, c color.Color) ggui.Widget { return glyph(g, 16).Color(c) }
	docker := l.Tool == "docker"
	switch v.Status {
	case ContextUnknown:
		return ui.Spinner().Size(14), tr("Checking %s…", l.Tool), ""
	case ContextNoCLI:
		if docker {
			return mark(lucide.CircleX, colorGray), tr("The docker command was not found."),
				tr("Install the Docker CLI alone (no local engine is needed), or name it in Settings → Docker.")
		}
		return mark(lucide.CircleX, colorGray), tr("The kubectl command was not found."), tr("Install kubectl, then check again.")
	case ContextPending:
		return mark(lucide.Clock, colorAmber), tr("The cluster has not written its kubeconfig yet."),
			tr("The kubeconfig appears at %s once the server is up.", recipes.Kubeconfig())
	case ContextFailed:
		return mark(lucide.TriangleExclamationPoint, colorAmber), tr("%s did not answer", l.Tool), v.C.Err
	case ContextMissing:
		if docker {
			return mark(lucide.CircleDashed, colorGray), tr("No %s context yet.", l.Name), tr("Create it to point docker at %s.", v.Want)
		}
		return mark(lucide.CircleDashed, colorGray), tr("No %s context yet.", l.Name),
			tr("kubectl adds it to %s; the file as it was is kept as %s.", tildePath(UserKubeconfig()), filepath.Base(KubeconfigBackup()))
	case ContextStale:
		if docker {
			return mark(lucide.TriangleExclamationPoint, colorAmber), tr("The %s context is out of date.", l.Name),
				tr("It points at %s; the engine is at %s.", v.C.Host, v.Want)
		}
		return mark(lucide.TriangleExclamationPoint, colorAmber), tr("The %s context is out of date.", l.Name),
			tr("It does not match the cluster's kubeconfig: the cluster was made anew, or its certificates changed.")
	case ContextElsewhere:
		if v.C.Current == "" {
			return mark(lucide.CircleDot, colorBlue), tr("The %s context exists; %s uses no context.", l.Name, l.Tool), ""
		}
		return mark(lucide.CircleDot, colorBlue), tr("The %s context exists; %s is using %s.", l.Name, l.Tool, v.C.Current), ""
	}
	if docker {
		if bd.Available && bd.Using && !bd.InPATH {
			return mark(lucide.Info, colorBlue), tr("docker uses the gvx context."),
				tr("Add %s to your PATH above to run docker from any terminal.", tildePath(bd.BinDir))
		}
		return mark(lucide.CircleCheck, colorGreen), tr("docker uses the gvx context."), tr("docker in a terminal reaches this engine.")
	}
	return mark(lucide.CircleCheck, colorGreen), tr("kubectl uses the gvx-k3s context."), tr("kubectl in a terminal reaches this cluster.")
}

// linkButton is the one action that fits a context's state, or nil.
func linkButton(m *Model, l *Link, v LinkView) *ui.ButtonWidget {
	var b *ui.ButtonWidget
	switch v.Status {
	case ContextNoCLI:
		if l.Tool != "docker" {
			return nil
		}
		b = ui.Button(tr("Open Settings"), func() { m.Go(PageSettings) }).Outline()
	case ContextMissing:
		b = ui.Button(tr("Create & use context"), func() { m.SetUpLink(l) })
	case ContextStale:
		b = ui.Button(tr("Update the context"), func() { m.UpdateLink(l) })
	case ContextElsewhere:
		b = ui.Button(tr("Use the %s context", l.Name), func() { m.UseLink(l) })
	case ContextInUse:
		label := tr("Switch back to %s", v.Back)
		if v.Back == "" {
			label = tr("Stop using %s", l.Name)
		}
		b = ui.Button(label, func() { m.SwitchLinkBack(l) }).Outline()
	default:
		return nil
	}
	return b.Size(ui.ButtonSmall).Disabled(v.C.Busy)
}

func dockerNotesCard(m *Model) ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return widget.Column(widget.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	limitsText := ggui.Map(m.RungvFeatures, func(f rungv.Features) string {
		if f.KeepBlobs {
			return tr("docker run -m and --cpus are enforced, and docker stats reports per-container BLOCK I/O.")
		}
		return tr("docker run -m and --cpus are enforced; BLOCK I/O reads 0B because the guest has no io controller.")
	})
	return ui.Card(
		ui.CardHeader(tr("Good to know"), tr("Limits that come from running dockerd in a user-space kernel.")),
		ui.CardContent(widget.Column(
			note(tr("Paths in docker run -v"), tr("A -v path names a path in the sandbox. The folders shared in Settings → Docker, your home folder unless you changed them, are at the same paths there, so -v and compose bind mounts of what is in them work; bind mounts outside them are rejected.")),
			note(tr("Ports"), tr("A port a container publishes reaches this computer at 127.0.0.1 under the same number once the container listens on it, unless something here has that port. The container's Connect tab says which.")),
			note(tr("Architecture"), tr("The engine runs images for this CPU (linux/%s) natively.", goarch())),
			widget.Column(widget.Text(tr("Limits")).Size(13).Weight(ggui.WeightMedium), ui.CaptionOf(limitsText)).Gap(2),
		).Gap(10).Stretch()),
	)
}

// ---- Kubernetes ---------------------------------------------------------

func kubernetesPage(m *Model) ggui.Widget {
	row := recipeRow(m, recipes.Kubernetes)
	ggui.Effect(func() ggui.Cleanup {
		st := row.Get().State
		if !st.Status.Active() || st.Paused {
			return nil
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					m.post(m.CheckKubeconfig)
				}
			}
		}()
		return func() { close(stop) }
	})
	cfg := recipes.Kubeconfig()
	kubeconfig := ggui.View(m.KubeconfigReady, func(ok bool) ggui.Widget {
		if !ok {
			return widget.Row(glyph(lucide.Clock, 14).Color(colorAmber), widget.Expanded(ui.Caption(tr("The kubeconfig appears at %s once the server is up.", cfg)))).Gap(8).Align(widget.AlignCenter)
		}
		return widget.Row(glyph(lucide.CircleCheck, 14).Color(colorGreen), widget.Expanded(ui.Caption(tr("kubeconfig written to %s", cfg)).NoWrap().Ellipsis()),
			ui.Button(tr("Reveal"), func() { m.Open(cfg) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(widget.AlignCenter)
	})
	return page(
		pageHeader("Kubernetes", ui.Caption(tr("Unmodified k3s nodes with their kubelet, containerd and flannel, each in an unprivileged sandbox of its own.")),
			tip(iconButton(lucide.RefreshCw, tr("Refresh"), func() {
				m.CheckKubeconfig()
				m.LoadPods()
				m.CheckLink(m.KubeLink)
			}), tr("Refresh")),
		),
		recipeHero(m, recipes.Kubernetes, row, tr("k3s cluster"),
			tr("API server on 127.0.0.1:6443, NodePorts 30000–30019 on the host, cluster state in the k3s volume. Starting takes up to two minutes."),
			lucide.ShipWheel),
		nodesCard(m),
		widget.Column(
			sectionTitle(tr("Pods"), tr("Pods run on the cluster's nodes, each in a sandbox of its own.")),
			podsSummary(m),
		).Gap(10).Stretch(),
		widget.Responsive(900,
			widget.Row(widget.Flex(kubectlCard(m, kubeconfig), 1), widget.Flex(kubeNotesCard(), 1)).Gap(16).Stretch(),
			widget.Column(kubectlCard(m, kubeconfig), kubeNotesCard()).Gap(16).Stretch(),
		),
	)
}

func kubectlCard(m *Model, status ggui.Widget) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return ui.Card(
		ui.CardHeader(tr("Use it with kubectl"), tr("kubectl on this machine reaches the cluster through a context named gvx-k3s.")),
		ui.CardContent(widget.Column(
			status,
			linkPanel(m, m.KubeLink, func(LinkView) []ggui.Widget {
				return []ggui.Widget{
					copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
					copyable(m, "kubectl get nodes", "kubectl get nodes"),
					copyable(m, "kubectl create deployment web --image=nginx:alpine", tr("example deployment")),
					copyable(m, "kubectl expose deployment web --type=NodePort --port=80 --overrides='{\"spec\":{\"ports\":[{\"port\":80,\"nodePort\":30000}]}}'", tr("example service")),
					ui.Caption(tr("Then open http://127.0.0.1:30000.")),
				}
			}),
		).Gap(10).Stretch()),
	)
}

func kubeNotesCard() ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return widget.Column(widget.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	return ui.Card(
		ui.CardHeader(tr("Good to know"), tr("What the guest kernel supports.")),
		ui.CardContent(widget.Column(
			note("iptables", tr("The node uses iptables-legacy (IPTABLES_MODE=legacy); nftables, IPVS and eBPF are not available, so kube-proxy runs in iptables mode.")),
			note("CNI", tr("flannel and Calico (VXLAN) work, with NetworkPolicy. Cilium needs eBPF and does not run.")),
			note("NodePorts", tr("Only the published range 30000–30019 reaches the host. Widen it in the sandbox settings.")),
			note(tr("Load balancers"), tr("With the service load balancer on (Settings → Kubernetes), a LoadBalancer service's ports 80 and 443 reach 127.0.0.1:8080 and :8443. Off, such a service stays pending; use a NodePort instead.")),
			note(tr("Images built here"), tr("Load them directly into the cluster's nodes (Images → Load into cluster) with a tag other than latest. For docker push, buildx --push, Tilt/Skaffold, or :latest tags, push to the built-in local registry (%s / %s), which starts automatically with Docker or Kubernetes.", recipes.RegistryLocal(), recipes.RegistryHost())),
			note(tr("Memory limits"), tr("Pod memory limits are enforced by polling, so a short burst can pass them.")),
		).Gap(10).Stretch()),
	)
}

// ---- Settings -----------------------------------------------------------
