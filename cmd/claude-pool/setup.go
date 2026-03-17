package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed embedded/skill.md
var embeddedSkill embed.FS

//go:embed embedded/hooks.json
var embeddedHooksJSON []byte

//go:embed embedded/hook-runner.sh
var embeddedHookRunner []byte

//go:embed embedded/pid-registry.sh
var embeddedPIDRegistry []byte

const (
	pluginName = "claude-pool"
	pluginKey  = "claude-pool@local-tools"
	// hookMarker identifies old standalone claude-pool entries in settings.json.
	hookMarker = "hook-runner.sh"
)

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return home, nil
}

// pluginVersion reads the version from .claude-plugin/plugin.json.
// Falls back to embedded version if the file isn't found.
func pluginVersion() string {
	// Try reading from the plugin.json embedded in the binary
	type manifest struct {
		Version string `json:"version"`
	}
	var m manifest
	// The binary doesn't embed plugin.json, so use a hardcoded default.
	// deploy-plugin.sh handles version bumping for local dev.
	return m.Version
}

func cmdInstall() error {
	changed, err := doInstall()
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("\n✅ claude-pool plugin installed!")
		fmt.Println("   Run /reload-plugins in active sessions, or start a new session.")
	} else {
		fmt.Println("✅ claude-pool plugin already installed (up to date).")
	}
	return nil
}

// doInstall writes plugin files to the Claude Code plugin cache and registers
// in installed_plugins.json. Also cleans up any old standalone hooks.
func doInstall() (bool, error) {
	home, err := homeDir()
	if err != nil {
		return false, err
	}
	claudeBase := filepath.Join(home, ".claude")
	cacheBase := filepath.Join(claudeBase, "plugins", "cache", "local-tools", pluginName)
	installedPath := filepath.Join(claudeBase, "plugins", "installed_plugins.json")

	// Read current version from source plugin.json, bump it
	srcPluginJSON := findSourcePluginJSON()
	currentVersion := readSourceVersion(srcPluginJSON)
	version := "0.1.0"
	if currentVersion != "" {
		version = bumpPatch(currentVersion)
	}
	// Write bumped version back to source plugin.json
	if srcPluginJSON != "" {
		writeSourceVersion(srcPluginJSON, version)
		fmt.Printf("  Version: %s → %s\n", currentVersion, version)
	}

	cacheDir := filepath.Join(cacheBase, version)
	changed := false

	// Remove old cached versions
	if entries, err := os.ReadDir(cacheBase); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				os.RemoveAll(filepath.Join(cacheBase, e.Name()))
			}
		}
	}

	// Write plugin files to cache
	skillContent, err := embeddedSkill.ReadFile("embedded/skill.md")
	if err != nil {
		return false, fmt.Errorf("read embedded skill: %w", err)
	}

	files := map[string]struct {
		content []byte
		perm    os.FileMode
	}{
		".claude-plugin/plugin.json":  {pluginJSON(version), 0o644},
		"skills/claude-pool/SKILL.md": {skillContent, 0o644},
		"hooks/hooks.json":            {embeddedHooksJSON, 0o644},
		"hooks/hook-runner.sh":        {embeddedHookRunner, 0o755},
		"hooks/pid-registry.sh":       {embeddedPIDRegistry, 0o755},
	}

	for relPath, f := range files {
		dest := filepath.Join(cacheDir, relPath)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return false, fmt.Errorf("create dir for %s: %w", relPath, err)
		}
		if err := os.WriteFile(dest, f.content, f.perm); err != nil {
			return false, fmt.Errorf("write %s: %w", relPath, err)
		}
	}
	fmt.Printf("  ✅ Plugin files cached (v%s)\n", version)
	changed = true

	// Register in installed_plugins.json
	if err := registerPlugin(installedPath, cacheDir, version); err != nil {
		return false, fmt.Errorf("register plugin: %w", err)
	}
	fmt.Println("  ✅ Registered in installed_plugins.json")

	// Clean up old standalone hooks from settings.json (if any)
	settingsPath := filepath.Join(claudeBase, "settings.json")
	if removedStandalone := cleanupStandaloneHooks(settingsPath); removedStandalone {
		fmt.Println("  ✅ Removed old standalone hooks from settings.json")

		// Also remove old standalone skill and hook-runner
		oldSkillDir := filepath.Join(claudeBase, "skills", "claude-pool")
		os.RemoveAll(oldSkillDir)
		oldRunner := filepath.Join(home, ".claude-pool", "hook-runner.sh")
		os.Remove(oldRunner)
	}

	return changed, nil
}

