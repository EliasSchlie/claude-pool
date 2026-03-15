package integration

// TestKeepFresh — Fresh slot maintenance behavior (CLI)
//
// Pool size: 3
//
// Tests that the pool proactively offloads idle sessions to maintain
// a target number of fresh slots. Uses keepFresh=1 (the spec default).
//
// Flow:
//
//   1.  "init with keepFresh=1"
//   2.  "fresh slots maintained after sessions go idle"
//   3.  "keepFresh respects pins"
//   4.  "keepFresh=0 disables proactive offloading"
//   5.  "config update triggers fresh slot maintenance"

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeepFresh(t *testing.T) {
	testDir := filepath.Join(runDir, t.Name())
	cpHome := filepath.Join(testDir, ".claude-pool")
	if err := os.MkdirAll(cpHome, 0755); err != nil {
		t.Fatalf("failed to create .claude-pool dir: %v", err)
	}
	pool := newNamedPool(t, "test", cpHome, filepath.Join(testDir, "workdir"))

	t.Run("init with keepFresh=1", func(t *testing.T) {
		result := pool.run("init", "--size", "3",
			"--dir", pool.workDir,
			"--keep-fresh", "1",
			"--flags", "--dangerously-skip-permissions --model haiku")
		assertExitOK(t, result)

		pool.waitForIdleCount(3, 90*time.Second)

		// Verify config has keepFresh=1
		resp := pool.runJSON("config")
		cfg, _ := resp["config"].(map[string]any)
		if numVal(cfg, "keepFresh") != 1 {
			t.Fatalf("expected keepFresh 1, got %v", numVal(cfg, "keepFresh"))
		}
	})

	t.Run("fresh slots maintained after sessions go idle", func(t *testing.T) {
		// Start 3 sessions to fill the pool
		var sessions []string
		for i := 0; i < 3; i++ {
			resp := pool.runJSON("start", "--prompt", fmt.Sprintf("respond with exactly: keep-fresh-%d", i))
			sessions = append(sessions, strVal(resp, "sessionId"))
		}

		// Wait for all to complete
		for _, sid := range sessions {
			pool.waitForIdle(sid, 300*time.Second)
		}

		// With keepFresh=1, the pool should proactively offload one idle session
		// to maintain 1 fresh slot. Wait for this to happen.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			health := pool.getHealth()
			counts, _ := health["counts"].(map[string]any)
			freshCount := numVal(counts, "fresh")
			if freshCount >= 1 {
				// Verify exactly one session was offloaded
				offloadedCount := numVal(counts, "offloaded")
				if offloadedCount < 1 {
					t.Fatalf("expected at least 1 offloaded session, got %v", offloadedCount)
				}
				return
			}
			time.Sleep(500 * time.Millisecond)
		}

		health := pool.getHealth()
		counts, _ := health["counts"].(map[string]any)
		t.Fatalf("expected at least 1 fresh slot after keepFresh maintenance, got %v (counts: %v)",
			numVal(counts, "fresh"), counts)
	})

	t.Run("keepFresh respects pins", func(t *testing.T) {
		// Pin all remaining idle sessions — pool should not be able to free more slots
		sessions := pool.listSessions("--status", "idle")
		for _, s := range sessions {
			pool.run("set", "--session", s.SessionID, "--pinned", "300")
		}

		// Set keepFresh higher to request more fresh slots
		pool.run("config", "--set", "keepFresh=2")

		// Wait briefly — proactive offloading should NOT happen on pinned sessions
		time.Sleep(5 * time.Second)

		health := pool.getHealth()
		counts, _ := health["counts"].(map[string]any)
		idleCount := numVal(counts, "idle")
		if idleCount == 0 {
			t.Fatal("all idle sessions were offloaded despite being pinned")
		}

		// Unpin all
		sessions = pool.listSessions("--status", "idle")
		for _, s := range sessions {
			pool.run("set", "--session", s.SessionID, "--pinned", "false")
		}

		// Restore keepFresh
		pool.run("config", "--set", "keepFresh=1")
	})

	t.Run("keepFresh=0 disables proactive offloading", func(t *testing.T) {
		pool.run("config", "--set", "keepFresh=0")

		// Archive everything and start fresh
		sessions := pool.listSessions()
		for _, s := range sessions {
			pool.run("archive", "--session", s.SessionID)
		}
		offloaded := pool.listSessions("--status", "offloaded")
		for _, s := range offloaded {
			pool.run("archive", "--session", s.SessionID)
		}

		// Start 3 sessions to fill all slots
		var sids []string
		for i := 0; i < 3; i++ {
			resp := pool.runJSON("start", "--prompt", fmt.Sprintf("respond with exactly: no-fresh-%d", i))
			sids = append(sids, strVal(resp, "sessionId"))
		}
		for _, sid := range sids {
			pool.waitForIdle(sid, 300*time.Second)
		}

		// With keepFresh=0, no proactive offloading — all 3 should remain idle
		time.Sleep(5 * time.Second)

		health := pool.getHealth()
		counts, _ := health["counts"].(map[string]any)
		if numVal(counts, "idle") != 3 {
			t.Fatalf("expected 3 idle sessions with keepFresh=0, got %v (counts: %v)",
				numVal(counts, "idle"), counts)
		}
		if numVal(counts, "fresh") != 0 {
			t.Fatalf("expected 0 fresh slots with keepFresh=0 and full pool, got %v",
				numVal(counts, "fresh"))
		}
	})

	t.Run("config update triggers fresh slot maintenance", func(t *testing.T) {
		// Pool is full (3 idle, 0 fresh, keepFresh=0). Set keepFresh=1 — should trigger offload.
		pool.run("config", "--set", "keepFresh=1")

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			health := pool.getHealth()
			counts, _ := health["counts"].(map[string]any)
			if numVal(counts, "fresh") >= 1 {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}

		health := pool.getHealth()
		counts, _ := health["counts"].(map[string]any)
		t.Fatalf("expected keepFresh config update to trigger maintenance, fresh=%v (counts: %v)",
			numVal(counts, "fresh"), counts)
	})
}
