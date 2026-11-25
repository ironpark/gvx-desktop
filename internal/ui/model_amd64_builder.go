package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// Amd64CheckState is the result of running the VM helper's requirements check
// (or Rosetta installation) for the experimental amd64 builder.
type Amd64CheckState struct {
	Checked    bool
	Checking   bool
	Installing bool
	Result     vmhelper.CheckResult
	Err        string // failure running the helper itself
}

// Equal makes Amd64CheckState comparable for signals.
func (s Amd64CheckState) Equal(o Amd64CheckState) bool { return s == o }

// OK reports whether all host requirements for the amd64 builder are met.
func (s Amd64CheckState) OK() bool {
	if !s.Checked || s.Err != "" || !s.Result.OK() {
		return false
	}
	if s.Result.MacOS != "" && !macOSAtLeast13(s.Result.MacOS) {
		return false
	}
	return true
}

// MicroVMOK reports whether host requirements for running Linux micro-VM
// containers are met (Apple silicon, macOS 13+, and Virtualization). Rosetta is
// not required so arm64 containers with --runtime=microvm work even when
// Rosetta is not installed; Rosetta is checked separately for amd64 containers.
func (s Amd64CheckState) MicroVMOK() bool {
	if !s.Checked || s.Err != "" || s.Result.Err != "" {
		return false
	}
	if !s.Result.AppleSilicon || !s.Result.Virtualization {
		return false
	}
	if s.Result.MacOS != "" {
		return macOSAtLeast13(s.Result.MacOS)
	}
	return s.Result.Rosetta != vmhelper.RosettaNotSupported
}

func macOSAtLeast13(version string) bool {
	if version == "" {
		return false
	}
	majorStr, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	major, err := strconv.Atoi(majorStr)
	return err == nil && major >= 13
}

type vmHelperRunner func(ctx context.Context, subcmd string) (vmhelper.CheckResult, error)

// defaultVMHelperRunner is the helper runner used by NewModel. Test init stubs
// it so `go test ./internal/ui` never recursively spawns `ui.test __vm-helper`.
var defaultVMHelperRunner vmHelperRunner = defaultRunVMHelper

// defaultRunVMHelper invokes this executable in VM-helper mode so the app
// process never loads Virtualization.framework directly.
func defaultRunVMHelper(ctx context.Context, subcmd string) (vmhelper.CheckResult, error) {
	if runtime.GOOS != "darwin" {
		return vmhelper.CheckResult{Rosetta: vmhelper.RosettaNotSupported}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return vmhelper.CheckResult{}, err
	}
	cmd := exec.CommandContext(ctx, exe, vmhelper.Arg, subcmd)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return vmhelper.CheckResult{}, errors.New(msg)
			}
		}
		return vmhelper.CheckResult{}, err
	}
	var res vmhelper.CheckResult
	if err := json.Unmarshal(out, &res); err != nil {
		return vmhelper.CheckResult{}, err
	}
	return res, nil
}

// CheckAmd64 runs `<exe> __vm-helper check` in the background to check Apple
// silicon, macOS 13+, virtualization entitlement, and Rosetta availability.
func (m *Model) CheckAmd64() {
	if m.Offline {
		return
	}
	cur := ggui.Peek(m.Amd64Check)
	if cur.Checking || cur.Installing {
		return
	}
	m.Amd64Check.Update(func(c Amd64CheckState) Amd64CheckState {
		c.Checking = true
		return c
	})
	run := m.runVMHelper
	var res vmhelper.CheckResult
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var err error
		res, err = run(ctx, vmhelper.CmdCheck)
		return err
	}, func(err error) {
		next := Amd64CheckState{Checked: true, Result: res}
		if err != nil {
			next.Err = firstLine(err.Error())
		}
		m.Amd64Check.Set(next)
	})
}

