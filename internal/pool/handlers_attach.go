package pool

import (
	"fmt"
	"log"
	"os"

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

	sl := m.slotForSession(s)
	if sl == nil || sl.Process == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	if err := sl.Process.WriteString(data); err != nil {
		log.Printf("[input] session %s: write error: %v", s.ID, err)
		return api.ErrorResponse(id, "write error: "+err.Error())
	}

	log.Printf("[input] session %s: wrote %d bytes", s.ID, len(data))
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

	sl := m.slotForSession(s)
	if sl == nil || sl.Process == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	// Reuse existing pipe if still open
	if sl.Pipe != nil {
		log.Printf("[attach] session %s: reusing existing pipe at %s", s.ID, sl.Pipe.socketPath)
		resp := api.Msg{"socketPath": sl.Pipe.socketPath}
		if cols, rows, err := sl.Process.GetSize(); err == nil {
			resp["cols"] = float64(cols)
			resp["rows"] = float64(rows)
		}
		return api.Response(id, "attached", resp)
	}

	pipe, err := newAttachPipe(s.ID, m.paths.Root, sl.Process)
	if err != nil {
		log.Printf("[attach] session %s: failed to create pipe: %v", s.ID, err)
		return api.ErrorResponse(id, "failed to create attach pipe: "+err.Error())
	}

	pipe.onInput = func() { m.triggerBufferPoll() }

	sl.Pipe = pipe
	log.Printf("[attach] session %s: pipe created at %s", s.ID, pipe.socketPath)
	resp := api.Msg{"socketPath": pipe.socketPath}
	if cols, rows, err := sl.Process.GetSize(); err == nil {
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
		return api.ErrorResponse(id, "session is not live (status: "+s.Status+")")
	}

	sl := m.slotForSession(s)
	if sl == nil || sl.Process == nil {
		return api.ErrorResponse(id, "no process for session")
	}

	if err := sl.Process.SetSize(uint16(cols), uint16(rows)); err != nil {
		log.Printf("[pty-resize] session %s: error: %v", s.ID, err)
		return api.ErrorResponse(id, "resize failed: "+err.Error())
	}

	log.Printf("[pty-resize] session %s: %dx%d", s.ID, int(cols), int(rows))
	return api.OkResponse(id)
}

// --- Debug commands ---

func (m *Manager) handleDebugSlots(id any) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]any, 0, len(m.slots))
	for _, sl := range m.slots {
		slot := api.Msg{
			"index":     float64(sl.Index),
			"state":     sl.State,
			"sessionId": sl.SessionID,
		}
		if sl.Process != nil {
			slot["pid"] = float64(sl.PID())
			slot["pidAlive"] = isPidAlive(sl.PID())
		}
		if s := m.sessions[sl.SessionID]; s != nil {
			slot["claudeUUID"] = s.ClaudeUUID
			slot["sessionStatus"] = s.Status
		}
		result = append(result, slot)
	}

	return api.Response(id, "debug-slots", api.Msg{"slots": result})
}

func (m *Manager) handleDebugCapture(id any, req api.Msg) api.Msg {
	slotIdx, ok := req["slot"].(float64)
	if !ok {
		return api.ErrorResponse(id, "slot is required")
	}
	raw, _ := req["raw"].(bool)

	m.mu.Lock()
	defer m.mu.Unlock()

	idx := int(slotIdx)
	if idx < 0 || idx >= len(m.slots) {
		return api.ErrorResponse(id, fmt.Sprintf("slot %d not found", idx))
	}

	sl := m.slots[idx]
	if sl.Process == nil {
		return api.ErrorResponse(id, fmt.Sprintf("slot %d has no process", idx))
	}

	content := string(sl.Process.Buffer())
	if !raw {
		content = stripANSI(content)
	}
	return api.Response(id, "result", api.Msg{"content": content})
}

func (m *Manager) handleDebugLogs(id any, req api.Msg) api.Msg {
	lines := 50
	if n, ok := req["lines"].(float64); ok && n > 0 {
		lines = int(n)
	}

	logPath := m.paths.DaemonLog()
	content, err := readTail(logPath, lines)
	if err != nil {
		if os.IsNotExist(err) {
			return api.Response(id, "result", api.Msg{"content": ""})
		}
		return api.ErrorResponse(id, "failed to read daemon.log: "+err.Error())
	}
	return api.Response(id, "result", api.Msg{"content": content})
}

// readTail reads the last n lines from a file by seeking from the end.
func readTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := stat.Size()
	if size == 0 {
		return "", nil
	}

	// Read from end in chunks to find enough newlines
	const chunkSize = 8192
	buf := make([]byte, 0, chunkSize)
	offset := size
	newlines := 0

	for offset > 0 && newlines <= n {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, offset); err != nil {
			return "", err
		}
		buf = append(chunk, buf...)
		for _, b := range chunk {
			if b == '\n' {
				newlines++
			}
		}
	}

	return tailLines(string(buf), n), nil
}
