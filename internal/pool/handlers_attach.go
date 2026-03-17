package pool

import (
	"fmt"
	"log"

	"github.com/EliasSchlie/claude-pool/internal/api"
)

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

	// Signal the buffer poller to re-check pendingInput
	m.triggerBufferPoll()

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
		resp := api.Msg{"socketPath": pipe.socketPath}
		if cols, rows, err := proc.GetSize(); err == nil {
			resp["cols"] = float64(cols)
			resp["rows"] = float64(rows)
		}
		return api.Response(id, "attached", resp)
	}

	pipe, err := newAttachPipe(s.ID, m.paths.Root, proc)
	if err != nil {
		log.Printf("[attach] session %s: failed to create pipe: %v", s.ID, err)
		return api.ErrorResponse(id, "failed to create attach pipe: "+err.Error())
	}

	pipe.onInput = func([]byte) {
		m.triggerBufferPoll()
	}

	m.pipes[s.ID] = pipe
	log.Printf("[attach] session %s: pipe created at %s", s.ID, pipe.socketPath)
	resp := api.Msg{"socketPath": pipe.socketPath}
	if cols, rows, err := proc.GetSize(); err == nil {
		resp["cols"] = float64(cols)
		resp["rows"] = float64(rows)
	}
	return api.Response(id, "attached", resp)
}

// --- PTY Resize ---

func (m *Manager) handlePtyResize(id any, req api.Msg) api.Msg {
	sessionID, _ := req["sessionId"].(string)
	if sessionID == "" {
		return api.ErrorResponse(id, "sessionId is required")
	}

	cols, colsOk := req["cols"].(float64)
	rows, rowsOk := req["rows"].(float64)
	if !colsOk || !rowsOk {
		return api.ErrorResponse(id, "cols and rows are required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.resolveSession(sessionID)
	if s == nil {
		return api.ErrorResponse(id, "session not found: "+sessionID)
	}
	if !s.IsLive() {
		return api.ErrorResponse(id, "session is not live (status: "+s.ExternalStatus()+")")
	}

	proc := m.procs[s.ID]
	if proc == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	if err := proc.SetSize(uint16(cols), uint16(rows)); err != nil {
		log.Printf("[pty-resize] session %s: error: %v", s.ID, err)
		return api.ErrorResponse(id, "resize failed: "+err.Error())
	}

	log.Printf("[pty-resize] session %s: %dx%d", s.ID, int(cols), int(rows))
	return api.OkResponse(id)
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
	return api.Response(id, "result", api.Msg{
		"content": "logs are written to daemon stderr (use --follow with process output)",
	})
}
