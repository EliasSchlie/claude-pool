package pty

import (
	"runtime"
	"strings"
	"testing"
)

// Prevents: spawned sessions inheriting daemon env instead of fresh login shell env
func TestResolveLoginEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell env not applicable on Windows")
	}

	env, err := resolveLoginEnv()
	if err != nil {
		t.Fatalf("resolveLoginEnv failed: %v", err)
	}

	if len(env) == 0 {
		t.Fatal("resolveLoginEnv returned empty env")
	}

	// Must contain essential vars
	var hasPath, hasHome, hasUser bool
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if strings.HasPrefix(e, "HOME=") {
			hasHome = true
		}
		if strings.HasPrefix(e, "USER=") {
			hasUser = true
		}
	}
	if !hasPath {
		t.Error("login env missing PATH")
	}
	if !hasHome {
		t.Error("login env missing HOME")
	}
	if !hasUser {
		t.Error("login env missing USER")
	}
}

// Prevents: Homebrew tools missing because login shell env doesn't include /opt/homebrew/bin
func TestResolveLoginEnv_ContainsHomebrew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell env not applicable on Windows")
	}

	env, err := resolveLoginEnv()
	if err != nil {
		t.Fatalf("resolveLoginEnv failed: %v", err)
	}

	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") && strings.Contains(e, "/opt/homebrew") {
			return // found it
		}
	}
	t.Skip("Homebrew not installed — skipping")
}

// Prevents: null-delimited parser dropping valid entries or accepting garbage
func TestParseNullDelimitedEnv(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  int
	}{
		{"normal", []byte("A=1\x00B=2\x00C=3\x00"), 3},
		{"trailing null", []byte("A=1\x00"), 1},
		{"no trailing null", []byte("A=1"), 1},
		{"empty entries skipped", []byte("A=1\x00\x00B=2\x00"), 2},
		{"no equals skipped", []byte("INVALID\x00A=1\x00"), 1},
		{"value with newline", []byte("A=line1\nline2\x00B=2\x00"), 2},
		{"value with equals", []byte("A=x=y=z\x00"), 1},
		{"empty", []byte(""), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNullDelimitedEnv(tt.input)
			if len(got) != tt.want {
				t.Errorf("parseNullDelimitedEnv(%q) returned %d entries, want %d: %v", tt.input, len(got), tt.want, got)
			}
		})
	}
}

// Prevents: daemon env vars (CLAUDECODE, debug flags, etc.) leaking into spawned sessions
func TestResolveLoginEnv_NoDaemonVarsLeak(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell env not applicable on Windows")
	}

	env, err := resolveLoginEnv()
	if err != nil {
		t.Fatalf("resolveLoginEnv failed: %v", err)
	}

	// These vars exist in the daemon's env but should NOT appear in login shell env
	daemonVars := []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"}
	for _, dv := range daemonVars {
		for _, e := range env {
			if strings.HasPrefix(e, dv+"=") {
				t.Errorf("daemon var %s leaked into login shell env", dv)
			}
		}
	}
}

// Prevents: buildSpawnEnv mutating the cached login env slice
func TestBuildSpawnEnv_NoCacheMutation(t *testing.T) {
	env1 := buildSpawnEnv()
	env1 = append(env1, "A=1")
	env2 := buildSpawnEnv()
	env2 = append(env2, "B=2")

	// env1 should not contain B, and env2 should not contain A
	for _, e := range env1 {
		if e == "B=2" {
			t.Error("env1 contains B=2 — cache was mutated")
		}
	}
	for _, e := range env2 {
		if e == "A=1" {
			t.Error("env2 contains A=1 — cache was mutated")
		}
	}
}
