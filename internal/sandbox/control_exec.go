package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func (c *runControl) Exec(ctx context.Context, o ExecOptions) (ExecProcess, error) {
	if c.isPaused() {
		return nil, ErrPaused
	}
	pidFile, err := c.allocPIDFile()
	if err != nil {
		return nil, err
	}

	ro := o.ExecOptions
	ro.PIDFile = pidFile
	args := c.client.ExecArgs(c.target, ro)
	cmd := c.client.Command(ctx, args...)
	cmd.Env = os.Environ()

	ep := &execProcess{
		ctl:       c,
		cmd:       cmd,
		args:      args,
		pidFile:   pidFile,
		wantTTY:   o.TTY,
		startedCh: make(chan struct{}),
		waitDone:  make(chan struct{}),
	}

	usePTY := o.TTY
	var (
		closeAfterStart []func()
		closeOnFail     []func()
		stderrR         *os.File
	)
	cleanupFail := func() {
		for _, fn := range closeOnFail {
			fn()
		}
		_ = os.Remove(pidFile)
	}

	if usePTY {
		ptyTerm, closeSlave, err := openExecPTY(cmd, o.Rows, o.Cols)
		if err != nil {
			cleanupFail()
			return nil, err
		}
		ep.hasPGroup = true
		ep.ptyTerm = ptyTerm
		ep.stdout = ptyTerm.reader()
		if o.Interactive {
			ep.stdin = &ptyStdinCloser{ep: ep}
		}
		closeAfterStart = append(closeAfterStart, closeSlave)
		closeOnFail = append(closeOnFail, closeSlave, func() { _ = ptyTerm.close() })
	} else {
		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			cleanupFail()
			return nil, err
		}
		cmd.Stdout = stdoutW
		ep.stdoutR = stdoutR
		ep.stdout = stdoutR
		closeAfterStart = append(closeAfterStart, func() { _ = stdoutW.Close() })
		closeOnFail = append(closeOnFail, func() { _ = stdoutW.Close(); _ = stdoutR.Close() })

		if o.Interactive {
			stdinR, stdinW, err := os.Pipe()
			if err != nil {
				cleanupFail()
				return nil, err
			}
			cmd.Stdin = stdinR
			ep.stdinW = stdinW
			ep.stdin = &pipeStdinCloser{ep: ep}
			closeAfterStart = append(closeAfterStart, func() { _ = stdinR.Close() })
			closeOnFail = append(closeOnFail, func() { _ = stdinR.Close(); _ = stdinW.Close() })
		}
	}

	sr, sw, err := os.Pipe()
	if err != nil {
		cleanupFail()
		return nil, err
	}
	cmd.Stderr = sw
	stderrR = sr
	closeAfterStart = append(closeAfterStart, func() { _ = sw.Close() })
	closeOnFail = append(closeOnFail, func() { _ = sw.Close(); _ = sr.Close() })

	if o.TTY {
		// With TTY, Stderr() is nil and rungv's own messages go into Wait's error.
		ep.stderrBuf = newStderrCollector(stderrR)
	} else {
		ep.stderrTap = newStderrTap(stderrR)
		ep.stderr = ep.stderrTap
	}

	if err := startCmd(ctx, cmd, ep.ptyTerm); err != nil {
		cleanupFail()
		return nil, err
	}
	for _, fn := range closeAfterStart {
		fn()
	}

	if ep.stderrBuf != nil {
		go ep.stderrBuf.run()
	}
	if ep.stderrTap != nil {
		go ep.stderrTap.run()
	}
	go ep.pollPID()
	go ep.waitLoop()

	return ep, nil
}

func (c *runControl) allocPIDFile() (string, error) {
	dir := filepath.Join(c.sup.DataDir, "run")
	if c.sup.DataDir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := sockName(c.id) + "-" + hex.EncodeToString(rnd[:]) + ".pid"
	return filepath.Join(dir, name), nil
}

type execProcess struct {
	ctl       *runControl
	cmd       *exec.Cmd
	args      []string
	pidFile   string
	wantTTY   bool
	hasPGroup bool

	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader

	ioMu        sync.Mutex
	ptyTerm     *term
	ptyClosed   bool
	stdinW      *os.File
	stdinClosed bool
	stdoutR     *os.File

	stderrBuf *stderrCollector
	stderrTap *stderrTap

	pid         atomic.Int64
	startedOnce sync.Once
	startedCh   chan struct{}

	waitDone chan struct{}
	waitCode int
	waitErr  error

	closeOnce sync.Once
}

