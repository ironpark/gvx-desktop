package ui

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"image/png"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggfx"
	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/i18n"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/updater"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// TitlebarHidden says whether the window draws its own title bar region:
// on macOS the content runs under a clear title bar, with the traffic
// lights over the sidebar's header.
var TitlebarHidden = runtime.GOOS == "darwin"

// Build is the root of the window. It builds the window again when the
// language changes: the words are chosen as the tree is built, and what
// the user was doing lives in the model, which stays. The tree is given
// the language's Localizer, which sets the language text is shaped in, and
// the words of ggui's own controls.
func Build(m *Model) ggui.Widget {
	return ggui.View(m.Lang, func(lang string) ggui.Widget {
		SetLanguage(lang)
		return i18n.Provide(localizer(lang), ui.ProvideStrings(controlStrings(lang), build(m)))
	})
}

func build(m *Model) ggui.Widget {
	pages := entering(m.Page, func(p string) ggui.Widget {
		switch p {
		case PageSandboxes:
			return sandboxesPage(m)
		case PageContainers:
			return containersPage(m)
		case PageNetworks:
			return networksPage(m)
		case PagePods:
			return podsPage(m)
		case PageWorkloads:
			return workloadsPage(m)
		case PageServices:
			return servicesPage(m)
		case PageImages:
			return imagesPage(m)
		case PageVolumes:
			return volumesPage(m)
		case PageSnapshots:
			return snapshotsPage(m)
		case PageDocker:
			return dockerPage(m)
		case PageKubernetes:
			return kubernetesPage(m)
		case PageSettings:
			return settingsPage(m)
		case PageLabs:
			return labsPage(m)
		case PageExtensions:
			return extensionsPage(m)
		}
		if extID, viewID, ok := ParseExtensionPage(p); ok {
			return extensionPage(m, extID, viewID)
		}
		return dashboardPage(m)
	})
	// The sidebar runs from the top of the window, with the traffic lights
	// over its header, down to the status bar, which has the window's
	// width; the top bar belongs to the content beside the sidebar and
	// holds its trigger. With the sidebar away the bar reaches the window's
	// edge, and makes room for the lights itself.
	var menubar ggui.Widget = widget.Box()
	if !ggui.NativeMenu() && m.appMenu != nil {
		menubar = ggui.View(m.Menus, func(MenuState) ggui.Widget { return ui.AppMenubar(m.appMenus()) })
	}
	main := widget.Column(
		menubar,
		widget.Expanded(widget.Row(
			sidebar(m),
			widget.Expanded(widget.Column(
				topBar(m),
				ui.Divider(),
				widget.Expanded(pages),
			).Stretch()),
		).Align(widget.AlignStretch)),
		ui.Divider(),
		statusBar(m),
	).Stretch()

	var updateDlg ggui.Widget = widget.Box()
	if m.UpdateCtrl != nil {
		updateDlg = updater.Dialog(m.UpdateCtrl)
	}
	return widget.Stack(
		main,
		editorSheet(m),
		updateDlg,
		confirmDialog(m),
		pullDialog(m),
		explorerDialog(m),
		volumeDialog(m),
		dockerConfigDialog(m),
		forwardDialog(m),
		scaleDialog(m),
		quitDialog(m),
		palette(m),
		m.Toasts,
	).Expand()
}

// dragRegion moves the window when pressed, standing in for the hidden
// title bar.
func dragRegion(child ggui.Widget) ggui.Widget {
	if !TitlebarHidden {
		return child
	}
	return widget.Pointer(child).OnDown(func(ggui.PointerEvent) {
		if w := ggui.UseWindow(); w != nil {
			w.StartDrag()
		}
	})
}

type sidebarExtKey struct {
	id        string
	viewID    string
	label     string
	icon      string
	badgeText string
	badgeTone string
	badgeTip  string
}

type sidebarKey struct {
	containers bool
	pods       bool
	extEnabled bool
	extItems   []sidebarExtKey
}

// sidebar is built again when the Containers, Pods, or Extensions items come or go,
// which a sidebar's entries are only as it is made; the sidebar carries its
// highlight, hover and collapse across.
func sidebar(m *Model) ggui.Widget {
	state := ggui.Derived(func() sidebarKey {
		lang := m.Lang.Get()
		var extItems []sidebarExtKey
		for _, info := range m.Extensions.Get() {
			if !info.State.CanRun() || len(info.Views) == 0 {
				continue
			}
			for _, v := range info.Views {
				lbl := info.LocalizedViewTitle(v.ID, lang)
				if lbl == "" {
					lbl = info.LocalizedName(lang)
				}
				ic := v.Icon
				if ic == "" {
					ic = info.Icon
				}
				extItems = append(extItems, sidebarExtKey{
					id:        info.ID,
					viewID:    v.ID,
					label:     lbl,
					icon:      ic,
					badgeText: info.Badge.Text,
					badgeTone: info.Badge.Tone,
					badgeTip:  info.Badge.Tooltip,
				})
			}
		}
		return sidebarKey{
			containers: m.ContainersOpen.Get(),
			pods:       m.PodsOpen.Get(),
			extEnabled: m.Settings.Get().Extensions,
			extItems:   extItems,
		}
	}).WithEqual(func(a, b sidebarKey) bool {
		return a.containers == b.containers && a.pods == b.pods && a.extEnabled == b.extEnabled && slices.Equal(a.extItems, b.extItems)
	})
	return ggui.View(state, func(k sidebarKey) ggui.Widget { return sidebarFor(m, k.containers, k.pods, k.extEnabled, k.extItems) })
}

