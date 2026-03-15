package integration

// TestSession — Core session workflow flow (CLI)
//
// Pool size: 3
//
// Tests fundamental session operations through the CLI: starting sessions, waiting
// for results, reading output in different formats, sending followups, metadata,
// and session lifecycle.
//
// Flow:
//
//   1.  "start returns sessionId and status"
//   2.  "wait returns content"
//   3.  "info shows session details"
//   4.  "set metadata via CLI"
//   5.  "metadata visible in ls"
//   6.  "cwd updates on directory change"
//   7.  "wait on already-idle returns immediately"
//   8.  "capture default"
//   9.  "capture detail=tools"
//  10.  "capture turns=0 detail=raw >= default"
//  11.  "followup to idle session"
//  12.  "capture turns=1 excludes earlier turns"
//  13.  "capture turns=0 includes all turns"
//  14.  "capture detail=raw includes metadata"
//  15.  "capture detail=assistant"
//  16.  "capture default matches explicit turns=1 detail=last"
//  17.  "capture buffer turns=1 vs turns=0"
//  18.  "capture buffer ignores detail parameter"
//  19.  "followup on processing errors"
//  20.  "stop interrupts processing session"
//  21.  "start with block"
//  22.  "wait with timeout"
//  23.  "session prefix resolution"
//  24.  "followup with block"
//  25.  "start with block and output flags"
//  26.  "stop on idle errors"
//  27.  "human-readable output"

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSession(t *testing.T) {
	pool := setupPool(t, 3)

	var s1 string

	t.Run("start returns sessionId and status", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly the text: hello world")
		s1 = strVal(resp, "sessionId")
		assertNonEmpty(t, "sessionId", s1)

		status := strVal(resp, "status")
		if status != "processing" && status != "queued" {
			t.Fatalf("expected status processing or queued, got %q", status)
		}
	})

	t.Run("wait returns content", func(t *testing.T) {
		resp := pool.waitForIdle(s1, 300*time.Second)
		assertContains(t, strVal(resp, "content"), "hello world")
	})

	t.Run("info shows session details", func(t *testing.T) {
		info := pool.getSessionInfo(s1)
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

	t.Run("set metadata via CLI", func(t *testing.T) {
		result := pool.run("set", "--session", s1, "--meta", "env=test", "--meta", "project=pool")
		assertExitOK(t, result)

		info := pool.getSessionInfo(s1)
		if info.Metadata["env"] != "test" {
			t.Fatalf("expected meta env=test, got %v", info.Metadata)
		}
		if info.Metadata["project"] != "pool" {
			t.Fatalf("expected meta project=pool, got %v", info.Metadata)
		}

		// Additional set merges, doesn't replace
		pool.run("set", "--session", s1, "--meta", "owner=ci")
		info = pool.getSessionInfo(s1)
		if info.Metadata["env"] != "test" {
			t.Fatalf("merge should preserve existing keys, got %v", info.Metadata)
		}
		if info.Metadata["owner"] != "ci" {
			t.Fatalf("expected meta owner=ci, got %v", info.Metadata)
		}
	})

	t.Run("metadata visible in ls", func(t *testing.T) {
		sessions := pool.listSessions("--verbosity", "full")
		found := false
		for _, s := range sessions {
			if s.SessionID == s1 {
				found = true
				if s.Metadata["env"] != "test" {
					t.Fatalf("ls should include metadata, got %v", s.Metadata)
				}
				break
			}
		}
		if !found {
			t.Fatal("s1 not found in ls")
		}
	})

	t.Run("cwd updates on directory change", func(t *testing.T) {
		before := pool.getSessionInfo(s1)
		initialCwd := before.Cwd

		pool.run("followup", "--session", s1, "--prompt", "run these bash commands: mkdir -p cwd_test_dir && cd cwd_test_dir")
		pool.waitForIdle(s1, 300*time.Second)

		after := pool.getSessionInfo(s1)
		if after.Cwd == initialCwd {
			t.Fatalf("cwd should have changed, still %q", after.Cwd)
		}
		assertContains(t, after.Cwd, "cwd_test_dir")

		if after.SpawnCwd != before.SpawnCwd {
			t.Fatalf("spawnCwd should be immutable: was %q, now %q", before.SpawnCwd, after.SpawnCwd)
		}
	})

	t.Run("wait on already-idle returns immediately", func(t *testing.T) {
		resp := pool.waitForIdle(s1, 5*time.Second)
		assertNonEmpty(t, "wait-on-idle content", strVal(resp, "content"))
	})

	t.Run("capture default", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1)
		assertNonEmpty(t, "default capture", strVal(resp, "content"))
	})

	t.Run("capture detail=tools", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "tools")
		assertNonEmpty(t, "detail=tools content", strVal(resp, "content"))
	})

	t.Run("capture turns=0 detail=raw >= default", func(t *testing.T) {
		defaultResp := pool.runJSON("capture", "--session", s1)
		defaultContent := strVal(defaultResp, "content")

		rawResp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "0", "--detail", "raw")
		rawContent := strVal(rawResp, "content")

		if len(rawContent) < len(defaultContent) {
			t.Fatalf("turns=0 detail=raw (%d bytes) should be >= default (%d bytes)",
				len(rawContent), len(defaultContent))
		}
	})

	t.Run("followup to idle session", func(t *testing.T) {
		resp := pool.runJSON("followup", "--session", s1, "--prompt", "respond with exactly the text: goodbye")
		if strVal(resp, "sessionId") != s1 {
			t.Fatalf("followup returned different sessionId")
		}

		waitResp := pool.waitForIdle(s1, 300*time.Second)
		assertContains(t, strVal(waitResp, "content"), "goodbye")
	})

	// s1 now has 3 turns: "hello world", cwd change, "goodbye"

	t.Run("capture turns=1 excludes earlier turns", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "last")
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")
		if strings.Contains(strings.ToLower(content), "hello world") {
			t.Fatalf("turns=1 should exclude earlier turns")
		}
	})

	t.Run("capture turns=0 includes all turns", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "0", "--detail", "last")
		content := strVal(resp, "content")
		assertContains(t, content, "hello world")
		assertContains(t, content, "goodbye")
	})

	t.Run("capture detail=raw includes metadata", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "raw")
		content := strVal(resp, "content")
		assertContains(t, content, "stop_reason")
	})

	t.Run("capture detail=assistant", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "assistant")
		content := strVal(resp, "content")
		assertContains(t, content, "goodbye")

		rawResp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "raw")
		rawContent := strVal(rawResp, "content")
		if len(content) >= len(rawContent) {
			t.Fatalf("detail=assistant (%d) should be smaller than detail=raw (%d)", len(content), len(rawContent))
		}
	})

	t.Run("capture default matches explicit turns=1 detail=last", func(t *testing.T) {
		defaultResp := pool.runJSON("capture", "--session", s1)
		explicitResp := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--turns", "1", "--detail", "last")
		if strVal(defaultResp, "content") != strVal(explicitResp, "content") {
			t.Fatal("default capture should equal explicit turns=1,detail=last")
		}
	})

	t.Run("capture buffer turns=1 vs turns=0", func(t *testing.T) {
		bufAll := pool.runJSON("capture", "--session", s1, "--source", "buffer", "--turns", "0")
		bufLast := pool.runJSON("capture", "--session", s1, "--source", "buffer", "--turns", "1")

		bufAllContent := strVal(bufAll, "content")
		bufLastContent := strVal(bufLast, "content")

		if len(bufLastContent) >= len(bufAllContent) {
			t.Fatalf("buffer turns=0 (%d) should be larger than turns=1 (%d)",
				len(bufAllContent), len(bufLastContent))
		}
	})

	t.Run("capture buffer ignores detail parameter", func(t *testing.T) {
		bufDefault := pool.runJSON("capture", "--session", s1, "--source", "buffer", "--turns", "1")
		bufWithDetail := pool.runJSON("capture", "--session", s1, "--source", "buffer", "--turns", "1", "--detail", "raw")
		if strVal(bufDefault, "content") != strVal(bufWithDetail, "content") {
			t.Fatal("buffer should return identical content regardless of detail")
		}
	})

	var s2 string

	t.Run("followup on processing errors", func(t *testing.T) {
		startResp := pool.runJSON("start", "--prompt", "run the bash command: sleep 60")
		s2 = strVal(startResp, "sessionId")
		pool.waitForStatus(s2, "processing", 15*time.Second)

		result := pool.run("followup", "--session", s2, "--prompt", "ignore", "--json")
		assertExitError(t, result)
	})

	t.Run("stop interrupts processing session", func(t *testing.T) {
		result := pool.run("stop", "--session", s2)
		assertExitOK(t, result)

		pool.waitForStatus(s2, "idle", 15*time.Second)
		pool.run("archive", "--session", s2)
	})

	t.Run("start with block", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: blocked", "--block")
		assertContains(t, strVal(resp, "content"), "blocked")

		sid := strVal(resp, "sessionId")
		assertNonEmpty(t, "block sessionId", sid)

		pool.run("archive", "--session", sid)
	})

	t.Run("wait with timeout", func(t *testing.T) {
		startResp := pool.runJSON("start", "--prompt", "run the bash command: sleep 60")
		sid := strVal(startResp, "sessionId")
		pool.waitForStatus(sid, "processing", 15*time.Second)

		result := pool.run("wait", "--session", sid, "--timeout", "1", "--json")
		assertExitError(t, result)

		pool.run("stop", "--session", sid)
		pool.run("archive", "--session", sid)
	})

	t.Run("session prefix resolution", func(t *testing.T) {
		prefix := s1[:6]
		result := pool.run("info", "--session", prefix, "--json")
		if result.ExitCode != 0 {
			if strings.Contains(strings.ToLower(result.Stderr), "ambiguous") {
				t.Skip("prefix is ambiguous")
			}
			t.Fatalf("prefix resolution failed: %s", result.Stderr)
		}

		var resp Msg
		json.Unmarshal([]byte(result.Stdout), &resp)
		session, _ := resp["session"].(map[string]any)
		if strVal(session, "sessionId") != s1 {
			t.Fatalf("prefix resolved to wrong session: got %s, want %s",
				strVal(session, "sessionId"), s1)
		}
	})

	t.Run("followup with block", func(t *testing.T) {
		resp := pool.runJSON("followup", "--session", s1, "--prompt", "respond with exactly: follow-blocked", "--block")
		assertContains(t, strVal(resp, "content"), "follow-blocked")
	})

	t.Run("start with block and output flags", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: block-detailed", "--block", "--detail", "assistant")
		content := strVal(resp, "content")
		assertContains(t, content, "block-detailed")
		sid := strVal(resp, "sessionId")
		pool.run("archive", "--session", sid)
	})

	t.Run("stop on idle errors", func(t *testing.T) {
		result := pool.run("stop", "--session", s1)
		assertExitError(t, result)
	})

	t.Run("human-readable output", func(t *testing.T) {
		result := pool.run("ping")
		assertExitOK(t, result)
		assertContains(t, result.Stdout, "pong")

		result = pool.run("info", "--session", s1)
		assertExitOK(t, result)
		if result.Stdout == "" {
			t.Fatal("expected non-empty human-readable output from info")
		}
	})
}
