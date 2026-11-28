package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/inspect"
	gguiruntime "github.com/ironpark/ggui/runtime"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

// ---- Test Helpers -------------------------------------------------------

type extUIMockHost struct {
	mu           sync.Mutex
	pickFolder   string
	openURLs     []string
	copied       []string
	toasts       [][2]string
	changedCount int
}

func (h *extUIMockHost) PickFolder(context.Context, string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pickFolder == "" {
		return "", false, nil
	}
	return h.pickFolder, true, nil
}

func (h *extUIMockHost) OpenTerminal(string, string, string) error { return nil }

func (h *extUIMockHost) OpenURL(rawURL string) error {
	h.mu.Lock()
	h.openURLs = append(h.openURLs, rawURL)
	h.mu.Unlock()
	return nil
}

func (h *extUIMockHost) CopyText(text string) error {
	h.mu.Lock()
	h.copied = append(h.copied, text)
	h.mu.Unlock()
	return nil
}

func (h *extUIMockHost) ShowToast(level, message string) {
	h.mu.Lock()
	h.toasts = append(h.toasts, [2]string{level, message})
	h.mu.Unlock()
}

func (h *extUIMockHost) Changed() {
	h.mu.Lock()
	h.changedCount++
	h.mu.Unlock()
}

func (h *extUIMockHost) snapshotCopied() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.copied)
}

type extUIMockBuilder struct {
	mu      sync.Mutex
	calls   []ext.BuildRequest
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *extUIMockBuilder) Build(ctx context.Context, req ext.BuildRequest, onLine func(string)) (string, error) {
	b.mu.Lock()
	b.calls = append(b.calls, req)
	b.mu.Unlock()
	if onLine != nil {
		onLine("Step 1/2 : FROM alpine:3.20")
	}
	if b.started != nil {
		b.once.Do(func() { close(b.started) })
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "sha256:testimage", nil
}

type extUIMockPuller struct {
	mu   sync.Mutex
	refs []string
}

func (p *extUIMockPuller) Pull(_ context.Context, ref string, line func(string)) error {
	p.mu.Lock()
	p.refs = append(p.refs, ref)
	p.mu.Unlock()
	if line != nil {
		line("Pulled " + ref)
	}
	return nil
}

func extUINewManager(t *testing.T, devMode bool, extraOpts ...func(*ext.Config)) (*ext.Manager, string, *extUIMockHost, *keychain.Fake) {
	t.Helper()
	dataDir := t.TempDir()
	kc := &keychain.Fake{}
	uiHost := &extUIMockHost{}
	cfg := ext.Config{
		DataDir:       dataDir,
		Keychain:      kc,
		DeveloperMode: func() bool { return devMode },
		HostInfo: func() ext.HostInfo {
			return ext.HostInfo{
				AppVersion: "test",
				OS:         runtime.GOOS,
				Arch:       runtime.GOARCH,
				Locale:     "en",
				Theme:      "light",
				DockerMode: "off",
			}
		},
		ImageBuilder: &extUIMockBuilder{},
		ImagePuller:  &extUIMockPuller{},
		UIHost:       uiHost,
	}
	for _, fn := range extraOpts {
		fn(&cfg)
	}
	mgr, err := ext.NewManager(cfg)
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close(2 * time.Second) })
	return mgr, dataDir, uiHost, kc
}

func extUIRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func extUIBuildSDKBinary(t *testing.T, src string) []byte {
	t.Helper()
	dir := t.TempDir()
	root := extUIRepoRoot(t)
	goMod := fmt.Sprintf("module example.com/extuifixture\n\ngo 1.27.0\n\nrequire github.com/ironpark/gvx-desktop v0.0.0\n\nreplace github.com/ironpark/gvx-desktop => %s\n", root)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	outBin := filepath.Join(dir, "ext-bin")
	cmd := exec.Command("go", "build", "-o", outBin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build SDK binary: %v\n%s", err, out)
	}
	b, err := os.ReadFile(outBin)
	if err != nil {
		t.Fatalf("read SDK binary: %v", err)
	}
	return b
}

func extUIManifestBinary(id, name, extraTOML string, perms ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\n")
	fmt.Fprintf(&b, "id = %q\n", id)
	fmt.Fprintf(&b, "version = \"0.1.0\"\n")
	fmt.Fprintf(&b, "publisher = \"acme\"\n")
	fmt.Fprintf(&b, "name = %q\n", name)
	fmt.Fprintf(&b, "description = \"UI unit test extension\"\n")
	fmt.Fprintf(&b, "icon = \"box\"\n")
	if len(perms) > 0 {
		q := make([]string, len(perms))
		for i, p := range perms {
			q[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(&b, "permissions = [%s]\n", strings.Join(q, ", "))
	}
	fmt.Fprintf(&b, "\n[runtime]\n")
	fmt.Fprintf(&b, "type = \"binary\"\n")
	fmt.Fprintf(&b, "dev_entrypoint = \"bin/ext\"\n")
	if extraTOML != "" {
		b.WriteString("\n" + extraTOML + "\n")
	}
	return b.String()
}

const extUIMinimalViewTOML = `
[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`

func extUIInstallBinary(t *testing.T, mgr *ext.Manager, id, name, src, extraTOML string, perms ...string) string {
	t.Helper()
	if extraTOML == "" {
		extraTOML = extUIMinimalViewTOML
	}
	bin := extUIBuildSDKBinary(t, src)
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extension.toml"), []byte(extUIManifestBinary(id, name, extraTOML, perms...)), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "ext"), bin, 0o755); err != nil {
		t.Fatalf("write bin/ext: %v", err)
	}
	if _, err := mgr.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal(%s): %v", id, err)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(%s): %v", id, err)
	}
	return dir
}

func extUINewProbe(t *testing.T, dataDir string, mgr *ext.Manager, size ggui.Size, setupFn func(*Model)) (*Model, *ggui.Probe) {
	t.Helper()
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	st := store.Store{Dir: dataDir}
	if cur, err := st.Settings(); err == nil && cur.Language == "" {
		cur.Language = LangEnglish
		_ = st.SaveSettings(cur)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, size)
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
		m = NewModel(st, dataDir)
		m.Offline = true
		m.Attach(p)
		if mgr != nil {
			m.SetExtensionManager(mgr)
		}
		if setupFn != nil {
			setupFn(m)
		}
	})
	p.Frame()
	if m == nil {
		t.Fatal("Model not initialized after initial Frame")
	}
	return m, p
}

func extUINewBlockProbe(t *testing.T, wFn func() ggui.Widget, size ggui.Size) *ggui.Probe {
	t.Helper()
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, wFn())
	}, size)
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
	})
	return p
}

func extUISettle(t *testing.T, p *ggui.Probe, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; semantics:\n%s", what, p.Semantics())
}

func extUISemHasText(sem *ggui.SemTree, substr string) bool {
	if sem == nil {
		return false
	}
	for _, n := range sem.All() {
		if strings.Contains(n.Name, substr) || strings.Contains(n.Value, substr) || strings.Contains(n.Description, substr) {
			return true
		}
	}
	return false
}

func extUIFindSemNode(sem *ggui.SemTree, role ggui.Role, nameSubstr string) (ggui.SemNode, bool) {
	if sem == nil {
		return ggui.SemNode{}, false
	}
	for _, n := range sem.All() {
		if (role == "" || n.Role == role) && strings.Contains(n.Name, nameSubstr) {
			return n, true
		}
	}
	return ggui.SemNode{}, false
}

func extUITapSem(t *testing.T, p *ggui.Probe, role ggui.Role, nameSubstr string) {
	t.Helper()
	sem := p.Semantics()
	n, ok := extUIFindSemNode(sem, role, nameSubstr)
	if !ok {
		t.Fatalf("missing semantic node role=%q name~=%q\n%s", role, nameSubstr, sem)
	}
	if !n.Offscreen && n.Rect.Size.W > 0 && n.Rect.Size.H > 0 {
		p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
		return
	}
	p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionPress})
}

// ---- Tests --------------------------------------------------------------

func TestExtRouteHelpers(t *testing.T) {
	if got := ExtensionPage("acme", "main"); got != "ext:acme/main" {
		t.Fatalf("ExtensionPage = %q, want ext:acme/main", got)
	}
	if id, view, ok := ParseExtensionPage("ext:acme/logs"); !ok || id != "acme" || view != "logs" {
		t.Fatalf("ParseExtensionPage = (%q, %q, %v)", id, view, ok)
	}
	for _, bad := range []string{"", "dashboard", "ext:", "ext:/main", "ext:acme/", "ext:acme"} {
		if _, _, ok := ParseExtensionPage(bad); ok {
			t.Fatalf("ParseExtensionPage(%q) should fail", bad)
		}
	}
}

func TestExtRenderAll35KindsAndFeatures(t *testing.T) {
	var u extsdk.UI
	treeNode := u.Column(
		u.Toolbar(
			u.Icon("box"),
			u.Pill("Live", extsdk.Tone("success")),
			u.Chip("v1", extsdk.Tone("accent")),
			u.Button("RunAction", extsdk.Key("run"), extsdk.OnTap("noop")),
			u.Menu("MoreMenu",
				u.MenuItem("RefreshItem", extsdk.OnTap("noop")),
				u.Divider(),
			),
		),
		u.Stack(u.Card(u.Text("CardContent"))),
		u.Scroll(
			u.Wrap(u.Breadcrumb([]string{"Home", "Runs"}, extsdk.Key("crumb"), extsdk.OnChange("noop"))),
			u.Grid(2,
				u.Input("init", extsdk.Key("inp"), extsdk.Placeholder("Search"), extsdk.OnChange("noop")),
				u.Input("secret", extsdk.Key("pw"), extsdk.Variant("password"), extsdk.Placeholder("Secret")),
				u.Textarea("notes", extsdk.Key("ta"), extsdk.Placeholder("Notes"), extsdk.OnChange("noop")),
				u.Toggle(true, "AutoSync", extsdk.Key("tog"), extsdk.OnChange("noop")),
				u.Select([]string{"fast", "safe"}, "fast", extsdk.Key("sel"), extsdk.OnChange("noop")),
				u.Tabs([]string{"Overview", "History"}, "Overview", extsdk.Key("tabs"), extsdk.OnChange("noop")),
				u.Checkbox("ConfirmBox", true, extsdk.Key("chk"), extsdk.OnChange("noop")),
			),
			u.Split("horizontal",
				u.Collapsible("DetailsGroup",
					u.List(
						u.Markdown("**MarkdownHeading** and [link](action:go)"),
						u.Code("go", "package main\nfunc main() {}"),
						u.Progress(0.65),
						u.Divider(),
					),
				),
				u.Tree(
					u.TreeItem("TreeNodeRoot", extsdk.Key("ti"), extsdk.Check("off"), extsdk.OnCheck("noop")),
				),
			),
			u.Table([]string{"Name", "Status"}, u.Row(u.Text("job-1"), u.Text("ok"))),
			u.Chart("line", []extsdk.ChartSeries{{Name: "CPU", Values: []float64{10, 20, 15}}}, extsdk.Labels("t1", "t2", "t3")),
			u.Graph([]extsdk.GraphNode{{ID: "a", Label: "Build"}, {ID: "b", Label: "Test"}}, []extsdk.GraphEdge{{From: "a", To: "b", Label: "next"}}, extsdk.MaxHeight(160)),
			u.Dag([]extsdk.DagRow{{ID: "d1", Text: "Checkout"}, {ID: "d2", Parents: []string{"d1"}, Text: "Compile"}}, extsdk.MaxHeight(160)),
			u.Dialog("SampleDialog", extsdk.Key("dlg"), extsdk.OnChange("noop"), u.Text("DialogBodyText")),
		),
	).Fill(true)

	raw, err := json.Marshal(treeNode)
	if err != nil {
		t.Fatalf("marshal 35-kind tree: %v", err)
	}
	parsed, err := uiblock.Read(raw)
	if err != nil {
		t.Fatalf("uiblock.Read: %v", err)
	}

	var gotEvents []uiblock.Event
	p := extUINewBlockProbe(t, func() ggui.Widget {
		return RenderUIBlock("acme-ext", "main", parsed.Root, func(ev uiblock.Event) {
			gotEvents = append(gotEvents, ev)
		})
	}, ggui.Sz(1280, 840))
	defer p.Close()
	p.Frame()

	sem := p.Semantics()
	for _, want := range []string{"RunAction", "CardContent", "AutoSync", "ConfirmBox", "SampleDialog", "acme-ext asks"} {
		if !extUISemHasText(sem, want) {
			t.Fatalf("missing %q in semantics:\n%s", want, sem)
		}
	}

	// Tabs MUST carry RoleTab.
	if _, ok := extUIFindSemNode(sem, ggui.RoleTab, "Overview"); !ok {
		t.Fatalf("expected Overview tab to have RoleTab:\n%s", sem)
	}
	// Password input must NOT expose plaintext value in semantics.
	if pw, ok := extUIFindSemNode(sem, ggui.RoleTextField, "Secret"); !ok {
		t.Fatalf("missing Secret password TextField:\n%s", sem)
	} else if strings.Contains(pw.Value, "secret") {
		t.Fatalf("password field leaked plaintext in semantics: %q", pw.Value)
	}

	// Escape on dialog sends change("dismiss").
	p.Key("escape")
	p.Frame()
	if !slices.ContainsFunc(gotEvents, func(ev uiblock.Event) bool {
		return strings.HasSuffix(ev.Node, "/dlg") && ev.Kind == uiblock.EventChange && ev.Value == "dismiss"
	}) {
		t.Fatalf("expected dialog dismiss event, got %+v", gotEvents)
	}
}

