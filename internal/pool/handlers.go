package pool

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

// parentFromReq reads the parent field with backward compat fallback.
func parentFromReq(req api.Msg) string {
	if v, _ := req["parent"].(string); v != "" {
		return v
	}
	v, _ := req["parentId"].(string)
	return v
}

// --- Config ---

func (m *Manager) handleConfig(id any, req api.Msg) api.Msg {
	if setMap, ok := req["set"].(map[string]any); ok {
		cfg, err := m.config.Update(setMap)
		if err != nil {
			return api.ErrorResponse(id, "config update failed: "+err.Error())
		}
		return api.ConfigResponse(id, configToMsg(cfg))
	}

	cfg, err := m.config.Load()
	if err != nil {
		return api.ErrorResponse(id, "config read failed: "+err.Error())
	}
	return api.ConfigResponse(id, configToMsg(cfg))
}

// --- Init ---

func (m *Manager) handleInit(id any, req api.Msg) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.initialized {
		log.Printf("[init] rejected: pool already initialized")
		return api.ErrorResponse(id, "pool already initialized")
	}

	cfg, _ := m.config.Load()
	size := cfg.Size
	if reqSize, ok := req["size"].(float64); ok {
		size = int(reqSize)
	}
	if size <= 0 {
		return api.ErrorResponse(id, "pool size must be positive")
	}

	noRestore, _ := req["noRestore"].(bool)
	log.Printf("[init] initializing pool: size=%d noRestore=%v", size, noRestore)

	m.initialized = true
	m.poolSize = size

	// Deploy hook scripts to pool directory — each pool owns its own hooks
	if err := m.deployHooks(); err != nil {
		m.initialized = false
		log.Printf("[init] %v", err)
		return api.ErrorResponse(id, err.Error())
	}

	// Try to restore sessions from pool.json (unless noRestore)
	var liveSessions, offloadedSessions []*Session
	if !noRestore {
		liveSessions, offloadedSessions = m.loadPoolState()
	}

	sessions := make([]any, 0)

	// Restore live sessions into slots first
	restored := 0
	log.Printf("[init] restoring state: %d live, %d offloaded sessions from pool.json", len(liveSessions), len(offloadedSessions))
	for _, s := range liveSessions {
		if restored >= size {
			log.Printf("[init] session %s exceeds pool size, marking offloaded", s.ID)
			s.Status = StatusOffloaded
			m.sessions[s.ID] = s
			continue
		}
		m.sessions[s.ID] = s
		s.Status = StatusFresh
		log.Printf("[init] restoring live session %s (claude=%s resume=%v)", s.ID, s.ClaudeUUID, s.ClaudeUUID != "")
		m.spawnSession(s, s.ClaudeUUID != "")
		sessions = append(sessions, s.ToMsg())
		restored++
	}

	// Restore offloaded sessions into remaining slots
	for _, s := range offloadedSessions {
		if restored >= size {
			log.Printf("[init] session %s exceeds pool size, keeping offloaded", s.ID)
			s.Status = StatusOffloaded
			m.sessions[s.ID] = s
			continue
		}
		m.sessions[s.ID] = s
		s.Status = StatusFresh
		log.Printf("[init] restoring offloaded session %s (claude=%s resume=%v)", s.ID, s.ClaudeUUID, s.ClaudeUUID != "")
		m.spawnSession(s, s.ClaudeUUID != "")
		sessions = append(sessions, s.ToMsg())
		restored++
	}

	// Fill remaining slots with fresh pre-warmed sessions
	fresh := size - restored
	if fresh > 0 {
		log.Printf("[init] spawning %d fresh pre-warmed sessions", fresh)
	}
	for i := restored; i < size; i++ {
		s := m.newSession("")
		s.Status = StatusFresh
		s.PreWarmed = true
		m.sessions[s.ID] = s
		m.spawnSession(s, false)
		sessions = append(sessions, s.ToMsg())
	}

	log.Printf("[init] pool initialized: %d sessions total", len(sessions))
	m.savePoolState()
	m.startTypingPoller()

	return api.Response(id, "pool", api.Msg{
		"pool": api.Msg{
			"size":     float64(size),
			"sessions": sessions,
		},
	})
}

// --- Health ---

func (m *Manager) handleHealth(id any) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	counts := map[string]float64{}
	healthSessions := make([]any, 0)

	for _, s := range m.sessions {
		if s.Status == StatusArchived {
			continue
		}
		status := s.ExternalStatus()
		counts[status]++

		hs := api.Msg{
			"sessionId": s.ID,
			"status":    status,
		}
		if s.PID > 0 {
			hs["pid"] = float64(s.PID)
			hs["pidAlive"] = isPidAlive(s.PID)
		} else {
			hs["pidAlive"] = false
		}
		healthSessions = append(healthSessions, hs)
	}

	return api.Response(id, "health", api.Msg{
		"health": api.Msg{
			"size":       float64(m.poolSize),
			"counts":     counts,
			"queueDepth": float64(len(m.queue)),
			"sessions":   healthSessions,
		},
	})
}

