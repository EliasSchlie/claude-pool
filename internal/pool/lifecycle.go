package pool

import (
	"encoding/json"
	"fmt"
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
	return &Session{
		ID:         generateSessionID(),
		ParentID:   parentID,
		CreatedAt:  now,
		LastUsedAt: now,
		SpawnCwd:   m.paths.Root,
		Cwd:        m.paths.Root,
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
	log.Printf("[spawn] session %s: resume=%v flags=%q cwd=%s", s.ID, resume, flags, m.paths.Root)

	env := map[string]string{
		"CLAUDE_POOL_DIR":        m.paths.Root,
		"CLAUDE_POOL_SESSION_ID": s.ID,
	}

	opts := ptyPkg.SpawnOpts{
		Flags: flags,
		Cwd:   m.paths.Root,
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
		if sess := m.sessions[sessionID]; sess != nil && sess.IsLive() {
			prevStatus := sess.Status
			sess.Status = StatusDead
			m.broadcastStatus(sess, prevStatus)
		}
		// Close attach pipe on process death
		if pipe := m.pipes[sessionID]; pipe != nil {
			pipe.Close()
			delete(m.pipes, sessionID)
		}
		delete(m.pidToSID, proc.PID())
		delete(m.procs, sessionID)
		m.tryDequeue()
		m.tryReplaceDeadSessions()
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

			// Check for Claude UUID from PID mapping
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

			// Check idle signal
			data, err := os.ReadFile(signalPath)
			if err != nil {
				continue
			}

			// Parse signal JSON for cwd and transcript
			var sig map[string]any
			if err := json.Unmarshal(data, &sig); err == nil {
				if cwd, ok := sig["cwd"].(string); ok && cwd != "" {
					m.mu.Lock()
					if s := m.sessions[sessionID]; s != nil && s.Cwd != cwd {
						s.Cwd = cwd
						m.broadcastEvent(api.Msg{
							"type": "event", "event": "updated",
							"sessionId": s.ID, "changes": api.Msg{"cwd": cwd},
						})
					}
					m.mu.Unlock()
				}
				if transcript, ok := sig["transcript"].(string); ok && transcript != "" {
					m.mu.Lock()
					if s := m.sessions[sessionID]; s != nil && s.ClaudeUUID == "" {
						base := filepath.Base(transcript)
						if uuid := strings.TrimSuffix(base, ".jsonl"); uuid != base {
							s.ClaudeUUID = uuid
						}
					}
					m.mu.Unlock()
				}
			}

			// Session is idle
			m.mu.Lock()
			s = m.sessions[sessionID]
			if s == nil {
				log.Printf("[idle-watch] session %s: gone, stopping watcher", sessionID)
				m.mu.Unlock()
				return
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
					m.mu.Unlock()
					os.Remove(signalPath)
					m.deliverPromptAsync(sessionID, prompt)
					continue
				}

				prevStatus := s.Status
				s.Status = StatusIdle
				log.Printf("[idle-watch] session %s: %s → idle (pid=%d)", sessionID, prevStatus, s.PID)
				m.broadcastStatus(s, prevStatus)
				m.savePoolState()
				os.Remove(signalPath)

				// Check if a queued session can claim this slot
				m.serveQueueFromSlot(s)
			}
			m.mu.Unlock()
		}
	}
}

// --- Prompt delivery ---

// deliverPrompt sends a prompt to a session's terminal.
// Uses OC-style buffer polling: Escape → Ctrl-U → text → poll until echoed → Enter.
func (m *Manager) deliverPrompt(s *Session, prompt string) {
	proc := m.procs[s.ID]
	if proc == nil {
		log.Printf("[deliver] session %s: no process, skipping prompt delivery", s.ID)
		return
	}

	sid := s.ID
	go func() {
		time.Sleep(200 * time.Millisecond)
		proc.WriteString("\x1b") // Escape
		time.Sleep(200 * time.Millisecond)
		proc.WriteString("\x15") // Ctrl-U (clear line)
		time.Sleep(100 * time.Millisecond)
		proc.WriteString(prompt)

		// Brief check that prompt text appeared in buffer (Claude's TUI uses raw
		// mode so exact-match echo rarely works — keep timeout short).
		if !waitForBufferContent(proc, prompt, 200*time.Millisecond) {
			log.Printf("[deliver] session %s: prompt not echoed in buffer (expected with TUI), sending Enter", sid)
		}

		proc.WriteString("\r") // Enter
		log.Printf("[deliver] session %s: prompt delivered (%d chars)", sid, len(prompt))
	}()
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
	m.mu.Unlock()
	if s == nil {
		log.Printf("[deliver] session %s gone before prompt delivery", sessionID)
		return
	}
	m.deliverPrompt(s, prompt)
}

func (m *Manager) deliverPromptWhenReady(s *Session) {
	sid := s.ID
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(120 * time.Second)
		for {
			select {
			case <-deadline:
				log.Printf("[deliver] session %s: timed out waiting for idle to deliver prompt", sid)
				return
			case <-m.done:
				return
			case <-ticker.C:
				m.mu.Lock()
				sess := m.sessions[sid]
				if sess == nil {
					m.mu.Unlock()
					log.Printf("[deliver] session %s: gone before prompt delivery", sid)
					return
				}
				if sess.Status == StatusIdle {
					prompt := sess.PendingPrompt
					if prompt == "" {
						m.mu.Unlock()
						return
					}
					sess.PendingPrompt = ""
					sess.Status = StatusProcessing
					log.Printf("[deliver] session %s: idle → processing, delivering queued prompt (%d chars)", sid, len(prompt))
					m.broadcastStatus(sess, StatusIdle)
					m.mu.Unlock()
					m.deliverPrompt(sess, prompt)
					return
				}
				m.mu.Unlock()
			}
		}
	}()
}