func TestExtRenderUnknownIconPrintableAndPanicResilience(t *testing.T) {
	// Include: nil child, unknown kind, invalid split, invalid chart, unknown icon,
	// and Markdown/Code with control/spoofing runes sanitized by Printable.
	p := extUINewBlockProbe(t, func() ggui.Widget {
		root := &uiblock.Node{
			Kind: uiblock.KindColumn,
			Children: []*uiblock.Node{
				nil,
				{ID: "/bogus", Kind: "totally_unknown", Text: "Skip"},
				{ID: "/split_bad", Kind: uiblock.KindSplit, Children: []*uiblock.Node{{ID: "/only", Kind: uiblock.KindText, Text: "OnlyOne"}}},
				{ID: "/chart_bad", Kind: uiblock.KindChart, Chart: "nonexistent"},
				{ID: "/ic", Kind: uiblock.KindIcon, Icon: "nonexistent-icon-name"},
				{ID: "/btn", Kind: uiblock.KindButton, Text: "FallbackIconBtn", Icon: "missing-icon", Tap: true},
				{ID: "/md", Kind: uiblock.KindMarkdown, Text: "Hello\u202eWorld"},
				{ID: "/cd", Kind: uiblock.KindCode, Text: "x\u202e := 1", Language: "go"},
				{ID: "/ok", Kind: uiblock.KindText, Text: "RecoveredLeaf"},
			},
		}
		return RenderUIBlock("corrupt-ext", "main", root, nil)
	}, ggui.Sz(800, 600))
	defer p.Close()
	p.Frame()

	sem := p.Semantics()
	if !extUISemHasText(sem, "FallbackIconBtn") || !extUISemHasText(sem, "RecoveredLeaf") {
		t.Fatalf("expected valid siblings to render despite corrupt nodes:\n%s", sem)
	}
	if strings.ContainsRune(sem.String(), '\u202e') {
		t.Fatalf("spoofing rune leaked into rendered semantics:\n%s", sem)
	}
}

func TestExtRenderSubmitDoesNotDoubleFireCommit(t *testing.T) {
	var events []uiblock.Event
	root := &uiblock.Node{
		Kind: uiblock.KindColumn,
		Children: []*uiblock.Node{
			{
				ID:          "/in",
				Kind:        uiblock.KindInput,
				Value:       "hello",
				Placeholder: "QueryBox",
				Change:      true,
			},
		},
	}
	p := extUINewBlockProbe(t, func() ggui.Widget {
		return RenderUIBlock("submit-ext", "main", root, func(ev uiblock.Event) {
			events = append(events, ev)
		})
	}, ggui.Sz(640, 480))
	defer p.Close()
	p.Frame()

	n, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "QueryBox")
	if !ok {
		t.Fatalf("missing QueryBox textfield:\n%s", p.Semantics())
	}
	p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
	p.Key("enter")
	p.Frame()

	if len(events) != 1 || events[0].Kind != uiblock.EventSubmit {
		t.Fatalf("expected exactly 1 submit event (no double-fired change), got %+v", events)
	}
}

func TestExtBlockFormPreservationLocalsAndLiftToolbar(t *testing.T) {
	form := newBlockForm()
	form.redraw()
	f1 := form.field("/name", "v1")
	f1.Set("edited")
	// Locals should never appear in values().
	loc := form.local("/split", "0.5")
	loc.Set("0.7")

	vals := form.values()
	if len(vals) != 1 || vals["/name"] != "edited" {
		t.Fatalf("unexpected form.values() = %v", vals)
	}
	if _, hasLocal := vals["/split"]; hasLocal {
		t.Fatalf("local leaked into form.values(): %v", vals)
	}

	// Same given keeps user edit; changed given overwrites.
	form.redraw()
	if got := ggui.Peek(form.field("/name", "v1")); got != "edited" {
		t.Fatalf("expected user edit preserved on unchanged given, got %q", got)
	}
	form.redraw()
	if got := ggui.Peek(form.field("/name", "v2")); got != "v2" {
		t.Fatalf("expected changed given to overwrite user edit, got %q", got)
	}

	// liftToolbar extracts leading row(variant="toolbar") and preserves toolbar form field in drawn.
	root := &uiblock.Node{
		Kind: uiblock.KindColumn,
		Children: []*uiblock.Node{
			{
				ID:      "/tb",
				Kind:    uiblock.KindRow,
				Variant: "toolbar",
				Children: []*uiblock.Node{
					{ID: "/tb/filter", Kind: uiblock.KindInput, Value: "all", Placeholder: "Filter"},
				},
			},
			{ID: "/body/in", Kind: uiblock.KindInput, Value: "body", Placeholder: "Body"},
		},
	}
	tb, rest := liftToolbar(root)
	if tb == nil || rest == nil || len(rest.Children) != 1 {
		t.Fatalf("liftToolbar failed: tb=%v rest=%v", tb, rest)
	}
	sharedForm := newBlockForm()
	sharedForm.redraw()
	p := extUINewBlockProbe(t, func() ggui.Widget {
		return widget.Column(
			renderUIBlockTree("tb-ext", "main", root, tb, sharedForm, nil),
			renderUIBlockTree("tb-ext", "main", root, rest, sharedForm, nil),
		)
	}, ggui.Sz(800, 600))
	defer p.Close()
	p.Frame()

	gotShared := sharedForm.values()
	if gotShared["/tb/filter"] != "all" || gotShared["/body/in"] != "body" {
		t.Fatalf("toolbar field missing from sharedForm.values(): %v", gotShared)
	}
}

func TestExtAutoSplitBreakpoint(t *testing.T) {
	root := &uiblock.Node{
		Kind: uiblock.KindColumn,
		Fill: true,
		Children: []*uiblock.Node{
			{
				ID:        "/sp",
				Kind:      uiblock.KindSplit,
				Direction: "auto",
				Children: []*uiblock.Node{
					{ID: "/left", Kind: uiblock.KindText, Text: "PaneLeft"},
					{ID: "/right", Kind: uiblock.KindText, Text: "PaneRight"},
				},
			},
		},
	}
	p := extUINewBlockProbe(t, func() ggui.Widget {
		return RenderUIBlock("split-ext", "main", root, nil)
	}, ggui.Sz(900, 500))
	defer p.Close()

	inspectLabel := func(lbl string) ggui.Rect {
		var r ggui.Rect
		p.OnInspect(func(f *inspect.Frame) {
			f.DescribeAll()
			for _, n := range f.Nodes {
				if strings.Contains(n.Label, lbl) {
					r = n.Rect
					return
				}
			}
		})
		p.Frame()
		return r
	}

	lWide := inspectLabel("PaneLeft")
	rWide := inspectLabel("PaneRight")
	if lWide.Origin.X >= rWide.Origin.X {
		t.Fatalf("expected horizontal split at 900px: left=%+v right=%+v", lWide, rWide)
	}

	p.Resize(ggui.Sz(500, 500))
	p.Frame()
	lNarrow := inspectLabel("PaneLeft")
	rNarrow := inspectLabel("PaneRight")
	if lNarrow.Origin.Y >= rNarrow.Origin.Y {
		t.Fatalf("expected vertical split below 640px: left=%+v right=%+v", lNarrow, rNarrow)
	}
}

