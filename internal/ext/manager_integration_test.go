package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func buildTestSDKFixture(t *testing.T, dir, goCode string, goos, goarch string) string {
	t.Helper()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(srcPath, []byte(goCode), 0o600); err != nil {
		t.Fatalf("write fixture main.go: %v", err)
	}
	binName := "ext"
	if goos == "windows" {
		binName = "ext.exe"
	}
	outPath := filepath.Join(dir, "bin", binName)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", outPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fixture (%s/%s): %v\n%s", goos, goarch, err, out)
	}
	return outPath
}

func waitForExtState(t *testing.T, mgr *Manager, id string, timeout time.Duration, pred func(ExtensionInfo) bool) ExtensionInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ExtensionInfo
	for time.Now().Before(deadline) {
		for _, info := range mgr.List() {
			if info.ID == id {
				last = info
				if pred(info) {
					return info
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for extension %q state (last=%+v)", id, last)
	return ExtensionInfo{}
}

func TestManager_FullSDKExtensionLifecycleAndCleanup(t *testing.T) {
	dataDir := t.TempDir()
	pickedDir := newNonReservedTestDir(t)
	extDir := t.TempDir()

	// Start a local HTTP server to verify LocalProxyRoutes for [contributes.web_ui].
	webLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer webLn.Close()
	webPort := webLn.Addr().(*net.TCPAddr).Port

	const sdkSource = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	label := "initial"
	var capturedGesture string

	app := extsdk.New()
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		if ev.Type == "host.theme" {
			_ = os.WriteFile(filepath.Join(ctx.DataDir, "last_event.json"), ev.Data, 0o600)
		}
	})
	app.OnSettingsChanged(func(ctx *extsdk.Context, s map[string]any) error {
		b, _ := json.Marshal(s)
		return os.WriteFile(filepath.Join(ctx.DataDir, "settings_changed.json"), b, 0o600)
	})
	app.View("main", extsdk.View{
		Title: "Main",
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			cur := label
			mu.Unlock()
			return ui.Column(
				ui.Text(cur, extsdk.Key("status")),
				ui.Button("Copy", extsdk.Key("copyBtn"), extsdk.OnTap("doCopy")),
			)
		},
		On: extsdk.Handlers{
			"doCopy": func(ctx *extsdk.EventContext) error {
				capturedGesture = extsdk.GestureIDFromContext(ctx)
				if err := ctx.Host.Copy(ctx, "copied-from-view"); err != nil {
					return err
				}
				mu.Lock()
				label = "after-copy"
				mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
		},
	})
	app.Command("lifecycle-cmd", func(ctx *extsdk.CommandContext) error {
		hi, err := ctx.Host.Info(ctx)
		if err != nil {
			return err
		}
		if err := ctx.Host.StorageSet(ctx, "locale", hi.Locale); err != nil {
			return err
		}
		if err := ctx.Host.SecretSet(ctx, "api_key", "secret-val-123"); err != nil {
			return err
		}
		picked, ok, err := ctx.Host.PickFolder(ctx)
		if err != nil || !ok {
			return fmt.Errorf("pick folder failed: ok=%v err=%v", ok, err)
		}
		if err := ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{Text: "OK", Tone: "success"}); err != nil {
			return err
		}
		startFalse := false
		if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "eph-sbx",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &startFalse,
		}); err != nil {
			return err
		}
		if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "persist-sbx",
			Image:     "alpine:3.20",
			Ephemeral: false,
			Start:     &startFalse,
		}); err != nil {
			return err
		}
		// Replay previously captured gesture -> must be rejected!
		replayErr := ctx.Host.Copy(extsdk.WithGestureID(context.Background(), capturedGesture), "replay")
		out, _ := json.Marshal(map[string]any{
			"picked":          picked,
			"replay_rejected": extsdk.IsGestureRequired(replayErr),
		})
		return os.WriteFile(filepath.Join(ctx.DataDir, "cmd_result.json"), out, 0o600)
	})
	app.Tool("ping_tool", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		return map[string]any{"pong": true, "echo": args["msg"]}, nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSource, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}

	manifest := fmt.Sprintf(`schema_version = 1
id = "sdk-lifecycle"
version = "0.1.0"
publisher = "acme"
name = "SDK Lifecycle"
permissions = ["storage", "secrets", "sandboxes", "ui:pick-folder", "localproxy"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "lifecycle-cmd"
title = "Lifecycle Cmd"

[[contributes.tool]]
id = "ping_tool"
description = "Ping tool"

[contributes.settings.api_token]
type = "secret"
label = "API Token"

[contributes.web_ui]
port = %d
path = "/ui"
title = "Web UI"
`, binRel, webPort)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	ui := &fakeUIHost{pickPath: pickedDir, pickOK: true}
	kc := &keychain.Fake{}
	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      kc,
		DeveloperMode: func() bool { return true },
		Supervisor:    &sandbox.Supervisor{DataDir: dataDir},
		UIHost:        ui,
		HostInfo: func() HostInfo {
			return HostInfo{AppVersion: "0.10.0", Locale: "ko", Theme: "dark", DockerMode: "off"}
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	info, err := mgr.InstallLocal(extDir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if info.State != StateWaiting {
		t.Fatalf("expected StateWaiting, got %s", info.State)
	}

	if err := mgr.AllowAndBuild("sdk-lifecycle"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	info = waitForExtState(t, mgr, "sdk-lifecycle", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	if !info.Unsandboxed {
		t.Fatalf("expected Unsandboxed=true for binary runtime")
	}

	// Verify LocalProxyRoutes active.
	if routes := mgr.LocalProxyRoutes(); len(routes) != 1 || routes[0].GuestPort != webPort {
		t.Fatalf("expected 1 LocalProxyRoute for port %d, got %+v", webPort, routes)
	}

	// SubscribeView -> receive initial tree, send tap event -> receive patch + verify Copy gesture.
	subCtx, subCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer subCancel()
	ch, err := mgr.SubscribeView(subCtx, "sdk-lifecycle", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	upd1 := <-ch
	if upd1.Tree == nil || len(upd1.Tree.Children) < 2 || upd1.Tree.Children[0].Text != "initial" {
		t.Fatalf("unexpected initial tree update: %+v", upd1)
	}
	if err := mgr.SendViewEvent("sdk-lifecycle", "main", uiblock.Event{Node: "/copyBtn", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent: %v", err)
	}
	upd2 := <-ch
	if upd2.Tree == nil || upd2.Tree.Children[0].Text != "after-copy" {
		t.Fatalf("expected patched tree with after-copy, got %+v", upd2)
	}
	subCancel()

	ui.mu.Lock()
	copied := slices.Clone(ui.copied)
	ui.mu.Unlock()
	if !slices.Contains(copied, "copied-from-view") {
		t.Fatalf("expected copied-from-view in UIHost.CopyText, got %v", copied)
	}

	// RunCommand -> Info, Storage, Secret, PickFolder, Badge, Ephemeral vs Persistent sandboxes, Replay check.
	if err := mgr.RunCommand("sdk-lifecycle", "lifecycle-cmd", "ko"); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	cmdBytes, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "sdk-lifecycle"), "cmd_result.json"))
	if err != nil {
		t.Fatalf("read cmd_result.json: %v", err)
	}
	var cmdRes struct {
		Picked         string `json:"picked"`
		ReplayRejected bool   `json:"replay_rejected"`
	}
	if err := json.Unmarshal(cmdBytes, &cmdRes); err != nil {
		t.Fatal(err)
	}
	if cmdRes.Picked == "" || !cmdRes.ReplayRejected {
		t.Fatalf("unexpected cmd_result: %+v", cmdRes)
	}
	info = waitForExtState(t, mgr, "sdk-lifecycle", 2*time.Second, func(i ExtensionInfo) bool {
		return i.Badge.Text == "OK" && len(i.AllowedFolders) == 1
	})
	if info.Badge.Tone != "success" {
		t.Fatalf("unexpected badge: %+v", info.Badge)
	}

	// InvokeTool.
	toolOut, err := mgr.InvokeTool(ctx, "sdk-lifecycle", "ping_tool", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if !strings.Contains(string(toolOut), `"pong":true`) {
		t.Fatalf("unexpected toolOut: %s", toolOut)
	}

	// SetSetting (secret) -> masked in settings_changed.json.
	if err := mgr.SetSetting("sdk-lifecycle", "api_token", "top-secret"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	scBytes, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "sdk-lifecycle"), "settings_changed.json"))
	if err != nil {
		t.Fatalf("read settings_changed.json: %v", err)
	}
	if strings.Contains(string(scBytes), "top-secret") || !strings.Contains(string(scBytes), `"set":true`) {
		t.Fatalf("secret leaked or not masked in settings_changed.json: %s", scBytes)
	}

	// NotifyHostEvent -> host.theme delivered via /v1/events.
	mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "light"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "sdk-lifecycle"), "last_event.json")); err == nil && strings.Contains(string(b), "light") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	evBytes, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "sdk-lifecycle"), "last_event.json"))
	if err != nil || !strings.Contains(string(evBytes), "light") {
		t.Fatalf("expected host.theme event written, err=%v data=%s", err, evBytes)
	}

	// Close stops running extension and cleans ephemeral sandbox while keeping persistent sandbox.
	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, info := range mgr.List() {
		if info.State.IsActive() {
			t.Fatalf("expected extension %s stopped after Close, got state=%s", info.ID, info.State)
		}
	}
	remSpecs, err := (store.Store{Dir: dataDir}).Sandboxes()
	if err != nil {
		t.Fatal(err)
	}
	if len(remSpecs) != 1 || remSpecs[0].Name != "persist-sbx" {
		t.Fatalf("expected only persist-sbx remaining after Close, got %+v", remSpecs)
	}
}

