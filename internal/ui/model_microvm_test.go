package ui

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func init() {
	// Never let `go test ./internal/ui` recursively spawn `ui.test __vm-helper check`
	// when tests navigate to PageLabs with Offline=false.
	defaultVMHelperRunner = func(context.Context, string) (vmhelper.CheckResult, error) {
		return vmhelper.CheckResult{Rosetta: vmhelper.RosettaNotSupported}, nil
	}
}

func TestAmd64CheckStateMicroVMOK(t *testing.T) {
	cases := []struct {
		name        string
		state       Amd64CheckState
		wantOK      bool
		wantMicroVM bool
	}{
		{
			name:        "unchecked",
			state:       Amd64CheckState{},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "helper error",
			state: Amd64CheckState{
				Checked: true,
				Err:     "helper failed",
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "15.4", Virtualization: true, Rosetta: vmhelper.RosettaInstalled},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "result error",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "15.4", Virtualization: true, Rosetta: vmhelper.RosettaNotInstalled, Err: "vz denied"},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "intel mac",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: false, MacOS: "15.4", Virtualization: true, Rosetta: vmhelper.RosettaNotSupported},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "no virtualization entitlement",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "15.4", Virtualization: false, Rosetta: vmhelper.RosettaInstalled},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "macOS 12 too old",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "12.6", Virtualization: true, Rosetta: vmhelper.RosettaNotSupported},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
		{
			name: "macOS 13 without Rosetta allows micro-VM but not amd64 builder",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "13.0", Virtualization: true, Rosetta: vmhelper.RosettaNotInstalled},
			},
			wantOK:      false,
			wantMicroVM: true,
		},
		{
			name: "macOS 15 with Rosetta allows both",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "15.4", Virtualization: true, Rosetta: vmhelper.RosettaInstalled},
			},
			wantOK:      true,
			wantMicroVM: true,
		},
		{
			name: "empty macOS with RosettaNotSupported rejected",
			state: Amd64CheckState{
				Checked: true,
				Result:  vmhelper.CheckResult{AppleSilicon: true, Virtualization: true, Rosetta: vmhelper.RosettaNotSupported},
			},
			wantOK:      false,
			wantMicroVM: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.OK(); got != tc.wantOK {
				t.Errorf("OK() = %v, want %v", got, tc.wantOK)
			}
			if got := tc.state.MicroVMOK(); got != tc.wantMicroVM {
				t.Errorf("MicroVMOK() = %v, want %v", got, tc.wantMicroVM)
			}
		})
	}
}

