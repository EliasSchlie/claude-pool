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
//   3a. "set-metadata merge semantics"
//   3b. "set-metadata clear with null"
//   3c. "set-metadata type validation"
//   3d. "metadata visible in ls"
//   4.  "cwd updates on directory change"
//   5.  "wait on already-idle returns immediately"
//   6.  "capture — detail=tools"
//   7.  "capture — full transcript >= default"
//   8.  "followup to idle session"
//   9.  "detail=tools < full transcript"
//   10. "capture: turns=1 detail=last returns only last turn"
//   11. "capture: turns=0 detail=last returns all turns"
//   12. "capture: detail=raw returns unfiltered JSONL with metadata"
//   13. "capture: detail=assistant returns all assistant text"
//   14. "capture: default params match turns=1 detail=last"
//   15. "capture: buffer turns=1 excludes earlier turn content"
//   16. "capture: buffer turns=0 contains all terminal output"
//   17. "capture: buffer ignores detail parameter"
//   18. "followup on processing errors without force"
//   19. "followup with force on processing"
//   20. "wait on offloaded session errors"
//   21. "wait with no sessionId — returns first idle"
//   22. "wait with no sessionId — errors if none busy"
//   23. "wait with timeout"
//   24. "input sends raw bytes and verifiable text"
//   25. "session prefix resolution"

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
		// s1 was started without metadata — should be empty
		if info.Metadata.Name != "" {
			t.Fatalf("expected empty metadata name, got %q", info.Metadata.Name)
		}
		if info.Metadata.Tags != nil {
			t.Fatalf("expected nil metadata tags, got %v", info.Metadata.Tags)
		}
	})

	t.Run("set-metadata merge semantics", func(t *testing.T) {
		// Set name and tags
		resp := pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"name": "test session",
			"tags": Msg{"env": "test", "project": "pool"},
		}})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if info.Metadata.Name != "test session" {
			t.Fatalf("expected name %q, got %q", "test session", info.Metadata.Name)
		}
		if info.Metadata.Tags["env"] != "test" || info.Metadata.Tags["project"] != "pool" {
			t.Fatalf("expected tags {env:test, project:pool}, got %v", info.Metadata.Tags)
		}

		// Merge: set description without touching name or tags
		resp = pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"description": "integration test session",
		}})
		assertNotError(t, resp)

		info = parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if info.Metadata.Name != "test session" {
			t.Fatalf("merge should preserve name, got %q", info.Metadata.Name)
		}
		if info.Metadata.Description != "integration test session" {
			t.Fatalf("expected description set, got %q", info.Metadata.Description)
		}
		if info.Metadata.Tags["env"] != "test" {
			t.Fatalf("merge should preserve tags, got %v", info.Metadata.Tags)
		}

		// Merge tags: add a key, leave existing keys
		resp = pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"tags": Msg{"owner": "ci"},
		}})
		assertNotError(t, resp)

		info = parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if info.Metadata.Tags["owner"] != "ci" {
			t.Fatalf("expected new tag owner=ci, got %v", info.Metadata.Tags)
		}
		if info.Metadata.Tags["env"] != "test" {
			t.Fatalf("existing tags should be preserved, got %v", info.Metadata.Tags)
		}
	})

	t.Run("set-metadata clear with null", func(t *testing.T) {
		// Clear description with null
		resp := pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"description": nil,
		}})
		assertNotError(t, resp)

		info := parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if info.Metadata.Description != "" {
			t.Fatalf("expected description cleared, got %q", info.Metadata.Description)
		}
		if info.Metadata.Name != "test session" {
			t.Fatalf("null on description should not affect name, got %q", info.Metadata.Name)
		}

		// Delete a single tag with null
		resp = pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"tags": Msg{"owner": nil},
		}})
		assertNotError(t, resp)

		info = parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if _, exists := info.Metadata.Tags["owner"]; exists {
			t.Fatalf("expected tag 'owner' deleted, got %v", info.Metadata.Tags)
		}
		if info.Metadata.Tags["env"] != "test" {
			t.Fatalf("other tags should be preserved, got %v", info.Metadata.Tags)
		}
	})

	t.Run("set-metadata type validation", func(t *testing.T) {
		resp := pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"name": 123,
		}})
		assertError(t, resp)
		assertContains(t, strVal(resp, "error"), "string")

		resp = pool.send(Msg{"type": "set-metadata", "sessionId": s1, "metadata": Msg{
			"tags": Msg{"bad": 456},
		}})
		assertError(t, resp)
		assertContains(t, strVal(resp, "error"), "string")

		// Verify metadata wasn't corrupted by the failed requests
		info := parseSession(t, pool.send(Msg{"type": "info", "sessionId": s1})["session"])
		if info.Metadata.Name != "test session" {
			t.Fatalf("failed set-metadata should not change name, got %q", info.Metadata.Name)
		}
	})

	t.Run("metadata visible in ls", func(t *testing.T) {
		resp := pool.send(Msg{"type": "ls", "all": true})
		assertNotError(t, resp)

		sessions := parseSessions(t, resp)
		found := false
		for _, s := range sessions {
			if s.SessionID == s1 {
				found = true
				if s.Metadata.Name != "test session" {
					t.Fatalf("ls should include metadata, got name=%q", s.Metadata.Name)
				}
				break
			}
		}
		if !found {
			t.Fatalf("s1 not found in ls results")
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
		// s1 is already idle from step 4 (cwd followup) — wait should return without blocking
		resp := pool.sendLong(
			Msg{"type": "wait", "sessionId": s1, "timeout": 5000},
			10*time.Second,
		)
		assertNotError(t, resp)
		assertNonEmpty(t, "wait-on-idle content", strVal(resp, "content"))
	})

	t.Run("capture — detail=tools", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "tools"})
		assertNotError(t, resp)
		assertNonEmpty(t, "detail=tools content", strVal(resp, "content"))
	})

	t.Run("capture — full transcript >= default", func(t *testing.T) {
		defaultResp := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertNotError(t, defaultResp)
		defaultContent := strVal(defaultResp, "content")
		assertNonEmpty(t, "default content", defaultContent)

		rawResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 0, "detail": "raw"})
		assertNotError(t, rawResp)
		rawContent := strVal(rawResp, "content")

		// Full unfiltered transcript should always be >= single-turn default
		if len(rawContent) < len(defaultContent) {
			t.Fatalf("turns=0 detail=raw (%d bytes) should be >= default (%d bytes)",
				len(rawContent), len(defaultContent))
		}
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

	t.Run("detail=tools < full transcript", func(t *testing.T) {
		// s1 now has multiple turns — detail=tools (last turn, stripped) should be
		// smaller than turns=0 detail=raw (complete unfiltered transcript)
		toolsResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "tools"})
		assertNotError(t, toolsResp)
		toolsContent := strVal(toolsResp, "content")

		rawResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 0, "detail": "raw"})
		assertNotError(t, rawResp)
		rawContent := strVal(rawResp, "content")

		if len(toolsContent) >= len(rawContent) {
			t.Fatalf("detail=tools turns=1 (%d bytes) should be smaller than detail=raw turns=0 (%d bytes)",
				len(toolsContent), len(rawContent))
		}
	})

	// --- Capture API tests (source/turns/detail) ---
	// s1 now has 3 turns. Turn 1: "hello world", Turn 2: cwd change, Turn 3 (latest): "goodbye"

	t.Run("capture: turns=1 detail=last returns only last turn", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "last"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")
		if strings.Contains(strings.ToLower(content), "hello world") {
			t.Fatalf("turns=1 should exclude earlier turns, but 'hello world' is present:\n%s", truncate(content, 500))
		}
	})

	t.Run("capture: turns=0 detail=last returns all turns", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 0, "detail": "last"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "hello world")
		assertContains(t, content, "goodbye")
	})

	t.Run("capture: detail=raw returns unfiltered JSONL with metadata", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "raw"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		// Raw should have metadata fields that other detail levels strip
		assertContains(t, content, "stop_reason")
	})

	t.Run("capture: detail=assistant returns all assistant text", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "assistant"})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")

		// detail=assistant should be smaller than detail=raw (no metadata)
		rawResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "raw"})
		assertNotError(t, rawResp)
		rawContent := strVal(rawResp, "content")
		if len(content) >= len(rawContent) {
			t.Fatalf("detail=assistant (%d bytes) should be smaller than detail=raw (%d bytes)",
				len(content), len(rawContent))
		}
	})

	t.Run("capture: default params match turns=1 detail=last", func(t *testing.T) {
		// No source/turns/detail → should default to jsonl, 1, last
		defaultResp := pool.send(Msg{"type": "capture", "sessionId": s1})
		explicitResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "turns": 1, "detail": "last"})
		assertNotError(t, defaultResp)
		assertNotError(t, explicitResp)
		if strVal(defaultResp, "content") != strVal(explicitResp, "content") {
			t.Fatalf("default capture should equal explicit turns=1,detail=last\ndefault: %s\nexplicit: %s",
				truncate(strVal(defaultResp, "content"), 300),
				truncate(strVal(explicitResp, "content"), 300))
		}
	})

	t.Run("capture: buffer turns=1 excludes earlier turn content", func(t *testing.T) {
		// s1 has 3 turns. buffer turns=1 should return terminal output since the last user message.
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
		if strings.Contains(strings.ToLower(bufLastContent), "respond with exactly the text: hello world") {
			t.Fatalf("buffer turns=1 should not contain turn 1's prompt, but it does:\n%s",
				truncate(bufLastContent, 500))
		}
	})

	t.Run("capture: buffer turns=0 contains all terminal output", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 0})
		assertNotError(t, resp)
		content := strVal(resp, "content")
		// Full buffer should contain output from both turns
		assertContains(t, content, "hello world")
		assertContains(t, content, "goodbye")
	})

	t.Run("capture: buffer ignores detail parameter", func(t *testing.T) {
		// Spec: "For buffer source, detail is ignored — buffer is always raw terminal text."
		bufDefault := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 1})
		bufWithDetail := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 1, "detail": "raw"})
		assertNotError(t, bufDefault)
		assertNotError(t, bufWithDetail)
		if strVal(bufDefault, "content") != strVal(bufWithDetail, "content") {
			t.Fatal("buffer should return identical content regardless of detail parameter")
		}
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
		bufResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 0})
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
