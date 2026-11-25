package ui

import (
	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
)

// appUpdateAlert says that a newer GVX Desktop is out, as engineAlert says
// it of rungv, with what updates the app as it was installed.
func appUpdateAlert(m *Model) ggui.Widget {
	type state struct {
		u     AppUpdate
		shown bool
	}
	s := ggui.Combine(m.AppUpdate, m.Settings, func(u AppUpdate, set storeSettings) state { return state{u, u.Offered(set)} })
	return ggui.View(s, func(s state) ggui.Widget {
		if !s.shown {
			return widget.Box()
		}
		u := s.u
		acts := []ggui.Widget{appUpdateButton(m, u, ui.ButtonDefault)}
		if u.Install.Method != appupdate.Manual {
			acts = append(acts, ui.Button(tr("Release notes"), func() { m.Open(u.page()) }).Outline().Icon(glyph(lucide.ExternalLink, 15)))
		}
		acts = append(acts, ui.Button(tr("Skip this version"), m.SkipAppVersion).Ghost().Disabled(u.Downloading))
		return widget.Padding(noticeBanner(lucide.CircleFadingArrowUp, 0x3b82f6, false,
			tr("GVX Desktop %s is available", u.Name()), appUpdateText(u), acts...), 16, 0, 0, 0)
	})
}

// appUpdateButton does what updates the app, and says how far the download
// is while it does.
func appUpdateButton(m *Model, u AppUpdate, size *ui.ButtonSizeVariant) *ui.ButtonWidget {
	label := appUpdateVerb(u)
	if u.Downloading {
		return ui.ButtonOf(widget.Row(ui.Spinner().Size(12), widget.Text(tr("Downloading… %d%%", u.Percent))).Gap(6).Align(widget.AlignCenter), func() {}).
			Name(label).Disabled(true).Size(size)
	}
	icon := lucide.Download
	switch {
	case u.Install.Method.SelfUpdates() && u.Ready:
		icon = lucide.RotateCw
	case u.Install.Method.SelfUpdates():
	case u.Command != "":
		icon = lucide.Copy
	default:
		icon = lucide.ExternalLink
	}
	return ui.Button(label, m.UpdateApp).Icon(glyph(icon, 14)).Size(size).Disabled(u.Checking)
}

// appUpdateSettings is the settings' group of the app's own updates: what
// the newest release is, the button that looks again and the one that
// updates, and whether the app looks by itself.
func appUpdateSettings(m *Model) ggui.Widget {
	auto := ggui.Bind(func() bool { return !m.Settings.Get().NoAppUpdateCheck }, func(v bool) {
		m.UpdateSettings(func(s *storeSettings) { s.NoAppUpdateCheck = !v })
	})
	return settingsGroup(lucide.CircleFadingArrowUp, tr("GVX Desktop updates"),
		appUpdateRow(m),
		settingRow(tr("Check for updates automatically"),
			tr("Asks GitHub for the newest release of GVX Desktop when the app starts and once a day. Nothing is installed until you say so."),
			ui.Switch(auto, "").Name(tr("Check for updates automatically"))),
	)
}

// appUpdateRow says what the app's newest release is, and updates to it.
func appUpdateRow(m *Model) ggui.Widget {
	return ggui.View(m.AppUpdate, func(u AppUpdate) ggui.Widget {
		var text string
		var row []ggui.Widget
		var more []ggui.Widget
		switch {
		case u.Downloading:
			text = tr("Downloading GVX Desktop %s. It goes in place when the app restarts.", u.Name())
			row = append(row, appUpdateButton(m, u, ui.ButtonSmall))
		case u.Checking:
			text = tr("Checking for the newest…")
		case u.Tag == "" && u.Err != "":
			text = tr("The newest could not be read: %s", u.Err)
		case u.Tag == "":
			text = tr("This is GVX Desktop %s. The newest release has not been looked up.", AppVersion)
		case !appupdate.Comparable(AppVersion):
			text = tr("This build (%s) is not a release, and does not update itself. The newest release is %s.", AppVersion, u.Name())
		case u.Available():
			text = tr("GVX Desktop %s is available.", u.Name()) + " " + appUpdateText(u)
			row = append(row, appUpdateButton(m, u, ui.ButtonSmall))
			if u.Command != "" {
				more = append(more, ui.CodeBlock(u.Command).Small().CopyButton(true))
			}
		default:
			text = tr("GVX Desktop is up to date: %s is the newest release.", AppVersion)
		}
		if u.Tag != "" && u.Err != "" && !u.Checking && !u.Downloading {
			more = append(more, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Last check failed: %s", u.Err))),
			).Gap(8).Align(widget.AlignStart))
		}
		row = append(row,
			ui.Button(tr("Check for updates"), func() { m.CheckAppUpdate(false) }).Outline().Size(ui.ButtonSmall).
				Disabled(u.Checking || u.Downloading),
			widget.Expanded(ui.Caption(text)),
		)
		return widget.Column(append([]ggui.Widget{widget.Row(row...).Gap(10).Align(widget.AlignCenter)}, more...)...).Gap(10).Stretch()
	})
}
