package sandbox

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"syscall"
)

// Output is a piece of what the guest of a run wrote. With a terminal
// (Spec.TTY), Stdout carries the terminal's bytes and Stderr what the
// guest wrote to its standard error, which rungv keeps apart.
type Output struct {
	Stream Stream // Stdout or Stderr, never System
	Data   []byte // owned by the receiver
}

var (
	errNotRunning = errors.New("the sandbox is not running")
	errNoTerminal = errors.New("the sandbox has no terminal")
)

// Stdin returns the writer to the standard input of the active run of id
// when started with Spec.Interactive (its pipe, or its terminal when
// Spec.TTY is also set); nil otherwise, or when none is active.
func (s *Supervisor) Stdin(id string) io.Writer {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() || !p.interactive {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.closed {
		return nil
	}
	if p.term != nil {
		return p.term.writer()
	}
	return p.stdinW
}

// CloseStdin ends the standard input of the active run of id: it closes
// the pipe, or sends end-of-file on the terminal.
func (s *Supervisor) CloseStdin(id string) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return errNotRunning
	}
	s.mu.Unlock()
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.closed {
		return errNotRunning
	}
	if p.term != nil {
		return p.term.closeStdin()
	}
	if p.stdinW == nil || p.stdinClosed {
		return nil
	}
	p.stdinClosed = true
	return p.stdinW.Close()
}

// Resize sets the window size of the terminal of the active run of id.
func (s *Supervisor) Resize(id string, rows, cols uint16) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return errNotRunning
	}
	s.mu.Unlock()
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.closed {
		return errNotRunning
	}
	if p.term == nil {
		return errNoTerminal
	}
	return p.term.resize(rows, cols)
}

// Signal sends sig to the rungv process of the active run of id. Unlike
// Stop, it does not mark the run as stopped by the user.
func (s *Supervisor) Signal(id string, sig syscall.Signal) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() || p.cmd.Process == nil {
		s.mu.Unlock()
		return errNotRunning
	}
	proc := p.cmd.Process
	s.mu.Unlock()
	return signalProc(proc, sig)
}

// maxLogLineBytes matches the 1 MiB scanner buffer cap used for LogBuffer.
const maxLogLineBytes = 1024 * 1024

// lineSplitter splits raw byte chunks into \n-delimited lines with trailing
// \r stripped, matching bufio.Scanner's line output while capping any single
// line at maxLogLineBytes.
type lineSplitter struct {
	carry []byte
	trunc bool
}

func (l *lineSplitter) write(p []byte, emit func(string)) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.appendCarry(p)
			return
		}
		l.appendCarry(p[:i])
		emit(strings.TrimRight(string(l.carry), "\r"))
		l.carry = l.carry[:0]
		l.trunc = false
		p = p[i+1:]
	}
}

func (l *lineSplitter) appendCarry(p []byte) {
	if l.trunc || len(p) == 0 {
		return
	}
	avail := maxLogLineBytes - len(l.carry)
	if len(p) > avail {
		l.carry = append(l.carry, p[:avail]...)
		l.trunc = true
		return
	}
	l.carry = append(l.carry, p...)
}

func (l *lineSplitter) flush(emit func(string)) {
	if len(l.carry) > 0 {
		emit(strings.TrimRight(string(l.carry), "\r"))
		l.carry = l.carry[:0]
		l.trunc = false
	}
}

// rungvFilter drops stderr lines written by rungv itself ("rungv: " and
// "gvx: ") while forwarding guest stderr without line buffering. At most 6
// bytes at the start of a line are held back until the prefix is decided.
type rungvFilter struct {
	state uint8
	buf   []byte
}

const (
	filterStart uint8 = iota
	filterPass
	filterDrop
)

var rungvPrefixes = []string{"rungv: ", "gvx: "}

func (f *rungvFilter) write(p []byte) []byte {
	var out []byte
	for len(p) > 0 {
		switch f.state {
		case filterPass:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				if out == nil {
					return p
				}
				return append(out, p...)
			}
			out = append(out, p[:i+1]...)
			p = p[i+1:]
			f.state = filterStart
		case filterDrop:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return out
			}
			p = p[i+1:]
			f.state = filterStart
		default: // filterStart
			b := p[0]
			p = p[1:]
			f.buf = append(f.buf, b)
			matched, couldMatch := matchRungvPrefix(f.buf)
			if matched {
				f.buf = f.buf[:0]
				f.state = filterDrop
			} else if !couldMatch {
				out = append(out, f.buf...)
				f.buf = f.buf[:0]
				if b != '\n' {
					f.state = filterPass
				}
			}
		}
	}
	return out
}

func (f *rungvFilter) flush() []byte {
	if f.state == filterStart && len(f.buf) > 0 {
		out := append([]byte(nil), f.buf...)
		f.buf = f.buf[:0]
		return out
	}
	return nil
}

func matchRungvPrefix(b []byte) (matched, couldMatch bool) {
	for _, p := range rungvPrefixes {
		if len(b) > len(p) {
			continue
		}
		if string(b) == p[:len(b)] {
			if len(b) == len(p) {
				return true, false
			}
			couldMatch = true
		}
	}
	return false, couldMatch
}
