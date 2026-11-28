package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// shortSock returns a short Unix socket path so macOS's ~104-byte limit is
// never exceeded by long test names.
func shortSock(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gpx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func startFakeBackend(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := shortSock(t, "be.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return sock
}

func startProxy(t *testing.T, backendSock string, configure func(*Proxy)) (*Proxy, string, *http.Client) {
	t.Helper()
	p := New(DialAddress("unix", backendSock))
	if configure != nil {
		configure(p)
	}
	frontSock := shortSock(t, "fe.sock")
	if err := p.Listen("unix", frontSock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", frontSock)
			},
		},
	}
	return p, frontSock, client
}

func TestPlainGETPreservesHeadersAndStatus(t *testing.T) {
	var gotHost string
	var gotXFF string
	var gotCustom string
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotCustom = r.Header.Get("X-Client-Header")
		w.Header().Set("X-Backend-Header", "keep-me")
		w.Header().Set("Api-Version", "1.47")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"Version":"29.0.0"}`))
	}))
	_, _, client := startProxy(t, be, nil)

	req, _ := http.NewRequest(http.MethodGet, "http://v1.47.local/v1.47/version", nil)
	req.Header.Set("X-Client-Header", "hello")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
	if string(body) != `{"Version":"29.0.0"}` {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-Backend-Header") != "keep-me" || resp.Header.Get("Api-Version") != "1.47" {
		t.Fatalf("headers not preserved: %v", resp.Header)
	}
	if gotCustom != "hello" {
		t.Errorf("backend did not see X-Client-Header: %q", gotCustom)
	}
	if gotXFF != "" {
		t.Errorf("X-Forwarded-For was added: %q", gotXFF)
	}
	if gotHost != "v1.47.local" {
		t.Errorf("Host altered: got %q, want %q", gotHost, "v1.47.local")
	}
}

func TestStreamedResponseFlushesImmediately(t *testing.T) {
	chunk1Read := make(chan struct{})
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("line1\n"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-chunk1Read:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = w.Write([]byte("line2\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	_, _, client := startProxy(t, be, nil)

	resp, err := client.Get("http://docker/v1.47/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line1, err := br.ReadString('\n')
	if err != nil || line1 != "line1\n" {
		t.Fatalf("chunk 1 = %q, %v", line1, err)
	}
	close(chunk1Read)
	line2, err := br.ReadString('\n')
	if err != nil || line2 != "line2\n" {
		t.Fatalf("chunk 2 = %q, %v", line2, err)
	}
}

func TestLargeStreamedRequestBodyIsNotBufferedWhole(t *testing.T) {
	const totalBytes = 12 << 20 // 12 MiB
	const chunkSize = 256 << 10
	firstChunkSeen := make(chan struct{})
	var backendBytes atomic.Int64

	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32*1024)
		var nTotal int64
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				if nTotal == 0 {
					close(firstChunkSeen)
				}
				nTotal += int64(n)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		backendBytes.Store(nTotal)
		w.WriteHeader(http.StatusOK)
	}))
	_, _, client := startProxy(t, be, nil)

	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte("a"), chunkSize)
		// Write the first chunk and wait for the backend to observe it before
		// sending the rest: proves the proxy streams without buffering whole.
		if _, err := pw.Write(chunk); err != nil {
			writeErr <- err
			return
		}
		select {
		case <-firstChunkSeen:
		case <-time.After(5 * time.Second):
			_ = pw.CloseWithError(fmt.Errorf("backend never saw streamed first chunk"))
			writeErr <- fmt.Errorf("backend never saw streamed first chunk")
			return
		}
		for sent := chunkSize; sent < totalBytes; sent += chunkSize {
			if _, err := pw.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- pw.Close()
	}()

	req, _ := http.NewRequest(http.MethodPost, "http://docker/v1.47/images/load", pr)
	req.ContentLength = -1
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := backendBytes.Load(); got != totalBytes {
		t.Fatalf("backend received %d bytes, want %d", got, totalBytes)
	}
}

func TestUpgradeBidirectionalAndHalfClose(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
			http.Error(w, "missing upgrade", http.StatusBadRequest)
			return
		}
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Respond with 101 and immediately send a banner in the same write to
		// test buffered bytes past the response headers.
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nBANNER:")
		_ = rw.Flush()

		// Read until client half-closes stdin (EOF), then echo everything back
		// plus a trailer after EOF.
		in, _ := io.ReadAll(rw.Reader)
		_, _ = conn.Write(append(in, []byte(":AFTER_EOF")...))
		closeWrite(conn)
	}))
	_, frontSock, _ := startProxy(t, be, nil)

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Pipeline the first stdin bytes immediately after the upgrade request to
	// verify buffered client bytes past the request headers are not lost.
	req := "POST /v1.47/containers/c1/attach?stdin=1&stdout=1&stream=1 HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n\r\n" +
		"hi "
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("there")); err != nil {
		t.Fatal(err)
	}
	// Half-close client write side: backend must receive EOF, then send its
	// remaining output, and client must receive all of it.
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	const want = "BANNER:hi there:AFTER_EOF"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

func TestLegacyRawStreamAttachAndDetachedExec(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.47/containers/c1/attach":
			hj := w.(http.Hijacker)
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nRAW:")
			_ = rw.Flush()
			in, _ := io.ReadAll(rw.Reader)
			_, _ = conn.Write(append(in, []byte(":DONE")...))
			closeWrite(conn)
		case "/v1.47/exec/e1/start":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"Detach":true`) {
				w.WriteHeader(http.StatusOK)
				return
			}
			hj := w.(http.Hijacker)
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nEXEC:" + string(body) + ":")
			_ = rw.Flush()
			in, _ := io.ReadAll(rw.Reader)
			_, _ = conn.Write(in)
			closeWrite(conn)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	_, frontSock, client := startProxy(t, be, nil)

	// 1. Legacy 200 raw-stream attach without Upgrade header (with stdin=1).
	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("POST /v1.47/containers/c1/attach?stdin=1&stdout=1&stream=1 HTTP/1.1\r\nHost: docker\r\n\r\nhello"))
	_ = conn.(*net.UnixConn).CloseWrite()
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/vnd.docker.raw-stream" {
		t.Fatalf("attach resp = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out, _ := io.ReadAll(br)
	conn.Close()
	if string(out) != "RAW:hello:DONE" {
		t.Fatalf("legacy attach got %q, want %q", string(out), "RAW:hello:DONE")
	}

	// 2. Legacy 200 raw-stream exec/start with JSON body + piped stdin.
	conn2, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"Detach":false,"Tty":true}`
	_, _ = fmt.Fprintf(conn2, "POST /v1.47/exec/e1/start HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%sstdin-data", len(payload), payload)
	_ = conn2.(*net.UnixConn).CloseWrite()
	br2 := bufio.NewReader(conn2)
	resp2, err := http.ReadResponse(br2, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("exec start status = %d", resp2.StatusCode)
	}
	out2, _ := io.ReadAll(br2)
	conn2.Close()
	if string(out2) != "EXEC:"+payload+":stdin-data" {
		t.Fatalf("legacy exec got %q", string(out2))
	}

	// 3. Detached exec/start returns normal 200 without hanging.
	dresp, err := client.Post("http://docker/v1.47/exec/e1/start", "application/json", strings.NewReader(`{"Detach":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("detached exec status = %d", dresp.StatusCode)
	}
}

