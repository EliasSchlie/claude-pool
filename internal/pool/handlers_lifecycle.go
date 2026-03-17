package pool

import (
	"log"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

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

	s := m.resolveSession(sessionID)
	if s == nil {
		m.mu.Unlock()
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}

	if s.Status == StatusArchived {
		log.Printf("[archive] session %s: already archived", s.ID)
		m.mu.Unlock()
		return api.OkResponse(id)
	}

	log.Printf("[archive] session %s: archiving (status=%s recursive=%v)", s.ID, s.Status, recursive)

	if !recursive {
		for _, other := range m.sessions {
			if other.ParentID == s.ID && other.Status != StatusArchived {
				log.Printf("[archive] session %s: rejected, has unarchived child %s", s.ID, other.ID)
				m.mu.Unlock()
				return api.ErrorResponse(id, "session has unarchived children; use recursive: true")
			}
		}
	} else {
		m.archiveDescendants(s.ID)
	}

	// Re-fetch: archiveDescendants releases and re-acquires the lock,
	// so the session's status may have changed.
	s = m.sessions[s.ID]
	if s == nil {
		m.mu.Unlock()
		return api.ErrorResponse(id, "session died during descendant archive")
	}
	if s.Status == StatusArchived {
		m.mu.Unlock()
		return api.OkResponse(id)
	}

	// If processing, stop first (Ctrl-C → wait for idle) before offloading.
	// /clear only works on an idle Claude — can't send it while processing.
	if s.Status == StatusProcessing {
		log.Printf("[archive] session %s: stopping processing session (pid=%d)", s.ID, s.PID)
		sid := s.ID
		m.mu.Unlock()
		m.stopProcessingSession(sid, 30*time.Second)
		m.mu.Lock()
		s = m.sessions[sid]
		if s == nil {
			m.mu.Unlock()
			return api.ErrorResponse(id, "session died during archive stop")
		}
	}

	m.archiveSessionLocked(s)
	m.savePoolState()
	m.mu.Unlock()
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
				m.tryDrainQueue()
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

	// Metadata — route structured fields (name, description) to their
	// dedicated fields, everything else to Tags. Matches handleSetMetadata
	// semantics so `set --meta name=foo` and `set-metadata {name: "foo"}`
	// produce the same result.
	if metadata, ok := req["metadata"].(map[string]any); ok {
		for k, v := range metadata {
			sv, _ := v.(string)
			switch k {
			case "name":
				s.Metadata.Name = sv
			case "description":
				s.Metadata.Description = sv
			default:
				if s.Metadata.Tags == nil {
					s.Metadata.Tags = map[string]string{}
				}
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

		// Try to find a fresh slot, evicting if necessary
		if m.findFreshSlot() == nil {
			if evicted := m.findEvictableSession(); evicted != nil {
				log.Printf("[pin] session %s: evicting idle session %s to free slot", s.ID, evicted.ID)
				m.offloadSessionLocked(evicted)
			}
		}
		if fresh := m.findFreshSlot(); fresh != nil {
			log.Printf("[pin] session %s: taking over slot from %s (status=%s pid=%d)", s.ID, fresh.ID, fresh.Status, fresh.PID)
			proc := m.transferProcess(fresh, s)
			if fresh.Status == StatusIdle {
				s.Status = StatusIdle
			} else {
				s.Status = StatusFresh
			}
			if proc != nil {
				// Only clear for idle slots — fresh slots' signals haven't fired yet
				if fresh.Status == StatusIdle {
					m.clearIdleSignals(s.PID)
				}
				m.startWatchers(s, proc)
			}
			delete(m.sessions, fresh.ID)
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
	m.tryDrainQueue()
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

	// SPEC: "Change slot count immediately and update config."
	if _, err := m.config.Update(map[string]any{"size": target}); err != nil {
		log.Printf("[resize] config update failed: %v", err)
	}

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
