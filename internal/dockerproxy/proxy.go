// Package dockerproxy serves the host-facing Docker Engine API endpoint and
// forwards requests to the sandbox's private dockerd Unix socket.
package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotRunningMessage is returned with HTTP 503 when dockerd's backend socket
// is not reachable.
const ErrNotRunningMessage = "The Docker Engine sandbox is not running. Start it in GVX Desktop."

const (
	maxCheckBody         = 4 << 20 // 4 MiB cap for buffering POST /containers/create
	maxHijackBody        = 1 << 20 // 1 MiB cap for buffering hijacked request bodies (e.g. exec start)
	maxResponseHeadBytes = 1 << 20
	defaultChangedWindow = 300 * time.Millisecond
	staleDialTimeout     = 500 * time.Millisecond
)

// DialAddress returns a dial function connecting to network and address.
func DialAddress(network, address string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
}

// Proxy listens on the user-facing Docker endpoint and forwards traffic to the
// sandbox's private dockerd backend.
type Proxy struct {
	// Check is called for buffered requests (POST /containers/create up to
	// 4 MiB). Returning a non-nil error sends a Docker-style 400 JSON error
	// and prevents the request from reaching the backend.
	Check func(r *http.Request, body []byte) error

	// Rewrite, when set, is called before Check with the body of the same
	// requests, and may return another body to send in its place; nil
	// keeps the one received.
	Rewrite func(r *http.Request, body []byte) []byte

	// Changed is invoked asynchronously after successful mutating requests,
	// with bursts coalesced over ChangedDebounce (default 300ms).
	Changed func()

	// ChangedDebounce is the coalescing window for Changed notifications.
	// Values <= 0 use the default of 300ms.
	ChangedDebounce time.Duration

	local atomic.Pointer[http.Handler]

	dial      func(ctx context.Context) (net.Conn, error)
	transport *http.Transport
	rp        *httputil.ReverseProxy

	mu          sync.Mutex
	closed      bool
	ln          net.Listener
	srv         *http.Server
	network     string
	address     string
	sockPath    string
	changeTimer *time.Timer
	tracked     map[net.Conn]struct{}
}

// New returns a Proxy forwarding to connections opened by dial.
func New(dial func(ctx context.Context) (net.Conn, error)) *Proxy {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		DisableCompression:  true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	p := &Proxy{
		dial:      dial,
		transport: tr,
		tracked:   map[net.Conn]struct{}{},
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			if pr.In.Host != "" {
				pr.Out.Host = pr.In.Host
			} else {
				pr.Out.Host = "docker"
			}
			// Do not invent a User-Agent header when the client sent none.
			if _, ok := pr.In.Header["User-Agent"]; !ok {
				pr.Out.Header["User-Agent"] = nil
			}
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			writeDockerError(w, r, http.StatusServiceUnavailable, ErrNotRunningMessage)
		},
	}
	return p
}

// Listen starts or moves the proxy listener to network and address, replacing
// any earlier listener. For Unix sockets, an existing entry is replaced only
// when it is a socket file whose dial fails with connection refused; symlinks,
// regular files, live sockets, and other errors are left untouched.
func (p *Proxy) Listen(network, address string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return net.ErrClosed
	}
	if p.ln != nil && p.network == network && p.address == address {
		return nil
	}
	p.stopListenerLocked()

	var (
		ln       net.Listener
		sockPath string
		err      error
	)
	if network == "unix" {
		ln, err = listenUnix(address)
		sockPath = address
	} else {
		ln, err = net.Listen(network, address)
	}
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:  p,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	p.ln = ln
	p.srv = srv
	p.network = network
	p.address = address
	p.sockPath = sockPath
	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

// listenUnix binds a Unix domain socket at sockPath. An existing path is
// replaced only if Lstat confirms it is a socket file and dialing it fails
// with connection refused.
func listenUnix(sockPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}
	stale := false
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s already exists and is not a socket", sockPath)
		}
		c, derr := net.DialTimeout("unix", sockPath, staleDialTimeout)
		if derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another process is already listening on %s", sockPath)
		}
		if !isConnRefused(derr) {
			return nil, fmt.Errorf("cannot check existing socket %s: %w", sockPath, derr)
		}
		stale = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return bindUnixSocket(sockPath, stale)
}

// Addr returns the current listener's address, or nil when not listening.
func (p *Proxy) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return nil
	}
	return p.ln.Addr()
}

// ActiveConns returns the number of currently tracked hijack/backend
// connections held by active handlers.
func (p *Proxy) ActiveConns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tracked)
}

