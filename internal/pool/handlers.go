package pool

import (
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

func verbosityFromReq(req api.Msg, fallback string) string {
	if v, _ := req["verbosity"].(string); v != "" {
		return v
	}
	return fallback
}

// --- Config ---

func (m *Manager) handleConfig(id any, req api.Msg) api.Msg {
	if setMap, ok := req["set"].(map[string]any); ok {
		cfg, err := m.config.Update(setMap)
		if err != nil {
			return api.ErrorResponse(id, "config update failed: "+err.Error())
		}
		// If keepFresh was updated, trigger fresh slot maintenance
		if _, ok := setMap["keepFresh"]; ok {
			m.mu.Lock()
			if m.initialized {
				m.maintainFreshSlots()
			}
			m.mu.Unlock()
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
		restored++
	}

	// Fill remaining slots with fresh pre-warmed sessions.
	// Spawn the first one and wait for it to become idle before spawning the
	// rest. This ensures the workspace trust prompt (if shown) is accepted
	// and cached before concurrent sessions start — Claude's TUI trust
	// prompt doesn't reliably process Enter when multiple PTYs race.
	fresh := size - restored
	if fresh > 0 {
		log.Printf("[init] spawning %d fresh pre-warmed sessions", fresh)
		s := m.newSession("")
		s.Status = StatusFresh
		s.PreWarmed = true
		m.sessions[s.ID] = s
		m.spawnSession(s, false)

		if fresh > 1 {
			// Wait for first session to become idle (trust accepted) before
			// spawning the rest. Release lock for the wait.
			sid := s.ID
			m.mu.Unlock()
			m.waitForSessionIdle(sid, 60*time.Second)
			m.mu.Lock()
			log.Printf("[init] first session ready, spawning %d more", fresh-1)
		}

		for i := 1; i < fresh; i++ {
			s := m.newSession("")
			s.Status = StatusFresh
			s.PreWarmed = true
			m.sessions[s.ID] = s
			m.spawnSession(s, false)
		}
	}

	log.Printf("[init] pool initialized: %d sessions total", restored+fresh)
	m.savePoolState()
	m.startTypingPoller()
	m.startMaintenanceLoop()

	// SPEC: "Pool state after initialization (same as health)."
	return m.buildHealthResponse(id)
}

// --- Health ---

func (m *Manager) handleHealth(id any) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	return m.buildHealthResponse(id)
}

// buildHealthResponse builds the Pool Object (SPEC: Pool Object table).
// Caller must hold m.mu.
func (m *Manager) buildHealthResponse(id any) api.Msg {
	// SPEC: slots — counts by slot state (sum = size).
	// "crashed" is omitted: crashed processes are recycled immediately
	// (watchProcessDone → tryReplaceDeadSessions), so the count is always 0.
	slots := map[string]float64{
		"fresh": 0, "spawning": 0, "resuming": 0, "clearing": 0,
		"idle": 0, "processing": 0,
	}
	// SPEC: sessions — counts by session state (all sessions)
	sessions := map[string]float64{
		"queued": 0, "idle": 0, "processing": 0,
		"offloaded": 0, "archived": 0,
	}

	for _, s := range m.sessions {
		// Count session states (all sessions including archived)
		sessions[s.ExternalStatus()]++

		// Count slot states (only live sessions occupy slots)
		if !s.IsLive() {
			continue
		}
		switch {
		case s.PreWarmed && s.Status == StatusFresh && s.Recycled:
			slots["clearing"]++
		case s.PreWarmed && s.Status == StatusFresh:
			slots["spawning"]++
		case s.PreWarmed && s.Status == StatusIdle:
			slots["fresh"]++
		case s.Status == StatusFresh && s.PendingResume != "":
			slots["resuming"]++
		case s.Status == StatusFresh:
			slots["clearing"]++
		case s.Status == StatusIdle:
			slots["idle"]++
		case s.Status == StatusProcessing:
			slots["processing"]++
		}
	}

	health := api.Msg{
		"name":       m.poolName,
		"size":       float64(m.poolSize),
		"queueDepth": float64(len(m.queue)),
		"slots":      slots,
		"sessions":   sessions,
	}
	if cfg, err := m.config.Load(); err == nil {
		health["config"] = configToMsg(cfg)
	} else {
		log.Printf("[health] config load failed: %v", err)
	}

	return api.Response(id, "health", api.Msg{"health": health})
}

// --- Start ---

func (m *Manager) handleStart(id any, req api.Msg) api.Msg {
	prompt, _ := req["prompt"].(string)
	parentID := parentFromReq(req)

	m.mu.Lock()

	if !m.initialized {
		m.mu.Unlock()
		return api.ErrorResponse(id, "pool not initialized")
	}

	s := m.newSession(parentID)
	s.PendingPrompt = prompt
	if _, hasMetadata := req["metadata"]; hasMetadata {
		s.Metadata = metadataFromMap(req)
	}
	m.sessions[s.ID] = s
	log.Printf("[start] created session %s (parent=%s, prompt=%d chars, name=%q)", s.ID, parentID, len(prompt), s.Metadata.Name)

	// Promptless start: claim a slot and leave the session idle (SPEC).
	// If the slot is still starting (fresh), block until ready so the
	// caller gets an idle session with a discovered ClaudeUUID.
	if prompt == "" {
		m.mu.Unlock()
		return m.handleStartPromptless(id, s)
	}

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
		// Only clear stale signals for idle slots (signal already consumed).
		// For fresh slots, the session-start signal hasn't fired yet —
		// clearing it would race with the hook and lose the signal.
		if !wasFresh {
			m.clearIdleSignals(s.PID)
		}
		m.startWatchers(s, proc)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": StatusProcessing, "parent": s.ParentID,
		})
		m.savePoolState()
		m.mu.Unlock()
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

	resp := api.Response(id, "started", api.Msg{
		"sessionId": s.ID,
		"status":    s.ExternalStatus(),
	})
	m.mu.Unlock()
	return resp
}

