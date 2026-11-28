package engine

// Owner: workstream B. Container create config merge, port resolution and
// mount resolution.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const defaultContainerPATH = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// mergeContainerConfig merges the client's create request with the image's
// config according to docker's precedence rules (§6.1). It returns the
// merged config, the final argv, any warnings, or a 400 error.
func mergeContainerConfig(id string, req *containerCreateRequest, img ImageInspect) (ContainerConfig, []string, []string, error) {
	var warnings []string

	// 1. Entrypoint / Cmd.
	var ep, cmd []string
	if len(req.Entrypoint) == 0 {
		cmd = slices.Clone([]string(req.Cmd))
		if len(cmd) == 0 {
			cmd = slices.Clone(img.Config.Cmd)
		}
		if req.Entrypoint == nil {
			ep = slices.Clone(img.Config.Entrypoint)
		}
	} else {
		ep = slices.Clone([]string(req.Entrypoint))
		cmd = slices.Clone([]string(req.Cmd))
	}
	if len(ep) == 1 && ep[0] == "" {
		ep = nil
	}
	argv := make([]string, 0, len(ep)+len(cmd))
	argv = append(argv, ep...)
	argv = append(argv, cmd...)
	if len(argv) == 0 {
		return ContainerConfig{}, nil, nil, errInvalid("No command specified")
	}

	// 2. Env: image first, request overrides same-key entries and appends new
	// ones; entries without '=' are dropped so host env never leaks.
	env := mergeEnv(img.Config.Env, req.Env)

	// 3. WorkingDir and User.
	workdir := req.WorkingDir
	if workdir == "" {
		workdir = img.Config.WorkingDir
	}
	if workdir == "" {
		workdir = "/"
	}
	user := req.User
	if user == "" {
		user = img.Config.User
	}

	// 4. Labels, ExposedPorts and Volumes.
	labels := make(map[string]string, len(img.Config.Labels)+len(req.Labels))
	maps.Copy(labels, img.Config.Labels)
	maps.Copy(labels, req.Labels)

	var exposed map[string]struct{}
	if len(img.Config.ExposedPorts) > 0 || len(req.ExposedPorts) > 0 {
		exposed = make(map[string]struct{}, len(img.Config.ExposedPorts)+len(req.ExposedPorts))
		for p := range img.Config.ExposedPorts {
			if norm, err := normalizeContainerPort(p); err == nil {
				exposed[norm] = struct{}{}
			} else {
				exposed[p] = struct{}{}
			}
		}
		for p := range req.ExposedPorts {
			norm, err := normalizeContainerPort(p)
			if err != nil {
				return ContainerConfig{}, nil, nil, err
			}
			exposed[norm] = struct{}{}
		}
	}

	var volumes map[string]struct{}
	if len(img.Config.Volumes) > 0 || len(req.Volumes) > 0 {
		volumes = make(map[string]struct{}, len(img.Config.Volumes)+len(req.Volumes))
		for v := range img.Config.Volumes {
			volumes[v] = struct{}{}
		}
		for v := range req.Volumes {
			volumes[v] = struct{}{}
		}
	}

	// 5. Hostname, StopSignal, Healthcheck.
	hostname := req.Hostname
	if hostname == "" {
		hostname = id[:12]
	}
	stopSignal := req.StopSignal
	if stopSignal == "" {
		stopSignal = img.Config.StopSignal
	}

	healthcheck, _, err := mergeHealthConfig(img.Config.Healthcheck, req.Healthcheck)
	if err != nil {
		return ContainerConfig{}, nil, nil, err
	}

	cfg := ContainerConfig{
		Hostname:     hostname,
		Domainname:   req.Domainname,
		User:         user,
		AttachStdin:  req.AttachStdin,
		AttachStdout: req.AttachStdout,
		AttachStderr: req.AttachStderr,
		ExposedPorts: exposed,
		Tty:          req.Tty,
		OpenStdin:    req.OpenStdin,
		StdinOnce:    req.StdinOnce,
		Env:          env,
		Cmd:          cmd,
		Healthcheck:  healthcheck,
		Image:        req.Image,
		Volumes:      volumes,
		WorkingDir:   workdir,
		Entrypoint:   ep,
		Labels:       labels,
		StopSignal:   stopSignal,
		StopTimeout:  req.StopTimeout,
	}
	return cfg, argv, warnings, nil
}

