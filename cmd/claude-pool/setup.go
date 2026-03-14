package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed embedded/skill.md
var embeddedFiles embed.FS

func claudeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func cmdInstall() error {
	claudeBase := claudeDir()

	// Install skill
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

	fmt.Println("\n✅ claude-pool installed!")
	fmt.Println("   Start a new Claude session to activate.")
	return nil
}

func cmdUninstall() error {
	claudeBase := claudeDir()

	// Remove skill
	skillDir := filepath.Join(claudeBase, "skills", "claude-pool")
	if err := os.RemoveAll(skillDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  Could not remove skill: %v\n", err)
	} else {
		fmt.Println("  ✅ Skill removed")
	}

	fmt.Println("\n✅ claude-pool uninstalled.")
	return nil
}