func TestBackendDownReturns503JSON(t *testing.T) {
	missingSock := shortSock(t, "missing.sock")
	_, _, client := startProxy(t, missingSock, nil)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/_ping"},
		{http.MethodHead, "/_ping"},
		{http.MethodGet, "/v1.47/containers/json"},
		{http.MethodPost, "/v1.47/containers/c1/attach"},
	} {
		req, _ := http.NewRequest(tc.method, "http://docker"+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q", tc.method, tc.path, ct)
		}
		if tc.method == http.MethodHead {
			if len(body) != 0 {
				t.Errorf("HEAD returned body %q", body)
			}
			continue
		}
		var errObj struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &errObj); err != nil || errObj.Message != ErrNotRunningMessage {
			t.Errorf("%s %s: body = %q", tc.method, tc.path, body)
		}
	}
}

func TestCheckRejectAndPass(t *testing.T) {
	var backendHits atomic.Int32
	var mu sync.Mutex
	var backendBody []byte
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		backendBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"abc"}`))
	}))

	_, _, client := startProxy(t, be, func(p *Proxy) {
		p.Check = func(r *http.Request, body []byte) error {
			if bytes.Contains(body, []byte("/forbidden")) {
				return fmt.Errorf("/forbidden is not allowed")
			}
			return nil
		}
	})

	// 1. Rejected request: 400 JSON, backend never sees it.
	resp, err := client.Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader(`{"HostConfig":{"Binds":["/forbidden:/data"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	var errObj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &errObj); err != nil || errObj.Message != "/forbidden is not allowed" {
		t.Fatalf("body = %q", raw)
	}
	if backendHits.Load() != 0 {
		t.Fatalf("backend saw %d requests, want 0", backendHits.Load())
	}

	// 2. Passed request (without version prefix): backend sees identical body.
	wantBody := `{"Image":"alpine","HostConfig":{"Binds":["/allowed:/data"]}}`
	resp2, err := client.Post("http://docker/containers/create", "application/json", strings.NewReader(wantBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp2.StatusCode)
	}
	mu.Lock()
	gotBody := string(backendBody)
	mu.Unlock()
	if gotBody != wantBody {
		t.Fatalf("backend body = %q, want %q", gotBody, wantBody)
	}
}

func TestChangedHookFiresOnMutatingAndNotGET(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.47/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	var calls atomic.Int32
	_, _, client := startProxy(t, be, func(p *Proxy) {
		p.ChangedDebounce = 40 * time.Millisecond
		p.Changed = func() {
			calls.Add(1)
		}
	})

	// GET must not trigger Changed.
	resp, err := client.Get("http://docker/v1.47/containers/json")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("GET triggered Changed %d times", n)
	}

	// Failed POST must not trigger Changed.
	resp, _ = client.Post("http://docker/v1.47/fail", "application/json", nil)
	_ = resp.Body.Close()
	// Non-mutating POST (wait) must not trigger Changed.
	resp, _ = client.Post("http://docker/v1.47/containers/c1/wait", "application/json", nil)
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("failed/wait POST triggered Changed %d times", n)
	}

	// Burst of mutating POSTs coalesces to a single Changed call.
	for range 3 {
		r, err := client.Post("http://docker/v1.47/containers/c1/start", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("burst of 3 POSTs triggered Changed %d times, want 1", n)
	}
}