// mergeEnv overlays override onto base, dropping entries without '=' or with
// empty/whitespace keys, and ensuring PATH is set.
func mergeEnv(base, override []string) []string {
	var out []string
	index := map[string]int{}
	add := func(entries []string) {
		for _, entry := range entries {
			k, _, ok := strings.Cut(entry, "=")
			if !ok || strings.TrimSpace(k) == "" || strings.ContainsAny(k, " \t") {
				continue
			}
			if idx, exists := index[k]; exists {
				out[idx] = entry
			} else {
				index[k] = len(out)
				out = append(out, entry)
			}
		}
	}
	add(base)
	add(override)
	if _, hasPath := index["PATH"]; !hasPath {
		out = append(out, defaultContainerPATH)
	}
	return out
}

// validateRestartPolicy checks the restart policy and its compatibility with
// AutoRemove.
func validateRestartPolicy(hc HostConfig) error {
	rp := hc.RestartPolicy
	switch rp.Name {
	case "", "no", "always", "unless-stopped", "on-failure":
	default:
		return errInvalid("invalid restart policy: %q", rp.Name)
	}
	if rp.MaximumRetryCount < 0 {
		return errInvalid("maximum retry count cannot be negative")
	}
	if rp.MaximumRetryCount > 0 && rp.Name != "on-failure" {
		return errInvalid("maximum retry count can only be used with on-failure restart policy")
	}
	if hc.AutoRemove && rp.Name != "" && rp.Name != "no" {
		return errInvalid("can't create 'AutoRemove' container with restart policy")
	}
	return nil
}

// normalizeContainerPort turns "80" or "80/TCP" into "80/tcp" and checks the
// port number and protocol.
func normalizeContainerPort(raw string) (string, error) {
	portStr, proto, hasProto := strings.Cut(strings.TrimSpace(raw), "/")
	if !hasProto || proto == "" {
		proto = "tcp"
	} else {
		proto = strings.ToLower(proto)
	}
	if proto != "tcp" && proto != "udp" {
		return "", errInvalid("invalid port protocol %q in %q", proto, raw)
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n < 1 || n > 65535 {
		return "", errInvalid("invalid container port %q", raw)
	}
	return strconv.Itoa(n) + "/" + proto, nil
}

func parsePortRange(raw string) (lo, hi int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return 0, 0, nil
	}
	aStr, bStr, isRange := strings.Cut(raw, "-")
	lo, err = strconv.Atoi(aStr)
	if err != nil || lo < 1 || lo > 65535 {
		return 0, 0, errInvalid("invalid host port %q", raw)
	}
	if !isRange {
		return lo, lo, nil
	}
	hi, err = strconv.Atoi(bStr)
	if err != nil || hi < lo || hi > 65535 {
		return 0, 0, errInvalid("invalid host port range %q", raw)
	}
	if hi-lo+1 > 1024 {
		return 0, 0, errInvalid("host port range %q exceeds 1024 ports", raw)
	}
	return lo, hi, nil
}

// normalizeHostIP resolves the host bind IP for one PortBinding entry.
// When allowLAN is false, every IP is rewritten to 127.0.0.1 and non-wildcard,
// non-loopback IPs set warn=true. When allowLAN is true, empty defaults to
// 0.0.0.0, explicit loopback and LAN/wildcard IPv4/IPv6 literals are honored
// without warning, and invalid IP strings fail with a 400 error.
func normalizeHostIP(raw string, allowLAN bool) (hostIP string, warn bool, err error) {
	ip := strings.TrimSpace(raw)
	if !allowLAN {
		switch ip {
		case "", "0.0.0.0", "::", "127.0.0.1":
			return "127.0.0.1", false, nil
		default:
			return "127.0.0.1", true, nil
		}
	}
	if ip == "" {
		return "0.0.0.0", false, nil
	}
	hadBrackets := strings.HasPrefix(ip, "[") && strings.HasSuffix(ip, "]")
	unbracketed := ip
	if hadBrackets {
		unbracketed = ip[1 : len(ip)-1]
	}
	addr, perr := netip.ParseAddr(unbracketed)
	if perr != nil || addr.Zone() != "" || (hadBrackets && !addr.Is6()) {
		return "", false, errInvalid("invalid host IP %q", ip)
	}
	return addr.String(), false, nil
}

