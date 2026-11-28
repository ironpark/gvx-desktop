package ui

import (
	"hash/fnv"
	"image/color"
	"math"
	"slices"
	"strconv"

	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

// blockForm holds user-edited field values across declarative UI updates, along
// with client-only state (collapsible/tree_item/split locals, graph view state,
// and dag virtual list state) that must NOT be sent in uiblock.Event.Values.
type blockForm struct {
	fields       map[string]*ggui.StateValue[string]
	given        map[string]string
	drawn        map[string]bool
	locals       map[string]*ggui.StateValue[string]
	localGiven   map[string]string
	dialogs      map[string]*ggui.StateValue[bool]
	drawnDialogs map[string]bool
	graphs       map[string]*graphViewState
	dags         map[string]*dagViewState
}

func newBlockForm() *blockForm {
	return &blockForm{
		fields:       map[string]*ggui.StateValue[string]{},
		given:        map[string]string{},
		drawn:        map[string]bool{},
		locals:       map[string]*ggui.StateValue[string]{},
		localGiven:   map[string]string{},
		dialogs:      map[string]*ggui.StateValue[bool]{},
		drawnDialogs: map[string]bool{},
		graphs:       map[string]*graphViewState{},
		dags:         map[string]*dagViewState{},
	}
}

func (f *blockForm) redraw() {
	if f == nil {
		return
	}
	f.drawn = map[string]bool{}
	f.drawnDialogs = map[string]bool{}
}

// field returns the persistent signal for form node id. If the extension sends
// a changed default value (given), it updates the signal; otherwise user edits
// survive across view updates.
func (f *blockForm) field(id, given string) *ggui.StateValue[string] {
	if f.fields == nil {
		f.fields = map[string]*ggui.StateValue[string]{}
	}
	if f.given == nil {
		f.given = map[string]string{}
	}
	if f.drawn == nil {
		f.drawn = map[string]bool{}
	}
	f.drawn[id] = true
	st, ok := f.fields[id]
	if !ok {
		st = ggui.State(given)
		f.fields[id] = st
	} else if f.given[id] != given {
		st.Set(given)
	}
	f.given[id] = given
	return st
}

func (f *blockForm) input(id, given string) ggui.Binding[string] {
	return f.field(id, given)
}

// local keeps client-only UI state (collapsible, tree_item, split) without
// registering the node ID in drawn / values().
func (f *blockForm) local(id, given string) *ggui.StateValue[string] {
	if f.locals == nil {
		f.locals = map[string]*ggui.StateValue[string]{}
		f.localGiven = map[string]string{}
	}
	st, ok := f.locals[id]
	if !ok {
		st = ggui.State(given)
		f.locals[id] = st
	} else if f.localGiven[id] != given {
		st.Set(given)
	}
	f.localGiven[id] = given
	return st
}

// dialogOpen keeps modal open state persistent across view rebuilds as long as
// the dialog node ID remains in the tree. Once the node ID is absent from a
// render pass, pruneDialogs drops it so a subsequent open starts as true again.
func (f *blockForm) dialogOpen(id string) *ggui.StateValue[bool] {
	if f == nil {
		return ggui.State(true)
	}
	if f.dialogs == nil {
		f.dialogs = map[string]*ggui.StateValue[bool]{}
	}
	if f.drawnDialogs == nil {
		f.drawnDialogs = map[string]bool{}
	}
	f.drawnDialogs[id] = true
	st, ok := f.dialogs[id]
	if !ok {
		st = ggui.State(true)
		f.dialogs[id] = st
	}
	return st
}

func (f *blockForm) pruneDialogs() {
	if f == nil || len(f.dialogs) == 0 {
		return
	}
	for id := range f.dialogs {
		if !f.drawnDialogs[id] {
			delete(f.dialogs, id)
		}
	}
}

func (f *blockForm) graphState(id string) *graphViewState {
	if f.graphs == nil {
		f.graphs = map[string]*graphViewState{}
	}
	st, ok := f.graphs[id]
	if !ok {
		st = newGraphViewState()
		f.graphs[id] = st
	}
	return st
}

func (f *blockForm) dagState(id string, rows []*uiblock.Row) *dagViewState {
	if f.dags == nil {
		f.dags = map[string]*dagViewState{}
	}
	clean := make([]*uiblock.Row, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			clean = append(clean, r)
		}
	}
	graph := dagGraph(clean)
	lanes := dagGraphWidth(graph)
	entries := make([]dagRowEntry, len(clean))
	for i, r := range clean {
		entries[i] = dagRowEntry{row: r, graph: graph[i], lanes: lanes}
	}
	if st, ok := f.dags[id]; ok {
		st.entries.Set(entries)
		return st
	}
	st := &dagViewState{
		entries: ggui.State(entries).WithEqual(equalDagEntries),
		offset:  ggui.State(0.0),
	}
	f.dags[id] = st
	return st
}