func TestListenReplacesListenerStaleAndLiveSocket(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	p := New(DialAddress("unix", be))
	defer p.Close()

	// 1. Live socket held by another process -> clear error.
	liveSock := shortSock(t, "live.sock")
	other, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := p.Listen("unix", liveSock); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("live socket listen err = %v, want clear 'already listening' error", err)
	}

	// 2. Stale socket file -> removed and bound with mode 0600 on Unix.
	staleSock := shortSock(t, "stale.sock")
	staleLn, err := net.Listen("unix", staleSock)
	if err != nil {
		t.Fatal(err)
	}
	// Close the underlying fd without unlinking by using net.UnixListener.SetUnlinkOnClose(false).
	staleLn.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = staleLn.Close()
	if _, err := os.Lstat(staleSock); err != nil {
		t.Fatalf("expected stale socket file to exist: %v", err)
	}
	if err := p.Listen("unix", staleSock); err != nil {
		t.Fatalf("listen over stale socket failed: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(staleSock)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("socket mode = %o, want 600", mode)
		}
	}

	// 3. Re-listening on TCP replaces the Unix listener and removes the old socket.
	if err := p.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("re-listen TCP failed: %v", err)
	}
	if _, err := os.Lstat(staleSock); !os.IsNotExist(err) {
		t.Errorf("old socket was not removed on re-listen: %v", err)
	}
	tcpAddr := p.Addr().String()
	resp, err := http.Get("http://" + tcpAddr + "/_ping")
	if err != nil {
		t.Fatalf("TCP proxy GET failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestHijackBackendEOFCleanupWithSilentClient(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nhello-done")
		_ = rw.Flush()
		// Backend ends the session immediately; client never closes its write side.
	}))
	p, frontSock, _ := startProxy(t, be, nil)

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte("POST /v1.47/containers/c1/attach?stdin=1&stdout=1&stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	got, err := io.ReadAll(br)
	if err != nil || string(got) != "hello-done" {
		t.Fatalf("got %q, %v", got, err)
	}
	// Do NOT close conn yet: verify proxy handler has already exited and freed
	// both tracked connections.
	deadline := time.Now().Add(2 * time.Second)
	for p.ActiveConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.ActiveConns(); n != 0 {
		t.Fatalf("expected 0 active conns after backend EOF with silent client, got %d", n)
	}
}

