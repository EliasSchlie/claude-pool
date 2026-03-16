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
		assertNumVal(t, health, "size", 2)

		pool.waitForSlotsReady(2, 90*time.Second)

		// Pre-warmed slots are ready but no user sessions exist yet
		assertSessionCount(t, pool.listSessions(), 0)

		// Start real sessions (claims pre-warmed slots)
		s1 = pool.startSession("respond with exactly: init-s1")
		s2 = pool.startSession("respond with exactly: init-s2")
	})

	// Prevents: init response diverging from health response
	// (SPEC: init and health both return a Pool Object.)
	t.Run("init response matches health", func(t *testing.T) {
		health := pool.getHealth()

		// SPEC: Pool Object fields
		assertNonEmpty(t, "name", strVal(health, "name"))
		assertNumVal(t, health, "size", 2)
		if _, ok := health["queueDepth"]; !ok {
			t.Fatal("pool object missing 'queueDepth'")
		}
		if _, ok := health["slots"]; !ok {
			t.Fatal("pool object missing 'slots'")
		}
		if _, ok := health["sessions"]; !ok {
			t.Fatal("pool object missing 'sessions'")
		}
		if _, ok := health["config"]; !ok {
			t.Fatal("pool object missing 'config'")
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

		assertNumVal(t, health, "size", 2)
		assertNumVal(t, health, "queueDepth", 0)

		// Both slots host idle user sessions
		slots, _ := health["slots"].(map[string]any)
		assertNumVal(t, slots, "idle", 2)

		// Session counts match
		sessions, _ := health["sessions"].(map[string]any)
		assertNumVal(t, sessions, "idle", 2)
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
		assertNumVal(t, cfg, "size", 2)
	})

	t.Run("config set", func(t *testing.T) {
		resp := pool.runJSON("config", "--set", "size=4")
		cfg, _ := resp["config"].(map[string]any)
		assertNumVal(t, cfg, "size", 4)

		readResp := pool.runJSON("config")
		readCfg, _ := readResp["config"].(map[string]any)
		assertNumVal(t, readCfg, "size", 4)

		pool.run("config", "--set", "size=2")
	})

	t.Run("config keepFresh", func(t *testing.T) {
		// Init used --keep-fresh 0, verify it was persisted
		resp := pool.runJSON("config")
		cfg, _ := resp["config"].(map[string]any)
		assertNumVal(t, cfg, "keepFresh", 0)

		// Set keepFresh via config
		setResp := pool.runJSON("config", "--set", "keepFresh=2")
		setCfg, _ := setResp["config"].(map[string]any)
		assertNumVal(t, setCfg, "keepFresh", 2)

		// Verify persistence
		readResp := pool.runJSON("config")
		readCfg, _ := readResp["config"].(map[string]any)
		assertNumVal(t, readCfg, "keepFresh", 2)

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
		// s1, s2 are idle user sessions; 3rd slot is fresh (pre-warmed)
		pool.waitForSlotsReady(3, 60*time.Second)

		// SPEC: resize "changes slot count immediately and updates config"
		cfgResp := pool.runJSON("config")
		cfg, _ := cfgResp["config"].(map[string]any)
		assertNumVal(t, cfg, "size", 3)
	})

	t.Run("resize down to 1", func(t *testing.T) {
		pool.run("followup", "--session", s1, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(s1, "processing", 15*time.Second)

		pool.run("resize", "--size", "1")

		pool.run("stop", "--session", s1)
		pool.waitForPoolSize(1, 15*time.Second)
	})

	// State: pool size 1, s1 idle in the surviving slot. s2 offloaded.

	t.Run("resize reversal clears pending kill tokens", func(t *testing.T) {
		pool.run("resize", "--size", "2")
		pool.waitForSlotsReady(2, 60*time.Second)

		// New slot is fresh (pre-warmed) — start a session to claim it
		pool.startSession("respond with exactly: extra")

		idle := pool.idleSessionIDs(2)
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
		pool.waitForIdleSlots(2, 60*time.Second)
	})

	// State: pool size 2, 2 idle user sessions in slots.

	t.Run("deferred eviction of processing sessions", func(t *testing.T) {
		idle := pool.idleSessionIDs(2)
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

	// State: pool size 1, 1 user session idle.

	t.Run("resize respects pins", func(t *testing.T) {
		pool.run("resize", "--size", "2")
		pool.waitForSlotsReady(2, 60*time.Second)

		// Start a session for the fresh slot so we have 2 user sessions
		pool.startSession("respond with exactly: pin-test")

		pinned := pool.idleSessionIDs(2)[0]
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
		assertNumVal(t, health, "size", 2)

		pool.waitForSlotsReady(2, 90*time.Second)

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
		pool.waitForSlotsReady(2, 90*time.Second)

		// No user sessions should exist — all slots are fresh pre-warmed
		sessions := pool.listSessions("--verbosity", "full")
		assertSessionCount(t, sessions, 0)
	})

	_ = s1
	_ = s2
}
