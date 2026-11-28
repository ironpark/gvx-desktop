package engine

// Containers on user networks reach each other through the overlay
// (internal/overlay): a WireGuard link in each sandbox, made by netinit,
// to a hub in the app, and a DNS server per container. See
// docs/design/sandbox-engine.md, "Networks between containers".

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/overlay"
	"github.com/ironpark/gvx-desktop/internal/overlay/guest"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

type refLock struct {
	mu   sync.Mutex
	refs int
}

type overlayState struct {
	mu      sync.Mutex
	fab     *overlay.Fabric
	netinit map[string]bool     // containers whose active run started with netinit
	applyMu map[string]*refLock // per-container serialization for live apply (ref-counted)

	binOnce sync.Once
	binPath string
	binErr  error
}

// netinit is guest.Netinit; the tests replace it.
var netinit = guest.Netinit

// overlayWarning is the create warning for a container on n when the
// overlay cannot join it: this build has no netinit.
func overlayWarning(n Network) string {
	if n.Builtin || n.Subnet == "" {
		return ""
	}
	if _, err := netinit(); err != nil {
		return fmt.Sprintf("Containers on the network %q cannot reach each other: %v", n.Name, err)
	}
	return ""
}

// fabric returns the overlay, making it on first use.
func (e *Engine) fabric() *overlay.Fabric {
	e.ovl.mu.Lock()
	defer e.ovl.mu.Unlock()
	if e.ovl.fab == nil {
		e.ovl.fab = overlay.New(e.logf)
	}
	return e.ovl.fab
}

// closeOverlay stops the hub and the DNS servers, after the containers
// stopped.
func (e *Engine) closeOverlay() {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.fab = nil
	clear(e.ovl.netinit)
	clear(e.ovl.applyMu)
	e.ovl.mu.Unlock()
	if fab != nil {
		fab.Close()
	}
}

func (e *Engine) lockOverlayApply(cid string) func() {
	e.ovl.mu.Lock()
	if e.ovl.applyMu == nil {
		e.ovl.applyMu = map[string]*refLock{}
	}
	rl := e.ovl.applyMu[cid]
	if rl == nil {
		rl = &refLock{}
		e.ovl.applyMu[cid] = rl
	}
	rl.refs++
	e.ovl.mu.Unlock()

	rl.mu.Lock()
	return func() {
		rl.mu.Unlock()
		e.ovl.mu.Lock()
		rl.refs--
		if rl.refs == 0 && e.ovl.applyMu[cid] == rl {
			delete(e.ovl.applyMu, cid)
		}
		e.ovl.mu.Unlock()
	}
}

func (e *Engine) markStartedWithNetinit(id string) {
	e.ovl.mu.Lock()
	if e.ovl.netinit == nil {
		e.ovl.netinit = map[string]bool{}
	}
	e.ovl.netinit[id] = true
	e.ovl.mu.Unlock()
}

func (e *Engine) startedWithNetinit(id string) bool {
	e.ovl.mu.Lock()
	defer e.ovl.mu.Unlock()
	return e.ovl.netinit[id]
}

// netinitPath writes netinit and its hostfs .gvx_meta into the engine's
// directory, once, for the sandboxes to bind-mount read-only.
func (e *Engine) netinitPath() (string, error) {
	e.ovl.binOnce.Do(func() {
		bin, err := netinit()
		if err != nil {
			e.ovl.binErr = err
			return
		}
		e.ovl.binPath, e.ovl.binErr = guest.Install(e.path(), bin)
	})
	return e.ovl.binPath, e.ovl.binErr
}

