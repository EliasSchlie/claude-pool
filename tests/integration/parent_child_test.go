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
//   1. "start root session"
//      Start s1 with a prompt (no parentId — caller is external).
//      Wait for idle.
//      Info on s1 — parentId is null, children is empty.
//
//   2. "start child with explicit parentId"
//      Start s2 with parentId: s1.
//      Wait for idle.
//      Info on s2 — parentId is s1.
//      Info on s1 — children should contain s2.
//
//   3. "start grandchild"
//      Start s3 with parentId: s2.
//      Wait for idle.
//      Info on s3 — parentId is s2.
//      Info on s2 — children contains s3.
//
//   4. "start second child of root"
//      Start s4 with parentId: s1.
//      Wait for idle.
//      Info on s4 — parentId is s1.
//      Info on s1 — children contains both s2 and s4.
//
//   5. "info shows recursive children"
//      Call info on s1.
//      Assert: children array contains s2 and s4.
//      s2's children array contains s3.
//      s3's children array is empty.
//      s4's children array is empty.
//      This is the full recursive tree from s1's perspective.
//
//   6. "ls returns owned direct children"
//      Call ls (default, no flags).
//      Assert: returns sessions owned by the caller. Since we're an external
//      caller, should return s1 (and s4 if direct children — depends on how
//      ownership is implemented for external callers. The key point: ls doesn't
//      return everything, just directly-owned sessions).
//
//   7. "ls with tree shows nested descendants"
//      Call ls with tree: true.
//      Assert: returned sessions have their children populated recursively.
//      s1's children contain s2 and s4, s2's children contain s3.
//
//   8. "ls with all shows all pool sessions"
//      Call ls with all: true.
//      Assert: returns all 4 sessions regardless of ownership (flat list).
//
//   9. "ls with all + tree shows all sessions as tree"
//      Call ls with all: true and tree: true.
//      Assert: returns a nested tree rooted at top-level sessions.
//
//  10. "archive with unarchived children errors"
//      Call archive on s2 (has child s3 which is not archived).
//      Assert: error (session has unarchived children).
//      Info on s2 — still idle (not archived).
//
//  11. "archive leaf session succeeds"
//      Call archive on s3 (has no children).
//      Assert: {type: "ok"}.
//      Info on s3 — status is "archived".
//
//  12. "archive parent after children archived"
//      Now s2's only child (s3) is archived.
//      Call archive on s2.
//      Assert: {type: "ok"} (no unarchived children remain).
//      Info on s2 — status is "archived".
//
//  13. "recursive archive archives entire subtree"
//      Unarchive s2 and s3 (restore them for this test).
//      Call archive on s1 with recursive: true.
//      Assert: {type: "ok"}.
//      All four sessions should be archived (depth-first: s3 first, then s2,
//      then s4, then s1).
//      Info on each — all "archived".
//      ls (default) — empty (all archived).
//      ls with archived: true — all 4 visible.
//
//  14. "session info fields"
//      Unarchive s1 and check detailed fields:
//      Assert: cwd is non-empty, spawnCwd is non-empty, createdAt is a valid
//      ISO timestamp, priority is 0, pinned is false.

import "testing"

func TestParentChild(t *testing.T) {
	t.Skip("not yet implemented")
}
