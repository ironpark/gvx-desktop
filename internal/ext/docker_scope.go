package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

const maxDockerCreateBodyBytes = 4 << 20

// scopedDockerProxy serves a per-extension Docker Engine API proxy.
//
// Unlike dockerproxy.Proxy.SetLocal (which bypasses Check/Rewrite), scopedDockerProxy
// runs its container-create inspection and rewriting in an outer http.Handler so
// BOTH DockerEngineHandler and DockerBackendDial enforce io.gvx.extension label
// isolation and allowed_folders bind-mount scoping.
type scopedDockerProxy struct {
	extID         string
	dataDir       string
	grants        *GrantStore
	manifestPerms []string
	hasNetwork    func() bool
	engineHandler func() http.Handler
	hasDial       bool
	dp            *dockerproxy.Proxy
	policyMu      *sync.RWMutex

	ownedMu         sync.Mutex
	ownedContainers map[string]struct{}
	ownedVolumes    map[string]struct{}
	ownedNetworks   map[string]struct{}
	ownedImages     map[string]struct{}

	mu        sync.Mutex
	closed    bool
	ln        net.Listener
	srv       *http.Server
	sockPath  string
	closeCtx  context.Context
	closeStop context.CancelFunc
}

func newScopedDockerProxy(
	extID, dataDir string,
	grants *GrantStore,
	manifestPerms []string,
	hasNetwork func() bool,
	dial func(context.Context) (net.Conn, error),
	engineHandler func() http.Handler,
) *scopedDockerProxy {
	return newScopedDockerProxyWithPolicyMu(extID, dataDir, grants, manifestPerms, hasNetwork, dial, engineHandler, nil)
}

func newScopedDockerProxyWithPolicyMu(
	extID, dataDir string,
	grants *GrantStore,
	manifestPerms []string,
	hasNetwork func() bool,
	dial func(context.Context) (net.Conn, error),
	engineHandler func() http.Handler,
	policyMu *sync.RWMutex,
) *scopedDockerProxy {
	hasDial := dial != nil
	safeDial := dial
	if safeDial == nil {
		safeDial = func(context.Context) (net.Conn, error) {
			return nil, errors.New("docker backend is not running")
		}
	}
	if policyMu == nil {
		policyMu = &sync.RWMutex{}
	}
	closeCtx, closeStop := context.WithCancel(context.Background())
	dp := dockerproxy.New(safeDial)
	return &scopedDockerProxy{
		extID:           extID,
		dataDir:         dataDir,
		grants:          grants,
		manifestPerms:   append([]string(nil), manifestPerms...),
		hasNetwork:      hasNetwork,
		engineHandler:   engineHandler,
		hasDial:         hasDial,
		dp:              dp,
		policyMu:        policyMu,
		ownedContainers: make(map[string]struct{}),
		ownedVolumes:    make(map[string]struct{}),
		ownedNetworks:   make(map[string]struct{}),
		ownedImages:     make(map[string]struct{}),
		closeCtx:        closeCtx,
		closeStop:       closeStop,
	}
}

func (s *scopedDockerProxy) policyRLock() {
	if s.policyMu != nil {
		s.policyMu.RLock()
	}
}

func (s *scopedDockerProxy) policyRUnlock() {
	if s.policyMu != nil {
		s.policyMu.RUnlock()
	}
}

func (s *scopedDockerProxy) hasNetworkPermission() bool {
	if s.hasNetwork != nil {
		return s.hasNetwork()
	}
	return s.grants != nil && s.grants.HasPermission(s.extID, PermNetwork, s.manifestPerms)
}

func (s *scopedDockerProxy) Listen() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", net.ErrClosed
	}
	if s.ln != nil {
		if s.sockPath != "" {
			return "unix://" + s.sockPath, nil
		}
		return "http://" + s.ln.Addr().String(), nil
	}

	var (
		ln       net.Listener
		sockPath string
		endpoint string
		err      error
	)
	if runtime.GOOS != "windows" {
		sockPath, err = NewPaths(s.dataDir).SocketPath("d", s.extID)
		if err != nil {
			return "", err
		}
		ln, err = listenPrivateUnixSocket(sockPath)
		if err != nil {
			return "", err
		}
		endpoint = "unix://" + sockPath
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		endpoint = "http://" + ln.Addr().String()
	}

	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			if s.closeCtx != nil {
				return s.closeCtx
			}
			return context.Background()
		},
	}
	s.ln = ln
	s.srv = srv
	s.sockPath = sockPath
	go func() {
		_ = srv.Serve(ln)
	}()
	return endpoint, nil
}

func (s *scopedDockerProxy) SocketPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sockPath
}

func (s *scopedDockerProxy) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *scopedDockerProxy) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.srv
	ln := s.ln
	sockPath := s.sockPath
	closeStop := s.closeStop
	s.srv = nil
	s.ln = nil
	s.sockPath = ""
	s.mu.Unlock()

	if closeStop != nil {
		closeStop()
	}
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
		}
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	removeSocketIfSocket(sockPath)
	return s.dp.Close()
}

