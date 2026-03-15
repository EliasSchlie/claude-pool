// Package pty manages pseudo-terminal sessions for Claude Code processes.
package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Process wraps a Claude Code process running in a PTY.
type Process struct {
	cmd  *exec.Cmd
	ptmx *os.File
	pid  int

	mu     sync.Mutex
	buf    *RingBuffer
	done   chan struct{}
	exited bool

	subsMu sync.Mutex
	subs   map[chan []byte]struct{}
}

// SpawnOpts configures a new Claude Code process.
type SpawnOpts struct {
	Flags  string            // CLI flags (e.g., "--dangerously-skip-permissions --model haiku")
	Cwd    string            // Working directory
	Env    map[string]string // Additional env vars
	Resume string            // Claude UUID to resume (empty = fresh session)
}

// Spawn starts a new Claude Code process in a PTY.
func Spawn(opts SpawnOpts) (*Process, error) {
	args := []string{}
	if opts.Flags != "" {
		args = append(args, strings.Fields(opts.Flags)...)
	}
	if opts.Resume != "" {
		args = append(args, "--resume", opts.Resume)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = opts.Cwd

	// Build environment — strip vars that would cause issues in child sessions
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDECODE=") || // Prevents "nested session" error
			strings.HasPrefix(e, "CLAUDE_CODE_SESSION_ID=") || // Stale parent session ID
			strings.HasPrefix(e, "CLAUDE_POOL_SESSION_ID=") || // Reset per-session (set via opts.Env)
			strings.HasPrefix(e, "OPEN_COCKPIT_POOL=") { // Prevents OC orphan cleanup from killing pool sessions
			continue
		}
		filtered = append(filtered, e)
	}
	for k, v := range opts.Env {
		filtered = append(filtered, k+"="+v)
	}
	cmd.Env = filtered

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	p := &Process{
		cmd:  cmd,
		ptmx: ptmx,
		pid:  cmd.Process.Pid,
		buf:  NewRingBuffer(256 * 1024), // 256KB ring buffer
		done: make(chan struct{}),
		subs: make(map[chan []byte]struct{}),
	}

	// Read PTY output into ring buffer
	go p.readLoop()

	// Wait for process exit
	go func() {
		cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.mu.Unlock()
		close(p.done)
	}()

	return p, nil
}

// PID returns the process ID.
func (p *Process) PID() int {
	return p.pid
}

// Done returns a channel that's closed when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Exited returns true if the process has exited.
func (p *Process) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// ExitCode returns the process exit code, or -1 if not yet exited.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	exited := p.exited
	p.mu.Unlock()
	if !exited {
		return -1
	}
	// Safe: ProcessState is immutable after cmd.Wait() returns.
	return p.cmd.ProcessState.ExitCode()
}

// Write sends bytes to the PTY input.
func (p *Process) Write(data []byte) error {
	_, err := p.ptmx.Write(data)
	return err
}

// WriteString sends a string to the PTY input.
func (p *Process) WriteString(s string) error {
	return p.Write([]byte(s))
}

// Buffer returns the current terminal buffer contents.
func (p *Process) Buffer() []byte {
	return p.buf.Bytes()
}

// BufferTail returns the last n bytes of the terminal buffer.
func (p *Process) BufferTail(n int) []byte {
	return p.buf.Tail(n)
}

// Kill sends SIGKILL to the process.
func (p *Process) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	// Best-effort: process may have already exited.
	err := p.cmd.Process.Kill()
	if err != nil && p.Exited() {
		return nil
	}
	return err
}

// Close cleans up the PTY. Closing the fd causes readLoop to exit,
// which stops broadcasting. Subscribers should be cleaned up via
// Unsubscribe before or after Close.
func (p *Process) Close() {
	p.ptmx.Close()
}

func (p *Process) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			data := buf[:n]
			p.buf.Write(data)
			p.broadcast(data)
		}
		if err != nil {
			if err != io.EOF {
				// PTY closed — expected on process exit
			}
			return
		}
	}
}

func (p *Process) broadcast(data []byte) {
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	for ch := range p.subs {
		// Non-blocking send — drop if subscriber is slow
		cp := make([]byte, len(data))
		copy(cp, data)
		select {
		case ch <- cp:
		default:
		}
	}
}

// Subscribe returns a channel that receives copies of all PTY output.
// Call Unsubscribe when done.
func (p *Process) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	p.subsMu.Lock()
	p.subs[ch] = struct{}{}
	p.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (p *Process) Unsubscribe(ch chan []byte) {
	p.subsMu.Lock()
	delete(p.subs, ch)
	p.subsMu.Unlock()
	close(ch)
}

// Ptmx returns the PTY master file descriptor for direct I/O.
func (p *Process) Ptmx() *os.File {
	return p.ptmx
}

// RingBuffer is a simple fixed-size ring buffer.
type RingBuffer struct {
	mu   sync.Mutex
	data []byte
	size int
	pos  int
	full bool
}

// NewRingBuffer creates a ring buffer of the given size.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		data: make([]byte, size),
		size: size,
	}
}

// Write appends data to the ring buffer using bulk copies.
func (r *RingBuffer) Write(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(data) >= r.size {
		// Data larger than buffer — keep only the tail
		copy(r.data, data[len(data)-r.size:])
		r.pos = 0
		r.full = true
		return
	}

	// Copy in up to two chunks (wrap around end)
	first := r.size - r.pos
	if first >= len(data) {
		copy(r.data[r.pos:], data)
		r.pos += len(data)
		if r.pos >= r.size {
			r.pos = 0
			r.full = true
		}
	} else {
		copy(r.data[r.pos:], data[:first])
		copy(r.data, data[first:])
		r.pos = len(data) - first
		r.full = true
	}
}

// Bytes returns the current buffer contents in order.
func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		return append([]byte(nil), r.data[:r.pos]...)
	}
	result := make([]byte, r.size)
	copy(result, r.data[r.pos:])
	copy(result[r.size-r.pos:], r.data[:r.pos])
	return result
}

// Tail returns the last n bytes of the buffer (or the full buffer if smaller).
func (r *RingBuffer) Tail(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := r.pos
	if r.full {
		total = r.size
	}
	if n >= total {
		// Return everything — same as Bytes()
		if !r.full {
			return append([]byte(nil), r.data[:r.pos]...)
		}
		result := make([]byte, r.size)
		copy(result, r.data[r.pos:])
		copy(result[r.size-r.pos:], r.data[:r.pos])
		return result
	}

	// n < total: extract just the tail
	result := make([]byte, n)
	// Logical end is at r.pos (or wraps). Work backwards from the write head.
	if r.pos >= n {
		copy(result, r.data[r.pos-n:r.pos])
	} else {
		// Wraps around: take from end of data, then from start
		fromEnd := n - r.pos
		copy(result, r.data[r.size-fromEnd:r.size])
		copy(result[fromEnd:], r.data[:r.pos])
	}
	return result
}