func TestAttachWithoutStdinClientCloseClosesBackend(t *testing.T) {
	backendDone := make(chan struct{})
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
			close(backendDone)
		}()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		// Block reading until peer closes the backend connection (a real daemon
		// watching client disconnect on a silent non-stdin attach).
		_, _ = io.Copy(io.Discard, rw.Reader)
	}))
	p, frontSock, _ := startProxy(t, be, nil)

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Write([]byte("POST /v1.47/containers/c1/attach?stdout=1&stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	// Client goes away (closes connection): backend must close promptly.
	_ = conn.Close()
	select {
	case <-backendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("backend attach conn did not close promptly after client closed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.ActiveConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.ActiveConns(); n != 0 {
		t.Fatalf("expected 0 active conns after client close, got %d", n)
	}
}

// Regression: docker run --rm alpine echo ok attaches without stdin=1 and
// immediately calls CloseWrite() while waiting to read stdout. The proxy must
// NOT close backendConn on CloseWrite alone; output must still arrive intact.
func TestAttachWithoutStdinHalfCloseStillReceivesOutput(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		// Wait for client's CloseWrite() EOF, then deliver stdout.
		_, _ = io.Copy(io.Discard, rw.Reader)
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Write([]byte("ok\n"))
		closeWrite(conn)
	}))
	_, frontSock, _ := startProxy(t, be, nil)

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte("POST /v1.47/containers/c1/attach?stderr=1&stdout=1&stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	// Docker CLI calls CloseWrite immediately when stdin is not attached.
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ok\n" {
		t.Fatalf("got %q, want %q", string(out), "ok\n")
	}
}