func TestMicroVMGatingWithAndWithoutRosettaAndLabsUI(t *testing.T) {
	prevPlatform := amd64BuilderPlatform
	amd64BuilderPlatform = func() bool { return true }
	defer func() { amd64BuilderPlatform = prevPlatform }()

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 2600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}
	switchDisabled := func(name string) bool {
		t.Helper()
		n, ok := p.Semantics().Find(ggui.RoleSwitch, name)
		if !ok {
			t.Fatalf("%q switch not found:\n%s", name, p.Semantics())
		}
		return n.Disabled
	}

	m.Go(PageLabs)
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaNotInstalled,
		},
	})
	p.Frame()

	if !shows("Cross-architecture & micro-VMs") {
		t.Fatalf("Labs missing Cross-architecture & micro-VMs heading:\n%s", p.Semantics())
	}

	// 1. Sandbox engine off: both switches disabled; SetMicroVMContainers(true) ignored.
	if !switchDisabled("amd64 builds with Rosetta") {
		t.Error("amd64 builds switch should be disabled when SandboxEngine is off")
	}
	if !switchDisabled("Micro-VM container runtime") {
		t.Error("Micro-VM switch should be disabled when SandboxEngine is off")
	}
	m.SetMicroVMContainers(true)
	if ggui.Peek(m.Settings).MicroVMContainers {
		t.Error("SetMicroVMContainers(true) should be ignored while SandboxEngine is off")
	}

	// 2. Sandbox engine on, Rosetta NOT installed:
	//    amd64 builds remains disabled, while Micro-VM container runtime IS enabled!
	m.SetSandboxEngine(true)
	p.Frame()
	if !switchDisabled("amd64 builds with Rosetta") {
		t.Error("amd64 builds switch should remain disabled without Rosetta")
	}
	if switchDisabled("Micro-VM container runtime") {
		t.Error("Micro-VM switch should be enabled without Rosetta for arm64 --runtime=microvm")
	}

	// Default resource fields show apple/container defaults (4 CPUs, 1024 MiB).
	if !shows("Default resources per VM") {
		t.Errorf("Labs missing Default resources per VM:\n%s", p.Semantics())
	}

	// Turning Micro-VM ON succeeds without Rosetta and shows idle status + Rosetta hint.
	m.SetMicroVMContainers(true)
	p.Frame()
	if !ggui.Peek(m.Settings).MicroVMContainers {
		t.Fatal("SetMicroVMContainers(true) did not enable setting without Rosetta")
	}
	for _, want := range []string{
		"Micro-VM runtime: idle",
		"VM assets download on the first micro-VM container.",
		"arm64 containers with --runtime=microvm are ready; install Rosetta to also run linux/amd64 containers.",
	} {
		if !shows(want) {
			t.Errorf("Labs micro-VM status card missing %q:\n%s", want, p.Semantics())
		}
	}

	// Running micro-VMs + VM assets v2 + Rosetta installed hides Rosetta hint.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})
	m.MicroVMState.Set(engine.MicroVMState{
		Enabled:    true,
		RunningVMs: 2,
		Assets:     "2",
	})
	p.Frame()
	for _, want := range []string{
		"2 micro-VMs running",
		"VM assets: v2 downloaded",
	} {
		if !shows(want) {
			t.Errorf("Labs micro-VM running state missing %q:\n%s", want, p.Semantics())
		}
	}
	if shows("install Rosetta to also run linux/amd64 containers") {
		t.Errorf("Rosetta hint should hide once Rosetta is installed:\n%s", p.Semantics())
	}

	// Download progress and error rendering.
	m.MicroVMState.Set(engine.MicroVMState{
		Enabled:     true,
		Downloading: true,
		Done:        24_000_000,
		Total:       74_000_000,
		Err:         "network unreachable",
	})
	p.Frame()
	for _, want := range []string{
		"Downloading the VM, 24 MB of 74 MB",
		"Micro-VM runtime error: network unreachable",
	} {
		if !shows(want) {
			t.Errorf("Labs micro-VM downloading/error state missing %q:\n%s", want, p.Semantics())
		}
	}

	// Turning OFF is always allowed even if host requirements later fail.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result:  vmhelper.CheckResult{AppleSilicon: false, Rosetta: vmhelper.RosettaNotSupported},
	})
	p.Frame()
	if switchDisabled("Micro-VM container runtime") {
		t.Error("turning Micro-VM container runtime OFF must stay enabled")
	}
	m.SetMicroVMContainers(false)
	p.Frame()
	if ggui.Peek(m.Settings).MicroVMContainers {
		t.Fatal("SetMicroVMContainers(false) did not turn off setting")
	}

	// macOS < 13 disables turning Micro-VM back ON.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result:  vmhelper.CheckResult{AppleSilicon: true, MacOS: "12.6", Virtualization: true, Rosetta: vmhelper.RosettaNotSupported},
	})
	p.Frame()
	if !switchDisabled("Micro-VM container runtime") {
		t.Error("Micro-VM switch should be disabled on macOS < 13")
	}
	m.SetMicroVMContainers(true)
	if ggui.Peek(m.Settings).MicroVMContainers {
		t.Error("SetMicroVMContainers(true) should be ignored on macOS < 13")
	}
}

