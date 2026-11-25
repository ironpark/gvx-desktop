package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/ext"
)

func extensionsPage(m *Model) ggui.Widget {
	disabledNotice := ggui.View(m.Settings, func(s storeSettings) ggui.Widget {
		if s.Extensions || m.extMgrPtr.Load() != nil {
			return widget.Box()
		}
		return ui.Alert(tr("Extensions are disabled"), tr("Turn on Extensions in Labs to install and run extensions.")).
			Icon(icons.Alert).
			Action(ui.Button(tr("Open Labs"), func() { m.Go(PageLabs) }).Outline().Size(ui.ButtonSmall))
	})

	return page(
		headed(pageHeader(tr("Extensions"), ui.Caption(tr("Install and manage sandboxed extensions, permissions, and settings."))), disabledNotice),
		extensionsSettings(m),
	)
}

func extensionsSettings(m *Model) ggui.Widget {
	devMode := ggui.Bind(
		func() bool { return m.Settings.Get().DeveloperMode },
		func(on bool) { m.UpdateSettings(func(s *storeSettings) { s.DeveloperMode = on }) },
	)

	devRow := widget.Column(
		settingRow(tr("Developer Mode"),
			tr("Allows running local binary extensions directly on the host without sandboxing (unsandboxed)."),
			ui.Switch(devMode, "").Name(tr("Developer Mode"))),
		ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.DeveloperMode }), func() ggui.Widget {
			return ui.Alert(tr("Developer Mode enabled"),
				tr("Binary extensions run unsandboxed with your user permissions. Only allow extensions you trust.")).
				Icon(icons.Alert)
		}),
	).Gap(8).Stretch()

	keychainNotice := ggui.View(m.ExtensionSecrets, func(sec ext.SecretStoreStatus) ggui.Widget {
		if sec.Store != "file" && sec.Detail == "" {
			return widget.Box()
		}
		detail := tr("Secrets are stored in a 0600 file fallback because the OS keychain is unavailable.")
		if sec.Detail != "" {
			detail = tr("File keychain fallback active: %s", sec.Detail)
		}
		return ui.Alert(tr("Keychain fallback"), detail).Icon(icons.Alert)
	})

	gitRepo := ggui.State("")
	gitRef := ggui.State("")
	localPath := ggui.State("")
	imageRef := ggui.State("")
	installing := ggui.State(false)

	installBox := widget.Column(
		settingField(tr("Install from Git repository"), tr("Clone a Git repository containing extension.toml and a Dockerfile, and review permissions before building."),
			widget.Row(
				widget.Expanded(ui.TextField(gitRepo).Placeholder("https://github.com/org/gvx-ext").Name(tr("Git repository URL")).BindDisabled(installing)),
				widget.Box(ui.TextField(gitRef).Placeholder(tr("Ref / branch")).Name(tr("Git ref")).BindDisabled(installing)).Width(130),
				ui.Button(tr("Install Git"), func() {
					repo := strings.TrimSpace(ggui.Peek(gitRepo))
					if repo == "" {
						return
					}
					installing.Set(true)
					m.InstallGitExtension(repo, ggui.Peek(gitRef), func(err error) {
						installing.Set(false)
						if err == nil {
							gitRepo.Set("")
							gitRef.Set("")
						}
					})
				}).Outline().BindDisabled(installing),
			).Gap(8)),
		settingField(tr("Install from local folder"), tr("Load an extension checkout directory on this computer."),
			widget.Row(
				widget.Expanded(ui.TextField(localPath).Placeholder("/path/to/extension").Name(tr("Local extension folder")).BindDisabled(installing)),
				ui.Button(tr("Browse…"), func() {
					browseFolder(m, localPath, nil)
				}).Outline().BindDisabled(installing),
				ui.Button(tr("Install local"), func() {
					dir := strings.TrimSpace(ggui.Peek(localPath))
					if dir == "" {
						return
					}
					installing.Set(true)
					m.InstallLocalExtension(dir, func(err error) {
						installing.Set(false)
						if err == nil {
							localPath.Set("")
						}
					})
				}).Outline().BindDisabled(installing),
			).Gap(8)),
		settingField(tr("Install prebuilt OCI image"), tr("Pull a prebuilt extension container image reference."),
			widget.Row(
				widget.Expanded(ui.TextField(imageRef).Placeholder("ghcr.io/org/ext:0.1.0").Name(tr("OCI image reference")).BindDisabled(installing)),
				ui.Button(tr("Install image"), func() {
					ref := strings.TrimSpace(ggui.Peek(imageRef))
					if ref == "" {
						return
					}
					installing.Set(true)
					m.InstallImageExtension(ref, func(err error) {
						installing.Set(false)
						if err == nil {
							imageRef.Set("")
						}
					})
				}).Outline().BindDisabled(installing),
			).Gap(8)),
	).Gap(12).Stretch()

	cardStates := map[string]*extSettingsCardState{}

	extList := ggui.View(m.Extensions, func(list []ext.ExtensionInfo) ggui.Widget {
		if len(list) == 0 {
			clear(cardStates)
			return ui.Caption(tr("No extensions installed."))
		}
		active := make(map[string]bool, len(list))
		cards := make([]ggui.Widget, 0, len(list))
		for _, info := range list {
			active[info.ID] = true
			st := cardStates[info.ID]
			if st == nil {
				st = &extSettingsCardState{
					showLogs:     ggui.State(false),
					purgeData:    ggui.State(false),
					secretDrafts: map[string]*ggui.StateValue[string]{},
					textDrafts:   map[string]*extTextDraft{},
				}
				cardStates[info.ID] = st
			}
			cards = append(cards, extensionSettingsCard(m, info, st))
		}
		for id := range cardStates {
			if !active[id] {
				delete(cardStates, id)
			}
		}
		return widget.Column(cards...).Gap(12).Stretch()
	})

	return settingsGroup(lucide.Puzzle, tr("Extensions"),
		devRow,
		keychainNotice,
		installBox,
		extList,
	)
}

