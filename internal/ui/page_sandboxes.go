package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// hasProgramPrefix reports whether line starts with rungv or rungvx followed by sep.
func hasProgramPrefix(line, sep string) bool {
	_, ok := rungv.CutProgram(line, sep)
	return ok
}

func sandboxesPage(m *Model) ggui.Widget {
	visible := ggui.Derived(func() []Row {
		rows := m.Rows.Get()
		q := strings.ToLower(strings.TrimSpace(m.Query.Get()))
		status := m.SandboxStatus.Get()
		var out []Row
		for _, r := range rows {
			switch status {
			case "running":
				if !r.State.Status.Active() {
					continue
				}
			case "stopped":
				if r.State.Status.Active() {
					continue
				}
			}
			if q == "" || strings.Contains(strings.ToLower(r.Spec.Name+" "+r.Spec.Image), q) {
				out = append(out, r)
			}
		}
		return out
	})
	summary := ui.CaptionOf(ggui.Combine(m.Running, m.Specs, func(n int, s []sandbox.Spec) string {
		return fmt.Sprintf(tr("%d running · %d total. Ports and mounts are fixed when a sandbox starts."), n, len(s))
	}))
	selected := ggui.Bind(m.Selected.Get, m.Select)

	count := ggui.Combine(visible, m.Rows, func(v, all []Row) string { return showing(len(v), len(all)) })
	statusFilter := ui.ToggleGroup(m.SandboxStatus).Options([]string{"all", "running", "stopped"}).Format(func(v string) string {
		switch v {
		case "running":
			return tr("Running")
		case "stopped":
			return tr("Stopped")
		}
		return tr("All")
	}).Outline().Size(ui.ButtonExtraSmall).Spacing(0).Name(tr("Status"))
	mainToolbar := widget.Row(
		widget.Expanded(filterBar(m.Query, tr("Filter by name or image"), count)),
		statusFilter,
	).Gap(12).Align(widget.AlignCenter)
	rowKey := func(r Row) string { return r.Spec.ID }
	rowLabel := func(r Row) string { return r.Spec.Name }
	checked := newBulkSelection()
	checkedItems := ggui.Combine(visible, checked, func(rows []Row, sel map[string]bool) []Row {
		return selectedItems(rows, sel, rowKey, nil)
	}).WithEqual(nil)
	checkedCount := ggui.Map(checkedItems, func(rows []Row) int { return len(rows) })
	canStartAny := ggui.Map(checkedItems, func(rows []Row) bool {
		for _, r := range rows {
			if !r.State.Status.Active() {
				return true
			}
		}
		return false
	})
	canStopAny := ggui.Map(checkedItems, func(rows []Row) bool {
		for _, r := range rows {
			if r.State.Status.Active() {
				return true
			}
		}
		return false
	})
	checkedIDs := func() []string {
		rows := ggui.Peek(checkedItems)
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.Spec.ID
		}
		return ids
	}
	clearChecked := func() { clearSelection(checked) }
	toolbar := bulkToolbar(mainToolbar, checkedCount, clearChecked,
		ui.Button(tr("Start"), func() { m.BulkStartSandboxes(checkedIDs(), clearChecked) }).
			Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Play, 13)).
			BindDisabled(ggui.Not(canStartAny)),
		ui.Button(tr("Stop"), func() { m.BulkStopSandboxes(checkedIDs(), clearChecked) }).
			Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Square, 13)).
			BindDisabled(ggui.Not(canStopAny)),
		ui.Button(tr("Delete…"), func() { m.AskDeleteSandboxes(checkedIDs(), clearChecked) }).
			Destructive().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Trash, 13)),
	)
	// The table is as tall as its rows up to five, and scrolls past that, so
	// the details below stay in reach above the fold.
	table := ui.Table(visible, rowKey,
		selectCheckboxColumn(visible, checked, rowKey, rowLabel, nil),
		ui.Col(tr("Status"), func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(r Row) sandbox.State { return r.State }), statusLabel)
		}).W(150),
		ui.Col(tr("Name"), func(r ggui.Readable[Row]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, func(r Row) string { return r.Spec.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, func(r Row) string { return r.Spec.Image })).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(2),
		ui.Col(tr("Ports"), func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, portsSummary)).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col(tr("CPU / Memory"), func(r ggui.Readable[Row]) ggui.Widget {
			return usageCell(ggui.Combine(r, m.SandboxUsage, func(r Row, all map[string]Usage) usageView {
				u, ok := all[r.Spec.ID]
				return usageOf(u, ok && r.State.Status.Active())
			}))
		}).W(usageWidth),
		ui.Col(tr("Uptime"), func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(r Row, now time.Time) string {
				if r.State.Status.Active() {
					return uptime(r.State.StartedAt, now)
				}
				if !r.State.ExitedAt.IsZero() {
					return tr("stopped %s", ago(r.State.ExitedAt, now))
				}
				return "—"
			})).Size(12).NoWrap()
		}).W(160),
		ui.Col("", func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(r, func(r Row) ggui.Widget {
				return widget.Row(sandboxQuickActions(m, r), rowMenu(m, r)).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(120).Identified("actions"),
	).BindSelected(selected).RowName(func(r Row) string { return r.Spec.Name }).RowHeight(48).MaxRows(5).
		Toolbar(toolbar)

	empty := ui.Empty(tr("No sandboxes"), tr("A sandbox runs one OCI image in its own unprivileged process. Create one, or open Docker or Kubernetes.")).
		Media(glyph(lucide.Box, 32).Color(colorGray)).
		Action(widget.Row(
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
			ui.Button("Docker", m.OpenDocker).Outline(),
		).Gap(8)).Outline()

	hasVisible := ggui.Map(visible, func(v []Row) bool { return len(v) > 0 })
	filteredEmpty := ggui.If(ggui.Not(hasVisible), func() ggui.Widget {
		return widget.Padding(widget.Row(
			ui.Caption(tr("No sandboxes match the current filter.")),
			ui.Button(tr("Clear filter"), func() {
				m.Query.Set("")
				m.SandboxStatus.Set("all")
			}).Outline().Size(ui.ButtonExtraSmall),
		).Gap(10).Align(widget.AlignCenter).Justify(widget.JustifyCenter), 16, 12)
	})

	list := ggui.If(ggui.Map(m.Specs, func(s []sandbox.Spec) bool { return len(s) > 0 }), func() ggui.Widget {
		return widget.Column(
			tableCard(widget.Column(table, filteredEmpty).Stretch()),
			ggui.If(hasVisible, func() ggui.Widget { return detailPanel(m) }),
		).Gap(16).Stretch()
	}).Else(func() ggui.Widget { return empty })

	return page(
		pageHeader(tr("Sandboxes"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh sandboxes"), m.RefreshAll), tr("Refresh")),
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
		),
		list,
	)
}

func portsSummary(r Row) string {
	if len(r.State.Ports) > 0 && r.State.Status.Active() {
		var parts []string
		for i, p := range r.State.Ports {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("+%d", len(r.State.Ports)-3))
				break
			}
			parts = append(parts, fmt.Sprintf("%s→%d", shortHost(p.Host), p.Guest))
		}
		return strings.Join(parts, ", ")
	}
	if len(r.Spec.Ports) == 0 {
		return "—"
	}
	return strings.Join(r.Spec.Ports, ", ")
}

