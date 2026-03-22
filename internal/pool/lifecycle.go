package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// --- Session creation & resolution ---

func (m *Manager) newSession(parentID string) *Session {
	now := time.Now()
	cwd := m.paths.Root
	if cfg, err := m.config.Load(); err != nil {
		log.Printf("[session] config load error (using defaults): %v", err)
	} else if cfg.Dir != "" {
		cwd = cfg.Dir
	}
	return &Session{
		ID:         generateSessionID(),
		ParentID:   parentID,
		CreatedAt:  now,
		LastUsedAt: now,
		SpawnCwd:   cwd,
		Cwd:        cwd,
		SlotIndex:  -1,
	}
}

func (m *Manager) resolveSession(sessionID string) *Session {
	if s, ok := m.sessions[sessionID]; ok {
		return s
	}
	var match *Session
	for _, s := range m.sessions {
		if strings.HasPrefix(s.ID, sessionID) {
			if match != nil {
				return nil // ambiguous
			}
			match = s
		}
	}
	return match
}

// --- Slot lifecycle ---

// spawnSlot starts a Claude process in a slot. Always spawns fresh —
// session restoration is handled by delivering /resume after the slot is ready.
func (m *Manager) spawnSlot(sl *Slot) {
	cfg, err := m.config.Load()
	if err != nil {
		log.Printf("[spawn] slot %d: config load error (using defaults): %v", sl.Index, err)
	}
	flags := cfg.Flags
	cwd := cfg.Dir
	if cwd == "" {
		cwd = m.paths.Root
	}
	log.Printf("[spawn] slot %d: flags=%q cwd=%s", sl.Index, flags, cwd)

	sl.State = SlotSpawning

	env := map[string]string{
		"CLAUDE_POOL_DIR": m.paths.Root,
	}
	if sl.SessionID != "" {
		env["CLAUDE_POOL_SESSION_ID"] = sl.SessionID
	}

	opts := ptyPkg.SpawnOpts{
		Flags: flags,
		Cwd:   cwd,
		Env:   env,
	}

	proc, err := ptyPkg.Spawn(opts)
	if err != nil {
		log.Printf("[spawn] slot %d: error: %v", sl.Index, err)
		sl.State = SlotCrashed
		// If a session is bound, track spawn failures
		if s := m.sessions[sl.SessionID]; s != nil {
			s.SpawnAttempts++
			if s.SpawnAttempts >= maxSpawnAttempts {
				log.Printf("[spawn] slot %d session %s: %d consecutive failures, marking error", sl.Index, s.ID, s.SpawnAttempts)
				prevStatus := s.Status
				m.unbindSession(sl)
				s.Status = StatusError
				m.broadcastStatus(s, prevStatus)
			}
		}
		return
	}

	sl.Process = proc
	sl.Term = m.newSlotTerm(sl)

	// Reset spawn attempts on success
	if s := m.sessions[sl.SessionID]; s != nil {
		s.SpawnAttempts = 0
		s.Flags = flags
	}

	log.Printf("[spawn] slot %d: spawned pid=%d (session=%s)", sl.Index, sl.PID(), sl.SessionID)

	m.watchProcessDone(sl)
	// Skip trust handler when permissions are already bypassed — the trust
	// prompt never appears, so the handler just wastes 30s timing out.
	if !strings.Contains(flags, "--dangerously-skip-permissions") {
		m.autoAcceptTrust(sl)
	}
	go m.watchIdleSignal(sl)
}

// bindSession loads a session into a slot. Must be called with m.mu held.
func (m *Manager) bindSession(sl *Slot, s *Session) {
	sl.SessionID = s.ID
	s.SlotIndex = sl.Index
	s.Cwd = sl.cwd()
	log.Printf("[bind] session %s → slot %d (pid=%d)", s.ID, sl.Index, sl.PID())
}

// unbindSession removes a session from its slot. Must be called with m.mu held.
func (m *Manager) unbindSession(sl *Slot) {
	if sl.SessionID == "" {
		return
	}
	if s := m.sessions[sl.SessionID]; s != nil {
		s.SlotIndex = -1
		s.PendingInput = ""
	}
	log.Printf("[unbind] session %s ← slot %d", sl.SessionID, sl.Index)
	sl.SessionID = ""
}