// --- Start ---

func (m *Manager) handleStart(id any, req api.Msg) api.Msg {
	prompt, _ := req["prompt"].(string)
	if prompt == "" {
		return api.ErrorResponse(id, "prompt is required")
	}
	parentID := parentFromReq(req)

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	s := m.newSession(parentID)
	s.PendingPrompt = prompt
	if _, hasMetadata := req["metadata"]; hasMetadata {
		s.Metadata = metadataFromMap(req)
	}
	m.sessions[s.ID] = s
	log.Printf("[start] created session %s (parent=%s, prompt=%d chars, name=%q)", s.ID, parentID, len(prompt), s.Metadata.Name)

	// Try to claim a fresh/idle slot
	if fresh := m.findFreshSlot(); fresh != nil {
		log.Printf("[start] session %s taking over slot from %s (status=%s, pid=%d)", s.ID, fresh.ID, fresh.Status, fresh.PID)
		proc := m.transferProcess(fresh, s)
		wasFresh := fresh.Status == StatusFresh
		delete(m.sessions, fresh.ID)

		if wasFresh {
			// Process not ready yet — queue prompt for delivery when
			// SessionStart hook signals readiness
			log.Printf("[start] session %s: slot still starting, queuing prompt for delivery on ready", s.ID)
			s.Status = StatusFresh
			s.PendingPrompt = prompt
			s.PendingForce = true
		} else {
			log.Printf("[start] session %s: slot ready, delivering prompt immediately", s.ID)
			s.Status = StatusProcessing
			s.LastUsedAt = time.Now()
			s.PendingPrompt = ""
			m.deliverPrompt(s, prompt)
		}
		m.clearIdleSignals(s.PID)
		m.startWatchers(s, proc)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": StatusProcessing, "parent": s.ParentID,
		})
		m.savePoolState()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    StatusProcessing,
		})
	}

	// No fresh slot available — queue the request, then try to fill from
	// available slots. Only evict if all slots are occupied.
	log.Printf("[start] session %s: no fresh slots, queuing (queue depth=%d)", s.ID, len(m.queue)+1)
	s.Status = StatusQueued
	m.queue = append(m.queue, s)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "created",
		"sessionId": s.ID, "status": s.Status, "parent": s.ParentID,
	})

	m.tryDequeueWithEviction(s, "")
	m.savePoolState()

	return api.Response(id, "started", api.Msg{
		"sessionId": s.ID,
		"status":    s.Status,
	})
}

// --- Followup ---

func (m *Manager) handleFollowup(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	prompt, _ := req["prompt"].(string)
	force, _ := req["force"].(bool)

	if sessionID == "" || prompt == "" {
		return api.ErrorResponse(id, "sessionId and prompt are required")
	}

	m.mu.Lock()

	s := m.resolveSession(sessionID)
	if s == nil {
		m.mu.Unlock()
		log.Printf("[followup] session not found: %s", sessionID)
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	log.Printf("[followup] session %s: status=%s force=%v prompt=%d chars", s.ID, s.Status, force, len(prompt))

	switch s.Status {
	case StatusFresh:
		// Fresh sessions aren't ready yet — queue prompt and wait for
		// the SessionStart hook to signal readiness and deliver it.
		log.Printf("[followup] session %s: fresh, queuing prompt and waiting for ready", s.ID)
		s.PendingPrompt = prompt
		s.PendingForce = true
		sid := s.ID
		m.mu.Unlock()

		// Poll until watchIdleSignal delivers the prompt (transitions to processing).
		// Lock is NOT held between polls — no fragile defer interaction.
		return m.waitForSessionReady(id, sid, 60*time.Second)

	case StatusIdle:
		log.Printf("[followup] session %s: idle → processing, delivering prompt", s.ID)
		s.Status = StatusProcessing
		s.LastUsedAt = time.Now()
		m.deliverPrompt(s, prompt)
		m.broadcastStatus(s, StatusIdle)
		m.savePoolState()
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
		})

	case StatusOffloaded, StatusError:
		log.Printf("[followup] session %s: %s → queued for respawn", s.ID, s.Status)
		s.PendingPrompt = prompt
		prevStatus := s.Status
		s.Status = StatusQueued
		m.queue = append(m.queue, s)
		m.broadcastStatus(s, prevStatus)
		m.tryDequeueWithEviction(s, s.ID)
		m.savePoolState()
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
		})

	case StatusProcessing:
		if !force {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session is processing; use force: true to override")
		}
		log.Printf("[followup] session %s: force-interrupting %s, sending Ctrl-C (pid=%d)", s.ID, s.Status, s.PID)
		s.LastUsedAt = time.Now()
		sid := s.ID
		m.savePoolState()
		m.mu.Unlock()

		// Wait for any in-flight prompt delivery to finish before
		// sending Ctrl-C — otherwise the old prompt's Enter keystroke
		// arrives after Ctrl-C, starting the old command.
		m.awaitDelivery(sid)

		m.mu.Lock()
		proc := m.procs[sid]
		if proc != nil {
			proc.WriteString("\x03")
		}
		m.mu.Unlock()

		// Give Claude time to process the Ctrl-C: cancel the current
		// task, write "[Request interrupted by user]", and return to
		// its prompt. Then deliver the new prompt directly — don't
		// rely on Stop hook (may not fire reliably for shell builtins).
		time.Sleep(3 * time.Second)

		m.mu.Lock()
		s = m.sessions[sid]
		if s != nil {
			m.deliverPrompt(s, prompt)
		}
		m.mu.Unlock()

		return api.Response(id, "started", api.Msg{
			"sessionId": sid,
			"status":    StatusProcessing,
		})

	case StatusQueued:
		if !force {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session is queued; use force: true to replace prompt")
		}
		s.PendingPrompt = prompt
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
		})

	case StatusArchived:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is archived; unarchive first")

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot followup in state: "+s.Status)
	}
}