// validatePortBindings validates HostConfig.PortBindings, normalizes its keys,
// expands host port ranges, and collects warnings for non-loopback host IPs
// when allowLAN is false.
func validatePortBindings(hc *HostConfig, allowLAN bool) ([]string, error) {
	if hc.NetworkMode == "none" && (len(hc.PortBindings) > 0 || hc.PublishAllPorts) {
		return nil, errInvalid("published ports are incompatible with network mode \"none\"")
	}
	if len(hc.PortBindings) == 0 {
		return nil, nil
	}
	var warnings []string
	normalized := make(map[string][]PortBinding, len(hc.PortBindings))
	for _, rawKey := range slices.Sorted(maps.Keys(hc.PortBindings)) {
		key, err := normalizeContainerPort(rawKey)
		if err != nil {
			return nil, err
		}
		bindings := hc.PortBindings[rawKey]
		if len(bindings) == 0 {
			bindings = []PortBinding{{}}
		}
		for _, b := range bindings {
			hostIP, warn, err := normalizeHostIP(b.HostIP, allowLAN)
			if err != nil {
				return nil, err
			}
			if warn {
				warnings = append(warnings, fmt.Sprintf("Port %s is published on 127.0.0.1 only, not on %s", key, strings.TrimSpace(b.HostIP)))
			}
			lo, hi, err := parsePortRange(b.HostPort)
			if err != nil {
				return nil, err
			}
			if lo == 0 {
				normalized[key] = append(normalized[key], PortBinding{HostIP: hostIP, HostPort: ""})
				continue
			}
			for p := lo; p <= hi; p++ {
				normalized[key] = append(normalized[key], PortBinding{HostIP: hostIP, HostPort: strconv.Itoa(p)})
			}
		}
	}
	hc.PortBindings = normalized
	return warnings, nil
}

type hostPortBind struct {
	ip    netip.Addr
	port  int
	proto string
}

func (a hostPortBind) conflicts(b hostPortBind) bool {
	if a.port != b.port || a.proto != b.proto {
		return false
	}
	if a.ip == b.ip {
		return true
	}
	// IPv6 wildcard (::) listens dual-stack under net.Listen("tcp", "[::]:port")
	// on macOS/Linux, overlapping IPv4 addresses on the same port as well.
	if (a.ip.Is6() && a.ip.IsUnspecified()) || (b.ip.Is6() && b.ip.IsUnspecified()) {
		return true
	}
	if a.ip.Is4() == b.ip.Is4() && (a.ip.IsUnspecified() || b.ip.IsUnspecified()) {
		return true
	}
	return false
}

func hasPortConflict(used []hostPortBind, cand hostPortBind) bool {
	for _, u := range used {
		if u.conflicts(cand) {
			return true
		}
	}
	return false
}

func formatSpecPort(hostIP string, hostPort int, normContainerPort string) string {
	return net.JoinHostPort(hostIP, strconv.Itoa(hostPort)) + ":" + normContainerPort
}

func isCannotAssignAddr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "assign requested address") ||
		strings.Contains(msg, "requested address is not valid")
}

// allocateFreePort finds an available port on hostIP for proto ("tcp" or
// "udp") not conflicting with used.
func allocateFreePort(hostIP, proto string, used []hostPortBind) (int, error) {
	ipAddr, err := netip.ParseAddr(hostIP)
	if err != nil {
		return 0, fmt.Errorf("invalid host IP %q: %w", hostIP, err)
	}
	listenAddr := net.JoinHostPort(hostIP, "0")
	for range 32 {
		var port int
		if proto == "udp" {
			pc, err := net.ListenPacket("udp", listenAddr)
			if err != nil {
				return 0, err
			}
			port = pc.LocalAddr().(*net.UDPAddr).Port
			_ = pc.Close()
		} else {
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				return 0, err
			}
			port = ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
		}
		if !hasPortConflict(used, hostPortBind{ip: ipAddr, port: port, proto: proto}) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not allocate a free host port on %s", hostIP)
}

// checkPortAvailable verifies that hostIP:port/proto is not already bound.
func checkPortAvailable(hostIP string, port int, proto string) error {
	addr := net.JoinHostPort(hostIP, strconv.Itoa(port))
	if proto == "udp" {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			if isCannotAssignAddr(err) {
				return fmt.Errorf("Bind for %s failed: %v", addr, err)
			}
			return fmt.Errorf("Bind for %s failed: port is already allocated", addr)
		}
		_ = pc.Close()
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isCannotAssignAddr(err) {
			return fmt.Errorf("Bind for %s failed: %v", addr, err)
		}
		return fmt.Errorf("Bind for %s failed: port is already allocated", addr)
	}
	_ = ln.Close()
	return nil
}

