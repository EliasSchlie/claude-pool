package integration

// TestPool — Pool lifecycle flow (CLI)
//
// Pool size: 2
//
// Tests pool-level CLI commands: init, ping, health, config, resize, destroy,
// and re-init with session restoration. Uses setupCLIDaemon because the first
// steps need to interact with the daemon before init.
//
// Flow:
//
//   1.  "ping before init"
//   2.  "config read before init"
//   3.  "config set"
//   4.  "init"
//   5.  "init errors if already running"
//   6.  "ping after init"
//   7.  "health"
//   8.  "pools lists known pools"
//   9.  "resize rejects size 0"
//  10.  "resize up to 3"
//  11.  "resize down to 1"
//  12.  "resize reversal clears pending kill tokens"
//  13.  "deferred eviction of processing sessions"
//  14.  "resize respects pins"
//  15.  "destroy without confirm errors"
//  16.  "destroy"
//  17.  "re-init restores sessions and config"
//  18.  "re-init with no-restore"

import (
	"testing"
	"time"
)

func TestPool(t *testing.T) {
	pool := setupCLIDaemon(t, 2)

	t.Run("ping before init", func(t *testing.T) {
		result := pool.run("ping")
		assertExitOK(t, result)
	})

	t.Run("config read before init", func(t *testing.T) {
		resp := pool.runJSON("config")
		cfg, ok := resp["config"].(map[string]any)
		if !ok {
			t.Fatalf("expected config object, got %T", resp["config"])
		}
		assertContains(t, strVal(cfg, "flags"), "haiku")
		if numVal(cfg, "size") != 2 {
			t.Fatalf("expected size 2, got %v", numVal(cfg, "size"))
		}
	})

	t.Run("config set", func(t *testing.T) {
		resp := pool.runJSON("config", "--set", "size=4")
		cfg, _ := resp["config"].(map[string]any)
		if numVal(cfg, "size") != 4 {
			t.Fatalf("expected size 4 after set, got %v", numVal(cfg, "size"))
		}

		// Verify persistence
		readResp := pool.runJSON("config")
		readCfg, _ := readResp["config"].(map[string]any)
		if numVal(readCfg, "size") != 4 {
			t.Fatalf("config not persisted: expected 4, got %v", numVal(readCfg, "size"))
		}

		// Restore to 2
		pool.run("config", "--set", "size=2")
	})

	var s1, s2 string

	t.Run("init", func(t *testing.T) {
		resp := pool.runJSON("init", "--size", "2")

		// Init returns pool state (same structure as health)
		poolState, ok := resp["pool"].(map[string]any)
		if !ok {
			t.Fatalf("expected pool object in init response, got %v", resp)
		}
		if numVal(poolState, "size") != 2 {
			t.Fatalf("expected pool size 2, got %v", numVal(poolState, "size"))
		}

		// Wait for both sessions to become idle
		pool.waitForIdleCount(2, 90*time.Second)

		// Get session IDs for later steps
		sessions := pool.listSessions()
		if len(sessions) < 2 {
			t.Fatalf("expected at least 2 sessions, got %d", len(sessions))
		}
		s1 = sessions[0].SessionID
		s2 = sessions[1].SessionID
	})

	t.Run("init errors if already running", func(t *testing.T) {
		result := pool.run("init", "--size", "2")
		assertExitError(t, result)
	})

	t.Run("ping after init", func(t *testing.T) {
		result := pool.run("ping")
		assertExitOK(t, result)
	})

	t.Run("health", func(t *testing.T) {
		health := pool.getHealth()

		if numVal(health, "size") != 2 {
			t.Fatalf("expected size 2, got %v", numVal(health, "size"))
		}
		counts, _ := health["counts"].(map[string]any)
		if numVal(counts, "idle") != 2 {
			t.Fatalf("expected 2 idle, got %v", numVal(counts, "idle"))
		}
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0, got %v", numVal(health, "queueDepth"))
		}
	})

	t.Run("pools lists known pools", func(t *testing.T) {
		resp := pool.runJSON("pools")
		// Should list at least our test pool
		if resp == nil {
			t.Fatal("expected non-nil pools response")
		}
	})

	t.Run("resize rejects size 0", func(t *testing.T) {
		result := pool.run("resize", "--size", "0")
		assertExitError(t, result)
	})

	t.Run("resize up to 3", func(t *testing.T) {
		result := pool.run("resize", "--size", "3")
		assertExitOK(t, result)
		pool.waitForIdleCount(3, 60*time.Second)
	})

	t.Run("resize down to 1", func(t *testing.T) {
		// Keep s1 processing so we can observe async slot reclamation
		pool.run("followup", "--session", s1, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(s1, "processing", 15*time.Second)

		pool.run("resize", "--size", "1")

		// Stop s1 so its slot can be reclaimed
		pool.run("stop", "--session", s1)
		pool.waitForPoolSize(1, 15*time.Second)
	})

	t.Run("resize reversal clears pending kill tokens", func(t *testing.T) {
		pool.run("resize", "--size", "2")
		pool.waitForIdleCount(2, 60*time.Second)

		sessions := pool.listSessions()
		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		if len(idle) < 2 {
			t.Fatalf("expected 2 idle sessions, got %d", len(idle))
		}
		sa, sb := idle[0], idle[1]

		// Make both processing so kill tokens can't be consumed
		pool.run("followup", "--session", sa, "--prompt", "run the bash command: sleep 60")
		pool.run("followup", "--session", sb, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(sa, "processing", 15*time.Second)
		pool.waitForStatus(sb, "processing", 15*time.Second)

		// Resize to 1, then back to 2 — token must be cleared
		pool.run("resize", "--size", "1")
		pool.run("resize", "--size", "2")

		// Both must still be processing (no premature eviction)
		infoA := pool.getSessionInfo(sa)
		infoB := pool.getSessionInfo(sb)
		if infoA.Status != "processing" {
			t.Fatalf("expected sa still processing, got %s", infoA.Status)
		}
		if infoB.Status != "processing" {
			t.Fatalf("expected sb still processing, got %s", infoB.Status)
		}

		pool.run("stop", "--session", sa)
		pool.run("stop", "--session", sb)
		pool.waitForIdleCount(2, 60*time.Second)
	})

	t.Run("deferred eviction of processing sessions", func(t *testing.T) {
		sessions := pool.listSessions()
		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		if len(idle) < 2 {
			t.Fatalf("expected 2 idle sessions, got %d", len(idle))
		}
		sa, sb := idle[0], idle[1]

		pool.run("followup", "--session", sa, "--prompt", "run the bash command: sleep 60")
		pool.run("followup", "--session", sb, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(sa, "processing", 15*time.Second)
		pool.waitForStatus(sb, "processing", 15*time.Second)

		// Resize to 1 — kill token lingers
		pool.run("resize", "--size", "1")

		// Stop both — lingering token should evict one
		pool.run("stop", "--session", sa)
		pool.run("stop", "--session", sb)
		pool.waitForPoolSize(1, 15*time.Second)
	})

	t.Run("resize respects pins", func(t *testing.T) {
		pool.run("resize", "--size", "2")
		pool.waitForIdleCount(2, 60*time.Second)

		sessions := pool.listSessions()
		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		if len(idle) < 2 {
			t.Fatalf("expected 2 idle sessions, got %d", len(idle))
		}

		pinned := idle[0]
		pool.run("set", "--session", pinned, "--pinned", "300")

		pool.run("resize", "--size", "1")
		pool.waitForPoolSize(1, 15*time.Second)

		// Pinned session must survive
		info := pool.getSessionInfo(pinned)
		if info.Status == "offloaded" || info.Status == "archived" {
			t.Fatalf("pinned session was evicted: status=%s", info.Status)
		}

		pool.run("set", "--session", pinned, "--pinned", "false")
	})

	t.Run("destroy without confirm errors", func(t *testing.T) {
		result := pool.run("destroy")
		assertExitError(t, result)
	})

	t.Run("destroy", func(t *testing.T) {
		result := pool.run("destroy", "--confirm")
		assertExitOK(t, result)
	})

	t.Run("re-init restores sessions and config", func(t *testing.T) {
		pool.awaitSocketGone(10 * time.Second)
		pool.startDaemon()

		// Config should survive restart
		resp := pool.runJSON("config")
		cfg, _ := resp["config"].(map[string]any)
		if numVal(cfg, "size") != 2 {
			t.Fatalf("config size not persisted: expected 2, got %v", numVal(cfg, "size"))
		}
		assertContains(t, strVal(cfg, "flags"), "haiku")

		initResp := pool.runJSON("init", "--size", "2")
		poolState, ok := initResp["pool"].(map[string]any)
		if !ok {
			t.Fatalf("expected pool object in init response")
		}
		if numVal(poolState, "size") != 2 {
			t.Fatalf("expected size 2, got %v", numVal(poolState, "size"))
		}

		pool.waitForIdleCount(2, 90*time.Second)

		// At least one session must be restored (has Claude UUID from prior run)
		sessions := pool.listSessions("--verbosity", "full")
		hasRestored := false
		for _, s := range sessions {
			if s.ClaudeUUID != "" {
				hasRestored = true
				break
			}
		}
		if !hasRestored {
			t.Fatal("expected at least one restored session with a Claude UUID")
		}
	})

	t.Run("re-init with no-restore", func(t *testing.T) {
		pool.run("destroy", "--confirm")
		pool.awaitSocketGone(10 * time.Second)
		pool.startDaemon()

		pool.runJSON("init", "--size", "2", "--no-restore")
		pool.waitForIdleCount(2, 90*time.Second)

		// All sessions should be fresh — none should match pre-destroy IDs
		sessions := pool.listSessions("--verbosity", "full")
		for _, s := range sessions {
			if s.SessionID == s1 || s.SessionID == s2 {
				t.Fatalf("session %s should not appear with --no-restore", s.SessionID)
			}
		}
	})

	_ = s1
	_ = s2
}