// extraHostLines are the /etc/hosts lines of docker's extra hosts.
func extraHostLines(hosts []string) ([]string, error) {
	var out []string
	for _, h := range hosts {
		l, err := guestcfg.ParseExtraHost(h, overlay.HostGateway)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// overlayAddrs are c's addresses on user networks, as its link will have
// them, and whether those networks are all internal, with no other network
// (the builtin bridge) besides.
func (e *Engine) overlayAddrs(c *Container) (addrs []netip.Prefix, isolated bool) {
	if c.HostConfig.NetworkMode == "none" {
		return nil, false
	}
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	var out []netip.Prefix
	isolated = true
	for _, n := range e.net.byID {
		if _, ok := n.Containers[c.ID]; !ok {
			continue
		}
		if n.Builtin || !n.Internal {
			isolated = false
		}
		if n.Builtin {
			continue
		}
		sub, err1 := netip.ParsePrefix(n.Subnet)
		a, err2 := netip.ParseAddr(n.IPs[c.ID])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, netip.PrefixFrom(a, sub.Bits()))
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return out, isolated && len(out) > 0
}

// joinOverlay joins c to the overlay for the start whose spec is spec, and
// returns the spec that runs netinit in front of the container's command.
// A container on no user network and with no extra hosts keeps spec. When
// netinit cannot run or the overlay cannot join it, the container starts
// without them and the reason goes to the log.
func (e *Engine) joinOverlay(c *Container, spec sandbox.Spec) sandbox.Spec {
	addrs, isolated := e.overlayAddrs(c)
	hosts, err := extraHostLines(c.HostConfig.ExtraHosts)
	if err != nil {
		e.logf("engine: container %s: %v", shortHex(c.ID), err)
		hosts = nil
	}
	if len(addrs) == 0 && len(hosts) == 0 {
		return spec
	}
	bin, err := e.netinitPath()
	if err != nil {
		e.logf("engine: container %s starts without its networks and extra hosts: %v", shortHex(c.ID), err)
		return spec
	}
	var (
		cfg    guestcfg.Config
		dns    netip.AddrPort
		joined bool
	)
	if len(addrs) > 0 {
		cfg, dns, err = e.fabric().Join(c.ID, addrs, overlay.JoinOptions{Isolated: isolated, MTU: e.linkMTU()})
		if err != nil {
			e.logf("engine: container %s starts without its networks: %v", shortHex(c.ID), err)
			cfg, dns = guestcfg.Config{}, netip.AddrPort{}
		} else {
			joined = true
			cfg.Isolated = isolated
			if !mountsResolvConf(spec.Mounts) {
				cfg.DNS = overlay.DNSRelay
			}
		}
	}
	cfg.Hosts = hosts
	if !cfg.HasLink() && len(cfg.Hosts) == 0 {
		return spec
	}
	out, err := overlay.WithNetinit(spec, cfg, bin, dns)
	if err != nil {
		if joined {
			e.leaveOverlay(c.ID)
		}
		e.logf("engine: container %s starts without its networks and extra hosts: %v", shortHex(c.ID), err)
		return spec
	}
	e.markStartedWithNetinit(c.ID)
	if joined {
		e.syncOverlay()
	}
	return out
}

// mountsResolvConf reports whether one of mounts puts something at
// /etc/resolv.conf: a file there, or a directory over /etc. The container
// then resolves with what it mounted, and netinit, writing the file, would
// write to the host's.
func mountsResolvConf(mounts []sandbox.Mount) bool {
	return slices.ContainsFunc(mounts, func(m sandbox.Mount) bool {
		t := path.Clean(m.Target)
		return t == "/etc/resolv.conf" || t == "/etc"
	})
}

// linkMTU is overlay.LinkMTU when rungv can raise eth0's MTU for it
// (-tune), and overlay.BaseMTU when it cannot.
func (e *Engine) linkMTU() int {
	if r, ok := e.runner.(interface{ HasRunFlag(string) bool }); ok && !r.HasRunFlag("tune") {
		return overlay.BaseMTU
	}
	return overlay.LinkMTU
}

// leaveOverlay takes a container off the overlay when its sandbox stopped.
func (e *Engine) leaveOverlay(id string) {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	delete(e.ovl.netinit, id)
	e.ovl.mu.Unlock()
	if fab != nil {
		fab.Leave(id)
	}
}

// applyOverlayLive updates the WireGuard link of a running container that
// joined the overlay at start, via netinit's --gvx-apply mode (§5.6).
// Calls for the same container are serialized, read committed addresses inside
// the critical section, use a background-derived timeout so client disconnects
// cannot cancel an in-flight apply, and roll back only if neither the peer
// generation nor the committed addresses have moved on.
func (e *Engine) applyOverlayLive(cid string) {
	unlock := e.lockOverlayApply(cid)
	defer unlock()

	c, err := e.lookup(cid)
	if err != nil || c.State.Status != StatusRunning {
		return
	}
	if e.isContainerPaused(c) {
		return
	}
	if c.isMicroVM() {
		e.applyMicroVMOverlayLive(cid, &c)
		return
	}
	ctl := e.runner.Control(cid)
	if ctl == nil {
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
	addrs, _ := e.overlayAddrs(&c)
	cfg, gen, ok, err := fab.SetAddrs(cid, addrs)
	if err != nil {
		e.logf("engine: overlay set addrs %s: %v", shortHex(cid), err)
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
	enc, err := cfg.Encode()
	if err != nil {
		revertIfUnchanged()
		e.logf("engine: overlay encode %s: %v", shortHex(cid), err)
		return
	}

	execCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	priv := true
	proc, err := ctl.Exec(execCtx, sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:       []string{guestcfg.Path, guestcfg.ApplyArg},
			Env:        []string{guestcfg.EnvVar + "=" + enc},
			User:       "0",
			Privileged: &priv,
		},
	})
	if err != nil {
		revertIfUnchanged()
		e.logf("engine: live network apply %s: %v", shortHex(cid), err)
		return
	}
	var stderr bytes.Buffer
	var wg sync.WaitGroup
	if r := proc.Stdout(); r != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(io.Discard, r)
		}()
	}
	if r := proc.Stderr(); r != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(&stderr, io.LimitReader(r, 8*1024))
		}()
	}
	code, waitErr := proc.Wait()
	wg.Wait()
	_ = proc.Close()
	if waitErr != nil || code != 0 {
		revertIfUnchanged()
		e.logf("engine: live network apply %s failed (code=%d err=%v stderr=%q)", shortHex(cid), code, waitErr, strings.TrimSpace(stderr.String()))
	}
}