// InstallRosetta runs `<exe> __vm-helper install-rosetta` so macOS shows its
// Rosetta installation prompt, then refreshes Amd64Check with the result.
func (m *Model) InstallRosetta() {
	if m.Offline {
		return
	}
	cur := ggui.Peek(m.Amd64Check)
	if cur.Installing || cur.Checking {
		return
	}
	m.Amd64Check.Update(func(c Amd64CheckState) Amd64CheckState {
		c.Installing = true
		c.Err = ""
		return c
	})
	run := m.runVMHelper
	var res vmhelper.CheckResult
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		var err error
		res, err = run(ctx, vmhelper.CmdInstallRosetta)
		return err
	}, func(err error) {
		next := Amd64CheckState{Checked: true, Result: res}
		if err != nil {
			next.Err = firstLine(err.Error())
			m.toastError(tr("Rosetta installation failed"), err)
		} else if res.Err != "" {
			m.toastError(tr("Rosetta installation failed"), errors.New(res.Err))
		} else if res.Rosetta == vmhelper.RosettaInstalled {
			m.toastOK(tr("Rosetta installed"), tr("You can now turn on amd64 builds."))
		}
		m.Amd64Check.Set(next)
	})
}

// amd64Config builds the engine's Amd64Config pointing at this executable in
// helper mode and resolving VM assets from the user's cache directory.
func (m *Model) amd64Config() (*engine.Amd64Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &engine.Amd64Config{
		Launcher: vmclient.Launcher{
			Path: exe,
			Args: []string{vmhelper.Arg, vmhelper.CmdServe},
			Log: func(line string) {
				log.Printf("vm-helper: %s", line)
			},
		},
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			cacheDir, err := vmassets.DefaultCacheDir()
			if err != nil {
				return vmassets.Dir{}, err
			}
			return vmassets.Resolve(ctx, vmassets.Options{
				CacheDir: cacheDir,
				Progress: progress,
			})
		},
	}, nil
}

func (m *Model) refreshAmd64State() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.Amd64State.Set(m.sandboxEngine.Amd64State())
}

const (
	rosettaUnknown   int32 = 0
	rosettaMissing   int32 = 1
	rosettaInstalled int32 = 2
)

func (m *Model) watchAmd64Check() {
	ggui.Watch(m.Amd64Check, func(c Amd64CheckState) {
		switch {
		case !c.Checked || c.Err != "":
			m.rosettaState.Store(rosettaUnknown)
		case c.Result.Rosetta == vmhelper.RosettaInstalled:
			m.rosettaState.Store(rosettaInstalled)
		default:
			m.rosettaState.Store(rosettaMissing)
		}
	})
}

const (
	minMicroVMCPUs      = 1
	minMicroVMMemoryMiB = 128
	maxMicroVMMemoryMiB = 65536
)

// microVMHostCPUs returns the maximum selectable vCPU count for a micro-VM.
// Tests may replace it.
var microVMHostCPUs = func() int {
	return max(1, runtime.NumCPU())
}

func clampMicroVMResources(cpus, memMiB int) (int, int) {
	maxCPUs := max(1, microVMHostCPUs())
	if cpus <= 0 {
		cpus = store.DefaultMicroVMCPUs
	}
	cpus = min(max(cpus, minMicroVMCPUs), maxCPUs)
	if memMiB <= 0 {
		memMiB = store.DefaultMicroVMMemoryMiB
	}
	memMiB = min(max(memMiB, minMicroVMMemoryMiB), maxMicroVMMemoryMiB)
	return cpus, memMiB
}

