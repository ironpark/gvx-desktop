package ui

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/mcpserver"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ---- Local HTTPS reverse proxy (*.localhost) ----------------------------

// LocalProxyState describes the runtime status and active routes of GVX
// Desktop's Labs *.localhost HTTP/HTTPS reverse proxy.
type LocalProxyState struct {
	Enabled   bool
	Running   bool
	HTTPAddr  string
	HTTPSAddr string
	HTTPPort  int
	HTTPSPort int
	CAPath    string
	Routes    []localproxy.Route
	Err       string
}

// Equal makes LocalProxyState comparable for ggui signals.
func (s LocalProxyState) Equal(o LocalProxyState) bool {
	return s.Enabled == o.Enabled &&
		s.Running == o.Running &&
		s.HTTPAddr == o.HTTPAddr &&
		s.HTTPSAddr == o.HTTPSAddr &&
		s.HTTPPort == o.HTTPPort &&
		s.HTTPSPort == o.HTTPSPort &&
		s.CAPath == o.CAPath &&
		s.Err == o.Err &&
		slices.EqualFunc(s.Routes, o.Routes, localproxy.Route.Equal)
}

var preferredWebPorts = []int{80, 443, 8080, 3000, 8000, 5173, 5000, 8443, 9000}

func webPortRank(port int) int {
	if i := slices.Index(preferredWebPorts, port); i >= 0 {
		return i
	}
	return len(preferredWebPorts)
}