// clearSlot initiates the multi-step clear workflow on a slot.
// /clear → /update-plugins → /clear. Each step is delivered when the
// previous one completes (detected by the typing poller).
// Must be called with m.mu held.
func (m *Manager) clearSlot(sl *Slot) {
	log.Printf("[clear-slot] slot %d: starting clear workflow (pid=%d)", sl.Index, sl.PID())

	// Close attach pipe
	if sl.Pipe != nil {
		sl.Pipe.Close()
		sl.Pipe = nil
	}

	// Remove stale PID→UUID mapping so the idle signal watcher doesn't
	// read the old session's UUID before /clear writes the new one.
	os.Remove(m.paths.SessionPID(sl.PID()))

	sl.ClearQueue = []string{"/update-plugins", "/clear"}
	sl.State = SlotClearing

	m.clearIdleSignals(sl.PID())
	if sl.Term != nil {
		sl.Term.resetIdleTracking()
	}
	m.deliverPrompt(sl, "/clear")
}

// --- Offload & Archive ---

func (m *Manager) offloadSession(s *Session) {
	sl := m.slotForSession(s)
	if sl == nil {
		// Not loaded — just mark offloaded
		prevStatus := s.Status
		s.Status = StatusOffloaded
		s.PendingInput = ""
		m.broadcastStatus(s, prevStatus)
		return
	}

	prevStatus := s.Status
	log.Printf("[offload] session %s: %s → offloaded (slot=%d pid=%d claude=%s)", s.ID, prevStatus, sl.Index, sl.PID(), s.ClaudeUUID)

	m.saveOffloadMeta(s)
	m.unbindSession(sl)

	s.Status = StatusOffloaded
	s.Pinned = false
	m.broadcastStatus(s, prevStatus)

	// Recycle the slot: clear it to make it fresh again.
	// SPEC: "the pool never spawns throwaway processes."
	if sl.Process != nil && !sl.Process.Exited() {
		m.clearSlot(sl)
	} else {
		sl.State = SlotCrashed
		m.tryReplaceDeadSlots()
	}
}

// archiveSessionLocked archives a session. Must be called with m.mu held.
// Callers must stop processing sessions first (via stopProcessingSession
// outside the lock).
func (m *Manager) archiveSessionLocked(s *Session) {
	if s.IsLoaded() {
		log.Printf("[archive] session %s: offloading loaded session (status=%s)", s.ID, s.Status)
		m.offloadSession(s)
	}
	if s.Status == StatusQueued {
		m.removeFromQueue(s)
	}

	s.Status = StatusArchived
	s.Pinned = false
	log.Printf("[archive] session %s: archived", s.ID)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "archived", "sessionId": s.ID,
	})
}

// archiveDescendants archives all descendants of a parent session.
func (m *Manager) archiveDescendants(parentID string) {
	var collectAll func(parent *Session)
	var all []string
	collectAll = func(parent *Session) {
		if parent == nil {
			return
		}
		for _, s := range m.sessions {
			if s.IsChildOf(parent) && s.Status != StatusArchived {
				collectAll(s)
				all = append(all, s.ID)
			}
		}
	}
	collectAll(m.sessions[parentID])

	for _, sid := range all {
		s := m.sessions[sid]
		if s != nil && s.Status == StatusProcessing {
			log.Printf("[archive] stopping processing descendant %s of parent %s", sid, parentID)
			m.mu.Unlock()
			m.stopProcessingSession(sid, 30*time.Second)
			m.mu.Lock()
		}
	}

	for _, sid := range all {
		s := m.sessions[sid]
		if s == nil || s.Status == StatusArchived {
			continue
		}
		if s.Status == StatusProcessing {
			m.mu.Unlock()
			m.stopProcessingSession(sid, 30*time.Second)
			m.mu.Lock()
			s = m.sessions[sid]
			if s == nil || s.Status == StatusArchived {
				continue
			}
		}
		m.archiveSessionLocked(s)
	}
}

// --- Watchers ---