func TestManager_CrashBackoffStateFailedAndIdleTimeoutWake(t *testing.T) {
	dataDir := t.TempDir()

	// 1. Crash 3x -> StateFailed; Enable resets crash count.
	crashExtDir := t.TempDir()
	const crashSrc = `package main
import (
	"context"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	marker := filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "stay_alive")
	if _, err := os.Stat(marker); err != nil {
		os.Exit(1)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("alive"))
	}})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, crashExtDir, crashSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	crashTOML := fmt.Sprintf(`schema_version = 1
id = "crash-ext"
version = "0.1.0"
publisher = "acme"
name = "Crash Ext"

[runtime]
type = "binary"
dev_entrypoint = %q
`, binRel)
	_ = os.WriteFile(filepath.Join(crashExtDir, ManifestName), []byte(crashTOML), 0o600)

	// 2. Idle timeout extension.
	idleExtDir := t.TempDir()
	const idleSrc = `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("idle-ready"))
	}})
	app.Command("wake", func(c *extsdk.CommandContext) error { return nil })
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, idleExtDir, idleSrc, runtime.GOOS, runtime.GOARCH)
	idleTOML := fmt.Sprintf(`schema_version = 1
id = "idle-ext"
version = "0.1.0"
publisher = "acme"
name = "Idle Ext"

[runtime]
type = "binary"
dev_entrypoint = %q
idle_timeout_sec = 1

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "wake"
title = "Wake"
`, binRel)
	_ = os.WriteFile(filepath.Join(idleExtDir, ManifestName), []byte(idleTOML), 0o600)

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr.Start(ctx)
	defer func() { _ = mgr.Close(5 * time.Second) }()

	// Crash test.
	_, _ = mgr.InstallLocal(crashExtDir)
	_ = mgr.AllowAndBuild("crash-ext")
	waitForExtState(t, mgr, "crash-ext", 15*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateFailed
	})
	// Now create stay_alive and verify Enable resets crash count and recovers to StateRunning.
	_ = os.MkdirAll(FilesDir(dataDir, "crash-ext"), 0o700)
	_ = os.WriteFile(filepath.Join(FilesDir(dataDir, "crash-ext"), "stay_alive"), []byte("1"), 0o600)
	if err := mgr.Enable("crash-ext"); err != nil {
		t.Fatalf("Enable after 3x crash: %v", err)
	}
	waitForExtState(t, mgr, "crash-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	// Idle timeout test: active SubscribeView pauses idle-stop.
	_, _ = mgr.InstallLocal(idleExtDir)
	if err := mgr.AllowAndBuild("idle-ext"); err != nil {
		t.Fatalf("AllowAndBuild(idle-ext): %v", err)
	}
	waitForExtState(t, mgr, "idle-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	subCtx, subCancel := context.WithCancel(ctx)
	ch, err := mgr.SubscribeView(subCtx, "idle-ext", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView(idle-ext): %v", err)
	}
	<-ch
	time.Sleep(1500 * time.Millisecond)
	info := waitForExtState(t, mgr, "idle-ext", 1*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	if info.State != StateRunning {
		t.Fatalf("active view stream must pause idle_timeout_sec, got %s", info.State)
	}

	// Unsubscribe -> should idle-stop after 1s.
	subCancel()
	waitForExtState(t, mgr, "idle-ext", 6*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateDiscovered
	})

	// RunCommand wakes idle-stopped extension transparently.
	if err := mgr.RunCommand("idle-ext", "wake", "en"); err != nil {
		t.Fatalf("RunCommand wake: %v", err)
	}
	waitForExtState(t, mgr, "idle-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	// Uninstall with purgeData=true removes data & keychain secrets.
	if err := mgr.Uninstall("crash-ext", true); err != nil {
		t.Fatalf("Uninstall(purge): %v", err)
	}
	if _, err := os.Stat(ExtensionDataDir(dataDir, "crash-ext")); !os.IsNotExist(err) {
		t.Fatalf("expected extension-data purged, stat err=%v", err)
	}
}

func TestExtRealRungvBuildAndSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real rungv + BuildKit test in -short mode")
	}
	rungvPath, err := rungv.Locate(os.Getenv("GVX_RUNGV"))
	if err != nil {
		t.Skipf("rungv unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Snapshot ~/.gvx before running so we prove nothing writes into ~/.gvx.
	home, _ := os.UserHomeDir()
	gvxDir := filepath.Join(home, ".gvx")
	beforeEntries, beforeErr := os.ReadDir(gvxDir)

	baseClient := &rungv.Client{Path: rungvPath}
	info, err := baseClient.Info(ctx)
	if err != nil || len(info.AvailableBackends) == 0 {
		t.Skipf("rungv backend unavailable: info=%+v err=%v", info, err)
	}
	feat := baseClient.Features(ctx)
	if !feat.UnixForward || !feat.Control {
		t.Skipf("rungv lacks UnixForward/Control features: %+v", feat)
	}

	// Verify buildkit & alpine images are cached (or skip if offline and not cached).
	imgs, err := baseClient.ImagesFull(ctx)
	if err != nil {
		t.Skipf("rungv images check failed: %v", err)
	}
	hasImage := func(want string) bool {
		for _, img := range imgs {
			if img.Repository+":"+img.Tag == want {
				return true
			}
		}
		return false
	}
	if !hasImage("moby/buildkit:v0.34.2") || !hasImage("alpine:3.20") {
		t.Skip("moby/buildkit:v0.34.2 and alpine:3.20 are not pre-cached locally")
	}

	dataDir := t.TempDir()
	engineDir := filepath.Join(t.TempDir(), "eng")
	supDir := filepath.Join(t.TempDir(), "sup")
	extDir := t.TempDir()
	allowedDir := newNonReservedTestDir(t)
	if err := os.WriteFile(filepath.Join(allowedDir, "hello.txt"), []byte("host-allowed"), 0o600); err != nil {
		t.Fatal(err)
	}

	egressLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen egress sentinel: %v", err)
	}
	defer egressLn.Close()
	var egressAccepted atomic.Int32
	go func() {
		for {
			c, aerr := egressLn.Accept()
			if aerr != nil {
				return
			}
			egressAccepted.Add(1)
			_ = c.Close()
		}
	}()
	egressPort := egressLn.Addr().(*net.TCPAddr).Port

	const sbxSDKSourceTemplate = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerReq(dockerHost, method, path string, body any) (int, string, error) {
	sockPath := strings.TrimPrefix(dockerHost, "unix://")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://docker"+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			label := "rungv-sbx-ready"
			if b, err := os.ReadFile(__ALLOWED_HELLO_PATH__); err == nil {
				label = "after-pick:" + strings.TrimSpace(string(b))
			}
			return ui.Column(ui.Text(label, extsdk.Key("status")))
		},
	})
	app.Command("verify-sandbox", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "in_sbx", "yes"); err != nil {
			return err
		}
		if err := ctx.Host.SecretSet(ctx, "sbx_sec", "sec-1"); err != nil {
			return err
		}
		if conn, derr := net.DialTimeout("tcp", fmt.Sprintf("10.0.2.2:%d", __EGRESS_PORT__), 600*time.Millisecond); derr == nil {
			_ = conn.Close()
			return errors.New("expected outbound tcp dial to be blocked without network permission")
		}
		pingCode, pingBody, err := dockerReq(ctx.DockerHost, http.MethodGet, "/_ping", nil)
		if err != nil || pingCode != 200 {
			return fmt.Errorf("docker /_ping failed: code=%d body=%q err=%v", pingCode, pingBody, err)
		}
		badCode, badBody, err := dockerReq(ctx.DockerHost, http.MethodPost, "/containers/create", map[string]any{
			"Image":      "alpine:3.20",
			"HostConfig": map[string]any{"Binds": []string{"/etc:/host-etc"}},
		})
		if err != nil || badCode != 403 {
			return fmt.Errorf("expected 403 on unapproved bind mount, got code=%d body=%q err=%v", badCode, badBody, err)
		}
		spoofCode, spoofBody, err := dockerReq(ctx.DockerHost, http.MethodPost, "/containers/create", map[string]any{
			"Image":  "alpine:3.20",
			"Labels": map[string]any{"io.gvx.extension": "evil-ext"},
		})
		if err != nil || spoofCode != 403 {
			return fmt.Errorf("expected 403 on spoofed label, got code=%d body=%q err=%v", spoofCode, spoofBody, err)
		}
		out, _ := json.Marshal(map[string]any{"ping": pingBody, "bad_code": badCode, "spoof_code": spoofCode})
		return os.WriteFile(filepath.Join(ctx.DataDir, "sbx_verify.json"), out, 0o600)
	})
	app.Command("pick", func(ctx *extsdk.CommandContext) error {
		p, ok, err := ctx.Host.PickFolder(ctx)
		if err != nil || !ok {
			return fmt.Errorf("pick failed: ok=%v err=%v", ok, err)
		}
		return os.WriteFile(filepath.Join(ctx.DataDir, "picked.txt"), []byte(p), 0o600)
	})
	_ = app.Run(context.Background())
}
`
	realAllowedDir, err := filepath.EvalSymlinks(allowedDir)
	if err != nil {
		realAllowedDir = allowedDir
	}
	sbxSDKSource := strings.ReplaceAll(sbxSDKSourceTemplate, "__EGRESS_PORT__", strconv.Itoa(egressPort))
	sbxSDKSource = strings.ReplaceAll(sbxSDKSource, "__ALLOWED_HELLO_PATH__", strconv.Quote(filepath.ToSlash(filepath.Join(realAllowedDir, "hello.txt"))))
	// Cross-compile Linux binary for the Alpine container.
	linuxBin := buildTestSDKFixture(t, extDir, sbxSDKSource, "linux", runtime.GOARCH)
	// Move out of bin/ext into ext-bin at root of extDir for Dockerfile COPY.
	extBinPath := filepath.Join(extDir, "ext-bin")
	if err := os.Rename(linuxBin, extBinPath); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(extDir, "bin"))

	dockerfile := `FROM alpine:3.20
COPY ext-bin /usr/local/bin/ext-bin
RUN chmod +x /usr/local/bin/ext-bin
ENTRYPOINT ["/usr/local/bin/ext-bin"]
`
	if err := os.WriteFile(filepath.Join(extDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	// Note: NO "network" permission -> runs with NATPolicyFile (egress deny) + IsolateLAN
	// (or Spec.Network="none" if NATPolicy is unsupported) and --unix-forward for Host/Docker sockets.
	toml := `schema_version = 1
id = "real-sbx-ext"
version = "0.1.0"
publisher = "acme"
name = "Real Sandbox Ext"
permissions = ["storage", "secrets", "docker", "ui:pick-folder"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "verify-sandbox"
title = "Verify Sandbox"

[[contributes.command]]
id = "pick"
title = "Pick"
`
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	eng := engine.New(engine.Config{
		Dir:                engineDir,
		Runner:             engine.NewRunner(func() *rungv.Client { return baseClient }, engineDir),
		Version:            "0.0.0-test",
		BuilderIdleTimeout: 30 * time.Second,
	})
	if err := eng.Open(ctx); err != nil {
		t.Fatalf("eng.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(10 * time.Second) })

	sup := &sandbox.Supervisor{
		Client:  func() *rungv.Client { return baseClient },
		DataDir: supDir,
	}
	t.Cleanup(func() { sup.StopAll(10 * time.Second) })

	builtTag := ExtensionImageTag("real-sbx-ext", "0.1.0", "")
	t.Cleanup(func() {
		_, _ = exec.Command(rungvPath, "rmi", "-f", builtTag).CombinedOutput()
	})

	ui := &fakeUIHost{pickPath: allowedDir, pickOK: true}
	mgr, err := NewManager(Config{
		DataDir:             dataDir,
		Keychain:            &keychain.Fake{},
		DeveloperMode:       func() bool { return false },
		Supervisor:          sup,
		DockerEngineHandler: eng.Handler,
		ImageBuilder:        NewEngineImageBuilder(func() http.Handler { return eng.Handler() }),
		ImagePuller:         baseClient,
		UIHost:              ui,
		HostInfo: func() HostInfo {
			return HostInfo{AppVersion: "0.10.0-test", DockerMode: "sandbox", DockerReady: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close(8 * time.Second) })

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("real-sbx-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v\nsandboxLogs=%v\nbuildLogs=%+v", err, mgr.Logs("real-sbx-ext"), mgr.BuildLogs("real-sbx-ext"))
	}
	waitForExtState(t, mgr, "real-sbx-ext", 20*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	if !slices.Contains(mgr.KeepImages(), builtTag) {
		t.Fatalf("expected KeepImages to contain %q, got %v", builtTag, mgr.KeepImages())
	}

	// Keep SubscribeView open across verify-sandbox AND ui:pick-folder restart inside real rungv sandbox.
	subCtx, subCancel := context.WithTimeout(ctx, 40*time.Second)
	defer subCancel()
	ch, err := mgr.SubscribeView(subCtx, "real-sbx-ext", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	upd := <-ch
	if upd.Tree == nil || upd.Tree.Children[0].Text != "rungv-sbx-ready" {
		t.Fatalf("unexpected sandbox view tree: %+v", upd)
	}

	// Verify Host API + Scoped Docker Proxy + NATPolicy egress denial inside real rungv sandbox.
	if err := mgr.RunCommand("real-sbx-ext", "verify-sandbox", "en"); err != nil {
		t.Fatalf("RunCommand(verify-sandbox): %v", err)
	}
	if got := egressAccepted.Load(); got != 0 {
		t.Fatalf("expected 0 connections accepted on host egress sentinel listener, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(FilesDir(dataDir, "real-sbx-ext"), "sbx_verify.json")); err != nil {
		t.Fatalf("expected sbx_verify.json written from sandbox: %v", err)
	}

	// Verify PickFolder restarts sandbox with allowed_folders mount AND open SubscribeView
	// transparently reconnects to the new ephemeral port + rotated token without 401 or hang!
	if err := mgr.RunCommand("real-sbx-ext", "pick", "en"); err != nil {
		t.Fatalf("RunCommand(pick): %v", err)
	}
	waitForExtState(t, mgr, "real-sbx-ext", 20*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning && len(i.AllowedFolders) == 1
	})

	deadline := time.Now().Add(20 * time.Second)
	gotAfterPick := false
	for time.Now().Before(deadline) {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatalf("SubscribeView channel closed prematurely during ui:pick-folder restart")
			}
			if u.Error != "" {
				t.Fatalf("SubscribeView emitted unexpected error across ui:pick-folder restart: %s", u.Error)
			}
			if u.Tree != nil && len(u.Tree.Children) > 0 && u.Tree.Children[0].Text == "after-pick:host-allowed" {
				gotAfterPick = true
			}
		case <-time.After(200 * time.Millisecond):
		}
		if gotAfterPick {
			break
		}
	}
	subCancel()
	if !gotAfterPick {
		t.Fatalf("expected open SubscribeView to stream post-restart tree \"after-pick:host-allowed\" after ui:pick-folder")
	}

	// Clean shutdown.
	if err := mgr.Close(8 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}

	// Verify ~/.gvx was NOT touched!
	afterEntries, afterErr := os.ReadDir(gvxDir)
	if (beforeErr != nil) != (afterErr != nil) || len(beforeEntries) != len(afterEntries) {
		t.Fatalf("~/.gvx directory changed during test! before=%d after=%d", len(beforeEntries), len(afterEntries))
	}
}

func TestManager_PostHealthThreeCrashesStateFailedAndEnableReset(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()

	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	dataDir := os.Getenv("GVX_EXT_DATA_DIR")
	countPath := filepath.Join(dataDir, "starts.txt")
	n := 0
	if b, err := os.ReadFile(countPath); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	_ = os.WriteFile(countPath, []byte(strconv.Itoa(n)), 0o600)

	if _, err := os.Stat(filepath.Join(dataDir, "stay_alive")); err != nil {
		go func() {
			time.Sleep(150 * time.Millisecond)
			os.Exit(2)
		}()
	}

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("launch-" + strconv.Itoa(n)))
		},
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "posthealth-crash"
version = "0.1.0"
publisher = "acme"
name = "PostHealth Crash Ext"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("posthealth-crash"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}

	// Must pass /v1/health 3 times, backoff restart twice (400ms + 800ms), and transition to StateFailed.
	waitForExtState(t, mgr, "posthealth-crash", 15*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateFailed
	})
	b, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "posthealth-crash"), "starts.txt"))
	if err != nil {
		t.Fatalf("read starts.txt: %v", err)
	}
	if strings.TrimSpace(string(b)) != "3" {
		t.Fatalf("expected 3 post-health starts before StateFailed, got %q", strings.TrimSpace(string(b)))
	}

	// Enable resets the crash counter and recovers to StateRunning.
	if err := os.WriteFile(filepath.Join(FilesDir(dataDir, "posthealth-crash"), "stay_alive"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("posthealth-crash"); err != nil {
		t.Fatalf("Enable after 3 post-health crashes: %v", err)
	}
	waitForExtState(t, mgr, "posthealth-crash", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	time.Sleep(300 * time.Millisecond)
	b2, _ := os.ReadFile(filepath.Join(FilesDir(dataDir, "posthealth-crash"), "starts.txt"))
	if strings.TrimSpace(string(b2)) != "4" {
		t.Fatalf("expected 4th start after Enable reset, got %q", strings.TrimSpace(string(b2)))
	}
}

func TestManager_StartConcurrentReadsNoRace(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()

	const src = `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("race-ok"))
	}})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "start-race-ext"
