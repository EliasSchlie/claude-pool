package integration

// TestSlots — Queue, priority, and eviction flow
//
// Pool size: 2
//
// This flow tests what happens when slots are scarce: queueing, priority-based
// eviction, pin protection, and graceful resize behavior. The small pool size (2)
// makes it easy to exhaust slots and trigger these behaviors.
//
// Flow:
//
//   1.  "start fills all slots"
//   2.  "third start queues"
//   3.  "stop cancels queued start"
//   4.  "queued session gets slot when one frees"
//   5.  "set-priority affects eviction order"
//   6.  "LRU within same priority"
//   7.  "pin prevents eviction"
//   8.  "pin on offloaded triggers priority load"
//   9.  "pin without sessionId allocates fresh"
//  10.  "unpin allows eviction"
//  11.  "followup on queued errors"
//  12.  "followup with force on queued replaces prompt"
//  13.  "graceful resize down"
//  14.  "resize respects pins during shrink"

import (
	"testing"
	"time"
)

func TestSlots(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string

	t.Run("start fills all slots", func(t *testing.T) {
		r1 := pool.send(Msg{"type": "start", "prompt": "respond with exactly: slot1"})
		assertNotError(t, r1)
		s1 = strVal(r1, "sessionId")

		r2 := pool.send(Msg{"type": "start", "prompt": "respond with exactly: slot2"})
		assertNotError(t, r2)
		s2 = strVal(r2, "sessionId")

		pool.awaitStatus(s1, "idle", 120*time.Second)
		pool.awaitStatus(s2, "idle", 120*time.Second)

		healthResp := pool.send(Msg{"type": "health"})
		health, _ := healthResp["health"].(map[string]any)
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0, got %v", numVal(health, "queueDepth"))
		}
	})

	var s3 string

	t.Run("third start queues", func(t *testing.T) {
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: queued"})
		assertNotError(t, resp)
		s3 = strVal(resp, "sessionId")

		if strVal(resp, "status") != "queued" {
			t.Fatalf("expected status queued, got %q", strVal(resp, "status"))
		}

		info := pool.send(Msg{"type": "info", "sessionId": s3})
		assertNotError(t, info)
		session := parseSession(t, info["session"])
		assertStatus(t, session, "queued")

		healthResp := pool.send(Msg{"type": "health"})
		health, _ := healthResp["health"].(map[string]any)
		if numVal(health, "queueDepth") != 1 {
			t.Fatalf("expected queue depth 1, got %v", numVal(health, "queueDepth"))
		}
	})

	t.Run("stop cancels queued start", func(t *testing.T) {
		resp := pool.send(Msg{"type": "stop", "sessionId": s3})
		assertNotError(t, resp)

		// Queued session that never got a slot should be gone
		infoResp := pool.send(Msg{"type": "info", "sessionId": s3})
		assertError(t, infoResp)

		healthResp := pool.send(Msg{"type": "health"})
		health, _ := healthResp["health"].(map[string]any)
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0 after stop, got %v", numVal(health, "queueDepth"))
		}
	})

	var s4 string

	t.Run("queued session gets slot when one frees", func(t *testing.T) {
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: dequeued"})
		assertNotError(t, r)
		s4 = strVal(r, "sessionId")

		if strVal(r, "status") != "queued" {
			t.Fatalf("expected queued, got %q", strVal(r, "status"))
		}

		// Free a slot by offloading s1
		pool.send(Msg{"type": "offload", "sessionId": s1})
		pool.awaitStatus(s1, "offloaded", 10*time.Second)

		// s4 should dequeue into the freed slot
		pool.awaitStatus(s4, "idle", 120*time.Second)

		healthResp := pool.send(Msg{"type": "health"})
		health, _ := healthResp["health"].(map[string]any)
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0, got %v", numVal(health, "queueDepth"))
		}
	})

	t.Run("set-priority affects eviction order", func(t *testing.T) {
		pool.awaitStatus(s2, "idle", 10*time.Second)
		pool.awaitStatus(s4, "idle", 10*time.Second)

		// High priority = evicted last, low priority = evicted first
		pool.send(Msg{"type": "set-priority", "sessionId": s2, "priority": 10})
		pool.send(Msg{"type": "set-priority", "sessionId": s4, "priority": -1})

		// Start s5 — pool is full, so an idle session must be evicted
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: priority"})
		assertNotError(t, r)
		s5 := strVal(r, "sessionId")

		// s4 (priority -1) should be evicted, not s2 (priority 10)
		pool.awaitStatus(s4, "offloaded", 30*time.Second)

		info2 := pool.send(Msg{"type": "info", "sessionId": s2})
		session2 := parseSession(t, info2["session"])
		if session2.Status != "idle" && session2.Status != "processing" {
			t.Fatalf("expected s2 to survive eviction, got status %q", session2.Status)
		}

		pool.awaitStatus(s5, "idle", 120*time.Second)

		// Reset priorities and clean up for next steps
		pool.send(Msg{"type": "set-priority", "sessionId": s2, "priority": 0})
	})

	t.Run("LRU within same priority", func(t *testing.T) {
		// Get current sessions in the 2 slots
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		var slotA, slotB string
		for _, s := range sessions {
			if s.Status == "idle" || s.Status == "processing" {
				if slotA == "" {
					slotA = s.SessionID
				} else {
					slotB = s.SessionID
				}
			}
		}
		if slotA == "" || slotB == "" {
			t.Fatal("expected 2 sessions in slots")
		}

		// Set both to same priority
		pool.send(Msg{"type": "set-priority", "sessionId": slotA, "priority": 0})
		pool.send(Msg{"type": "set-priority", "sessionId": slotB, "priority": 0})

		// Touch slotB by sending a followup — makes it most recently used
		pool.send(Msg{"type": "followup", "sessionId": slotB, "prompt": "respond with exactly: recent"})
		pool.awaitStatus(slotB, "idle", 120*time.Second)

		// Start new session — slotA (older/less recent) should be evicted
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: lru"})
		assertNotError(t, r)
		newSid := strVal(r, "sessionId")

		pool.awaitStatus(slotA, "offloaded", 30*time.Second)

		infoB := pool.send(Msg{"type": "info", "sessionId": slotB})
		sessionB := parseSession(t, infoB["session"])
		if sessionB.Status != "idle" {
			t.Fatalf("expected recently-used session to survive, got status %q", sessionB.Status)
		}

		pool.awaitStatus(newSid, "idle", 120*time.Second)

		// Archive old sessions to manage capacity
		pool.send(Msg{"type": "archive", "sessionId": slotA})
	})

	t.Run("pin prevents eviction", func(t *testing.T) {
		// Get current 2 sessions in slots
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		var pinTarget, unpinTarget string
		for _, s := range sessions {
			if s.Status == "idle" {
				if pinTarget == "" {
					pinTarget = s.SessionID
				} else {
					unpinTarget = s.SessionID
				}
			}
		}

		pool.send(Msg{"type": "pin", "sessionId": pinTarget})

		// Start new — unpinned session should be evicted
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: pintest"})
		assertNotError(t, r)
		newSid := strVal(r, "sessionId")

		pool.awaitStatus(unpinTarget, "offloaded", 30*time.Second)

		infoP := pool.send(Msg{"type": "info", "sessionId": pinTarget})
		sessionP := parseSession(t, infoP["session"])
		if sessionP.Status != "idle" {
			t.Fatalf("pinned session should survive eviction, got %q", sessionP.Status)
		}
		if !sessionP.Pinned {
			t.Fatal("expected pinned=true")
		}

		pool.awaitStatus(newSid, "idle", 120*time.Second)
		pool.send(Msg{"type": "unpin", "sessionId": pinTarget})
		pool.send(Msg{"type": "archive", "sessionId": unpinTarget})
	})

	t.Run("pin on offloaded triggers priority load", func(t *testing.T) {
		// s4 was offloaded in step 5
		resp := pool.send(Msg{"type": "pin", "sessionId": s4})
		assertNotError(t, resp)

		// Should be queued for priority load
		status := strVal(resp, "status")
		if status != "queued" && status != "processing" && status != "idle" {
			t.Fatalf("expected queued/processing/idle after pin, got %q", status)
		}

		pool.awaitStatus(s4, "idle", 120*time.Second)

		info := pool.send(Msg{"type": "info", "sessionId": s4})
		session := parseSession(t, info["session"])
		if !session.Pinned {
			t.Fatal("expected s4 to be pinned after pin command")
		}

		pool.send(Msg{"type": "unpin", "sessionId": s4})
	})

	t.Run("pin without sessionId allocates fresh", func(t *testing.T) {
		// Unpin everything first
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		for _, s := range parseSessions(t, lsResp) {
			if s.Pinned {
				pool.send(Msg{"type": "unpin", "sessionId": s.SessionID})
			}
		}

		resp := pool.send(Msg{"type": "pin"})
		assertNotError(t, resp)

		pinSid := strVal(resp, "sessionId")
		assertNonEmpty(t, "pin sessionId", pinSid)

		pool.awaitStatus(pinSid, "idle", 120*time.Second)

		info := pool.send(Msg{"type": "info", "sessionId": pinSid})
		session := parseSession(t, info["session"])
		if !session.Pinned {
			t.Fatal("freshly pinned session should have pinned=true")
		}

		pool.send(Msg{"type": "unpin", "sessionId": pinSid})
	})

	t.Run("unpin allows eviction", func(t *testing.T) {
		// Get current sessions
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		var target string
		for _, s := range sessions {
			if s.Status == "idle" {
				target = s.SessionID
				break
			}
		}
		if target == "" {
			t.Fatal("no idle session found")
		}

		// Pin then unpin
		pool.send(Msg{"type": "pin", "sessionId": target})
		pool.send(Msg{"type": "unpin", "sessionId": target})

		info := pool.send(Msg{"type": "info", "sessionId": target})
		session := parseSession(t, info["session"])
		if session.Pinned {
			t.Fatal("expected pinned=false after unpin")
		}
	})

	t.Run("followup on queued errors", func(t *testing.T) {
		// Fill both slots with processing sessions
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		// Make them all processing
		for _, sid := range idle {
			pool.send(Msg{"type": "followup", "sessionId": sid, "prompt": "run the bash command: sleep 60"})
		}
		for _, sid := range idle {
			pool.awaitStatus(sid, "processing", 30*time.Second)
		}

		// Queue a new session
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: queued"})
		assertNotError(t, r)
		queuedSid := strVal(r, "sessionId")

		if strVal(r, "status") != "queued" {
			t.Fatalf("expected queued, got %q", strVal(r, "status"))
		}

		// Followup on queued should error
		resp := pool.send(Msg{"type": "followup", "sessionId": queuedSid, "prompt": "nope"})
		assertError(t, resp)

		// Clean up: stop the queued session
		pool.send(Msg{"type": "stop", "sessionId": queuedSid})

		for _, sid := range idle {
			pool.stopAndWait(sid)
		}
	})

	t.Run("followup with force on queued replaces prompt", func(t *testing.T) {
		// Fill slots with processing sessions (same setup as previous test)
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		for _, sid := range idle {
			pool.send(Msg{"type": "followup", "sessionId": sid, "prompt": "run the bash command: sleep 60"})
		}
		for _, sid := range idle {
			pool.awaitStatus(sid, "processing", 30*time.Second)
		}

		// Queue a new session
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: original"})
		assertNotError(t, r)
		queuedSid := strVal(r, "sessionId")

		// Force followup replaces the pending prompt
		resp := pool.send(Msg{
			"type": "followup", "sessionId": queuedSid,
			"prompt": "respond with exactly: replaced", "force": true,
		})
		assertNotError(t, resp)
		assertType(t, resp, "started")

		for _, sid := range idle {
			pool.stopAndWait(sid)
		}

		// Wait for the queued session to finish
		waitResp := pool.sendLong(
			Msg{"type": "wait", "sessionId": queuedSid, "timeout": 120000},
			150*time.Second,
		)
		assertNotError(t, waitResp)
		assertContains(t, strVal(waitResp, "content"), "replaced")

		// Clean up
		pool.send(Msg{"type": "archive", "sessionId": queuedSid})
	})

	t.Run("graceful resize down", func(t *testing.T) {
		pool.send(Msg{"type": "resize", "size": 2})
		pool.awaitIdleCount(2, 120*time.Second)

		// Get one idle session and make it processing
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		var processingID string
		for _, s := range sessions {
			if s.Status == "idle" {
				processingID = s.SessionID
				break
			}
		}
		pool.send(Msg{"type": "followup", "sessionId": processingID, "prompt": "run the bash command: sleep 60"})
		pool.awaitStatus(processingID, "processing", 30*time.Second)

		// Resize to 1 — should not interrupt the processing session
		resp := pool.send(Msg{"type": "resize", "size": 1})
		assertNotError(t, resp)

		pool.stopAndWait(processingID)
		pool.awaitPoolSize(1, 30*time.Second)
	})

	t.Run("resize respects pins during shrink", func(t *testing.T) {
		pool.send(Msg{"type": "resize", "size": 2})
		pool.awaitIdleCount(2, 120*time.Second)

		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		var pinned string
		for _, s := range sessions {
			if s.Status == "idle" {
				pinned = s.SessionID
				break
			}
		}
		pool.send(Msg{"type": "pin", "sessionId": pinned})

		pool.send(Msg{"type": "resize", "size": 1})
		pool.awaitPoolSize(1, 30*time.Second)

		info := pool.send(Msg{"type": "info", "sessionId": pinned})
		session := parseSession(t, info["session"])
		if session.Status == "offloaded" || session.Status == "archived" {
			t.Fatalf("pinned session was evicted during resize: status=%s", session.Status)
		}
		pool.send(Msg{"type": "unpin", "sessionId": pinned})
	})
}