func TestExpect100ContinueCreate(t *testing.T) {
	var gotExpect string
	var gotBody []byte
	var mu sync.Mutex
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotExpect = r.Header.Get("Expect")
		gotBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"c100"}`))
	}))

	var changed atomic.Int32
	_, frontSock, _ := startProxy(t, be, func(p *Proxy) {
		p.Check = func(_ *http.Request, body []byte) error { return nil }
		p.ChangedDebounce = 30 * time.Millisecond
		p.Changed = func() { changed.Add(1) }
	})

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := `{"Image":"alpine"}`
	_, err = fmt.Fprintf(conn, "POST /v1.47/containers/create HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(payload))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	// First response on the wire must be 100 Continue.
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "100") {
		t.Fatalf("expected 100 Continue, got %q (%v)", line, err)
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}
	// Send the body; next response on the wire must be 201 Created (not a second 100).
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("final status = %d, want 201", resp.StatusCode)
	}
	mu.Lock()
	ex, b := gotExpect, string(gotBody)
	mu.Unlock()
	if ex != "" {
		t.Errorf("backend saw Expect header %q, want empty", ex)
	}
	if b != payload {
		t.Errorf("backend body = %q, want %q", b, payload)
	}
	deadline := time.Now().Add(time.Second)
	for changed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if changed.Load() != 1 {
		t.Fatalf("Changed called %d times, want 1", changed.Load())
	}
}

func TestExpect100ContinueExecStart(t *testing.T) {
	var gotExpect string
	var mu sync.Mutex
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotExpect = r.Header.Get("Expect")
		mu.Unlock()
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Emit an informational 102 before the 200 raw-stream to verify the proxy
		// loops past 1xx headers before deciding upgraded.
		_, _ = rw.WriteString("HTTP/1.1 102 Processing\r\n\r\n")
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nEXEC:" + string(body) + ":")
		_ = rw.Flush()
		in, _ := io.ReadAll(rw.Reader)
		_, _ = conn.Write(in)
		closeWrite(conn)
	}))
	_, frontSock, _ := startProxy(t, be, nil)

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := `{"Detach":false,"Tty":true}`
	_, err = fmt.Fprintf(conn, "POST /v1.47/exec/e1/start HTTP/1.1\r\nHost: docker\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(payload))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "100") {
		t.Fatalf("expected 100 Continue, got %q (%v)", line, err)
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}
	if _, err := conn.Write([]byte(payload + "stream-in")); err != nil {
		t.Fatal(err)
	}
	_ = conn.(*net.UnixConn).CloseWrite()
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(br)
	if string(out) != "EXEC:"+payload+":stream-in" {
		t.Fatalf("got %q", string(out))
	}
	mu.Lock()
	ex := gotExpect
	mu.Unlock()
	if ex != "" {
		t.Errorf("backend saw Expect header %q, want empty", ex)
	}
}

func TestUncleanedPathsMatchRoutes(t *testing.T) {
	var mu sync.Mutex
	var backendPaths []string
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendPaths = append(backendPaths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/attach") {
			hj := w.(http.Hijacker)
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nHIJACKED")
			_ = rw.Flush()
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))

	var checked atomic.Int32
	_, frontSock, _ := startProxy(t, be, func(p *Proxy) {
		p.Check = func(_ *http.Request, body []byte) error {
			checked.Add(1)
			if bytes.Contains(body, []byte("bad")) {
				return fmt.Errorf("rejected")
			}
			return nil
		}
	})

	// Send raw HTTP requests so Go's http.Client does not clean the URL paths.
	sendRaw := func(rawPath, body string) int {
		t.Helper()
		conn, err := net.Dial("unix", frontSock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s", rawPath, len(body), body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	uncleanCreates := []string{
		"//v1.47/containers/create",
		"/v1.47/./containers/create",
		"/v1.47/containers/create/",
	}
	for _, p := range uncleanCreates {
		if code := sendRaw(p, `{"bad":true}`); code != http.StatusBadRequest {
			t.Errorf("POST %s (rejected): status = %d, want 400", p, code)
		}
		if code := sendRaw(p, `{"ok":true}`); code != http.StatusCreated {
			t.Errorf("POST %s (allowed): status = %d, want 201", p, code)
		}
	}
	if n := checked.Load(); n != int32(len(uncleanCreates)*2) {
		t.Fatalf("Check called %d times, want %d", n, len(uncleanCreates)*2)
	}
	// Backend must see original untouched paths for allowed creates.
	mu.Lock()
	gotPaths := append([]string(nil), backendPaths...)
	mu.Unlock()
	if strings.Join(gotPaths, ",") != strings.Join(uncleanCreates, ",") {
		t.Errorf("forwarded paths = %v, want %v", gotPaths, uncleanCreates)
	}

	// Uncleaned attach path must still take the hijack path.
	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST //v1.47/containers/x/attach HTTP/1.1\r\nHost: docker\r\n\r\n"))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("attach resp = %v, %v", resp, err)
	}
	out, _ := io.ReadAll(br)
	if string(out) != "HIJACKED" {
		t.Fatalf("attach out = %q, want HIJACKED", string(out))
	}
}

func TestListenUnixRejectsSymlinkAndRegularFile(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	p := New(DialAddress("unix", be))
	defer p.Close()

	// 1. Regular file at target path -> error, file left untouched.
	regPath := shortSock(t, "reg")
	if err := os.WriteFile(regPath, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Listen("unix", regPath); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file listen err = %v", err)
	}
	if b, err := os.ReadFile(regPath); err != nil || string(b) != "keep-me" {
		t.Fatalf("regular file modified or removed: %q, %v", b, err)
	}

	// 2. Symlink at target path (even dangling or pointing to a dead socket) -> error, symlink left untouched.
	if runtime.GOOS != "windows" {
		symPath := shortSock(t, "sym")
		if err := os.Symlink("/nonexistent/target", symPath); err != nil {
			t.Fatal(err)
		}
		if err := p.Listen("unix", symPath); err == nil || !strings.Contains(err.Error(), "not a socket") {
			t.Fatalf("symlink listen err = %v", err)
		}
		if fi, err := os.Lstat(symPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink removed or changed: %v, %v", fi, err)
		}
	}

	// 3. Atomic bind+rename (Unix): client connects through final path with 0600 mode.
	finalSock := shortSock(t, "final.sock")
	if err := p.Listen("unix", finalSock); err != nil {
		t.Fatalf("listen on finalSock: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(finalSock)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("socket mode = %o, want 600", mode)
		}
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", finalSock)
			},
		},
	}
	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("GET via renamed socket failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestProxyCloseUnblocksTrackedBackendAndNonUpgradedHopHeaders(t *testing.T) {
	// Part 1: non-upgraded branch strips hop-by-hop headers and flushes body.
	chunk1Read := make(chan struct{})
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Connection", "X-Hop-Custom")
		w.Header().Set("X-Hop-Custom", "drop-me")
		w.Header().Set("X-Keep", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("part1\n"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-chunk1Read:
		case <-time.After(3 * time.Second):
			return
		}
		_, _ = w.Write([]byte("part2\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	_, _, client := startProxy(t, be, nil)
	resp, err := client.Post("http://docker/v1.47/exec/e1/start", "application/json", strings.NewReader(`{"Detach":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("X-Hop-Custom") != "" {
		t.Errorf("hop-by-hop header X-Hop-Custom was not stripped")
	}
	if resp.Header.Get("X-Keep") != "yes" {
		t.Errorf("X-Keep header lost: %v", resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	l1, err := br.ReadString('\n')
	if err != nil || l1 != "part1\n" {
		t.Fatalf("part1 = %q, %v", l1, err)
	}
	close(chunk1Read)
	l2, err := br.ReadString('\n')
	if err != nil || l2 != "part2\n" {
		t.Fatalf("part2 = %q, %v", l2, err)
	}
	_ = resp.Body.Close()

	// Part 2: Proxy.Close() closes tracked backendConn even before hijack finishes.
	backendReached := make(chan struct{})
	backendClosed := make(chan struct{})
	be2 := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
			close(backendClosed)
		}()
		close(backendReached)
		// Block without sending response headers until proxy closes backendConn.
		_, _ = io.Copy(io.Discard, rw.Reader)
	}))
	p2 := New(DialAddress("unix", be2))
	sock2 := shortSock(t, "fe2.sock")
	if err := p2.Listen("unix", sock2); err != nil {
		t.Fatal(err)
	}
	client2 := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock2)
			},
		},
	}
	go func() {
		r, err := client2.Post("http://docker/v1.47/exec/e1/start", "application/json", strings.NewReader(`{"Detach":false}`))
		if err == nil {
			_ = r.Body.Close()
		}
	}()
	select {
	case <-backendReached:
	case <-time.After(2 * time.Second):
		t.Fatal("backend never received request")
	}
	_ = p2.Close()
	select {
	case <-backendClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("Proxy.Close did not unblock pre-hijack backendConn")
	}
}