// --- Wait ---

func (m *Manager) handleWait(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	parentFilter := parentFromReq(req)
	timeoutMs := 300000.0
	if t, ok := req["timeout"].(float64); ok {
		timeoutMs = t
	}
	source, turns, detail := parseCaptureParams(req)

	timeout := time.Duration(timeoutMs) * time.Millisecond

	m.mu.Lock()

	if sessionID == "" {
		// Find the most recently created busy session matching the filter
		var busySession *Session
		for _, s := range m.sessions {
			if s.PreWarmed {
				continue
			}
			if s.Status == StatusProcessing || s.Status == StatusQueued || s.Status == StatusFresh {
				// Apply parent filter if set
				if parentFilter != "" && s.ParentID != parentFilter {
					continue
				}
				if busySession == nil || s.CreatedAt.After(busySession.CreatedAt) {
					busySession = s
				}
			}
		}
		if busySession == nil {
			m.mu.Unlock()
			log.Printf("[wait] no busy sessions found")
			return api.ErrorResponse(id, "no busy sessions")
		}
		sessionID = busySession.ID
		log.Printf("[wait] no sessionId specified, selected most recent busy session %s (status=%s)", sessionID, busySession.Status)
	}

	s := m.resolveSession(sessionID)
	if s == nil {
		m.mu.Unlock()
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	switch s.Status {
	case StatusOffloaded:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is offloaded; use followup to resume")
	case StatusArchived:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is archived")
	case StatusError:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session has error")
	}

	if s.Status == StatusIdle {
		content := m.captureOutput(s, source, turns, detail)
		m.mu.Unlock()
		return api.Response(id, "result", api.Msg{
			"sessionId": s.ID,
			"content":   content,
		})
	}

	sid := s.ID
	m.mu.Unlock()

	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return api.ErrorResponse(id, "timeout")
		case <-ticker.C:
			m.mu.Lock()
			s := m.sessions[sid]
			if s == nil {
				m.mu.Unlock()
				return api.ErrorResponse(id, "session not found")
			}
			switch s.Status {
			case StatusIdle:
				content := m.captureOutput(s, source, turns, detail)
				m.mu.Unlock()
				return api.Response(id, "result", api.Msg{
					"sessionId": sid,
					"content":   content,
				})
			case StatusOffloaded:
				m.mu.Unlock()
				return api.ErrorResponse(id, "session is offloaded; use followup to resume")
			case StatusError:
				m.mu.Unlock()
				return api.ErrorResponse(id, "session error")
			}
			m.mu.Unlock()
		}
	}
}

// --- Stop ---

