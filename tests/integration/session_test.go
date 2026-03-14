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
//   4.  "cwd updates on directory change"
//   5.  "wait on already-idle returns immediately"
//   6.  "capture while idle — jsonl-short (default)"
//   7.  "capture — jsonl-last"
//   8.  "capture — jsonl-long"
//   9.  "capture — jsonl-full"
//   10. "capture — buffer-last"
//   11. "capture — buffer-full"
//   12. "followup to idle session"
//   13. "jsonl-short after followup excludes earlier turns"
//   14. "jsonl-long strips repetitive fields"
//   15. "new-capture: turns=1 detail=last returns only last turn"
//   16. "new-capture: turns=0 detail=last returns all turns"
//   17. "new-capture: detail=raw returns unfiltered JSONL with metadata"
//   18. "new-capture: default params match turns=1 detail=last"
//   19. "new-capture: buffer turns=1 excludes earlier turn content"
//   20. "new-capture: buffer turns=0 contains all terminal output"
//   21. "followup on processing errors without force"
//   22. "followup with force on processing"
//   23. "wait on offloaded session errors"
//   24. "wait with no sessionId — returns first idle"
//   25. "wait with no sessionId — errors if none busy"
//   26. "wait with timeout"
//   27. "input sends raw bytes and verifiable text"
//   28. "session prefix resolution"

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
			Msg{"type": "wait", "sessionId": s1, "timeout": 60000},
			75*time.Second,
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

	t.Run("cwd updates on directory change", func(t *testing.T) {
		// Record initial cwd
		info := pool.send(Msg{"type": "info", "sessionId": s1})
		before := parseSession(t, info["session"])
		initialCwd := before.Cwd

		// Create a subdirectory and ask Claude to cd into it
		// (Claude sessions can only access local dirs, not system dirs like /tmp)
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "run these bash commands: mkdir -p cwd_test_dir && cd cwd_test_dir"})
		pool.sendLong(
			Msg{"type": "wait", "sessionId": s1, "timeout": 60000},
			75*time.Second,
		)

		info = pool.send(Msg{"type": "info", "sessionId": s1})
		after := parseSession(t, info["session"])
		if after.Cwd == initialCwd {
			t.Fatalf("cwd should have changed after cd, still %q", after.Cwd)
		}
		assertContains(t, after.Cwd, "cwd_test_dir")

		// spawnCwd must NOT change — it's immutable
		if after.SpawnCwd != before.SpawnCwd {
			t.Fatalf("spawnCwd should be immutable: was %q, now %q", before.SpawnCwd, after.SpawnCwd)
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

	var shortContent string

	t.Run("capture while idle — jsonl-short default", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "result")
		shortContent = strVal(resp, "content")
		assertNonEmpty(t, "capture content", shortContent)
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
			Msg{"type": "wait", "sessionId": s1, "timeout": 60000},
			75*time.Second,
		)
		assertNotError(t, waitResp)
		assertContains(t, strVal(waitResp, "content"), "goodbye")
	})

	t.Run("jsonl-short after followup excludes earlier turns", func(t *testing.T) {
		// s1 now has 2+ turns: "hello world" from step 1 and "goodbye" from
		// step 12. SPEC says jsonl-short = "all assistant messages since last
		// user message" — so only the latest turn should appear.
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-short"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")
		if strings.Contains(strings.ToLower(content), "hello world") {
			t.Fatalf("jsonl-short should only return messages since last user message, but earlier turn's 'hello world' is present:\n%s", truncate(content, 500))
		}
	})

	t.Run("jsonl-long strips repetitive fields", func(t *testing.T) {
		longResp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-long"})
		assertNotError(t, longResp)
		longContent := strVal(longResp, "content")

		fullResp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-full"})
		assertNotError(t, fullResp)
		fullContent := strVal(fullResp, "content")

		// jsonl-long filters to since-last-user AND strips repetitive fields,
		// so it must be strictly smaller than jsonl-full (complete unfiltered)
		if len(longContent) >= len(fullContent) {
			t.Fatalf("jsonl-long (%d bytes) should be smaller than jsonl-full (%d bytes) — repetitive fields should be stripped",
				len(longContent), len(fullContent))
		}
	})

	// --- New capture API tests (source/turns/detail) ---
	// s1 now has 2+ turns. Turn 1: "hello world", Turn 2 (latest): "goodbye"

	t.Run("new-capture: turns=1 detail=last returns only last turn", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "last"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")
		if strings.Contains(strings.ToLower(content), "hello world") {
			t.Fatalf("turns=1 should exclude earlier turns, but 'hello world' is present:\n%s", truncate(content, 500))
		}
	})

	t.Run("new-capture: turns=0 detail=last returns all turns", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 0, "detail": "last"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "hello world")
		assertContains(t, content, "goodbye")
	})

	t.Run("new-capture: detail=raw returns unfiltered JSONL with metadata", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "raw"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		// Raw should have metadata fields that other detail levels strip
		assertContains(t, content, "stop_reason")
	})

	t.Run("new-capture: default params match turns=1 detail=last", func(t *testing.T) {
		// No source/turns/detail → should default to jsonl, 1, last
		defaultResp := pool.send(Msg{"type": "capture", "sessionId": s1})
		explicitResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "last"})
		assertNotError(t, defaultResp)
		assertNotError(t, explicitResp)
		// Both should return the same content
		if strVal(defaultResp, "content") != strVal(explicitResp, "content") {
			t.Fatalf("default capture should equal explicit turns=1,detail=last\ndefault: %s\nexplicit: %s",
				truncate(strVal(defaultResp, "content"), 300),
				truncate(strVal(explicitResp, "content"), 300))
		}
	})

	t.Run("new-capture: buffer turns=1 excludes earlier turn content", func(t *testing.T) {
		// s1 has 2 turns. Turn 1 prompt contained "hello world", turn 2 "goodbye".
		// buffer turns=1 should return terminal output since the last user message.
		// The first turn's prompt text should NOT appear in the filtered buffer.
		bufAll := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 0})
		assertNotError(t, bufAll)
		bufAllContent := strVal(bufAll, "content")

		bufLast := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 1})
		assertNotError(t, bufLast)
		bufLastContent := strVal(bufLast, "content")

		// buffer turns=0 should be larger (full scrollback)
		if len(bufLastContent) >= len(bufAllContent) {
			t.Fatalf("buffer turns=0 (%d bytes) should be larger than turns=1 (%d bytes)",
				len(bufAllContent), len(bufLastContent))
		}

		// The first turn's user prompt should be in the full buffer but not the last-turn buffer.
		// "hello world" was the text we asked Claude to respond with, and it appeared in the
		// terminal output. The exact prompt "respond with exactly the text: hello world" should
		// be in the full buffer from turn 1.
		if strings.Contains(strings.ToLower(bufLastContent), "respond with exactly the text: hello world") {
			t.Fatalf("buffer turns=1 should not contain turn 1's prompt, but it does:\n%s",
				truncate(bufLastContent, 500))
		}
	})

	t.Run("new-capture: buffer turns=0 contains all terminal output", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 0})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		// Full buffer should contain output from both turns
		assertContains(t, content, "hello world")
		assertContains(t, content, "goodbye")
	})

	var s2 string

	t.Run("followup on processing errors without force", func(t *testing.T) {
		// Use a bash sleep to keep s2 processing without burning tokens
		startResp := pool.send(Msg{"type": "start", "prompt": "run the bash command: sleep 60"})
		assertNotError(t, startResp)
		s2 = strVal(startResp, "sessionId")

		// Wait until s2 is actually processing before testing followup rejection
		pool.awaitStatus(s2, "processing", 15*time.Second)

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
			Msg{"type": "wait", "sessionId": s2, "timeout": 60000},
			75*time.Second,
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
		resp := pool.sendLong(Msg{"type": "wait", "timeout": 60000}, 75*time.Second)
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
			Msg{"type": "wait", "sessionId": other, "timeout": 60000},
			75*time.Second,
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

		pool.awaitStatus(sid, "processing", 15*time.Second)

		// 1ms timeout — guaranteed to expire before sleep finishes
		resp := pool.sendLong(Msg{"type": "wait", "sessionId": sid, "timeout": 1}, 5*time.Second)
		assertError(t, resp)
		assertContains(t, strVal(resp, "error"), "timeout")

		// Clean up: stop is synchronous (session idle on return), then archive
		pool.send(Msg{"type": "stop", "sessionId": sid})
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
		pool.sendLong(Msg{"type": "wait", "sessionId": s1, "timeout": 60000}, 75*time.Second)
	})

	t.Run("session prefix resolution", func(t *testing.T) {
		// The protocol allows addressing sessions by unique prefix of their ID.
		// Use 6 chars — collision with <10 sessions is astronomically unlikely.
		prefix := s1[:6]
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
