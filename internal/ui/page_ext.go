package ui

import (
	"image/color"
	"slices"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

func extensionPage(m *Model, extID, viewID string) ggui.Widget {
	sess := m.extensionViewSession(extID, viewID)
	logsOpen := ggui.State(false)
	routeKey := ExtensionPage(extID, viewID)

	ggui.Effect(func() ggui.Cleanup {
		curPage := m.Page.Get()
		exts := m.Extensions.Get()
		locale := m.Lang.Get()
		if curPage != routeKey {
			m.cancelExtensionStream()
			return nil
		}
		info, found := findExtensionInfo(exts, extID)
		if !found || !canStreamExtension(info) {
			m.cancelExtensionStream()
			return nil
		}
		m.ensureExtensionStream(extID, viewID, locale)
		return nil
	})
	ggui.OnCleanup(m.cancelExtensionStream)

	type viewKey struct {
		found  bool
		info   ext.ExtensionInfo
		rev    int
		logs   int
		devMod bool
	}
	combined := ggui.Derived(func() viewKey {
		info, found := findExtensionInfo(m.Extensions.Get(), extID)
		logs := 0
		if found && info.State == ext.StateBuilding {
			logs = m.ExtBuildLogsTick.Get()
		}
		return viewKey{
			found:  found,
			info:   info,
			rev:    sess.rev.Get(),
			logs:   logs,
			devMod: m.Settings.Get().DeveloperMode,
		}
	}).WithEqual(func(a, b viewKey) bool {
		return a.found == b.found &&
			a.rev == b.rev &&
			a.logs == b.logs &&
			a.devMod == b.devMod &&
			extensionInfoEqual(a.info, b.info)
	})

	body := ggui.View(combined, func(vk viewKey) ggui.Widget {
		info, found := vk.info, vk.found
		if !found {
			return widget.Center(widget.Column(
				extIcon("package").Size(28).Color(colorGray),
				ui.Heading(tr("Extension not found")).Size(16),
				ui.Caption(tr("Extension %s is not installed.", extID)),
				ui.Button(tr("Open Extensions"), func() { m.Go(PageExtensions) }).Outline(),
			).Gap(10).Align(widget.AlignCenter))
		}

		var viewInfo ext.ViewInfo
		hasView := false
		for _, v := range info.Views {
			if v.ID == viewID {
				viewInfo = v
				hasView = true
				break
			}
		}

		sess.mu.Lock()
		tree := sess.tree
		streamTitle := sess.title
		streamState := sess.state
		streamErr := sess.err
		if streamErr == "" {
			streamErr = sess.streamErr
		}
		streamChrome := sess.chrome
		stale := sess.stale
		gone := sess.gone
		sess.mu.Unlock()

		effectiveChrome := viewInfo.Chrome
		if streamChrome != "" {
			effectiveChrome = streamChrome
		}

		var liftedTB *uiblock.Node
		bodyTree := tree
		if tree != nil && effectiveChrome == ext.ViewChromeToolbar && canStreamExtension(info) && !gone {
			if tb, rest := liftToolbar(tree); tb != nil {
				liftedTB = tb
				bodyTree = rest
			}
		}

		dispatchEvent := func(ev uiblock.Event) {
			if ev.Block == "" {
				ev.Block = viewID
			}
			m.SendExtensionViewEvent(extID, viewID, ev)
		}

		if tree != nil && canStreamExtension(info) && !gone {
			sess.form.redraw()
		} else if tree == nil || gone {
			sess.form.redraw()
			sess.form.pruneDialogs()
		}

		// Header chrome
		lang := ggui.Peek(m.Lang)
		iconName := viewInfo.Icon
		if iconName == "" {
			iconName = info.Icon
		}
		titleText := info.LocalizedName(lang)
		subTitle := info.LocalizedViewTitle(viewID, lang)
		if streamTitle != "" {
			subTitle = streamTitle
		}

		var titleParts []ggui.Widget
		titleParts = append(titleParts,
			extIcon(iconName).Size(18).Color(colorGray),
			ui.Heading(titleText).Size(15).NoWrap(),
		)
		if subTitle != "" && subTitle != titleText {
			titleParts = append(titleParts, ui.Caption("·").Size(14), widget.Text(subTitle).Size(13.5).Color(colorGray).NoWrap())
		}
		titleParts = append(titleParts, extStatusChip(info.State))
		if info.Unsandboxed {
			titleParts = append(titleParts, ui.Badge(tr("Dev · Unsandboxed")).Outline())
		}

		var hostActions []ggui.Widget
		if info.WebUI != nil && slices.Contains(info.Granted, ext.PermLocalProxy) && (info.State == ext.StateRunning || info.State == ext.StateDegraded) {
			infoCopy := info
			hostActions = append(hostActions,
				ui.Button(tr("Open in Browser"), func() { m.OpenExtensionWebUI(infoCopy) }).
					Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)),
			)
		}
		hostActions = append(hostActions,
			tip(iconButton(lucide.RefreshCw, tr("Reload"), func() { m.ReloadExtensionView(extID, viewID) }), tr("Reload")),
			tip(iconButton(lucide.ScrollText, tr("Logs"), func() { logsOpen.Set(true) }), tr("Logs")),
			tip(iconButton(lucide.Settings, tr("Settings"), func() { m.Go(PageExtensions) }), tr("Settings")),
		)

		headerParts := []ggui.Widget{
			widget.Row(titleParts...).Gap(8).Align(widget.AlignCenter),
		}
		if liftedTB != nil {
			tbWidget := renderUIBlockTree(extID, viewID, tree, liftedTB, sess.form, dispatchEvent)
			tbScroll := looseHeight{
				widget.Scroll(tbWidget).Horizontal().Shrink().Bar(color.Transparent),
			}
			headerParts = append(headerParts,
				widget.Expanded(looseHeight{widget.Align(tbScroll).At(0, 0.5)}),
				widget.Box().Width(1).Height(18).Fill(colorBorder),
			)
		} else {
			headerParts = append(headerParts, widget.Spacer())
		}
		headerParts = append(headerParts, widget.Row(hostActions...).Gap(4).Align(widget.AlignCenter))

		header := widget.Box(widget.Row(headerParts...).Gap(12).Align(widget.AlignCenter)).Pad(10, 16)

		var content ggui.Widget
		switch {
		case !hasView:
			content = widget.Center(ui.Alert(tr("View not found"), tr("Extension %s does not declare view %s.", extID, viewID)).Icon(icons.Alert))
		case info.State == ext.StateWaiting:
			content = extWaitingCard(m, info)
		case info.State == ext.StateBuilding:
			content = extBuildingCard(m, info)
		case info.State == ext.StateDisabled:
			content = extDisabledCard(m, info)
		case info.State == ext.StateInvalid || info.State == ext.StateFailed || info.State == ext.StateCrashed:
			content = extFailedCard(m, info, viewID, logsOpen)
		case gone:
			content = widget.Center(widget.Column(
				extIcon("circle-slash").Size(24).Color(colorGray),
				ui.Caption(tr("This view was closed.")),
				ui.Button(tr("Reload"), func() { m.ReloadExtensionView(extID, viewID) }).Outline(),
			).Gap(10).Align(widget.AlignCenter))
		case tree == nil && streamErr != "":
			content = extStreamErrorCard(m, info, viewID, streamErr, logsOpen)
		case tree == nil:
			content = widget.Center(widget.Column(
				ui.Spinner().Size(18),
				ui.Caption(tr("Starting…")),
			).Gap(10).Align(widget.AlignCenter))
		default:
			var sections []ggui.Widget
			if streamErr != "" || streamState == string(ext.StateFailed) {
				errMsg := streamErr
				if errMsg == "" {
					errMsg = info.Error
				}
				label := tr("Extension view error")
				if stale {
					label = tr("Showing previous render; update failed")
				}
				sections = append(sections, widget.Padding(
					ui.Alert(label, errMsg).Destructive().Icon(icons.Alert),
					10, 12, 0, 12,
				))
			}
			rendered := renderUIBlockTree(extID, viewID, tree, bodyTree, sess.form, dispatchEvent)
			sess.form.pruneDialogs()
			if tree.Fill {
				rendered = widget.Padding(rendered, 12, 16)
				sections = append(sections, widget.Expanded(rendered))
			} else {
				sections = append(sections, widget.Expanded(widget.Scroll(widget.Padding(rendered, 12, 16))))
			}
			content = widget.Column(sections...).Stretch()
		}

		return widget.Column(
			header,
			ui.Divider(),
			widget.Expanded(content),
		).Stretch()
	})

	return widget.Stack(
		body,
		extLogsDialog(m, extID, logsOpen),
	).Expand()
}

