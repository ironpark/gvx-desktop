package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/overlay"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	// DefaultMicroVMCPUs matches apple/container's per-container default vCPU
	// count (clamped to runtime.NumCPU()).
	DefaultMicroVMCPUs = 4
	// DefaultMicroVMMemoryMiB matches apple/container's per-container default
	// RAM allocation (1 GiB).
	DefaultMicroVMMemoryMiB = 1024

	// RuntimeGVX is the default gVisor sandbox runtime (rungv).
	RuntimeGVX = "gvx"
	// RuntimeMicroVM is the per-container Linux micro-VM runtime.
	RuntimeMicroVM = "microvm"

	microVMDirName             = "microvm"
	microVMRootfsDirName       = "rootfs"
	defaultMicroVMRootfsSize   = 16 << 30 // 16 GiB sparse ext4 per container
	minMicroVMMemoryMiB        = 128
	maxFastpathEROFSLayers     = 8
	maxOverflowEROFSLayers     = 128
	maxMicroVMVolumes          = 8
	maxMicroVMBindShares       = 12
	defaultMicroVMStartTimeout = 2 * time.Minute
	defaultMicroVMStopGrace    = 10 * time.Second
	microVMHelperCloseGrace    = 15 * time.Second
	microVMDialTimeout         = 10 * time.Second
)

// MicroVMConfig turns on the per-container Linux micro-VM runtime (Labs, Apple
// silicon macOS). Zero resource fields take the defaults (min(4, NumCPU) vCPUs,
// 1024 MiB RAM).
type MicroVMConfig struct {
	// Launcher starts the per-container VM helper (`<exe> __vm-helper serve`).
	// Its Dir is left empty here; the engine creates a short temp dir per VM.
	Launcher vmclient.Launcher
	// Assets resolves the VM assets (vm-assets-v2). It may download on first
	// use, reporting progress into MicroVMState.
	Assets func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error)
	// RosettaReady reports whether Rosetta for Linux is installed on the host.
	// Checked only when running linux/amd64 containers; nil assumes ready.
	RosettaReady func() bool
	// DefaultCPUs is the default vCPU count per micro-VM (0 -> min(4, NumCPU)).
	DefaultCPUs int
	// DefaultMemMB is the default memory in MiB per micro-VM (0 -> 1024).
	DefaultMemMB int
	// Images lists image references or wildcard patterns that automatically run
	// in the micro-VM runtime when no explicit --runtime flag overrides it.
	Images []string
}

// MicroVMState reports the micro-VM runtime's live status for the UI.
type MicroVMState struct {
	Enabled     bool
	RunningVMs  int
	Downloading bool
	Done, Total int64
	Assets      string // "2", "local"
	Err         string
}

// Equal makes MicroVMState comparable for reactive UI signals.
func (s MicroVMState) Equal(o MicroVMState) bool { return s == o }

// ContainerRuntimeInfo describes a container's execution runtime and platform.
type ContainerRuntimeInfo struct {
	Runtime  string // "gvx" (or "") or "microvm"
	Platform string // "linux/arm64", "linux/amd64", or ""
}

// Equal makes ContainerRuntimeInfo comparable.
func (r ContainerRuntimeInfo) Equal(o ContainerRuntimeInfo) bool { return r == o }

func (c Container) isMicroVM() bool {
	return c.Runtime == RuntimeMicroVM
}

func cloneMicroVMConfig(cfg *MicroVMConfig) *MicroVMConfig {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	if cfg.Launcher.Args != nil {
		cp.Launcher.Args = slices.Clone(cfg.Launcher.Args)
	}
	if cfg.Images != nil {
		cp.Images = slices.Clone(cfg.Images)
	}
	return &cp
}

func (e *Engine) matchesMicroVMImage(ref string, repoTags []string) bool {
	e.mvm.mu.Lock()
	var patterns []string
	if e.mvm.cfg != nil {
		patterns = e.mvm.cfg.Images
	}
	e.mvm.mu.Unlock()
	if len(patterns) == 0 {
		return false
	}
	if MatchMicroVMImage(patterns, ref) {
		return true
	}
	for _, tag := range repoTags {
		if MatchMicroVMImage(patterns, tag) {
			return true
		}
	}
	return false
}

// microVMStream is the subset of *vmclient.ContainerStream used by microvm.go.
type microVMStream interface {
	PID() int
	State() *vmproto.ContainerState
	Stdin() io.WriteCloser
	WriteStdin(p []byte) (int, error)
	CloseStdin() error
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(rows, cols uint16) error
	Wait() (vmproto.ContainerExit, error)
	Close() error
}

// microVMNetStream is the subset of *vmclient.ContainerNetStream used by microvm.go.
type microVMNetStream interface {
	ReadFrame() (kind byte, payload []byte, err error)
	SendPacket(pkt []byte) error
	SendDNSReply(seq uint32, reply []byte) error
	Close() error
}

// microVMClient is the subset of *vmclient.Client that microVMState drives.
// Unit tests replace launchFn to return a fake implementation.
type microVMClient interface {
	Socket() string
	Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error)
	Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error)
	Done() <-chan struct{}
	ExitErr() error
	Close(timeout time.Duration) error

	ContainerStart(ctx context.Context, spec vmproto.ContainerSpec) (vmproto.ContainerState, error)
	ContainerAttach(ctx context.Context) (microVMStream, error)
	ContainerSignal(ctx context.Context, req vmproto.ContainerSignalRequest) error
	ContainerResize(ctx context.Context, rows, cols uint16) error
	ContainerExec(ctx context.Context, req vmproto.ContainerExecRequest) (microVMStream, error)
	ContainerTop(ctx context.Context) ([]vmproto.ContainerProcess, error)
	ContainerStats(ctx context.Context) (vmproto.ContainerStats, error)
	ContainerPause(ctx context.Context) error
	ContainerUnpause(ctx context.Context) error
	ContainerStat(ctx context.Context, req vmproto.ContainerCopyRequest) (vmproto.ContainerPathStat, error)
	ContainerCopyFrom(ctx context.Context, req vmproto.ContainerCopyRequest) (io.ReadCloser, error)
	ContainerCopyTo(ctx context.Context, req vmproto.ContainerCopyRequest, r io.Reader) error
	ContainerExport(ctx context.Context, req vmproto.ContainerExportRequest) (io.ReadCloser, error)
	ContainerNetAttach(ctx context.Context, cfg vmproto.ContainerNetConfig) (microVMNetStream, error)
	ContainerNetUpdate(ctx context.Context, cfg vmproto.ContainerNetConfig) error
}

type realMicroVMClient struct {
	c *vmclient.Client
}

func (r realMicroVMClient) Socket() string { return r.c.Socket() }
func (r realMicroVMClient) Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error) {
	return r.c.Start(ctx, spec)
}
func (r realMicroVMClient) Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error) {
	return r.c.Dial(ctx, req)
}
func (r realMicroVMClient) Done() <-chan struct{}             { return r.c.Done() }
func (r realMicroVMClient) ExitErr() error                    { return r.c.ExitErr() }
func (r realMicroVMClient) Close(timeout time.Duration) error { return r.c.Close(timeout) }
func (r realMicroVMClient) ContainerStart(ctx context.Context, spec vmproto.ContainerSpec) (vmproto.ContainerState, error) {
	return r.c.ContainerStart(ctx, spec)
}
func (r realMicroVMClient) ContainerAttach(ctx context.Context) (microVMStream, error) {
	return r.c.ContainerAttach(ctx)
}
func (r realMicroVMClient) ContainerSignal(ctx context.Context, req vmproto.ContainerSignalRequest) error {
	return r.c.ContainerSignal(ctx, req)
}
func (r realMicroVMClient) ContainerResize(ctx context.Context, rows, cols uint16) error {
	return r.c.ContainerResize(ctx, rows, cols)
}
func (r realMicroVMClient) ContainerExec(ctx context.Context, req vmproto.ContainerExecRequest) (microVMStream, error) {
	return r.c.ContainerExec(ctx, req)
}
func (r realMicroVMClient) ContainerTop(ctx context.Context) ([]vmproto.ContainerProcess, error) {
	return r.c.ContainerTop(ctx)
}
func (r realMicroVMClient) ContainerStats(ctx context.Context) (vmproto.ContainerStats, error) {
	return r.c.ContainerStats(ctx)
}
func (r realMicroVMClient) ContainerPause(ctx context.Context) error {
	return r.c.ContainerPause(ctx)
}
func (r realMicroVMClient) ContainerUnpause(ctx context.Context) error {
	return r.c.ContainerUnpause(ctx)
}
func (r realMicroVMClient) ContainerStat(ctx context.Context, req vmproto.ContainerCopyRequest) (vmproto.ContainerPathStat, error) {
	return r.c.ContainerStat(ctx, req)
}
func (r realMicroVMClient) ContainerCopyFrom(ctx context.Context, req vmproto.ContainerCopyRequest) (io.ReadCloser, error) {
	return r.c.ContainerCopyFrom(ctx, req)
}
func (r realMicroVMClient) ContainerCopyTo(ctx context.Context, req vmproto.ContainerCopyRequest, rd io.Reader) error {
	return r.c.ContainerCopyTo(ctx, req, rd)
}
func (r realMicroVMClient) ContainerExport(ctx context.Context, req vmproto.ContainerExportRequest) (io.ReadCloser, error) {
	return r.c.ContainerExport(ctx, req)
}
func (r realMicroVMClient) ContainerNetAttach(ctx context.Context, cfg vmproto.ContainerNetConfig) (microVMNetStream, error) {
	return r.c.ContainerNetAttach(ctx, cfg)
}
func (r realMicroVMClient) ContainerNetUpdate(ctx context.Context, cfg vmproto.ContainerNetConfig) error {
	return r.c.ContainerNetUpdate(ctx, cfg)
}

