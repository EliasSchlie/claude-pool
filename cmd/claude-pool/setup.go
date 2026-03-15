package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/EliasSchlie/claude-pool/internal/hookfiles"
)

//go:embed embedded/skill.md
var embeddedSkill embed.FS

// hookMarker identifies claude-pool entries in settings.json for add/remove.
const hookMarker = "hook-runner.sh"

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return home, nil
}

func cmdInstall() error {
	changed, err := doInstall()
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("\n✅ claude-pool installed!")
		fmt.Println("   Start a new Claude session to activate.")
	} else {
		fmt.Println("✅ claude-pool already installed (up to date).")
	}
	return nil
}

// doInstall ensures skill, hook-runner, and hooks are installed.
// Only writes files that are missing or outdated. Returns true if anything changed.
func doInstall() (bool, error) {
	home, err := homeDir()
	if err != nil {
		return false, err
	}
	claudeBase := filepath.Join(home, ".claude")
	poolBase := filepath.Join(home, ".claude-pool")

	changed := false

	// 1. Install skill (only if content differs)
	skillDir := filepath.Join(claudeBase, "skills", "claude-pool")
	skillPath := filepath.Join(skillDir, "SKILL.md")
	skillContent, err := embeddedSkill.ReadFile("embedded/skill.md")
	if err != nil {
		return false, fmt.Errorf("read embedded skill: %w", err)
	}
	if !fileMatchesContent(skillPath, skillContent) {
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return false, fmt.Errorf("create skill dir: %w", err)
		}
		if err := os.WriteFile(skillPath, skillContent, 0o644); err != nil {
			return false, fmt.Errorf("write skill: %w", err)
		}
		fmt.Println("  ✅ Skill installed")
		changed = true
	}

	// 2. Install hook runner (only if content differs)
	runnerPath := filepath.Join(poolBase, "hook-runner.sh")
	if !fileMatchesContent(runnerPath, hookfiles.HookRunner) {
		if err := os.MkdirAll(poolBase, 0o755); err != nil {
			return false, fmt.Errorf("create pool base dir: %w", err)
		}
		if err := os.WriteFile(runnerPath, hookfiles.HookRunner, 0o755); err != nil {
			return false, fmt.Errorf("write hook-runner.sh: %w", err)
		}
		fmt.Println("  ✅ Hook runner installed")
		changed = true
	}

	// 3. Register hooks in settings.json (only if missing)
	settingsPath := filepath.Join(claudeBase, "settings.json")
	if !hooksRegistered(settingsPath) {
		if err := addHooksToSettings(settingsPath, runnerPath); err != nil {
			return false, fmt.Errorf("register hooks: %w", err)
		}
		fmt.Println("  ✅ Hooks registered in settings.json")
		changed = true
	}

	return changed, nil
}

// fileMatchesContent returns true if the file at path exists and has the given content.
func fileMatchesContent(path string, content []byte) bool {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(existing) == string(content)
}

// hooksRegistered checks if claude-pool hooks are present in settings.json.
func hooksRegistered(settingsPath string) bool {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return false
	}
	hooks, _ := data["hooks"].(map[string]interface{})
	if hooks == nil {
		return false
	}
	// Check that at least one event type has a pool entry
	for _, val := range hooks {
		entries, ok := val.([]interface{})
		if !ok {
			continue
		}
		for _, entry := range entries {
			if entryBelongsToPool(entry) {
				return true
			}
		}
	}
	return false
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

	// 3. Remove hook runner
	_ = os.Remove(filepath.Join(poolBase, "hook-runner.sh"))

	fmt.Println("\n✅ claude-pool uninstalled.")
	fmt.Println("   Pool state preserved in ~/.claude-pool/ (delete manually if unwanted).")
	return nil
}

// hookSettings builds the settings.json hooks structure.
// All commands go through hook-runner.sh, which delegates to pool-local scripts
// via $CLAUDE_POOL_DIR at runtime. This keeps pools self-contained — each pool
// deploys its own hook scripts on init, so different pools can run different versions.
func hookSettings(runnerPath string) map[string]interface{} {
	run := func(args string) string {
		return fmt.Sprintf("bash %s %s", runnerPath, args)
	}

	return map[string]interface{}{
		"SessionStart": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("session-pid-map.sh")},
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh write session-start")},
				},
			},
			map[string]interface{}{
				"matcher": "clear",
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh write session-clear")},
				},
			},
		},
		"Stop": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh write stop"), "async": true},
				},
			},
		},
		"PreToolUse": []interface{}{
			map[string]interface{}{
				"matcher": "AskUserQuestion|ExitPlanMode",
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh write tool")},
				},
			},
		},
		"PermissionRequest": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh write permission")},
				},
			},
		},
		"PostToolUse": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh clear")},
				},
			},
		},
		"UserPromptSubmit": []interface{}{
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": run("idle-signal.sh clear")},
				},
			},
		},
	}
}

// addHooksToSettings registers claude-pool hooks in settings.json.
// Creates settings.json if it doesn't exist. Replaces existing claude-pool hooks.
func addHooksToSettings(path string, runnerPath string) error {
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
	newHooks := hookSettings(runnerPath)
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
