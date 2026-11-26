package ext

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

const (
	maxHostRequestBytes = 8 << 20
	maxCopyBytes        = 64 << 10
	maxOpenURLBytes     = 2048
	maxToastPerMinute   = 12
	maxGestureIntents   = 8
	gestureTokenTTL     = 60 * time.Second
	maxExecOutputBytes  = 1 << 20
)

// gestureStore tracks short-lived per-interaction X-GVX-Gesture-ID tokens.
type gestureStore struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens map[string]*gestureEntry
}

type gestureEntry struct {
	extID     string
	expiresAt time.Time
	used      int
	maxUses   int
	active    bool
}

func newGestureStore(now func() time.Time) *gestureStore {
	if now == nil {
		now = time.Now
	}
	return &gestureStore{
		now:    now,
		tokens: make(map[string]*gestureEntry),
	}
}

func (g *gestureStore) mint(extID string) (string, error) {
	tok, err := randomHexToken(16)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	// Opportunistic sweep of expired tokens.
	for k, e := range g.tokens {
		if !e.active || now.After(e.expiresAt) {
			delete(g.tokens, k)
		}
	}
	g.tokens[tok] = &gestureEntry{
		extID:     extID,
		expiresAt: now.Add(gestureTokenTTL),
		maxUses:   maxGestureIntents,
		active:    true,
	}
	return tok, nil
}

func (g *gestureStore) revoke(tok string) {
	if tok == "" {
		return
	}
	g.mu.Lock()
	if e, ok := g.tokens[tok]; ok {
		e.active = false
		delete(g.tokens, tok)
	}
	g.mu.Unlock()
}

func (g *gestureStore) consume(extID, tok string) error {
	if strings.TrimSpace(tok) == "" {
		return errors.New("missing X-GVX-Gesture-ID header")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.tokens[tok]
	if !ok || !e.active {
		return errors.New("invalid or revoked gesture token")
	}
	if e.extID != extID {
		return errors.New("gesture token does not belong to this extension")
	}
	if g.now().After(e.expiresAt) {
		e.active = false
		delete(g.tokens, tok)
		return errors.New("gesture token expired")
	}
	if e.used >= e.maxUses {
		return fmt.Errorf("gesture token exceeded maximum of %d intents", e.maxUses)
	}
	e.used++
	return nil
}

type hostSSEMsg struct {
	id   uint64
	typ  string
	data []byte
}

type hostEventHub struct {
	mu     sync.Mutex
	seq    uint64
	closed bool
	subs   map[chan hostSSEMsg]struct{}
}

func newHostEventHub() *hostEventHub {
	return &hostEventHub{
		subs: make(map[chan hostSSEMsg]struct{}),
	}
}

func (h *hostEventHub) subscribe() (chan hostSSEMsg, func()) {
	ch := make(chan hostSSEMsg, 64)
	h.mu.Lock()
	if h.closed {
		close(ch)
		h.mu.Unlock()
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if _, ok := h.subs[ch]; ok {
				delete(h.subs, ch)
				close(ch)
			}
			h.mu.Unlock()
		})
	}
}

func (h *hostEventHub) publish(eventType string, payload any) {
	_ = h.publishCount(eventType, payload)
}

func (h *hostEventHub) publishCount(eventType string, payload any) int {
	if eventType == "" {
		return 0
	}
	var data []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0
		}
		data = b
	} else {
		data = []byte("{}")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0
	}
	h.seq++
	msg := hostSSEMsg{id: h.seq, typ: eventType, data: data}
	delivered := 0
	for ch := range h.subs {
		select {
		case ch <- msg:
			delivered++
		default:
			// Slow subscriber; drop oldest or skip to avoid blocking host.
		}
	}
	return delivered
}

func (h *hostEventHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for ch := range h.subs {
		close(ch)
	}
	clear(h.subs)
	h.mu.Unlock()
}

type hostAPIConfig struct {
	extID               string
	dataDir             string
	token               string
	manifest            *Manifest
	grants              *GrantStore
	storage             *Storage
	vault               *Vault
	settings            *SettingsStore
	gestures            *gestureStore
	toastLimiter        *RateLimiter
	hostInfoFn          func() HostInfo
	uiHost              UIHost
	supervisor          *sandbox.Supervisor
	sandboxesMu         *sync.Mutex
	policyMu            *sync.RWMutex
	dockerSockFn        func() string
	dockerBackendDial   func(context.Context) (net.Conn, error)
	dockerEngineHandler func() http.Handler
	onBadge             func(extID string, badge BadgeInfo)
	onFolderPicked      func(extID, folder string)
	onSandboxChange     func(extID string, info extsdk.SandboxInfo)
	events              *hostEventHub
}

type hostAPIServer struct {
	cfg       hostAPIConfig
	mux       *http.ServeMux
	mu        sync.Mutex
	snapMu    sync.Mutex
	snapLocks map[string]*sync.Mutex
	closed    bool
	ln        net.Listener
	srv       *http.Server
	sockPath  string
	closeCtx  context.Context
	closeStop context.CancelFunc
}

func newHostAPIServer(cfg hostAPIConfig) *hostAPIServer {
	if cfg.toastLimiter == nil {
		cfg.toastLimiter = NewRateLimiter(maxToastPerMinute, time.Minute, nil)
	}
	if cfg.events == nil {
		cfg.events = newHostEventHub()
	}
	if cfg.gestures == nil {
		cfg.gestures = newGestureStore(nil)
	}
	if cfg.policyMu == nil {
		cfg.policyMu = &sync.RWMutex{}
	}
	closeCtx, closeStop := context.WithCancel(context.Background())
	s := &hostAPIServer{
		cfg:       cfg,
		snapLocks: make(map[string]*sync.Mutex),
		closeCtx:  closeCtx,
		closeStop: closeStop,
	}
	s.mux = s.buildMux()
	return s
}

func (s *hostAPIServer) snapLock(sbxID string) *sync.Mutex {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snapLocks == nil {
		s.snapLocks = make(map[string]*sync.Mutex)
	}
	m, ok := s.snapLocks[sbxID]
	if !ok {
		m = &sync.Mutex{}
		s.snapLocks[sbxID] = m
	}
	return m
}

func (s *hostAPIServer) Handler() http.Handler {
	return s.requireBearer(s.mux)
}

func (s *hostAPIServer) Listen() (endpoint string, sockPath string, port int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", "", 0, net.ErrClosed
	}
	if s.ln != nil {
		if s.sockPath != "" {
			return "unix://" + s.sockPath, s.sockPath, 0, nil
		}
		if ta, ok := s.ln.Addr().(*net.TCPAddr); ok {
			return "http://" + s.ln.Addr().String(), "", ta.Port, nil
		}
		return "http://" + s.ln.Addr().String(), "", 0, nil
	}

	var ln net.Listener
	if runtime.GOOS != "windows" {
		sockPath, err = NewPaths(s.cfg.dataDir).SocketPath("h", s.cfg.extID)
		if err != nil {
			return "", "", 0, err
		}
		ln, err = listenPrivateUnixSocket(sockPath)
		if err != nil {
			return "", "", 0, err
		}
		endpoint = "unix://" + sockPath
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", "", 0, err
		}
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			port = ta.Port
		}
		endpoint = "http://" + ln.Addr().String()
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return s.closeCtx
		},
	}
	s.ln = ln
	s.srv = srv
	s.sockPath = sockPath
	go func() {
		_ = srv.Serve(ln)
	}()
	return endpoint, sockPath, port, nil
}

