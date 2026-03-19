package pty

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Prevents: spawned sessions missing Homebrew/user-installed tools because
// the daemon was launched from a non-login context (e.g., Electron app, launchd)
func TestGetLoginShellPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell PATH not applicable on Windows")
	}

	path := resolveLoginPATH()
	if path == "" {
		t.Fatal("resolveLoginPATH returned empty string")
	}

	// Should contain standard system paths
	if !strings.Contains(path, "/usr/bin") {
		t.Errorf("login PATH missing /usr/bin: %s", path)
	}
}

// Prevents: buildEnvWithLoginPATH dropping or duplicating PATH entries
func TestBuildEnvWithLoginPATH(t *testing.T) {
	env := buildEnvWithLoginPATH()

	var foundPath bool
	pathCount := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathCount++
			foundPath = true
		}
	}

	if !foundPath {
		t.Error("PATH not found in env")
	}
	if pathCount != 1 {
		t.Errorf("expected exactly 1 PATH entry, got %d", pathCount)
	}
}

// Prevents: login PATH resolution hanging on broken shell profiles
func TestResolveLoginPATH_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell PATH not applicable on Windows")
	}

	// With a bogus shell, should fall back to current PATH
	orig := os.Getenv("SHELL")
	os.Setenv("SHELL", "/nonexistent/shell")
	defer os.Setenv("SHELL", orig)

	path := resolveLoginPATH()
	if path != os.Getenv("PATH") {
		t.Errorf("expected fallback to current PATH, got: %s", path)
	}
}