type microVMVolRef struct {
	exclusive bool
	shared    int
}

// microVMState owns the per-container micro-VM runtime configuration, memoized
// VM assets, and live VM instances.
// Lock order: assetMu -> mu. Never call e.notify() or acquire e.ctr.mu while
// holding either.
type microVMState struct {
	assetMu sync.Mutex

	mu          sync.Mutex
	cfg         *MicroVMConfig
	engineOpen  bool
	disableGen  uint64
	runCtx      context.Context
	runCancel   context.CancelFunc
	assets      *vmassets.Dir
	assetsLabel string
	downloading bool
	dlDone      int64
	dlTotal     int64
	lastErr     string
	instances   map[string]*microVMInstance
	volRefs     map[string]*microVMVolRef

	// launchFn overrides Launcher.Launch in unit tests.
	launchFn func(ctx context.Context, l vmclient.Launcher) (microVMClient, error)
}

func (e *Engine) ensureMicroVMRunCtxLocked() context.Context {
	if e.mvm.runCtx == nil {
		e.mvm.runCtx, e.mvm.runCancel = context.WithCancel(context.Background())
	}
	return e.mvm.runCtx
}

func (e *Engine) cancelMicroVMRunCtxLocked() {
	if e.mvm.runCancel != nil {
		e.mvm.runCancel()
		e.mvm.runCancel = nil
		e.mvm.runCtx = nil
	}
}

// microVMInstance tracks one running container's helper process, VM, attach
// stream, host TCP port forwarders, and held volume flocks.
type microVMInstance struct {
	id         string
	client     microVMClient
	stream     microVMStream
	sockDir    string
	openStdin  bool
	tty        bool
	helperPath string
	unlocks    []func()
	forwarders *microVMForwarders
	netStream  microVMNetStream
	control    *microvmControl
	done       chan struct{}

	mu       sync.Mutex
	state    sandbox.State
	stopping bool
}

func (inst *microVMInstance) getState() sandbox.State {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.state
}

func (inst *microVMInstance) isPaused() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.state.Paused
}

// ---- Public Engine API seams ----

// SetMicroVM turns the micro-VM container runtime on with cfg, or off with nil.
// Updating cfg while already on preserves running micro-VMs and cached assets
// so resource defaults can be changed live. Turning it off gracefully stops
// all running micro-VM containers.
func (e *Engine) SetMicroVM(cfg *MicroVMConfig) error {
	cp := cloneMicroVMConfig(cfg)

	e.mvm.mu.Lock()
	wasEnabled := e.mvm.cfg != nil
	engineOpen := e.mvm.engineOpen
	e.mvm.cfg = cp
	if cp == nil {
		e.mvm.disableGen++
		e.cancelMicroVMRunCtxLocked()
		e.mvm.assets = nil
		e.mvm.assetsLabel = ""
		e.mvm.downloading = false
		e.mvm.dlDone = 0
		e.mvm.dlTotal = 0
		e.mvm.lastErr = ""
	} else if !wasEnabled {
		e.mvm.lastErr = ""
		if engineOpen {
			e.ensureMicroVMRunCtxLocked()
		}
	}
	e.mvm.mu.Unlock()

	if cp != nil {
		e.markForeignImagesPossible()
	}
	if !engineOpen {
		return nil
	}
	if cp == nil && wasEnabled {
		e.stopMicroVMContainers(defaultMicroVMStopGrace)
	}
	e.notify()
	return nil
}

// MicroVMState reports the micro-VM runtime's current status.
func (e *Engine) MicroVMState() MicroVMState {
	e.mvm.mu.Lock()
	st := MicroVMState{
		Enabled:     e.mvm.cfg != nil,
		RunningVMs:  len(e.mvm.instances),
		Downloading: e.mvm.downloading,
		Done:        e.mvm.dlDone,
		Total:       e.mvm.dlTotal,
		Assets:      e.mvm.assetsLabel,
		Err:         e.mvm.lastErr,
	}
	e.mvm.mu.Unlock()
	return st
}

// ContainerRuntimes returns the runtime and platform of every known container,
// keyed by full container ID.
func (e *Engine) ContainerRuntimes() map[string]ContainerRuntimeInfo {
	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	out := make(map[string]ContainerRuntimeInfo, len(e.ctr.byID))
	for id, c := range e.ctr.byID {
		rt := c.Runtime
		if rt == "" {
			rt = RuntimeGVX
		}
		out[id] = ContainerRuntimeInfo{
			Runtime:  rt,
			Platform: c.Platform,
		}
	}
	return out
}

// ---- Engine runtime dispatch helpers ----

func (e *Engine) isMicroVMEnabled() bool {
	e.mvm.mu.Lock()
	enabled := e.mvm.cfg != nil
	e.mvm.mu.Unlock()
	return enabled
}

func (e *Engine) isMicroVMRosettaReady() bool {
	e.mvm.mu.Lock()
	cfg := e.mvm.cfg
	e.mvm.mu.Unlock()
	if cfg == nil || cfg.RosettaReady == nil {
		return true
	}
	return cfg.RosettaReady()
}

func (e *Engine) isMicroVMContainer(id string) bool {
	e.ctr.mu.Lock()
	c := e.ctr.byID[id]
	ok := c != nil && c.isMicroVM()
	e.ctr.mu.Unlock()
	return ok
}

func (e *Engine) hasControlFor(ref string) bool {
	if e.runner.Features().Control {
		return true
	}
	if c, err := e.lookup(ref); err == nil && c.isMicroVM() {
		return true
	}
	return false
}

func (e *Engine) hasControlForExec(execID string) bool {
	if e.runner.Features().Control {
		return true
	}
	e.exc.mu.Lock()
	rec := e.exc.byID[execID]
	var cid string
	if rec != nil {
		cid = rec.ContainerID
	}
	e.exc.mu.Unlock()
	if cid != "" {
		return e.isMicroVMContainer(cid)
	}
	return e.isMicroVMEnabled()
}

func (e *Engine) microVMInstance(id string) *microVMInstance {
	e.mvm.mu.Lock()
	inst := e.mvm.instances[id]
	e.mvm.mu.Unlock()
	return inst
}

func (e *Engine) ctrControl(c Container) sandbox.Control {
	if c.isMicroVM() {
		if inst := e.microVMInstance(c.ID); inst != nil {
			return inst.control
		}
		return nil
	}
	return e.runner.Control(c.ID)
}

func (e *Engine) ctrState(c Container) sandbox.State {
	if c.isMicroVM() {
		if inst := e.microVMInstance(c.ID); inst != nil {
			return inst.getState()
		}
		return sandbox.State{}
	}
	return e.runner.State(c.ID)
}

func (e *Engine) ctrStdin(id string) io.Writer {
	if e.isMicroVMContainer(id) {
		inst := e.microVMInstance(id)
		if inst == nil || !inst.openStdin || inst.stream == nil {
			return nil
		}
		return inst.stream.Stdin()
	}
	return e.runner.Stdin(id)
}

func (e *Engine) ctrCloseStdin(id string) error {
	if e.isMicroVMContainer(id) {
		inst := e.microVMInstance(id)
		if inst == nil || inst.stream == nil {
			return nil
		}
		return inst.stream.CloseStdin()
	}
	return e.runner.CloseStdin(id)
}

func (e *Engine) ctrResize(c Container, rows, cols uint16) error {
	if c.isMicroVM() {
		inst := e.microVMInstance(c.ID)
		if inst == nil {
			return sandbox.ErrNoControl
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return mapVMProtoErr(inst.client.ContainerResize(ctx, rows, cols))
	}
	return e.runner.Resize(c.ID, rows, cols)
}

func (e *Engine) ctrPause(ctx context.Context, c Container) error {
	if c.isMicroVM() {
		return e.microVMPause(ctx, c.ID)
	}
	return e.runner.Pause(ctx, c.ID)
}

func (e *Engine) ctrUnpause(ctx context.Context, c Container) error {
	if c.isMicroVM() {
		return e.microVMUnpause(ctx, c.ID)
	}
	return e.runner.Unpause(ctx, c.ID)
}

func (e *Engine) ctrSignal(c Container, sig syscall.Signal) error {
	if c.isMicroVM() {
		inst := e.microVMInstance(c.ID)
		if inst == nil {
			return errors.New("container is not running")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return mapVMProtoErr(inst.client.ContainerSignal(ctx, vmproto.ContainerSignalRequest{
			Signal: signalName(sig),
		}))
	}
	return e.runner.Signal(c.ID, sig)
}

func (e *Engine) ctrShutdown(id string, isMicroVM bool, sig string, timeout time.Duration) error {
	if isMicroVM {
		return e.microVMShutdown(id, sig, timeout)
	}
	return e.runner.Shutdown(id, sig, timeout)
}

func (e *Engine) ctrForget(id string, isMicroVM bool) {
	if isMicroVM {
		return
	}
	e.runner.Forget(id)
}

// ---- Engine Open / Close & rootfs housekeeping ----

func (e *Engine) openMicroVM() {
	e.mvm.mu.Lock()
	e.mvm.engineOpen = true
	if e.mvm.instances == nil {
		e.mvm.instances = map[string]*microVMInstance{}
	}
	enabled := e.mvm.cfg != nil
	if enabled {
		e.ensureMicroVMRunCtxLocked()
	}
	e.mvm.mu.Unlock()
	if enabled {
		e.markForeignImagesPossible()
	}
	e.sweepMicroVMRootfs()
}

func (e *Engine) closeMicroVM(timeout time.Duration) {
	e.mvm.mu.Lock()
	e.mvm.engineOpen = false
	e.mvm.disableGen++
	e.cancelMicroVMRunCtxLocked()
	var leftover []*microVMInstance
	for _, inst := range e.mvm.instances {
		leftover = append(leftover, inst)
	}
	e.mvm.mu.Unlock()

	if timeout <= 0 {
		timeout = defaultMicroVMStopGrace
	}
	var wg sync.WaitGroup
	for _, inst := range leftover {
		wg.Go(func() {
			_ = e.microVMShutdown(inst.id, "SIGTERM", timeout)
		})
	}
	wg.Wait()
}

func (e *Engine) stopMicroVMContainers(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultMicroVMStopGrace
	}
	var ids []string
	for _, c := range e.allContainers() {
		if c.isMicroVM() && (c.State.Running() || c.State.Status == StatusRestarting) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			_ = e.stopContainerByRef(context.Background(), id, "", timeout, true)
		})
	}
	wg.Wait()
}