func TestMicroVMLifecycleSandboxToggleAndResourceClamping(t *testing.T) {
	prevCPUs := microVMHostCPUs
	microVMHostCPUs = func() int { return 8 }
	defer func() { microVMHostCPUs = prevCPUs }()

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{}
	m.sandboxEngine = fake
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaNotInstalled,
		},
	})

	waitConfigs := func(wantLen int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			fake.mu.Lock()
			n := len(fake.microVMConfigs)
			fake.mu.Unlock()
			if n >= wantLen {
				p.Frame()
				return
			}
			p.Frame()
			time.Sleep(10 * time.Millisecond)
		}
		fake.mu.Lock()
		n := len(fake.microVMConfigs)
		fake.mu.Unlock()
		t.Fatalf("timed out waiting for %d SetMicroVM call(s), got %d", wantLen, n)
	}

	// 1. Toggle ON without Rosetta: calls SetMicroVM with defaults (4 CPUs, 1024 MiB).
	m.SetMicroVMContainers(true)
	waitConfigs(1)

	fake.mu.Lock()
	cfg := fake.microVMConfigs[0]
	fake.mu.Unlock()
	if cfg == nil {
		t.Fatal("SetMicroVM called with nil on toggle ON")
	}
	if cfg.Launcher.Path == "" || !slices.Equal(cfg.Launcher.Args, []string{vmhelper.Arg, vmhelper.CmdServe}) {
		t.Errorf("Launcher = %+v, want executable with [%s %s]", cfg.Launcher, vmhelper.Arg, vmhelper.CmdServe)
	}
	if cfg.Assets == nil {
		t.Error("MicroVMConfig.Assets is nil")
	}
	if cfg.DefaultCPUs != 4 || cfg.DefaultMemMB != 1024 {
		t.Errorf("default resources = (%d CPUs, %d MiB), want (4, 1024)", cfg.DefaultCPUs, cfg.DefaultMemMB)
	}
	if cfg.RosettaReady == nil {
		t.Fatal("MicroVMConfig.RosettaReady is nil")
	}

	// Verify RosettaReady is safe to call off the UI goroutine (ggui_debug + -race)
	// and tracks Amd64Check changes dynamically.
	rosettaFromGoroutine := func() bool {
		ch := make(chan bool, 1)
		go func() { ch <- cfg.RosettaReady() }()
		return <-ch
	}
	if rosettaFromGoroutine() {
		t.Fatal("RosettaReady() = true while Rosetta is not installed")
	}
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})
	p.Frame()
	if !rosettaFromGoroutine() {
		t.Fatal("RosettaReady() = false after Rosetta became installed")
	}

	// 2. Resource validation & clamping + live SetMicroVM update.
	m.SetMicroVMResources(2, 2048)
	waitConfigs(2)
	fake.mu.Lock()
	cfg2 := fake.microVMConfigs[1]
	fake.mu.Unlock()
	if cfg2 == nil || cfg2.DefaultCPUs != 2 || cfg2.DefaultMemMB != 2048 {
		t.Fatalf("after SetMicroVMResources(2, 2048): cfg = %+v", cfg2)
	}
	if saved, _ := st.Settings(); saved.MicroVMCPUs != 2 || saved.MicroVMMemoryMiB != 2048 {
		t.Errorf("persisted resources = (%d, %d), want (2, 2048)", saved.MicroVMCPUs, saved.MicroVMMemoryMiB)
	}

	// Clamp above host CPUs (99 -> 8) and below min memory (64 -> 128).
	m.SetMicroVMResources(99, 64)
	waitConfigs(3)
	fake.mu.Lock()
	cfg3 := fake.microVMConfigs[2]
	fake.mu.Unlock()
	if cfg3 == nil || cfg3.DefaultCPUs != 8 || cfg3.DefaultMemMB != 128 {
		t.Fatalf("after clamping (99, 64): cfg = %+v, want (8, 128)", cfg3)
	}

	// Negative / huge values fall back to default CPUs (4) and cap memory (65536).
	m.SetMicroVMResources(-3, 999999)
	waitConfigs(4)
	fake.mu.Lock()
	cfg4 := fake.microVMConfigs[3]
	fake.mu.Unlock()
	if cfg4 == nil || cfg4.DefaultCPUs != 4 || cfg4.DefaultMemMB != 65536 {
		t.Fatalf("after clamping (-3, 999999): cfg = %+v, want (4, 65536)", cfg4)
	}

	// 3. Turning SandboxEngine OFF stops micro-VMs while preserving MicroVMContainers setting.
	fake.mu.Lock()
	fake.microVMState.RunningVMs = 2
	fake.mu.Unlock()
	m.SetSandboxEngine(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, closes, _, _ := fake.counts()
		if closes >= 1 && ggui.Peek(m.MicroVMState).RunningVMs == 0 {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	if !ggui.Peek(m.Settings).MicroVMContainers {
		t.Fatal("closing sandbox engine cleared Settings.MicroVMContainers")
	}
	if got := ggui.Peek(m.MicroVMState).RunningVMs; got != 0 {
		t.Fatalf("MicroVMState.RunningVMs after close = %d, want 0", got)
	}

	// 4. Turning SandboxEngine back ON reapplies SetMicroVM with preserved resources.
	m.SetSandboxEngine(true)
	waitConfigs(5)
	fake.mu.Lock()
	cfg5 := fake.microVMConfigs[4]
	fake.mu.Unlock()
	if cfg5 == nil || cfg5.DefaultCPUs != 4 || cfg5.DefaultMemMB != 65536 {
		t.Fatalf("after reopening SandboxEngine: cfg = %+v, want non-nil (4, 65536)", cfg5)
	}

	// 5. Turning MicroVMContainers OFF calls SetMicroVM(nil) and persists false.
	m.SetMicroVMContainers(false)
	waitConfigs(6)
	fake.mu.Lock()
	cfg6 := fake.microVMConfigs[5]
	fake.mu.Unlock()
	if cfg6 != nil {
		t.Fatalf("after toggle OFF: SetMicroVM cfg = %+v, want nil", cfg6)
	}
	if saved, _ := st.Settings(); saved.MicroVMContainers {
		t.Error("MicroVMContainers=false was not persisted")
	}
}

func TestMicroVMRosettaReadyFallbackWhenUnchecked(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.SandboxEngine = true
	set.MicroVMContainers = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(800, 600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	var mu sync.Mutex
	calls := 0
	installed := false
	m.runVMHelper = func(_ context.Context, subcmd string) (vmhelper.CheckResult, error) {
		mu.Lock()
		calls++
		rosetta := vmhelper.RosettaNotInstalled
		if installed {
			rosetta = vmhelper.RosettaInstalled
		}
		mu.Unlock()
		return vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        rosetta,
		}, nil
	}

	cfg, err := m.microVMConfig(ggui.Peek(m.Settings))
	if err != nil {
		t.Fatalf("microVMConfig: %v", err)
	}
	probeOffUI := func() bool {
		ch := make(chan bool, 1)
		go func() { ch <- cfg.RosettaReady() }()
		return <-ch
	}

	// 1. Before Rosetta is installed, fallback probe returns false and does NOT
	//    permanently cache the negative result.
	if probeOffUI() {
		t.Fatal("RosettaReady() returned true while Rosetta was not installed")
	}

	// 2. After installing Rosetta (e.g. via Terminal), retry re-probes and succeeds.
	mu.Lock()
	installed = true
	mu.Unlock()
	if !probeOffUI() {
		t.Fatal("RosettaReady() returned false after Rosetta became installed (negative result was cached)")
	}

	// 3. Subsequent calls use the cached positive state without re-probing.
	if !probeOffUI() {
		t.Fatal("cached RosettaReady() returned false")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("runVMHelper called %d times, want 2 (1 negative + 1 positive then cached)", gotCalls)
	}
}

func TestImagesPageMicroVMActions(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	set.MicroVMContainers = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.DockerImages.Set(Listing[dockerapi.Image]{
		Loaded: true,
		Items: []dockerapi.Image{
			{
				ID: "sha256:aaaa111122223333", RepoTags: []string{"arm-app:1"}, Size: 10 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:aaaa", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: "arm64", OS: "linux"}},
				}},
			},
			{
				ID: "sha256:bbbb111122223333", RepoTags: []string{"x86-app:1"}, Size: 20 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:bbbb", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: "amd64", OS: "linux"}},
				}},
			},
		},
	})
	m.GoImages(ImagesDocker)
	p.Frame()

	if runtime.GOARCH == "arm64" {
		// On Apple Silicon (arm64 host) with MicroVMContainers ON:
		// 1. amd64 image offers Copy docker run command (--platform linux/amd64) while Run… stays disabled.
		p.Tap("More actions for x86-app:1")
		p.Frame()
		if runItem, ok := p.Semantics().Find(ggui.RoleMenuItem, "Run…"); !ok || !runItem.Disabled {
			t.Errorf("amd64 image Run… menu item should stay disabled (ok=%v, disabled=%v)", ok, runItem.Disabled)
		}
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Copy docker run command"); !ok {
			t.Fatalf("amd64 image menu missing Copy docker run command when MicroVMContainers is ON:\n%s", p.Semantics())
		}
		p.Tap("Copy docker run command")
		p.Frame()
		if got, want := p.Clipboard().Read(), "docker run --rm -it --platform linux/amd64 x86-app:1"; got != want {
			t.Errorf("amd64 copied command = %q, want %q", got, want)
		}

		// 2. arm64 image offers both standard and --runtime=microvm copy commands.
		p.Tap("More actions for arm-app:1")
		p.Frame()
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Copy docker run command"); !ok {
			t.Errorf("arm64 image menu missing Copy docker run command:\n%s", p.Semantics())
		}
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Copy docker run (--runtime=microvm)"); !ok {
			t.Fatalf("arm64 image menu missing Copy docker run (--runtime=microvm) when MicroVMContainers is ON:\n%s", p.Semantics())
		}
		p.Tap("Copy docker run (--runtime=microvm)")
		p.Frame()
		if got, want := p.Clipboard().Read(), "docker run --rm -it --runtime=microvm arm-app:1"; got != want {
			t.Errorf("arm64 microvm copied command = %q, want %q", got, want)
		}

		// 3. With MicroVMContainers OFF, neither micro-VM copy item is offered.
		m.SetMicroVMContainers(false)
		p.Frame()
		p.Tap("More actions for arm-app:1")
		p.Frame()
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Copy docker run (--runtime=microvm)"); ok {
			t.Errorf("arm64 image menu unexpectedly offered --runtime=microvm when MicroVMContainers is OFF:\n%s", p.Semantics())
		}
	}
}