func (e *execProcess) Stdin() io.WriteCloser    { return e.stdin }
func (e *execProcess) Stdout() io.Reader        { return e.stdout }
func (e *execProcess) Stderr() io.Reader        { return e.stderr }
func (e *execProcess) PID() int                 { return int(e.pid.Load()) }
func (e *execProcess) Started() <-chan struct{} { return e.startedCh }

func (e *execProcess) markStarted(pid int) {
	if pid > 0 {
		e.pid.Store(int64(pid))
	}
	e.startedOnce.Do(func() { close(e.startedCh) })
}

func (e *execProcess) pollPID() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pid := readPIDFile(e.pidFile); pid > 0 {
			e.markStarted(pid)
			return
		}
		select {
		case <-e.waitDone:
			return
		case <-e.startedCh:
			return
		case <-ticker.C:
		}
	}
}

func readPIDFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (e *execProcess) waitLoop() {
	err := waitCmd(e.cmd, e.ptyTerm)

	// Fast commands can exit before the 20 ms poll tick; read the PID file
	// once more before removing it.
	if e.pid.Load() == 0 {
		if pid := readPIDFile(e.pidFile); pid > 0 {
			e.markStarted(pid)
		}
	}
	e.markStarted(0)
	_ = os.Remove(e.pidFile)

	var stderrText string
	if e.stderrBuf != nil {
		<-e.stderrBuf.done
		stderrText = e.stderrBuf.String()
	} else if e.stderrTap != nil {
		stderrText = e.stderrTap.waitTail(200 * time.Millisecond)
	}

	code := 0
	if e.cmd.ProcessState != nil {
		code = e.cmd.ProcessState.ExitCode()
	}

	if err == nil {
		e.waitCode = 0
		e.waitErr = nil
	} else {
		pid := int(e.pid.Load())
		lastLine := lastExecStderrLine(stderrText)
		isRungvErr := strings.HasPrefix(lastLine, "rungv exec: ")
		if pid == 0 || isRungvErr {
			e.waitCode = -1
			e.waitErr = &rungv.Error{
				Args:   e.args,
				Code:   code,
				Stderr: stderrText,
				Err:    err,
			}
		} else {
			e.waitCode = code
			e.waitErr = nil
		}
	}
	close(e.waitDone)
}

func lastExecStderrLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}

func (e *execProcess) Wait() (int, error) {
	<-e.waitDone
	return e.waitCode, e.waitErr
}

func (e *execProcess) Resize(rows, cols uint16) error {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	if !e.wantTTY {
		return errNoTerminal
	}
	if e.ptyTerm == nil {
		return errNoTerminal
	}
	if e.ptyClosed {
		return os.ErrClosed
	}
	return e.ptyTerm.resize(rows, cols)
}

func (e *execProcess) Signal(ctx context.Context, sig string) error {
	pid := e.PID()
	if pid == 0 {
		return errors.New("exec process has not started")
	}
	return e.ctl.client.Kill(ctx, e.ctl.target, rungv.KillOptions{Signal: sig, PID: pid})
}

func (e *execProcess) Close() error {
	e.closeOnce.Do(func() {
		// Unblock any stderr reader before waiting for the child.
		if e.stderrTap != nil {
			e.stderrTap.discardUnread()
		}

		select {
		case <-e.waitDone:
		default:
			if proc := e.cmd.Process; proc != nil {
				// SIGKILL drops the rungv exec client immediately without
				// letting rungv trap SIGTERM and forward SIGINT to the guest.
				killExecClient(proc, e.hasPGroup)
				<-e.waitDone
			}
		}

		e.ioMu.Lock()
		if e.stdinW != nil && !e.stdinClosed {
			e.stdinClosed = true
			_ = e.stdinW.Close()
		}
		if e.stdoutR != nil {
			_ = e.stdoutR.Close()
		}
		if e.ptyTerm != nil && !e.ptyClosed {
			e.ptyClosed = true
			_ = e.ptyTerm.close()
		}
		e.ioMu.Unlock()

		if e.stderrBuf != nil {
			<-e.stderrBuf.done
		}
		if e.stderrTap != nil {
			e.stderrTap.close()
		}
		_ = os.Remove(e.pidFile)
	})
	return nil
}

type ptyStdinCloser struct {
	ep *execProcess
}