// waitForSessionReady polls until a session transitions out of StatusFresh.
// Must be called WITHOUT m.mu held. Returns an API response.
func (m *Manager) waitForSessionReady(id any, sid string, timeout time.Duration) api.Msg {
	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			log.Printf("[followup] session %s: timed out waiting for ready", sid)
			return api.ErrorResponse(id, "session failed to become ready")
		case <-ticker.C:
			m.mu.Lock()
			s := m.sessions[sid]
			if s == nil {
				m.mu.Unlock()
				log.Printf("[followup] session %s: died before ready", sid)
				return api.ErrorResponse(id, "session died before ready")
			}
			status := s.Status
			m.mu.Unlock()
			if status == StatusProcessing || status == StatusIdle {
				log.Printf("[followup] session %s: ready (status=%s)", sid, status)
				return api.Response(id, "started", api.Msg{
					"sessionId": sid,
					"status":    status,
				})
			}
		}
	}
}

// writeIdleSignal writes a synthetic idle signal for cases where no hook fires
// (e.g., after Ctrl-C interrupts processing).
func (m *Manager) writeIdleSignal(pid int, trigger string) {
	signalPath := m.paths.IdleSignal(pid)
	signal := fmt.Sprintf(`{"cwd":"%s","session_id":"","transcript":"","ts":%d,"trigger":"%s"}`,
		m.paths.Root, time.Now().Unix(), trigger)
	log.Printf("[idle-signal] writing synthetic idle signal for pid %d (trigger=%s)", pid, trigger)
	if err := os.WriteFile(signalPath, []byte(signal+"\n"), 0600); err != nil {
		log.Printf("[idle-signal] error writing signal for pid %d: %v", pid, err)
	}
}

// --- Queue management ---

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

	// First pass: consume kill tokens by shrinking poolSize
	if m.killTokens > 0 {
		consumed := m.killTokens
		m.poolSize -= consumed
		if m.poolSize < 0 {
			consumed += m.poolSize // adjust: don't go below 0
			m.poolSize = 0
		}
		m.killTokens = 0
		log.Printf("[dequeue] consumed %d kill tokens, poolSize now %d", consumed, m.poolSize)
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
		if s.PendingPrompt != "" {
			m.deliverPromptWhenReady(s)
		}
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
		// Transfer the process from the idle session to the queued one
		proc := m.procs[idle.ID]
		if proc == nil {
			return
		}

		delete(m.procs, idle.ID)
		delete(m.pidToSID, idle.PID)
		delete(m.sessions, idle.ID)

		m.procs[queued.ID] = proc
		m.pidToSID[proc.PID()] = queued.ID
		queued.PID = idle.PID
		queued.ClaudeUUID = idle.ClaudeUUID
		queued.Cwd = idle.Cwd
		queued.Status = StatusProcessing

		prompt := queued.PendingPrompt
		queued.PendingPrompt = ""
		queued.PendingForce = false

		// Remove from queue
		m.queue = append(m.queue[:i], m.queue[i+1:]...)

		log.Printf("[serve-queue] session %s claimed slot from %s (pid=%d)", queued.ID, idle.ID, queued.PID)
		// Clear stale idle signal before starting watcher
		os.Remove(m.paths.IdleSignal(queued.PID))
		os.Remove(m.paths.IdleSignal(queued.PID) + ".pending")
		m.deliverPrompt(queued, prompt)
		go m.watchIdleSignal(queued.ID, queued.PID)
		m.watchProcessDone(queued.ID, proc)

		m.broadcastEvent(api.Msg{
			"type": "event", "event": "status",
			"sessionId": queued.ID, "status": StatusProcessing, "prevStatus": StatusQueued,
		})
		m.savePoolState()
		return
	}
}

// --- Slot management ---

func (m *Manager) findFreshSlot() *Session {
	// Only claim pre-warmed sessions (pool-owned, never used by a client).
	// Prefer idle (ready) over fresh (still starting).
	for _, s := range m.sessions {
		if s.PreWarmed && s.Status == StatusIdle && !s.Pinned {
			return s
		}
	}
	for _, s := range m.sessions {
		if s.PreWarmed && s.Status == StatusFresh && !s.Pinned {
			return s
		}
	}
	return nil
}

func (m *Manager) findEvictableSession() *Session {
	return m.findEvictableSessionBelow(float64(1 << 53)) // effectively no ceiling
}

// findEvictableSessionBelow returns the lowest-priority idle unpinned session
// with priority strictly below maxPriority, or nil if none qualifies.
func (m *Manager) findEvictableSessionBelow(maxPriority float64) *Session {
	var best *Session
	for _, s := range m.sessions {
		if s.Status != StatusIdle || s.Pinned {
			continue
		}
		if s.Priority >= maxPriority {
			continue
		}
		if best == nil || s.Priority < best.Priority ||
			(s.Priority == best.Priority && s.LastUsedAt.Before(best.LastUsedAt)) {
			best = s
		}
	}
	return best
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
