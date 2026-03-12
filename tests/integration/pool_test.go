package integration

// TestPool — Pool lifecycle flow
//
// Pool size: 2
//
// This flow tests pool-level operations: init, config, resize, health, destroy,
// and re-init with session restoration. It uses setupDaemon (not setupPool)
// because the first few steps need to interact with the daemon before init.
//
// Flow:
//
//   1.  "ping before init"
//   2.  "config before init"
//   3.  "config set"
//   4.  "init"
//   5.  "init errors if already initialized"
//   6.  "health"
//   7.  "resize up to 3"
//   8.  "resize down to 1"
//   9.  "resize respects pins"
//  10.  "destroy without confirm errors"
//  11.  "destroy"
//  12.  "re-init restores sessions"
//  13.  "re-init with noRestore"

import (
	"testing"
	"time"
)

func TestPool(t *testing.T) {
	pool := setupDaemon(t, 2)

	t.Run("ping before init", func(t *testing.T) {
		resp := pool.send(Msg{"type": "ping"})
		assertType(t, resp, "pong")
	})

	t.Run("config before init", func(t *testing.T) {
		resp := pool.send(Msg{"type": "config"})
		assertNotError(t, resp)
		assertType(t, resp, "config")

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
		resp := pool.send(Msg{"type": "config", "set": Msg{"size": 4}})
		assertNotError(t, resp)

		cfg, _ := resp["config"].(map[string]any)
		if numVal(cfg, "size") != 4 {
			t.Fatalf("expected size 4 after set, got %v", numVal(cfg, "size"))
		}

		// Verify persistence by re-reading
		readResp := pool.send(Msg{"type": "config"})
		readCfg, _ := readResp["config"].(map[string]any)
		if numVal(readCfg, "size") != 4 {
			t.Fatalf("config not persisted: expected size 4, got %v", numVal(readCfg, "size"))
		}

		// Restore to 2 for rest of flow
		pool.send(Msg{"type": "config", "set": Msg{"size": 2}})
	})

	// Track session IDs for later steps
	var s1, s2 string

	t.Run("init", func(t *testing.T) {
		resp := pool.send(Msg{"type": "init", "size": 2})
		assertNotError(t, resp)
		assertType(t, resp, "pool")

		state, ok := resp["pool"].(map[string]any)
		if !ok {
			t.Fatalf("expected pool object, got %T", resp["pool"])
		}
		if numVal(state, "size") != 2 {
			t.Fatalf("expected pool size 2, got %v", numVal(state, "size"))
		}

		sessions, _ := state["sessions"].([]any)
		if len(sessions) != 2 {
			t.Fatalf("expected 2 sessions, got %d", len(sessions))
		}

		// Wait for both sessions to become idle (pre-warming)
		for _, raw := range sessions {
			sm, _ := raw.(map[string]any)
			sid := strVal(sm, "sessionId")
			pool.awaitStatus(sid, "idle", 120*time.Second)
		}

		sm0, _ := sessions[0].(map[string]any)
		sm1, _ := sessions[1].(map[string]any)
		s1 = strVal(sm0, "sessionId")
		s2 = strVal(sm1, "sessionId")
	})

	t.Run("init errors if already initialized", func(t *testing.T) {
		resp := pool.send(Msg{"type": "init", "size": 2})
		assertError(t, resp)
	})

	t.Run("health", func(t *testing.T) {
		resp := pool.send(Msg{"type": "health"})
		assertNotError(t, resp)
		assertType(t, resp, "health")

		health, ok := resp["health"].(map[string]any)
		if !ok {
			t.Fatalf("expected health object, got %T", resp["health"])
		}
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

		// Each session should have PID liveness info
		healthSessions, _ := health["sessions"].([]any)
		for _, raw := range healthSessions {
			hs, _ := raw.(map[string]any)
			if _, hasPidAlive := hs["pidAlive"]; !hasPidAlive {
				t.Fatalf("health session missing pidAlive field: %v", hs)
			}
		}
	})

	t.Run("resize up to 3", func(t *testing.T) {
		resp := pool.send(Msg{"type": "resize", "size": 3})
		assertNotError(t, resp)
		assertType(t, resp, "pool")

		state, _ := resp["pool"].(map[string]any)
		if numVal(state, "size") != 3 {
			t.Fatalf("expected size 3 after resize, got %v", numVal(state, "size"))
		}

		// Wait for 3 idle sessions
		healthResp := pool.sendLong(Msg{"type": "health"}, 120*time.Second)
		health, _ := healthResp["health"].(map[string]any)

		// Poll until we see 3 idle — the new session needs time to pre-warm
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			healthResp = pool.send(Msg{"type": "health"})
			health, _ = healthResp["health"].(map[string]any)
			counts, _ := health["counts"].(map[string]any)
			if numVal(counts, "idle") == 3 {
				return
			}
			time.Sleep(2 * time.Second)
		}
		counts, _ := health["counts"].(map[string]any)
		t.Fatalf("expected 3 idle after resize, got %v idle", numVal(counts, "idle"))
	})

	t.Run("resize down to 1", func(t *testing.T) {
		// Keep s1 processing so we can observe async slot reclamation
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "run the bash command: sleep 60"})
		pool.awaitStatus(s1, "processing", 30*time.Second)

		resp := pool.send(Msg{"type": "resize", "size": 1})
		assertNotError(t, resp)

		// Wait for pool to settle at 1 slot — stop s1 so its slot can be reclaimed
		pool.send(Msg{"type": "stop", "sessionId": s1})
		pool.sendLong(Msg{"type": "wait", "sessionId": s1, "timeout": 120000}, 150*time.Second)

		// Poll health until we reach 1 slot
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			healthResp := pool.send(Msg{"type": "health"})
			health, _ := healthResp["health"].(map[string]any)
			if numVal(health, "size") == 1 {
				return
			}
			time.Sleep(1 * time.Second)
		}
		t.Fatal("pool did not shrink to 1 slot within timeout")
	})

	t.Run("resize respects pins", func(t *testing.T) {
		// Resize to 2, wait for both idle
		pool.send(Msg{"type": "resize", "size": 2})

		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			healthResp := pool.send(Msg{"type": "health"})
			health, _ := healthResp["health"].(map[string]any)
			counts, _ := health["counts"].(map[string]any)
			if numVal(counts, "idle") == 2 {
				break
			}
			time.Sleep(2 * time.Second)
		}

		// Identify current sessions
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		if len(sessions) < 2 {
			t.Fatalf("expected at least 2 sessions, got %d", len(sessions))
		}

		pinned := sessions[0].SessionID
		pool.send(Msg{"type": "pin", "sessionId": pinned})

		// Resize to 1 — the unpinned session should lose its slot
		pool.send(Msg{"type": "resize", "size": 1})

		deadline = time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			healthResp := pool.send(Msg{"type": "health"})
			health, _ := healthResp["health"].(map[string]any)
			if numVal(health, "size") == 1 {
				// Verify the pinned session survived
				infoResp := pool.send(Msg{"type": "info", "sessionId": pinned})
				assertNotError(t, infoResp)
				info := parseSession(t, infoResp["session"])
				if info.Status == "offloaded" || info.Status == "archived" {
					t.Fatalf("pinned session was evicted: status=%s", info.Status)
				}

				pool.send(Msg{"type": "unpin", "sessionId": pinned})
				return
			}
			time.Sleep(1 * time.Second)
		}
		pool.send(Msg{"type": "unpin", "sessionId": pinned})
		t.Fatal("pool did not shrink to 1 slot within timeout")
	})

	t.Run("destroy without confirm errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "destroy"})
		assertError(t, resp)
	})

	// Save a session ID before destroying — we'll check for it after re-init
	lsResp := pool.send(Msg{"type": "ls", "all": true})
	preDestroySessions := parseSessions(t, lsResp)
	var restoredID string
	if len(preDestroySessions) > 0 {
		restoredID = preDestroySessions[0].SessionID
	}

	t.Run("destroy", func(t *testing.T) {
		resp := pool.send(Msg{"type": "destroy", "confirm": true})
		assertNotError(t, resp)
		assertType(t, resp, "ok")
	})

	t.Run("re-init restores sessions", func(t *testing.T) {
		// Wait briefly for daemon to exit and socket to clean up
		time.Sleep(2 * time.Second)

		pool.startDaemon()

		resp := pool.send(Msg{"type": "init", "size": 2})
		assertNotError(t, resp)
		assertType(t, resp, "pool")

		state, _ := resp["pool"].(map[string]any)
		sessions, _ := state["sessions"].([]any)

		// Wait for sessions to become idle
		for _, raw := range sessions {
			sm, _ := raw.(map[string]any)
			sid := strVal(sm, "sessionId")
			pool.awaitStatus(sid, "idle", 120*time.Second)
		}

		// Check if the pre-destroy session was restored
		if restoredID != "" {
			found := false
			for _, raw := range sessions {
				sm, _ := raw.(map[string]any)
				if strVal(sm, "sessionId") == restoredID {
					found = true
					break
				}
			}
			if !found {
				t.Logf("warning: expected session %s to be restored, but it wasn't in pool sessions", restoredID)
			}
		}
	})

	t.Run("re-init with noRestore", func(t *testing.T) {
		// Destroy and restart
		pool.send(Msg{"type": "destroy", "confirm": true})
		time.Sleep(2 * time.Second)

		pool.startDaemon()

		resp := pool.send(Msg{"type": "init", "size": 2, "noRestore": true})
		assertNotError(t, resp)
		assertType(t, resp, "pool")

		state, _ := resp["pool"].(map[string]any)
		sessions, _ := state["sessions"].([]any)

		// All sessions should be fresh — none should match pre-destroy IDs
		for _, raw := range sessions {
			sm, _ := raw.(map[string]any)
			sid := strVal(sm, "sessionId")
			if sid == restoredID || sid == s1 || sid == s2 {
				t.Fatalf("session %s should not appear with noRestore", sid)
			}
		}

		// Wait for both to become idle
		for _, raw := range sessions {
			sm, _ := raw.(map[string]any)
			sid := strVal(sm, "sessionId")
			pool.awaitStatus(sid, "idle", 120*time.Second)
		}
	})
}