// LabsSupported reports whether the current platform has any Labs features.
// Local HTTPS routing and the built-in MCP server work on macOS, Windows, and
// Linux; Rosetta amd64 builds remains Apple silicon macOS only inside Labs.
func LabsSupported() bool {
	return true
}

// sidebarFor is the sidebar with Docker's Networks/Containers and
// Kubernetes's Pods/Workloads/Services nested under their engine while it runs.
func sidebarFor(m *Model, containers, pods, extEnabled bool, extItems []sidebarExtKey) ggui.Widget {
	nav := ggui.Bind(m.Page.Get, m.Go)
	item := func(key, label string, g *icons.Glyph) ui.SidebarEntry {
		return ui.SidebarItem(key, label).Leading(glyph(g, 16)).Tooltip(label)
	}
	// Docker and Kubernetes open their engine's page; while running, their
	// views nest underneath in a compact collapsible group.
	docker := item(PageDocker, "Docker", dockerIcon)
	if containers {
		dockerBadge := runningBadge(m.RunningContainers,
			func(n int) string { return trn(n, "%d container running", "%d containers running") })
		docker = ui.SidebarCustom(newSidebarTreeGroup(m,
			sidebarNavItem{key: PageDocker, label: "Docker", icon: dockerIcon},
			m.DockerNavOpen,
			runningBadge(m.RunningContainers, func(n int) string { return trn(n, "%d container running", "%d containers running") }),
			sidebarNavItem{key: PageNetworks, label: tr("Networks"), icon: lucide.Network},
			sidebarNavItem{key: PageContainers, label: tr("Containers"), icon: lucide.Container, action: dockerBadge},
		))
	}
	kube := item(PageKubernetes, "Kubernetes", lucide.ShipWheel)
	if pods {
		kubeBadge := runningBadge(m.RunningPods,
			func(n int) string { return trn(n, "%d pod running", "%d pods running") })
		kube = ui.SidebarCustom(newSidebarTreeGroup(m,
			sidebarNavItem{key: PageKubernetes, label: "Kubernetes", icon: lucide.ShipWheel},
			m.KubeNavOpen,
			runningBadge(m.RunningPods, func(n int) string { return trn(n, "%d pod running", "%d pods running") }),
			sidebarNavItem{key: PagePods, label: tr("Pods"), icon: lucide.Hexagon, action: kubeBadge},
			sidebarNavItem{key: PageWorkloads, label: tr("Workloads"), icon: lucide.Boxes},
			sidebarNavItem{key: PageServices, label: tr("Services"), icon: lucide.Waypoints},
		))
	}
	entries := []ui.SidebarEntry{
		item(PageDashboard, tr("Dashboard"), lucide.LayoutDashboard),
		item(PageSandboxes, tr("Sandboxes"), lucide.Box).Action(runningBadge(m.Running, func(n int) string { return trn(n, "%d sandbox running", "%d sandboxes running") })),
		item(PageImages, tr("Images"), lucide.Layers),
		item(PageVolumes, tr("Volumes"), lucide.HardDrive),
		item(PageSnapshots, tr("Snapshots"), lucide.Camera),
		ui.SidebarSeparator(),
		docker,
		kube,
	}
	if len(extItems) > 0 {
		entries = append(entries, ui.SidebarSection(tr("Extensions")).Collapsible().BindOpen(m.ExtNavOpen))
		for _, it := range extItems {
			tipText := it.label
			if it.badgeTip != "" {
				tipText = it.label + " — " + it.badgeTip
			}
			entry := ui.SidebarItem(ExtensionPage(it.id, it.viewID), it.label).
				Leading(extIcon(it.icon).Size(16)).
				Tooltip(tipText)
			if it.badgeText != "" {
				entry = entry.Badge(it.badgeText)
				if it.badgeTone != "" || it.badgeTip != "" {
					var badgeDot ggui.Widget = dot(toneColor(cmpTone(it.badgeTone, "muted")), 6)
					if it.badgeTip != "" {
						badgeDot = tip(badgeDot, it.badgeTip)
					}
					entry = entry.Action(badgeDot)
				}
			}
			entries = append(entries, entry)
		}
	}
	brandTop := 4.0
	if TitlebarHidden {
		brandTop = 40 // room for the traffic lights
	}
	brand := dragRegion(widget.Padding(widget.Row(
		widget.Image(brandMark()).Size(40, 40),
		widget.Column(
			widget.Text("GVX Desktop").Size(15).Weight(ggui.WeightSemibold).NoWrap(),
			ui.CaptionOf(ggui.Map(m.Engine, engineShort)).Size(11).NoWrap(),
		).Gap(1),
	).Gap(10), brandTop, 12, 8, 12))

	footer := []ui.SidebarEntry{}
	if LabsSupported() {
		footer = append(footer, item(PageLabs, tr("Labs"), lucide.FlaskConical))
	}
	if extEnabled {
		footer = append(footer, item(PageExtensions, tr("Extensions"), lucide.Puzzle))
	}
	footer = append(footer, item(PageSettings, tr("Settings"), lucide.Settings))

	return ui.Sidebar(nav, entries...).Header(brand).
		FooterItems(footer...).
		Width(232).
		Collapsible(ui.SidebarCollapseOffcanvas).BindCollapsed(m.SidebarNarrow).
		Shortcut("cmd+b").Name(tr("Navigation")).
		Easing(sidebarEasing)
}