func TestExtSidebarPalettePageAndSettingsIntegration(t *testing.T) {
	const src = `package main

import (
	"context"
	"fmt"
	"os"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	fmt.Fprintln(os.Stderr, "unit-boot-log-marker")
	app := extsdk.New()
	app.View("main", extsdk.View{
		Chrome: "toolbar",
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Toolbar(u.Button("ToolbarBtn", extsdk.Key("tb"), extsdk.OnTap("noop"))),
				u.Text("BodyViewText"),
			).Fill(true)
		},
		On: extsdk.Handlers{"noop": func(ctx *extsdk.EventContext) error { return nil }},
	})
	app.Command("copy-cmd", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.Copy(ctx, "palette-copied-ok")
	})
	app.Command("badge-cmd", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{Text: "7", Tone: "accent", Tooltip: "7 alerts"})
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := `
[[contributes.view]]
id = "main"
title = "Demo View"
icon = "box"
chrome = "toolbar"

[[contributes.command]]
id = "copy-cmd"
title = "Demo: Copy Marker"

[[contributes.command]]
id = "badge-cmd"
title = "Demo: Set Badge"

[contributes.web_ui]
port = 8080
path = "/app"

[contributes.settings]
api_url = { type = "string", label = "API URL", default = "https://default.invalid" }
api_secret = { type = "secret", label = "API Secret" }
enabled_flag = { type = "boolean", label = "Enabled Flag", default = true }
count_num = { type = "number", label = "Count Num", default = 3 }
mode_sel = { type = "select", label = "Mode Sel", options = ["fast", "safe"], default = "fast" }
`
	mgr, dataDir, uiHost, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// Empty manager -> sidebar Extensions hidden.
	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1280, 860), nil)
	defer p.Close()
	if extUISemHasText(p.Semantics(), "Extensions") {
		t.Fatalf("expected sidebar Extensions hidden when 0 extensions installed:\n%s", p.Semantics())
	}

	// Install runnable binary extension + disabled extension + waiting extension.
	extUIInstallBinary(t, mgr, "demo-run", "Demo Run", src, extraTOML, ext.PermLocalProxy, ext.PermSecrets)
	if err := mgr.SetSetting("demo-run", "api_secret", "top-secret-never-shown"); err != nil {
		t.Fatalf("SetSetting(secret): %v", err)
	}

	extUIInstallBinary(t, mgr, "demo-dis", "Demo Disabled", src, `
[[contributes.view]]
id = "main"
title = "Disabled View"
icon = "box"
[[contributes.command]]
id = "copy-cmd"
title = "Disabled: HiddenCmd"
`)
	if err := mgr.Disable("demo-dis"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// Sidebar shows Extensions + Demo View; tapping navigates to PageExtension.
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Extensions") && extUISemHasText(sem, "Demo View")
	}, "sidebar Extensions group and Demo View visible")

	if extUISemHasText(p.Semantics(), "Disabled View") {
		t.Fatalf("disabled extension view should not appear in sidebar:\n%s", p.Semantics())
	}

	// Live badge update via RunCommand reflected in sidebar.
	if err := mgr.RunCommand("demo-run", "badge-cmd", "en"); err != nil {
		t.Fatalf("RunCommand(badge-cmd): %v", err)
	}
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "7")
	}, "sidebar live badge '7' reflected")

	// Tap sidebar tab -> navigates to PageExtension, renders body, toolbar, Dev · Unsandboxed, Open in Browser.
	extUITapSem(t, p, ggui.RoleTab, "Demo View")
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return ggui.Peek(m.Page) == ExtensionPage("demo-run", "main") &&
			extUISemHasText(sem, "BodyViewText") &&
			extUISemHasText(sem, "ToolbarBtn") &&
			extUISemHasText(sem, "Dev · Unsandboxed") &&
			extUISemHasText(sem, "Open in Browser")
	}, "PageExtension rendered with toolbar, Dev badge, and Open in Browser")

	// Disabled extension page shows disabled card.
	m.Go(ExtensionPage("demo-dis", "main"))
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Extension disabled") && extUISemHasText(sem, "Enable")
	}, "disabled extension card rendered")

	// Command palette shows runnable command and omits disabled command.
	m.PaletteQuery.Set("")
	m.PaletteOpen.Set(true)
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Demo: Copy Marker")
	}, "runnable extension command in palette")
	if extUISemHasText(p.Semantics(), "Disabled: HiddenCmd") {
		t.Fatalf("disabled extension command leaked into palette:\n%s", p.Semantics())
	}
	extUITapSem(t, p, "", "Demo: Copy Marker")
	extUISettle(t, p, func() bool {
		return slices.Contains(uiHost.snapshotCopied(), "palette-copied-ok")
	}, "palette command executed Copy")

	// Extensions page shows controls, masked secret, and logs.
	m.Go(PageExtensions)
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Developer Mode") &&
			extUISemHasText(sem, "Demo Run") &&
			extUISemHasText(sem, "API URL") &&
			extUISemHasText(sem, "API Secret") &&
			extUISemHasText(sem, "Enabled Flag") &&
			extUISemHasText(sem, "Count Num") &&
			extUISemHasText(sem, "Mode Sel")
	}, "Extensions page rendered")

	if strings.Contains(p.Semantics().String(), "top-secret-never-shown") {
		t.Fatalf("secret setting leaked plaintext into Settings UI:\n%s", p.Semantics())
	}
}

func TestExtModelKeepImagesLocalProxyHostEventsAndShutdown(t *testing.T) {
	t.Setenv(keychain.EnvVar, "file")
	mgr, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// KeepImages + LocalProxyRoutes merge + HostEvents + Keychain notice.
	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1000, 700), func(mod *Model) {
		mod.Specs.Set([]sandbox.Spec{{ID: "s1", Name: "s1", Image: "alpine:3.20"}})
	})
	defer p.Close()

	// Verify ExtensionSecrets shows file fallback when GVX_KEYCHAIN=file or note is set.
	mgrFallback, dataDir2, _, _ := extUINewManager(t, true, func(cfg *ext.Config) {
		cfg.Keychain = nil
		cfg.KeychainNote = "Secret Service unavailable"
	})
	m2, p2 := extUINewProbe(t, dataDir2, mgrFallback, ggui.Sz(1000, 700), nil)
	defer p2.Close()
	m2.Go(PageExtensions)
	extUISettle(t, p2, func() bool {
		return extUISemHasText(p2.Semantics(), "Secret Service unavailable")
	}, "keychain fallback notice shown on Extensions page")

	// sandboxKeepImages returns model images without crashing when extMgr has none.
	if imgs := m.sandboxKeepImages(); !slices.Contains(imgs, "alpine:3.20") {
		t.Fatalf("sandboxKeepImages = %v, want alpine:3.20", imgs)
	}

	// syncLocalProxyWith merges extension routes without duplicates.
	extUIInstallBinary(t, mgr, "proxy-ext", "Proxy Ext", `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, u *extsdk.UI) extsdk.Node { return u.Text("ok") }})
	_ = app.Run(context.Background())
}
`, extUIMinimalViewTOML+"\n[contributes.web_ui]\nport = 8080\npath = \"/ui\"\n", ext.PermLocalProxy)
	m.syncLocalProxy(false)
	lpState := ggui.Peek(m.LocalProxy)
	wantHost := localproxy.BuildHost("proxy-ext")
	if !slices.ContainsFunc(lpState.Routes, func(r localproxy.Route) bool { return r.Host == wantHost }) {
		t.Fatalf("expected LocalProxy routes to contain %q, got %+v", wantHost, lpState.Routes)
	}

	// SetExtensionManager(nil) and CloseExtensions are nil-safe, idempotent, and fast.
	var nilModel *Model
	nilModel.SetExtensionManager(nil)
	nilModel.CloseExtensions(8 * time.Second)

	start := time.Now()
	m.CloseExtensions(8 * time.Second)
	m.CloseExtensions(8 * time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("CloseExtensions took too long: %v", elapsed)
	}
	m.SetExtensionManager(nil)

	// Host events sent on followExtensions without panicking.
	m3, p3 := extUINewProbe(t, dataDir, mgr, ggui.Sz(900, 600), func(mod *Model) {
		mod.followExtensions()
	})
	defer p3.Close()
	m3.UpdateSettings(func(s *store.Settings) {
		s.Theme = "dark"
		s.Language = LangKorean
	})
	p3.Frame()
}

func TestExtPageStateCardsAndDeveloperModeSwitch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mgr, dataDir, _, _ := extUINewManager(t, false, func(cfg *ext.Config) {
		cfg.ImageBuilder = &extUIMockBuilder{started: started, release: release}
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// Waiting sandbox extension.
	waitDir := filepath.Join(t.TempDir(), "wait-sbx")
	if err := os.MkdirAll(waitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	waitTOML := `schema_version = 1
id = "wait-sbx"
version = "0.1.0"
publisher = "acme"
name = "Wait Sandbox"
description = "Waiting fixture"
icon = "box"
permissions = ["storage"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`
	if err := os.WriteFile(filepath.Join(waitDir, "extension.toml"), []byte(waitTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(waitDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(waitDir); err != nil {
		t.Fatalf("InstallLocal(wait-sbx): %v", err)
	}

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1200, 840), nil)
	defer p.Close()

	// Waiting card.
	m.Go(ExtensionPage("wait-sbx", "main"))
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Approval required") && extUISemHasText(sem, "Allow & Build")
	}, "waiting card rendered")

	// Building card while ImageBuilder is blocked.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.AllowAndBuild("wait-sbx")
	}()
	<-started
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Building") && extUISemHasText(sem, "Step 1/2")
	}, "building card with live BuildLogs rendered")
	close(release)
	wg.Wait()

	// Extensions page Developer Mode switch persists store.Settings.DeveloperMode.
	m.Go(PageExtensions)
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Developer Mode")
	}, "Extensions Developer Mode visible")
	extUITapSem(t, p, ggui.RoleSwitch, "Developer Mode")
	st := store.Store{Dir: dataDir}
	extUISettle(t, p, func() bool {
		saved, err := st.Settings()
		return err == nil && saved.DeveloperMode
	}, "DeveloperMode persisted to store.Settings")
}

func TestExtLabsToggleAndSidebarMenuBetweenLabsAndSettings(t *testing.T) {
	dataDir := t.TempDir()
	m, p := extUINewProbe(t, dataDir, nil, ggui.Sz(1280, 860), func(mod *Model) {
		mod.Offline = false
		mod.sup = &sandbox.Supervisor{DataDir: dataDir}
		mod.followExtensions()
	})
	defer p.Close()
	defer m.CloseExtensions(2 * time.Second)

	// 1. Default (Extensions disabled): footer shows Labs and Settings, NOT Extensions.
	p.Frame()
	if _, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Extensions"); ok {
		t.Fatalf("expected sidebar Extensions menu hidden when Labs toggle is off:\n%s", p.Semantics())
	}

	// 2. Enable Extensions in Labs -> persists store.Settings.Extensions, starts manager,
	//    and shows Extensions menu between Labs and Settings in sidebar footer.
	m.Go(PageLabs)
	extUISettle(t, p, func() bool {
		_, ok := extUIFindSemNode(p.Semantics(), ggui.RoleSwitch, "Enable extensions")
		return ok
	}, "Labs Enable extensions switch visible")
	extUITapSem(t, p, ggui.RoleSwitch, "Enable extensions")

	st := store.Store{Dir: dataDir}
	extUISettle(t, p, func() bool {
		saved, err := st.Settings()
		if err != nil || !saved.Extensions {
			return false
		}
		_, hasExtTab := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Extensions")
		return hasExtTab && m.extMgrPtr.Load() != nil
	}, "Extensions enabled, manager initialized, and sidebar Extensions menu shown")

	labsTab, ok1 := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Labs")
	extTab, ok2 := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Extensions")
	settingsTab, ok3 := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Settings")
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("missing footer tabs: labs=%v ext=%v settings=%v\n%s", ok1, ok2, ok3, p.Semantics())
	}
	if !(labsTab.Rect.Origin.Y < extTab.Rect.Origin.Y && extTab.Rect.Origin.Y < settingsTab.Rect.Origin.Y) {
		t.Fatalf("expected sidebar footer order Labs < Extensions < Settings, got Y=(%.1f, %.1f, %.1f)",
			labsTab.Rect.Origin.Y, extTab.Rect.Origin.Y, settingsTab.Rect.Origin.Y)
	}

	// 3. Tapping sidebar Extensions menu opens PageExtensions with install & Developer Mode UI.
	extUITapSem(t, p, ggui.RoleTab, "Extensions")
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return ggui.Peek(m.Page) == PageExtensions &&
			extUISemHasText(sem, "Developer Mode") &&
			extUISemHasText(sem, "Install from Git repository")
	}, "PageExtensions opened from sidebar")

	// 4. Settings page no longer embeds Developer Mode / Extensions install section.
	m.Go(PageSettings)
	extUISettle(t, p, func() bool {
		return ggui.Peek(m.Page) == PageSettings && extUISemHasText(p.Semantics(), "Appearance")
	}, "PageSettings rendered")
	if extUISemHasText(p.Semantics(), "Install from Git repository") {
		t.Fatalf("expected Extensions install UI moved out of Settings page:\n%s", p.Semantics())
	}

	// 5. Disabling Extensions while on PageExtensions closes owned manager, hides sidebar item,
	//    and navigates back to PageLabs.
	m.Go(PageExtensions)
	p.Frame()
	m.SetExtensionsEnabled(false)
	extUISettle(t, p, func() bool {
		_, hasExtTab := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Extensions")
		return ggui.Peek(m.Page) == PageLabs && !hasExtTab && m.extMgrPtr.Load() == nil
	}, "Extensions disabled, redirected to Labs, and sidebar menu hidden")
}

func TestExtModelStartUsesFakeKeychainUnderTesting(t *testing.T) {
	// Verify initExtensions selects &keychain.Fake{} under testing.Testing() and
	// uses DataDir (not ~/.gvx) without ever calling keychain.System().
	dataDir := t.TempDir()
	m, p := extUINewProbe(t, dataDir, nil, ggui.Sz(800, 600), nil)
	defer p.Close()
	m.Offline = false
	m.sup = &sandbox.Supervisor{DataDir: dataDir}
	m.initExtensions()
	defer m.CloseExtensions(2 * time.Second)

	mgr := m.extMgrPtr.Load()
	if mgr == nil {
		t.Fatal("expected initExtensions to construct *ext.Manager")
	}
	if got := mgr.SecretStoreStatus(); got.Store != "test keychain" && got.Store != "fake" && got.Store != "file" {
		t.Fatalf("SecretStoreStatus().Store = %q, want test keychain / fake / file under testing.Testing()", got.Store)
	}
}

func TestExtMainGoShutdownHookOrdering(t *testing.T) {
	mainPath := filepath.Join(extUIRepoRoot(t), "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var runDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "run" {
			runDecl = fd
			break
		}
	}
	if runDecl == nil || runDecl.Body == nil {
		t.Fatal("func run() not found in main.go")
	}
	callPos := map[string]token.Pos{}
	ast.Inspect(runDecl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "CloseTerminals", "CloseForwards", "CloseExtensions", "StopAll", "CloseSandboxEngine":
			if _, exists := callPos[sel.Sel.Name]; !exists {
				callPos[sel.Sel.Name] = call.Pos()
			}
		}
		return true
	})
	for _, req := range []string{"CloseTerminals", "CloseForwards", "CloseExtensions", "StopAll", "CloseSandboxEngine"} {
		if _, ok := callPos[req]; !ok {
			t.Fatalf("main.go run() missing %s", req)
		}
	}
	if callPos["CloseExtensions"] <= callPos["CloseTerminals"] ||
		callPos["CloseExtensions"] <= callPos["CloseForwards"] ||
		callPos["CloseExtensions"] >= callPos["StopAll"] ||
		callPos["CloseExtensions"] >= callPos["CloseSandboxEngine"] {
		t.Fatalf("invalid shutdown ordering in main.go: %+v", callPos)
	}
}

func TestExtHostEventDeliveryToRunningExtension(t *testing.T) {
	const src = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var mu sync.Mutex
	theme, locale := "", ""
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			mu.Lock()
			t, l := theme, locale
			mu.Unlock()
			return u.Column(u.Text(fmt.Sprintf("ev:%s:%s", t, l)))
		},
	})
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		var m map[string]string
		_ = json.Unmarshal(ev.Data, &m)
		mu.Lock()
		if ev.Type == "host.theme" {
			theme = m["theme"]
		}
		if ev.Type == "host.locale" {
			locale = m["locale"]
		}
		mu.Unlock()
		ctx.NotifyChanged("main")
	})
	_ = app.Run(context.Background())
}
`
	mgr, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	extUIInstallBinary(t, mgr, "ev-ext", "Event Ext", src, extUIMinimalViewTOML)

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(900, 600), func(mod *Model) {
		mod.followExtensions()
	})
	defer p.Close()

	m.Go(ExtensionPage("ev-ext", "main"))
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "ev:")
	}, "initial event view rendered")

	m.UpdateSettings(func(s *store.Settings) {
		s.Theme = "dark"
		s.Language = LangKorean
	})
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "ev:dark:ko")
	}, "host.theme and host.locale delivered to extension")
}

