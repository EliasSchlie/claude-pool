package cli

// TestEnv — Environment variable propagation and parent-child auto-detection flow
//
// Pool size: 2
//
// Tests the real parent-child workflow: when a CLI command runs with
// CLAUDE_POOL_SESSION_ID set, the daemon auto-detects the parent.
// This is the path that matters — Claude sessions spawning sub-sessions
// without explicitly threading IDs.
//
// Flow:
//
//   1. "start root session"
//      Run: claude-pool start --prompt "respond with exactly: root" --json
//      Parse sessionId as s1.
//      Run: claude-pool wait --session s1 --timeout 120000
//
//   2. "child auto-detects parent from env"
//      Run with CLAUDE_POOL_SESSION_ID=s1:
//        claude-pool start --prompt "respond with exactly: child" --json
//      Parse sessionId as s2.
//      Assert: s2's parentId is s1 (auto-detected from env var, not explicitly set).
//      Run: claude-pool wait --session s2 --timeout 120000
//
//   3. "explicit parentId overrides env"
//      Run with CLAUDE_POOL_SESSION_ID=s1:
//        claude-pool start --prompt "respond with exactly: explicit" --parent s2 --json
//      Parse sessionId as s3.
//      Assert: s3's parentId is s2 (explicit flag wins over env var).
//      Run: claude-pool wait --session s3 --timeout 120000
//
//   4. "info shows parent-child tree"
//      Run: claude-pool info --session s1 --json
//      Assert: children includes s2.
//      Run: claude-pool info --session s2 --json
//      Assert: children includes s3, parentId is s1.
//
//   5. "ls from session context shows owned children"
//      Run with CLAUDE_POOL_SESSION_ID=s1:
//        claude-pool ls --json
//      Assert: returns s2 (direct child of s1). Does NOT return s3
//      (grandchild, not direct child).
//      Run with CLAUDE_POOL_SESSION_ID=s1:
//        claude-pool ls --tree --json
//      Assert: returns s2 with s3 nested in its children.
//
//   6. "ls without session env shows all top-level"
//      Run (no CLAUDE_POOL_SESSION_ID):
//        claude-pool ls --json
//      Assert: returns s1 (top-level, no parent).
//
//   7. "real end-to-end: Claude session spawns child via CLI"
//      Start s_parent via socket API with a prompt that tells it to run:
//        claude-pool start --pool <name> --prompt "respond with exactly: spawned-child"
//      Wait for s_parent to become idle.
//      Assert: a new session appeared in the pool with parentId = s_parent
//      (the daemon set CLAUDE_POOL_SESSION_ID on s_parent's process, the CLI
//      read it, and the daemon auto-detected the parent).
//      Wait for the child session to become idle.
//      Capture child output — should contain "spawned-child".

import (
	"fmt"
	"strings"
	"testing"
)

