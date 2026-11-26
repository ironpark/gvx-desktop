package ui

import (
	"fmt"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/icons/lucide/all"
	gguiruntime "github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/chart"
	"github.com/ironpark/ggui/ui/icons"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

var extMarkdownOpenURL = gguiruntime.OpenURL

func handleExtMarkdownLink(dest string, change bool, onChange func(string)) {
	if (len(dest) >= 8 && strings.EqualFold(dest[:8], "https://")) || (len(dest) >= 7 && strings.EqualFold(dest[:7], "http://")) {
		if ext.ValidateOpenURL(dest) == nil {
			_ = extMarkdownOpenURL(dest)
		}
		return
	}
	if change && onChange != nil {
		onChange(dest)
	}
}

const (
	autoSplitBreakpoint = 640.0
	chartAnimation      = 250 * time.Millisecond
)

// RenderUIBlock renders a validated declarative uiblock tree into native ggui widgets.
func RenderUIBlock(extID, viewID string, root *uiblock.Node, onEvent func(uiblock.Event)) ggui.Widget {
	form := newBlockForm()
	form.redraw()
	w := renderUIBlockTree(extID, viewID, root, root, form, onEvent)
	form.pruneDialogs()
	return w
}

// renderUIBlockTree renders target using root for context menu indexing and form
// for persistent form/local state. Callers sharing one form across a lifted
// toolbar and body should call form.redraw() once before rendering both and
// form.pruneDialogs() once after rendering the body.
func renderUIBlockTree(extID, viewID string, root, target *uiblock.Node, form *blockForm, onEvent func(uiblock.Event)) (w ggui.Widget) {
	defer func() {
		if r := recover(); r != nil {
			w = ui.Alert(tr("UI render error"), fmt.Sprint(r)).Destructive().Icon(icons.Alert)
		}
	}()
	if target == nil {
		return widget.Box()
	}
	if root == nil {
		root = target
	}
	if form == nil {
		form = newBlockForm()
		form.redraw()
	}
	v := &extTreeView{
		extID:     extID,
		viewID:    viewID,
		form:      form,
		onEvent:   onEvent,
		inToolbar: target.Kind == uiblock.KindRow && target.Variant == "toolbar",
	}
	v.indexMenus(root)
	if v.inToolbar {
		return v.node(target, true)
	}
	return v.node(target, false)
}

// liftToolbar extracts a leading row(variant="toolbar") from a vertical container
// root, returning (toolbar, rest) with a shallow copy of root's remaining children.
func liftToolbar(t *uiblock.Node) (toolbar, rest *uiblock.Node) {
	if t == nil {
		return nil, nil
	}
	switch t.Kind {
	case uiblock.KindColumn, uiblock.KindList, uiblock.KindCard:
	case uiblock.KindScroll:
		if t.Direction == "horizontal" {
			return nil, t
		}
	default:
		return nil, t
	}
	if len(t.Children) == 0 {
		return nil, t
	}
	first := t.Children[0]
	if first == nil || first.Kind != uiblock.KindRow || first.Variant != "toolbar" {
		return nil, t
	}
	return first, shallowCopyUINode(t, t.Children[1:])
}

func shallowCopyUINode(n *uiblock.Node, children []*uiblock.Node) *uiblock.Node {
	if n == nil {
		return nil
	}
	cp := *n
	cp.Children = children
	return &cp
}

func toneColor(tone string) color.Color {
	switch tone {
	case "muted":
		return colorGray
	case "faint":
		return uitheme.MutedFg.Alpha(0.65)
	case "accent":
		return colorBlue
	case "success":
		return colorGreen
	case "warning":
		return colorAmber
	case "danger":
		return colorRed
	}
	return uitheme.Fg
}

func textSize(size string) float64 {
	switch size {
	case "small":
		return 11.5
	case "large":
		return 15
	}
	return 13
}

func textWeight(w string) ggui.FontWeight {
	switch w {
	case "medium":
		return ggui.WeightMedium
	case "semibold":
		return ggui.WeightSemibold
	}
	return ggui.WeightRegular
}

func extIcon(name string) *icons.Widget {
	if name != "" {
		if g := all.Lookup(name); g != nil {
			return g.Icon()
		}
	}
	if g := all.Lookup("square"); g != nil {
		return g.Icon()
	}
	return lucide.Square.Icon()
}

type extBlockDrag struct {
	ExtID  string
	ViewID string
	Node   string
	Value  string
	Group  string
}

type extTreeView struct {
	extID     string
	viewID    string
	form      *blockForm
	menus     map[string]*uiblock.Node
	onEvent   func(uiblock.Event)
	inToolbar bool
}

func (v *extTreeView) blockID() string {
	return v.extID + "/" + v.viewID
}

func (v *extTreeView) keyPrefix() string {
	return v.extID + "\x00" + v.viewID + "\x00"
}

func (v *extTreeView) indexMenus(root *uiblock.Node) {
	if v.menus == nil {
		v.menus = map[string]*uiblock.Node{}
	}
	var walk func(n *uiblock.Node)
	walk = func(n *uiblock.Node) {
		if n == nil {
			return
		}
		if n.Kind == uiblock.KindMenu && n.Variant == "context" {
			if id := n.ID; id != "" {
				v.menus[id] = n
				key := id
				if idx := strings.LastIndexByte(id, '/'); idx >= 0 {
					key = id[idx+1:]
				}
				if key != "" && !strings.HasPrefix(key, "#") {
					v.menus[key] = n
				}
			}
			if k := n.ContextMenu; k != "" {
				v.menus[k] = n
			}
			if k := n.Value; k != "" {
				v.menus[k] = n
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
}

func (v *extTreeView) send(nodeID, kind, value string) {
	if v.onEvent == nil {
		return
	}
	v.onEvent(uiblock.Event{
		Block:  v.viewID,
		Node:   nodeID,
		Kind:   kind,
		Value:  value,
		Values: v.form.values(),
	})
}

func (v *extTreeView) node(n *uiblock.Node, inLine bool) ggui.Widget {
	return v.nodeWith(n, inLine, false)
}

func (v *extTreeView) nodeWith(n *uiblock.Node, inLine, bare bool) (out ggui.Widget) {
	defer func() {
		if recover() != nil {
			out = widget.Box()
		}
	}()
	if n == nil {
		return widget.Box()
	}
	if v.menus == nil {
		v.indexMenus(n)
	}
	tap := func() {}
	isTap := !bare && n.Tap
	if isTap {
		id := n.ID
		tap = func() { v.send(id, uiblock.EventTap, "") }
	}
	switch n.Kind {
	case uiblock.KindColumn, uiblock.KindRow, uiblock.KindStack, uiblock.KindCard, uiblock.KindScroll,
		uiblock.KindList, uiblock.KindTable, uiblock.KindWrap, uiblock.KindGrid, uiblock.KindSplit:
		out = v.container(n, bare)
	case uiblock.KindCollapsible:
		out = v.collapsibleNode(n, bare)
	case uiblock.KindBreadcrumb:
		out = v.breadcrumbNode(n)
	case uiblock.KindTree:
		out = v.treeNode(n, bare)
	case uiblock.KindTreeItem:
		return v.treeItemNode(n, 0, bare)
	case uiblock.KindGraph:
		out = v.graphNode(n)
	case uiblock.KindDag:
		return v.dagNode(n, bare)
	case uiblock.KindMenu:
		if n.Variant == "context" {
			return widget.Box()
		}
		out = v.menuNode(n, bare)
	case uiblock.KindMenuItem:
		return v.menuItemWidget(n, "")
	case uiblock.KindCheckbox:
		out = v.checkboxNode(n, bare)
	case uiblock.KindDialog:
		return v.dialogNode(n)
	case uiblock.KindText:
		t := widget.Text(n.Text).Size(textSize(n.Size)).Weight(textWeight(n.Weight)).Color(toneColor(n.Tone))
		if inLine {
			t = t.NoWrap().Ellipsis()
		}
		out = v.maybeDragDrop(tappableWhen(appendBadges(t, n.Badges), n, isTap, tap), n, bare)
	case uiblock.KindIcon:
		w := tappableWhen(extIcon(n.Icon).Size(textSize(n.Size)+2).Color(toneColor(cmpTone(n.Tone, "muted"))), n, isTap, tap)
		if ph := n.Placeholder; ph != "" && !bare {
			w = ui.Tooltip(w, ph)
		}
		out = v.maybeDragDrop(w, n, bare)
	case uiblock.KindPill:
		w := tappableWhen(appendBadges(v.pillNode(n), n.Badges), n, isTap, tap)
		if ph := n.Placeholder; ph != "" && !bare {
			w = ui.Tooltip(w, ph)
		}
		out = v.maybeDragDrop(w, n, bare)
	case uiblock.KindChip:
		w := tappableWhen(appendBadges(v.chipNode(n), n.Badges), n, isTap, tap)
		if ph := n.Placeholder; ph != "" && !bare {
			w = ui.Tooltip(w, ph)
		}
		out = v.maybeDragDrop(w, n, bare)
	case uiblock.KindButton:
		out = v.buttonNode(n, tap, bare)
	case uiblock.KindInput:
		id := n.ID
		ph := cmpTone(n.Placeholder, n.Text)
		f := ui.TextField(v.form.input(id, n.Value)).Key(v.keyPrefix() + id).Placeholder(ph).Name(cmpTone(ph, tr("Input")))
		if n.Variant == "password" {
			f = f.Password()
		}
		if n.Change {
			justSubmitted := false
			f = f.OnSubmit(func(s string) {
				justSubmitted = true
				v.send(id, uiblock.EventSubmit, s)
			}).OnCommit(func(s string) {
				if justSubmitted {
					justSubmitted = false
					return
				}
				v.send(id, uiblock.EventChange, s)
			})
		}
		out = f.Disabled(n.Disabled)
	case uiblock.KindTextarea:
		id := n.ID
		lines := 4
		switch n.Size {
		case "large":
			lines = 14
		case "small":
			lines = 3
		}
		ph := cmpTone(n.Placeholder, n.Text)
		f := ui.TextField(v.form.input(id, n.Value)).Key(v.keyPrefix() + id).Lines(lines).Placeholder(ph).Name(cmpTone(ph, tr("Input")))
		if n.Change {
			justSubmitted := false
			f = f.OnSubmit(func(s string) {
				justSubmitted = true
				v.send(id, uiblock.EventSubmit, s)
			}).OnCommit(func(s string) {
				if justSubmitted {
					justSubmitted = false
					return
				}
				v.send(id, uiblock.EventChange, s)
			})
		}
		out = f.Disabled(n.Disabled)
	case uiblock.KindToggle:
		id := n.ID
		field := v.form.field(id, strconv.FormatBool(n.Checked))
		sw := ui.Switch(ggui.Bind(func() bool { return field.Get() == "true" }, func(on bool) {
			field.Set(strconv.FormatBool(on))
			if n.Change {
				v.send(id, uiblock.EventChange, strconv.FormatBool(on))
			}
		}), n.Text).Name(n.Text)
		out = sw.Disabled(n.Disabled)
	case uiblock.KindSelect:
		id := n.ID
		field := v.form.field(id, n.Value)
		sel := ui.Select(ggui.Bind(field.Get, func(x string) {
			field.Set(x)
			if n.Change {
				v.send(id, uiblock.EventChange, x)
			}
		})).Options(n.Options).Name(tr("Choose")).Disabled(n.Disabled)
		if v.inToolbar || inLine {
			out = widget.Box(sel).MaxWidth(260)
		} else {
			out = sel
		}
	case uiblock.KindTabs:
		id := n.ID
		field := v.form.field(id, n.Value)
		opts := slices.Clone(n.Options)
		small := n.Size == "small" || (v.inToolbar && n.Size != "large")
		pad := 3.0
		if small {
			pad = 2.0
		}
		out = ggui.View(field, func(active string) ggui.Widget {
			btns := make([]ggui.Widget, 0, len(opts))
			for _, opt := range opts {
				o := opt
				b := ui.Button(o, func() {
					if n.Disabled {
						return
					}
					field.Set(o)
					if n.Change {
						v.send(id, uiblock.EventChange, o)
					}
				}).Disabled(n.Disabled).Name(o)
				b.Role = ggui.RoleTab
				if small {
					b = b.Size(ui.ButtonSmall)
				}
				if o == active {
					b = b.Secondary()
				} else {
					b = b.Ghost()
				}
				btns = append(btns, b)
			}
			return widget.Box(looseHeight{widget.Scroll(widget.Row(btns...).Gap(4).Align(widget.AlignCenter)).Horizontal().Shrink().Bar(color.Transparent)}).
				Fill(uitheme.Card).Border(1, colorBorder).Radius(8).Pad(pad)
		})
	case uiblock.KindMarkdown:
		md := ui.Markdown(uiblock.Printable(n.Text)).Selectable(true)
		id := n.ID
		change := n.Change && !n.Disabled
		md = md.OnLink(func(dest string) {
			handleExtMarkdownLink(dest, change, func(val string) {
				v.send(id, uiblock.EventChange, val)
			})
		})
		out = md
	case uiblock.KindCode:
		cb := ui.CodeBlock(uiblock.Printable(n.Text)).Small()
		if n.Language != "" {
			cb = cb.Language(n.Language)
		}
		out = cb
	case uiblock.KindProgress:
		if n.Variant == "spinner" {
			if n.Text == "" {
				out = ui.Spinner().Size(13)
			} else {
				out = widget.Row(
					ui.Spinner().Size(13),
					widget.Text(n.Text).Size(12).Color(toneColor(cmpTone(n.Tone, "muted"))).NoWrap().Ellipsis(),
				).Gap(6).Align(widget.AlignCenter)
			}
		} else {
			out = ui.Progress(ggui.Const(clampFloat(n.Progress, 0, 1)))
		}
	case uiblock.KindDivider:
		return ui.Divider()
	case uiblock.KindChart:
		out = extChartNode(n)
	default:
		return widget.Box()
	}
	if bare {
		return out
	}
	return v.withContextMenu(out, n, "", "")
}

type looseHeight struct{ child ggui.Widget }

func (w looseHeight) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	inner := c
	inner.MinH = 0
	inner.MaxH = ggui.Unbounded
	return c.Constrain(w.child.Layout(inner, env))
}

func (w looseHeight) Paint(dst *ggui.Canvas, r ggui.Rect) {
	dst.Paint(w.child, r)
}

func tappableWhen(w ggui.Widget, n *uiblock.Node, active bool, tap func()) ggui.Widget {
	if !active {
		return w
	}
	return ui.ButtonOf(w, tap).Ghost().Pad(2, 4).Radius(6).Name(cmpTone(n.Text, n.Placeholder))
}

func cmpTone(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type extWashed struct {
	child  ggui.Widget
	color  color.Color
	radius float64
	tint   color.Color
}

func (w *extWashed) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	w.tint = color.Transparent
	if col := ggui.ResolveColor(w.color, env); col != nil {
		r, g, b, _ := col.RGBA()
		w.tint = color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0x2B}
	}
	return w.child.Layout(c, env)
}

func (w *extWashed) Paint(dst *ggui.Canvas, r ggui.Rect) {
	rad := w.radius
	if rad <= 0 {
		rad = r.Size.H / 2
	}
	dst.FillRoundRect(r, rad, w.tint)
	dst.Paint(w.child, r)
}

func extChip(text string) ggui.Widget {
	return widget.Box(widget.Text(text).Size(10.5).Font(ggui.DefaultMonoFont()).Color(colorGray).NoWrap().Ellipsis()).
		Pad(1, 5).Radius(4).Fill(uitheme.Muted).MaxWidth(140)
}

func extListRow(content ggui.Widget, name string, onTap func()) ggui.Widget {
	return widget.Box(ui.ButtonOf(content, onTap).Ghost().Pad(6, 8).Radius(8).Name(name)).Pad(1, 6)
}

func extBorderlessMenu(m *ui.MenuWidget) ggui.Widget {
	return uitheme.Override(func(t *uitheme.Theme) {
		t.Border = color.Transparent
		t.Bg = color.Transparent
	}, m)
}

func extIconMenu(m *ui.MenuWidget, pad float64) ggui.Widget {
	return uitheme.Override(func(t *uitheme.Theme) {
		t.Border = color.Transparent
		t.Bg = color.Transparent
		t.Space = pad
	}, m)
}

func solidToneColors(tone string) (fill, fg color.Color) {
	switch tone {
	case "accent":
		return colorBlue, extContrastInk{bg: colorBlue}
	case "success":
		return colorGreen, extContrastInk{bg: colorGreen}
	case "warning":
		return colorAmber, extContrastInk{bg: colorAmber}
	case "danger":
		return uitheme.Destructive, extContrastInk{bg: uitheme.Destructive}
	case "muted":
		return uitheme.Accent, uitheme.Fg
	case "faint":
		return uitheme.Muted, uitheme.Fg
	default:
		return uitheme.Primary, uitheme.PrimaryFg
	}
}

type extContrastInk struct{ bg color.Color }

func (c extContrastInk) RGBA() (uint32, uint32, uint32, uint32) {
	return pickContrastInk(c.bg).RGBA()
}

func (c extContrastInk) Resolve(env ggui.Env) color.Color {
	return pickContrastInk(ggui.ResolveColor(c.bg, env))
}

func pickContrastInk(bg color.Color) color.Color {
	light := color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	dark := color.NRGBA{R: 0x18, G: 0x1B, B: 0x1A, A: 0xFF}
	if bg == nil {
		return dark
	}
	if wcagContrast(bg, light) >= wcagContrast(bg, dark) {
		return light
	}
	return dark
}

func wcagContrast(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relativeLuminance(c color.Color) float64 {
	if c == nil {
		return 0
	}
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	lin := func(v uint8) float64 {
		s := float64(v) / 255.0
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(n.R) + 0.7152*lin(n.G) + 0.0722*lin(n.B)
}

func (v *extTreeView) pillNode(n *uiblock.Node) ggui.Widget {
	c := toneColor(cmpTone(n.Tone, "muted"))
	iconName := n.Icon
	content := func(fg color.Color) ggui.Widget {
		lbl := widget.Text(n.Text).Size(10.5).Weight(ggui.WeightMedium).Color(fg).NoWrap().Ellipsis()
		if iconName == "" {
			return lbl
		}
		return widget.Row(extIcon(iconName).Size(11).Color(fg), lbl).Gap(4).Align(widget.AlignCenter)
	}
	switch n.Variant {
	case "solid":
		fill, fg := solidToneColors(n.Tone)
		return widget.Box(content(fg)).Pad(2, 8).Radius(999).Fill(fill).Clip().MaxWidth(200)
	case "outline":
		return widget.Box(content(c)).Pad(2, 8).Radius(999).Border(1, c).Clip().MaxWidth(200)
	default:
		return widget.Box(&extWashed{child: widget.Box(content(c)).Pad(2, 8).Clip(), color: c}).MaxWidth(200)
	}
}

func (v *extTreeView) chipNode(n *uiblock.Node) ggui.Widget {
	iconName := n.Icon
	withIcon := func(fg color.Color, lbl ggui.Widget) ggui.Widget {
		if iconName == "" {
			return lbl
		}
		return widget.Row(extIcon(iconName).Size(11).Color(fg), lbl).Gap(4).Align(widget.AlignCenter)
	}
	switch n.Variant {
	case "soft":
		c := toneColor(cmpTone(n.Tone, "muted"))
		lbl := widget.Text(n.Text).Size(11.5).Color(c).NoWrap().Ellipsis()
		return widget.Box(&extWashed{child: widget.Box(withIcon(c, lbl)).Pad(2, 8).Clip(), color: c}).MaxWidth(200)
	case "solid":
		fill, fg := solidToneColors(n.Tone)
		lbl := widget.Text(n.Text).Size(11.5).Weight(ggui.WeightMedium).Color(fg).NoWrap().Ellipsis()
		return widget.Box(withIcon(fg, lbl)).Pad(2, 8).Radius(999).Fill(fill).Clip().MaxWidth(200)
	case "outline":
		border := colorBorder
		if n.Tone != "" {
			border = toneColor(n.Tone)
		}
		fg := toneColor(cmpTone(n.Tone, "muted"))
		lbl := widget.Text(n.Text).Size(11.5).Color(fg).NoWrap().Ellipsis()
		return widget.Box(withIcon(fg, lbl)).Pad(2, 8).Radius(999).Border(1, border).Clip().MaxWidth(200)
	default:
		lbl := widget.Text(n.Text).Size(10.5).Font(ggui.DefaultMonoFont()).Color(colorGray).NoWrap().Ellipsis()
		return widget.Box(withIcon(colorGray, lbl)).Pad(1, 5).Radius(4).Fill(uitheme.Muted).Clip().MaxWidth(140)
	}
}

func renderExtBadge(b *uiblock.Badge) ggui.Widget {
	if b == nil {
		return widget.Box()
	}
	iconName := b.Icon
	content := func(fg color.Color, w ggui.FontWeight) ggui.Widget {
		lbl := widget.Box(widget.Text(b.Text).Size(10.5).Weight(w).Color(fg).NoWrap().Ellipsis()).MaxWidth(90)
		if iconName == "" {
			return lbl
		}
		return widget.Row(extIcon(iconName).Size(10).Color(fg), lbl).Gap(3).Align(widget.AlignCenter)
	}
	switch b.Variant {
	case "mono":
		fg := toneColor(cmpTone(b.Tone, "muted"))
		lbl := widget.Text(b.Text).Size(11.5).Font(ggui.DefaultMonoFont()).Color(fg).NoWrap()
		if iconName == "" {
			return lbl
		}
		return widget.Row(extIcon(iconName).Size(10.5).Color(fg), lbl).Gap(3).Align(widget.AlignCenter)
	case "solid":
		fill, fg := solidToneColors(b.Tone)
		return widget.Box(content(fg, ggui.WeightMedium)).Pad(1, 5).Radius(4).Fill(fill).Clip()
	case "outline":
		c := toneColor(cmpTone(b.Tone, "muted"))
		border := colorBorder
		if b.Tone != "" {
			border = c
		}
		return widget.Box(content(c, ggui.WeightRegular)).Pad(1, 5).Radius(4).Border(1, border).Clip()
	default:
		fg := toneColor(cmpTone(b.Tone, "muted"))
		return widget.Box(content(fg, ggui.WeightRegular)).Pad(1, 5).Radius(4).Fill(uitheme.Muted).Clip()
	}
}

func renderExtBadges(badges []*uiblock.Badge) []ggui.Widget {
	out := make([]ggui.Widget, 0, len(badges))
	for _, b := range badges {
		if b != nil {
			out = append(out, renderExtBadge(b))
		}
	}
	return out
}

func appendBadges(w ggui.Widget, badges []*uiblock.Badge) ggui.Widget {
	rendered := renderExtBadges(badges)
	if len(rendered) == 0 {
		return w
	}
	parts := make([]ggui.Widget, 0, 1+len(rendered))
	parts = append(parts, w)
	parts = append(parts, rendered...)
	return widget.Row(parts...).Gap(5).Align(widget.AlignCenter)
}

var extDangerButton = ui.NewVariant("ext-danger", func(t *uitheme.Theme) ui.ButtonStyle {
	fill := t.Destructive
	if fill == nil {
		fill = colorRed
	}
	return ui.ButtonStyle{
		Fill:  fill,
		Hover: shadeColor(fill, 0.88),
		Label: pickContrastInk(fill),
		Ring:  fill,
	}
})

func shadeColor(c color.Color, factor float64) color.Color {
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	scale := func(v uint8) uint8 {
		f := math.Round(float64(v) * factor)
		if f < 0 {
			return 0
		}
		if f > 255 {
			return 255
		}
		return uint8(f)
	}
	return color.NRGBA{R: scale(n.R), G: scale(n.G), B: scale(n.B), A: n.A}
}

func (v *extTreeView) buttonNode(n *uiblock.Node, tap func(), bare bool) ggui.Widget {
	text := n.Text
	iconName := n.Icon
	badges := n.Badges
	var b *ui.ButtonWidget
	iconOnly := text == "" && iconName != "" && len(badges) == 0
	btnSize := n.Size
	if btnSize == "" && v.inToolbar {
		btnSize = "small"
	}
	switch {
	case len(badges) > 0:
		var parts []ggui.Widget
		if iconName != "" {
			parts = append(parts, extIcon(iconName).Size(14).Color(toneColor(n.Tone)))
		}
		if text != "" {
			parts = append(parts, widget.Text(text).Size(textSize(btnSize)).NoWrap().Ellipsis())
		}
		parts = append(parts, renderExtBadges(badges)...)
		b = ui.ButtonOf(widget.Row(parts...).Gap(4).Align(widget.AlignCenter), tap)
		switch btnSize {
		case "small":
			b = b.Size(ui.ButtonSmall)
		case "large":
			b = b.Size(ui.ButtonLarge)
		}
	case iconOnly:
		b = ui.ButtonOf(widget.Box(), tap).Icon(extIcon(iconName))
		switch btnSize {
		case "small":
			b = b.Size(ui.ButtonIconSmall)
		case "large":
			b = b.Size(ui.ButtonIconLarge)
		default:
			b = b.Size(ui.ButtonIcon)
		}
	default:
		b = ui.Button(text, tap)
		if iconName != "" {
			b = b.Icon(extIcon(iconName))
		}
		switch btnSize {
		case "small":
			b = b.Size(ui.ButtonSmall)
		case "large":
			b = b.Size(ui.ButtonLarge)
		}
	}

	switch n.Variant {
	case "primary":
	case "secondary":
		b = b.Secondary()
	case "outline":
		b = b.Outline()
	case "ghost":
		b = b.Ghost()
	case "danger":
		b = b.Variant(extDangerButton)
	default:
		switch n.Tone {
		case "accent":
		case "danger":
			b = b.Variant(extDangerButton)
		default:
			b = b.Outline()
		}
	}

	ph := n.Placeholder
	name := cmpTone(text, cmpTone(ph, iconName))
	var out ggui.Widget = b.Disabled(n.Disabled || bare || !n.Tap).Name(name)
	if ph != "" && !bare {
		out = ui.Tooltip(out, ph)
	}
	return v.maybeDragDrop(out, n, bare)
}

func (v *extTreeView) menuNode(n *uiblock.Node, bare bool) ggui.Widget {
	if n.Variant == "context" {
		return widget.Box()
	}
	items := v.buildMenuItems(n.Children, "", 0)
	text := n.Text
	iconName := n.Icon
	badges := n.Badges

	var face ggui.Widget
	iconOnly := text == "" && iconName != "" && len(badges) == 0
	if iconOnly {
		face = extIcon(iconName).Size(14).Color(toneColor(cmpTone(n.Tone, "muted")))
	} else {
		var parts []ggui.Widget
		if iconName != "" {
			parts = append(parts, extIcon(iconName).Size(14).Color(toneColor(cmpTone(n.Tone, "muted"))))
		}
		if text != "" {
			parts = append(parts, widget.Box(widget.Text(text).Size(12.5).Weight(ggui.WeightMedium).Color(toneColor(n.Tone)).NoWrap().Ellipsis()).MaxWidth(150))
		}
		if len(badges) > 0 {
			parts = append(parts, renderExtBadges(badges)...)
		}
		parts = append(parts, extIcon("chevron-down").Size(12).Color(uitheme.MutedFg.Alpha(0.75)))
		face = widget.Row(parts...).Gap(5).Align(widget.AlignCenter)
	}

	ph := n.Placeholder
	name := cmpTone(text, cmpTone(ph, cmpTone(iconName, tr("Menu"))))
	menu := ui.MenuOf(face, items...).Disabled(n.Disabled || bare).Name(name)
	var out ggui.Widget
	switch {
	case n.Variant == "outline":
		out = menu
	case iconOnly:
		out = extIconMenu(menu, 5)
	default:
		out = extBorderlessMenu(menu)
	}
	if ph != "" && !bare {
		out = ui.Tooltip(out, ph)
	}
	return v.maybeDragDrop(out, n, bare)
}

func (v *extTreeView) buildMenuItems(children []*uiblock.Node, contextArg string, depth int) []ggui.Widget {
	out := make([]ggui.Widget, 0, len(children))
	for _, c := range children {
		if c == nil {
			continue
		}
		switch c.Kind {
		case uiblock.KindDivider:
			out = append(out, ui.MenuDivider())
		case uiblock.KindMenu:
			if depth >= 2 {
				continue
			}
			sub := ui.MenuSub(c.Text, v.buildMenuItems(c.Children, contextArg, depth+1)...).Disabled(c.Disabled)
			if c.Icon != "" {
				sub = sub.Icon(extIcon(c.Icon).Size(13).Color(toneColor(cmpTone(c.Tone, "muted"))))
			}
			out = append(out, sub)
		case uiblock.KindMenuItem:
			out = append(out, v.menuItemWidget(c, contextArg))
		}
	}
	return out
}

func (v *extTreeView) menuItemWidget(c *uiblock.Node, contextArg string) ggui.Widget {
	id := c.ID
	onTap := func() {}
	if c.Tap && !c.Disabled {
		onTap = func() { v.send(id, uiblock.EventTap, contextArg) }
	}
	it := ui.MenuItem(c.Text, onTap).Checked(c.Checked).Disabled(c.Disabled)
	if c.Tone == "danger" {
		it = it.Destructive()
	}
	if c.Icon != "" {
		col := toneColor(cmpTone(c.Tone, "muted"))
		if c.Tone == "danger" {
			col = colorRed
		}
		it = it.Icon(extIcon(c.Icon).Size(13).Color(col))
	}
	return it
}

func (v *extTreeView) withContextMenu(w ggui.Widget, n *uiblock.Node, argOverride, labelOverride string) ggui.Widget {
	key := n.ContextMenu
	if key == "" || v.menus == nil {
		return w
	}
	menuNode := v.menus[key]
	if menuNode == nil {
		return w
	}
	arg := argOverride
	if arg == "" {
		arg = n.ContextArg
	}
	cm := ui.ContextMenuFunc(w, func() []ggui.Widget {
		return v.buildMenuItems(menuNode.Children, arg, 0)
	})
	label := labelOverride
	if label == "" {
		label = n.Text
	}
	if label != "" {
		cm = cm.Name(tr("%s actions", label))
	}
	return cm
}

func (v *extTreeView) checkboxNode(n *uiblock.Node, bare bool) ggui.Widget {
	id := n.ID
	initial := n.Check
	if initial == "" {
		if n.Checked {
			initial = "on"
		} else {
			initial = "off"
		}
	}
	field := v.form.field(id, initial)
	on := ggui.Bind(
		func() bool { return field.Get() == "on" },
		func(next bool) {
			if n.Disabled {
				return
			}
			val := "off"
			if next {
				val = "on"
			}
			field.Set(val)
			if n.Change {
				v.send(id, uiblock.EventChange, val)
			} else if n.Checks {
				v.send(id, uiblock.EventCheck, val)
			}
		},
	)
	ind := ggui.Map(field, func(s string) bool { return s == "mixed" })
	ph := n.Placeholder
	name := cmpTone(n.Text, cmpTone(ph, tr("Checkbox")))
	cb := ui.Checkbox(on, n.Text).BindIndeterminate(ind).Disabled(n.Disabled || bare).Name(name)
	out := appendBadges(cb, n.Badges)
	if ph != "" && !bare {
		out = ui.Tooltip(out, ph)
	}
	return v.maybeDragDrop(out, n, bare)
}

func extQuietBox(box ggui.Widget) ggui.Widget {
	return uitheme.Override(func(t *uitheme.Theme) {
		t.Primary, t.PrimaryFg = t.Accent, t.Fg
		t.ControlSize = min(t.ControlSize, 14)
	}, box)
}

func (v *extTreeView) dialogNode(n *uiblock.Node) ggui.Widget {
	id := n.ID
	gap := n.Gap
	if gap == 0 {
		gap = 10
	}
	var kids []ggui.Widget
	if n.Variant == "danger" && n.Text != "" {
		kids = append(kids, widget.Row(
			extIcon("shield-alert").Size(16).Color(colorRed),
			widget.Expanded(widget.Text(n.Text).Size(15).Weight(ggui.WeightSemibold).Color(colorRed)),
		).Gap(8).Align(widget.AlignCenter))
	}
	for _, c := range n.Children {
		if c == nil || (c.Kind == uiblock.KindMenu && c.Variant == "context") {
			continue
		}
		w := v.node(c, false)
		switch g := c.Grow; {
		case g == 1:
			w = widget.Expanded(w)
		case g > 0:
			w = widget.Flex(w, g)
		}
		kids = append(kids, w)
	}
	body := widget.Column(kids...).Gap(gap).Stretch()

	open := v.form.dialogOpen(id)
	dismissed := false
	sendDismiss := func() {
		if dismissed {
			return
		}
		dismissed = true
		if n.Change {
			v.send(id, uiblock.EventChange, "dismiss")
		}
	}
	dlg := ui.Dialog(open, body).Width(420).Name(cmpTone(n.Text, tr("Dialog"))).OnClose(sendDismiss)
	if n.Variant != "danger" && n.Text != "" {
		dlg = dlg.Title(n.Text)
	}
	if v.extID != "" {
		dlg = dlg.Footer(widget.Row(
			extIcon("package").Size(12).Color(colorGray),
			widget.Text(tr("%s asks", v.extID)).Size(11).Color(colorGray).NoWrap(),
		).Gap(5).Align(widget.AlignCenter))
	}
	return dlg
}

func (v *extTreeView) collapsibleNode(n *uiblock.Node, bare bool) ggui.Widget {
	id := n.ID
	st := v.form.local(id, strconv.FormatBool(n.Checked))
	open := ggui.Bind(
		func() bool { return st.Get() == "true" },
		func(next bool) {
			val := strconv.FormatBool(next)
			st.Set(val)
			if n.Change {
				v.send(id, uiblock.EventChange, val)
			}
		},
	)

	trigger := ggui.View(st, func(cur string) ggui.Widget {
		chev := "chevron-right"
		if cur == "true" {
			chev = "chevron-down"
		}
		head := []ggui.Widget{extIcon(chev).Size(13).Color(colorGray)}
		if n.Icon != "" {
			head = append(head, extIcon(n.Icon).Size(14).Color(toneColor(cmpTone(n.Tone, "muted"))))
		}
		head = append(head, widget.Expanded(widget.Text(n.Text).Size(12.5).Weight(ggui.WeightMedium).Color(toneColor(n.Tone)).NoWrap().Ellipsis()))
		if len(n.Badges) > 0 {
			head = append(head, renderExtBadges(n.Badges)...)
		}
		return widget.Box(widget.Row(head...).Gap(6).Align(widget.AlignCenter)).Pad(6, 8).Radius(6).Fill(uitheme.Card)
	})

	gap := n.Gap
	if gap == 0 {
		gap = 6
	}
	kids := make([]ggui.Widget, 0, len(n.Children))
	var dialogs []ggui.Widget
	for _, c := range n.Children {
		if c == nil || (c.Kind == uiblock.KindMenu && c.Variant == "context") {
			continue
		}
		if c.Kind == uiblock.KindDialog {
			dialogs = append(dialogs, v.dialogNode(c))
			continue
		}
		w := v.node(c, false)
		switch g := c.Grow; {
		case g == 1:
			w = widget.Expanded(w)
		case g > 0:
			w = widget.Flex(w, g)
		}
		kids = append(kids, w)
	}
	var body ggui.Widget = widget.Padding(widget.Column(kids...).Gap(gap).Stretch(), 6, 8, 4, 16)
	if len(dialogs) > 0 {
		body = widget.Stack(append([]ggui.Widget{body}, dialogs...)...)
	}
	cw := ui.CollapsibleOf(open, trigger, body).Disabled(n.Disabled)
	cw.SetName(n.Text)
	return v.maybeDragDrop(cw, n, bare)
}

func (v *extTreeView) breadcrumbNode(n *uiblock.Node) ggui.Widget {
	opts := n.Options
	id := n.ID
	crumbs := make([]ui.BreadcrumbEntry, len(opts))
	for i, seg := range opts {
		var onTap func()
		if n.Change && !n.Disabled && i < len(opts)-1 {
			idx := i
			onTap = func() { v.send(id, uiblock.EventChange, strconv.Itoa(idx)) }
		}
		crumbs[i] = ui.Crumb(seg, onTap)
	}
	bc := ui.Breadcrumb(crumbs...)
	if n.Text != "" {
		bc = bc.Name(n.Text)
	}
	return bc
}

func (v *extTreeView) treeNode(n *uiblock.Node, bare bool) ggui.Widget {
	gap := n.Gap
	kids := make([]ggui.Widget, 0, len(n.Children))
	for _, c := range n.Children {
		if c == nil {
			continue
		}
		kids = append(kids, v.treeItemNode(c, 0, false))
	}
	return v.maybeDragDrop(widget.Column(kids...).Gap(gap).Stretch(), n, bare)
}

func (v *extTreeView) treeItemNode(n *uiblock.Node, depth int, bare bool) ggui.Widget {
	if n == nil {
		return widget.Box()
	}
	id := n.ID
	hasKids := len(n.Children) > 0
	expandable := hasKids || n.Change
	st := v.form.local(id, strconv.FormatBool(n.Checked))
	toggle := func() {
		if n.Disabled || !expandable {
			return
		}
		next := ggui.Peek(st) != "true"
		val := strconv.FormatBool(next)
		st.Set(val)
		if n.Change {
			v.send(id, uiblock.EventChange, val)
		}
	}

	return ggui.View(st, func(cur string) ggui.Widget {
		expanded := cur == "true"
		active := n.Tone == "accent"

		var cells []ggui.Widget
		for range depth {
			guide := widget.Row(
				widget.Box().Width(7),
				widget.Box().Width(1).Height(22).Fill(colorBorder),
				widget.Box().Width(8),
			)
			cells = append(cells, guide)
		}

		if expandable {
			chev := "chevron-right"
			chevName := tr("Expand %s", n.Text)
			if expanded {
				chev = "chevron-down"
				chevName = tr("Collapse %s", n.Text)
			}
			cells = append(cells, ui.ButtonOf(extIcon(chev).Size(12).Color(colorGray), toggle).
				Ghost().Pad(2).Radius(4).Disabled(n.Disabled).Name(chevName))
		} else {
			cells = append(cells, widget.Box().Width(16))
		}

		if chk := n.Check; chk != "" {
			chkState := v.form.local(id+"\x00check", chk)
			on := ggui.Bind(
				func() bool { return chkState.Get() != "off" },
				func(next bool) {
					if n.Disabled || bare {
						return
					}
					val := "off"
					if next {
						val = "on"
					}
					chkState.Set(val)
					if n.Checks {
						v.send(id, uiblock.EventCheck, val)
					}
				},
			)
			ind := ggui.Map(chkState, func(s string) bool { return s == "mixed" })
			chkName := n.Placeholder
			if chkName == "" {
				chkName = tr("Select")
				if n.Text != "" {
					chkName = tr("Select %s", n.Text)
				}
			}
			cb := ui.Checkbox(on, "").BindIndeterminate(ind).Disabled(n.Disabled || bare || !n.Checks).Name(chkName)
			cells = append(cells, extQuietBox(cb))
		}

		badgeMode := n.Variant == "badge"
		var bodyItems []ggui.Widget
		if n.Icon != "" {
			bodyItems = append(bodyItems, extIcon(n.Icon).Size(14).Color(toneColor(cmpTone(n.Tone, "muted"))))
		}
		weight := textWeight(n.Weight)
		if active && n.Weight == "" {
			weight = ggui.WeightMedium
		}
		labelColor := toneColor(n.Tone)
		if badgeMode && !active {
			labelColor = uitheme.Fg
		}
		bodyItems = append(bodyItems, widget.Expanded(
			widget.Text(n.Text).Size(12.5).Weight(weight).Color(labelColor).NoWrap().Ellipsis(),
		))
		if n.Value != "" {
			if badgeMode {
				bodyItems = append(bodyItems, widget.Box(widget.Text(n.Value).Size(10.5).Color(colorGray).NoWrap()).
					Pad(1, 6).Radius(6).Fill(uitheme.Muted))
			} else {
				bodyItems = append(bodyItems, widget.Text(n.Value).Size(11).Color(colorGray).NoWrap())
			}
		}
		if len(n.Badges) > 0 {
			bodyItems = append(bodyItems, renderExtBadges(n.Badges)...)
		}
		body := widget.Row(bodyItems...).Gap(6).Align(widget.AlignCenter)

		var rowContent ggui.Widget = widget.Expanded(widget.Padding(body, 4, 6))
		switch {
		case !bare && n.Tap && !n.Disabled:
			rowContent = widget.Expanded(ui.ButtonOf(body, func() { v.send(id, uiblock.EventTap, "") }).
				Ghost().Pad(4, 6).Radius(6).Name(n.Text))
		case expandable && !n.Disabled:
			rowContent = widget.Expanded(ui.ButtonOf(body, toggle).
				Ghost().Pad(4, 6).Radius(6).Name(n.Text))
		}
		cells = append(cells, rowContent)

		rowBox := widget.Box(widget.Row(cells...).Gap(2).Align(widget.AlignCenter)).Radius(6).Pad(1, 4)
		if active {
			rowBox = rowBox.Fill(uitheme.Muted)
		}
		var row ggui.Widget = rowBox
		if !bare {
			row = v.treeItemDragDrop(rowBox, n)
			row = v.withContextMenu(row, n, "", "")
		}

		if !hasKids || !expanded {
			return row
		}
		sub := make([]ggui.Widget, 0, 1+len(n.Children))
		sub = append(sub, row)
		for _, child := range n.Children {
			if child == nil {
				continue
			}
			sub = append(sub, v.treeItemNode(child, depth+1, false))
		}
		return widget.Column(sub...).Gap(1).Stretch()
	})
}

func (v *extTreeView) treeItemDragDrop(w ggui.Widget, n *uiblock.Node) ggui.Widget {
	drag := n.Drag
	drop := n.Drop
	if drag == "" && !drop {
		return w
	}
	id := n.ID
	accept := n.DropAccept
	blk := v.blockID()
	accepts := func(d extBlockDrag) bool {
		if d.ExtID+"\x00"+d.ViewID != blk || d.Node == id || d.Value == "" {
			return false
		}
		if len(accept) > 0 && !slices.Contains(accept, d.Group) {
			return false
		}
		return true
	}
	if drop {
		inner := w
		over := ggui.State(false)
		framed := ggui.View(over, func(hot bool) ggui.Widget {
			if hot {
				return widget.Box(inner).Fill(uitheme.Muted).Border(1.5, colorBlue).Radius(6)
			}
			return inner
		})
		w = widget.DropZone(framed, func(d widget.Dropped[extBlockDrag]) {
			over.Set(false)
			if !accepts(d.Value) {
				return
			}
			v.send(id, uiblock.EventDrop, d.Value.Value)
		}).Accept(accepts).OnHover(over.Set)
	}
	if drag != "" && !n.Disabled {
		w = widget.DragSource(w, extBlockDrag{ExtID: v.extID, ViewID: v.viewID, Node: id, Value: drag, Group: n.DragGroup})
	}
	return w
}

func (v *extTreeView) container(n *uiblock.Node, bare bool) ggui.Widget {
	kind := n.Kind
	gap := n.Gap

	switch kind {
	case uiblock.KindWrap:
		if gap == 0 {
			gap = 6
		}
		kids := make([]ggui.Widget, 0, len(n.Children))
		for _, c := range n.Children {
			if c == nil {
				continue
			}
			kids = append(kids, v.node(c, true))
		}
		return v.maybeDragDrop(v.tappableBoxWhen(widget.Wrap(kids...).Gap(gap).RunGap(gap).Align(widget.AlignCenter), n, !bare), n, bare)

	case uiblock.KindGrid:
		if gap == 0 {
			gap = 8
		}
		cols := n.Columns
		if cols < 1 {
			cols = 1
		}
		if cols > 6 {
			cols = 6
		}
		children := n.Children
		var rows []ggui.Widget
		for i := 0; i < len(children); i += cols {
			rowKids := make([]ggui.Widget, 0, cols)
			for j := range cols {
				if i+j < len(children) && children[i+j] != nil {
					rowKids = append(rowKids, widget.Expanded(v.node(children[i+j], false)))
				} else {
					rowKids = append(rowKids, widget.Expanded(widget.Box()))
				}
			}
			rows = append(rows, widget.Row(rowKids...).Gap(gap).Align(widget.AlignStart))
		}
		return v.maybeDragDrop(v.tappableBoxWhen(widget.Column(rows...).Gap(gap).Stretch(), n, !bare), n, bare)

	case uiblock.KindSplit:
		children := n.Children
		if len(children) < 2 {
			return widget.Box()
		}
		id := n.ID
		initial := n.Ratio
		if initial < 0.1 || initial > 0.9 {
			initial = 0.5
		}
		st := v.form.local(id, strconv.FormatFloat(initial, 'f', 4, 64))
		ratio := ggui.Bind(
			func() float64 {
				f, err := strconv.ParseFloat(st.Get(), 64)
				if err != nil || f < 0.1 || f > 0.9 {
					return initial
				}
				return f
			},
			func(f float64) {
				st.Set(strconv.FormatFloat(clampFloat(f, 0.1, 0.9), 'f', 4, 64))
			},
		)
		first := v.node(children[0], false)
		second := v.node(children[1], false)
		hFirst := widget.Padding(first, 0, 6, 0, 0)
		hSecond := widget.Padding(second, 0, 0, 0, 6)
		vFirst := widget.Padding(first, 0, 0, 6, 0)
		vSecond := widget.Padding(second, 6, 0, 0, 0)
		dividerName := cmpTone(n.Text, tr("Pane divider"))
		var out ggui.Widget
		if n.Direction == "auto" {
			horiz := ui.Resizable(ratio, hFirst, hSecond).WithHandle().MinSizes(60, 60).
				Disabled(n.Disabled).Name(dividerName)
			vert := ui.Resizable(ratio, vFirst, vSecond).Vertical().WithHandle().MinSizes(60, 60).
				Disabled(n.Disabled).Name(dividerName)
			out = &autoSplit{horiz: horiz, vert: vert}
		} else if n.Direction == "vertical" {
			out = ui.Resizable(ratio, vFirst, vSecond).Vertical().WithHandle().MinSizes(60, 60).
				Disabled(n.Disabled).Name(dividerName)
		} else {
			out = ui.Resizable(ratio, hFirst, hSecond).WithHandle().MinSizes(60, 60).
				Disabled(n.Disabled).Name(dividerName)
		}
		if h := n.MaxHeight; h > 0 {
			out = widget.Box(out).MaxHeight(h)
		}
		return v.maybeDragDrop(out, n, bare)

	case uiblock.KindTable:
		return v.tableNode(n, bare)
	}

	row := kind == uiblock.KindRow
	toolbar := row && n.Variant == "toolbar"
	prevToolbar := v.inToolbar
	if toolbar {
		v.inToolbar = true
	}
	defer func() { v.inToolbar = prevToolbar }()
	hasGrow := false
	if row {
		for _, c := range n.Children {
			if c == nil || (c.Kind == uiblock.KindMenu && c.Variant == "context") {
				continue
			}
			if c.Grow > 0 {
				hasGrow = true
				break
			}
		}
	}
	kids := make([]ggui.Widget, 0, len(n.Children))
	var dialogs []ggui.Widget
	for _, c := range n.Children {
		if c == nil || (c.Kind == uiblock.KindMenu && c.Variant == "context") {
			continue
		}
		if c.Kind == uiblock.KindDialog {
			dialogs = append(dialogs, v.dialogNode(c))
			continue
		}
		var w ggui.Widget
		if kind == uiblock.KindList && c.Tap {
			id := c.ID
			inner := appendBadges(v.nodeWith(c, false, true), c.Badges)
			w = extListRow(inner, strings.Join(extNodeTexts(c), " "), func() { v.send(id, uiblock.EventTap, "") })
			w = v.dragDrop(w, c)
			w = v.withContextMenu(w, c, "", "")
		} else {
			w = v.node(c, row)
		}
		if !toolbar {
			switch g := c.Grow; {
			case g == 1:
				w = widget.Expanded(w)
			case g > 0:
				w = widget.Flex(w, g)
			case row && !hasGrow && (c.Kind == uiblock.KindPill || c.Kind == uiblock.KindChip):
				w = widget.Flexible(w)
			}
		}
		kids = append(kids, w)
	}
	wrapDialogs := func(main ggui.Widget) ggui.Widget {
		if len(dialogs) == 0 {
			return main
		}
		return widget.Stack(append([]ggui.Widget{main}, dialogs...)...)
	}
	switch kind {
	case uiblock.KindRow:
		if gap == 0 {
			if toolbar {
				gap = 8
			} else {
				gap = 8
			}
		}
		align := widget.AlignCenter
		if !toolbar {
			for _, c := range n.Children {
				if c == nil {
					continue
				}
				switch c.Kind {
				case uiblock.KindColumn, uiblock.KindList, uiblock.KindCard, uiblock.KindScroll, uiblock.KindGrid, uiblock.KindSplit:
					align = widget.AlignStart
				}
				if align == widget.AlignStart {
					break
				}
			}
		}
		return wrapDialogs(v.maybeDragDrop(v.tappableBoxWhen(widget.Row(kids...).Gap(gap).Align(align), n, !bare), n, bare))
	case uiblock.KindStack:
		return wrapDialogs(v.maybeDragDrop(widget.Stack(kids...), n, bare))
	case uiblock.KindCard:
		if gap == 0 {
			gap = 8
		}
		inner := widget.Column(kids...).Gap(gap).Stretch()
		if !bare && (n.Drag != "" || n.Drop) {
			return wrapDialogs(v.dragDrop(v.tappableBoxWhen(inner, n, !bare), n))
		}
		border := colorBorder
		if n.Tone != "" {
			border = toneColor(n.Tone)
		}
		var out ggui.Widget
		radius := 8.0
		switch n.Variant {
		case "ghost":
			out = widget.Box(inner).Radius(8).Pad(12, 14)
		case "outline":
			out = widget.Box(inner).Border(1, border).Radius(8).Pad(12, 14)
		case "secondary":
			out = widget.Box(inner).Fill(uitheme.Muted).Border(1, border).Radius(8).Pad(12, 14)
		case "danger":
			out = widget.Box(inner).Fill(uitheme.Destructive.Alpha(0.12)).Border(1, colorRed).Radius(8).Pad(12, 14)
		case "warning":
			out = &extWashed{child: widget.Box(inner).Border(1, colorAmber).Radius(8).Pad(12, 14), color: colorAmber, radius: 8}
		case "primary":
			out = widget.Box(inner).Fill(uitheme.Muted).Border(1, colorBlue).Radius(8).Pad(12, 14)
		case "soft":
			radius = 10
			if n.Tone != "" {
				out = &extWashed{child: widget.Box(inner).Pad(12, 14), color: toneColor(n.Tone), radius: 10}
			} else {
				out = widget.Box(inner).Fill(uitheme.Muted).Radius(10).Pad(12, 14)
			}
		default:
			out = widget.Box(inner).Fill(uitheme.Card).Border(1, border).Radius(8).Pad(12, 14)
		}
		if !bare && n.Tap {
			id := n.ID
			out = ui.ButtonOf(out, func() { v.send(id, uiblock.EventTap, "") }).
				Ghost().Pad(0).Radius(radius).Name(strings.Join(extNodeTexts(n), " "))
		}
		return wrapDialogs(out)
	case uiblock.KindScroll:
		horiz := n.Direction == "horizontal"
		var body ggui.Widget
		if horiz {
			if gap == 0 {
				gap = 8
			}
			body = widget.Row(kids...).Gap(gap).Align(widget.AlignStart)
		} else {
			if gap == 0 {
				gap = 6
			}
			body = widget.Column(kids...).Gap(gap).Stretch()
		}
		sc := widget.Scroll(body).Shrink().Bar(uitheme.MutedFg.Alpha(0.45))
		if n.Variant == "follow" {
			sc = sc.FollowEnd()
		}
		var out ggui.Widget = sc
		if horiz {
			sc = sc.Horizontal()
			out = looseHeight{sc}
		}
		if h := n.MaxHeight; h > 0 {
			out = widget.Box(out).MaxHeight(h)
		}
		return wrapDialogs(v.maybeDragDrop(out, n, bare))
	case uiblock.KindList:
		return wrapDialogs(v.maybeDragDrop(widget.Column(kids...).Gap(gap).Stretch(), n, bare))
	}
	if gap == 0 {
		gap = 6
	}
	return wrapDialogs(v.maybeDragDrop(v.tappableBoxWhen(widget.Column(kids...).Gap(gap).Stretch(), n, !bare), n, bare))
}

func (v *extTreeView) tableNode(n *uiblock.Node, bare bool) ggui.Widget {
	hdrs := n.Labels
	children := n.Children
	numCols := len(hdrs)
	for _, c := range children {
		if c != nil && c.Kind == uiblock.KindRow && len(c.Children) > numCols {
			numCols = len(c.Children)
		}
	}
	weights := tableColumnWeights(numCols, children)

	const colGap = 8.0
	var trows []ggui.Widget
	if len(hdrs) > 0 && numCols > 0 {
		cols := make([]ggui.Widget, numCols)
		for i := range numCols {
			label := ""
			if i < len(hdrs) {
				label = hdrs[i]
			}
			hdr := widget.Text(label).Size(11.5).Weight(ggui.WeightSemibold).Color(colorGray).NoWrap().Ellipsis()
			cols[i] = widget.Flex(widget.Align(hdr).At(0, 0.5), weights[i])
		}
		trows = append(trows, widget.Box(widget.Row(cols...).Gap(colGap).Align(widget.AlignCenter)).Pad(6, 10).Fill(uitheme.Card), ui.Divider())
	}
	for i, c := range children {
		if c == nil {
			continue
		}
		if i > 0 {
			trows = append(trows, ui.Divider())
		}
		if c.Kind == uiblock.KindRow && numCols > 0 {
			cols := make([]ggui.Widget, numCols)
			rowKids := c.Children
			for j := range numCols {
				var cell ggui.Widget = widget.Box()
				if j < len(rowKids) && rowKids[j] != nil {
					cell = v.node(rowKids[j], true)
				}
				cols[j] = widget.Flex(widget.Align(cell).At(0, 0.5), weights[j])
			}
			rowW := widget.Row(cols...).Gap(colGap).Align(widget.AlignCenter)
			var wrapped ggui.Widget
			if c.Tap {
				id := c.ID
				wrapped = ui.ButtonOf(rowW, func() { v.send(id, uiblock.EventTap, "") }).Ghost().Pad(6, 10).Radius(0).Name(strings.Join(extNodeTexts(c), " "))
			} else {
				wrapped = widget.Box(rowW).Pad(6, 10)
			}
			trows = append(trows, v.withContextMenu(v.dragDrop(wrapped, c), c, "", ""))
			continue
		}
		trows = append(trows, widget.Box(v.node(c, false)).Pad(6, 10))
	}
	return v.maybeDragDrop(widget.Box(widget.Column(trows...).Stretch()).Border(1, colorBorder).Radius(8), n, bare)
}

type autoSplit struct {
	horiz, vert ggui.Widget
	useHoriz    bool
}

func (a *autoSplit) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	a.useHoriz = !math.IsInf(c.MaxW, 0) && c.MaxW >= autoSplitBreakpoint
	if a.useHoriz {
		return a.horiz.Layout(c, env)
	}
	return a.vert.Layout(c, env)
}

func (a *autoSplit) Paint(dst *ggui.Canvas, r ggui.Rect) {
	if a.useHoriz {
		dst.Paint(a.horiz, r)
		return
	}
	dst.Paint(a.vert, r)
}

func tableColumnWeights(numCols int, rows []*uiblock.Node) []float64 {
	if numCols <= 0 {
		return nil
	}
	maxGrow := make([]float64, numCols)
	anyGrow, allGrow := false, true
	for _, r := range rows {
		if r == nil || r.Kind != uiblock.KindRow {
			continue
		}
		for i, cell := range r.Children {
			if cell != nil && i < numCols && cell.Grow > maxGrow[i] {
				maxGrow[i] = cell.Grow
			}
		}
	}
	for _, g := range maxGrow {
		if g > 0 {
			anyGrow = true
		} else {
			allGrow = false
		}
	}
	weights := make([]float64, numCols)
	for i, g := range maxGrow {
		switch {
		case !anyGrow:
			weights[i] = 1
		case allGrow:
			weights[i] = g
		case g > 0:
			weights[i] = 1 + g
		default:
			weights[i] = 1
		}
	}
	return weights
}

func isContainerKind(kind string) bool {
	switch kind {
	case uiblock.KindColumn, uiblock.KindRow, uiblock.KindStack, uiblock.KindCard, uiblock.KindScroll,
		uiblock.KindList, uiblock.KindTable, uiblock.KindWrap, uiblock.KindGrid, uiblock.KindSplit:
		return true
	}
	return false
}

func (v *extTreeView) dragDrop(w ggui.Widget, n *uiblock.Node) ggui.Widget {
	drag := n.Drag
	drop := n.Drop
	if drag == "" && !drop {
		return w
	}
	id := n.ID
	accept := n.DropAccept
	isCard := n.Kind == uiblock.KindCard || (drag != "" && isContainerKind(n.Kind))
	isLane := !isCard && isContainerKind(n.Kind)
	blk := v.blockID()
	accepts := func(d extBlockDrag) bool {
		if d.ExtID+"\x00"+d.ViewID != blk || d.Node == id || d.Value == "" {
			return false
		}
		if len(accept) > 0 && !slices.Contains(accept, d.Group) {
			return false
		}
		return true
	}
	if drop {
		inner := w
		over := ggui.State(false)
		framed := ggui.View(over, func(hot bool) ggui.Widget {
			switch {
			case hot && isCard:
				bar := widget.Box().Height(2).Fill(colorBlue).Radius(1)
				card := widget.Box(inner).Fill(uitheme.Muted).Border(1.5, colorBlue).Radius(8).Pad(8, 10)
				return widget.Column(bar, card).Gap(4).Stretch()
			case isCard:
				return widget.Box(inner).Fill(uitheme.Card).Border(1, colorBorder).Radius(8).Pad(8, 10)
			case hot && isLane:
				return widget.Box(inner).Fill(uitheme.Muted).Border(1.5, colorBlue).Radius(8).Pad(8)
			case isLane:
				return widget.Box(inner).Fill(uitheme.Card).Border(1, colorBorder).Radius(8).Pad(8)
			case hot:
				return widget.Box(inner).Border(1.5, colorBlue).Radius(6)
			default:
				return inner
			}
		})
		w = widget.DropZone(framed, func(d widget.Dropped[extBlockDrag]) {
			over.Set(false)
			if !accepts(d.Value) {
				return
			}
			v.send(id, uiblock.EventDrop, d.Value.Value)
		}).Accept(accepts).OnHover(over.Set)
	} else if isCard {
		w = widget.Box(w).Fill(uitheme.Card).Border(1, colorBorder).Radius(8).Pad(8, 10)
	}
	if drag != "" && !n.Disabled {
		w = widget.DragSource(w, extBlockDrag{ExtID: v.extID, ViewID: v.viewID, Node: id, Value: drag, Group: n.DragGroup})
	}
	return w
}

func (v *extTreeView) maybeDragDrop(w ggui.Widget, n *uiblock.Node, bare bool) ggui.Widget {
	if bare {
		return w
	}
	return v.dragDrop(w, n)
}

func (v *extTreeView) tappableBoxWhen(w ggui.Widget, n *uiblock.Node, active bool) ggui.Widget {
	if !active || !n.Tap {
		return w
	}
	id := n.ID
	return extListRow(w, strings.Join(extNodeTexts(n), " "), func() { v.send(id, uiblock.EventTap, "") })
}

func extNodeTexts(n *uiblock.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	if n.Text != "" {
		out = append(out, n.Text)
	}
	for _, c := range n.Children {
		out = append(out, extNodeTexts(c)...)
	}
	return out
}

// ---- Chart node ---------------------------------------------------------

func extChartNode(n *uiblock.Node) ggui.Widget {
	if n == nil {
		return widget.Box()
	}
	switch n.Chart {
	case "", "line", "bar", "sparkline", "donut", "pie":
	default:
		return widget.Box()
	}
	var config chart.Config
	var cleanSeries []*uiblock.Series
	points := 0
	for _, s := range n.Series {
		if s == nil {
			continue
		}
		j := len(cleanSeries)
		cleanSeries = append(cleanSeries, s)
		sc := chart.Series{Key: strconv.Itoa(j), Label: s.Name}
		if s.Tone != "" {
			sc.Color = toneColor(s.Tone)
		}
		config = append(config, sc)
		points = max(points, len(s.Values))
	}
	if len(config) == 0 || points == 0 {
		return widget.Box()
	}
	data := make([]chart.Datum, points)
	for i := range data {
		label := strconv.Itoa(i + 1)
		if i < len(n.Labels) {
			label = n.Labels[i]
		}
		data[i] = chart.Datum{Label: label, Values: map[string]float64{}}
		for j, s := range cleanSeries {
			if i < len(s.Values) {
				v := s.Values[i]
				if math.IsNaN(v) || math.IsInf(v, 0) {
					v = 0
				}
				data[i].Values[strconv.Itoa(j)] = v
			}
		}
	}
	name := cmpTone(n.Text, tr("Chart"))
	var c *chart.Widget
	switch n.Chart {
	case "bar":
		c = chart.Bar(data, config).Height(160).Legend(len(config) > 1)
	case "sparkline":
		c = chart.Line(data, config).Height(36).Axes(false, false).Grid(false).Legend(false).Dots(false).
			Curve(chart.CurveLinear).TooltipCursor(false).StrokeWidth(1.5)
	case "donut":
		c = chart.Pie(data, config).Height(190).InnerRadius(0.55).OuterRadius(0.85).Legend(true).AnimationDelay(0)
	case "pie":
		c = chart.Pie(data, config).Height(190).InnerRadius(0).OuterRadius(0.85).Legend(true).AnimationDelay(0)
	default:
		c = chart.Line(data, config).Height(160).Legend(len(config) > 1).Dots(false).Curve(chart.CurveMonotone)
	}
	c = c.Animation(chartAnimation).Name(name)
	if n.Text == "" {
		return c
	}
	return widget.Column(
		widget.Text(n.Text).Size(textSize(n.Size)).Weight(textWeight(n.Weight)).Color(toneColor(cmpTone(n.Tone, "muted"))),
		c,
	).Gap(6).Stretch()
}

// ---- Graph node ---------------------------------------------------------

func (v *extTreeView) graphNode(n *uiblock.Node) ggui.Widget {
	id := n.ID
	st := v.form.graphState(id)
	nodes := n.GraphNodes
	edges := n.GraphEdges
	sig := graphSignature(nodes, edges)
	if st.sig != sig || st.layout.pos == nil {
		st.sig = sig
		st.layout = computeGraphLayout(nodes, edges)
	}

	height := n.MaxHeight
	if height <= 0 {
		height = 320
	}
	if height < 120 {
		height = 120
	}
	if height > 1200 {
		height = 1200
	}

	tick := ggui.State(0)
	bump := func() { tick.Update(func(x int) int { return x + 1 }) }

	var onSelect func(string)
	if n.Change && !n.Disabled {
		onSelect = func(nodeID string) {
			v.send(id, uiblock.EventChange, nodeID)
		}
	}

	canvas := &extGraphCanvas{
		state:      st,
		nodes:      nodes,
		edges:      edges,
		selectedID: n.Value,
		height:     height,
		onSelect:   onSelect,
		tick:       tick,
		name:       cmpTone(n.Text, tr("Graph")),
	}
	canvas.Role = ggui.RoleGroup
	canvas.SetName(canvas.name)
	canvas.SetKey("graph:" + v.extID + ":" + v.viewID + ":" + id)
	canvas.SetInert(n.Disabled)

	controls := widget.Align(
		widget.Padding(
			widget.Box(widget.Row(
				ui.Tooltip(
					ui.ButtonOf(widget.Text("+").Size(13).Weight(ggui.WeightMedium).Color(uitheme.Fg).NoWrap(), func() {
						st.zoom = clampFloat(st.zoom*1.25, 0.35, 3.0)
						bump()
					}).Ghost().Pad(2, 6).Radius(5).Name(tr("Zoom in")),
					tr("Zoom in"),
				),
				ui.Tooltip(
					ui.ButtonOf(widget.Text("−").Size(13).Weight(ggui.WeightMedium).Color(uitheme.Fg).NoWrap(), func() {
						st.zoom = clampFloat(st.zoom/1.25, 0.35, 3.0)
						bump()
					}).Ghost().Pad(2, 6).Radius(5).Name(tr("Zoom out")),
					tr("Zoom out"),
				),
				ui.Tooltip(
					ui.ButtonOf(extIcon("refresh-cw").Size(12).Color(colorGray), func() {
						st.reset()
						bump()
					}).Ghost().Pad(4).Radius(5).Name(tr("Reset")),
					tr("Reset"),
				),
			).Gap(2).Align(widget.AlignCenter)).Fill(uitheme.Bg).Border(1, colorBorder).Radius(7).Pad(2),
			8,
		),
	).At(1, 0)

	layered := widget.Stack(canvas, controls)
	if n.Text == "" {
		return layered
	}
	return widget.Column(
		widget.Text(n.Text).Size(textSize(n.Size)).Weight(textWeight(n.Weight)).Color(toneColor(cmpTone(n.Tone, "muted"))),
		layered,
	).Gap(6).Stretch()
}

type extGraphCanvas struct {
	ggui.Interactive
	state      *graphViewState
	nodes      []*uiblock.GraphNode
	edges      []*uiblock.GraphEdge
	selectedID string
	height     float64
	onSelect   func(string)
	tick       *ggui.StateValue[int]
	name       string

	env   ggui.Env
	theme *uitheme.Theme
	font  *ggui.Font
	rect  ggui.Rect

	colBg       color.Color
	colSurface0 color.Color
	colBorder   color.Color
	colFg       color.Color
	colMuted    color.Color
	colAccent   color.Color
	colGridDot  color.Color

	dragging bool
	moved    bool
	downPos  ggui.Point
	lastPos  ggui.Point
}

func (g *extGraphCanvas) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	g.Sync()
	if g.tick != nil {
		_ = g.tick.Get()
	}
	g.env = env
	g.theme = uitheme.Of(env)
	g.font = env.ResolveText(ggui.TextStyle{Size: 11}).Font
	g.colBg = resolveOr(uitheme.Card, env, g.theme.Card)
	g.colSurface0 = resolveOr(uitheme.Bg, env, g.theme.Bg)
	g.colBorder = resolveOr(uitheme.Border, env, g.theme.Border)
	g.colFg = resolveOr(uitheme.Fg, env, g.theme.Fg)
	g.colMuted = resolveOr(uitheme.MutedFg, env, g.theme.MutedFg)
	g.colAccent = resolveOr(colorBlue, env, g.theme.Primary)
	g.colGridDot = graphFade(g.colBorder, 0.55)

	w := c.MaxW
	if math.IsInf(w, 1) || w <= 0 {
		w = 420
	}
	h := g.height
	if !math.IsInf(c.MaxH, 1) && c.MaxH > 0 && h > c.MaxH {
		h = c.MaxH
	}
	return c.Constrain(ggui.Sz(w, h))
}

func (g *extGraphCanvas) Describe() ggui.Node {
	return ggui.Node{
		Role:     ggui.RoleGroup,
		Name:     g.name,
		Disabled: g.IsInert(),
	}
}

func (g *extGraphCanvas) CaptureDrag() bool      { return !g.IsInert() }
func (g *extGraphCanvas) CaptureTouchDrag() bool { return !g.IsInert() }

func (g *extGraphCanvas) Adopt(prev any) {
	g.Interactive.Adopt(prev)
	if p, ok := prev.(*extGraphCanvas); ok {
		g.dragging = p.dragging
		g.moved = p.moved
		g.downPos = p.downPos
		g.lastPos = p.lastPos
	}
}

func (g *extGraphCanvas) worldToScreen(r ggui.Rect, pt ggui.Point) ggui.Point {
	scale := g.baseScale(r) * g.state.zoom
	cx := r.Origin.X + r.Size.W/2 + g.state.panX
	cy := r.Origin.Y + r.Size.H/2 + g.state.panY
	return ggui.Pt(cx+(pt.X-g.state.layout.centerX)*scale, cy+(pt.Y-g.state.layout.centerY)*scale)
}

func (g *extGraphCanvas) baseScale(r ggui.Rect) float64 {
	availW := max(r.Size.W-96, 40)
	availH := max(r.Size.H-76, 40)
	s := min(availW/max(g.state.layout.spanW, 1), availH/max(g.state.layout.spanH, 1))
	return clampFloat(s, 10, 95)
}

func (g *extGraphCanvas) nodeRadius(nd *uiblock.GraphNode) float64 {
	w := nd.Weight
	if w <= 0 {
		w = 1
	}
	base := clampFloat(7.5+math.Sqrt(w)*4.2, 9, 22)
	return base * clampFloat(math.Sqrt(g.state.zoom), 0.72, 1.45)
}

func (g *extGraphCanvas) nodeColor(nd *uiblock.GraphNode) color.Color {
	if nd.Tone != "" {
		return resolveOr(toneColor(nd.Tone), g.env, g.colAccent)
	}
	if grp := nd.Group; grp != "" && g.theme != nil && len(g.theme.Chart) > 0 {
		idx := int(hashString64(grp) % uint64(len(g.theme.Chart)))
		if c := g.theme.Chart[idx]; c != nil {
			return c
		}
	}
	return g.colAccent
}

func (g *extGraphCanvas) hitTestNode(r ggui.Rect, p ggui.Point) string {
	hitNode := func(nd *uiblock.GraphNode) bool {
		wp, ok := g.state.layout.pos[nd.ID]
		if !ok {
			return false
		}
		sp := g.worldToScreen(r, wp)
		rad := max(g.nodeRadius(nd)+4, 14)
		dx := p.X - sp.X
		dy := p.Y - sp.Y
		return dx*dx+dy*dy <= rad*rad
	}
	for i := len(g.nodes) - 1; i >= 0; i-- {
		nd := g.nodes[i]
		if nd != nil && (nd.ID == g.selectedID || nd.ID == g.state.hoveredID) && hitNode(nd) {
			return nd.ID
		}
	}
	for i := len(g.nodes) - 1; i >= 0; i-- {
		nd := g.nodes[i]
		if nd != nil && nd.ID != g.selectedID && nd.ID != g.state.hoveredID && hitNode(nd) {
			return nd.ID
		}
	}
	return ""
}

func (g *extGraphCanvas) Paint(dst *ggui.Canvas, r ggui.Rect) {
	g.rect = r
	const radius = 8.0
	dst.FillRoundRect(r, radius, g.colBg)

	cursor := ggui.CursorShapeMove
	if g.state.hoveredID != "" {
		cursor = ggui.CursorShapePointer
	}
	g.Hit(dst, r, g, cursor)

	dst.Node(r, g.Describe(), func(dst *ggui.Canvas) {
		dst.ClipRoundRect(r, radius, func(dst *ggui.Canvas) {
			g.paintGrid(dst, r)
			if len(g.nodes) == 0 {
				msg := tr("No nodes.")
				tw := dst.TextWidth(msg, g.font, 12)
				dst.DrawText(msg, g.font, 12, ggui.Pt(r.Origin.X+(r.Size.W-tw)/2, r.Origin.Y+r.Size.H/2-7), g.colMuted)
				return
			}
			g.paintEdges(dst, r)
			g.paintNodes(dst, r)
		})
	})

	dst.StrokeRoundRect(r, radius, 1, g.colBorder)
}

func (g *extGraphCanvas) paintGrid(dst *ggui.Canvas, r ggui.Rect) {
	step := clampFloat(28*g.state.zoom, 18, 56)
	offX := math.Mod(g.state.panX, step)
	offY := math.Mod(g.state.panY, step)
	if offX < 0 {
		offX += step
	}
	if offY < 0 {
		offY += step
	}
	for x := r.Origin.X + offX; x < r.Origin.X+r.Size.W; x += step {
		for y := r.Origin.Y + offY; y < r.Origin.Y+r.Size.H; y += step {
			dst.FillCircle(ggui.Pt(x, y), 1, g.colGridDot)
		}
	}
}

func (g *extGraphCanvas) paintEdges(dst *ggui.Canvas, r ggui.Rect) {
	nodeByID := make(map[string]*uiblock.GraphNode, len(g.nodes))
	for _, nd := range g.nodes {
		if nd != nil {
			nodeByID[nd.ID] = nd
		}
	}
	focus := g.selectedID
	if g.state.hoveredID != "" {
		focus = g.state.hoveredID
	}

	for _, e := range g.edges {
		if e == nil {
			continue
		}
		fromNode, okA := nodeByID[e.From]
		toNode, okB := nodeByID[e.To]
		if !okA || !okB {
			continue
		}
		pA, okA := g.state.layout.pos[e.From]
		pB, okB := g.state.layout.pos[e.To]
		if !okA || !okB {
			continue
		}
		a := g.worldToScreen(r, pA)
		b := g.worldToScreen(r, pB)
		vx, vy := b.X-a.X, b.Y-a.Y
		dist := math.Hypot(vx, vy)
		rA := g.nodeRadius(fromNode) + 2
		rB := g.nodeRadius(toNode) + 3
		if dist <= rA+rB+4 {
			continue
		}
		ux, uy := vx/dist, vy/dist
		start := ggui.Pt(a.X+ux*rA, a.Y+uy*rA)
		end := ggui.Pt(b.X-ux*rB, b.Y-uy*rB)

		incident := focus != "" && (e.From == focus || e.To == focus)
		baseCol := g.colMuted
		if e.Tone != "" {
			baseCol = resolveOr(toneColor(e.Tone), g.env, g.colMuted)
		}
		alpha := 0.55
		strokeW := 1.4
		if incident {
			alpha = 0.95
			strokeW = 2.0
		} else if focus != "" {
			alpha = 0.28
		}
		col := graphFade(baseCol, alpha)

		if e.Dashed {
			drawDashedLine(dst, start, end, 6, 4, strokeW, col)
		} else {
			dst.StrokeLine(start, end, strokeW, col)
		}

		const arrowLen = 7.0
		const arrowHalfW = 3.2
		tip := end
		baseX, baseY := tip.X-ux*arrowLen, tip.Y-uy*arrowLen
		px, py := -uy*arrowHalfW, ux*arrowHalfW
		var arrow ggui.Path
		arrow.MoveTo(tip.X, tip.Y)
		arrow.LineTo(baseX+px, baseY+py)
		arrow.LineTo(baseX-px, baseY-py)
		arrow.Close()
		dst.FillPath(&arrow, col)

		if lbl := e.Label; lbl != "" && dist > 64 {
			mid := ggui.Pt((start.X+end.X)/2, (start.Y+end.Y)/2)
			tw := dst.TextWidth(lbl, g.font, 9.5)
			padX, padY := 4.0, 2.0
			pillRect := ggui.Rct(ggui.Pt(mid.X-tw/2-padX, mid.Y-6-padY), ggui.Sz(tw+padX*2, 12+padY*2))
			dst.FillRoundRect(pillRect, 4, graphFade(g.colSurface0, 0.88))
			dst.DrawText(lbl, g.font, 9.5, ggui.Pt(mid.X-tw/2, mid.Y-6), graphFade(g.colMuted, max(alpha, 0.75)))
		}
	}
}

func (g *extGraphCanvas) paintNodes(dst *ggui.Canvas, r ggui.Rect) {
	order := make([]*uiblock.GraphNode, 0, len(g.nodes))
	var top []*uiblock.GraphNode
	for _, nd := range g.nodes {
		if nd == nil {
			continue
		}
		if nd.ID == g.selectedID || nd.ID == g.state.hoveredID {
			top = append(top, nd)
		} else {
			order = append(order, nd)
		}
	}
	order = append(order, top...)

	for _, nd := range order {
		wp, ok := g.state.layout.pos[nd.ID]
		if !ok {
			continue
		}
		sp := g.worldToScreen(r, wp)
		rad := g.nodeRadius(nd)
		col := g.nodeColor(nd)
		isSelected := nd.ID == g.selectedID
		isHovered := nd.ID == g.state.hoveredID

		if isSelected {
			dst.FillCircle(sp, rad+7, graphFade(col, 0.24))
			var ring ggui.Path
			ring.Arc(sp.X, sp.Y, rad+3.5, 0, 2*math.Pi)
			dst.StrokePath(&ring, 2, col)
		} else if isHovered {
			dst.FillCircle(sp, rad+4.5, graphFade(col, 0.22))
		}

		dst.FillCircle(sp, rad, col)
		var rim ggui.Path
		rim.Arc(sp.X, sp.Y, rad, 0, 2*math.Pi)
		dst.StrokePath(&rim, 1.5, g.colSurface0)

		label := nd.Label
		if label != "" {
			const fontSize = 11.0
			fitted := dst.FitText(label, g.font, fontSize, 130)
			tw := dst.TextWidth(fitted, g.font, fontSize)
			padX, padY := 6.0, 2.5
			pillW := tw + padX*2
			pillH := 15.0 + padY*2
			pillRect := ggui.Rct(ggui.Pt(sp.X-pillW/2, sp.Y+rad+5), ggui.Sz(pillW, pillH))
			dst.FillRoundRect(pillRect, 6, graphFade(g.colSurface0, 0.9))
			borderCol := g.colBorder
			if isSelected || isHovered {
				borderCol = col
			}
			dst.StrokeRoundRect(pillRect, 6, 1, borderCol)
			dst.DrawText(fitted, g.font, fontSize, ggui.Pt(pillRect.Origin.X+padX, pillRect.Origin.Y+padY), g.colFg)

			if (isSelected || isHovered) && nd.Subtitle != "" {
				sub := dst.FitText(nd.Subtitle, g.font, 9.5, 140)
				stw := dst.TextWidth(sub, g.font, 9.5)
				subRect := ggui.Rct(ggui.Pt(sp.X-stw/2-5, pillRect.Origin.Y+pillRect.Size.H+2), ggui.Sz(stw+10, 14))
				dst.FillRoundRect(subRect, 4, graphFade(g.colSurface0, 0.88))
				dst.DrawText(sub, g.font, 9.5, ggui.Pt(subRect.Origin.X+5, subRect.Origin.Y+1), g.colMuted)
			}
		}

		hitR := ggui.Rct(ggui.Pt(sp.X-rad, sp.Y-rad), ggui.Sz(rad*2, rad*2))
		dst.Leaf(hitR, ggui.Node{
			Role:     ggui.RoleButton,
			Name:     nd.Label,
			Selected: isSelected,
			Disabled: g.IsInert() || g.onSelect == nil,
		})
	}
}

func (g *extGraphCanvas) HandleKey(ev ggui.KeyEvent) {
	g.Keyboard(ev, nil)
}

func (g *extGraphCanvas) HandlePointer(ev ggui.PointerEvent) bool {
	if g.IsInert() {
		return false
	}
	switch ev.Kind {
	case ggui.PointerMove:
		hit := g.hitTestNode(g.rect, ev.Pos)
		if hit != g.state.hoveredID {
			g.state.hoveredID = hit
			ggui.Invalidate(g.env)
		}
		return false
	case ggui.PointerExit:
		if g.state.hoveredID != "" {
			g.state.hoveredID = ""
			ggui.Invalidate(g.env)
		}
		return g.Pointer(ev, nil)
	case ggui.PointerDown:
		if ev.Button != ggui.MouseButtonLeft {
			return false
		}
		g.dragging = true
		g.moved = false
		g.downPos = ev.Pos
		g.lastPos = ev.Pos
		return g.Pointer(ev, nil)
	case ggui.PointerDrag:
		if g.dragging {
			if !g.moved && math.Hypot(ev.Pos.X-g.downPos.X, ev.Pos.Y-g.downPos.Y) > 4 {
				g.moved = true
			}
			if g.moved {
				g.state.panX += ev.Pos.X - g.lastPos.X
				g.state.panY += ev.Pos.Y - g.lastPos.Y
				ggui.Invalidate(g.env)
			}
			g.lastPos = ev.Pos
			return true
		}
	case ggui.PointerUp:
		wasDragging, wasMoved := g.dragging, g.moved
		g.dragging = false
		g.moved = false
		if wasDragging && !wasMoved && ev.Button == ggui.MouseButtonLeft {
			if hit := g.hitTestNode(g.rect, ev.Pos); hit != "" && g.onSelect != nil {
				g.onSelect(hit)
			}
		}
		return g.Pointer(ev, nil)
	case ggui.PointerScroll:
		if !(ev.Mods.Ctrl || ev.Mods.Meta || ev.Mods.Shift) {
			return false
		}
		if math.IsNaN(ev.Scroll.Y) || math.IsInf(ev.Scroll.Y, 0) {
			return true
		}
		if ev.Scroll.Y != 0 {
			factor := 1.0 - ev.Scroll.Y*0.08
			if factor < 0.75 {
				factor = 0.75
			}
			if factor > 1.35 {
				factor = 1.35
			}
			g.state.zoom = clampFloat(g.state.zoom*factor, 0.35, 3.0)
			ggui.Invalidate(g.env)
			return true
		}
		return false
	}
	return g.Pointer(ev, nil)
}

func drawDashedLine(dst *ggui.Canvas, a, b ggui.Point, dash, gap, width float64, col color.Color) {
	vx, vy := b.X-a.X, b.Y-a.Y
	total := math.Hypot(vx, vy)
	if total <= 0 {
		return
	}
	ux, uy := vx/total, vy/total
	for d := 0.0; d < total; d += dash + gap {
		end := min(d+dash, total)
		p0 := ggui.Pt(a.X+ux*d, a.Y+uy*d)
		p1 := ggui.Pt(a.X+ux*end, a.Y+uy*end)
		dst.StrokeLine(p0, p1, width, col)
	}
}

func resolveOr(c color.Color, env ggui.Env, fallback color.Color) color.Color {
	if col := ggui.ResolveColor(c, env); col != nil {
		return col
	}
	if fallback != nil {
		return fallback
	}
	return color.Black
}

func graphFade(c color.Color, alpha float64) color.Color {
	if c == nil {
		return color.Transparent
	}
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	a := clampFloat(float64(n.A)*alpha, 0, 255)
	return color.NRGBA{R: n.R, G: n.G, B: n.B, A: uint8(a)}
}

func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- DAG node -----------------------------------------------------------

func (v *extTreeView) dagNode(n *uiblock.Node, bare bool) ggui.Widget {
	rows := n.Rows
	if len(rows) == 0 {
		if note := n.Text; note != "" {
			return widget.Box(ui.Caption(note).Size(12.5)).Pad(14, 16)
		}
		return widget.Box()
	}
	id := n.ID
	ds := v.form.dagState(id, rows)
	sel := v.form.local(id+"\x00sel", n.Value)

	list := widget.Each(ds.entries, func(item widget.EachItem[dagRowEntry]) ggui.Widget {
		e := item.Item
		if e.row == nil {
			return widget.Box()
		}
		return v.dagRowWidget(n, e.row, e.graph, e.lanes, sel, bare)
	}).Stretch().ItemExtent(dagRowHeight)

	var out ggui.Widget = widget.Scroll(widget.Padding(list, 0, 0, 4, 0)).
		Key(v.keyPrefix() + "dag\x00" + id).
		BindOffset(ds.offset).
		Bar(uitheme.MutedFg.Alpha(0.45))
	if h := n.MaxHeight; h > 0 {
		out = widget.Box(out).MaxHeight(h)
	}
	return v.maybeDragDrop(out, n, bare)
}

func (v *extTreeView) dagRowWidget(n *uiblock.Node, r *uiblock.Row, g dagGraphRow, lanes int, sel *ggui.StateValue[string], bare bool) ggui.Widget {
	rowID := r.ID
	var top []ggui.Widget
	if r.Icon != "" {
		top = append(top, extIcon(r.Icon).Size(12).Color(toneColor(cmpTone(r.Tone, "muted"))))
	}
	subject := widget.Text(r.Text).Size(12.5).Color(toneColor(r.Tone)).NoWrap().Ellipsis()
	if r.Strong {
		subject = subject.Weight(ggui.WeightMedium)
	}
	top = append(top, widget.Expanded(subject))
	if len(r.Badges) > 0 {
		top = append(top, renderDagBadges(r.Badges)...)
	}

	var lines []ggui.Widget
	lines = append(lines, widget.Row(top...).Gap(5).Align(widget.AlignCenter))
	if r.Detail != "" || r.Meta != "" {
		var bottom []ggui.Widget
		bottom = append(bottom, widget.Expanded(widget.Text(r.Detail).Size(11).Color(colorGray).NoWrap().Ellipsis()))
		if r.Meta != "" {
			bottom = append(bottom, extChip(r.Meta))
		}
		lines = append(lines, widget.Row(bottom...).Gap(6).Align(widget.AlignCenter))
	}
	textCol := widget.Column(lines...).Gap(2).Stretch()
	content := widget.Row(&dagGraphCell{row: g, lanes: lanes, strong: r.Strong}, widget.Expanded(textCol)).Gap(6).Align(widget.AlignCenter)

	chosen := ggui.Map(sel, func(cur string) bool { return cur != "" && cur == rowID })
	rowBox := ggui.View(chosen, func(on bool) ggui.Widget {
		box := widget.Box(content).Height(dagRowHeight).Pad(0, 10, 0, 6)
		if on {
			box = box.Fill(uitheme.Muted)
		}
		return box
	})

	nodeID := n.ID
	rowName := cmpTone(r.Text, rowID)
	tap := ui.ButtonOf(rowBox, func() {
		if bare || n.Disabled {
			return
		}
		sel.Set(rowID)
		if n.Change {
			v.send(nodeID, uiblock.EventChange, rowID)
		}
	}).Ghost().Pad(0).Radius(0).Name(rowName)

	if !bare && n.ContextMenu != "" {
		return v.withContextMenu(tap, n, rowID, cmpTone(r.Meta, rowName))
	}
	return tap
}

func renderDagBadges(badges []*uiblock.Badge) []ggui.Widget {
	out := make([]ggui.Widget, 0, min(len(badges), 3))
	count := 0
	for _, b := range badges {
		if b == nil {
			continue
		}
		if count == 2 {
			out = append(out, ui.Caption(fmt.Sprintf("+%d", len(badges)-2)).Size(10.5))
			break
		}
		out = append(out, renderExtBadge(b))
		count++
	}
	return out
}