// watchProcessDone monitors when a slot's process exits.
func (m *Manager) watchProcessDone(sl *Slot) {
	proc := sl.Process
	go func() {
		<-proc.Done()
		exitCode := -1
		if proc.ExitCode() >= 0 {
			exitCode = proc.ExitCode()
		}
		log.Printf("process exited: pid=%d exit=%d", proc.PID(), exitCode)
		m.mu.Lock()
		defer m.mu.Unlock()

		sl := m.findSlotByProcess(proc)
		if sl == nil {
			return // slot was killed or process replaced
		}

		// If a session is loaded, mark it offloaded
		if s := m.sessions[sl.SessionID]; s != nil && s.IsLive() {
			prevStatus := s.Status
			s.Status = StatusOffloaded
			s.PendingInput = ""
			s.SlotIndex = -1
			log.Printf("[process-exit] slot %d session %s: %s → offloaded (process died)", sl.Index, s.ID, prevStatus)
			m.broadcastStatus(s, prevStatus)
		}

		// Clean up slot — don't kill process (already dead)
		if sl.Pipe != nil {
			sl.Pipe.Close()
			sl.Pipe = nil
		}
		m.stopSlotTerm(sl)
		sl.Process = nil
		sl.SessionID = ""
		sl.State = SlotCrashed
		sl.ClearQueue = nil

		m.tryDequeue()
		m.tryReplaceDeadSlots()
	}()
}

func (m *Manager) watchIdleSignal(sl *Slot) {
	m.mu.Lock()
	proc := sl.Process
	if proc == nil {
		m.mu.Unlock()
		return
	}
	pid := proc.PID()
	m.mu.Unlock()

	signalPath := m.paths.IdleSignal(pid)
	pidMapPath := m.paths.SessionPID(pid)
	var lastSignalTS float64

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			sl := m.findSlotByProcess(proc)
			if sl == nil || !sl.IsLive() {
				m.mu.Unlock()
				return
			}

			// Check for Claude UUID from PID mapping.
			// Only discover UUID when the slot is processing or idle — during
			// clearing/spawning/resuming, /clear generates intermediate UUIDs
			// that don't correspond to the session's actual transcript.
			sessionID := sl.SessionID
			if sessionID != "" && (sl.State == SlotProcessing || sl.State == SlotIdle) {
				if s := m.sessions[sessionID]; s != nil && s.ClaudeUUID == "" {
					m.mu.Unlock()
					if data, err := os.ReadFile(pidMapPath); err == nil {
						claudeUUID := strings.TrimSpace(string(data))
						if claudeUUID != "" {
							m.mu.Lock()
							if s := m.sessions[sessionID]; s != nil && s.ClaudeUUID == "" {
								log.Printf("[idle-watch] slot %d session %s: discovered claude UUID %s", sl.Index, sessionID, claudeUUID)
								s.ClaudeUUID = claudeUUID
							}
							m.mu.Unlock()
						}
					}
					m.mu.Lock()
					// Re-find slot after releasing lock
					sl = m.findSlotByProcess(proc)
					if sl == nil {
						m.mu.Unlock()
						return
					}
				}
			}

			// Read signal file
			data, err := os.ReadFile(signalPath)
			if err != nil {
				m.mu.Unlock()
				continue
			}

			var sig map[string]any
			if err := json.Unmarshal(data, &sig); err != nil {
				m.mu.Unlock()
				continue
			}
			ts, _ := sig["ts"].(float64)
			if ts != 0 && ts == lastSignalTS {
				m.mu.Unlock()
				continue
			}
			lastSignalTS = ts
			log.Printf("[idle-watch] slot %d: new signal (pid=%d): %s", sl.Index, pid, strings.TrimSpace(string(data)))

			// Extract cwd and transcript from signal.
			// Only set UUID when processing/idle — clearing generates intermediate UUIDs.
			if s := m.sessions[sl.SessionID]; s != nil {
				if cwd, ok := sig["cwd"].(string); ok && cwd != "" && s.Cwd != cwd {
					s.Cwd = cwd
					m.broadcastEvent(api.Msg{
						"type": "event", "event": "updated",
						"sessionId": s.ID, "changes": api.Msg{"cwd": cwd},
					})
				}
				if (sl.State == SlotProcessing || sl.State == SlotIdle) && s.ClaudeUUID == "" {
					if transcript, ok := sig["transcript"].(string); ok && transcript != "" {
						base := filepath.Base(transcript)
						if uuid := strings.TrimSuffix(base, ".jsonl"); uuid != base {
							s.ClaudeUUID = uuid
						}
					}
				}
			}

			// watchIdleSignal only handles startup signals (session-start, session-clear).
			// Processing→idle is handled by content monitoring in pollBufferInput.
			trigger, _ := sig["trigger"].(string)
			if trigger != "session-start" && trigger != "session-clear" {
				m.mu.Unlock()
				continue
			}

			if sl.State == SlotSpawning || sl.State == SlotClearing || sl.State == SlotResuming || sl.State == SlotProcessing {
				m.transitionSlotToIdle(sl)
			}
			m.mu.Unlock()
		}
	}
}

