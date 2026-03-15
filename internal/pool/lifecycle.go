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
	if cfg, err := m.config.Load(); err == nil && cfg.Dir != "" {
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
	cfg, _ := m.config.Load()
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
		log.Printf("spawn error for session %s: %v", s.ID, err)
		s.Status = StatusError
		return
	}

	s.PID = proc.PID()
	s.Flags = flags
	m.procs[s.ID] = proc
	m.pidToSID[proc.PID()] = s.ID
	log.Printf("[spawn] session %s: spawned pid=%d", s.ID, proc.PID())

	m.watchProcessDone(s.ID, proc)

	// Auto-accept workspace trust prompt if Claude asks.
	// SessionStart hook handles idle signal — no manual signal needed.
	go func() {
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-deadline:
				return
			case <-m.done:
				return
			case <-ticker.C:
				if proc.Exited() {
					return
				}
				buf := strings.ToLower(string(proc.Buffer()))
				if strings.Contains(buf, "trust?") {
					time.Sleep(200 * time.Millisecond)
					proc.WriteString("\r")
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

	// Close attach pipe before killing process
	if pipe := m.pipes[s.ID]; pipe != nil {
		pipe.Close()
		delete(m.pipes, s.ID)
	}

	if proc := m.procs[s.ID]; proc != nil {
		log.Printf("[offload] session %s: killing pid %d", s.ID, proc.PID())
		proc.Kill()
		proc.Close()
		delete(m.procs, s.ID)
		delete(m.pidToSID, s.PID)
	}

	m.saveOffloadMeta(s)
	s.Status = StatusOffloaded
	s.PID = 0
	s.Pinned = false
	s.PendingInput = ""
	delete(m.attachTyping, s.ID)
	m.broadcastStatus(s, prevStatus)
}

func (m *Manager) archiveSessionLocked(s *Session) {
	if s.IsLive() {
		if s.Status == StatusProcessing {
			log.Printf("[archive] session %s: interrupting processing session (pid=%d)", s.ID, s.PID)
			if proc := m.procs[s.ID]; proc != nil {
				proc.WriteString("\x03")
			}
			// No sleep — offloadSessionLocked kills the process immediately.
			// The Ctrl-C is a courtesy signal before the kill.
		}
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

func (m *Manager) archiveDescendants(parentID string) {
	for _, s := range m.sessions {
		if s.ParentID == parentID && s.Status != StatusArchived {
			log.Printf("[archive] archiving descendant %s of parent %s", s.ID, parentID)
			m.archiveDescendants(s.ID)
			m.archiveSessionLocked(s)
		}
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

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			s := m.sessions[sessionID]
			if s == nil {
				m.mu.Unlock()
				return
			}
			if !s.IsLive() {
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()

			// Check for Claude UUID from PID mapping (skip if already known)
			m.mu.Lock()
			needUUID := m.sessions[sessionID] != nil && m.sessions[sessionID].ClaudeUUID == ""
			m.mu.Unlock()
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

			// Read and process idle signal atomically under the mutex.
			// This prevents races with handleAttachInput's clearIdleSignals —
			// without this, the idle-watch could read a stale signal file,
			// then handleAttachInput sets Processing + clears the file, then
			// the idle-watch acquires the mutex and falsely transitions to Idle.
			m.mu.Lock()
			data, err := os.ReadFile(signalPath)
			if err != nil {
				m.mu.Unlock()
				continue
			}
			os.Remove(signalPath)
			log.Printf("[idle-watch] session %s: read signal file (pid=%d): %s", sessionID, pid, strings.TrimSpace(string(data)))

			s = m.sessions[sessionID]
			if s == nil {
				log.Printf("[idle-watch] session %s: gone, stopping watcher", sessionID)
				m.mu.Unlock()
				return
			}

			// Parse signal JSON for cwd and transcript
			var sig map[string]any
			if err := json.Unmarshal(data, &sig); err == nil {
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
			}

			// Startup signals (session-start, session-clear) are only valid for
			// Fresh → Idle transitions. If the session is Processing, a startup
			// signal is stale (arrived after handleAttachInput set Processing)
			// and must not cause a false Processing → Idle transition.
			trigger, _ := sig["trigger"].(string)
			if s.Status == StatusProcessing && (trigger == "session-start" || trigger == "session-clear") {
				log.Printf("[idle-watch] session %s: ignoring stale %s signal (status=%s)", sessionID, trigger, s.Status)
				m.mu.Unlock()
				continue
			}

			if s.Status == StatusFresh || s.Status == StatusProcessing {
				// Deliver any pending prompt immediately — avoids a transient
				// idle state that confuses wait-without-sessionId.
				if s.PendingPrompt != "" {
					prompt := s.PendingPrompt
					prevStatus := s.Status
					s.PendingPrompt = ""
					s.PendingForce = false
					s.Status = StatusProcessing
					s.LastUsedAt = time.Now()
					log.Printf("[idle-watch] session %s: idle signal received, delivering pending prompt (%d chars)", sessionID, len(prompt))
					m.broadcastStatus(s, prevStatus)
					// Register delivery under the lock so awaitDelivery
					// can't miss it between unlock and goroutine start.
					m.deliverPrompt(s, prompt)
					m.mu.Unlock()
					continue
				}

				prevStatus := s.Status
				s.Status = StatusIdle
				log.Printf("[idle-watch] session %s: %s → idle (pid=%d)", sessionID, prevStatus, s.PID)
				m.broadcastStatus(s, prevStatus)
				m.savePoolState()

				// Check if a queued session can claim this slot
				m.serveQueueFromSlot(s)
			} else {
				log.Printf("[idle-watch] session %s: consumed stale signal (status=%s)", sessionID, s.Status)
			}
			m.mu.Unlock()
		}
	}
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
		if !waitForBufferContent(proc, prompt, 200*time.Millisecond) {
			log.Printf("[deliver] session %s: prompt not echoed in buffer (expected with TUI), sending Enter", sid)
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

// writeIdleSignal writes a synthetic idle signal for cases where no hook fires
// (e.g., after Ctrl-C interrupts processing).
func (m *Manager) writeIdleSignal(pid int, trigger string) {
	signalPath := m.paths.IdleSignal(pid)
	signal := map[string]any{
		"cwd":        m.paths.Root,
		"session_id": "",
		"transcript": "",
		"ts":         time.Now().Unix(),
		"trigger":    trigger,
	}
	data, _ := json.Marshal(signal)
	log.Printf("[idle-signal] writing synthetic idle signal for pid %d (trigger=%s)", pid, trigger)
	if err := os.WriteFile(signalPath, append(data, '\n'), 0600); err != nil {
		log.Printf("[idle-signal] error writing signal for pid %d: %v", pid, err)
	}
}

// --- Queue management ---

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

	// Second pass: fill available slots from queue
	active := 0
	for _, s := range m.sessions {
		if s.IsLive() {
			active++
		}
	}

	for active < m.poolSize && len(m.queue) > 0 {
		s := m.queue[0]
		m.queue = m.queue[1:]

		log.Printf("[dequeue] dequeuing session %s (active=%d/%d, remaining queue=%d)", s.ID, active, m.poolSize, len(m.queue))
		s.Status = StatusFresh
		m.spawnSession(s, s.ClaudeUUID != "")
		// PendingPrompt is delivered by watchIdleSignal when the session
		// becomes ready (Fresh → idle signal → checks PendingPrompt).
		active++
	}
}

// serveQueueFromSlot takes a pre-warmed idle session and gives it to the first
// queued session that has a pending prompt. Must be called with m.mu held.
// Only pre-warmed sessions can be consumed this way — user sessions are preserved.
func (m *Manager) serveQueueFromSlot(idle *Session) {
	if len(m.queue) == 0 || !idle.PreWarmed {
		return
	}

	// Find first queued session with a pending prompt
	for i, queued := range m.queue {
		if queued.PendingPrompt == "" {
			continue
		}
		proc := m.transferProcess(idle, queued)
		if proc == nil {
			return
		}
		delete(m.sessions, idle.ID)
		queued.Status = StatusProcessing

		prompt := queued.PendingPrompt
		queued.PendingPrompt = ""
		queued.PendingForce = false

		// Remove from queue
		m.queue = append(m.queue[:i], m.queue[i+1:]...)

		log.Printf("[serve-queue] session %s claimed slot from %s (pid=%d)", queued.ID, idle.ID, queued.PID)
		m.clearIdleSignals(queued.PID)
		m.deliverPrompt(queued, prompt)
		m.startWatchers(queued, proc)

		m.broadcastEvent(api.Msg{
			"type": "event", "event": "status",
			"sessionId": queued.ID, "status": StatusProcessing, "prevStatus": StatusQueued,
		})
		m.savePoolState()
		return
	}
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
	delete(m.procs, from.ID)
	delete(m.pidToSID, from.PID)
	m.procs[to.ID] = proc
	m.pidToSID[proc.PID()] = to.ID
	to.PID = from.PID
	to.ClaudeUUID = from.ClaudeUUID
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
// Does NOT remove .pending files — those are coordination files for the Stop
// hook's background verification process (see idle-signal.sh).
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
// qualifies. Fresh and idle unpinned sessions are candidates. Fresh sessions
// (still pre-warming) are evicted before idle ones since they haven't served
// any user work yet.
func (m *Manager) findEvictableSession() *Session {
	var best *Session
	for _, s := range m.sessions {
		if (s.Status != StatusIdle && s.Status != StatusFresh) || s.Pinned {
			continue
		}
		if best == nil || evictsBefore(s, best) {
			best = s
		}
	}
	return best
}

// evictsBefore returns true if a should be evicted before b.
// Order: fresh pre-warmed first → lower priority → pre-warmed → fresh → empty pendingInput → oldest LRU.
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
	// Fresh user sessions evicted before idle (haven't done work yet)
	aFresh := a.Status == StatusFresh
	bFresh := b.Status == StatusFresh
	if aFresh != bFresh {
		return aFresh
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
}

func (m *Manager) tryKillTokens() {
	for m.killTokens > 0 {
		evicted := m.findEvictableSession()
		if evicted == nil {
			log.Printf("[kill-tokens] %d tokens remaining but no evictable sessions", m.killTokens)
			break
		}
		log.Printf("[kill-tokens] evicting session %s (tokens remaining=%d)", evicted.ID, m.killTokens-1)
		m.offloadSessionLocked(evicted)
		m.killTokens--
	}
}