func TestContainersPageMicroVMAndAmd64Badges(t *testing.T) {
	// Unit-test badge classification helper directly.
	runtimes := map[string]engine.ContainerRuntimeInfo{
		"ctr-gvx": {Runtime: engine.RuntimeGVX, Platform: "linux/arm64"},
		"ctr-arm": {Runtime: engine.RuntimeMicroVM, Platform: "linux/arm64"},
		"ctr-x86": {Runtime: engine.RuntimeMicroVM, Platform: "linux/amd64"},
	}
	if v := containerRuntimeBadgeView(dockerapi.Container{ID: "ctr-gvx"}, runtimes); v.microVM || v.amd64 {
		t.Errorf("gvx container badge = %+v, want none", v)
	}
	if v := containerRuntimeBadgeView(dockerapi.Container{ID: "ctr-arm"}, runtimes); !v.microVM || v.amd64 {
		t.Errorf("arm64 microvm container badge = %+v, want microVM only", v)
	}
	if v := containerRuntimeBadgeView(dockerapi.Container{ID: "ctr-x86"}, runtimes); !v.microVM || !v.amd64 {
		t.Errorf("amd64 microvm container badge = %+v, want microVM+amd64", v)
	}
	if v := containerRuntimeBadgeView(dockerapi.Container{ID: "ctr-lbl", Labels: map[string]string{"gvx.runtime": engine.RuntimeMicroVM}}, nil); !v.microVM || v.amd64 {
		t.Errorf("label fallback badge = %+v, want microVM only", v)
	}

	// Render Containers page and verify micro-VM and amd64 badges appear.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	set.MicroVMContainers = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 900))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.EngineContainerRuntimes.Set(runtimes)
	m.DockerContainers.Set(Listing[dockerapi.Container]{
		Loaded: true,
		Items: []dockerapi.Container{
			{ID: "ctr-gvx", Names: []string{"/web-gvx"}, Image: "nginx:alpine", State: "running", Status: "Up 2m"},
			{ID: "ctr-arm", Names: []string{"/db-arm"}, Image: "alpine:latest", State: "running", Status: "Up 1m"},
			{ID: "ctr-x86", Names: []string{"/legacy-x86"}, Image: "amd64/alpine:latest", State: "running", Status: "Up 30s"},
		},
	})
	m.Go(PageContainers)
	p.Frame()

	microVMCount := 0
	amd64Count := 0
	for _, n := range p.Semantics().All() {
		if n.Name == "micro-VM" {
			microVMCount++
		}
		if n.Name == "amd64" {
			amd64Count++
		}
	}
	if microVMCount < 2 {
		t.Errorf("expected at least 2 micro-VM badges (for db-arm and legacy-x86), got %d:\n%s", microVMCount, p.Semantics())
	}
	if amd64Count < 1 {
		t.Errorf("expected at least 1 amd64 badge (for legacy-x86), got %d:\n%s", amd64Count, p.Semantics())
	}
}