// resolveStartPorts computes the concrete host ports and rungv -p specs for
// one start of c. usedByOthers lists host ports already held by other running
// containers in this engine.
func resolveStartPorts(c *Container, usedByOthers []hostPortBind, allowLAN bool) (map[string][]PortBinding, []string, error) {
	if c.HostConfig.NetworkMode == "none" {
		return nil, nil, nil
	}
	used := slices.Clone(usedByOthers)
	resolved := map[string][]PortBinding{}
	var specPorts []string

	defaultHostIP := "127.0.0.1"
	if allowLAN {
		defaultHostIP = "0.0.0.0"
	}

	for _, key := range slices.Sorted(maps.Keys(c.HostConfig.PortBindings)) {
		normKey, err := normalizeContainerPort(key)
		if err != nil {
			return nil, nil, err
		}
		_, proto, _ := strings.Cut(normKey, "/")
		bindings := c.HostConfig.PortBindings[key]
		if len(bindings) == 0 {
			bindings = []PortBinding{{}}
		}
		for _, b := range bindings {
			hostIP := "127.0.0.1"
			if allowLAN {
				hostIP, _, err = normalizeHostIP(b.HostIP, true)
				if err != nil {
					return nil, nil, err
				}
			}
			ipAddr := netip.MustParseAddr(hostIP)
			var port int
			if b.HostPort == "" || b.HostPort == "0" {
				port, err = allocateFreePort(hostIP, proto, used)
				if err != nil {
					return nil, nil, err
				}
			} else {
				lo, hi, err := parsePortRange(b.HostPort)
				if err != nil {
					return nil, nil, err
				}
				for p := lo; p <= hi; p++ {
					cand := hostPortBind{ip: ipAddr, port: p, proto: proto}
					if hasPortConflict(used, cand) {
						return nil, nil, fmt.Errorf("Bind for %s failed: port is already allocated", net.JoinHostPort(hostIP, strconv.Itoa(p)))
					}
					if err := checkPortAvailable(hostIP, p, proto); err != nil {
						return nil, nil, err
					}
					used = append(used, cand)
					resolved[normKey] = append(resolved[normKey], PortBinding{HostIP: hostIP, HostPort: strconv.Itoa(p)})
					specPorts = append(specPorts, formatSpecPort(hostIP, p, normKey))
				}
				continue
			}
			used = append(used, hostPortBind{ip: ipAddr, port: port, proto: proto})
			resolved[normKey] = append(resolved[normKey], PortBinding{HostIP: hostIP, HostPort: strconv.Itoa(port)})
			specPorts = append(specPorts, formatSpecPort(hostIP, port, normKey))
		}
	}

	if c.HostConfig.PublishAllPorts {
		defaultAddr := netip.MustParseAddr(defaultHostIP)
		for _, rawKey := range slices.Sorted(maps.Keys(c.Config.ExposedPorts)) {
			normKey, err := normalizeContainerPort(rawKey)
			if err != nil {
				continue
			}
			if len(resolved[normKey]) > 0 {
				continue
			}
			_, proto, _ := strings.Cut(normKey, "/")
			port, err := allocateFreePort(defaultHostIP, proto, used)
			if err != nil {
				return nil, nil, err
			}
			used = append(used, hostPortBind{ip: defaultAddr, port: port, proto: proto})
			resolved[normKey] = append(resolved[normKey], PortBinding{HostIP: defaultHostIP, HostPort: strconv.Itoa(port)})
			specPorts = append(specPorts, formatSpecPort(defaultHostIP, port, normKey))
		}
	}

	return resolved, specPorts, nil
}

// isAbsoluteHostPath reports whether p is an absolute Unix or Windows path.
func isAbsoluteHostPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	if len(p) >= 3 && isASCIIAlpha(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return false
}