// TestRealProxyServe is an env-gated harness for real verification against a
// throwaway docker:dind sandbox. Set GVXD_PROXY_BACKEND to the backend Unix
// socket path and GVXD_PROXY_FRONTEND to the frontend Unix socket path.
// Optionally set GVXD_PROXY_SHARED to comma-separated shared host folders to
// enable the unshared bind-mount check hook.
func TestRealProxyServe(t *testing.T) {
	backend := os.Getenv("GVXD_PROXY_BACKEND")
	frontend := os.Getenv("GVXD_PROXY_FRONTEND")
	if backend == "" || frontend == "" {
		t.Skip("set GVXD_PROXY_BACKEND and GVXD_PROXY_FRONTEND to run the real proxy harness")
	}
	var engine sandbox.Spec
	if sharedEnv := os.Getenv("GVXD_PROXY_SHARED"); sharedEnv != "" {
		var folders []string
		for s := range strings.SplitSeq(sharedEnv, ",") {
			if s = strings.TrimSpace(s); s != "" {
				folders = append(folders, s)
			}
		}
		engine = recipes.WithSharedFolders(recipes.DockerEngine(), folders)
	}

	p := New(DialAddress("unix", backend))
	if engine.Recipe == recipes.Docker {
		p.Check = func(_ *http.Request, body []byte) error {
			var req struct {
				HostConfig *struct {
					Binds  []string `json:"Binds"`
					Mounts []struct {
						Type   string `json:"Type"`
						Source string `json:"Source"`
					} `json:"Mounts"`
				} `json:"HostConfig"`
			}
			if err := json.Unmarshal(body, &req); err != nil || req.HostConfig == nil {
				return nil
			}
			for _, b := range req.HostConfig.Binds {
				src, _, ok := strings.Cut(b, ":")
				if ok && strings.HasPrefix(src, "/") && recipes.UnsharedBind(src, engine) {
					return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again.", src)
				}
			}
			for _, m := range req.HostConfig.Mounts {
				if strings.EqualFold(m.Type, "bind") && strings.HasPrefix(m.Source, "/") && recipes.UnsharedBind(m.Source, engine) {
					return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again.", m.Source)
				}
			}
			return nil
		}
	}
	if err := p.Listen("unix", frontend); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if readyFile := os.Getenv("GVXD_PROXY_READY"); readyFile != "" {
		_ = os.WriteFile(readyFile, []byte("ready\n"), 0o600)
	}
	if debugAddr := os.Getenv("GVXD_PROXY_DEBUG_ADDR"); debugAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/active", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "%d\n", p.ActiveConns())
		})
		dbgSrv := &http.Server{Addr: debugAddr, Handler: mux}
		go func() { _ = dbgSrv.ListenAndServe() }()
		defer dbgSrv.Close()
	}
	fmt.Printf("PROXY_READY %s -> %s\n", frontend, backend)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	<-sigs
}

