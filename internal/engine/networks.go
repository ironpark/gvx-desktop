package engine

// Owner: workstream C.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/overlay"
)

// networkState is the engine's networks, as networks.json keeps them.
// Every sandbox has a NAT of its own; containers on a user network reach
// each other through the overlay (overlay.go), at the addresses their
// network gives them. The builtin bridge has no overlay, as containers
// there have no names on Docker either.
type networkState struct {
	mu   sync.Mutex
	byID map[string]*Network
}

// Network is a network: bridge, host and none always exist; the others
// are the ones clients made.
type Network struct {
	ID         string            `json:"id"` // 64 hex digits
	Name       string            `json:"name"`
	Driver     string            `json:"driver"` // bridge, host, null
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	// Containers are the IDs of the containers on it, with their aliases.
	Containers map[string][]string `json:"containers,omitempty"`
	// Subnet and Gateway are a user network's IPv4 subnet and its first
	// address, and IPs the containers' addresses on it, by container ID.
	Subnet  string            `json:"subnet,omitempty"`
	Gateway string            `json:"gateway,omitempty"`
	IPs     map[string]string `json:"ips,omitempty"`
	Builtin bool              `json:"-"`
}

type networksFile struct {
	Version  int        `json:"version"`
	Networks []*Network `json:"networks"`
}

type dockerNetwork struct {
	Name       string                     `json:"Name"`
	ID         string                     `json:"Id"`
	Created    time.Time                  `json:"Created"`
	Scope      string                     `json:"Scope"`
	Driver     string                     `json:"Driver"`
	EnableIPv6 bool                       `json:"EnableIPv6"`
	IPAM       networkIPAM                `json:"IPAM"`
	Internal   bool                       `json:"Internal"`
	Attachable bool                       `json:"Attachable"`
	Ingress    bool                       `json:"Ingress"`
	ConfigFrom struct{ Network string }   `json:"ConfigFrom"`
	ConfigOnly bool                       `json:"ConfigOnly"`
	Containers map[string]networkEndpoint `json:"Containers"`
	Options    map[string]string          `json:"Options"`
	Labels     map[string]string          `json:"Labels"`
}

type networkIPAM struct {
	Driver  string              `json:"Driver"`
	Options map[string]string   `json:"Options"`
	Config  []networkIPAMConfig `json:"Config"`
}

type networkIPAMConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

type networkEndpoint struct {
	Name        string `json:"Name"`
	EndpointID  string `json:"EndpointID"`
	MacAddress  string `json:"MacAddress"`
	IPv4Address string `json:"IPv4Address"`
	IPv6Address string `json:"IPv6Address"`
}

var (
	builtinBridgeID = deterministicHex64("gvx-desktop:network:bridge")
	builtinHostID   = deterministicHex64("gvx-desktop:network:host")
	builtinNoneID   = deterministicHex64("gvx-desktop:network:none")
)