// microVMConfig builds the engine's MicroVMConfig pointing at this executable
// in helper mode, resolving VM assets from the user's cache directory, and
// supplying a thread-safe Rosetta readiness probe and default per-VM resources.
func (m *Model) microVMConfig(set store.Settings) (*engine.MicroVMConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	run := m.runVMHelper
	cpus, memMiB := clampMicroVMResources(set.MicroVMCPUsOrDefault(), set.MicroVMMemoryMiBOrDefault())
	return &engine.MicroVMConfig{
		Launcher: vmclient.Launcher{
			Path: exe,
			Args: []string{vmhelper.Arg, vmhelper.CmdServe},
			Log: func(line string) {
				log.Printf("vm-helper: %s", line)
			},
		},
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			cacheDir, err := vmassets.DefaultCacheDir()
			if err != nil {
				return vmassets.Dir{}, err
			}
			return vmassets.Resolve(ctx, vmassets.Options{
				CacheDir: cacheDir,
				Progress: progress,
			})
		},
		RosettaReady: func() bool {
			switch m.rosettaState.Load() {
			case rosettaInstalled:
				return true
			case rosettaMissing:
				return false
			}
			if run == nil {
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := run(ctx, vmhelper.CmdCheck)
			if err != nil {
				return false
			}
			if res.Rosetta == vmhelper.RosettaInstalled {
				m.rosettaState.Store(rosettaInstalled)
				return true
			}
			return false
		},
		DefaultCPUs:  cpus,
		DefaultMemMB: memMiB,
		Images:       slices.Clone(set.MicroVMImages),
	}, nil
}

func (m *Model) refreshMicroVMState() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.MicroVMState.Set(m.sandboxEngine.MicroVMState())
}

// SetMicroVMContainers turns the experimental per-container Linux micro-VM
// runtime on or off. Turning it on requires the sandbox engine and host
// virtualization support (Apple silicon, macOS 13+, Virtualization); Rosetta is
// NOT required so arm64 containers with --runtime=microvm work immediately.
// Turning it off is always allowed and stops any running micro-VM containers.
func (m *Model) SetMicroVMContainers(on bool) {
	set := ggui.Peek(m.Settings)
	if set.MicroVMContainers == on {
		return
	}
	if on && (!set.SandboxEngine || !ggui.Peek(m.Amd64Check).MicroVMOK()) {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.MicroVMContainers = on })
	if m.Offline || m.sandboxEngine == nil || !ggui.Peek(m.Settings).SandboxEngine {
		return
	}
	m.applyMicroVMConfig(on)
}

// SetMicroVMResources persists the default per-VM CPU and memory limits (clamped
// to valid host bounds) and updates the live engine configuration without
// disrupting running micro-VMs.
func (m *Model) SetMicroVMResources(cpus, memMiB int) {
	cpus, memMiB = clampMicroVMResources(cpus, memMiB)
	cur := ggui.Peek(m.Settings)
	if cur.MicroVMCPUs == cpus && cur.MicroVMMemoryMiB == memMiB {
		return
	}
	if cur.MicroVMCPUs == 0 && cpus == min(store.DefaultMicroVMCPUs, max(1, microVMHostCPUs())) &&
		cur.MicroVMMemoryMiB == 0 && memMiB == store.DefaultMicroVMMemoryMiB {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.MicroVMCPUs = cpus
		s.MicroVMMemoryMiB = memMiB
	})
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	if set := ggui.Peek(m.Settings); set.SandboxEngine && set.MicroVMContainers {
		m.applyMicroVMConfig(true)
	}
}

// AddMicroVMImage registers an image reference or wildcard pattern so containers
// created from matching images automatically run in the Linux micro-VM runtime.
func (m *Model) AddMicroVMImage(raw string) {
	pat := engine.NormalizeMicroVMImagePattern(raw)
	if pat == "" {
		return
	}
	cur := ggui.Peek(m.Settings)
	if slices.Contains(cur.MicroVMImages, pat) {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		if !slices.Contains(s.MicroVMImages, pat) {
			s.MicroVMImages = append(slices.Clone(s.MicroVMImages), pat)
		}
	})
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	if set := ggui.Peek(m.Settings); set.SandboxEngine && set.MicroVMContainers {
		m.applyMicroVMConfig(true)
	}
}

