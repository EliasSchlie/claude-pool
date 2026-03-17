package pool

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
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

// sessionTerm wraps a persistent headless VT100 terminal emulator that
// receives PTY output incrementally — just like a real terminal. This
// produces correct rendering for cursor movement, insert, backspace, etc.
// Re-rendering the full buffer from scratch each poll causes vt10x to
// produce wrong output for complex cursor sequences.
type sessionTerm struct {
	mu   sync.Mutex
	term vt10x.Terminal
	sub  chan []byte
	done chan struct{}
}

// newSessionTerm creates a persistent terminal emulator for a session's process.
// It subscribes to PTY output and feeds it incrementally. Caller must call
// stop() when the process is no longer associated with this session.
func newSessionTerm(proc *ptyPkg.Process, cols, rows int) *sessionTerm {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	st := &sessionTerm{
		term: vt10x.New(vt10x.WithSize(cols, rows)),
		sub:  proc.Subscribe(),
		done: make(chan struct{}),
	}

	// Don't feed the existing buffer — it can corrupt vt10x state when
	// replayed in bulk. Subscribe delivers all NEW output incrementally,
	// which vt10x processes correctly (just like a real terminal).
	// By the time we poll for pendingInput, all relevant output will
	// have arrived through the subscriber.

	// Feed new output incrementally
	go func() {
		for {
			select {
			case data, ok := <-st.sub:
				if !ok {
					return
				}
				st.mu.Lock()
				st.term.Write(data)
				st.mu.Unlock()
			case <-st.done:
				return
			}
		}
	}()

	return st
}

// renderedScreen returns the current terminal screen content.
func (st *sessionTerm) renderedScreen() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.term.String()
}

// stop unsubscribes from PTY output and stops the feed goroutine.
func (st *sessionTerm) stop(proc *ptyPkg.Process) {
	close(st.done)
	if proc != nil {
		proc.Unsubscribe(st.sub)
	}
}

// parseRenderedInput extracts text typed after the ❯ prompt character from
// rendered terminal screen content. Searches backwards from the end to find
// the most recent prompt line.
func parseRenderedInput(rendered string) string {
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

// parseBufferInput renders raw PTY output through a fresh VT100 terminal
// emulator and extracts text after the ❯ prompt. Used by unit tests and
// as fallback when no persistent terminal exists.
func parseBufferInput(buf []byte, cols, rows int) string {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	term := vt10x.New(vt10x.WithSize(cols, rows))
	term.Write(buf)
	return parseRenderedInput(term.String())
}

// startTypingPoller launches a goroutine that periodically reads the
// persistent terminal emulator for each idle session to detect text typed
// after the ❯ prompt. This mirrors Open Cockpit's buffer-based typing
// detection.
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

type sessionCheck struct {
	id       string
	rendered string
}

func (m *Manager) pollBufferInput() {
	m.mu.Lock()
	var toCheck []sessionCheck
	for id, s := range m.sessions {
		// Only poll idle sessions — fresh sessions have startup artifacts
		// (trust dialog, etc.) that cause false positives.
		if s.Status != StatusIdle {
			continue
		}
		st := m.terms[id]
		if st == nil {
			continue
		}
		toCheck = append(toCheck, sessionCheck{id: id, rendered: st.renderedScreen()})
	}
	m.mu.Unlock()

	for _, item := range toCheck {
		input := parseRenderedInput(item.rendered)

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

// startSessionTerm creates and registers a persistent terminal emulator
// for a session. Must be called with m.mu held.
func (m *Manager) startSessionTerm(sessionID string, proc *ptyPkg.Process) {
	cols, rows := 80, 24
	if c, r, err := proc.GetSize(); err == nil {
		cols, rows = int(c), int(r)
	}
	st := newSessionTerm(proc, cols, rows)
	m.terms[sessionID] = st
}

// stopSessionTerm stops and removes a session's terminal emulator.
// Must be called with m.mu held.
func (m *Manager) stopSessionTerm(sessionID string) {
	if st := m.terms[sessionID]; st != nil {
		proc := m.procs[sessionID]
		st.stop(proc)
		delete(m.terms, sessionID)
	}
}