func deterministicHex64(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func builtinNetworks() []*Network {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []*Network{
		{
			ID:         builtinBridgeID,
			Name:       "bridge",
			Driver:     "bridge",
			Created:    created,
			Options:    map[string]string{"com.docker.network.bridge.default_bridge": "true"},
			Containers: map[string][]string{},
			Builtin:    true,
		},
		{
			ID:         builtinHostID,
			Name:       "host",
			Driver:     "host",
			Created:    created,
			Containers: map[string][]string{},
			Builtin:    true,
		},
		{
			ID:         builtinNoneID,
			Name:       "none",
			Driver:     "null",
			Created:    created,
			Containers: map[string][]string{},
			Builtin:    true,
		},
	}
}

func cloneNetwork(n *Network) Network {
	cp := *n
	cp.Labels = maps.Clone(n.Labels)
	cp.Options = maps.Clone(n.Options)
	cp.IPs = maps.Clone(n.IPs)
	if n.Containers != nil {
		cp.Containers = make(map[string][]string, len(n.Containers))
		for k, v := range n.Containers {
			cp.Containers[k] = slices.Clone(v)
		}
	}
	return cp
}

// ---- seams: called by engine.go and workstream B; frozen ----

// loadNetworks reads networks.json.
func (e *Engine) loadNetworks() error {
	var f networksFile
	if err := loadFile(e.path("networks.json"), &f); err != nil {
		return err
	}
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	e.net.byID = map[string]*Network{}
	for _, b := range builtinNetworks() {
		e.net.byID[b.ID] = b
	}
	for _, n := range f.Networks {
		if n == nil || n.ID == "" || n.Name == "" {
			continue
		}
		if n.Name == "bridge" || n.Name == "host" || n.Name == "none" {
			continue
		}
		if n.Containers == nil {
			n.Containers = map[string][]string{}
		}
		n.Builtin = false
		e.net.byID[n.ID] = n
	}
	// Networks from before the overlay have no subnet, and their
	// containers no addresses.
	dirty := false
	for _, n := range slices.SortedFunc(maps.Values(e.net.byID), func(a, b *Network) int { return strings.Compare(a.Name, b.Name) }) {
		if n.Builtin || !hasSubnet(n.Driver) {
			continue
		}
		if n.Subnet == "" {
			sub, err := e.allocSubnetLocked("", "")
			if err != nil {
				continue
			}
			n.Subnet, n.Gateway = sub.subnet.String(), sub.gateway.String()
			dirty = true
		}
		for _, cid := range slices.Sorted(maps.Keys(n.Containers)) {
			if _, ok := n.IPs[cid]; !ok {
				if _, err := allocAddr(n, cid, ""); err == nil {
					dirty = true
				}
			}
		}
	}
	if dirty {
		return e.saveNetworksLocked()
	}
	return nil
}

// hasSubnet reports whether a user network of the driver gets a subnet
// and addresses: bridge does, as on Docker; so do the drivers Docker has
// and the engine treats as bridge.
func hasSubnet(driver string) bool {
	switch driver {
	case "host", "null", "none":
		return false
	}
	return true
}

type subnetAlloc struct {
	subnet  netip.Prefix
	gateway netip.Addr
}

// allocSubnetLocked returns a subnet for a new network: want, when the
// client asked for one, or the first free /24 of overlay.Pool. Caller holds
// e.net.mu.
func (e *Engine) allocSubnetLocked(want, wantGateway string) (subnetAlloc, error) {
	var taken []netip.Prefix
	for _, n := range e.net.byID {
		if p, err := netip.ParsePrefix(n.Subnet); err == nil {
			taken = append(taken, p)
		}
	}
	overlaps := func(p netip.Prefix) bool {
		return slices.ContainsFunc(taken, func(t netip.Prefix) bool { return t.Overlaps(p) })
	}
	if want != "" {
		p, err := netip.ParsePrefix(want)
		if err != nil || !p.Addr().Is4() || p.Bits() > 30 || p.Bits() < 8 {
			return subnetAlloc{}, errInvalid("invalid subnet %q: an IPv4 subnet from /8 to /30 is supported", want)
		}
		p = p.Masked()
		if overlaps(p) {
			return subnetAlloc{}, &apiError{http.StatusForbidden, "Pool overlaps with other one on this address space"}
		}
		gw := p.Addr().Next()
		if wantGateway != "" {
			g, err := netip.ParseAddr(wantGateway)
			if err != nil || !p.Contains(g) {
				return subnetAlloc{}, errInvalid("invalid gateway %q for subnet %s", wantGateway, p)
			}
			gw = g
		}
		return subnetAlloc{p, gw}, nil
	}
	base := overlay.Pool.Addr().As4()
	for i := 1; i < 256; i++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{base[0], base[1], byte(i), 0}), 24)
		if !overlaps(p) {
			return subnetAlloc{p, p.Addr().Next()}, nil
		}
	}
	return subnetAlloc{}, &apiError{http.StatusForbidden, "could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network"}
}