func isASCIIAlpha(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// parseBindSpec parses a docker HostConfig.Binds entry "src:dst[:opts]",
// preserving Windows drive letters like C:\dir:/dst:ro.
func parseBindSpec(raw string) (src, dst, opts string, err error) {
	start := 0
	if len(raw) >= 3 && isASCIIAlpha(raw[0]) && raw[1] == ':' && (raw[2] == '\\' || raw[2] == '/') {
		start = 2
	}
	idx := strings.IndexByte(raw[start:], ':')
	if idx < 0 {
		return "", "", "", errInvalid("invalid bind specification %q", raw)
	}
	src = raw[:start+idx]
	rest := raw[start+idx+1:]
	dst, opts, _ = strings.Cut(rest, ":")
	if dst == "" {
		return "", "", "", errInvalid("invalid bind specification %q: empty destination", raw)
	}
	return src, dst, opts, nil
}

func validateMountTarget(target string) (string, error) {
	if !strings.HasPrefix(target, "/") {
		return "", errInvalid("invalid mount target %q: must be an absolute path", target)
	}
	clean := path.Clean(target)
	if clean == "/" {
		return "", errInvalid("invalid mount target %q: cannot mount on /", target)
	}
	for _, reserved := range []string{"/proc", "/sys", "/dev"} {
		if (clean == reserved || strings.HasPrefix(clean, reserved+"/")) && !strings.HasPrefix(clean, "/dev/shm") {
			return "", errInvalid("invalid mount target %q: reserved path", target)
		}
	}
	return clean, nil
}

type plannedMount struct {
	spec        sandbox.Mount
	unixForward string
	point       MountPoint
	createVol   bool
	anonymous   bool
}

const (
	dockerSocketGuestSource = "/var/run/docker.sock"
	sshAuthSocketSource     = "/run/host-services/ssh-auth.sock"
)

type mountPlanOptions struct {
	unixForward  bool
	dockerSocket string
	sshAuthSock  string
	bindSymlinks bool
	volumesFrom  []plannedMount
}

// bindSymlinkMode returns sandbox.SymlinksContained for existing host
// directory bind mounts when opts.bindSymlinks is enabled. Regular files,
// missing paths (which -v creates automatically), and paths containing commas
// stay on the default mode so rungvx never rejects them.
func bindSymlinkMode(src, dst string, opts mountPlanOptions) string {
	if !opts.bindSymlinks || strings.Contains(src, ",") || strings.Contains(dst, ",") {
		return ""
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() {
		return ""
	}
	return sandbox.SymlinksContained
}

// resolveVolumesFrom resolves HostConfig.VolumesFrom entries ("<container>[:ro|:rw]")
// into planned mounts inherited from the referenced containers. Anonymous
// volumes are inherited without transferring ownership so removing the
// inheriting container does not delete the source container's volumes.
func resolveVolumesFrom(specs []string, lookup func(string) (Container, error), opts mountPlanOptions) ([]plannedMount, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	var out []plannedMount
	for _, raw := range specs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ref, mode, hasMode := strings.Cut(raw, ":")
		if ref == "" {
			return nil, errInvalid("invalid volumes-from specification %q", raw)
		}
		forceRO := false
		if hasMode {
			switch mode {
			case "ro":
				forceRO = true
			case "rw":
			default:
				return nil, errInvalid("invalid volumes-from mode %q in %q", mode, raw)
			}
		}
		src, err := lookup(ref)
		if err != nil {
			return nil, err
		}
		specByTarget := make(map[string]sandbox.Mount, len(src.Spec.Mounts))
		for _, sm := range src.Spec.Mounts {
			specByTarget[sm.Target] = sm
		}
		for _, mp := range src.Mounts {
			dst, err := validateMountTarget(mp.Destination)
			if err != nil {
				continue
			}
			ro := !mp.RW || forceRO
			inheritedPoint := mp
			inheritedPoint.Destination = dst
			inheritedPoint.RW = !ro
			inheritedPoint.Anonymous = false
			if ro && inheritedPoint.Mode == "" {
				inheritedPoint.Mode = "ro"
			}
			switch mp.Type {
			case "bind":
				pm := plannedMount{point: inheritedPoint}
				if uf, ok := resolveUnixForwardBind(mp.Source, dst, opts); ok {
					pm.unixForward = uf
				} else {
					pm.spec = sandbox.Mount{
						Type:     sandbox.MountBind,
						Source:   mp.Source,
						Target:   dst,
						ReadOnly: ro,
						Symlinks: bindSymlinkMode(mp.Source, dst, opts),
					}
				}
				out = append(out, pm)
			case "volume":
				volName := mp.Name
				if volName == "" {
					volName = mp.Source
				}
				if volName == "" {
					continue
				}
				out = append(out, plannedMount{
					spec: sandbox.Mount{
						Type:     sandbox.MountVolume,
						Source:   volName,
						Target:   dst,
						ReadOnly: ro,
					},
					point:     inheritedPoint,
					createVol: true,
					anonymous: false,
				})
			case "tmpfs":
				size := ""
				if sm, ok := specByTarget[dst]; ok {
					size = sm.Size
				}
				out = append(out, plannedMount{
					spec: sandbox.Mount{
						Type:     sandbox.MountTmpfs,
						Target:   dst,
						ReadOnly: ro,
						Size:     size,
					},
					point: inheritedPoint,
				})
			}
		}
	}
	return out, nil
}

func isUnixSocket(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// effectiveDockerSocket returns the expanded host Docker API Unix socket path
// when src targets /var/run/docker.sock, UnixForward is enabled, and the
// configured DockerSocket currently exists as a Unix socket (not e.g. TCP mode).
func effectiveDockerSocket(src string, opts mountPlanOptions) string {
	if !opts.unixForward || !strings.HasPrefix(src, "/") || path.Clean(src) != dockerSocketGuestSource {
		return ""
	}
	ds := rungv.ExpandHome(strings.TrimSpace(opts.dockerSocket))
	if isUnixSocket(ds) {
		return ds
	}
	return ""
}

// effectiveSSHAuthSocket returns the expanded host SSH agent Unix socket path
// when src targets /run/host-services/ssh-auth.sock, UnixForward is enabled,
// and the configured SSHAuthSock currently exists as a Unix socket.
func effectiveSSHAuthSocket(src string, opts mountPlanOptions) string {
	if !opts.unixForward || !strings.HasPrefix(src, "/") || path.Clean(src) != sshAuthSocketSource {
		return ""
	}
	ss := rungv.ExpandHome(strings.TrimSpace(opts.sshAuthSock))
	if isUnixSocket(ss) {
		return ss
	}
	return ""
}

// resolveUnixForwardBind reports whether a host bind mount src -> dst should be
// routed through rungv --unix-forward instead of a hostfs MountBind.
func resolveUnixForwardBind(src, dst string, opts mountPlanOptions) (string, bool) {
	if !opts.unixForward {
		return "", false
	}
	if ds := effectiveDockerSocket(src, opts); ds != "" {
		return dst + "=" + ds, true
	}
	if ss := effectiveSSHAuthSocket(src, opts); ss != "" {
		return dst + "=" + ss, true
	}
	if isUnixSocket(src) {
		return dst + "=" + src, true
	}
	return "", false
}

// planMountsWithOptions parses and validates Binds, Mounts, Tmpfs and
// Config.Volumes without side effects, returning the planned mounts to apply.
func planMountsWithOptions(hc HostConfig, cfg ContainerConfig, opts mountPlanOptions) ([]plannedMount, error) {
	var planned []plannedMount
	targets := map[string]bool{}

	addTarget := func(dst string) error {
		if targets[dst] {
			return errInvalid("Duplicate mount point: %s", dst)
		}
		targets[dst] = true
		return nil
	}

	// 1. HostConfig.Binds.
	for _, raw := range hc.Binds {
		src, rawDst, bindOpts, err := parseBindSpec(raw)
		if err != nil {
			return nil, err
		}
		dst, err := validateMountTarget(rawDst)
		if err != nil {
			return nil, err
		}
		if err := addTarget(dst); err != nil {
			return nil, err
		}
		ro := false
		if bindOpts != "" {
			for opt := range strings.SplitSeq(bindOpts, ",") {
				switch opt {
				case "ro":
					ro = true
				case "rw":
					ro = false
				case "", "z", "Z", "cached", "delegated", "consistent", "nocopy",
					"rprivate", "private", "rshared", "shared", "rslave", "slave":
				}
			}
		}
		if isAbsoluteHostPath(src) {
			pm := plannedMount{
				point: MountPoint{
					Type:        "bind",
					Source:      src,
					Destination: dst,
					Mode:        bindOpts,
					RW:          !ro,
					Propagation: "rprivate",
				},
			}
			if uf, ok := resolveUnixForwardBind(src, dst, opts); ok {
				pm.unixForward = uf
			} else {
				pm.spec = sandbox.Mount{
					Type:     sandbox.MountBind,
					Source:   src,
					Target:   dst,
					ReadOnly: ro,
					Symlinks: bindSymlinkMode(src, dst, opts),
				}
			}
			planned = append(planned, pm)
		} else {
			anon := false
			volName := src
			if volName == "" {
				volName = randomHex64()
				anon = true
			} else if !volumeNameRe.MatchString(volName) {
				return nil, errInvalid("invalid volume name %q", volName)
			}
			planned = append(planned, plannedMount{
				spec: sandbox.Mount{
					Type:     sandbox.MountVolume,
					Source:   volName,
					Target:   dst,
					ReadOnly: ro,
				},
				point: MountPoint{
					Type:        "volume",
					Name:        volName,
					Source:      volName,
					Destination: dst,
					Driver:      "local",
					Mode:        bindOpts,
					RW:          !ro,
					Anonymous:   anon,
				},
				createVol: true,
				anonymous: anon,
			})
		}
	}

	// 2. HostConfig.Mounts.
	for _, m := range hc.Mounts {
		dst, err := validateMountTarget(m.Target)
		if err != nil {
			return nil, err
		}
		if err := addTarget(dst); err != nil {
			return nil, err
		}
		switch m.Type {
		case "bind":
			if m.Source == "" {
				return nil, errInvalid("invalid mount config for type \"bind\": field Source must not be empty")
			}
			statPath := m.Source
			if ds := effectiveDockerSocket(m.Source, opts); ds != "" {
				statPath = ds
			} else if ss := effectiveSSHAuthSocket(m.Source, opts); ss != "" {
				statPath = ss
			}
			if _, err := os.Stat(statPath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, errInvalid("invalid mount config for type \"bind\": bind source path does not exist: %s", m.Source)
				}
				return nil, errInvalid("invalid mount config for type \"bind\": %v", err)
			}
			pm := plannedMount{
				point: MountPoint{
					Type:        "bind",
					Source:      m.Source,
					Destination: dst,
					RW:          !m.ReadOnly,
					Propagation: "rprivate",
				},
			}
			if uf, ok := resolveUnixForwardBind(m.Source, dst, opts); ok {
				pm.unixForward = uf
			} else {
				pm.spec = sandbox.Mount{
					Type:     sandbox.MountBind,
					Source:   m.Source,
					Target:   dst,
					ReadOnly: m.ReadOnly,
					Symlinks: bindSymlinkMode(m.Source, dst, opts),
				}
			}
			planned = append(planned, pm)
		case "volume":
			anon := false
			volName := m.Source
			if volName == "" {
				volName = randomHex64()
				anon = true
			} else if !volumeNameRe.MatchString(volName) {
				return nil, errInvalid("invalid volume name %q", volName)
			}
			planned = append(planned, plannedMount{
				spec: sandbox.Mount{
					Type:     sandbox.MountVolume,
					Source:   volName,
					Target:   dst,
					ReadOnly: m.ReadOnly,
				},
				point: MountPoint{
					Type:        "volume",
					Name:        volName,
					Source:      volName,
					Destination: dst,
					Driver:      "local",
					RW:          !m.ReadOnly,
					Anonymous:   anon,
				},
				createVol: true,
				anonymous: anon,
			})
		case "tmpfs":
			size := ""
			if m.TmpfsOptions != nil && m.TmpfsOptions.SizeBytes > 0 {
				size = strconv.FormatInt(m.TmpfsOptions.SizeBytes, 10)
			}
			planned = append(planned, plannedMount{
				spec: sandbox.Mount{
					Type:     sandbox.MountTmpfs,
					Target:   dst,
					ReadOnly: m.ReadOnly,
					Size:     size,
				},
				point: MountPoint{
					Type:        "tmpfs",
					Destination: dst,
					RW:          !m.ReadOnly,
				},
			})
		default:
			return nil, errInvalid("invalid mount type %q", m.Type)
		}
	}

	// 3. HostConfig.Tmpfs.
	for _, rawDst := range slices.Sorted(maps.Keys(hc.Tmpfs)) {
		dst, err := validateMountTarget(rawDst)
		if err != nil {
			return nil, err
		}
		if err := addTarget(dst); err != nil {
			return nil, err
		}
		opts := hc.Tmpfs[rawDst]
		ro := false
		size := ""
		if opts != "" {
			for opt := range strings.SplitSeq(opts, ",") {
				opt = strings.TrimSpace(opt)
				if opt == "ro" {
					ro = true
				} else if opt == "rw" {
					ro = false
				} else if val, ok := strings.CutPrefix(opt, "size="); ok {
					size = val
				}
			}
		}
		planned = append(planned, plannedMount{
			spec: sandbox.Mount{
				Type:     sandbox.MountTmpfs,
				Target:   dst,
				ReadOnly: ro,
				Size:     size,
			},
			point: MountPoint{
				Type:        "tmpfs",
				Destination: dst,
				Mode:        opts,
				RW:          !ro,
			},
		})
	}

	// 4. Inherited mounts from HostConfig.VolumesFrom (explicit mounts above win).
	for _, pm := range opts.volumesFrom {
		dst := pm.point.Destination
		if targets[dst] {
			continue
		}
		targets[dst] = true
		planned = append(planned, pm)
	}

	// 5. Image and request Volumes not already covered by an explicit mount.
	for _, rawDst := range slices.Sorted(maps.Keys(cfg.Volumes)) {
		dst, err := validateMountTarget(rawDst)
		if err != nil {
			continue
		}
		if targets[dst] {
			continue
		}
		targets[dst] = true
		volName := randomHex64()
		planned = append(planned, plannedMount{
			spec: sandbox.Mount{
				Type:   sandbox.MountVolume,
				Source: volName,
				Target: dst,
			},
			point: MountPoint{
				Type:        "volume",
				Name:        volName,
				Source:      volName,
				Destination: dst,
				Driver:      "local",
				RW:          true,
				Anonymous:   true,
			},
			createVol: true,
			anonymous: true,
		})
	}

	return planned, nil
}

// materializeMounts ensures all volumes in planned exist via workstream C's
// ensureVolume seam and returns the sandbox mounts, reverse Unix socket
// forwards, inspect mount points and anonymous volume names. On error, any
// anonymous volumes already created by this call are removed.
func (e *Engine) materializeMounts(ctx context.Context, planned []plannedMount) ([]sandbox.Mount, []string, []MountPoint, []string, error) {
	specMounts := make([]sandbox.Mount, 0, len(planned))
	var unixForwards []string
	points := make([]MountPoint, 0, len(planned))
	var anonVols []string

	for _, pm := range planned {
		if pm.createVol {
			if err := e.ensureVolume(ctx, pm.spec.Source, nil, pm.anonymous); err != nil {
				for _, v := range anonVols {
					_ = e.removeVolume(ctx, v, true)
				}
				return nil, nil, nil, nil, err
			}
			if pm.anonymous {
				anonVols = append(anonVols, pm.spec.Source)
			}
		}
		if pm.unixForward != "" {
			unixForwards = append(unixForwards, pm.unixForward)
		} else {
			specMounts = append(specMounts, pm.spec)
		}
		points = append(points, pm.point)
	}
	return specMounts, unixForwards, points, anonVols, nil
}

// buildSandboxSpec creates the sandbox.Spec for a container (without ports,
// which are filled at each start). specImage is the resolved image reference
// rungv knows in its cache, falling back to cfg.Image when empty.
func buildSandboxSpec(id, name, specImage string, cfg ContainerConfig, hc HostConfig, argv []string, mounts []sandbox.Mount) (sandbox.Spec, error) {
	return buildSandboxSpecWithForwards(id, name, specImage, cfg, hc, argv, mounts, nil)
}

func buildSandboxSpecWithForwards(id, name, specImage string, cfg ContainerConfig, hc HostConfig, argv []string, mounts []sandbox.Mount, unixForwards []string) (sandbox.Spec, error) {
	if hc.Memory < 0 {
		return sandbox.Spec{}, errInvalid("memory limit cannot be negative")
	}
	if hc.Memory > 0 && hc.Memory < minContainerMemoryBytes {
		return sandbox.Spec{}, errInvalid("Minimum memory limit allowed is 6MB")
	}
	if hc.NanoCpus < 0 {
		return sandbox.Spec{}, errInvalid("CPU limit cannot be negative")
	}
	if hc.NanoCpus > 0 && (hc.CpuQuota != 0 || hc.CpuPeriod != 0) {
		return sandbox.Spec{}, errInvalid("Conflicting options: Nano CPUs cannot be combined with CPU quota or period")
	}
	if hc.CpuPeriod < 0 || (hc.CpuPeriod > 0 && (hc.CpuPeriod < 1000 || hc.CpuPeriod > 1000000)) {
		return sandbox.Spec{}, errInvalid("CPU cfs period cannot be less than 1ms (i.e. 1000) or larger than 1s (i.e. 1000000)")
	}
	if hc.CpuQuota < -1 || (hc.CpuQuota > 0 && hc.CpuQuota < 1000) {
		return sandbox.Spec{}, errInvalid("CPU cfs quota cannot be less than 1ms (i.e. 1000)")
	}
	if specImage == "" {
		specImage = cfg.Image
	}
	memory := ""
	if hc.Memory > 0 {
		memory = strconv.FormatInt(hc.Memory, 10) + "b"
	}
	cpus := ""
	if hc.NanoCpus > 0 {
		cpus = strconv.FormatFloat(float64(hc.NanoCpus)/1e9, 'f', -1, 64)
	} else if hc.CpuQuota > 0 {
		period := hc.CpuPeriod
		if period <= 0 {
			period = 100000
		}
		cpus = strconv.FormatFloat(float64(hc.CpuQuota)/float64(period), 'f', -1, 64)
	}
	netMode := "nat"
	if hc.NetworkMode == "none" {
		netMode = "none"
	}
	extraArgs := ""
	if hc.ReadonlyRootfs {
		extraArgs = "-read-only"
	}
	spec := sandbox.Spec{
		ID:            id,
		Name:          name,
		Image:         specImage,
		SetEntrypoint: true,
		Entrypoint:    argv[0],
		Command:       rungv.FormatArgs(argv[1:]),
		Env:           slices.Clone(cfg.Env),
		UnixForwards:  slices.Clone(unixForwards),
		Mounts:        mounts,
		Memory:        memory,
		CPUs:          cpus,
		Network:       netMode,
		Hostname:      cfg.Hostname,
		User:          cfg.User,
		Workdir:       cfg.WorkingDir,
		Unprivileged:  !hc.Privileged,
		Pull:          "never",
		ExtraArgs:     extraArgs,
		Interactive:   cfg.OpenStdin,
		TTY:           cfg.Tty,
		ContainerID:   id,
	}
	if err := spec.Validate(); err != nil {
		return sandbox.Spec{}, errInvalid("%v", err)
	}
	return spec, nil
}