func (e *Engine) microVMRootfsDir() string {
	if e.cfg.Dir == "" {
		return ""
	}
	return e.path(microVMDirName, microVMRootfsDirName)
}

func (e *Engine) microVMRootfsPath(id string) string {
	dir := e.microVMRootfsDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, id+".ext4")
}

func (e *Engine) removeMicroVMRootfs(id string) {
	p := e.microVMRootfsPath(id)
	if p == "" {
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		e.logf("engine: remove microvm rootfs of %s: %v", shortHex(id), err)
	}
}

func (e *Engine) sweepMicroVMRootfs() {
	dir := e.microVMRootfsDir()
	if dir == "" {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	e.ctr.mu.Lock()
	keep := map[string]bool{}
	for id, c := range e.ctr.byID {
		if c.isMicroVM() {
			keep[id+".ext4"] = true
		}
	}
	e.ctr.mu.Unlock()
	for _, ent := range ents {
		if ent.Type().IsRegular() && strings.HasSuffix(ent.Name(), ".ext4") && !keep[ent.Name()] {
			_ = os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}

// ---- Resource computation (§7.2, apple/container defaults) ----

type microVMResources struct {
	cpus      int
	memMiB    int
	memoryMax int64
	cpuQuota  int64
	cpuPeriod int64
	pidsLimit int64
}

func resolveMicroVMResources(cfg *MicroVMConfig, hc HostConfig) microVMResources {
	hostCPUs := max(1, runtime.NumCPU())
	cpus := DefaultMicroVMCPUs
	if cfg != nil && cfg.DefaultCPUs > 0 {
		cpus = cfg.DefaultCPUs
	}
	cpus = clampInt(cpus, 1, hostCPUs)

	var cpuQuota, cpuPeriod int64
	if hc.NanoCpus > 0 {
		cpus = clampInt(int(math.Ceil(float64(hc.NanoCpus)/1e9)), 1, hostCPUs)
		cpuPeriod = 100000
		cpuQuota = max(1000, int64(math.Round(float64(hc.NanoCpus)*float64(cpuPeriod)/1e9)))
	} else if hc.CpuQuota > 0 {
		cpuPeriod = hc.CpuPeriod
		if cpuPeriod <= 0 {
			cpuPeriod = 100000
		}
		cpuQuota = hc.CpuQuota
		cpus = clampInt(int(math.Ceil(float64(hc.CpuQuota)/float64(cpuPeriod))), 1, hostCPUs)
	}

	maxMemMiB := 0
	if hm := hostMemTotal(); hm > 0 {
		maxMemMiB = max(minMicroVMMemoryMiB, int((hm*3/4)/(1024*1024)))
	}
	clampMem := func(mib int) int {
		if mib < minMicroVMMemoryMiB {
			mib = minMicroVMMemoryMiB
		}
		if maxMemMiB > 0 && mib > maxMemMiB {
			mib = maxMemMiB
		}
		return mib
	}

	memMiB := DefaultMicroVMMemoryMiB
	if cfg != nil && cfg.DefaultMemMB > 0 {
		memMiB = cfg.DefaultMemMB
	}
	memMiB = clampMem(memMiB)

	var memoryMax int64
	if hc.Memory > 0 {
		memoryMax = hc.Memory
		reqMiB := int((hc.Memory + (1 << 20) - 1) >> 20)
		memMiB = clampMem(reqMiB)
	}

	var pidsLimit int64
	if hc.PidsLimit != nil && *hc.PidsLimit > 0 {
		pidsLimit = *hc.PidsLimit
	}

	return microVMResources{
		cpus:      cpus,
		memMiB:    memMiB,
		memoryMax: memoryMax,
		cpuQuota:  cpuQuota,
		cpuPeriod: cpuPeriod,
		pidsLimit: pidsLimit,
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- Asset resolution & memoization ----

func (e *Engine) resolveMicroVMAssets(ctx context.Context, cfg *MicroVMConfig, startGen uint64) (vmassets.Dir, string, error) {
	e.mvm.mu.Lock()
	if e.mvm.assets != nil {
		dir := *e.mvm.assets
		label := e.mvm.assetsLabel
		e.mvm.mu.Unlock()
		return dir, label, nil
	}
	e.mvm.mu.Unlock()

	e.mvm.assetMu.Lock()
	defer e.mvm.assetMu.Unlock()

	e.mvm.mu.Lock()
	if !e.mvm.engineOpen || e.mvm.cfg == nil || e.mvm.disableGen != startGen {
		open := e.mvm.engineOpen
		e.mvm.mu.Unlock()
		if !open {
			return vmassets.Dir{}, "", errors.New("engine is closed")
		}
		return vmassets.Dir{}, "", errConflict("Micro-VM container runtime is disabled; enable it in Labs")
	}
	if e.mvm.assets != nil {
		dir := *e.mvm.assets
		label := e.mvm.assetsLabel
		e.mvm.mu.Unlock()
		return dir, label, nil
	}
	e.mvm.mu.Unlock()

	var lastNotify atomic.Int64
	progress := func(done, total int64) {
		e.mvm.mu.Lock()
		if !e.mvm.engineOpen || e.mvm.cfg == nil || e.mvm.disableGen != startGen {
			e.mvm.mu.Unlock()
			return
		}
		e.mvm.downloading = true
		e.mvm.dlDone = done
		e.mvm.dlTotal = total
		e.mvm.mu.Unlock()
		now := time.Now().UnixNano()
		prev := lastNotify.Load()
		if prev == 0 || now-prev >= int64(100*time.Millisecond) || (total > 0 && done >= total) {
			if lastNotify.CompareAndSwap(prev, now) {
				go e.notify()
			}
		}
	}

	var (
		assetsDir vmassets.Dir
		err       error
	)
	if cfg.Assets != nil {
		assetsDir, err = cfg.Assets(ctx, progress)
	} else {
		assetsDir, err = vmassets.Resolve(ctx, vmassets.Options{Progress: progress})
	}

	assetsLabel := assetsDir.Manifest.Version
	if assetsDir.Override {
		assetsLabel = "local"
	} else if assetsLabel == "" {
		assetsLabel = vmassets.Version
	}

	e.mvm.mu.Lock()
	if !e.mvm.engineOpen || e.mvm.cfg == nil || e.mvm.disableGen != startGen {
		open := e.mvm.engineOpen
		e.mvm.downloading = false
		e.mvm.mu.Unlock()
		if err != nil {
			return vmassets.Dir{}, "", err
		}
		if !open {
			return vmassets.Dir{}, "", errors.New("engine is closed")
		}
		return vmassets.Dir{}, "", errConflict("Micro-VM container runtime is disabled; enable it in Labs")
	}
	e.mvm.downloading = false
	if err != nil {
		e.mvm.lastErr = err.Error()
		e.mvm.mu.Unlock()
		go e.notify()
		return vmassets.Dir{}, "", err
	}
	ad := assetsDir
	e.mvm.assets = &ad
	e.mvm.assetsLabel = assetsLabel
	e.mvm.lastErr = ""
	e.mvm.mu.Unlock()
	go e.notify()
	return assetsDir, assetsLabel, nil
}

// ---- Storage planning: EROFS layers (fastpath + overflow), volumes, binds, tmpfs ----

type microVMStoragePlan struct {
	layers           []vmproto.LayerSource
	extraDisks       []vmproto.ExtraDiskSpec
	shares           []vmproto.ShareSpec
	mounts           []vmproto.ContainerMount
	unlocks          []func()
	preStartRWUnlock []func() // OS LOCK_EX handoff to VZDiskImageStorageDeviceAttachment on darwin
}

func (e *Engine) acquireMicroVMVol(path string, readOnly bool) (func(), error) {
	e.mvm.mu.Lock()
	defer e.mvm.mu.Unlock()
	if e.mvm.volRefs == nil {
		e.mvm.volRefs = map[string]*microVMVolRef{}
	}
	ref := e.mvm.volRefs[path]
	if ref != nil {
		if ref.exclusive || (!readOnly && ref.shared > 0) {
			return nil, fmt.Errorf("volume %s is in use (locked)", path)
		}
	} else {
		ref = &microVMVolRef{}
		e.mvm.volRefs[path] = ref
	}
	if readOnly {
		ref.shared++
	} else {
		ref.exclusive = true
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mvm.mu.Lock()
			defer e.mvm.mu.Unlock()
			cur := e.mvm.volRefs[path]
			if cur == nil {
				return
			}
			if readOnly {
				cur.shared--
			} else {
				cur.exclusive = false
			}
			if !cur.exclusive && cur.shared <= 0 {
				delete(e.mvm.volRefs, path)
			}
		})
	}, nil
}

func releaseUnlocks(unlocks []func()) {
	for i := len(unlocks) - 1; i >= 0; i-- {
		if unlocks[i] != nil {
			unlocks[i]()
		}
	}
}

func virtioBlkDev(idx int) string {
	if idx < 26 {
		return "/dev/vd" + string(rune('a'+idx))
	}
	hi := (idx / 26) - 1
	lo := idx % 26
	return "/dev/vd" + string(rune('a'+hi)) + string(rune('a'+lo))
}

func buildTmpfsOptionsMap(c *Container) map[string]string {
	out := map[string]string{}
	for dst, raw := range c.HostConfig.Tmpfs {
		clean := path.Clean(dst)
		if s := strings.TrimSpace(raw); s != "" {
			out[clean] = s
		}
	}
	for _, m := range c.HostConfig.Mounts {
		if strings.EqualFold(m.Type, "tmpfs") && m.TmpfsOptions != nil {
			var parts []string
			if m.TmpfsOptions.SizeBytes > 0 {
				parts = append(parts, "size="+strconv.FormatInt(m.TmpfsOptions.SizeBytes, 10))
			}
			if m.TmpfsOptions.Mode > 0 {
				parts = append(parts, fmt.Sprintf("mode=%o", m.TmpfsOptions.Mode))
			}
			if len(parts) > 0 {
				out[path.Clean(m.Target)] = strings.Join(parts, ",")
			}
		}
	}
	for _, sm := range c.Spec.Mounts {
		if sm.Type == sandbox.MountTmpfs && sm.Size != "" {
			clean := path.Clean(sm.Target)
			if _, ok := out[clean]; !ok {
				out[clean] = "size=" + sm.Size
			}
		}
	}
	return out
}

func (e *Engine) planMicroVMStorage(ctx context.Context, c *Container) (microVMStoragePlan, error) {
	info, err := e.runner.Info(ctx)
	if err != nil {
		return microVMStoragePlan{}, fmt.Errorf("inspect rungv cache directories: %w", err)
	}
	if strings.TrimSpace(info.LayerCacheDir) == "" {
		return microVMStoragePlan{}, errors.New("layer cache directory is unknown")
	}

	layerPaths, err := imagefs.ResolveGVXLayers(info.LayerCacheDir, c.ImageID)
	if err != nil {
		return microVMStoragePlan{}, fmt.Errorf("resolve image EROFS layers: %w", err)
	}
	if len(layerPaths) == 0 {
		return microVMStoragePlan{}, fmt.Errorf("image %s has no filesystem layers", shortHex(c.ImageID))
	}
	if len(layerPaths) > maxOverflowEROFSLayers {
		return microVMStoragePlan{}, fmt.Errorf("image has %d layers, exceeding micro-VM limit of %d", len(layerPaths), maxOverflowEROFSLayers)
	}

	var plan microVMStoragePlan
	cleanupOnErr := func(err error) (microVMStoragePlan, error) {
		releaseUnlocks(plan.unlocks)
		return microVMStoragePlan{}, err
	}

	// 1. EROFS image layers: fastpath (<=8 virtio-blk) vs overflow (>8 virtiofs + loop).
	if len(layerPaths) <= maxFastpathEROFSLayers {
		for _, lp := range layerPaths {
			dev := virtioBlkDev(1 + len(plan.extraDisks))
			plan.extraDisks = append(plan.extraDisks, vmproto.ExtraDiskSpec{
				Path:     lp,
				ReadOnly: true,
			})
			plan.layers = append(plan.layers, vmproto.LayerSource{Device: dev})
		}
	} else {
		absCache, err := filepath.Abs(info.LayerCacheDir)
		if err != nil {
			return cleanupOnErr(fmt.Errorf("resolve layer cache dir: %w", err))
		}
		if resolved, err := filepath.EvalSymlinks(absCache); err == nil {
			absCache = resolved
		}
		plan.shares = append(plan.shares, vmproto.ShareSpec{
			Tag:      "layers",
			Dir:      absCache,
			ReadOnly: true,
		})
		for _, lp := range layerPaths {
			realLP := lp
			if resolved, err := filepath.EvalSymlinks(lp); err == nil {
				realLP = resolved
			}
			rel, err := filepath.Rel(absCache, realLP)
			if err != nil || strings.HasPrefix(rel, "..") {
				return cleanupOnErr(fmt.Errorf("layer %q is outside layer cache dir %q", lp, absCache))
			}
			plan.layers = append(plan.layers, vmproto.LayerSource{
				File: "/mnt/layers/" + filepath.ToSlash(rel),
			})
		}
	}

	// 2. Volumes, bind mounts, and tmpfs mounts.
	tmpfsOpts := buildTmpfsOptionsMap(c)
	var rvMap map[string]rungv.Volume
	for _, m := range c.Mounts {
		if m.Type == "volume" {
			rvMap = e.rungvVolumeMap(ctx)
			break
		}
	}

	// Track attached ext4 volumes by host path so mounting the same volume at
	// two targets in one container shares a single virtio-blk device & flock.
	type attachedVol struct {
		devIdx     int
		dev        string
		readOnly   bool
		memLockIdx int
		osLockIdx  int
		path       string
	}
	attachedVols := map[string]*attachedVol{}
	ext4VolCount := 0
	bindShareCount := 0

	cleanupOnErr = func(err error) (microVMStoragePlan, error) {
		releaseUnlocks(plan.preStartRWUnlock)
		releaseUnlocks(plan.unlocks)
		return microVMStoragePlan{}, err
	}

	for _, m := range c.Mounts {
		dst := path.Clean(m.Destination)
		ro := !m.RW
		switch m.Type {
		case "tmpfs":
			plan.mounts = append(plan.mounts, vmproto.ContainerMount{
				Kind:     "tmpfs",
				Target:   dst,
				ReadOnly: ro,
				Options:  tmpfsOpts[dst],
			})

		case "bind":
			src := strings.TrimSpace(m.Source)
			if src == "" {
				return cleanupOnErr(errInvalid("empty bind mount source for %s", dst))
			}
			absSrc, err := filepath.Abs(src)
			if err != nil {
				return cleanupOnErr(fmt.Errorf("bind source %q: %w", src, err))
			}
			if resolved, err := filepath.EvalSymlinks(absSrc); err == nil {
				absSrc = resolved
			}
			fi, err := os.Stat(absSrc)
			if err != nil {
				return cleanupOnErr(fmt.Errorf("bind source %q: %w", src, err))
			}
			if bindShareCount >= maxMicroVMBindShares {
				return cleanupOnErr(fmt.Errorf("micro-VM supports at most %d bind mounts", maxMicroVMBindShares))
			}
			tag := fmt.Sprintf("b%d", bindShareCount)
			bindShareCount++
			if fi.IsDir() {
				plan.shares = append(plan.shares, vmproto.ShareSpec{
					Tag:      tag,
					Dir:      absSrc,
					ReadOnly: ro,
				})
				plan.mounts = append(plan.mounts, vmproto.ContainerMount{
					Kind:     "bind",
					Source:   "/mnt/" + tag,
					Target:   dst,
					ReadOnly: ro,
				})
			} else {
				// Single-file bind: share parent dir under isolated tag; guest
				// bind-mounts only the file and pivot_root+MNT_DETACH hides parent.
				plan.shares = append(plan.shares, vmproto.ShareSpec{
					Tag:      tag,
					Dir:      filepath.Dir(absSrc),
					ReadOnly: ro,
				})
				plan.mounts = append(plan.mounts, vmproto.ContainerMount{
					Kind:     "bind",
					Source:   "/mnt/" + tag + "/" + filepath.Base(absSrc),
					Target:   dst,
					ReadOnly: ro,
				})
			}

		case "volume":
			volName := m.Name
			if volName == "" {
				volName = m.Source
			}
			volPath := ""
			if rv, ok := rvMap[volName]; ok && strings.TrimSpace(rv.Mountpoint) != "" {
				volPath = strings.TrimSpace(rv.Mountpoint)
			}
			if volPath == "" && strings.TrimSpace(info.VolumeDir) != "" && volName != "" {
				volPath = filepath.Join(info.VolumeDir, volName+".ext4")
			}
			if volPath == "" {
				return cleanupOnErr(fmt.Errorf("cannot locate volume %q on host", volName))
			}
			absVol, err := filepath.Abs(volPath)
			if err != nil {
				return cleanupOnErr(fmt.Errorf("volume %q: %w", volName, err))
			}
			if resolved, err := filepath.EvalSymlinks(absVol); err == nil {
				absVol = resolved
			}
			fi, err := os.Stat(absVol)
			if err != nil {
				return cleanupOnErr(fmt.Errorf("volume %q (%s): %w", volName, absVol, err))
			}
			if fi.IsDir() {
				// hostfs volume: share via virtiofs.
				if bindShareCount >= maxMicroVMBindShares {
					return cleanupOnErr(fmt.Errorf("micro-VM supports at most %d virtiofs shares", maxMicroVMBindShares))
				}
				tag := fmt.Sprintf("b%d", bindShareCount)
				bindShareCount++
				plan.shares = append(plan.shares, vmproto.ShareSpec{
					Tag:      tag,
					Dir:      absVol,
					ReadOnly: ro,
				})
				plan.mounts = append(plan.mounts, vmproto.ContainerMount{
					Kind:     "bind",
					Source:   "/mnt/" + tag,
					Target:   dst,
					ReadOnly: ro,
				})
				continue
			}

			if prev, exists := attachedVols[absVol]; exists {
				if prev.readOnly && !ro {
					// Upgrade both in-process and OS locks from shared to exclusive.
					if prev.osLockIdx >= 0 && prev.osLockIdx < len(plan.unlocks) && plan.unlocks[prev.osLockIdx] != nil {
						plan.unlocks[prev.osLockIdx]()
						plan.unlocks[prev.osLockIdx] = nil
					}
					if prev.memLockIdx >= 0 && prev.memLockIdx < len(plan.unlocks) && plan.unlocks[prev.memLockIdx] != nil {
						plan.unlocks[prev.memLockIdx]()
						plan.unlocks[prev.memLockIdx] = nil
					}
					memUnlock, err := e.acquireMicroVMVol(absVol, false)
					if err != nil {
						return cleanupOnErr(err)
					}
					plan.unlocks[prev.memLockIdx] = memUnlock
					osUnlock, err := lockVolumeFile(absVol, false)
					if err != nil {
						return cleanupOnErr(err)
					}
					plan.preStartRWUnlock = append(plan.preStartRWUnlock, osUnlock)
					prev.osLockIdx = -1
					prev.readOnly = false
					plan.extraDisks[prev.devIdx].ReadOnly = false
				}
				plan.mounts = append(plan.mounts, vmproto.ContainerMount{
					Kind:     "volume",
					Source:   prev.dev,
					Target:   dst,
					ReadOnly: ro,
				})
				continue
			}

			if ext4VolCount >= maxMicroVMVolumes {
				return cleanupOnErr(fmt.Errorf("micro-VM supports at most %d ext4 volumes", maxMicroVMVolumes))
			}
			memUnlock, err := e.acquireMicroVMVol(absVol, ro)
			if err != nil {
				return cleanupOnErr(err)
			}
			memLockIdx := len(plan.unlocks)
			plan.unlocks = append(plan.unlocks, memUnlock)

			osUnlock, err := lockVolumeFile(absVol, ro)
			if err != nil {
				return cleanupOnErr(err)
			}
			osLockIdx := -1
			if ro {
				// Keep shared flock held in host process (VZ readOnly attachment
				// does not acquire an exclusive lock).
				osLockIdx = len(plan.unlocks)
				plan.unlocks = append(plan.unlocks, osUnlock)
			} else {
				// Writable ext4 volumes: we verified no rungv/external process
				// holds a flock. On macOS VZ, VZDiskImageStorageDeviceAttachment
				// acquires its own exclusive lock when the VM starts, so we hand
				// off this OS flock right before client.Start while keeping
				// memUnlock held for the VM's full lifetime.
				plan.preStartRWUnlock = append(plan.preStartRWUnlock, osUnlock)
			}

			devIdx := len(plan.extraDisks)
			dev := virtioBlkDev(1 + devIdx)
			plan.extraDisks = append(plan.extraDisks, vmproto.ExtraDiskSpec{
				Path:     absVol,
				ReadOnly: ro,
			})
			ext4VolCount++
			attachedVols[absVol] = &attachedVol{
				devIdx:     devIdx,
				dev:        dev,
				readOnly:   ro,
				memLockIdx: memLockIdx,
				osLockIdx:  osLockIdx,
				path:       absVol,
			}
			plan.mounts = append(plan.mounts, vmproto.ContainerMount{
				Kind:     "volume",
				Source:   dev,
				Target:   dst,
				ReadOnly: ro,
			})
		}
	}

	return plan, nil
}

// ---- Host TCP port forwarders (§8) ----

type microVMForwarders struct {
	mu      sync.Mutex
	closed  bool
	lns     []net.Listener
	tracked map[net.Conn]struct{}
}

func (e *Engine) startMicroVMForwarders(client microVMClient, resolvedPorts map[string][]PortBinding) (*microVMForwarders, error) {
	if len(resolvedPorts) == 0 {
		return &microVMForwarders{tracked: map[net.Conn]struct{}{}}, nil
	}
	allowLAN := e.allowLANPorts()
	fw := &microVMForwarders{tracked: map[net.Conn]struct{}{}}

	type listenTarget struct {
		ln        net.Listener
		guestPort int
	}
	var targets []listenTarget

	for containerPort, bindings := range resolvedPorts {
		guestPort, proto := splitPortProto(containerPort)
		if proto != "tcp" || guestPort <= 0 {
			continue
		}
		for _, b := range bindings {
			hostPort := strings.TrimSpace(b.HostPort)
			if hostPort == "" {
				continue
			}
			hostIP := strings.TrimSpace(b.HostIP)
			if !allowLAN || hostIP == "" {
				hostIP = "127.0.0.1"
			}
			addr := net.JoinHostPort(hostIP, hostPort)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				fw.Close()
				return nil, fmt.Errorf("Bind for %s failed: %w", addr, err)
			}
			fw.lns = append(fw.lns, ln)
			targets = append(targets, listenTarget{ln: ln, guestPort: guestPort})
		}
	}

	for _, t := range targets {
		go fw.acceptLoop(e, client, t.ln, t.guestPort)
	}
	return fw, nil
}

func (fw *microVMForwarders) track(conns ...net.Conn) bool {
	fw.mu.Lock()
	if fw.closed {
		fw.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	for _, c := range conns {
		fw.tracked[c] = struct{}{}
	}
	fw.mu.Unlock()
	return true
}

func (fw *microVMForwarders) untrack(conns ...net.Conn) {
	fw.mu.Lock()
	for _, c := range conns {
		delete(fw.tracked, c)
	}
	fw.mu.Unlock()
}

func (fw *microVMForwarders) acceptLoop(e *Engine, client microVMClient, ln net.Listener, guestPort int) {
	guestAddr := fmt.Sprintf("127.0.0.1:%d", guestPort)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(clientConn net.Conn) {
			if !fw.track(clientConn) {
				return
			}
			defer func() {
				fw.untrack(clientConn)
				_ = clientConn.Close()
			}()

			dialCtx, cancel := context.WithTimeout(context.Background(), microVMDialTimeout)
			backendConn, err := client.Dial(dialCtx, vmproto.DialRequest{
				Network: "tcp",
				Addr:    guestAddr,
			})
			cancel()
			if err != nil {
				e.logf("engine: microvm forward %s -> %s: %v", ln.Addr(), guestAddr, err)
				return
			}
			if !fw.track(backendConn) {
				return
			}
			defer func() {
				fw.untrack(backendConn)
				_ = backendConn.Close()
			}()

			pipeHalfCloseCount(clientConn, backendConn)
		}(conn)
	}
}

func (fw *microVMForwarders) Close() {
	if fw == nil {
		return
	}
	fw.mu.Lock()
	if fw.closed {
		fw.mu.Unlock()
		return
	}
	fw.closed = true
	lns := fw.lns
	fw.lns = nil
	conns := make([]net.Conn, 0, len(fw.tracked))
	for c := range fw.tracked {
		conns = append(conns, c)
	}
	clear(fw.tracked)
	fw.mu.Unlock()

	for _, ln := range lns {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// ---- Micro-VM Start / Watch / Stop / Pause ----

func makeMicroVMSockDir(id string) (string, error) {
	prefix := "gvxd-c-" + shortHex(id) + "-"
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	if len(filepath.Join(dir, vmhelper.SocketName)) > builderMaxUnixSockPath {
		_ = os.RemoveAll(dir)
		shortPrefix := "gvxd-c-" + shortHex(id)[:min(8, len(shortHex(id)))] + "-"
		dir, err = os.MkdirTemp("/tmp", shortPrefix)
		if err != nil {
			return "", err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// checkMicroVMContainerActive verifies under e.ctr.mu (never nested with
// e.mvm.mu) that container id has not been stopped, removed, superseded, or
// caught by engine shutdown while an async start was in flight.
func (e *Engine) checkMicroVMContainerActive(id string, gen uint64) error {
	e.ctr.mu.Lock()
	closing := e.ctr.closing
	cur := e.ctr.byID[id]
	aborted := cur == nil || cur.State.Status == StatusRemoving || cur.State.StoppedByUser || (gen != 0 && cur.runGen != gen)
	e.ctr.mu.Unlock()
	if closing {
		return errors.New("engine is closed")
	}
	if aborted {
		return errConflict("container %s was stopped or removed during start", shortHex(id))
	}
	return nil
}

func (e *Engine) checkMicroVMRuntimeActive(startGen uint64) error {
	e.mvm.mu.Lock()
	open := e.mvm.engineOpen
	enabled := e.mvm.cfg != nil && e.mvm.disableGen == startGen
	e.mvm.mu.Unlock()
	if !open {
		return errors.New("engine is closed")
	}
	if !enabled {
		return errConflict("Micro-VM container runtime is disabled; enable it in Labs")
	}
	return nil
}

func (e *Engine) microVMStart(c *Container, gen uint64, resolvedPorts map[string][]PortBinding) error {
	e.mvm.mu.Lock()
	open := e.mvm.engineOpen
	cfg := cloneMicroVMConfig(e.mvm.cfg)
	startGen := e.mvm.disableGen
	launchFn := e.mvm.launchFn
	var parentCtx context.Context
	if open && cfg != nil {
		parentCtx = e.ensureMicroVMRunCtxLocked()
	}
	e.mvm.mu.Unlock()

	if !open {
		return errors.New("engine is closed")
	}
	if cfg == nil {
		return errConflict("Micro-VM container runtime is disabled; enable it in Labs")
	}
	rosetta := parsePlatformArch(c.Platform) == "amd64"
	if rosetta && cfg.RosettaReady != nil && !cfg.RosettaReady() {
		return errInvalid("Rosetta is required to run linux/amd64 containers; install Rosetta in Labs")
	}

	ctx, cancel := context.WithTimeout(parentCtx, defaultMicroVMStartTimeout)
	defer cancel()

	assetsDir, _, err := e.resolveMicroVMAssets(ctx, cfg, startGen)
	if err != nil {
		return fmt.Errorf("resolve VM assets: %w", err)
	}
	if err := e.checkMicroVMContainerActive(c.ID, gen); err != nil {
		return err
	}
	if err := e.checkMicroVMRuntimeActive(startGen); err != nil {
		return err
	}

	plan, err := e.planMicroVMStorage(ctx, c)
	if err != nil {
		return err
	}
	unlocks := plan.unlocks
	preStartRWUnlock := plan.preStartRWUnlock
	if launchFn != nil || runtime.GOOS != "darwin" {
		// In unit tests (fake vmclient without real VZ) or non-darwin builds,
		// keep the OS LOCK_EX held for the full lifetime of the instance.
		unlocks = append(unlocks, preStartRWUnlock...)
		preStartRWUnlock = nil
	}
	cleanupLocks := func() {
		releaseUnlocks(preStartRWUnlock)
		preStartRWUnlock = nil
		releaseUnlocks(unlocks)
		unlocks = nil
	}

	sockDir, err := makeMicroVMSockDir(c.ID)
	if err != nil {
		cleanupLocks()
		return fmt.Errorf("create microvm socket dir: %w", err)
	}
	cleanupSockDir := func() {
		if sockDir != "" {
			_ = os.RemoveAll(sockDir)
			sockDir = ""
		}
	}

	rootfsPath := e.microVMRootfsPath(c.ID)
	if rootfsPath != "" {
		if err := os.MkdirAll(filepath.Dir(rootfsPath), 0o700); err != nil {
			cleanupSockDir()
			cleanupLocks()
			return fmt.Errorf("create microvm rootfs dir: %w", err)
		}
	} else {
		rootfsPath = filepath.Join(sockDir, "rootfs.ext4")
	}

	launcher := cfg.Launcher
	launcher.Dir = sockDir
	if launcher.Log == nil {
		cidShort := shortHex(c.ID)
		launcher.Log = func(line string) {
			e.logf("engine: microvm %s helper: %s", cidShort, line)
		}
	}

	var client microVMClient
	if launchFn != nil {
		client, err = launchFn(ctx, launcher)
	} else {
		var realClient *vmclient.Client
		realClient, err = launcher.Launch(ctx)
		if err == nil {
			client = realMicroVMClient{c: realClient}
		}
	}
	if err != nil {
		if client != nil {
			_ = client.Close(microVMHelperCloseGrace)
		}
		cleanupSockDir()
		cleanupLocks()
		return fmt.Errorf("launch microvm helper: %w", err)
	}

	var (
		netStream     microVMNetStream
		joinedOverlay bool
	)
	abort := func(reason error) error {
		if netStream != nil {
			_ = netStream.Close()
			netStream = nil
		}
		if joinedOverlay {
			e.leaveOverlay(c.ID)
			joinedOverlay = false
		}
		_ = client.Close(microVMHelperCloseGrace)
		cleanupSockDir()
		cleanupLocks()
		return reason
	}

	ovlAddrs, ovlIsolated := e.overlayAddrs(c)

	res := resolveMicroVMResources(cfg, c.HostConfig)
	vmSpec := vmproto.VMSpec{
		Kernel:     assetsDir.Kernel(),
		Initrd:     assetsDir.Initrd(),
		CPUs:       res.cpus,
		MemoryMiB:  res.memMiB,
		Disk:       &vmproto.DiskSpec{Path: rootfsPath, Size: defaultMicroVMRootfsSize},
		Rosetta:    rosetta,
		Network:    c.HostConfig.NetworkMode != "none" && !ovlIsolated,
		Shares:     plan.shares,
		ExtraDisks: plan.extraDisks,
	}

	// On macOS, release the parent's pre-check LOCK_EX right before handing
	// the writable ext4 image to VZDiskImageStorageDeviceAttachment, which
	// acquires its own exclusive lock for the VM's lifetime.
	releaseUnlocks(preStartRWUnlock)
	preStartRWUnlock = nil

	status, err := client.Start(ctx, vmSpec)
	if err != nil {
		return abort(fmt.Errorf("start microvm: %w", err))
	}
	if status.State == vmproto.Failed {
		msg := status.Err
		if msg == "" {
			msg = "micro-VM failed to start"
		}
		return abort(errors.New(msg))
	}

	var ctrNetCfg *vmproto.ContainerNetConfig
	if len(ovlAddrs) > 0 {
		peer, ovlCfg, jerr := e.fabric().JoinDirect(c.ID, ovlAddrs, overlay.JoinOptions{
			Isolated: ovlIsolated,
			MTU:      e.linkMTU(),
		})
		if jerr != nil {
			return abort(fmt.Errorf("join overlay network: %w", jerr))
		}
		joinedOverlay = true
		e.syncOverlay()

		nc := vmproto.ContainerNetConfig{
			Addresses: ovlCfg.Addresses,
			Routes:    ovlCfg.Routes,
			MTU:       ovlCfg.MTU,
			Isolated:  ovlIsolated,
			DNS:       true,
		}
		ns, aerr := client.ContainerNetAttach(ctx, nc)
		if aerr != nil {
			return abort(fmt.Errorf("attach microvm overlay network: %w", mapVMProtoErr(aerr)))
		}
		netStream = ns
		ctrNetCfg = &nc
		startMicroVMOverlayRelay(ns, peer)
	}

	argv := make([]string, 0, len(c.Config.Entrypoint)+len(c.Config.Cmd))
	argv = append(argv, c.Config.Entrypoint...)
	argv = append(argv, c.Config.Cmd...)
	if len(argv) == 0 && c.Spec.Command != "" {
		if split, err := rungv.SplitArgs(c.Spec.Command); err == nil {
			argv = split
		}
	}

	var rows, cols uint16
	if c.Config.Tty {
		if c.HostConfig.ConsoleSize[0] > 0 {
			rows = uint16(min(c.HostConfig.ConsoleSize[0], 65535))
		}
		if c.HostConfig.ConsoleSize[1] > 0 {
			cols = uint16(min(c.HostConfig.ConsoleSize[1], 65535))
		}
	}

	ctrSpec := vmproto.ContainerSpec{
		Argv:           argv,
		Env:            slices.Clone(c.Config.Env),
		Workdir:        c.Config.WorkingDir,
		User:           c.Config.User,
		Hostname:       c.Config.Hostname,
		ExtraHosts:     slices.Clone(c.HostConfig.ExtraHosts),
		TTY:            c.Config.Tty,
		Interactive:    c.Config.OpenStdin,
		Rows:           rows,
		Cols:           cols,
		Privileged:     c.HostConfig.Privileged,
		ReadonlyRootfs: c.HostConfig.ReadonlyRootfs,
		MemoryMax:      res.memoryMax,
		CPUQuota:       res.cpuQuota,
		CPUPeriod:      res.cpuPeriod,
		PidsLimit:      res.pidsLimit,
		Layers:         plan.layers,
		Mounts:         plan.mounts,
		Network:        ctrNetCfg,
	}

	ctrState, err := client.ContainerStart(ctx, ctrSpec)
	if err != nil {
		return abort(fmt.Errorf("start container in microvm: %w", mapVMProtoErr(err)))
	}

	stream, err := client.ContainerAttach(ctx)
	if err != nil {
		return abort(fmt.Errorf("attach to microvm container: %w", mapVMProtoErr(err)))
	}

	forwarders, err := e.startMicroVMForwarders(client, resolvedPorts)
	if err != nil {
		_ = stream.Close()
		return abort(err)
	}

	pid := ctrState.PID
	if pid <= 0 {
		pid = status.VMPID
	}
	if pid <= 0 {
		pid = 1
	}
	now := time.Now().UTC()
	initState := sandbox.State{
		Status:    sandbox.Running,
		PID:       pid,
		StartedAt: now,
		ReadyAt:   now,
		Control:   true,
	}

	inst := &microVMInstance{
		id:         c.ID,
		client:     client,
		stream:     stream,
		sockDir:    sockDir,
		openStdin:  c.Config.OpenStdin,
		tty:        c.Config.Tty,
		helperPath: launcher.Path,
		unlocks:    unlocks,
		forwarders: forwarders,
		netStream:  netStream,
		done:       make(chan struct{}),
		state:      initState,
	}
	inst.control = &microvmControl{
		e:            e,
		id:           c.ID,
		inst:         inst,
		helperPath:   launcher.Path,
		useHelperArg: len(launcher.Args) == 0 || launcher.Args[0] == vmhelper.Arg,
		socket:       client.Socket(),
	}

	if err := e.checkMicroVMContainerActive(c.ID, gen); err != nil {
		forwarders.Close()
		_ = stream.Close()
		return abort(err)
	}

	e.mvm.mu.Lock()
	if !e.mvm.engineOpen || e.mvm.cfg == nil || e.mvm.disableGen != startGen {
		open := e.mvm.engineOpen
		e.mvm.mu.Unlock()
		forwarders.Close()
		_ = stream.Close()
		if !open {
			return abort(errors.New("engine is closed"))
		}
		return abort(errConflict("Micro-VM container runtime is disabled; enable it in Labs"))
	}
	if e.mvm.instances == nil {
		e.mvm.instances = map[string]*microVMInstance{}
	}
	e.mvm.instances[c.ID] = inst
	e.mvm.mu.Unlock()

	// Re-check container state after registering inst to close the narrow
	// window where stop/remove/close ran between checkMicroVMContainerActive
	// and e.mvm.instances[c.ID] = inst.
	if err := e.checkMicroVMContainerActive(c.ID, gen); err != nil {
		e.mvm.mu.Lock()
		if e.mvm.instances[c.ID] == inst {
			delete(e.mvm.instances, c.ID)
		}
		e.mvm.mu.Unlock()
		forwarders.Close()
		_ = stream.Close()
		abortErr := abort(err)
		close(inst.done)
		return abortErr
	}

	// Emit Running & Ready BEFORE watchMicroVM can emit Exited, guaranteeing
	// strict lifecycle ordering even for instant-exit containers.
	e.onRunnerEvent(sandbox.Event{ID: c.ID, Kind: sandbox.EventChanged, State: initState})
	e.onRunnerEvent(sandbox.Event{ID: c.ID, Kind: sandbox.EventReady, State: initState})

	var pumpWG sync.WaitGroup
	if r := stream.Stdout(); r != nil {
		pumpWG.Add(1)
		go e.pumpMicroVMOutput(c.ID, sandbox.Stdout, r, &pumpWG)
	}
	if !c.Config.Tty {
		if r := stream.Stderr(); r != nil {
			pumpWG.Add(1)
			go e.pumpMicroVMOutput(c.ID, sandbox.Stderr, r, &pumpWG)
		}
	}

	go e.watchMicroVM(inst, &pumpWG)
	return nil
}

func (e *Engine) pumpMicroVMOutput(id string, kind sandbox.Stream, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			e.handleRunnerOutput(id, sandbox.Output{Stream: kind, Data: chunk})
		}
		if err != nil {
			return
		}
	}
}

func (e *Engine) watchMicroVM(inst *microVMInstance, pumpWG *sync.WaitGroup) {
	defer close(inst.done)

	type waitRes struct {
		exit vmproto.ContainerExit
		err  error
	}
	waitCh := make(chan waitRes, 1)
	go func() {
		ex, err := inst.stream.Wait()
		waitCh <- waitRes{exit: ex, err: err}
	}()

	var (
		gotExit  bool
		exitInfo vmproto.ContainerExit
		waitErr  error
	)
	select {
	case res := <-waitCh:
		gotExit = true
		exitInfo = res.exit
		waitErr = res.err
	case <-inst.client.Done():
		// Helper crashed or exited unexpectedly.
		_ = inst.stream.Close()
		res := <-waitCh
		waitErr = res.err
	}

	// Wait for stdout/stderr pumps to finish delivering every buffered byte
	// before reporting container exit.
	pumpWG.Wait()
	_ = inst.stream.Close()

	// Tear down host TCP forwarders and overlay relay immediately.
	inst.forwarders.Close()
	if inst.netStream != nil {
		_ = inst.netStream.Close()
	}
	e.leaveOverlay(inst.id)

	// Shut down the VM cleanly (guest syncs & unmounts ext4 disks and powers off).
	_ = inst.client.Close(microVMHelperCloseGrace)

	// Release volume flocks only after the guest has cleanly unmounted them.
	releaseUnlocks(inst.unlocks)
	inst.unlocks = nil

	if inst.sockDir != "" {
		_ = os.RemoveAll(inst.sockDir)
	}

	e.mvm.mu.Lock()
	if e.mvm.instances[inst.id] == inst {
		delete(e.mvm.instances, inst.id)
	}
	e.mvm.mu.Unlock()

	inst.mu.Lock()
	stopping := inst.stopping
	st := inst.state
	st.PID = 0
	st.Paused = false
	st.ExitedAt = time.Now().UTC()
	if gotExit && waitErr == nil {
		st.ExitCode = exitInfo.Code
		if exitInfo.Signal != "" {
			if sigNum := linuxSignalNum(exitInfo.Signal); sigNum > 0 {
				st.Signal = sigNum
				if st.ExitCode == 0 {
					st.ExitCode = 128 + sigNum
				}
			}
		}
		if exitInfo.Error != "" {
			st.Error = exitInfo.Error
			if st.ExitCode == 0 {
				st.ExitCode = 126
			}
		}
		if stopping {
			st.Status = sandbox.Stopped
		} else if st.ExitCode == 0 {
			st.Status = sandbox.Exited
		} else {
			st.Status = sandbox.Failed
		}
	} else {
		st.ExitCode = 255
		if stopping {
			st.Status = sandbox.Stopped
		} else {
			st.Status = sandbox.Failed
			if cerr := inst.client.ExitErr(); cerr != nil {
				st.Error = cerr.Error()
			} else if waitErr != nil {
				st.Error = waitErr.Error()
			}
		}
	}
	inst.state = st
	inst.mu.Unlock()

	e.onRunnerEvent(sandbox.Event{ID: inst.id, Kind: sandbox.EventExited, State: st})
}

func (e *Engine) microVMShutdown(id, sig string, timeout time.Duration) error {
	inst := e.microVMInstance(id)
	if inst == nil {
		return nil
	}
	inst.mu.Lock()
	inst.stopping = true
	paused := inst.state.Paused
	inst.mu.Unlock()

	if paused {
		uCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = inst.client.ContainerUnpause(uCtx)
		cancel()
	}

	sig = strings.TrimSpace(sig)
	if sig == "" {
		sig = "SIGTERM"
	}

	if timeout <= 0 {
		sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = inst.client.ContainerSignal(sCtx, vmproto.ContainerSignalRequest{Signal: "SIGKILL"})
		cancel()
	} else {
		sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = inst.client.ContainerSignal(sCtx, vmproto.ContainerSignalRequest{Signal: sig})
		cancel()

		timer := time.NewTimer(timeout)
		select {
		case <-inst.done:
			timer.Stop()
			return nil
		case <-timer.C:
			kCtx, kCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = inst.client.ContainerSignal(kCtx, vmproto.ContainerSignalRequest{Signal: "SIGKILL"})
			kCancel()
		}
	}

	// Wait briefly for graceful VM teardown; force-close helper if still stuck.
	killTimer := time.NewTimer(5 * time.Second)
	defer killTimer.Stop()
	select {
	case <-inst.done:
		return nil
	case <-killTimer.C:
		_ = inst.client.Close(2 * time.Second)
		<-inst.done
		return nil
	}
}

func (e *Engine) microVMPause(ctx context.Context, id string) error {
	inst := e.microVMInstance(id)
	if inst == nil {
		return sandbox.ErrNoControl
	}
	if err := inst.client.ContainerPause(ctx); err != nil {
		return mapVMProtoErr(err)
	}
	inst.mu.Lock()
	inst.state.Paused = true
	st := inst.state
	inst.mu.Unlock()
	e.onRunnerEvent(sandbox.Event{ID: id, Kind: sandbox.EventChanged, State: st})
	return nil
}

func (e *Engine) microVMUnpause(ctx context.Context, id string) error {
	inst := e.microVMInstance(id)
	if inst == nil {
		return sandbox.ErrNoControl
	}
	if err := inst.client.ContainerUnpause(ctx); err != nil {
		return mapVMProtoErr(err)
	}
	inst.mu.Lock()
	inst.state.Paused = false
	st := inst.state
	inst.mu.Unlock()
	e.onRunnerEvent(sandbox.Event{ID: id, Kind: sandbox.EventChanged, State: st})
	return nil
}

// ---- Micro-VM overlay network relay & live connect/disconnect (Phase 4) ----

func startMicroVMOverlayRelay(ns microVMNetStream, peer *overlay.DirectPeer) {
	// Host Fabric -> guest gvx0 TUN.
	go func() {
		for {
			pkt, ok := peer.Recv()
			if !ok {
				return
			}
			if err := ns.SendPacket(pkt); err != nil {
				return
			}
		}
	}()

	// Guest gvx0 TUN / 127.0.0.11:53 DNS -> Host Fabric.
	go func() {
		dnsSem := make(chan struct{}, 64)
		for {
			kind, payload, err := ns.ReadFrame()
			if err != nil {
				return
			}
			switch kind {
			case guestproto.FrameNetPacket:
				peer.Send(payload)
			case guestproto.FrameDNSQuery:
				if len(payload) < 6 {
					continue
				}
				seq := binary.BigEndian.Uint32(payload[0:4])
				proto := "udp"
				if payload[4] == 1 {
					proto = "tcp"
				}
				q := append([]byte(nil), payload[5:]...)
				select {
				case dnsSem <- struct{}{}:
					go func(seq uint32, proto string, query []byte) {
						defer func() { <-dnsSem }()
						reply := peer.ExchangeDNS(query, proto)
						_ = ns.SendDNSReply(seq, reply)
					}(seq, proto, q)
				default:
					// Saturated: drop to preserve packet throughput.
				}
			}
		}
	}()
}

// applyMicroVMOverlayLive reconciles a running micro-VM container's gvx0 link
// via OpContainerNetUpdate after docker network connect/disconnect. Caller
// holds overlayApplyLock(cid).
func (e *Engine) applyMicroVMOverlayLive(cid string, c *Container) {
	inst := e.microVMInstance(cid)
	if inst == nil || inst.netStream == nil {
		return
	}
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.mu.Unlock()
	if fab == nil {
		return
	}
	prevAddrs, joined := fab.PeerAddrs(cid)
	if !joined {
		return
	}
	addrs, isolated := e.overlayAddrs(c)
	cfg, gen, ok, err := fab.SetAddrs(cid, addrs)
	if err != nil {
		e.logf("engine: microvm overlay set addrs %s: %v", shortHex(cid), err)
		return
	}
	if !ok {
		return
	}
	revertIfUnchanged := func() {
		if latest, lerr := e.lookup(cid); lerr == nil && latest.State.Status == StatusRunning {
			curAddrs, _ := e.overlayAddrs(&latest)
			if !slices.Equal(curAddrs, addrs) {
				return
			}
		}
		fab.RevertAddrs(cid, gen, prevAddrs)
	}

	updCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = inst.client.ContainerNetUpdate(updCtx, vmproto.ContainerNetConfig{
		Addresses: cfg.Addresses,
		Routes:    cfg.Routes,
		MTU:       cfg.MTU,
		Isolated:  isolated,
		DNS:       true,
	})
	if err != nil {
		revertIfUnchanged()
		e.logf("engine: microvm live network update %s: %v", shortHex(cid), err)
	}
}

// ---- microvmControl implementing sandbox.Control ----

var (
	_ sandbox.Control     = (*microvmControl)(nil)
	_ sandbox.ExecProcess = (*microvmExecProcess)(nil)
)

type microvmControl struct {
	e            *Engine
	id           string
	inst         *microVMInstance
	helperPath   string
	useHelperArg bool
	socket       string
}

func (c *microvmControl) Target() rungv.Target {
	return rungv.Target{Socket: c.socket}
}

func (c *microvmControl) ExecArgv(o rungv.ExecOptions) []string {
	exe := c.helperPath
	if exe == "" {
		exe, _ = os.Executable()
	}
	args := []string{exe}
	if c.useHelperArg {
		args = append(args, vmhelper.Arg)
	}
	args = append(args, vmhelper.CmdExec, "-socket", c.socket)
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	if o.Workdir != "" {
		args = append(args, "-w", o.Workdir)
	}
	for _, kv := range o.Env {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			args = append(args, "-e", kv)
		}
	}
	if o.Privileged != nil && *o.Privileged {
		args = append(args, "-privileged")
	}
	args = append(args, "--")
	args = append(args, o.Argv...)
	return args
}

func (c *microvmControl) Exec(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
	if c.inst.isPaused() {
		return nil, sandbox.ErrPaused
	}
	rows, cols := o.Rows, o.Cols
	if o.TTY {
		if rows == 0 {
			rows = 24
		}
		if cols == 0 {
			cols = 80
		}
	}
	req := vmproto.ContainerExecRequest{
		Argv:        slices.Clone(o.Argv),
		Env:         slices.Clone(o.Env),
		Workdir:     o.Workdir,
		User:        o.User,
		TTY:         o.TTY,
		Interactive: o.Interactive,
		Rows:        rows,
		Cols:        cols,
		Privileged:  o.Privileged != nil && *o.Privileged,
	}
	stream, err := c.inst.client.ContainerExec(ctx, req)
	if err != nil {
		return nil, mapVMProtoErr(err)
	}
	started := make(chan struct{})
	close(started)
	return &microvmExecProcess{
		client:      c.inst.client,
		stream:      stream,
		tty:         o.TTY,
		interactive: o.Interactive,
		started:     started,
	}, nil
}

func (c *microvmControl) CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error) {
	if c.inst.isPaused() {
		return nil, sandbox.ErrPaused
	}
	rc, err := c.inst.client.ContainerCopyFrom(ctx, vmproto.ContainerCopyRequest{
		Path:           guestPath,
		Archive:        o.Archive,
		FollowLink:     o.FollowLink,
		NoOverwriteDir: o.NoOverwriteDir,
		User:           o.User,
	})
	if err != nil {
		return nil, mapVMProtoErr(err)
	}
	return rc, nil
}

func (c *microvmControl) CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error {
	if c.inst.isPaused() {
		return sandbox.ErrPaused
	}
	err := c.inst.client.ContainerCopyTo(ctx, vmproto.ContainerCopyRequest{
		Path:           guestDir,
		Archive:        o.Archive,
		FollowLink:     o.FollowLink,
		NoOverwriteDir: o.NoOverwriteDir,
		User:           o.User,
	}, r)
	return mapVMProtoErr(err)
}

func (c *microvmControl) StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error) {
	st, err := c.inst.client.ContainerStat(ctx, vmproto.ContainerCopyRequest{
		Path: guestPath,
	})
	if err != nil {
		return rungv.PathStat{}, mapVMProtoErr(err)
	}
	return rungv.PathStat{
		Name:       st.Name,
		Size:       st.Size,
		Mode:       st.Mode,
		Mtime:      st.Mtime,
		LinkTarget: st.LinkTarget,
	}, nil
}

func (c *microvmControl) Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error) {
	rc, err := c.inst.client.ContainerExport(ctx, vmproto.ContainerExportRequest{
		Path:      guestPath,
		UpperOnly: upperOnly,
	})
	if err != nil {
		return nil, mapVMProtoErr(err)
	}
	return rc, nil
}

func (c *microvmControl) Stats(ctx context.Context) (rungv.Stats, error) {
	st, err := c.inst.client.ContainerStats(ctx)
	if err != nil {
		return rungv.Stats{}, mapVMProtoErr(err)
	}
	return convertMicroVMStats(st), nil
}

func (c *microvmControl) StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error {
	if interval <= 0 {
		interval = time.Second
	}
	st, err := c.Stats(ctx)
	if err != nil {
		return err
	}
	if fn != nil {
		fn(st)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.inst.done:
			return nil
		case <-tick.C:
			st, err := c.Stats(ctx)
			if err != nil {
				return err
			}
			if fn != nil {
				fn(st)
			}
		}
	}
}

