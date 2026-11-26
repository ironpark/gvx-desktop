package ui

import (
	"maps"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"
)

func newBulkSelection() *ggui.StateValue[map[string]bool] {
	return ggui.State(map[string]bool(nil))
}

func toggleSelectionKey(sel *ggui.StateValue[map[string]bool], key string, on bool) {
	sel.Update(func(cur map[string]bool) map[string]bool {
		next := maps.Clone(cur)
		if on {
			if next == nil {
				next = map[string]bool{}
			}
			next[key] = true
		} else if len(next) > 0 {
			delete(next, key)
		}
		return next
	})
}

func setSelectionKeys(sel *ggui.StateValue[map[string]bool], keys []string, on bool) {
	sel.Update(func(cur map[string]bool) map[string]bool {
		next := maps.Clone(cur)
		if on {
			if next == nil {
				next = map[string]bool{}
			}
			for _, k := range keys {
				next[k] = true
			}
		} else if len(next) > 0 {
			for _, k := range keys {
				delete(next, k)
			}
		}
		return next
	})
}

func clearSelection(sel *ggui.StateValue[map[string]bool]) {
	if len(ggui.Peek(sel)) == 0 {
		return
	}
	sel.Set(nil)
}

func selectedItems[T any](items []T, sel map[string]bool, keyFn func(T) string, canSelect func(T) (bool, string)) []T {
	if len(sel) == 0 || len(items) == 0 {
		return nil
	}
	var out []T
	for _, it := range items {
		if !sel[keyFn(it)] {
			continue
		}
		if canSelect != nil {
			if ok, _ := canSelect(it); !ok {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// quietTableCheckbox renders a compact checkbox tuned for table rows and headers.
func quietTableCheckbox(cb *ui.CheckboxWidget) ggui.Widget {
	return uitheme.Override(func(t *uitheme.Theme) {
		t.ControlSize = min(t.ControlSize, 15)
	}, cb)
}

// selectCheckboxColumn builds a leading checkbox column with a tri-state
// Select-All checkbox in its heading. canSelect may be nil when every row is
// selectable; otherwise returning (false, reason) disables the row checkbox
// and excludes that row from Select-All.
func selectCheckboxColumn[T any](
	visible ggui.Readable[[]T],
	sel *ggui.StateValue[map[string]bool],
	keyFn func(T) string,
	labelFn func(T) string,
	canSelect func(T) (bool, string),
) ui.Column[T] {
	type headState struct {
		total   int
		checked int
	}
	hs := ggui.Derived(func() headState {
		items := visible.Get()
		cur := sel.Get()
		var st headState
		for _, it := range items {
			if canSelect != nil {
				if ok, _ := canSelect(it); !ok {
					continue
				}
			}
			st.total++
			if cur[keyFn(it)] {
				st.checked++
			}
		}
		return st
	})
	headerAll := ggui.Bind(
		func() bool {
			st := hs.Get()
			return st.total > 0 && st.checked == st.total
		},
		func(on bool) {
			items := ggui.Peek(visible)
			var keys []string
			for _, it := range items {
				if canSelect != nil {
					if ok, _ := canSelect(it); !ok {
						continue
					}
				}
				keys = append(keys, keyFn(it))
			}
			setSelectionKeys(sel, keys, on)
		},
	)
	headerInd := ggui.Map(hs, func(st headState) bool { return st.checked > 0 && st.checked < st.total })
	headerDisabled := ggui.Map(hs, func(st headState) bool { return st.total == 0 })
	header := quietTableCheckbox(
		ui.Checkbox(headerAll, "").
			BindIndeterminate(headerInd).
			BindDisabled(headerDisabled).
			Name(tr("Select all")),
	)

	col := ui.Col("", func(r ggui.Readable[T]) ggui.Widget {
		type rowCheck struct {
			key    string
			label  string
			ok     bool
			reason string
		}
		info := ggui.Map(r, func(it T) rowCheck {
			ok, reason := true, ""
			if canSelect != nil {
				ok, reason = canSelect(it)
			}
			return rowCheck{key: keyFn(it), label: labelFn(it), ok: ok, reason: reason}
		})
		return ggui.View(info, func(rc rowCheck) ggui.Widget {
			b := ggui.Bind(
				func() bool {
					cur := sel.Get()
					return rc.ok && cur[rc.key]
				},
				func(on bool) {
					if !rc.ok {
						return
					}
					toggleSelectionKey(sel, rc.key, on)
				},
			)
			w := quietTableCheckbox(
				ui.Checkbox(b, "").
					Disabled(!rc.ok).
					Name(tr("Select %s", rc.label)),
			)
			if !rc.ok && rc.reason != "" {
				return tip(w, rc.reason)
			}
			return w
		})
	}).W(38).Center().Identified("select")
	col.Header = header
	return col
}

// bulkActionStrip renders the inline selection summary and bulk actions bar
// shown whenever at least one item is checked.
func bulkActionStrip(count ggui.Readable[int], clear func(), actions ...ggui.Widget) ggui.Widget {
	label := ggui.View(count, func(n int) ggui.Widget {
		return ui.Badge(trn(n, "%d selected", "%d selected")).Secondary()
	})
	parts := []ggui.Widget{label, widget.Spacer()}
	parts = append(parts, actions...)
	parts = append(parts, tip(iconButton(lucide.X, tr("Clear selection"), clear), tr("Clear selection")))
	return widget.Box(widget.Row(parts...).Gap(6).Align(widget.AlignCenter)).Pad(4, 4, 0, 4)
}

// bulkToolbar stacks a table's normal toolbar with an inline bulk action bar
// that appears only when count > 0.
func bulkToolbar(mainBar ggui.Widget, count ggui.Readable[int], clear func(), actions ...ggui.Widget) ggui.Widget {
	hasSel := ggui.Map(count, func(n int) bool { return n > 0 })
	bar := ggui.If(hasSel, func() ggui.Widget {
		return widget.Column(
			widget.Padding(ui.Divider(), 6, 0, 2, 0),
			bulkActionStrip(count, clear, actions...),
		).Stretch()
	})
	return widget.Column(mainBar, bar).Stretch()
}