// RemoveMicroVMImage unregisters a micro-VM image rule and updates the live
// engine configuration without disrupting running micro-VMs.
func (m *Model) RemoveMicroVMImage(raw string) {
	pat := engine.NormalizeMicroVMImagePattern(raw)
	if pat == "" {
		pat = strings.TrimSpace(raw)
	}
	cur := ggui.Peek(m.Settings)
	if !slices.Contains(cur.MicroVMImages, pat) && !slices.Contains(cur.MicroVMImages, raw) {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		next := make([]string, 0, len(s.MicroVMImages))
		for _, item := range s.MicroVMImages {
			if item == pat || item == raw {
				continue
			}
			next = append(next, item)
		}
		if len(next) == 0 {
			s.MicroVMImages = nil
		} else {
			s.MicroVMImages = next
		}
	})
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	if set := ggui.Peek(m.Settings); set.SandboxEngine && set.MicroVMContainers {
		m.applyMicroVMConfig(true)
	}
}

func (m *Model) applyMicroVMConfig(on bool) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.microVMGen++
	gen := m.microVMGen
	eng := m.sandboxEngine
	var cfg *engine.MicroVMConfig
	if on {
		var err error
		cfg, err = m.microVMConfig(ggui.Peek(m.Settings))
		if err != nil {
			m.toastError(tr("Could not configure micro-VM runtime"), err)
			return
		}
	}
	m.async(func(context.Context) error {
		return eng.SetMicroVM(cfg)
	}, func(err error) {
		if m.microVMGen != gen {
			return
		}
		m.refreshMicroVMState()
		m.refreshEngineRunning()
		m.refreshEngineContainerRuntimes()
		if err != nil {
			m.toastError(tr("Could not configure micro-VM runtime"), err)
		}
	})
}

func (m *Model) syncAmd64BuildxNode(on bool) {
	if m.syncAmd64NodeHook != nil {
		m.syncAmd64NodeHook(on)
		return
	}
	m.syncAmd64Node(on)
}

// SetAmd64Builds turns the experimental amd64 builder VM on or off. Turning it
// on requires both the sandbox engine and passing host requirements; turning it
// off is always allowed.
func (m *Model) SetAmd64Builds(on bool) {
	set := ggui.Peek(m.Settings)
	if set.Amd64Builds == on {
		return
	}
	if on && (!set.SandboxEngine || !ggui.Peek(m.Amd64Check).OK()) {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.Amd64Builds = on })
	if m.Offline || m.sandboxEngine == nil || !ggui.Peek(m.Settings).SandboxEngine {
		return
	}
	m.applyAmd64Config(on)
}

func (m *Model) applyAmd64Config(on bool) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.amd64Gen++
	gen := m.amd64Gen
	eng := m.sandboxEngine
	var cfg *engine.Amd64Config
	if on {
		var err error
		cfg, err = m.amd64Config()
		if err != nil {
			m.toastError(tr("Could not configure amd64 builds"), err)
			return
		}
	}
	m.async(func(context.Context) error {
		return eng.SetAmd64(cfg)
	}, func(err error) {
		if m.amd64Gen != gen {
			return
		}
		m.refreshAmd64State()
		if err != nil {
			m.toastError(tr("Could not configure amd64 builds"), err)
			return
		}
		set := ggui.Peek(m.Settings)
		m.syncAmd64BuildxNode(on && set.SandboxEngine && set.Amd64Builds)
	})
}

// AskResetAmd64Disk confirms and deletes the amd64 builder's persistent cache
// disk, stopping the VM first if it is running.
func (m *Model) AskResetAmd64Disk() {
	if m.Offline || m.sandboxEngine == nil || ggui.Peek(m.Amd64Resetting) {
		return
	}
	m.Confirm(
		tr("Reset amd64 build cache?"),
		tr("Stops the amd64 builder VM if it is running and deletes its persistent disk image. The next amd64 build starts with an empty cache."),
		tr("Reset build cache"),
		func() {
			if m.sandboxEngine == nil || ggui.Peek(m.Amd64Resetting) {
				return
			}
			eng := m.sandboxEngine
			m.Amd64Resetting.Set(true)
			m.async(func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return eng.ResetAmd64Disk(ctx)
			}, func(err error) {
				m.Amd64Resetting.Set(false)
				m.refreshAmd64State()
				if err != nil {
					m.toastError(tr("Could not reset amd64 build cache"), err)
					return
				}
				m.toastOK(tr("amd64 build cache reset"), "")
			})
		},
	)
}