// values returns current values of all drawn form fields (excluding locals,
// graphs, and dags).
func (f *blockForm) values() map[string]string {
	if f == nil || len(f.drawn) == 0 {
		return nil
	}
	out := make(map[string]string, len(f.drawn))
	for id := range f.drawn {
		if st := f.fields[id]; st != nil {
			out[id] = ggui.Peek(st)
		}
	}
	return out
}

// ---- Graph layout & view state ------------------------------------------

type graphViewState struct {
	panX, panY float64
	zoom       float64
	hoveredID  string

	sig    uint64
	layout graphLayout
}

func newGraphViewState() *graphViewState {
	return &graphViewState{zoom: 1}
}

func (s *graphViewState) reset() {
	s.panX, s.panY, s.zoom = 0, 0, 1
}

type graphLayout struct {
	pos              map[string]ggui.Point
	minX, maxX       float64
	minY, maxY       float64
	centerX, centerY float64
	spanW, spanH     float64
}

func graphSignature(nodes []*uiblock.GraphNode, edges []*uiblock.GraphEdge) uint64 {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		write(n.ID)
		write(n.Group)
		write(strconv.FormatFloat(n.Weight, 'g', 6, 64))
	}
	_, _ = h.Write([]byte{1})
	for _, e := range edges {
		if e == nil {
			continue
		}
		write(e.From)
		write(e.To)
	}
	return h.Sum64()
}

func computeGraphLayout(nodes []*uiblock.GraphNode, edges []*uiblock.GraphEdge) graphLayout {
	cleanNodes := make([]*uiblock.GraphNode, 0, len(nodes))
	for _, nd := range nodes {
		if nd != nil {
			cleanNodes = append(cleanNodes, nd)
		}
	}
	n := len(cleanNodes)
	pos := make(map[string]ggui.Point, n)
	if n == 0 {
		return graphLayout{pos: pos, spanW: 1, spanH: 1}
	}
	if n == 1 {
		pos[cleanNodes[0].ID] = ggui.Pt(0, 0)
		return graphLayout{pos: pos, spanW: 1, spanH: 1}
	}

	idxByID := make(map[string]int, n)
	xs := make([]float64, n)
	ys := make([]float64, n)
	groups := make([]string, n)

	const goldenAngle = 2.399963229728653
	for i, nd := range cleanNodes {
		id := nd.ID
		idxByID[id] = i
		groups[i] = nd.Group
		h := hashString64(id)
		angle := float64(h%3600)*math.Pi/1800.0 + float64(i)*goldenAngle*0.05
		rad := 1.0 + math.Sqrt(float64(i+1))*0.85 + float64((h>>16)%100)/250.0
		xs[i] = math.Cos(angle) * rad
		ys[i] = math.Sin(angle) * rad
	}

	type edgeIdx struct{ a, b int }
	el := make([]edgeIdx, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		a, okA := idxByID[e.From]
		b, okB := idxByID[e.To]
		if okA && okB && a != b {
			el = append(el, edgeIdx{a, b})
		}
	}

	const iterations = 120
	area := float64(n) * 4.5
	k := math.Sqrt(area / float64(n))
	if k < 1.2 {
		k = 1.2
	}
	k2 := k * k
	dx := make([]float64, n)
	dy := make([]float64, n)

	for iter := range iterations {
		for i := range n {
			dx[i], dy[i] = 0, 0
		}

		for i := range n {
			for j := i + 1; j < n; j++ {
				vx := xs[i] - xs[j]
				vy := ys[i] - ys[j]
				dist2 := vx*vx + vy*vy
				if dist2 < 1e-4 {
					vx = 0.01 * float64((i%3)-1)
					vy = 0.01 * float64(((j+1)%3)-1)
					if vx == 0 && vy == 0 {
						vx = 0.01
					}
					dist2 = vx*vx + vy*vy
				}
				dist := math.Sqrt(dist2)
				rep := k2 / dist
				ux, uy := vx/dist, vy/dist
				dx[i] += ux * rep
				dy[i] += uy * rep
				dx[j] -= ux * rep
				dy[j] -= uy * rep

				if groups[i] != "" && groups[i] == groups[j] {
					coh := dist * 0.06
					dx[i] -= ux * coh
					dy[i] -= uy * coh
					dx[j] += ux * coh
					dy[j] += uy * coh
				}
			}
		}

		for _, e := range el {
			vx := xs[e.a] - xs[e.b]
			vy := ys[e.a] - ys[e.b]
			dist := math.Hypot(vx, vy)
			if dist < 1e-4 {
				continue
			}
			att := (dist * dist) / k
			ux, uy := vx/dist, vy/dist
			dx[e.a] -= ux * att
			dy[e.a] -= uy * att
			dx[e.b] += ux * att
			dy[e.b] += uy * att
		}

		for i := range n {
			dx[i] -= xs[i] * 0.18
			dy[i] -= ys[i] * 0.18
		}

		t := float64(iterations-iter) / float64(iterations)
		temp := 0.04 + 0.55*t*t
		for i := range n {
			disp := math.Hypot(dx[i], dy[i])
			if disp > 1e-6 {
				step := min(disp, temp)
				xs[i] += (dx[i] / disp) * step
				ys[i] += (dy[i] / disp) * step
			}
		}
	}

	minX, maxX := xs[0], xs[0]
	minY, maxY := ys[0], ys[0]
	for i := range n {
		if xs[i] < minX {
			minX = xs[i]
		}
		if xs[i] > maxX {
			maxX = xs[i]
		}
		if ys[i] < minY {
			minY = ys[i]
		}
		if ys[i] > maxY {
			maxY = ys[i]
		}
	}
	cx := (minX + maxX) / 2
	cy := (minY + maxY) / 2
	for i, nd := range cleanNodes {
		pos[nd.ID] = ggui.Pt(xs[i]-cx, ys[i]-cy)
	}
	spanW := max(maxX-minX, 1.0)
	spanH := max(maxY-minY, 1.0)
	return graphLayout{
		pos:     pos,
		minX:    minX - cx,
		maxX:    maxX - cx,
		minY:    minY - cy,
		maxY:    maxY - cy,
		centerX: 0,
		centerY: 0,
		spanW:   spanW,
		spanH:   spanH,
	}
}