// allocAddr gives cid an address on n: want, when the client asked for
// one, or the first free one after the gateway. Caller holds e.net.mu.
func allocAddr(n *Network, cid, want string) (netip.Addr, error) {
	sub, err := netip.ParsePrefix(n.Subnet)
	if err != nil {
		return netip.Addr{}, errInvalid("network %s has no subnet", n.Name)
	}
	gw, _ := netip.ParseAddr(n.Gateway)
	used := map[netip.Addr]string{}
	for id, s := range n.IPs {
		if a, err := netip.ParseAddr(s); err == nil {
			used[a] = id
		}
	}
	if n.IPs == nil {
		n.IPs = map[string]string{}
	}
	if cur, ok := n.IPs[cid]; ok && want == "" {
		if a, err := netip.ParseAddr(cur); err == nil {
			return a, nil
		}
	}
	last := lastAddr(sub)
	if want != "" {
		a, err := netip.ParseAddr(want)
		if err != nil || !sub.Contains(a) || a == sub.Addr() || a == last || a == gw {
			return netip.Addr{}, errInvalid("invalid IPv4 address %q for network %s (subnet %s)", want, n.Name, sub)
		}
		if owner, ok := used[a]; ok && owner != cid {
			return netip.Addr{}, errConflict("Address already in use")
		}
		n.IPs[cid] = a.String()
		return a, nil
	}
	for a := sub.Addr().Next(); a.IsValid() && sub.Contains(a) && a != last; a = a.Next() {
		if a == gw {
			continue
		}
		if _, ok := used[a]; ok {
			continue
		}
		n.IPs[cid] = a.String()
		return a, nil
	}
	return netip.Addr{}, &apiError{http.StatusForbidden, fmt.Sprintf("no available IPv4 addresses on network %s", n.Name)}
}