// handleStartPromptless creates a session without a prompt — claims a slot and
// leaves it idle. Blocks until the session is ready if the slot was still starting.
// Must be called WITHOUT m.mu held.
func (m *Manager) handleStartPromptless(id any, s *Session) api.Msg {
	m.mu.Lock()

	if fresh := m.findFreshSlot(); fresh != nil {
		proc := m.transferProcess(fresh, s)
		wasFresh := fresh.Status == StatusFresh
		delete(m.sessions, fresh.ID)

		if wasFresh {
			s.Status = StatusFresh
		} else {
			s.Status = StatusIdle
			m.clearIdleSignals(s.PID)
		}
		m.startWatchers(s, proc)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": s.ExternalStatus(), "parent": s.ParentID,
		})
		m.savePoolState()

		if wasFresh {
			sid := s.ID
			m.mu.Unlock()
			return m.waitForSessionReady(id, sid, 60*time.Second)
		}
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    StatusIdle,
		})
	}

	// No free slot — queue without prompt
	s.Status = StatusQueued
	m.queue = append(m.queue, s)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "created",
		"sessionId": s.ID, "status": s.Status, "parent": s.ParentID,
	})
	m.tryDequeueWithEviction(s, "")
	m.savePoolState()

	// If dequeued into a slot (status changed from queued), wait for ready
	if s.Status != StatusQueued {
		sid := s.ID
		m.mu.Unlock()
		return m.waitForSessionReady(id, sid, 60*time.Second)
	}

	// Still queued — return immediately with queued status
	m.mu.Unlock()
	return api.Response(id, "started", api.Msg{
		"sessionId": s.ID,
		"status":    s.ExternalStatus(),
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
		log.Printf("[followup] session %s: fresh, queuing prompt and waiting for ready", s.ID)
		s.PendingPrompt = prompt
		s.PendingForce = true
		sid := s.ID
		m.mu.Unlock()
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

		// Stop then deliver: Ctrl-C → wait for idle → deliver new prompt.
		m.stopProcessingSession(sid, 30*time.Second)

		m.mu.Lock()
		s = m.sessions[sid]
		if s == nil || !s.IsLive() {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session died during force followup")
		}
		s.Status = StatusProcessing
		s.LastUsedAt = time.Now()
		m.deliverPrompt(s, prompt)
		m.broadcastStatus(s, StatusIdle)
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
	ch := m.statusNotify
	m.mu.Unlock()

	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return api.ErrorResponse(id, "timeout")
		case <-ch:
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
			ch = m.statusNotify
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
	case StatusFresh:
		if s.PendingPrompt == "" {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session is not processing or queued (status: "+s.ExternalStatus()+")")
		}
		// Fresh with PendingPrompt = externally "processing" (prompt queued
		// for delivery on session-start). Cancel the pending prompt.
		log.Printf("[stop] session %s: cancelling pending prompt (fresh, %d chars)", s.ID, len(s.PendingPrompt))
		s.PendingPrompt = ""
		s.PendingForce = false
		m.broadcastStatus(s, StatusProcessing)
		m.savePoolState()
		m.mu.Unlock()
		return api.OkResponse(id)

	case StatusIdle:
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
		m.mu.Unlock()

		// Ctrl-C → wait for idle. The Stop hook fires asynchronously but
		// its deferred signal may not arrive if the transcript size changes
		// during interruption processing. stopProcessingSession handles
		// the full wait with a timeout fallback.
		m.stopProcessingSession(sid, 30*time.Second)
		return api.OkResponse(id)

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot stop session in state: "+s.Status)
	}
}

