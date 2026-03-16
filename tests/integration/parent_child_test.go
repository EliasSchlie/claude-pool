package integration

// TestParentChild — Ownership, tree listing, and recursive archive flow (CLI)
//
// Pool size: 3
//
// Tests parent-child session relationships through the CLI: explicit --parent flag,
// env-based auto-detection, ls filtering by parent, verbosity levels for tree views,
// and recursive archive.
//
// Session tree built during this flow:
//
//   s1 (parent: none — external caller)
//   ├── s2 (parent: s1)
//   │   └── s3 (parent: s2)
//   └── s4 (parent: s1)
//
// Flow:
//
//   1.  "start root session"
//   2.  "start child with explicit parent"
//   3.  "start grandchild"
//   4.  "start second child of root"
//   5.  "info shows recursive children"
//   6.  "ls without parent from non-Claude caller shows all"
//   7.  "ls with parent filter shows direct children"
//   8.  "ls with verbosity nested shows tree"
//   9.  "child auto-detects parent from env"
//  10.  "explicit parent overrides env"
//  11.  "parent none disables auto-detection"
//  12.  "ls with status filter"
//  13.  "wait with parent filter"
//  14.  "ls parent none from session context shows all"
//  15.  "ls from session context shows owned children"
//  16.  "archive with unarchived children errors"
//  17.  "archive leaf session succeeds"
//  18.  "archive parent after children archived"
//  19.  "recursive archive archives entire subtree"
//  20.  "real parent auto-detection via Claude prompt"

import (
	"fmt"
	"testing"
	"time"
)