func (s *scopedDockerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.grants == nil || !s.grants.HasPermission(s.extID, PermDocker, s.manifestPerms) {
		writeDockerJSONError(w, http.StatusForbidden, "docker permission denied for extension "+s.extID)
		return
	}

	rawEscaped := r.URL.EscapedPath()
	if strings.Contains(rawEscaped, "%2f") || strings.Contains(rawEscaped, "%2F") || strings.Contains(rawEscaped, "%5c") || strings.Contains(rawEscaped, "%5C") {
		writeDockerJSONError(w, http.StatusBadRequest, "encoded path separators are not allowed in Docker API paths")
		return
	}

	stripped := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))

	// Block global destructive prune endpoints.
	if stripped == "/containers/prune" || stripped == "/images/prune" || stripped == "/volumes/prune" || stripped == "/networks/prune" {
		writeDockerJSONError(w, http.StatusForbidden, "global prune endpoint "+stripped+" is forbidden for extension "+s.extID)
		return
	}

	// Block BuildKit / legacy build / session / gRPC / system df & prune / unsupported cluster endpoints.
	if stripped == "/build" || strings.HasPrefix(stripped, "/build/") ||
		stripped == "/session" || strings.HasPrefix(stripped, "/session/") ||
		stripped == "/grpc" || strings.HasPrefix(stripped, "/grpc/") ||
		stripped == "/system" || strings.HasPrefix(stripped, "/system/") ||
		stripped == "/swarm" || strings.HasPrefix(stripped, "/swarm/") ||
		stripped == "/nodes" || strings.HasPrefix(stripped, "/nodes/") ||
		stripped == "/services" || strings.HasPrefix(stripped, "/services/") ||
		stripped == "/tasks" || strings.HasPrefix(stripped, "/tasks/") ||
		stripped == "/secrets" || strings.HasPrefix(stripped, "/secrets/") ||
		stripped == "/configs" || strings.HasPrefix(stripped, "/configs/") ||
		stripped == "/plugins" || strings.HasPrefix(stripped, "/plugins/") {
		writeDockerJSONError(w, http.StatusForbidden, "endpoint "+stripped+" is forbidden for extension "+s.extID)
		return
	}

	switch {
	case r.Method == http.MethodGet && stripped == "/events":
		s.serveFilteredEvents(w, r)
		return

	case r.Method == http.MethodPost && stripped == "/containers/create":
		if isDockerUpgradeRequest(r) {
			writeDockerJSONError(w, http.StatusBadRequest, "upgrade is not supported on /containers/create")
			return
		}
		s.policyRLock()
		defer s.policyRUnlock()
		if s.grants == nil || !s.grants.HasPermission(s.extID, PermDocker, s.manifestPerms) {
			writeDockerJSONError(w, http.StatusForbidden, "docker permission denied for extension "+s.extID)
			return
		}
		nextReq, status, err := s.inspectAndRewriteCreate(r)
		if err != nil {
			writeDockerJSONError(w, status, err.Error())
			return
		}
		s.serveCreateAndRecordContainer(w, nextReq)
		return

	case r.Method == http.MethodGet && stripped == "/containers/json":
		s.serveFilteredContainerList(w, r)
		return

	case strings.HasPrefix(stripped, "/containers/"):
		rest := strings.TrimPrefix(stripped, "/containers/")
		cid, sub, _ := strings.Cut(rest, "/")
		if cid == "" || cid == "." || cid == ".." {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid container identifier")
			return
		}
		if r.Method == http.MethodPost && sub == "exec" {
			if isDockerUpgradeRequest(r) {
				writeDockerJSONError(w, http.StatusBadRequest, "upgrade is not supported on /containers/exec")
				return
			}
			s.policyRLock()
			defer s.policyRUnlock()
			if s.grants == nil || !s.grants.HasPermission(s.extID, PermDocker, s.manifestPerms) {
				writeDockerJSONError(w, http.StatusForbidden, "docker permission denied for extension "+s.extID)
				return
			}
			if status, msg := s.verifyContainerOwnership(r.Context(), cid); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
			nextReq, status, err := inspectAndRewriteExecCreate(r)
			if err != nil {
				writeDockerJSONError(w, status, err.Error())
				return
			}
			s.dispatchBackend(w, nextReq)
			return
		}
		if r.Method == http.MethodPost && (sub == "start" || sub == "restart" || sub == "unpause") {
			if isDockerUpgradeRequest(r) {
				writeDockerJSONError(w, http.StatusBadRequest, "upgrade is not supported on /containers/"+sub)
				return
			}
			s.policyRLock()
			defer s.policyRUnlock()
			if s.grants == nil || !s.grants.HasPermission(s.extID, PermDocker, s.manifestPerms) {
				writeDockerJSONError(w, http.StatusForbidden, "docker permission denied for extension "+s.extID)
				return
			}
			if status, msg := s.verifyContainerOwnershipAndMounts(r.Context(), cid); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
			s.dispatchBackend(w, r)
			return
		} else if status, msg := s.verifyContainerOwnership(r.Context(), cid); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}

	case strings.HasPrefix(stripped, "/exec/"):
		rest := strings.TrimPrefix(stripped, "/exec/")
		execID, _, _ := strings.Cut(rest, "/")
		if execID == "" || execID == "." || execID == ".." {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid exec identifier")
			return
		}
		if status, msg := s.verifyExecOwnership(r.Context(), execID); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}

	case r.Method == http.MethodPost && stripped == "/commit":
		s.serveScopedCommit(w, r)
		return

	case r.Method == http.MethodPost && stripped == "/volumes/create":
		nextReq, status, err := s.inspectVolumeCreate(r)
		if err != nil {
			writeDockerJSONError(w, status, err.Error())
			return
		}
		s.serveCreateAndRecordVolume(w, nextReq)
		return

	case r.Method == http.MethodGet && stripped == "/volumes":
		s.serveFilteredVolumeList(w, r)
		return

	case strings.HasPrefix(stripped, "/volumes/"):
		volName := strings.TrimPrefix(stripped, "/volumes/")
		if volName == "" || volName == "." || volName == ".." || strings.Contains(volName, "/") {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid volume identifier")
			return
		}
		if status, msg := s.verifyVolumeOwnership(r.Context(), volName); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}

	case r.Method == http.MethodPost && stripped == "/networks/create":
		nextReq, status, err := s.inspectNetworkCreate(r)
		if err != nil {
			writeDockerJSONError(w, status, err.Error())
			return
		}
		s.serveCreateAndRecordNetwork(w, nextReq)
		return

	case r.Method == http.MethodGet && stripped == "/networks":
		s.serveFilteredNetworkList(w, r)
		return

	case strings.HasPrefix(stripped, "/networks/"):
		rest := strings.TrimPrefix(stripped, "/networks/")
		netID, action, hasAction := strings.Cut(rest, "/")
		if netID == "" || netID == "." || netID == ".." {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid network identifier")
			return
		}
		if hasAction {
			if r.Method == http.MethodPost && (action == "connect" || action == "disconnect") {
				if isHostNetworkName(netID) {
					writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("host network %q is not allowed for extension %q", netID, s.extID))
					return
				}
				nextReq, status, err := s.inspectNetworkContainerOp(r)
				if err != nil {
					writeDockerJSONError(w, status, err.Error())
					return
				}
				if status, msg := s.verifyNetworkWritableOrBuiltin(r.Context(), netID); status != 0 {
					writeDockerJSONError(w, status, msg)
					return
				}
				r = nextReq
			} else {
				writeDockerJSONError(w, http.StatusForbidden, "network endpoint is forbidden for extension "+s.extID)
				return
			}
		} else {
			switch r.Method {
			case http.MethodGet:
				s.serveScopedNetworkInspect(w, r, netID)
				return
			case http.MethodDelete:
				if status, msg := s.verifyNetworkDeletable(r.Context(), netID); status != 0 {
					writeDockerJSONError(w, status, msg)
					return
				}
			default:
				writeDockerJSONError(w, http.StatusForbidden, "network operation is forbidden for extension "+s.extID)
				return
			}
		}

	case r.Method == http.MethodGet && stripped == "/images/json":
		s.serveScopedImageList(w, r)
		return

	case r.Method == http.MethodPost && stripped == "/images/create":
		s.serveScopedImageCreate(w, r)
		return

	case r.Method == http.MethodPost && stripped == "/images/load":
		writeDockerJSONError(w, http.StatusForbidden, "POST /images/load is forbidden for extension "+s.extID)
		return

	case r.Method == http.MethodGet && stripped == "/images/get":
		s.serveScopedImageGet(w, r)
		return

	case r.Method == http.MethodGet && stripped == "/images/search":
		if !s.hasNetworkPermission() {
			writeDockerJSONError(w, http.StatusForbidden, "network permission denied for extension "+s.extID)
			return
		}

	case strings.HasPrefix(stripped, "/images/"):
		rest := strings.TrimPrefix(stripped, "/images/")
		if rest == "" || rest == "." || rest == ".." {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid image identifier")
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
			return
		}
		if len(q["platform"]) > 1 {
			writeDockerJSONError(w, http.StatusBadRequest, "multiple platform query parameters are not allowed")
			return
		}
		platform := strings.TrimSpace(q.Get("platform"))
		switch {
		case r.Method == http.MethodDelete:
			s.serveScopedImageDelete(w, r, rest)
			return
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/json"):
			imgRef := strings.TrimSuffix(rest, "/json")
			if status, msg := s.verifyImageAccessible(r.Context(), imgRef, platform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/history"):
			imgRef := strings.TrimSuffix(rest, "/history")
			if status, msg := s.verifyImageAccessible(r.Context(), imgRef, platform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/get"):
			imgRef := strings.TrimSuffix(rest, "/get")
			if status, msg := s.verifyImageAccessible(r.Context(), imgRef, platform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/tag"):
			imgRef := strings.TrimSuffix(rest, "/tag")
			status, resolvedPlatform, msg := s.verifyImageAccessibleWithPlatform(r.Context(), imgRef, platform)
			if status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
			if platform != "" && resolvedPlatform != "" && dockerPlatformsConflict(platform, resolvedPlatform) {
				writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("source image %q platform %q conflicts with requested platform %q", imgRef, resolvedPlatform, platform))
				return
			}
			effectivePlatform := resolvedPlatform
			if effectivePlatform == "" {
				effectivePlatform = platform
			}
			if status, msg := s.verifyImageTagAllowed(r.Context(), r, effectivePlatform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/push"):
			if !s.hasNetworkPermission() {
				writeDockerJSONError(w, http.StatusForbidden, "network permission denied for extension "+s.extID)
				return
			}
			imgRef := strings.TrimSuffix(rest, "/push")
			tag := strings.TrimSpace(q.Get("tag"))
			targetRef := formatImagePullTarget(imgRef, tag)
			if tag == "" {
				targetRef = imgRef
			}
			if status, msg := s.verifyImageAccessible(r.Context(), targetRef, platform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		default:
			writeDockerJSONError(w, http.StatusForbidden, "image operation is forbidden for extension "+s.extID)
			return
		}
	}

	s.dispatchBackend(w, r)
}

func (s *scopedDockerProxy) dispatchBackend(w http.ResponseWriter, r *http.Request) {
	if s.engineHandler != nil {
		if h := s.engineHandler(); h != nil {
			h.ServeHTTP(w, r)
			return
		}
	}
	if !s.hasDial {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	s.dp.ServeHTTP(w, r)
}

func isDockerUpgradeRequest(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, val := range r.Header.Values("Connection") {
		for tok := range strings.SplitSeq(val, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

type dockerMemResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newDockerMemResponseWriter() *dockerMemResponseWriter {
	return &dockerMemResponseWriter{header: make(http.Header)}
}

func (w *dockerMemResponseWriter) Header() http.Header { return w.header }

func (w *dockerMemResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *dockerMemResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (s *scopedDockerProxy) backendDispatch(r *http.Request) (int, http.Header, []byte) {
	if s.engineHandler != nil {
		if h := s.engineHandler(); h != nil {
			rec := newDockerMemResponseWriter()
			h.ServeHTTP(rec, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			return status, rec.header, rec.body.Bytes()
		}
	}
	if !s.hasDial {
		return http.StatusServiceUnavailable, nil, nil
	}
	rec := newDockerMemResponseWriter()
	s.dp.ServeHTTP(rec, r)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, rec.header, rec.body.Bytes()
}

func (s *scopedDockerProxy) backendGet(ctx context.Context, pathWithQuery string) (int, []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+pathWithQuery, http.NoBody)
	if err != nil {
		return http.StatusBadRequest, nil
	}
	status, _, body := s.backendDispatch(req)
	return status, body
}

func (s *scopedDockerProxy) markOwnedContainer(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.ownedMu.Lock()
	if s.ownedContainers == nil {
		s.ownedContainers = make(map[string]struct{})
	}
	s.ownedContainers[id] = struct{}{}
	s.ownedMu.Unlock()
}

func (s *scopedDockerProxy) isMarkedOwnedContainer(id string) bool {
	s.ownedMu.Lock()
	_, ok := s.ownedContainers[strings.TrimSpace(id)]
	s.ownedMu.Unlock()
	return ok
}

func (s *scopedDockerProxy) markOwnedVolume(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	s.ownedMu.Lock()
	if s.ownedVolumes == nil {
		s.ownedVolumes = make(map[string]struct{})
	}
	s.ownedVolumes[name] = struct{}{}
	s.ownedMu.Unlock()
}

func (s *scopedDockerProxy) consumeOwnedVolume(name string) bool {
	name = strings.TrimSpace(name)
	s.ownedMu.Lock()
	_, ok := s.ownedVolumes[name]
	if ok {
		delete(s.ownedVolumes, name)
	}
	s.ownedMu.Unlock()
	return ok
}

func (s *scopedDockerProxy) markOwnedNetwork(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.ownedMu.Lock()
	if s.ownedNetworks == nil {
		s.ownedNetworks = make(map[string]struct{})
	}
	s.ownedNetworks[id] = struct{}{}
	s.ownedMu.Unlock()
}

func (s *scopedDockerProxy) consumeOwnedNetwork(id string) bool {
	id = strings.TrimSpace(id)
	s.ownedMu.Lock()
	_, ok := s.ownedNetworks[id]
	if ok {
		delete(s.ownedNetworks, id)
	}
	s.ownedMu.Unlock()
	return ok
}

func (s *scopedDockerProxy) markOwnedImage(ref string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	s.ownedMu.Lock()
	if s.ownedImages == nil {
		s.ownedImages = make(map[string]struct{})
	}
	s.ownedImages[ref] = struct{}{}
	s.ownedMu.Unlock()
}

func (s *scopedDockerProxy) isMarkedOwnedImage(id, name string) bool {
	s.ownedMu.Lock()
	defer s.ownedMu.Unlock()
	if id != "" {
		if _, ok := s.ownedImages[id]; ok {
			return true
		}
	}
	if name != "" {
		if _, ok := s.ownedImages[name]; ok {
			return true
		}
	}
	return false
}

func parseDockerFiltersJSON(raw string) (map[string][]string, error) {
	f := make(map[string][]string)
	if strings.TrimSpace(raw) == "" {
		return f, nil
	}
	var m map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &m); err == nil && m != nil {
		for k, vals := range m {
			for v, on := range vals {
				if on {
					f[k] = append(f[k], v)
				}
			}
		}
		return f, nil
	}
	var l map[string][]string
	if err := json.Unmarshal([]byte(raw), &l); err != nil || l == nil {
		return nil, errors.New("invalid filters query parameter")
	}
	for k, vals := range l {
		f[k] = append(f[k], vals...)
	}
	return f, nil
}

func writeBackendRawResponse(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for k, vals := range hdr {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *scopedDockerProxy) serveCreateAndRecordContainer(w http.ResponseWriter, r *http.Request) {
	status, hdr, body := s.backendDispatch(r)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status >= 200 && status < 300 {
		var resp struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.ID != "" {
			s.markOwnedContainer(resp.ID)
		}
	}
	writeBackendRawResponse(w, status, hdr, body)
}

func (s *scopedDockerProxy) serveCreateAndRecordVolume(w http.ResponseWriter, r *http.Request) {
	status, hdr, body := s.backendDispatch(r)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status >= 200 && status < 300 {
		var resp struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.Name != "" {
			if resp.Labels != nil && resp.Labels["io.gvx.extension"] != s.extID {
				writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("volume %q is already owned by host or another extension", resp.Name))
				return
			}
			s.markOwnedVolume(resp.Name)
		}
	}
	writeBackendRawResponse(w, status, hdr, body)
}

func (s *scopedDockerProxy) serveCreateAndRecordNetwork(w http.ResponseWriter, r *http.Request) {
	status, hdr, body := s.backendDispatch(r)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status >= 200 && status < 300 {
		var resp struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.ID != "" {
			s.markOwnedNetwork(resp.ID)
		}
	}
	writeBackendRawResponse(w, status, hdr, body)
}

type eventsFilterResponseWriter struct {
	w          http.ResponseWriter
	ctx        context.Context
	proxy      *scopedDockerProxy
	mu         sync.Mutex
	status     int
	hdrWritten bool
	buf        bytes.Buffer
}

func (fw *eventsFilterResponseWriter) Header() http.Header {
	return fw.w.Header()
}

func (fw *eventsFilterResponseWriter) WriteHeader(code int) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.hdrWritten {
		return
	}
	fw.status = code
	fw.hdrWritten = true
	if code == http.StatusOK {
		fw.w.Header().Del("Content-Length")
	}
	fw.w.WriteHeader(code)
}

func (fw *eventsFilterResponseWriter) Flush() {
	fw.mu.Lock()
	if !fw.hdrWritten {
		fw.status = http.StatusOK
		fw.hdrWritten = true
		fw.w.Header().Del("Content-Length")
		fw.w.WriteHeader(http.StatusOK)
	}
	fw.mu.Unlock()
	_ = http.NewResponseController(fw.w).Flush()
}

func (fw *eventsFilterResponseWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	if !fw.hdrWritten {
		fw.status = http.StatusOK
		fw.hdrWritten = true
		fw.w.Header().Del("Content-Length")
		fw.w.WriteHeader(http.StatusOK)
	}
	status := fw.status
	fw.mu.Unlock()

	if status != http.StatusOK {
		return fw.w.Write(p)
	}

	fw.mu.Lock()
	fw.buf.Write(p)
	var outLines [][]byte
	for {
		b := fw.buf.Bytes()
		idx := bytes.IndexByte(b, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), bytes.TrimSpace(b[:idx])...)
		fw.buf.Next(idx + 1)
		if len(line) == 0 {
			continue
		}
		outLines = append(outLines, line)
	}
	fw.mu.Unlock()

	for _, line := range outLines {
		if !fw.proxy.isEventLineAllowed(fw.ctx, line) {
			continue
		}
		if _, err := fw.w.Write(append(line, '\n')); err != nil {
			return 0, err
		}
		_ = http.NewResponseController(fw.w).Flush()
	}
	return len(p), nil
}

func (s *scopedDockerProxy) serveFilteredEvents(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	if len(q["filters"]) > 1 {
		writeDockerJSONError(w, http.StatusBadRequest, "multiple filters query parameters are not allowed")
		return
	}
	if rawFilters := strings.TrimSpace(q.Get("filters")); rawFilters != "" {
		f, err := parseDockerFiltersJSON(rawFilters)
		if err != nil {
			writeDockerJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(f["before"]) > 0 || len(f["since"]) > 0 {
			writeDockerJSONError(w, http.StatusBadRequest, "before/since filters are not supported on /events")
			return
		}
	}

	fw := &eventsFilterResponseWriter{
		w:     w,
		ctx:   r.Context(),
		proxy: s,
	}
	if s.engineHandler != nil {
		if h := s.engineHandler(); h != nil {
			h.ServeHTTP(fw, r)
			return
		}
	}
	if !s.hasDial {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	s.dp.ServeHTTP(fw, r)
}

func (s *scopedDockerProxy) isEventLineAllowed(ctx context.Context, line []byte) bool {
	var ev struct {
		Type   string `json:"Type"`
		Action string `json:"Action"`
		ID     string `json:"id"`
		Actor  struct {
			ID         string            `json:"ID"`
			Attributes map[string]string `json:"Attributes"`
		} `json:"Actor"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return false
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	switch ev.Type {
	case "container":
		if owner, hasOwner := ev.Actor.Attributes["io.gvx.extension"]; hasOwner {
			if owner != s.extID {
				return false
			}
			s.markOwnedContainer(ev.Actor.ID)
			return true
		}
		cid := ev.Actor.ID
		if cid == "" {
			cid = ev.ID
		}
		if cid == "" {
			return false
		}
		if status, _ := s.verifyContainerOwnership(checkCtx, cid); status == 0 {
			return true
		}
		return ev.Action == "destroy" && s.isMarkedOwnedContainer(cid)

	case "network":
		act := strings.ToLower(strings.TrimSpace(ev.Action))
		if act == "connect" || act == "disconnect" || strings.HasPrefix(act, "connect:") || strings.HasPrefix(act, "disconnect:") {
			cid := strings.TrimSpace(ev.Actor.Attributes["container"])
			if cid == "" {
				return false
			}
			if status, _ := s.verifyContainerOwnership(checkCtx, cid); status == 0 {
				return true
			}
			return act == "disconnect" && s.isMarkedOwnedContainer(cid)
		}
		netID := strings.TrimSpace(ev.Actor.ID)
		if netID == "" {
			return false
		}
		if act == "destroy" {
			if status, _ := s.verifyNetworkOwnedCustom(checkCtx, netID); status == 0 {
				s.consumeOwnedNetwork(netID)
				return true
			}
			return s.consumeOwnedNetwork(netID)
		}
		status, _ := s.verifyNetworkOwnedCustom(checkCtx, netID)
		return status == 0

	case "volume":
		volName := strings.TrimSpace(ev.Actor.ID)
		if volName == "" {
			volName = strings.TrimSpace(ev.Actor.Attributes["name"])
		}
		if volName == "" {
			return false
		}
		act := strings.ToLower(strings.TrimSpace(ev.Action))
		if act == "destroy" {
			if status, _ := s.verifyVolumeOwnership(checkCtx, volName); status == 0 {
				s.consumeOwnedVolume(volName)
				return true
			}
			return s.consumeOwnedVolume(volName)
		}
		status, _ := s.verifyVolumeOwnership(checkCtx, volName)
		return status == 0

	case "image":
		if ev.Actor.Attributes["io.gvx.extension"] == s.extID {
			return true
		}
		imgID := strings.TrimSpace(ev.Actor.ID)
		if imgID == "" {
			imgID = strings.TrimSpace(ev.ID)
		}
		imgName := strings.TrimSpace(ev.Actor.Attributes["name"])
		if s.isMarkedOwnedImage(imgID, imgName) {
			return true
		}
		if imgID != "" {
			if status, _ := s.verifyImageOwnership(checkCtx, imgID, ""); status == 0 {
				return true
			}
		}
		return false

	default:
		return false
	}
}

func (s *scopedDockerProxy) serveFilteredContainerList(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	if len(q["filters"]) > 1 {
		writeDockerJSONError(w, http.StatusBadRequest, "multiple filters query parameters are not allowed")
		return
	}
	if rawFilters := strings.TrimSpace(q.Get("filters")); rawFilters != "" {
		f, err := parseDockerFiltersJSON(rawFilters)
		if err != nil {
			writeDockerJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, anchor := range append(slices.Clone(f["before"]), f["since"]...) {
			status, msg := s.verifyContainerOwnership(r.Context(), anchor)
			if status == http.StatusServiceUnavailable {
				writeDockerJSONError(w, http.StatusServiceUnavailable, msg)
				return
			}
			if status != 0 {
				// Uniform 403 for unowned and nonexistent anchor containers prevents existence oracles.
				writeDockerJSONError(w, http.StatusForbidden, "container filter target is not owned by extension "+s.extID)
				return
			}
		}
	}

	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status != http.StatusOK {
		writeBackendRawResponse(w, status, hdr, body)
		return
	}

	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		writeDockerJSONError(w, http.StatusBadGateway, "invalid container list response from docker backend")
		return
	}
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var meta struct {
			ID     string            `json:"Id"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal(item, &meta) == nil && meta.Labels["io.gvx.extension"] == s.extID {
			s.markOwnedContainer(meta.ID)
			filtered = append(filtered, item)
		}
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		writeDockerJSONError(w, http.StatusInternalServerError, "encode filtered container list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

type containerInspectMeta struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Binds  []string `json:"Binds"`
		Mounts []struct {
			Type   string `json:"Type"`
			Source string `json:"Source"`
		} `json:"Mounts"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type   string `json:"Type"`
		Source string `json:"Source"`
	} `json:"Mounts"`
}

func (s *scopedDockerProxy) verifyContainerOwnership(ctx context.Context, containerID string) (int, string) {
	status, _, msg := s.verifyContainerOwnershipWithImage(ctx, containerID)
	return status, msg
}

func (s *scopedDockerProxy) verifyContainerOwnershipWithImage(ctx context.Context, containerID string) (int, string, string) {
	inspect, status, msg := s.inspectOwnedContainer(ctx, containerID)
	if status != 0 {
		return status, "", msg
	}
	imgRef := strings.TrimSpace(inspect.Image)
	if imgRef == "" {
		imgRef = strings.TrimSpace(inspect.Config.Image)
	}
	return 0, imgRef, ""
}

func (s *scopedDockerProxy) verifyContainerOwnershipAndMounts(ctx context.Context, containerID string) (int, string) {
	inspect, status, msg := s.inspectOwnedContainer(ctx, containerID)
	if status != 0 {
		return status, msg
	}
	if err := validateContainerInspectAllowedMounts(inspect, s.extID, s.dataDir, s.grants); err != nil {
		return http.StatusForbidden, err.Error()
	}
	return 0, ""
}

func (s *scopedDockerProxy) inspectOwnedContainer(ctx context.Context, containerID string) (containerInspectMeta, int, string) {
	var empty containerInspectMeta
	if s.engineHandler == nil && !s.hasDial {
		return empty, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	cid := strings.TrimPrefix(strings.TrimSpace(containerID), "/")
	if cid == "" || cid == "." || cid == ".." || strings.ContainsAny(cid, `/\`) || strings.ContainsRune(cid, 0) {
		return empty, http.StatusBadRequest, "invalid container identifier"
	}
	status, body := s.backendGet(ctx, "/containers/"+url.PathEscape(cid)+"/json")
	if status == http.StatusServiceUnavailable {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return empty, http.StatusServiceUnavailable, msg
		}
		return empty, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return empty, http.StatusNotFound, msg
		}
		return empty, http.StatusNotFound, "No such container: " + containerID
	}
	if status != http.StatusOK {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return empty, status, msg
		}
		return empty, http.StatusForbidden, "container access denied for extension " + s.extID
	}
	var inspect containerInspectMeta
	if err := json.Unmarshal(body, &inspect); err != nil {
		return empty, http.StatusForbidden, "container access denied: invalid inspect response"
	}
	if inspect.Config.Labels["io.gvx.extension"] != s.extID {
		return empty, http.StatusForbidden, fmt.Sprintf("container %q is not owned by extension %q", containerID, s.extID)
	}
	s.markOwnedContainer(cid)
	if inspect.ID != "" {
		s.markOwnedContainer(inspect.ID)
	}
	return inspect, 0, ""
}

func validateContainerInspectAllowedMounts(inspect containerInspectMeta, extID, dataDir string, grants *GrantStore) error {
	for _, bStr := range inspect.HostConfig.Binds {
		src, _, isBind, err := parseDockerBindSpec(bStr)
		if err != nil {
			return err
		}
		if isBind {
			if _, err := validateAndMapExtensionBind(src, extID, dataDir, grants); err != nil {
				return err
			}
		}
	}
	for _, m := range inspect.HostConfig.Mounts {
		mt := strings.ToLower(strings.TrimSpace(m.Type))
		src := strings.TrimSpace(m.Source)
		if mt == "bind" || (mt == "" && (strings.ContainsAny(m.Source, `/\`) || strings.HasPrefix(m.Source, ".") || isWindowsDrivePath(m.Source))) {
			if src == "" {
				return errors.New("bind mount requires a Source path")
			}
			if _, err := validateAndMapExtensionBind(m.Source, extID, dataDir, grants); err != nil {
				return err
			}
		}
	}
	for _, m := range inspect.Mounts {
		mt := strings.ToLower(strings.TrimSpace(m.Type))
		src := strings.TrimSpace(m.Source)
		if mt == "bind" || (mt == "" && (strings.ContainsAny(m.Source, `/\`) || strings.HasPrefix(m.Source, ".") || isWindowsDrivePath(m.Source))) {
			if src == "" {
				return errors.New("bind mount requires a Source path")
			}
			if _, err := validateAndMapExtensionBind(m.Source, extID, dataDir, grants); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scopedDockerProxy) verifyExecOwnership(ctx context.Context, execID string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	eid := strings.TrimSpace(execID)
	if eid == "" || eid == "." || eid == ".." || strings.ContainsAny(eid, `/\`) || strings.ContainsRune(eid, 0) {
		return http.StatusBadRequest, "invalid exec identifier"
	}
	status, body := s.backendGet(ctx, "/exec/"+url.PathEscape(eid)+"/json")
	if status == http.StatusServiceUnavailable {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusServiceUnavailable, msg
		}
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, msg
		}
		return http.StatusNotFound, "No such exec instance: " + execID
	}
	if status != http.StatusOK {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return status, msg
		}
		return http.StatusForbidden, "exec access denied for extension " + s.extID
	}
	var inspect struct {
		ContainerID string `json:"ContainerID"`
	}
	if err := json.Unmarshal(body, &inspect); err != nil || strings.TrimSpace(inspect.ContainerID) == "" {
		return http.StatusForbidden, "exec access denied: missing owning container"
	}
	return s.verifyContainerOwnership(ctx, inspect.ContainerID)
}

func isExtensionLabelKey(k string) bool {
	trimmed := strings.TrimSpace(k)
	return strings.EqualFold(trimmed, "io.gvx.extension") ||
		canonicalSimpleFold(trimmed) == canonicalSimpleFold("io.gvx.extension")
}

func unfoldDockerfileInstructions(raw string, allowTrailingContinuation bool) ([]string, int, error) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	normalized = strings.TrimLeft(normalized, "\ufeff")
	lines := strings.Split(normalized, "\n")

	var (
		out         []string
		cur         strings.Builder
		continuing  bool
		seenContent bool
	)
	for _, line := range lines {
		if !continuing {
			if trimmedLeading := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmedLeading, "\ufeff") {
				line = strings.TrimLeft(trimmedLeading, "\ufeff")
			}
		}
		trimmed := strings.TrimSpace(line)
		if !continuing {
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "#") {
				if !seenContent {
					body := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
					if k, v, ok := strings.Cut(body, "="); ok && strings.EqualFold(strings.TrimSpace(k), "escape") {
						if strings.TrimSpace(v) != `\` {
							return nil, http.StatusForbidden, errors.New("custom Dockerfile escape directive is not allowed in changes")
						}
					}
				}
				continue
			}
			seenContent = true
		} else if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			// Inside a line continuation, Dockerfile ignores empty lines and comment lines.
			continue
		}

		noTrailWS := strings.TrimRight(line, " \t")
		bsCount := 0
		for i := len(noTrailWS) - 1; i >= 0 && noTrailWS[i] == '\\'; i-- {
			bsCount++
		}
		if bsCount%2 == 1 {
			chunk := noTrailWS[:len(noTrailWS)-1]
			if continuing {
				cur.WriteString(chunk)
			} else {
				cur.WriteString(strings.TrimLeft(chunk, " \t"))
			}
			continuing = true
			continue
		}

		if continuing {
			cur.WriteString(line)
			out = append(out, cur.String())
			cur.Reset()
			continuing = false
		} else {
			out = append(out, trimmed)
		}
	}
	if continuing {
		if !allowTrailingContinuation {
			return nil, http.StatusBadRequest, errors.New("unterminated line continuation in changes")
		}
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out, 0, nil
}

func validateImageChangeLine(line string) (int, error) {
	line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "\ufeff"))
	if line == "" || strings.HasPrefix(line, "#") {
		return 0, nil
	}
	for {
		word := line
		rest := ""
		if idx := strings.IndexAny(line, " \t"); idx >= 0 {
			word = line[:idx]
			rest = strings.TrimSpace(line[idx+1:])
		}
		if strings.EqualFold(word, "ONBUILD") {
			if rest == "" {
				return http.StatusBadRequest, errors.New("ONBUILD change requires an instruction")
			}
			line = rest
			continue
		}
		if !strings.EqualFold(word, "LABEL") {
			return 0, nil
		}
		words, err := rungv.SplitArgs(rest)
		if err != nil {
			return http.StatusBadRequest, fmt.Errorf("invalid LABEL change %q: %w", line, err)
		}
		if len(words) == 0 {
			return http.StatusBadRequest, errors.New("LABEL change requires at least one argument")
		}
		for _, w := range words {
			if k, _, ok := strings.Cut(w, "="); ok {
				if isExtensionLabelKey(k) {
					return http.StatusForbidden, errors.New("cannot override io.gvx.extension label via changes")
				}
				continue
			}
			if isExtensionLabelKey(w) {
				return http.StatusForbidden, errors.New("cannot override io.gvx.extension label via changes")
			}
		}
		return 0, nil
	}
}

func validateImageChanges(changes []string) (int, error) {
	if len(changes) > 0 {
		joined, status, err := unfoldDockerfileInstructions(strings.Join(changes, "\n"), false)
		if err != nil {
			return status, err
		}
		for _, line := range joined {
			if status, err := validateImageChangeLine(line); err != nil {
				return status, err
			}
		}
	}
	for _, ch := range changes {
		unfolded, status, err := unfoldDockerfileInstructions(ch, true)
		if err != nil {
			return status, err
		}
		for _, line := range unfolded {
			if status, err := validateImageChangeLine(line); err != nil {
				return status, err
			}
		}
		for _, rawLine := range strings.FieldsFunc(ch, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if status, err := validateImageChangeLine(rawLine); err != nil {
				return status, err
			}
		}
	}
	return 0, nil
}

func formatImageRepoTag(repo, tag string) string {
	if repo == "" {
		return ""
	}
	if tag != "" {
		return repo + ":" + tag
	}
	if strings.LastIndex(repo, ":") > strings.LastIndex(repo, "/") {
		return repo
	}
	return repo + ":latest"
}

func (s *scopedDockerProxy) serveScopedCommit(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	vals := q["container"]
	if len(vals) != 1 || strings.TrimSpace(vals[0]) == "" {
		writeDockerJSONError(w, http.StatusBadRequest, "exactly one non-empty container query parameter is required")
		return
	}
	if len(q["repo"]) > 1 || len(q["tag"]) > 1 || len(q["platform"]) > 1 {
		writeDockerJSONError(w, http.StatusBadRequest, "duplicate repo, tag, or platform query parameter is not allowed")
		return
	}
	cid := strings.TrimSpace(vals[0])
	status, containerImgRef, msg := s.verifyContainerOwnershipWithImage(r.Context(), cid)
	if status != 0 {
		writeDockerJSONError(w, status, msg)
		return
	}
	if status, err := validateImageChanges(q["changes"]); err != nil {
		writeDockerJSONError(w, status, err.Error())
		return
	}
	if strings.TrimSpace(q.Get("repo")) != "" {
		commitPlatform := strings.TrimSpace(q.Get("platform"))
		if containerImgRef != "" {
			if _, resolvedPlatform, _ := s.verifyImageAccessibleWithPlatform(r.Context(), containerImgRef, ""); resolvedPlatform != "" {
				commitPlatform = resolvedPlatform
			}
		}
		if status, msg := s.verifyImageTagAllowed(r.Context(), r, commitPlatform); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}
	}

	raw, err := readDockerJSONBody(r)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	nextReq := r
	if len(bytes.TrimSpace(raw)) > 0 {
		payload, err := decodeJSONObjectStrictNoCaseCollisions(raw)
		if err != nil {
			writeDockerJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid commit JSON: %v", err))
			return
		}
		if payload == nil {
			writeDockerJSONError(w, http.StatusBadRequest, "commit JSON must be an object")
			return
		}
		if status, err := enforceExtensionLabelInPayload(payload, s.extID); err != nil {
			writeDockerJSONError(w, status, err.Error())
			return
		}
		out, err := json.Marshal(payload)
		if err != nil {
			writeDockerJSONError(w, http.StatusInternalServerError, "encode commit payload")
			return
		}
		nextReq = cloneRequestWithBody(r, out)
	} else {
		nextReq = cloneRequestWithBody(r, nil)
	}
	q.Add("changes", "LABEL io.gvx.extension="+s.extID)
	nextReq.URL.RawQuery = q.Encode()
	nextReq.RequestURI = ""

	status, hdr, body := s.backendDispatch(nextReq)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status >= 200 && status < 300 {
		var resp struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(body, &resp) == nil && resp.ID != "" {
			s.markOwnedImage(resp.ID)
		}
		if target := formatImageRepoTag(strings.TrimSpace(q.Get("repo")), strings.TrimSpace(q.Get("tag"))); target != "" {
			s.markOwnedImage(target)
		}
	}
	writeBackendRawResponse(w, status, hdr, body)
}

func (s *scopedDockerProxy) serveScopedImageList(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	if len(q["filters"]) > 1 {
		writeDockerJSONError(w, http.StatusBadRequest, "multiple filters query parameters are not allowed")
		return
	}
	hasAnchorFilter := false
	if rawFilters := strings.TrimSpace(q.Get("filters")); rawFilters != "" {
		f, err := parseDockerFiltersJSON(rawFilters)
		if err != nil {
			writeDockerJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		anchors := append(slices.Clone(f["before"]), f["since"]...)
		hasAnchorFilter = len(anchors) > 0
		for _, anchor := range anchors {
			status, msg := s.verifyImageOwnership(r.Context(), anchor, "")
			if status == http.StatusServiceUnavailable {
				writeDockerJSONError(w, http.StatusServiceUnavailable, msg)
				return
			}
			if status != 0 {
				// Uniform 403 for unowned and nonexistent anchor images prevents existence oracles.
				writeDockerJSONError(w, http.StatusForbidden, "image filter target is not owned by extension "+s.extID)
				return
			}
		}
	}

	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if hasAnchorFilter && status == http.StatusNotFound {
		writeDockerJSONError(w, http.StatusForbidden, "image filter target is not owned by extension "+s.extID)
		return
	}
	if status != http.StatusOK {
		writeBackendRawResponse(w, status, hdr, body)
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		writeDockerJSONError(w, http.StatusBadGateway, "invalid image list response from docker backend")
		return
	}
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var meta struct {
			ID     string            `json:"Id"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal(item, &meta) != nil {
			continue
		}
		owner := meta.Labels["io.gvx.extension"]
		if owner != "" && owner != s.extID {
			continue
		}
		if owner == s.extID && meta.ID != "" {
			s.markOwnedImage(meta.ID)
		}
		filtered = append(filtered, item)
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		writeDockerJSONError(w, http.StatusInternalServerError, "encode filtered image list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *scopedDockerProxy) serveScopedImageGet(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	if len(q["platform"]) > 1 {
		writeDockerJSONError(w, http.StatusBadRequest, "multiple platform query parameters are not allowed")
		return
	}
	platform := strings.TrimSpace(q.Get("platform"))
	var refs []string
	for _, n := range q["names"] {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			refs = append(refs, trimmed)
		}
	}
	if len(refs) == 0 {
		writeDockerJSONError(w, http.StatusBadRequest, "at least one image name is required")
		return
	}
	for _, ref := range refs {
		if status, msg := s.verifyImageAccessible(r.Context(), ref, platform); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}
	}
	s.dispatchBackend(w, r)
}

func validateImageCreateRefParam(ref string) bool {
	if ref == "" || ref == "." || ref == ".." || hasDotDotSegment(ref) || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if ref[i] <= 0x20 || ref[i] == 0x7f {
			return false
		}
	}
	return true
}

func formatImagePullTarget(fromImage, tag string) string {
	if fromImage == "" {
		return ""
	}
	if tag != "" {
		if strings.HasPrefix(tag, "sha256:") {
			return fromImage + "@" + tag
		}
		if !strings.Contains(fromImage, "@") && strings.LastIndex(fromImage, ":") <= strings.LastIndex(fromImage, "/") {
			return fromImage + ":" + tag
		}
		return fromImage
	}
	if strings.Contains(fromImage, "@") || strings.LastIndex(fromImage, ":") > strings.LastIndex(fromImage, "/") {
		return fromImage
	}
	return fromImage + ":latest"
}

func (s *scopedDockerProxy) serveScopedImageCreate(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeDockerJSONError(w, http.StatusBadRequest, "invalid query string")
		return
	}
	for _, param := range []string{"fromSrc", "fromImage", "repo", "tag", "platform"} {
		if len(q[param]) > 1 {
			writeDockerJSONError(w, http.StatusBadRequest, fmt.Sprintf("multiple %s query parameters are not allowed", param))
			return
		}
	}
	if status, err := validateImageChanges(q["changes"]); err != nil {
		writeDockerJSONError(w, status, err.Error())
		return
	}
	fromSrc := strings.TrimSpace(q.Get("fromSrc"))
	if fromSrc != "" {
		if fromSrc != "-" {
			writeDockerJSONError(w, http.StatusForbidden, "remote URL fromSrc is forbidden for extension "+s.extID)
			return
		}
		if status, msg := s.verifyImageTagAllowed(r.Context(), r, strings.TrimSpace(q.Get("platform"))); status != 0 {
			writeDockerJSONError(w, status, msg)
			return
		}
		nextReq := r.Clone(r.Context())
		q.Add("changes", "LABEL io.gvx.extension="+s.extID)
		nextReq.URL.RawQuery = q.Encode()
		nextReq.RequestURI = ""

		status, hdr, body := s.backendDispatch(nextReq)
		if status == http.StatusServiceUnavailable && len(body) == 0 {
			writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
			return
		}
		if status >= 200 && status < 300 {
			var resp struct {
				ID     string `json:"Id"`
				Status string `json:"status"`
			}
			if json.Unmarshal(body, &resp) == nil {
				if resp.ID != "" {
					s.markOwnedImage(resp.ID)
				}
				if strings.HasPrefix(resp.Status, "sha256:") {
					s.markOwnedImage(resp.Status)
				}
			}
			if target := formatImageRepoTag(strings.TrimSpace(q.Get("repo")), strings.TrimSpace(q.Get("tag"))); target != "" {
				s.markOwnedImage(target)
			}
		}
		writeBackendRawResponse(w, status, hdr, body)
		return
	}

	fromImage := strings.TrimSpace(q.Get("fromImage"))
	tag := strings.TrimSpace(q.Get("tag"))
	platform := strings.TrimSpace(q.Get("platform"))
	if fromImage != "" {
		if !s.hasNetworkPermission() {
			writeDockerJSONError(w, http.StatusForbidden, "network permission denied for extension "+s.extID)
			return
		}
		if !validateImageCreateRefParam(fromImage) || (tag != "" && !validateImageCreateRefParam(tag)) {
			writeDockerJSONError(w, http.StatusBadRequest, "invalid image reference")
			return
		}
		if isForeignExtensionImageRef(fromImage, s.extID) {
			writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("image %q is not accessible to extension %q", fromImage, s.extID))
			return
		}
		if strings.TrimSpace(q.Get("repo")) != "" {
			if status, msg := s.verifyImageTagAllowed(r.Context(), r, platform); status != 0 {
				writeDockerJSONError(w, status, msg)
				return
			}
		}
		target := formatImagePullTarget(fromImage, tag)
		if isForeignExtensionImageRef(target, s.extID) {
			writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("image %q is not accessible to extension %q", target, s.extID))
			return
		}
		if status, msg := s.verifyImageAccessible(r.Context(), target, platform); status != 0 && status != http.StatusNotFound {
			writeDockerJSONError(w, status, msg)
			return
		}
	}

	s.dispatchBackend(w, r)
}

func (s *scopedDockerProxy) verifyVolumeOwnership(ctx context.Context, volName string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	name := strings.TrimSpace(volName)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return http.StatusBadRequest, "invalid volume identifier"
	}
	status, body := s.backendGet(ctx, "/volumes/"+url.PathEscape(name))
	if status == http.StatusServiceUnavailable {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusServiceUnavailable, msg
		}
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, msg
		}
		return http.StatusNotFound, "no such volume: " + volName
	}
	if status != http.StatusOK {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return status, msg
		}
		return http.StatusForbidden, "volume access denied for extension " + s.extID
	}
	var vol struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &vol); err != nil {
		return http.StatusForbidden, "volume access denied: invalid inspect response"
	}
	if vol.Labels["io.gvx.extension"] != s.extID {
		return http.StatusForbidden, fmt.Sprintf("volume %q is not owned by extension %q", volName, s.extID)
	}
	s.markOwnedVolume(name)
	return 0, ""
}