func shortHost(h string) string {
	if strings.HasPrefix(h, "/") || strings.Contains(h, `\`) {
		return "unix:" + h[strings.LastIndexAny(h, `/\`)+1:]
	}
	return strings.TrimPrefix(h, "127.0.0.1:")
}

// sandboxPauseAction returns "pause", "resume", or "" for a sandbox's state.
func sandboxPauseAction(st sandbox.State) string {
	if !st.Status.Active() || st.Status == sandbox.Stopping {
		return ""
	}
	if st.Paused {
		return "resume"
	}
	if st.Control {
		return "pause"
	}
	return ""
}

func rowMenu(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	entries := []ggui.Widget{
		ui.MenuItem(tr("Logs"), func() { m.ShowSandbox(id, 0) }),
		ui.MenuItem(tr("Edit…"), func() { m.EditSandbox(id) }),
		ui.MenuItem(tr("Duplicate…"), func() { m.Duplicate(id) }),
		ui.MenuItem(tr("Copy command line"), func() { m.Copy(r.Spec.CommandLine(sandbox.Paths{}), tr("rungvx command line")) }),
	}
	switch sandboxPauseAction(r.State) {
	case "pause":
		entries = append(entries, ui.MenuItem(tr("Pause"), func() { m.PauseSandbox(id) }))
	case "resume":
		entries = append(entries, ui.MenuItem(tr("Resume"), func() { m.ResumeSandbox(id) }))
	}
	if r.State.Status == sandbox.Running && !r.State.Paused {
		entries = append(entries, ui.MenuItem(tr("Restart"), func() { m.RestartSandbox(id) }))
	}
	if r.State.Status.Active() && r.State.Control {
		entries = append(entries, ui.MenuSub(tr("Sentry log level"),
			ui.MenuItem(tr("Warning"), func() { m.SetSandboxLogLevel(id, "warning") }),
			ui.MenuItem(tr("Info"), func() { m.SetSandboxLogLevel(id, "info") }),
			ui.MenuItem(tr("Debug"), func() { m.SetSandboxLogLevel(id, "debug") }),
		))
	}
	entries = append(entries, ui.MenuDivider(), ui.MenuItem(tr("Delete…"), func() { m.AskDelete(id) }).Destructive())
	return moreMenu(tr("More actions for %s", r.Spec.Name), entries...)
}

// ---- Detail -------------------------------------------------------------

func detailPanel(m *Model) ggui.Widget {
	current := ggui.Combine(m.Rows, m.Selected, func(rows []Row, id string) Row {
		for _, r := range rows {
			if r.Spec.ID == id {
				return r
			}
		}
		return Row{}
	})
	hasSelection := ggui.Map(current, func(r Row) bool { return r.Spec.ID != "" })
	return entering(m.Selected, func(string) ggui.Widget {
		return ggui.If(hasSelection, func() ggui.Widget {
			header := ggui.View(current, func(r Row) ggui.Widget { return detailHeader(m, r) })
			tabs := ui.Tabs(m.Detail,
				ui.Tab(tr("Logs"), logsView(m, current)),
				ui.Tab(tr("Connect"), ggui.View(current, func(r Row) ggui.Widget { return connectView(m, r) })),
				ui.Tab(tr("Configuration"), ggui.View(ggui.Map(current, func(r Row) sandbox.Spec { return r.Spec }).WithEqual(sandbox.Spec.Equal), func(s sandbox.Spec) ggui.Widget { return configView(m, s) })),
				ui.Tab(tr("Snapshots"), ggui.View(current, func(r Row) ggui.Widget { return sandboxSnapshots(m, r) })),
				ui.Tab(tr("Terminal"), sandboxTerminal(m)),
			).Line().Name(tr("Sandbox details"))
			return ui.Card(widget.Column(header, tabs).Gap(12).Stretch())
		}).Else(func() ggui.Widget {
			return ui.Empty(tr("Select a sandbox"), tr("Its logs, ports and settings show here.")).Outline()
		})
	})
}

func detailHeader(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	st := r.State
	var actions []ggui.Widget
	switch {
	case st.Status == sandbox.Stopping:
		actions = append(actions, ui.Button(tr("Stopping…"), nil).Outline().Disabled(true))
	case st.Status.Active() && st.Paused:
		actions = append(actions,
			ui.Button(tr("Resume"), func() { m.ResumeSandbox(id) }).Icon(glyph(lucide.Play, 14)),
			ui.Button(tr("Stop"), func() { m.StopSandbox(id) }).Outline().Icon(glyph(lucide.Square, 14)),
		)
	case st.Status.Active():
		if st.Control {
			actions = append(actions, ui.Button(tr("Pause"), func() { m.PauseSandbox(id) }).Outline().Icon(glyph(lucide.Pause, 14)))
		}
		actions = append(actions,
			ui.Button(tr("Stop"), func() { m.StopSandbox(id) }).Outline().Icon(glyph(lucide.Square, 14)),
			ui.Button(tr("Restart"), func() { m.RestartSandbox(id) }).Outline().Icon(glyph(lucide.RotateCcw, 14)),
		)
	default:
		actions = append(actions, ui.Button(tr("Start"), func() { m.StartSandbox(id) }).Icon(glyph(lucide.Play, 14)))
	}
	actions = append(actions,
		ui.Button(tr("Edit"), func() { m.EditSandbox(id) }).Outline().Icon(glyph(lucide.Pencil, 14)),
		rowMenu(m, r),
	)
	meta := []string{r.Spec.Image}
	if st.Platform != "" {
		meta = append(meta, st.Platform)
	}
	if st.PID != 0 {
		meta = append(meta, tr("pid %d", st.PID))
	}
	if st.Restored != "" {
		meta = append(meta, tr("restored from %s", shortPath(st.Restored)))
	}
	usage := usageLine(ggui.Map(m.SandboxUsage, func(all map[string]Usage) usageView {
		u, ok := all[id]
		return usageOf(u, ok && st.Status.Active())
	}))
	head := widget.Column(
		widget.Row(ui.Title(r.Spec.Name).Size(20), statusLabel(st), usage).Gap(12).Align(widget.AlignCenter),
		ui.Caption(strings.Join(meta, " · ")).Size(12),
	).Gap(4)
	var problem ggui.Widget = widget.Box()
	if st.Status == sandbox.Failed {
		desc := st.Error
		if st.Advice != "" {
			desc = st.Advice + "\n" + st.Error
		}
		problem = ui.Alert(tr("Exited with code %d", st.ExitCode), desc).Destructive().Icon(icons.Alert)
	}
	return widget.Column(
		widget.Responsive(680,
			widget.Row(widget.Expanded(head), widget.Row(actions...).Gap(8)).Gap(16).Align(widget.AlignCenter),
			widget.Column(head, widget.Wrap(actions...).Gap(8)).Gap(12).Stretch(),
		),
		problem,
	).Gap(12).Stretch()
}

func shortPath(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// logsView follows the selected sandbox's output, and on running gvx-ctrl/2
// sandboxes allows switching to buffered guest kernel logs (rungvx dmesg).
func logsView(m *Model, current ggui.Readable[Row]) ggui.Widget {
	hasCtrl := ggui.Map(current, func(r Row) bool {
		return r.State.Status.Active() && r.State.Control
	})
	mode := ggui.Combine(hasCtrl, m.DmesgMode, func(ctrl, dmesg bool) bool {
		return ctrl && dmesg
	})
	return ggui.View(mode, func(showDmesg bool) ggui.Widget {
		switchBtn := ggui.If(hasCtrl, func() ggui.Widget {
			if showDmesg {
				return ui.Button(tr("Output"), func() { m.DmesgMode.Set(false) }).Outline().Size(ui.ButtonExtraSmall)
			}
			return ui.Button(tr("Kernel (dmesg)"), func() {
				m.DmesgMode.Set(true)
				m.LoadSandboxDmesg(ggui.Peek(m.Selected))
			}).Outline().Size(ui.ButtonExtraSmall)
		})
		if showDmesg {
			return logViewer(m, m.DmesgLines, tr("No kernel log entries."),
				switchBtn,
				tip(iconButton(lucide.RefreshCw, tr("Refresh kernel logs"), func() { m.LoadSandboxDmesg(ggui.Peek(m.Selected)) }), tr("Refresh")))
		}
		return logViewer(m, m.Logs, tr("No output yet. Start the sandbox to see what it writes."),
			switchBtn,
			tip(iconButton(lucide.Eraser, tr("Clear logs"), func() { m.ClearLogs(ggui.Peek(m.Selected)) }), tr("Clear")))
	})
}

// logViewer shows lines of a log, with a field that filters them, their
// count, a wrap toggle, a button that copies them and the actions given after it.
func logViewer(m *Model, lines ggui.Readable[[]sandbox.LogLine], empty string, actions ...ggui.Widget) ggui.Widget {
	filter := ggui.State("")
	wrap := ggui.State(false)
	shown := ggui.Combine(lines, filter, func(lines []sandbox.LogLine, f string) []sandbox.LogLine {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			return lines
		}
		var out []sandbox.LogLine
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l.Text), f) {
				out = append(out, l)
			}
		}
		return out
	}).WithEqual(nil)
	makeRow := func(l sandbox.LogLine, wrapped bool) ggui.Widget {
		t := ui.Mono(l.Text).Size(12).Selectable()
		if !wrapped {
			t = t.NoWrap().Ellipsis()
		}
		switch l.Stream {
		case sandbox.System:
			t = t.Color(colorLink)
		case sandbox.Stderr:
			lower := strings.ToLower(l.Text)
			switch {
			case hasProgramPrefix(l.Text, ":"):
				t = t.Color(colorGray)
			case strings.Contains(lower, "[error]") || strings.Contains(lower, "error:") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic"):
				t = t.Color(colorRed)
			case strings.Contains(lower, "[warn]") || strings.Contains(lower, "warning:"):
				t = t.Color(colorAmber)
			}
		}
		return widget.Row(
			ui.Mono(l.Time.Format("15:04:05")).Size(11).Color(colorGray).NoWrap(),
			widget.Expanded(t),
		).Gap(10).Align(widget.AlignStart)
	}
	list := ggui.View(wrap, func(wrapped bool) ggui.Widget {
		each := widget.EachKeyed(shown, func(l sandbox.LogLine) int64 { return l.Seq }, func(row widget.EachItem[sandbox.LogLine]) ggui.Widget {
			return makeRow(row.Item, wrapped)
		}).Else(func() ggui.Widget {
			return ggui.View(ggui.Map(filter, func(s string) bool { return strings.TrimSpace(s) != "" }), func(filtered bool) ggui.Widget {
				if filtered {
					return widget.Padding(ui.Caption(tr("No matching lines.")), 12)
				}
				return widget.Padding(ui.Caption(empty), 12)
			})
		})
		if !wrapped {
			each = each.ItemExtent(19)
		} else {
			each = each.Gap(2)
		}
		return widget.Scroll(widget.Padding(each, 8, 12)).FollowEnd()
	})
	clearFilter := ggui.If(ggui.Map(filter, func(s string) bool { return s != "" }), func() ggui.Widget {
		return iconButton(lucide.X, tr("Clear the filter"), func() { filter.Set("") })
	})
	lineCount := ui.CaptionOf(ggui.Combine(shown, lines, func(s, all []sandbox.LogLine) string {
		if len(s) == len(all) {
			return fmt.Sprintf(tr("%d lines"), len(all))
		}
		return tr("%d of %d", len(s), len(all))
	})).Size(12).NoWrap()
	wrapBtn := ggui.View(wrap, func(on bool) ggui.Widget {
		b := ui.ButtonOf(glyph(lucide.TextWrap, 15), func() { wrap.Set(!on) }).Name(tr("Wrap lines")).Size(ui.ButtonIconSmall)
		if on {
			b = b.Secondary()
		} else {
			b = b.Ghost()
		}
		return tip(b, tr("Wrap lines"))
	})
	bar := []ggui.Widget{
		widget.Box(ui.TextField(filter).Placeholder(tr("Filter lines")).Name(tr("Filter logs"))).Width(240),
		clearFilter,
		widget.Spacer(),
		lineCount,
		wrapBtn,
		tip(iconButton(lucide.Copy, tr("Copy logs"), func() {
			var b strings.Builder
			for _, l := range ggui.Peek(shown) {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
			m.Copy(b.String(), tr("The visible log lines are on the clipboard."))
		}), tr("Copy")),
	}
	toolbar := widget.Row(append(bar, actions...)...).Gap(6).Align(widget.AlignCenter)
	return widget.Column(
		toolbar,
		widget.Box(list).Height(340).Radius(8).Border(1, colorBorder).Fill(colorSurfaceInset),
	).Gap(10).Stretch()
}

// connectView tells how to reach what the sandbox serves.
func connectView(m *Model, r Row) ggui.Widget {
	var parts []ggui.Widget
	switch r.Spec.Recipe {
	case recipes.Docker:
		parts = append(parts, dockerConnect(m, r))
	case recipes.Kubernetes:
		parts = append(parts, kubeConnect(m, r))
	case recipes.Registry:
		parts = append(parts, registryConnect(m, r))
	}
	note := tr("Bound on the host before the guest starts. A guest port outside these is reachable only inside the sandbox.")
	if sandbox.HasAutoPublish(r.Spec) {
		note = tr("Bound on the host before the guest starts, and as the guest listens on a port: at 127.0.0.1 under the same number, while it listens.")
	}
	parts = append(parts, sectionTitle(tr("Published ports"), note))
	if r.State.Status.Active() && (len(r.State.Ports) > 0 || len(r.State.Busy) > 0) {
		for _, p := range r.State.Ports {
			label := fmt.Sprintf("%s → %d/%s", p.Host, p.Guest, p.Proto)
			if src := portSourceLabel(p.Source); src != "" {
				label += " · " + src
			}
			var act ggui.Widget = widget.Box()
			if url := p.URL(); url != "" {
				act = widget.Row(
					ui.Button(tr("Open"), func() { m.Open(url) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)),
					iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(url, url) }),
				).Gap(4)
			} else {
				act = iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(p.Host, p.Host) })
			}
			parts = append(parts, widget.Row(glyph(lucide.Plug, 14).Color(colorGreen), widget.Expanded(ui.Mono(label).Size(12)), act).Gap(10).Align(widget.AlignCenter))
		}
		for _, b := range r.State.Busy {
			parts = append(parts, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
				widget.Expanded(widget.Column(
					ui.Mono(fmt.Sprintf("%s/%s", b.Host, b.Proto)).Size(12),
					ui.Caption(tr("The guest listens on it, but something on this computer has the port. It is published once that lets it go and the guest listens again.")).Size(11),
				).Gap(1)),
			).Gap(10).Align(widget.AlignStart))
		}
	} else if len(r.Spec.Ports) > 0 {
		for _, p := range r.Spec.Ports {
			parts = append(parts, widget.Row(glyph(lucide.Plug, 14).Color(colorGray), ui.Mono(p).Size(12)).Gap(10))
		}
		parts = append(parts, ui.Caption(tr("Start the sandbox to bind these ports; a port written without a host port gets a free one.")))
	} else {
		parts = append(parts, ui.Caption(tr("No ports are published. Edit the sandbox to add some, such as 8080:80.")))
	}
	if r.Spec.Workspace != "" {
		parts = append(parts, sectionTitle(tr("Workspace"), tr("Mounted at /workspace in the guest.")),
			widget.Row(widget.Expanded(ui.Mono(rungv.ExpandHome(r.Spec.Workspace)).Size(12)),
				ui.Button(tr("Reveal"), func() { m.Open(rungv.ExpandHome(r.Spec.Workspace)) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(widget.AlignCenter))
	}
	return widget.Padding(widget.Column(parts...).Gap(10).Stretch(), 8, 0)
}

func dockerConnect(m *Model, r Row) ggui.Widget {
	status := tr("Start the sandbox, then point the docker CLI at it.")
	switch {
	case r.State.Status == sandbox.Running && r.State.Paused:
		status = tr("The Docker Engine is paused. Resume it so the docker CLI on this machine can use it.")
	case r.State.Status == sandbox.Running:
		status = tr("dockerd is listening. The docker CLI on this machine can use it.")
	}
	return widget.Column(
		sectionTitle("Docker CLI", status),
		linkPanel(m, m.DockerLink, func(v LinkView) []ggui.Widget {
			return dockerCommands(m, v, copyable(m, "export DOCKER_HOST="+v.Want, "DOCKER_HOST"))
		}),
		widget.Row(ui.Button(tr("Open Docker page"), func() { m.Go(PageDocker) }).Outline().Size(ui.ButtonSmall)),
		ui.Divider(),
	).Gap(10).Stretch()
}

func kubeConnect(m *Model, r Row) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return widget.Column(
		sectionTitle("kubectl", tr("The server writes its kubeconfig to the host when it starts.")),
		linkPanel(m, m.KubeLink, func(LinkView) []ggui.Widget {
			return []ggui.Widget{
				copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
				copyable(m, "kubectl get nodes", "kubectl get nodes"),
			}
		}),
		widget.Row(
			ui.Button(tr("Reveal kubeconfig"), func() { m.Open(cfg) }).Outline().Size(ui.ButtonSmall),
			ui.Button(tr("Open Kubernetes page"), func() { m.Go(PageKubernetes) }).Outline().Size(ui.ButtonSmall),
		).Gap(8),
		ui.Divider(),
	).Gap(10).Stretch()
}

// registryConnect tells how images go through the image registry: pushed
// from the Docker Engine, and pulled by the cluster's pods.
func registryConnect(m *Model, r Row) ggui.Widget {
	host, local := recipes.RegistryHost(), recipes.RegistryLocal()
	catalog := "http://127.0.0.1:" + itoa(recipes.RegistryPort) + "/v2/_catalog"
	return widget.Column(
		sectionTitle(tr("Push from the Docker Engine"), tr("Images built with docker go to the registry as %s, the name the engine's sandbox reaches this computer by. Images → Docker Engine → Push to cluster registry does the same.", host)),
		copyable(m, "docker tag app:dev "+host+"/app:dev", "docker tag"),
		copyable(m, "docker push "+host+"/app:dev", "docker push"),
		sectionTitle(tr("Run it in the cluster"), tr("Pods of the Kubernetes recipe's cluster pull images named %s/… or %s/… from the registry.", host, local)),
		copyable(m, "kubectl create deployment app --image="+host+"/app:dev", tr("example deployment")),
		sectionTitle(tr("From tools on this computer"), tr("Tools that run here, not in a sandbox, reach the registry as %s.", local)),
		widget.Row(
			ui.Button(tr("List images"), func() { m.Open(catalog) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)).Disabled(r.State.Status != sandbox.Running || r.State.Paused),
		).Gap(8),
		ui.Divider(),
	).Gap(10).Stretch()
}

// configView shows the spec in the words of the rungv command line.
func configView(m *Model, s sandbox.Spec) ggui.Widget {
	line := wrapCommand(s.RunArgs(sandbox.Paths{}))
	join := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return strings.Join(v, "\n")
	}
	var mounts []string
	for _, mt := range s.Mounts {
		switch mt.Type {
		case sandbox.MountTmpfs:
			mounts = append(mounts, fmt.Sprintf("tmpfs %s %s", mt.Target, mt.Size))
		default:
			var opts []string
			if mt.ReadOnly {
				opts = append(opts, "ro")
			}
			if mt.Size != "" {
				opts = append(opts, "size="+mt.Size)
			}
			if mt.Ephemeral {
				opts = append(opts, "ephemeral")
			}
			if mt.Symlinks != "" {
				opts = append(opts, "symlinks="+mt.Symlinks)
			}
			suffix := ""
			if len(opts) > 0 {
				suffix = " (" + strings.Join(opts, ", ") + ")"
			}
			src := mt.Source
			if src == "" && mt.Ephemeral {
				src = "ephemeral"
			}
			mounts = append(mounts, fmt.Sprintf("%s %s → %s%s", mt.Type, src, mt.Target, suffix))
		}
	}
	if s.Workspace != "" {
		mounts = append(mounts, tr("workspace %s → /workspace", s.Workspace))
	}
	if s.RootfsVolume != "" {
		mounts = append(mounts, tr("root file system → volume %s", s.RootfsVolume))
	}
	net := s.Network
	if net == "" {
		net = "nat"
	}
	if s.IsolateLAN {
		net += tr(", LAN isolated")
	}
	limits := []string{}
	if s.Memory != "" {
		limits = append(limits, tr("memory %s", s.Memory))
	}
	if s.CPUs != "" {
		limits = append(limits, tr("%s CPUs", s.CPUs))
	}
	if s.PidsLimit > 0 {
		limits = append(limits, tr("%d PIDs", s.PidsLimit))
	}
	if s.UsesGPU() {
		gpuText := tr("GPU %s", s.GPU)
		if s.GPUMemoryLimit != "" {
			gpuText += " (" + s.GPUMemoryLimit + ")"
		}
		limits = append(limits, gpuText)
	}
	sec := tr("privileged inside the sandbox")
	if s.Unprivileged {
		sec = tr("unprivileged")
	}
	if s.ReadOnlyRoot {
		sec += ", " + tr("read-only root")
	}
	if s.HostCACerts {
		sec += tr(", trusts host root certificates")
	}
	snapSummary := tr("off")
	switch {
	case s.UsesGPU():
		snapSummary = tr("unavailable with GPU acceleration")
	case s.Checkpoint:
		snapSummary = tr("enabled (control socket + bundle directory)")
	case ggui.Peek(m.RungvFeatures).Control:
		snapSummary = tr("on demand (control socket)")
	}
	return widget.Padding(widget.Column(
		ui.CodeBlock(line).Header(tr("Command line")).Language("sh").CopyButton(true).Small(),
		widget.Column(
			kvText(tr("Image"), s.Image),
			kvText(tr("Command"), s.Command),
			kvText(tr("Environment"), join(s.Env)),
			kvText(tr("Environment files"), join(s.EnvFiles)),
			kvText(tr("Mounts"), join(mounts)),
			kvText(tr("Network"), net),
			kvText(tr("Limits"), strings.Join(limits, ", ")),
			kvText(tr("Security"), sec),
			kvText(tr("Snapshots"), snapSummary),
			kvText(tr("Start with app"), startsWithApp(s)),
			kvText(tr("Ready when log has"), s.ReadyPattern),
		).Gap(8).Stretch(),
	).Gap(14).Stretch(), 8, 0)
}

// startsWithApp says whether a sandbox starts with the app: one set to
// does, and a recipe's engine does when it was left running.
func startsWithApp(s sandbox.Spec) string {
	switch {
	case s.AutoStart:
		return tr("yes")
	case s.Recipe != "":
		return tr("when left running")
	}
	return tr("no")
}

func sandboxSnapshots(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if r.Spec.UsesGPU() {
		return widget.Padding(ui.Alert(tr("Snapshots are unavailable with GPU acceleration"),
			tr("rungvx does not support checkpoint and restore while GPU acceleration (-gpu) is enabled. Turn GPU acceleration off to take snapshots.")).
			Icon(icons.Info), 8, 0)
	}
	if !r.Spec.Checkpoint && !r.State.Control && !ggui.Peek(m.RungvFeatures).Control {
		return widget.Padding(ui.Alert(tr("Snapshots are off for this sandbox"),
			tr("A snapshot needs a control socket and a bundle directory opened before the guest starts. Turn snapshots on, then restart the sandbox.")).
			Icon(icons.Info).
			Action(ui.Button(tr("Turn on"), func() {
				spec := r.Spec.Clone()
				spec.Checkpoint = true
				m.OpenEditor(spec)
			}).Size(ui.ButtonExtraSmall).Outline()), 8, 0)
	}
	running := r.State.Status.Active() && r.State.Status != sandbox.Stopping && !r.State.Paused && (r.Spec.Checkpoint || r.State.Control)
	snaps := ggui.Map(m.Snapshots, func(all []sandbox.Snapshot) []sandbox.Snapshot {
		var out []sandbox.Snapshot
		for _, s := range all {
			if s.SandboxID == id {
				out = append(out, s)
			}
		}
		return out
	})
	return widget.Padding(widget.Column(
		widget.Row(
			ui.Button(tr("Snapshot and stop"), func() { m.Checkpoint(id, false) }).Disabled(!running).Icon(glyph(lucide.Camera, 14)),
			ui.Button(tr("Snapshot, keep running"), func() { m.Checkpoint(id, true) }).Outline().Disabled(!running),
		).Gap(8),
		ui.Caption(tr("A snapshot holds the task graph, memory pages, volumes and network namespaces. Restoring needs the same mounts, which this sandbox's settings provide.")),
		snapshotList(m, snaps, false, ggui.Const(tr("No snapshots yet."))),
	).Gap(12).Stretch(), 8, 0)
}

func snapshotList(m *Model, snaps ggui.Readable[[]sandbox.Snapshot], showSandbox bool, emptyText ggui.Readable[string]) ggui.Widget {
	return snapshotListWithSelection(m, snaps, showSandbox, emptyText, nil)
}

func snapshotListWithSelection(m *Model, snaps ggui.Readable[[]sandbox.Snapshot], showSandbox bool, emptyText ggui.Readable[string], sel *ggui.StateValue[map[string]bool]) ggui.Widget {
	render := func(specNames map[string]string) ggui.Widget {
		return widget.EachKeyed(snaps, func(s sandbox.Snapshot) string { return s.Dir }, func(row widget.EachItem[sandbox.Snapshot]) ggui.Widget {
			s := row.Item
			title := s.Time.Format(tr("Jan 2, 15:04:05"))
			desc := formatBytes(s.Size)
			if showSandbox {
				if name, ok := specNames[s.SandboxID]; ok {
					title = name + " · " + title
				} else {
					title = tr("(deleted sandbox) · ") + title
				}
			}
			var media ggui.Widget = glyph(lucide.Camera, 18).Color(colorBlue)
			if sel != nil {
				dir := s.Dir
				cb := quietTableCheckbox(ui.Checkbox(ggui.Bind(
					func() bool { return sel.Get()[dir] },
					func(on bool) { toggleSelectionKey(sel, dir, on) },
				), "").Name(tr("Select %s", title)))
				media = widget.Row(cb, media).Gap(8).Align(widget.AlignCenter)
			}
			return ui.Item(title, desc+" · "+s.Dir).
				Media(media).
				Action(widget.Row(
					ui.Button(tr("Restore"), func() { m.Restore(s) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RotateCcw, 14)),
					tip(iconButton(lucide.FolderOpen, tr("Reveal"), func() { m.Open(s.Dir) }), tr("Reveal in file manager")),
					tip(iconButton(lucide.Trash, tr("Delete snapshot"), func() { m.DeleteSnapshot(s) }), tr("Delete snapshot")),
				).Gap(4).Align(widget.AlignCenter)).Outline().Small()
		}).Gap(8).Else(func() ggui.Widget {
			return ui.CaptionOf(emptyText)
		})
	}
	if !showSandbox {
		return render(nil)
	}
	names := ggui.Map(m.Specs, func(specs []sandbox.Spec) map[string]string {
		out := make(map[string]string, len(specs))
		for _, s := range specs {
			out[s.ID] = s.Name
		}
		return out
	}).WithEqual(nil)
	return ggui.View(names, render)
}

// sandboxTerminal is a shell in the chosen sandbox, as rungv exec -it runs one.
func sandboxTerminal(m *Model) ggui.Widget {
	return terminalTab(m, m.sandboxShell(), tr("A shell in the sandbox, as rungvx exec -it runs one."))
}

// portSourceLabel words what published a port, "" for one published at
// start, as most are.
func portSourceLabel(source string) string {
	switch source {
	case "auto":
		return tr("auto")
	case "dynamic":
		return tr("added")
	}
	return ""
}