func TestParentChild(t *testing.T) {
	pool := setupPool(t, 3)

	var s1, s2, s3, s4 string

	t.Run("start root session", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: root")
		s1 = strVal(resp, "sessionId")
		pool.waitForIdle(s1, 300*time.Second)

		info := pool.getSessionInfo(s1)
		if info.Parent != "" {
			t.Fatalf("root session should have no parent, got %q", info.Parent)
		}
		if len(info.Children) != 0 {
			t.Fatalf("new session should have no children, got %d", len(info.Children))
		}
	})

	t.Run("start child with explicit parent", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: child1", "--parent", s1)
		s2 = strVal(resp, "sessionId")
		pool.waitForIdle(s2, 300*time.Second)

		info2 := pool.getSessionInfo(s2)
		if info2.Parent != s1 {
			t.Fatalf("expected parent %s, got %q", s1, info2.Parent)
		}

		info1 := pool.getSessionInfo(s1)
		assertHasChild(t, info1, s2)
	})

	t.Run("start grandchild", func(t *testing.T) {
		resp := pool.runJSON("start", "--prompt", "respond with exactly: grandchild", "--parent", s2)
		s3 = strVal(resp, "sessionId")
		pool.waitForIdle(s3, 300*time.Second)

		info3 := pool.getSessionInfo(s3)
		if info3.Parent != s2 {
			t.Fatalf("expected parent %s, got %q", s2, info3.Parent)
		}

		info2 := pool.getSessionInfo(s2)
		assertHasChild(t, info2, s3)
	})

	t.Run("start second child of root", func(t *testing.T) {
		// Pool is full (3 slots). Evict s3 (least recently used) by starting s4.
		resp := pool.runJSON("start", "--prompt", "respond with exactly: child2", "--parent", s1)
		s4 = strVal(resp, "sessionId")
		pool.waitForIdle(s4, 300*time.Second)

		info1 := pool.getSessionInfo(s1)
		assertHasChild(t, info1, s2)
		assertHasChild(t, info1, s4)
	})

	t.Run("info shows recursive children", func(t *testing.T) {
		info := pool.getSessionInfo(s1)

		if len(info.Children) != 2 {
			t.Fatalf("expected 2 children for s1, got %d", len(info.Children))
		}

		var child2 SessionInfo
		for _, c := range info.Children {
			if c.SessionID == s2 {
				child2 = c
			}
		}

		if len(child2.Children) != 1 {
			t.Fatalf("expected 1 child for s2, got %d", len(child2.Children))
		}
		if child2.Children[0].SessionID != s3 {
			t.Fatalf("expected s2's child to be s3, got %s", child2.Children[0].SessionID)
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
		sessions := pool.listSessions("--parent", s1)
		if _, found := findSession(sessions, s2); !found {
			t.Fatal("expected s2 in ls --parent s1")
		}
		if _, found := findSession(sessions, s4); !found {
			t.Fatal("expected s4 in ls --parent s1")
		}
		if _, found := findSession(sessions, s3); found {
			t.Fatal("s3 (grandchild) should not appear in --parent s1")
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
	})

	t.Run("child auto-detects parent from env", func(t *testing.T) {
		resp := pool.runInSessionJSON(s1, "start", "--prompt", "respond with exactly: auto-child")
		autoChild := strVal(resp, "sessionId")

		pool.waitForIdle(autoChild, 300*time.Second)

		info := pool.getSessionInfo(autoChild)
		if info.Parent != s1 {
			t.Fatalf("expected parent auto-detected as %s, got %q", s1, info.Parent)
		}

		pool.run("archive", "--session", autoChild)
	})

	t.Run("explicit parent overrides env", func(t *testing.T) {
		resp := pool.runInSessionJSON(s1, "start", "--prompt", "respond with exactly: explicit", "--parent", s2)
		explicitChild := strVal(resp, "sessionId")

		pool.waitForIdle(explicitChild, 300*time.Second)

		info := pool.getSessionInfo(explicitChild)
		if info.Parent != s2 {
			t.Fatalf("expected parent %s (explicit), got %q", s2, info.Parent)
		}

		pool.run("archive", "--session", explicitChild)
	})

	t.Run("parent none disables auto-detection", func(t *testing.T) {
		resp := pool.runInSessionJSON(s1, "start", "--prompt", "respond with exactly: orphan", "--parent", "none")
		orphan := strVal(resp, "sessionId")

		pool.waitForIdle(orphan, 300*time.Second)

		info := pool.getSessionInfo(orphan)
		if info.Parent != "" {
			t.Fatalf("expected no parent with --parent none, got %q", info.Parent)
		}

		pool.run("archive", "--session", orphan)
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
		resp := pool.runJSON("start", "--prompt", "respond with exactly: wait-parent-test", "--parent", s1)
		childSid := strVal(resp, "sessionId")

		waitResp := pool.runJSON("wait", "--parent", s1, "--timeout", "120000")
		waitedSid := strVal(waitResp, "sessionId")
		if waitedSid != childSid {
			t.Fatalf("expected wait to return child %s, got %s", childSid, waitedSid)
		}
		assertContains(t, strVal(waitResp, "content"), "wait-parent-test")
		pool.run("archive", "--session", childSid)
	})

	t.Run("ls parent none from session context shows all", func(t *testing.T) {
		resp := pool.runInSessionJSON(s1, "ls", "--parent", "none")
		sessions := parseSessions(t, resp)
		if _, found := findSession(sessions, s1); !found {
			t.Fatal("expected s1 in ls --parent none from session context")
		}
	})

	t.Run("ls from session context shows owned children", func(t *testing.T) {
		resp := pool.runInSessionJSON(s1, "ls")
		sessions := parseSessions(t, resp)

		hasS2 := false
		for _, s := range sessions {
			if s.SessionID == s2 {
				hasS2 = true
			}
			if s.SessionID == s3 {
				t.Fatal("s3 (grandchild) should not appear in ls from s1 context")
			}
		}
		if !hasS2 {
			t.Fatal("expected s2 in ls from s1 context")
		}
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

	t.Run("real parent auto-detection via Claude prompt", func(t *testing.T) {
		// All previous sessions archived. Pool has 3 fresh slots.
		root := pool.startSession("respond with exactly: auto-root")

		rootInfo := pool.getSessionInfo(root)
		rootUUID := rootInfo.ClaudeUUID
		if rootUUID == "" {
			t.Fatal("root session should have a Claude UUID by now")
		}

		// Ask Claude to start a child session via bash — tests real auto-detection
		// through env var propagation, not simulated runInSession.
		cmd := fmt.Sprintf(
			"CLAUDE_POOL_HOME=%s CLAUDE_POOL_DAEMON=%s %s --pool %s start --prompt 'respond with exactly: auto-spawned-child'",
			pool.homeDir, daemonBinPath, cliBinPath, pool.name,
		)
		pool.run("followup", "--session", root,
			"--prompt", fmt.Sprintf("run this exact bash command: %s", cmd))
		pool.waitForIdle(root, 300*time.Second)

		// Find the child session — it's the non-archived session that isn't root
		sessions := pool.listSessions()
		var childSid string
		for _, s := range sessions {
			if s.SessionID != root {
				childSid = s.SessionID
				break
			}
		}
		if childSid == "" {
			t.Fatal("expected Claude to have started a child session via bash")
		}

		pool.waitForIdle(childSid, 300*time.Second)

		// SPEC: auto-detected parent is the caller's Claude UUID
		childInfo := pool.getSessionInfo(childSid)
		if childInfo.Parent != rootUUID {
			t.Fatalf("child parent should be root's Claude UUID %q (auto-detected), got %q",
				rootUUID, childInfo.Parent)
		}

		// SPEC: ls top-level — children not repeated as separate entries
		topLevel := pool.listSessions()
		for _, s := range topLevel {
			if s.SessionID == childSid {
				t.Fatal("child session should not appear at top level in ls")
			}
		}

		// Nested: child appears under root
		nestedResp := pool.runJSON("ls", "--verbosity", "nested")
		nestedSessions := parseSessions(t, nestedResp)
		rootNested, found := findSession(nestedSessions, root)
		if !found {
			t.Fatal("root not found in nested ls")
		}
		assertHasChild(t, rootNested, childSid)
	})
}