func cmdUninstall() error {
	home, err := homeDir()
	if err != nil {
		return err
	}
	claudeBase := filepath.Join(home, ".claude")
	cacheBase := filepath.Join(claudeBase, "plugins", "cache", "local-tools", pluginName)
	installedPath := filepath.Join(claudeBase, "plugins", "installed_plugins.json")

	// Remove from installed_plugins.json
	if err := unregisterPlugin(installedPath); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Could not unregister plugin: %v\n", err)
	} else {
		fmt.Println("  ✅ Removed from installed_plugins.json")
	}

	// Remove cached files
	if err := os.RemoveAll(cacheBase); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Could not remove cache: %v\n", err)
	} else {
		fmt.Println("  ✅ Plugin cache removed")
	}

	// Also clean up any old standalone hooks
	settingsPath := filepath.Join(claudeBase, "settings.json")
	if removedStandalone := cleanupStandaloneHooks(settingsPath); removedStandalone {
		fmt.Println("  ✅ Removed old standalone hooks from settings.json")
	}

	// Remove old standalone files
	oldSkillDir := filepath.Join(claudeBase, "skills", "claude-pool")
	os.RemoveAll(oldSkillDir)
	oldRunner := filepath.Join(home, ".claude-pool", "hook-runner.sh")
	os.Remove(oldRunner)

	fmt.Println("\n✅ claude-pool uninstalled.")
	fmt.Println("   Pool state preserved in ~/.claude-pool/ (delete manually if unwanted).")
	return nil
}

// --- Plugin JSON generation ---

func pluginJSON(version string) []byte {
	m := map[string]interface{}{
		"name":        pluginName,
		"description": "Managed pools of Claude Code sessions — spawn, offload, restore, prompt, attach",
		"version":     version,
		"author":      map[string]string{"name": "Elias Schlie"},
		"repository":  "https://github.com/EliasSchlie/claude-pool",
		"license":     "MIT",
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return append(out, '\n')
}

// --- Source plugin.json management ---

func findSourcePluginJSON() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for range 5 {
			candidate := filepath.Join(dir, ".claude-plugin", "plugin.json")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			dir = filepath.Dir(dir)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, ".claude-plugin", "plugin.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func readSourceVersion(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, _ := m["version"].(string)
	return v
}

func writeSourceVersion(path, version string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	m["version"] = version
	out, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(path, append(out, '\n'), 0o644)
}

// --- installed_plugins.json management ---

func registerPlugin(path, cacheDir, version string) error {
	data := map[string]interface{}{"version": 2, "plugins": map[string]interface{}{}}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &data)
	}

	plugins, _ := data["plugins"].(map[string]interface{})
	if plugins == nil {
		plugins = map[string]interface{}{}
		data["plugins"] = plugins
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	entry := map[string]interface{}{
		"scope":       "user",
		"installPath": cacheDir,
		"version":     version,
		"lastUpdated": now,
	}

	// Preserve installedAt from existing entry
	if existing, ok := plugins[pluginKey].([]interface{}); ok && len(existing) > 0 {
		if e, ok := existing[0].(map[string]interface{}); ok {
			if at, ok := e["installedAt"].(string); ok {
				entry["installedAt"] = at
			}
		}
	}
	if _, ok := entry["installedAt"]; !ok {
		entry["installedAt"] = now
	}

	plugins[pluginKey] = []interface{}{entry}
	return writeJSON(path, data)
}

func unregisterPlugin(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	plugins, _ := data["plugins"].(map[string]interface{})
	delete(plugins, pluginKey)
	return writeJSON(path, data)
}

// --- Standalone hook cleanup (migration from old install) ---

func cleanupStandaloneHooks(settingsPath string) bool {
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

	removed := false
	for event, val := range hooks {
		entries, ok := val.([]interface{})
		if !ok {
			continue
		}
		var filtered []interface{}
		for _, entry := range entries {
			if entryBelongsToPool(entry) {
				removed = true
			} else {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = filtered
		}
	}

	if removed {
		writeJSON(settingsPath, data)
	}
	return removed
}

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

func bumpPatch(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return version
	}
	patch := 0
	fmt.Sscanf(parts[2], "%d", &patch)
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
}

func writeJSON(path string, data map[string]interface{}) error {
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