type extTextDraft struct {
	val   *ggui.StateValue[string]
	given string
}

type extSettingsCardState struct {
	showLogs     *ggui.StateValue[bool]
	purgeData    *ggui.StateValue[bool]
	secretDrafts map[string]*ggui.StateValue[string]
	textDrafts   map[string]*extTextDraft
}

func extensionSettingsCard(m *Model, info ext.ExtensionInfo, st *extSettingsCardState) ggui.Widget {
	id := info.ID
	showLogs := st.showLogs
	purgeData := st.purgeData

	lang := ggui.Peek(m.Lang)
	title := info.LocalizedName(lang)
	iconName := info.Icon
	if iconName == "" {
		iconName = "package"
	}

	headLeft := []ggui.Widget{
		extIcon(iconName).Size(16).Color(colorGray),
		widget.Text(title).Size(14).Weight(ggui.WeightSemibold),
		ui.Badge(info.Version).Outline(),
		extStatusChip(info.State),
	}
	if info.Unsandboxed {
		headLeft = append(headLeft, ui.Badge(tr("Dev · Unsandboxed")).Outline())
	}

	var actions []ggui.Widget
	if len(info.Views) > 0 && canStreamExtension(info) {
		viewID := info.Views[0].ID
		actions = append(actions, ui.Button(tr("Open"), func() { m.Go(ExtensionPage(id, viewID)) }).Outline().Size(ui.ButtonSmall))
	}
	if info.Source.Kind == ext.SourceGit && info.State != ext.StateWaiting && info.State != ext.StateBuilding {
		actions = append(actions, ui.Button(tr("Update"), func() { m.UpdateExtension(id) }).Outline().Size(ui.ButtonSmall))
	}
	switch info.State {
	case ext.StateWaiting:
		actions = append(actions,
			ui.Button(tr("Deny"), func() { m.DenyExtension(id) }).Outline().Size(ui.ButtonSmall),
			ui.Button(tr("Allow & Build"), func() { m.AllowExtension(id) }).Size(ui.ButtonSmall),
		)
	case ext.StateDisabled:
		actions = append(actions, ui.Button(tr("Enable"), func() { m.EnableExtension(id) }).Size(ui.ButtonSmall))
	case ext.StateRunning, ext.StateStarting, ext.StateDegraded:
		actions = append(actions,
			ui.Button(tr("Rebuild"), func() { m.RebuildExtension(id) }).Outline().Size(ui.ButtonSmall),
			ui.Button(tr("Disable"), func() { m.DisableExtension(id) }).Outline().Size(ui.ButtonSmall),
		)
	case ext.StateFailed, ext.StateCrashed, ext.StateDiscovered:
		actions = append(actions,
			ui.Button(tr("Enable"), func() { m.EnableExtension(id) }).Outline().Size(ui.ButtonSmall),
			ui.Button(tr("Rebuild"), func() { m.RebuildExtension(id) }).Outline().Size(ui.ButtonSmall),
		)
	case ext.StateInvalid:
		actions = append(actions, ui.Button(tr("Rebuild"), func() { m.RebuildExtension(id) }).Outline().Size(ui.ButtonSmall))
	}
	actions = append(actions, ui.Button(tr("Logs"), func() {
		showLogs.Set(!ggui.Peek(showLogs))
	}).Outline().Size(ui.ButtonSmall))

	headerRow := widget.Row(
		widget.Expanded(widget.Column(
			widget.Row(headLeft...).Gap(8).Align(widget.AlignCenter),
			ui.Caption(fmt.Sprintf("%s · %s · %s", info.ID, info.Publisher, info.Source.Kind)),
		).Gap(2)),
		widget.Row(actions...).Gap(6).Align(widget.AlignCenter),
	).Gap(12).Align(widget.AlignCenter)

	rows := []ggui.Widget{headerRow}
	if desc := info.LocalizedDescription(lang); desc != "" {
		rows = append(rows, ui.Caption(desc))
	}
	if info.Error != "" {
		rows = append(rows, ui.Alert(tr("Error"), info.Error).Destructive().Icon(icons.Alert))
	}

	// Permissions
	if len(info.Permissions) > 0 || len(info.Granted) > 0 {
		permRows := []ggui.Widget{
			widget.Row(
				ui.Heading(tr("Permissions")).Size(13),
				widget.Spacer(),
				func() ggui.Widget {
					if len(info.Granted) == 0 {
						return widget.Box()
					}
					return ui.Button(tr("Revoke permissions"), func() { m.RevokeExtensionPermissions(id) }).Outline().Size(ui.ButtonSmall)
				}(),
			).Align(widget.AlignCenter),
		}
		for _, p := range info.Permissions {
			status := tr("Requested")
			if slices.Contains(info.Granted, p) {
				status = tr("Granted")
			} else if slices.Contains(info.Denied, p) {
				status = tr("Denied")
			}
			permRows = append(permRows, widget.Row(
				widget.Expanded(widget.Text(extPermissionDescription(p)).Size(12.5)),
				ui.Badge(extPermissionRisk(p)).Outline(),
				ui.Badge(status).Outline(),
				extChip(p),
			).Gap(6).Align(widget.AlignCenter))
		}
		rows = append(rows, ui.Divider(), widget.Column(permRows...).Gap(6).Stretch())
	}

	// Allowed folders
	if len(info.AllowedFolders) > 0 {
		folderRows := []ggui.Widget{ui.Heading(tr("Allowed folders")).Size(13)}
		for _, dir := range info.AllowedFolders {
			d := dir
			folderRows = append(folderRows, widget.Row(
				extIcon("folder").Size(14).Color(colorGray),
				widget.Expanded(widget.Text(d).Size(12.5).NoWrap().Ellipsis()),
				ui.Button(tr("Remove"), func() { m.RemoveExtensionFolder(id, d) }).Outline().Size(ui.ButtonSmall),
			).Gap(8).Align(widget.AlignCenter))
		}
		rows = append(rows, ui.Divider(), widget.Column(folderRows...).Gap(6).Stretch())
	}

	// Auto-generated settings form
	if len(info.Settings) > 0 {
		setRows := []ggui.Widget{ui.Heading(tr("Settings")).Size(13)}
		for _, s := range info.Settings {
			s.Label = info.LocalizedSettingLabel(s.Key, lang)
			s.Description = info.LocalizedSettingDescription(s.Key, lang)
			setRows = append(setRows, extSettingRow(m, id, s, st))
		}
		rows = append(rows, ui.Divider(), widget.Column(setRows...).Gap(10).Stretch())
	}

	// Expandable logs
	rows = append(rows, ggui.If(showLogs, func() ggui.Widget {
		if info.State == ext.StateBuilding {
			return ggui.View(m.ExtBuildLogsTick, func(int) ggui.Widget {
				return extensionCardLogsView(m, id)
			})
		}
		return extensionCardLogsView(m, id)
	}))

	// Footer: storage & uninstall
	storageText := ""
	if info.StorageQuota > 0 {
		storageText = tr("Storage: %s of %s", formatBytes(int64(info.StorageUsed)), formatBytes(int64(info.StorageQuota)))
	}
	footerRow := widget.Row(
		ui.Caption(storageText).Size(12),
		widget.Spacer(),
		ui.Checkbox(purgeData, tr("Purge data")).Name(tr("Purge data")),
		ui.Button(tr("Uninstall"), func() {
			m.UninstallExtension(id, ggui.Peek(purgeData))
		}).Destructive().Size(ui.ButtonSmall),
	).Gap(10).Align(widget.AlignCenter)
	rows = append(rows, ui.Divider(), footerRow)

	return ui.Card(widget.Column(rows...).Gap(10).Stretch())
}