version = "0.1.0"
publisher = "acme"
name = "Start Race Ext"
permissions = ["localproxy"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[contributes.web_ui]
port = 18999
path = "/ui"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed approved extension on disk with manager 1, then close it.
	mgr1, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr1.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr1.AllowAndBuild("start-race-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	_ = mgr1.Close(5 * time.Second)

	// Start manager 2 while concurrently calling KeepImages, LocalProxyRoutes, and List.
	mgr2, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr2.Close(5 * time.Second) }()

	stopPoll := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopPoll:
					return
				default:
					_ = mgr2.KeepImages()
					_ = mgr2.LocalProxyRoutes()
					_ = mgr2.List()
				}
			}
		}()
	}

	if err := mgr2.Start(context.Background()); err != nil {
		close(stopPoll)
		wg.Wait()
		t.Fatalf("mgr2.Start: %v", err)
	}
	waitForExtState(t, mgr2, "start-race-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	close(stopPoll)
	wg.Wait()
}

func TestManager_SubscribeViewSurvivesRuntimeRestart(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()

	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	dataDir := os.Getenv("GVX_EXT_DATA_DIR")
	countPath := filepath.Join(dataDir, "starts.txt")
	n := 0
	if b, err := os.ReadFile(countPath); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	_ = os.WriteFile(countPath, []byte(strconv.Itoa(n)), 0o600)

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("launch-" + strconv.Itoa(n)))
		},
	})
	app.Command("crash", func(c *extsdk.CommandContext) error {
		go func() {
			time.Sleep(50 * time.Millisecond)
			os.Exit(2)
		}()
		return nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "sub-restart-ext"
version = "0.1.0"
publisher = "acme"
name = "Sub Restart Ext"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "crash"
title = "Crash"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AllowAndBuild("sub-restart-ext"); err != nil {
		t.Fatal(err)
	}
	waitForExtState(t, mgr, "sub-restart-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	subCtx, subCancel := context.WithTimeout(ctx, 20*time.Second)
	defer subCancel()
	ch, err := mgr.SubscribeView(subCtx, "sub-restart-ext", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}

	expectText := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case u, ok := <-ch:
				if !ok {
					t.Fatalf("SubscribeView closed before %q arrived", want)
				}
				if u.Error != "" {
					t.Fatalf("SubscribeView emitted unexpected error waiting for %q: %s", want, u.Error)
				}
				if u.Tree != nil && len(u.Tree.Children) > 0 && u.Tree.Children[0].Text == want {
					return
				}
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %q on open SubscribeView", want)
	}

	expectText("launch-1")

	// 1. Explicit restart via Rebuild (rotates extToken) -> open SubscribeView reconnects cleanly.
	if err := mgr.Rebuild("sub-restart-ext"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	expectText("launch-2")

	// 2. Crash recovery restart -> open SubscribeView reconnects cleanly after backoff.
	if err := mgr.RunCommand("sub-restart-ext", "crash", "en"); err != nil {
		t.Fatalf("RunCommand(crash): %v", err)
	}
	expectText("launch-3")

	// 3. Terminal Disable -> SubscribeView channel closes cleanly.
	if err := mgr.Disable("sub-restart-ext"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any buffered update, then expect close.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Fatal("expected SubscribeView channel to close after Disable")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for SubscribeView channel close after Disable")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SubscribeView channel close after Disable")
	}
}

type stubKeepBuilder struct{}

func (stubKeepBuilder) Build(_ context.Context, req BuildRequest, _ func(string)) (string, error) {
	return "sha256:stub-" + req.Tags[0], nil
}

type stubKeepPuller struct{}

func (stubKeepPuller) Pull(context.Context, string, func(string)) error { return nil }

func TestManager_KeepImagesExcludesRevokedAndDenied(t *testing.T) {
	dataDir := t.TempDir()
	localDir := t.TempDir()

	const manifest = `schema_version = 1
id = "keepimg-ext"
version = "0.1.0"
publisher = "acme"
name = "KeepImg Ext"
permissions = ["storage"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
`
	if err := os.WriteFile(filepath.Join(localDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:      dataDir,
		Keychain:     &keychain.Fake{},
		ImageBuilder: stubKeepBuilder{},
		ImagePuller:  stubKeepPuller{},
		UIHost:       &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(3 * time.Second) }()

	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	wantTag := ExtensionImageTag("keepimg-ext", "0.1.0", "")
	if slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("unapproved extension must not be in KeepImages: %v", mgr.KeepImages())
	}

	// AllowAndBuild (fails runtime start because Supervisor==nil, which is expected here) builds & approves image.
	_ = mgr.AllowAndBuild("keepimg-ext")
	if !slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("expected built image %q in KeepImages after AllowAndBuild, got %v", wantTag, mgr.KeepImages())
	}

	// RevokePermissions -> must exclude built image from KeepImages.
	if err := mgr.RevokePermissions("keepimg-ext"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	if slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("KeepImages must exclude %q after RevokePermissions, got %v", wantTag, mgr.KeepImages())
	}

	// Re-approve, then Deny -> must also exclude built image from KeepImages.
	_ = mgr.AllowAndBuild("keepimg-ext")
	if !slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("expected %q back in KeepImages after re-approval, got %v", wantTag, mgr.KeepImages())
	}
	if err := mgr.Deny("keepimg-ext"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("KeepImages must exclude %q after Deny, got %v", wantTag, mgr.KeepImages())
	}

	// Also verify prebuilt OCI image source follows the same rule.
	if _, err := mgr.InstallImage("ghcr.io/acme/pre-keep:1.0.0"); err != nil {
		t.Fatalf("InstallImage: %v", err)
	}
	const imgRef = "ghcr.io/acme/pre-keep:1.0.0"
	if slices.Contains(mgr.KeepImages(), imgRef) {
		t.Fatalf("unapproved image extension must not be in KeepImages: %v", mgr.KeepImages())
	}
	_ = mgr.AllowAndBuild("pre-keep")
	if !slices.Contains(mgr.KeepImages(), imgRef) {
		t.Fatalf("expected %q in KeepImages after AllowAndBuild, got %v", imgRef, mgr.KeepImages())
	}
	if err := mgr.RevokePermissions("pre-keep"); err != nil {
		t.Fatalf("RevokePermissions(pre-keep): %v", err)
	}
	if slices.Contains(mgr.KeepImages(), imgRef) {
		t.Fatalf("KeepImages must exclude %q after RevokePermissions, got %v", imgRef, mgr.KeepImages())
	}
}

type stubViewRuntime struct {
	client *extClient
	done   chan error
}

func (s *stubViewRuntime) Start(context.Context) error { return nil }
func (s *stubViewRuntime) Stop(time.Duration) error    { return nil }
func (s *stubViewRuntime) Client() *extClient          { return s.client }
func (s *stubViewRuntime) WebUITarget() string         { return "" }
func (s *stubViewRuntime) Done() <-chan error          { return s.done }
func (s *stubViewRuntime) Logs() []string              { return nil }