func (s *hostAPIServer) isClosed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed
}

func (s *hostAPIServer) Close() error {
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
	s.cfg.events.close()
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
	return nil
}

func (s *hostAPIServer) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/host/info", s.handleHostInfo)
	mux.HandleFunc("GET /v1/settings", s.handleSettings)

	mux.HandleFunc("GET /v1/storage", s.handleStorageList)
	mux.HandleFunc("GET /v1/storage/{key...}", s.handleStorageGet)
	mux.HandleFunc("PUT /v1/storage/{key...}", s.handleStoragePut)
	mux.HandleFunc("DELETE /v1/storage/{key...}", s.handleStorageDelete)

	mux.HandleFunc("GET /v1/secrets", s.handleSecretList)
	mux.HandleFunc("GET /v1/secrets/{key...}", s.handleSecretGet)
	mux.HandleFunc("PUT /v1/secrets/{key...}", s.handleSecretPut)
	mux.HandleFunc("DELETE /v1/secrets/{key...}", s.handleSecretDelete)

	mux.HandleFunc("GET /v1/sandboxes", s.handleSandboxList)
	mux.HandleFunc("POST /v1/sandboxes", s.handleSandboxCreate)
	mux.HandleFunc("POST /v1/sandboxes/{id}/start", s.handleSandboxStart)
	mux.HandleFunc("POST /v1/sandboxes/{id}/stop", s.handleSandboxStop)
	mux.HandleFunc("POST /v1/sandboxes/{id}/delete", s.handleSandboxDelete)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.handleSandboxExec)
	mux.HandleFunc("GET /v1/sandboxes/{id}/logs", s.handleSandboxLogs)
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshots", s.handleSnapshotCreate)
	mux.HandleFunc("GET /v1/sandboxes/{id}/snapshots", s.handleSnapshotList)
	mux.HandleFunc("POST /v1/sandboxes/{id}/restore", s.handleSandboxRestore)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/snapshots/{name}", s.handleSnapshotDelete)

	mux.HandleFunc("POST /v1/ui/badge", s.handleUIBadge)
	mux.HandleFunc("POST /v1/ui/toast", s.handleUIToast)
	mux.HandleFunc("POST /v1/ui/pick-folder", s.handleUIPickFolder)
	mux.HandleFunc("POST /v1/ui/open-terminal", s.handleUIOpenTerminal)
	mux.HandleFunc("POST /v1/ui/open-url", s.handleUIOpenURL)
	mux.HandleFunc("POST /v1/ui/copy", s.handleUICopy)

	mux.HandleFunc("GET /v1/events", s.handleEvents)
	return mux
}

func (s *hostAPIServer) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isClosed() {
			writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closed")
			return
		}
		token := s.cfg.token
		if token == "" {
			writeHostError(w, http.StatusUnauthorized, "unauthorized", "host API token not configured")
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeHostError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		got := auth[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeHostError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *hostAPIServer) hasPermission(perm string) bool {
	if s.cfg.grants == nil || s.cfg.manifest == nil {
		return false
	}
	return s.cfg.grants.HasPermission(s.cfg.extID, perm, s.cfg.manifest.Permissions)
}

func (s *hostAPIServer) requirePermission(w http.ResponseWriter, perm string) bool {
	if !s.hasPermission(perm) {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("permission %q is not granted", perm))
		return false
	}
	return true
}

func (s *hostAPIServer) requireGesture(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.gestures == nil {
		writeHostError(w, http.StatusForbidden, "gesture_required", "gesture store unavailable")
		return false
	}
	gid := r.Header.Get("X-GVX-Gesture-ID")
	if err := s.cfg.gestures.consume(s.cfg.extID, gid); err != nil {
		writeHostError(w, http.StatusForbidden, "gesture_required", err.Error())
		return false
	}
	return true
}

func (s *hostAPIServer) handleHostInfo(w http.ResponseWriter, _ *http.Request) {
	var hi HostInfo
	if s.cfg.hostInfoFn != nil {
		hi = s.cfg.hostInfoFn()
	}
	if hi.OS == "" {
		hi.OS = runtime.GOOS
	}
	if hi.Arch == "" {
		hi.Arch = runtime.GOARCH
	}
	if hi.Locale == "" {
		hi.Locale = "en"
	}
	if hi.Theme == "" {
		hi.Theme = "dark"
	}
	if hi.DockerMode == "" {
		hi.DockerMode = "off"
	}
	writeHostJSON(w, http.StatusOK, hi)
}

func (s *hostAPIServer) handleSettings(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.settings == nil || s.cfg.manifest == nil {
		writeHostJSON(w, http.StatusOK, map[string]any{})
		return
	}
	vals, err := s.cfg.settings.ForHost(s.cfg.manifest)
	if err != nil {
		writeHostError(w, http.StatusInternalServerError, "settings_error", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, vals)
}

func (s *hostAPIServer) handleStorageList(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermStorage) {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	keys, err := s.cfg.storage.Keys(prefix)
	if err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *hostAPIServer) handleStorageGet(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermStorage) {
		return
	}
	key := r.PathValue("key")
	val, ok, err := s.cfg.storage.Get(key)
	if err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !ok {
		writeHostError(w, http.StatusNotFound, "not_found", fmt.Sprintf("storage key %q not found", key))
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]string{"value": val})
}

func (s *hostAPIServer) handleStoragePut(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermStorage) {
		return
	}
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	used, err := s.cfg.storage.Set(key, req.Value)
	if err != nil {
		if strings.Contains(err.Error(), "quota exceeded") {
			writeHostError(w, http.StatusRequestEntityTooLarge, "quota_exceeded", err.Error())
			return
		}
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"used":  used,
		"quota": s.cfg.storage.Quota(),
	})
}

func (s *hostAPIServer) handleStorageDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermStorage) {
		return
	}
	key := r.PathValue("key")
	deleted, err := s.cfg.storage.Delete(key)
	if err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

func (s *hostAPIServer) handleSecretList(w http.ResponseWriter, _ *http.Request) {
	if !s.requirePermission(w, PermSecrets) {
		return
	}
	if s.cfg.vault == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "secret vault unavailable")
		return
	}
	keys, err := s.cfg.vault.ListSecrets()
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			writeHostError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *hostAPIServer) handleSecretGet(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSecrets) {
		return
	}
	if s.cfg.vault == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "secret vault unavailable")
		return
	}
	key := r.PathValue("key")
	var (
		val string
		ok  bool
		err error
	)
	if settingKey, isSetting := strings.CutPrefix(key, VaultSettingPrefix); isSetting {
		if err := CheckSecretName(settingKey); err != nil {
			writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if s.cfg.manifest != nil {
			isSecretSetting := false
			for _, d := range s.cfg.manifest.Settings {
				if d.Key == settingKey && d.Type == SettingSecret {
					isSecretSetting = true
					break
				}
			}
			if !isSecretSetting {
				writeHostError(w, http.StatusNotFound, "not_found", fmt.Sprintf("secret %q not found", key))
				return
			}
		}
		if s.cfg.settings != nil && s.cfg.manifest != nil {
			s.cfg.settings.MigrateLegacySecrets(s.cfg.manifest)
		}
		val, ok, err = s.cfg.vault.GetSettingSecret(settingKey)
	} else {
		val, ok, err = s.cfg.vault.GetSecret(key)
	}
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			writeHostError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !ok {
		writeHostError(w, http.StatusNotFound, "not_found", fmt.Sprintf("secret %q not found", key))
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]string{"value": val})
}