//go:embed mark.png
var markPNG []byte

// brandMark is the app's mark, the helmeted gopher of assets/, made into
// a picture the first time the window is built.
var brandMark = sync.OnceValue(func() *ggfx.Image {
	img, err := png.Decode(bytes.NewReader(markPNG))
	if err != nil {
		panic("ui: mark.png: " + err.Error())
	}
	return ggfx.NewImageFromImage(img)
})

// MarkPNG is the app's mark as a PNG, for what draws it outside the window.
func MarkPNG() []byte { return markPNG }

// AppVersion is the version of the app, which main sets.
var AppVersion = "dev"

// EngineLine says in a few words which rungvx the app runs sandboxes with,
// or that it has none.
func EngineLine(e Engine) string { return "rungvx · " + engineShort(e) }

// ShortVersion is a version of gvx without its build stamp.
func ShortVersion(v string) string { return shortVersion(v) }

// trafficLightsWidth is the room the window's close, minimise and zoom
// buttons take at the start of the top bar, where the title bar is hidden.
const trafficLightsWidth = 68

// The sidebar slides away and back along sidebarEasing: gently off, fast
// through the middle, gently home. sidebarDuration is the theme's
// MotionSlow, which the sidebar itself takes.
var sidebarEasing = ggui.EaseInOutCubic

const sidebarDuration = 200 * time.Millisecond

// pageDuration is how long a page takes to come in, leaveDuration how long
// the one before it took to go, and hoverDuration how long a surface takes
// to answer the pointer.
const (
	pageDuration  = 180 * time.Millisecond
	leaveDuration = 70 * time.Millisecond
	hoverDuration = 120 * time.Millisecond
)

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

type storeSettings = store.Settings

func engineShort(e Engine) string {
	switch {
	case e.Loading && !e.Checked:
		return tr("Checking engine…")
	case e.Err != "":
		return tr("Engine unavailable")
	case len(e.Info.AvailableBackends) == 0:
		return tr("No backend")
	}
	v := engineVersion(e)
	if v == "" {
		v = "rungvx"
	}
	return v + " · " + e.Info.DefaultBackend
}

func topBar(m *Model) ggui.Widget {
	search := ui.ButtonOf(widget.Row(
		glyph(lucide.Search, 14).Color(colorGray),
		ui.Caption(tr("Search or run a command…")),
		widget.Spacer(),
		ui.KbdGroup(ui.Kbd(modKey()), ui.Kbd("K")),
	).Gap(8).Align(widget.AlignCenter), func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }).
		Outline().Name(tr("Command palette")).Size(ui.ButtonSmall)

	engine := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		c, text := colorGreen, tr("Engine running")
		switch {
		case e.Loading && !e.Checked:
			c, text = colorAmber, tr("Checking engine")
		case e.Err != "":
			c, text = colorRed, tr("Engine unavailable")
		case len(e.Info.AvailableBackends) == 0:
			c, text = colorRed, tr("No backend")
		}
		pill := ui.ButtonOf(widget.Row(dot(c, 8), widget.Text(text).Size(12)).Gap(8), func() { m.Go(PageSettings) }).
			Ghost().Size(ui.ButtonSmall).Name(text)
		return tip(pill, engineTooltip(e))
	})
	theme := ggui.View(m.Settings, func(s storeSettings) ggui.Widget {
		g, next, label := lucide.Monitor, "light", tr("Theme: system")
		switch s.Theme {
		case "light":
			g, next, label = lucide.Sun, "dark", tr("Theme: light")
		case "dark":
			g, next, label = lucide.Moon, "system", tr("Theme: dark")
		}
		return tip(iconButton(g, label, func() { m.UpdateSettings(func(s *storeSettings) { s.Theme = next }) }), label)
	})
	// With the sidebar away the traffic lights stand over the start of this
	// bar: room for them opens as the sidebar leaves, at its pace.
	var lights ggui.Widget = widget.Box()
	if TitlebarHidden {
		room := ggui.TweenOf(ggui.Map(m.SidebarNarrow, func(away bool) float64 {
			if away {
				return trafficLightsWidth
			}
			return 0
		}), sidebarDuration).Easing(sidebarEasing)
		lights = dragRegion(widget.Box().Height(52).BindWidth(room))
	}
	return widget.Box(widget.Row(
		lights,
		ui.SidebarTrigger(m.SidebarNarrow),
		dragRegion(widget.Box().Size(16, 52)),
		widget.Box(search).Width(340),
		widget.Expanded(dragRegion(widget.Box().Height(52))),
		engine,
		theme,
	).Gap(4).Align(widget.AlignCenter)).Pad(0, 12, 0, 8).Height(52)
}

func modKey() string {
	if runtime.GOOS == "darwin" {
		return "⌘"
	}
	return "Ctrl"
}

func diskTypeLabel(kind string) string {
	switch kind {
	case "Images":
		return tr("Images")
	case "Local Volumes":
		return tr("Volumes")
	case "Build Cache":
		return tr("Build Cache")
	}
	return kind
}

func engineTooltip(e Engine) string {
	if e.Err != "" {
		return e.Err
	}
	if !e.Checked {
		return tr("Looking for rungvx…")
	}
	return fmt.Sprintf(tr("%s · backends: %s · lockdown: %s"), e.Path, strings.Join(e.Info.AvailableBackends, ", "), e.Info.HostLockdown)
}