func TestManager_StartAndSubscribeViewCloseWaitGroupNoRace(t *testing.T) {
	// 1. Concurrent Start() + Close() hammering m.wg.Add vs m.wg.Wait.
	extDir := t.TempDir()
	const binSrc = `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("ok"))
	}})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, binSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "wg-race-ext"
version = "0.1.0"
publisher = "acme"
name = "WG Race Ext"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	seedDir := t.TempDir()
	seedMgr, err := NewManager(Config{
		DataDir:       seedDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedMgr.InstallLocal(extDir); err != nil {
		t.Fatal(err)
	}
	if err := seedMgr.grants.Approve("wg-race-ext", seedMgr.exts["wg-race-ext"].manifest, ""); err != nil {
		t.Fatal(err)
	}
	_ = seedMgr.Close(time.Second)

	for iter := 0; iter < 40; iter++ {
		mgr, err := NewManager(Config{
			DataDir:       seedDir,
			Keychain:      &keychain.Fake{},
			DeveloperMode: func() bool { return true },
			UIHost:        &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.Start(context.Background())
		}(iter%2 == 0)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.Close(2 * time.Second)
		}(iter%2 == 1)
		close(gate)
		wg.Wait()
	}

	// 2. Concurrent SubscribeView() + Close() (both before Start() when m.wg==0 and after Start()).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/views/main/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"text\":\"ok\"}]}\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, err := newExtClient(srv.URL, "tok-wg")
	if err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 60; iter++ {
		dataDir := t.TempDir()
		mgr, err := NewManager(Config{
			DataDir:       dataDir,
			Keychain:      &keychain.Fake{},
			DeveloperMode: func() bool { return true },
			UIHost:        &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if iter%3 == 0 {
			if err := mgr.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		}

		rec := mgr.newBaseRecord("sub-wg")
		rec.manifest = &Manifest{
			SchemaVersion: 1,
			ID:            "sub-wg",
			Version:       "0.1.0",
			Publisher:     "acme",
			Name:          "Sub WG",
			Runtime:       RuntimeDecl{Type: RuntimeBinary, DevEntrypoint: "bin/ext"},
			Views:         []ViewDecl{{ID: "main", Title: "Main"}},
		}
		rec.state = StateRunning
		rec.runtime = &stubViewRuntime{client: client, done: make(chan error)}
		mgr.mu.Lock()
		mgr.exts["sub-wg"] = rec
		mgr.mu.Unlock()

		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			ch, err := mgr.SubscribeView(context.Background(), "sub-wg", "main", "en")
			if err == nil {
				for range ch {
				}
			}
		}(iter%2 == 0)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.Close(2 * time.Second)
		}(iter%2 == 1)
		close(gate)
		wg.Wait()
	}
}

func TestManager_ConcurrentReinstallVsSetSettingViewCommandToolNoRace(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()

	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
		On: extsdk.Handlers{
			"tap": func(c *extsdk.EventContext) error { return nil },
		},
	})
	app.Command("cmd", func(c *extsdk.CommandContext) error { return nil })
	app.Tool("tool", func(c *extsdk.ToolContext, args map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "reinstall-race-ext"
version = "0.1.0"
publisher = "acme"
name = "Reinstall Race Ext"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "cmd"
title = "Cmd"

[[contributes.tool]]
id = "tool"
description = "Tool"

[contributes.settings.label]
type = "string"
label = "Label"
default = "init"

[contributes.settings.api_token]
type = "secret"
label = "API Token"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("reinstall-race-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "reinstall-race-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	const iters = 40
	var wg sync.WaitGroup

	// Goroutine 1: repeated re-install via InstallLocal (mutates rec.manifest/rec.source under opMu+mu).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := mgr.InstallLocal(extDir); err != nil {
				t.Errorf("InstallLocal(%d): %v", i, err)
				return
			}
			if i%5 == 0 {
				_ = mgr.AllowAndBuild("reinstall-race-ext")
			}
		}
	}()

	// Goroutine 2: concurrent SetSetting (non-secret and secret).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters*2; i++ {
			if err := mgr.SetSetting("reinstall-race-ext", "label", fmt.Sprintf("v-%d", i)); err != nil {
				t.Errorf("SetSetting(label, %d): %v", i, err)
				return
			}
			if err := mgr.SetSetting("reinstall-race-ext", "api_token", fmt.Sprintf("tok-%d", i)); err != nil {
				t.Errorf("SetSetting(api_token, %d): %v", i, err)
				return
			}
		}
	}()

	// Goroutine 3: concurrent SubscribeView.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			subCtx, subCancel := context.WithTimeout(ctx, 100*time.Millisecond)
			ch, err := mgr.SubscribeView(subCtx, "reinstall-race-ext", "main", "en")
			if err == nil {
				for range ch {
				}
			}
			subCancel()
		}
	}()

	// Goroutine 4: concurrent SendViewEvent, RunCommand, InvokeTool.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters*2; i++ {
			_ = mgr.SendViewEvent("reinstall-race-ext", "main", uiblock.Event{Node: "/", Kind: uiblock.EventTap})
			_ = mgr.RunCommand("reinstall-race-ext", "cmd", "en")
			_, _ = mgr.InvokeTool(ctx, "reinstall-race-ext", "tool", json.RawMessage(`{}`))
		}
	}()

	// Goroutine 5: concurrent Logs, BuildLogs, List.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters*3; i++ {
			_ = mgr.Logs("reinstall-race-ext")
			_ = mgr.BuildLogs("reinstall-race-ext")
			_ = mgr.List()
		}
	}()

	wg.Wait()
}

func TestManager_UninstallVsAllowAndBuildAndCloseNoOrphanNoRace(t *testing.T) {
	extDir := t.TempDir()
	pidLogPath := filepath.Join(t.TempDir(), "pids.log")

	const srcTemplate = `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	if f, err := os.OpenFile(__PID_LOG_PATH__, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Close()
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	src := strings.ReplaceAll(srcTemplate, "__PID_LOG_PATH__", strconv.Quote(pidLogPath))
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "uninst-orphan-ext"
version = "0.1.0"
publisher = "acme"
name = "Uninstall Orphan Ext"
permissions = ["storage"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	seenPIDs := map[int]bool{}
	for iter := 0; iter < 15; iter++ {
		dataDir := t.TempDir()
		mgr, err := NewManager(Config{
			DataDir:       dataDir,
			Keychain:      &keychain.Fake{},
			DeveloperMode: func() bool { return true },
			UIHost:        &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err := mgr.Start(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		if _, err := mgr.InstallLocal(extDir); err != nil {
			cancel()
			_ = mgr.Close(2 * time.Second)
			t.Fatal(err)
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		var uninstallErr error
		wg.Add(2)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			uninstallErr = mgr.Uninstall("uninst-orphan-ext", true)
		}(iter%2 == 0)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.AllowAndBuild("uninst-orphan-ext")
		}(iter%2 == 1)
		close(gate)
		wg.Wait()

		if err := mgr.Close(3 * time.Second); err != nil {
			cancel()
			t.Fatalf("iter %d: Close: %v", iter, err)
		}
		cancel()

		if uninstallErr != nil {
			t.Fatalf("iter %d: Uninstall failed: %v", iter, uninstallErr)
		}
		if got := mgr.List(); len(got) != 0 {
			t.Fatalf("iter %d: expected empty List() after Uninstall, got %+v", iter, got)
		}
		if entry := mgr.grants.Get("uninst-orphan-ext"); len(entry.Granted) != 0 || len(entry.Denied) != 0 || entry.ApprovedCommit != "" {
			t.Fatalf("iter %d: grants resurrected after Uninstall: %+v", iter, entry)
		}
		reloaded, err := LoadGrantStore(GrantsPath(dataDir))
		if err == nil {
			if entry := reloaded.Get("uninst-orphan-ext"); len(entry.Granted) != 0 || len(entry.Denied) != 0 {
				t.Fatalf("iter %d: persisted grants resurrected after Uninstall: %+v", iter, entry)
			}
		}

		if b, err := os.ReadFile(pidLogPath); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				pid, perr := strconv.Atoi(line)
				if perr != nil || pid <= 0 || seenPIDs[pid] {
					continue
				}
				seenPIDs[pid] = true
				if runtime.GOOS != "windows" {
					if proc, ferr := os.FindProcess(pid); ferr == nil {
						if serr := proc.Signal(syscall.Signal(0)); serr == nil {
							_ = proc.Kill()
							t.Fatalf("iter %d: orphaned extension process pid=%d still alive after Uninstall+Close", iter, pid)
						}
					}
				}
			}
		}
	}
}

func TestManager_UninstallVsSetSettingNoSecretOrDataResurrectionNoRace(t *testing.T) {
	extDir := t.TempDir()
	const src = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	manifest := fmt.Sprintf(`schema_version = 1
id = "uninst-setting-ext"
version = "0.1.0"
publisher = "acme"
name = "Uninstall Setting Ext"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[contributes.settings.label]
type = "string"
label = "Label"
default = "init"

[contributes.settings.api_token]
type = "secret"
label = "API Token"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 25; iter++ {
		dataDir := t.TempDir()
		kc := &keychain.Fake{}
		mgr, err := NewManager(Config{
			DataDir:       dataDir,
			Keychain:      kc,
			DeveloperMode: func() bool { return true },
			UIHost:        &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.InstallLocal(extDir); err != nil {
			_ = mgr.Close(2 * time.Second)
			t.Fatal(err)
		}
		if err := mgr.SetSetting("uninst-setting-ext", "api_token", "seed-secret"); err != nil {
			_ = mgr.Close(2 * time.Second)
			t.Fatal(err)
		}
		if err := mgr.SetSetting("uninst-setting-ext", "label", "seed-label"); err != nil {
			_ = mgr.Close(2 * time.Second)
			t.Fatal(err)
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		var uninstallErr error
		wg.Add(3)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			uninstallErr = mgr.Uninstall("uninst-setting-ext", true)
		}(iter%2 == 0)
		go func(yield bool, n int) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.SetSetting("uninst-setting-ext", "api_token", fmt.Sprintf("raced-secret-%d", n))
		}(iter%2 == 1, iter)
		go func(yield bool, n int) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_ = mgr.SetSetting("uninst-setting-ext", "label", fmt.Sprintf("raced-label-%d", n))
		}(iter%3 == 0, iter)
		close(gate)
		wg.Wait()

		_ = mgr.Close(2 * time.Second)

		if uninstallErr != nil {
			t.Fatalf("iter %d: Uninstall failed: %v", iter, uninstallErr)
		}
		if got := mgr.List(); len(got) != 0 {
			t.Fatalf("iter %d: expected empty List(), got %+v", iter, got)
		}
		if entries := kc.Entries(); len(entries) != 0 {
			t.Fatalf("iter %d: keychain entries remain after Uninstall(purgeData=true): %v", iter, entries)
		}
		if _, err := os.Stat(ExtensionDataDir(dataDir, "uninst-setting-ext")); !os.IsNotExist(err) {
			t.Fatalf("iter %d: expected ExtensionDataDir removed, stat err=%v", iter, err)
		}
	}
}

func TestManager_UninstallVsInstallLocalConsistencyNoGhostNoRace(t *testing.T) {
	const id = "uninst-inst-race"
	extDir := t.TempDir()
	const manifest = `schema_version = 1
id = "uninst-inst-race"
version = "0.1.0"
publisher = "acme"
name = "Uninstall Install Race Ext"
permissions = ["storage"]

[runtime]
type = "sandbox"
image = "alpine:3.20"

[[contributes.view]]
id = "main"
title = "Main"
`
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 30; iter++ {
		dataDir := t.TempDir()
		mgr, err := NewManager(Config{
			DataDir:  dataDir,
			Keychain: &keychain.Fake{},
			UIHost:   &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err := mgr.Start(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}

		// Seed on 2/3 of iterations so Uninstall removes an active record + ExtensionDir;
		// leave unseeded on 1/3 to also cover concurrent Uninstall-before-InstallLocal.
		seeded := iter%3 != 2
		if seeded {
			if _, err := mgr.InstallLocal(extDir); err != nil {
				cancel()
				_ = mgr.Close(2 * time.Second)
				t.Fatalf("iter %d: seed InstallLocal: %v", iter, err)
			}
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		var uninstallErr, installErr error
		wg.Add(2)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			uninstallErr = mgr.Uninstall(id, true)
		}(iter%2 == 0)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_, installErr = mgr.InstallLocal(extDir)
		}(iter%2 == 1)
		close(gate)
		wg.Wait()

		if installErr != nil {
			cancel()
			_ = mgr.Close(2 * time.Second)
			t.Fatalf("iter %d: InstallLocal failed unexpectedly: %v", iter, installErr)
		}

		curList := mgr.List()
		_, manifestStatErr := os.Stat(ManifestSnapshotPath(dataDir, id))
		_, extDirStatErr := os.Stat(ExtensionDir(dataDir, id))

		switch len(curList) {
		case 1:
			if curList[0].ID != id || curList[0].State != StateWaiting {
				cancel()
				_ = mgr.Close(2 * time.Second)
				t.Fatalf("iter %d: unexpected List()[0]=%+v", iter, curList[0])
			}
			if manifestStatErr != nil {
				cancel()
				_ = mgr.Close(2 * time.Second)
				t.Fatalf("iter %d: ghost extension in mgr.List() without %s on disk: %v", iter, ManifestName, manifestStatErr)
			}
		case 0:
			if uninstallErr != nil {
				cancel()
				_ = mgr.Close(2 * time.Second)
				t.Fatalf("iter %d: List() is empty yet Uninstall returned error: %v", iter, uninstallErr)
			}
			if !os.IsNotExist(extDirStatErr) {
				cancel()
				_ = mgr.Close(2 * time.Second)
				t.Fatalf("iter %d: expected ExtensionDir removed after Uninstall, stat err=%v", iter, extDirStatErr)
			}
		default:
			cancel()
			_ = mgr.Close(2 * time.Second)
			t.Fatalf("iter %d: unexpected len(mgr.List())=%d: %+v", iter, len(curList), curList)
		}

		if err := mgr.Close(2 * time.Second); err != nil {
			cancel()
			t.Fatalf("iter %d: mgr.Close: %v", iter, err)
		}
		cancel()

		// Reload from disk with a fresh Manager and verify 100% consistency with mgr.List().
		mgr2, err := NewManager(Config{
			DataDir:  dataDir,
			Keychain: &keychain.Fake{},
			UIHost:   &fakeUIHost{},
		})
		if err != nil {
			t.Fatalf("iter %d: reload NewManager: %v", iter, err)
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		if err := mgr2.Start(ctx2); err != nil {
			cancel2()
			_ = mgr2.Close(2 * time.Second)
			t.Fatalf("iter %d: mgr2.Start: %v", iter, err)
		}
		reloaded := mgr2.List()
		_ = mgr2.Close(2 * time.Second)
		cancel2()

		if len(reloaded) != len(curList) {
			t.Fatalf("iter %d: reloaded List() len=%d != mgr.List() len=%d (reloaded=%+v cur=%+v)",
				iter, len(reloaded), len(curList), reloaded, curList)
		}
		if len(reloaded) == 1 && (reloaded[0].ID != curList[0].ID || reloaded[0].State != curList[0].State) {
			t.Fatalf("iter %d: reloaded List()[0]=%+v != curList[0]=%+v", iter, reloaded[0], curList[0])
		}
	}
}

func TestManager_InstallLocalVsCloseNoGhostNoRace(t *testing.T) {
	const id = "inst-close-race"
	extDir := t.TempDir()
	const manifest = `schema_version = 1
id = "inst-close-race"
version = "0.1.0"
publisher = "acme"
name = "Install Close Race Ext"

[runtime]
type = "sandbox"
image = "alpine:3.20"
`
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	for iter := 0; iter < 30; iter++ {
		dataDir := t.TempDir()
		mgr, err := NewManager(Config{
			DataDir:  dataDir,
			Keychain: &keychain.Fake{},
			UIHost:   &fakeUIHost{},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err := mgr.Start(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		var installErr, closeErr error
		wg.Add(2)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			_, installErr = mgr.InstallLocal(extDir)
		}(iter%2 == 0)
		go func(yield bool) {
			defer wg.Done()
			<-gate
			if yield {
				runtime.Gosched()
			}
			closeErr = mgr.Close(2 * time.Second)
		}(iter%2 == 1)
		close(gate)
		wg.Wait()
		cancel()

		if closeErr != nil {
			t.Fatalf("iter %d: Close failed: %v", iter, closeErr)
		}

		curList := mgr.List()
		_, manifestStatErr := os.Stat(ManifestSnapshotPath(dataDir, id))
		_, extDirStatErr := os.Stat(ExtensionDir(dataDir, id))

		if installErr != nil {
			if len(curList) != 0 {
				t.Fatalf("iter %d: InstallLocal failed (%v) yet mgr.List() is non-empty: %+v", iter, installErr, curList)
			}
			if !os.IsNotExist(extDirStatErr) {
				t.Fatalf("iter %d: InstallLocal failed (%v) yet ExtensionDir exists on disk (stat err=%v)", iter, installErr, extDirStatErr)
			}
		} else {
			if len(curList) != 1 || curList[0].ID != id {
				t.Fatalf("iter %d: InstallLocal succeeded yet mgr.List()=%+v", iter, curList)
			}
			if manifestStatErr != nil {
				t.Fatalf("iter %d: InstallLocal succeeded yet %s missing: %v", iter, ManifestName, manifestStatErr)
			}
		}
	}
}

type closeBlockingBuilder struct {
	blockOnCall int32
	calls       atomic.Int32
	entered     chan struct{}
	enterOnce   sync.Once
}

func (b *closeBlockingBuilder) Build(ctx context.Context, req BuildRequest, _ func(string)) (string, error) {
	call := b.calls.Add(1)
	if b.blockOnCall == 0 || call == b.blockOnCall {
		b.enterOnce.Do(func() { close(b.entered) })
		<-ctx.Done()
	}
	return "sha256:stub-" + req.Tags[0], nil
}

type closeBlockingPuller struct {
	entered   chan struct{}
	enterOnce sync.Once
}

func (p *closeBlockingPuller) Pull(ctx context.Context, _ string, _ func(string)) error {
	p.enterOnce.Do(func() { close(p.entered) })
	<-ctx.Done()
	return nil
}

func TestManager_CloseDuringBuildOrPullResetsStateBuildingNoRace(t *testing.T) {
	runBuildCase := func(t *testing.T, useRebuild bool) {
		t.Helper()
		dataDir := t.TempDir()
		extDir := t.TempDir()
		const manifest = `schema_version = 1
id = "build-close-race"
version = "0.1.0"
publisher = "acme"
name = "Build Close Race"

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
`
		if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		blockOn := int32(1)
		if useRebuild {
			blockOn = 2
		}
		builder := &closeBlockingBuilder{
			blockOnCall: blockOn,
			entered:     make(chan struct{}),
		}
		uiHost := &fakeUIHost{}
		mgr, err := NewManager(Config{
			DataDir:      dataDir,
			Keychain:     &keychain.Fake{},
			ImageBuilder: builder,
			UIHost:       uiHost,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := mgr.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.InstallLocal(extDir); err != nil {
			t.Fatal(err)
		}

		if useRebuild {
			// Initial build completes; start fails because Supervisor is nil -> ready for Rebuild.
			_ = mgr.AllowAndBuild("build-close-race")
		}

		opDone := make(chan error, 1)
		go func() {
			if useRebuild {
				opDone <- mgr.Rebuild("build-close-race")
			} else {
				opDone <- mgr.AllowAndBuild("build-close-race")
			}
		}()

		select {
		case <-builder.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for ImageBuilder.Build to enter")
		}

		// Confirm extension is in StateBuilding before Close begins.
		waitForExtState(t, mgr, "build-close-race", 2*time.Second, func(info ExtensionInfo) bool {
			return info.State == StateBuilding
		})

		uiHost.mu.Lock()
		changedBeforeClose := uiHost.changedCount
		uiHost.mu.Unlock()

		// Close sets m.closed = true and cancels ctx, unblocking Build which returns nil.
		if err := mgr.Close(2 * time.Second); err != nil {
			t.Fatalf("mgr.Close: %v", err)
		}

		select {
		case opErr := <-opDone:
			if opErr == nil {
				t.Fatalf("expected non-nil closed error after build finished during Close")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for AllowAndBuild/Rebuild to return")
		}

		list := mgr.List()
		if len(list) != 1 {
			t.Fatalf("expected 1 extension, got %+v", list)
		}
		if list[0].State != StateDiscovered {
			t.Fatalf("expected StateDiscovered after Close during build, got %q (info=%+v)", list[0].State, list[0])
		}
		if list[0].Error != "" {
			t.Fatalf("expected empty Error after Close during build, got %q", list[0].Error)
		}

		uiHost.mu.Lock()
		changedAfterClose := uiHost.changedCount
		uiHost.mu.Unlock()
		if changedAfterClose <= changedBeforeClose {
			t.Fatalf("expected UIHost.Changed() notification when leaving StateBuilding (before=%d after=%d)", changedBeforeClose, changedAfterClose)
		}
	}

	t.Run("AllowAndBuild_Dockerfile", func(t *testing.T) {
		runBuildCase(t, false)
	})
	t.Run("Rebuild_Dockerfile", func(t *testing.T) {
		runBuildCase(t, true)
	})
	t.Run("AllowAndBuild_ImagePull", func(t *testing.T) {
		dataDir := t.TempDir()
		puller := &closeBlockingPuller{entered: make(chan struct{})}
		uiHost := &fakeUIHost{}
		mgr, err := NewManager(Config{
			DataDir:     dataDir,
			Keychain:    &keychain.Fake{},
			ImagePuller: puller,
			UIHost:      uiHost,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := mgr.Start(ctx); err != nil {
			t.Fatal(err)
		}
		info, err := mgr.InstallImage("ghcr.io/acme/pull-close-race:1.0.0")
		if err != nil {
			t.Fatal(err)
		}

		opDone := make(chan error, 1)
		go func() {
			opDone <- mgr.AllowAndBuild(info.ID)
		}()

		select {
		case <-puller.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for ImagePuller.Pull to enter")
		}

		waitForExtState(t, mgr, info.ID, 2*time.Second, func(i ExtensionInfo) bool {
			return i.State == StateBuilding
		})

		if err := mgr.Close(2 * time.Second); err != nil {
			t.Fatalf("mgr.Close: %v", err)
		}

		select {
		case opErr := <-opDone:
			if opErr == nil {
				t.Fatalf("expected non-nil closed error after pull finished during Close")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for AllowAndBuild to return")
		}

		list := mgr.List()
		if len(list) != 1 || list[0].State != StateDiscovered {
			t.Fatalf("expected [StateDiscovered] after Close during pull, got %+v", list)
		}
	})
}

func TestManager_DiscoverExtensionsSymlinkAndClosedGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	dataDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, ManifestName), []byte("schema_version = 1\nid = \"sym-disc\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Sym\"\n[runtime]\ntype = \"sandbox\"\nimage = \"alpine:3.20\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	extsRoot := ExtensionsDir(dataDir)
	if err := os.MkdirAll(extsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(extsRoot, "sym-disc")); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-populate an existing entry and verify discoverExtensions does not overwrite it.
	existing := mgr.newBaseRecord("keep-me")
	existing.state = StateDisabled
	mgr.mu.Lock()
	mgr.exts["keep-me"] = existing
	mgr.mu.Unlock()
	if err := os.MkdirAll(ExtensionDir(dataDir, "keep-me"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := mgr.discoverExtensions(); err != nil {
		t.Fatalf("discoverExtensions: %v", err)
	}
	mgr.mu.RLock()
	symRec := mgr.exts["sym-disc"]
	keepRec := mgr.exts["keep-me"]
	mgr.mu.RUnlock()
	if symRec == nil || symRec.state != StateInvalid {
		t.Fatalf("expected symlinked valid id marked StateInvalid, got %+v", symRec)
	}
	if keepRec != existing || keepRec.state != StateDisabled {
		t.Fatalf("discoverExtensions overwrote existing m.exts entry: %+v", keepRec)
	}

	// Closed manager refuses discoverExtensions mutations.
	_ = mgr.Close(time.Second)
	if err := os.MkdirAll(ExtensionDir(dataDir, "after-close"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mgr.discoverExtensions(); err != nil {
		t.Fatalf("discoverExtensions after Close: %v", err)
	}
	mgr.mu.RLock()
	_, found := mgr.exts["after-close"]
	mgr.mu.RUnlock()
	if found {
		t.Fatal("discoverExtensions populated m.exts after Close")
	}
}

func TestManager_UninstallPurgeRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}

	// Subcase A: symlinked extension-data/<id> must fail Uninstall(id, true) without deleting outside files.
	dataDir := t.TempDir()
	outside := t.TempDir()
	outsideSecrets := filepath.Join(outside, "secrets.json")
	outsideIndex := filepath.Join(outside, "secrets-index.json")
	if err := os.WriteFile(outsideSecrets, []byte(`{"secret:api":"preserve"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideIndex, []byte(`["secret:api"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	extSrc := filepath.Join(t.TempDir(), "local-src")
	if err := os.MkdirAll(extSrc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extSrc, ManifestName), []byte("schema_version = 1\nid = \"sym-uninst\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Sym Uninst\"\n[runtime]\ntype = \"sandbox\"\nimage = \"alpine:3.20\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(2 * time.Second) }()
	if _, err := mgr.InstallLocal(extSrc); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}

	if err := os.MkdirAll(ExtensionDataRoot(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ExtensionDataDir(dataDir, "sym-uninst")); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Uninstall("sym-uninst", true); err == nil {
		t.Fatal("Uninstall(id, true) succeeded through symlinked extension-data/<id>")
	}
	if _, err := os.Stat(outsideSecrets); err != nil {
		t.Fatalf("outside secrets.json was deleted by Uninstall: %v", err)
	}
	if _, err := os.Stat(outsideIndex); err != nil {
		t.Fatalf("outside secrets-index.json was deleted by Uninstall: %v", err)
	}

	// Subcase B: symlinked extensions/ parent must fail Uninstall without deleting outside directory.
	dataDir2 := t.TempDir()
	outsideExts := t.TempDir()
	outsideChild := filepath.Join(outsideExts, "sym-parent-ext")
	if err := os.MkdirAll(outsideChild, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideManifest := filepath.Join(outsideChild, ManifestName)
	if err := os.WriteFile(outsideManifest, []byte("schema_version = 1\nid = \"sym-parent-ext\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Sym Parent\"\n[runtime]\ntype = \"sandbox\"\nimage = \"alpine:3.20\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr2, err := NewManager(Config{DataDir: dataDir2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr2.Close(2 * time.Second) }()
	// Install first, then replace extensions/ dir with symlink to outsideExts.
	if _, err := mgr2.InstallLocal(extSrc); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := os.RemoveAll(ExtensionsDir(dataDir2)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideExts, ExtensionsDir(dataDir2)); err != nil {
		t.Fatal(err)
	}
	// Rename outsideChild to match installed id "sym-uninst".
	if err := os.Rename(outsideChild, filepath.Join(outsideExts, "sym-uninst")); err != nil {
		t.Fatal(err)
	}
	if err := mgr2.Uninstall("sym-uninst", false); err == nil {
		t.Fatal("Uninstall succeeded through symlinked extensions parent directory")
	}
	if _, err := os.Stat(filepath.Join(outsideExts, "sym-uninst", ManifestName)); err != nil {
		t.Fatalf("outside extension directory was deleted through symlinked extensions/: %v", err)
	}
}

type blockingMarkerBuilder struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	builtMarker string
}

func (b *blockingMarkerBuilder) Build(_ context.Context, req BuildRequest, _ func(string)) (string, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	raw, _ := os.ReadFile(filepath.Join(req.ContextDir, "marker.txt"))
	b.builtMarker = strings.TrimSpace(string(raw))
	return "sha256:built-marker", nil
}

func TestManager_InstallLocalSerializedWithAllowAndBuild(t *testing.T) {
	dataDir := t.TempDir()
	writeSrc := func(ver, marker string) string {
		dir := t.TempDir()
		man := fmt.Sprintf("schema_version = 1\nid = \"ser-ext\"\nversion = %q\npublisher = \"acme\"\nname = \"Serialized\"\n[runtime]\ntype = \"sandbox\"\ndockerfile = \"Dockerfile\"\n", ver)
		if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(man), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	v1Dir := writeSrc("0.1.0", "v1")
	v2Dir := writeSrc("0.2.0", "v2")

	builder := &blockingMarkerBuilder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	mgr, err := NewManager(Config{
		DataDir:      dataDir,
		ImageBuilder: builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(2 * time.Second) }()
	defer func() {
		select {
		case <-builder.release:
		default:
			close(builder.release)
		}
	}()

	if _, err := mgr.InstallLocal(v1Dir); err != nil {
		t.Fatalf("InstallLocal(v1): %v", err)
	}

	buildDone := make(chan error, 1)
	go func() {
		buildDone <- mgr.AllowAndBuild("ser-ext")
	}()
	<-builder.started

	installDone := make(chan error, 1)
	go func() {
		_, err := mgr.InstallLocal(v2Dir)
		installDone <- err
	}()

	// Allow goroutine scheduling time; v2 MUST block behind AllowAndBuild's rec.opMu and MUST NOT mutate ExtensionsDir mid-build.
	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-installDone:
		t.Fatalf("InstallLocal(v2) completed while AllowAndBuild(v1) was mid-build (err=%v)", err)
	default:
	}
	midRaw, err := os.ReadFile(ManifestSnapshotPath(dataDir, "ser-ext"))
	if err != nil {
		t.Fatalf("read mid-build manifest snapshot: %v", err)
	}
	if !strings.Contains(string(midRaw), `version = "0.1.0"`) {
		t.Fatalf("ExtensionDir manifest mutated mid-build:\n%s", string(midRaw))
	}

	close(builder.release)
	<-buildDone // startLocked fails without supervisor, which is expected; build phase already completed.
	if err := <-installDone; err != nil {
		t.Fatalf("InstallLocal(v2) failed after AllowAndBuild released: %v", err)
	}
	if builder.builtMarker != "v1" {
		t.Fatalf("Build saw marker=%q, want v1", builder.builtMarker)
	}
	finalRaw, err := os.ReadFile(ManifestSnapshotPath(dataDir, "ser-ext"))
	if err != nil {
		t.Fatalf("read final manifest snapshot: %v", err)
	}
	if !strings.Contains(string(finalRaw), `version = "0.2.0"`) {
		t.Fatalf("final manifest snapshot missing v2:\n%s", string(finalRaw))
	}
}

func TestManager_AllowAndBuildRespectsDisabledEntry(t *testing.T) {
	dataDir := t.TempDir()
	extSrc := t.TempDir()
	startMarker := filepath.Join(t.TempDir(), "started")
	goCode := fmt.Sprintf(`package main
import ("context"; "os"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	_ = os.WriteFile(%q, []byte("started"), 0o600)
	app := extsdk.New()
	_ = app.Run(context.Background())
}`, startMarker)

	binDir := filepath.Join(extSrc, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	builtBin := buildTestSDKFixture(t, t.TempDir(), goCode, "", "")
	binBytes, err := os.ReadFile(builtBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "ext"), binBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	man := "schema_version = 1\nid = \"dis-allow\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Disabled Allow\"\n[runtime]\ntype = \"binary\"\ndev_entrypoint = \"bin/ext\"\n"
	if err := os.WriteFile(filepath.Join(extSrc, ManifestName), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		DeveloperMode: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close(2 * time.Second) }()

	if _, err := mgr.InstallLocal(extSrc); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.Disable("dis-allow"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr.AllowAndBuild("dis-allow"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}

	var got ExtensionState
	for _, info := range mgr.List() {
		if info.ID == "dis-allow" {
			got = info.State
		}
	}
	if got != StateDisabled {
		t.Fatalf("AllowAndBuild on disabled extension set state=%q, want %q", got, StateDisabled)
	}
	if _, err := os.Stat(startMarker); !os.IsNotExist(err) {
		t.Fatalf("disabled extension was started during AllowAndBuild (marker stat err=%v)", err)
	}
}

func TestManager_ShortTimeoutEnsureRunningDoesNotPoisonColdStart(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()
	delayMarker := filepath.Join(t.TempDir(), "delayed")
	const src = `package main

import (
	"context"
	"os"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	if _, err := os.Stat(os.Getenv("GVX_DELAY_MARKER")); err == nil {
		time.Sleep(300 * time.Millisecond)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("cold-start-ok", extsdk.Key("k")))
		},
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "cold-ctx-ext"
version = "0.1.0"
publisher = "acme"
name = "Cold Ctx Ext"

[runtime]
type = "binary"
dev_entrypoint = %q
idle_timeout_sec = 1

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("cold-ctx-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "cold-ctx-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	// Let it idle-stop cleanly to StateDiscovered.
	waitForExtState(t, mgr, "cold-ctx-ext", 5*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateDiscovered
	})

	// Enable 300ms startup delay for cold wake.
	t.Setenv("GVX_DELAY_MARKER", delayMarker)
	if err := os.WriteFile(delayMarker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer shortCancel()
	if _, err := mgr.SubscribeView(shortCtx, "cold-ctx-ext", "main", "en"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded from short SubscribeView, got %v", err)
	}

	// Cold start must continue in the background and reach StateRunning (NOT StateFailed).
	info := waitForExtState(t, mgr, "cold-ctx-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning || i.State == StateFailed
	})
	if info.State != StateRunning {
		t.Fatalf("short-timeout SubscribeView poisoned extension to state=%q err=%q", info.State, info.Error)
	}

	subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
	defer subCancel()
	ch, err := mgr.SubscribeView(subCtx, "cold-ctx-ext", "main", "en")
	if err != nil {
		t.Fatalf("subsequent SubscribeView: %v", err)
	}
	upd := <-ch
	treeJSON, _ := json.Marshal(upd.Tree)
	if upd.Tree == nil || !strings.Contains(string(treeJSON), "cold-start-ok") {
		t.Fatalf("unexpected view update after cold start: %+v (%s)", upd, string(treeJSON))
	}
}

func TestManager_CloseDuringInFlightColdStartRaceFree(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()
	delayMarker := filepath.Join(t.TempDir(), "close-delay")
	const src = `package main

import (
	"context"
	"os"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	if _, err := os.Stat(os.Getenv("GVX_CLOSE_DELAY_MARKER")); err == nil {
		time.Sleep(500 * time.Millisecond)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "close-cold-ext"
version = "0.1.0"
publisher = "acme"
name = "Close Cold Ext"

[runtime]
type = "binary"
dev_entrypoint = %q
idle_timeout_sec = 1

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(2 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("close-cold-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "close-cold-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})
	waitForExtState(t, mgr, "close-cold-ext", 5*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateDiscovered
	})

	t.Setenv("GVX_CLOSE_DELAY_MARKER", delayMarker)
	if err := os.WriteFile(delayMarker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	_, _ = mgr.SubscribeView(shortCtx, "close-cold-ext", "main", "en")

	// Close while cold start is still in flight; must return cleanly without races or leaked goroutines.
	if err := mgr.Close(2 * time.Second); err != nil {
		t.Fatalf("mgr.Close during in-flight cold start: %v", err)
	}
}

func TestManager_PostStopFailureTransitionsOutOfRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory chmod semantics required")
	}
	if os.Geteuid() == 0 {
		t.Skip("cannot enforce 0500 directory write failure when running as root")
	}

	dataDir := t.TempDir()
	extDir := t.TempDir()
	extDirV2 := t.TempDir()

	const src = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	app.Command("ping", func(c *extsdk.CommandContext) error {
		return nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	buildTestSDKFixture(t, extDirV2, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	writeManifest := func(dir, ver string) {
		tomlBody := fmt.Sprintf(`schema_version = 1
id = "post-stop-err"
version = %q
publisher = "acme"
name = "Post Stop Err"

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "ping"
title = "Ping"
`, ver, binRel)
		if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(tomlBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(extDir, "0.1.0")
	writeManifest(extDirV2, "0.2.0")

	ui := &fakeUIHost{}
	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        ui,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dataDir, 0o700)
		_ = os.Chmod(ExtensionsDir(dataDir), 0o700)
		_ = os.Chmod(ExtensionDir(dataDir, "post-stop-err"), 0o700)
		_ = mgr.Close(2 * time.Second)
	})

	changedCount := func() int {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		return ui.changedCount
	}
	findInfo := func() ExtensionInfo {
		for _, info := range mgr.List() {
			if info.ID == "post-stop-err" {
				return info
			}
		}
		t.Fatalf("extension post-stop-err missing from List()")
		return ExtensionInfo{}
	}
	assertStoppedAndFailed := func(step string) {
		t.Helper()
		info := findInfo()
		if info.State == StateRunning || info.State.IsActive() || info.State == StateBuilding {
			t.Fatalf("%s: expected non-running state after post-stop failure, got state=%s err=%q", step, info.State, info.Error)
		}
		if info.State != StateFailed {
			t.Fatalf("%s: expected StateFailed, got %s (err=%q)", step, info.State, info.Error)
		}
		if info.Error == "" {
			t.Fatalf("%s: expected non-empty ExtensionInfo.Error", step)
		}
		mgr.mu.RLock()
		rec := mgr.exts["post-stop-err"]
		var rt extRuntime
		var hapi *hostAPIServer
		var dp *scopedDockerProxy
		if rec != nil {
			rt = rec.runtime
			hapi = rec.hostAPI
			dp = rec.dockerProxy
		}
		mgr.mu.RUnlock()
		if rt != nil || hapi != nil || dp != nil {
			t.Fatalf("%s: expected torn-down runtime/hostAPI/dockerProxy, got rt=%v hapi=%v dp=%v", step, rt != nil, hapi != nil, dp != nil)
		}
	}
	startToRunning := func(step string) {
		t.Helper()
		if err := mgr.AllowAndBuild("post-stop-err"); err != nil {
			t.Fatalf("%s AllowAndBuild: %v", step, err)
		}
		waitForExtState(t, mgr, "post-stop-err", 10*time.Second, func(i ExtensionInfo) bool {
			return i.State == StateRunning
		})
		if err := mgr.RunCommand("post-stop-err", "ping", "en"); err != nil {
			t.Fatalf("%s RunCommand: %v", step, err)
		}
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	startToRunning("initial")

	// 1. registerInstalled commitDisk failure on already-running extension.
	extInstallDir := ExtensionDir(dataDir, "post-stop-err")
	if err := os.Chmod(extInstallDir, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify := changedCount()
	if _, err := mgr.InstallLocal(extDirV2); err == nil {
		_ = os.Chmod(extInstallDir, 0o700)
		t.Fatalf("expected InstallLocal commitDisk failure on read-only ExtensionDir")
	}
	if err := os.Chmod(extInstallDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on failed reinstall")
	}
	assertStoppedAndFailed("registerInstalled(commitDisk)")

	// Confirm recovery operations (Disable -> AllowAndBuild) work cleanly without wedging.
	if err := mgr.Disable("post-stop-err"); err != nil {
		t.Fatalf("Disable after failed reinstall: %v", err)
	}
	if info := findInfo(); info.State != StateDisabled {
		t.Fatalf("expected StateDisabled after Disable, got %s", info.State)
	}
	if err := mgr.Enable("post-stop-err"); err != nil {
		t.Fatalf("Enable after Disable: %v", err)
	}
	waitForExtState(t, mgr, "post-stop-err", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	// 2. AllowAndBuild post-stopLocked grants.Approve failure.
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify = changedCount()
	if err := mgr.AllowAndBuild("post-stop-err"); err == nil {
		_ = os.Chmod(dataDir, 0o700)
		t.Fatalf("expected AllowAndBuild grants.Approve failure on read-only DataDir")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on AllowAndBuild grants failure")
	}
	assertStoppedAndFailed("AllowAndBuild")
	startToRunning("recover-after-allow")

	// 3. Deny post-stopLocked grants.Deny failure.
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify = changedCount()
	if err := mgr.Deny("post-stop-err"); err == nil {
		_ = os.Chmod(dataDir, 0o700)
		t.Fatalf("expected Deny grants.Deny failure on read-only DataDir")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on Deny grants failure")
	}
	assertStoppedAndFailed("Deny")
	startToRunning("recover-after-deny")

	// 4. RevokePermissions post-stopLocked grants.RevokePermissions failure.
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify = changedCount()
	if err := mgr.RevokePermissions("post-stop-err"); err == nil {
		_ = os.Chmod(dataDir, 0o700)
		t.Fatalf("expected RevokePermissions failure on read-only DataDir")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on RevokePermissions failure")
	}
	assertStoppedAndFailed("RevokePermissions")
	startToRunning("recover-after-revoke")

	// 5. Disable post-stopLocked grants.SetDisabled failure.
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify = changedCount()
	if err := mgr.Disable("post-stop-err"); err == nil {
		_ = os.Chmod(dataDir, 0o700)
		t.Fatalf("expected Disable grants.SetDisabled failure on read-only DataDir")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on Disable grants failure")
	}
	assertStoppedAndFailed("Disable")
	startToRunning("recover-after-disable")

	// 6. Uninstall post-stopLocked removeAllNoSymlinkWithin failure.
	extsRoot := ExtensionsDir(dataDir)
	if err := os.Chmod(extsRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	beforeNotify = changedCount()
	if err := mgr.Uninstall("post-stop-err", false); err == nil {
		_ = os.Chmod(extsRoot, 0o700)
		t.Fatalf("expected Uninstall removeAll failure on read-only ExtensionsDir")
	}
	if err := os.Chmod(extsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if changedCount() <= beforeNotify {
		t.Fatalf("expected UIHost.Changed() notification on Uninstall failure")
	}
	assertStoppedAndFailed("Uninstall")

	// Confirm clean Uninstall succeeds once writable again.
	if err := mgr.Uninstall("post-stop-err", true); err != nil {
		t.Fatalf("final Uninstall: %v", err)
	}
	for _, info := range mgr.List() {
		if info.ID == "post-stop-err" {
			t.Fatalf("expected post-stop-err removed after Uninstall, still present: %+v", info)
		}
	}
}

func TestManager_SubscribeViewVsTightIdleReaperDoesNotWedge(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()
	const src = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("tight-reap-ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, src, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "tight-reap-ext"
version = "0.1.0"
publisher = "acme"
name = "Tight Reap Ext"

[runtime]
type = "binary"
dev_entrypoint = %q
idle_timeout_sec = 1

[[contributes.view]]
id = "main"
title = "Main"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("tight-reap-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "tight-reap-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	mgr.mu.RLock()
	rec := mgr.exts["tight-reap-ext"]
	mgr.mu.RUnlock()
	if rec == nil {
		t.Fatal("missing tight-reap-ext record")
	}

	const iterations = 64
	for iter := 0; iter < iterations; iter++ {
		// Force idle eligibility so reapIdleExtensions races tightly with SubscribeView.
		mgr.mu.Lock()
		rec.lastActivity = time.Now().Add(-10 * time.Second)
		mgr.mu.Unlock()

		if iter%2 == 1 {
			// Every other iteration, test waking directly after an idle-stop too.
			mgr.reapIdleExtensions()
		}

		var wg sync.WaitGroup
		wg.Add(1)
		stopReap := make(chan struct{})
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReap:
					return
				default:
					mgr.mu.Lock()
					rec.lastActivity = time.Now().Add(-10 * time.Second)
					mgr.mu.Unlock()
					mgr.reapIdleExtensions()
					runtime.Gosched()
				}
			}
		}()

		subCtx, subCancel := context.WithTimeout(context.Background(), 3*time.Second)
		ch, err := mgr.SubscribeView(subCtx, "tight-reap-ext", "main", "en")
		if err != nil {
			close(stopReap)
			wg.Wait()
			subCancel()
			t.Fatalf("iter %d: SubscribeView error: %v", iter, err)
		}

		gotTree := false
		for !gotTree {
			select {
			case upd, ok := <-ch:
				if !ok {
					close(stopReap)
					wg.Wait()
					subCancel()
					t.Fatalf("iter %d: SubscribeView channel closed before delivering tree", iter)
				}
				if upd.Tree != nil {
					gotTree = true
				}
			case <-subCtx.Done():
				close(stopReap)
				wg.Wait()
				subCancel()
				t.Fatalf("iter %d: SubscribeView wedged during concurrent reapIdleExtensions", iter)
			}
		}

		// While view subscription is active, reapIdleExtensions must NOT stop the extension.
		mgr.reapIdleExtensions()
		mgr.mu.RLock()
		active := rec.activeViews
		state := rec.state
		mgr.mu.RUnlock()
		if active < 1 || state != StateRunning {
			close(stopReap)
			wg.Wait()
			subCancel()
			t.Fatalf("iter %d: expected activeViews>=1 and StateRunning while subscribed, got activeViews=%d state=%s", iter, active, state)
		}

		close(stopReap)
		wg.Wait()
		subCancel()
		for range ch {
		}
	}
}

func TestManager_RemoveAllowedFolderAndRevokeStopsChildSandboxesAndDockerContainers(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := newNonReservedTestDir(t)
	fakeRungvBin := buildMinimalFakeRungv(t)

	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	type mockCtr struct {
		labels  map[string]string
		binds   []string
		running bool
		stopped bool
	}
	var (
		ctrMu      sync.Mutex
		nextCtrID  int
		containers = map[string]*mockCtr{}
	)
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))

		case r.Method == http.MethodPost && p == "/containers/create":
			var req struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					Binds []string `json:"Binds"`
				} `json:"HostConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ctrMu.Lock()
			nextCtrID++
			cid := fmt.Sprintf("mgr-ctr-%d", nextCtrID)
			containers[cid] = &mockCtr{
				labels: req.Labels,
				binds:  req.HostConfig.Binds,
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})

		case r.Method == http.MethodGet && p == "/containers/json":
			ctrMu.Lock()
			var list []map[string]any
			for cid, c := range containers {
				if c.running || r.URL.Query().Get("all") == "1" {
					list = append(list, map[string]any{"Id": cid, "Labels": c.labels})
				}
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			ctrMu.Lock()
			c, ok := containers[cid]
			ctrMu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         cid,
				"Image":      "alpine:3.20",
				"Config":     map[string]any{"Labels": c.labels},
				"HostConfig": map[string]any{"Binds": c.binds},
			})

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = true
				c.stopped = false
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stop"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = false
				c.stopped = true
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/"):
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	extDir := t.TempDir()
	const sdkSrc = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerDo(ctx context.Context, method, p, body string) (int, string, error) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{}
	base := ep
	if strings.HasPrefix(ep, "unix://") {
		sock := strings.TrimPrefix(ep, "unix://")
		tr.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(c, "unix", sock)
		}
		base = "http://docker"
	}
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.Command("setup", func(ctx *extsdk.CommandContext) error {
		picked, ok, err := ctx.Host.PickFolder(ctx)
		if err != nil || !ok {
			return fmt.Errorf("PickFolder: ok=%v err=%v", ok, err)
		}
		startTrue := true
		sbxAllowed, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:   "child-allowed",
			Image:  "alpine:3.20",
			Mounts: []extsdk.SandboxMount{{Type: "bind", Source: picked, Target: "/work"}},
			Start:  &startTrue,
		})
		if err != nil {
			return fmt.Errorf("SandboxCreate(allowed): %w", err)
		}
		sbxData, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:   "child-data",
			Image:  "alpine:3.20",
			Mounts: []extsdk.SandboxMount{{Type: "bind", Source: "/data/keep", Target: "/keep"}},
			Start:  &startTrue,
		})
		if err != nil {
			return fmt.Errorf("SandboxCreate(data): %w", err)
		}

		c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", fmt.Sprintf(` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":[%q]}}` + "`" + `, picked+":/work:rw"))
		if e != nil || c != 201 {
			return fmt.Errorf("create ctrAllowed: %d %s %v", c, b, e)
		}
		var r1 struct{ Id string }
		_ = json.Unmarshal([]byte(b), &r1)
		if sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+r1.Id+"/start", ""); se != nil || sc != 204 {
			return fmt.Errorf("start ctrAllowed: %d %s %v", sc, sb, se)
		}

		c2, b2, e2 := dockerDo(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["/data/keep:/keep:rw"]}}` + "`" + `)
		if e2 != nil || c2 != 201 {
			return fmt.Errorf("create ctrData: %d %s %v", c2, b2, e2)
		}
		var r2 struct{ Id string }
		_ = json.Unmarshal([]byte(b2), &r2)
		if sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+r2.Id+"/start", ""); se != nil || sc != 204 {
			return fmt.Errorf("start ctrData: %d %s %v", sc, sb, se)
		}

		out, _ := json.Marshal(map[string]string{
			"sbx_allowed": sbxAllowed.ID,
			"sbx_data":    sbxData.ID,
			"ctr_allowed": r1.Id,
			"ctr_data":    r2.Id,
		})
		return os.WriteFile(filepath.Join(ctx.DataDir, "ids.json"), out, 0o600)
	})
	app.Tool("probe_after_revoke", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		sbxID, _ := args["sbx_allowed"].(string)
		ctrID, _ := args["ctr_allowed"].(string)
		_, sbxStartErr := ctx.Host.SandboxStart(ctx, sbxID)
		cStart, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/start", "")
		cRestart, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/restart", "")
		cUnpause, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/unpause", "")
		return map[string]any{
			"sbx_start_denied": extsdk.IsPermissionDenied(sbxStartErr),
			"ctr_start_code":   cStart,
			"ctr_restart_code": cRestart,
			"ctr_unpause_code": cUnpause,
		}, nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "revoke-folder-ext"
version = "0.1.0"
publisher = "acme"
name = "Revoke Folder Ext"
permissions = ["sandboxes", "docker", "ui:pick-folder"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.command]]
id = "setup"
title = "Setup"