func (s *hostAPIServer) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSecrets) {
		return
	}
	if s.cfg.vault == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "secret vault unavailable")
		return
	}
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.cfg.vault.SetSecret(key, req.Value); err != nil {
		if errors.Is(err, ErrRateLimited) {
			writeHostError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *hostAPIServer) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSecrets) {
		return
	}
	if s.cfg.vault == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "secret vault unavailable")
		return
	}
	key := r.PathValue("key")
	deleted, err := s.cfg.vault.DeleteSecret(key)
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			writeHostError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
			return
		}
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

// --- Sandboxes & Snapshots ---

func (s *hostAPIServer) sandboxStore() store.Store {
	return store.Store{Dir: s.cfg.dataDir}
}

func (s *hostAPIServer) lockSandboxes() {
	if s.cfg.sandboxesMu != nil {
		s.cfg.sandboxesMu.Lock()
	}
}

func (s *hostAPIServer) unlockSandboxes() {
	if s.cfg.sandboxesMu != nil {
		s.cfg.sandboxesMu.Unlock()
	}
}

func (s *hostAPIServer) policyRLock() {
	if s.cfg.policyMu != nil {
		s.cfg.policyMu.RLock()
	}
}

func (s *hostAPIServer) policyRUnlock() {
	if s.cfg.policyMu != nil {
		s.cfg.policyMu.RUnlock()
	}
}

func (s *hostAPIServer) findOwnedSandbox(id string) (sandbox.Spec, bool) {
	if id == "" {
		return sandbox.Spec{}, false
	}
	if s.cfg.supervisor != nil {
		if spec, ok := s.cfg.supervisor.ActiveSpec(id); ok {
			if spec.Labels["io.gvx.extension"] == s.cfg.extID {
				return spec, true
			}
			return sandbox.Spec{}, false
		}
	}
	s.lockSandboxes()
	specs, err := s.sandboxStore().Sandboxes()
	s.unlockSandboxes()
	if err != nil {
		return sandbox.Spec{}, false
	}
	for _, sp := range specs {
		if sp.ID == id {
			if sp.Labels["io.gvx.extension"] == s.cfg.extID {
				return sp, true
			}
			return sandbox.Spec{}, false
		}
	}
	return sandbox.Spec{}, false
}

func (s *hostAPIServer) sandboxInfoFor(spec sandbox.Spec) extsdk.SandboxInfo {
	status := "stopped"
	paused := false
	var ports []string
	if s.cfg.supervisor != nil {
		st := s.cfg.supervisor.State(spec.ID)
		status = strings.ToLower(st.Status.String())
		paused = st.Paused
		for _, p := range st.Ports {
			if p.Host != "" {
				ports = append(ports, fmt.Sprintf("%s:%d/%s", p.Host, p.Guest, p.Proto))
			}
		}
	}
	if len(ports) == 0 && len(spec.Ports) > 0 {
		ports = append([]string(nil), spec.Ports...)
	}
	return extsdk.SandboxInfo{
		ID:         spec.ID,
		Name:       spec.Name,
		Image:      spec.Image,
		Status:     status,
		Paused:     paused,
		Checkpoint: spec.Checkpoint,
		Ports:      ports,
		Recipe:     spec.Recipe,
	}
}

func (s *hostAPIServer) handleSandboxList(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	ownedOnly := r.URL.Query().Get("owned") == "true"
	s.lockSandboxes()
	specs, err := s.sandboxStore().Sandboxes()
	s.unlockSandboxes()
	if err != nil {
		writeHostError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	seen := make(map[string]bool, len(specs))
	out := make([]extsdk.SandboxInfo, 0, len(specs))
	for _, sp := range specs {
		seen[sp.ID] = true
		if ownedOnly && sp.Labels["io.gvx.extension"] != s.cfg.extID {
			continue
		}
		out = append(out, s.sandboxInfoFor(sp))
	}
	if s.cfg.supervisor != nil {
		for _, id := range s.cfg.supervisor.Running() {
			if seen[id] {
				continue
			}
			if sp, ok := s.cfg.supervisor.ActiveSpec(id); ok {
				if ownedOnly && sp.Labels["io.gvx.extension"] != s.cfg.extID {
					continue
				}
				out = append(out, s.sandboxInfoFor(sp))
			}
		}
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

func (s *hostAPIServer) closingOrCancelled(r *http.Request) bool {
	if s.isClosed() {
		return true
	}
	if s.closeCtx != nil && s.closeCtx.Err() != nil {
		return true
	}
	if r != nil && r.Context().Err() != nil {
		return true
	}
	return false
}

func hasDockerSocketForwarding(spec sandbox.Spec) bool {
	if len(spec.UnixForwards) > 0 {
		return true
	}
	for _, e := range spec.Env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.TrimSpace(k) == "DOCKER_HOST" && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func (s *hostAPIServer) validateSandboxSpecCapabilitiesLocked(spec *sandbox.Spec) error {
	if spec == nil {
		return errors.New("missing sandbox spec")
	}
	if err := validateSandboxSpecAllowedMounts(*spec, s.cfg.extID, s.cfg.dataDir, s.cfg.grants); err != nil {
		return err
	}
	netMode := strings.TrimSpace(spec.Network)
	if netMode != "" && netMode != "none" && !s.hasPermission(PermNetwork) {
		return fmt.Errorf("sandbox %q requires %q permission for network %q", spec.ID, PermNetwork, netMode)
	}
	if netMode == "" && !s.hasPermission(PermNetwork) {
		// Force explicit -net none so rungv does not default to NAT without PermNetwork.
		spec.Network = "none"
	}
	if hasDockerSocketForwarding(*spec) && !s.hasPermission(PermDocker) {
		return fmt.Errorf("sandbox %q requires %q permission for Docker socket forwarding", spec.ID, PermDocker)
	}
	return nil
}

func (s *hostAPIServer) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	var req extsdk.CreateSandboxRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := ValidateImageRef(req.Image); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if status, code, msg := s.verifySandboxImageAccessible(r.Context(), req.Image); status != 0 {
		writeHostError(w, status, code, msg)
		return
	}

	for _, p := range req.Ports {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "unix:") {
			writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("unix socket port forward %q is not permitted", p))
			return
		}
	}

	var env []string
	for _, e := range req.Env {
		if !strings.Contains(e, "=") {
			writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("sandbox env entry %q must be KEY=VALUE", e))
			return
		}
		env = append(env, e)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = sandbox.SuggestName(req.Image)
	}

	s.policyRLock()
	policyLocked := true
	unlockPolicy := func() {
		if policyLocked {
			policyLocked = false
			s.policyRUnlock()
		}
	}
	defer unlockPolicy()

	if s.closingOrCancelled(r) {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
		return
	}
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	if req.MountDockerSock && !s.hasPermission(PermDocker) {
		writeHostError(w, http.StatusForbidden, "permission_denied", "mount_docker_sock requires both sandboxes and docker permissions")
		return
	}

	var mounts []sandbox.Mount
	for _, m := range req.Mounts {
		mt := sandbox.MountType(strings.ToLower(strings.TrimSpace(m.Type)))
		if mt == "" {
			mt = sandbox.MountBind
		}
		switch mt {
		case sandbox.MountBind:
			mapped, err := validateAndMapExtensionBind(m.Source, s.cfg.extID, s.cfg.dataDir, s.cfg.grants)
			if err != nil {
				writeHostError(w, http.StatusForbidden, "permission_denied", err.Error())
				return
			}
			mounts = append(mounts, sandbox.Mount{
				Type:     sandbox.MountBind,
				Source:   mapped,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
			})
		case sandbox.MountVolume:
			writeHostError(w, http.StatusForbidden, "permission_denied", "volume mounts are not permitted for extension sandboxes")
			return
		case sandbox.MountTmpfs:
			mounts = append(mounts, sandbox.Mount{
				Type:     sandbox.MountTmpfs,
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
				Size:     m.Size,
			})
		default:
			writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("unsupported mount type %q", m.Type))
			return
		}
	}

	workspace := strings.TrimSpace(req.Workspace)
	if workspace != "" {
		mapped, err := validateAndMapExtensionBind(workspace, s.cfg.extID, s.cfg.dataDir, s.cfg.grants)
		if err != nil {
			writeHostError(w, http.StatusForbidden, "permission_denied", err.Error())
			return
		}
		workspace = mapped
	}

	network := "none"
	if s.hasPermission(PermNetwork) {
		network = "nat"
	}

	checkpoint := true
	if req.Checkpoint != nil {
		checkpoint = *req.Checkpoint
	}

	var unixForwards []string
	if req.MountDockerSock {
		var dEndpoint string
		if s.cfg.dockerSockFn != nil {
			dEndpoint = s.cfg.dockerSockFn()
		}
		if dEndpoint == "" {
			writeHostError(w, http.StatusServiceUnavailable, "unavailable", "scoped docker proxy is not available")
			return
		}
		if strings.HasPrefix(dEndpoint, "unix://") {
			hostSock := strings.TrimPrefix(dEndpoint, "unix://")
			unixForwards = append(unixForwards, "/var/run/docker.sock="+hostSock+",mode=0600")
			env = append(env, "DOCKER_HOST=unix:///var/run/docker.sock")
		} else {
			env = append(env, "DOCKER_HOST="+dEndpoint)
		}
	}

	labels := map[string]string{
		"io.gvx.extension": s.cfg.extID,
	}
	if req.Ephemeral {
		labels["io.gvx.ephemeral"] = "true"
	}

	spec := sandbox.Spec{
		ID:           sandbox.NewID(),
		Name:         name,
		Image:        req.Image,
		Command:      req.Command,
		Env:          env,
		Ports:        append([]string(nil), req.Ports...),
		UnixForwards: unixForwards,
		Mounts:       mounts,
		Workspace:    workspace,
		Memory:       req.Memory,
		CPUs:         req.CPUs,
		Network:      network,
		Checkpoint:   checkpoint,
		CreatedAt:    time.Now(),
		Labels:       labels,
	}
	if err := spec.Validate(); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	shouldStart := req.Start == nil || *req.Start
	if shouldStart && s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}

	s.lockSandboxes()
	specs, err := s.sandboxStore().Sandboxes()
	if err == nil {
		specs = append(specs, spec)
		err = s.sandboxStore().SaveSandboxes(specs)
	}
	s.unlockSandboxes()
	if err != nil {
		writeHostError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	if shouldStart {
		if s.closingOrCancelled(r) {
			s.removeStoredSandbox(spec.ID)
			writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
			return
		}
		if err := s.cfg.supervisor.Start(spec); err != nil {
			s.removeStoredSandbox(spec.ID)
			writeHostError(w, http.StatusInternalServerError, "sandbox_start_failed", err.Error())
			return
		}
	}
	unlockPolicy()

	info := s.sandboxInfoFor(spec)
	if s.cfg.onSandboxChange != nil {
		s.cfg.onSandboxChange(s.cfg.extID, info)
	}
	writeHostJSON(w, http.StatusOK, info)
}