func (m *Manager) handleStop(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	s := m.resolveSession(sessionID)
	if s == nil {
		m.mu.Unlock()
		log.Printf("[stop] session not found: %s", sessionID)
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	log.Printf("[stop] session %s: status=%s pid=%d", s.ID, s.Status, s.PID)

	switch s.Status {
	case StatusIdle, StatusFresh:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is not processing or queued (status: "+s.ExternalStatus()+")")

	case StatusQueued:
		log.Printf("[stop] session %s: removing from queue", s.ID)
		m.removeFromQueue(s)
		if s.PID > 0 {
			s.Status = StatusOffloaded
		} else {
			delete(m.sessions, s.ID)
		}
		m.savePoolState()
		m.mu.Unlock()
		return api.OkResponse(id)

	case StatusProcessing:
		log.Printf("[stop] session %s: sending Ctrl-C to pid %d", s.ID, s.PID)
		sid := s.ID
		// Transition directly — Ctrl-C interrupts the bash tool, and we
		// don't need to wait for a hook round-trip. The next prompt delivery
		// (Escape → Ctrl-U → text → Enter) resets Claude's input line anyway.
		prevStatus := s.Status
		s.Status = StatusIdle
		s.PendingInput = ""
		log.Printf("[stop] session %s: processing → idle", s.ID)
		m.broadcastStatus(s, prevStatus)
		m.savePoolState()
		m.mu.Unlock()

		// Wait for any in-flight delivery to finish, then send Ctrl-C
		m.awaitDelivery(sid)
		m.mu.Lock()
		if proc := m.procs[sid]; proc != nil {
			proc.WriteString("\x03")
		}
		// Check if a queued session can claim this now-idle slot.
		if s := m.sessions[sid]; s != nil && s.Status == StatusIdle {
			m.serveQueueFromSlot(s)
			// If the queue still has entries, evict an idle session to
			// make room — the stopped session is now evictable.
			if len(m.queue) > 0 {
				if evicted := m.findEvictableSession(); evicted != nil {
					m.offloadSessionLocked(evicted)
				}
			}
		}
		m.tryDequeue()
		m.mu.Unlock()
		return api.OkResponse(id)

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot stop session in state: "+s.Status)
	}
}

// parseCaptureParams extracts source/turns/detail from a request with defaults.
// Also supports the legacy "format" field (e.g., "jsonl-short", "buffer-full")
// which maps to the equivalent source/turns/detail combination.
func parseCaptureParams(req api.Msg) (source string, turns int, detail string) {
	if format, ok := req["format"].(string); ok && format != "" {
		switch format {
		case "jsonl-short", "jsonl-last": // aliases — both mean last turn, last assistant response
			return "jsonl", 1, "last"
		case "jsonl-long":
			return "jsonl", 1, "tools"
		case "jsonl-full":
			return "jsonl", 0, "raw"
		case "buffer-last":
			return "buffer", 1, "last"
		case "buffer-full":
			return "buffer", 0, "last"
		}
	}

	source, _ = req["source"].(string)
	if source == "" {
		source = "jsonl"
	}
	turns = 1
	if t, ok := req["turns"].(float64); ok {
		turns = int(t)
	}
	detail, _ = req["detail"].(string)
	if detail == "" {
		detail = "last"
	}
	return
}

// --- Capture ---

func (m *Manager) handleCapture(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	source, turns, detail := parseCaptureParams(req)

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status == StatusQueued {
		return api.ErrorResponse(id, "session is queued (no output yet)")
	}

	if source == "buffer" && !s.IsLive() {
		return api.ErrorResponse(id, "buffer source requires live terminal")
	}

	content := m.captureOutput(s, source, turns, detail)
	return api.Response(id, "result", api.Msg{
		"sessionId": s.ID,
		"content":   content,
	})
}

// --- Info ---

func (m *Manager) handleInfo(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	// Fallback: read cwd from OS process if hook hasn't reported one yet.
	// Hook-reported cwd (from idle signals) is authoritative — it tracks
	// Bash tool cd commands. OS process cwd only reflects the spawn directory.
	if s.Cwd == "" && s.IsLive() && s.PID > 0 {
		if cwd := getCwd(s.PID); cwd != "" {
			s.Cwd = cwd
		}
	}

	msg := s.ToMsgWithChildren(m.sessions)
	return api.Response(id, "session", api.Msg{"session": msg})
}

// --- Ls ---

func (m *Manager) handleLs(id any, req api.Msg) api.Msg {
	all, _ := req["all"].(bool)
	tree, _ := req["tree"].(bool)
	showArchived, _ := req["archived"].(bool)
	callerId, _ := req["callerId"].(string)

	// Parse optional statuses filter
	var statusFilter map[string]bool
	if raw, ok := req["statuses"].([]any); ok && len(raw) > 0 {
		statusFilter = make(map[string]bool, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				statusFilter[s] = true
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	results := make([]any, 0)
	for _, s := range m.sessions {
		if s.Status == StatusArchived && !showArchived {
			continue
		}
		if statusFilter != nil && !statusFilter[s.Status] {
			continue
		}

		// Ownership filtering: if callerId is set (and not "all"), only
		// show direct children of the caller
		if callerId != "" && !all {
			if s.ParentID != callerId {
				continue
			}
		}

		if tree {
			// In tree mode without ownership filter, only show root-level sessions
			if callerId == "" && !all {
				if s.ParentID != "" {
					if _, hasParent := m.sessions[s.ParentID]; hasParent {
						continue
					}
				}
			}
			results = append(results, s.ToMsgWithChildren(m.sessions))
		} else {
			results = append(results, s.ToMsg())
		}
	}

	return api.Response(id, "sessions", api.Msg{"sessions": results})
}

// --- Offload ---

func (m *Manager) handleOffload(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status != StatusIdle {
		log.Printf("[offload] session %s: rejected, status=%s (need idle)", s.ID, s.Status)
		return api.ErrorResponse(id, "can only offload idle sessions (current: "+s.Status+")")
	}

	if s.Pinned {
		log.Printf("[offload] session %s: auto-unpinning before offload", s.ID)
	}
	log.Printf("[offload] session %s: offloading (pid=%d claude=%s)", s.ID, s.PID, s.ClaudeUUID)
	m.offloadSessionLocked(s)
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Archive ---

func (m *Manager) handleArchive(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	recursive, _ := req["recursive"].(bool)

	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status == StatusArchived {
		log.Printf("[archive] session %s: already archived", s.ID)
		return api.OkResponse(id)
	}

	log.Printf("[archive] session %s: archiving (status=%s recursive=%v)", s.ID, s.Status, recursive)

	if !recursive {
		for _, other := range m.sessions {
			if other.ParentID == s.ID && other.Status != StatusArchived {
				log.Printf("[archive] session %s: rejected, has unarchived child %s", s.ID, other.ID)
				return api.ErrorResponse(id, "session has unarchived children; use recursive: true")
			}
		}
	} else if recursive {
		m.archiveDescendants(s.ID)
	}

	m.archiveSessionLocked(s)
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Unarchive ---

func (m *Manager) handleUnarchive(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status != StatusArchived {
		log.Printf("[unarchive] session %s: rejected, status=%s (need archived)", s.ID, s.Status)
		return api.ErrorResponse(id, "only archived sessions can be unarchived")
	}

	log.Printf("[unarchive] session %s: archived → offloaded", s.ID)
	s.Status = StatusOffloaded
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "unarchived", "sessionId": s.ID,
	})
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Set (unified priority/pinned/metadata) ---

func (m *Manager) handleSet(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	// Priority
	if priority, ok := req["priority"].(float64); ok {
		log.Printf("[set] session %s: priority %.1f → %.1f", s.ID, s.Priority, priority)
		s.Priority = priority
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"priority": priority},
		})
	}

	// Pinned
	if pinned, ok := req["pinned"]; ok {
		switch v := pinned.(type) {
		case bool:
			if !v {
				log.Printf("[set] session %s: unpinning", s.ID)
				s.Pinned = false
				m.broadcastEvent(api.Msg{
					"type": "event", "event": "updated",
					"sessionId": s.ID, "changes": api.Msg{"pinned": false},
				})
			}
		case float64:
			duration := v
			log.Printf("[set] session %s: pinning for %.0fs", s.ID, duration)
			s.Pinned = true
			s.PinExpiry = time.Now().Add(time.Duration(duration) * time.Second)

			// If offloaded, queue for loading
			if s.Status == StatusOffloaded || s.Status == StatusError {
				prevStatus := s.Status
				s.Status = StatusQueued
				m.queue = append([]*Session{s}, m.queue...)
				m.broadcastStatus(s, prevStatus)
				m.tryDequeueWithEviction(s, s.ID)
			}

			m.broadcastEvent(api.Msg{
				"type": "event", "event": "updated",
				"sessionId": s.ID, "changes": api.Msg{"pinned": true},
			})
		}
	}

	// Metadata
	if metadata, ok := req["metadata"].(map[string]any); ok {
		if s.Metadata.Tags == nil {
			s.Metadata.Tags = map[string]string{}
		}
		for k, v := range metadata {
			if sv, ok := v.(string); ok {
				s.Metadata.Tags[k] = sv
			}
		}
	}

	m.savePoolState()
	return api.OkResponse(id)
}

// --- Pin ---

func (m *Manager) handlePin(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	parentID := parentFromReq(req)
	duration := 120.0
	if d, ok := req["duration"].(float64); ok {
		duration = d
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	if sessionID == "" {
		s := m.newSession(parentID)
		if _, hasMetadata := req["metadata"]; hasMetadata {
			s.Metadata = metadataFromMap(req)
		}
		m.sessions[s.ID] = s
		log.Printf("[pin] creating new pinned session %s (parent=%s duration=%.0fs)", s.ID, parentID, duration)

		if fresh := m.findFreshSlot(); fresh != nil {
			log.Printf("[pin] session %s: taking over slot from %s (status=%s pid=%d)", s.ID, fresh.ID, fresh.Status, fresh.PID)
			proc := m.transferProcess(fresh, s)
			if fresh.Status == StatusIdle {
				s.Status = StatusIdle
			} else {
				s.Status = StatusFresh
			}
			if proc != nil {
				m.clearIdleSignals(s.PID)
				m.startWatchers(s, proc)
			}
			delete(m.sessions, fresh.ID)
		} else if evicted := m.findEvictableSession(); evicted != nil {
			log.Printf("[pin] session %s: evicting idle session %s to free slot", s.ID, evicted.ID)
			m.offloadSessionLocked(evicted)
			s.Status = StatusFresh
			m.spawnSession(s, false)
		} else {
			log.Printf("[pin] session %s: no slots available, queuing at front", s.ID)
			s.Status = StatusQueued
			m.queue = append([]*Session{s}, m.queue...)
		}

		s.Pinned = true
		s.PinExpiry = time.Now().Add(time.Duration(duration) * time.Second)
		m.savePoolState()

		return api.Response(id, "ok", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
		})
	}

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status == StatusArchived {
		return api.ErrorResponse(id, "session is archived; unarchive first")
	}

	log.Printf("[pin] session %s: pinning (status=%s duration=%.0fs)", s.ID, s.Status, duration)
	s.Pinned = true
	s.PinExpiry = time.Now().Add(time.Duration(duration) * time.Second)

	responseStatus := s.Status
	if s.Status == StatusOffloaded || s.Status == StatusError {
		prevStatus := s.Status
		s.Status = StatusQueued
		responseStatus = StatusQueued
		m.queue = append([]*Session{s}, m.queue...)
		m.broadcastStatus(s, prevStatus)
		m.tryDequeueWithEviction(s, s.ID)
	} else if s.Status == StatusQueued {
		m.removeFromQueue(s)
		m.queue = append([]*Session{s}, m.queue...)
	}

	m.broadcastEvent(api.Msg{
		"type": "event", "event": "updated",
		"sessionId": s.ID, "changes": api.Msg{"pinned": true},
	})
	m.savePoolState()

	return api.Response(id, "ok", api.Msg{
		"sessionId": s.ID,
		"status":    responseStatus,
	})
}

// --- Unpin ---

func (m *Manager) handleUnpin(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	log.Printf("[unpin] session %s: unpinning", s.ID)
	s.Pinned = false
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "updated",
		"sessionId": s.ID, "changes": api.Msg{"pinned": false},
	})
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Set Priority ---

