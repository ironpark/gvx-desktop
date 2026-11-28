package engine

// Owner: frozen (architect). The routing table names the handler of every
// endpoint; the handlers live in their owners' files. Ask the coordinator
// to add a route.

import (
	"net/http"
	"path"
	"strings"
)

type handlerFunc func(e *Engine, w http.ResponseWriter, r *http.Request)

// route is one endpoint. In a pattern, {x} matches one path segment and
// {x...} one or more, as an image name with slashes; handlers read them
// with r.PathValue.
type route struct {
	method  string
	pattern string
	handle  handlerFunc
}

// unsupported answers 501 for what the engine does not do.
func unsupported(what string) handlerFunc {
	return func(e *Engine, w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotSupported(what))
	}
}

// unsupportedBuildDriver answers 501 directing buildx clients to the
// engine-managed gvx-desktop remote builder.
func unsupportedBuildDriver() handlerFunc {
	return func(e *Engine, w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errBuildDriverNotSupported())
	}
}

// routes are tried in order: a fixed path before a pattern that could
// match it, as /images/json before /images/{name...}/json.
var routes = []route{
	// System: workstream C (system.go, events.go).
	{"GET", "/_ping", (*Engine).ping},
	{"HEAD", "/_ping", (*Engine).ping},
	{"GET", "/version", (*Engine).version},
	{"GET", "/info", (*Engine).info},
	{"GET", "/events", (*Engine).events},
	{"GET", "/system/df", (*Engine).systemDF},
	{"POST", "/auth", unsupported("Logging in through the engine (use rungv login)")},

	// Containers: workstream B (containers.go), streams: workstream A (attach.go).
	{"GET", "/containers/json", (*Engine).containerList},
	{"POST", "/containers/create", (*Engine).containerCreate},
	{"POST", "/containers/prune", (*Engine).containerPrune},
	{"GET", "/containers/{id}/json", (*Engine).containerInspect},
	{"POST", "/containers/{id}/start", (*Engine).containerStart},
	{"POST", "/containers/{id}/stop", (*Engine).containerStop},
	{"POST", "/containers/{id}/restart", (*Engine).containerRestart},
	{"POST", "/containers/{id}/kill", (*Engine).containerKill},
	{"POST", "/containers/{id}/rename", (*Engine).containerRename},
	{"DELETE", "/containers/{id}", (*Engine).containerDelete},
	{"POST", "/containers/{id}/attach", (*Engine).containerAttach},
	{"POST", "/containers/{id}/wait", (*Engine).containerWait},
	{"GET", "/containers/{id}/logs", (*Engine).containerLogs},
	{"POST", "/containers/{id}/resize", (*Engine).containerResize},
	{"GET", "/containers/{id}/stats", (*Engine).containerStats},
	{"GET", "/containers/{id}/top", (*Engine).containerTop},
	{"GET", "/containers/{id}/changes", (*Engine).containerChanges},
	{"GET", "/containers/{id}/export", (*Engine).containerExport},
	{"POST", "/containers/{id}/update", (*Engine).containerUpdate},
	{"POST", "/containers/{id}/pause", (*Engine).containerPause},
	{"POST", "/containers/{id}/unpause", (*Engine).containerUnpause},
	{"GET", "/containers/{id}/attach/ws", unsupported("Attaching over a WebSocket")},
	{"HEAD", "/containers/{id}/archive", (*Engine).containerArchiveHead},
	{"GET", "/containers/{id}/archive", (*Engine).containerArchiveGet},
	{"PUT", "/containers/{id}/archive", (*Engine).containerArchivePut},
	{"POST", "/containers/{id}/exec", (*Engine).execCreate},
	{"POST", "/exec/{id}/start", (*Engine).execStart},
	{"POST", "/exec/{id}/resize", (*Engine).execResize},
	{"GET", "/exec/{id}/json", (*Engine).execInspect},
	{"POST", "/commit", (*Engine).commit},

	// Images: workstream C (images.go).
	{"GET", "/images/json", (*Engine).imageList},
	{"POST", "/images/create", (*Engine).imageCreate},
	{"POST", "/images/load", (*Engine).imageLoad},
	{"GET", "/images/get", (*Engine).imageGet},
	{"POST", "/images/prune", (*Engine).imagePrune},
	{"GET", "/images/search", (*Engine).imageSearch},
	{"GET", "/images/{name...}/json", (*Engine).imageInspect},
	{"GET", "/images/{name...}/get", (*Engine).imageGet},
	{"POST", "/images/{name...}/tag", (*Engine).imageTag},
	{"GET", "/images/{name...}/history", (*Engine).imageHistory},
	{"POST", "/images/{name...}/push", (*Engine).imagePush},
	{"DELETE", "/images/{name...}", (*Engine).imageDelete},
	{"GET", "/distribution/{name...}/json", unsupported("Reading a registry's manifest")},
	{"POST", "/build", (*Engine).imageBuild},
	{"POST", "/build/prune", unsupportedBuildDriver()},
	{"POST", "/build/cancel", unsupportedBuildDriver()},
	{"POST", "/session", unsupportedBuildDriver()},
	{"POST", "/grpc", unsupportedBuildDriver()},

	// Volumes: workstream C (volumes.go).
	{"GET", "/volumes", (*Engine).volumeList},
	{"POST", "/volumes/create", (*Engine).volumeCreate},
	{"POST", "/volumes/prune", (*Engine).volumePrune},
	{"GET", "/volumes/{name}", (*Engine).volumeInspect},
	{"PUT", "/volumes/{name}", unsupported("Updating a cluster volume")},
	{"DELETE", "/volumes/{name}", (*Engine).volumeDelete},

	// Networks: workstream C (networks.go).
	{"GET", "/networks", (*Engine).networkList},
	{"POST", "/networks/create", (*Engine).networkCreate},
	{"POST", "/networks/prune", (*Engine).networkPrune},
	{"GET", "/networks/{id}", (*Engine).networkInspect},
	{"DELETE", "/networks/{id}", (*Engine).networkDelete},
	{"POST", "/networks/{id}/connect", (*Engine).networkConnect},
	{"POST", "/networks/{id}/disconnect", (*Engine).networkDisconnect},
}

