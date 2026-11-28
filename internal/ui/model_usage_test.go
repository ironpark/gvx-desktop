package ui

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// composeContainers are a project shop of two, one of them stopped, a
// standalone container, and a project blog of one.
func composeContainers() []dockerapi.Container {
	in := func(project, service string) map[string]string {
		return map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service}
	}
	return []dockerapi.Container{
		{ID: "a1", Names: []string{"/shop-web-1"}, Image: "nginx:alpine", State: "running", Labels: in("shop", "web")},
		{ID: "b2", Names: []string{"/migrate"}, Image: "app:dev", State: "exited"},
		{ID: "c3", Names: []string{"/shop-redis-1"}, Image: "redis:alpine", State: "exited", Labels: in("shop", "redis")},
		{ID: "d4", Names: []string{"/blog-db-1"}, Image: "postgres:17", State: "running", Labels: in("blog", "db")},
	}
}

func rowsText(rows []containerRow) string {
	var out []string
	for _, r := range rows {
		if r.Head {
			running, total := projectCounts(r.Members)
			s := "[" + r.Project + " " + itoa(running) + "/" + itoa(total)
			if r.Folded {
				s += " folded"
			}
			out = append(out, s+"]")
			continue
		}
		out = append(out, r.C.Name())
	}
	return strings.Join(out, ",")
}

func TestContainerRows(t *testing.T) {
	list := composeContainers()
	for _, c := range []struct {
		q      string
		folded map[string]bool
		want   string
	}{
		{"", nil, "migrate,[blog 1/1],blog-db-1,[shop 1/2],shop-web-1,shop-redis-1"},
		{"", map[string]bool{"shop": true}, "migrate,[blog 1/1],blog-db-1,[shop 1/2 folded]"},
		// A project shows while one of its containers is left, counts
		// them all, and auto-unfolds while searching so matches are visible.
		{"redis", nil, "[shop 1/2],shop-redis-1"},
		{"redis", map[string]bool{"shop": true}, "[shop 1/2],shop-redis-1"},
		{"SHOP", nil, "[shop 1/2],shop-web-1,shop-redis-1"},
		{"dev", nil, "migrate"},
		{"nothing", nil, ""},
	} {
		if got := rowsText(containerRows(list, c.q, c.folded)); got != c.want {
			t.Errorf("%q %v:\n%s\nwant\n%s", c.q, c.folded, got, c.want)
		}
	}
	rows := containerRows(list, "", nil)
	if k := rows[1].key(); k != "compose:blog" || rows[1].name() != "Compose project blog" || rows[2].key() != "d4" || rows[2].name() != "blog-db-1" {
		t.Errorf("keys %q %q, names %q %q", k, rows[2].key(), rows[1].name(), rows[2].name())
	}
}

func TestProjectTargets(t *testing.T) {
	shop := composeContainers()
	shop = []dockerapi.Container{shop[0], shop[2], {ID: "e5", Names: []string{"/shop-worker-1"}, State: "restarting"}}
	names := func(l []dockerapi.Container) string {
		var out []string
		for _, c := range l {
			out = append(out, c.Name())
		}
		return strings.Join(out, ",")
	}
	for action, want := range map[string]string{
		"start":   "shop-redis-1",
		"stop":    "shop-web-1,shop-worker-1",
		"restart": "shop-web-1,shop-redis-1,shop-worker-1",
		"remove":  "shop-web-1,shop-redis-1,shop-worker-1",
		"pause":   "",
	} {
		if got := names(projectTargets(action, shop)); got != want {
			t.Errorf("%s: %s, want %s", action, got, want)
		}
	}
}