// transitionSlotToIdle handles completion of work in a slot.
// Must be called with m.mu held.
func (m *Manager) transitionSlotToIdle(sl *Slot) {
	// Clear workflow: pop next step
	if len(sl.ClearQueue) > 0 {
		next := sl.ClearQueue[0]
		sl.ClearQueue = sl.ClearQueue[1:]
		log.Printf("[idle] slot %d: clear queue step %q (%d remaining)", sl.Index, next, len(sl.ClearQueue))
		m.deliverPrompt(sl, next)
		return
	}

	// Clear workflow complete — slot is fresh
	if sl.State == SlotClearing {
		log.Printf("[idle] slot %d: clearing → fresh (pid=%d)", sl.Index, sl.PID())
		sl.State = SlotFresh

		// If a session was pre-bound during clearing (via findFreshSlot),
		// fall through to deliver its pending resume/prompt.
		if !sl.IsOccupied() {
			m.serveQueueFromSlot(sl)
			return
		}
		// Fall through to handle the pre-bound session's pending work
	}

	s := m.sessions[sl.SessionID]
	if s == nil {
		// No session — mark slot fresh and wake any waiters (e.g., init waitLoop)
		sl.State = SlotFresh
		close(m.statusNotify)
		m.statusNotify = make(chan struct{})
		return
	}

	// Deliver pending resume first
	if s.PendingResume != "" {
		uuid := s.PendingResume
		s.PendingResume = ""
		prevStatus := s.Status
		s.Status = StatusProcessing
		sl.State = SlotResuming

		log.Printf("[idle] slot %d session %s: delivering /resume %s", sl.Index, s.ID, uuid)
		m.broadcastStatus(s, prevStatus)
		if sl.Term != nil {
			sl.Term.resetIdleTracking()
		}
		m.deliverPrompt(sl, "/resume "+uuid)
		return
	}

	// Deliver pending prompt
	if s.PendingPrompt != "" {
		prompt := s.PendingPrompt
		prevStatus := s.Status
		s.PendingPrompt = ""
		s.Status = StatusProcessing
		sl.State = SlotProcessing

		s.LastUsedAt = time.Now()
		log.Printf("[idle] slot %d session %s: delivering pending prompt (%d chars)", sl.Index, s.ID, len(prompt))
		m.broadcastStatus(s, prevStatus)
		if sl.Term != nil {
			sl.Term.resetIdleTracking()
		}
		m.deliverPrompt(sl, prompt)
		return
	}

	// No pending work — mark idle
	prevStatus := s.Status
	s.Status = StatusIdle
	sl.State = SlotIdle
	log.Printf("[idle] slot %d session %s: %s → idle (pid=%d)", sl.Index, s.ID, prevStatus, sl.PID())
	m.broadcastStatus(s, prevStatus)
	m.savePoolState()

	// Consume pending kill tokens (deferred resize-down).
	// This may kill the current slot, so bail out if the session was evicted.
	if m.killTokens > 0 {
		m.tryKillTokens()
		if s.Status != StatusIdle {
			return // this session was evicted by kill tokens
		}
	}

	// Try to serve queue from this slot or evict to free one
	if len(m.queue) > 0 {
		if evicted := m.findEvictableSession(); evicted != nil {
			log.Printf("[idle] slot %d: evicting %s to serve queue (depth=%d)", sl.Index, evicted.ID, len(m.queue))
			m.offloadSession(evicted)
			m.tryDequeue()
		}
	}

	m.maintainFreshSlots()
}