func statusBar(m *Model) ggui.Widget {
	engine := ui.CaptionOf(ggui.Map(m.Engine, func(e Engine) string {
		if e.Err != "" || !e.Checked {
			return engineShort(e)
		}
		return fmt.Sprintf(tr("rungvx %s · %s · %s lockdown"), engineVersion(e), e.Info.DefaultBackend, e.Info.HostLockdown)
	})).Size(11).NoWrap()
	running := ui.CaptionOf(ggui.Derived(func() string {
		s := fmt.Sprintf(tr("%d of %d sandboxes running"), m.Running.Get(), len(m.Specs.Get()))
		if ec := m.EngineRunning.Get(); ec > 0 {
			s += " · " + trn(ec, "%d container running", "%d containers running")
		}
		return s
	})).Size(11).NoWrap()
	disk := ui.CaptionOf(ggui.Map(m.Disk, func(rows []rungv.DiskUsage) string {
		var parts []string
		for _, r := range rows {
			parts = append(parts, diskTypeLabel(r.Type)+" "+formatSize(r.Size))
		}
		return strings.Join(parts, " · ")
	})).Size(11).NoWrap()
	sep := func() ggui.Widget { return widget.Box().Size(1, 12).Fill(colorBorder) }
	return widget.Box(widget.Row(
		ggui.View(m.Engine, func(e Engine) ggui.Widget {
			c := colorGreen
			if !e.Ready() {
				c = colorRed
				if !e.Checked {
					c = colorAmber
				}
			}
			return dot(c, 6)
		}),
		engine, sep(), running, widget.Spacer(), disk,
	).Gap(10).Align(widget.AlignCenter)).Pad(0, 14).Height(28)
}

// ---- Dialogs ------------------------------------------------------------

func confirmDialog(m *Model) ggui.Widget {
	head := widget.Row(
		ggui.View(m.ConfirmSafe, func(safe bool) ggui.Widget {
			if safe {
				return widget.Box(glyph(lucide.Info, 16).Color(colorBlue)).Pad(7).Radius(8).Fill(rgbA(0x3b82f6, 0x1c))
			}
			return widget.Box(glyph(lucide.TriangleExclamationPoint, 16).Color(colorRed)).Pad(7).Radius(8).Fill(rgbA(0xef4444, 0x1c))
		}),
		widget.Expanded(ui.HeadingOf(m.ConfirmTitle).Size(17)),
	).Gap(10).Align(widget.AlignCenter)
	return ui.Dialog(m.ConfirmOpen, widget.Column(
		head,
		ui.CaptionOf(m.ConfirmText),
	).Gap(12).Stretch()).Width(440).Dismissible(false).Name(tr("Confirm")).Footer(
		ui.Button(tr("Cancel"), func() { m.ConfirmOpen.Set(false) }).Outline(),
		ggui.View(m.ConfirmSafe, func(safe bool) ggui.Widget {
			b := ui.ButtonOf(widget.TextOf(m.ConfirmLabel), func() {
				m.ConfirmOpen.Set(false)
				m.RunConfirmed()
			}).BindName(m.ConfirmLabel)
			if !safe {
				b = b.Destructive()
			}
			return b
		}),
	)
}