func TestUsageTexts(t *testing.T) {
	for p, want := range map[float64]string{0: "0.0%", 0.04: "0.0%", 3.21: "3.2%", 9.94: "9.9%", 9.96: "10%", 123.4: "123%"} {
		if got := cpuText(p); got != want {
			t.Errorf("cpuText(%v) = %s, want %s", p, got, want)
		}
	}
	for m, want := range map[float64]string{0: "0m", 0.4: "<1m", 2.5: "2m", 14.07: "14m", 999: "999m", 1250: "1.25"} {
		if got := milliCPUText(m); got != want {
			t.Errorf("milliCPUText(%v) = %s, want %s", m, got, want)
		}
	}
	for s, want := range map[string]int64{"": 0, "512m": 512 << 20, "1g": 1 << 30, "1.5G": 3 << 29, "64k": 64 << 10, "4096": 4096, "100b": 100, "x": 0} {
		if got := memoryLimit(s); got != want {
			t.Errorf("memoryLimit(%q) = %d, want %d", s, got, want)
		}
	}

	if v := usageOf(Usage{}, false); v.Known || v.Line != "" || v.CPU != "—" {
		t.Errorf("unknown %+v", v)
	}
	v := usageOf(Usage{CPU: 12.34, HasCPU: true, Memory: 48 << 20, Limit: 1 << 30}, true)
	if !v.Known || v.CPU != "12%" || v.Mem != "48.0 MiB" || v.Share != 0.1234 || v.Line != "CPU 12% · memory 48.0 MiB / 1.0 GiB" {
		t.Errorf("known %+v", v)
	}
	if v := usageOf(Usage{CPU: 250, HasCPU: true, Memory: 1 << 20}, true); v.Share != 1 || v.Line != "CPU 250% · memory 1.0 MiB" {
		t.Errorf("past a CPU %+v", v)
	}
	if v := usageOf(Usage{Memory: 1 << 20}, true); !v.Known || v.CPU != "—" || v.Share != 0 {
		t.Errorf("memory alone %+v", v)
	}
	if v := podUsageOf(kubeapi.Usage{MilliCPU: 250, Memory: 30 << 20}, true); v.CPU != "250m" || v.Share != 0.25 || v.Line != "CPU 250m · memory 30.0 MiB" {
		t.Errorf("pod %+v", v)
	}

	shop := composeContainers()
	all := map[string]Usage{"a1": {CPU: 1, HasCPU: true, Memory: 10}, "c3": {Memory: 5}}
	if u, ok := projectUsage(shop, all); !ok || u.CPU != 1 || u.HasCPU || u.Memory != 15 {
		t.Errorf("a project with one CPU unknown: %+v %v", u, ok)
	}
	if _, ok := projectUsage(shop[1:2], all); ok {
		t.Error("none known")
	}
}

func TestPodUsesAndSetDetails(t *testing.T) {
	m, p, _ := kubeModel(t, true)
	var web kubeapi.PodMetrics
	web.Metadata = kubeapi.Meta{Namespace: "default", Name: "web"}
	web.Containers = make([]kubeapi.ContainerMetrics, 2)
	web.Containers[0].Name, web.Containers[0].Usage.CPU, web.Containers[0].Usage.Memory = "web", "2m", "6Mi"
	web.Containers[1].Name, web.Containers[1].Usage.CPU, web.Containers[1].Usage.Memory = "exporter", "1500000n", "10Mi"

	m.setDetails(clusterList{metrics: []kubeapi.PodMetrics{web}}, false, false, true)
	p.Frame()
	got := ggui.Peek(m.PodUsage)["default/web"]
	if got.Total.MilliCPU != 3.5 || got.Total.Memory != 16<<20 || got.Containers["exporter"].MilliCPU != 1.5 {
		t.Errorf("web %+v", got)
	}
	// Without metrics-server nothing is known, and nothing is said of it.
	m.setDetails(clusterList{metricsErr: errors.New("the server could not find the requested resource")}, false, false, true)
	if u := ggui.Peek(m.PodUsage); len(u) != 0 || u == nil {
		t.Errorf("no metrics-server: %v", u)
	}
	// Not asked for, what is known stays.
	m.PodUsage.Set(podUses([]kubeapi.PodMetrics{web}))
	m.setDetails(clusterList{}, false, false, false)
	if len(ggui.Peek(m.PodUsage)) != 1 {
		t.Error("cleared when not read")
	}
	m.clearCluster()
	if ggui.Peek(m.PodUsage) != nil {
		t.Error("kept past the cluster's stop")
	}
}

// usageModel is a model whose Docker Engine runs and answers on dockerd,
// over TCP, with the page given.
func usageModel(t *testing.T, dockerd http.HandlerFunc) (*Model, *ggui.Probe, sandbox.Spec) {
	t.Helper()
	srv := httptest.NewServer(dockerd)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	engine := recipes.DockerEngine()
	web := sandbox.Spec{ID: "w1", Name: "web", Image: "nginx:alpine", Memory: "512m"}
	if err := st.SaveSandboxes([]sandbox.Spec{engine, web}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.dockerBackend = "tcp://127.0.0.1:" + port
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{engine.ID: {Status: sandbox.Running}})
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p, web
}