func (s *hostAPIServer) removeStoredSandbox(id string) {
	s.lockSandboxes()
	defer s.unlockSandboxes()
	specs, err := s.sandboxStore().Sandboxes()
	if err != nil {
		return
	}
	next := slices.DeleteFunc(specs, func(sp sandbox.Spec) bool {
		return sp.ID == id
	})
	_ = s.sandboxStore().SaveSandboxes(next)
}

func validateSandboxSpecAllowedMounts(spec sandbox.Spec, extID, dataDir string, grants *GrantStore) error {
	for _, m := range spec.Mounts {
		switch m.Type {
		case sandbox.MountBind, "":
			if _, err := validateAndMapExtensionBind(m.Source, extID, dataDir, grants); err != nil {
				return err
			}
		case sandbox.MountVolume:
			return errors.New("volume mounts are not permitted for extension sandboxes")
		}
	}
	if ws := strings.TrimSpace(spec.Workspace); ws != "" {
		if _, err := validateAndMapExtensionBind(ws, extID, dataDir, grants); err != nil {
			return err
		}
	}
	return nil
}

func validateRestoreManifestAllowedMounts(dir, extID, dataDir string, grants *GrantStore) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > 1<<20 {
		return errors.New("snapshot manifest exceeds 1 MiB limit")
	}
	var m struct {
		Mounts []struct {
			Type    string `json:"type"`
			Dynamic bool   `json:"dynamic"`
			Label   string `json:"label"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("invalid snapshot manifest: %w", err)
	}
	for _, mm := range m.Mounts {
		mt := strings.ToLower(strings.TrimSpace(mm.Type))
		label := strings.TrimSpace(mm.Label)
		if label == "" {
			continue
		}
		if (mt == "bind" && mm.Dynamic) || mt == "rpcfs" {
			if _, err := validateAndMapExtensionBind(label, extID, dataDir, grants); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *hostAPIServer) handleSandboxStart(w http.ResponseWriter, r *http.Request) {
	s.policyRLock()
	policyLocked := true
	unlockPolicy := func() {
		if policyLocked {
			policyLocked = false
			s.policyRUnlock()
		}
	}
	defer unlockPolicy()

	if s.closingOrCancelled(r) {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
		return
	}
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	spec, ok := s.findOwnedSandbox(id)
	if !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if err := s.validateSandboxSpecCapabilitiesLocked(&spec); err != nil {
		writeHostError(w, http.StatusForbidden, "permission_denied", err.Error())
		return
	}
	if s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	if s.closingOrCancelled(r) {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
		return
	}
	if st := s.cfg.supervisor.State(id); !st.Status.Active() {
		if err := s.cfg.supervisor.Start(spec); err != nil {
			writeHostError(w, http.StatusInternalServerError, "sandbox_start_failed", err.Error())
			return
		}
	}
	unlockPolicy()

	info := s.sandboxInfoFor(spec)
	if s.cfg.onSandboxChange != nil {
		s.cfg.onSandboxChange(s.cfg.extID, info)
	}
	writeHostJSON(w, http.StatusOK, info)
}

func (s *hostAPIServer) handleSandboxStop(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	spec, ok := s.findOwnedSandbox(id)
	if !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	if err := s.cfg.supervisor.Stop(id, 5*time.Second); err != nil {
		writeHostError(w, http.StatusInternalServerError, "sandbox_stop_failed", err.Error())
		return
	}
	info := s.sandboxInfoFor(spec)
	if s.cfg.onSandboxChange != nil {
		s.cfg.onSandboxChange(s.cfg.extID, info)
	}
	writeHostJSON(w, http.StatusOK, info)
}

func (s *hostAPIServer) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	sbxMu := s.snapLock(id)
	sbxMu.Lock()
	defer sbxMu.Unlock()

	_, ok := s.findOwnedSandbox(id)
	if !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	snapPath := s.snapshotIndexPath(id)
	if err := checkStrictChildNoSymlinkWithin(s.cfg.dataDir, snapPath); err != nil {
		writeHostError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	if s.cfg.supervisor != nil {
		snapRoot := s.cfg.supervisor.SnapshotRoot(id)
		if supDataDir := s.cfg.supervisor.DataDir; supDataDir != "" {
			if err := checkStrictChildNoSymlinkWithin(supDataDir, snapRoot); err != nil {
				writeHostError(w, http.StatusInternalServerError, "delete_failed", err.Error())
				return
			}
		}
		_ = s.cfg.supervisor.Stop(id, 5*time.Second)
		s.cfg.supervisor.Forget(id)
		if supDataDir := s.cfg.supervisor.DataDir; supDataDir != "" {
			_ = removeAllNoSymlinkWithin(supDataDir, snapRoot)
		} else {
			_ = os.RemoveAll(snapRoot)
		}
	}
	if err := removeFileNoSymlinkWithin(s.cfg.dataDir, snapPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeHostError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	s.removeStoredSandbox(id)
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *hostAPIServer) handleSandboxExec(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findOwnedSandbox(id); !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	var req extsdk.ExecRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Argv) == 0 {
		writeHostError(w, http.StatusBadRequest, "bad_request", "argv is required")
		return
	}
	for _, e := range req.Env {
		if !strings.Contains(e, "=") {
			writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("exec env %q must be KEY=VALUE", e))
			return
		}
	}
	res, status, code, err := execInSandbox(r.Context(), s.cfg.supervisor, id, req.Argv, req.Workdir, req.Env, req.TimeoutSec)
	if err != nil {
		writeHostError(w, status, code, err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, res)
}

// restoreExecRetryDelays bounds how long post-Restore exec retries when a
// freshly restored sandbox's control socket has not yet finished binding.
var restoreExecRetryDelays = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	time.Second,
	time.Second,
	time.Second,
}

func execInSandbox(
	ctx context.Context,
	sup *sandbox.Supervisor,
	id string,
	argv []string,
	workdir string,
	env []string,
	timeoutSec int,
) (extsdk.ExecResult, int, string, error) {
	return execInSandboxWithRetry(ctx, sup, id, argv, workdir, env, timeoutSec, nil)
}

func execInSandboxWithRetry(
	ctx context.Context,
	sup *sandbox.Supervisor,
	id string,
	argv []string,
	workdir string,
	env []string,
	timeoutSec int,
	retryDelays []time.Duration,
) (extsdk.ExecResult, int, string, error) {
	execCtx := ctx
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	var lastErr error
	lastStatus := http.StatusInternalServerError
	lastCode := "exec_failed"
	for attempt := 0; ; attempt++ {
		ctl, ok := sup.Control(id)
		if !ok {
			if lastErr != nil {
				return extsdk.ExecResult{}, lastStatus, lastCode, lastErr
			}
			return extsdk.ExecResult{}, http.StatusServiceUnavailable, "unavailable", fmt.Errorf("sandbox %q is not running with control socket", id)
		}
		res, status, code, err := execInSandboxOnce(execCtx, ctl, argv, workdir, env)
		if err == nil {
			return res, status, code, nil
		}
		if execCtx.Err() != nil {
			return extsdk.ExecResult{}, http.StatusInternalServerError, "exec_failed", execCtx.Err()
		}
		if !rungv.IsUnreachable(err) || attempt >= len(retryDelays) {
			return res, status, code, err
		}
		if !sup.State(id).Status.Active() {
			return res, status, code, err
		}
		lastErr = err
		lastStatus = status
		lastCode = code
		timer := time.NewTimer(retryDelays[attempt])
		select {
		case <-execCtx.Done():
			timer.Stop()
			return extsdk.ExecResult{}, http.StatusInternalServerError, "exec_failed", execCtx.Err()
		case <-timer.C:
		}
		if !sup.State(id).Status.Active() {
			return res, status, code, err
		}
	}
}

func execInSandboxOnce(
	execCtx context.Context,
	ctl sandbox.Control,
	argv []string,
	workdir string,
	env []string,
) (extsdk.ExecResult, int, string, error) {
	proc, err := ctl.Exec(execCtx, sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:    argv,
			Workdir: workdir,
			Env:     env,
		},
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrPaused) {
			return extsdk.ExecResult{}, http.StatusConflict, "paused", err
		}
		return extsdk.ExecResult{}, http.StatusInternalServerError, "exec_failed", err
	}
	defer proc.Close()

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.limit = maxExecOutputBytes
	stderrBuf.limit = maxExecOutputBytes

	var wg sync.WaitGroup
	if rOut := proc.Stdout(); rOut != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(&stdoutBuf, rOut)
		}()
	}
	if rErr := proc.Stderr(); rErr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(&stderrBuf, rErr)
		}()
	}

	code, waitErr := proc.Wait()
	wg.Wait()
	if waitErr != nil {
		return extsdk.ExecResult{}, http.StatusInternalServerError, "exec_failed", waitErr
	}
	return extsdk.ExecResult{
		ExitCode:  code,
		Stdout:    stdoutBuf.String(),
		Stderr:    stderrBuf.String(),
		Truncated: stdoutBuf.truncated || stderrBuf.truncated,
	}, http.StatusOK, "", nil
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	rem := b.limit - b.buf.Len()
	if rem > 0 {
		take := min(len(p), rem)
		_, _ = b.buf.Write(p[:take])
		if take < len(p) {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

func (s *hostAPIServer) handleSandboxLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findOwnedSandbox(id); !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	tail := 200
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		if n, err := strconv.Atoi(rawTail); err == nil && n > 0 {
			tail = min(n, 5000)
		}
	}
	lb := s.cfg.supervisor.Logs(id)
	if r.URL.Query().Get("follow") != "true" {
		lines := lb.Tail(tail, 255)
		writeHostJSON(w, http.StatusOK, map[string]string{"logs": strings.Join(lines, "\n")})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeHostError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	snap := lb.Snapshot()
	if len(snap) > tail {
		snap = snap[len(snap)-tail:]
	}
	var lastSeq int64
	for _, ln := range snap {
		lastSeq = ln.Seq
		b, _ := json.Marshal(map[string]string{"line": ln.Text})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}
	flusher.Flush()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			lines := lb.Since(lastSeq)
			if len(lines) == 0 {
				continue
			}
			for _, ln := range lines {
				lastSeq = ln.Seq
				b, _ := json.Marshal(map[string]string{"line": ln.Text})
				if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}

func (s *hostAPIServer) snapshotIndexPath(sbxID string) string {
	return filepath.Join(ExtensionDataDir(s.cfg.dataDir, s.cfg.extID), "snapshots", sbxID+".json")
}

func (s *hostAPIServer) loadSnapshotIndex(sbxID string) map[string]string {
	out := map[string]string{}
	b, err := ReadFileNoSymlinkWithin(s.cfg.dataDir, s.snapshotIndexPath(sbxID))
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (s *hostAPIServer) saveSnapshotIndex(sbxID string, idx map[string]string) {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return
	}
	_ = WriteFileAtomicWithin(s.cfg.dataDir, s.snapshotIndexPath(sbxID), append(b, '\n'), 0o600)
}

func validSnapshotName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\:`+"\x00") || strings.ContainsFunc(name, uiblock.Spoofing) {
		return false
	}
	return utf8.ValidString(name)
}