func TestEnv(t *testing.T) {
	pool := setupCLIPool(t, 2)

	var s1, s2, s3 string

	t.Run("start root session", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: root")
		s1 = strVal(resp, "sessionId")
		if s1 == "" {
			t.Fatalf("no sessionId in start response: %v", resp)
		}

		result := pool.run("wait", "--session", s1, "--timeout", "120000")
		if result.ExitCode != 0 {
			t.Fatalf("wait exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
	})

	t.Run("child auto-detects parent from env", func(t *testing.T) {
		result := pool.runInSessionJSON(s1, "start", "--prompt", "respond with exactly: child")
		s2 = strVal(result, "sessionId")
		if s2 == "" {
			t.Fatalf("no sessionId in start response: %v", result)
		}

		// Verify parent was auto-detected from CLAUDE_POOL_SESSION_ID
		info := pool.runJSON("info", "--session", s2)
		session, _ := info["session"].(map[string]any)
		parentID := strVal(session, "parentId")
		if parentID != s1 {
			t.Fatalf("expected parentId %s (auto-detected from env), got %s", s1, parentID)
		}

		waitResult := pool.run("wait", "--session", s2, "--timeout", "120000")
		if waitResult.ExitCode != 0 {
			t.Fatalf("wait exit code %d, stderr: %s", waitResult.ExitCode, waitResult.Stderr)
		}
	})

	t.Run("explicit parentId overrides env", func(t *testing.T) {
		// CLAUDE_POOL_SESSION_ID=s1, but --parent s2 should win
		result := pool.runInSessionJSON(s1, "start", "--prompt", "respond with exactly: explicit", "--parent", s2)
		s3 = strVal(result, "sessionId")
		if s3 == "" {
			t.Fatalf("no sessionId in start response: %v", result)
		}

		info := pool.runJSON("info", "--session", s3)
		session, _ := info["session"].(map[string]any)
		parentID := strVal(session, "parentId")
		if parentID != s2 {
			t.Fatalf("expected parentId %s (explicit --parent), got %s", s2, parentID)
		}

		waitResult := pool.run("wait", "--session", s3, "--timeout", "120000")
		if waitResult.ExitCode != 0 {
			t.Fatalf("wait exit code %d, stderr: %s", waitResult.ExitCode, waitResult.Stderr)
		}
	})

	t.Run("info shows parent-child tree", func(t *testing.T) {
		info := pool.runJSON("info", "--session", s1)
		session, _ := info["session"].(map[string]any)

		children, _ := session["children"].([]any)
		found := false
		for _, c := range children {
			cm, _ := c.(map[string]any)
			if strVal(cm, "sessionId") == s2 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected s1's children to include s2 (%s)", s2)
		}

		info = pool.runJSON("info", "--session", s2)
		session, _ = info["session"].(map[string]any)
		if strVal(session, "parentId") != s1 {
			t.Fatalf("expected s2 parentId to be s1")
		}

		children, _ = session["children"].([]any)
		found = false
		for _, c := range children {
			cm, _ := c.(map[string]any)
			if strVal(cm, "sessionId") == s3 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected s2's children to include s3 (%s)", s3)
		}
	})

	t.Run("ls from session context shows owned children", func(t *testing.T) {
		// ls as s1 — should show s2 (direct child) but NOT s3 (grandchild)
		resp := pool.runInSessionJSON(s1, "ls")
		sessions, _ := resp["sessions"].([]any)

		hasS2 := false
		hasS3 := false
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			sid := strVal(sm, "sessionId")
			if sid == s2 {
				hasS2 = true
			}
			if sid == s3 {
				hasS3 = true
			}
		}
		if !hasS2 {
			t.Fatalf("expected ls from s1 context to include s2")
		}
		if hasS3 {
			t.Fatalf("expected ls from s1 context to NOT include s3 (grandchild)")
		}

		// ls --tree as s1 — s2 should have s3 nested in children
		resp = pool.runInSessionJSON(s1, "ls", "--tree")
		sessions, _ = resp["sessions"].([]any)

		foundNested := false
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			if strVal(sm, "sessionId") == s2 {
				kids, _ := sm["children"].([]any)
				for _, k := range kids {
					km, _ := k.(map[string]any)
					if strVal(km, "sessionId") == s3 {
						foundNested = true
					}
				}
			}
		}
		if !foundNested {
			t.Fatalf("expected s3 nested under s2 in tree view")
		}
	})

	t.Run("ls without session env shows all top-level", func(t *testing.T) {
		resp := pool.runJSON("ls")
		sessions, _ := resp["sessions"].([]any)

		found := false
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			if strVal(sm, "sessionId") == s1 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected ls without session env to include s1 (top-level)")
		}
	})

	t.Run("real e2e: Claude session spawns child via CLI", func(t *testing.T) {
		// The parent session runs the CLI binary directly. The daemon sets
		// CLAUDE_POOL_SESSION_ID on its env, so the CLI auto-detects the parent.
		// We also set CLAUDE_POOL_REGISTRY so the CLI finds our test pool.
		prompt := fmt.Sprintf(
			"Run this exact bash command and nothing else: "+
				"CLAUDE_POOL_REGISTRY=%s %s --pool %s start --prompt 'respond with exactly: spawned-child'",
			pool.registryPath, pool.cliBin, pool.poolName,
		)

		// Start parent via socket — the prompt references test-specific paths
		resp := pool.send(Msg{"type": "start", "prompt": prompt})
		if resp["type"] == "error" {
			t.Fatalf("start parent failed: %v", resp["error"])
		}
		sParent := strVal(resp, "sessionId")

		// Wait for parent to become idle (it should run the CLI command and finish)
		waitResp := pool.send(Msg{"type": "wait", "sessionId": sParent, "timeout": 120000})
		if waitResp["type"] == "error" {
			t.Fatalf("wait for parent failed: %v", waitResp["error"])
		}

		// Find the child session — it should have parentId = sParent
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		if lsResp["type"] == "error" {
			t.Fatalf("ls failed: %v", lsResp["error"])
		}
		sessions, _ := lsResp["sessions"].([]any)

		var childID string
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			if strVal(sm, "parentId") == sParent {
				childID = strVal(sm, "sessionId")
				break
			}
		}
		if childID == "" {
			t.Fatalf("no child session found with parentId=%s. Sessions: %v", sParent, sessions)
		}

		// Wait for child to finish
		childWait := pool.send(Msg{"type": "wait", "sessionId": childID, "timeout": 120000})
		if childWait["type"] == "error" {
			t.Fatalf("wait for child failed: %v", childWait["error"])
		}

		// Capture child output — should contain "spawned-child"
		captureResp := pool.send(Msg{"type": "capture", "sessionId": childID})
		if captureResp["type"] == "error" {
			t.Fatalf("capture child failed: %v", captureResp["error"])
		}
		content, _ := captureResp["content"].(string)
		if !strings.Contains(strings.ToLower(content), "spawned-child") {
			t.Fatalf("expected child output to contain 'spawned-child', got: %s", truncate(content, 500))
		}
	})
}
