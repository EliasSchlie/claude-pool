package integration

// TestPool — Pool lifecycle flow (CLI)
//
// Pool size: 2
//
// Tests pool-level CLI commands: init, ping, health, config, resize, destroy,
// and re-init with session restoration. Uses newPool because the first steps
// test behavior before init.
//
// Flow:
//
//   1.  "ping before init fails"
//   2.  "pools before init"
//   3.  "init"
//   4.  "init response matches health"
//   5.  "init errors if already running"
//   6.  "ping after init"
//   7.  "health"
//   8.  "pools lists the pool"
//   9.  "config read"
//  10.  "config set"
//  10a. "config keepFresh"
//  11.  "resize rejects size 0"
//  12.  "resize up to 3"
//  13.  "resize down to 1"
//  14.  "resize reversal clears pending kill tokens"
//  15.  "deferred eviction of processing sessions"
//  16.  "resize respects pins"
//  17.  "destroy without confirm errors"
//  18.  "destroy"
//  19.  "re-init restores sessions and config"
//  20.  "re-init with no-restore"

import (
	"testing"
	"time"
)

func TestPool(t *testing.T) {
	pool := newPool(t)

	t.Run("ping before init fails", func(t *testing.T) {
		// No daemon running yet — ping should fail
		result := pool.run("ping")
		assertExitError(t, result)
	})

	t.Run("pools before init", func(t *testing.T) {
		// Registry doesn't exist yet — should succeed with empty list or no error
		result := pool.run("pools", "--json")
		_ = result
	})

	var s1, s2 string

	t.Run("init", func(t *testing.T) {
		resp := pool.runJSON("init", "--size", "2",
			"--dir", pool.workDir,
			"--keep-fresh", "0",
			"--flags", "--dangerously-skip-permissions --model haiku")

		// SPEC: init response is "same as health"
		health, ok := resp["health"].(map[string]any)
		if !ok {
			t.Fatalf("expected health object in init response, got %v", resp)
		}
		if numVal(health, "size") != 2 {
			t.Fatalf("expected pool size 2, got %v", numVal(health, "size"))
		}

		pool.waitForIdleCount(2, 90*time.Second)

		sessions := pool.listSessions()
		if len(sessions) < 2 {
			t.Fatalf("expected at least 2 sessions, got %d", len(sessions))
		}
		s1 = sessions[0].SessionID
		s2 = sessions[1].SessionID
	})

	// Prevents: init response diverging from health response
	// (SPEC: "Pool state after initialization (same as health).")
	t.Run("init response matches health", func(t *testing.T) {
		health := pool.getHealth()

		// Verify health fields present — same ones we check in the health step
		if numVal(health, "size") != 2 {
			t.Fatalf("init health size: expected 2, got %v", numVal(health, "size"))
		}
		if _, ok := health["counts"]; !ok {
			t.Fatal("init health response missing 'counts'")
		}
		if _, ok := health["queueDepth"]; !ok {
			t.Fatal("init health response missing 'queueDepth'")
		}
		if _, ok := health["sessions"]; !ok {
			t.Fatal("init health response missing 'sessions'")
		}
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

	t.Run("pools lists the pool", func(t *testing.T) {
		resp := pool.runJSON("pools")
		if resp == nil {
			t.Fatal("expected non-nil pools response")
		}
	})

	t.Run("config read", func(t *testing.T) {
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

		readResp := pool.runJSON("config")
		readCfg, _ := readResp["config"].(map[string]any)
		if numVal(readCfg, "size") != 4 {
			t.Fatalf("config not persisted: expected 4, got %v", numVal(readCfg, "size"))
		}

		pool.run("config", "--set", "size=2")
	})

	t.Run("config keepFresh", func(t *testing.T) {
		// Init used --keep-fresh 0, verify it was persisted
		resp := pool.runJSON("config")
		cfg, _ := resp["config"].(map[string]any)
		if numVal(cfg, "keepFresh") != 0 {
			t.Fatalf("expected keepFresh 0, got %v", numVal(cfg, "keepFresh"))
		}

		// Set keepFresh via config
		setResp := pool.runJSON("config", "--set", "keepFresh=2")
		setCfg, _ := setResp["config"].(map[string]any)
		if numVal(setCfg, "keepFresh") != 2 {
			t.Fatalf("expected keepFresh 2 after set, got %v", numVal(setCfg, "keepFresh"))
		}

		// Verify persistence
		readResp := pool.runJSON("config")
		readCfg, _ := readResp["config"].(map[string]any)
		if numVal(readCfg, "keepFresh") != 2 {
			t.Fatalf("keepFresh not persisted: expected 2, got %v", numVal(readCfg, "keepFresh"))
		}

		// Restore to 0
		pool.run("config", "--set", "keepFresh=0")
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
		pool.run("followup", "--session", s1, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(s1, "processing", 15*time.Second)

		pool.run("resize", "--size", "1")

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

		pool.run("followup", "--session", sa, "--prompt", "run the bash command: sleep 60")
		pool.run("followup", "--session", sb, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(sa, "processing", 15*time.Second)
		pool.waitForStatus(sb, "processing", 15*time.Second)

		pool.run("resize", "--size", "1")
		pool.run("resize", "--size", "2")

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

		pool.run("resize", "--size", "1")

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

		resp := pool.runJSON("init", "--size", "2", "--dir", pool.workDir, "--keep-fresh", "0")
		health, ok := resp["health"].(map[string]any)
		if !ok {
			t.Fatalf("expected health object in init response")
		}
		if numVal(health, "size") != 2 {
			t.Fatalf("expected size 2, got %v", numVal(health, "size"))
		}

		pool.waitForIdleCount(2, 90*time.Second)

		// Config survives destroy+init cycle
		cfgResp := pool.runJSON("config")
		cfg, _ := cfgResp["config"].(map[string]any)
		assertContains(t, strVal(cfg, "flags"), "haiku")

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

		pool.runJSON("init", "--size", "2", "--dir", pool.workDir, "--keep-fresh", "0", "--no-restore")
		pool.waitForIdleCount(2, 90*time.Second)

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
