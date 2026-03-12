package integration

// TestPool — Pool lifecycle flow
//
// Pool size: 2
//
// This flow tests pool-level operations: init, config, resize, health, destroy,
// and re-init with session restoration.
//
// Flow:
//
//   1. "ping before init"
//      Send ping to the daemon socket before calling init.
//      Assert: response is {type: "pong"}.
//      Confirms the daemon is reachable and responds before any pool exists.
//
//   2. "config before init"
//      Read config.
//      Assert: returns current config with flags (--model haiku --dangerously-skip-permissions)
//      and size matching what setupPool configured.
//
//   3. "config set"
//      Set a new config value: size = 4.
//      Assert: response reflects the updated size.
//      Read config again to confirm persistence.
//      Set it back to 2 for the rest of the flow.
//
//   4. "init"
//      Call init with size 2.
//      Assert: response type is "pool", pool state shows 2 sessions.
//      Wait for both sessions to become idle (pre-warming complete).
//
//   5. "init errors if already initialized"
//      Call init again.
//      Assert: response is an error (pool already running).
//
//   6. "health"
//      Call health.
//      Assert: reports 2 total slots, 2 idle sessions, queue depth 0.
//      Check that PID liveness info is present.
//
//   7. "resize up to 3"
//      Call resize with size 3.
//      Assert: response shows 3 slots.
//      Wait for the new session to become idle.
//      Call health — should show 3 idle.
//
//   8. "resize down to 1"
//      Start a prompt on one session so it's processing.
//      Call resize with size 1.
//      Assert: response accepted (resize is async — kill tokens enqueued).
//      The two idle sessions should get offloaded as kill tokens consume their slots.
//      Wait for the processing session to finish — its slot should also get consumed
//      by a kill token once it becomes idle, but since we're going to size 1, one
//      slot remains. Verify via health that we end up with 1 slot.
//
//   9. "resize respects pins"
//      Resize back to 2, wait for 2 idle.
//      Pin one session.
//      Resize to 1.
//      Assert: the unpinned session gets its slot removed, the pinned session stays.
//      Health shows 1 slot, 1 idle (pinned). Unpin for cleanup.
//
//  10. "destroy without confirm errors"
//      Call destroy without confirm: true.
//      Assert: error response.
//
//  11. "destroy"
//      Call destroy with confirm: true.
//      Assert: response is {type: "ok"}.
//      The daemon should exit. Socket connection should close/EOF.
//
//  12. "re-init restores sessions"
//      Start a new daemon on the same pool directory.
//      Call init with size 2.
//      Assert: the session that was idle before destroy is restored (same internal ID,
//      loaded via /resume). The pool should have the restored session + one fresh
//      session to fill the remaining slot.
//      Wait for both to become idle. Verify restored session's claudeUUID is
//      discoverable via info.
//
//  13. "re-init with noRestore"
//      Destroy again (confirm: true). Start new daemon.
//      Call init with size 2 and noRestore: true.
//      Assert: all sessions are fresh (no restored internal IDs from before).
//      Wait for both to become idle.

import "testing"

func TestPool(t *testing.T) {
	t.Skip("not yet implemented")
}
