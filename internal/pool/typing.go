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
	mu             sync.Mutex
	term           vt10x.Terminal
	sub            chan []byte
	done           chan struct{}
	lastOutputTime time.Time // updated on every PTY data chunk

	// Screen change tracking for idle detection. Updated by pollBufferInput.
	lastContentAbovePrompt string
	contentChangedAt       time.Time
	lastIdleTransitionAt   time.Time // prevents duplicate idle transitions for same stable period
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
				st.lastOutputTime = time.Now()
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

// outputSilentFor returns how long since the last PTY output was received.
func (st *sessionTerm) outputSilentFor() time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastOutputTime.IsZero() {
		return 0
	}
	return time.Since(st.lastOutputTime)
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
// emulator and extracts text after the ❯ prompt. Test-only convenience
// wrapper — production code uses parseRenderedInput via the persistent
// sessionTerm.
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

type sessionPoll struct {
	id       string
	rendered string
}

func (m *Manager) pollBufferInput() {
	m.mu.Lock()
	var toPoll []sessionPoll
	for id, s := range m.sessions {
		if s.Status != StatusIdle && s.Status != StatusProcessing {
			continue
		}
		st := m.terms[id]
		if st == nil {
			continue
		}
		toPoll = append(toPoll, sessionPoll{id: id, rendered: st.renderedScreen()})
	}
	m.mu.Unlock()

	const idleThreshold = 3 * time.Second

	for _, item := range toPoll {
		content := contentAbovePrompt(item.rendered)
		input := parseRenderedInput(item.rendered)

		m.mu.Lock()
		s := m.sessions[item.id]
		st := m.terms[item.id]
		if s == nil || st == nil {
			m.mu.Unlock()
			continue
		}

		// Track whether the content area changed since last poll
		now := time.Now()
		if content != st.lastContentAbovePrompt {
			st.lastContentAbovePrompt = content
			st.contentChangedAt = now
		}

		contentStable := !st.contentChangedAt.IsZero() && now.Sub(st.contentChangedAt) >= idleThreshold

		if s.Status == StatusIdle {
			// Content changing → processing started
			if !contentStable && !st.contentChangedAt.IsZero() {
				log.Printf("[typing] session %s: content changing (idle→processing)", s.ID)
				s.Status = StatusProcessing
				s.PendingInput = ""
				m.broadcastStatus(s, StatusIdle)
				m.mu.Unlock()
				continue
			}

			// Pending input detection
			if s.PendingInput != input {
				prev := s.PendingInput
				s.PendingInput = input
				if input != "" || prev != "" {
					s.LastUsedAt = time.Now()
				}
				log.Printf("[typing] session %s: pendingInput %q → %q", item.id, prev, input)
				m.broadcastEvent(api.Msg{
					"type": "event", "event": "updated",
					"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
				})
			}
		} else if s.Status == StatusProcessing {
			// Content stable for 3s OR PTY silent for 3s → idle.
			// Content comparison is the primary signal. PTY silence is the
			// fallback for when processing produces no visible output change
			// (e.g., no-op response, output only in prompt area).
			ptySilent := st.outputSilentFor() >= idleThreshold
			shouldTransition := (contentStable || ptySilent) && st.lastIdleTransitionAt != st.contentChangedAt

			if shouldTransition {
				st.lastIdleTransitionAt = st.contentChangedAt
				reason := "content stable"
				if !contentStable {
					reason = "PTY silent"
				}
				log.Printf("[typing] session %s: %s for %s (processing→idle)", s.ID, reason, now.Sub(st.contentChangedAt).Round(time.Second))
				m.transitionToIdle(s)
			}
		}
		m.mu.Unlock()
	}
}

// contentAbovePrompt returns the screen content above the first ───
// separator line (Claude Code's TUI divider above the prompt area).
// Changes in this area indicate processing; user typing only affects
// the prompt area below.
func contentAbovePrompt(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if containsBoxDrawing(lines[i]) {
			// Find the upper separator (there are two — skip the lower one)
			for j := i - 1; j >= 0; j-- {
				if containsBoxDrawing(lines[j]) {
					return strings.Join(lines[:j], "\n")
				}
			}
			return strings.Join(lines[:i], "\n")
		}
	}
	return rendered
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
