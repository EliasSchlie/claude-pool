package integration

// TestSlots — Queue, priority, and eviction flow
//
// Pool size: 2
//
// This flow tests what happens when slots are scarce: queueing, priority-based
// eviction, pin protection, and graceful resize behavior. The small pool size (2)
// makes it easy to exhaust slots and trigger these behaviors.
//
// Flow:
//
//   1. "start fills all slots"
//      Start two sessions with prompts. Wait for both to become idle.
//      Assert: both are idle, ls shows 2 sessions, health shows 0 queue depth.
//      Save as s1, s2.
//
//   2. "third start queues"
//      Start a third session with a prompt.
//      Assert: response status is "queued". Save as s3.
//      Call info on s3 — status should be "queued".
//      Health should show queue depth 1.
//
//   3. "stop cancels queued start"
//      Call stop on s3.
//      Assert: response is {type: "ok"}.
//      Call info on s3 — should error (session was a new start that never got a
//      slot, so it's removed entirely).
//      Health should show queue depth 0.
//
//   4. "queued session gets slot when one frees"
//      Start a new session s4 (queues because pool is full).
//      Assert: status is "queued".
//      Offload s1 (idle → offloaded, frees a slot).
//      Wait for s4 to become idle — it should have been dequeued into the freed slot.
//      Assert: s4 status is "idle". Health shows queue depth 0.
//
//   5. "set-priority affects eviction order"
//      Ensure s2 and s4 are both idle.
//      Set s2 priority to 10 (high — evicted last).
//      Set s4 priority to -1 (low — evicted first).
//      Start a new session s5 — pool is full, so it needs to evict an idle session.
//      Assert: s4 was evicted (offloaded), not s2, because s4 has lower priority.
//      Info on s4 should show status "offloaded".
//      Info on s2 should show status "idle".
//      Wait for s5 to become idle.
//
//   6. "LRU within same priority"
//      Set both s2 and s5 to the same priority (0).
//      Send a followup to s5 so it becomes the most recently used.
//      Wait for s5 to become idle.
//      Now start another session — eviction should pick s2 (older) over s5 (newer).
//      Assert: s2 was offloaded, s5 is still idle.
//
//   7. "pin prevents eviction"
//      Restore state: 2 idle sessions (s5 and the new one from step 6). Pin s5.
//      Start another session — pool is full, needs eviction.
//      Assert: the unpinned session is evicted, s5 stays (it's pinned).
//      Info on s5 — pinned is true, status is "idle".
//
//   8. "pin with sessionId on offloaded triggers priority load"
//      s4 is still offloaded from step 5.
//      Pin s4.
//      Assert: response status is "queued" (s4 jumped to front of load queue).
//      Wait for s4 — should become idle (loaded into a slot, possibly evicting
//      the unpinned session).
//      Info on s4 — pinned is true.
//
//   9. "pin without sessionId allocates fresh session"
//      Unpin all pinned sessions.
//      Call pin with no sessionId.
//      Assert: response includes a new sessionId and status.
//      The session should be pinned. If the pool was full, an unpinned idle
//      session was evicted to make room.
//
//  10. "unpin allows eviction"
//      Unpin the session from step 9.
//      Info shows pinned: false.
//      Start a new session — the previously pinned (now unpinned) session is
//      eligible for eviction.
//      Assert: eviction can target it.
//
//  11. "followup on queued errors"
//      Fill both slots with processing sessions (send slow prompts).
//      Start s_queued (it queues).
//      Call followup on s_queued.
//      Assert: error (session is queued).
//
//  12. "followup with force on queued replaces prompt"
//      Call followup on s_queued with force: true and a new prompt.
//      Assert: response is "started" (the pending prompt was replaced).
//      Wait for s_queued to finish — result should relate to the forced prompt.
//
//  13. "graceful resize down"
//      Ensure pool has 2 slots with 2 idle sessions.
//      Start a slow prompt on one session (now processing).
//      Call resize to size 1.
//      Assert: resize returns immediately (accepted).
//      The idle session should be offloaded quickly (kill token consumes its slot).
//      The processing session should NOT be interrupted — it finishes naturally.
//      Once it becomes idle, the kill token consumes its slot too... but we resized
//      to 1, so only one slot is killed. Wait for the processing session to finish.
//      Health should show 1 slot remaining.
//
//  14. "resize respects pins during shrink"
//      Resize back to 2, wait for 2 idle.
//      Pin one session.
//      Resize to 1.
//      Assert: the unpinned session's slot is killed, the pinned session stays.
//      Health: 1 slot, 1 idle (pinned).

import "testing"

func TestSlots(t *testing.T) {
	t.Skip("not yet implemented")
}
