package ui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func bulkTestModel(t *testing.T, specs []sandbox.Spec, dockerd http.HandlerFunc) (*Model, *ggui.Probe) {
	t.Helper()
	var port string
	if dockerd != nil {
		srv := httptest.NewServer(dockerd)
		t.Cleanup(srv.Close)
		_, port, _ = net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	}
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	if len(specs) > 0 {
		if err := st.SaveSandboxes(specs); err != nil {
			t.Fatal(err)
		}
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 900))
	p.Setup(func() {
		m = NewModel(st, dir)
		if port != "" {
			m.dockerBackend = "tcp://127.0.0.1:" + port
		} else {
			m.Offline = true
		}
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p
}

func tapSem(t *testing.T, p *ggui.Probe, role ggui.Role, name string) {
	t.Helper()
	n, ok := p.Semantics().Find(role, name)
	if !ok {
		t.Fatalf("node %s %q not found:\n%s", role, name, p.Semantics())
	}
	p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
	for range 3 {
		p.Frame()
	}
}

func waitUntil(t *testing.T, p *ggui.Probe, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestBulkSelectCachedImages(t *testing.T) {
	now := time.Now().Add(-time.Hour).Unix()
	var mu sync.Mutex
	images := []dockerapi.Image{
		{ID: "sha256:1111222233334444", RepoTags: []string{"nginx:alpine"}, Size: 32 << 20, Created: now},
		{ID: "sha256:5555666677778888", RepoTags: []string{"redis:alpine"}, Size: 24 << 20, Created: now},
	}
	var deleted []string
	m, p := bulkTestModel(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/images/"):
			target := strings.TrimPrefix(r.URL.Path, "/images/")
			mu.Lock()
			deleted = append(deleted, target)
			images = slices.DeleteFunc(images, func(img dockerapi.Image) bool {
				return img.ID == target || slices.Contains(img.RepoTags, target)
			})
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/images/json":
			mu.Lock()
			data, _ := json.Marshal(images)
			mu.Unlock()
			_, _ = w.Write(data)
		case r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})

	m.GoImages(ImagesGVX)
	waitUntil(t, p, "cached image checkboxes rendered", func() bool {
		_, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select nginx:alpine")
		return ok
	})

	// Checking a row selects it without opening the Image Explorer.
	tapSem(t, p, ggui.RoleCheckbox, "Select nginx:alpine")
	if ggui.Peek(m.ExploreOpen) {
		t.Fatal("tapping row checkbox should not open Image Explorer")
	}
	if !semShows(p, "1 selected") {
		t.Fatalf("expected bulk toolbar with '1 selected':\n%s", p.Semantics())
	}

	// Select all checks every visible image.
	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "2 selected") {
		t.Fatalf("expected '2 selected' after Select all:\n%s", p.Semantics())
	}

	// Clicking Remove… opens bulk confirmation and removes both images.
	tapSem(t, p, ggui.RoleButton, "Remove…")
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Remove 2 images?" {
		t.Fatalf("confirm = open:%v title:%q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmTitle))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	waitUntil(t, p, "both images deleted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(deleted) == 2
	})
	waitUntil(t, p, "selection cleared", func() bool {
		return !semShows(p, "2 selected") && !semShows(p, "1 selected")
	})
}

func TestBulkSelectContainersAndComposeGroup(t *testing.T) {
	var mu sync.Mutex
	containers := []dockerapi.Container{
		{ID: "c1", Names: []string{"/shop-web-1"}, Image: "nginx:alpine", State: "running", Labels: map[string]string{"com.docker.compose.project": "shop"}},
		{ID: "c2", Names: []string{"/shop-redis-1"}, Image: "redis:alpine", State: "exited", Labels: map[string]string{"com.docker.compose.project": "shop"}},
		{ID: "c3", Names: []string{"/solo"}, Image: "alpine:latest", State: "exited"},
	}
	var removed []string
	m, p := bulkTestModel(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			mu.Lock()
			removed = append(removed, id)
			containers = slices.DeleteFunc(containers, func(c dockerapi.Container) bool { return c.ID == id })
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/containers/json":
			mu.Lock()
			data, _ := json.Marshal(containers)
			mu.Unlock()
			_, _ = w.Write(data)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})

	m.Go(PageContainers)
	waitUntil(t, p, "container checkboxes rendered", func() bool {
		_, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select shop")
		return ok
	})

	// Clicking the Compose group checkbox selects all members without folding it.
	tapSem(t, p, ggui.RoleCheckbox, "Select shop")
	if ggui.Peek(m.ComposeFolded)["shop"] {
		t.Fatal("tapping Compose group checkbox folded the project")
	}
	if !semShows(p, "2 selected") {
		t.Fatalf("expected '2 selected' after selecting Compose project:\n%s", p.Semantics())
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select shop-web-1"); !ok || n.Checked != 2 {
		t.Fatalf("shop-web-1 checkbox = found:%v checked:%d", ok, n.Checked)
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select solo"); !ok || n.Checked == 2 {
		t.Fatalf("solo should stay unchecked: found:%v checked:%d", ok, n.Checked)
	}

	// Unchecking one project member leaves 1 selected and makes the group indeterminate.
	tapSem(t, p, ggui.RoleCheckbox, "Select shop-redis-1")
	if !semShows(p, "1 selected") {
		t.Fatalf("expected '1 selected' after unchecking member:\n%s", p.Semantics())
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select shop"); !ok || n.Checked == 2 || n.Checked == 0 {
		t.Fatalf("shop checkbox should be indeterminate/unchecked, got checked=%d", n.Checked)
	}

	// Clear selection hides the bulk bar.
	tapSem(t, p, ggui.RoleButton, "Clear selection")
	if semShows(p, "1 selected") {
		t.Fatal("Clear selection did not hide bulk bar")
	}

	// Select all selects all 3 containers and bulk Remove removes them all.
	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "3 selected") {
		t.Fatalf("expected '3 selected' after Select all:\n%s", p.Semantics())
	}
	tapSem(t, p, ggui.RoleButton, "Remove…")
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Remove 3 containers?" {
		t.Fatalf("confirm = open:%v title:%q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmTitle))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	waitUntil(t, p, "all 3 containers removed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(removed) == 3
	})
	waitUntil(t, p, "container selection cleared", func() bool {
		return !semShows(p, "3 selected")
	})
}