func hashString64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// ---- DAG layout & view state --------------------------------------------

type dagGraphRow struct {
	Col   int
	Lanes int
	Lines []dagGraphLine
}

type dagGraphLine struct {
	From, To int
	Lower    bool
}

func dagGraph(items []*uiblock.Row) []dagGraphRow {
	rows := make([]dagGraphRow, len(items))
	var lanes []string
	free := func(ls []string, not int) int {
		for i, id := range ls {
			if id == "" && i != not {
				return i
			}
		}
		return len(ls)
	}
	put := func(ls []string, i int, id string) []string {
		for len(ls) <= i {
			ls = append(ls, "")
		}
		ls[i] = id
		return ls
	}
	for n, r := range items {
		row := &rows[n]
		if r == nil {
			continue
		}
		rid := r.ID
		col := slices.Index(lanes, rid)
		incoming := col >= 0
		if !incoming {
			col = free(lanes, -1)
			lanes = put(lanes, col, rid)
		}
		row.Col = col
		next := slices.Clone(lanes)
		for i, h := range lanes {
			switch {
			case h == "":
			case h == rid:
				if i != col || incoming {
					row.Lines = append(row.Lines, dagGraphLine{From: i, To: col})
				}
				next[i] = ""
			default:
				row.Lines = append(row.Lines, dagGraphLine{From: i, To: i}, dagGraphLine{From: i, To: i, Lower: true})
			}
		}
		for k, p := range r.Parents {
			j := slices.Index(next, p)
			if j < 0 {
				j = col
				if k > 0 || next[col] != "" {
					j = free(next, col)
				}
				next = put(next, j, p)
			}
			row.Lines = append(row.Lines, dagGraphLine{From: col, To: j, Lower: true})
		}
		for len(next) > 0 && next[len(next)-1] == "" {
			next = next[:len(next)-1]
		}
		row.Lanes = max(len(lanes), len(next), col+1)
		lanes = next
	}
	return rows
}

