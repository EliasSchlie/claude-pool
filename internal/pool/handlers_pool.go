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

	if err := m.deployHooks(); err != nil {
		m.initialized = false
		log.Printf("[init] %v", err)
		return api.ErrorResponse(id, err.Error())
	}

	// Create slots
	m.slots = make([]*Slot, size)
	for i := 0; i < size; i++ {
		m.slots[i] = &Slot{Index: i, State: SlotCrashed}
	}

	// Try to restore sessions from pool.json
	var liveSessions, offloadedSessions []*Session
	if !noRestore {
		liveSessions, offloadedSessions = m.loadPoolState()
	}

	// Bind sessions to slots (but don't spawn yet).
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
		sl := m.slots[restored]
		m.bindSession(sl, s)
		s.Status = StatusProcessing
		if s.ClaudeUUID != "" {
			s.PendingResume = s.ClaudeUUID
		}
		log.Printf("[init] restoring live session %s into slot %d (resume=%v)", s.ID, sl.Index, s.PendingResume != "")
		restored++
	}

	for _, s := range offloadedSessions {
		if restored >= size {
			log.Printf("[init] session %s exceeds pool size, keeping offloaded", s.ID)
			s.Status = StatusOffloaded
			m.sessions[s.ID] = s
			continue
		}
		m.sessions[s.ID] = s
		sl := m.slots[restored]
		m.bindSession(sl, s)
		s.Status = StatusProcessing
		if s.ClaudeUUID != "" {
			s.PendingResume = s.ClaudeUUID
		}
		log.Printf("[init] restoring offloaded session %s into slot %d (resume=%v)", s.ID, sl.Index, s.PendingResume != "")
		restored++
	}

	remaining := size - restored
	if remaining > 0 {
		log.Printf("[init] spawning %d fresh slots", remaining)
	}

	// Spawn slot 0 first and wait for it to become ready before spawning
	// the rest. This ensures the first Claude process initializes its
	// plugin cache, accepts the workspace trust prompt, etc. without
	// racing against other instances. Without this, concurrent Claude
	// startups cause plugin hooks to fail for most sessions.
	m.spawnSlot(m.slots[0])

	if size > 1 {
		m.mu.Unlock()
		deadline := time.After(60 * time.Second)
	waitLoop:
		for {
			m.mu.Lock()
			sl := m.slots[0]
			if sl.State == SlotFresh || sl.State == SlotIdle {
				m.mu.Unlock()
				break
			}
			ch := m.statusNotify
			m.mu.Unlock()
			select {
			case <-deadline:
				log.Printf("[init] first slot timed out waiting for ready")
				break waitLoop
			case <-ch:
			}
		}
		m.mu.Lock()
		log.Printf("[init] first slot ready, spawning %d more", size-1)

		for i := 1; i < size; i++ {
			m.spawnSlot(m.slots[i])
		}
	}

	log.Printf("[init] pool initialized: %d slots", size)
	m.savePoolState()
	m.startTypingPoller()
	m.startMaintenanceLoop()

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

// buildHealthResponse constructs the health/pool response.
// Must be called with m.mu held.
func (m *Manager) buildHealthResponse(id any) api.Msg {
	slots := map[string]float64{
		"fresh": 0, "spawning": 0, "resuming": 0, "clearing": 0,
		"idle": 0, "processing": 0, "crashed": 0,
	}
	sessions := map[string]float64{
		"queued": 0, "idle": 0, "processing": 0,
		"offloaded": 0, "error": 0, "archived": 0,
	}

	// Count slot states directly
	for _, sl := range m.slots {
		slots[sl.State]++
	}

	// Count session states
	for _, s := range m.sessions {
		sessions[s.Status]++
	}

	health := api.Msg{
		"name":       m.poolName,
		"size":       float64(len(m.slots)),
		"queueDepth": float64(len(m.queue)),
		"slots":      slots,
		"sessions":   sessions,
	}
	if cfg, err := m.config.Load(); err == nil {
		health["config"] = configToMsg(cfg)
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

	log.Printf("[destroy] destroying pool: killing %d slots", len(m.slots))
	for _, sl := range m.slots {
		if s := m.sessions[sl.SessionID]; s != nil {
			if s.IsLive() {
				s.Status = StatusOffloaded
			}
			s.SlotIndex = -1
			s.PendingInput = ""
		}
		sl.SessionID = ""
		log.Printf("[destroy] killing slot %d (pid=%d)", sl.Index, sl.PID())
		sl.cleanup(m)
		sl.State = SlotCrashed
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