func normalizeLoopbackTarget(raw string) (target string, hostPort int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "/") || strings.Contains(raw, `\`) {
		return "", 0, false
	}
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil || portStr == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, false
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, portStr), n, true
}

// deriveLocalProxyRoutes builds the deterministic *.localhost route table from
// running user sandboxes, running Docker containers (including Compose
// services), and active Kubernetes port forwards.
func deriveLocalProxyRoutes(rows []Row, containers []dockerapi.Container, forwards []PortForward) []localproxy.Route {
	taken := make(map[string]bool)
	out := make([]localproxy.Route, 0)
	addRoute := func(rt localproxy.Route) bool {
		if rt.Host == "" || rt.Target == "" || taken[rt.Host] {
			return false
		}
		taken[rt.Host] = true
		out = append(out, rt)
		return true
	}

	// 1. Running user sandboxes (excluding built-in engine/recipe sandboxes).
	type sandboxPort struct {
		target   string
		hostPort int
		guest    int
	}
	for _, r := range rows {
		if r.State.Status != sandbox.Running && r.State.Status != sandbox.Starting {
			continue
		}
		switch r.Spec.Recipe {
		case recipes.Docker, recipes.Kubernetes, recipes.Registry:
			continue
		}
		var ports []sandboxPort
		seenPort := make(map[string]bool)
		for _, p := range r.State.Ports {
			if p.Proto != "" && !strings.EqualFold(p.Proto, "tcp") {
				continue
			}
			target, hostPort, ok := normalizeLoopbackTarget(p.Host)
			if !ok {
				continue
			}
			key := fmt.Sprintf("%s|%d", target, p.Guest)
			if seenPort[key] {
				continue
			}
			seenPort[key] = true
			ports = append(ports, sandboxPort{target: target, hostPort: hostPort, guest: p.Guest})
		}
		if len(ports) == 0 {
			continue
		}
		slices.SortFunc(ports, func(a, b sandboxPort) int {
			return cmp.Or(
				cmp.Compare(webPortRank(a.guest), webPortRank(b.guest)),
				cmp.Compare(a.guest, b.guest),
				cmp.Compare(a.hostPort, b.hostPort),
				strings.Compare(a.target, b.target),
			)
		})
		for i, p := range ports {
			var host string
			if i == 0 {
				host = localproxy.BuildHost(r.Spec.Name)
			} else {
				portNum := p.guest
				if portNum <= 0 {
					portNum = p.hostPort
				}
				host = localproxy.BuildHost(fmt.Sprintf("%s-%d", r.Spec.Name, portNum))
			}
			addRoute(localproxy.Route{
				Host:      host,
				Target:    p.target,
				Kind:      "sandbox",
				Name:      r.Spec.Name,
				GuestPort: p.guest,
			})
		}
	}

	// 2. Running Docker containers (standalone & Compose).
	type containerPort struct {
		publicPort  int
		privatePort int
	}
	for _, c := range containers {
		if !strings.EqualFold(c.State, "running") {
			continue
		}
		var ports []containerPort
		seenPort := make(map[[2]int]bool)
		for _, p := range c.Ports {
			if p.PublicPort <= 0 {
				continue
			}
			if p.Type != "" && !strings.EqualFold(p.Type, "tcp") {
				continue
			}
			key := [2]int{p.PublicPort, p.PrivatePort}
			if seenPort[key] {
				continue
			}
			seenPort[key] = true
			ports = append(ports, containerPort{publicPort: p.PublicPort, privatePort: p.PrivatePort})
		}
		if len(ports) == 0 {
			continue
		}
		slices.SortFunc(ports, func(a, b containerPort) int {
			return cmp.Or(
				cmp.Compare(webPortRank(a.privatePort), webPortRank(b.privatePort)),
				cmp.Compare(a.privatePort, b.privatePort),
				cmp.Compare(a.publicPort, b.publicPort),
			)
		})
		proj, svc := strings.TrimSpace(c.Project()), strings.TrimSpace(c.Service())
		ctrName := c.Name()
		for i, p := range ports {
			var host string
			if proj != "" && svc != "" {
				if i == 0 {
					host = localproxy.BuildHost(svc, proj)
				} else {
					portNum := p.privatePort
					if portNum <= 0 {
						portNum = p.publicPort
					}
					host = localproxy.BuildHost(fmt.Sprintf("%s-%d", svc, portNum), proj)
				}
			} else {
				if i == 0 {
					host = localproxy.BuildHost(ctrName)
				} else {
					portNum := p.privatePort
					if portNum <= 0 {
						portNum = p.publicPort
					}
					host = localproxy.BuildHost(fmt.Sprintf("%s-%d", ctrName, portNum))
				}
			}
			addRoute(localproxy.Route{
				Host:      host,
				Target:    fmt.Sprintf("127.0.0.1:%d", p.publicPort),
				Kind:      "container",
				Name:      ctrName,
				GuestPort: p.privatePort,
			})
		}
	}

	// 3. Active Kubernetes port forwards.
	for _, f := range forwards {
		if f.Local <= 0 {
			continue
		}
		var base, name string
		if f.Target.Service != "" {
			base = f.Target.Service
			name = "svc/" + f.Target.Service
		} else if f.Target.Pod != "" {
			base = f.Target.Pod
			name = "pod/" + f.Target.Pod
		} else {
			continue
		}
		host := localproxy.BuildHost(base, "k8s")
		if taken[host] && f.Target.Port > 0 {
			host = localproxy.BuildHost(fmt.Sprintf("%s-%d", base, f.Target.Port), "k8s")
		}
		if taken[host] {
			host = localproxy.BuildHost(fmt.Sprintf("%s-%d", base, f.Local), "k8s")
		}
		addRoute(localproxy.Route{
			Host:      host,
			Target:    f.Address(),
			Kind:      "k8s",
			Name:      name,
			GuestPort: f.Target.Port,
		})
	}

	slices.SortFunc(out, func(a, b localproxy.Route) int {
		return strings.Compare(a.Host, b.Host)
	})
	return out
}

func localProxyHTTPSURLFor(running bool, httpsPort int, routes []localproxy.Route, kind, name string) string {
	if !running || name == "" {
		return ""
	}
	for _, rt := range routes {
		if rt.Kind == kind && rt.Name == name {
			return localproxy.FormatURL("https", rt.Host, httpsPort)
		}
	}
	return ""
}

// LocalProxyURLFor returns the active https://*.localhost URL for the given
// resource kind ("sandbox", "container", "k8s") and name, or "" when the local
// proxy is not running or has no matching route. Call from the UI thread.
func (m *Model) LocalProxyURLFor(kind, name string) string {
	st := ggui.Peek(m.LocalProxy)
	return localProxyHTTPSURLFor(st.Running, st.HTTPSPort, st.Routes, kind, name)
}

// followLocalProxy watches Settings, sandboxes, containers, and port forwards
// to keep the Labs *.localhost reverse proxy running and its routes synced.
func (m *Model) followLocalProxy() {
	ggui.Effect(func() ggui.Cleanup {
		set := m.Settings.Get()
		rows := m.Rows.Get()
		ctrs := m.DockerContainers.Get().Items
		fwds := m.Forwards.Get()
		m.syncLocalProxyWith(set, rows, ctrs, fwds, false)
		return nil
	})
}

func (m *Model) syncLocalProxy(forceRetry bool) {
	m.syncLocalProxyWith(
		ggui.Peek(m.Settings),
		ggui.Peek(m.Rows),
		ggui.Peek(m.DockerContainers).Items,
		ggui.Peek(m.Forwards),
		forceRetry,
	)
}

func (m *Model) syncLocalProxyWith(set store.Settings, rows []Row, containers []dockerapi.Container, forwards []PortForward, forceRetry bool) {
	wantRunning := set.LocalProxy
	cfgHTTP := set.LocalProxyHTTPPortOrDefault()
	cfgHTTPS := set.LocalProxyHTTPSPortOrDefault()
	routes := deriveLocalProxyRoutes(rows, containers, forwards)
	if extRoutes := m.extensionLocalProxyRoutes(); len(extRoutes) > 0 {
		taken := make(map[string]bool, len(routes))
		for _, rt := range routes {
			taken[rt.Host] = true
		}
		for _, rt := range extRoutes {
			if rt.Host == "" || rt.Target == "" || taken[rt.Host] {
				continue
			}
			taken[rt.Host] = true
			routes = append(routes, rt)
		}
		slices.SortFunc(routes, func(a, b localproxy.Route) int {
			return strings.Compare(a.Host, b.Host)
		})
	}

	caPath := ""
	if m.dataDir != "" {
		caPath = filepath.Join(m.dataDir, "localproxy", "ca.crt")
	}

	m.localProxyMu.Lock()
	wasWant := m.localProxyWant
	m.localProxyWant = wantRunning
	srv := m.localProxy
	m.localProxyRoutes = slices.Clone(routes)
	boundHTTP := m.localProxyBoundHTTP
	boundHTTPS := m.localProxyBoundHTTPS
	inFlight := m.localProxyInFlight
	m.localProxyMu.Unlock()

	if srv != nil && srv.CAPath() != "" {
		caPath = srv.CAPath()
	}

	portsChanged := srv != nil && (boundHTTP != cfgHTTP || boundHTTPS != cfgHTTPS)

	m.LocalProxy.Update(func(st LocalProxyState) LocalProxyState {
		st.Enabled = wantRunning
		st.Routes = slices.Clone(routes)
		if !st.Running {
			st.HTTPPort = cfgHTTP
			st.HTTPSPort = cfgHTTPS
		}
		if st.CAPath == "" {
			st.CAPath = caPath
		}
		if !wantRunning || forceRetry || portsChanged || (!wasWant && wantRunning) {
			st.Err = ""
		}
		return st
	})

	if m.Offline || m.quitting || m.dataDir == "" {
		return
	}

	running := srv != nil && srv.Running()
	if wantRunning {
		if running && !portsChanged && inFlight != "stop" {
			srv.SetRoutes(routes)
			httpAddr, httpsAddr := srv.HTTPAddr(), srv.HTTPSAddr()
			httpPort, httpsPort := srv.HTTPPort(), srv.HTTPSPort()
			ca := srv.CAPath()
			srvRoutes := srv.Routes()
			m.LocalProxy.Update(func(st LocalProxyState) LocalProxyState {
				st.Enabled = true
				st.Running = true
				st.HTTPAddr = httpAddr
				st.HTTPSAddr = httpsAddr
				st.HTTPPort = httpPort
				st.HTTPSPort = httpsPort
				st.CAPath = ca
				st.Routes = srvRoutes
				st.Err = ""
				return st
			})
			return
		}
		if inFlight == "start" && !forceRetry && !portsChanged {
			if srv != nil {
				srv.SetRoutes(routes)
			}
			return
		}
		if !forceRetry && !portsChanged && wasWant && ggui.Peek(m.LocalProxy).Err != "" {
			return
		}
		m.reconcileLocalProxy(true, cfgHTTP, cfgHTTPS, "start")
		return
	}

	if !running && inFlight != "start" {
		m.LocalProxy.Update(func(st LocalProxyState) LocalProxyState {
			st.Enabled = false
			st.Running = false
			st.HTTPAddr = ""
			st.HTTPSAddr = ""
			st.HTTPPort = cfgHTTP
			st.HTTPSPort = cfgHTTPS
			st.Err = ""
			return st
		})
		return
	}
	if inFlight == "stop" {
		return
	}
	m.reconcileLocalProxy(false, cfgHTTP, cfgHTTPS, "stop")
}

func (m *Model) reconcileLocalProxy(want bool, cfgHTTP, cfgHTTPS int, op string) {
	m.localProxyMu.Lock()
	m.localProxyDesired = want
	m.localProxyWantHTTP = cfgHTTP
	m.localProxyWantHTTPS = cfgHTTPS
	m.localProxySyncGen++
	gen := m.localProxySyncGen
	m.localProxyInFlight = op
	m.localProxyMu.Unlock()

	var (
		runningAfter   bool
		httpAddrAfter  string
		httpsAddrAfter string
		httpPortAfter  int
		httpsPortAfter int
		caPathAfter    string
		routesAfter    []localproxy.Route
	)

	m.async(func(context.Context) error {
		m.localProxyOpMu.Lock()
		defer m.localProxyOpMu.Unlock()

		m.localProxyMu.Lock()
		desired := m.localProxyDesired && !m.localProxyClosed
		wantHTTP := m.localProxyWantHTTP
		wantHTTPS := m.localProxyWantHTTPS
		routesCopy := slices.Clone(m.localProxyRoutes)
		srv := m.localProxy
		boundHTTP := m.localProxyBoundHTTP
		boundHTTPS := m.localProxyBoundHTTPS
		m.localProxyMu.Unlock()

		if !desired {
			if srv != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := srv.Stop(ctx)
				cancel()
				runningAfter = srv.Running()
				httpAddrAfter = srv.HTTPAddr()
				httpsAddrAfter = srv.HTTPSAddr()
				httpPortAfter = wantHTTP
				httpsPortAfter = wantHTTPS
				caPathAfter = srv.CAPath()
				routesAfter = routesCopy
				return err
			}
			httpPortAfter = wantHTTP
			httpsPortAfter = wantHTTPS
			routesAfter = routesCopy
			return nil
		}

		needNew := srv == nil || boundHTTP != wantHTTP || boundHTTPS != wantHTTPS
		if needNew {
			if srv != nil && srv.Running() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = srv.Stop(ctx)
				cancel()
			}
			bindHTTP, bindHTTPS := wantHTTP, wantHTTPS
			if m.localProxyTestPorts {
				bindHTTP, bindHTTPS = -1, -1
			}
			srv = localproxy.New(localproxy.Config{
				Dir:       filepath.Join(m.dataDir, "localproxy"),
				HTTPPort:  bindHTTP,
				HTTPSPort: bindHTTPS,
			})
			m.localProxyMu.Lock()
			m.localProxy = srv
			m.localProxyBoundHTTP = wantHTTP
			m.localProxyBoundHTTPS = wantHTTPS
			routesCopy = slices.Clone(m.localProxyRoutes)
			m.localProxyMu.Unlock()
		}

		srv.SetRoutes(routesCopy)
		err := srv.Start()
		runningAfter = srv.Running()
		httpAddrAfter = srv.HTTPAddr()
		httpsAddrAfter = srv.HTTPSAddr()
		if runningAfter {
			httpPortAfter = srv.HTTPPort()
			httpsPortAfter = srv.HTTPSPort()
		} else {
			httpPortAfter = wantHTTP
			httpsPortAfter = wantHTTPS
		}
		caPathAfter = srv.CAPath()
		routesAfter = srv.Routes()
		return err
	}, func(err error) {
		m.localProxyMu.Lock()
		stale := m.localProxySyncGen != gen
		if !stale {
			m.localProxyInFlight = ""
		}
		m.localProxyMu.Unlock()
		if stale {
			return
		}
		if m.quitting {
			return
		}
		m.LocalProxy.Update(func(st LocalProxyState) LocalProxyState {
			st.Running = runningAfter
			st.HTTPAddr = httpAddrAfter
			st.HTTPSAddr = httpsAddrAfter
			st.HTTPPort = httpPortAfter
			st.HTTPSPort = httpsPortAfter
			if caPathAfter != "" {
				st.CAPath = caPathAfter
			}
			if runningAfter {
				st.Routes = routesAfter
			}
			if err != nil {
				st.Err = err.Error()
			} else {
				st.Err = ""
			}
			return st
		})
	})
}

// SetLocalProxy enables or disables the Labs *.localhost reverse proxy.
func (m *Model) SetLocalProxy(enabled bool) {
	if ggui.Peek(m.Settings).LocalProxy != enabled {
		m.UpdateSettings(func(s *store.Settings) {
			s.LocalProxy = enabled
		})
	}
	m.syncLocalProxy(false)
}

// SetLocalProxyPorts updates the configured HTTP and HTTPS loopback ports for
// the local reverse proxy and restarts it immediately if running.
func (m *Model) SetLocalProxyPorts(httpPort, httpsPort int) {
	if httpPort < 1 || httpPort > 65535 || httpsPort < 1 || httpsPort > 65535 || httpPort == httpsPort {
		return
	}
	cur := ggui.Peek(m.Settings)
	if cur.LocalProxyHTTPPortOrDefault() == httpPort && cur.LocalProxyHTTPSPortOrDefault() == httpsPort {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.LocalProxyHTTPPort = httpPort
		s.LocalProxyHTTPSPort = httpsPort
	})
	m.syncLocalProxy(false)
}

// RetryLocalProxy clears the last local proxy error and retries starting it.
func (m *Model) RetryLocalProxy() {
	m.syncLocalProxy(true)
}

// CloseLocalProxy stops the local proxy cleanly on shutdown.
func (m *Model) CloseLocalProxy() {
	m.localProxyMu.Lock()
	m.localProxyClosed = true
	m.localProxyDesired = false
	m.localProxyMu.Unlock()

	m.localProxyOpMu.Lock()
	defer m.localProxyOpMu.Unlock()
	m.localProxyMu.Lock()
	srv := m.localProxy
	m.localProxyMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
}

// ---- Built-in AI Agent MCP server ---------------------------------------

// MCPServerState describes the runtime status and live tool call metrics of
// GVX Desktop's Labs built-in Streamable HTTP MCP server.
type MCPServerState struct {
	Enabled      bool
	Running      bool
	Addr         string
	EndpointURL  string
	Port         int
	CallCount    int64
	LastTool     string
	LastCalledAt time.Time
	LastErr      string
	Err          string
}

// Equal makes MCPServerState comparable for ggui signals.
func (s MCPServerState) Equal(o MCPServerState) bool {
	return s.Enabled == o.Enabled &&
		s.Running == o.Running &&
		s.Addr == o.Addr &&
		s.EndpointURL == o.EndpointURL &&
		s.Port == o.Port &&
		s.CallCount == o.CallCount &&
		s.LastTool == o.LastTool &&
		s.LastCalledAt.Equal(o.LastCalledAt) &&
		s.LastErr == o.LastErr &&
		s.Err == o.Err
}

// mcpSnapshot is a thread-safe copy of UI state maintained on the UI goroutine
// and read by MCP HTTP request handlers without ever touching ggui signals off
// the UI thread.
type mcpSnapshot struct {
	specs          []sandbox.Spec
	states         map[string]sandbox.State
	containers     []dockerapi.Container
	containersOpen bool
	dockerHost     string
	stopTimeout    time.Duration
	proxyRunning   bool
	proxyHTTPPort  int
	proxyHTTPSPort int
	proxyRoutes    []localproxy.Route
	cachedImages   []rungv.Image
	offline        bool
}

func (m *Model) refreshMCPSnapshot() {
	specs := ggui.Peek(m.Specs)
	states := ggui.Peek(m.States)
	ctrs := ggui.Peek(m.DockerContainers).Items
	containersOpen := ggui.Peek(m.ContainersOpen)
	proxy := ggui.Peek(m.LocalProxy)
	set := ggui.Peek(m.Settings)
	imgs := ggui.Peek(m.Images).Items
	dockerHost := m.dockerBackendHost()

	m.storeMCPSnapshot(specs, states, ctrs, containersOpen, dockerHost, set, proxy, imgs)
}

func (m *Model) storeMCPSnapshot(
	specs []sandbox.Spec,
	states map[string]sandbox.State,
	containers []dockerapi.Container,
	containersOpen bool,
	dockerHost string,
	set store.Settings,
	proxy LocalProxyState,
	cachedImages []rungv.Image,
) {
	specsCopy := make([]sandbox.Spec, len(specs))
	for i, s := range specs {
		specsCopy[i] = s.Clone()
	}
	statesCopy := maps.Clone(states)
	if statesCopy == nil {
		statesCopy = map[string]sandbox.State{}
	}
	timeout := time.Duration(set.StopTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	snap := mcpSnapshot{
		specs:          specsCopy,
		states:         statesCopy,
		containers:     slices.Clone(containers),
		containersOpen: containersOpen,
		dockerHost:     dockerHost,
		stopTimeout:    timeout,
		proxyRunning:   proxy.Running,
		proxyHTTPPort:  proxy.HTTPPort,
		proxyHTTPSPort: proxy.HTTPSPort,
		proxyRoutes:    slices.Clone(proxy.Routes),
		cachedImages:   slices.Clone(cachedImages),
		offline:        m.Offline,
	}
	m.mcpSnapMu.Lock()
	m.mcpSnap = snap
	m.mcpSnapMu.Unlock()
}

func (m *Model) snapshotMCP() mcpSnapshot {
	m.mcpSnapMu.RLock()
	defer m.mcpSnapMu.RUnlock()
	return m.mcpSnap
}

// callUI executes fn on the UI goroutine via m.post and waits for its result
// or context cancellation. Used by MCP HTTP handlers for safe UI mutations.
func (m *Model) callUI(ctx context.Context, fn func() error) error {
	if m.host == nil {
		return errors.New("desktop UI host is not attached")
	}
	done := make(chan error, 1)
	m.post(func() {
		if m.quitting {
			done <- errors.New("GVX Desktop is shutting down")
			return
		}
		done <- fn()
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// followMCPServer maintains the thread-safe MCP state snapshot on the UI
// goroutine and watches Settings.MCPServer / Settings.MCPPort to start, stop,
// or restart the built-in MCP server.
func (m *Model) followMCPServer() {
	ggui.Effect(func() ggui.Cleanup {
		specs := m.Specs.Get()
		states := m.States.Get()
		ctrs := m.DockerContainers.Get().Items
		containersOpen := m.ContainersOpen.Get()
		proxy := m.LocalProxy.Get()
		_ = m.DockerEndpoint.Get()
		set := m.Settings.Get()
		imgs := m.Images.Get().Items
		m.storeMCPSnapshot(specs, states, ctrs, containersOpen, m.dockerBackendHost(), set, proxy, imgs)
		return nil
	})
	ggui.Effect(func() ggui.Cleanup {
		set := m.Settings.Get()
		m.syncMCPServerWith(set, false)
		return nil
	})
}

func (m *Model) syncMCPServer(forceRetry bool) {
	m.syncMCPServerWith(ggui.Peek(m.Settings), forceRetry)
}

func (m *Model) syncMCPServerWith(set store.Settings, forceRetry bool) {
	wantRunning := set.MCPServer
	cfgPort := set.MCPPortOrDefault()

	wasWant := m.mcpWant
	m.mcpWant = wantRunning

	m.mcpMu.Lock()
	srv := m.mcpSrv
	boundPort := m.mcpBoundPort
	m.mcpMu.Unlock()

	portChanged := srv != nil && boundPort != cfgPort

	m.MCPServer.Update(func(st MCPServerState) MCPServerState {
		st.Enabled = wantRunning
		if !st.Running {
			st.Port = cfgPort
		}
		if !wantRunning || forceRetry || portChanged || (!wasWant && wantRunning) {
			st.Err = ""
		}
		return st
	})

	if m.Offline || m.quitting {
		return
	}

	running := srv != nil && srv.Running()
	m.mcpMu.Lock()
	inFlight := m.mcpInFlight
	m.mcpMu.Unlock()
	if wantRunning {
		if running && !portChanged && inFlight != "stop" {
			addr := srv.Addr()
			ep := srv.EndpointURL()
			port := srv.Port()
			stats := srv.Stats()
			m.MCPServer.Update(func(st MCPServerState) MCPServerState {
				st.Enabled = true
				st.Running = true
				st.Addr = addr
				st.EndpointURL = ep
				st.Port = port
				st.CallCount = stats.CallCount
				st.LastTool = stats.LastTool
				st.LastCalledAt = stats.LastCalledAt
				st.LastErr = stats.LastErr
				st.Err = ""
				return st
			})
			return
		}
		if inFlight == "start" && !forceRetry && !portChanged {
			return
		}
		if !forceRetry && !portChanged && wasWant && ggui.Peek(m.MCPServer).Err != "" {
			return
		}
		m.reconcileMCPServer(true, cfgPort, "start")
		return
	}

	if !running && inFlight != "start" {
		m.MCPServer.Update(func(st MCPServerState) MCPServerState {
			st.Enabled = false
			st.Running = false
			st.Addr = ""
			st.EndpointURL = ""
			st.Port = cfgPort
			st.Err = ""
			return st
		})
		return
	}
	if inFlight == "stop" {
		return
	}
	m.reconcileMCPServer(false, cfgPort, "stop")
}

func (m *Model) reconcileMCPServer(want bool, cfgPort int, op string) {
	m.mcpMu.Lock()
	m.mcpDesired = want
	m.mcpWantPort = cfgPort
	m.mcpSyncGen++
	gen := m.mcpSyncGen
	m.mcpInFlight = op
	m.mcpMu.Unlock()

	var (
		runningAfter bool
		addrAfter    string
		epAfter      string
		portAfter    int
		statsAfter   mcpserver.Stats
	)

	m.async(func(context.Context) error {
		m.mcpOpMu.Lock()
		defer m.mcpOpMu.Unlock()

		m.mcpMu.Lock()
		desired := m.mcpDesired && !m.mcpClosed
		wantPort := m.mcpWantPort
		srv := m.mcpSrv
		boundPort := m.mcpBoundPort
		m.mcpMu.Unlock()

		if !desired {
			if srv != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := srv.Stop(ctx)
				cancel()
				runningAfter = srv.Running()
				addrAfter = srv.Addr()
				if runningAfter {
					epAfter = srv.EndpointURL()
					portAfter = srv.Port()
				} else {
					portAfter = wantPort
				}
				statsAfter = srv.Stats()
				return err
			}
			portAfter = wantPort
			return nil
		}

		needNew := srv == nil || boundPort != wantPort
		if needNew {
			if srv != nil && srv.Running() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = srv.Stop(ctx)
				cancel()
			}
			bindPort := wantPort
			if m.mcpTestPort < 0 {
				bindPort = -1
			}
			srv = mcpserver.New(mcpserver.Config{
				Port:    bindPort,
				Version: AppVersion,
				Backend: &modelMCPBackend{m: m},
				OnActivity: func(stats mcpserver.Stats) {
					m.post(func() {
						if m.quitting {
							return
						}
						m.MCPServer.Update(func(st MCPServerState) MCPServerState {
							st.CallCount = stats.CallCount
							st.LastTool = stats.LastTool
							st.LastCalledAt = stats.LastCalledAt
							st.LastErr = stats.LastErr
							return st
						})
					})
				},
			})
			m.mcpMu.Lock()
			m.mcpSrv = srv
			m.mcpBoundPort = wantPort
			m.mcpMu.Unlock()
		}

		err := srv.Start()
		runningAfter = srv.Running()
		addrAfter = srv.Addr()
		if runningAfter {
			epAfter = srv.EndpointURL()
			portAfter = srv.Port()
		} else {
			portAfter = wantPort
		}
		statsAfter = srv.Stats()
		return err
	}, func(err error) {
		m.mcpMu.Lock()
		stale := m.mcpSyncGen != gen
		if !stale {
			m.mcpInFlight = ""
		}
		m.mcpMu.Unlock()
		if stale || m.quitting {
			return
		}
		m.MCPServer.Update(func(st MCPServerState) MCPServerState {
			st.Running = runningAfter
			st.Addr = addrAfter
			st.EndpointURL = epAfter
			st.Port = portAfter
			st.CallCount = statsAfter.CallCount
			st.LastTool = statsAfter.LastTool
			st.LastCalledAt = statsAfter.LastCalledAt
			st.LastErr = statsAfter.LastErr
			if err != nil {
				st.Err = err.Error()
			} else {
				st.Err = ""
			}
			return st
		})
	})
}

// SetMCPServer enables or disables the Labs built-in MCP server.
func (m *Model) SetMCPServer(enabled bool) {
	if ggui.Peek(m.Settings).MCPServer != enabled {
		m.UpdateSettings(func(s *store.Settings) {
			s.MCPServer = enabled
		})
	}
	m.syncMCPServer(false)
}

// SetMCPPort updates the configured MCP server loopback port and restarts the
// server immediately if running.
func (m *Model) SetMCPPort(port int) {
	if port < 1 || port > 65535 {
		return
	}
	if ggui.Peek(m.Settings).MCPPortOrDefault() == port {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.MCPPort = port
	})
	m.syncMCPServer(false)
}

// RetryMCPServer clears the last MCP server error and retries starting it.
func (m *Model) RetryMCPServer() {
	m.syncMCPServer(true)
}

// CloseMCPServer stops the built-in MCP server cleanly on shutdown.
func (m *Model) CloseMCPServer() {
	m.mcpMu.Lock()
	m.mcpClosed = true
	m.mcpDesired = false
	m.mcpMu.Unlock()

	m.mcpOpMu.Lock()
	defer m.mcpOpMu.Unlock()
	m.mcpMu.Lock()
	srv := m.mcpSrv
	m.mcpMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
}

// ---- MCP Backend implementation -----------------------------------------

type modelMCPBackend struct {
	m *Model
}

var _ mcpserver.Backend = (*modelMCPBackend)(nil)

func resolveSandboxSpec(specs []sandbox.Spec, target string) (sandbox.Spec, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return sandbox.Spec{}, errors.New("sandbox target is required")
	}
	for _, s := range specs {
		if s.ID == target {
			return s, nil
		}
	}
	for _, s := range specs {
		if s.Name == target {
			return s, nil
		}
	}
	for _, s := range specs {
		if strings.EqualFold(s.Name, target) {
			return s, nil
		}
	}
	return sandbox.Spec{}, fmt.Errorf("sandbox %q not found", target)
}

func (b *modelMCPBackend) sandboxState(snap mcpSnapshot, id string) sandbox.State {
	st := snap.states[id]
	if !snap.offline && b.m.sup != nil {
		live := b.m.sup.State(id)
		if live.Status != sandbox.Stopped || st.Status == sandbox.Stopped {
			st = live
		}
	}
	return st
}

func sandboxInfoFrom(spec sandbox.Spec, st sandbox.State, snap mcpSnapshot) mcpserver.SandboxInfo {
	status := strings.ToLower(st.Status.String())
	if st.Status.Active() && st.Paused {
		status = "paused"
	}
	var ports []string
	if len(st.Ports) > 0 {
		ports = make([]string, 0, len(st.Ports))
		for _, p := range st.Ports {
			proto := p.Proto
			if proto == "" {
				proto = "tcp"
			}
			if p.Guest > 0 {
				ports = append(ports, fmt.Sprintf("%s->%d/%s", p.Host, p.Guest, proto))
			} else {
				ports = append(ports, p.Host)
			}
		}
	} else if len(spec.Ports) > 0 {
		ports = slices.Clone(spec.Ports)
	}
	return mcpserver.SandboxInfo{
		ID:         spec.ID,
		Name:       spec.Name,
		Image:      spec.Image,
		Status:     status,
		Paused:     st.Paused,
		Checkpoint: (spec.Checkpoint || st.CheckpointStream) && !spec.UsesGPU(),
		Ports:      ports,
		Recipe:     spec.Recipe,
		LocalURL:   localProxyHTTPSURLFor(snap.proxyRunning, snap.proxyHTTPSPort, snap.proxyRoutes, "sandbox", spec.Name),
	}
}

func (b *modelMCPBackend) ListSandboxes(_ context.Context) ([]mcpserver.SandboxInfo, error) {
	snap := b.m.snapshotMCP()
	out := make([]mcpserver.SandboxInfo, 0, len(snap.specs))
	for _, spec := range snap.specs {
		st := b.sandboxState(snap, spec.ID)
		out = append(out, sandboxInfoFrom(spec, st, snap))
	}
	return out, nil
}

func (b *modelMCPBackend) CreateSandbox(ctx context.Context, req mcpserver.CreateSandboxRequest) (mcpserver.SandboxInfo, error) {
	img := strings.TrimSpace(req.Image)
	if img == "" {
		return mcpserver.SandboxInfo{}, errors.New("image is required")
	}
	var info mcpserver.SandboxInfo
	err := b.m.callUI(ctx, func() error {
		specs := slices.Clone(ggui.Peek(b.m.Specs))
		baseName := strings.TrimSpace(req.Name)
		if baseName == "" {
			baseName = sandbox.SuggestName(img)
		}
		name := uniqueName(specs, baseName)
		spec := sandbox.Spec{
			ID:         sandbox.NewID(),
			Name:       name,
			Image:      img,
			Command:    strings.TrimSpace(req.Command),
			Env:        slices.Clone(req.Env),
			Ports:      slices.Clone(req.Ports),
			Workspace:  strings.TrimSpace(req.Workspace),
			Memory:     strings.TrimSpace(req.Memory),
			CPUs:       strings.TrimSpace(req.CPUs),
			Checkpoint: req.CheckpointEnabled(),
			CreatedAt:  time.Now(),
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		specs = append(specs, spec)
		b.m.saveSpecs(specs)
		b.m.Select(spec.ID)
		if req.ShouldStart() {
			if !ggui.Peek(b.m.Engine).Ready() {
				b.m.refreshMCPSnapshot()
				return errors.New(engineProblem(ggui.Peek(b.m.Engine)))
			}
			b.m.StartSandbox(spec.ID)
		}
		b.m.refreshMCPSnapshot()
		snap := b.m.snapshotMCP()
		info = sandboxInfoFrom(spec, b.sandboxState(snap, spec.ID), snap)
		return nil
	})
	return info, err
}

func (b *modelMCPBackend) StartSandbox(ctx context.Context, target string) (mcpserver.SandboxInfo, error) {
	var info mcpserver.SandboxInfo
	err := b.m.callUI(ctx, func() error {
		spec, err := resolveSandboxSpec(ggui.Peek(b.m.Specs), target)
		if err != nil {
			return err
		}
		if !b.m.sup.State(spec.ID).Status.Active() {
			if !ggui.Peek(b.m.Engine).Ready() {
				return errors.New(engineProblem(ggui.Peek(b.m.Engine)))
			}
			b.m.StartSandbox(spec.ID)
		}
		b.m.refreshMCPSnapshot()
		snap := b.m.snapshotMCP()
		info = sandboxInfoFrom(spec, b.sandboxState(snap, spec.ID), snap)
		return nil
	})
	return info, err
}

func (b *modelMCPBackend) StopSandbox(ctx context.Context, target string) (mcpserver.SandboxInfo, error) {
	var (
		spec    sandbox.Spec
		stopIDs []string
		timeout time.Duration
	)
	if err := b.m.callUI(ctx, func() error {
		var err error
		spec, err = resolveSandboxSpec(ggui.Peek(b.m.Specs), target)
		if err != nil {
			return err
		}
		b.m.setResume(spec.ID, false)
		stopIDs = []string{spec.ID}
		if spec.Recipe == recipes.Kubernetes {
			for _, a := range agentsOf(ggui.Peek(b.m.Specs)) {
				b.m.setResume(a, false)
				stopIDs = append(stopIDs, a)
			}
		}
		timeout = time.Duration(ggui.Peek(b.m.Settings).StopTimeout) * time.Second
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		return nil
	}); err != nil {
		return mcpserver.SandboxInfo{}, err
	}

	var errs []error
	for _, id := range stopIDs {
		if err := b.m.sup.Stop(id, timeout); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return mcpserver.SandboxInfo{}, err
	}

	var info mcpserver.SandboxInfo
	err := b.m.callUI(ctx, func() error {
		b.m.refreshStates()
		b.m.refreshMCPSnapshot()
		snap := b.m.snapshotMCP()
		info = sandboxInfoFrom(spec, b.sandboxState(snap, spec.ID), snap)
		return nil
	})
	return info, err
}

func (b *modelMCPBackend) DeleteSandbox(ctx context.Context, target string) error {
	var (
		gone    []string
		timeout time.Duration
	)
	if err := b.m.callUI(ctx, func() error {
		specs := ggui.Peek(b.m.Specs)
		spec, err := resolveSandboxSpec(specs, target)
		if err != nil {
			return err
		}
		gone = []string{spec.ID}
		if spec.Recipe == recipes.Kubernetes {
			gone = append(gone, agentsOf(specs)...)
		}
		timeout = time.Duration(ggui.Peek(b.m.Settings).StopTimeout) * time.Second
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	for _, g := range gone {
		if err := b.m.sup.Stop(g, timeout); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	return b.m.callUI(ctx, func() error {
		for _, g := range gone {
			b.m.sup.Forget(g)
			b.m.setResume(g, false)
		}
		specs := slices.DeleteFunc(slices.Clone(ggui.Peek(b.m.Specs)), func(s sandbox.Spec) bool {
			return slices.Contains(gone, s.ID)
		})
		if slices.Contains(gone, ggui.Peek(b.m.Selected)) {
			next := ""
			if len(specs) > 0 {
				next = specs[0].ID
			}
			b.m.Select(next)
		}
		b.m.saveSpecs(specs)
		b.m.refreshMCPSnapshot()
		return nil
	})
}

func readLimitedStream(r io.Reader, limit int64) string {
	if r == nil {
		return ""
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(r, limit))
	_, _ = io.Copy(io.Discard, r)
	return buf.String()
}

func (b *modelMCPBackend) ExecInSandbox(ctx context.Context, target string, command []string, workdir string, env []string) (mcpserver.ExecResult, error) {
	snap := b.m.snapshotMCP()
	spec, err := resolveSandboxSpec(snap.specs, target)
	if err != nil {
		return mcpserver.ExecResult{}, err
	}
	ctrl, ok := b.m.sandboxControl(spec.ID)
	if !ok {
		return mcpserver.ExecResult{}, fmt.Errorf("sandbox %q is not running or does not expose a control socket", spec.Name)
	}
	cleanEnv := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.Contains(kv, "=") {
			cleanEnv = append(cleanEnv, kv)
		}
	}
	proc, err := ctrl.Exec(ctx, sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:    command,
			Workdir: workdir,
			Env:     cleanEnv,
		},
	})
	if err != nil {
		return mcpserver.ExecResult{}, err
	}
	defer func() { _ = proc.Close() }()

	// Read slightly over MaxOutputBytes so mcpserver's truncation marker fires cleanly.
	const captureLimit = int64(mcpserver.MaxOutputBytes) + 64
	var (
		stdout string
		stderr string
		wg     sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdout = readLimitedStream(proc.Stdout(), captureLimit)
	}()
	go func() {
		defer wg.Done()
		stderr = readLimitedStream(proc.Stderr(), captureLimit)
	}()

	code, waitErr := proc.Wait()
	wg.Wait()
	res := mcpserver.ExecResult{
		ExitCode: code,
		Stdout:   stdout,
		Stderr:   stderr,
	}
	if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

func (b *modelMCPBackend) SandboxLogs(_ context.Context, target string, tail int) (string, error) {
	snap := b.m.snapshotMCP()
	spec, err := resolveSandboxSpec(snap.specs, target)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = mcpserver.DefaultLogTail
	}
	lines := b.m.sup.Logs(spec.ID).Tail(tail, 255)
	return strings.Join(lines, "\n"), nil
}

func (b *modelMCPBackend) CreateSnapshot(ctx context.Context, target string, leaveRunning bool) (mcpserver.SnapshotInfo, error) {
	snap := b.m.snapshotMCP()
	spec, err := resolveSandboxSpec(snap.specs, target)
	if err != nil {
		return mcpserver.SnapshotInfo{}, err
	}
	dir, err := b.m.sup.Checkpoint(ctx, spec.ID, leaveRunning)
	if err != nil {
		return mcpserver.SnapshotInfo{}, err
	}
	b.m.post(func() {
		if b.m.quitting {
			return
		}
		if !leaveRunning {
			b.m.setResume(spec.ID, false)
		}
		b.m.LoadSnapshots()
		b.m.refreshStates()
	})
	for _, s := range b.m.sup.Snapshots(spec.ID) {
		if s.Dir == dir {
			return mcpserver.SnapshotInfo{
				SandboxID:   spec.ID,
				SandboxName: spec.Name,
				Name:        s.Name,
				Dir:         s.Dir,
				Size:        s.Size,
				CreatedAt:   s.Time,
			}, nil
		}
	}
	return mcpserver.SnapshotInfo{
		SandboxID:   spec.ID,
		SandboxName: spec.Name,
		Name:        filepath.Base(dir),
		Dir:         dir,
		CreatedAt:   time.Now(),
	}, nil
}

func (b *modelMCPBackend) ListSnapshots(_ context.Context, target string) ([]mcpserver.SnapshotInfo, error) {
	snap := b.m.snapshotMCP()
	var specs []sandbox.Spec
	if strings.TrimSpace(target) != "" {
		spec, err := resolveSandboxSpec(snap.specs, target)
		if err != nil {
			return nil, err
		}
		specs = []sandbox.Spec{spec}
	} else {
		specs = snap.specs
	}
	out := make([]mcpserver.SnapshotInfo, 0)
	for _, spec := range specs {
		for _, s := range b.m.sup.Snapshots(spec.ID) {
			out = append(out, mcpserver.SnapshotInfo{
				SandboxID:   spec.ID,
				SandboxName: spec.Name,
				Name:        s.Name,
				Dir:         s.Dir,
				Size:        s.Size,
				CreatedAt:   s.Time,
			})
		}
	}
	slices.SortFunc(out, func(a, b mcpserver.SnapshotInfo) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return out, nil
}

func (b *modelMCPBackend) RestoreSnapshot(ctx context.Context, target string, snapshot string) (mcpserver.SandboxInfo, error) {
	snap := b.m.snapshotMCP()
	spec, err := resolveSandboxSpec(snap.specs, target)
	if err != nil {
		return mcpserver.SandboxInfo{}, err
	}
	var found *sandbox.Snapshot
	for _, s := range b.m.sup.Snapshots(spec.ID) {
		if s.Name == snapshot || s.Dir == snapshot {
			sc := s
			found = &sc
			break
		}
	}
	if found == nil {
		return mcpserver.SandboxInfo{}, fmt.Errorf("snapshot %q not found for sandbox %q", snapshot, spec.Name)
	}
	var info mcpserver.SandboxInfo
	err = b.m.callUI(ctx, func() error {
		curSpec, ok := b.m.spec(spec.ID)
		if !ok {
			return fmt.Errorf("sandbox %q was deleted", spec.Name)
		}
		if b.m.sup.State(curSpec.ID).Status.Active() {
			return fmt.Errorf("stop %s before restoring a snapshot", curSpec.Name)
		}
		if err := b.m.sup.Restore(curSpec, found.Dir); err != nil {
			return err
		}
		b.m.setResume(curSpec.ID, true)
		b.m.refreshStates()
		b.m.refreshMCPSnapshot()
		snapNow := b.m.snapshotMCP()
		info = sandboxInfoFrom(curSpec, b.sandboxState(snapNow, curSpec.ID), snapNow)
		return nil
	})
	return info, err
}

func (b *modelMCPBackend) ListContainers(ctx context.Context, all bool) ([]mcpserver.ContainerInfo, error) {
	snap := b.m.snapshotMCP()
	var items []dockerapi.Container
	if snap.containersOpen && !snap.offline && snap.dockerHost != "" {
		c, err := dockerapi.New(snap.dockerHost)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		items, err = c.Containers(ctx, all)
		if err != nil {
			return nil, err
		}
	} else {
		for _, ctr := range snap.containers {
			if !all && !strings.EqualFold(ctr.State, "running") {
				continue
			}
			items = append(items, ctr)
		}
	}
	out := make([]mcpserver.ContainerInfo, 0, len(items))
	for _, c := range items {
		var ports []string
		seen := make(map[string]bool)
		for _, p := range c.Ports {
			proto := p.Type
			if proto == "" {
				proto = "tcp"
			}
			var s string
			if p.PublicPort > 0 {
				s = fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, proto)
			} else if p.PrivatePort > 0 {
				s = fmt.Sprintf("%d/%s", p.PrivatePort, proto)
			}
			if s != "" && !seen[s] {
				seen[s] = true
				ports = append(ports, s)
			}
		}
		name := c.Name()
		out = append(out, mcpserver.ContainerInfo{
			ID:       c.ID,
			ShortID:  c.ShortID(),
			Name:     name,
			Image:    c.Image,
			State:    c.State,
			Status:   c.Status,
			Ports:    ports,
			Project:  c.Project(),
			Service:  c.Service(),
			LocalURL: localProxyHTTPSURLFor(snap.proxyRunning, snap.proxyHTTPSPort, snap.proxyRoutes, "container", name),
		})
	}
	return out, nil
}

func (b *modelMCPBackend) ContainerLogs(ctx context.Context, target string, tail int) (string, error) {
	snap := b.m.snapshotMCP()
	if !snap.containersOpen || snap.dockerHost == "" {
		return "", errors.New("docker engine is not running")
	}
	c, err := dockerapi.New(snap.dockerHost)
	if err != nil {
		return "", err
	}
	defer c.Close()

	resolvedID := target
	if ctrs, err := c.Containers(ctx, true); err == nil {
		found := false
		for _, ctr := range ctrs {
			if ctr.ID == target || ctr.ShortID() == target || ctr.Name() == target {
				resolvedID = ctr.ID
				found = true
				break
			}
		}
		if !found {
			for _, ctr := range ctrs {
				if strings.EqualFold(ctr.Name(), target) || (len(target) >= 4 && strings.HasPrefix(ctr.ID, target)) {
					resolvedID = ctr.ID
					found = true
					break
				}
			}
		}
		if !found {
			return "", fmt.Errorf("container %q not found", target)
		}
	}

	if tail <= 0 {
		tail = mcpserver.DefaultLogTail
	}
	lines, err := c.Logs(ctx, resolvedID, tail)
	if err != nil {
		return "", err
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	return strings.Join(texts, "\n"), nil
}

func (b *modelMCPBackend) ListImages(ctx context.Context, source string) ([]mcpserver.ImageInfo, error) {
	snap := b.m.snapshotMCP()
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "all"
	}
	out := make([]mcpserver.ImageInfo, 0)

	if source == "all" || source == "cached" {
		var imgs []rungv.Image
		client := b.m.Client()
		if !snap.offline && client.Path != "" {
			if live, err := client.Images(ctx); err == nil {
				imgs = live
			} else {
				imgs = snap.cachedImages
			}
		} else {
			imgs = snap.cachedImages
		}
		for _, img := range imgs {
			out = append(out, mcpserver.ImageInfo{
				Source:     "cached",
				Repository: img.Repository,
				Tag:        img.Tag,
				ID:         img.ID,
				Digest:     img.Digest,
				SizeText:   formatSize(img.Size),
			})
		}
	}

	if source == "all" || source == "registry" {
		if srv := b.m.ensureLocalRegServer(); srv != nil {
			if entries, err := srv.List(); err == nil {
				for _, e := range entries {
					out = append(out, mcpserver.ImageInfo{
						Source:     "registry",
						Repository: e.Repository,
						Tag:        e.Tag,
						Digest:     e.Digest,
						Size:       e.Size,
						SizeText:   formatBytes(e.Size),
						Platforms:  slices.Clone(e.Platforms),
					})
				}
			}
		}
	}

	return out, nil
}

func (b *modelMCPBackend) ListRoutes(_ context.Context) ([]mcpserver.RouteInfo, error) {
	b.m.localProxyMu.Lock()
	srv := b.m.localProxy
	b.m.localProxyMu.Unlock()

	if srv != nil && srv.Running() {
		routes := srv.Routes()
		out := make([]mcpserver.RouteInfo, 0, len(routes))
		for _, rt := range routes {
			out = append(out, mcpserver.RouteInfo{
				Host:     rt.Host,
				Target:   rt.Target,
				Kind:     rt.Kind,
				Name:     rt.Name,
				HTTPURL:  srv.HTTPURL(rt.Host),
				HTTPSURL: srv.HTTPSURL(rt.Host),
			})
		}
		return out, nil
	}

	snap := b.m.snapshotMCP()
	if !snap.proxyRunning {
		return []mcpserver.RouteInfo{}, nil
	}
	out := make([]mcpserver.RouteInfo, 0, len(snap.proxyRoutes))
	for _, rt := range snap.proxyRoutes {
		out = append(out, mcpserver.RouteInfo{
			Host:     rt.Host,
			Target:   rt.Target,
			Kind:     rt.Kind,
			Name:     rt.Name,
			HTTPURL:  localproxy.FormatURL("http", rt.Host, snap.proxyHTTPPort),
			HTTPSURL: localproxy.FormatURL("https", rt.Host, snap.proxyHTTPSPort),
		})
	}
	return out, nil
}
