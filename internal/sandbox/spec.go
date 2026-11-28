// Package sandbox describes the sandboxes the desktop app keeps and runs
// them as supervised rungv processes.
package sandbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// MountType names the kind of a mount.
type MountType string

const (
	MountVolume MountType = "volume" // a named ext4lite volume, or an .ext4 file at a path
	MountBind   MountType = "bind"   // a host directory or file
	MountTmpfs  MountType = "tmpfs"  // memory-backed
)

// SymlinkMode values for hostfs directory bind mounts (rungvx v0.26+).
const (
	SymlinksVirtual   = "virtual"   // store guest symlinks only in .gvx_meta (default)
	SymlinksContained = "contained" // materialize safe relative symlinks on macOS/Linux hosts
)

// Mount is one mount of a sandbox.
type Mount struct {
	Type      MountType `json:"type"`
	Source    string    `json:"source,omitempty"` // volume name, image path, or host path
	Target    string    `json:"target"`
	ReadOnly  bool      `json:"readOnly,omitempty"`
	Size      string    `json:"size,omitempty"`      // tmpfs or ext4lite volume size cap, such as 64m or 2g
	Ephemeral bool      `json:"ephemeral,omitempty"` // volume mounts only: scratch ext4lite image discarded at exit (rungvx v0.22+)
	Symlinks  string    `json:"symlinks,omitempty"`  // bind mounts only: "", "virtual", or "contained" (rungvx v0.26+)
}

