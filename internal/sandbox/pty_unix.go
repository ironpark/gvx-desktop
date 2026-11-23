//go:build !windows

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type term struct {
	ptmx *os.File
}

// openPTY attaches a pseudo-terminal slave to cmd's stdin and stdout and
// makes it cmd's controlling terminal (Setsid + Setctty) so window resizes
// reach the guest. The caller must call closeSlave right after cmd.Start,
// and close t on failure or after the process exits.
func openPTY(cmd *exec.Cmd) (t *term, stdout io.Reader, closeSlave func(), err error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
		cmd.SysProcAttr = attr
	}
	attr.Setsid = true
	attr.Setctty = true
	return &term{ptmx: ptmx}, ptmx, func() { _ = tty.Close() }, nil
}

// openExecPTY opens a pseudo-terminal for rungv exec -t, configures the slave
// in raw mode and sets the initial window size before Start so early input is
// neither echoed nor cooked and rungv reads the requested size at startup.
func openExecPTY(cmd *exec.Cmd, rows, cols uint16) (t *term, closeSlave func(), err error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	t = &term{ptmx: ptmx}
	if err := t.resize(rows, cols); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		return nil, nil, err
	}
	if err := setRawTermios(tty); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		return nil, nil, err
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
		cmd.SysProcAttr = attr
	}
	attr.Setsid = true
	attr.Setctty = true
	return t, func() { _ = tty.Close() }, nil
}

// startCmd starts cmd, made with exec.CommandContext(ctx), on the terminal
// openPTY or openExecPTY gave it, if any; waitCmd waits for it. On Unix
// they are cmd's own.
func startCmd(_ context.Context, cmd *exec.Cmd, _ *term) error { return cmd.Start() }

func waitCmd(cmd *exec.Cmd, _ *term) error { return cmd.Wait() }

func (t *term) writer() io.Writer { return t.ptmx }

func (t *term) reader() io.Reader { return &ptyEOFReader{f: t.ptmx} }

type ptyEOFReader struct {
	f *os.File
}

func (r *ptyEOFReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if err != nil && isPTYEIO(err) {
		return n, io.EOF
	}
	return n, err
}

func (t *term) closeStdin() error {
	_, err := t.ptmx.Write([]byte{0x04}) // EOT
	return err
}

// resize issues TIOCSWINSZ through SyscallConn().Control so ptmx is never
// switched into blocking mode by (*os.File).Fd().
func (t *term) resize(rows, cols uint16) error {
	rc, err := t.ptmx.SyscallConn()
	if err != nil {
		return err
	}
	ws := &unix.Winsize{Row: rows, Col: cols}
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws)
	}); err != nil {
		return err
	}
	return ioctlErr
}

func (t *term) close() error { return t.ptmx.Close() }

func isPTYEIO(err error) bool { return errors.Is(err, syscall.EIO) }

func waitSignal(ps *os.ProcessState) int {
	if ps == nil {
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

func signalProc(proc *os.Process, sig syscall.Signal) error {
	return proc.Signal(sig)
}

// killExecClient terminates the rungv exec client with SIGKILL (killing its
// process group when hasPGroup is true) so rungv cannot trap the signal and
// forward SIGINT to the guest command.
func killExecClient(proc *os.Process, hasPGroup bool) {
	if proc == nil {
		return
	}
	if hasPGroup && proc.Pid > 0 {
		if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err == nil {
			return
		}
	}
	_ = proc.Kill()
}