// syncOverlay tells the overlay which containers share which networks, and
// their names, after a change.
func (e *Engine) syncOverlay() {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.mu.Unlock()
	if fab == nil {
		return
	}
	names := map[string]string{}
	e.ctr.mu.Lock()
	for id, c := range e.ctr.byID {
		names[id] = c.Name
	}
	e.ctr.mu.Unlock()

	members := map[string]*overlay.Member{}
	e.net.mu.Lock()
	for _, n := range e.net.byID {
		if n.Builtin {
			continue
		}
		for cid, aliases := range n.Containers {
			a, err := netip.ParseAddr(n.IPs[cid])
			if err != nil {
				continue
			}
			name, ok := names[cid]
			if !ok {
				continue
			}
			m := members[cid]
			if m == nil {
				m = &overlay.Member{ID: cid, Endpoints: map[string]overlay.Endpoint{}}
				members[cid] = m
			}
			m.Endpoints[n.ID] = overlay.Endpoint{Addr: a, Names: buildDNSNames(name, shortHex(cid), aliases)}
		}
	}
	e.net.mu.Unlock()

	list := make([]overlay.Member, 0, len(members))
	for _, id := range slices.Sorted(maps.Keys(members)) {
		list = append(list, *members[id])
	}
	fab.SetMembers(list)
}

// reconcileEndpoints makes the containers' endpoints on user networks show
// the addresses their networks gave them (containers made before the overlay
// recorded the NAT's address) and restores builtin network memberships (which
// networks.json does not persist) so disconnect and overlay isolation reflect
// them after an engine restart.
func (e *Engine) reconcileEndpoints() {
	type addr struct {
		ip, gateway string
		bits        int
	}
	byNet := map[string]map[string]addr{} // network name → container → addr
	builtinNames := map[string]string{}   // builtin network name → network ID
	e.net.mu.Lock()
	for _, n := range e.net.byID {
		if n.Builtin {
			builtinNames[n.Name] = n.ID
			continue
		}
		sub, err := netip.ParsePrefix(n.Subnet)
		if err != nil {
			continue
		}
		m := map[string]addr{}
		for cid, ip := range n.IPs {
			m[cid] = addr{ip, n.Gateway, sub.Bits()}
		}
		byNet[n.Name] = m
	}
	e.net.mu.Unlock()

	builtinMembers := map[string]map[string][]string{} // builtin network ID → container ID → aliases

	e.ctr.mu.Lock()
	dirty := false
	for _, c := range e.ctr.byID {
		for name, ep := range c.Networks {
			if bid, ok := builtinNames[name]; ok {
				if builtinMembers[bid] == nil {
					builtinMembers[bid] = map[string][]string{}
				}
				builtinMembers[bid][c.ID] = slices.Clone(ep.Aliases)
				continue
			}
			a, ok := byNet[name][c.ID]
			if !ok || (ep.IPAddress == a.ip && ep.Gateway == a.gateway && ep.IPPrefixLen == a.bits) {
				continue
			}
			ep.IPAddress, ep.Gateway, ep.IPPrefixLen = a.ip, a.gateway, a.bits
			ep.MacAddress = containerMAC(c.ID)
			c.Networks[name] = ep
			dirty = true
		}
	}
	if dirty {
		if err := e.saveContainersLocked(); err != nil {
			e.logf("engine: save containers.json: %v", err)
		}
	}
	e.ctr.mu.Unlock()

	if len(builtinMembers) > 0 {
		e.net.mu.Lock()
		for bid, members := range builtinMembers {
			n := e.net.byID[bid]
			if n == nil || !n.Builtin {
				continue
			}
			if n.Containers == nil {
				n.Containers = map[string][]string{}
			}
			for cid, aliases := range members {
				if _, exists := n.Containers[cid]; !exists {
					n.Containers[cid] = aliases
				}
			}
		}
		e.net.mu.Unlock()
	}
}