func (w *ptyStdinCloser) Write(p []byte) (int, error) {
	w.ep.ioMu.Lock()
	if w.ep.ptyClosed || w.ep.ptyTerm == nil {
		w.ep.ioMu.Unlock()
		return 0, os.ErrClosed
	}
	wr := w.ep.ptyTerm.writer()
	w.ep.ioMu.Unlock()
	return wr.Write(p)
}

func (w *ptyStdinCloser) Close() error {
	w.ep.ioMu.Lock()
	defer w.ep.ioMu.Unlock()
	if w.ep.stdinClosed || w.ep.ptyClosed || w.ep.ptyTerm == nil {
		return nil
	}
	w.ep.stdinClosed = true
	return w.ep.ptyTerm.closeStdin()
}

type pipeStdinCloser struct {
	ep *execProcess
}

func (w *pipeStdinCloser) Write(p []byte) (int, error) {
	w.ep.ioMu.Lock()
	if w.ep.stdinClosed || w.ep.stdinW == nil {
		w.ep.ioMu.Unlock()
		return 0, os.ErrClosed
	}
	f := w.ep.stdinW
	w.ep.ioMu.Unlock()
	return f.Write(p)
}

func (w *pipeStdinCloser) Close() error {
	w.ep.ioMu.Lock()
	defer w.ep.ioMu.Unlock()
	if w.ep.stdinClosed || w.ep.stdinW == nil {
		return nil
	}
	w.ep.stdinClosed = true
	return w.ep.stdinW.Close()
}

// stderrCollector drains a TTY exec's stderr pipe into a bounded buffer for
// Wait's error reporting.
type stderrCollector struct {
	r    *os.File
	done chan struct{}
	buf  bytes.Buffer
}

func newStderrCollector(r *os.File) *stderrCollector {
	return &stderrCollector{r: r, done: make(chan struct{})}
}

func (c *stderrCollector) run() {
	defer close(c.done)
	defer c.r.Close()
	_, _ = io.Copy(&c.buf, io.LimitReader(c.r, 64*1024))
	_, _ = io.Copy(io.Discard, c.r)
}

func (c *stderrCollector) String() string { return c.buf.String() }

// stderrTap exposes a non-TTY exec's stderr to the caller via io.Reader while
// keeping the last lines available for Wait even when the caller has not read
// Stderr yet.
type stderrTap struct {
	r    *os.File
	done chan struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	buf     bytes.Buffer
	eof     bool
	discard bool

	tail []byte // last <= 8 KiB of stderr
}

const (
	stderrTapMaxBuf  = 256 * 1024
	stderrTapMaxTail = 8 * 1024
)

func newStderrTap(r *os.File) *stderrTap {
	t := &stderrTap{r: r, done: make(chan struct{})}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *stderrTap) run() {
	defer func() {
		_ = t.r.Close()
		t.mu.Lock()
		t.eof = true
		t.cond.Broadcast()
		t.mu.Unlock()
		close(t.done)
	}()

	var tmp [32 * 1024]byte
	for {
		n, err := t.r.Read(tmp[:])
		if n > 0 {
			chunk := tmp[:n]
			t.mu.Lock()
			t.appendTailLocked(chunk)
			for !t.discard && t.buf.Len() >= stderrTapMaxBuf {
				t.cond.Wait()
			}
			if !t.discard {
				_, _ = t.buf.Write(chunk)
				t.cond.Broadcast()
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (t *stderrTap) appendTailLocked(p []byte) {
	if len(p) >= stderrTapMaxTail {
		t.tail = append(t.tail[:0], p[len(p)-stderrTapMaxTail:]...)
		return
	}
	t.tail = append(t.tail, p...)
	if len(t.tail) > stderrTapMaxTail {
		keep := copy(t.tail, t.tail[len(t.tail)-stderrTapMaxTail:])
		t.tail = t.tail[:keep]
	}
}

func (t *stderrTap) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.buf.Len() == 0 && !t.eof && !t.discard {
		t.cond.Wait()
	}
	if t.buf.Len() == 0 {
		return 0, io.EOF
	}
	n, err := t.buf.Read(p)
	if t.buf.Len() < stderrTapMaxBuf {
		t.cond.Broadcast()
	}
	return n, err
}

func (t *stderrTap) waitTail(timeout time.Duration) string {
	select {
	case <-t.done:
	case <-time.After(timeout):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.tail)
}

func (t *stderrTap) discardUnread() {
	t.mu.Lock()
	t.discard = true
	t.buf.Reset()
	t.cond.Broadcast()
	t.mu.Unlock()
}

func (t *stderrTap) close() {
	t.discardUnread()
	_ = t.r.Close()
	<-t.done
}
