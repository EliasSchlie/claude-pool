package pool

import (
	"log"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

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

	if prompt == "" {
		m.mu.Unlock()
		return m.handleStartPromptless(id, s)
	}

	// Try to claim a fresh slot
	if sl := m.findFreshSlot(); sl != nil {
		log.Printf("[start] session %s taking slot %d (state=%s, pid=%d)", s.ID, sl.Index, sl.State, sl.PID())
		m.bindSession(sl, s)

		if sl.State == SlotFresh {
			// Slot ready — deliver immediately
			s.Status = StatusProcessing
			sl.State = SlotProcessing
			s.LastUsedAt = time.Now()
			m.clearIdleSignals(sl.PID())
			m.deliverSlotPrompt(sl, prompt, 200*time.Millisecond)
			s.PendingPrompt = ""
		} else {
			// Slot still clearing — queue prompt for delivery when ready
			log.Printf("[start] session %s: slot %d still %s, queuing prompt", s.ID, sl.Index, sl.State)
			s.Status = StatusProcessing
			s.PendingPrompt = prompt
			s.PendingForce = true
		}

		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": s.ExternalStatus(), "parent": s.ParentID,
		})
		m.savePoolState()
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.ExternalStatus(),
		})
	}

	// No fresh slot — queue
	log.Printf("[start] session %s: no fresh slots, queuing (queue depth=%d)", s.ID, len(m.queue)+1)
	s.Status = StatusQueued
	m.queue = append(m.queue, s)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "created",
		"sessionId": s.ID, "status": s.ExternalStatus(), "parent": s.ParentID,
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

func (m *Manager) handleStartPromptless(id any, s *Session) api.Msg {
	m.mu.Lock()

	if sl := m.findFreshSlot(); sl != nil {
		m.bindSession(sl, s)

		if sl.State == SlotFresh {
			s.Status = StatusIdle
			sl.State = SlotIdle
			m.clearIdleSignals(sl.PID())
		} else {
			// Slot still clearing — will transition to idle when ready
			s.Status = StatusProcessing
		}

		m.broadcastEvent(api.Msg{
			"type": "event", "event": "created",
			"sessionId": s.ID, "status": s.ExternalStatus(), "parent": s.ParentID,
		})
		m.savePoolState()

		if s.Status != StatusIdle {
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

	// No free slot — queue
	s.Status = StatusQueued
	m.queue = append(m.queue, s)
	m.broadcastEvent(api.Msg{
		"type": "event", "event": "created",
		"sessionId": s.ID, "status": s.ExternalStatus(), "parent": s.ParentID,
	})
	m.tryDequeueWithEviction(s, "")
	m.savePoolState()

	if s.Status != StatusQueued {
		sid := s.ID
		m.mu.Unlock()
		return m.waitForSessionReady(id, sid, 60*time.Second)
	}

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
	case StatusIdle:
		sl := m.slotForSession(s)
		if sl == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session has no slot")
		}
		log.Printf("[followup] session %s: idle → processing, delivering prompt", s.ID)
		s.Status = StatusProcessing
		sl.State = SlotProcessing
		s.LastUsedAt = time.Now()
		m.deliverSlotPrompt(sl, prompt, 200*time.Millisecond)
		m.broadcastStatus(s, StatusIdle)
		m.savePoolState()
		m.mu.Unlock()
		return api.Response(id, "started", api.Msg{
			"sessionId": s.ID,
			"status":    s.ExternalStatus(),
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
			"status":    s.ExternalStatus(),
		})

	case StatusProcessing:
		if !force {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session is processing; use force: true to override")
		}
		sl := m.slotForSession(s)
		if sl == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session has no slot")
		}
		log.Printf("[followup] session %s: force-interrupting, sending Ctrl-C (pid=%d)", s.ID, sl.PID())
		s.LastUsedAt = time.Now()
		sid := s.ID
		m.savePoolState()
		m.mu.Unlock()

		m.stopProcessingSession(sid, 30*time.Second)

		m.mu.Lock()
		s = m.sessions[sid]
		if s == nil || !s.IsLoaded() {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session died during force followup")
		}
		sl = m.slotForSession(s)
		if sl == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session lost its slot during force followup")
		}
		s.Status = StatusProcessing
		sl.State = SlotProcessing
		s.LastUsedAt = time.Now()
		m.deliverSlotPrompt(sl, prompt, 200*time.Millisecond)
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
			"status":    s.ExternalStatus(),
		})

	case StatusArchived:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is archived; unarchive first")

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot followup in state: "+s.ExternalStatus())
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
		var busySession *Session
		for _, s := range m.sessions {
			if !s.IsLoaded() && s.Status != StatusQueued {
				continue
			}
			if s.Status == StatusProcessing || s.Status == StatusQueued {
				if s.ParentID != parentFilter {
					continue
				}
				if busySession == nil || s.CreatedAt.After(busySession.CreatedAt) {
					busySession = s
				}
			}
		}
		if busySession == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "no busy sessions")
		}
		sessionID = busySession.ID
	}

	s := m.resolveSession(sessionID)
	if s == nil {
		m.mu.Unlock()
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	captureAndReturn := func(s *Session) api.Msg {
		if source == "buffer" && !s.IsLoaded() {
			m.mu.Unlock()
			return api.ErrorResponse(id, "buffer source requires live terminal")
		}
		content := m.captureOutput(s, source, turns, detail)
		m.mu.Unlock()
		return api.Response(id, "result", api.Msg{
			"sessionId": s.ID,
			"content":   content,
		})
	}

	switch s.Status {
	case StatusIdle, StatusOffloaded:
		return captureAndReturn(s)
	case StatusArchived:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is archived")
	case StatusError:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session has error")
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
			case StatusIdle, StatusOffloaded:
				return captureAndReturn(s)
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
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	log.Printf("[stop] session %s: status=%s", s.ID, s.Status)

	switch s.Status {
	case StatusProcessing:
		if !s.IsLoaded() {
			// Processing but pending prompt hasn't been delivered yet
			s.ClearPending()
			s.Status = StatusIdle
			m.broadcastStatus(s, StatusProcessing)
			m.savePoolState()
			m.mu.Unlock()
			return api.OkResponse(id)
		}
		sl := m.slotForSession(s)
		if sl == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session has no slot")
		}
		log.Printf("[stop] session %s: sending Ctrl-C to slot %d pid %d", s.ID, sl.Index, sl.PID())
		sid := s.ID
		m.mu.Unlock()
		m.stopProcessingSession(sid, 30*time.Second)
		return api.OkResponse(id)

	case StatusIdle:
		m.mu.Unlock()
		return api.ErrorResponse(id, "session is not processing or queued (status: "+s.ExternalStatus()+")")

	case StatusQueued:
		log.Printf("[stop] session %s: removing from queue", s.ID)
		m.removeFromQueue(s)
		s.Status = StatusOffloaded
		m.savePoolState()
		m.mu.Unlock()
		return api.OkResponse(id)

	default:
		m.mu.Unlock()
		return api.ErrorResponse(id, "cannot stop session in state: "+s.ExternalStatus())
	}
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

	if source == "buffer" && !s.IsLoaded() {
		return api.ErrorResponse(id, "buffer source requires live terminal")
	}

	content := m.captureOutput(s, source, turns, detail)
	return api.Response(id, "result", api.Msg{
		"sessionId": s.ID,
		"content":   content,
	})
}