// parseCaptureParams extracts source/turns/detail from a request with defaults.
func parseCaptureParams(req api.Msg) (source string, turns int, detail string) {
	if format, ok := req["format"].(string); ok && format != "" {
		switch format {
		case "jsonl-short", "jsonl-last":
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

	if s.Cwd == "" && s.IsLive() && s.PID > 0 {
		if cwd := getCwd(s.PID); cwd != "" {
			s.Cwd = cwd
		}
	}

	msg := s.ToMsgWithChildren(m.sessions, verbosityFromReq(req, VerbosityFull))
	return api.Response(id, "session", api.Msg{"session": msg})
}

// --- Ls ---

func (m *Manager) handleLs(id any, req api.Msg) api.Msg {
	all, _ := req["all"].(bool)
	tree, _ := req["tree"].(bool)
	showArchived, _ := req["archived"].(bool)
	callerId, _ := req["callerId"].(string)

	verbosity := verbosityFromReq(req, VerbosityFlat)

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
		// Pre-warmed sessions are slot infrastructure, not user sessions.
		// Sessions don't exist until start creates them (SPEC invariant #5).
		if s.PreWarmed {
			continue
		}
		if s.Status == StatusArchived && !showArchived {
			continue
		}
		if statusFilter != nil && !statusFilter[s.ExternalStatus()] {
			continue
		}

		if callerId != "" && !all {
			if s.ParentID != callerId {
				continue
			}
		}

		// SPEC: "Only filters the top level — if a session appears as a child
		// of another session, it's not repeated as a separate entry."
		// Applied to default ls only. When explicit filters are active (--status,
		// --archived, --parent), show all matching sessions without dedup.
		if callerId == "" && !all && statusFilter == nil && !showArchived {
			if s.ParentID != "" {
				if _, hasParent := m.sessions[s.ParentID]; hasParent {
					continue
				}
			}
		}

		if tree {
			results = append(results, s.ToMsgWithChildren(m.sessions, verbosity))
		} else {
			results = append(results, s.ToMsg(verbosity))
		}
	}

	return api.Response(id, "sessions", api.Msg{"sessions": results})
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
		existing.UpdateFilters(req)
		return
	}

	connectedAt := time.Now()
	if m.connAcceptedAt != nil {
		connectedAt = m.connAcceptedAt(conn)
	}

	log.Printf("[subscribe] new subscriber from %s", conn.RemoteAddr())
	sub := api.NewSubscriber(conn, req, connectedAt)
	m.hub.Add(sub)
	api.CommitAfter(sub, 10*time.Millisecond)
}