// Keep unused-import-free verification if atomic is used in future tests.
var _ = atomic.Bool{}
var _ = errors.New

func TestExtOnSubmitOnlyInputAndTextareaBlurNoToast(t *testing.T) {
	const src = `package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var mu sync.Mutex
	var log []string
	var lastOtherQ, lastOtherTA, lastOtherPassive, lastSubmit string
	record := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}

	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			mu.Lock()
			summary := fmt.Sprintf("events=%d q=%s ta=%s p=%s sub=%s", len(log), lastOtherQ, lastOtherTA, lastOtherPassive, lastSubmit)
			mu.Unlock()
			return u.Column(
				u.Text(summary),
				u.Input("", extsdk.Key("q"), extsdk.Placeholder("SubmitOnlyInput"), extsdk.OnSubmit("on_q_submit")),
				u.Textarea("", extsdk.Key("ta"), extsdk.Placeholder("SubmitOnlyArea"), extsdk.OnSubmit("on_ta_submit")),
				u.Input("", extsdk.Key("fail"), extsdk.Placeholder("FailOnChange"), extsdk.OnChange("on_fail_change")),
				u.Input("", extsdk.Key("passive"), extsdk.Placeholder("PassiveInput")),
				u.Button("OtherBtn", extsdk.Key("other"), extsdk.OnTap("on_other")),
			)
		},
		On: extsdk.Handlers{
			"on_q_submit": func(c *extsdk.EventContext) error {
				mu.Lock()
				lastSubmit = c.Kind + ":" + c.EventValue()
				mu.Unlock()
				record("submit:" + c.Kind + ":" + c.EventValue())
				c.NotifyChanged()
				return nil
			},
			"on_ta_submit": func(c *extsdk.EventContext) error {
				record("submit_ta:" + c.Kind + ":" + c.EventValue())
				c.NotifyChanged()
				return nil
			},
			"on_fail_change": func(c *extsdk.EventContext) error {
				record("fail_change:" + c.EventValue())
				return errors.New("intentional change failure")
			},
			"on_other": func(c *extsdk.EventContext) error {
				mu.Lock()
				lastOtherQ = c.FormValue("q")
				lastOtherTA = c.FormValue("ta")
				lastOtherPassive = c.FormValue("passive")
				mu.Unlock()
				record("other")
				c.NotifyChanged()
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	extUIInstallBinary(t, mgr, "submit-only-ext", "Submit Only Ext", src, extUIMinimalViewTOML)

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()

	m.Go(ExtensionPage("submit-only-ext", "main"))
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "OtherBtn")
	}, "submit-only-ext main view rendered")

	clickFieldAndType := func(placeholder, text string) {
		n, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, placeholder)
		if !ok {
			t.Fatalf("missing textfield %q\n%s", placeholder, p.Semantics())
		}
		p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
		p.Text(text)
		p.Frame()
	}

	// 1. OnSubmit-only Input blur without Enter -> zero error toast, value preserved.
	clickFieldAndType("SubmitOnlyInput", "hello-q")
	extUITapSem(t, p, ggui.RoleButton, "OtherBtn")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "q=hello-q")
	}, "OtherBtn observed /q after blur")
	// Allow any async EventChange error callback to flush via m.post.
	for range 10 {
		time.Sleep(15 * time.Millisecond)
		p.Frame()
	}
	if extUISemHasText(p.Semantics(), "Extension action failed") {
		t.Fatalf("unexpected error toast on OnSubmit-only Input blur:\n%s", p.Semantics())
	}

	// 2. OnSubmit-only Textarea blur without Enter -> zero error toast, value preserved.
	clickFieldAndType("SubmitOnlyArea", "hello-ta")
	extUITapSem(t, p, ggui.RoleButton, "OtherBtn")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "ta=hello-ta")
	}, "OtherBtn observed /ta after blur")
	for range 10 {
		time.Sleep(15 * time.Millisecond)
		p.Frame()
	}
	if extUISemHasText(p.Semantics(), "Extension action failed") {
		t.Fatalf("unexpected error toast on OnSubmit-only Textarea blur:\n%s", p.Semantics())
	}

	// 3. Pressing Enter on OnSubmit-only Input delivers EventSubmit.
	clickFieldAndType("SubmitOnlyInput", "!")
	p.Key("enter")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "events=3") && extUISemHasText(p.Semantics(), "sub=submit:hello-q!")
	}, "Enter delivered EventSubmit on SubmitOnlyInput")
	if extUISemHasText(p.Semantics(), "Extension action failed") {
		t.Fatalf("unexpected error toast on EventSubmit:\n%s", p.Semantics())
	}

	// 4. n.Change == false input emits no events on Enter or blur, while keeping Event.Values.
	clickFieldAndType("PassiveInput", "passive-val")
	p.Key("enter")
	p.Frame()
	extUITapSem(t, p, ggui.RoleButton, "OtherBtn")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "events=4") && extUISemHasText(p.Semantics(), "p=passive-val")
	}, "PassiveInput kept in Event.Values without emitting events")

	// 5. Failing OnChange handler still displays "Extension action failed" error toast.
	clickFieldAndType("FailOnChange", "boom")
	extUITapSem(t, p, ggui.RoleButton, "OtherBtn")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Extension action failed")
	}, "failing OnChange error toast shown on blur")
}

func TestExtConcurrentSetExtensionManagerAndCloseNoRaceNoLeak(t *testing.T) {
	mgr1, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr1.Start(context.Background()); err != nil {
		t.Fatalf("mgr1.Start: %v", err)
	}
	mgr2, _, _, _ := extUINewManager(t, true)
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}

	m, p := extUINewProbe(t, dataDir, nil, ggui.Sz(900, 600), func(mod *Model) {
		mod.followLocalProxy()
	})
	defer p.Close()

	// Let probe settle before recording baseline goroutine count.
	for range 5 {
		p.Frame()
	}
	baseGoroutines := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 15 {
				switch (id + i) % 4 {
				case 0:
					m.SetExtensionManager(mgr1)
				case 1:
					m.SetExtensionManager(mgr2)
				case 2:
					m.SetExtensionManager(nil)
				case 3:
					m.CloseExtensions(50 * time.Millisecond)
				}
			}
		}(g)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		p.Frame()
		select {
		case <-done:
			goto finished
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
finished:
	m.CloseExtensions(time.Second)
	m.SetExtensionManager(nil)
	p.Frame()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if runtime.NumGoroutine() <= baseGoroutines {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak after concurrent SetExtensionManager/CloseExtensions: base=%d after=%d", baseGoroutines, runtime.NumGoroutine())
}

type extUITickingMockBuilder struct {
	started chan struct{}
	release chan struct{}
	tickCh  chan string
	once    sync.Once
}

func (b *extUITickingMockBuilder) Build(ctx context.Context, _ ext.BuildRequest, onLine func(string)) (string, error) {
	if onLine != nil {
		onLine("Step 1/3 : FROM alpine:3.20")
	}
	b.once.Do(func() {
		if b.started != nil {
			close(b.started)
		}
	})
	for {
		select {
		case line := <-b.tickCh:
			if onLine != nil {
				onLine(line)
			}
		case <-b.release:
			return "sha256:built", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func TestExtSettingsCardStatePreservedAcrossBuildTicks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	tickCh := make(chan string, 16)
	builder := &extUITickingMockBuilder{started: started, release: release, tickCh: tickCh}

	mgr, dataDir, _, _ := extUINewManager(t, true, func(cfg *ext.Config) {
		cfg.ImageBuilder = builder
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// Extension A: binary extension with text & secret settings.
	extAExtra := extUIMinimalViewTOML + `
[contributes.settings]
endpoint = { type = "string", label = "CardA Endpoint", default = "http://initial" }
api_token = { type = "secret", label = "CardA Secret" }
`
	extUIInstallBinary(t, mgr, "aaa-card-ext", "Card Ext A", `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, u *extsdk.UI) extsdk.Node { return u.Text("ok") }})
	_ = app.Run(context.Background())
}
`, extAExtra)

	// Extension B: sandbox extension ready to enter StateBuilding.
	sbxDir := filepath.Join(t.TempDir(), "zzz-sbx-build")
	if err := os.MkdirAll(sbxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sbxTOML := `schema_version = 1