func (m *Manager) handleSetPriority(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	priority, ok := req["priority"].(float64)
	if sessionID == "" || !ok {
		return api.ErrorResponse(id, "sessionId and priority are required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	log.Printf("[set-priority] session %s: priority %.1f → %.1f", s.ID, s.Priority, priority)
	s.Priority = priority
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "updated",
		"sessionId": s.ID, "changes": api.Msg{"priority": priority},
	})
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Set Metadata ---

func (m *Manager) handleSetMetadata(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}
	metaRaw, hasMetadata := req["metadata"].(map[string]any)
	if !hasMetadata {
		return api.ErrorResponse(id, "metadata is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	changes := map[string]any{}

	// Merge semantics: only update fields that are present in the request.
	// Explicit null clears the field. Wrong types are rejected.
	if v, ok := metaRaw["name"]; ok {
		if v == nil {
			s.Metadata.Name = ""
		} else if sv, ok := v.(string); ok {
			s.Metadata.Name = sv
		} else {
			return api.ErrorResponse(id, "metadata.name must be a string or null")
		}
		changes["name"] = s.Metadata.Name
	}
	if v, ok := metaRaw["description"]; ok {
		if v == nil {
			s.Metadata.Description = ""
		} else if sv, ok := v.(string); ok {
			s.Metadata.Description = sv
		} else {
			return api.ErrorResponse(id, "metadata.description must be a string or null")
		}
		changes["description"] = s.Metadata.Description
	}
	if v, ok := metaRaw["tags"]; ok {
		if v == nil {
			s.Metadata.Tags = nil
		} else if tagMap, ok := v.(map[string]any); ok {
			if s.Metadata.Tags == nil {
				s.Metadata.Tags = make(map[string]string)
			}
			for tk, tv := range tagMap {
				if tv == nil {
					delete(s.Metadata.Tags, tk)
				} else if sv, ok := tv.(string); ok {
					s.Metadata.Tags[tk] = sv
				} else {
					return api.ErrorResponse(id, "metadata.tags values must be strings or null")
				}
			}
			if len(s.Metadata.Tags) == 0 {
				s.Metadata.Tags = nil
			}
		} else {
			return api.ErrorResponse(id, "metadata.tags must be an object or null")
		}
		// Report current tags state in changes (copy to avoid aliasing)
		if len(s.Metadata.Tags) > 0 {
			tagsCopy := make(map[string]string, len(s.Metadata.Tags))
			for k, v := range s.Metadata.Tags {
				tagsCopy[k] = v
			}
			changes["tags"] = tagsCopy
		} else {
			changes["tags"] = nil
		}
	}

	log.Printf("[set-metadata] session %s: updated metadata (name=%q, desc=%d chars, tags=%d)",
		s.ID, s.Metadata.Name, len(s.Metadata.Description), len(s.Metadata.Tags))

	m.broadcastEvent(api.Msg{
		"type": "event", "event": "updated",
		"sessionId": s.ID, "changes": api.Msg{"metadata": changes},
	})
	m.savePoolState()
	return api.OkResponse(id)
}

// --- Resize ---

func (m *Manager) handleResize(id any, req api.Msg) api.Msg {
	newSize, ok := req["size"].(float64)
	if !ok {
		return api.ErrorResponse(id, "size is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	target := int(newSize)
	if target < 1 {
		return api.ErrorResponse(id, "size must be >= 1")
	}

	// Reset kill tokens — resize is absolute ("to size N"), so any pending
	// evictions from a prior shrink are superseded by the new target.
	if m.killTokens > 0 {
		log.Printf("[resize] clearing %d kill tokens from prior shrink", m.killTokens)
		m.killTokens = 0
	}

	oldSize := m.poolSize
	m.poolSize = target
	log.Printf("[resize] pool size: %d → %d", oldSize, target)

	if target > oldSize {
		log.Printf("[resize] spawning %d new sessions", target-oldSize)
		for i := oldSize; i < target; i++ {
			s := m.newSession("")
			s.Status = StatusFresh
			s.PreWarmed = true
			m.sessions[s.ID] = s
			m.spawnSession(s, false)
		}
	} else if target < oldSize {
		log.Printf("[resize] shrinking: adding %d kill tokens", oldSize-target)
		m.killTokens = oldSize - target
		m.tryKillTokens()
	}

	m.savePoolState()
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "pool", "action": "resize", "size": float64(target),
	})

	return api.Response(id, "pool", api.Msg{
		"pool": api.Msg{
			"size": float64(m.poolSize),
		},
	})
}

// --- Input ---

func (m *Manager) handleInput(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	data, _ := req["data"].(string)

	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if !s.IsLive() {
		return api.ErrorResponse(id, "session has no live terminal (status: "+s.Status+")")
	}

	proc := m.procs[s.ID]
	if proc == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	if err := proc.WriteString(data); err != nil {
		log.Printf("[input] session %s: write error: %v", s.ID, err)
		return api.ErrorResponse(id, "write error: "+err.Error())
	}

	// Track pendingInput the same way attach does
	m.trackInput(s, []byte(data))

	log.Printf("[input] session %s: wrote %d bytes", s.ID, len(data))
	return api.OkResponse(id)
}

// --- Attach ---

func (m *Manager) handleAttach(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if !s.IsLive() {
		return api.ErrorResponse(id, "session is not live (status: "+s.Status+")")
	}

	proc := m.procs[s.ID]
	if proc == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	// Reuse existing pipe if still open
	if pipe, ok := m.pipes[s.ID]; ok {
		log.Printf("[attach] session %s: reusing existing pipe at %s", s.ID, pipe.socketPath)
		return api.Response(id, "attached", api.Msg{
			"socketPath": pipe.socketPath,
		})
	}

	pipe, err := newAttachPipe(s.ID, m.paths.Root, proc)
	if err != nil {
		log.Printf("[attach] session %s: failed to create pipe: %v", s.ID, err)
		return api.ErrorResponse(id, "failed to create attach pipe: "+err.Error())
	}

	sid := s.ID
	pipe.onInput = func(data []byte) {
		m.handleAttachInput(sid, data)
	}

	m.pipes[s.ID] = pipe
	log.Printf("[attach] session %s: pipe created at %s", s.ID, pipe.socketPath)
	return api.Response(id, "attached", api.Msg{
		"socketPath": pipe.socketPath,
	})
}

// handleAttachInput processes raw input from attach pipe clients for typing
// detection and prompt delivery.
//
// trackInput updates pendingInput based on raw bytes written to a session's PTY.
// Caller must hold m.mu. Used by both handleInput (debug input) and handleAttachInput.
func (m *Manager) trackInput(s *Session, data []byte) {
	hasCtrlU := false
	var printable []byte
	for _, b := range data {
		switch {
		case b == 0x15: // Ctrl-U
			hasCtrlU = true
		case b >= 0x20 && b != 0x7f: // printable (not DEL)
			printable = append(printable, b)
		}
	}

	switch {
	case hasCtrlU:
		if s.PendingInput != "" {
			s.PendingInput = ""
			s.LastUsedAt = time.Now()
			delete(m.attachTyping, s.ID)
			m.broadcastEvent(api.Msg{
				"type": "event", "event": "updated",
				"sessionId": s.ID, "changes": api.Msg{"pendingInput": ""},
			})
		}
	case len(printable) > 0 && (s.Status == StatusIdle || s.Status == StatusFresh):
		m.attachTyping[s.ID] = append(m.attachTyping[s.ID], printable...)
		s.PendingInput = string(m.attachTyping[s.ID])
		s.LastUsedAt = time.Now()
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
		})
	}
}

