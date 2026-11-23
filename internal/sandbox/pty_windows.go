//go:build windows

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// term is a Windows pseudoconsole (ConPTY) that rungv runs on, as on a
// pty elsewhere: rungv finds a console on its standard input and output,
// puts it in raw mode with VT input, and follows its size. Its standard
// error stays a pipe of the caller's, so rungv's own lines (ports, errors)
// are not mixed into the terminal's bytes.
//
// exec.Cmd cannot start a process on a pseudoconsole, so startCmd starts
// cmd itself and sets cmd.Process; waitCmd then waits through it.
type term struct {
	console windows.Handle
	in      *os.File // the console's input, written
	out     *os.File // the console's output, read
	// The console's own ends of the pipes, closed once rungv started.
	inRead, outWrite windows.Handle

	closeConsole func()
	started      bool
}

func newTerm(rows, cols uint16) (t *term, err error) {
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, err
	}
	t = &term{
		in:     os.NewFile(uintptr(inWrite), "console-in"),
		out:    os.NewFile(uintptr(outRead), "console-out"),
		inRead: inRead, outWrite: outWrite,
	}
	if err := windows.CreatePseudoConsole(coord(rows, cols), inRead, outWrite, 0, &t.console); err != nil {
		t.closeSlave()
		_ = t.in.Close()
		_ = t.out.Close()
		return nil, err
	}
	console := t.console
	t.closeConsole = sync.OnceFunc(func() { windows.ClosePseudoConsole(console) })
	return t, nil
}

// coord is a size as a console takes it.
func coord(rows, cols uint16) windows.Coord {
	return windows.Coord{X: int16(min(max(cols, 2), 0x7fff)), Y: int16(min(max(rows, 2), 0x7fff))}
}

// openPTY gives cmd a pseudoconsole of 80 by 24, which Resize changes.
func openPTY(_ *exec.Cmd) (*term, io.Reader, func(), error) {
	t, err := newTerm(24, 80)
	if err != nil {
		return nil, nil, nil, err
	}
	return t, t.reader(), t.closeSlave, nil
}

// openExecPTY gives cmd a pseudoconsole rows by cols (24 by 80 for zero).
// A console has no line discipline to set raw: rungv sets the console's
// modes itself.
func openExecPTY(_ *exec.Cmd, rows, cols uint16) (*term, func(), error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	t, err := newTerm(rows, cols)
	if err != nil {
		return nil, nil, err
	}
	return t, t.closeSlave, nil
}

// closeSlave closes the console's ends of the pipes, which it holds itself.
func (t *term) closeSlave() {
	if t.inRead != 0 {
		windows.CloseHandle(t.inRead)
		t.inRead = 0
	}
	if t.outWrite != 0 {
		windows.CloseHandle(t.outWrite)
		t.outWrite = 0
	}
}

// startCmd starts cmd, made with exec.CommandContext(ctx): on its terminal
// when it has one, as exec.Cmd.Start would otherwise. cmd.Stderr must then
// be an *os.File, or nil.
func startCmd(ctx context.Context, cmd *exec.Cmd, t *term) error {
	if t == nil {
		return cmd.Start()
	}
	if cmd.Process != nil {
		return errors.New("exec: already started")
	}
	var stderr windows.Handle
	switch f := cmd.Stderr.(type) {
	case nil:
	case *os.File:
		stderr = windows.Handle(f.Fd())
	default:
		return errors.New("a terminal's command needs a file for its standard error")
	}
	p, pi, err := startOnConsole(cmd, t.console, stderr)
	if err != nil {
		return err
	}
	cmd.Process = p
	t.started = true
	// exec.CommandContext kills the process when ctx ends; so does this.
	stop := context.AfterFunc(ctx, func() { _ = p.Kill() })
	// A pseudoconsole's output does not end when its process does: closing
	// the console once rungv exits ends it after the last of it.
	go func() {
		_, _ = windows.WaitForSingleObject(pi, windows.INFINITE)
		windows.CloseHandle(pi)
		stop()
		t.closeConsole()
	}()
	return nil
}