func pullDialog(m *Model) ggui.Widget {
	// What is typed is searched for, once the typing pauses.
	ggui.Watch(m.PullRef, func(string) { m.SearchTyped() })
	ggui.Watch(m.PullOpen, func(bool) { m.SearchTyped() })
	ggui.Watch(m.Pulling, func(bool) { m.SearchTyped() })
	ggui.Watch(ggui.Debounced(m.searchWaiting(), searchDelay), m.searchPaused)
	source := ggui.Bind(m.SearchSource, func(v registry.Source) {
		m.UpdateSettings(func(s *storeSettings) { s.SearchRegistry = string(v) })
		m.SearchTyped()
	})

	field := ui.TextField(m.PullRef).Placeholder(tr("Search, or a reference: nginx, ghcr.io/org/app:1.0")).
		Name(tr("Image reference")).OnSubmit(func(string) { m.Pull() }).BindDisabled(m.Pulling)
	searchBar := widget.Row(
		ui.Caption(tr("Search in")).Size(12),
		ui.ToggleGroup[registry.Source](source).Options(registry.Sources()).Format(registry.Source.Title).
			Outline().Size(ui.ButtonExtraSmall).Spacing(0).Name(tr("Registry to search")),
		ggui.If(m.SearchBusy, func() ggui.Widget { return ui.Spinner().Size(12) }),
		widget.Spacer(),
		ui.CaptionOf(ggui.Combine(m.SearchResults, m.SearchFor, func(r []registry.Result, q string) string {
			if q == "" {
				return ""
			}
			return tr("%d for “%s”", len(r), q)
		})).Size(12).NoWrap(),
	).Gap(8).Align(widget.AlignCenter)

	suggestions := widget.Wrap(
		suggestion(m, "alpine:3.20"), suggestion(m, "nginx:alpine"), suggestion(m, "python:3.12-alpine"),
		suggestion(m, "postgres:17-alpine"), suggestion(m, "redis:alpine"), suggestion(m, "docker:dind"),
	).Gap(6)

	// 0: nothing searched, 1: results, 2: none found, 3: the search failed.
	state := ggui.Combine(ggui.Combine(m.SearchResults, m.SearchFor, func(r []registry.Result, q string) int {
		switch {
		case q == "":
			return 0
		case len(r) == 0:
			return 2
		}
		return 1
	}), m.SearchErr, func(s int, err string) int {
		if err != "" {
			return 3
		}
		return s
	})
	found := ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 1:
			return widget.Box(widget.Scroll(ggui.View(m.SearchResults, func(rs []registry.Result) ggui.Widget {
				rows := make([]ggui.Widget, len(rs))
				for i, r := range rs {
					rows[i] = searchResult(m, r)
				}
				return widget.Column(rows...).Gap(2).Stretch()
			})).FadeEdges(10)).Height(248)
		case 2:
			return ui.CaptionOf(ggui.Map(m.SearchFor, func(q string) string {
				return tr("Nothing found for “%s”. A full reference can still be pulled.", q)
			}))
		case 3:
			return ui.CaptionOf(ggui.Map(m.SearchErr, func(e string) string { return tr("The search failed: %s", e) })).Color(colorRed)
		}
		return suggestions
	})
	tags := ggui.View(ggui.Combine(m.SearchTags, m.SearchTagsBusy, func(t []string, busy bool) int {
		if busy {
			return -1
		}
		return len(t)
	}), func(n int) ggui.Widget {
		switch {
		case n < 0:
			return widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Reading tags…")).Size(12)).Gap(8).Align(widget.AlignCenter)
		case n == 0:
			return widget.Box()
		}
		chips := make([]ggui.Widget, 0, n)
		for _, t := range ggui.Peek(m.SearchTags) {
			chips = append(chips, ui.Badge(t).Outline().OnTap(func() { m.PickTag(t) }))
		}
		return widget.Column(
			ui.CaptionOf(ggui.Map(m.SearchTagsOf, func(ref string) string { return tr("Tags of %s, newest first", ref) })).Size(12),
			widget.Wrap(chips...).Gap(6),
		).Gap(6).Stretch()
	})
	// While a pull runs, its output has the room.
	browse := ggui.If(m.Pulling, func() ggui.Widget { return widget.Box() }).Else(func() ggui.Widget {
		return widget.Column(searchBar, found, tags).Gap(10).Stretch()
	})
	logs := ggui.If(ggui.Map(m.PullLog, func(s string) bool { return s != "" }), func() ggui.Widget {
		return ui.CodeBlockOf(m.PullLog).Height(180).Small().CopyButton(false)
	})
	heading := ui.HeadingOf(ggui.Combine(m.PullTarget, m.Settings, func(t int, s storeSettings) string {
		if t == PullTargetDocker && !s.SandboxEngine {
			return tr("Pull an image into Docker Engine")
		}
		return tr("Pull an image")
	})).Size(17)
	help := ui.CaptionOf(ggui.Combine(m.PullTarget, m.Settings, func(t int, s storeSettings) string {
		if t == PullTargetDocker && !s.SandboxEngine {
			return tr("Images run natively on this CPU (linux/%s) inside the Docker Engine sandbox.", goarch())
		}
		return tr("Images run natively on this CPU (linux/%s). Layers are compiled to read-only EROFS files once and cached.", goarch())
	})).Size(12)
	return ui.Dialog(m.PullOpen, widget.Column(
		heading,
		widget.Column(
			ui.Field(tr("Image"), field),
			help,
		).Gap(6).Stretch(),
		browse,
		logs,
	).Gap(14).Stretch()).Name(tr("Pull an image")).Width(600).Footer(
		ui.Button(tr("Close"), func() { m.PullOpen.Set(false) }).Outline(),
		ui.ButtonOf(ggui.If(m.Pulling, func() ggui.Widget {
			return widget.Row(ui.Spinner().Size(14), widget.Text(tr("Pulling…"))).Gap(8)
		}).Else(func() ggui.Widget { return widget.Text(tr("Pull")) }), m.Pull).
			Name(tr("Pull")).BindDisabled(ggui.Or(m.Pulling, ggui.Map(m.PullRef, func(s string) bool { return strings.TrimSpace(s) == "" }))),
	)
}

// searchResult is one repository a registry search found: choosing it puts
// it in the field.
func searchResult(m *Model, r registry.Result) ggui.Widget {
	title := []ggui.Widget{widget.Text(r.Name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()}
	if r.Official {
		title = append(title, ui.Badge(tr("Official")).Outline())
	}
	text := []ggui.Widget{widget.Row(title...).Gap(8).Align(widget.AlignCenter)}
	if r.Description != "" {
		text = append(text, ui.Caption(r.Description).Size(12).NoWrap().Ellipsis())
	}
	var stats []ggui.Widget
	if r.Stars > 0 {
		stats = append(stats, glyph(lucide.Star, 12).Color(colorGray), ui.Caption(compact(int64(r.Stars))).Size(12))
	}
	if r.Pulls > 0 {
		stats = append(stats, glyph(lucide.Download, 12).Color(colorGray), ui.Caption(compact(r.Pulls)).Size(12))
	}
	row := widget.Row(
		widget.Expanded(widget.Column(text...).Gap(2)),
		widget.Row(stats...).Gap(5).Align(widget.AlignCenter),
	).Gap(12).Align(widget.AlignCenter)
	return hoverRow(row, func() { m.PickResult(r) })
}

// compact writes a count in a few characters: 950, 21k, 1.4M, 13B.
func compact(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n < 1_000_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n < 10_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	}
	return fmt.Sprintf("%dB", n/1_000_000_000)
}