func TestBulkSelectSandboxesDelete(t *testing.T) {
	specs := []sandbox.Spec{
		{ID: "sb-1", Name: "alpha", Image: "alpine:latest", Memory: "512m"},
		{ID: "sb-2", Name: "beta", Image: "alpine:latest", Memory: "512m"},
	}
	m, p := bulkTestModel(t, specs, nil)
	m.Go(PageSandboxes)
	for range 3 {
		p.Frame()
	}

	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "2 selected") {
		t.Fatalf("expected '2 selected' on Sandboxes:\n%s", p.Semantics())
	}
	tapSem(t, p, ggui.RoleButton, "Delete…")
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Delete 2 sandboxes?" {
		t.Fatalf("confirm = open:%v title:%q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmTitle))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	waitUntil(t, p, "both sandboxes deleted", func() bool {
		return len(ggui.Peek(m.Specs)) == 0
	})
}

func TestBulkSelectVolumesAndNetworksDisableNonRemovable(t *testing.T) {
	m, p := bulkTestModel(t, nil, nil)

	// Volumes: in-use volume checkbox is disabled and excluded from Select all.
	m.Volumes.Set(Listing[rungv.Volume]{Loaded: true, Items: []rungv.Volume{
		{Name: "vol-free", Driver: "local", Size: "10 MB", InUse: false},
		{Name: "vol-busy", Driver: "local", Size: "20 MB", InUse: true},
	}})
	m.Go(PageVolumes)
	for range 3 {
		p.Frame()
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select vol-busy"); !ok || !n.Disabled {
		t.Fatalf("vol-busy checkbox should be disabled: found:%v disabled:%v", ok, n.Disabled)
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select vol-free"); !ok || n.Disabled {
		t.Fatalf("vol-free checkbox should be enabled: found:%v disabled:%v", ok, n.Disabled)
	}
	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "1 selected") {
		t.Fatalf("Select all on Volumes should select only unused volume:\n%s", p.Semantics())
	}

	// Networks: built-in and active-container networks are disabled and excluded from Select all.
	m.DockerNetworks.Set(Listing[dockerapi.Network]{Loaded: true, Items: testNetworks()})
	m.Go(PageNetworks)
	for range 3 {
		p.Frame()
	}
	for _, locked := range []string{"Select bridge", "Select shop_default"} {
		if n, ok := p.Semantics().Find(ggui.RoleCheckbox, locked); !ok || !n.Disabled {
			t.Fatalf("%q checkbox should be disabled: found:%v disabled:%v", locked, ok, n.Disabled)
		}
	}
	if n, ok := p.Semantics().Find(ggui.RoleCheckbox, "Select scratch"); !ok || n.Disabled {
		t.Fatalf("scratch checkbox should be enabled: found:%v disabled:%v", ok, n.Disabled)
	}
	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "1 selected") {
		t.Fatalf("Select all on Networks should select only removable network:\n%s", p.Semantics())
	}
}

func TestBulkSelectSnapshotsDelete(t *testing.T) {
	m, p := bulkTestModel(t, []sandbox.Spec{
		{ID: "sb-1", Name: "alpha", Image: "alpine:latest", Checkpoint: true},
	}, nil)

	snapRoot := t.TempDir()
	dirA := filepath.Join(snapRoot, "snap-a")
	dirB := filepath.Join(snapRoot, "snap-b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	m.Go(PageSnapshots)
	waitUntil(t, p, "initial snapshots scan settled", func() bool {
		return ggui.Peek(m.SnapshotsLoaded)
	})
	m.Snapshots.Set([]sandbox.Snapshot{
		{SandboxID: "sb-1", Name: "snap-a", Dir: dirA, Size: 4 << 20, Time: time.Now().Add(-2 * time.Hour)},
		{SandboxID: "sb-1", Name: "snap-b", Dir: dirB, Size: 6 << 20, Time: time.Now().Add(-time.Hour)},
	})
	for range 3 {
		p.Frame()
	}

	tapSem(t, p, ggui.RoleCheckbox, "Select all")
	if !semShows(p, "2 selected") {
		t.Fatalf("expected '2 selected' on Snapshots:\n%s", p.Semantics())
	}
	tapSem(t, p, ggui.RoleButton, "Delete…")
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Delete 2 snapshots?" {
		t.Fatalf("confirm = open:%v title:%q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmTitle))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	waitUntil(t, p, "snapshot bundles deleted from disk", func() bool {
		_, errA := os.Stat(dirA)
		_, errB := os.Stat(dirB)
		return os.IsNotExist(errA) && os.IsNotExist(errB)
	})
}
