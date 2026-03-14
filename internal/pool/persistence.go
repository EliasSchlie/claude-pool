package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/hookfiles"
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
			"lastUsedAt": s.LastUsedAt.UTC().Format(time.RFC3339),
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
	// Atomic write: temp file + rename prevents corruption on crash mid-write
	tmpPath := m.paths.PoolJSON() + ".tmp"
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0644); err != nil {
		log.Printf("[persist] error writing pool.json.tmp: %v", err)
		return
	}
	if err := os.Rename(tmpPath, m.paths.PoolJSON()); err != nil {
		log.Printf("[persist] error renaming pool.json.tmp → pool.json: %v", err)
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
	// Sort offloaded sessions: most recently used first, so user-started
	// sessions get restored into slots before unused pre-warmed ones.
	sort.Slice(offloaded, func(i, j int) bool {
		return offloaded[i].LastUsedAt.After(offloaded[j].LastUsedAt)
	})
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
	if t := strVal(sm, "lastUsedAt"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			s.LastUsedAt = parsed
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

// checkGlobalInstall verifies that claude-pool hooks are installed globally.
// Returns an error if the install is missing — the daemon cannot function
// without hooks to detect session state changes.
func checkGlobalInstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	hookDir := filepath.Join(home, ".claude-pool", "hooks")
	for _, name := range []string{"common.sh", "idle-signal.sh", "session-pid-map.sh"} {
		if _, err := os.Stat(filepath.Join(hookDir, name)); err != nil {
			return fmt.Errorf("claude-pool is not installed (missing %s). Run: claude-pool install", name)
		}
	}
	return nil
}

// deployLocalHooks writes hook scripts and settings.json to the pool directory.
// Used when localHooks is true — provides full isolation from the global install.
// Global hooks defer to local hooks when they detect pool-dir/.claude/settings.json.
func (m *Manager) deployLocalHooks() error {
	log.Printf("[hooks] deploying local hooks to %s", m.paths.Root)

	// Write settings.json to pool-dir/.claude/
	claudeDir := filepath.Join(m.paths.Root, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), hookfiles.LocalSettings, 0644); err != nil {
		return fmt.Errorf("write local settings.json: %w", err)
	}

	// Write hook scripts to pool-dir/hooks/
	hookDir := filepath.Join(m.paths.Root, "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	// common-local.sh is deployed as common.sh (the scripts source "common.sh")
	localScripts := map[string]string{
		"common-local.sh":    "common.sh",
		"idle-signal.sh":     "idle-signal.sh",
		"session-pid-map.sh": "session-pid-map.sh",
	}
	for src, dst := range localScripts {
		data, err := hookfiles.LocalScripts.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(hookDir, dst), data, 0755); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}

	log.Printf("[hooks] local hooks deployed")
	return nil
}