// handleAttachInput classifies raw bytes from an attach client and manages
// pendingInput property and state transitions. Raw bytes always pass through
// to the PTY (the attach pipe writes them directly).
//
// When Enter is detected, the accumulated text is delivered via deliverPrompt
// (Escape → Ctrl-U → text → Enter with proper timing) to ensure Claude Code's
// TUI processes the prompt reliably.
func (m *Manager) handleAttachInput(sessionID string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.sessions[sessionID]
	if s == nil {
		return
	}

	// Classify input and extract printable text
	hasCtrlU := false
	hasEnter := false
	var printable []byte
	for _, b := range data {
		switch {
		case b == 0x15: // Ctrl-U
			hasCtrlU = true
		case b == '\r' || b == '\n': // Enter
			hasEnter = true
		case b >= 0x20 && b != 0x7f: // printable (not DEL)
			printable = append(printable, b)
		}
	}

	switch {
	case hasCtrlU && s.PendingInput != "":
		// Clear pending input
		s.PendingInput = ""
		s.LastUsedAt = time.Now()
		delete(m.attachTyping, sessionID)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"pendingInput": ""},
		})

	case hasEnter && (s.PendingInput != "" || ((s.Status == StatusIdle || s.Status == StatusFresh) && len(printable) > 0)):
		// Submit prompt
		buf := m.attachTyping[sessionID]
		buf = append(buf, printable...)
		prompt := string(buf)
		delete(m.attachTyping, sessionID)

		if prompt == "" {
			return
		}

		// Clear pendingInput and transition to processing
		s.PendingInput = ""
		prev := s.Status
		s.Status = StatusProcessing
		s.LastUsedAt = time.Now()
		m.broadcastStatus(s, prev)
		m.clearIdleSignals(s.PID)
		log.Printf("[attach] session %s: prompt submitted via attach (%d chars)", sessionID, len(prompt))

		// Also deliver via the reliable prompt mechanism (Escape → Ctrl-U → text → Enter).
		// Raw bytes reach the PTY too, but Claude's TUI may not reliably process
		// raw input without the Escape/Ctrl-U reset sequence.
		go m.deliverPromptAsync(sessionID, prompt)

	case len(printable) > 0 && (s.Status == StatusIdle || s.Status == StatusFresh):
		// Accumulate typed text as pendingInput
		m.attachTyping[sessionID] = append(m.attachTyping[sessionID], printable...)
		s.PendingInput = string(m.attachTyping[sessionID])
		s.LastUsedAt = time.Now()
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
		})
	}
}

