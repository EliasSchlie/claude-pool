package pool

import (
	"log"
	"strings"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

const promptChar = "❯"

// parseBufferInput extracts text typed after the ❯ prompt character in
// terminal output. Searches backwards from the end of the buffer to find
// the most recent prompt line. Callers should pass only the buffer tail
// (e.g. 8KB) to avoid processing the full ring buffer.
//
// Split on both \n and \r — Claude Code's TUI uses \r for status bar
// redraws, and without splitting on \r the status bar text gets
// concatenated with the prompt line causing false positives.
func parseBufferInput(buf []byte) string {
	stripped := stripANSI(string(buf))

	// Split on any line boundary (\n, \r, or \r\n)
	lines := strings.FieldsFunc(stripped, func(r rune) bool {
		return r == '\n' || r == '\r'
	})

	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if idx := strings.LastIndex(line, promptChar); idx >= 0 {
			input := strings.TrimSpace(line[idx+len(promptChar):])
			// Reject false positives: TUI artifacts contain box-drawing
			// chars, status bar text, etc. Real user input is plain text.
			if strings.ContainsAny(input, "─│┌┐└┘├┤┬┴┼━┃") {
				continue
			}
			return input
		}
	}
	return ""
}

// startTypingPoller launches a goroutine that periodically scans PTY buffers
// of idle sessions (without an attach pipe) to detect text typed after the ❯
// prompt. This mirrors Open Cockpit's buffer-based typing detection.
//
// Sessions with an active attach pipe are skipped — the attach handler
// provides lower-latency keystroke-by-keystroke tracking.
func (m *Manager) startTypingPoller() {
	go m.typingPollLoop()
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
		}
	}
}

type bufferCheck struct {
	id  string
	buf []byte
}

func (m *Manager) pollBufferInput() {
	m.mu.Lock()
	var toCheck []bufferCheck
	for id, s := range m.sessions {
		// Only poll idle sessions — fresh sessions have startup artifacts
		// (trust dialog, etc.) that cause false positives.
		if s.Status != StatusIdle || m.pipes[id] != nil {
			continue
		}
		proc := m.procs[id]
		if proc == nil {
			continue
		}
		toCheck = append(toCheck, bufferCheck{id: id, buf: proc.BufferTail(8192)})
	}
	m.mu.Unlock()

	for _, item := range toCheck {
		input := parseBufferInput(item.buf)

		m.mu.Lock()
		s := m.sessions[item.id]
		if s == nil || s.Status != StatusIdle || m.pipes[item.id] != nil {
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