// unsupportedPrefixes answer 501 for every endpoint under them.
var unsupportedPrefixes = []struct{ prefix, what string }{
	{"/swarm", "Swarm"},
	{"/nodes", "Swarm"},
	{"/services", "Swarm services"},
	{"/tasks", "Swarm services"},
	{"/secrets", "Secrets"},
	{"/configs", "Configs"},
	{"/plugins", "Plugins"},
}

// compiled routes, split into segments once.
var compiled = func() [][]string {
	out := make([][]string, len(routes))
	for i, rt := range routes {
		out[i] = strings.Split(strings.Trim(rt.pattern, "/"), "/")
	}
	return out
}()

func (e *Engine) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p := stripVersion(cleanPath(r.URL.Path))
	e.logf("engine: %s %s", r.Method, r.URL.RequestURI())
	if !e.isOpen() {
		writeError(w, r, &apiError{http.StatusServiceUnavailable, "GVX Desktop's sandbox engine is not running."})
		return
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, rt := range routes {
		if rt.method != r.Method {
			continue
		}
		vars, ok := matchSegments(compiled[i], segs)
		if !ok {
			continue
		}
		for k, v := range vars {
			r.SetPathValue(k, v)
		}
		rt.handle(e, w, r)
		return
	}
	for _, u := range unsupportedPrefixes {
		if p == u.prefix || strings.HasPrefix(p, u.prefix+"/") {
			writeError(w, r, errNotSupported(u.what))
			return
		}
	}
	writeError(w, r, errNotFound("page not found"))
}

// matchSegments matches a path against a pattern, both split at slashes.
// A {x...} segment takes as many path segments as the fixed segments after
// it leave, at least one.
func matchSegments(pattern, segs []string) (map[string]string, bool) {
	var vars map[string]string
	set := func(k, v string) {
		if vars == nil {
			vars = map[string]string{}
		}
		vars[k] = v
	}
	for i, ps := range pattern {
		if name, ok := strings.CutSuffix(strings.TrimPrefix(ps, "{"), "...}"); ok && strings.HasPrefix(ps, "{") {
			rest := pattern[i+1:]
			n := len(segs) - i - len(rest)
			if n < 1 {
				return nil, false
			}
			set(name, strings.Join(segs[i:i+n], "/"))
			tail, ok := matchSegments(rest, segs[i+n:])
			if !ok {
				return nil, false
			}
			for k, v := range tail {
				set(k, v)
			}
			return vars, true
		}
		if i >= len(segs) {
			return nil, false
		}
		if strings.HasPrefix(ps, "{") && strings.HasSuffix(ps, "}") {
			if segs[i] == "" {
				return nil, false
			}
			set(ps[1:len(ps)-1], segs[i])
			continue
		}
		if ps != segs[i] {
			return nil, false
		}
	}
	if len(segs) != len(pattern) {
		return nil, false
	}
	return vars, true
}

// cleanPath normalizes a request path for matching.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if !strings.HasPrefix(c, "/") {
		return "/" + c
	}
	return c
}

// stripVersion removes a leading API version, as /v1.52, from a path.
func stripVersion(p string) string {
	if v := pathVersion(p); v != "" {
		if rest := p[len("/v")+len(v):]; rest != "" {
			return rest
		}
		return "/"
	}
	return p
}

// pathVersion is the API version a path starts with, as 1.52, or "".
func pathVersion(p string) string {
	rest, ok := strings.CutPrefix(p, "/v")
	if !ok {
		return ""
	}
	ver, _, _ := strings.Cut(rest, "/")
	major, minor, ok := strings.Cut(ver, ".")
	if !ok || !digits(major) || !digits(minor) {
		return ""
	}
	return ver
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// requestVersion is the API version the client asked for in the path, or
// APIVersion without one. Some answers depend on it, as the content type
// of an attach (multiplexed-stream from 1.42).
func requestVersion(r *http.Request) string {
	if v := pathVersion(cleanPath(r.URL.Path)); v != "" {
		return v
	}
	return APIVersion
}

// versionAtLeast compares API versions, as 1.42 <= 1.52.
func versionAtLeast(v, min string) bool {
	vMaj, vMin := splitVersion(v)
	mMaj, mMin := splitVersion(min)
	return vMaj > mMaj || vMaj == mMaj && vMin >= mMin
}

func splitVersion(v string) (int, int) {
	a, b, _ := strings.Cut(v, ".")
	return atoi(a), atoi(b)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
