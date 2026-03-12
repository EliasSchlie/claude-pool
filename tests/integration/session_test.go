package integration

// TestSession — Core session workflow flow
//
// Pool size: 3
//
// This flow tests the fundamental session operations: starting sessions, waiting
// for results, reading output in different formats, sending followups, and raw
// input. It exercises the most common usage patterns.
//
// Session tracking: the flow creates sessions deliberately and archives them when
// no longer needed to stay within the pool's 3-slot capacity.
//
// Flow:
//
//   1.  "start returns sessionId and status"
//   2.  "wait returns result"
//   3.  "info shows session details"
//   4.  "wait on already-idle returns immediately"
//   5.  "capture while idle — jsonl-short (default)"
//   6.  "capture — jsonl-last"
//   7.  "capture — jsonl-long"
//   8.  "capture — jsonl-full"
//   9.  "capture — buffer-last"
//   10. "capture — buffer-full"
//   11. "followup to idle session"
//   12. "followup on processing errors without force"
//   13. "followup with force on processing"
//   14. "wait on offloaded session errors"
//   15. "wait with no sessionId — returns first idle"
//   16. "wait with no sessionId — errors if none busy"
//   17. "wait with timeout"
//   18. "input sends raw bytes and verifiable text"
//   19. "session prefix resolution"

import (
	"strings"
	"testing"
	"time"
)

