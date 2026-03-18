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

func (st *sessionTerm) renderedScreen() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.term.String()
}

func (st *sessionTerm) outputSilentFor() time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastOutputTime.IsZero() {
		return 0
	}
	return time.Since(st.lastOutputTime)
}

func (st *sessionTerm) stop(proc *ptyPkg.Process) {
	close(st.done)
	if proc != nil {
		proc.Unsubscribe(st.sub)
	}
}

// parseRenderedInput extracts text typed after the ❯ prompt character from
// rendered terminal screen content.
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
// emulator and extracts text after the ❯ prompt. Test-only convenience wrapper.
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

func (m *Manager) startTypingPoller() {
	go m.typingPollLoop()
}

func (m *Manager) triggerBufferPoll() {
	select {
	case m.bufferPollSignal <- struct{}{}:
	default:
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
			time.Sleep(50 * time.Millisecond)
			m.pollBufferInput()
		}
	}
}

type slotPoll struct {
	slotIdx  int
	rendered string
}

// pollBufferInput detects idle/processing transitions and pending input on slots.
func (m *Manager) pollBufferInput() {
	m.mu.Lock()
	var toPoll []slotPoll
	for _, sl := range m.slots {
		if sl.Term == nil {
			continue
		}
		switch sl.State {
		case SlotIdle, SlotProcessing, SlotClearing, SlotResuming:
			toPoll = append(toPoll, slotPoll{slotIdx: sl.Index, rendered: sl.Term.renderedScreen()})
		}
	}
	m.mu.Unlock()

	const idleThreshold = 1 * time.Second

	for _, item := range toPoll {
		content := contentAbovePrompt(item.rendered)
		input := parseRenderedInput(item.rendered)

		m.mu.Lock()
		if item.slotIdx >= len(m.slots) {
			m.mu.Unlock()
			continue
		}
		sl := m.slots[item.slotIdx]
		if sl.Term == nil {
			m.mu.Unlock()
			continue
		}

		now := time.Now()
		if content != sl.Term.lastContentAbovePrompt {
			sl.Term.lastContentAbovePrompt = content
			sl.Term.contentChangedAt = now
		}

		contentStable := !sl.Term.contentChangedAt.IsZero() && now.Sub(sl.Term.contentChangedAt) >= idleThreshold

		switch sl.State {
		case SlotIdle:
			// Content changing → processing started
			if !contentStable && !sl.Term.contentChangedAt.IsZero() {
				log.Printf("[typing] slot %d: content changing (idle→processing)", sl.Index)
				sl.State = SlotProcessing
				sl.PendingInput = ""
				if s := m.sessions[sl.SessionID]; s != nil {
					s.Status = StatusProcessing
					s.PendingInput = ""
					m.broadcastStatus(s, StatusIdle)
				}
				m.mu.Unlock()
				continue
			}

			// Pending input detection — surface to session
			if sl.PendingInput != input {
				prev := sl.PendingInput
				sl.PendingInput = input
				if s := m.sessions[sl.SessionID]; s != nil {
					s.PendingInput = input
					if input != "" || prev != "" {
						s.LastUsedAt = time.Now()
					}
					log.Printf("[typing] slot %d session %s: pendingInput %q → %q", sl.Index, s.ID, prev, input)
					m.broadcastEvent(api.Msg{
						"type": "event", "event": "updated",
						"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
					})
				}
			}

		case SlotProcessing, SlotClearing, SlotResuming:
			ptySilent := sl.Term.outputSilentFor() >= idleThreshold
			shouldTransition := (contentStable || ptySilent) && sl.Term.lastIdleTransitionAt != sl.Term.contentChangedAt

			if shouldTransition {
				sl.Term.lastIdleTransitionAt = sl.Term.contentChangedAt
				reason := "content stable"
				if !contentStable {
					reason = "PTY silent"
				}
				log.Printf("[typing] slot %d: %s for %s (%s→idle)", sl.Index, reason, now.Sub(sl.Term.contentChangedAt).Round(time.Second), sl.State)
				m.transitionSlotToIdle(sl)
			}
		}
		m.mu.Unlock()
	}
}

// contentAbovePrompt returns the screen content above the first ───
// separator line (Claude Code's TUI divider above the prompt area).
func contentAbovePrompt(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if containsBoxDrawing(lines[i]) {
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

// newSlotTerm creates and returns a persistent terminal emulator for a slot.
// Must be called with m.mu held.
func (m *Manager) newSlotTerm(sl *Slot) *sessionTerm {
	proc := sl.Process
	cols, rows := 80, 24
	if c, r, err := proc.GetSize(); err == nil {
		cols, rows = int(c), int(r)
	}
	return newSessionTerm(proc, cols, rows)
}

// stopSlotTerm stops and cleans up a slot's terminal emulator.
// Must be called with m.mu held.
func (m *Manager) stopSlotTerm(sl *Slot) {
	if sl.Term != nil {
		sl.Term.stop(sl.Process)
		sl.Term = nil
	}
}