// --- Prompt delivery ---

// deliverPrompt sends a prompt to a slot's terminal with the default 200ms settle delay.
func (m *Manager) deliverPrompt(sl *Slot, prompt string) {
	m.deliverSlotPrompt(sl, prompt, 200*time.Millisecond)
}

// deliverSlotPrompt sends a prompt to a slot's terminal asynchronously.
func (m *Manager) deliverSlotPrompt(sl *Slot, prompt string, settleDelay time.Duration) {
	proc := sl.Process
	if proc == nil {
		log.Printf("[deliver] slot %d: no process, skipping prompt delivery", sl.Index)
		return
	}

	pid := proc.PID()
	done := m.done
	ch := make(chan struct{})
	sl.Delivering = ch
	go func() {
		defer func() {
			close(ch)
			m.mu.Lock()
			if sl := m.findSlotByProcess(proc); sl != nil && sl.Delivering == ch {
				sl.Delivering = nil
			}
			m.mu.Unlock()
		}()

		select {
		case <-done:
			return
		case <-time.After(settleDelay):
		}

		if err := proc.WriteString("\x1b"); err != nil {
			log.Printf("[deliver] pid=%d: write error (esc): %v", pid, err)
			return
		}
		time.Sleep(100 * time.Millisecond)
		if err := proc.WriteString("\x15"); err != nil {
			log.Printf("[deliver] pid=%d: write error (ctrl-u): %v", pid, err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		if err := proc.WriteString(prompt); err != nil {
			log.Printf("[deliver] pid=%d: write error (prompt): %v", pid, err)
			return
		}

		if !strings.HasPrefix(prompt, "/") {
			if !waitForBufferContent(proc, prompt, 200*time.Millisecond) {
				log.Printf("[deliver] pid=%d: prompt not echoed in buffer, sending Enter", pid)
			}
		}

		if err := proc.WriteString("\r"); err != nil {
			log.Printf("[deliver] pid=%d: write error (enter): %v", pid, err)
			return
		}
		log.Printf("[deliver] pid=%d: prompt delivered (%d chars)", pid, len(prompt))
	}()
}

// awaitSlotDelivery waits for any in-flight prompt delivery on a slot.
// Must be called WITHOUT m.mu held.
func (m *Manager) awaitSlotDelivery(slotIdx int) {
	m.mu.Lock()
	var ch chan struct{}
	if slotIdx < len(m.slots) {
		ch = m.slots[slotIdx].Delivering
	}
	m.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func waitForBufferContent(proc *ptyPkg.Process, text string, timeout time.Duration) bool {
	tailSize := len(text) + 500
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			buf := string(proc.Buffer())
			tail := buf
			if len(buf) > tailSize {
				tail = buf[len(buf)-tailSize:]
			}
			if strings.Contains(tail, text) {
				return true
			}
		}
	}
}

// stopProcessingSession sends Ctrl-C and waits for the session to become idle.
// Must be called WITHOUT m.mu held.
func (m *Manager) stopProcessingSession(sid string, timeout time.Duration) {
	m.mu.Lock()
	s := m.sessions[sid]
	if s == nil {
		m.mu.Unlock()
		return
	}
	sl := m.slotForSession(s)
	if sl != nil {
		slotIdx := sl.Index
		m.mu.Unlock()
		m.awaitSlotDelivery(slotIdx)
		m.mu.Lock()
	}

	// Clear any pending work
	if s := m.sessions[sid]; s != nil {
		s.ClearPending()
	}
	// Re-resolve slot (may have changed after releasing lock)
	if s2 := m.sessions[sid]; s2 != nil {
		if sl2 := m.slotForSession(s2); sl2 != nil && sl2.Process != nil {
			sl2.Process.WriteString("\x03")
		}
	}
	m.mu.Unlock()

	m.waitForSessionIdle(sid, timeout)
}

// waitForSessionIdle waits until a session reaches StatusIdle.
// Must be called WITHOUT m.mu held.
func (m *Manager) waitForSessionIdle(sid string, timeout time.Duration) {
	deadline := time.After(timeout)

	m.mu.Lock()
	for {
		s := m.sessions[sid]
		if s == nil || s.Status == StatusIdle || s.Status == StatusOffloaded || !s.IsLoaded() {
			m.mu.Unlock()
			return
		}
		ch := m.statusNotify
		m.mu.Unlock()

		select {
		case <-deadline:
			log.Printf("[wait-idle] session %s: timed out waiting for idle", sid)
			return
		case <-ch:
			m.mu.Lock()
		}
	}
}

// waitForSessionIdleResponse waits until a session reaches StatusIdle,
// returning error responses on timeout or session death. Unlike the void
// waitForSessionIdle, this returns a proper api.Msg for the caller.
// Used by promptless starts where the only acceptable outcome is idle.
func (m *Manager) waitForSessionIdleResponse(id any, sid string, timeout time.Duration) api.Msg {
	deadline := time.After(timeout)

	m.mu.Lock()
	for {
		s := m.sessions[sid]
		if s == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session died before ready")
		}
		if s.Status == StatusIdle {
			m.mu.Unlock()
			return api.Response(id, "started", api.Msg{
				"sessionId": sid,
				"status":    StatusIdle,
			})
		}
		ch := m.statusNotify
		m.mu.Unlock()

		select {
		case <-deadline:
			return api.ErrorResponse(id, "session failed to become idle")
		case <-ch:
			m.mu.Lock()
		}
	}
}