func TestSession(t *testing.T) {
	pool := setupPool(t, 3)

	// Track active sessions so we can manage pool capacity explicitly.
	// Pool has 3 slots — we must archive/offload before exceeding that.
	var s1 string

	t.Run("start returns sessionId and status", func(t *testing.T) {
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly the text: hello world"})
		assertNotError(t, resp)
		assertType(t, resp, "started")

		s1 = strVal(resp, "sessionId")
		assertNonEmpty(t, "sessionId", s1)

		// With pool size 3 and only 1 session, should get a slot immediately (processing).
		// Queued is also acceptable if slots aren't pre-warmed yet.
		status := strVal(resp, "status")
		if status != "processing" && status != "queued" {
			t.Fatalf("expected status processing or queued, got %q", status)
		}
	})

	t.Run("wait returns result", func(t *testing.T) {
		resp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s1, "timeout": 120000},
			150*time.Second,
		)
		assertNotError(t, resp)
		assertType(t, resp, "result")

		if strVal(resp, "sessionId") != s1 {
			t.Fatalf("wait returned wrong sessionId: got %s, want %s", strVal(resp, "sessionId"), s1)
		}
		assertContains(t, strVal(resp, "content"), "hello world")
	})

	t.Run("info shows session details", func(t *testing.T) {
		resp := pool.send(Msg{"type": "info", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "session")

		info := parseSession(t, resp["session"])
		if info.SessionID != s1 {
			t.Fatalf("expected sessionId %s, got %s", s1, info.SessionID)
		}
		assertStatus(t, info, "idle")
		assertNonEmpty(t, "claudeUUID", info.ClaudeUUID)
		if info.PID <= 0 {
			t.Fatalf("expected positive PID, got %v", info.PID)
		}
		if info.Pinned {
			t.Fatal("expected pinned=false for new session")
		}
		if info.Priority != 0 {
			t.Fatalf("expected priority 0, got %v", info.Priority)
		}
		assertNonEmpty(t, "spawnCwd", info.SpawnCwd)
		assertNonEmpty(t, "cwd", info.Cwd)
		assertNonEmpty(t, "createdAt", info.CreatedAt)
		if len(info.Children) != 0 {
			t.Fatalf("expected no children, got %d", len(info.Children))
		}
	})

	t.Run("wait on already-idle returns immediately", func(t *testing.T) {
		// s1 is already idle from step 2 — wait should return without blocking
		resp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s1, "timeout": 5000},
			10*time.Second,
		)
		assertNotError(t, resp)
		assertContains(t, strVal(resp, "content"), "hello world")
	})

	t.Run("capture while idle — jsonl-short default", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "result")
		assertNonEmpty(t, "capture content", strVal(resp, "content"))
	})

	t.Run("capture — jsonl-last", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-last"})
		assertNotError(t, resp)
		assertNonEmpty(t, "jsonl-last content", strVal(resp, "content"))
	})

	t.Run("capture — jsonl-long", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-long"})
		assertNotError(t, resp)
		assertNonEmpty(t, "jsonl-long content", strVal(resp, "content"))
	})

	t.Run("capture — jsonl-full", func(t *testing.T) {
		respFull := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-full"})
		assertNotError(t, respFull)
		fullContent := strVal(respFull, "content")
		assertNonEmpty(t, "jsonl-full content", fullContent)

		// jsonl-full is the complete unfiltered transcript — should always be >= jsonl-short
		// which only includes assistant messages since the last user message
		respShort := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-short"})
		shortContent := strVal(respShort, "content")
		if len(fullContent) < len(shortContent) {
			t.Fatalf("jsonl-full (%d bytes) should be >= jsonl-short (%d bytes)",
				len(fullContent), len(shortContent))
		}
	})

	t.Run("capture — buffer-last", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-last"})
		assertNotError(t, resp)
		assertNonEmpty(t, "buffer-last content", strVal(resp, "content"))
	})

	t.Run("capture — buffer-full", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-full"})
		assertNotError(t, resp)
		assertNonEmpty(t, "buffer-full content", strVal(resp, "content"))
	})

	t.Run("followup to idle session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly the text: goodbye"})
		assertNotError(t, resp)
		assertType(t, resp, "started")

		if strVal(resp, "sessionId") != s1 {
			t.Fatalf("followup returned different sessionId: got %s, want %s", strVal(resp, "sessionId"), s1)
		}
		assertNonEmpty(t, "followup status", strVal(resp, "status"))

		waitResp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s1, "timeout": 120000},
			150*time.Second,
		)
		assertNotError(t, waitResp)
		assertContains(t, strVal(waitResp, "content"), "goodbye")
	})

	var s2 string

	t.Run("followup on processing errors without force", func(t *testing.T) {
		// Use a bash sleep to keep s2 processing without burning tokens
		startResp := pool.send(Msg{"type": "start", "prompt": "run the bash command: sleep 60"})
		assertNotError(t, startResp)
		s2 = strVal(startResp, "sessionId")

		// Wait until s2 is actually processing before testing followup rejection
		pool.awaitStatus(s2, "processing", 30*time.Second)

		resp := pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "ignore that"})
		assertError(t, resp)

		errMsg := strVal(resp, "error")
		if !strings.Contains(strings.ToLower(errMsg), "processing") &&
			!strings.Contains(strings.ToLower(errMsg), "force") {
			t.Logf("warning: error message doesn't mention processing/force: %s", errMsg)
		}
	})

	t.Run("followup with force on processing", func(t *testing.T) {
		// Confirm s2 is still processing before testing force
		pool.awaitStatus(s2, "processing", 10*time.Second)

		resp := pool.send(Msg{
			"type": "followup", "sessionId": s2,
			"prompt": "respond with exactly the text: interrupted", "force": true,
		})
		assertNotError(t, resp)
		assertType(t, resp, "started")

		if strVal(resp, "sessionId") != s2 {
			t.Fatalf("expected sessionId %s, got %s", s2, strVal(resp, "sessionId"))
		}

		waitResp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s2, "timeout": 120000},
			150*time.Second,
		)
		assertNotError(t, waitResp)
		assertContains(t, strVal(waitResp, "content"), "interrupted")
	})

	t.Run("wait on offloaded session errors", func(t *testing.T) {
		// Offload s2 (idle after force-followup) to test wait on offloaded
		pool.send(Msg{"type": "offload", "sessionId": s2})
		pool.awaitStatus(s2, "offloaded", 10*time.Second)

		resp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s2, "timeout": 5000},
			10*time.Second,
		)
		assertError(t, resp)
	})

	t.Run("wait with no sessionId — returns first idle", func(t *testing.T) {
		// s2 is offloaded from the previous step — archive it to free capacity
		pool.send(Msg{"type": "archive", "sessionId": s2})

		// Start two new sessions — pool has s1 (idle) + 2 fresh = 3 slots, within capacity
		r1 := pool.send(Msg{"type": "start", "prompt": "respond with exactly the text: first"})
		assertNotError(t, r1)
		s3 := strVal(r1, "sessionId")

		r2 := pool.send(Msg{"type": "start", "prompt": "respond with exactly the text: second"})
		assertNotError(t, r2)
		s4 := strVal(r2, "sessionId")

		// No sessionId — returns the first owned session that becomes idle
		resp := pool.sendLong(Msg{"type": "wait", "timeout": 120000}, 150*time.Second)
		assertNotError(t, resp)

		sid := strVal(resp, "sessionId")
		if sid != s3 && sid != s4 {
			t.Fatalf("waitAny returned unknown session %s (expected %s or %s)", sid, s3, s4)
		}
		assertNonEmpty(t, "waitAny content", strVal(resp, "content"))

		// Clean up: wait for the other session so it doesn't leak into later steps
		other := s3
		if sid == s3 {
			other = s4
		}
		cleanup := pool.sendLong(
			Msg{"type": "wait", "sessionId": other, "timeout": 120000},
			150*time.Second,
		)
		assertNotError(t, cleanup)

		// Archive s3 and s4 — we only need s1 going forward
		pool.send(Msg{"type": "archive", "sessionId": s3})
		pool.send(Msg{"type": "archive", "sessionId": s4})
	})

	t.Run("wait with no sessionId — errors if none busy", func(t *testing.T) {
		// All remaining sessions are idle — wait should error immediately
		resp := pool.sendLong(Msg{"type": "wait", "timeout": 5000}, 10*time.Second)
		assertError(t, resp)
	})

	t.Run("wait with timeout", func(t *testing.T) {
		// Use bash sleep so the session stays processing without burning tokens
		startResp := pool.send(Msg{"type": "start", "prompt": "run the bash command: sleep 60"})
		assertNotError(t, startResp)
		sid := strVal(startResp, "sessionId")

		pool.awaitStatus(sid, "processing", 30*time.Second)

		// 1ms timeout — guaranteed to expire before sleep finishes
		resp := pool.sendLong(Msg{"type": "wait", "sessionId": sid, "timeout": 1}, 5*time.Second)
		assertError(t, resp)
		assertContains(t, strVal(resp, "error"), "timeout")

		// Clean up: stop the sleep, then archive the session
		pool.send(Msg{"type": "stop", "sessionId": sid})
		pool.sendLong(
			Msg{"type": "wait", "sessionId": sid, "timeout": 120000},
			150*time.Second,
		)
		pool.send(Msg{"type": "archive", "sessionId": sid})
	})

	t.Run("input sends raw bytes and verifiable text", func(t *testing.T) {
		// Type a visible string into s1's terminal, then capture buffer to verify it arrived
		resp := pool.send(Msg{"type": "input", "sessionId": s1, "data": "test_input_marker"})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		// Give the terminal a moment to reflect the input, then check the buffer
		time.Sleep(500 * time.Millisecond)
		bufResp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-full"})
		assertNotError(t, bufResp)
		assertContains(t, strVal(bufResp, "content"), "test_input_marker")

		// Clear the typed text so it doesn't interfere (Ctrl-U clears line)
		pool.send(Msg{"type": "input", "sessionId": s1, "data": "\x15"})

		// Offload s1 so it has no live terminal — input must error
		offloadResp := pool.send(Msg{"type": "offload", "sessionId": s1})
		assertNotError(t, offloadResp)

		resp = pool.send(Msg{"type": "input", "sessionId": s1, "data": "\x1b"})
		assertError(t, resp)

		// Restore s1 via followup so the next step can use it
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly the text: restored"})
		pool.sendLong(Msg{"type": "wait", "sessionId": s1, "timeout": 120000}, 150*time.Second)
	})

	t.Run("session prefix resolution", func(t *testing.T) {
		// The protocol allows addressing sessions by unique prefix of their ID
		prefix := s1[:3]
		resp := pool.send(Msg{"type": "info", "sessionId": prefix})

		if resp["type"] == "error" {
			errMsg := strVal(resp, "error")
			if strings.Contains(strings.ToLower(errMsg), "ambiguous") {
				t.Skip("prefix is ambiguous — multiple sessions share this prefix")
			}
			t.Fatalf("prefix resolution failed: %s", errMsg)
		}

		assertType(t, resp, "session")
		session := parseSession(t, resp["session"])
		if session.SessionID != s1 {
			t.Fatalf("prefix resolved to wrong session: got %s, want %s",
				session.SessionID, s1)
		}
	})
}
