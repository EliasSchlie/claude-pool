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

import "testing"

func TestEnv(t *testing.T) {
	t.Skip("not yet implemented")
}
