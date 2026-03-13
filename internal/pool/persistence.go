package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/hooks"
)

func (m *Manager) saveOffloadMeta(s *Session) {
	dir := m.paths.SessionOffloaded(s.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[persist] error creating offload dir %s: %v", dir, err)
		return
	}

	meta := map[string]any{
		"sessionId":  s.ID,
		"claudeUUID": s.ClaudeUUID,
		"parentId":   s.ParentID,
		"priority":   s.Priority,
		"spawnCwd":   s.SpawnCwd,
		"cwd":        s.Cwd,
		"createdAt":  s.CreatedAt.UTC().Format(time.RFC3339),
		"flags":      s.Flags,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Printf("[persist] error marshaling offload meta for %s: %v", s.ID, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(data, '\n'), 0644); err != nil {
		log.Printf("[persist] error writing offload meta for %s: %v", s.ID, err)
	}
}

func (m *Manager) savePoolState() {
	state := map[string]any{
		"size": float64(m.poolSize),
	}

	sessions := make([]map[string]any, 0)
	for _, s := range m.sessions {
		sess := map[string]any{
			"sessionId":  s.ID,
			"claudeUUID": s.ClaudeUUID,
			"status":     s.Status,
			"parentId":   s.ParentID,
			"priority":   s.Priority,
			"spawnCwd":   s.SpawnCwd,
			"cwd":        s.Cwd,
			"createdAt":  s.CreatedAt.UTC().Format(time.RFC3339),
			"flags":      s.Flags,
			"pinned":     s.Pinned,
		}
		sessions = append(sessions, sess)
	}
	state["sessions"] = sessions

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[persist] error marshaling pool state: %v", err)
		return
	}
	if err := os.WriteFile(m.paths.PoolJSON(), append(data, '\n'), 0644); err != nil {
		log.Printf("[persist] error writing pool.json: %v", err)
	}
}

func (m *Manager) loadPoolState() (live, offloaded []*Session) {
	data, err := os.ReadFile(m.paths.PoolJSON())
	if err != nil {
		log.Printf("[persist] no pool.json to restore: %v", err)
		return nil, nil
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[persist] error parsing pool.json: %v", err)
		return nil, nil
	}
	log.Printf("[persist] loaded pool.json successfully")

	rawSessions, _ := state["sessions"].([]any)
	for _, raw := range rawSessions {
		sm, _ := raw.(map[string]any)
		if sm == nil {
			continue
		}

		status, _ := sm["status"].(string)
		s := m.sessionFromMap(sm)
		if s == nil {
			continue
		}

		switch status {
		case StatusIdle, StatusProcessing, StatusFresh:
			live = append(live, s)
		case StatusOffloaded:
			offloaded = append(offloaded, s)
		case StatusArchived:
			s.Status = StatusArchived
			m.sessions[s.ID] = s
		}
	}
	return live, offloaded
}

func (m *Manager) sessionFromMap(sm map[string]any) *Session {
	sid := strVal(sm, "sessionId")
	if sid == "" {
		return nil
	}

	s := &Session{
		ID:         sid,
		ClaudeUUID: strVal(sm, "claudeUUID"),
		ParentID:   strVal(sm, "parentId"),
		Priority:   numVal(sm, "priority"),
		SpawnCwd:   strVal(sm, "spawnCwd"),
		Cwd:        strVal(sm, "cwd"),
		Flags:      strVal(sm, "flags"),
		Pinned:     boolVal(sm, "pinned"),
	}

	if t := strVal(sm, "createdAt"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			s.CreatedAt = parsed
		}
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.SpawnCwd == "" {
		s.SpawnCwd = m.paths.Root
	}
	if s.Cwd == "" {
		s.Cwd = s.SpawnCwd
	}

	return s
}

func (m *Manager) deployHooks() {
	log.Printf("[hooks] deploying hooks to %s", m.paths.Root)
	// Write settings.json to .claude/settings.json (Claude Code loads hooks from here)
	data, err := hooks.Files.ReadFile("files/settings.json")
	if err != nil {
		log.Printf("[hooks] error: embedded settings.json not found: %v", err)
		return
	}
	if err := os.MkdirAll(m.paths.ClaudeDir(), 0755); err != nil {
		log.Printf("[hooks] error creating .claude dir: %v", err)
		return
	}
	if err := os.WriteFile(m.paths.SettingsJSON(), data, 0644); err != nil {
		log.Printf("[hooks] error writing settings.json: %v", err)
		return
	}
	log.Printf("[hooks] wrote settings.json to %s", m.paths.SettingsJSON())

	// Write hook scripts to pool dir/hooks/
	if err := os.MkdirAll(m.paths.HooksDir(), 0755); err != nil {
		log.Printf("[hooks] error creating hooks dir: %v", err)
		return
	}

	for _, name := range []string{"common.sh", "idle-signal.sh", "session-pid-map.sh"} {
		data, err := hooks.Files.ReadFile("files/" + name)
		if err != nil {
			log.Printf("[hooks] error: embedded %s not found: %v", name, err)
			continue
		}
		dst := filepath.Join(m.paths.HooksDir(), name)
		if err := os.WriteFile(dst, data, 0755); err != nil {
			log.Printf("[hooks] error writing %s: %v", name, err)
			continue
		}
		log.Printf("[hooks] wrote %s", dst)
	}
}
