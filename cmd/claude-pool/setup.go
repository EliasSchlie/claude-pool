package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed embedded/skill.md
//go:embed embedded/common.sh
//go:embed embedded/idle-signal.sh
//go:embed embedded/session-pid-map.sh
var embeddedFiles embed.FS

const hookMarker = "/.claude-pool/hooks/"

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return home, nil
}

func cmdInstall() error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	claudeBase := filepath.Join(home, ".claude")
	poolBase := filepath.Join(home, ".claude-pool")

	// 1. Install skill
	skillDir := filepath.Join(claudeBase, "skills", "claude-pool")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	skillContent, err := embeddedFiles.ReadFile("embedded/skill.md")
	if err != nil {
		return fmt.Errorf("read embedded skill: %w", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), skillContent, 0o644); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	fmt.Println("  ✅ Skill installed")

	// 2. Install hook scripts
	hookDir := filepath.Join(poolBase, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		return fmt.Errorf("create hook dir: %w", err)
	}
	hookScripts := []string{"common.sh", "idle-signal.sh", "session-pid-map.sh"}
	for _, name := range hookScripts {
		content, err := embeddedFiles.ReadFile("embedded/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(hookDir, name), content, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Println("  ✅ Hook scripts installed")

	// 3. Register hooks in settings.json
	settingsPath := filepath.Join(claudeBase, "settings.json")
	if err := addHooksToSettings(settingsPath, hookDir); err != nil {
		return fmt.Errorf("register hooks: %w", err)
	}
	fmt.Println("  ✅ Hooks registered in settings.json")

	fmt.Println("\n✅ claude-pool installed!")
	fmt.Println("   Start a new Claude session to activate.")
	return nil
}

func cmdUninstall() error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	claudeBase := filepath.Join(home, ".claude")
	poolBase := filepath.Join(home, ".claude-pool")

	// 1. Remove skill
	skillDir := filepath.Join(claudeBase, "skills", "claude-pool")
	if err := os.RemoveAll(skillDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Could not remove skill: %v\n", err)
	} else {
		fmt.Println("  ✅ Skill removed")
	}

	// 2. Remove hooks from settings.json
	settingsPath := filepath.Join(claudeBase, "settings.json")
	if err := removeHooksFromSettings(settingsPath); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Could not remove hooks: %v\n", err)
	} else {
		fmt.Println("  ✅ Hooks removed from settings.json")
	}

	// 3. Remove hook scripts (but keep pool state)
	hookDir := filepath.Join(poolBase, "hooks")
	_ = os.RemoveAll(hookDir)

	fmt.Println("\n✅ claude-pool uninstalled.")
	fmt.Println("   Pool state preserved in ~/.claude-pool/ (delete manually if unwanted).")
	return nil
}

// hookSettings returns the hooks structure matching the per-pool settings.json format.
// Hook commands reference scripts at the given hookDir.
func hookSettings(hookDir string) map[string]interface{} {
	idle := fmt.Sprintf("bash %s/idle-signal.sh", hookDir)
	pidMap := fmt.Sprintf("bash %s/session-pid-map.sh", hookDir)

	return map[string]interface{}{
		"SessionStart": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": pidMap},
					map[string]interface{}{"type": "command", "command": idle + " write session-start"},
				},
			},
			map[string]interface{}{
				"matcher": "clear",
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " write session-clear"},
				},
			},
		},
		"Stop": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " write stop", "async": true},
				},
			},
		},
		"PreToolUse": []interface{}{
			map[string]interface{}{
				"matcher": "AskUserQuestion|ExitPlanMode",
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " write tool"},
				},
			},
		},
		"PermissionRequest": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " write permission"},
				},
			},
		},
		"PostToolUse": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " clear"},
				},
			},
		},
		"UserPromptSubmit": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": idle + " clear"},
				},
			},
		},
	}
}

// addHooksToSettings registers claude-pool hooks in settings.json.
// Creates settings.json if it doesn't exist. Replaces existing claude-pool hooks.
func addHooksToSettings(path string, hookDir string) error {
	data := map[string]interface{}{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parse settings.json: %w", err)
		}
	}

	hooks, _ := data["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
		data["hooks"] = hooks
	}

	// Remove existing claude-pool entries first (for clean upgrade)
	removePoolEntries(hooks)

	// Add claude-pool hooks to each event type
	newHooks := hookSettings(hookDir)
	for event, entries := range newHooks {
		existing, _ := hooks[event].([]interface{})
		newEntries, _ := entries.([]interface{})
		hooks[event] = append(existing, newEntries...)
	}

	return writeJSON(path, data)
}

// removeHooksFromSettings removes all claude-pool hooks from settings.json.
func removeHooksFromSettings(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil // no settings.json, nothing to remove
	}

	data := map[string]interface{}{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}

	hooks, _ := data["hooks"].(map[string]interface{})
	if hooks == nil {
		return nil
	}

	removePoolEntries(hooks)
	return writeJSON(path, data)
}

// removePoolEntries filters out all claude-pool hook entries from every event type.
func removePoolEntries(hooks map[string]interface{}) {
	for event, val := range hooks {
		entries, ok := val.([]interface{})
		if !ok {
			continue
		}

		var filtered []interface{}
		for _, entry := range entries {
			if !entryBelongsToPool(entry) {
				filtered = append(filtered, entry)
			}
		}

		if len(filtered) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = filtered
		}
	}
}

// entryBelongsToPool checks if a hook entry contains a claude-pool command.
func entryBelongsToPool(entry interface{}) bool {
	entryMap, _ := entry.(map[string]interface{})
	hooksList, _ := entryMap["hooks"].([]interface{})
	for _, h := range hooksList {
		hMap, _ := h.(map[string]interface{})
		if cmd, _ := hMap["command"].(string); strings.Contains(cmd, hookMarker) {
			return true
		}
	}
	return false
}

func writeJSON(path string, data map[string]interface{}) error {
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	// Atomic write: temp file + rename prevents corruption on crash mid-write
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