id = "zzz-sbx-build"
version = "0.1.0"
publisher = "acme"
name = "Build Ext B"
description = "Building fixture"
icon = "box"

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
port = 18901

[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`
	if err := os.WriteFile(filepath.Join(sbxDir, "extension.toml"), []byte(sbxTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sbxDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(sbxDir); err != nil {
		t.Fatalf("InstallLocal(zzz-sbx-build): %v", err)
	}

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1200, 900), nil)
	defer p.Close()
	defer close(release)

	m.Go(PageExtensions)
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "Card Ext A") && extUISemHasText(sem, "Build Ext B")
	}, "Extensions page cards for both extensions rendered")

	findNthByY := func(role ggui.Role, name string, index int) ggui.SemNode {
		var matches []ggui.SemNode
		for _, n := range p.Semantics().All() {
			if n.Role == role && strings.Contains(n.Name, name) {
				matches = append(matches, n)
			}
		}
		slices.SortFunc(matches, func(a, b ggui.SemNode) int {
			if a.Rect.Origin.Y < b.Rect.Origin.Y {
				return -1
			}
			if a.Rect.Origin.Y > b.Rect.Origin.Y {
				return 1
			}
			return 0
		})
		if index >= len(matches) {
			t.Fatalf("missing match #%d for role=%q name=%q (found %d)\n%s", index, role, name, len(matches), p.Semantics())
		}
		return matches[index]
	}

	// 1. Check "Purge data" on Card A (index 0).
	purgeA := findNthByY(ggui.RoleCheckbox, "Purge data", 0)
	p.PerformAction(purgeA.ID, ggui.Action{Kind: ggui.ActionPress})
	p.Frame()
	if got := findNthByY(ggui.RoleCheckbox, "Purge data", 0).Checked; got != 2 {
		t.Fatalf("expected Card A Purge data checked=2, got %d", got)
	}

	// 2. Open "Logs" on Card A (index 0).
	logsBtnA := findNthByY(ggui.RoleButton, "Logs", 0)
	p.PerformAction(logsBtnA.ID, ggui.Action{Kind: ggui.ActionPress})
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "No logs recorded yet.")
	}, "Card A Logs panel expanded")

	// 3. Type uncommitted text & secret drafts on Card A (without blurring/committing).
	if n, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "CardA Endpoint"); !ok {
		t.Fatalf("missing CardA Endpoint:\n%s", p.Semantics())
	} else {
		p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionSetValue, Text: "http://uncommitted-draft"})
	}
	if n, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "CardA Secret"); !ok {
		t.Fatalf("missing CardA Secret:\n%s", p.Semantics())
	} else {
		p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionSetValue, Text: "uncommitted-secret-draft"})
	}
	p.Frame()
	secBeforeNode, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "CardA Secret")
	if !ok || secBeforeNode.Value == "" || strings.Contains(secBeforeNode.Value, "uncommitted-secret-draft") {
		t.Fatalf("expected non-empty masked CardA Secret draft before build ticks, got ok=%v value=%q", ok, secBeforeNode.Value)
	}

	// 4. Transition Extension B to StateBuilding and emit multiple build ticks.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.AllowAndBuild("zzz-sbx-build")
	}()
	<-started

	for i := range 5 {
		tickCh <- fmt.Sprintf("Step 2/3 : RUN tick-%d", i)
		time.Sleep(70 * time.Millisecond)
		p.Frame()
	}
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Building")
	}, "Extension B entered StateBuilding")

	// Verify Card A state remained completely intact across build ticks!
	if got := findNthByY(ggui.RoleCheckbox, "Purge data", 0).Checked; got != 2 {
		t.Fatalf("Card A Purge data state reset across build ticks: got checked=%d, want 2", got)
	}
	if !extUISemHasText(p.Semantics(), "No logs recorded yet.") {
		t.Fatalf("Card A Logs panel collapsed during Extension B build ticks:\n%s", p.Semantics())
	}
	epNode, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "CardA Endpoint")
	if !ok || epNode.Value != "http://uncommitted-draft" {
		t.Fatalf("Card A uncommitted text draft lost across build ticks: node=%+v ok=%v", epNode, ok)
	}
	// Secret draft stays masked and non-empty in semantics (not cleared or leaked).
	secNode, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTextField, "CardA Secret")
	if !ok || secNode.Value != secBeforeNode.Value || strings.Contains(secNode.Value, "uncommitted-secret-draft") {
		t.Fatalf("Card A secret draft changed across build ticks: ok=%v before=%q after=%q", ok, secBeforeNode.Value, secNode.Value)
	}

	release <- struct{}{}
	wg.Wait()
}

func TestExtGraphCanvasPanZoomHitTestAndEdgeCases(t *testing.T) {
	var events []uiblock.Event
	nodes := []*uiblock.GraphNode{
		{ID: "n1", Label: "AlphaNode", Weight: 2},
		{ID: "n2", Label: "BetaNode", Weight: 1},
	}
	edges := []*uiblock.GraphEdge{
		{From: "n1", To: "n2", Label: "edge"},
	}
	root := &uiblock.Node{
		Kind: uiblock.KindScroll,
		ID:   "/sc",
		Children: []*uiblock.Node{
			{
				ID:         "/gr",
				Kind:       uiblock.KindGraph,
				Text:       "ServiceGraph",
				MaxHeight:  260,
				Change:     true,
				GraphNodes: nodes,
				GraphEdges: edges,
			},
			{ID: "/spacer", Kind: uiblock.KindText, Text: strings.Repeat("FooterLine\n", 40)},
		},
	}
	form := newBlockForm()
	form.redraw()

	p := extUINewBlockProbe(t, func() ggui.Widget {
		return renderUIBlockTree("graph-ext", "main", root, root, form, func(ev uiblock.Event) {
			events = append(events, ev)
		})
	}, ggui.Sz(800, 400))
	defer p.Close()
	p.Frame()

	st := form.graphState("/gr")

	// 1. Drag pan starting on a node (>4px) updates panX/panY and does NOT fire EventChange.
	alpha, ok := extUIFindSemNode(p.Semantics(), ggui.RoleButton, "AlphaNode")
	if !ok {
		t.Fatalf("missing AlphaNode semantic leaf:\n%s", p.Semantics())
	}
	startPt := ggui.Pt(alpha.Rect.Origin.X+alpha.Rect.Size.W/2, alpha.Rect.Origin.Y+alpha.Rect.Size.H/2)
	p.Press(startPt)
	p.Move(ggui.Pt(startPt.X+24, startPt.Y+16))
	p.Release(ggui.Pt(startPt.X+24, startPt.Y+16))
	p.Frame()
	if st.panX == 0 || st.panY == 0 {
		t.Fatalf("expected drag pan to update panX/panY, got (%v, %v)", st.panX, st.panY)
	}
	if len(events) != 0 {
		t.Fatalf("drag pan should not emit selection EventChange, got %+v", events)
	}

	// 2. Clicking node without dragging fires EventChange with that node's ID.
	alphaAfter, ok := extUIFindSemNode(p.Semantics(), ggui.RoleButton, "AlphaNode")
	if !ok {
		t.Fatalf("missing AlphaNode after pan:\n%s", p.Semantics())
	}
	p.Click(ggui.Pt(alphaAfter.Rect.Origin.X+alphaAfter.Rect.Size.W/2, alphaAfter.Rect.Origin.Y+alphaAfter.Rect.Size.H/2))
	p.Frame()
	if len(events) != 1 || events[0].Kind != uiblock.EventChange || events[0].Value != "n1" {
		t.Fatalf("expected click on AlphaNode to emit EventChange(\"n1\"), got %+v", events)
	}

	// 3. Plain Scroll passes through to parent KindScroll without changing st.zoom.
	grp, ok := extUIFindSemNode(p.Semantics(), ggui.RoleGroup, "ServiceGraph")
	if !ok {
		t.Fatalf("missing ServiceGraph group:\n%s", p.Semantics())
	}
	center := ggui.Pt(grp.Rect.Origin.X+grp.Rect.Size.W/2, grp.Rect.Origin.Y+grp.Rect.Size.H/2)
	zoomBefore := st.zoom
	p.Scroll(center, ggui.Pt(0, -120))
	p.Frame()
	if st.zoom != zoomBefore {
		t.Fatalf("plain wheel scroll changed graph zoom from %v to %v", zoomBefore, st.zoom)
	}
	grpAfter, _ := extUIFindSemNode(p.Semantics(), ggui.RoleGroup, "ServiceGraph")
	if grpAfter.Rect.Origin.Y >= grp.Rect.Origin.Y {
		t.Fatalf("expected plain scroll to scroll parent container: beforeY=%v afterY=%v", grp.Rect.Origin.Y, grpAfter.Rect.Origin.Y)
	}

	// 4. Ctrl/Meta/Shift + PointerScroll zooms within [0.35, 3.0] and guards NaN/Inf.
	canvas := &extGraphCanvas{state: st}
	for range 30 {
		if !canvas.HandlePointer(ggui.PointerEvent{Kind: ggui.PointerScroll, Mods: ggui.Mods{Ctrl: true}, Scroll: ggui.Pt(0, -10)}) {
			t.Fatal("expected Ctrl+PointerScroll to be consumed")
		}
	}
	if st.zoom != 3.0 {
		t.Fatalf("expected max zoom clamp 3.0, got %v", st.zoom)
	}
	for range 40 {
		canvas.HandlePointer(ggui.PointerEvent{Kind: ggui.PointerScroll, Mods: ggui.Mods{Meta: true}, Scroll: ggui.Pt(0, 10)})
	}
	if st.zoom != 0.35 {
		t.Fatalf("expected min zoom clamp 0.35, got %v", st.zoom)
	}
	canvas.HandlePointer(ggui.PointerEvent{Kind: ggui.PointerScroll, Mods: ggui.Mods{Shift: true}, Scroll: ggui.Pt(0, math.NaN())})
	canvas.HandlePointer(ggui.PointerEvent{Kind: ggui.PointerScroll, Mods: ggui.Mods{Ctrl: true}, Scroll: ggui.Pt(0, math.Inf(1))})
	if math.IsNaN(st.zoom) || math.IsInf(st.zoom, 0) {
		t.Fatalf("NaN/Inf scroll poisoned zoom: %v", st.zoom)
	}
	if got := clampFloat(math.NaN(), 0.35, 3.0); got != 0.35 {
		t.Fatalf("clampFloat(NaN, 0.35, 3.0) = %v, want 0.35", got)
	}

	// 5. Zero-bounds resilience and nil/duplicate nodes do not panic or invalidate cached layout.
	canvas.hitTestNode(ggui.Rect{}, ggui.Pt(0, 0))
	dupNodes := []*uiblock.GraphNode{
		nil,
		{ID: "dup", Label: "Dup1", Weight: math.NaN()},
		{ID: "dup", Label: "Dup2", Weight: 2},
	}
	dupRoot := &uiblock.Node{
		ID:         "/dup",
		Kind:       uiblock.KindGraph,
		GraphNodes: dupNodes,
	}
	dupForm := newBlockForm()
	dupForm.redraw()
	_ = renderUIBlockTree("dup-ext", "main", dupRoot, dupRoot, dupForm, nil)
	dupSt := dupForm.graphState("/dup")
	dupPosBefore := dupSt.layout.pos
	if dupPosBefore == nil {
		t.Fatal("expected non-nil pos map after first layout")
	}
	dupForm.redraw()
	_ = renderUIBlockTree("dup-ext", "main", dupRoot, dupRoot, dupForm, nil)
	// Same signature and non-nil pos map -> layout was not recomputed (same map pointer/entry).
	dupPosBefore["__sentinel"] = ggui.Pt(99, 99)
	dupForm.redraw()
	_ = renderUIBlockTree("dup-ext", "main", dupRoot, dupRoot, dupForm, nil)
	if _, kept := dupForm.graphState("/dup").layout.pos["__sentinel"]; !kept {
		t.Fatal("expected graph layout cache to be preserved across rebuild with nil/duplicate nodes")
	}
}

func TestExtDagNodeVirtualScrollAnd1000PlusRows(t *testing.T) {
	makeRows := func(n int) []*uiblock.Row {
		rows := make([]*uiblock.Row, 0, n+2)
		for i := range n {
			var parents []string
			if i > 0 {
				parents = append(parents, fmt.Sprintf("r%d", i-1))
			}
			// Add branch merges & >8 lanes + a forward cycle edge on row 0.
			if i == 0 && n > 10 {
				parents = append(parents, "r5")
			}
			if i > 12 && i%10 == 0 {
				parents = append(parents, fmt.Sprintf("r%d", i-12))
			}
			rows = append(rows, &uiblock.Row{
				ID:      fmt.Sprintf("r%d", i),
				Parents: parents,
				Text:    fmt.Sprintf("Row %d", i),
				Detail:  fmt.Sprintf("commit-%d", i),
			})
		}
		if n > 10 {
			rows = append(rows, nil, &uiblock.Row{ID: "", Text: "EmptyIDRow"})
		}
		return rows
	}

	rowsSig := ggui.State(makeRows(1200))
	var events []uiblock.Event
	form := newBlockForm()

	p := extUINewBlockProbe(t, func() ggui.Widget {
		return ggui.View(rowsSig, func(rows []*uiblock.Row) ggui.Widget {
			form.redraw()
			root := &uiblock.Node{
				Kind: uiblock.KindColumn,
				Children: []*uiblock.Node{
					{
						ID:        "/dg",
						Kind:      uiblock.KindDag,
						MaxHeight: 320,
						Change:    true,
						Rows:      rows,
					},
				},
			}
			return renderUIBlockTree("dag-ext", "main", root, root, form, func(ev uiblock.Event) {
				events = append(events, ev)
			})
		})
	}, ggui.Sz(900, 600))
	defer p.Close()
	p.Frame()

	// 1. ItemExtent virtualization: row 0 present, row 900 absent initially.
	sem := p.Semantics()
	if !extUISemHasText(sem, "Row 0") {
		t.Fatalf("expected Row 0 visible initially:\n%s", sem)
	}
	if extUISemHasText(sem, "Row 900") {
		t.Fatalf("Row 900 should not be materialized before scrolling (virtualization broken)")
	}

	// 2. Scroll down across 1000+ rows, verify ds.offset > 0, click a scrolled row -> EventChange.
	for range 25 {
		p.Scroll(ggui.Pt(400, 160), ggui.Pt(0, -180))
	}
	p.Frame()
	ds := form.dags["/dg"]
	if ds == nil || ggui.Peek(ds.offset) <= 0 {
		t.Fatalf("expected positive dag scroll offset after scrolling, got %v", ds)
	}
	offsetBeforeUpdate := ggui.Peek(ds.offset)

	// Find a currently visible scrolled row (>0) in semantics and click it.
	var clickedID, clickedLabel string
	for _, n := range p.Semantics().All() {
		if n.Role == ggui.RoleButton && strings.HasPrefix(n.Name, "Row ") && n.Name != "Row 0" && !n.Offscreen && n.Rect.Size.H > 0 {
			clickedLabel = n.Name
			clickedID = "r" + strings.TrimPrefix(n.Name, "Row ")
			p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
			p.Frame()
			break
		}
	}
	if clickedID == "" {
		t.Fatalf("expected a scrolled row button in semantics:\n%s", p.Semantics())
	}
	if !slices.ContainsFunc(events, func(ev uiblock.Event) bool {
		return ev.Node == "/dg" && ev.Kind == uiblock.EventChange && ev.Value == clickedID
	}) {
		t.Fatalf("expected EventChange(%q) when clicking %q, got %+v", clickedID, clickedLabel, events)
	}

	// 3. Tree update preserves scroll offset.
	rowsSig.Set(makeRows(1200))
	p.Frame()
	if got := ggui.Peek(ds.offset); got != offsetBeforeUpdate {
		t.Fatalf("expected scroll offset %v preserved across update, got %v", offsetBeforeUpdate, got)
	}

	// 4. Shrinking 1200 -> 5 rows clamps scroll offset cleanly without panic.
	rowsSig.Set(makeRows(5))
	p.Frame()
	p.Frame()
	if !extUISemHasText(p.Semantics(), "Row 0") {
		t.Fatalf("expected Row 0 visible after shrinking to 5 rows:\n%s", p.Semantics())
	}
}

func TestExtPageDialogDismissedNotReopenedOnOtherExtUpdate(t *testing.T) {
	const srcA = `package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var mu sync.Mutex
	showDialog := true
	rev := 0

	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			mu.Lock()
			dlg := showDialog
			r := rev
			mu.Unlock()
			items := []any{
				u.Text(fmt.Sprintf("rev=%d", r)),
				u.Button("KeepDlgUpdate", extsdk.Key("keep"), extsdk.OnTap("keep")),
				u.Button("RemoveDlg", extsdk.Key("remove"), extsdk.OnTap("remove")),
				u.Button("ReopenDlg", extsdk.Key("reopen"), extsdk.OnTap("reopen")),
			}
			if dlg {
				items = append(items, u.Dialog("ConfirmAction",
					u.Text("DialogBodyMsg"),
					extsdk.Key("dlg"),
				))
			}
			return u.Column(items...)
		},
		On: extsdk.Handlers{
			"keep": func(c *extsdk.EventContext) error {
				mu.Lock()
				rev++
				mu.Unlock()
				c.NotifyChanged()
				return nil
			},
			"remove": func(c *extsdk.EventContext) error {
				mu.Lock()
				rev++
				showDialog = false
				mu.Unlock()
				c.NotifyChanged()
				return nil
			},
			"reopen": func(c *extsdk.EventContext) error {
				mu.Lock()
				rev++
				showDialog = true
				mu.Unlock()
				c.NotifyChanged()
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	const srcC = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Text("ext-c-running")
		},
	})
	_ = app.Run(context.Background())
}
`

	started := make(chan struct{})
	release := make(chan struct{})
	tickCh := make(chan string, 16)
	builder := &extUITickingMockBuilder{started: started, release: release, tickCh: tickCh}

	mgr, dataDir, _, _ := extUINewManager(t, true, func(cfg *ext.Config) {
		cfg.ImageBuilder = builder
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	extUIInstallBinary(t, mgr, "ext-a-dlg", "Ext A Dialog", srcA, extUIMinimalViewTOML)

	// Install Extension B (sandbox, stays in StateWaiting until AllowAndBuild).
	sbxDir := filepath.Join(t.TempDir(), "ext-b-build")
	if err := os.MkdirAll(sbxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sbxTOML := `schema_version = 1
