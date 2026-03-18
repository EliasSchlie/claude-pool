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

// --- Spawn ---

func (m *Manager) spawnSession(s *Session, resume bool) {
	cfg, err := m.config.Load()
	if err != nil {
		log.Printf("[spawn] session %s: config load error (using defaults): %v", s.ID, err)
	}
	flags := cfg.Flags
	cwd := cfg.Dir
	if cwd == "" {
		cwd = m.paths.Root
	}
	log.Printf("[spawn] session %s: resume=%v flags=%q cwd=%s", s.ID, resume, flags, cwd)

	env := map[string]string{
		"CLAUDE_POOL_DIR":        m.paths.Root,
		"CLAUDE_POOL_SESSION_ID": s.ID,
	}

	opts := ptyPkg.SpawnOpts{
		Flags: flags,
		Cwd:   cwd,
		Env:   env,
	}
	if resume && s.ClaudeUUID != "" {
		opts.Resume = s.ClaudeUUID
	}

	proc, err := ptyPkg.Spawn(opts)
	if err != nil {
		log.Printf("[spawn] error for session %s: %v", s.ID, err)
		s.SpawnAttempts++
		if s.SpawnAttempts >= maxSpawnAttempts {
			log.Printf("[spawn] session %s: %d consecutive failures, marking error", s.ID, s.SpawnAttempts)
			s.Status = StatusError
			m.broadcastStatus(s, StatusFresh)
		} else {
			log.Printf("[spawn] session %s: attempt %d/%d failed, will retry on next dequeue", s.ID, s.SpawnAttempts, maxSpawnAttempts)
			s.Status = StatusQueued
			m.queue = append(m.queue, s)
		}
		return
	}
	s.SpawnAttempts = 0 // reset on success

	s.PID = proc.PID()
	s.Flags = flags
	m.procs[s.ID] = proc
	m.pidToSID[proc.PID()] = s.ID
	m.startSessionTerm(s.ID, proc)
	log.Printf("[spawn] session %s: spawned pid=%d", s.ID, proc.PID())

	m.watchProcessDone(s.ID, proc)

	// Auto-accept workspace trust prompt if Claude asks.
	// SessionStart hook handles idle signal — no manual signal needed.
	sid := s.ID
	go func() {
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-deadline:
				log.Printf("[trust] session %s pid=%d: trust handler timed out (30s)", sid, proc.PID())
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
					log.Printf("[trust] session %s pid=%d: detected trust prompt, accepting", sid, proc.PID())
					// Wait for TUI to fully render, then press Enter twice.
					// Some TUI frameworks need multiple Enter presses, or
					// the first one is consumed by the prompt framework.
					time.Sleep(1 * time.Second)
					proc.WriteString("\r")
					time.Sleep(500 * time.Millisecond)
					proc.WriteString("\r")
					log.Printf("[trust] session %s pid=%d: sent Enter (x2)", sid, proc.PID())
					return
				}
			}
		}
	}()

	go m.watchIdleSignal(s.ID, proc.PID())
}

// --- Offload & Archive ---