var dagLaneColors = []color.Color{
	colorBlue,
	colorGreen,
	colorAmber,
	color.NRGBA{R: 0xA8, G: 0x55, B: 0xF7, A: 0xFF},
	color.NRGBA{R: 0xEC, G: 0x48, B: 0x99, A: 0xFF},
	color.NRGBA{R: 0x06, G: 0xB6, B: 0xD4, A: 0xFF},
}

const (
	dagLaneWidth = 14
	dagRowHeight = 40
	dagMaxLanes  = 8
	dagLineWidth = 2
	dagDotRadius = 4.5
)

func dagGraphWidth(rows []dagGraphRow) int {
	w := 1
	for _, r := range rows {
		w = max(w, r.Lanes)
	}
	return min(w, dagMaxLanes)
}

type dagGraphCell struct {
	row    dagGraphRow
	lanes  int
	strong bool
	hole   color.Color
}

func (g *dagGraphCell) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	t := uitheme.Of(env)
	g.hole = t.Bg
	return c.Constrain(ggui.Sz(float64(max(g.lanes, 1))*dagLaneWidth+4, dagRowHeight))
}

func (g *dagGraphCell) Paint(dst *ggui.Canvas, r ggui.Rect) {
	x := func(lane int) float64 { return r.Origin.X + 2 + float64(lane)*dagLaneWidth + dagLaneWidth/2 }
	top, mid, bottom := r.Origin.Y, r.Origin.Y+r.Size.H/2, r.Origin.Y+r.Size.H
	through := map[int]bool{}
	for _, l := range g.row.Lines {
		if l.Lower && l.From == l.To && slices.Contains(g.row.Lines, dagGraphLine{From: l.From, To: l.To}) {
			through[l.From] = true
		}
	}
	for _, l := range g.row.Lines {
		if l.From >= dagMaxLanes || l.To >= dagMaxLanes {
			continue
		}
		lane := l.From
		a, b := ggui.Pt(x(l.From), top), ggui.Pt(x(l.To), mid)
		switch {
		case l.From == l.To && through[l.From]:
			if l.Lower {
				continue
			}
			b = ggui.Pt(x(l.To), bottom)
		case l.Lower:
			lane = l.To
			a, b = ggui.Pt(x(l.From), mid), ggui.Pt(x(l.To), bottom)
		}
		strokeDagLane(dst, a, b, dagLaneColors[lane%len(dagLaneColors)])
	}
	if g.row.Col >= dagMaxLanes {
		return
	}
	dotPt, col := ggui.Pt(x(g.row.Col), mid), dagLaneColors[g.row.Col%len(dagLaneColors)]
	dst.FillCircle(dotPt, dagDotRadius+1.5, g.hole)
	dst.FillCircle(dotPt, dagDotRadius, col)
	if g.strong {
		dst.FillCircle(dotPt, dagDotRadius-2, g.hole)
	}
}

func strokeDagLane(dst *ggui.Canvas, a, b ggui.Point, col color.Color) {
	if a.X == b.X {
		dst.StrokeLine(a, b, dagLineWidth, col)
		return
	}
	var p ggui.Path
	p.MoveTo(a.X, a.Y)
	m := (a.Y + b.Y) / 2
	p.CubicTo(a.X, m, b.X, m, b.X, b.Y)
	dst.StrokePath(&p, dagLineWidth, col)
}

type dagRowEntry struct {
	row   *uiblock.Row
	graph dagGraphRow
	lanes int
}

type dagViewState struct {
	entries *ggui.StateValue[[]dagRowEntry]
	offset  *ggui.StateValue[float64]
}

func equalDagEntries(a, b []dagRowEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].lanes != b[i].lanes ||
			a[i].graph.Col != b[i].graph.Col ||
			a[i].graph.Lanes != b[i].graph.Lanes ||
			!slices.Equal(a[i].graph.Lines, b[i].graph.Lines) ||
			!rowEqual(a[i].row, b[i].row) {
			return false
		}
	}
	return true
}

func rowEqual(a, b *uiblock.Row) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID &&
		slices.Equal(a.Parents, b.Parents) &&
		a.Text == b.Text &&
		a.Detail == b.Detail &&
		a.Meta == b.Meta &&
		a.Tone == b.Tone &&
		a.Strong == b.Strong &&
		a.Icon == b.Icon &&
		badgesEqual(a.Badges, b.Badges)
}

func badgesEqual(a, b []*uiblock.Badge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == b[i] {
			continue
		}
		if a[i] == nil || b[i] == nil || *a[i] != *b[i] {
			return false
		}
	}
	return true
}