// --- Destroy ---

func (m *Manager) handleDestroy(id any, req api.Msg) api.Msg {
	confirm, _ := req["confirm"].(bool)
	if !confirm {
		return api.ErrorResponse(id, "destroy requires confirm: true")
	}

	m.mu.Lock()

	log.Printf("[destroy] destroying pool: killing %d processes", len(m.procs))
	for sid, pipe := range m.pipes {
		pipe.Close()
		delete(m.pipes, sid)
	}
	for sid, proc := range m.procs {
		log.Printf("[destroy] killing session %s (pid=%d)", sid, proc.PID())
		proc.Kill()
		proc.Close()
		delete(m.procs, sid)
		if s := m.sessions[sid]; s != nil {
			delete(m.pidToSID, s.PID)
			if s.IsLive() {
				s.Status = StatusOffloaded
			}
			s.PID = 0
			s.PendingInput = ""
		}
	}

	m.savePoolState()
	m.initialized = false
	log.Printf("[destroy] pool destroyed")
	m.mu.Unlock()

	select {
	case <-m.done:
	default:
		close(m.done)
	}

	return api.OkResponse(id)
}

// --- Subscribe ---

func (m *Manager) handleSubscribe(conn net.Conn, req api.Msg) {
	if existing := m.hub.FindByConn(conn); existing != nil {
		log.Printf("[subscribe] re-subscribe from %s", conn.RemoteAddr())
		existing.UpdateFilters(req) // clears pending buffer, commits
		return
	}

	connectedAt := time.Now()
	if m.connAcceptedAt != nil {
		connectedAt = m.connAcceptedAt(conn)
	}

	log.Printf("[subscribe] new subscriber from %s", conn.RemoteAddr())
	sub := api.NewSubscriber(conn, req, connectedAt)
	m.hub.Add(sub)
	// Commit after a short delay to allow a potential re-subscribe to
	// clear the buffer and update filters before events are flushed.
	api.CommitAfter(sub, 10*time.Millisecond)
}

