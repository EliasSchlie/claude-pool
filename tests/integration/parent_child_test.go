package integration

// TestParentChild — Ownership, tree listing, and recursive archive flow (CLI)
//
// Pool size: 3
//
// Tests parent-child session relationships through the CLI: auto-detection via
// PID registry (real Claude → bash → CLI chain), explicit --parent flag,
// --parent none, ls filtering by parent, verbosity levels for tree views,
// and recursive archive.
//
// Session tree built via real auto-detection:
//
//   s1 (root — no parent, external caller)
//   ├── s2 (child — parent auto-detected as s1's Claude UUID)
//   │   └── s3 (grandchild — parent auto-detected as s2's Claude UUID)
//   └── s4 (second child — parent auto-detected as s1's Claude UUID)
//
// Flow:
//
//   1.  "start root session"
//   2.  "root spawns child via bash — auto-detection"
//   3.  "child spawns grandchild via bash — auto-detection"
//   4.  "root spawns second child via bash — auto-detection"
//   5.  "info shows recursive children"
//   6.  "ls without parent from non-Claude caller shows all"
//   7.  "ls with parent filter shows direct children"
//   8.  "ls with verbosity nested shows tree"
//   9.  "ls with status filter"
//  10.  "wait with parent filter"
//  11.  "explicit parent overrides auto-detection"
//  12.  "parent none disables auto-detection"
//  13.  "archive with unarchived children errors"
//  14.  "archive leaf session succeeds"
//  15.  "archive parent after children archived"
//  16.  "recursive archive archives entire subtree"

import (
	"fmt"
	"testing"
	"time"
)