func goarch() string {
	if runtime.GOARCH == "amd64" {
		return "amd64"
	}
	return runtime.GOARCH
}

func suggestion(m *Model, ref string) ggui.Widget {
	return ui.Badge(ref).Outline().OnTap(func() { m.PullRef.Set(ref) })
}

func volumeDialog(m *Model) ggui.Widget {
	disabled := ggui.Derived(func() bool {
		return !isValidVolumeName(m.VolumeName.Get()) || m.BusyOps.Get()["vol:create"]
	})
	return ui.Dialog(m.VolumeOpen, widget.Column(
		ui.Field(tr("Name"), ui.TextField(m.VolumeName).Placeholder("pgdata").Name(tr("Volume name")).OnSubmit(func(string) { m.CreateVolume() })).
			Help(tr("Letters, digits, '-', '_' and '.'. Mount it in a sandbox as name:/guest/path.")),
		ui.Field(tr("Driver"), ui.ToggleGroup(m.VolumeDriver).Options([]string{"ext4lite", "hostfs"}).Outline().Name(tr("Volume driver"))).
			Help(tr("ext4lite keeps a journaled ext4 image in one file: real POSIX semantics, fast. hostfs keeps plain host files, slower.")),
	).Gap(14).Stretch()).Title(tr("Create a volume")).Width(460).Footer(
		ui.Button(tr("Cancel"), func() { m.VolumeOpen.Set(false) }).Outline(),
		ui.Button(tr("Create"), m.CreateVolume).BindDisabled(disabled),
	)
}

// quitText says what quitting with n sandboxes running does to them.
func quitText(n int) string {
	return trn(n, "%d sandbox is running. gvx has no daemon: sandboxes live only while GVX Desktop runs.",
		"%d sandboxes are running. gvx has no daemon: sandboxes live only while GVX Desktop runs.")
}

func quitHeading(sandboxes, containers int) string {
	switch {
	case sandboxes > 0 && containers > 0:
		return tr("Stop running sandboxes and containers?")
	case containers > 0:
		return tr("Stop running containers?")
	default:
		return tr("Stop running sandboxes?")
	}
}

func quitSummary(sandboxes, containers int) string {
	switch {
	case sandboxes > 0 && containers > 0:
		return tr("%s and %s are running. gvx has no daemon: they live only while GVX Desktop runs.",
			trn(sandboxes, "%d sandbox", "%d sandboxes"),
			trn(containers, "%d container", "%d containers"))
	case containers > 0:
		return trn(containers, "%d container is running. gvx has no daemon: containers live only while GVX Desktop runs.",
			"%d containers are running. gvx has no daemon: containers live only while GVX Desktop runs.")
	default:
		return quitText(sandboxes)
	}
}

func quitDialog(m *Model) ggui.Widget {
	head := widget.Row(
		widget.Box(glyph(lucide.TriangleExclamationPoint, 16).Color(colorAmber)).Pad(7).Radius(8).Fill(rgbA(0xf59e0b, 0x1c)),
		widget.Expanded(ui.HeadingOf(ggui.Combine(m.Active, m.EngineRunning, quitHeading)).Size(17)),
	).Gap(10).Align(widget.AlignCenter)
	return ui.Dialog(m.QuitOpen, widget.Column(
		head,
		ui.CaptionOf(ggui.Combine(m.Active, m.EngineRunning, quitSummary)),
	).Gap(12).Stretch()).Width(460).Dismissible(false).Name(tr("Quit")).Footer(
		ui.Button(tr("Cancel"), func() { m.QuitOpen.Set(false) }).Outline(),
		ui.Button(tr("Stop and quit"), func() {
			m.QuitOpen.Set(false)
			m.StopAllAndThen(func() { quitApp() })
		}).Destructive(),
	)
}

// quitApp ends the app; main sets it.
var quitApp = func() {}

// SetQuit installs the function that ends the app.
func SetQuit(fn func()) { quitApp = fn }

type paletteSandbox struct {
	id, name, image string
	status          sandbox.Status
	paused          bool
}

type paletteExtCmd struct {
	extID, cmdID, title, extName, icon string
}

type paletteState struct {
	containers, pods  bool
	sandboxEngine     bool
	extensionsEnabled bool
	active            int
	sandboxes         []paletteSandbox
	extCommands       []paletteExtCmd
}

func (a paletteState) equal(b paletteState) bool {
	return a.containers == b.containers &&
		a.pods == b.pods &&
		a.sandboxEngine == b.sandboxEngine &&
		a.extensionsEnabled == b.extensionsEnabled &&
		a.active == b.active &&
		slices.Equal(a.sandboxes, b.sandboxes) &&
		slices.Equal(a.extCommands, b.extCommands)
}

