package integration

// TestOffload — Offload, capture, restore, and archive lifecycle flow
//
// Pool size: 2
//
// This flow tests the full lifecycle of sessions being offloaded, having their
// output read in different states, being restored, and then archived/unarchived.
//
// Flow:
//
//   1. "start and wait for idle"
//      Start a session with prompt "respond with exactly: offload test".
//      Wait for idle. Save as s1.
//      Also start s2 with a different prompt. Wait for idle.
//
//   2. "offload idle session"
//      Call offload on s1.
//      Assert: response is {type: "ok"}.
//      Info on s1 — status is "offloaded", pid is null.
//
//   3. "offload pinned session errors"
//      Pin s2. Call offload on s2.
//      Assert: error (session is pinned, unpin first).
//      Unpin s2.
//
//   4. "offload non-idle errors"
//      Start a slow prompt on s2 (now processing).
//      Call offload on s2.
//      Assert: error (session is processing).
//      Wait for s2 to become idle.
//
//   5. "capture JSONL on offloaded session works"
//      s1 is offloaded. Call capture on s1 with format "jsonl-short".
//      Assert: content is non-empty, contains "offload test".
//      Also try "jsonl-last", "jsonl-long", "jsonl-full".
//      Assert: all return non-empty content.
//
//   6. "capture buffer on offloaded session errors"
//      Call capture on s1 with format "buffer-last".
//      Assert: error (no live terminal).
//      Same for "buffer-full" — error.
//
//   7. "followup restores offloaded session"
//      Call followup on s1 with prompt "respond with exactly: restored".
//      Assert: response is "started", status is "queued" (s1 is being loaded).
//      Wait for s1 to become idle.
//      Assert: capture content contains "restored".
//      Info on s1 — status is "idle", pid is non-null again.
//
//   8. "archive idle session"
//      Call archive on s1.
//      Assert: response is {type: "ok"}.
//      Info on s1 — status is "archived".
//
//   9. "archived session hidden from ls"
//      Call ls (default).
//      Assert: s1 is NOT in the results.
//      Call ls with archived: true.
//      Assert: s1 IS in the results with status "archived".
//
//  10. "capture JSONL on archived session works"
//      Call capture on s1 (archived) with format "jsonl-short".
//      Assert: content is non-empty (transcript still accessible).
//
//  11. "capture buffer on archived session errors"
//      Call capture on s1 with format "buffer-last".
//      Assert: error (no live terminal).
//
//  12. "followup on archived errors"
//      Call followup on s1 with a prompt.
//      Assert: error (session is archived, unarchive first).
//
//  13. "pin on archived errors"
//      Call pin on s1.
//      Assert: error (session is archived, unarchive first).
//
//  14. "unarchive restores to offloaded"
//      Call unarchive on s1.
//      Assert: response is {type: "ok"}.
//      Info on s1 — status is "offloaded".
//      ls (default) — s1 is visible again.
//
//  15. "unarchive on non-archived errors"
//      Call unarchive on s2 (which is idle, not archived).
//      Assert: error.
//
//  16. "archive stops active session first"
//      Send a slow prompt to s2 (now processing).
//      Call archive on s2.
//      Assert: response is {type: "ok"} (archive stops it first, then offloads,
//      then archives).
//      Info on s2 — status is "archived".
//
//  17. "archive is idempotent"
//      Call archive on s2 again (already archived).
//      Assert: response is {type: "ok"} (no-op).

import "testing"

func TestOffload(t *testing.T) {
	t.Skip("not yet implemented")
}
