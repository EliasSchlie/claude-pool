package pool

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

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

	// Deploy hooks to pool directory
	m.deployHooks()

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
		status := s.Status
		if status == StatusFresh {
			status = StatusIdle
		}
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
	parentID, _ := req["parentId"].(string)

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	s := m.newSession(parentID)
	s.PendingPrompt = prompt
	m.sessions[s.ID] = s
	log.Printf("[start] created session %s (parent=%s, prompt=%d chars)", s.ID, parentID, len(prompt))

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
			"sessionId": s.ID, "status": StatusProcessing, "parentId": s.ParentID,
		})
		m.savePoolState()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    StatusProcessing,
		})
	}

	// Try to evict an idle session with strictly lower priority
	if evicted := m.findEvictableSessionBelow(s.Priority); evicted != nil {
		log.Printf("[start] session %s: evicting idle session %s (priority=%.1f) to free slot", s.ID, evicted.ID, evicted.Priority)
		m.offloadSessionLocked(evicted)
		s.Status = StatusFresh
		m.spawnSession(s, false)
		s.PendingPrompt = prompt
		m.deliverPromptWhenReady(s)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": StatusProcessing, "parentId": s.ParentID,
		})
		m.savePoolState()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    StatusProcessing,
		})
	}

	// Queue it
	log.Printf("[start] session %s: no slots available, queuing (queue depth=%d)", s.ID, len(m.queue)+1)
	s.Status = StatusQueued
	m.queue = append(m.queue, s)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "created",
		"sessionId": s.ID, "status": s.Status, "parentId": s.ParentID,
	})
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

	case StatusOffloaded, StatusDead, StatusError:
		log.Printf("[followup] session %s: %s → queued for respawn", s.ID, s.Status)
		s.PendingPrompt = prompt
		prevStatus := s.Status
		s.Status = StatusQueued
		m.queue = append(m.queue, s)
		m.broadcastStatus(s, prevStatus)
		// Try to free a slot by offloading the lowest-priority idle session.
		// Restored sessions have "earned" their slot from prior use.
		if evicted := m.findEvictableSession(); evicted != nil && evicted.ID != s.ID {
			log.Printf("[followup] session %s: evicting idle session %s (priority=%.1f) to free slot for respawn", s.ID, evicted.ID, evicted.Priority)
			m.offloadSessionLocked(evicted)
		}
		m.tryDequeue()
		m.savePoolState()
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
		})

	case StatusProcessing, StatusTyping:
		if !force {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session is processing; use force: true to override")
		}
		log.Printf("[followup] session %s: force-interrupting %s, sending Ctrl-C (pid=%d)", s.ID, s.Status, s.PID)
		s.PendingPrompt = prompt
		s.PendingForce = true
		pid := s.PID
		if proc := m.procs[s.ID]; proc != nil {
			proc.WriteString("\x03")
		}
		m.savePoolState()
		m.mu.Unlock()
		// Ctrl-C doesn't trigger a hook — manually write idle signal
		// so watchIdleSignal picks up the pending force prompt.
		go func() {
			time.Sleep(500 * time.Millisecond)
			m.writeIdleSignal(pid, "ctrl-c")
		}()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.Status,
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
	timeoutMs := 300000.0
	if t, ok := req["timeout"].(float64); ok {
		timeoutMs = t
	}
	format, _ := req["format"].(string)
	if format == "" {
		format = "jsonl-short"
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond

	m.mu.Lock()

	if sessionID == "" {
		// Pick the most recently created busy user session (not pre-warmed)
		var busySession *Session
		for _, s := range m.sessions {
			if s.PreWarmed {
				continue
			}
			if s.Status == StatusProcessing || s.Status == StatusQueued || s.Status == StatusFresh {
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
	case StatusDead:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session died")
	case StatusError:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session has error")
	}

	if s.Status == StatusIdle {
		content := m.captureContent(s, format)
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
				content := m.captureContent(s, format)
				m.mu.Unlock()
				return api.Response(id, "result", api.Msg{
					"sessionId": sid,
					"content":   content,
				})
			case StatusDead:
				m.mu.Unlock()
				return api.ErrorResponse(id, "session died")
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
	case StatusIdle, StatusTyping, StatusFresh:
		log.Printf("[stop] session %s: already %s, nothing to stop", s.ID, s.Status)
		m.mu.Unlock()
		return api.OkResponse(id)

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
		proc := m.procs[s.ID]
		if proc != nil {
			proc.WriteString("\x03")
		}
		// Ctrl-C doesn't trigger a Claude hook — manually transition to idle.
		// Give Claude a moment to process the interrupt, then write a fresh
		// idle signal so watchIdleSignal picks it up.
		pid := s.PID
		m.mu.Unlock()

		time.Sleep(500 * time.Millisecond)
		m.writeIdleSignal(pid, "ctrl-c")

		// Wait for watchIdleSignal to pick up the signal and transition status
		deadline := time.After(10 * time.Second)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline:
				return api.ErrorResponse(id, "stop timeout")
			case <-ticker.C:
				m.mu.Lock()
				s := m.sessions[sessionID]
				if s == nil || s.Status == StatusIdle || s.Status == StatusDead {
					m.mu.Unlock()
					return api.OkResponse(id)
				}
				m.mu.Unlock()
			}
		}

	case StatusOffloaded, StatusDead, StatusError, StatusArchived:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot stop session in state: "+s.Status)

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot stop session in state: "+s.Status)
	}
}