func (c *microvmControl) Top(ctx context.Context) ([]rungv.Process, error) {
	procs, err := c.inst.client.ContainerTop(ctx)
	if err != nil {
		return nil, mapVMProtoErr(err)
	}
	out := make([]rungv.Process, 0, len(procs))
	for _, p := range procs {
		out = append(out, rungv.Process{
			PID:      p.PID,
			PPID:     p.PPID,
			UID:      p.UID,
			GID:      p.GID,
			State:    p.State,
			Threads:  p.Threads,
			CPUNanos: p.CPUNanos,
			RSSBytes: p.RSSBytes,
			Comm:     p.Comm,
			Cmdline:  slices.Clone(p.Cmdline),
		})
	}
	return out, nil
}

func (c *microvmControl) Kill(ctx context.Context, o rungv.KillOptions) error {
	err := c.inst.client.ContainerSignal(ctx, vmproto.ContainerSignalRequest{
		Signal: o.Signal,
		PID:    o.PID,
		All:    o.All,
	})
	return mapVMProtoErr(err)
}

type microvmExecProcess struct {
	client      microVMClient
	stream      microVMStream
	tty         bool
	interactive bool
	started     chan struct{}
}

func (p *microvmExecProcess) Stdin() io.WriteCloser {
	if !p.interactive {
		return nil
	}
	return p.stream.Stdin()
}