// Close stops the listener, closes active connections, cancels any pending
// Changed notification, and removes a Unix socket created by the proxy.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.changeTimer != nil {
		p.changeTimer.Stop()
		p.changeTimer = nil
	}
	p.stopListenerLocked()
	conns := make([]net.Conn, 0, len(p.tracked))
	for c := range p.tracked {
		conns = append(conns, c)
	}
	clear(p.tracked)
	p.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	p.transport.CloseIdleConnections()
	return nil
}

func (p *Proxy) stopListenerLocked() {
	if p.srv != nil {
		_ = p.srv.Close()
		p.srv = nil
	}
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	if p.sockPath != "" {
		_ = os.Remove(p.sockPath)
		p.sockPath = ""
	}
	p.network = ""
	p.address = ""
}

func (p *Proxy) trackConns(conns ...net.Conn) bool {
	p.mu.Lock()
	closed := p.closed
	if !closed {
		for _, c := range conns {
			p.tracked[c] = struct{}{}
		}
	}
	p.mu.Unlock()
	if closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	return true
}

func (p *Proxy) untrackConns(conns ...net.Conn) {
	p.mu.Lock()
	for _, c := range conns {
		delete(p.tracked, c)
	}
	p.mu.Unlock()
}

// SetLocal makes the proxy hand every request to h, the app's own engine,
// in place of the backend: hijacks too, and without Check or Rewrite, which
// are the backend's concerns. Changed still follows the mutating requests h
// answers with success. nil goes back to the backend. It takes effect with
// the next request; the listener stays as it is.
func (p *Proxy) SetLocal(h http.Handler) {
	if h == nil {
		p.local.Store(nil)
		return
	}
	p.local.Store(&h)
}

// Local returns the handler set with SetLocal, or nil.
func (p *Proxy) Local() http.Handler {
	if h := p.local.Load(); h != nil {
		return *h
	}
	return nil
}

// NotifyChanged schedules Changed as a mutating request does, coalesced
// with the others: for changes that come from elsewhere, such as a
// container of the local engine that ended by itself.
func (p *Proxy) NotifyChanged() { p.notifyChanged() }

// ServeHTTP handles Docker API requests, checking container creation bodies,
// hijacking upgraded or raw-stream endpoints with half-close propagation, and
// reverse-proxying ordinary streaming requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := p.Local(); h != nil {
		// The handler hijacks through http.ResponseController, which
		// unwraps the recorder.
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status >= 200 && rec.status < 300 && isMutating(r.Method, stripVersion(cleanMatchPath(r.URL.Path))) {
			p.notifyChanged()
		}
		return
	}
	// Clean the path for route matching only; forward r.URL untouched.
	stripped := stripVersion(cleanMatchPath(r.URL.Path))

	if (p.Check != nil || p.Rewrite != nil) && r.Method == http.MethodPost && stripped == "/containers/create" {
		var ok bool
		if r, ok = p.checkCreate(w, r); !ok {
			return
		}
	}

	if isUpgradeRequest(r) || isLegacyHijack(r.Method, stripped) {
		p.serveHijack(w, r, stripped)
		return
	}

	rec := &statusRecorder{ResponseWriter: w}
	p.rp.ServeHTTP(rec, r)
	if rec.status >= 200 && rec.status < 300 && isMutating(r.Method, stripped) {
		p.notifyChanged()
	}
}