// --- Capture ---

func (m *Manager) handleCapture(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	format, _ := req["format"].(string)
	if format == "" {
		format = "jsonl-short"
	}

	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status == StatusQueued {
		return api.ErrorResponse(id, "session is queued (no output yet)")
	}

	if strings.HasPrefix(format, "buffer-") && !s.IsLive() {
		return api.ErrorResponse(id, "buffer format requires live terminal")
	}

	content := m.captureContent(s, format)
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
		if !all {
			// TODO: ownership filtering
		}

		if tree {
			if s.ParentID != "" {
				if _, hasParent := m.sessions[s.ParentID]; hasParent {
					continue
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

	if s.Pinned {
		log.Printf("[offload] session %s: rejected, session is pinned", s.ID)
		return api.ErrorResponse(id, "cannot offload pinned session; unpin first")
	}

	if s.Status != StatusIdle && s.Status != StatusTyping {
		log.Printf("[offload] session %s: rejected, status=%s (need idle or typing)", s.ID, s.Status)
		return api.ErrorResponse(id, "can only offload idle sessions (current: "+s.Status+")")
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
	} else {
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

// --- Pin ---

func (m *Manager) handlePin(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	parentID, _ := req["parentId"].(string)
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

	if s.Status == StatusOffloaded || s.Status == StatusDead || s.Status == StatusError {
		prevStatus := s.Status
		s.Status = StatusQueued
		m.queue = append([]*Session{s}, m.queue...)
		m.broadcastStatus(s, prevStatus)
		// Evict an idle session if needed to free a slot for priority load
		if evicted := m.findEvictableSession(); evicted != nil && evicted.ID != s.ID {
			log.Printf("[pin] session %s: evicting idle session %s to free slot for reload", s.ID, evicted.ID)
			m.offloadSessionLocked(evicted)
		}
		m.tryDequeue()
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
		"status":    s.Status,
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
	if target < 0 {
		return api.ErrorResponse(id, "size must be >= 0")
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
		m.killTokens += oldSize - target
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

// handleAttachInput processes raw input from attach pipe clients for typing detection.
func (m *Manager) handleAttachInput(sessionID string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.sessions[sessionID]
	if s == nil {
		return
	}

	// Classify input
	hasCtrlU := false
	hasPrintable := false
	for _, b := range data {
		if b == 0x15 { // Ctrl-U
			hasCtrlU = true
		} else if b >= 0x20 && b != 0x7f { // printable (not DEL)
			hasPrintable = true
		}
	}

	switch {
	case hasCtrlU && s.Status == StatusTyping:
		s.Status = StatusIdle
		m.broadcastStatus(s, StatusTyping)
	case hasPrintable && (s.Status == StatusIdle || s.Status == StatusFresh):
		prev := s.Status
		s.Status = StatusTyping
		m.broadcastStatus(s, prev)
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
	log.Printf("[subscribe] new subscriber from %s", conn.RemoteAddr())
	sub := api.NewSubscriber(conn, req)
	m.hub.Add(sub)

	go func() {
		defer m.hub.Remove(sub)
		scanner := json.NewDecoder(conn)
		for {
			var msg api.Msg
			if err := scanner.Decode(&msg); err != nil {
				return
			}
			if t, _ := msg["type"].(string); t == "subscribe" {
				sub.UpdateFilters(msg)
			}
		}
	}()
}
