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
		"parent":     s.ParentID,
		"priority":   s.Priority,
		"spawnCwd":   s.SpawnCwd,
		"cwd":        s.Cwd,
		"createdAt":  s.CreatedAt.UTC().Format(time.RFC3339),
		"flags":      s.Flags,
		"metadata":   s.Metadata,
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
		"size": float64(len(m.slots)),
	}

	sessions := make([]map[string]any, 0)
	for _, s := range m.sessions {
		sess := map[string]any{
			"sessionId":  s.ID,
			"claudeUUID": s.ClaudeUUID,
			"status":     s.Status,
			"parent":     s.ParentID,
			"priority":   s.Priority,
			"spawnCwd":   s.SpawnCwd,
			"cwd":        s.Cwd,
			"createdAt":  s.CreatedAt.UTC().Format(time.RFC3339),
			"lastUsedAt": s.LastUsedAt.UTC().Format(time.RFC3339),
			"flags":      s.Flags,
			"pinned":     s.Pinned,
			"metadata":   s.Metadata,
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
		case StatusIdle, StatusProcessing, "fresh":
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
		ParentID:   coalesce(strVal(sm, "parent"), strVal(sm, "parentId")),
		Priority:   numVal(sm, "priority"),
		SpawnCwd:   strVal(sm, "spawnCwd"),
		Cwd:        strVal(sm, "cwd"),
		Flags:      strVal(sm, "flags"),
		Pinned:     boolVal(sm, "pinned"),
		Metadata:   metadataFromMap(sm),
		SlotIndex:  -1, // not loaded until bindSession
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
		if cfg, err := m.config.Load(); err == nil && cfg.Dir != "" {
			s.SpawnCwd = cfg.Dir
		} else {
			s.SpawnCwd = m.paths.Root
		}
	}
	if s.Cwd == "" {
		s.Cwd = s.SpawnCwd
	}

	return s
}

// deployHooks writes hook scripts to the pool directory.
// Called on every init — each pool owns its hook scripts, so different pools
// (or different branches under test) can run different hook versions independently.
// The global hook-runner.sh (installed via `claude-pool install`) delegates to
// these pool-local scripts via $CLAUDE_POOL_DIR at runtime.
func (m *Manager) deployHooks() error {
	log.Printf("[hooks] deploying hooks to %s", m.paths.Root)

	if err := os.MkdirAll(m.paths.HooksDir(), 0755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	for _, name := range []string{"common.sh", "idle-signal.sh", "session-pid-map.sh"} {
		data, err := hookfiles.Scripts.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(m.paths.HooksDir(), name), data, 0755); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	log.Printf("[hooks] hooks deployed")
	return nil
}
