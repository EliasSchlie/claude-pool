package integration

// TestSlots — Queue, priority, and eviction flow (CLI)
//
// Pool size: 2
//
// Tests what happens when slots are scarce: queueing, priority-based eviction,
// pin protection, and queued-session behavior. Small pool (2) makes it easy to
// exhaust slots and trigger these behaviors.
//
// Flow:
//
//   1.  "start fills all slots"
//   2.  "start queues when all slots busy"
//   3.  "capture on fresh-queued session errors"
//   4.  "stop cancels queued start"
//   5.  "queued session gets slot when one frees"
//   6.  "set priority affects eviction order"
//   7.  "LRU within same priority"
//   8.  "set pinned prevents eviction"
//   9.  "set pinned false clears pin"
//  10.  "pendingInput resets LRU timestamp"
//  11.  "followup on queued errors"
//  12.  "debug input sends raw bytes"
//  13.  "debug capture reads slot buffer"

import (
	"testing"
	"time"
)

func TestSlots(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string

	t.Run("start fills all slots", func(t *testing.T) {
		r1 := pool.runJSON("start", "--prompt", "respond with exactly: slot1")
		s1 = strVal(r1, "sessionId")

		r2 := pool.runJSON("start", "--prompt", "respond with exactly: slot2")
		s2 = strVal(r2, "sessionId")

		pool.waitForIdle(s1, 300*time.Second)
		pool.waitForIdle(s2, 300*time.Second)

		health := pool.getHealth()
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0, got %v", numVal(health, "queueDepth"))
		}
	})

	var s3 string

	t.Run("start queues when all slots busy", func(t *testing.T) {
		// Both sessions must be processing — idle sessions would be evicted
		pool.run("followup", "--session", s1, "--prompt", "run the bash command: sleep 60")
		pool.run("followup", "--session", s2, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(s1, "processing", 15*time.Second)
		pool.waitForStatus(s2, "processing", 15*time.Second)

		resp := pool.runJSON("start", "--prompt", "respond with exactly: queued")
		s3 = strVal(resp, "sessionId")

		if strVal(resp, "status") != "queued" {
			t.Fatalf("expected status queued, got %q", strVal(resp, "status"))
		}

		info := pool.getSessionInfo(s3)
		assertStatus(t, info, "queued")
		if info.ClaudeUUID != "" {
			t.Fatalf("queued session should have no claudeUUID, got %q", info.ClaudeUUID)
		}

		health := pool.getHealth()
		if numVal(health, "queueDepth") != 1 {
			t.Fatalf("expected queue depth 1, got %v", numVal(health, "queueDepth"))
		}
	})

	t.Run("capture on fresh-queued session errors", func(t *testing.T) {
		for _, args := range [][]string{
			{"--source", "jsonl"},
			{"--source", "jsonl", "--detail", "raw"},
			{"--source", "buffer", "--turns", "1"},
			{"--source", "buffer", "--turns", "0"},
		} {
			fullArgs := append([]string{"capture", "--session", s3}, args...)
			result := pool.run(fullArgs...)
			assertExitError(t, result)
		}
	})

	t.Run("stop cancels queued start", func(t *testing.T) {
		result := pool.run("stop", "--session", s3)
		assertExitOK(t, result)

		infoResult := pool.run("info", "--session", s3, "--json")
		assertExitError(t, infoResult)

		health := pool.getHealth()
		if numVal(health, "queueDepth") != 0 {
			t.Fatalf("expected queue depth 0, got %v", numVal(health, "queueDepth"))
		}
	})

	var s4 string

	t.Run("queued session gets slot when one frees", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: dequeued")
		s4 = strVal(resp, "sessionId")

		if strVal(resp, "status") != "queued" {
			t.Fatalf("expected queued, got %q", strVal(resp, "status"))
		}

		// Stop s1 to free its slot — s4 should dequeue
		pool.run("stop", "--session", s1)
		pool.waitForIdle(s4, 300*time.Second)

		info := pool.getSessionInfo(s4)
		assertNonEmpty(t, "claudeUUID after dequeue", info.ClaudeUUID)
		if info.PID <= 0 {
			t.Fatalf("expected positive PID after dequeue, got %v", info.PID)
		}

		// Stop s2 so it's idle for later steps
		pool.run("stop", "--session", s2)
		pool.waitForStatus(s2, "idle", 15*time.Second)
	})

	t.Run("set priority affects eviction order", func(t *testing.T) {
		pool.waitForStatus(s2, "idle", 10*time.Second)
		pool.waitForStatus(s4, "idle", 10*time.Second)

		pool.run("set", "--session", s2, "--priority", "10")
		pool.run("set", "--session", s4, "--priority", "-1")

		resp := pool.runJSON("start", "--prompt", "respond with exactly: priority")
		s5 := strVal(resp, "sessionId")

		// s4 (priority -1) should be evicted
		pool.waitForStatus(s4, "offloaded", 15*time.Second)

		info2 := pool.getSessionInfo(s2)
		if info2.Status != "idle" && info2.Status != "processing" {
			t.Fatalf("expected s2 to survive eviction, got %q", info2.Status)
		}

		pool.waitForIdle(s5, 300*time.Second)

		pool.run("set", "--session", s2, "--priority", "0")
	})

	t.Run("LRU within same priority", func(t *testing.T) {
		sessions := pool.listSessions()
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

		pool.run("set", "--session", slotA, "--priority", "0")
		pool.run("set", "--session", slotB, "--priority", "0")

		// Touch slotB — makes it most recently used
		pool.run("followup", "--session", slotB, "--prompt", "respond with exactly: recent")
		pool.waitForIdle(slotB, 300*time.Second)

		resp := pool.runJSON("start", "--prompt", "respond with exactly: lru")
		newSid := strVal(resp, "sessionId")

		pool.waitForStatus(slotA, "offloaded", 15*time.Second)

		infoB := pool.getSessionInfo(slotB)
		if infoB.Status != "idle" {
			t.Fatalf("recently-used session should survive, got %q", infoB.Status)
		}

		pool.waitForIdle(newSid, 300*time.Second)
		pool.run("archive", "--session", slotA)
	})

	t.Run("set pinned prevents eviction", func(t *testing.T) {
		sessions := pool.listSessions()
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

		pool.run("set", "--session", pinTarget, "--pinned", "300")

		resp := pool.runJSON("start", "--prompt", "respond with exactly: pintest")
		newSid := strVal(resp, "sessionId")

		pool.waitForStatus(unpinTarget, "offloaded", 15*time.Second)

		infoP := pool.getSessionInfo(pinTarget)
		if infoP.Status != "idle" {
			t.Fatalf("pinned session should survive, got %q", infoP.Status)
		}
		if !infoP.Pinned {
			t.Fatal("expected pinned=true")
		}

		pool.waitForIdle(newSid, 300*time.Second)
		pool.run("set", "--session", pinTarget, "--pinned", "false")
		pool.run("archive", "--session", unpinTarget)
	})

	t.Run("set pinned false clears pin", func(t *testing.T) {
		sessions := pool.listSessions()
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

		pool.run("set", "--session", target, "--pinned", "300")
		pool.run("set", "--session", target, "--pinned", "false")

		info := pool.getSessionInfo(target)
		if info.Pinned {
			t.Fatal("expected pinned=false after unpin")
		}
	})

	t.Run("pendingInput resets LRU timestamp", func(t *testing.T) {
		// Get two idle sessions at same priority
		sessions := pool.listSessions()
		var slotA, slotB string
		for _, s := range sessions {
			if s.Status == "idle" {
				if slotA == "" {
					slotA = s.SessionID
				} else {
					slotB = s.SessionID
				}
			}
		}
		if slotA == "" || slotB == "" {
			t.Fatal("expected 2 idle sessions in slots")
		}

		pool.run("set", "--session", slotA, "--priority", "0")
		pool.run("set", "--session", slotB, "--priority", "0")

		// Type into slotA via debug input (simulates pendingInput) — makes it most recently used
		pool.run("debug", "input", "--session", slotA, "--data", "some text")

		// Start new session — slotB (no pendingInput activity) should be evicted, not slotA
		resp := pool.runJSON("start", "--prompt", "respond with exactly: lru-pending")
		newSid := strVal(resp, "sessionId")

		pool.waitForStatus(slotB, "offloaded", 15*time.Second)

		infoA := pool.getSessionInfo(slotA)
		if infoA.Status == "offloaded" {
			t.Fatal("session with pendingInput activity should survive eviction")
		}

		pool.waitForIdle(newSid, 300*time.Second)

		// Clear pendingInput via Ctrl-U
		pool.run("debug", "input", "--session", slotA, "--data", "\x15")
		pool.run("archive", "--session", slotB)
	})

	t.Run("followup on queued errors", func(t *testing.T) {
		sessions := pool.listSessions()
		var idle []string
		for _, s := range sessions {
			if s.Status == "idle" {
				idle = append(idle, s.SessionID)
			}
		}
		for _, sid := range idle {
			pool.run("followup", "--session", sid, "--prompt", "run the bash command: sleep 60")
		}
		for _, sid := range idle {
			pool.waitForStatus(sid, "processing", 15*time.Second)
		}

		resp := pool.runJSON("start", "--prompt", "respond with exactly: queued")
		queuedSid := strVal(resp, "sessionId")

		if strVal(resp, "status") != "queued" {
			t.Fatalf("expected queued, got %q", strVal(resp, "status"))
		}

		result := pool.run("followup", "--session", queuedSid, "--prompt", "nope")
		assertExitError(t, result)

		pool.run("stop", "--session", queuedSid)
		for _, sid := range idle {
			pool.run("stop", "--session", sid)
		}
	})

	t.Run("debug input sends raw bytes", func(t *testing.T) {
		sessions := pool.listSessions()
		var target string
		for _, s := range sessions {
			if s.Status == "idle" {
				target = s.SessionID
				break
			}
		}
		if target == "" {
			t.Fatal("no idle session for debug input test")
		}

		// Send text via debug input — should populate pendingInput
		result := pool.run("debug", "input", "--session", target, "--data", "debug-test")
		assertExitOK(t, result)

		// Clear it
		pool.run("debug", "input", "--session", target, "--data", "\x15")
	})

	t.Run("debug capture reads slot buffer", func(t *testing.T) {
		// debug capture works on slot index, not session ID
		result := pool.run("debug", "capture", "--slot", "0")
		assertExitOK(t, result)
		if result.Stdout == "" {
			t.Fatal("expected non-empty slot buffer")
		}
	})
}