// startOnConsole creates cmd's process on console with stderr, when set, as
// its standard error, and returns it with a handle of its own to wait on.
func startOnConsole(cmd *exec.Cmd, console, stderr windows.Handle) (*os.Process, windows.Handle, error) {
	self := windows.CurrentProcess()
	nattr := uint32(1)
	var inherit windows.Handle
	if stderr != 0 {
		// The one handle rungv inherits, and only it: the list keeps the
		// app's other inheritable handles out.
		if err := windows.DuplicateHandle(self, stderr, self, &inherit, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, 0, err
		}
		defer windows.CloseHandle(inherit)
		nattr++
	}
	attrs, err := windows.NewProcThreadAttributeList(nattr)
	if err != nil {
		return nil, 0, err
	}
	defer attrs.Delete()
	// The attribute's value is the console's handle itself.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		return nil, 0, err
	}
	si := &windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Standard handles of the caller's choosing: none for input and
	// output, which are then the console's, and stderr for errors.
	si.Flags = windows.STARTF_USESTDHANDLES
	handles := []windows.Handle{inherit}
	if inherit != 0 {
		if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
			return nil, 0, err
		}
		si.StdErr = inherit
	}
	si.ProcThreadAttributeList = attrs.List()

	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, 0, err
	}
	app, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return nil, 0, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return nil, 0, err
		}
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	block, err := envBlock(env)
	if err != nil {
		return nil, 0, err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(app, line, nil, nil, inherit != 0, flags, block, dir, &si.StartupInfo, &pi); err != nil {
		return nil, 0, &os.PathError{Op: "fork/exec", Path: cmd.Path, Err: err}
	}
	runtime.KeepAlive(handles)
	windows.CloseHandle(pi.Thread)
	// The process cannot go, nor its PID be reused, while pi.Process is
	// open, so FindProcess finds this one.
	p, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return nil, 0, err
	}
	return p, pi.Process, nil
}

// waitCmd waits for cmd as exec.Cmd.Wait does, for one startCmd started on
// a terminal too.
func waitCmd(cmd *exec.Cmd, t *term) error {
	if t == nil || !t.started {
		return cmd.Wait()
	}
	if cmd.ProcessState != nil {
		return errors.New("exec: Wait was already called")
	}
	state, err := cmd.Process.Wait()
	if err != nil {
		return err
	}
	cmd.ProcessState = state
	if !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return nil
}

// envBlock is env as CreateProcess takes it: each variable ended by a
// NUL, the whole by another, sorted by name, case aside.
func envBlock(env []string) (*uint16, error) {
	env = slices.Clone(env)
	name := func(kv string) string {
		// "=C:=C:\dir", as cmd keeps the drives' directories, is named "=C:".
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			return strings.ToUpper(kv[:i+1])
		}
		return strings.ToUpper(kv)
	}
	slices.SortStableFunc(env, func(a, b string) int { return strings.Compare(name(a), name(b)) })
	var block []uint16
	for _, kv := range env {
		if strings.ContainsRune(kv, 0) {
			return nil, windows.ERROR_BAD_ENVIRONMENT
		}
		block = append(block, windows.StringToUTF16(kv)...)
	}
	block = append(block, 0)
	return &block[0], nil
}

func (t *term) writer() io.Writer { return t.in }

func (t *term) reader() io.Reader { return consoleReader{t.out} }

// consoleReader ends at the console's close, which breaks its pipe.
type consoleReader struct{ f *os.File }

func (r consoleReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	return n, err
}

func (t *term) closeStdin() error {
	_, err := t.in.Write([]byte{0x04}) // ^D, as a terminal sends EOF
	return err
}

func (t *term) resize(rows, cols uint16) error {
	return windows.ResizePseudoConsole(t.console, coord(rows, cols))
}

// close closes the console and this process's ends of its pipes.
func (t *term) close() error {
	t.closeConsole()
	t.closeSlave()
	return errors.Join(t.in.Close(), t.out.Close())
}

func isPTYEIO(error) bool             { return false }
func waitSignal(*os.ProcessState) int { return 0 }

func signalProc(proc *os.Process, sig syscall.Signal) error {
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM || sig == syscall.SIGINT {
		return proc.Kill()
	}
	return proc.Signal(sig)
}

func killExecClient(proc *os.Process, _ bool) {
	if proc != nil {
		_ = proc.Kill()
	}
}
