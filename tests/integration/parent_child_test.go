package integration

// TestParentChild — Ownership, tree listing, and recursive archive flow
//
// Pool size: 3
//
// This flow tests parent-child session relationships: how ownership is set, how
// ls/info respect it, and how recursive archive propagates through the tree.
//
// Session tree built during this flow:
//
//   s1 (parent: null — external caller)
//   ├── s2 (parent: s1)
//   │   └── s3 (parent: s2)
//   └── s4 (parent: s1)
//
// Flow:
//
//   1.  "start root session"
//   2.  "start child with explicit parentId"
//   3.  "start grandchild"
//   4.  "start second child of root"
//   5.  "info shows recursive children"
//   6.  "ls returns owned direct children"
//   7.  "ls with tree shows nested descendants"
//   8.  "ls with all shows all pool sessions"
//   9.  "ls with all + tree shows all sessions as tree"
//  10.  "archive with unarchived children errors"
//  11.  "archive leaf session succeeds"
//  12.  "archive parent after children archived"
//  13.  "recursive archive archives entire subtree"

import (
	"testing"
	"time"
)

func TestParentChild(t *testing.T) {
	pool := setupPool(t, 3)

	var s1, s2, s3, s4 string

	t.Run("start root session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: root"})
		assertNotError(t, resp)
		s1 = strVal(resp, "sessionId")

		pool.awaitStatus(s1, "idle", 60*time.Second)

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		if session.ParentID != "" {
			t.Fatalf("root session should have no parent, got %q", session.ParentID)
		}
		if len(session.Children) != 0 {
			t.Fatalf("new session should have no children, got %d", len(session.Children))
		}
	})

	t.Run("start child with explicit parentId", func(t *testing.T) {
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: child1", "parentId": s1})
		assertNotError(t, resp)
		s2 = strVal(resp, "sessionId")

		pool.awaitStatus(s2, "idle", 60*time.Second)

		info2 := pool.send(Msg{"type": "info", "sessionId": s2})
		session2 := parseSession(t, info2["session"])
		if session2.ParentID != s1 {
			t.Fatalf("expected parentId %s, got %q", s1, session2.ParentID)
		}

		info1 := pool.send(Msg{"type": "info", "sessionId": s1})
		session1 := parseSession(t, info1["session"])
		assertHasChild(t, session1, s2)
	})

	t.Run("start grandchild", func(t *testing.T) {
		// Pool has 3 slots — s1, s2 use 2. s3 gets the 3rd.
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: grandchild", "parentId": s2})
		assertNotError(t, resp)
		s3 = strVal(resp, "sessionId")

		pool.awaitStatus(s3, "idle", 60*time.Second)

		info3 := pool.send(Msg{"type": "info", "sessionId": s3})
		session3 := parseSession(t, info3["session"])
		if session3.ParentID != s2 {
			t.Fatalf("expected parentId %s, got %q", s2, session3.ParentID)
		}

		info2 := pool.send(Msg{"type": "info", "sessionId": s2})
		session2 := parseSession(t, info2["session"])
		assertHasChild(t, session2, s3)
	})

	t.Run("start second child of root", func(t *testing.T) {
		// Pool is full (3 slots). Offload s3 to make room.
		pool.send(Msg{"type": "offload", "sessionId": s3})
		pool.awaitStatus(s3, "offloaded", 10*time.Second)

		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: child2", "parentId": s1})
		assertNotError(t, resp)
		s4 = strVal(resp, "sessionId")

		pool.awaitStatus(s4, "idle", 60*time.Second)

		info1 := pool.send(Msg{"type": "info", "sessionId": s1})
		session1 := parseSession(t, info1["session"])
		assertHasChild(t, session1, s2)
		assertHasChild(t, session1, s4)
	})

	t.Run("info shows recursive children", func(t *testing.T) {
		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])

		// s1 should have s2 and s4 as children
		if len(session.Children) != 2 {
			t.Fatalf("expected 2 children for s1, got %d", len(session.Children))
		}

		var child2 SessionInfo
		for _, c := range session.Children {
			if c.SessionID == s2 {
				child2 = c
			}
		}

		// s2 should have s3 as a child (even though s3 is offloaded)
		if len(child2.Children) != 1 {
			t.Fatalf("expected 1 child for s2, got %d", len(child2.Children))
		}
		if child2.Children[0].SessionID != s3 {
			t.Fatalf("expected s2's child to be s3, got %s", child2.Children[0].SessionID)
		}
	})

	t.Run("ls returns owned direct children", func(t *testing.T) {
		lsResp := pool.send(Msg{"type": "ls"})
		assertNotError(t, lsResp)
		assertType(t, lsResp, "sessions")
		// External caller — should get root-level sessions
		sessions := parseSessions(t, lsResp)
		if len(sessions) == 0 {
			t.Fatal("ls should return at least one session")
		}
	})

	t.Run("ls with tree shows nested descendants", func(t *testing.T) {
		lsResp := pool.send(Msg{"type": "ls", "tree": true})
		assertNotError(t, lsResp)
		sessions := parseSessions(t, lsResp)

		root, found := findSession(sessions, s1)
		if !found {
			t.Fatal("s1 not found in tree ls")
		}
		if len(root.Children) < 2 {
			t.Fatalf("expected at least 2 children in tree, got %d", len(root.Children))
		}
	})

	t.Run("ls with all shows all pool sessions", func(t *testing.T) {
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)

		// Should include all 4 sessions (flat, regardless of ownership)
		ids := map[string]bool{}
		for _, s := range sessions {
			ids[s.SessionID] = true
		}
		for _, sid := range []string{s1, s2, s3, s4} {
			if !ids[sid] {
				t.Fatalf("ls all should include %s", sid)
			}
		}
	})

	t.Run("ls with all + tree shows all sessions as tree", func(t *testing.T) {
		lsResp := pool.send(Msg{"type": "ls", "all": true, "tree": true})
		assertNotError(t, lsResp)
		sessions := parseSessions(t, lsResp)

		// Root s1 should appear with children nested underneath
		root, found := findSession(sessions, s1)
		if !found {
			t.Fatal("s1 not found in all+tree ls")
		}
		if len(root.Children) < 2 {
			t.Fatalf("expected s1 to have at least 2 children in tree, got %d", len(root.Children))
		}

		// s2 should be nested under s1 with s3 as its child
		var child2 SessionInfo
		for _, c := range root.Children {
			if c.SessionID == s2 {
				child2 = c
			}
		}
		if child2.SessionID == "" {
			t.Fatal("s2 not found as child of s1 in tree")
		}
		if len(child2.Children) != 1 || child2.Children[0].SessionID != s3 {
			t.Fatalf("expected s2 to have s3 as child, got %d children", len(child2.Children))
		}
	})

	t.Run("archive with unarchived children errors", func(t *testing.T) {
		// s2 has child s3 (offloaded but not archived)
		resp := pool.send(Msg{"type": "archive", "sessionId": s2})
		assertError(t, resp)

		// Confirm s2 is still in its previous state
		info := pool.send(Msg{"type": "info", "sessionId": s2})
		session := parseSession(t, info["session"])
		if session.Status == "archived" {
			t.Fatal("s2 should not be archived — it has unarchived children")
		}
	})

	t.Run("archive leaf session succeeds", func(t *testing.T) {
		resp := pool.send(Msg{"type": "archive", "sessionId": s3})
		assertNotError(t, resp)

		info := pool.send(Msg{"type": "info", "sessionId": s3})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
	})

	t.Run("archive parent after children archived", func(t *testing.T) {
		// s2's only child (s3) is now archived
		// First offload s2 since it's idle in a slot
		pool.send(Msg{"type": "offload", "sessionId": s2})
		pool.awaitStatus(s2, "offloaded", 10*time.Second)

		resp := pool.send(Msg{"type": "archive", "sessionId": s2})
		assertNotError(t, resp)

		info := pool.send(Msg{"type": "info", "sessionId": s2})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
	})

	t.Run("recursive archive archives entire subtree", func(t *testing.T) {
		// Unarchive s2 and s3 to test recursive archive from s1
		pool.send(Msg{"type": "unarchive", "sessionId": s2})
		pool.send(Msg{"type": "unarchive", "sessionId": s3})

		resp := pool.send(Msg{"type": "archive", "sessionId": s1, "recursive": true})
		assertNotError(t, resp)

		// All 4 sessions should be archived
		for _, sid := range []string{s1, s2, s3, s4} {
			info := pool.send(Msg{"type": "info", "sessionId": sid})
			session := parseSession(t, info["session"])
			assertStatus(t, session, "archived")
		}

		// Default ls should be empty
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		if len(sessions) != 0 {
			t.Fatalf("expected 0 sessions in default ls after recursive archive, got %d", len(sessions))
		}

		// ls with archived should show all 4
		lsArchived := pool.send(Msg{"type": "ls", "all": true, "archived": true})
		archivedSessions := parseSessions(t, lsArchived)
		if len(archivedSessions) < 4 {
			t.Fatalf("expected at least 4 sessions with archived flag, got %d", len(archivedSessions))
		}
	})
}