func TestContainerUsageWhileThePageShows(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	var total uint64
	m, p, _ := usageModel(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		total += 1e8 // a tenth of a second of CPU between samples
		used := total
		mu.Unlock()
		switch r.URL.Path {
		case "/containers/a1/stats":
			// The engine's system time moves on by a second of two CPUs.
			_, _ = w.Write([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":` + itoa(int(used)) + `},"system_cpu_usage":` + itoa(int(used/1e8)*2e9) + `,"online_cpus":2},
				"precpu_stats":{"cpu_usage":{"total_usage":0}},"memory_stats":{"usage":3145728,"limit":1073741824,"stats":{"inactive_file":1048576}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: composeContainers()[:2]})
	took := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}

	m.LoadContainerUsage()
	p.Frame()
	if len(took()) != 0 || ggui.Peek(m.ContainerUsage) != nil {
		t.Fatalf("read off the page: %v", took())
	}
	// Set rather than gone to: Go would list the containers too.
	m.Page.Set(PageContainers)
	m.LoadContainerUsage()
	settle(t, p, func() bool { _, ok := ggui.Peek(m.ContainerUsage)["a1"]; return ok }, "the first reading")
	u := ggui.Peek(m.ContainerUsage)["a1"]
	if u.Memory != 2<<20 || u.Limit != 1<<30 || u.HasCPU {
		t.Errorf("first: %+v (the engine filled no precpu_stats)", u)
	}
	if got := took(); len(got) != 1 || got[0] != "/containers/a1/stats?stream=false" {
		t.Errorf("first asked %v: only the running container, with the engine's two samples", got)
	}
	m.LoadContainerUsage()
	settle(t, p, func() bool { return ggui.Peek(m.ContainerUsage)["a1"].HasCPU }, "the second reading")
	// 0.1 s of CPU over 2 s of the machine's two CPUs: 10% of one.
	if u := ggui.Peek(m.ContainerUsage)["a1"]; u.CPU < 9.99 || u.CPU > 10.01 {
		t.Errorf("second: %+v", u)
	}
	if got := took(); got[1] != "/containers/a1/stats?stream=false&one-shot=true" {
		t.Errorf("second asked %v", got)
	}

	m.Page.Set(PageDashboard)
	m.LoadContainerUsage()
	p.Frame()
	if ggui.Peek(m.ContainerUsage) != nil || m.containerStats != nil {
		t.Error("kept off the page")
	}
}

func TestSandboxUsageWhileThePageShows(t *testing.T) {
	m, p, web := usageModel(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	// The test's own process stands in for the sandbox's.
	states := ggui.Peek(m.States)
	states[web.ID] = sandbox.State{Status: sandbox.Running, PID: os.Getpid()}
	m.States.Set(states)
	m.Page.Set(PageImages)
	m.LoadSandboxUsage()
	p.Frame()
	if ggui.Peek(m.SandboxUsage) != nil {
		t.Fatal("sampled off the page")
	}
	m.Page.Set(PageSandboxes)
	m.LoadSandboxUsage()
	settle(t, p, func() bool { _, ok := ggui.Peek(m.SandboxUsage)[web.ID]; return ok }, "a sample")
	u := ggui.Peek(m.SandboxUsage)[web.ID]
	// Sampled twice at first, its CPU shows at once.
	if !u.HasCPU || u.Memory <= 0 || u.Limit != 512<<20 {
		t.Errorf("%+v", u)
	}
	if v := usageOf(u, true); !v.Known || !strings.HasSuffix(v.Line, " / 512.0 MiB") {
		t.Errorf("%+v", v)
	}
	m.Page.Set(PageSettings)
	m.LoadSandboxUsage()
	if ggui.Peek(m.SandboxUsage) != nil || m.sandboxSamples != nil {
		t.Error("kept off the page")
	}
}

// The table heads each Compose project with a row that folds its
// containers away, and shows what each uses.
func TestTheContainersTableGroupsProjects(t *testing.T) {
	m, p, _ := usageModel(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	m.Offline = true
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: composeContainers()})
	m.ContainerUsage.Set(map[string]Usage{"a1": {CPU: 3.2, HasCPU: true, Memory: 48 << 20}})
	m.Go(PageContainers)
	for range 4 {
		p.Frame()
	}
	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text {
				return true
			}
		}
		return false
	}
	if !shows("Compose project shop") || !shows("shop-web-1") || !shows("1 of 2 running") || !shows("3.2%") || !shows("48.0 MiB") {
		t.Fatalf("the table:\n%s", p.Semantics())
	}
	if !shows("Fold shop") {
		t.Fatalf("no fold button:\n%s", p.Semantics())
	}
	m.ToggleProject("shop")
	p.Frame()
	p.Frame()
	row := func(name string) bool {
		for _, n := range p.Semantics().Nodes(ggui.RoleRow) {
			if n.Name == name {
				return true
			}
		}
		return false
	}
	if row("shop-web-1") || !row("blog-db-1") || !shows("Unfold shop") || !row("Compose project shop") {
		t.Errorf("folded:\n%s", p.Semantics())
	}
	// Clicking a head leaves the container chosen and unfolds the project;
	// its fold button folds and unfolds it without double-toggling.
	m.SelectContainer("d4")
	p.Frame()
	f, ok := p.FindRole(ggui.RoleRow, "Compose project shop")
	if !ok {
		t.Fatalf("no head:\n%s", p.Semantics())
	}
	p.Click(f.Center())
	p.Frame()
	p.Frame()
	if got := ggui.Peek(m.Container).ID; got != "d4" {
		t.Errorf("chosen %s after the head", got)
	}
	if !row("shop-web-1") {
		t.Errorf("clicking folded head did not unfold shop:\n%s", p.Semantics())
	}
	p.Tap("Fold shop")
	p.Frame()
	p.Frame()
	if row("shop-web-1") {
		t.Errorf("Fold button did not fold shop:\n%s", p.Semantics())
	}
	p.Tap("Unfold shop")
	p.Frame()
	p.Frame()
	if !row("shop-web-1") {
		t.Errorf("unfolded:\n%s", p.Semantics())
	}
}