func (s *hostAPIServer) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSnapshots) {
		return
	}
	id := r.PathValue("id")
	if s.cfg.supervisor == nil {
		if _, ok := s.findOwnedSandbox(id); !ok {
			writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
			return
		}
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	var req extsdk.CreateSnapshotRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = time.Now().Format("20060102-150405")
	}
	if !validSnapshotName(name) {
		writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("invalid snapshot name %q", req.Name))
		return
	}

	sbxMu := s.snapLock(id)
	sbxMu.Lock()
	defer sbxMu.Unlock()

	spec, ok := s.findOwnedSandbox(id)
	if !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}

	dir, err := s.cfg.supervisor.Checkpoint(r.Context(), id, req.LeaveRunning)
	if err != nil {
		writeHostError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	base := filepath.Base(dir)
	idx := s.loadSnapshotIndex(id)
	idx[name] = base
	s.saveSnapshotIndex(id, idx)

	var size int64
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				size += info.Size()
			}
		}
	}
	writeHostJSON(w, http.StatusOK, extsdk.SnapshotInfo{
		SandboxID:   id,
		SandboxName: spec.Name,
		Name:        name,
		Dir:         dir,
		Size:        size,
		CreatedAt:   time.Now(),
	})
}

func (s *hostAPIServer) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSnapshots) {
		return
	}
	id := r.PathValue("id")
	sbxMu := s.snapLock(id)
	sbxMu.Lock()
	defer sbxMu.Unlock()

	spec, ok := s.findOwnedSandbox(id)
	if !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if s.cfg.supervisor == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	snaps := s.cfg.supervisor.Snapshots(id)
	idx := s.loadSnapshotIndex(id)
	rev := make(map[string]string, len(idx))
	for friendly, base := range idx {
		rev[base] = friendly
	}
	out := make([]extsdk.SnapshotInfo, 0, len(snaps))
	for _, sn := range snaps {
		name := sn.Name
		if friendly, ok := rev[filepath.Base(sn.Dir)]; ok {
			name = friendly
		}
		out = append(out, extsdk.SnapshotInfo{
			SandboxID:   id,
			SandboxName: spec.Name,
			Name:        name,
			Dir:         sn.Dir,
			Size:        sn.Size,
			CreatedAt:   sn.Time,
		})
	}
	writeHostJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