[[contributes.tool]]
id = "probe_after_revoke"
description = "Probe"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	ui := &fakeUIHost{pickPath: allowedDir, pickOK: true}
	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		Supervisor:    sup,
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: ui,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("revoke-folder-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "revoke-folder-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	if err := mgr.RunCommand("revoke-folder-ext", "setup", "en"); err != nil {
		t.Fatalf("RunCommand(setup): %v", err)
	}
	rawIDs, err := os.ReadFile(filepath.Join(FilesDir(dataDir, "revoke-folder-ext"), "ids.json"))
	if err != nil {
		t.Fatalf("read ids.json: %v", err)
	}
	var ids struct {
		SbxAllowed string `json:"sbx_allowed"`
		SbxData    string `json:"sbx_data"`
		CtrAllowed string `json:"ctr_allowed"`
		CtrData    string `json:"ctr_data"`
	}
	if err := json.Unmarshal(rawIDs, &ids); err != nil {
		t.Fatal(err)
	}

	if !sup.State(ids.SbxAllowed).Status.Active() || !sup.State(ids.SbxData).Status.Active() {
		t.Fatalf("expected both child sandboxes active before revoke: allowed=%v data=%v",
			sup.State(ids.SbxAllowed).Status, sup.State(ids.SbxData).Status)
	}

	// Revoke allowedDir -> child sandbox & Docker container referencing allowedDir must be stopped,
	// while /data/keep resources stay running.
	if err := mgr.RemoveAllowedFolder("revoke-folder-ext", allowedDir); err != nil {
		t.Fatalf("RemoveAllowedFolder: %v", err)
	}

	if st := sup.State(ids.SbxAllowed).Status; st.Active() {
		t.Fatalf("expected sbx_allowed stopped after RemoveAllowedFolder, got %v", st)
	}
	if st := sup.State(ids.SbxData).Status; !st.Active() {
		t.Fatalf("expected sbx_data to remain active after RemoveAllowedFolder, got %v", st)
	}

	ctrMu.Lock()
	allowedStopped := containers[ids.CtrAllowed].stopped && !containers[ids.CtrAllowed].running
	dataRunning := containers[ids.CtrData].running && !containers[ids.CtrData].stopped
	ctrMu.Unlock()
	if !allowedStopped {
		t.Fatalf("expected ctr_allowed stopped on RemoveAllowedFolder")
	}
	if !dataRunning {
		t.Fatalf("expected ctr_data to remain running on RemoveAllowedFolder")
	}

	// Subsequent SandboxStart and Docker start/restart/unpause on revoked-folder resources must return 403.
	args, _ := json.Marshal(map[string]string{
		"sbx_allowed": ids.SbxAllowed,
		"ctr_allowed": ids.CtrAllowed,
	})
	toolRaw, err := mgr.InvokeTool(context.Background(), "revoke-folder-ext", "probe_after_revoke", args)
	if err != nil {
		t.Fatalf("InvokeTool(probe_after_revoke): %v", err)
	}
	var probe struct {
		SbxStartDenied bool `json:"sbx_start_denied"`
		CtrStartCode   int  `json:"ctr_start_code"`
		CtrRestartCode int  `json:"ctr_restart_code"`
		CtrUnpauseCode int  `json:"ctr_unpause_code"`
	}
	if err := json.Unmarshal(toolRaw, &probe); err != nil {
		t.Fatal(err)
	}
	if !probe.SbxStartDenied || probe.CtrStartCode != 403 || probe.CtrRestartCode != 403 || probe.CtrUnpauseCode != 403 {
		t.Fatalf("unexpected post-revoke probe result: %+v", probe)
	}

	// RevokePermissions stops remaining owned child sandboxes & Docker containers as well.
	if err := mgr.RevokePermissions("revoke-folder-ext"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	if st := sup.State(ids.SbxData).Status; st.Active() {
		t.Fatalf("expected sbx_data stopped after RevokePermissions, got %v", st)
	}
	ctrMu.Lock()
	dataStopped := containers[ids.CtrData].stopped && !containers[ids.CtrData].running
	ctrMu.Unlock()
	if !dataStopped {
		t.Fatalf("expected ctr_data stopped after RevokePermissions")
	}
}

func TestManager_UninstallStopsOwnedDockerContainers(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()

	type mockCtr struct {
		labels    map[string]string
		running   bool
		stopCalls int
	}
	var (
		ctrMu      sync.Mutex
		nextCtrID  int
		containers = map[string]*mockCtr{
			"foreign-ctr": {labels: map[string]string{"io.gvx.extension": "other-ext"}, running: true},
			"host-ctr":    {labels: map[string]string{}, running: true},
		}
	)
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))

		case r.Method == http.MethodPost && p == "/containers/create":
			var req struct {
				Labels map[string]string `json:"Labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ctrMu.Lock()
			nextCtrID++
			cid := fmt.Sprintf("uninstall-ctr-%d", nextCtrID)
			containers[cid] = &mockCtr{labels: req.Labels}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})

		case r.Method == http.MethodGet && p == "/containers/json":
			ctrMu.Lock()
			var list []map[string]any
			for cid, c := range containers {
				if c.running || r.URL.Query().Get("all") == "1" {
					list = append(list, map[string]any{"Id": cid, "Labels": c.labels})
				}
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			ctrMu.Lock()
			c, ok := containers[cid]
			ctrMu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     cid,
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": c.labels},
			})

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = true
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stop"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = false
				c.stopCalls++
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	const sdkSrc = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerDo(ctx context.Context, method, p, body string) (int, []byte, error) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{}
	base := ep
	if strings.HasPrefix(ep, "unix://") {
		sock := strings.TrimPrefix(ep, "unix://")
		tr.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(c, "unix", sock)
		}
		base = "http://docker"
	}
	var r *bytes.Buffer
	if body != "" {
		r = bytes.NewBufferString(body)
	} else {
		r = &bytes.Buffer{}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes(), nil
}

func main() {
	app := extsdk.New()
	app.Tool("start_ctr", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20"}` + "`" + `)
		if e != nil || c != 201 {
			return nil, fmt.Errorf("create: %d %s %v", c, b, e)
		}
		var res struct{ Id string }
		_ = json.Unmarshal(b, &res)
		if sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+res.Id+"/start", ""); se != nil || sc != 204 {
			return nil, fmt.Errorf("start: %d %s %v", sc, sb, se)
		}
		return map[string]string{"id": res.Id}, nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "uninstall-docker-ext"
version = "0.1.0"
publisher = "acme"
name = "Uninstall Docker Ext"
permissions = ["docker"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.tool]]
id = "start_ctr"
description = "Start owned docker container"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: &fakeUIHost{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("uninstall-docker-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "uninstall-docker-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	raw, err := mgr.InvokeTool(context.Background(), "uninstall-docker-ext", "start_ctr", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("InvokeTool(start_ctr): %v", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Uninstall("uninstall-docker-ext", true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	ctrMu.Lock()
	owned := containers[out.ID]
	foreign := containers["foreign-ctr"]
	host := containers["host-ctr"]
	ctrMu.Unlock()
	if owned == nil || owned.running || owned.stopCalls == 0 {
		t.Fatalf("expected owned Docker container stopped on Uninstall, got %+v", owned)
	}
	if !foreign.running || foreign.stopCalls != 0 {
		t.Fatalf("expected foreign container untouched, got %+v", foreign)
	}
	if !host.running || host.stopCalls != 0 {
		t.Fatalf("expected host container untouched, got %+v", host)
	}
}

func TestManager_DockerScopeTOCTOU_RemoveAllowedFolderDuringCreateAndStart(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := newNonReservedTestDir(t)
	extDir := t.TempDir()

	type mockCtr struct {
		labels  map[string]string
		binds   []string
		running bool
		stopped bool
	}
	var (
		ctrMu      sync.Mutex
		nextCtrID  int
		containers = map[string]*mockCtr{
			"ctr-race-1": {
				labels: map[string]string{"io.gvx.extension": "docker-toctou-ext"},
				binds:  []string{allowedDir + ":/work:rw"},
			},
		}
		blockStartOnce  atomic.Bool
		startEntered    = make(chan struct{})
		startRelease    = make(chan struct{})
		blockCreateOnce atomic.Bool
		createEntered   = make(chan struct{})
		createRelease   = make(chan struct{})
	)
	t.Cleanup(func() {
		select {
		case <-startRelease:
		default:
			close(startRelease)
		}
		select {
		case <-createRelease:
		default:
			close(createRelease)
		}
	})

	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))

		case r.Method == http.MethodPost && p == "/containers/create":
			if blockCreateOnce.CompareAndSwap(true, false) {
				close(createEntered)
				<-createRelease
			}
			var req struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					Binds []string `json:"Binds"`
				} `json:"HostConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ctrMu.Lock()
			nextCtrID++
			cid := fmt.Sprintf("ctr-created-%d", nextCtrID)
			containers[cid] = &mockCtr{
				labels: req.Labels,
				binds:  req.HostConfig.Binds,
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})

		case r.Method == http.MethodGet && p == "/containers/json":
			ctrMu.Lock()
			var list []map[string]any
			for cid, c := range containers {
				if c.running || r.URL.Query().Get("all") == "1" {
					list = append(list, map[string]any{"Id": cid, "Labels": c.labels})
				}
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			ctrMu.Lock()
			c, ok := containers[cid]
			ctrMu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         cid,
				"Image":      "alpine:3.20",
				"Config":     map[string]any{"Labels": c.labels},
				"HostConfig": map[string]any{"Binds": c.binds},
			})

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
			if cid == "ctr-race-1" && blockStartOnce.CompareAndSwap(true, false) {
				close(startEntered)
				<-startRelease
			}
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = true
				c.stopped = false
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stop"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = false
				c.stopped = true
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	const sdkSrc = `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	_ = extsdk.New().Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "docker-toctou-ext"
version = "0.1.0"
publisher = "acme"
name = "Docker TOCTOU Ext"
permissions = ["docker", "ui:pick-folder"]

[runtime]
type = "binary"
dev_entrypoint = %q
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: &fakeUIHost{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("docker-toctou-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "docker-toctou-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	mgr.mu.RLock()
	dp := mgr.exts["docker-toctou-ext"].dockerProxy
	mgr.mu.RUnlock()
	if dp == nil {
		t.Fatal("missing scopedDockerProxy")
	}

	// Subtest 1: In-flight POST /containers/ctr-race-1/start serialized against RemoveAllowedFolder.
	if _, err := mgr.grants.AddAllowedFolder("docker-toctou-ext", allowedDir); err != nil {
		t.Fatal(err)
	}
	blockStartOnce.Store(true)
	startCodeCh := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "http://docker/containers/ctr-race-1/start", http.NoBody)
		rec := httptest.NewRecorder()
		dp.ServeHTTP(rec, req)
		startCodeCh <- rec.Code
	}()

	<-startEntered
	removeDone := make(chan error, 1)
	go func() {
		removeDone <- mgr.RemoveAllowedFolder("docker-toctou-ext", allowedDir)
	}()

	select {
	case err := <-removeDone:
		t.Fatalf("RemoveAllowedFolder returned while /containers/ctr-race-1/start held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	close(startRelease)

	if err := <-removeDone; err != nil {
		t.Fatalf("RemoveAllowedFolder: %v", err)
	}
	startCode := <-startCodeCh
	if startCode != http.StatusNoContent {
		t.Fatalf("expected in-flight start to complete 204 before enforcement stopped it, got %d", startCode)
	}
	ctrMu.Lock()
	c1 := containers["ctr-race-1"]
	runningAfterStartRace := c1.running
	stoppedAfterStartRace := c1.stopped
	ctrMu.Unlock()
	if runningAfterStartRace || !stoppedAfterStartRace {
		t.Fatalf("TOCTOU leak: ctr-race-1 remained running=%v stopped=%v after RemoveAllowedFolder returned", runningAfterStartRace, stoppedAfterStartRace)
	}

	// Subtest 2: In-flight POST /containers/create serialized against RemoveAllowedFolder, and subsequent /start is 403.
	if _, err := mgr.grants.AddAllowedFolder("docker-toctou-ext", allowedDir); err != nil {
		t.Fatal(err)
	}
	blockCreateOnce.Store(true)
	type createResult struct {
		code int
		id   string
	}
	createResCh := make(chan createResult, 1)
	go func() {
		body := fmt.Sprintf(`{"Image":"alpine:3.20","HostConfig":{"Binds":[%q]}}`, allowedDir+":/work:rw")
		req := httptest.NewRequest(http.MethodPost, "http://docker/containers/create", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		dp.ServeHTTP(rec, req)
		var out struct{ Id string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		createResCh <- createResult{code: rec.Code, id: out.Id}
	}()

	<-createEntered
	removeDone2 := make(chan error, 1)
	go func() {
		removeDone2 <- mgr.RemoveAllowedFolder("docker-toctou-ext", allowedDir)
	}()

	select {
	case err := <-removeDone2:
		t.Fatalf("RemoveAllowedFolder returned while /containers/create held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	close(createRelease)

	if err := <-removeDone2; err != nil {
		t.Fatalf("RemoveAllowedFolder 2: %v", err)
	}
	cres := <-createResCh
	if cres.code != http.StatusCreated || cres.id == "" {
		t.Fatalf("expected in-flight create to finish 201, got %+v", cres)
	}
	for _, ep := range []string{"start", "restart", "unpause"} {
		req := httptest.NewRequest(http.MethodPost, "http://docker/containers/"+cres.id+"/"+ep, http.NoBody)
		rec := httptest.NewRecorder()
		dp.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected POST /containers/%s/%s after folder revocation to return 403, got %d (%s)", cres.id, ep, rec.Code, rec.Body.String())
		}
	}
	ctrMu.Lock()
	for cid, c := range containers {
		if c.running {
			ctrMu.Unlock()
			t.Fatalf("container %s left running after RemoveAllowedFolder", cid)
		}
	}
	ctrMu.Unlock()
}

func TestManager_HostAPITOCTOU_RemoveAllowedFolderDuringSandboxStartAndRestore(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := newNonReservedTestDir(t)
	extDir := t.TempDir()
	fakeRungvBin := buildMinimalFakeRungv(t)

	var (
		blockLaunchOnce atomic.Bool
		launchMu        sync.Mutex
		launchEntered   chan struct{}
		launchRelease   chan struct{}
	)
	armBarrier := func() (entered <-chan struct{}, release func()) {
		launchMu.Lock()
		launchEntered = make(chan struct{})
		launchRelease = make(chan struct{})
		ent := launchEntered
		rel := launchRelease
		launchMu.Unlock()
		blockLaunchOnce.Store(true)
		var once sync.Once
		return ent, func() {
			once.Do(func() { close(rel) })
		}
	}

	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			if blockLaunchOnce.CompareAndSwap(true, false) {
				launchMu.Lock()
				ent := launchEntered
				rel := launchRelease
				launchMu.Unlock()
				if ent != nil {
					close(ent)
				}
				if rel != nil {
					<-rel
				}
			}
			return spec, nil, nil
		},
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	const sdkSrc = `package main
import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	_ = extsdk.New().Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "hostapi-toctou-ext"
version = "0.1.0"
publisher = "acme"
name = "HostAPI TOCTOU Ext"
permissions = ["sandboxes", "snapshots", "ui:pick-folder"]