func (s *scopedDockerProxy) serveFilteredVolumeList(w http.ResponseWriter, r *http.Request) {
	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status != http.StatusOK {
		writeBackendRawResponse(w, status, hdr, body)
		return
	}

	var resp struct {
		Volumes  []json.RawMessage `json:"Volumes"`
		Warnings []string          `json:"Warnings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		writeDockerJSONError(w, http.StatusBadGateway, "invalid volume list response from docker backend")
		return
	}
	filtered := make([]json.RawMessage, 0, len(resp.Volumes))
	for _, item := range resp.Volumes {
		var meta struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal(item, &meta) == nil && meta.Labels["io.gvx.extension"] == s.extID {
			s.markOwnedVolume(meta.Name)
			filtered = append(filtered, item)
		}
	}
	if resp.Warnings == nil {
		resp.Warnings = []string{}
	}
	out, err := json.Marshal(struct {
		Volumes  []json.RawMessage `json:"Volumes"`
		Warnings []string          `json:"Warnings"`
	}{
		Volumes:  filtered,
		Warnings: resp.Warnings,
	})
	if err != nil {
		writeDockerJSONError(w, http.StatusInternalServerError, "encode filtered volume list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func isBuiltinDockerNetworkName(name string) bool {
	return name == "bridge" || name == "host" || name == "none"
}

func isHostNetworkName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return strings.EqualFold(trimmed, "host") || canonicalSimpleFold(trimmed) == "host"
}

func isBuiltinWritableNetworkName(name string) bool {
	return name == "bridge" || name == "none"
}

func (s *scopedDockerProxy) ownedContainerIDSet(ctx context.Context) (map[string]bool, int, string) {
	status, body := s.backendGet(ctx, "/containers/json?all=1")
	if status == http.StatusServiceUnavailable {
		return nil, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status != http.StatusOK {
		return map[string]bool{}, 0, ""
	}
	var items []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return map[string]bool{}, 0, ""
	}
	owned := make(map[string]bool, len(items))
	for _, item := range items {
		if item.ID != "" && item.Labels["io.gvx.extension"] == s.extID {
			owned[item.ID] = true
			s.markOwnedContainer(item.ID)
		}
	}
	return owned, 0, ""
}

func scrubNetworkContainersMap(netObj map[string]any, ownedIDs map[string]bool) {
	rawCtrs, ok := netObj["Containers"]
	if !ok || rawCtrs == nil {
		return
	}
	ctrs, ok := rawCtrs.(map[string]any)
	if !ok {
		return
	}
	for cid := range ctrs {
		if !ownedIDs[cid] {
			delete(ctrs, cid)
		}
	}
	netObj["Containers"] = ctrs
}

func (s *scopedDockerProxy) inspectNetworkCreate(r *http.Request) (*http.Request, int, error) {
	raw, err := readDockerJSONBody(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	payload, err := decodeJSONObjectStrictNoCaseCollisions(raw)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid network create JSON: %w", err)
	}
	if payload == nil {
		return nil, http.StatusBadRequest, errors.New("network create JSON must be an object")
	}
	if rawName, hasName, err := popCaseInsensitive(payload, "Name"); err != nil {
		return nil, http.StatusBadRequest, err
	} else if hasName && rawName != nil {
		name, ok := rawName.(string)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("network Name must be a string")
		}
		trimmed := strings.TrimSpace(name)
		if isBuiltinDockerNetworkName(trimmed) || isHostNetworkName(trimmed) || strings.EqualFold(trimmed, "bridge") || strings.EqualFold(trimmed, "none") {
			return nil, http.StatusForbidden, fmt.Errorf("cannot create built-in network %q", trimmed)
		}
		payload["Name"] = trimmed
	}
	if rawDriver, hasDriver, err := popCaseInsensitive(payload, "Driver"); err != nil {
		return nil, http.StatusBadRequest, err
	} else if hasDriver && rawDriver != nil {
		drv, ok := rawDriver.(string)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("network Driver must be a string")
		}
		if strings.EqualFold(strings.TrimSpace(drv), "host") {
			return nil, http.StatusForbidden, errors.New("host network driver is not allowed for extensions")
		}
		payload["Driver"] = drv
	}
	if status, err := enforceExtensionLabelInPayload(payload, s.extID); err != nil {
		return nil, status, err
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encode network create payload: %w", err)
	}
	return cloneRequestWithBody(r, out), 0, nil
}

func (s *scopedDockerProxy) serveFilteredNetworkList(w http.ResponseWriter, r *http.Request) {
	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status != http.StatusOK {
		writeBackendRawResponse(w, status, hdr, body)
		return
	}

	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		writeDockerJSONError(w, http.StatusBadGateway, "invalid network list response from docker backend")
		return
	}
	ownedIDs, st, msg := s.ownedContainerIDSet(r.Context())
	if st != 0 {
		writeDockerJSONError(w, st, msg)
		return
	}

	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		name, _ := item["Name"].(string)
		netID, _ := item["Id"].(string)
		labels, _ := item["Labels"].(map[string]any)
		owner, _ := labels["io.gvx.extension"].(string)
		isOwnedCustom := owner == s.extID
		if !isBuiltinDockerNetworkName(name) && !isOwnedCustom {
			continue
		}
		if isOwnedCustom && netID != "" {
			s.markOwnedNetwork(netID)
		}
		scrubNetworkContainersMap(item, ownedIDs)
		filtered = append(filtered, item)
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		writeDockerJSONError(w, http.StatusInternalServerError, "encode filtered network list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *scopedDockerProxy) serveScopedNetworkInspect(w http.ResponseWriter, r *http.Request, netID string) {
	if s.engineHandler == nil && !s.hasDial {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status != http.StatusOK {
		writeBackendRawResponse(w, status, hdr, body)
		return
	}
	var item map[string]any
	if err := json.Unmarshal(body, &item); err != nil {
		writeDockerJSONError(w, http.StatusBadGateway, "invalid network inspect response from docker backend")
		return
	}
	name, _ := item["Name"].(string)
	id, _ := item["Id"].(string)
	labels, _ := item["Labels"].(map[string]any)
	owner, _ := labels["io.gvx.extension"].(string)
	if !isBuiltinDockerNetworkName(name) && owner != s.extID {
		writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("network %q is not owned by extension %q", netID, s.extID))
		return
	}
	if owner == s.extID && id != "" {
		s.markOwnedNetwork(id)
	}
	ownedIDs, st, msg := s.ownedContainerIDSet(r.Context())
	if st != 0 {
		writeDockerJSONError(w, st, msg)
		return
	}
	scrubNetworkContainersMap(item, ownedIDs)
	out, err := json.Marshal(item)
	if err != nil {
		writeDockerJSONError(w, http.StatusInternalServerError, "encode network inspect response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *scopedDockerProxy) verifyNetworkOwnedCustom(ctx context.Context, netID string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	nid := strings.TrimSpace(netID)
	if nid == "" || nid == "." || nid == ".." || strings.ContainsAny(nid, `/\`) || strings.ContainsRune(nid, 0) {
		return http.StatusBadRequest, "invalid network identifier"
	}
	status, body := s.backendGet(ctx, "/networks/"+url.PathEscape(nid))
	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, msg
		}
		return http.StatusNotFound, "network " + netID + " not found"
	}
	if status != http.StatusOK {
		return http.StatusForbidden, "network access denied for extension " + s.extID
	}
	var netInfo struct {
		ID     string            `json:"Id"`
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &netInfo); err != nil {
		return http.StatusForbidden, "network access denied: invalid inspect response"
	}
	if isBuiltinDockerNetworkName(netInfo.Name) {
		return http.StatusForbidden, fmt.Sprintf("built-in network %q cannot be modified/removed by extension %q", netInfo.Name, s.extID)
	}
	if netInfo.Labels["io.gvx.extension"] != s.extID {
		return http.StatusForbidden, fmt.Sprintf("network %q is not owned by extension %q", netID, s.extID)
	}
	if netInfo.ID != "" {
		s.markOwnedNetwork(netInfo.ID)
	}
	return 0, ""
}

func (s *scopedDockerProxy) verifyNetworkDeletable(ctx context.Context, netID string) (int, string) {
	if isBuiltinDockerNetworkName(strings.TrimSpace(netID)) {
		return http.StatusForbidden, fmt.Sprintf("built-in network %q cannot be removed by extension %q", netID, s.extID)
	}
	return s.verifyNetworkOwnedCustom(ctx, netID)
}

func (s *scopedDockerProxy) verifyNetworkWritableOrBuiltin(ctx context.Context, netID string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	nid := strings.TrimSpace(netID)
	if nid == "" || nid == "." || nid == ".." || strings.ContainsAny(nid, `/\`) || strings.ContainsRune(nid, 0) {
		return http.StatusBadRequest, "invalid network identifier"
	}
	if isHostNetworkName(nid) {
		return http.StatusForbidden, fmt.Sprintf("host network %q is not allowed for extension %q", netID, s.extID)
	}
	if isBuiltinWritableNetworkName(nid) {
		return 0, ""
	}
	status, body := s.backendGet(ctx, "/networks/"+url.PathEscape(nid))
	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, msg
		}
		return http.StatusNotFound, "network " + netID + " not found"
	}
	if status != http.StatusOK {
		return http.StatusForbidden, "network access denied for extension " + s.extID
	}
	var netInfo struct {
		ID     string            `json:"Id"`
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &netInfo); err != nil {
		return http.StatusForbidden, "network access denied"
	}
	if isHostNetworkName(netInfo.Name) || isHostNetworkName(netInfo.Driver) {
		return http.StatusForbidden, fmt.Sprintf("host network %q is not allowed for extension %q", netID, s.extID)
	}
	if isBuiltinWritableNetworkName(netInfo.Name) {
		return 0, ""
	}
	if (netInfo.Name == "" && netInfo.ID == "") || netInfo.Labels["io.gvx.extension"] != s.extID {
		return http.StatusForbidden, fmt.Sprintf("network %q is not owned by extension %q", netID, s.extID)
	}
	if netInfo.ID != "" {
		s.markOwnedNetwork(netInfo.ID)
	}
	return 0, ""
}

func escapeImagePathSegments(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func normalizeDockerArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "", "unknown":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

func dockerPlatformsConflict(requested, resolved string) bool {
	req := strings.ToLower(strings.TrimSpace(requested))
	res := strings.ToLower(strings.TrimSpace(resolved))
	if req == "" || res == "" {
		return false
	}
	reqParts := strings.Split(req, "/")
	resParts := strings.Split(res, "/")
	var reqOS, reqArch, resOS, resArch string
	if len(reqParts) >= 2 {
		reqOS, reqArch = reqParts[0], normalizeDockerArch(reqParts[1])
	} else {
		reqArch = normalizeDockerArch(reqParts[0])
	}
	if len(resParts) >= 2 {
		resOS, resArch = resParts[0], normalizeDockerArch(resParts[1])
	} else {
		resArch = normalizeDockerArch(resParts[0])
	}
	if reqOS != "" && resOS != "" && reqOS != resOS {
		return true
	}
	if reqArch != "" && resArch != "" && reqArch != resArch {
		return true
	}
	return false
}

func (s *scopedDockerProxy) verifyImageAccessible(ctx context.Context, imgRef, platform string) (int, string) {
	status, _, msg := s.verifyImageAccessibleWithPlatform(ctx, imgRef, platform)
	return status, msg
}

func (s *scopedDockerProxy) verifyImageAccessibleWithPlatform(ctx context.Context, imgRef, platform string) (int, string, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, "", dockerproxy.ErrNotRunningMessage
	}
	ref := strings.TrimSpace(imgRef)
	if ref == "" || ref == "." || ref == ".." || hasDotDotSegment(ref) || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) || strings.ContainsFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return http.StatusBadRequest, "", "invalid image identifier"
	}
	if isForeignExtensionImageRef(ref, s.extID) {
		return http.StatusForbidden, "", fmt.Sprintf("image %q is not accessible to extension %q", imgRef, s.extID)
	}
	status, resolvedPlatform, msg := s.verifySingleImageAccessible(ctx, ref, platform)
	if status != 0 && status != http.StatusNotFound {
		return status, resolvedPlatform, msg
	}
	if tagPart, digestPart, hasDigest := strings.Cut(ref, "@"); hasDigest {
		if tp := strings.TrimSpace(tagPart); tp != "" {
			if isForeignExtensionImageRef(tp, s.extID) {
				return http.StatusForbidden, "", fmt.Sprintf("image %q is not accessible to extension %q", imgRef, s.extID)
			}
			if pStatus, _, pMsg := s.verifySingleImageAccessible(ctx, tp, platform); pStatus != 0 && pStatus != http.StatusNotFound {
				return pStatus, "", pMsg
			}
		}
		if dp := strings.TrimSpace(digestPart); dp != "" {
			if pStatus, _, pMsg := s.verifySingleImageAccessible(ctx, dp, platform); pStatus != 0 && pStatus != http.StatusNotFound {
				return pStatus, "", pMsg
			}
		}
	}
	return status, resolvedPlatform, msg
}

func (s *scopedDockerProxy) verifySingleImageAccessible(ctx context.Context, ref, platform string) (int, string, string) {
	if ref == "" || ref == "." || ref == ".." || hasDotDotSegment(ref) || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) || strings.ContainsFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return http.StatusBadRequest, "", "invalid image identifier"
	}
	qPath := "/images/" + escapeImagePathSegments(ref) + "/json"
	if platform != "" {
		qPath += "?platform=" + url.QueryEscape(platform)
	}
	status, body := s.backendGet(ctx, qPath)
	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, "", dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, "", msg
		}
		return http.StatusNotFound, "", "No such image: " + ref
	}
	if status != http.StatusOK {
		return http.StatusForbidden, "", "image access denied for extension " + s.extID
	}
	var inspect struct {
		ID           string `json:"Id"`
		Os           string `json:"Os"`
		Architecture string `json:"Architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &inspect); err != nil {
		return http.StatusForbidden, "", "image access denied: invalid inspect response"
	}
	owner := inspect.Config.Labels["io.gvx.extension"]
	if owner != "" && owner != s.extID {
		return http.StatusForbidden, "", fmt.Sprintf("image %q is not accessible to extension %q", ref, s.extID)
	}
	if owner == s.extID && inspect.ID != "" {
		s.markOwnedImage(inspect.ID)
	}
	var resolvedPlatform string
	if osName, arch := strings.TrimSpace(inspect.Os), strings.TrimSpace(inspect.Architecture); osName != "" && arch != "" {
		resolvedPlatform = osName + "/" + arch
	}
	return 0, resolvedPlatform, ""
}