func (s *hostAPIServer) resolveSnapshotDirLocked(sbxID, name string) (string, bool) {
	if s.cfg.supervisor == nil || !validSnapshotName(name) {
		return "", false
	}
	snaps := s.cfg.supervisor.Snapshots(sbxID)
	idx := s.loadSnapshotIndex(sbxID)
	targetBase := idx[name]
	for _, sn := range snaps {
		base := filepath.Base(sn.Dir)
		if (targetBase != "" && base == targetBase) || sn.Name == name || base == name {
			return sn.Dir, true
		}
	}
	return "", false
}

func (s *hostAPIServer) handleSandboxRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSnapshots) || !s.requirePermission(w, PermSandboxes) {
		return
	}
	id := r.PathValue("id")
	var req extsdk.RestoreRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for _, e := range req.Env {
		if !strings.Contains(e, "=") {
			writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("restore env %q must be KEY=VALUE", e))
			return
		}
	}

	sbxMu := s.snapLock(id)
	sbxMu.Lock()
	s.policyRLock()
	if s.closingOrCancelled(r) {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
		return
	}
	if !s.requirePermission(w, PermSnapshots) || !s.requirePermission(w, PermSandboxes) {
		s.policyRUnlock()
		sbxMu.Unlock()
		return
	}
	spec, ok := s.findOwnedSandbox(id)
	if !ok {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	if err := s.validateSandboxSpecCapabilitiesLocked(&spec); err != nil {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusForbidden, "permission_denied", err.Error())
		return
	}
	if s.cfg.supervisor == nil {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "sandbox supervisor is unavailable")
		return
	}
	dir, ok := s.resolveSnapshotDirLocked(id, strings.TrimSpace(req.Snapshot))
	if !ok {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusNotFound, "not_found", fmt.Sprintf("snapshot %q not found", req.Snapshot))
		return
	}
	if err := validateRestoreManifestAllowedMounts(dir, s.cfg.extID, s.cfg.dataDir, s.cfg.grants); err != nil {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusForbidden, "permission_denied", err.Error())
		return
	}
	if s.closingOrCancelled(r) {
		s.policyRUnlock()
		sbxMu.Unlock()
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closing")
		return
	}

	_ = s.cfg.supervisor.Stop(id, 5*time.Second)
	err := s.cfg.supervisor.Restore(spec, dir)
	s.policyRUnlock()
	sbxMu.Unlock()
	if err != nil {
		writeHostError(w, http.StatusInternalServerError, "restore_failed", err.Error())
		return
	}
	res := extsdk.RestoreResult{
		Sandbox: s.sandboxInfoFor(spec),
	}
	if len(req.Exec) > 0 {
		execRes, status, code, err := execInSandboxWithRetry(r.Context(), s.cfg.supervisor, id, req.Exec, "", req.Env, 60, restoreExecRetryDelays)
		if err != nil {
			writeHostError(w, status, code, err.Error())
			return
		}
		res.Exec = &execRes
		res.Sandbox = s.sandboxInfoFor(spec)
	}
	if s.cfg.onSandboxChange != nil {
		s.cfg.onSandboxChange(s.cfg.extID, res.Sandbox)
	}
	writeHostJSON(w, http.StatusOK, res)
}

