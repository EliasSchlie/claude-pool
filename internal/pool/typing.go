package pool

import (
	"log"
	"strings"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/hinshun/vt10x"
)

const promptChar = "❯"

// containsBoxDrawing checks if s contains Unicode box-drawing or block element
// characters (U+2500–U+259F). These appear in Claude Code's TUI status bar
// and cause false positive pendingInput detections.
func containsBoxDrawing(s string) bool {
	for _, r := range s {
		if r >= 0x2500 && r <= 0x259F {
			return true
		}
	}
	return false
}

// renderBuffer processes raw PTY output through a VT100 terminal emulator
// to produce the actual rendered screen content. This correctly handles
// cursor movement, screen clearing, and other escape sequences that
// Claude Code's TUI uses heavily.
func renderBuffer(buf []byte, cols, rows int) string {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	term := vt10x.New(vt10x.WithSize(cols, rows))
	term.Write(buf)
	return term.String()
}

// parseBufferInput extracts text typed after the ❯ prompt character.
// The buf is first rendered through a VT100 terminal emulator to resolve
// cursor movements and produce the actual screen content, then the
// rendered lines are searched backwards for the prompt.
func parseBufferInput(buf []byte, cols, rows int) string {
	rendered := renderBuffer(buf, cols, rows)

	lines := strings.Split(rendered, "\n")

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " ")
		if idx := strings.LastIndex(line, promptChar); idx >= 0 {
			input := strings.TrimSpace(line[idx+len(promptChar):])
			if containsBoxDrawing(input) {
				continue
			}
			return input
		}
	}
	return ""
}

// startTypingPoller launches a goroutine that periodically scans PTY buffers
// of idle sessions to detect text typed after the ❯ prompt. This mirrors
// Open Cockpit's buffer-based typing detection.
//
// The poller is the sole source of pendingInput — no keystroke tracking.
// PTY writes (attach, debug input) trigger immediate re-polls via
// triggerBufferPoll so detection latency stays low.
func (m *Manager) startTypingPoller() {
	go m.typingPollLoop()
}

// triggerBufferPoll signals that a PTY write occurred and the buffer should
// be re-checked soon. Called after any raw write (attach input, debug input).
func (m *Manager) triggerBufferPoll() {
	select {
	case m.bufferPollSignal <- struct{}{}:
	default:
		// Already signaled, poll will run soon
	}
}

func (m *Manager) typingPollLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.pollBufferInput()
		case <-m.bufferPollSignal:
			// Brief delay to let the terminal process the input
			time.Sleep(50 * time.Millisecond)
			m.pollBufferInput()
		}
	}
}

type bufferCheck struct {
	id   string
	buf  []byte
	cols int
	rows int
}

func (m *Manager) pollBufferInput() {
	m.mu.Lock()
	var toCheck []bufferCheck
	for id, s := range m.sessions {
		// Only poll idle sessions — fresh sessions have startup artifacts
		// (trust dialog, etc.) that cause false positives.
		if s.Status != StatusIdle {
			continue
		}
		proc := m.procs[id]
		if proc == nil {
			continue
		}
		cols, rows := 80, 24
		if c, r, err := proc.GetSize(); err == nil {
			cols, rows = int(c), int(r)
		}
		toCheck = append(toCheck, bufferCheck{
			id:   id,
			buf:  proc.Buffer(),
			cols: cols,
			rows: rows,
		})
	}
	m.mu.Unlock()

	for _, item := range toCheck {
		input := parseBufferInput(item.buf, item.cols, item.rows)

		m.mu.Lock()
		s := m.sessions[item.id]
		if s == nil || s.Status != StatusIdle {
			m.mu.Unlock()
			continue
		}
		if s.PendingInput != input {
			prev := s.PendingInput
			s.PendingInput = input
			if input != "" || prev != "" {
				s.LastUsedAt = time.Now()
			}
			log.Printf("[typing] session %s: pendingInput %q → %q (buffer poll)", item.id, prev, input)
			m.broadcastEvent(api.Msg{
				"type": "event", "event": "updated",
				"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
			})
		}
		m.mu.Unlock()
	}
}