func (s *scopedDockerProxy) verifyImageOwnership(ctx context.Context, imgRef, platform string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	ref := strings.TrimSpace(imgRef)
	if ref == "" || ref == "." || ref == ".." || strings.ContainsRune(ref, 0) {
		return http.StatusBadRequest, "invalid image identifier"
	}
	if isForeignExtensionImageRef(ref, s.extID) {
		return http.StatusForbidden, fmt.Sprintf("image %q is not owned by extension %q", imgRef, s.extID)
	}
	qPath := "/images/" + escapeImagePathSegments(ref) + "/json"
	if platform != "" {
		qPath += "?platform=" + url.QueryEscape(platform)
	}
	status, body := s.backendGet(ctx, qPath)
	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusNotFound {
		if msg := parseDockerErrorMessage(body); msg != "" {
			return http.StatusNotFound, msg
		}
		return http.StatusNotFound, "No such image: " + imgRef
	}
	if status != http.StatusOK {
		return http.StatusForbidden, "image access denied for extension " + s.extID
	}
	var inspect struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &inspect); err != nil {
		return http.StatusForbidden, "image access denied: invalid inspect response"
	}
	if inspect.Config.Labels["io.gvx.extension"] != s.extID {
		return http.StatusForbidden, fmt.Sprintf("image %q is not owned by extension %q", imgRef, s.extID)
	}
	if inspect.ID != "" {
		s.markOwnedImage(inspect.ID)
	}
	return 0, ""
}