func TestParentChild(t *testing.T) {
	pool := setupPool(t, 3)

	// CLI command prefix for commands run inside Claude sessions.
	// Sessions need CLAUDE_POOL_HOME and CLAUDE_POOL_DAEMON because the
	// test pool lives in an isolated directory, not ~/.claude-pool/.
	cliPrefix := fmt.Sprintf(
		"CLAUDE_POOL_HOME=%s CLAUDE_POOL_DAEMON=%s %s --pool %s",
		pool.homeDir, daemonBinPath, cliBinPath, pool.name,
	)

	var s1, s2, s3, s4 string
	var s1UUID, s2UUID string

	t.Run("start root session", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: root")
		s1 = strVal(resp, "sessionId")
		pool.waitForIdle(s1, 300*time.Second)

		info := pool.getSessionInfo(s1)
		s1UUID = info.ClaudeUUID
		if s1UUID == "" {
			t.Fatal("root session should have a Claude UUID")
		}
		if info.Parent != "" {
			t.Fatalf("root session should have no parent, got %q", info.Parent)
		}
	})

	t.Run("root spawns child via bash — auto-detection", func(t *testing.T) {
		cmd := fmt.Sprintf("%s start --prompt 'respond with exactly: child1'", cliPrefix)
		pool.run("followup", "--session", s1,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s1, 300*time.Second)

		// Find the child via ls --parent (children are deduped from default ls)
		sessions := pool.listSessions("--parent", s1UUID)
		if len(sessions) == 0 {
			t.Fatal("expected root to have spawned a child session via bash")
		}
		s2 = sessions[0].SessionID
		pool.waitForIdle(s2, 300*time.Second)

		// SPEC: auto-detected parent is the caller's Claude UUID
		info := pool.getSessionInfo(s2)
		s2UUID = info.ClaudeUUID
		if info.Parent != s1UUID {
			t.Fatalf("child parent should be root's Claude UUID %q (auto-detected), got %q",
				s1UUID, info.Parent)
		}

		rootInfo := pool.getSessionInfo(s1)
		assertHasChild(t, rootInfo, s2)
	})

	t.Run("child spawns grandchild via bash — auto-detection", func(t *testing.T) {
		cmd := fmt.Sprintf("%s start --prompt 'respond with exactly: grandchild'", cliPrefix)
		pool.run("followup", "--session", s2,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s2, 300*time.Second)

		// Find the grandchild via ls --parent (children are deduped from default ls)
		sessions := pool.listSessions("--parent", s2UUID)
		if len(sessions) == 0 {
			t.Fatal("expected child to have spawned a grandchild session via bash")
		}
		s3 = sessions[0].SessionID
		pool.waitForIdle(s3, 300*time.Second)

		info := pool.getSessionInfo(s3)
		if info.Parent != s2UUID {
			t.Fatalf("grandchild parent should be child's Claude UUID %q (auto-detected), got %q",
				s2UUID, info.Parent)
		}

		childInfo := pool.getSessionInfo(s2)
		assertHasChild(t, childInfo, s3)
	})

	t.Run("root spawns second child via bash — auto-detection", func(t *testing.T) {
		// Pool is full (3 slots). This evicts the LRU session (s3).
		cmd := fmt.Sprintf("%s start --prompt 'respond with exactly: child2'", cliPrefix)
		pool.run("followup", "--session", s1,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s1, 300*time.Second)

		// Root now has 2 children — find the new one
		sessions := pool.listSessions("--parent", s1UUID)
		for _, s := range sessions {
			if s.SessionID != s2 {
				s4 = s.SessionID
				break
			}
		}
		if s4 == "" {
			t.Fatal("expected root to have spawned a second child via bash")
		}
		pool.waitForIdle(s4, 300*time.Second)

		info := pool.getSessionInfo(s4)
		if info.Parent != s1UUID {
			t.Fatalf("child2 parent should be root's Claude UUID %q (auto-detected), got %q",
				s1UUID, info.Parent)
		}

		rootInfo := pool.getSessionInfo(s1)
		assertHasChild(t, rootInfo, s2)
		assertHasChild(t, rootInfo, s4)
	})

	t.Run("info shows recursive children", func(t *testing.T) {
		info := pool.getSessionInfo(s1)

		if len(info.Children) != 2 {
			t.Fatalf("expected 2 children for s1, got %d", len(info.Children))
		}

		var child1Info SessionInfo
		for _, c := range info.Children {
			if c.SessionID == s2 {
				child1Info = c
			}
		}

		if len(child1Info.Children) != 1 {
			t.Fatalf("expected 1 child for s2, got %d", len(child1Info.Children))
		}
		if child1Info.Children[0].SessionID != s3 {
			t.Fatalf("expected s2's child to be s3, got %s", child1Info.Children[0].SessionID)
		}
	})

	t.Run("ls without parent from non-Claude caller shows all", func(t *testing.T) {
		sessions := pool.listSessions()
		if len(sessions) == 0 {
			t.Fatal("ls should return sessions")
		}
		if _, found := findSession(sessions, s1); !found {
			t.Fatal("expected s1 in top-level ls")
		}
	})

	t.Run("ls with parent filter shows direct children", func(t *testing.T) {
		sessions := pool.listSessions("--parent", s1UUID)
		if _, found := findSession(sessions, s2); !found {
			t.Fatal("expected s2 in ls --parent <rootUUID>")
		}
		if _, found := findSession(sessions, s4); !found {
			t.Fatal("expected s4 in ls --parent <rootUUID>")
		}
		if _, found := findSession(sessions, s3); found {
			t.Fatal("s3 (grandchild) should not appear in --parent <rootUUID>")
		}
	})

	t.Run("ls with verbosity nested shows tree", func(t *testing.T) {
		resp := pool.runJSON("ls", "--verbosity", "nested")
		sessions := parseSessions(t, resp)

		root, found := findSession(sessions, s1)
		if !found {
			t.Fatal("s1 not found in nested ls")
		}
		if len(root.Children) < 2 {
			t.Fatalf("expected at least 2 children in tree, got %d", len(root.Children))
		}

		for _, c := range root.Children {
			if c.SessionID == s2 {
				if len(c.Children) != 1 || c.Children[0].SessionID != s3 {
					t.Fatalf("expected s3 nested under s2")
				}
			}
		}

		// SPEC: children not repeated as separate top-level entries
		for _, s := range sessions {
			if s.SessionID == s2 || s.SessionID == s3 || s.SessionID == s4 {
				t.Fatalf("child %s should not appear at top level in nested ls", s.SessionID)
			}
		}
	})

	t.Run("ls with status filter", func(t *testing.T) {
		sessions := pool.listSessions("--status", "idle")
		for _, s := range sessions {
			if s.Status != "idle" {
				t.Fatalf("expected only idle sessions with --status idle, got %q", s.Status)
			}
		}
		if len(sessions) == 0 {
			t.Fatal("expected at least one idle session")
		}
	})

	t.Run("wait with parent filter", func(t *testing.T) {
		// Use a slow prompt so the child stays processing while wait runs.
		// The child is started by root via bash — auto-detected parent = rootUUID.
		cmd := fmt.Sprintf("%s start --prompt 'run the bash command: sleep 30 && echo wait-parent-done'", cliPrefix)
		pool.run("followup", "--session", s1,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s1, 300*time.Second)

		waitResp := pool.runJSON("wait", "--parent", s1UUID, "--timeout", "120000")
		waitedSid := strVal(waitResp, "sessionId")
		assertNonEmpty(t, "waited sessionId", waitedSid)

		pool.run("archive", "--session", waitedSid)
	})

	t.Run("explicit parent overrides auto-detection", func(t *testing.T) {
		// Root runs start with explicit --parent pointing to s2UUID.
		// Auto-detection would set parent to root's UUID, but explicit wins.
		cmd := fmt.Sprintf("%s start --prompt 'respond with exactly: explicit' --parent %s", cliPrefix, s2UUID)
		pool.run("followup", "--session", s1,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s1, 300*time.Second)

		// Child was created with --parent s2UUID, find it there
		sessions := pool.listSessions("--parent", s2UUID)
		var explicitChild string
		for _, s := range sessions {
			if s.SessionID != s3 {
				explicitChild = s.SessionID
				break
			}
		}
		if explicitChild == "" {
			t.Fatal("expected explicit-parent child session")
		}
		pool.waitForIdle(explicitChild, 300*time.Second)

		info := pool.getSessionInfo(explicitChild)
		if info.Parent != s2UUID {
			t.Fatalf("expected parent %s (explicit), got %q", s2UUID, info.Parent)
		}

		pool.run("archive", "--session", explicitChild)
	})

	t.Run("parent none disables auto-detection", func(t *testing.T) {
		cmd := fmt.Sprintf("%s start --prompt 'respond with exactly: orphan' --parent none", cliPrefix)
		pool.run("followup", "--session", s1,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(s1, 300*time.Second)

		sessions := pool.listSessions()
		var orphan string
		for _, s := range sessions {
			if s.SessionID != s1 && s.SessionID != s2 && s.SessionID != s3 && s.SessionID != s4 {
				orphan = s.SessionID
				break
			}
		}
		if orphan == "" {
			t.Fatal("expected orphan session")
		}
		pool.waitForIdle(orphan, 300*time.Second)

		info := pool.getSessionInfo(orphan)
		if info.Parent != "" {
			t.Fatalf("expected no parent with --parent none, got %q", info.Parent)
		}

		pool.run("archive", "--session", orphan)
	})

	t.Run("archive with unarchived children errors", func(t *testing.T) {
		result := pool.run("archive", "--session", s2)
		assertExitError(t, result)

		info := pool.getSessionInfo(s2)
		if info.Status == "archived" {
			t.Fatal("s2 should not be archived — it has unarchived children")
		}
	})

	t.Run("archive leaf session succeeds", func(t *testing.T) {
		result := pool.run("archive", "--session", s3)
		assertExitOK(t, result)

		info := pool.getSessionInfo(s3)
		assertStatus(t, info, "archived")
	})

	t.Run("archive parent after children archived", func(t *testing.T) {
		result := pool.run("archive", "--session", s2)
		assertExitOK(t, result)

		info := pool.getSessionInfo(s2)
		assertStatus(t, info, "archived")
	})

	t.Run("recursive archive archives entire subtree", func(t *testing.T) {
		pool.run("unarchive", "--session", s2)
		pool.run("unarchive", "--session", s3)

		result := pool.run("archive", "--session", s1, "--recursive")
		assertExitOK(t, result)

		for _, sid := range []string{s1, s2, s3, s4} {
			info := pool.getSessionInfo(sid)
			assertStatus(t, info, "archived")
		}

		sessions := pool.listSessions()
		if len(sessions) != 0 {
			t.Fatalf("expected 0 sessions after recursive archive, got %d", len(sessions))
		}

		archivedSessions := pool.listSessions("--archived")
		if len(archivedSessions) < 4 {
			t.Fatalf("expected at least 4 with --archived, got %d", len(archivedSessions))
		}
	})
}