// --- Debug commands ---

// handleDebugSlots shows slot states and slot↔session mappings.
func (m *Manager) handleDebugSlots(id any) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	slots := make([]any, 0)
	for sid, proc := range m.procs {
		s := m.sessions[sid]
		slot := api.Msg{
			"sessionId": sid,
			"pid":       float64(proc.PID()),
			"pidAlive":  isPidAlive(proc.PID()),
		}
		if s != nil {
			slot["status"] = s.Status
			slot["claudeUUID"] = s.ClaudeUUID
		}
		slots = append(slots, slot)
	}

	return api.Response(id, "debug-slots", api.Msg{"slots": slots})
}

// handleDebugCapture captures raw terminal buffer from a slot by index.
func (m *Manager) handleDebugCapture(id any, req api.Msg) api.Msg {
	slotIdx, ok := req["slot"].(float64)
	if !ok {
		return api.ErrorResponse(id, "slot is required")
	}
	raw, _ := req["raw"].(bool)

	m.mu.Lock()
	defer m.mu.Unlock()

	idx := int(slotIdx)
	i := 0
	for _, proc := range m.procs {
		if i == idx {
			content := string(proc.Buffer())
			if !raw {
				content = stripANSI(content)
			}
			return api.Response(id, "result", api.Msg{"content": content})
		}
		i++
	}

	return api.ErrorResponse(id, fmt.Sprintf("slot %d not found", idx))
}

// handleDebugLogs tails the daemon log.
func (m *Manager) handleDebugLogs(id any, req api.Msg) api.Msg {
	// Logs are written to stdout/stderr by the standard log package.
	// A full implementation would read from a log file. For now, return
	// a message indicating the logs are on stderr.
	return api.Response(id, "result", api.Msg{
		"content": "logs are written to daemon stderr (use --follow with process output)",
	})
}