func (s *hostAPIServer) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermSnapshots) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")

	sbxMu := s.snapLock(id)
	sbxMu.Lock()
	defer sbxMu.Unlock()

	if _, ok := s.findOwnedSandbox(id); !ok {
		writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", id))
		return
	}
	dir, ok := s.resolveSnapshotDirLocked(id, name)
	if !ok {
		writeHostError(w, http.StatusNotFound, "not_found", fmt.Sprintf("snapshot %q not found", name))
		return
	}
	root := s.cfg.supervisor.SnapshotRoot(id)
	if !isWithinDir(root, dir) || dir == root {
		writeHostError(w, http.StatusBadRequest, "bad_request", "invalid snapshot directory")
		return
	}
	checkRoot := root
	if s.cfg.supervisor.DataDir != "" {
		checkRoot = s.cfg.supervisor.DataDir
	}
	if err := removeAllNoSymlinkWithin(checkRoot, dir); err != nil {
		writeHostError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	idx := s.loadSnapshotIndex(id)
	base := filepath.Base(dir)
	delete(idx, name)
	for k, v := range idx {
		if v == base {
			delete(idx, k)
		}
	}
	s.saveSnapshotIndex(id, idx)
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- UI Bridge ---

func (s *hostAPIServer) handleUIBadge(w http.ResponseWriter, r *http.Request) {
	var req extsdk.BadgeRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !utf8.ValidString(req.Text) || !utf8.ValidString(req.Tooltip) {
		writeHostError(w, http.StatusBadRequest, "bad_request", "badge text must be valid UTF-8")
		return
	}
	if !slices.Contains(uiblock.Tones, req.Tone) {
		writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("invalid badge tone %q", req.Tone))
		return
	}
	text := strings.TrimSpace(uiblock.Printable(req.Text))
	if utf8.RuneCountInString(text) > 32 {
		runes := []rune(text)
		text = string(runes[:32])
	}
	tooltip := strings.TrimSpace(uiblock.Printable(req.Tooltip))
	if utf8.RuneCountInString(tooltip) > 160 {
		runes := []rune(tooltip)
		tooltip = string(runes[:160])
	}
	badge := BadgeInfo{
		Text:    text,
		Tone:    req.Tone,
		Tooltip: tooltip,
	}
	if s.cfg.onBadge != nil {
		s.cfg.onBadge(s.cfg.extID, badge)
	}
	if s.cfg.uiHost != nil {
		s.cfg.uiHost.Changed()
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *hostAPIServer) handleUIToast(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !utf8.ValidString(req.Message) || strings.TrimSpace(req.Message) == "" {
		writeHostError(w, http.StatusBadRequest, "bad_request", "toast message must be non-empty UTF-8")
		return
	}
	if !s.cfg.toastLimiter.Allow() {
		writeHostError(w, http.StatusTooManyRequests, "rate_limited", "toast rate limit exceeded (12/minute)")
		return
	}
	level := strings.ToLower(strings.TrimSpace(req.Level))
	switch level {
	case "", "info", "success", "warning", "error", "danger":
	default:
		level = "info"
	}
	if level == "" {
		level = "info"
	}
	msg := uiblock.Printable(req.Message)
	if utf8.RuneCountInString(msg) > 300 {
		msg = string([]rune(msg)[:300])
	}
	if s.cfg.uiHost != nil {
		s.cfg.uiHost.ShowToast(level, msg)
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *hostAPIServer) handleUIPickFolder(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermUIPickFolder) {
		return
	}
	if !s.requireGesture(w, r) {
		return
	}
	if s.cfg.uiHost == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "UI host is unavailable")
		return
	}
	dir, ok, err := s.cfg.uiHost.PickFolder(r.Context(), s.cfg.extID)
	if err != nil {
		if errors.Is(err, context.Canceled) && !s.isClosed() && r.Context().Err() == nil {
			writeHostJSON(w, http.StatusOK, map[string]any{"cancelled": true, "path": ""})
			return
		}
		writeHostError(w, http.StatusInternalServerError, "pick_folder_failed", err.Error())
		return
	}
	if s.isClosed() || r.Context().Err() != nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "host API server is closed")
		return
	}
	if !ok || strings.TrimSpace(dir) == "" {
		writeHostJSON(w, http.StatusOK, map[string]any{"cancelled": true, "path": ""})
		return
	}
	canonical, err := s.cfg.grants.AddAllowedFolder(s.cfg.extID, dir)
	if err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.cfg.onFolderPicked != nil {
		s.cfg.onFolderPicked(s.cfg.extID, canonical)
	}
	s.cfg.uiHost.Changed()
	writeHostJSON(w, http.StatusOK, map[string]any{"cancelled": false, "path": canonical})
}

func (s *hostAPIServer) handleUIOpenTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermUITerminal) {
		return
	}
	if !s.requireGesture(w, r) {
		return
	}
	if s.cfg.uiHost == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "UI host is unavailable")
		return
	}
	var req extsdk.OpenTerminalRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.TargetType != "container" && req.TargetType != "sandbox" {
		writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("unsupported target_type %q", req.TargetType))
		return
	}
	targetID := strings.TrimSpace(req.TargetID)
	if targetID == "" || targetID == "." || targetID == ".." ||
		strings.Contains(targetID, "..") ||
		strings.ContainsAny(targetID, "/\\?#%") ||
		strings.ContainsFunc(targetID, uiblock.Spoofing) {
		writeHostError(w, http.StatusBadRequest, "bad_request", "invalid target_id")
		return
	}
	if strings.ContainsFunc(req.Shell, uiblock.Spoofing) {
		writeHostError(w, http.StatusBadRequest, "bad_request", "invalid shell")
		return
	}

	switch req.TargetType {
	case "sandbox":
		if _, ok := s.findOwnedSandbox(targetID); !ok {
			writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("sandbox %q is not owned by this extension", targetID))
			return
		}
	case "container":
		if !s.hasPermission(PermDocker) {
			writeHostError(w, http.StatusForbidden, "permission_denied", fmt.Sprintf("extension %q lacks %q permission for container terminal", s.cfg.extID, PermDocker))
			return
		}
		if status, code, msg := s.verifyOwnedContainer(r.Context(), targetID); status != 0 {
			writeHostError(w, status, code, msg)
			return
		}
	}

	if err := s.cfg.uiHost.OpenTerminal(req.TargetType, targetID, req.Shell); err != nil {
		writeHostError(w, http.StatusInternalServerError, "open_terminal_failed", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func isForeignExtensionImageRef(imgRef, ownExtID string) bool {
	ref := strings.TrimSpace(imgRef)
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		if len(ref) > len(prefix) && strings.EqualFold(ref[:len(prefix)], prefix) {
			ref = ref[len(prefix):]
			break
		}
	}
	if len(ref) < len("gvx-ext/") || !strings.EqualFold(ref[:len("gvx-ext/")], "gvx-ext/") {
		return false
	}
	rest := ref[len("gvx-ext/"):]
	id := rest
	if idx := strings.IndexAny(id, ":@"); idx >= 0 {
		id = id[:idx]
	}
	return id != ownExtID
}

func (s *hostAPIServer) verifySandboxImageAccessible(ctx context.Context, imgRef string) (int, string, string) {
	if isForeignExtensionImageRef(imgRef, s.cfg.extID) {
		return http.StatusForbidden, "permission_denied", fmt.Sprintf("image %q is not accessible to extension %q", imgRef, s.cfg.extID)
	}
	if s.cfg.dockerEngineHandler == nil && s.cfg.dockerBackendDial == nil {
		return 0, "", ""
	}
	ref := strings.TrimSpace(imgRef)
	if ref == "" || ref == "." || ref == ".." || hasDotDotSegment(ref) || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) {
		return http.StatusBadRequest, "bad_request", fmt.Sprintf("invalid image reference %q", imgRef)
	}
	if st, code, msg := s.verifySingleSandboxImageAccessible(ctx, ref); st != 0 {
		return st, code, msg
	}
	if tagPart, digestPart, hasDigest := strings.Cut(ref, "@"); hasDigest {
		if tp := strings.TrimSpace(tagPart); tp != "" {
			if isForeignExtensionImageRef(tp, s.cfg.extID) {
				return http.StatusForbidden, "permission_denied", fmt.Sprintf("image %q is not accessible to extension %q", imgRef, s.cfg.extID)
			}
			if st, code, msg := s.verifySingleSandboxImageAccessible(ctx, tp); st != 0 {
				return st, code, msg
			}
		}
		if dp := strings.TrimSpace(digestPart); dp != "" {
			if st, code, msg := s.verifySingleSandboxImageAccessible(ctx, dp); st != 0 {
				return st, code, msg
			}
		}
	}
	return 0, "", ""
}