id = "ext-b-build"
version = "0.1.0"
publisher = "acme"
name = "Ext B Build"
description = "Building fixture"
icon = "box"

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
port = 18901

[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`
	if err := os.WriteFile(filepath.Join(sbxDir, "extension.toml"), []byte(sbxTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sbxDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(sbxDir); err != nil {
		t.Fatalf("InstallLocal(ext-b-build): %v", err)
	}

	// Install Extension C (binary, without AllowAndBuild yet so it starts in StateWaiting).
	binC := extUIBuildSDKBinary(t, srcC)
	dirC := filepath.Join(t.TempDir(), "ext-c-bin")
	if err := os.MkdirAll(filepath.Join(dirC, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirC, "extension.toml"), []byte(extUIManifestBinary("ext-c-bin", "Ext C", extUIMinimalViewTOML)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirC, "bin", "ext"), binC, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(dirC); err != nil {
		t.Fatalf("InstallLocal(ext-c-bin): %v", err)
	}

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1200, 900), nil)
	defer p.Close()

	// 1. Navigate to Extension A and confirm KindDialog opens.
	m.Go(ExtensionPage("ext-a-dlg", "main"))
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		_, hasDlg := extUIFindSemNode(sem, ggui.RoleDialog, "ConfirmAction")
		return hasDlg && extUISemHasText(sem, "DialogBodyMsg") && extUISemHasText(sem, "rev=0")
	}, "Extension A dialog opened")

	// 2. Dismiss dialog via Escape and verify it disappears from semantics.
	p.Key("escape")
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		_, hasDlg := extUIFindSemNode(sem, ggui.RoleDialog, "ConfirmAction")
		return !hasDlg && !extUISemHasText(sem, "DialogBodyMsg")
	}, "Extension A dialog dismissed on Escape")

	// 3. Transition Extension B (StateWaiting -> StateBuilding -> finished) and Extension C (StateWaiting -> StateRunning).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.AllowAndBuild("ext-b-build")
	}()
	<-started
	tickCh <- "Step 2/3 : RUN echo build"
	extUISettle(t, p, func() bool {
		info, ok := findExtensionInfo(ggui.Peek(m.Extensions), "ext-b-build")
		return ok && info.State == ext.StateBuilding
	}, "Extension B entered StateBuilding in m.Extensions")
	close(release)
	wg.Wait()

	if err := mgr.AllowAndBuild("ext-c-bin"); err != nil {
		t.Fatalf("AllowAndBuild(ext-c-bin): %v", err)
	}
	extUISettle(t, p, func() bool {
		info, ok := findExtensionInfo(ggui.Peek(m.Extensions), "ext-c-bin")
		return ok && info.State == ext.StateRunning
	}, "Extension C entered StateRunning in m.Extensions")

	for range 5 {
		p.Frame()
	}
	if _, hasDlg := extUIFindSemNode(p.Semantics(), ggui.RoleDialog, "ConfirmAction"); hasDlg || extUISemHasText(p.Semantics(), "DialogBodyMsg") {
		t.Fatalf("dismissed KindDialog on Extension A reopened after unrelated extension updates:\n%s", p.Semantics())
	}

	// 4. Extension A emits an SSE tree update still containing /dlg -> dismissed dialog stays closed.
	extUITapSem(t, p, ggui.RoleButton, "KeepDlgUpdate")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "rev=1")
	}, "Extension A rendered SSE update rev=1")
	for range 3 {
		p.Frame()
	}
	if _, hasDlg := extUIFindSemNode(p.Semantics(), ggui.RoleDialog, "ConfirmAction"); hasDlg || extUISemHasText(p.Semantics(), "DialogBodyMsg") {
		t.Fatalf("dismissed KindDialog on Extension A reopened on SSE update still containing /dlg:\n%s", p.Semantics())
	}

	// 5. Extension A removes /dlg in an SSE update, then re-adds /dlg -> dialog opens cleanly again!
	extUITapSem(t, p, ggui.RoleButton, "RemoveDlg")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "rev=2")
	}, "Extension A rendered SSE update rev=2 without /dlg")

	extUITapSem(t, p, ggui.RoleButton, "ReopenDlg")
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		_, hasDlg := extUIFindSemNode(sem, ggui.RoleDialog, "ConfirmAction")
		return extUISemHasText(sem, "rev=3") && hasDlg && extUISemHasText(sem, "DialogBodyMsg")
	}, "Extension A reopened dialog cleanly after /dlg was removed and re-added")
}

func TestExtUI_UninstallDropsViewSessionState(t *testing.T) {
	mgr, dataDir, _, _ := extUINewManager(t, true)
	srcV1 := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Text("v1-banner", extsdk.Key("b")),
				ui.Input("secret", extsdk.Key("note")),
			)
		},
	})
	_ = app.Run(context.Background())
}`
	extUIInstallBinary(t, mgr, "sess-drop", "Session Drop", srcV1, "")

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1200, 800), nil)
	defer p.Close()
	m.Go(ExtensionPage("sess-drop", "main"))
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "v1-banner")
	}, "v1 view rendered")

	oldSess := m.extViews["sess-drop/main"]
	if oldSess == nil {
		t.Fatal("expected extViews entry for sess-drop/main")
	}
	oldSess.mu.Lock()
	oldForm := oldSess.form
	oldSess.err = "stale-error"
	oldSess.mu.Unlock()
	if oldForm == nil {
		t.Fatal("expected non-nil form on active session")
	}

	m.UninstallExtension("sess-drop", true)
	extUISettle(t, p, func() bool {
		m.extMu.Lock()
		_, stillPresent := m.extViews["sess-drop/main"]
		m.extMu.Unlock()
		return !stillPresent
	}, "UninstallExtension dropped sess-drop/main from m.extViews")

	oldSess.mu.Lock()
	gone := oldSess.gone
	oldTree := oldSess.tree
	oldErr := oldSess.err
	oldSess.mu.Unlock()
	if !gone || oldTree != nil || oldErr != "" {
		t.Fatalf("expected evicted session state cleared (gone=%v tree=%v err=%q)", gone, oldTree, oldErr)
	}
	m.extStreamMu.Lock()
	streamKey := m.extStreamKey
	m.extStreamMu.Unlock()
	if strings.HasPrefix(streamKey, "sess-drop/") {
		t.Fatalf("expected active stream key for uninstalled extension cleared, got %q", streamKey)
	}

	// Reinstall fresh v2 and verify no stale form/session survives across uninstall+reinstall.
	srcV2 := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Text("v2-fresh", extsdk.Key("b")),
				ui.Input("secret", extsdk.Key("note")),
			)
		},
	})
	_ = app.Run(context.Background())
}`
	extUIInstallBinary(t, mgr, "sess-drop", "Session Drop", srcV2, "")
	m.Go(ExtensionPage("sess-drop", "main"))
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "v2-fresh")
	}, "v2 fresh view rendered")

	newSess := m.extViews["sess-drop/main"]
	if newSess == nil || newSess == oldSess {
		t.Fatalf("expected new session instance after reinstall, got %p (old=%p)", newSess, oldSess)
	}
	newSess.mu.Lock()
	newForm := newSess.form
	newErr := newSess.err
	newSess.mu.Unlock()
	if newForm == nil || newForm == oldForm || newErr != "" {
		t.Fatalf("expected fresh form/session after reinstall (newForm=%p oldForm=%p err=%q)", newForm, oldForm, newErr)
	}
}

func TestExtUI_MarkdownOnLinkValidation(t *testing.T) {
	var opened []string
	orig := extMarkdownOpenURL
	extMarkdownOpenURL = func(raw string) error {
		opened = append(opened, raw)
		return nil
	}
	defer func() { extMarkdownOpenURL = orig }()

	var sent []uiblock.Event
	node := &uiblock.Node{
		ID:     "md-1",
		Kind:   uiblock.KindMarkdown,
		Change: true,
		Text:   "[cred](https://user:pass@evil.example/a) [ctrl](https://evil.example/%00) [js](javascript:alert(1)) [ok](https://example.com/docs) [upper](HTTPS://example.com/upper)",
	}
	p := extUINewBlockProbe(t, func() ggui.Widget {
		return RenderUIBlock("md-ext", "main", node, func(ev uiblock.Event) {
			sent = append(sent, ev)
		})
	}, ggui.Sz(600, 300))
	defer p.Close()
	p.Frame()

	for _, tc := range []struct {
		dest      string
		wantOpen  bool
		wantEvent bool
	}{
		{"https://user:pass@evil.example/a", false, false},
		{"https://evil.example/\x00bad", false, false},
		{"HTTP://user:pass@evil.example/", false, false},
		{"https://example.com/docs", true, false},
		{"HTTPS://example.com/upper", true, false},
		{"#local-anchor", false, true},
	} {
		opened = nil
		sent = nil
		handleExtMarkdownLink(tc.dest, true, func(val string) {
			sent = append(sent, uiblock.Event{Node: "md", Kind: uiblock.EventChange, Value: val})
		})
		if (len(opened) > 0) != tc.wantOpen {
			t.Fatalf("dest=%q opened=%v, wantOpen=%v", tc.dest, opened, tc.wantOpen)
		}
		if (len(sent) > 0) != tc.wantEvent {
			t.Fatalf("dest=%q sent=%v, wantEvent=%v", tc.dest, sent, tc.wantEvent)
		}
	}
}

func TestExtUI_InitExtensionsDoesNotCancelManagerContext(t *testing.T) {
	// Regression test: initExtensions runs mgr.Start inside m.async, whose ctx
	// is canceled as soon as Start returns. Manager.Start must receive a
	// long-lived context so subsequent operations (git clone, build, etc.) do
	// not fail immediately with context.Canceled.
	dataDir := t.TempDir()
	m, p := extUINewProbe(t, dataDir, nil, ggui.Sz(800, 600), func(mod *Model) {
		mod.Offline = false
		mod.sup = &sandbox.Supervisor{DataDir: dataDir}
	})
	defer p.Close()
	defer m.CloseExtensions(2 * time.Second)

	m.initExtensions()
	extUISettle(t, p, func() bool {
		return m.extMgrPtr.Load() != nil
	}, "owned extension manager initialized")

	// Give the async Start callback time to finish and run its defer cancel().
	time.Sleep(100 * time.Millisecond)
	p.Frame()

	errCh := make(chan error, 1)
	m.InstallGitExtension("https://127.0.0.1:1/owner/repo", "", func(err error) {
		errCh <- err
	})

	var gotErr error
	extUISettle(t, p, func() bool {
		select {
		case gotErr = <-errCh:
			return true
		default:
			return false
		}
	}, "InstallGitExtension completed")

	if gotErr == nil {
		t.Fatal("expected error cloning unreachable loopback port, got nil")
	}
	if errors.Is(gotErr, context.Canceled) || strings.Contains(gotErr.Error(), "context canceled") {
		t.Fatalf("InstallGitExtension failed with canceled manager context: %v", gotErr)
	}
}

func TestExtUI_ToolbarHeaderNoOverlapAndBodyPadding(t *testing.T) {
	// Regression test: a wide lifted toolbar (chrome="toolbar" with tabs, pills,
	// and action buttons) must never squash or overlap the host Reload / Logs /
	// Settings buttons, even in narrow windows, and Fill views must keep outer
	// content padding so cards do not touch the window borders.
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Title:  "GitHub Actions",
		Chrome: "toolbar",
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Toolbar(
					u.Tabs("Local Workflows", []string{"Local Workflows", "Self-Hosted Runners", "History"},
						extsdk.Key("tabs"), extsdk.Size("small"), extsdk.OnChange("noop")),
					u.Pill("Docker Ready", extsdk.Tone("success"), extsdk.Variant("soft")),
					u.Pill("arm64", extsdk.Tone("muted"), extsdk.Variant("outline"), extsdk.Icon("cpu")),
					u.Button("Secrets (0)", extsdk.Key("sec"), extsdk.Icon("key"), extsdk.Size("small"), extsdk.Variant("outline"), extsdk.OnTap("noop")),
					u.Button("Refresh", extsdk.Key("ref"), extsdk.Icon("refresh-cw"), extsdk.Size("small"), extsdk.Variant("ghost"), extsdk.OnTap("noop")),
				),
				u.Split("auto",
					u.Card(extsdk.Key("left_card"), extsdk.Grow(1), u.Text("LeftPaneMarker")),
					u.Card(extsdk.Key("right_card"), extsdk.Grow(1), u.Text("RightPaneMarker")),
					extsdk.Key("split"), extsdk.Ratio(0.4), extsdk.Grow(1),
				),
			).Fill(true).Gap(8)
		},
		On: extsdk.Handlers{"noop": func(ctx *extsdk.EventContext) error { return nil }},
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := `
[[contributes.view]]
id = "main"
title = "GitHub Actions"
icon = "workflow"
chrome = "toolbar"
`
	mgr, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	extUIInstallBinary(t, mgr, "wide-tb", "GitHub Actions", src, extraTOML)

	for _, sz := range []ggui.Size{ggui.Sz(1280, 800), ggui.Sz(840, 700)} {
		t.Run(fmt.Sprintf("%.0fx%.0f", sz.W, sz.H), func(t *testing.T) {
			m, p := extUINewProbe(t, dataDir, mgr, sz, nil)
			defer p.Close()
			p.Frame()

			m.Go(ExtensionPage("wide-tb", "main"))
			extUISettle(t, p, func() bool {
				sem := p.Semantics()
				return extUISemHasText(sem, "LeftPaneMarker") && extUISemHasText(sem, "RightPaneMarker")
			}, "wide toolbar extension page rendered")

			sem := p.Semantics()
			reloadBtn, ok1 := extUIFindSemNode(sem, ggui.RoleButton, "Reload")
			logsBtn, ok2 := extUIFindSemNode(sem, ggui.RoleButton, "Logs")
			settingsBtn, ok3 := extUIFindSemNode(sem, ggui.RoleButton, "Settings")
			if !ok1 || !ok2 || !ok3 {
				t.Fatalf("missing host header buttons: reload=%v logs=%v settings=%v\n%s", ok1, ok2, ok3, sem)
			}
			for _, pair := range []struct {
				name string
				n    ggui.SemNode
			}{
				{"Reload", reloadBtn},
				{"Logs", logsBtn},
				{"Settings", settingsBtn},
			} {
				if pair.n.Full.Size.W < 20 || pair.n.Full.Size.H < 20 {
					t.Fatalf("host button %s squashed to %+v", pair.name, pair.n.Full)
				}
			}
			if reloadBtn.Full.Origin.X+reloadBtn.Full.Size.W > logsBtn.Full.Origin.X+0.5 {
				t.Fatalf("Reload (%+v) overlaps Logs (%+v)", reloadBtn.Full, logsBtn.Full)
			}
			if logsBtn.Full.Origin.X+logsBtn.Full.Size.W > settingsBtn.Full.Origin.X+0.5 {
				t.Fatalf("Logs (%+v) overlaps Settings (%+v)", logsBtn.Full, settingsBtn.Full)
			}
			if settingsBtn.Full.Origin.X+settingsBtn.Full.Size.W > sz.W+0.5 {
				t.Fatalf("Settings (%+v) overflows window width %.1f", settingsBtn.Full, sz.W)
			}

			// Verify lifted toolbar tabs stay on the same header band and do not overlap Reload.
			localTab, okTab := extUIFindSemNode(sem, ggui.RoleTab, "Local Workflows")
			if !okTab {
				t.Fatalf("missing lifted Local Workflows tab\n%s", sem)
			}
			dy := localTab.Full.Origin.Y - reloadBtn.Full.Origin.Y
			if dy < -40 || dy > 40 {
				t.Fatalf("lifted tab Y=%.1f not in header band with Reload Y=%.1f", localTab.Full.Origin.Y, reloadBtn.Full.Origin.Y)
			}
			if localTab.Full.Origin.X+localTab.Full.Size.W > reloadBtn.Full.Origin.X+0.5 {
				t.Fatalf("lifted tab (%+v) overlaps host Reload button (%+v)", localTab.Full, reloadBtn.Full)
			}

			// Verify header stays compact at the top of the page (never greedily expands vertically).
			if reloadBtn.Full.Origin.Y > 120 {
				t.Fatalf("header expanded vertically; Reload at Y=%.1f", reloadBtn.Full.Origin.Y)
			}

			// Verify body split pane is visible below the header with non-zero height and clean padding.
			leftMarker, okLeft := extUIFindSemNode(sem, "", "LeftPaneMarker")
			rightMarker, okRight := extUIFindSemNode(sem, "", "RightPaneMarker")
			if !okLeft || !okRight {
				t.Fatalf("missing pane markers: left=%v right=%v\n%s", okLeft, okRight, sem)
			}
			if leftMarker.Full.Size.H <= 0 || rightMarker.Full.Size.H <= 0 {
				t.Fatalf("body pane crushed to zero height: left=%+v right=%+v", leftMarker.Full, rightMarker.Full)
			}
			if leftMarker.Full.Origin.Y <= reloadBtn.Full.Origin.Y+reloadBtn.Full.Size.H {
				t.Fatalf("body content (%+v) overlaps header (%+v)", leftMarker.Full, reloadBtn.Full)
			}
			if leftMarker.Full.Origin.Y > 220 {
				t.Fatalf("body content pushed too far down: %+v", leftMarker.Full)
			}
			horizSeparated := rightMarker.Full.Origin.X > leftMarker.Full.Origin.X+leftMarker.Full.Size.W
			vertSeparated := rightMarker.Full.Origin.Y > leftMarker.Full.Origin.Y+leftMarker.Full.Size.H
			if !horizSeparated && !vertSeparated {
				t.Fatalf("right pane (%+v) overlaps left pane (%+v)", rightMarker.Full, leftMarker.Full)
			}
		})
	}
}

func TestExtUI_ToolbarTabsDoNotShiftAcrossTabSwitchesAndManifestI18n(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })

	const src = `package main