func (s *scopedDockerProxy) serveScopedImageDelete(w http.ResponseWriter, r *http.Request, imgRef string) {
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if status, msg := s.verifyImageOwnership(r.Context(), imgRef, platform); status != 0 {
		writeDockerJSONError(w, status, msg)
		return
	}

	// Ensure no foreign or host container references this image (and avoid leaking foreign container IDs).
	qPath := "/images/" + escapeImagePathSegments(strings.TrimSpace(imgRef)) + "/json"
	if platform != "" {
		qPath += "?platform=" + url.QueryEscape(platform)
	}
	if st, body := s.backendGet(r.Context(), qPath); st == http.StatusOK {
		var img struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(body, &img) == nil && img.ID != "" {
			if cst, cbody := s.backendGet(r.Context(), "/containers/json?all=1"); cst == http.StatusOK {
				var ctrs []struct {
					ImageID string            `json:"ImageID"`
					Labels  map[string]string `json:"Labels"`
				}
				if json.Unmarshal(cbody, &ctrs) == nil {
					for _, c := range ctrs {
						if c.ImageID == img.ID && c.Labels["io.gvx.extension"] != s.extID {
							writeDockerJSONError(w, http.StatusForbidden, fmt.Sprintf("cannot delete image %q: referenced by non-owned container", imgRef))
							return
						}
					}
				}
			}
		}
	}

	req2 := r.Clone(r.Context())
	req2.Body = http.NoBody
	status, hdr, body := s.backendDispatch(req2)
	if status == http.StatusServiceUnavailable && len(body) == 0 {
		writeDockerJSONError(w, http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage)
		return
	}
	if status == http.StatusConflict {
		// Never leak foreign/host container IDs in conflict responses.
		writeDockerJSONError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to remove image %q", imgRef))
		return
	}
	writeBackendRawResponse(w, status, hdr, body)
}