func TestRewriteChangesTheBodyBeforeCheck(t *testing.T) {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.47/containers/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"abc","Warnings":[]}`)
	})
	var checked string
	_, _, client := startProxy(t, startFakeBackend(t, mux), func(p *Proxy) {
		p.Rewrite = func(_ *http.Request, body []byte) []byte {
			return []byte(strings.ReplaceAll(string(body), "old", "new"))
		}
		p.Check = func(_ *http.Request, body []byte) error { checked = string(body); return nil }
	})
	resp, err := client.Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader(`{"Image":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(got) != `{"Id":"abc","Warnings":[]}` {
		t.Errorf("%d %s", resp.StatusCode, got)
	}
	if seen != `{"Image":"new"}` || checked != seen {
		t.Errorf("backend saw %q, Check %q", seen, checked)
	}
}

func TestSetLocalRoutesRequestsBypassesHooksAndFallsBack(t *testing.T) {
	var backendHits atomic.Int32
	var rewriteHits atomic.Int32
	var checkHits atomic.Int32
	var changedHits atomic.Int32

	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Origin", "backend")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(b)
	}))

	p, _, client := startProxy(t, be, func(p *Proxy) {
		p.ChangedDebounce = 25 * time.Millisecond
		p.Changed = func() { changedHits.Add(1) }
		p.Rewrite = func(_ *http.Request, body []byte) []byte {
			rewriteHits.Add(1)
			return bytes.ReplaceAll(body, []byte("raw"), []byte("rewritten"))
		}
		p.Check = func(_ *http.Request, body []byte) error {
			checkHits.Add(1)
			if bytes.Contains(body, []byte("forbidden")) {
				return fmt.Errorf("blocked")
			}
			return nil
		}
	})

	var localHits atomic.Int32
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHits.Add(1)
		w.Header().Set("X-Origin", "local")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
			return
		}
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte("bad")) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"bad"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(b)
	})

	p.SetLocal(local)
	if p.Local() == nil {
		t.Fatal("Local() = nil after SetLocal")
	}

	// Non-mutating GET goes to local and does not trigger Changed.
	resp, err := client.Get("http://docker/v1.52/_ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Origin") != "local" || string(body) != "ok" {
		t.Fatalf("GET: status=%d origin=%q body=%q", resp.StatusCode, resp.Header.Get("X-Origin"), body)
	}
	time.Sleep(60 * time.Millisecond)
	if changedHits.Load() != 0 {
		t.Fatalf("GET triggered Changed (%d)", changedHits.Load())
	}

	// POST /containers/create goes straight to local, bypassing Check and Rewrite,
	// and fires Changed on 201.
	const payload = `{"Image":"raw","HostConfig":{"Binds":["/forbidden:/x"]}}`
	resp, err = client.Post("http://docker/v1.52/containers/create", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != payload {
		t.Fatalf("local POST: status=%d body=%q", resp.StatusCode, body)
	}
	if backendHits.Load() != 0 || rewriteHits.Load() != 0 || checkHits.Load() != 0 {
		t.Fatalf("local mode leaked to backend/hooks: backend=%d rewrite=%d check=%d",
			backendHits.Load(), rewriteHits.Load(), checkHits.Load())
	}
	deadline := time.Now().Add(time.Second)
	for changedHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if changedHits.Load() != 1 {
		t.Fatalf("Changed hits = %d, want 1", changedHits.Load())
	}

	// Non-2xx mutating response on local does not trigger Changed.
	resp, err = client.Post("http://docker/v1.52/containers/create", "application/json", strings.NewReader(`{"Image":"bad"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad POST status = %d", resp.StatusCode)
	}
	time.Sleep(60 * time.Millisecond)
	if changedHits.Load() != 1 {
		t.Fatalf("400 response triggered Changed (%d)", changedHits.Load())
	}

	// SetLocal(nil) immediately returns traffic to the backend with Rewrite/Check active.
	p.SetLocal(nil)
	if p.Local() != nil {
		t.Fatal("Local() != nil after SetLocal(nil)")
	}
	resp, err = client.Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader(`{"Image":"raw"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Origin") != "backend" || string(body) != `{"Image":"rewritten"}` {
		t.Fatalf("fallback POST: status=%d origin=%q body=%q", resp.StatusCode, resp.Header.Get("X-Origin"), body)
	}
	if backendHits.Load() != 1 || rewriteHits.Load() != 1 || checkHits.Load() != 1 {
		t.Fatalf("fallback counts: backend=%d rewrite=%d check=%d",
			backendHits.Load(), rewriteHits.Load(), checkHits.Load())
	}
	if localHits.Load() != 3 {
		t.Fatalf("localHits = %d, want 3", localHits.Load())
	}
}

func TestSetLocalHijack(t *testing.T) {
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend should not be contacted while local handler is set")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	p, frontSock, _ := startProxy(t, be, nil)
	p.SetLocal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		conn, rw, err := rc.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nLOCAL:")
		_ = rw.Flush()
		in, _ := io.ReadAll(rw.Reader)
		_, _ = conn.Write(append(in, []byte(":DONE")...))
		closeWrite(conn)
	}))

	conn, err := net.Dial("unix", frontSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := "POST /v1.52/containers/c1/attach?stdin=1&stdout=1&stream=1 HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n\r\n" +
		"hello"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "LOCAL:hello:DONE" {
		t.Fatalf("hijacked stream = %q, want %q", got, "LOCAL:hello:DONE")
	}
}

func TestNotifyChangedCoalesces(t *testing.T) {
	var hits atomic.Int32
	p := New(DialAddress("unix", shortSock(t, "unused.sock")))
	p.ChangedDebounce = 40 * time.Millisecond
	p.Changed = func() { hits.Add(1) }
	t.Cleanup(func() { _ = p.Close() })

	for range 5 {
		p.NotifyChanged()
		time.Sleep(5 * time.Millisecond)
	}
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("NotifyChanged fired %d times, want 1 coalesced call", got)
	}
}

func TestCreateRejectsOversizedBodyInsteadOfBypassingCheck(t *testing.T) {
	var backendHit atomic.Bool
	be := startFakeBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	_, _, client := startProxy(t, be, func(p *Proxy) {
		p.Check = func(_ *http.Request, _ []byte) error {
			return fmt.Errorf("should not reach check for oversized body")
		}
	})

	big := bytes.Repeat([]byte("a"), maxCheckBody+1024)
	resp, err := client.Post("http://docker/v1.47/containers/create", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if backendHit.Load() {
		t.Fatal("oversized /containers/create bypassed Check and reached backend")
	}
}