func palette(m *Model) ggui.Widget {
	close := func(fn func()) func() {
		return func() { m.PaletteOpen.Set(false); fn() }
	}
	ic := func(g *icons.Glyph) ggui.Widget { return glyph(g, 14).Color(colorGray) }
	entries := func(st paletteState) []ui.CommandEntry {
		dockerAction := ui.CommandItem(tr("Start Docker Engine"), close(m.OpenDocker)).Icon(ic(dockerIcon)).Group(tr("Recipes")).Keywords("dind dockerd")
		if st.sandboxEngine {
			dockerAction = ui.CommandItem(tr("Open Docker"), close(m.OpenDocker)).Icon(ic(dockerIcon)).Group(tr("Recipes")).Keywords("docker engine sandbox")
		}
		imagesNav := []ui.CommandEntry{
			ui.CommandItem(tr("Images"), close(func() { m.GoImages(ImagesGVX) })).Icon(ic(lucide.Layers)).Group(tr("Go to")).Shortcut("cmd+3").Keywords("docker images rmi pull cache"),
		}
		if !st.sandboxEngine {
			imagesNav = []ui.CommandEntry{
				ui.CommandItem(tr("Images"), close(func() { m.GoImages(ImagesGVX) })).Icon(ic(lucide.Layers)).Group(tr("Go to")).Shortcut("cmd+3"),
				ui.CommandItem(tr("Docker images"), close(func() { m.GoImages(ImagesDocker) })).Icon(ic(dockerIcon)).Group(tr("Go to")).Keywords("docker images rmi pull"),
			}
		}
		out := []ui.CommandEntry{
			ui.CommandItem(tr("New sandbox"), close(func() { m.NewSandbox("") })).Icon(ic(lucide.Plus)).Group(tr("Sandboxes")).Shortcut("cmd+n").Keywords("create run"),
			ui.CommandItem(tr("Stop all sandboxes"), close(m.StopAll)).Icon(ic(lucide.Square)).Group(tr("Sandboxes")).Disabled(st.active == 0),
			ui.CommandItem(tr("Pull image"), close(func() { m.OpenPull("") })).Icon(ic(lucide.Download)).Group(tr("Images")).Shortcut("cmd+shift+p").Keywords("download"),
			ui.CommandItem(tr("Prune images"), close(func() {
				if ggui.Peek(m.Settings).SandboxEngine {
					m.AskPruneCachedImages()
				} else {
					m.AskPruneImages()
				}
			})).Icon(ic(lucide.Eraser)).Group(tr("Images")).Keywords("clean unused"),
			dockerAction,
			ui.CommandItem(tr("Start Kubernetes (k3s)"), close(func() { m.CreateFromRecipe(recipes.Kubernetes); m.Go(PageKubernetes) })).Icon(ic(lucide.ShipWheel)).Group(tr("Recipes")).Keywords("k8s cluster kubectl"),
			ui.CommandItem(tr("Create volume"), close(m.OpenVolumeDialog)).Icon(ic(lucide.HardDrive)).Group(tr("Volumes")),
			ui.CommandItem(tr("Prune volumes"), close(m.AskPruneVolumes)).Icon(ic(lucide.Eraser)).Group(tr("Volumes")).Keywords("clean unused"),
			ui.CommandItem(tr("Dashboard"), close(func() { m.Go(PageDashboard) })).Icon(ic(lucide.LayoutDashboard)).Group(tr("Go to")).Shortcut("cmd+1"),
			ui.CommandItem(tr("Sandboxes"), close(func() { m.Go(PageSandboxes) })).Icon(ic(lucide.Box)).Group(tr("Go to")).Shortcut("cmd+2"),
		}
		out = append(out, imagesNav...)
		out = append(out,
			ui.CommandItem(tr("Volumes"), close(func() { m.Go(PageVolumes) })).Icon(ic(lucide.HardDrive)).Group(tr("Go to")).Shortcut("cmd+4"),
			ui.CommandItem(tr("Snapshots"), close(func() { m.Go(PageSnapshots) })).Icon(ic(lucide.Camera)).Group(tr("Go to")).Shortcut("cmd+5"),
			ui.CommandItem("Docker", close(func() { m.Go(PageDocker) })).Icon(ic(dockerIcon)).Group(tr("Go to")).Shortcut("cmd+6").Keywords("docker engine dind dockerd"),
			ui.CommandItem(tr("Networks"), close(func() { m.Go(PageNetworks) })).Icon(ic(lucide.Network)).Group(tr("Go to")).Keywords("docker network ls").Disabled(!st.containers),
			ui.CommandItem(tr("Containers"), close(func() { m.Go(PageContainers) })).Icon(ic(lucide.Container)).Group(tr("Go to")).Shortcut("cmd+7").Keywords("docker ps").Disabled(!st.containers),
			ui.CommandItem(tr("Remove unused networks"), close(m.AskPruneNetworks)).Icon(ic(lucide.Eraser)).Group(tr("Networks")).Keywords("docker network prune clean").Disabled(!st.containers),
			ui.CommandItem("Kubernetes", close(func() { m.Go(PageKubernetes) })).Icon(ic(lucide.ShipWheel)).Group(tr("Go to")).Shortcut("cmd+8").Keywords("k3s k8s"),
			ui.CommandItem(tr("Pods"), close(func() { m.GoKube(KubePods) })).Icon(ic(lucide.Hexagon)).Group(tr("Go to")).Shortcut("cmd+9").Keywords("kubectl get pods k8s").Disabled(!st.pods),
			ui.CommandItem(tr("Workloads"), close(func() { m.GoKube(KubeWorkloads) })).Icon(ic(lucide.Boxes)).Group(tr("Go to")).Keywords("k8s deployments statefulsets daemonsets jobs").Disabled(!st.pods),
			ui.CommandItem(tr("Services"), close(func() { m.GoKube(KubeServices) })).Icon(ic(lucide.Waypoints)).Group(tr("Go to")).Keywords("k8s svc nodeport port-forward").Disabled(!st.pods),
			ui.CommandItem(tr("Local registry"), close(m.OpenRegistry)).Icon(ic(lucide.Warehouse)).Group(tr("Go to")).Keywords("registry local push images cluster 5001"),
			ui.CommandItem(tr("Settings"), close(func() { m.Go(PageSettings) })).Icon(ic(lucide.Settings)).Group(tr("Go to")).Shortcut("cmd+,"),
		)
		if LabsSupported() {
			out = append(out, ui.CommandItem(tr("Labs"), close(func() { m.Go(PageLabs) })).Icon(ic(lucide.FlaskConical)).Group(tr("Go to")).Keywords("experimental local proxy https localhost mcp ai agent rosetta extensions"))
		}
		if st.extensionsEnabled {
			out = append(out, ui.CommandItem(tr("Extensions"), close(func() { m.Go(PageExtensions) })).Icon(ic(lucide.Puzzle)).Group(tr("Go to")).Keywords("plugins extensions install github actions"))
		}
		out = append(out,
			ui.CommandItem(tr("Refresh"), close(m.RefreshAll)).Icon(ic(lucide.RefreshCw)).Group(tr("App")).Shortcut("cmd+r"),
			ui.CommandItem(tr("Toggle sidebar"), close(func() { m.SidebarNarrow.Set(!ggui.Peek(m.SidebarNarrow)) })).Icon(ic(lucide.PanelLeft)).Group(tr("App")).Shortcut("cmd+b").Keywords("navigation panel"),
			ui.CommandItem(tr("Check for updates"), close(func() { m.CheckAppUpdate(false) })).Icon(ic(lucide.CircleFadingArrowUp)).Group(tr("App")).Keywords("update version release"),
			ui.CommandItem(tr("Toggle dark mode"), close(func() {
				m.UpdateSettings(func(s *storeSettings) {
					if s.Theme == "dark" {
						s.Theme = "light"
					} else {
						s.Theme = "dark"
					}
				})
			})).Icon(ic(lucide.Moon)).Group(tr("App")).Keywords("theme appearance"),
		)
		for _, sb := range st.sandboxes {
			id := sb.id
			state := sandbox.State{Status: sb.status, Paused: sb.paused}
			kw := sb.status.String()
			if sb.paused {
				kw = "paused"
			}
			out = append(out, ui.CommandItem(sb.name, close(func() { m.ShowSandbox(id, 0) })).
				Icon(widget.Box(dot(stateColor(state), 8)).Pad(3)).
				Group(tr("Open sandbox")).
				Keywords(sb.image, statusText(state), kw))
		}
		for _, c := range st.extCommands {
			cmd := c
			out = append(out, ui.CommandItem(cmd.title, close(func() { m.RunExtensionCommand(cmd.extID, cmd.cmdID) })).
				Icon(extIcon(cmd.icon).Size(14).Color(colorGray)).
				Group(cmd.extName).
				Keywords(cmd.extID, cmd.cmdID, cmd.extName))
		}
		return out
	}
	cmd := ui.Command(m.PaletteQuery).Height(340).StableHeight().Hints()
	// The Containers and Pods entries are greyed out as the sidebar's
	// items are, and existing sandboxes can be jumped to directly.
	state := ggui.Derived(func() paletteState {
		rows := m.Rows.Get()
		sbs := make([]paletteSandbox, len(rows))
		for i, r := range rows {
			sbs[i] = paletteSandbox{id: r.Spec.ID, name: r.Spec.Name, image: r.Spec.Image, status: r.State.Status, paused: r.State.Paused}
		}
		lang := m.Lang.Get()
		var extCmds []paletteExtCmd
		for _, info := range m.Extensions.Get() {
			if !info.State.CanRun() {
				continue
			}
			extName := info.LocalizedName(lang)
			for _, c := range info.Commands {
				title := info.LocalizedCommandTitle(c.ID, lang)
				extCmds = append(extCmds, paletteExtCmd{
					extID:   info.ID,
					cmdID:   c.ID,
					title:   title,
					extName: extName,
					icon:    info.Icon,
				})
			}
		}
		return paletteState{
			containers:        m.ContainersOpen.Get(),
			pods:              m.PodsOpen.Get(),
			sandboxEngine:     m.Settings.Get().SandboxEngine,
			extensionsEnabled: m.Settings.Get().Extensions,
			active:            m.TotalRunning.Get(),
			sandboxes:         sbs,
			extCommands:       extCmds,
		}
	}).WithEqual(paletteState.equal)
	ggui.Watch(state, func(st paletteState) { cmd.Entries(entries(st)...) })
	return ui.CommandDialog(m.PaletteOpen, cmd)
}

// RefreshAll reloads everything rungv reports.
func (m *Model) RefreshAll() {
	m.DetectEngine(nil)
	m.LoadDocker()
	m.LoadDockerImages()
	m.LoadLocalRegistry()
	m.LoadPods()
	switch ggui.Peek(m.Page) {
	case PageContainers:
		m.LoadContainerLogs()
	case PageNetworks:
		m.LoadDockerNetworks()
	case PagePods:
		m.LoadPodLogs()
	case PageLabs:
		if amd64BuilderPlatform() {
			m.CheckAmd64()
			m.refreshAmd64State()
			m.refreshMicroVMState()
		}
		m.refreshExtensions()
	case PageExtensions:
		m.refreshExtensions()
	}
	m.CheckKubeconfig()
	for _, l := range m.Links() {
		m.CheckLink(l)
	}
	m.refreshStates()
}