func (s *scopedDockerProxy) verifyImageTagAllowed(ctx context.Context, r *http.Request, overridePlatform string) (int, string) {
	if s.engineHandler == nil && !s.hasDial {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return http.StatusBadRequest, "invalid query string"
	}
	if len(q["repo"]) > 1 || len(q["tag"]) > 1 || len(q["platform"]) > 1 {
		return http.StatusBadRequest, "duplicate repo, tag, or platform query parameter is not allowed"
	}
	repo := strings.TrimSpace(q.Get("repo"))
	tag := strings.TrimSpace(q.Get("tag"))
	platform := strings.TrimSpace(overridePlatform)
	if platform == "" {
		platform = strings.TrimSpace(q.Get("platform"))
	}
	if repo == "" {
		return 0, ""
	}
	target := formatImageRepoTag(repo, tag)
	if target == "." || target == ".." || hasDotDotSegment(target) || strings.ContainsRune(target, 0) {
		return http.StatusBadRequest, "invalid target image reference"
	}
	if isForeignExtensionImageRef(target, s.extID) {
		return http.StatusForbidden, fmt.Sprintf("cannot write into reserved extension namespace %q", target)
	}
	if st, msg := s.verifySingleImageTagTarget(ctx, target, platform); st != 0 {
		return st, msg
	}
	if tagPart, digestPart, hasDigest := strings.Cut(target, "@"); hasDigest {
		if tp := strings.TrimSpace(tagPart); tp != "" {
			if st, msg := s.verifySingleImageTagTarget(ctx, tp, platform); st != 0 {
				return st, msg
			}
		}
		if dp := strings.TrimSpace(digestPart); dp != "" {
			if st, msg := s.verifyImageAccessible(ctx, dp, platform); st != 0 && st != http.StatusNotFound {
				return st, msg
			}
		}
	}
	return 0, ""
}