func (m *Manager) offloadSessionLocked(s *Session) {
	prevStatus := s.Status
	log.Printf("[offload] session %s: %s → offloaded (pid=%d claude=%s)", s.ID, prevStatus, s.PID, s.ClaudeUUID)

	// Close attach pipe before clearing
	if pipe := m.pipes[s.ID]; pipe != nil {
		pipe.Close()
		delete(m.pipes, s.ID)
	}

	proc := m.procs[s.ID]

	// Dissociate process from session (but keep process alive).
	// Keep the terminal emulator — it'll be moved to the pre-warmed session below.
	delete(m.procs, s.ID)
	delete(m.pidToSID, s.PID)

	m.saveOffloadMeta(s)
	s.Status = StatusOffloaded
	s.PID = 0
	s.Pinned = false
	s.PendingInput = ""
	m.broadcastStatus(s, prevStatus)

	// Recycle the process: create a pre-warmed session to hold it,
	// send /clear to reset the Claude context. The process stays alive —
	// SPEC: "the pool never spawns throwaway processes."
	if proc != nil && !proc.Exited() {
		pw := m.newSession("")
		pw.Status = StatusFresh
		pw.PreWarmed = true
		pw.Recycled = true
		pw.PID = proc.PID()
		pw.Cwd = s.Cwd
		// Don't carry over ClaudeUUID — /clear will create a new session.
		// Remove the stale PID→UUID mapping so the idle signal watcher doesn't
		// read the old session's UUID before /clear writes the new one.
		os.Remove(m.paths.SessionPID(proc.PID()))
		m.sessions[pw.ID] = pw
		m.procs[pw.ID] = proc
		m.pidToSID[proc.PID()] = pw.ID
		// Move the terminal emulator to the pre-warmed session (preserves incremental state)
		if st := m.terms[s.ID]; st != nil {
			m.terms[pw.ID] = st
			delete(m.terms, s.ID)
		}
		log.Printf("[offload] session %s: recycled pid %d into pre-warmed session %s", s.ID, proc.PID(), pw.ID)

		// Clear stale idle signals before starting watchers — the old
		// session's signals must not trigger prompt delivery in the new
		// session. /clear will produce a fresh session-clear signal.
		m.clearIdleSignals(proc.PID())
		m.deliverPromptWithSettle(pw, "/clear", 200*time.Millisecond)
		m.startWatchers(pw, proc)
	} else {
		// Process is nil or exited — can't recycle. Clean up orphaned terminal.
		m.stopSessionTerm(s.ID)
	}
}