func TestRefreshAllOnLabsRefreshesMicroVMState(t *testing.T) {
	prevPlatform := amd64BuilderPlatform
	amd64BuilderPlatform = func() bool { return true }
	defer func() { amd64BuilderPlatform = prevPlatform }()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GVX_RUNGV", "")

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.SandboxEngine = true
	set.MicroVMContainers = true
	set.RungvPath = dir + "/missing-rungv"
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{
		microVMState: engine.MicroVMState{Enabled: true, RunningVMs: 3, Assets: "2"},
	}
	m.sandboxEngine = fake
	m.Page.Set(PageLabs)
	m.RefreshAll()
	p.Frame()

	got := ggui.Peek(m.MicroVMState)
	if !got.Enabled || got.RunningVMs != 3 || got.Assets != "2" {
		t.Fatalf("after RefreshAll on PageLabs: MicroVMState = %+v, want Enabled=true RunningVMs=3 Assets=2", got)
	}
}

func TestMicroVMImagesRegistrationPersistenceLiveSyncAndUI(t *testing.T) {
	prevPlatform := amd64BuilderPlatform
	amd64BuilderPlatform = func() bool { return true }
	defer func() { amd64BuilderPlatform = prevPlatform }()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GVX_RUNGV", "")

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	set.MicroVMContainers = true
	set.RungvPath = dir + "/missing-rungv"
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 2600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{
		microVMState: engine.MicroVMState{Enabled: true, Assets: "2"},
	}
	m.sandboxEngine = fake

	// 1. Register image rules -> normalized, deduplicated, persisted, and synced live to engine.
	m.AddMicroVMImage("  Postgres:16 ")
	extUISettle(t, p, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.microVMConfigs) >= 1
	}, "SetMicroVM synced after first AddMicroVMImage")

	m.AddMicroVMImage("postgres:16")          // duplicate ignored
	m.AddMicroVMImage("bad image with space") // invalid ignored
	m.AddMicroVMImage("ghcr.io/ironpark/*")
	extUISettle(t, p, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.microVMConfigs) >= 2
	}, "SetMicroVM synced after second AddMicroVMImage")

	saved, err := st.Settings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	wantImages := []string{"postgres:16", "ghcr.io/ironpark/*"}
	if !slices.Equal(saved.MicroVMImages, wantImages) {
		t.Fatalf("persisted MicroVMImages = %v, want %v", saved.MicroVMImages, wantImages)
	}
	fake.mu.Lock()
	lastCfg := fake.microVMConfigs[len(fake.microVMConfigs)-1]
	fake.mu.Unlock()
	if lastCfg == nil || !slices.Equal(lastCfg.Images, wantImages) {
		t.Fatalf("live SetMicroVM Images = %+v, want %v", lastCfg, wantImages)
	}

	// 2. Labs page displays the registered patterns and remove controls.
	m.Page.Set(PageLabs)
	p.Frame()
	sem := p.Semantics().String()
	if !strings.Contains(sem, "Images that always run in a micro-VM") ||
		!strings.Contains(sem, "postgres:16") ||
		!strings.Contains(sem, "ghcr.io/ironpark/*") {
		t.Fatalf("Labs page missing micro-VM image rules:\n%s", sem)
	}

	// 3. Images page shows micro-VM badge for matching host-arch images and menu toggle.
	m.Offline = true
	m.DockerImages.Set(Listing[dockerapi.Image]{
		Loaded: true,
		Items: []dockerapi.Image{
			{
				ID: "sha256:aaa111122223333", RepoTags: []string{"postgres:16"}, Size: 10 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:aaa", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: runtime.GOARCH, OS: "linux"}},
				}},
			},
			{
				ID: "sha256:bbb111122223333", RepoTags: []string{"redis:7"}, Size: 10 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:bbb", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: runtime.GOARCH, OS: "linux"}},
				}},
			},
		},
	})
	m.GoImages(ImagesDocker)
	p.Frame()
	if !strings.Contains(p.Semantics().String(), "micro-VM") {
		t.Fatalf("Images page missing micro-VM badge for registered postgres:16:\n%s", p.Semantics())
	}
	if runtime.GOARCH == "arm64" {
		p.Tap("More actions for postgres:16")
		p.Frame()
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Stop running in micro-VM"); !ok {
			t.Fatalf("registered image menu missing Stop running in micro-VM:\n%s", p.Semantics())
		}
		p.Tap("Copy reference")
		p.Frame()
		p.Tap("More actions for redis:7")
		p.Frame()
		if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Always run in micro-VM"); !ok {
			t.Fatalf("unregistered image menu missing Always run in micro-VM:\n%s", p.Semantics())
		}
		p.Tap("Copy reference")
		p.Frame()
	}
	m.Offline = false

	// 4. RemoveMicroVMImage removes rule, persists, and pushes live SetMicroVM.
	fake.mu.Lock()
	prevCalls := len(fake.microVMConfigs)
	fake.mu.Unlock()
	m.RemoveMicroVMImage("postgres:16")
	extUISettle(t, p, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.microVMConfigs) > prevCalls
	}, "SetMicroVM synced after RemoveMicroVMImage")

	saved, _ = st.Settings()
	if !slices.Equal(saved.MicroVMImages, []string{"ghcr.io/ironpark/*"}) {
		t.Fatalf("after remove MicroVMImages = %v, want [ghcr.io/ironpark/*]", saved.MicroVMImages)
	}
}