import (
	"context"
	"sync"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	tab := "Tab A"
	app := extsdk.New()
	app.View("main", extsdk.View{
		Chrome: "toolbar",
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			mu.Lock()
			cur := tab
			mu.Unlock()
			actionLabel := "Short"
			if cur == "Tab B" {
				actionLabel = "Much Longer Action Button Label"
			}
			return u.Column(
				u.Toolbar(
					u.Tabs(cur, []string{"Tab A", "Tab B"}, extsdk.Key("tabs"), extsdk.OnChange("set_tab")),
					u.Button(actionLabel, extsdk.Key("act"), extsdk.Variant("outline"), extsdk.OnTap("noop")),
				),
				u.Text("Active:"+cur),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"set_tab": func(ctx *extsdk.EventContext, val string) error {
				mu.Lock()
				tab = val
				mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
			"noop": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := `
[[contributes.view]]
id = "main"
title = "English View"
icon = "box"
chrome = "toolbar"

[[contributes.command]]
id = "run-cmd"
title = "English Command"

[contributes.settings]
api_url = { type = "string", label = "English Setting", description = "English setting desc" }

[i18n.ko]
name = "한국어 확장"
description = "한국어 확장 설명"
[i18n.ko.views]
main = "한국어 뷰"
[i18n.ko.commands]
run-cmd = "한국어 명령"
[i18n.ko.settings.api_url]
label = "한국어 설정"
description = "한국어 설정 설명"
`
	mgr, dataDir, _, _ := extUINewManager(t, true)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	extUIInstallBinary(t, mgr, "stable-tb", "English Ext", src, extraTOML)

	m, p := extUINewProbe(t, dataDir, mgr, ggui.Sz(1280, 800), nil)
	defer p.Close()
	p.Frame()

	m.Go(ExtensionPage("stable-tb", "main"))
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Active:Tab A")
	}, "Tab A rendered")

	tabABefore, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Tab A")
	if !ok {
		t.Fatalf("Tab A not found:\n%s", p.Semantics())
	}

	// Switch to Tab B (which renders a much wider trailing action button) and verify Tab A's X did not shift!
	extUITapSem(t, p, ggui.RoleTab, "Tab B")
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "Active:Tab B")
	}, "Tab B rendered")

	tabAAfter, ok := extUIFindSemNode(p.Semantics(), ggui.RoleTab, "Tab A")
	if !ok {
		t.Fatalf("Tab A not found after switch:\n%s", p.Semantics())
	}
	dx := tabAAfter.Full.Origin.X - tabABefore.Full.Origin.X
	if dx < -0.5 || dx > 0.5 {
		t.Fatalf("lifted toolbar tab shifted horizontally on tab switch: before=%.1f after=%.1f",
			tabABefore.Full.Origin.X, tabAAfter.Full.Origin.X)
	}

	// Switch app language to Korean -> verify manifest [i18n.ko] translates sidebar, header, settings, and palette!
	m.UpdateSettings(func(s *store.Settings) { s.Language = LangKorean })
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "한국어 뷰") && extUISemHasText(sem, "한국어 확장")
	}, "Korean manifest name and view title rendered")

	m.PaletteQuery.Set("")
	m.PaletteOpen.Set(true)
	extUISettle(t, p, func() bool {
		return extUISemHasText(p.Semantics(), "한국어 명령")
	}, "Korean manifest command rendered in palette")
	m.PaletteOpen.Set(false)

	m.Go(PageExtensions)
	extUISettle(t, p, func() bool {
		sem := p.Semantics()
		return extUISemHasText(sem, "한국어 확장 설명") &&
			extUISemHasText(sem, "한국어 설정") &&
			extUISemHasText(sem, "한국어 설정 설명")
	}, "Korean manifest description and setting labels rendered in Extensions page")
}