func canStreamExtension(info ext.ExtensionInfo) bool {
	switch info.State {
	case ext.StateWaiting, ext.StateBuilding, ext.StateDisabled, ext.StateFailed, ext.StateInvalid:
		return false
	}
	return info.State.CanRun()
}

func findExtensionInfo(list []ext.ExtensionInfo, id string) (ext.ExtensionInfo, bool) {
	for _, info := range list {
		if info.ID == id {
			return info, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func extStatusColor(s ext.ExtensionState) ggui.Widget {
	c := colorGray
	switch s {
	case ext.StateRunning:
		c = colorGreen
	case ext.StateStarting, ext.StateBuilding, ext.StateWaiting, ext.StateDegraded:
		c = colorAmber
	case ext.StateFailed, ext.StateInvalid, ext.StateCrashed:
		c = colorRed
	}
	return dot(c, 8)
}

func extStatusLabel(s ext.ExtensionState) string {
	switch s {
	case ext.StateRunning:
		return tr("Running")
	case ext.StateStarting:
		return tr("Starting")
	case ext.StateBuilding:
		return tr("Building")
	case ext.StateWaiting:
		return tr("Waiting")
	case ext.StateDisabled:
		return tr("Disabled")
	case ext.StateDegraded:
		return tr("Degraded")
	case ext.StateCrashed:
		return tr("Crashed")
	case ext.StateFailed:
		return tr("Failed")
	case ext.StateInvalid:
		return tr("Invalid")
	case ext.StateDiscovered:
		return tr("Idle")
	default:
		return string(s)
	}
}

func extStatusChip(s ext.ExtensionState) ggui.Widget {
	return widget.Row(
		extStatusColor(s),
		ui.Caption(extStatusLabel(s)).Size(12).NoWrap(),
	).Gap(6).Align(widget.AlignCenter)
}

func extPermissionRisk(perm string) string {
	switch ext.PermissionRisk(perm) {
	case "high":
		return tr("High")
	case "medium":
		return tr("Medium")
	case "low":
		return tr("Low")
	default:
		return tr("Unknown")
	}
}

func extPermissionDescription(perm string) string {
	switch perm {
	case ext.PermStorage:
		return tr("Read and write per-extension key-value storage")
	case ext.PermSecrets:
		return tr("Store and read encrypted secrets in the OS keychain vault")
	case ext.PermDocker:
		return tr("Access the scoped Docker Engine API for extension containers")
	case ext.PermSandboxes:
		return tr("Create, control, and execute commands in extension-owned sandboxes")
	case ext.PermSnapshots:
		return tr("Create and restore checkpoint snapshots of extension-owned sandboxes")
	case ext.PermUIPickFolder:
		return tr("Prompt you to choose a host folder on user interaction")
	case ext.PermUITerminal:
		return tr("Open an interactive terminal to an extension container or sandbox")
	case ext.PermUIOpenURL:
		return tr("Open external http(s) URLs in your default browser")
	case ext.PermLocalProxy:
		return tr("Expose a web UI at https://<ext-id>.localhost via the local proxy")
	case ext.PermNetwork:
		return tr("Access the external network from the extension runtime")
	default:
		return ext.PermissionDescription(perm)
	}
}

func extWaitingCard(m *Model, info ext.ExtensionInfo) ggui.Widget {
	perms := info.Asked
	if len(perms) == 0 {
		perms = info.Permissions
	}
	rows := []ggui.Widget{
		widget.Row(
			extIcon("shield").Size(18).Color(colorAmber),
			ui.Heading(tr("Approval required")).Size(16),
		).Gap(8).Align(widget.AlignCenter),
		ui.Caption(tr("Review the permissions requested by %s before building or running it.", info.Name)),
	}
	if info.RuntimeType == ext.RuntimeBinary && !m.developerMode.Load() {
		rows = append(rows, ui.Alert(tr("Developer Mode required"),
			tr("This extension uses an unsandboxed binary runtime. Turn on Developer Mode in Extensions to allow it.")).
			Icon(icons.Alert))
	}
	for _, perm := range perms {
		rows = append(rows, widget.Row(
			extIcon("check").Size(13).Color(colorGray),
			widget.Expanded(widget.Text(extPermissionDescription(perm)).Size(13)),
			ui.Badge(extPermissionRisk(perm)).Outline(),
			extChip(perm),
		).Gap(8).Align(widget.AlignCenter))
	}
	if info.Error != "" {
		rows = append(rows, ui.Alert(tr("Last error"), info.Error).Destructive().Icon(icons.Alert))
	}
	id := info.ID
	rows = append(rows, widget.Row(
		widget.Spacer(),
		ui.Button(tr("Deny"), func() { m.DenyExtension(id) }).Outline(),
		ui.Button(tr("Allow & Build"), func() { m.AllowExtension(id) }),
	).Gap(8))
	return widget.Center(widget.Box(ui.Card(widget.Column(rows...).Gap(12).Stretch())).MaxWidth(580).Pad(24))
}

func extBuildingCard(m *Model, info ext.ExtensionInfo) ggui.Widget {
	var lines []string
	if mgr := m.extMgrPtr.Load(); mgr != nil {
		for _, entry := range mgr.BuildLogs(info.ID) {
			lines = append(lines, entry.Line)
		}
	}
	logText := strings.Join(lines, "\n")
	if logText == "" {
		logText = tr("Preparing build…")
	}
	return widget.Center(widget.Box(ui.Card(widget.Column(
		widget.Row(ui.Spinner().Size(16), ui.Heading(tr("Building %s…", info.Name)).Size(16)).Gap(10).Align(widget.AlignCenter),
		ui.Caption(tr("State: building")),
		ui.CodeBlock(logText).Small().Height(240),
	).Gap(12).Stretch())).MaxWidth(680).Pad(24))
}

func extDisabledCard(m *Model, info ext.ExtensionInfo) ggui.Widget {
	id := info.ID
	return widget.Center(widget.Box(ui.Card(widget.Column(
		widget.Row(extIcon("package").Size(20).Color(colorGray), ui.Heading(tr("Extension disabled")).Size(16)).Gap(8).Align(widget.AlignCenter),
		ui.Caption(tr("%s is currently disabled.", info.Name)),
		widget.Row(
			widget.Spacer(),
			ui.Button(tr("Enable"), func() { m.EnableExtension(id) }),
		),
	).Gap(12).Stretch())).MaxWidth(480).Pad(24))
}

func extFailedCard(m *Model, info ext.ExtensionInfo, viewID string, logsOpen *ggui.StateValue[bool]) ggui.Widget {
	id := info.ID
	errMsg := info.Error
	if errMsg == "" {
		errMsg = tr("The extension failed to run.")
	}
	return widget.Center(widget.Box(ui.Card(widget.Column(
		widget.Row(extIcon("triangle-alert").Size(20).Color(colorRed), ui.Heading(tr("Extension failed")).Size(16)).Gap(8).Align(widget.AlignCenter),
		widget.Text(errMsg).Size(12.5).Font(ggui.DefaultMonoFont()).Color(uitheme.MutedFg),
		widget.Row(
			widget.Spacer(),
			ui.Button(tr("Logs"), func() { logsOpen.Set(true) }).Outline(),
			ui.Button(tr("Reload"), func() { m.ReloadExtensionView(id, viewID) }).Outline(),
			ui.Button(tr("Rebuild"), func() { m.RebuildExtension(id) }),
		).Gap(8),
	).Gap(12).Stretch())).MaxWidth(580).Pad(24))
}

func extStreamErrorCard(m *Model, info ext.ExtensionInfo, viewID, errMsg string, logsOpen *ggui.StateValue[bool]) ggui.Widget {
	id := info.ID
	return widget.Center(widget.Box(ui.Card(widget.Column(
		widget.Row(extIcon("triangle-alert").Size(20).Color(colorRed), ui.Heading(tr("View failed to load")).Size(16)).Gap(8).Align(widget.AlignCenter),
		widget.Text(errMsg).Size(12.5).Font(ggui.DefaultMonoFont()).Color(uitheme.MutedFg),
		widget.Row(
			widget.Spacer(),
			ui.Button(tr("Logs"), func() { logsOpen.Set(true) }).Outline(),
			ui.Button(tr("Reload"), func() { m.ReloadExtensionView(id, viewID) }),
		).Gap(8),
	).Gap(12).Stretch())).MaxWidth(580).Pad(24))
}

func extLogsDialog(m *Model, extID string, open *ggui.StateValue[bool]) ggui.Widget {
	refreshTick := ggui.State(0)
	combined := ggui.Combine(open, refreshTick, func(isOpen bool, _ int) bool { return isOpen })
	body := ggui.View(combined, func(isOpen bool) ggui.Widget {
		if !isOpen {
			return widget.Box()
		}
		var buildLines []string
		var runtimeLines []string
		if mgr := m.extMgrPtr.Load(); mgr != nil {
			for _, e := range mgr.BuildLogs(extID) {
				buildLines = append(buildLines, e.Line)
			}
			runtimeLines = mgr.Logs(extID)
		}
		buildStr := strings.Join(buildLines, "\n")
		if buildStr == "" {
			buildStr = tr("No build logs.")
		}
		rtStr := strings.Join(runtimeLines, "\n")
		if rtStr == "" {
			rtStr = tr("No runtime logs.")
		}
		return widget.Column(
			ui.Heading(tr("Build logs")).Size(13),
			ui.CodeBlock(buildStr).Small().Height(140),
			ui.Heading(tr("Runtime logs")).Size(13),
			ui.CodeBlock(rtStr).Small().Height(200),
		).Gap(8).Stretch()
	})
	return ui.Dialog(open, body).Title(tr("Extension logs: %s", extID)).Width(680).Footer(
		ui.Button(tr("Refresh"), func() { refreshTick.Update(func(v int) int { return v + 1 }) }).Outline(),
		ui.Button(tr("Close"), func() { open.Set(false) }),
	)
}
