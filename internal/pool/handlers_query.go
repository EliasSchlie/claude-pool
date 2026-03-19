package pool

import (
	"log"
	"net"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

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

	if s.Cwd == "" && s.IsLoaded() {
		if sl := m.slotForSession(s); sl != nil {
			if cwd := getCwd(sl.PID()); cwd != "" {
				s.Cwd = cwd
			}
		}
	}

	msg := s.ToMsgWithChildren(m.sessions, verbosityFromReq(req, VerbosityFull), m.sessionPID)
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

	pidFn := m.sessionPID
	results := make([]any, 0)
	for _, s := range m.sessions {
		if s.Status == StatusArchived && !showArchived {
			continue
		}
		if statusFilter != nil && !statusFilter[s.Status] {
			continue
		}

		if callerId != "" && !all {
			if s.ParentID != callerId {
				continue
			}
		}

		// SPEC: "if a session appears as a child of another session,
		// it's not repeated as a separate entry."
		// In tree mode, always dedup. In flat mode, dedup when showing all
		// sessions (no caller filter) without explicit status/archived filters.
		dedup := tree || (callerId == "" && !all && statusFilter == nil && !showArchived)
		if dedup && s.ParentID != "" {
			if m.findParentSession(s) != nil {
				continue
			}
		}

		if tree {
			results = append(results, s.ToMsgWithChildren(m.sessions, verbosity, pidFn))
		} else {
			results = append(results, s.ToMsg(verbosity, pidFn(s.ID)))
		}
	}

	return api.Response(id, "sessions", api.Msg{"sessions": results})
}

func (m *Manager) findParentSession(s *Session) *Session {
	if s.ParentID == "" {
		return nil
	}
	if parent, ok := m.sessions[s.ParentID]; ok {
		return parent
	}
	for _, other := range m.sessions {
		if s.IsChildOf(other) {
			return other
		}
	}
	return nil
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