func extensionCardLogsView(m *Model, id string) ggui.Widget {
	var buildLines []string
	var rtLines []string
	if mgr := m.extMgrPtr.Load(); mgr != nil {
		for _, e := range mgr.BuildLogs(id) {
			buildLines = append(buildLines, e.Line)
		}
		rtLines = mgr.Logs(id)
	}
	parts := []ggui.Widget{ui.Divider()}
	if len(buildLines) > 0 {
		parts = append(parts, ui.Caption(tr("Build logs")), ui.CodeBlock(strings.Join(buildLines, "\n")).Small().Height(120))
	}
	if len(rtLines) > 0 {
		parts = append(parts, ui.Caption(tr("Runtime logs")), ui.CodeBlock(strings.Join(rtLines, "\n")).Small().Height(160))
	}
	if len(buildLines) == 0 && len(rtLines) == 0 {
		parts = append(parts, ui.Caption(tr("No logs recorded yet.")))
	}
	return widget.Column(parts...).Gap(6).Stretch()
}

func extSettingRow(m *Model, extID string, s ext.SettingSummary, st *extSettingsCardState) ggui.Widget {
	label := s.Label
	if label == "" {
		label = s.Key
	}
	effectiveVal := s.Value
	if !s.Set && effectiveVal == "" && s.Default != nil {
		effectiveVal = fmt.Sprint(s.Default)
	}

	var control ggui.Widget
	switch s.Type {
	case ext.SettingBoolean:
		on := effectiveVal == "true"
		control = ui.Switch(ggui.Controlled(on, func(v bool) {
			m.SetExtensionSetting(extID, s.Key, v)
		}), "").Name(label)
	case ext.SettingNumber:
		n, _ := strconv.ParseFloat(effectiveVal, 64)
		control = widget.Box(ui.NumberInput(ggui.Controlled(n, func(v float64) {
			m.SetExtensionSetting(extID, s.Key, v)
		})).Name(label)).Width(140)
	case ext.SettingSelect:
		cur := effectiveVal
		control = widget.Box(ui.Select(ggui.Controlled(cur, func(x string) {
			m.SetExtensionSetting(extID, s.Key, x)
		})).Options(s.Options).Name(label)).Width(200)
	case ext.SettingSecret:
		// Secret inputs are NEVER pre-filled with plaintext values.
		draft := st.secretDrafts[s.Key]
		if draft == nil {
			draft = ggui.State("")
			st.secretDrafts[s.Key] = draft
		}
		placeholder := tr("Not set")
		if s.Set {
			placeholder = tr("Set; type to replace it")
		}
		justSubmitted := false
		save := func(v string) {
			if v != "" {
				m.SetExtensionSetting(extID, s.Key, v)
				draft.Set("")
			}
		}
		field := ui.TextField(draft).Key("ext-secret:" + extID + ":" + s.Key).Password().Placeholder(placeholder).Name(label).
			OnSubmit(func(v string) {
				justSubmitted = true
				save(v)
			}).
			OnCommit(func(v string) {
				if justSubmitted {
					justSubmitted = false
					return
				}
				save(v)
			})
		parts := []ggui.Widget{widget.Box(field).Width(220)}
		if s.Set {
			parts = append(parts, ui.Button(tr("Clear"), func() {
				draft.Set("")
				m.SetExtensionSetting(extID, s.Key, "")
			}).Outline().Size(ui.ButtonSmall))
		}
		control = widget.Row(parts...).Gap(8).Align(widget.AlignCenter)
	default:
		d := st.textDrafts[s.Key]
		if d == nil {
			d = &extTextDraft{
				val:   ggui.State(effectiveVal),
				given: effectiveVal,
			}
			st.textDrafts[s.Key] = d
		} else if d.given != effectiveVal {
			d.given = effectiveVal
			d.val.Set(effectiveVal)
		}
		draft := d.val
		justSubmitted := false
		save := func(v string) {
			if v != effectiveVal {
				m.SetExtensionSetting(extID, s.Key, v)
			}
		}
		field := ui.TextField(draft).Key("ext-setting:" + extID + ":" + s.Key).Name(label).
			OnSubmit(func(v string) {
				justSubmitted = true
				save(v)
			}).
			OnCommit(func(v string) {
				if justSubmitted {
					justSubmitted = false
					return
				}
				save(v)
			})
		control = widget.Box(field).Width(220)
	}

	text := []ggui.Widget{widget.Text(label).Size(13).Weight(ggui.WeightMedium)}
	if s.Description != "" {
		text = append(text, ui.Caption(s.Description).Size(12))
	}
	return widget.Row(
		widget.Expanded(widget.Column(text...).Gap(2)),
		control,
	).Gap(16).Align(widget.AlignCenter)
}
