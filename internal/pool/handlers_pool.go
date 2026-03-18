package pool

import (
	"log"
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
	// "crashed" is always 0 in practice (watchProcessDone → tryReplaceDeadSessions
	// recycles immediately), but the spec requires the key to be present.
	slots := map[string]float64{
		"fresh": 0, "spawning": 0, "resuming": 0, "clearing": 0,
		"idle": 0, "processing": 0, "crashed": 0,
	}
	// SPEC: sessions — counts by session state (all sessions).
	// Pool Object lists queued/idle/processing/offloaded/archived; error is
	// also a valid session state (Session States table) — include for consistency.
	sessions := map[string]float64{
		"queued": 0, "idle": 0, "processing": 0,
		"offloaded": 0, "error": 0, "archived": 0,
	}

	for _, s := range m.sessions {
		// Count session states (all sessions including archived)
		sessions[s.ExternalStatus()]++

		// Count slot states (only live sessions occupy slots)
		if slotState := s.SlotState(); slotState != "" {
			slots[slotState]++
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