func (p *microvmExecProcess) Stdout() io.Reader {
	return p.stream.Stdout()
}

func (p *microvmExecProcess) Stderr() io.Reader {
	if p.tty {
		return nil
	}
	return p.stream.Stderr()
}

func (p *microvmExecProcess) Resize(rows, cols uint16) error {
	if !p.tty {
		return errors.New("cannot resize non-TTY exec session")
	}
	return p.stream.Resize(rows, cols)
}

func (p *microvmExecProcess) PID() int {
	return p.stream.PID()
}

func (p *microvmExecProcess) Started() <-chan struct{} {
	return p.started
}

func (p *microvmExecProcess) Wait() (int, error) {
	ex, err := p.stream.Wait()
	if err != nil {
		return -1, err
	}
	if ex.Error != "" {
		return ex.Code, errors.New(ex.Error)
	}
	if ex.Code == 0 && ex.Signal != "" {
		if sigNum := linuxSignalNum(ex.Signal); sigNum > 0 {
			return 128 + sigNum, nil
		}
	}
	return ex.Code, nil
}

func (p *microvmExecProcess) Signal(ctx context.Context, sig string) error {
	pid := p.stream.PID()
	if pid <= 0 {
		return errors.New("exec process has no PID")
	}
	return mapVMProtoErr(p.client.ContainerSignal(ctx, vmproto.ContainerSignalRequest{
		Signal: sig,
		PID:    pid,
	}))
}