// lastAddr is the broadcast address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= (1 << (32 - p.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// saveNetworksLocked writes user networks to networks.json. Caller holds e.net.mu.
func (e *Engine) saveNetworksLocked() error {
	var list []*Network
	for _, n := range e.net.byID {
		if n.Builtin {
			continue
		}
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return saveFile(e.path("networks.json"), networksFile{
		Version:  storeVersion,
		Networks: list,
	})
}

func (e *Engine) findNetworkLocked(ref string) (*Network, error) {
	if e.net.byID == nil {
		e.net.byID = map[string]*Network{}
		for _, b := range builtinNetworks() {
			e.net.byID[b.ID] = b
		}
	}
	if n, ok := e.net.byID[ref]; ok {
		return n, nil
	}
	for _, n := range e.net.byID {
		if n.Name == ref {
			return n, nil
		}
	}
	var match *Network
	for _, n := range e.net.byID {
		if strings.HasPrefix(n.ID, ref) {
			if match != nil {
				return nil, errNotFound("network %s is ambiguous", ref)
			}
			match = n
		}
	}
	if match != nil {
		return match, nil
	}
	return nil, errNotFound("network %s not found", ref)
}

// network finds a network by name, full ID or unique ID prefix, builtins
// included. Not found is a 404 "network ref not found".
func (e *Engine) network(ref string) (Network, error) {
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	n, err := e.findNetworkLocked(ref)
	if err != nil {
		return Network{}, err
	}
	return cloneNetwork(n), nil
}

// connectNetwork records a container on a network with its aliases, and
// gives it an address on a user network: ipv4 when the client asked for
// one. It returns the container's endpoint, without EndpointID, MacAddress
// and DNSNames, which the caller fills.
func (e *Engine) connectNetwork(ref, containerID string, aliases []string, ipv4 string) (EndpointSettings, error) {
	e.net.mu.Lock()
	n, err := e.findNetworkLocked(ref)
	if err != nil {
		e.net.mu.Unlock()
		return EndpointSettings{}, err
	}
	if n.Containers == nil {
		n.Containers = map[string][]string{}
	}
	ep := EndpointSettings{Aliases: slices.Clone(aliases), NetworkID: n.ID}
	switch {
	case n.Builtin && n.Name == "bridge":
		ep.Gateway, ep.IPAddress, ep.IPPrefixLen = "10.0.2.2", "10.0.2.15", 24
	case n.Builtin:
	case n.Subnet != "":
		a, err := allocAddr(n, containerID, ipv4)
		if err != nil {
			e.net.mu.Unlock()
			return EndpointSettings{}, err
		}
		sub, _ := netip.ParsePrefix(n.Subnet)
		ep.Gateway, ep.IPAddress, ep.IPPrefixLen = n.Gateway, a.String(), sub.Bits()
	}
	n.Containers[containerID] = slices.Clone(aliases)
	netID, netName, netDriver := n.ID, n.Name, n.Driver
	if !n.Builtin {
		err = e.saveNetworksLocked()
	}
	e.net.mu.Unlock()
	if err != nil {
		return EndpointSettings{}, err
	}

	e.emit(Event{
		Type:   "network",
		Action: "connect",
		ID:     netID,
		Attributes: map[string]string{
			"container": containerID,
			"name":      netName,
			"type":      netDriver,
		},
	})
	e.notify()
	return ep, nil
}

// disconnectNetworks takes a removed container off every network.
func (e *Engine) disconnectNetworks(containerID string) {
	type disconnected struct {
		id, name, driver string
	}
	var removed []disconnected
	dirty := false

	e.net.mu.Lock()
	for _, n := range e.net.byID {
		if _, ok := n.Containers[containerID]; ok {
			delete(n.Containers, containerID)
			delete(n.IPs, containerID)
			removed = append(removed, disconnected{n.ID, n.Name, n.Driver})
			if !n.Builtin {
				dirty = true
			}
		}
	}
	if dirty {
		_ = e.saveNetworksLocked()
	}
	e.net.mu.Unlock()

	for _, d := range removed {
		e.emit(Event{
			Type:   "network",
			Action: "disconnect",
			ID:     d.id,
			Attributes: map[string]string{
				"container": containerID,
				"name":      d.name,
				"type":      d.driver,
			},
		})
	}
	if len(removed) > 0 {
		e.notify()
	}
}

func (e *Engine) toDockerNetwork(n Network, ctrs map[string]Container) dockerNetwork {
	labels := maps.Clone(n.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	opts := maps.Clone(n.Options)
	if opts == nil {
		opts = map[string]string{}
	}
	ipam := networkIPAM{
		Driver:  "default",
		Options: map[string]string{},
		Config:  []networkIPAMConfig{},
	}
	switch {
	case n.Builtin && n.Name == "bridge":
		ipam.Config = []networkIPAMConfig{{Subnet: "10.0.2.0/24", Gateway: "10.0.2.2"}}
	case n.Subnet != "":
		ipam.Config = []networkIPAMConfig{{Subnet: n.Subnet, Gateway: n.Gateway}}
	}
	bits := 24
	if p, err := netip.ParsePrefix(n.Subnet); err == nil {
		bits = p.Bits()
	}

	endpoints := map[string]networkEndpoint{}
	addEndpoint := func(cid string) {
		name := shortHex(cid)
		if c, ok := ctrs[cid]; ok && c.Name != "" {
			name = c.Name
		}
		ep := networkEndpoint{
			Name:       name,
			EndpointID: deterministicHex64(n.ID + ":" + cid),
		}
		switch {
		case n.Builtin && n.Name == "bridge":
			ep.MacAddress = "02:42:0a:00:02:0f"
			ep.IPv4Address = "10.0.2.15/24"
		case n.IPs[cid] != "":
			ep.MacAddress = containerMAC(cid)
			ep.IPv4Address = n.IPs[cid] + "/" + strconv.Itoa(bits)
		}
		endpoints[cid] = ep
	}
	for cid := range n.Containers {
		addEndpoint(cid)
	}
	// Also reflect containers that record this network in their Networks map
	// (e.g. builtin bridge after engine restart).
	for cid, c := range ctrs {
		if _, ok := c.Networks[n.Name]; ok {
			if _, exists := endpoints[cid]; !exists {
				addEndpoint(cid)
			}
		}
	}

	return dockerNetwork{
		Name:       n.Name,
		ID:         n.ID,
		Created:    n.Created,
		Scope:      "local",
		Driver:     n.Driver,
		EnableIPv6: false,
		IPAM:       ipam,
		Internal:   n.Internal,
		Attachable: n.Attachable,
		Ingress:    false,
		Containers: endpoints,
		Options:    opts,
		Labels:     labels,
	}
}

func (e *Engine) containerMap() map[string]Container {
	ctrs := e.allContainers()
	m := make(map[string]Container, len(ctrs))
	for _, c := range ctrs {
		m[c.ID] = c
	}
	return m
}

// ---- handlers ----

func (e *Engine) networkList(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	e.net.mu.Lock()
	if e.net.byID == nil {
		e.net.byID = map[string]*Network{}
		for _, b := range builtinNetworks() {
			e.net.byID[b.ID] = b
		}
	}
	nets := make([]Network, 0, len(e.net.byID))
	for _, n := range e.net.byID {
		nets = append(nets, cloneNetwork(n))
	}
	e.net.mu.Unlock()

	sort.Slice(nets, func(i, j int) bool {
		if nets[i].Builtin != nets[j].Builtin {
			return nets[i].Builtin
		}
		return nets[i].Name < nets[j].Name
	})

	ctrs := e.containerMap()
	out := make([]dockerNetwork, 0, len(nets))
	for _, n := range nets {
		if !f.matchAny("name", func(s string) bool { return strings.Contains(n.Name, s) }) {
			continue
		}
		if !f.matchAny("id", func(s string) bool { return strings.HasPrefix(n.ID, s) }) {
			continue
		}
		if !f.matchAny("driver", func(s string) bool { return n.Driver == s }) {
			continue
		}
		if !f.matchAny("type", func(s string) bool {
			switch s {
			case "builtin":
				return n.Builtin
			case "custom":
				return !n.Builtin
			default:
				return false
			}
		}) {
			continue
		}
		if !f.matchLabels(n.Labels) {
			continue
		}
		dn := e.toDockerNetwork(n, ctrs)
		if f.has("dangling") {
			isDangling := !n.Builtin && len(dn.Containers) == 0
			if !f.matchAny("dangling", func(s string) bool {
				want := strings.EqualFold(s, "true") || s == "1"
				return isDangling == want
			}) {
				continue
			}
		}
		out = append(out, dn)
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) networkCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Internal   bool              `json:"Internal"`
		Attachable bool              `json:"Attachable"`
		Options    map[string]string `json:"Options"`
		Labels     map[string]string `json:"Labels"`
		EnableIPv6 *bool             `json:"EnableIPv6"`
		IPAM       *struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	var wantSubnet, wantGateway string
	if req.IPAM != nil {
		for _, c := range req.IPAM.Config {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is6() {
				continue // IPv6 is not supported; the IPv4 subnet is kept
			}
			if wantSubnet != "" {
				writeError(w, r, errInvalid("only one IPv4 subnet per network is supported"))
				return
			}
			wantSubnet, wantGateway = c.Subnet, c.Gateway
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, errInvalid("network name cannot be empty"))
		return
	}
	driver := strings.TrimSpace(req.Driver)
	if driver == "" {
		driver = "bridge"
	}

	e.net.mu.Lock()
	if e.net.byID == nil {
		e.net.byID = map[string]*Network{}
		for _, b := range builtinNetworks() {
			e.net.byID[b.ID] = b
		}
	}
	for _, existing := range e.net.byID {
		if existing.Name == name {
			e.net.mu.Unlock()
			writeError(w, r, errConflict("network with name %s already exists", name))
			return
		}
	}
	n := &Network{
		ID:         randomHex64(),
		Name:       name,
		Driver:     driver,
		Created:    time.Now().UTC(),
		Labels:     maps.Clone(req.Labels),
		Options:    maps.Clone(req.Options),
		Internal:   req.Internal,
		Attachable: req.Attachable,
		Containers: map[string][]string{},
	}
	if hasSubnet(driver) {
		sub, err := e.allocSubnetLocked(wantSubnet, wantGateway)
		if err != nil {
			e.net.mu.Unlock()
			writeError(w, r, err)
			return
		}
		n.Subnet, n.Gateway = sub.subnet.String(), sub.gateway.String()
	}
	e.net.byID[n.ID] = n
	err := e.saveNetworksLocked()
	id := n.ID
	e.net.mu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}

	e.emit(Event{
		Type:   "network",
		Action: "create",
		ID:     id,
		Attributes: map[string]string{
			"name": name,
			"type": driver,
		},
	})
	e.notify()

	writeJSON(w, http.StatusCreated, struct {
		ID      string `json:"Id"`
		Warning string `json:"Warning"`
	}{ID: id, Warning: ""})
}

func (e *Engine) networkInspect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := e.network(id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e.toDockerNetwork(n, e.containerMap()))
}

