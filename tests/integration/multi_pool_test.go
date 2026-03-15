package integration

// TestMultiPool — Multiple pools running simultaneously (CLI)
//
// Tests invariant #1: "Pool isolation is absolute." Two pools share the same
// CLAUDE_POOL_HOME (same registry, same machine) but must be completely
// independent — separate daemons, separate sessions, no shared state.
//
// Pool alpha: size 1
// Pool beta:  size 1
//
// Flow:
//
//   1.  "init alpha"
//   2.  "init beta"
//   3.  "pools lists both"
//   4.  "start session in alpha"
//   5.  "start session in beta"
//   6.  "sessions are isolated"
//   7.  "concurrent operations"
//   8.  "destroy alpha"
//   9.  "beta unaffected by alpha destroy"
//  10.  "alpha sessions gone after destroy"

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMultiPool(t *testing.T) {
	testDir := filepath.Join(runDir, t.Name())
	cpHome := filepath.Join(testDir, ".claude-pool")

	alpha := newNamedPool(t, "alpha", cpHome, filepath.Join(testDir, "workdir-alpha"))
	beta := newNamedPool(t, "beta", cpHome, filepath.Join(testDir, "workdir-beta"))

	t.Run("init alpha", func(t *testing.T) {
		result := alpha.run("init", "--size", "1",
			"--dir", alpha.workDir,
			"--flags", "--dangerously-skip-permissions --model haiku")
		assertExitOK(t, result)
		alpha.waitForIdleCount(1, 90*time.Second)
	})

	t.Run("init beta", func(t *testing.T) {
		result := beta.run("init", "--size", "1",
			"--dir", beta.workDir,
			"--flags", "--dangerously-skip-permissions --model haiku")
		assertExitOK(t, result)
		beta.waitForIdleCount(1, 90*time.Second)
	})

	t.Run("pools lists both", func(t *testing.T) {
		// Both pools should appear in the shared registry
		resp := alpha.runJSON("pools")
		pools, ok := resp["pools"].([]any)
		if !ok {
			t.Fatalf("expected pools array, got %T", resp["pools"])
		}

		names := make(map[string]bool)
		for _, p := range pools {
			if pm, ok := p.(map[string]any); ok {
				names[strVal(pm, "name")] = true
			}
		}
		if !names["alpha"] {
			t.Fatal("expected alpha in pools list")
		}
		if !names["beta"] {
			t.Fatal("expected beta in pools list")
		}
	})

	var alphaSession, betaSession string

	t.Run("start session in alpha", func(t *testing.T) {
		resp := alpha.runJSON("start", "--prompt", "respond with exactly: from-alpha")
		alphaSession = strVal(resp, "sessionId")
		assertNonEmpty(t, "alpha sessionId", alphaSession)
		alpha.waitForIdle(alphaSession, 300*time.Second)
	})

	t.Run("start session in beta", func(t *testing.T) {
		resp := beta.runJSON("start", "--prompt", "respond with exactly: from-beta")
		betaSession = strVal(resp, "sessionId")
		assertNonEmpty(t, "beta sessionId", betaSession)
		beta.waitForIdle(betaSession, 300*time.Second)
	})

	t.Run("sessions are isolated", func(t *testing.T) {
		// Alpha's ls should only show alpha's sessions
		alphaSessions := alpha.listSessions()
		for _, s := range alphaSessions {
			if s.SessionID == betaSession {
				t.Fatal("alpha should not see beta's sessions")
			}
		}

		// Beta's ls should only show beta's sessions
		betaSessions := beta.listSessions()
		for _, s := range betaSessions {
			if s.SessionID == alphaSession {
				t.Fatal("beta should not see alpha's sessions")
			}
		}

		// Info on alpha's session from beta should fail
		result := beta.run("info", "--session", alphaSession, "--json")
		assertExitError(t, result)

		// Info on beta's session from alpha should fail
		result = alpha.run("info", "--session", betaSession, "--json")
		assertExitError(t, result)
	})

	t.Run("concurrent operations", func(t *testing.T) {
		// Send followups to both pools simultaneously
		alpha.run("followup", "--session", alphaSession, "--prompt", "respond with exactly: alpha-concurrent")
		beta.run("followup", "--session", betaSession, "--prompt", "respond with exactly: beta-concurrent")

		alphaResp := alpha.waitForIdle(alphaSession, 300*time.Second)
		betaResp := beta.waitForIdle(betaSession, 300*time.Second)

		assertContains(t, strVal(alphaResp, "content"), "alpha-concurrent")
		assertContains(t, strVal(betaResp, "content"), "beta-concurrent")
	})

	t.Run("destroy alpha", func(t *testing.T) {
		result := alpha.run("destroy", "--confirm")
		assertExitOK(t, result)
	})

	t.Run("beta unaffected by alpha destroy", func(t *testing.T) {
		// Beta should still be fully functional
		result := beta.run("ping")
		assertExitOK(t, result)

		health := beta.getHealth()
		if numVal(health, "size") != 1 {
			t.Fatalf("expected beta size 1, got %v", numVal(health, "size"))
		}

		// Beta's session should still be accessible
		info := beta.getSessionInfo(betaSession)
		assertStatus(t, info, "idle")

		// Followup still works
		resp := beta.runJSON("followup", "--session", betaSession,
			"--prompt", "respond with exactly: beta-still-alive", "--block")
		assertContains(t, strVal(resp, "content"), "beta-still-alive")
	})

	t.Run("alpha sessions gone after destroy", func(t *testing.T) {
		// Alpha commands should fail — daemon is dead
		result := alpha.run("ping")
		assertExitError(t, result)

		// Pools should show alpha as stopped
		poolsResp := beta.runJSON("pools")
		pools, _ := poolsResp["pools"].([]any)
		for _, p := range pools {
			pm, _ := p.(map[string]any)
			if strVal(pm, "name") == "alpha" {
				status := strVal(pm, "status")
				if status != "stopped" {
					t.Fatalf("expected alpha status stopped, got %q", status)
				}
			}
		}
	})

	_ = fmt.Sprint() // keep fmt import for newNamedPool path building
}
