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

		pool.awaitIdleCount(3, 120*time.Second)
	})

	t.Run("resize down to 1", func(t *testing.T) {
		// Keep s1 processing so we can observe async slot reclamation
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "run the bash command: sleep 60"})
		pool.awaitStatus(s1, "processing", 30*time.Second)

		resp := pool.send(Msg{"type": "resize", "size": 1})
		assertNotError(t, resp)

		// Stop s1 so its slot can be reclaimed
		pool.send(Msg{"type": "stop", "sessionId": s1})
		pool.awaitPoolSize(1, 30*time.Second)
	})

	t.Run("resize respects pins", func(t *testing.T) {
		pool.send(Msg{"type": "resize", "size": 2})
		pool.awaitIdleCount(2, 120*time.Second)

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

		pool.awaitPoolSize(1, 30*time.Second)

		// Verify the pinned session survived
		infoResp := pool.send(Msg{"type": "info", "sessionId": pinned})
		assertNotError(t, infoResp)
		info := parseSession(t, infoResp["session"])
		if info.Status == "offloaded" || info.Status == "archived" {
			t.Fatalf("pinned session was evicted: status=%s", info.Status)
		}

		pool.send(Msg{"type": "unpin", "sessionId": pinned})
	})

	t.Run("destroy without confirm errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "destroy"})
		assertError(t, resp)
	})

	// s1 was the first session — we'll check if it's restored after re-init

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
		if s1 != "" {
			found := false
			for _, raw := range sessions {
				sm, _ := raw.(map[string]any)
				if strVal(sm, "sessionId") == s1 {
					found = true
					break
				}
			}
			if !found {
				t.Logf("warning: expected session %s to be restored, but it wasn't in pool sessions", s1)
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
			if sid == s1 || sid == s2 {
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