[runtime]
type = "binary"
dev_entrypoint = %q
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		Supervisor:    sup,
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("hostapi-toctou-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "hostapi-toctou-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	mgr.mu.RLock()
	hapi := mgr.exts["hostapi-toctou-ext"].hostAPI
	mgr.mu.RUnlock()
	if hapi == nil {
		t.Fatal("missing hostAPIServer")
	}

	doHost := func(method, p string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, "http://host"+p, rdr)
		req.Header.Set("Authorization", "Bearer "+hapi.cfg.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		hapi.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Subtest 1: SandboxCreate(Start=false) then in-flight SandboxStart vs RemoveAllowedFolder.
	if _, err := mgr.grants.AddAllowedFolder("hostapi-toctou-ext", allowedDir); err != nil {
		t.Fatal(err)
	}
	startFalse := false
	createRec := doHost(http.MethodPost, "/v1/sandboxes", extsdk.CreateSandboxRequest{
		Name:   "toctou-sbx-1",
		Image:  "alpine:3.20",
		Mounts: []extsdk.SandboxMount{{Type: "bind", Source: allowedDir, Target: "/work"}},
		Start:  &startFalse,
	})
	if createRec.Code != http.StatusOK {
		t.Fatalf("create stopped sandbox failed: %d %s", createRec.Code, createRec.Body.String())
	}
	var sbx1 extsdk.SandboxInfo
	if err := json.Unmarshal(createRec.Body.Bytes(), &sbx1); err != nil {
		t.Fatal(err)
	}

	entered, release := armBarrier()
	t.Cleanup(release)
	startStatusCh := make(chan int, 1)
	go func() {
		rec := doHost(http.MethodPost, "/v1/sandboxes/"+sbx1.ID+"/start", nil)
		startStatusCh <- rec.Code
	}()

	<-entered
	remDone1 := make(chan error, 1)
	go func() {
		remDone1 <- mgr.RemoveAllowedFolder("hostapi-toctou-ext", allowedDir)
	}()
	select {
	case err := <-remDone1:
		t.Fatalf("RemoveAllowedFolder returned while SandboxStart held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	release()

	if err := <-remDone1; err != nil {
		t.Fatalf("RemoveAllowedFolder(sbxStart): %v", err)
	}
	<-startStatusCh
	if st := sup.State(sbx1.ID).Status; st.Active() {
		t.Fatalf("TOCTOU leak: child sandbox %s remained %v after RemoveAllowedFolder", sbx1.ID, st)
	}

	// Subtest 2: In-flight SandboxCreate(Start=true) vs RemoveAllowedFolder.
	if _, err := mgr.grants.AddAllowedFolder("hostapi-toctou-ext", allowedDir); err != nil {
		t.Fatal(err)
	}
	entered2, release2 := armBarrier()
	t.Cleanup(release2)
	create2Ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		startTrue := true
		rec := doHost(http.MethodPost, "/v1/sandboxes", extsdk.CreateSandboxRequest{
			Name:   "toctou-sbx-2",
			Image:  "alpine:3.20",
			Mounts: []extsdk.SandboxMount{{Type: "bind", Source: allowedDir, Target: "/work"}},
			Start:  &startTrue,
		})
		create2Ch <- rec
	}()

	<-entered2
	remDone2 := make(chan error, 1)
	go func() {
		remDone2 <- mgr.RemoveAllowedFolder("hostapi-toctou-ext", allowedDir)
	}()
	select {
	case err := <-remDone2:
		t.Fatalf("RemoveAllowedFolder returned while SandboxCreate(Start=true) held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	release2()

	if err := <-remDone2; err != nil {
		t.Fatalf("RemoveAllowedFolder(sbxCreate): %v", err)
	}
	rec2 := <-create2Ch
	if rec2.Code == http.StatusOK {
		var sbx2 extsdk.SandboxInfo
		_ = json.Unmarshal(rec2.Body.Bytes(), &sbx2)
		if st := sup.State(sbx2.ID).Status; st.Active() {
			t.Fatalf("TOCTOU leak: SandboxCreate(Start=true) child %s remained %v after RemoveAllowedFolder", sbx2.ID, st)
		}
	}
	for _, runningID := range sup.Running() {
		if runningID != "ext-hostapi-toctou-ext" {
			t.Fatalf("unexpected active child sandbox %s after RemoveAllowedFolder", runningID)
		}
	}

	// Subtest 3: In-flight SandboxRestore vs RemoveAllowedFolder.
	snapDir := filepath.Join(sup.SnapshotRoot(sbx1.ID), "snap-warm")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ExtensionDataDir(dataDir, "hostapi-toctou-ext"), "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ExtensionDataDir(dataDir, "hostapi-toctou-ext"), "snapshots", sbx1.ID+".json"), []byte(`{"warm":"snap-warm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.grants.AddAllowedFolder("hostapi-toctou-ext", allowedDir); err != nil {
		t.Fatal(err)
	}
	entered3, release3 := armBarrier()
	t.Cleanup(release3)
	restoreCh := make(chan int, 1)
	go func() {
		rec := doHost(http.MethodPost, "/v1/sandboxes/"+sbx1.ID+"/restore", extsdk.RestoreRequest{Snapshot: "warm"})
		restoreCh <- rec.Code
	}()

	<-entered3
	remDone3 := make(chan error, 1)
	go func() {
		remDone3 <- mgr.RemoveAllowedFolder("hostapi-toctou-ext", allowedDir)
	}()
	select {
	case err := <-remDone3:
		t.Fatalf("RemoveAllowedFolder returned while SandboxRestore held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	release3()

	if err := <-remDone3; err != nil {
		t.Fatalf("RemoveAllowedFolder(sbxRestore): %v", err)
	}
	<-restoreCh
	if st := sup.State(sbx1.ID).Status; st.Active() {
		t.Fatalf("TOCTOU leak: restored child sandbox %s remained %v after RemoveAllowedFolder", sbx1.ID, st)
	}
}

func TestManagerClose_WaitsForInFlightEphemeralSandboxCreateAfterShutdownTimeout(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()
	fakeRungvBin := buildMinimalFakeRungv(t)

	var (
		slowOnce      atomic.Bool
		launchEntered = make(chan struct{})
	)
	slowOnce.Store(true)

	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			if spec.Name == "slow-eph-sbx" && slowOnce.CompareAndSwap(true, false) {
				close(launchEntered)
				// Exceed hostAPIServer.Close()'s 2s srv.Shutdown timeout so Manager.Close
				// must wait on policyMu.Lock() before running cleanupOwnedSandboxes.
				time.Sleep(2400 * time.Millisecond)
			}
			return spec, nil, nil
		},
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	const sdkSrc = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Tool("spawn_slow_ephemeral", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		startTrue := true
		sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "slow-eph-sbx",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &startTrue,
		})
		if err != nil {
			return nil, err
		}
		return sbx, nil
	})
	_ = app.Run(context.Background())
}
`
	buildTestSDKFixture(t, extDir, sdkSrc, runtime.GOOS, runtime.GOARCH)
	binRel := "bin/ext"
	if runtime.GOOS == "windows" {
		binRel = "bin/ext.exe"
	}
	tomlBody := fmt.Sprintf(`schema_version = 1
id = "close-eph-ext"
version = "0.1.0"
publisher = "acme"
name = "Close Ephemeral Ext"
permissions = ["sandboxes"]

[runtime]
type = "binary"
dev_entrypoint = %q

[[contributes.tool]]
id = "spawn_slow_ephemeral"
description = "Spawn slow ephemeral sandbox"
`, binRel)
	if err := os.WriteFile(filepath.Join(extDir, ManifestName), []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Config{
		DataDir:       dataDir,
		Keychain:      &keychain.Fake{},
		DeveloperMode: func() bool { return true },
		Supervisor:    sup,
		UIHost:        &fakeUIHost{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = mgr.Close(5 * time.Second)
		}
	}()

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("close-eph-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	waitForExtState(t, mgr, "close-eph-ext", 10*time.Second, func(i ExtensionInfo) bool {
		return i.State == StateRunning
	})

	toolDone := make(chan error, 1)
	go func() {
		_, err := mgr.InvokeTool(context.Background(), "close-eph-ext", "spawn_slow_ephemeral", nil)
		toolDone <- err
	}()

	select {
	case <-launchEntered:
	case err := <-toolDone:
		t.Fatalf("spawn_slow_ephemeral exited before Launch: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for slow ephemeral Launch")
	}

	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}
	closed = true

	if running := sup.Running(); len(running) != 0 {
		t.Fatalf("Manager.Close returned with orphaned running ephemeral child sandbox(es): %v", running)
	}
	<-toolDone
}
