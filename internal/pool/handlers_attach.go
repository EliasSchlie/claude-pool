package pool

import (
	"fmt"
	"log"
	"time"

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

	sid := s.ID
	pipe.onInput = func(data []byte) {
		m.handleAttachInput(sid, data)
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

// inputClass holds classified raw input bytes.
type inputClass struct {
	hasCtrlU  bool
	hasEnter  bool
	printable []byte
}

// classifyInput categorizes raw bytes into control signals and printable text.
func classifyInput(data []byte) inputClass {
	var ic inputClass
	for _, b := range data {
		switch {
		case b == 0x15: // Ctrl-U
			ic.hasCtrlU = true
		case b == '\r' || b == '\n': // Enter
			ic.hasEnter = true
		case b >= 0x20 && b != 0x7f: // printable (not DEL)
			ic.printable = append(ic.printable, b)
		}
	}
	return ic
}

// trackInput updates pendingInput based on raw bytes written to a session's PTY.
// Caller must hold m.mu. Used by handleInput (debug input) and handleAttachInput.
func (m *Manager) trackInput(s *Session, data []byte) {
	ic := classifyInput(data)

	switch {
	case ic.hasCtrlU:
		if s.PendingInput != "" {
			s.PendingInput = ""
			s.LastUsedAt = time.Now()
			delete(m.attachTyping, s.ID)
			m.broadcastEvent(api.Msg{
				"type": "event", "event": "updated",
				"sessionId": s.ID, "changes": api.Msg{"pendingInput": ""},
			})
		}
	case len(ic.printable) > 0 && (s.Status == StatusIdle || s.Status == StatusFresh):
		m.attachTyping[s.ID] = append(m.attachTyping[s.ID], ic.printable...)
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

	ic := classifyInput(data)

	switch {
	case ic.hasCtrlU && s.PendingInput != "":
		s.PendingInput = ""
		s.LastUsedAt = time.Now()
		delete(m.attachTyping, sessionID)
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"pendingInput": ""},
		})

	case ic.hasEnter && (s.PendingInput != "" || ((s.Status == StatusIdle || s.Status == StatusFresh) && len(ic.printable) > 0)):
		// Submit prompt
		buf := m.attachTyping[sessionID]
		buf = append(buf, ic.printable...)
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

	case len(ic.printable) > 0 && (s.Status == StatusIdle || s.Status == StatusFresh):
		m.attachTyping[sessionID] = append(m.attachTyping[sessionID], ic.printable...)
		s.PendingInput = string(m.attachTyping[sessionID])
		s.LastUsedAt = time.Now()
		m.broadcastEvent(api.Msg{
			"type": "event", "event": "updated",
			"sessionId": s.ID, "changes": api.Msg{"pendingInput": s.PendingInput},
		})
	}
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
