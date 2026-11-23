//go:build windows

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test binary is its own child: with GVX_CONPTY_CHILD set, TestConsoleChild
// plays rungv on the console, saying what it finds there.
func TestConsoleChild(t *testing.T) {
	if os.Getenv("GVX_CONPTY_CHILD") != "1" {
		t.Skip("run by TestPseudoconsole")
	}
	var mode uint32
	inConsole := windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
	outConsole := windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode) == nil
	errConsole := windows.GetConsoleMode(windows.Handle(os.Stderr.Fd()), &mode) == nil
	var info windows.ConsoleScreenBufferInfo
	_ = windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info)
	cols := info.Window.Right - info.Window.Left + 1
	rows := info.Window.Bottom - info.Window.Top + 1
	fmt.Fprintf(os.Stdout, "OUT in=%t out=%t size=%dx%d\r\n", inConsole, outConsole, rows, cols)
	fmt.Fprintf(os.Stderr, "ERR err-console=%t\n", errConsole)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\r')
	fmt.Fprintf(os.Stdout, "GOT %q\r\n", strings.TrimSpace(line))
	// Wait for the resize, then report the new size.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info)
		if c := info.Window.Right - info.Window.Left + 1; c != cols {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stdout, "SIZE %dx%d\r\n", info.Window.Bottom-info.Window.Top+1, info.Window.Right-info.Window.Left+1)
	os.Exit(7)
}

// rungv on a pseudoconsole: its standard input and output are the console,
// sized as asked and resized, its standard error the caller's pipe; input
// reaches it; its exit code comes back, and the output ends when it exits.
func TestPseudoconsole(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleChild$", "-test.v")
	cmd.Env = append(os.Environ(), "GVX_CONPTY_CHILD=1")
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderrW
	tm, closeSlave, err := openExecPTY(cmd, 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.close()
	if err := startCmd(context.Background(), cmd, tm); err != nil {
		t.Fatal(err)
	}
	closeSlave()
	_ = stderrW.Close()

	var (
		mu  sync.Mutex
		out bytes.Buffer
	)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 4096)
		for {
			n, err := tm.reader().Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	errText := make(chan string, 1)
	go func() { b, _ := io.ReadAll(stderrR); errText <- string(b) }()
	seen := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(out.String(), s)
	}
	waitSeen := func(s string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !seen(s) {
			if time.Now().After(deadline) {
				mu.Lock()
				t.Fatalf("no %q in the console's output:\n%q", s, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	waitSeen("OUT in=true out=true size=30x100")
	if _, err := tm.writer().Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waitSeen(`GOT "hello"`)
	if err := tm.resize(40, 120); err != nil {
		t.Fatal(err)
	}
	waitSeen("SIZE 40x120")

	err = waitCmd(cmd, tm)
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 || err == nil {
		t.Errorf("exit: %v %v", cmd.ProcessState, err)
	}
	select {
	case <-outDone:
	case <-time.After(10 * time.Second):
		t.Error("the console's output did not end after the process exited")
	}
	if e := <-errText; !strings.Contains(e, "ERR err-console=false") {
		t.Errorf("stderr: %q", e)
	}
	if seen("ERR ") {
		t.Error("stderr reached the console")
	}
}