func (s *hostAPIServer) verifySingleSandboxImageAccessible(ctx context.Context, ref string) (int, string, string) {
	if ref == "" || ref == "." || ref == ".." || hasDotDotSegment(ref) || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) {
		return http.StatusBadRequest, "bad_request", fmt.Sprintf("invalid image reference %q", ref)
	}
	reqPath := "/images/" + escapeImagePathSegments(ref) + "/json"
	var (
		status int
		body   []byte
	)
	if s.cfg.dockerEngineHandler != nil {
		if h := s.cfg.dockerEngineHandler(); h != nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+reqPath, nil)
			if err != nil {
				return http.StatusBadRequest, "bad_request", err.Error()
			}
			rec := newDockerMemResponseWriter()
			h.ServeHTTP(rec, req)
			status = rec.status
			if status == 0 {
				status = http.StatusOK
			}
			body = rec.body.Bytes()
		}
	}
	if status == 0 && s.cfg.dockerBackendDial != nil {
		tr := &http.Transport{
			DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
				return s.cfg.dockerBackendDial(c)
			},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		cl := &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+reqPath, nil)
		if err != nil {
			return http.StatusBadRequest, "bad_request", err.Error()
		}
		resp, err := cl.Do(req)
		if err != nil {
			// Docker backend is stopped/unreachable; non-gvx-ext/<other> sandboxes may still run via rungv.
			return 0, "", ""
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		status = resp.StatusCode
	}
	if status == 0 || status == http.StatusServiceUnavailable {
		return 0, "", ""
	}
	if status == http.StatusNotFound {
		return 0, "", ""
	}
	if status == http.StatusBadRequest {
		return http.StatusBadRequest, "bad_request", fmt.Sprintf("invalid image reference %q", ref)
	}
	if status != http.StatusOK {
		return http.StatusServiceUnavailable, "unavailable", fmt.Sprintf("docker backend returned unexpected status %d inspecting image %q", status, ref)
	}
	var inspect struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &inspect); err != nil {
		return http.StatusServiceUnavailable, "unavailable", "invalid image inspect response from docker backend"
	}
	owner := inspect.Config.Labels["io.gvx.extension"]
	if owner != "" && owner != s.cfg.extID {
		return http.StatusForbidden, "permission_denied", fmt.Sprintf("image %q is not accessible to extension %q", ref, s.cfg.extID)
	}
	return 0, "", ""
}

func (s *hostAPIServer) verifyOwnedContainer(ctx context.Context, containerID string) (int, string, string) {
	reqPath := "/containers/" + url.PathEscape(containerID) + "/json"
	var (
		status int
		body   []byte
	)
	if s.cfg.dockerEngineHandler != nil {
		if h := s.cfg.dockerEngineHandler(); h != nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+reqPath, nil)
			if err != nil {
				return http.StatusBadRequest, "bad_request", err.Error()
			}
			rec := newDockerMemResponseWriter()
			h.ServeHTTP(rec, req)
			status = rec.status
			if status == 0 {
				status = http.StatusOK
			}
			body = rec.body.Bytes()
		}
	}
	if status == 0 {
		if s.cfg.dockerBackendDial == nil {
			return http.StatusServiceUnavailable, "unavailable", "docker backend is unavailable"
		}
		tr := &http.Transport{
			DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
				return s.cfg.dockerBackendDial(c)
			},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		cl := &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+reqPath, nil)
		if err != nil {
			return http.StatusBadRequest, "bad_request", err.Error()
		}
		resp, err := cl.Do(req)
		if err != nil {
			return http.StatusServiceUnavailable, "unavailable", "docker backend is unavailable"
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		status = resp.StatusCode
	}

	if status == http.StatusServiceUnavailable {
		return http.StatusServiceUnavailable, "unavailable", "docker backend is unavailable"
	}
	if status != http.StatusOK {
		return http.StatusForbidden, "permission_denied", fmt.Sprintf("container %q is not owned by this extension", containerID)
	}
	var inspect struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &inspect); err != nil || inspect.Config.Labels["io.gvx.extension"] != s.cfg.extID {
		return http.StatusForbidden, "permission_denied", fmt.Sprintf("container %q is not owned by this extension", containerID)
	}
	return 0, "", ""
}

func (s *hostAPIServer) handleUIOpenURL(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, PermUIOpenURL) {
		return
	}
	if !s.requireGesture(w, r) {
		return
	}
	if s.cfg.uiHost == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "UI host is unavailable")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := ValidateOpenURL(req.URL); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.cfg.uiHost.OpenURL(req.URL); err != nil {
		writeHostError(w, http.StatusInternalServerError, "open_url_failed", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ValidateOpenURL verifies that raw is a safe, bounded HTTP(S) URL without
// userinfo credentials or control/spoofing runes.
func ValidateOpenURL(raw string) error {
	if raw == "" || len(raw) > maxOpenURLBytes || !utf8.ValidString(raw) {
		return fmt.Errorf("URL must be non-empty UTF-8 of at most %d bytes", maxOpenURLBytes)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f || uiblock.Spoofing(r) }) {
		return errors.New("URL contains control or spoofing characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme %q is not allowed (only http or https)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("URL must include a non-empty host")
	}
	if u.User != nil {
		return errors.New("URL must not contain userinfo credentials")
	}
	return nil
}

func (s *hostAPIServer) handleUICopy(w http.ResponseWriter, r *http.Request) {
	if !s.requireGesture(w, r) {
		return
	}
	if s.cfg.uiHost == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "UI host is unavailable")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeHostError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !utf8.ValidString(req.Text) || len(req.Text) > maxCopyBytes {
		writeHostError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("copy text must be valid UTF-8 of at most %d KiB", maxCopyBytes>>10))
		return
	}
	if err := s.cfg.uiHost.CopyText(req.Text); err != nil {
		writeHostError(w, http.StatusInternalServerError, "copy_failed", err.Error())
		return
	}
	writeHostJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *hostAPIServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeHostError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsub := s.cfg.events.subscribe()
	defer unsub()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", msg.typ, msg.id, msg.data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func decodeJSONBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxHostRequestBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxHostRequestBytes {
		return fmt.Errorf("request body exceeds %d MiB limit", maxHostRequestBytes>>20)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("empty JSON body")
	}
	if !utf8.Valid(data) {
		return errors.New("request body must be valid UTF-8")
	}
	if hasLoneSurrogateEscape(data) {
		return errors.New("request body contains unpaired UTF-16 surrogate")
	}
	return json.Unmarshal(data, dst)
}

// hasLoneSurrogateEscape scans JSON byte data for unpaired \uD800..\uDFFF
// escape sequences inside JSON strings (which encoding/json silently coerces to U+FFFD),
// while permitting valid surrogate pairs (\uD800..\uDBFF followed by \uDC00..\uDFFF),
// escaped backslashes (\\uD800), and explicit \uFFFD.
func hasLoneSurrogateEscape(data []byte) bool {
	inString := false
	for i := 0; i < len(data); {
		b := data[i]
		if !inString {
			if b == '"' {
				inString = true
			}
			i++
			continue
		}
		if b == '"' {
			inString = false
			i++
			continue
		}
		if b != '\\' {
			i++
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i += 2
			continue
		}
		if i+6 > len(data) {
			i += 2
			continue
		}
		u1, ok := parseHex4(data[i+2 : i+6])
		if !ok {
			i += 2
			continue
		}
		if u1 >= 0xD800 && u1 <= 0xDBFF {
			if i+12 <= len(data) && data[i+6] == '\\' && data[i+7] == 'u' {
				if u2, ok2 := parseHex4(data[i+8 : i+12]); ok2 && u2 >= 0xDC00 && u2 <= 0xDFFF {
					i += 12
					continue
				}
			}
			return true
		}
		if u1 >= 0xDC00 && u1 <= 0xDFFF {
			return true
		}
		i += 6
	}
	return false
}

func parseHex4(b []byte) (uint16, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := b[i]
		var nib uint16
		switch {
		case c >= '0' && c <= '9':
			nib = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			nib = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			nib = uint16(c-'A') + 10
		default:
			return 0, false
		}
		v = (v << 4) | nib
	}
	return v, true
}

func writeHostJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeHostError(w http.ResponseWriter, status int, code, message string) {
	writeHostJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    code,
		Message: message,
	})
}
