package integration

// TestSession — Core session workflow flow
//
// Pool size: 3
//
// This flow tests the fundamental session operations: starting sessions, waiting
// for results, reading output in different formats, sending followups, and raw
// input. It exercises the most common usage patterns.
//
// Flow:
//
//   1. "start returns sessionId and status"
//   2. "wait returns result"
//   3. "info shows session details"
//   4. "wait on already-idle returns immediately"
//   5. "capture while idle — jsonl-short (default)"
//   6. "capture — jsonl-last"
//   7. "capture — jsonl-long"
//   8. "capture — jsonl-full"
//   9. "capture — buffer-last"
//  10. "capture — buffer-full"
//  11. "followup to idle session"
//  12. "followup on processing errors without force"
//  13. "followup with force on processing"
//  14. "wait with no sessionId — returns first idle"
//  15. "wait with no sessionId — errors if none busy"
//  16. "wait with timeout"
//  17. "input sends raw bytes"
//  18. "session prefix resolution"

import (
	"strings"
	"testing"
)

func TestSession(t *testing.T) {
	pool := setupPool(t, 3)

	var s1 string

	t.Run("start returns sessionId and status", func(t *testing.T) {
		resp := pool.startRaw("respond with exactly the text: hello world")
		assertNotError(t, resp)
		if resp["type"] != "started" {
			t.Fatalf("expected type started, got %v", resp["type"])
		}
		s1 = strVal(resp, "sessionId")
		assertNonEmpty(t, "sessionId", s1)
		status := strVal(resp, "status")
		// Should be processing or queued (processing expected with 3 slots and 1 session)
		if status != "processing" && status != "queued" {
			t.Fatalf("expected status processing or queued, got %q", status)
		}
	})

	t.Run("wait returns result", func(t *testing.T) {
		content := pool.waitIdle(s1)
		assertContains(t, content, "hello world")
	})

	t.Run("info shows session details", func(t *testing.T) {
		info := pool.info(s1)
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
		content := pool.waitIdle(s1)
		// Should return immediately with the previous response
		assertContains(t, content, "hello world")
	})

	t.Run("capture while idle — jsonl-short default", func(t *testing.T) {
		content := pool.capture(s1)
		assertNonEmpty(t, "capture content", content)
	})

	t.Run("capture — jsonl-last", func(t *testing.T) {
		content := pool.captureFormat(s1, "jsonl-last")
		assertNonEmpty(t, "jsonl-last content", content)
	})

	t.Run("capture — jsonl-long", func(t *testing.T) {
		content := pool.captureFormat(s1, "jsonl-long")
		assertNonEmpty(t, "jsonl-long content", content)
	})

	t.Run("capture — jsonl-full", func(t *testing.T) {
		content := pool.captureFormat(s1, "jsonl-full")
		assertNonEmpty(t, "jsonl-full content", content)
		// jsonl-full should be the longest format (complete transcript)
		shortContent := pool.captureFormat(s1, "jsonl-short")
		if len(content) < len(shortContent) {
			t.Fatalf("jsonl-full (%d bytes) should be >= jsonl-short (%d bytes)",
				len(content), len(shortContent))
		}
	})

	t.Run("capture — buffer-last", func(t *testing.T) {
		content := pool.captureFormat(s1, "buffer-last")
		assertNonEmpty(t, "buffer-last content", content)
	})

	t.Run("capture — buffer-full", func(t *testing.T) {
		content := pool.captureFormat(s1, "buffer-full")
		assertNonEmpty(t, "buffer-full content", content)
	})

	t.Run("followup to idle session", func(t *testing.T) {
		resp := pool.followup(s1, "respond with exactly the text: goodbye")
		sid := strVal(resp, "sessionId")
		if sid != s1 {
			t.Fatalf("followup returned different sessionId: got %s, want %s", sid, s1)
		}
		// followup response should include status
		status := strVal(resp, "status")
		assertNonEmpty(t, "followup status", status)

		content := pool.waitIdle(s1)
		assertContains(t, content, "goodbye")
	})

	var s2 string

	t.Run("followup on processing errors without force", func(t *testing.T) {
		s2 = pool.start("write a 200-word essay about the history of trees in urban environments")
		// s2 should be processing — immediately try followup
		resp := pool.followupRaw(s2, "ignore that")
		assertError(t, resp)
		// Error message should mention processing or force
		errMsg := strVal(resp, "error")
		if !strings.Contains(strings.ToLower(errMsg), "processing") &&
			!strings.Contains(strings.ToLower(errMsg), "force") {
			t.Logf("warning: error message doesn't mention processing/force: %s", errMsg)
		}
	})

	t.Run("followup with force on processing", func(t *testing.T) {
		resp := pool.followupForce(s2, "respond with exactly the text: interrupted")
		sid := strVal(resp, "sessionId")
		if sid != s2 {
			t.Fatalf("expected sessionId %s, got %s", s2, sid)
		}
		content := pool.waitIdle(s2)
		assertContains(t, content, "interrupted")
	})

	t.Run("wait with no sessionId — returns first idle", func(t *testing.T) {
		s3 := pool.start("respond with exactly the text: first")
		s4 := pool.start("respond with exactly the text: second")

		sid, content := pool.waitAny()
		// Should be one of the two
		if sid != s3 && sid != s4 {
			t.Fatalf("waitAny returned unknown session %s (expected %s or %s)", sid, s3, s4)
		}
		assertNonEmpty(t, "waitAny content", content)

		// Wait for the other one too (cleanup)
		other := s3
		if sid == s3 {
			other = s4
		}
		pool.waitIdle(other)
	})

	t.Run("wait with no sessionId — errors if none busy", func(t *testing.T) {
		// All sessions should be idle at this point
		resp := pool.waitWithTimeout("", 5000)
		assertError(t, resp)
	})

	t.Run("wait with timeout", func(t *testing.T) {
		sid := pool.start("write a very long and detailed 500-word essay about quantum computing")
		// Wait with 1ms timeout — should expire immediately
		resp := pool.waitWithTimeout(sid, 1)
		assertError(t, resp)
		errMsg := strVal(resp, "error")
		assertContains(t, errMsg, "timeout")
		// Let it finish so we don't leave a processing session
		pool.waitIdle(sid)
	})

	t.Run("input sends raw bytes", func(t *testing.T) {
		// s1 is idle — input should succeed
		pool.input(s1, "\x1b") // Escape — harmless on idle

		// Offload s1, then try input — should error
		pool.offload(s1)
		resp := pool.send(Msg{"type": "input", "sessionId": s1, "data": "\x1b"})
		assertError(t, resp)

		// Restore s1 for the next test
		pool.followup(s1, "respond with exactly the text: restored")
		pool.waitIdle(s1)
	})

	t.Run("session prefix resolution", func(t *testing.T) {
		// Use first 3 chars of s1 as prefix
		prefix := s1[:3]
		resp := pool.infoRaw(prefix)
		// If it resolves uniquely, should succeed
		if resp["type"] == "error" {
			errMsg := strVal(resp, "error")
			if strings.Contains(strings.ToLower(errMsg), "ambiguous") {
				t.Skip("prefix is ambiguous — multiple sessions share this prefix")
			}
			t.Fatalf("prefix resolution failed: %s", errMsg)
		}
		session := parseSession(t, resp["session"])
		if session.SessionID != s1 {
			t.Fatalf("prefix resolved to wrong session: got %s, want %s",
				session.SessionID, s1)
		}
	})
}