func TestExtUI_PickFolderCancelReturnsNoError(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(900, 600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	uiHost := modelUIHost{m: m}
	stub, ok := p.Dialogs().(*gguiruntime.StubFilePicker)
	if !ok {
		t.Fatalf("Probe.Dialogs() = %T, want *runtime.StubFilePicker", p.Dialogs())
	}

	// 1. Default StubFilePicker has no Paths -> returns runtime.ErrCanceled -> must report cancelled without error.
	picked, chosen, err := uiHost.PickFolder(context.Background(), "github-actions")
	if err != nil || chosen || picked != "" {
		t.Fatalf("PickFolder(cancel) = (%q, %v, %v), want (\"\", false, nil)", picked, chosen, err)
	}

	// 2. Explicit ErrCanceled wrapped -> still treated as normal cancel.
	stub.Err = fmt.Errorf("dialog: %w", gguiruntime.ErrCanceled)
	picked, chosen, err = uiHost.PickFolder(context.Background(), "github-actions")
	if err != nil || chosen || picked != "" {
		t.Fatalf("PickFolder(wrapped ErrCanceled) = (%q, %v, %v), want (\"\", false, nil)", picked, chosen, err)
	}

	// 3. Non-cancel dialog error still propagates.
	stub.Err = errors.New("dialog backend failed")
	if _, _, err := uiHost.PickFolder(context.Background(), "github-actions"); err == nil {
		t.Fatal("PickFolder(real error) returned nil, want error")
	}

	// 4. Valid folder selection succeeds.
	stub.Err = nil
	stub.Paths = []string{"/tmp/demo-repo"}
	picked, chosen, err = uiHost.PickFolder(context.Background(), "github-actions")
	if err != nil || !chosen || picked != "/tmp/demo-repo" {
		t.Fatalf("PickFolder(selected) = (%q, %v, %v), want (\"/tmp/demo-repo\", true, nil)", picked, chosen, err)
	}
}

func TestExtUI_FormSelectStretchesAndClickableCardTaps(t *testing.T) {
	u := &extsdk.UI{}
	raw, err := json.Marshal(u.Column(
		u.Card(
			extsdk.Key("form_card"),
			u.Select([]string{"ci.yml", "release.yml"}, "ci.yml", extsdk.Key("wf_sel"), extsdk.OnChange("pick")),
			u.Card(
				extsdk.Key("job_card"),
				extsdk.Variant("outline"),
				extsdk.OnTap("inspect_job", "lint"),
				u.Text("Lint Job", extsdk.Weight("semibold")),
			),
		),
	).Fill(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tree, err := uiblock.Read(raw)
	if err != nil {
		t.Fatalf("uiblock.Read: %v", err)
	}

	var tappedNode string
	form := newBlockForm()
	p := ggui.ProbeBuilder(func() ggui.Widget {
		form.redraw()
		w := renderUIBlockTree("test-ext", "main", tree.Root, tree.Root, form, func(ev uiblock.Event) {
			if ev.Kind == uiblock.EventTap {
				tappedNode = ev.Node
			}
		})
		form.pruneDialogs()
		return ggui.Provide(ggui.ReducedMotionKey, true, w)
	}, ggui.Sz(460, 400))
	defer p.Close()
	p.Frame()

	// Select inside a vertical card stretches beyond the old 260px cap.
	selNode, ok := extUIFindSemNode(p.Semantics(), ggui.RoleButton, "Choose")
	if !ok {
		selNode, ok = extUIFindSemNode(p.Semantics(), ggui.RoleCombobox, "Choose")
	}
	if !ok {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, "Choose") || strings.Contains(n.Value, "ci.yml") {
				selNode, ok = n, true
				break
			}
		}
	}
	if !ok {
		t.Fatalf("select node not found in semantics:\n%s", p.Semantics())
	}
	if selNode.Full.Size.W <= 300 {
		t.Fatalf("vertical form select width = %.1f, want > 300 (stretched full width)", selNode.Full.Size.W)
	}

	// Clickable Card fires EventTap when tapped.
	extUITapSem(t, p, ggui.RoleButton, "Lint Job")
	p.Frame()
	if tappedNode == "" {
		t.Fatalf("tapping clickable card did not dispatch EventTap:\n%s", p.Semantics())
	}
}