func (s *scopedDockerProxy) verifySingleImageTagTarget(ctx context.Context, target, platform string) (int, string) {
	if target == "." || target == ".." || hasDotDotSegment(target) || strings.HasPrefix(target, "-") || strings.ContainsRune(target, 0) {
		return http.StatusBadRequest, "invalid target image reference"
	}
	if isForeignExtensionImageRef(target, s.extID) {
		return http.StatusForbidden, fmt.Sprintf("cannot write into reserved extension namespace %q", target)
	}
	qPath := "/images/" + escapeImagePathSegments(target) + "/json"
	if platform != "" {
		qPath += "?platform=" + url.QueryEscape(platform)
	}
	status, body := s.backendGet(ctx, qPath)
	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, dockerproxy.ErrNotRunningMessage
	}
	if status == http.StatusOK {
		var inspect struct {
			ID     string `json:"Id"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal(body, &inspect); err != nil || inspect.ID == "" || inspect.Config.Labels["io.gvx.extension"] != s.extID {
			return http.StatusForbidden, fmt.Sprintf("cannot overwrite existing image tag %q not owned by extension %q", target, s.extID)
		}
		return 0, ""
	}
	if status != http.StatusNotFound {
		return http.StatusForbidden, fmt.Sprintf("cannot verify ownership of target image tag %q", target)
	}
	return 0, ""
}

func parseDockerErrorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return ""
}

func readDockerJSONBody(r *http.Request) ([]byte, error) {
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxDockerCreateBodyBytes+1))
	_ = body.Close()
	r.Header.Del("Expect")
	if err != nil {
		return nil, fmt.Errorf("read docker request body: %w", err)
	}
	if len(raw) > maxDockerCreateBodyBytes {
		return nil, fmt.Errorf("docker payload exceeds %d MiB limit", maxDockerCreateBodyBytes>>20)
	}
	return raw, nil
}

func cloneRequestWithBody(r *http.Request, body []byte) *http.Request {
	r2 := r.Clone(r.Context())
	if len(body) == 0 {
		r2.Body = http.NoBody
		r2.ContentLength = 0
	} else {
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
	}
	r2.TransferEncoding = nil
	r2.Trailer = nil
	return r2
}

func inspectAndRewriteExecCreate(r *http.Request) (*http.Request, int, error) {
	raw, err := readDockerJSONBody(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cloneRequestWithBody(r, nil), 0, nil
	}
	payload, err := decodeJSONObjectStrictNoCaseCollisions(raw)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid exec create JSON: %w", err)
	}
	if payload == nil {
		return nil, http.StatusBadRequest, errors.New("exec create JSON must be an object")
	}
	rawPriv, ok, err := popCaseInsensitive(payload, "Privileged")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if ok && rawPriv != nil {
		priv, ok := rawPriv.(bool)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("Privileged must be a boolean")
		}
		if priv {
			return nil, http.StatusForbidden, errors.New("Privileged exec is not allowed for extensions")
		}
		payload["Privileged"] = false
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encode exec create payload: %w", err)
	}
	return cloneRequestWithBody(r, out), 0, nil
}

func (s *scopedDockerProxy) inspectAndRewriteCreate(r *http.Request) (*http.Request, int, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("invalid query string")
	}
	if len(q["platform"]) > 1 {
		return nil, http.StatusBadRequest, errors.New("multiple platform query parameters are not allowed")
	}
	platform := strings.TrimSpace(q.Get("platform"))

	raw, err := readDockerJSONBody(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	rewritten, status, err := scopeContainerCreatePayload(raw, s.extID, s.dataDir, s.grants)
	if err != nil {
		return nil, status, err
	}
	if s.engineHandler != nil || s.hasDial {
		if status, err := s.verifyContainerCreateCrossResourceRefs(r.Context(), rewritten, platform); err != nil {
			return nil, status, err
		}
	}
	return cloneRequestWithBody(r, rewritten), 0, nil
}

func (s *scopedDockerProxy) verifyContainerCreateCrossResourceRefs(ctx context.Context, payloadBytes []byte, platform string) (int, error) {
	var payload struct {
		Image      string `json:"Image"`
		HostConfig struct {
			NetworkMode string   `json:"NetworkMode"`
			Binds       []string `json:"Binds"`
			Mounts      []struct {
				Type   string `json:"Type"`
				Source string `json:"Source"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
		NetworkingConfig struct {
			EndpointsConfig map[string]any `json:"EndpointsConfig"`
		} `json:"NetworkingConfig"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return 0, nil
	}
	if strings.TrimSpace(payload.Image) != "" {
		if isForeignExtensionImageRef(payload.Image, s.extID) {
			return http.StatusForbidden, fmt.Errorf("image %q is not accessible to extension %q", payload.Image, s.extID)
		}
		if status, msg := s.verifyImageAccessible(ctx, payload.Image, platform); status != 0 && status != http.StatusNotFound {
			return status, errors.New(msg)
		}
	}
	for _, bStr := range payload.HostConfig.Binds {
		src, _, isBind, err := parseDockerBindSpec(bStr)
		if err != nil {
			return http.StatusForbidden, err
		}
		if !isBind {
			volName := strings.TrimSpace(src)
			status, msg := s.verifyVolumeOwnership(ctx, volName)
			if status == http.StatusServiceUnavailable {
				return http.StatusServiceUnavailable, errors.New(msg)
			}
			if status != 0 {
				return http.StatusForbidden, fmt.Errorf("volume %q is not owned by extension %q", volName, s.extID)
			}
		}
	}
	for _, m := range payload.HostConfig.Mounts {
		mt := strings.ToLower(strings.TrimSpace(m.Type))
		src := strings.TrimSpace(m.Source)
		if mt == "volume" || (mt == "" && src != "") {
			status, msg := s.verifyVolumeOwnership(ctx, src)
			if status == http.StatusServiceUnavailable {
				return http.StatusServiceUnavailable, errors.New(msg)
			}
			if status != 0 {
				return http.StatusForbidden, fmt.Errorf("volume %q is not owned by extension %q", src, s.extID)
			}
			if mt == "" {
				return http.StatusBadRequest, errors.New("HostConfig.Mounts Type is required")
			}
		}
	}
	if nm := strings.TrimSpace(payload.HostConfig.NetworkMode); nm != "" && nm != "default" && nm != "nat" {
		if isHostNetworkName(nm) {
			return http.StatusForbidden, fmt.Errorf("HostConfig.NetworkMode %q is not allowed for extensions", nm)
		}
		if !isBuiltinWritableNetworkName(nm) {
			if status, msg := s.verifyNetworkWritableOrBuiltin(ctx, nm); status != 0 && status != http.StatusNotFound {
				return status, errors.New(msg)
			}
		}
	}
	for netName := range payload.NetworkingConfig.EndpointsConfig {
		nm := strings.TrimSpace(netName)
		if nm == "" || nm == "default" || nm == "nat" {
			continue
		}
		if isHostNetworkName(nm) {
			return http.StatusForbidden, fmt.Errorf("NetworkingConfig.EndpointsConfig network %q is not allowed for extensions", netName)
		}
		if !isBuiltinWritableNetworkName(nm) {
			if status, msg := s.verifyNetworkWritableOrBuiltin(ctx, nm); status != 0 && status != http.StatusNotFound {
				return status, errors.New(msg)
			}
		}
	}
	return 0, nil
}

func (s *scopedDockerProxy) inspectNetworkContainerOp(r *http.Request) (*http.Request, int, error) {
	raw, err := readDockerJSONBody(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	payload, err := decodeJSONObjectStrictNoCaseCollisions(raw)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid network request JSON: %w", err)
	}
	if payload == nil {
		return nil, http.StatusBadRequest, errors.New("network request JSON must be an object")
	}
	rawCID, hasCID, err := popCaseInsensitive(payload, "Container")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if !hasCID || rawCID == nil {
		return nil, http.StatusBadRequest, errors.New("Container is required")
	}
	cid, ok := rawCID.(string)
	if !ok {
		return nil, http.StatusBadRequest, errors.New("Container must be a string")
	}
	cid = strings.TrimSpace(cid)
	if cid == "" {
		return nil, http.StatusBadRequest, errors.New("Container must be a non-empty string")
	}
	if status, msg := s.verifyContainerOwnership(r.Context(), cid); status != 0 {
		return nil, status, errors.New(msg)
	}
	payload["Container"] = cid
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encode network request payload: %w", err)
	}
	return cloneRequestWithBody(r, out), 0, nil
}

func (s *scopedDockerProxy) inspectVolumeCreate(r *http.Request) (*http.Request, int, error) {
	raw, err := readDockerJSONBody(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	var payload map[string]any
	if len(bytes.TrimSpace(raw)) == 0 {
		payload = map[string]any{}
	} else {
		payload, err = decodeJSONObjectStrictNoCaseCollisions(raw)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid volume create JSON: %w", err)
		}
		if payload == nil {
			return nil, http.StatusBadRequest, errors.New("volume create JSON must be an object")
		}
	}
	rawName, hasName, err := popCaseInsensitive(payload, "Name")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if hasName && rawName != nil {
		name, ok := rawName.(string)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("volume Name must be a string")
		}
		trimmed := strings.TrimSpace(name)
		if trimmed == "gvx-buildkit" {
			return nil, http.StatusForbidden, fmt.Errorf("volume %q is reserved and forbidden for extensions", trimmed)
		}
		payload["Name"] = trimmed
	}
	rawOpts, hasOpts, err := popCaseInsensitive(payload, "DriverOpts")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if hasOpts && rawOpts != nil {
		opts, ok := rawOpts.(map[string]any)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("DriverOpts must be a JSON object")
		}
		if status, err := validateVolumeDriverOptsMap(opts); err != nil {
			return nil, status, err
		}
		payload["DriverOpts"] = opts
	}
	if status, err := enforceExtensionLabelInPayload(payload, s.extID); err != nil {
		return nil, status, err
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encode volume create payload: %w", err)
	}
	return cloneRequestWithBody(r, out), 0, nil
}

func validateAndCanonicalizePayloadOnBuild(payload map[string]any) (int, error) {
	rawOnBuild, hasOnBuild, err := popCaseInsensitive(payload, "OnBuild")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if !hasOnBuild {
		return 0, nil
	}
	if rawOnBuild == nil {
		payload["OnBuild"] = nil
		return 0, nil
	}
	arr, ok := rawOnBuild.([]any)
	if !ok {
		return http.StatusBadRequest, errors.New("OnBuild must be an array of strings")
	}
	entries := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return http.StatusBadRequest, errors.New("OnBuild entries must be strings")
		}
		entries[i] = s
	}
	if status, err := validateImageChanges(entries); err != nil {
		return status, err
	}
	payload["OnBuild"] = entries
	return 0, nil
}

func enforceExtensionLabelInPayload(payload map[string]any, extID string) (int, error) {
	labels := map[string]any{}
	rawLabels, exists, err := popCaseInsensitive(payload, "Labels")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if exists && rawLabels != nil {
		m, ok := rawLabels.(map[string]any)
		if !ok {
			return http.StatusBadRequest, errors.New("Labels must be a JSON object")
		}
		labels = m
	}
	cur, hasOwner, err := popCaseInsensitive(labels, "io.gvx.extension")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if hasOwner && cur != nil {
		curStr, ok := cur.(string)
		if !ok || curStr != extID {
			return http.StatusForbidden, fmt.Errorf("cannot spoof io.gvx.extension label (expected %q)", extID)
		}
	}
	if status, err := validateAndCanonicalizePayloadOnBuild(payload); err != nil {
		return status, err
	}
	labels["io.gvx.extension"] = extID
	payload["Labels"] = labels
	return 0, nil
}

func scopeContainerCreatePayload(raw []byte, extID, dataDir string, grants *GrantStore) ([]byte, int, error) {
	payload, err := decodeJSONObjectStrictNoCaseCollisions(raw)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid container create JSON: %w", err)
	}
	if payload == nil {
		return nil, http.StatusBadRequest, errors.New("container create JSON must be an object")
	}

	// Canonicalize Image key if present.
	rawImg, hasImg, err := popCaseInsensitive(payload, "Image")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if hasImg && rawImg != nil {
		imgStr, ok := rawImg.(string)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("Image must be a string")
		}
		payload["Image"] = imgStr
	}

	// Enforce io.gvx.extension label and validate OnBuild canonically.
	if status, err := enforceExtensionLabelInPayload(payload, extID); err != nil {
		return nil, status, err
	}

	// Validate & canonically rewrite HostConfig.
	rawHC, hasHC, err := popCaseInsensitive(payload, "HostConfig")
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if hasHC && rawHC != nil {
		hc, ok := rawHC.(map[string]any)
		if !ok {
			return nil, http.StatusBadRequest, errors.New("HostConfig must be a JSON object")
		}
		if status, err := validateDangerousHostConfig(hc); err != nil {
			return nil, status, err
		}

		rawBinds, hasBinds, err := popCaseInsensitive(hc, "Binds")
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		if hasBinds && rawBinds != nil {
			binds, ok := rawBinds.([]any)
			if !ok {
				return nil, http.StatusBadRequest, errors.New("HostConfig.Binds must be an array")
			}
			for i, item := range binds {
				bStr, ok := item.(string)
				if !ok {
					return nil, http.StatusBadRequest, errors.New("HostConfig.Binds entries must be strings")
				}
				src, rest, isBind, err := parseDockerBindSpec(bStr)
				if err != nil {
					return nil, http.StatusForbidden, err
				}
				if !isBind {
					volName := strings.TrimSpace(src)
					if volName == "" || volName == "." || volName == ".." || strings.ContainsAny(volName, `/\`) || strings.ContainsRune(volName, 0) || volName == "gvx-buildkit" {
						return nil, http.StatusForbidden, fmt.Errorf("volume mount %q is not allowed for extension %q", bStr, extID)
					}
					binds[i] = volName + rest
					continue
				}
				rewrittenSrc, err := validateAndMapExtensionBind(src, extID, dataDir, grants)
				if err != nil {
					return nil, http.StatusForbidden, err
				}
				if rewrittenSrc != "" {
					binds[i] = rewrittenSrc + rest
				}
			}
			hc["Binds"] = binds
		}

		rawMounts, hasMounts, err := popCaseInsensitive(hc, "Mounts")
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		if hasMounts && rawMounts != nil {
			mounts, ok := rawMounts.([]any)
			if !ok {
				return nil, http.StatusBadRequest, errors.New("HostConfig.Mounts must be an array")
			}
			for _, item := range mounts {
				m, ok := item.(map[string]any)
				if !ok {
					return nil, http.StatusBadRequest, errors.New("HostConfig.Mounts entries must be objects")
				}
				if status, err := validateMountVolumeDriverOptions(m); err != nil {
					return nil, status, err
				}
				var mType string
				rawType, hasType, err := popCaseInsensitive(m, "Type")
				if err != nil {
					return nil, http.StatusBadRequest, err
				}
				if hasType && rawType != nil {
					s, ok := rawType.(string)
					if !ok {
						return nil, http.StatusBadRequest, errors.New("HostConfig.Mounts Type must be a string")
					}
					mType = s
					m["Type"] = mType
				}
				var src string
				rawSrc, hasSrc, err := popCaseInsensitive(m, "Source")
				if err != nil {
					return nil, http.StatusBadRequest, err
				}
				if hasSrc && rawSrc != nil {
					s, ok := rawSrc.(string)
					if !ok {
						return nil, http.StatusBadRequest, errors.New("HostConfig.Mounts Source must be a string")
					}
					src = s
					m["Source"] = src
				}
				trimmedType := strings.ToLower(strings.TrimSpace(mType))
				trimmedSrc := strings.TrimSpace(src)
				if trimmedType == "" && (strings.ContainsAny(src, `/\`) || strings.HasPrefix(src, ".") || isWindowsDrivePath(src)) {
					return nil, http.StatusForbidden, fmt.Errorf("mount with path source %q requires explicit bind type", src)
				}
				switch trimmedType {
				case "bind":
					m["Type"] = "bind"
					if trimmedSrc == "" {
						return nil, http.StatusForbidden, errors.New("bind mount requires a Source path")
					}
					rewrittenSrc, err := validateAndMapExtensionBind(src, extID, dataDir, grants)
					if err != nil {
						return nil, http.StatusForbidden, err
					}
					if rewrittenSrc != "" {
						m["Source"] = rewrittenSrc
					}
				case "volume":
					m["Type"] = "volume"
					if trimmedSrc == "" || trimmedSrc == "." || trimmedSrc == ".." || strings.ContainsAny(trimmedSrc, `/\`) || strings.ContainsRune(trimmedSrc, 0) || trimmedSrc == "gvx-buildkit" {
						return nil, http.StatusForbidden, fmt.Errorf("volume mount %q is not allowed for extension %q", src, extID)
					}
					m["Source"] = trimmedSrc
				case "tmpfs":
					m["Type"] = "tmpfs"
					if trimmedSrc != "" {
						return nil, http.StatusForbidden, fmt.Errorf("tmpfs mount must not specify Source %q", src)
					}
				case "":
					if trimmedSrc == "" || trimmedSrc == "." || trimmedSrc == ".." || strings.ContainsAny(trimmedSrc, `/\`) || strings.ContainsRune(trimmedSrc, 0) || trimmedSrc == "gvx-buildkit" {
						return nil, http.StatusForbidden, errors.New("mount requires an explicit allowed type (bind, volume, tmpfs)")
					}
					m["Source"] = trimmedSrc
				default:
					return nil, http.StatusForbidden, fmt.Errorf("mount type %q is not allowed for extensions", mType)
				}
			}
			hc["Mounts"] = mounts
		}

		payload["HostConfig"] = hc
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("encode container create payload: %w", err)
	}
	return out, 0, nil
}

func decodeJSONObjectStrictNoCaseCollisions(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("JSON payload must be valid UTF-8")
	}
	if hasLoneSurrogateEscape(raw) {
		return nil, errors.New("JSON payload contains unpaired UTF-16 surrogate")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeJSONValueStrict(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON object")
	}
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("must be a JSON object")
	}
	return obj, nil
}

// canonicalSimpleFold maps every rune in s to the minimum rune in its
// unicode.SimpleFold equivalence cycle, matching strings.EqualFold / encoding/json.
func canonicalSimpleFold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		minR := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < minR {
				minR = f
			}
		}
		b.WriteRune(minR)
	}
	return b.String()
}

func decodeJSONValueStrict(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := make(map[string]any)
		seenFold := make(map[string]string)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errors.New("expected JSON object key")
			}
			if !utf8.ValidString(key) {
				return nil, errors.New("JSON object key must be valid UTF-8")
			}
			fold := canonicalSimpleFold(key)
			if prev, dup := seenFold[fold]; dup {
				return nil, fmt.Errorf("duplicate or case-colliding JSON key %q (collides with %q)", key, prev)
			}
			seenFold[fold] = key
			val, err := decodeJSONValueStrict(dec)
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		end, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if end != json.Delim('}') {
			return nil, errors.New("expected '}'")
		}
		return obj, nil
	case '[':
		var arr []any
		for dec.More() {
			val, err := decodeJSONValueStrict(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		end, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if end != json.Delim(']') {
			return nil, errors.New("expected ']'")
		}
		if arr == nil {
			arr = []any{}
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func popCaseInsensitive(m map[string]any, canonical string) (any, bool, error) {
	var (
		matchedVal any
		matches    []string
	)
	for k, v := range m {
		if strings.EqualFold(k, canonical) {
			matches = append(matches, k)
			matchedVal = v
		}
	}
	for _, k := range matches {
		delete(m, k)
	}
	if len(matches) > 1 {
		return nil, false, fmt.Errorf("duplicate or case-colliding JSON key for %q", canonical)
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	return matchedVal, true, nil
}

func lookupCaseInsensitive(m map[string]any, canonical string) (any, bool, error) {
	var (
		matchedVal any
		matches    int
	)
	for k, v := range m {
		if strings.EqualFold(k, canonical) {
			matchedVal = v
			matches++
		}
	}
	if matches > 1 {
		return nil, false, fmt.Errorf("duplicate or case-colliding JSON key for %q", canonical)
	}
	if matches == 0 {
		return nil, false, nil
	}
	return matchedVal, true, nil
}

func validateDangerousHostConfig(hc map[string]any) (int, error) {
	rawPriv, ok, err := popCaseInsensitive(hc, "Privileged")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if ok && rawPriv != nil {
		priv, ok := rawPriv.(bool)
		if !ok {
			return http.StatusBadRequest, errors.New("HostConfig.Privileged must be a boolean")
		}
		if priv {
			return http.StatusForbidden, errors.New("HostConfig.Privileged is not allowed for extensions")
		}
		hc["Privileged"] = priv
	}

	for _, arrField := range []string{"VolumesFrom", "CapAdd", "SecurityOpt", "Devices"} {
		rawArr, ok, err := popCaseInsensitive(hc, arrField)
		if err != nil {
			return http.StatusBadRequest, err
		}
		if ok && rawArr != nil {
			arr, ok := rawArr.([]any)
			if !ok {
				return http.StatusBadRequest, fmt.Errorf("HostConfig.%s must be an array", arrField)
			}
			if len(arr) > 0 {
				return http.StatusForbidden, fmt.Errorf("HostConfig.%s is not allowed for extensions", arrField)
			}
			hc[arrField] = arr
		}
	}

	for _, nsField := range []string{"PidMode", "IpcMode", "UTSMode", "UsernsMode", "CgroupnsMode"} {
		rawMode, ok, err := popCaseInsensitive(hc, nsField)
		if err != nil {
			return http.StatusBadRequest, err
		}
		if ok && rawMode != nil {
			mode, ok := rawMode.(string)
			if !ok {
				return http.StatusBadRequest, fmt.Errorf("HostConfig.%s must be a string", nsField)
			}
			if strings.TrimSpace(mode) != "" {
				return http.StatusForbidden, fmt.Errorf("HostConfig.%s %q is not allowed for extensions", nsField, mode)
			}
			hc[nsField] = mode
		}
	}

	rawNet, ok, err := popCaseInsensitive(hc, "NetworkMode")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if ok && rawNet != nil {
		netMode, ok := rawNet.(string)
		if !ok {
			return http.StatusBadRequest, errors.New("HostConfig.NetworkMode must be a string")
		}
		trimmed := strings.TrimSpace(netMode)
		nm := strings.ToLower(trimmed)
		if strings.EqualFold(trimmed, "host") || strings.HasPrefix(nm, "container:") {
			return http.StatusForbidden, fmt.Errorf("HostConfig.NetworkMode %q is not allowed for extensions", netMode)
		}
		hc["NetworkMode"] = netMode
	}
	return 0, nil
}

func validateMountVolumeDriverOptions(m map[string]any) (int, error) {
	rawVO, hasVO, err := popCaseInsensitive(m, "VolumeOptions")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if !hasVO || rawVO == nil {
		return 0, nil
	}
	vo, ok := rawVO.(map[string]any)
	if !ok {
		return http.StatusBadRequest, errors.New("HostConfig.Mounts.VolumeOptions must be an object")
	}
	rawDC, hasDC, err := popCaseInsensitive(vo, "DriverConfig")
	if err != nil {
		return http.StatusBadRequest, err
	}
	if hasDC && rawDC != nil {
		dc, ok := rawDC.(map[string]any)
		if !ok {
			return http.StatusBadRequest, errors.New("HostConfig.Mounts.VolumeOptions.DriverConfig must be an object")
		}
		rawOpts, hasOpts, err := popCaseInsensitive(dc, "Options")
		if err != nil {
			return http.StatusBadRequest, err
		}
		if hasOpts && rawOpts != nil {
			opts, ok := rawOpts.(map[string]any)
			if !ok {
				return http.StatusBadRequest, errors.New("HostConfig.Mounts.VolumeOptions.DriverConfig.Options must be an object")
			}
			if status, err := validateVolumeDriverOptsMap(opts); err != nil {
				return status, err
			}
			dc["Options"] = opts
		}
		vo["DriverConfig"] = dc
	}
	m["VolumeOptions"] = vo
	return 0, nil
}

func validateVolumeDriverOptsMap(opts map[string]any) (int, error) {
	for k, v := range opts {
		s, ok := v.(string)
		if !ok {
			return http.StatusForbidden, fmt.Errorf("volume driver option %q is not allowed", k)
		}
		trimK := strings.TrimSpace(k)
		trimV := strings.TrimSpace(s)
		lowerK := strings.ToLower(trimK)
		lowerV := strings.ToLower(trimV)
		foldV := canonicalSimpleFold(trimV)
		switch {
		case strings.EqualFold(trimK, "device") || lowerK == "device":
			if lowerV != "" {
				return http.StatusForbidden, errors.New("volume driver device bind option is not allowed for extensions")
			}
		case strings.EqualFold(trimK, "o") || lowerK == "o":
			if strings.Contains(lowerV, "bind") || strings.Contains(foldV, "BIND") {
				return http.StatusForbidden, errors.New("volume driver bind mount option is not allowed for extensions")
			}
		case strings.EqualFold(trimK, "type") || lowerK == "type":
			if lowerV == "none" || lowerV == "bind" || foldV == "NONE" || foldV == "BIND" {
				return http.StatusForbidden, fmt.Errorf("volume driver type %q is not allowed for extensions", s)
			}
		}
	}
	return 0, nil
}

// parseDockerBindSpec parses a Docker HostConfig.Binds entry.
// Returns (src, restIncludingLeadingColon, isBindPath, error).
// Named Docker volumes (no '/', '\\', '.', or drive prefix) return isBindPath=false.
func parseDockerBindSpec(spec string) (src string, rest string, isBind bool, err error) {
	if spec == "" {
		return "", "", false, errors.New("empty bind mount specification")
	}
	if strings.ContainsRune(spec, 0) {
		return "", "", false, errors.New("bind mount contains NUL byte")
	}
	// Windows drive-letter path, e.g. C:\foo:/bar or C:/foo:/bar:ro.
	if len(spec) >= 3 && isASCIIAlpha(spec[0]) && spec[1] == ':' && (spec[2] == '\\' || spec[2] == '/') {
		idx := strings.IndexByte(spec[3:], ':')
		if idx < 0 {
			return "", "", false, fmt.Errorf("invalid bind mount %q: missing container target", spec)
		}
		src = spec[:3+idx]
		rest = spec[3+idx:]
		if rest == ":" {
			return "", "", false, fmt.Errorf("invalid bind mount %q: empty container target", spec)
		}
		return src, rest, true, nil
	}
	// Reject drive-relative Windows specs like C:foo:/bar.
	if len(spec) >= 2 && isASCIIAlpha(spec[0]) && spec[1] == ':' {
		return "", "", false, fmt.Errorf("relative drive bind mount %q is not allowed", spec)
	}

	rawSrc, rawDst, hasColon := strings.Cut(spec, ":")
	if !hasColon {
		return "", "", false, fmt.Errorf("invalid volume specification %q: missing container target", spec)
	}
	if rawSrc == "" || rawDst == "" {
		return "", "", false, fmt.Errorf("invalid bind mount %q", spec)
	}
	if strings.HasPrefix(rawSrc, "\\\\") {
		return "", "", false, fmt.Errorf("UNC bind mount %q is not allowed", spec)
	}
	if strings.HasPrefix(rawSrc, "/") {
		return rawSrc, ":" + rawDst, true, nil
	}
	// Reject relative host paths such as ./foo, ../foo, foo/bar, foo\bar.
	if rawSrc == "." || rawSrc == ".." || strings.ContainsAny(rawSrc, `/\`) {
		return "", "", false, fmt.Errorf("relative bind mount %q is not allowed", spec)
	}
	// Plain named Docker volume (e.g. "my-vol:/data").
	return rawSrc, ":" + rawDst, false, nil
}

func validateAndMapExtensionBind(rawSrc, extID, dataDir string, grants *GrantStore) (string, error) {
	if rawSrc == "" || strings.ContainsRune(rawSrc, 0) {
		return "", errors.New("bind mount source is empty or contains NUL")
	}
	if hasDotDotSegment(rawSrc) {
		return "", fmt.Errorf("bind mount source %q must not contain '..'", rawSrc)
	}

	filesDir := FilesDir(dataDir, extID)
	if err := EnsureDirWithin(dataDir, filesDir); err != nil {
		return "", fmt.Errorf("prepare extension files dir: %w", err)
	}
	realFilesDir, err := resolvePathWithAncestor(filesDir)
	if err != nil {
		return "", fmt.Errorf("resolve extension files dir: %w", err)
	}

	var allowedFolders []string
	if grants != nil {
		allowedFolders = grants.Get(extID).AllowedFolders
	}

	// 1. Sandbox extension /data or /data/... maps to extension-data/<id>/files.
	if rawSrc == "/data" || strings.HasPrefix(rawSrc, "/data/") {
		sub := strings.TrimPrefix(rawSrc, "/data")
		sub = strings.TrimPrefix(sub, "/")
		candidate := realFilesDir
		if sub != "" {
			candidate = filepath.Join(realFilesDir, filepath.FromSlash(sub))
		}
		resolved, err := resolvePathWithAncestor(candidate)
		if err != nil {
			return "", fmt.Errorf("resolve /data bind mount %q: %w", rawSrc, err)
		}
		if !isWithinDir(realFilesDir, resolved) {
			return "", fmt.Errorf("bind mount %q escapes extension files directory", rawSrc)
		}
		if runtime.GOOS == "windows" {
			if gp, err := recipes.GuestPath(resolved); err == nil {
				return gp, nil
			}
		}
		return resolved, nil
	}

	// 2. Windows drive-letter path (e.g. C:\Users\alice\proj) or MSYS /c/... path.
	if isWindowsDrivePath(rawSrc) || (runtime.GOOS == "windows" && isMSYSDrivePath(rawSrc)) {
		return validateWindowsBind(rawSrc, realFilesDir, allowedFolders)
	}

	// 3. Reject non-absolute host paths.
	if !strings.HasPrefix(rawSrc, "/") {
		return "", fmt.Errorf("bind mount source %q must be an absolute path", rawSrc)
	}

	resolvedSrc, err := resolvePathWithAncestor(rawSrc)
	if err != nil {
		return "", fmt.Errorf("bind mount source %q: %w", rawSrc, err)
	}
	if isWithinDir(realFilesDir, resolvedSrc) {
		return resolvedSrc, nil
	}
	for _, folder := range allowedFolders {
		if strings.TrimSpace(folder) == "" || isWindowsDrivePath(folder) {
			continue
		}
		realAllowed, err := resolvePathWithAncestor(folder)
		if err != nil {
			continue
		}
		if isWithinDir(realAllowed, resolvedSrc) {
			return resolvedSrc, nil
		}
	}
	return "", fmt.Errorf("bind mount source %q is outside allowed_folders and extension files directory", rawSrc)
}

func validateWindowsBind(rawSrc, realFilesDir string, allowedFolders []string) (string, error) {
	var guestSrc string
	if isWindowsDrivePath(rawSrc) {
		var err error
		guestSrc, err = recipes.WindowsGuestPath(rawSrc)
		if err != nil {
			return "", fmt.Errorf("invalid windows bind mount %q: %w", rawSrc, err)
		}
	} else {
		guestSrc = canonicalMSYSPath(rawSrc)
		if guestSrc == "" {
			return "", fmt.Errorf("invalid bind mount %q", rawSrc)
		}
	}

	if runtime.GOOS == "windows" {
		winPath := msysToWindowsPath(guestSrc)
		resolved, err := resolvePathWithAncestor(winPath)
		if err != nil {
			return "", fmt.Errorf("bind mount source %q: %w", rawSrc, err)
		}
		gp, err := recipes.WindowsGuestPath(resolved)
		if err != nil {
			return "", fmt.Errorf("bind mount source %q: %w", rawSrc, err)
		}
		if isWithinDir(realFilesDir, resolved) {
			return gp, nil
		}
		for _, folder := range allowedFolders {
			realAllowed, err := resolvePathWithAncestor(folder)
			if err != nil {
				continue
			}
			if isWithinDir(realAllowed, resolved) {
				return gp, nil
			}
		}
		return "", fmt.Errorf("bind mount source %q is outside allowed_folders and extension files directory", rawSrc)
	}

	// Non-Windows host (e.g., cross-platform unit tests for Windows bind normalization).
	for _, folder := range allowedFolders {
		if !isWindowsDrivePath(folder) && !isMSYSDrivePath(folder) {
			continue
		}
		var guestAllowed string
		if isWindowsDrivePath(folder) {
			var err error
			guestAllowed, err = recipes.WindowsGuestPath(folder)
			if err != nil {
				continue
			}
		} else {
			guestAllowed = canonicalMSYSPath(folder)
		}
		if guestAllowed != "" && isWithinSlashDirCaseInsensitive(guestAllowed, guestSrc) {
			return guestSrc, nil
		}
	}
	return "", fmt.Errorf("bind mount source %q is outside allowed_folders", rawSrc)
}

// resolvePathWithAncestor resolves symlinks for p, walking up to the deepest
// existing ancestor when leaf components do not exist yet.
// Any dangling symlink along the path is resolved explicitly (or rejected) so
// dangling symlinks cannot hide an escape outside allowed_folders.
func resolvePathWithAncestor(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", errors.New("invalid path")
	}
	if hasDotDotSegment(p) {
		return "", fmt.Errorf("path %q must not contain '..'", p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return resolveAbsWithHops(filepath.Clean(abs), 0)
}

func resolveAbsWithHops(clean string, hops int) (string, error) {
	if hops > 16 {
		return "", fmt.Errorf("too many symlinks resolving %q", clean)
	}
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(real), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	resolved, _, _, err := resolveSegments("", 0, clean, hops)
	return resolved, err
}

func resolveSegments(baseDir string, baseNonExist int, rawTarget string, hops int) (string, int, bool, error) {
	if hops > 16 {
		return "", 0, false, fmt.Errorf("too many symlinks resolving %q", rawTarget)
	}
	root, hasRoot, segs := splitPathSegments(rawTarget)
	var (
		cur      string
		nonExist int
		isFile   bool
	)
	if hasRoot {
		if root == "" {
			vol := filepath.VolumeName(baseDir)
			if vol != "" {
				root = vol + string(filepath.Separator)
			} else {
				root = string(filepath.Separator)
			}
		}
		cur = root
	} else {
		cur = baseDir
		nonExist = baseNonExist
	}

	for _, seg := range segs {
		if seg == "" || seg == "." {
			if nonExist == 0 && isFile {
				return "", 0, false, fmt.Errorf("not a directory: %s", cur)
			}
			continue
		}
		if seg == ".." {
			if nonExist > 0 {
				cur = filepath.Dir(cur)
				nonExist--
				isFile = false
			} else {
				if isFile {
					return "", 0, false, fmt.Errorf("not a directory: %s", cur)
				}
				cur = filepath.Dir(cur)
			}
			continue
		}
		if nonExist > 0 {
			cur = filepath.Join(cur, seg)
			nonExist++
			continue
		}
		next := filepath.Join(cur, seg)
		fi, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				cur = next
				nonExist = 1
				isFile = false
				continue
			}
			return "", 0, false, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(next)
			if rerr != nil {
				return "", 0, false, rerr
			}
			resolved, nextNonExist, nextIsFile, rerr := resolveSegments(cur, 0, target, hops+1)
			if rerr != nil {
				return "", 0, false, rerr
			}
			cur = resolved
			nonExist = nextNonExist
			isFile = nextIsFile
			continue
		}
		isFile = !fi.IsDir()
		if canonical, eerr := filepath.EvalSymlinks(next); eerr == nil {
			cur = canonical
		} else {
			cur = next
		}
	}
	return filepath.Clean(cur), nonExist, isFile, nil
}

func splitPathSegments(p string) (root string, hasRoot bool, segs []string) {
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	if vol != "" {
		hasRoot = true
		if len(rest) > 0 && (rest[0] == '/' || rest[0] == '\\') {
			root = vol + string(filepath.Separator)
			rest = rest[1:]
		} else {
			root = vol + string(filepath.Separator)
		}
	} else if len(rest) > 0 && (rest[0] == '/' || rest[0] == '\\') {
		hasRoot = true
		if filepath.Separator == '/' {
			root = "/"
		}
		rest = rest[1:]
	}
	segs = strings.FieldsFunc(rest, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	return root, hasRoot, segs
}

func hasDotDotSegment(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 3 && isASCIIAlpha(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isMSYSDrivePath(p string) bool {
	if len(p) < 2 || p[0] != '/' || !isASCIIAlpha(p[1]) {
		return false
	}
	return len(p) == 2 || p[2] == '/'
}

func canonicalMSYSPath(p string) string {
	if !isMSYSDrivePath(p) {
		return ""
	}
	cleaned := path.Clean(p)
	if len(cleaned) < 2 || cleaned[0] != '/' || !isASCIIAlpha(cleaned[1]) {
		return ""
	}
	drive := strings.ToLower(cleaned[1:2])
	if len(cleaned) == 2 {
		return "/" + drive
	}
	if cleaned[2] != '/' {
		return ""
	}
	return "/" + drive + cleaned[2:]
}

func msysToWindowsPath(guest string) string {
	if len(guest) < 2 || guest[0] != '/' {
		return guest
	}
	drive := strings.ToUpper(guest[1:2])
	rest := ""
	if len(guest) > 3 {
		rest = strings.ReplaceAll(guest[3:], "/", `\`)
	}
	return drive + `:\` + rest
}

func isWithinSlashDir(root, target string) bool {
	root = strings.TrimRight(path.Clean(root), "/")
	target = strings.TrimRight(path.Clean(target), "/")
	if root == "" {
		root = "/"
	}
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+"/")
}

func isWithinSlashDirCaseInsensitive(root, target string) bool {
	return isWithinSlashDir(strings.ToLower(root), strings.ToLower(target))
}

func cleanDockerMatchPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if strings.HasPrefix(cleaned, "//") {
		return "/" + strings.TrimLeft(cleaned, "/")
	}
	return cleaned
}

func stripDockerVersion(p string) string {
	if len(p) < 3 || p[0] != '/' || p[1] != 'v' {
		return p
	}
	rest := p[2:]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return p
	}
	ver := rest[:slash]
	major, minor, ok := strings.Cut(ver, ".")
	if !ok || !allDigits(major) || !allDigits(minor) {
		return p
	}
	return rest[slash:]
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func writeDockerJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{Message: msg})
}