// --- Queue management ---

func (m *Manager) tryDrainQueue() {
	for len(m.queue) > 0 {
		before := len(m.queue)
		m.tryDequeue()
		if len(m.queue) == 0 {
			return
		}
		if evicted := m.findEvictableSession(); evicted != nil {
			log.Printf("[queue] evicting %s to serve queue", evicted.ID)
			m.offloadSession(evicted)
			m.tryDequeue()
		}
		if len(m.queue) >= before {
			return
		}
	}
}

func (m *Manager) tryDequeueWithEviction(s *Session, excludeID string) {
	m.tryDequeue()
	if s.Status == StatusQueued {
		if evicted := m.findEvictableSession(); evicted != nil && evicted.ID != excludeID {
			log.Printf("[evict] evicting idle session %s (priority=%.1f) to free slot for %s", evicted.ID, evicted.Priority, s.ID)
			m.offloadSession(evicted)
		}
		m.tryDequeue()
	}
}

func (m *Manager) removeFromQueue(s *Session) {
	for i, q := range m.queue {
		if q.ID == s.ID {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return
		}
	}
}

func (m *Manager) tryDequeue() {
	if len(m.queue) == 0 && m.killTokens == 0 {
		return
	}

	if m.killTokens > 0 {
		m.tryKillTokens()
	}

	for len(m.queue) > 0 {
		sl := m.findFreshSlot()
		if sl == nil {
			break
		}
		queued := m.queue[0]
		m.queue = m.queue[1:]
		m.claimSlotForQueued(sl, queued)
	}
}

// serveQueueFromSlot gives a fresh slot to the first queued session.
// Must be called with m.mu held.
func (m *Manager) serveQueueFromSlot(sl *Slot) {
	if len(m.queue) == 0 || sl.IsOccupied() {
		return
	}
	queued := m.queue[0]
	m.queue = m.queue[1:]
	m.claimSlotForQueued(sl, queued)
}