// checkCreate reads the body of a container's creation, lets Rewrite change
// it and Check refuse it, and returns the request to send on with the body
// to forward.
func (p *Proxy) checkCreate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	origBody := r.Body
	buf, err := io.ReadAll(io.LimitReader(origBody, maxCheckBody+1))
	if err != nil {
		writeDockerError(w, r, http.StatusBadRequest, err.Error())
		return r, false
	}
	// Reading r.Body already triggered http.Server to answer any
	// Expect: 100-continue; strip it so the backend does not emit a second 100.
	r.Header.Del("Expect")
	if len(buf) > maxCheckBody {
		_ = origBody.Close()
		writeDockerError(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("container create request body exceeds %d bytes", maxCheckBody))
		return r, false
	}
	_ = origBody.Close()
	if p.Rewrite != nil {
		if out := p.Rewrite(r, buf); out != nil {
			buf = out
		}
	}
	if p.Check != nil {
		if err := p.Check(r, buf); err != nil {
			writeDockerError(w, r, http.StatusBadRequest, err.Error())
			return r, false
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	r.TransferEncoding = nil
	r.Trailer = nil
	return r, true
}

func (p *Proxy) serveHijack(w http.ResponseWriter, r *http.Request, stripped string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		p.rp.ServeHTTP(w, r)
		return
	}

	var reqBody []byte
	if r.Body != nil && (r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		var err error
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, maxHijackBody+1))
		if err != nil {
			writeDockerError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		r.Header.Del("Expect")
		if len(reqBody) > maxHijackBody {
			r.Body = &multiReadCloser{
				Reader: io.MultiReader(bytes.NewReader(reqBody), r.Body),
				Closer: r.Body,
			}
			p.rp.ServeHTTP(w, r)
			return
		}
		r.Trailer = nil
	}

	// Do not use r.Context() here: Go's http.Server cancels r.Context() when
	// a client half-closes its write side (CloseWrite) after sending headers/body,
	// even though the client is still waiting for the hijacked stream's output.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	backendConn, err := p.dial(dialCtx)
	cancelDial()
	if err != nil {
		writeDockerError(w, r, http.StatusServiceUnavailable, ErrNotRunningMessage)
		return
	}
	if !p.trackConns(backendConn) {
		writeDockerError(w, r, http.StatusServiceUnavailable, ErrNotRunningMessage)
		return
	}
	defer func() {
		p.untrackConns(backendConn)
		_ = backendConn.Close()
	}()

	if err := writeRequestRaw(backendConn, r, reqBody); err != nil {
		writeDockerError(w, r, http.StatusServiceUnavailable, ErrNotRunningMessage)
		return
	}

	backendBR := bufio.NewReader(backendConn)
	var (
		rawHead     []byte
		status      int
		contentType string
	)
	for {
		rawHead, status, contentType, err = readRawResponseHead(backendBR)
		if err != nil {
			writeDockerError(w, r, http.StatusServiceUnavailable, ErrNotRunningMessage)
			return
		}
		if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
			continue
		}
		break
	}

	upgraded := status == http.StatusSwitchingProtocols || (status == http.StatusOK && isDockerRawStream(contentType))
	if !upgraded {
		// Normal HTTP response (for example, POST /exec/{id}/start with Detach:true,
		// or a 404/409 error). Forward it on w without hijacking the client connection.
		resp, err := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(rawHead), backendBR)), r)
		if err != nil {
			writeDockerError(w, r, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		copyHeadersWithoutHopByHop(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			copyFlushing(w, resp.Body)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && isMutating(r.Method, stripped) {
			p.notifyChanged()
		}
		return
	}

	clientConn, clientRW, err := hj.Hijack()
	if err != nil {
		return
	}
	if !p.trackConns(clientConn) {
		return
	}
	defer func() {
		p.untrackConns(clientConn)
		_ = clientConn.Close()
	}()

	if _, err := clientConn.Write(rawHead); err != nil {
		return
	}

	backendDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Read from clientRW.Reader directly so bytes buffered past the request
		// headers (or held by net/http's connReader) are never lost.
		_, err := io.Copy(backendConn, clientRW.Reader)
		if err != nil {
			_ = backendConn.Close()
			return
		}
		// Client closed its write side (e.g. stdin EOF, or docker run/attach
		// without stdin calling CloseWrite immediately). Propagate half-close
		// to dockerd so output continues flowing, then watch for the client
		// closing its read end (process exit/kill) so silent attaches release
		// backendConn promptly.
		closeWrite(backendConn)
		if waitPeerClose(clientConn, backendDone) {
			_ = backendConn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		// Read from backendBR directly so bytes buffered past the response
		// headers are never lost. Once the backend stream ends for any reason,
		// half-close clientConn so buffered output flushes cleanly, wake
		// waitPeerClose, and close both conns so client->backend cannot block.
		_, _ = io.Copy(clientConn, backendBR)
		closeWrite(clientConn)
		close(backendDone)
		_ = clientConn.Close()
		_ = backendConn.Close()
	}()
	wg.Wait()
}

func (p *Proxy) notifyChanged() {
	if p.Changed == nil {
		return
	}
	d := p.ChangedDebounce
	if d <= 0 {
		d = defaultChangedWindow
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if p.changeTimer != nil {
		p.changeTimer.Reset(d)
		return
	}
	p.changeTimer = time.AfterFunc(d, func() {
		p.mu.Lock()
		closed := p.closed
		p.changeTimer = nil
		p.mu.Unlock()
		if !closed {
			p.Changed()
		}
	})
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func writeRequestRaw(w io.Writer, r *http.Request, body []byte) error {
	reqURI := r.URL.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s HTTP/1.1\r\n", r.Method, reqURI)
	host := r.Host
	if host == "" {
		host = "docker"
	}
	fmt.Fprintf(&buf, "Host: %s\r\n", host)
	for k, vals := range r.Header {
		if strings.EqualFold(k, "Host") ||
			strings.EqualFold(k, "Content-Length") ||
			strings.EqualFold(k, "Transfer-Encoding") ||
			strings.EqualFold(k, "Expect") {
			continue
		}
		for _, v := range vals {
			fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
		}
	}
	if len(body) > 0 {
		fmt.Fprintf(&buf, "Content-Length: %d\r\n", len(body))
	}
	buf.WriteString("\r\n")
	if len(body) > 0 {
		buf.Write(body)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func readRawResponseHead(br *bufio.Reader) (raw []byte, status int, contentType string, err error) {
	var buf bytes.Buffer
	statusLine, err := br.ReadBytes('\n')
	if err != nil {
		return nil, 0, "", err
	}
	buf.Write(statusLine)
	fields := strings.Fields(string(statusLine))
	if len(fields) < 2 {
		return nil, 0, "", fmt.Errorf("malformed HTTP status line: %q", strings.TrimSpace(string(statusLine)))
	}
	status, err = strconv.Atoi(fields[1])
	if err != nil {
		return nil, 0, "", fmt.Errorf("malformed HTTP status code: %q", fields[1])
	}

	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, 0, "", err
		}
		buf.Write(line)
		if buf.Len() > maxResponseHeadBytes {
			return nil, 0, "", errors.New("response header too large")
		}
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			break
		}
		if name, val, ok := strings.Cut(string(line), ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
			contentType = strings.TrimSpace(val)
		}
	}
	return buf.Bytes(), status, contentType, nil
}

func writeDockerError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: msg})
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeadersWithoutHopByHop(dst, src http.Header) {
	skip := map[string]bool{}
	for _, v := range src.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if name := strings.TrimSpace(tok); name != "" {
				skip[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	for k, vals := range src {
		ck := http.CanonicalHeaderKey(k)
		if hopByHopHeaders[ck] || skip[ck] {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func copyFlushing(w http.ResponseWriter, src io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// isUpgradeRequest reports whether r requests an HTTP connection upgrade.
func isUpgradeRequest(r *http.Request) bool {
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

// isLegacyHijack reports whether method and version-stripped path are a Docker
// attach or exec-start endpoint that may answer with a legacy 200 raw stream.
func isLegacyHijack(method, strippedPath string) bool {
	if method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.Trim(strippedPath, "/"), "/")
	if len(parts) == 3 && parts[0] == "containers" && parts[2] == "attach" {
		return true
	}
	if len(parts) == 3 && parts[0] == "exec" && parts[2] == "start" {
		return true
	}
	return false
}

func isDockerRawStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "application/vnd.docker.raw-stream" || mt == "application/vnd.docker.multiplexed-stream"
}

// isMutating reports whether a successful response to method and strippedPath
// changes container/image state worth refreshing in the UI.
func isMutating(method, strippedPath string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	switch strippedPath {
	case "/session", "/grpc", "/auth":
		return false
	}
	parts := strings.Split(strings.Trim(strippedPath, "/"), "/")
	if len(parts) >= 3 && parts[0] == "containers" {
		switch parts[2] {
		case "attach", "wait", "exec", "resize":
			return false
		}
	}
	if len(parts) >= 2 && parts[0] == "exec" {
		return false
	}
	return true
}

// cleanMatchPath normalizes a request path for route matching without altering
// the forwarded request URL.
func cleanMatchPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if !strings.HasPrefix(c, "/") {
		return "/" + c
	}
	return c
}

// stripVersion removes a leading Docker API version prefix such as /v1.47.
func stripVersion(p string) string {
	if !strings.HasPrefix(p, "/v") {
		return p
	}
	rest := p[2:]
	slash := strings.IndexByte(rest, '/')
	ver := rest
	if slash >= 0 {
		ver = rest[:slash]
	}
	if !isVersionSegment(ver) {
		return p
	}
	if slash < 0 {
		return "/"
	}
	return rest[slash:]
}

func isVersionSegment(s string) bool {
	major, minor, ok := strings.Cut(s, ".")
	if !ok || major == "" || minor == "" {
		return false
	}
	for _, r := range major {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range minor {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		// Ignore informational 1xx responses (e.g. 100 Continue) so the final
		// response status is captured for Changed notifications.
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