// archiveSessionLocked archives a session. Must be called with m.mu held.
// The caller must ensure processing sessions have been stopped first
// (via stopProcessingSession outside the lock).
func (m *Manager) archiveSessionLocked(s *Session) {
	if s.IsLive() {
		log.Printf("[archive] session %s: offloading live session (status=%s)", s.ID, s.Status)
		m.offloadSessionLocked(s)
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
// Processing descendants are stopped first (Ctrl-C + wait for idle)
// by releasing and re-acquiring the lock per descendant.
// Must be called with m.mu held. May temporarily release m.mu.
func (m *Manager) archiveDescendants(parentID string) {
	// Collect all descendants first (recursive).
	// Match by both session ID and Claude UUID since auto-detected parents
	// use Claude UUIDs.
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

	// Stop any processing descendants first (requires releasing the lock)
	for _, sid := range all {
		s := m.sessions[sid]
		if s != nil && s.Status == StatusProcessing {
			log.Printf("[archive] stopping processing descendant %s of parent %s", sid, parentID)
			m.mu.Unlock()
			m.stopProcessingSession(sid, 30*time.Second)
			m.mu.Lock()
		}
	}

	// Now archive them all under the lock. Re-check for processing —
	// a descendant could have started processing while we were stopping
	// a sibling (lock was released during stopProcessingSession).
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

// watchProcessDone monitors when a process exits and updates session status.
func (m *Manager) watchProcessDone(sessionID string, proc *ptyPkg.Process) {
	go func() {
		<-proc.Done()
		exitCode := -1
		if proc.ExitCode() >= 0 {
			exitCode = proc.ExitCode()
		}
		log.Printf("process exited: session=%s pid=%d exit=%d", sessionID, proc.PID(), exitCode)
		m.mu.Lock()
		defer m.mu.Unlock()

		// If the session has been respawned with a new process, this watcher
		// is stale — don't modify session state or clean up the new process.
		// This happens when: offload kills process → followup respawns session
		// → old watchProcessDone fires after new process is already running.
		currentProc := m.procs[sessionID]
		stale := currentProc != nil && currentProc != proc

		// Track whether the process was still live (unexpected death) before
		// we transition status. Intentional exits (offload, archive, destroy)
		// set the session status before killing — IsLive() is already false.
		unexpectedDeath := false
		if !stale {
			if sess := m.sessions[sessionID]; sess != nil && sess.IsLive() {
				unexpectedDeath = true
				prevStatus := sess.Status
				sess.Status = StatusOffloaded
				sess.PID = 0
				sess.PendingInput = ""
				log.Printf("[process-exit] session %s: %s → offloaded (process died)", sessionID, prevStatus)
				m.broadcastStatus(sess, prevStatus)
			}
			if pipe := m.pipes[sessionID]; pipe != nil {
				pipe.Close()
				delete(m.pipes, sessionID)
			}
			m.stopSessionTerm(sessionID)
			delete(m.procs, sessionID)
		}

		// Only clean up pidToSID if this watcher still owns the PID.
		// After transferProcess, the PID maps to the new session —
		// the old watcher must not remove it.
		if m.pidToSID[proc.PID()] == sessionID {
			delete(m.pidToSID, proc.PID())
		}
		m.tryDequeue()
		if unexpectedDeath {
			m.tryReplaceDeadSessions()
		}
	}()
}

func (m *Manager) watchIdleSignal(sessionID string, pid int) {
	signalPath := m.paths.IdleSignal(pid)
	pidMapPath := m.paths.SessionPID(pid)
	var lastSignalTS float64 // timestamp of last processed signal (avoid re-processing)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			s := m.sessions[sessionID]
			if s == nil || !s.IsLive() || s.PID != pid {
				m.mu.Unlock()
				return
			}
			needUUID := s.ClaudeUUID == ""
			m.mu.Unlock()

			// Check for Claude UUID from PID mapping (skip if already known)
			if needUUID {
				if data, err := os.ReadFile(pidMapPath); err == nil {
					claudeUUID := strings.TrimSpace(string(data))
					if claudeUUID != "" {
						m.mu.Lock()
						if s := m.sessions[sessionID]; s != nil && s.ClaudeUUID == "" {
							log.Printf("[idle-watch] session %s: discovered claude UUID %s from pid map", sessionID, claudeUUID)
							s.ClaudeUUID = claudeUUID
						}
						m.mu.Unlock()
					}
				}
			}

			// Read signal file. Only SessionStart hooks write signals now;
			// processing↔idle detection is handled by content monitoring.
			m.mu.Lock()
			data, err := os.ReadFile(signalPath)
			if err != nil {
				// No signal file — cleared by clearIdleSignals during recycling
				// or externally. Idle→processing detection is handled by the
				// spinner check in pollBufferInput, not here.
				m.mu.Unlock()
				continue
			}

			// Check session still exists, is live, AND still owns this PID.
			// If the session was transferred to a different PID (offloaded →
			// restored on another slot), this watcher is stale.
			s = m.sessions[sessionID]
			if s == nil || !s.IsLive() || s.PID != pid {
				sPID := 0
				if s != nil {
					sPID = s.PID
				}
				log.Printf("[idle-watch] session %s: stale watcher (watching pid=%d, session pid=%d), leaving signal", sessionID, pid, sPID)
				m.mu.Unlock()
				return
			}

			// Check if this is a new signal (by timestamp) to avoid
			// re-processing the same signal on every tick.
			var sig map[string]any
			if err := json.Unmarshal(data, &sig); err != nil {
				m.mu.Unlock()
				continue
			}
			ts, _ := sig["ts"].(float64)
			if ts != 0 && ts == lastSignalTS {
				// Already processed this signal — skip
				m.mu.Unlock()
				continue
			}
			lastSignalTS = ts
			log.Printf("[idle-watch] session %s: new signal file (pid=%d): %s", sessionID, pid, strings.TrimSpace(string(data)))

			// Extract cwd and transcript from signal
			if cwd, ok := sig["cwd"].(string); ok && cwd != "" && s.Cwd != cwd {
				s.Cwd = cwd
				m.broadcastEvent(api.Msg{
					"type": "event", "event": "updated",
					"sessionId": s.ID, "changes": api.Msg{"cwd": cwd},
				})
			}
			if transcript, ok := sig["transcript"].(string); ok && transcript != "" && s.ClaudeUUID == "" {
				base := filepath.Base(transcript)
				if uuid := strings.TrimSuffix(base, ".jsonl"); uuid != base {
					s.ClaudeUUID = uuid
				}
			}

			// watchIdleSignal only handles startup signals (session-start,
			// session-clear). Processing→idle is handled by content monitoring
			// in pollBufferInput, which calls transitionToIdle directly.
			trigger, _ := sig["trigger"].(string)
			if trigger != "session-start" && trigger != "session-clear" {
				m.mu.Unlock()
				continue
			}

			if s.Status == StatusFresh || s.Status == StatusProcessing {
				m.transitionToIdle(s)
			}
			m.mu.Unlock()
		}
	}
}

// transitionToIdle handles the processing → idle transition for a session.
// Delivers pending prompts, serves the queue, and maintains fresh slots.
// Must be called with m.mu held. May release and re-acquire the lock
// for prompt delivery.
func (m *Manager) transitionToIdle(s *Session) {
	// Deliver pending resume first
	if s.PendingResume != "" {
		uuid := s.PendingResume
		s.PendingResume = ""
		prevStatus := s.Status
		s.Status = StatusProcessing

		log.Printf("[idle] session %s: delivering /resume %s", s.ID, uuid)
		m.broadcastStatus(s, prevStatus)
		m.deliverPromptWithSettle(s, "/resume "+uuid, 200*time.Millisecond)
		return
	}

	// Deliver pending prompt
	if s.PendingPrompt != "" {
		prompt := s.PendingPrompt
		prevStatus := s.Status
		s.PendingPrompt = ""
		s.PendingForce = false
		s.Status = StatusProcessing

		s.LastUsedAt = time.Now()
		log.Printf("[idle] session %s: delivering pending prompt (%d chars)", s.ID, len(prompt))
		m.broadcastStatus(s, prevStatus)
		m.deliverPromptWithSettle(s, prompt, 200*time.Millisecond)
		return
	}

	// No pending work — mark idle
	prevStatus := s.Status
	s.Status = StatusIdle
	s.Recycled = false
	log.Printf("[idle] session %s: %s → idle (pid=%d)", s.ID, prevStatus, s.PID)
	m.broadcastStatus(s, prevStatus)
	m.savePoolState()

	m.serveQueueFromSlot(s)

	if len(m.queue) > 0 {
		if evicted := m.findEvictableSession(); evicted != nil {
			log.Printf("[idle] session %s: evicting %s to serve queue (depth=%d)", s.ID, evicted.ID, len(m.queue))
			m.offloadSessionLocked(evicted)
			m.tryDequeue()
		}
	}

	m.maintainFreshSlots()
}

// --- Prompt delivery ---

// deliverPrompt sends a prompt to a session's terminal asynchronously.
// Escape → Ctrl-U → text → brief echo check → Enter.
// Must be called with m.mu held. Callers that need to wait for delivery
// can select on m.delivering[s.ID] (closed when the goroutine finishes).
func (m *Manager) deliverPrompt(s *Session, prompt string) {
	m.deliverPromptWithSettle(s, prompt, 200*time.Millisecond)
}

// deliverPromptWithSettle is like deliverPrompt but with a custom settle delay.
// Use a longer delay after Ctrl-C interrupts, since Claude needs time to
// process the cancellation and return to its prompt.
func (m *Manager) deliverPromptWithSettle(s *Session, prompt string, settleDelay time.Duration) {
	proc := m.procs[s.ID]
	if proc == nil {
		log.Printf("[deliver] session %s: no process, skipping prompt delivery", s.ID)
		return
	}

	sid := s.ID
	done := m.done
	ch := make(chan struct{})
	m.delivering[sid] = ch
	go func() {
		defer func() {
			close(ch)
			m.mu.Lock()
			if m.delivering[sid] == ch {
				delete(m.delivering, sid)
			}
			m.mu.Unlock()
		}()

		// Delay to let terminal settle after state transition
		select {
		case <-done:
			return
		case <-time.After(settleDelay):
		}

		if err := proc.WriteString("\x1b"); err != nil { // Escape
			log.Printf("[deliver] session %s: write error (esc): %v", sid, err)
			return
		}
		time.Sleep(100 * time.Millisecond)
		if err := proc.WriteString("\x15"); err != nil { // Ctrl-U (clear line)
			log.Printf("[deliver] session %s: write error (ctrl-u): %v", sid, err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		if err := proc.WriteString(prompt); err != nil {
			log.Printf("[deliver] session %s: write error (prompt): %v", sid, err)
			return
		}

		// Brief check that prompt text appeared in buffer (Claude's TUI uses raw
		// mode so exact-match echo rarely works — keep timeout short).
		// Skip for slash commands (/clear, /resume) — they never echo in TUI mode.
		if !strings.HasPrefix(prompt, "/") {
			if !waitForBufferContent(proc, prompt, 200*time.Millisecond) {
				log.Printf("[deliver] session %s: prompt not echoed in buffer (expected with TUI), sending Enter", sid)
			}
		}

		if err := proc.WriteString("\r"); err != nil { // Enter
			log.Printf("[deliver] session %s: write error (enter): %v", sid, err)
			return
		}
		log.Printf("[deliver] session %s: prompt delivered (%d chars)", sid, len(prompt))
	}()
}

// awaitDelivery waits for any in-flight prompt delivery on a session.
// Must be called WITHOUT m.mu held (it blocks on the goroutine).
func (m *Manager) awaitDelivery(sessionID string) {
	m.mu.Lock()
	ch := m.delivering[sessionID]
	m.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// waitForBufferContent polls the process buffer until it contains the given text,
// checking only the tail to avoid false matches in scrollback.
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

func (m *Manager) deliverPromptAsync(sessionID, prompt string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	if s == nil {
		m.mu.Unlock()
		log.Printf("[deliver] session %s gone before prompt delivery", sessionID)
		return
	}
	m.deliverPrompt(s, prompt)
	m.mu.Unlock()
}

// stopProcessingSession sends Ctrl-C and waits for the session to become idle.
// Used by handleStop, handleArchive, and handleFollowup (force) to stop a
// processing session before further action. Must be called WITHOUT m.mu held.
//
// PTY silence detection (3s) will write the idle signal once the spinner stops.
func (m *Manager) stopProcessingSession(sid string, timeout time.Duration) {
	m.awaitDelivery(sid)

	m.mu.Lock()
	// Clear any pending work — stop means cancel everything.
	// Without this, watchIdleSignal would deliver PendingPrompt after
	// Ctrl-C completes, making the session process again.
	if s := m.sessions[sid]; s != nil {
		s.ClearPending()
	}
	if proc := m.procs[sid]; proc != nil {
		proc.WriteString("\x03")
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
		if s == nil || s.Status == StatusIdle || s.Status == StatusOffloaded || !s.IsLive() {
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

// waitForSessionReady waits until a session transitions out of StatusFresh.
// Must be called WITHOUT m.mu held. Returns an API response.
func (m *Manager) waitForSessionReady(id any, sid string, timeout time.Duration) api.Msg {
	deadline := time.After(timeout)

	m.mu.Lock()
	for {
		s := m.sessions[sid]
		if s == nil {
			m.mu.Unlock()
			log.Printf("[followup] session %s: died before ready", sid)
			return api.ErrorResponse(id, "session died before ready")
		}
		status := s.Status
		if status == StatusProcessing || status == StatusIdle {
			m.mu.Unlock()
			log.Printf("[followup] session %s: ready (status=%s)", sid, status)
			return api.Response(id, "started", api.Msg{
				"sessionId": sid,
				"status":    status,
			})
		}
		ch := m.statusNotify
		m.mu.Unlock()

		select {
		case <-deadline:
			log.Printf("[followup] session %s: timed out waiting for ready", sid)
			return api.ErrorResponse(id, "session failed to become ready")
		case <-ch:
			m.mu.Lock()
		}
	}
}

// --- Queue management ---

// tryDrainQueue attempts to serve queued requests by trying free slots first,
// then evicting an idle session if needed. Used after unpin/pin-expiry when
// previously-pinned sessions become evictable. Must be called with m.mu held.
func (m *Manager) tryDrainQueue() {
	for len(m.queue) > 0 {
		before := len(m.queue)
		m.tryDequeue()
		if len(m.queue) == 0 {
			return
		}
		if evicted := m.findEvictableSession(); evicted != nil {
			log.Printf("[queue] evicting %s to serve queue", evicted.ID)
			m.offloadSessionLocked(evicted)
			m.tryDequeue()
		}
		// No progress — all remaining sessions are pinned/processing
		if len(m.queue) >= before {
			return
		}
	}
}

// tryDequeueWithEviction attempts to dequeue a session by first trying free
// slots, then evicting an idle session if needed. excludeID prevents evicting
// the session itself (e.g., during followup/pin). Must be called with m.mu held.
func (m *Manager) tryDequeueWithEviction(s *Session, excludeID string) {
	m.tryDequeue()
	if s.Status == StatusQueued {
		if evicted := m.findEvictableSession(); evicted != nil && evicted.ID != excludeID {
			log.Printf("[evict] evicting idle session %s (priority=%.1f) to free slot for %s", evicted.ID, evicted.Priority, s.ID)
			m.offloadSessionLocked(evicted)
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

	// First pass: try to evict sessions for any outstanding kill tokens.
	// The resize handler already set poolSize to the target — we must NOT
	// shrink poolSize here. Instead, evict now-available sessions (e.g. a
	// session that was processing during resize but has since become idle).
	if m.killTokens > 0 {
		m.tryKillTokens()
	}

	// Second pass: assign queued sessions to available fresh/idle pre-warmed slots.
	// No process spawning — SPEC: "the pool never spawns throwaway processes."
	for len(m.queue) > 0 {
		slot := m.findFreshSlot()
		if slot == nil {
			break
		}
		queued := m.queue[0]
		m.queue = m.queue[1:]
		if !m.claimSlotForQueued(slot, queued) {
			break
		}
	}
}

// serveQueueFromSlot takes a pre-warmed idle session and gives it to the first
// queued session. Must be called with m.mu held.
// Only pre-warmed sessions can be consumed this way — user sessions are preserved.
func (m *Manager) serveQueueFromSlot(idle *Session) {
	if len(m.queue) == 0 || !idle.PreWarmed {
		return
	}
	queued := m.queue[0]
	m.queue = m.queue[1:]
	m.claimSlotForQueued(idle, queued)
}

// claimSlotForQueued transfers a pre-warmed slot's process to a queued session
// and delivers any pending work (/resume, prompt, or nothing). Returns false
// if the slot has no process. Must be called with m.mu held.
func (m *Manager) claimSlotForQueued(slot, queued *Session) bool {
	log.Printf("[claim] session %s claiming slot from %s (pid=%d, status=%s)",
		queued.ID, slot.ID, slot.PID, slot.Status)

	// Set up two-stage delivery for resume: /resume first, then PendingPrompt.
	if queued.ClaudeUUID != "" {
		queued.PendingResume = queued.ClaudeUUID
	}

	proc := m.transferProcess(slot, queued)
	if proc == nil {
		return false
	}
	delete(m.sessions, slot.ID)
	// Only clear stale signals when the slot is idle (signal already consumed).
	// For Fresh slots (/clear in progress), the signal hasn't fired yet —
	// clearing it would race with the /clear completion signal.
	if slot.Status == StatusIdle {
		m.clearIdleSignals(queued.PID)
	}

	if slot.Status == StatusIdle {
		// Slot is ready — deliver immediately (idle signal already consumed).
		if queued.PendingResume != "" {
			uuid := queued.PendingResume
			queued.PendingResume = ""
			queued.Status = StatusProcessing
			log.Printf("[claim] session %s: delivering /resume %s", queued.ID, uuid)
			m.deliverPrompt(queued, "/resume "+uuid)
		} else if queued.PendingPrompt != "" {
			queued.Status = StatusProcessing
			prompt := queued.PendingPrompt
			queued.PendingPrompt = ""
			queued.PendingForce = false
			log.Printf("[claim] session %s: delivering prompt (%d chars)", queued.ID, len(prompt))
			m.deliverPrompt(queued, prompt)
		} else {
			queued.Status = StatusIdle
			log.Printf("[claim] session %s: claiming slot (promptless)", queued.ID)
		}
	} else {
		// Slot still starting (/clear in progress) — wait for idle signal
		// to deliver PendingResume then PendingPrompt.
		queued.Status = StatusFresh
	}

	m.startWatchers(queued, proc)
	if queued.Status != StatusFresh {
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "status",
			"sessionId": queued.ID, "status": queued.Status, "prevStatus": StatusQueued,
		})
	}
	m.savePoolState()
	return true
}

// --- Process transfer helpers ---

// transferProcess moves a process from one session to another, updating all
// tracking maps. Returns the process (nil if source had none). Must be called
// with m.mu held.
func (m *Manager) transferProcess(from, to *Session) *ptyPkg.Process {
	proc := m.procs[from.ID]
	if proc == nil {
		return nil
	}
	// Move the persistent terminal emulator (don't recreate — preserves
	// incremental state that would be lost by re-rendering the full buffer).
	if st := m.terms[from.ID]; st != nil {
		m.terms[to.ID] = st
		delete(m.terms, from.ID)
	}
	delete(m.procs, from.ID)
	delete(m.pidToSID, from.PID)
	m.procs[to.ID] = proc
	m.pidToSID[proc.PID()] = to.ID
	to.PID = from.PID
	// Only copy ClaudeUUID if the target doesn't already have one
	// (resume sessions keep their own UUID for /resume delivery)
	if to.ClaudeUUID == "" {
		to.ClaudeUUID = from.ClaudeUUID
	}
	to.Cwd = from.Cwd
	return proc
}

// startWatchers launches the idle signal and process-done watchers for a session.
// Must be called with m.mu held (watchers acquire it internally as needed).
func (m *Manager) startWatchers(s *Session, proc *ptyPkg.Process) {
	go m.watchIdleSignal(s.ID, s.PID)
	m.watchProcessDone(s.ID, proc)
}

// clearIdleSignals removes stale idle signal files for a PID. Called before
// starting watchers for a transferred process to prevent immediate false triggers.
func (m *Manager) clearIdleSignals(pid int) {
	os.Remove(m.paths.IdleSignal(pid))
}

// --- Slot management ---

func (m *Manager) findFreshSlot() *Session {
	// Only claim pre-warmed sessions (pool-owned, never used by a client).
	// Prefer idle (ready) over fresh (still starting).
	var freshCandidate *Session
	for _, s := range m.sessions {
		if !s.PreWarmed || s.Pinned {
			continue
		}
		if s.Status == StatusIdle {
			return s // best case — immediately ready
		}
		if s.Status == StatusFresh && freshCandidate == nil {
			freshCandidate = s
		}
	}
	return freshCandidate
}

// findEvictableSession returns the best session to evict, or nil if none
// qualifies. Idle sessions and fresh pre-warmed sessions are candidates.
// Fresh user sessions (being /cleared+/resumed) are excluded — interrupting
// them would corrupt the resume sequence.
func (m *Manager) findEvictableSession() *Session {
	var best *Session
	for _, s := range m.sessions {
		if s.Pinned {
			continue
		}
		if s.Status == StatusIdle {
			// Idle sessions are always evictable
		} else if s.Status == StatusFresh && s.PreWarmed {
			// Fresh pre-warmed sessions are evictable (pool-owned, just starting)
		} else {
			continue
		}
		if best == nil || evictsBefore(s, best) {
			best = s
		}
	}
	return best
}

// evictsBefore returns true if a should be evicted before b.
// Order: fresh pre-warmed first → lower priority → pre-warmed → empty pendingInput → oldest LRU.
func evictsBefore(a, b *Session) bool {
	// Fresh pre-warmed sessions evicted first — no user work, no user intent
	aFreshPW := a.Status == StatusFresh && a.PreWarmed
	bFreshPW := b.Status == StatusFresh && b.PreWarmed
	if aFreshPW != bFreshPW {
		return aFreshPW
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	// Pre-warmed sessions evicted before user sessions
	if a.PreWarmed != b.PreWarmed {
		return a.PreWarmed
	}
	// Sessions with empty pendingInput evicted before those with pending text
	aHasInput := a.PendingInput != ""
	bHasInput := b.PendingInput != ""
	if aHasInput != bHasInput {
		return !aHasInput // evict the one WITHOUT input first
	}
	return a.LastUsedAt.Before(b.LastUsedAt)
}

// tryReplaceDeadSessions spawns new sessions to maintain pool size when
// sessions die unexpectedly. Only replaces parentless sessions (pool-owned).
func (m *Manager) tryReplaceDeadSessions() {
	if !m.initialized {
		return
	}
	liveCount := 0
	for _, s := range m.sessions {
		if s.IsLive() || s.Status == StatusQueued {
			liveCount++
		}
	}
	if deficit := m.poolSize - liveCount; deficit > 0 {
		log.Printf("[replace] spawning %d replacement sessions (live=%d pool=%d)", deficit, liveCount, m.poolSize)
	}
	for liveCount < m.poolSize {
		s := m.newSession("")
		s.Status = StatusFresh
		s.PreWarmed = true
		m.sessions[s.ID] = s
		m.spawnSession(s, false)
		liveCount++
	}
	m.savePoolState()
}

func (m *Manager) tryKillTokens() {
	for m.killTokens > 0 {
		evicted := m.findEvictableSession()
		if evicted == nil {
			log.Printf("[kill-tokens] %d tokens remaining but no evictable sessions", m.killTokens)
			break
		}
		log.Printf("[kill-tokens] evicting session %s (tokens remaining=%d)", evicted.ID, m.killTokens-1)

		if evicted.PreWarmed {
			// Pre-warmed slot: kill the process and remove the session entirely
			// to actually reduce pool capacity. offloadSessionLocked would
			// recycle the process into a new pre-warmed session, defeating
			// the purpose of shrinking.
			m.killSessionLocked(evicted)
		} else {
			// User session: offload it (process gets recycled into a pre-warmed
			// slot). The next iteration will kill that pre-warmed slot.
			m.offloadSessionLocked(evicted)
		}
		m.killTokens--
	}
}

// killSessionLocked kills a session's process and removes it entirely.
// Used only for pool shrink (tryKillTokens) and daemon shutdown —
// normal offload uses /clear to recycle the process.
func (m *Manager) killSessionLocked(s *Session) {
	prevStatus := s.Status
	log.Printf("[kill] session %s: killing pid %d (pool shrink)", s.ID, s.PID)

	if pipe := m.pipes[s.ID]; pipe != nil {
		pipe.Close()
		delete(m.pipes, s.ID)
	}

	m.stopSessionTerm(s.ID)
	if proc := m.procs[s.ID]; proc != nil {
		proc.Kill()
		proc.Close()
		delete(m.procs, s.ID)
		delete(m.pidToSID, s.PID)
	}

	delete(m.sessions, s.ID)
	m.broadcastStatus(s, prevStatus)
}

// maintainFreshSlots proactively offloads idle sessions to keep at least
// keepFresh fresh slots available. Must be called with m.mu held.
// SPEC: Fresh Slot Maintenance.
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

		// Find an evictable idle user session (not pinned, not pre-warmed).
		// Pre-warmed sessions are already fresh slots — offloading them
		// would just cycle them without progress.
		var best *Session
		for _, s := range m.sessions {
			if s.Status != StatusIdle || s.Pinned || s.PreWarmed {
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
		m.offloadSessionLocked(best)
		// No spawn needed — offloadSessionLocked creates a pre-warmed session
		// from the recycled process (SPEC: process reuse via /clear).
		m.savePoolState()
	}
}

// countFreshSlots returns the number of pre-warmed sessions (fresh or idle+prewarmed).
func (m *Manager) countFreshSlots() int {
	count := 0
	for _, s := range m.sessions {
		if s.PreWarmed && (s.Status == StatusFresh || s.Status == StatusIdle) {
			count++
		}
	}
	return count
}

// startMaintenanceLoop launches a goroutine that periodically expires pins
// and cleans up old archived sessions. SPEC: "Pin is time-limited" and
// "Archived: Auto-cleaned after 30 days."
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

// expirePins unpins sessions whose PinExpiry has passed.
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

// cleanupArchivedSessions removes archived sessions older than 30 days.
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