// claimSlotForQueued binds a queued session to a slot and delivers pending work.
func (m *Manager) claimSlotForQueued(sl *Slot, queued *Session) {
	log.Printf("[claim] session %s claiming slot %d (pid=%d, state=%s)",
		queued.ID, sl.Index, sl.PID(), sl.State)

	// Set up two-stage delivery for resume: /resume first, then PendingPrompt.
	if queued.ClaudeUUID != "" {
		queued.PendingResume = queued.ClaudeUUID
	}

	m.bindSession(sl, queued)

	if sl.State == SlotFresh || sl.State == SlotIdle {
		// Slot is ready — deliver immediately
		m.clearIdleSignals(sl.PID())
		if sl.Term != nil {
			sl.Term.resetIdleTracking()
		}

		if queued.PendingResume != "" {
			uuid := queued.PendingResume
			queued.PendingResume = ""
			queued.Status = StatusProcessing
			sl.State = SlotResuming
			log.Printf("[claim] slot %d session %s: delivering /resume %s", sl.Index, queued.ID, uuid)
			m.deliverPrompt(sl, "/resume "+uuid)
		} else if queued.PendingPrompt != "" {
			queued.Status = StatusProcessing
			sl.State = SlotProcessing
			prompt := queued.PendingPrompt
			queued.PendingPrompt = ""
			log.Printf("[claim] slot %d session %s: delivering prompt (%d chars)", sl.Index, queued.ID, len(prompt))
			m.deliverPrompt(sl, prompt)
		} else {
			queued.Status = StatusIdle
			sl.State = SlotIdle
			log.Printf("[claim] slot %d session %s: claiming slot (promptless)", sl.Index, queued.ID)
		}
	} else {
		// Slot still starting (clearing/spawning) — keep queued status until
		// transitionSlotToIdle fires and delivers the actual work.
		// Don't set StatusProcessing here: the session isn't doing work yet,
		// and reporting it as "processing" inflates the health count.
	}

	if queued.Status != StatusQueued {
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "status",
			"sessionId": queued.ID, "status": queued.Status, "prevStatus": StatusQueued,
		})
	}
	m.savePoolState()
}

// --- Slot management ---

// findEvictableSession returns the best session to evict, or nil if none qualifies.
func (m *Manager) findEvictableSession() *Session {
	var best *Session
	for _, s := range m.sessions {
		if s.Pinned || !s.IsLoaded() {
			continue
		}
		if s.Status != StatusIdle {
			continue
		}
		if best == nil || evictsBefore(s, best) {
			best = s
		}
	}
	return best
}

// evictsBefore returns true if a should be evicted before b.
func evictsBefore(a, b *Session) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	// Sessions with empty pendingInput evicted before those with pending text
	aHasInput := a.PendingInput != ""
	bHasInput := b.PendingInput != ""
	if aHasInput != bHasInput {
		return !aHasInput
	}
	return a.LastUsedAt.Before(b.LastUsedAt)
}

// tryReplaceDeadSlots respawns crashed slots. Must be called with m.mu held.
func (m *Manager) tryReplaceDeadSlots() {
	if !m.initialized {
		return
	}
	respawned := false
	for _, sl := range m.slots {
		if sl.State == SlotCrashed {
			log.Printf("[replace] respawning slot %d", sl.Index)
			m.spawnSlot(sl)
			respawned = true
		}
	}
	if respawned {
		m.savePoolState()
	}
}

func (m *Manager) tryKillTokens() {
	for m.killTokens > 0 {
		// Find a slot to kill (prefer unoccupied fresh/clearing, then evict idle)
		var target *Slot
		for _, sl := range m.slots {
			if !sl.IsOccupied() && sl.IsLive() {
				target = sl
				break
			}
		}
		if target == nil {
			if evicted := m.findEvictableSession(); evicted != nil {
				log.Printf("[kill-tokens] evicting session %s (tokens remaining=%d)", evicted.ID, m.killTokens)
				m.offloadSession(evicted)
				continue
			}
			log.Printf("[kill-tokens] %d tokens remaining but no killable slots", m.killTokens)
			break
		}

		log.Printf("[kill-tokens] killing slot %d (tokens remaining=%d)", target.Index, m.killTokens-1)
		m.killSlot(target)
		m.killTokens--
	}
}

// killSlot kills a slot's process and removes it from the pool.
func (m *Manager) killSlot(sl *Slot) {
	log.Printf("[kill] slot %d: killing pid %d", sl.Index, sl.PID())

	// Unbind session before cleanup
	if s := m.sessions[sl.SessionID]; s != nil {
		prevStatus := s.Status
		s.Status = StatusOffloaded
		s.SlotIndex = -1
		s.PendingInput = ""
		m.broadcastStatus(s, prevStatus)
	}
	sl.SessionID = ""

	sl.cleanup(m)
	sl.State = SlotCrashed
	sl.ClearQueue = nil

	// Remove slot from pool (shrink)
	for i, s := range m.slots {
		if s == sl {
			m.slots = append(m.slots[:i], m.slots[i+1:]...)
			// Re-index remaining slots
			for j := i; j < len(m.slots); j++ {
				m.slots[j].Index = j
				if sid := m.slots[j].SessionID; sid != "" {
					if sess := m.sessions[sid]; sess != nil {
						sess.SlotIndex = j
					}
				}
			}
			break
		}
	}
}