func (e *Engine) networkDelete(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	ctrs := e.containerMap()

	e.net.mu.Lock()
	n, err := e.findNetworkLocked(ref)
	if err != nil {
		e.net.mu.Unlock()
		writeError(w, r, err)
		return
	}
	if n.Builtin {
		name := n.Name
		e.net.mu.Unlock()
		writeError(w, r, &apiError{http.StatusForbidden, fmt.Sprintf("%s is a pre-defined network and cannot be removed", name)})
		return
	}
	hasEndpoints := len(n.Containers) > 0
	if !hasEndpoints {
		for _, c := range ctrs {
			if _, ok := c.Networks[n.Name]; ok {
				hasEndpoints = true
				break
			}
		}
	}
	if hasEndpoints {
		name, id := n.Name, n.ID
		e.net.mu.Unlock()
		writeError(w, r, &apiError{http.StatusForbidden, fmt.Sprintf("error while removing network: network %s id %s has active endpoints", name, id)})
		return
	}
	id, name, driver := n.ID, n.Name, n.Driver
	delete(e.net.byID, id)
	err = e.saveNetworksLocked()
	e.net.mu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}

	e.emit(Event{
		Type:   "network",
		Action: "destroy",
		ID:     id,
		Attributes: map[string]string{
			"name": name,
			"type": driver,
		},
	})
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) networkConnect(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	var req struct {
		Container      string          `json:"Container"`
		EndpointConfig *endpointConfig `json:"EndpointConfig"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	cid := strings.TrimSpace(req.Container)
	if cid == "" {
		writeError(w, r, errInvalid("container is required"))
		return
	}
	c, err := e.lookup(cid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cid = c.ID
	n, err := e.network(ref)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, ok := c.Networks[n.Name]; ok {
		writeError(w, r, &apiError{http.StatusForbidden, fmt.Sprintf("endpoint with name %s already exists in network %s", c.Name, n.Name)})
		return
	}
	if c.HostConfig.NetworkMode == "none" || n.Name == "none" {
		writeError(w, r, &apiError{http.StatusForbidden, "container cannot be disconnected from network none or connected to it"})
		return
	}
	var aliases []string
	ipv4 := ""
	if req.EndpointConfig != nil {
		aliases = req.EndpointConfig.Aliases
		ipv4 = req.EndpointConfig.ipv4()
	}
	ep, err := e.connectNetwork(n.ID, cid, aliases, ipv4)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ep.EndpointID = randomHex64()
	ep.DNSNames = buildDNSNames(c.Name, cid[:12], aliases)
	if ep.IPAddress != "" {
		ep.MacAddress = containerMAC(cid)
	}
	e.ctr.mu.Lock()
	cur := e.ctr.byID[cid]
	if cur != nil {
		if cur.Networks == nil {
			cur.Networks = map[string]EndpointSettings{}
		}
		cur.Networks[n.Name] = ep
		if err := e.saveContainersLocked(); err != nil {
			e.logf("engine: save containers.json: %v", err)
		}
	}
	e.ctr.mu.Unlock()
	if cur == nil {
		e.disconnectNetworks(cid)
		writeError(w, r, errNotFound("No such container: %s", cid))
		return
	}
	e.syncOverlay()
	e.applyOverlayLive(cid)
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) networkDisconnect(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	var req struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	cid := strings.TrimSpace(req.Container)
	if cid == "" {
		writeError(w, r, errInvalid("container is required"))
		return
	}
	var ctrNetworks map[string]EndpointSettings
	if c, err := e.lookup(cid); err == nil {
		cid = c.ID
		ctrNetworks = c.Networks
	} else if !req.Force {
		writeError(w, r, err)
		return
	}

	e.net.mu.Lock()
	n, err := e.findNetworkLocked(ref)
	if err != nil {
		e.net.mu.Unlock()
		writeError(w, r, err)
		return
	}
	_, connected := n.Containers[cid]
	if !connected && ctrNetworks != nil {
		_, connected = ctrNetworks[n.Name]
	}
	if !connected && !req.Force {
		name := n.Name
		e.net.mu.Unlock()
		writeError(w, r, errNotFound("container %s is not connected to network %s", req.Container, name))
		return
	}
	delete(n.Containers, cid)
	delete(n.IPs, cid)
	netID, netName, netDriver := n.ID, n.Name, n.Driver
	if !n.Builtin {
		err = e.saveNetworksLocked()
	}
	e.net.mu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}
	e.ctr.mu.Lock()
	if cur := e.ctr.byID[cid]; cur != nil {
		if _, ok := cur.Networks[netName]; ok {
			delete(cur.Networks, netName)
			if err := e.saveContainersLocked(); err != nil {
				e.logf("engine: save containers.json: %v", err)
			}
		}
	}
	e.ctr.mu.Unlock()
	e.syncOverlay()
	e.applyOverlayLive(cid)

	if connected {
		e.emit(Event{
			Type:   "network",
			Action: "disconnect",
			ID:     netID,
			Attributes: map[string]string{
				"container": cid,
				"name":      netName,
				"type":      netDriver,
			},
		})
		e.notify()
	}
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) networkPrune(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var untilTime time.Time
	if f.has("until") {
		t, err := parseEventTime(f["until"][0], time.Now().UTC())
		if err != nil {
			writeError(w, r, err)
			return
		}
		untilTime = t
	}

	ctrs := e.containerMap()
	type removedNet struct {
		id, name, driver string
	}
	var removed []removedNet
	deleted := []string{}

	e.net.mu.Lock()
	for id, n := range e.net.byID {
		if n.Builtin || len(n.Containers) > 0 {
			continue
		}
		inUse := false
		for _, c := range ctrs {
			if _, ok := c.Networks[n.Name]; ok {
				inUse = true
				break
			}
		}
		if inUse {
			continue
		}
		if !f.matchLabels(n.Labels) {
			continue
		}
		if !untilTime.IsZero() && !n.Created.Before(untilTime) {
			continue
		}
		removed = append(removed, removedNet{id, n.Name, n.Driver})
		deleted = append(deleted, n.Name)
		delete(e.net.byID, id)
	}
	if len(removed) > 0 {
		err = e.saveNetworksLocked()
	}
	e.net.mu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}

	sort.Strings(deleted)
	for _, d := range removed {
		e.emit(Event{
			Type:   "network",
			Action: "destroy",
			ID:     d.id,
			Attributes: map[string]string{
				"name": d.name,
				"type": d.driver,
			},
		})
	}
	if len(removed) > 0 {
		e.notify()
	}
	writeJSON(w, http.StatusOK, struct {
		NetworksDeleted []string `json:"NetworksDeleted"`
	}{NetworksDeleted: deleted})
}