func (p *microvmExecProcess) Close() error {
	return p.stream.Close()
}

// ---- Conversion & error mapping helpers ----

func convertMicroVMStats(st vmproto.ContainerStats) rungv.Stats {
	var out rungv.Stats
	out.Timestamp = st.Timestamp
	out.CPU.UserNanos = st.CPUUserNanos
	out.CPU.SystemNanos = st.CPUSystemNanos
	out.CPU.Periods = st.CPUPeriods
	out.CPU.Throttled = st.CPUThrottled
	out.CPU.ThrottledNanos = st.CPUThrottledNs
	out.Memory.TotalBytes = st.MemoryBytes
	out.Memory.AnonBytes = st.MemoryAnonBytes
	out.Memory.FileBytes = st.MemoryFileBytes
	out.Memory.TmpfsBytes = st.MemoryTmpfsBytes
	out.Memory.LimitBytes = st.MemoryLimitBytes
	if len(st.Network) > 0 {
		out.Network = make([]struct {
			Interface string `json:"interface"`
			RxBytes   uint64 `json:"rx_bytes"`
			TxBytes   uint64 `json:"tx_bytes"`
			RxPackets uint64 `json:"rx_packets"`
			TxPackets uint64 `json:"tx_packets"`
		}, len(st.Network))
		for i, n := range st.Network {
			out.Network[i].Interface = n.Interface
			out.Network[i].RxBytes = n.RxBytes
			out.Network[i].TxBytes = n.TxBytes
			out.Network[i].RxPackets = n.RxPackets
			out.Network[i].TxPackets = n.TxPackets
		}
	}
	out.PIDs = st.PIDs
	out.PIDsLimit = st.PIDsLimit
	return out
}

func mapVMProtoErr(err error) error {
	if err == nil {
		return nil
	}
	if ve, ok := errors.AsType[*vmproto.Error](err); ok {
		switch ve.Code {
		case vmproto.CodePaused:
			return fmt.Errorf("%w: %w", sandbox.ErrPaused, ve)
		case vmproto.CodeNotRunning:
			return fmt.Errorf("%w: %w", sandbox.ErrNoControl, ve)
		}
	}
	return err
}

func linuxSignalNum(sig string) int {
	s := strings.TrimSpace(strings.ToUpper(sig))
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 64 {
		return n
	}
	s = strings.TrimPrefix(s, "SIG")
	switch s {
	case "HUP":
		return 1
	case "INT":
		return 2
	case "QUIT":
		return 3
	case "ILL":
		return 4
	case "TRAP":
		return 5
	case "ABRT":
		return 6
	case "BUS":
		return 7
	case "FPE":
		return 8
	case "KILL":
		return 9
	case "USR1":
		return 10
	case "SEGV":
		return 11
	case "USR2":
		return 12
	case "PIPE":
		return 13
	case "ALRM":
		return 14
	case "TERM":
		return 15
	case "CONT":
		return 18
	case "STOP":
		return 19
	default:
		return 0
	}
}