// maintainFreshSlots proactively offloads idle sessions to keep fresh slots available.
func (m *Manager) maintainFreshSlots() {
	cfg, err := m.config.Load()
	if err != nil {
		return
	}
	target := cfg.KeepFreshVal()
	if target <= 0 {
		return
	}

	for {
		fresh := m.countFreshSlots()
		if fresh >= target {
			return
		}

		// Find an evictable idle session (not pinned)
		var best *Session
		for _, s := range m.sessions {
			if s.Status != StatusIdle || s.Pinned || !s.IsLoaded() {
				continue
			}
			if best == nil || evictsBefore(s, best) {
				best = s
			}
		}
		if best == nil {
			log.Printf("[keep-fresh] want %d fresh slots, have %d, but no evictable idle sessions", target, fresh)
			return
		}

		log.Printf("[keep-fresh] offloading session %s to maintain %d fresh slots (currently %d)", best.ID, target, fresh)
		m.offloadSession(best)
		m.savePoolState()
	}
}

// countFreshSlots returns the number of fresh/clearing (becoming fresh) slots.
func (m *Manager) countFreshSlots() int {
	count := 0
	for _, sl := range m.slots {
		if !sl.IsOccupied() && (sl.State == SlotFresh || sl.State == SlotClearing) {
			count++
		}
	}
	return count
}

// clearIdleSignals removes stale idle signal files for a PID.
func (m *Manager) clearIdleSignals(pid int) {
	os.Remove(m.paths.IdleSignal(pid))
}

// autoAcceptTrust auto-accepts the workspace trust prompt if Claude asks.
func (m *Manager) autoAcceptTrust(sl *Slot) {
	proc := sl.Process
	pid := proc.PID()
	go func() {
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-deadline:
				log.Printf("[trust] pid=%d: trust handler timed out (30s)", pid)
				return
			case <-m.done:
				return
			case <-ticker.C:
				if proc.Exited() {
					return
				}
				raw := string(proc.Buffer())
				buf := strings.ToLower(stripANSI(raw))
				if strings.Contains(buf, "yes,") && strings.Contains(buf, "trust") {
					log.Printf("[trust] pid=%d: detected trust prompt, accepting", pid)
					time.Sleep(1 * time.Second)
					proc.WriteString("\r")
					time.Sleep(500 * time.Millisecond)
					proc.WriteString("\r")
					log.Printf("[trust] pid=%d: sent Enter (x2)", pid)
					return
				}
			}
		}
	}()
}

// cwd returns the current working directory of the slot's process.
func (sl *Slot) cwd() string {
	if sl.Process == nil {
		return ""
	}
	return getCwd(sl.PID())
}

// --- Maintenance ---

func (m *Manager) startMaintenanceLoop() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.done:
				return
			case <-ticker.C:
				m.expirePins()
				m.cleanupArchivedSessions()
			}
		}
	}()
}

func (m *Manager) expirePins() {
	m.mu.Lock()
	defer m.mu.Unlock()

	expired := false
	now := time.Now()
	for _, s := range m.sessions {
		if s.Pinned && !s.PinExpiry.IsZero() && now.After(s.PinExpiry) {
			log.Printf("[pin-expiry] session %s: pin expired", s.ID)
			s.Pinned = false
			expired = true
			m.broadcastEvent(api.Msg{
				"type": "event", "event": "updated",
				"sessionId": s.ID, "changes": api.Msg{"pinned": false},
			})
		}
	}
	if expired {
		m.tryDrainQueue()
	}
}

const archiveRetention = 30 * 24 * time.Hour

func (m *Manager) cleanupArchivedSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-archiveRetention)
	for id, s := range m.sessions {
		if s.Status == StatusArchived && s.CreatedAt.Before(cutoff) {
			log.Printf("[archive-cleanup] removing session %s (created %s)", id, s.CreatedAt.Format(time.RFC3339))
			delete(m.sessions, id)
		}
	}
}