// Spec is everything a sandbox starts with. Because a gvx sandbox locks its
// host process down before the guest runs, every port and mount here is
// fixed for the life of a run.
type Spec struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	// Command replaces the image Cmd, as a shell-quoted line.
	Command    string `json:"command,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
	// SetEntrypoint makes Entrypoint apply even when empty, which clears it.
	SetEntrypoint bool `json:"setEntrypoint,omitempty"`

	Env      []string `json:"env,omitempty"`      // KEY=VALUE, or KEY to copy from the host
	EnvFiles []string `json:"envFiles,omitempty"` // host .env files (--env-file)
	Ports    []string `json:"ports,omitempty"`    // rungv -p specs
	// UnixForwards forward guest Unix sockets to host Unix sockets (--unix-forward):
	// GUEST_PATH=HOST_SOCKET[,uid=N,gid=N,mode=0600].
	UnixForwards []string `json:"unixForwards,omitempty"`

	Mounts            []Mount `json:"mounts,omitempty"`
	Workspace         string  `json:"workspace,omitempty"`
	WorkspaceRO       bool    `json:"workspaceRO,omitempty"`
	WorkspaceSymlinks string  `json:"workspaceSymlinks,omitempty"` // "", "virtual", or "contained" (rungvx v0.26+)
	RootfsVolume      string  `json:"rootfsVolume,omitempty"`
	ReadOnlyRoot      bool    `json:"readOnlyRoot,omitempty"` // --read-only (incompatible with RootfsVolume)

	Memory         string `json:"memory,omitempty"`         // 512m, 2g
	CPUs           string `json:"cpus,omitempty"`           // 1.5
	PidsLimit      int64  `json:"pidsLimit,omitempty"`      // --pids-limit (0 = unlimited)
	GPU            string `json:"gpu,omitempty"`            // "", "venus", or "virgl" (rungvx v0.28+ -tags ext_gpu)
	GPUMemoryLimit string `json:"gpuMemoryLimit,omitempty"` // -gpu-memory-limit (e.g. 512m, 2g)

	Network    string `json:"network,omitempty"` // nat (default), none, wg
	WGConf     string `json:"wgConf,omitempty"`
	IsolateLAN bool   `json:"isolateLAN,omitempty"`
	// NetTXBPS and NetRXBPS cap NAT bandwidth in bits per second (0 = unlimited).
	NetTXBPS uint64 `json:"netTxBps,omitempty"`
	NetRXBPS uint64 `json:"netRxBps,omitempty"`
	// NATPolicyFile is a host .json/.yaml/.yml NAT firewall policy file (--nat-policy-file).
	NATPolicyFile string `json:"natPolicyFile,omitempty"`
	// DynamicPortAddrs are extra host IP literals allowed for runtime `rungv port add`
	// under host lockdown (--dynamic-ports-addr).
	DynamicPortAddrs []string `json:"dynamicPortAddrs,omitempty"`
	// AutoPublishAddr is the host IP literal for --auto-publish (--auto-publish-addr).
	AutoPublishAddr string `json:"autoPublishAddr,omitempty"`
	Backend         string `json:"backend,omitempty"` // auto (default), hvf, whpx, systrap

	Hostname string `json:"hostname,omitempty"`
	User     string `json:"user,omitempty"`
	Workdir  string `json:"workdir,omitempty"`

	// Unprivileged turns --privileged off. rungv keeps it on by default,
	// because its privileges stay inside the sandbox.
	Unprivileged bool   `json:"unprivileged,omitempty"`
	HostCACerts  bool   `json:"hostCACerts,omitempty"`
	Pull         string `json:"pull,omitempty"` // missing (default), always, never

	// Checkpoint pre-opens a checkpoint directory and a control socket so
	// the running sandbox can be snapshotted.
	Checkpoint bool `json:"checkpoint,omitempty"`
	AutoStart  bool `json:"autoStart,omitempty"`

	// Recipe names the preset the sandbox came from: docker, k3s, or "".
	Recipe string `json:"recipe,omitempty"`
	// ReadyPattern is a log substring that marks the guest as ready.
	ReadyPattern string `json:"readyPattern,omitempty"`

	ExtraArgs string    `json:"extraArgs,omitempty"` // more rungv run flags, shell-quoted
	CreatedAt time.Time `json:"createdAt"`

	// Interactive gives the run a standard input that Supervisor.Stdin
	// writes to (rungv -i), in place of the empty one that stays open.
	Interactive bool `json:"interactive,omitempty"`
	// TTY runs the guest on a pseudo-terminal (rungv -t) that the
	// supervisor holds the other side of.
	TTY bool `json:"tty,omitempty"`
	// ContainerID marks a sandbox the app's Docker engine runs for a
	// container: it is not one of the user's sandboxes.
	ContainerID string `json:"containerId,omitempty"`
	// Labels stores host metadata such as extension ownership (io.gvx.extension).
	Labels map[string]string `json:"labels,omitempty"`
}

// NewID returns a short random identifier.
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var (
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	volumeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	memoryRe = regexp.MustCompile(`^(?i)\d+(\.\d+)?[bkmgt]?$`)
)

// SuggestName derives a sandbox name from an image reference.
func SuggestName(image string) string {
	name := image
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-._")
	if name == "" {
		return "sandbox"
	}
	return name
}

// Validate reports the first problem that would stop the sandbox.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("Give the sandbox a name.")
	}
	if !nameRe.MatchString(s.Name) {
		return errors.New("Use letters, digits, '-', '_' and '.' in the name.")
	}
	if strings.TrimSpace(s.Image) == "" {
		return errors.New("Choose an image to run.")
	}
	if strings.ContainsAny(s.Image, " \t") {
		return errors.New("An image reference has no spaces.")
	}
	if _, err := rungv.SplitArgs(s.Command); err != nil {
		return fmt.Errorf("Command: %v.", err)
	}
	if _, err := rungv.SplitArgs(s.ExtraArgs); err != nil {
		return fmt.Errorf("Extra flags: %v.", err)
	}
	for _, p := range s.Ports {
		if err := ValidatePort(p); err != nil {
			return err
		}
	}
	ufGuests := map[string]bool{}
	for _, uf := range s.UnixForwards {
		if err := ValidateUnixForward(uf); err != nil {
			return err
		}
		gp, _, _ := strings.Cut(strings.TrimSpace(uf), "=")
		gp = path.Clean(strings.TrimSpace(gp))
		if ufGuests[gp] {
			return fmt.Errorf("Two Unix forwards share the guest path %s.", gp)
		}
		ufGuests[gp] = true
	}
	targets := map[string]bool{}
	if s.Workspace != "" {
		targets["/workspace"] = true
	}
	if err := validateSymlinkMode(s.WorkspaceSymlinks); err != nil {
		return fmt.Errorf("Workspace symlinks: %w", err)
	}
	if s.Workspace != "" && s.WorkspaceSymlinks == SymlinksContained {
		if strings.Contains(rungv.ExpandHome(s.Workspace), ",") {
			return errors.New("Workspace path cannot contain a comma when host symbolic links are enabled.")
		}
	}
	for _, m := range s.Mounts {
		if err := m.validate(); err != nil {
			return err
		}
		if targets[m.Target] {
			return fmt.Errorf("Two mounts share the guest path %s.", m.Target)
		}
		targets[m.Target] = true
	}
	if s.ReadOnlyRoot && s.RootfsVolume != "" {
		return errors.New("A read-only root file system cannot be combined with a persistent root volume.")
	}
	if s.RootfsVolume != "" {
		for _, m := range s.Mounts {
			if m.Type == MountVolume && !m.Ephemeral && m.Source == s.RootfsVolume {
				return errors.New("The root file system volume cannot also be mounted as a data volume.")
			}
		}
	}
	if s.Memory != "" && !memoryRe.MatchString(s.Memory) {
		return errors.New("Write the memory limit as a number with a unit, such as 512m or 2g.")
	}
	if s.CPUs != "" {
		if v, err := strconv.ParseFloat(s.CPUs, 64); err != nil || v <= 0 {
			return errors.New("Write the CPU limit as a positive number, such as 1.5.")
		}
	}
	if s.PidsLimit < 0 {
		return errors.New("Write the PIDs limit as a positive integer, or leave it empty for unlimited.")
	}
	switch strings.ToLower(strings.TrimSpace(s.GPU)) {
	case "", "off", "venus", "virgl":
	default:
		return fmt.Errorf("Unsupported GPU mode %q (use off, venus, or virgl).", s.GPU)
	}
	if s.UsesGPU() && s.Checkpoint {
		return errors.New("GPU acceleration cannot be combined with snapshots (checkpoint and restore).")
	}
	if strings.TrimSpace(s.GPUMemoryLimit) != "" {
		if !s.UsesGPU() {
			return errors.New("GPU memory limit requires GPU acceleration (venus or virgl) to be enabled.")
		}
		if !memoryRe.MatchString(strings.TrimSpace(s.GPUMemoryLimit)) {
			return errors.New("Write the GPU memory limit as a number with a unit, such as 512m or 2g.")
		}
	}
	for _, e := range s.Env {
		if k, _, _ := strings.Cut(e, "="); strings.TrimSpace(k) == "" || strings.ContainsAny(k, " \t") {
			return fmt.Errorf("Environment entry %q needs a KEY.", e)
		}
	}
	if s.Network == "wg" && s.WGConf == "" {
		return errors.New("The WireGuard network needs a configuration file.")
	}
	isNAT := s.Network == "" || s.Network == "nat"
	if !isNAT {
		for _, p := range s.Ports {
			if !strings.HasPrefix(strings.TrimSpace(p), "unix:") {
				return errors.New("Published ports need the NAT network.")
			}
		}
	}
	if !isNAT && (s.NetTXBPS > 0 || s.NetRXBPS > 0) {
		return errors.New("Network bandwidth limits need the NAT network.")
	}
	if f := strings.TrimSpace(s.NATPolicyFile); f != "" {
		if !isNAT {
			return errors.New("NAT firewall policy needs the NAT network.")
		}
		switch strings.ToLower(filepath.Ext(f)) {
		case ".json", ".yaml", ".yml":
		default:
			return fmt.Errorf("NAT policy file %q must end in .json, .yaml, or .yml.", f)
		}
	}
	if len(s.DynamicPortAddrs) > 0 {
		if !isNAT {
			return errors.New("Dynamic port bind addresses need the NAT network.")
		}
		for _, raw := range s.DynamicPortAddrs {
			if _, err := parseBareIPAddr(raw); err != nil {
				return fmt.Errorf("Dynamic port bind address %q: must be a valid IP literal.", raw)
			}
		}
	}
	if a := strings.TrimSpace(s.AutoPublishAddr); a != "" {
		if !isNAT {
			return errors.New("Auto-publish address needs the NAT network.")
		}
		if !HasAutoPublish(s) {
			return errors.New("Auto-publish address requires auto-publish to be enabled.")
		}
		if _, err := parseBareIPAddr(a); err != nil {
			return fmt.Errorf("Auto-publish address %q: must be a valid IP literal.", a)
		}
	}
	return nil
}

func parseBareIPAddr(raw string) (netip.Addr, error) {
	s := strings.TrimSpace(raw)
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	if addr.Zone() != "" {
		return netip.Addr{}, errors.New("IP address must not include a zone")
	}
	return addr, nil
}

const maxUnixSocketPathLen = 107

// ValidateUnixForward checks a --unix-forward specification of the form
// GUEST_PATH=HOST_SOCKET[,uid=N][,gid=N][,mode=0600].
func ValidateUnixForward(spec string) error {
	raw := strings.TrimSpace(spec)
	guestPath, rhs, ok := strings.Cut(raw, "=")
	if !ok {
		return fmt.Errorf("Unix forward %q: write GUEST_PATH=HOST_SOCKET[,uid=N,gid=N,mode=0600].", spec)
	}
	guestPath = strings.TrimSpace(guestPath)
	if !strings.HasPrefix(guestPath, "/") || guestPath == "/" {
		return fmt.Errorf("Unix forward guest path %q must be an absolute path other than /.", guestPath)
	}
	if strings.ContainsRune(guestPath, 0) {
		return fmt.Errorf("Unix forward guest path %q must not contain NUL bytes.", guestPath)
	}
	for seg := range strings.SplitSeq(guestPath, "/") {
		if seg == ".." {
			return fmt.Errorf("Unix forward guest path %q must not contain '..'.", guestPath)
		}
	}
	if len(guestPath) > maxUnixSocketPathLen {
		return fmt.Errorf("Unix forward guest path %q exceeds the %d-byte socket path limit.", guestPath, maxUnixSocketPathLen)
	}
	parts := strings.Split(rhs, ",")
	hostSock := strings.TrimSpace(parts[0])
	if hostSock == "" {
		return fmt.Errorf("Unix forward %q: host socket path must not be empty.", spec)
	}
	if strings.ContainsRune(hostSock, 0) {
		return fmt.Errorf("Unix forward %q: host socket path must not contain NUL bytes.", spec)
	}
	for _, opt := range parts[1:] {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		k, v, hasEq := strings.Cut(opt, "=")
		if !hasEq {
			return fmt.Errorf("Unix forward %q: option %q must be key=value.", spec, opt)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "uid":
			if _, err := strconv.ParseUint(v, 10, 32); err != nil {
				return fmt.Errorf("Unix forward %q: invalid uid %q.", spec, v)
			}
		case "gid":
			if _, err := strconv.ParseUint(v, 10, 32); err != nil {
				return fmt.Errorf("Unix forward %q: invalid gid %q.", spec, v)
			}
		case "mode":
			n, err := strconv.ParseUint(v, 8, 32)
			if err != nil || n == 0 || n > 0o777 {
				return fmt.Errorf("Unix forward %q: mode %q must be octal 0001..0777.", spec, v)
			}
		default:
			return fmt.Errorf("Unix forward %q: unknown option %q (want uid, gid, or mode).", spec, k)
		}
	}
	return nil
}

func validateSymlinkMode(mode string) error {
	switch mode {
	case "", SymlinksVirtual, SymlinksContained:
		return nil
	default:
		return fmt.Errorf("unsupported symlink mode %q (use %q or %q).", mode, SymlinksVirtual, SymlinksContained)
	}
}

// UsesBindSymlinks reports whether the spec configures hostfs symlink mode
// options that require rungvx v0.26+ (--mount ...,symlinks=...).
func (s Spec) UsesBindSymlinks() bool {
	if s.Workspace != "" && s.WorkspaceSymlinks == SymlinksContained {
		return true
	}
	for _, m := range s.Mounts {
		if m.Type == MountBind && m.Symlinks != "" {
			return true
		}
	}
	return false
}

// UsesVolumeOptions reports whether the spec configures ext4lite volume
// mount options (size= or ephemeral) that require rungvx v0.22+.
func (s Spec) UsesVolumeOptions() bool {
	for _, m := range s.Mounts {
		if m.Type == MountVolume && (m.Ephemeral || m.Size != "") {
			return true
		}
	}
	return false
}

// UsesGPU reports whether the spec enables virtio-gpu acceleration (-gpu).
func (s Spec) UsesGPU() bool {
	mode := strings.ToLower(strings.TrimSpace(s.GPU))
	return mode != "" && mode != "off"
}

func (m Mount) validate() error {
	if !strings.HasPrefix(m.Target, "/") || m.Target == "/" {
		return fmt.Errorf("Mount target %q must be an absolute guest path other than /.", m.Target)
	}
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if (m.Target == p || strings.HasPrefix(m.Target, p+"/")) && !strings.HasPrefix(m.Target, "/dev/shm") {
			return fmt.Errorf("Mount target %s is reserved by the guest kernel.", m.Target)
		}
	}
	if err := validateSymlinkMode(m.Symlinks); err != nil {
		return fmt.Errorf("Mount at %s: %w", m.Target, err)
	}
	if m.Symlinks != "" && m.Type != MountBind {
		return fmt.Errorf("Mount at %s: symlink mode applies to bind mounts only.", m.Target)
	}
	if m.Ephemeral && m.Type != MountVolume {
		return fmt.Errorf("Mount at %s: ephemeral applies to volume mounts only.", m.Target)
	}
	switch m.Type {
	case MountVolume:
		if m.Size != "" && !memoryRe.MatchString(m.Size) {
			return fmt.Errorf("Volume at %s: write size as a number with a unit, such as 512m or 2g.", m.Target)
		}
		if m.Ephemeral && m.ReadOnly {
			return fmt.Errorf("Volume at %s: ephemeral volumes cannot be read-only.", m.Target)
		}
		if !m.Ephemeral {
			if m.Source == "" {
				return fmt.Errorf("The volume at %s needs a name.", m.Target)
			}
			if !strings.ContainsAny(m.Source, `/\`) && !volumeRe.MatchString(m.Source) {
				return fmt.Errorf("Volume name %q: use letters, digits, '-', '_' and '.'.", m.Source)
			}
		} else if m.Source != "" && !strings.ContainsAny(m.Source, `/\`) && !volumeRe.MatchString(m.Source) {
			return fmt.Errorf("Volume name %q: use letters, digits, '-', '_' and '.'.", m.Source)
		}
	case MountBind:
		if m.Source == "" {
			return fmt.Errorf("The bind mount at %s needs a host path.", m.Target)
		}
		if m.Symlinks != "" && (strings.Contains(rungv.ExpandHome(m.Source), ",") || strings.Contains(m.Target, ",")) {
			return fmt.Errorf("Bind mount at %s: paths cannot contain a comma when symlinks mode is set.", m.Target)
		}
	case MountTmpfs:
	default:
		return fmt.Errorf("Unknown mount type %q.", m.Type)
	}
	return nil
}

var portRe = regexp.MustCompile(`^(?:(\[[0-9a-fA-F:.]+\]|\d{1,3}(?:\.\d{1,3}){3}):)?(?:(\d+(?:-\d+)?):)?(\d+(?:-\d+)?)(?:/(tcp|udp))?$`)

// ValidatePort checks a -p specification.
func ValidatePort(p string) error {
	p = strings.TrimSpace(p)
	if after, ok := strings.CutPrefix(p, "unix:"); ok {
		rest := after
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 {
			return fmt.Errorf("Port %q: write unix:PATH:GUEST_PORT.", p)
		}
		if _, err := strconv.Atoi(rest[i+1:]); err != nil {
			return fmt.Errorf("Port %q: the guest port must be a number.", p)
		}
		if strings.Contains(rest, ",") {
			return fmt.Errorf("Port %q: a socket path cannot contain a comma.", p)
		}
		return nil
	}
	m := portRe.FindStringSubmatch(p)
	if m == nil {
		return fmt.Errorf("Port %q: write [IP:][HOST:]GUEST[/udp], such as 8080:80.", p)
	}
	host, guest := m[2], m[3]
	gl, err := rangeLen(guest)
	if err != nil {
		return fmt.Errorf("Port %q: %v", p, err)
	}
	if host != "" {
		hl, err := rangeLen(host)
		if err != nil {
			return fmt.Errorf("Port %q: %v", p, err)
		}
		if hl != gl {
			return fmt.Errorf("Port %q: the host and guest ranges must have the same length.", p)
		}
	}
	return nil
}

func rangeLen(s string) (int, error) {
	lo, hi, isRange := strings.Cut(s, "-")
	a, err := strconv.Atoi(lo)
	if err != nil || a > 65535 {
		return 0, errors.New("ports run from 0 to 65535.")
	}
	if !isRange {
		return 1, nil
	}
	b, err := strconv.Atoi(hi)
	if err != nil || b > 65535 || b < a {
		return 0, errors.New("a range runs from a low port to a higher one.")
	}
	if b-a+1 > 1024 {
		return 0, errors.New("one range holds at most 1024 ports.")
	}
	return b - a + 1, nil
}

// Paths are the host files a run of a sandbox uses besides its spec.
type Paths struct {
	// ControlSocket is set when the spec has Checkpoint (as with rungv
	// v0.19) and, with a rungv whose Features have Control, for every run:
	// passed as -control-socket. See docs/design/gvx-v020.md §4.1.
	ControlSocket string
	// TokenFile is set with ControlSocket where the socket needs a token
	// (a Unix socket on Windows): --control-token-file on run and restore,
	// --token-file on every client command (rungv.Target). Owner: W1.
	TokenFile string
	// Control reports that the run's socket speaks gvx-ctrl/2: the rungv
	// that starts it has Features.Control. Owner: W1.
	Control       bool
	CheckpointDir string // set when the spec has Checkpoint
	// DynamicPorts passes --dynamic-ports on run and restore so `rungv port add`
	// works under host lockdown.
	DynamicPorts bool
	// AllowBindAttach passes --allow-bind-attach on run and restore so POSIX
	// SCM_RIGHTS `rungv mount add --type bind` works under host lockdown.
	AllowBindAttach bool
	// AllowVolumeAttach passes --allow-volume-attach on run and restore so POSIX
	// SCM_RIGHTS `rungv mount add --type volume` works under host lockdown.
	AllowVolumeAttach bool
	// RestoreLiveMounts reports that rungv restore accepts --attach-bind and
	// --serve, so RestoreArgs inspects <dir>/manifest.json to reattach
	// checkpointed dynamic bind and rpcfs mounts.
	RestoreLiveMounts bool
}

// RunArgs is the argument vector of rungv run for the spec, without the
// rungv executable itself.
func (s Spec) RunArgs(p Paths) []string {
	args := []string{"run"}
	args = append(args, s.flags(p, false, nil)...)
	args = append(args, s.Image)
	cmd, _ := rungv.SplitArgs(s.Command)
	return append(args, cmd...)
}

const maxRestoreManifestBytes = 1 << 20

type restoreManifest struct {
	Mounts []restoreManifestMount `json:"mounts,omitempty"`
}

type restoreManifestMount struct {
	Type       string `json:"type"`
	GuestPath  string `json:"guest_path"`
	ReadOnly   bool   `json:"read_only"`
	Kind       string `json:"kind,omitempty"`
	Dynamic    bool   `json:"dynamic,omitempty"`
	Label      string `json:"label,omitempty"`
	SingleFile bool   `json:"single_file,omitempty"`
}

// restoreLiveMountFlags reads <dir>/manifest.json and returns guest paths that
// must be omitted from -v/-workspace along with the --attach-bind and --serve
// values required to reattach checkpointed dynamic bind and rpcfs mounts.
func (s Spec) restoreLiveMountFlags(dir string) (skipTargets map[string]bool, attachBinds, serveMounts []string) {
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRestoreManifestBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxRestoreManifestBytes {
		return nil, nil, nil
	}
	var m restoreManifest
	if err := json.Unmarshal(data, &m); err != nil || len(m.Mounts) == 0 {
		return nil, nil, nil
	}

	specBinds := make(map[string]string, len(s.Mounts)+1)
	if ws := strings.TrimSpace(s.Workspace); ws != "" {
		specBinds["/workspace"] = rungv.ExpandHome(ws)
	}
	for _, sm := range s.Mounts {
		if sm.Type != MountBind {
			continue
		}
		gp := path.Clean(strings.TrimSpace(sm.Target))
		if src := strings.TrimSpace(sm.Source); gp != "" && src != "" {
			specBinds[gp] = rungv.ExpandHome(src)
		}
	}

	resolveHost := func(mm restoreManifestMount, guestPath string) string {
		if label := strings.TrimSpace(mm.Label); label != "" {
			return rungv.ExpandHome(label)
		}
		return specBinds[guestPath]
	}

	seen := make(map[string]bool, len(m.Mounts))
	for _, mm := range m.Mounts {
		rawGuest := strings.TrimSpace(mm.GuestPath)
		if !strings.HasPrefix(rawGuest, "/") {
			continue
		}
		guestPath := path.Clean(rawGuest)
		if guestPath == "/" || seen[guestPath] {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(mm.Type)) {
		case "bind":
			if !mm.Dynamic {
				continue
			}
			hostPath := resolveHost(mm, guestPath)
			if hostPath == "" {
				continue
			}
			seen[guestPath] = true
			if skipTargets == nil {
				skipTargets = map[string]bool{}
			}
			skipTargets[guestPath] = true
			attachBinds = append(attachBinds, volumeSpec(hostPath, guestPath, mm.ReadOnly))
		case "rpcfs":
			seen[guestPath] = true
			if skipTargets == nil {
				skipTargets = map[string]bool{}
			}
			skipTargets[guestPath] = true
			if hostPath := resolveHost(mm, guestPath); hostPath != "" {
				serveMounts = append(serveMounts, volumeSpec(hostPath, guestPath, mm.ReadOnly))
			}
		}
	}
	return skipTargets, attachBinds, serveMounts
}

// RestoreArgs resumes the checkpoint bundle in dir with the same mounts,
// ports and network the bundle was written with.
func (s Spec) RestoreArgs(dir string, p Paths) []string {
	args := []string{"restore", "-checkpoint-dir", dir}
	// A restore takes a new listener and directory for the next snapshot.
	if p.ControlSocket != "" {
		args = append(args, "-control-socket", p.ControlSocket)
	}
	if p.TokenFile != "" {
		args = append(args, "--control-token-file", p.TokenFile)
	}
	var skipMounts map[string]bool
	var attachBinds, serveMounts []string
	if p.RestoreLiveMounts && dir != "" {
		skipMounts, attachBinds, serveMounts = s.restoreLiveMountFlags(dir)
	}
	args = append(args, s.flags(Paths{
		DynamicPorts:      p.DynamicPorts,
		AllowBindAttach:   p.AllowBindAttach,
		AllowVolumeAttach: p.AllowVolumeAttach,
	}, true, skipMounts)...)
	for _, ab := range attachBinds {
		args = append(args, "--attach-bind", ab)
	}
	for _, sm := range serveMounts {
		args = append(args, "--serve", sm)
	}
	return args
}

func (s Spec) flags(p Paths, restore bool, skipMounts map[string]bool) []string {
	var a []string
	add := func(v ...string) { a = append(a, v...) }
	// The name is the guest's container ID, which a restore needs to match
	// the bundle's host mounts.
	if s.Name != "" {
		add("--name", s.Name)
	}
	if !restore {
		if s.Hostname != "" {
			add("--hostname", s.Hostname)
		}
		if s.User != "" {
			add("--user", s.User)
		}
		if s.SetEntrypoint || s.Entrypoint != "" {
			add("--entrypoint", s.Entrypoint)
		}
		for _, e := range s.Env {
			add("-e", e)
		}
		for _, ef := range s.EnvFiles {
			if f := strings.TrimSpace(ef); f != "" {
				add("--env-file", rungv.ExpandHome(f))
			}
		}
		if s.Memory != "" {
			add("--memory", s.Memory)
		}
		if s.CPUs != "" {
			add("--cpus", s.CPUs)
		}
		if s.PidsLimit > 0 {
			add("--pids-limit", strconv.FormatInt(s.PidsLimit, 10))
		}
		if s.UsesGPU() {
			add("-gpu", strings.ToLower(strings.TrimSpace(s.GPU)))
			if lim := strings.TrimSpace(s.GPUMemoryLimit); lim != "" {
				add("-gpu-memory-limit", lim)
			}
		}
		if s.ReadOnlyRoot {
			add("--read-only")
		}
		if s.Backend != "" && s.Backend != "auto" {
			add("--backend", s.Backend)
		}
		if s.Unprivileged {
			add("--privileged=false")
		}
		if s.HostCACerts {
			add("--host-ca-certs")
		}
		if s.Pull != "" && s.Pull != "missing" {
			add("--pull", s.Pull)
		}
		if s.Interactive {
			add("-i")
		}
		if s.TTY {
			add("-t")
		}
	}
	if s.Workdir != "" {
		add("--workdir", s.Workdir)
	}
	switch s.Network {
	case "", "nat":
	default:
		add("-net", s.Network)
	}
	if s.Network == "wg" && s.WGConf != "" {
		add("-wg-conf", rungv.ExpandHome(s.WGConf))
	}
	if s.IsolateLAN {
		add("-isolate-lan")
	}
	isNAT := s.Network == "" || s.Network == "nat"
	if isNAT {
		if s.NetTXBPS > 0 {
			add("--net-tx-bps", strconv.FormatUint(s.NetTXBPS, 10))
		}
		if s.NetRXBPS > 0 {
			add("--net-rx-bps", strconv.FormatUint(s.NetRXBPS, 10))
		}
		if f := strings.TrimSpace(s.NATPolicyFile); f != "" {
			add("--nat-policy-file", rungv.ExpandHome(f))
		}
		if !restore {
			if a := strings.TrimSpace(s.AutoPublishAddr); a != "" && HasAutoPublish(s) {
				add("--auto-publish-addr", a)
			}
		}
		if p.DynamicPorts {
			add("--dynamic-ports")
		}
		for _, addr := range s.DynamicPortAddrs {
			if a := strings.TrimSpace(addr); a != "" {
				add("--dynamic-ports-addr", a)
			}
		}
	}
	for _, port := range s.Ports {
		add("-p", strings.TrimSpace(port))
	}
	for _, uf := range s.UnixForwards {
		if u := strings.TrimSpace(uf); u != "" {
			add("--unix-forward", u)
		}
	}
	if s.Workspace != "" && !skipMounts["/workspace"] {
		if s.WorkspaceSymlinks == SymlinksContained {
			add(bindMountArgs(rungv.ExpandHome(s.Workspace), "/workspace", s.WorkspaceRO, s.WorkspaceSymlinks)...)
		} else {
			add("-workspace", rungv.ExpandHome(s.Workspace))
			if s.WorkspaceRO {
				add("-ro")
			}
		}
	}
	if s.RootfsVolume != "" {
		add("-rootfs-volume", rungv.ExpandHome(s.RootfsVolume))
	}
	for _, m := range s.Mounts {
		if len(skipMounts) > 0 && skipMounts[path.Clean(strings.TrimSpace(m.Target))] {
			continue
		}
		add(m.args()...)
	}
	if p.AllowBindAttach {
		add("--allow-bind-attach")
	}
	if p.AllowVolumeAttach {
		add("--allow-volume-attach")
	}
	if !restore && p.ControlSocket != "" {
		add("-control-socket", p.ControlSocket)
	}
	if !restore && p.TokenFile != "" {
		add("--control-token-file", p.TokenFile)
	}
	if !restore && p.CheckpointDir != "" {
		add("-checkpoint-dir", p.CheckpointDir)
	}
	if !restore {
		extra, _ := rungv.SplitArgs(s.ExtraArgs)
		a = append(a, extra...)
	}
	return a
}

func (m Mount) args() []string {
	switch m.Type {
	case MountTmpfs:
		v := m.Target
		var opts []string
		if m.Size != "" {
			opts = append(opts, "size="+m.Size)
		}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		if len(opts) > 0 {
			v += ":" + strings.Join(opts, ",")
		}
		return []string{"--tmpfs", v}
	case MountVolume:
		if strings.ContainsAny(m.Source, `/\`) || m.Size != "" || m.Ephemeral {
			// An .ext4 image at a host path, or volume options (size=, ephemeral),
			// go through --mount type=volume,...
			v := "type=volume"
			if m.Source != "" {
				src := m.Source
				if strings.ContainsAny(src, `/\`) {
					src = rungv.ExpandHome(src)
				}
				v += ",src=" + src
			}
			v += ",dst=" + m.Target
			if m.Size != "" {
				v += ",size=" + m.Size
			}
			if m.Ephemeral {
				v += ",ephemeral"
			}
			if m.ReadOnly {
				v += ",readonly"
			}
			return []string{"--mount", v}
		}
		return []string{"-v", volumeSpec(m.Source, m.Target, m.ReadOnly)}
	default:
		src := rungv.ExpandHome(m.Source)
		if m.Symlinks != "" {
			return bindMountArgs(src, m.Target, m.ReadOnly, m.Symlinks)
		}
		return []string{"-v", volumeSpec(src, m.Target, m.ReadOnly)}
	}
}

func bindMountArgs(src, dst string, ro bool, symlinks string) []string {
	v := "type=bind,src=" + src + ",dst=" + dst
	if ro {
		v += ",readonly"
	}
	if symlinks != "" {
		v += ",symlinks=" + symlinks
	}
	return []string{"--mount", v}
}

func volumeSpec(src, dst string, ro bool) string {
	v := src + ":" + dst
	if ro {
		v += ":ro"
	}
	return v
}

// CommandLine is the run command as one line a person could paste into a
// shell.
func (s Spec) CommandLine(p Paths) string {
	return "rungvx " + rungv.FormatArgs(s.RunArgs(p))
}

// Prepare creates the host directories a run needs before rungv starts:
// the parent of each published Unix socket. rungv creates missing bind
// directories and volume images itself.
func (s Spec) Prepare() error {
	for _, port := range s.Ports {
		if !strings.HasPrefix(port, "unix:") {
			continue
		}
		rest := strings.TrimPrefix(port, "unix:")
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 {
			continue
		}
		dir := filepath.Dir(rungv.ExpandHome(rest[:i]))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Volumes lists the named volumes the spec mounts, its root volume
// included.
func (s Spec) Volumes() []string {
	var out []string
	if s.RootfsVolume != "" && !strings.ContainsAny(s.RootfsVolume, `/\`) {
		out = append(out, s.RootfsVolume)
	}
	for _, m := range s.Mounts {
		if m.Type == MountVolume && !m.Ephemeral && m.Source != "" && !strings.ContainsAny(m.Source, `/\`) {
			out = append(out, m.Source)
		}
	}
	return out
}

// UnixSockets lists the expanded host socket paths the spec publishes.
func (s Spec) UnixSockets() []string {
	var out []string
	for _, port := range s.Ports {
		if rest, ok := strings.CutPrefix(port, "unix:"); ok {
			if i := strings.LastIndexByte(rest, ':'); i > 0 {
				out = append(out, rungv.ExpandHome(rest[:i]))
			}
		}
	}
	return out
}

// Clone returns a deep copy of the spec.
func (s Spec) Clone() Spec {
	c := s
	c.Env = append([]string(nil), s.Env...)
	c.EnvFiles = append([]string(nil), s.EnvFiles...)
	c.Ports = append([]string(nil), s.Ports...)
	c.UnixForwards = append([]string(nil), s.UnixForwards...)
	c.DynamicPortAddrs = append([]string(nil), s.DynamicPortAddrs...)
	c.Mounts = append([]Mount(nil), s.Mounts...)
	c.Labels = maps.Clone(s.Labels)
	return c
}

// Equal compares two specs field by field; a nil and an empty list are
// the same.
func (s Spec) Equal(o Spec) bool {
	a, errA := json.Marshal(s)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// autoPublish is the rungv flag that publishes on 127.0.0.1, under the same
// number, every TCP port the guest comes to listen on or forward, and
// takes it back when the guest stops: what a sandbox whose ports are not
// known when it starts needs, such as a Docker Engine.
const autoPublish = "-auto-publish"

// isAutoPublish reports whether arg is the flag, however it is written, and
// what it is set to.
func isAutoPublish(arg string) (on, ok bool) {
	name, value, valued := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
	if name != autoPublish && name != autoPublish[1:] || !strings.HasPrefix(arg, "-") {
		return false, false
	}
	return !valued || value != "false" && value != "0", true
}

// AutoPublishSet reports whether the spec's extra flags say anything about
// auto-publishing, on or off.
func AutoPublishSet(s Spec) bool {
	extra, _ := rungv.SplitArgs(s.ExtraArgs)
	for _, a := range extra {
		if _, ok := isAutoPublish(a); ok {
			return true
		}
	}
	return false
}

// HasAutoPublish reports whether the spec runs with -auto-publish.
func HasAutoPublish(s Spec) bool {
	extra, _ := rungv.SplitArgs(s.ExtraArgs)
	on := false
	for _, a := range extra {
		if v, ok := isAutoPublish(a); ok {
			on = v
		}
	}
	return on
}

// WithAutoPublish returns the spec with -auto-publish among its extra
// flags, or without it.
func WithAutoPublish(s Spec, on bool) Spec {
	extra, err := rungv.SplitArgs(s.ExtraArgs)
	if err != nil {
		return s
	}
	out := make([]string, 0, len(extra)+1)
	for _, a := range extra {
		if _, ok := isAutoPublish(a); !ok {
			out = append(out, a)
		}
	}
	if on {
		out = append(out, autoPublish)
	}
	s = s.Clone()
	s.ExtraArgs = rungv.FormatArgs(out)
	return s
}
