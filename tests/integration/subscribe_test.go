package integration

// TestSubscribe — Event stream, filters, re-subscribe, and updated events flow
//
// Pool size: 2
//
// This flow tests the subscribe system: receiving events, applying filters, dynamically
// updating subscriptions, and verifying the "updated" event type for property changes.
//
// Subscribe is tested by opening a subscription connection, then performing actions
// on the pool and asserting that the expected events arrive on the stream.
//
// Flow:
//
//   1. "subscribe receives status events"
//      Open a subscription with no filters (all events).
//      Start a session s1 with a prompt.
//      Assert events received:
//        - {event: "created", sessionId: s1, status: "queued" or "processing"}
//        - {event: "status", sessionId: s1, status: "processing", prevStatus: ...}
//          (if it transitioned through queued first)
//      Wait for s1 to become idle.
//      Assert event: {event: "status", sessionId: s1, status: "idle", prevStatus: "processing"}.
//
//   2. "subscribe receives created events with parentId"
//      Start s2 with parentId: s1.
//      Assert event: {event: "created", sessionId: s2, parentId: s1}.
//      Wait for s2 to become idle.
//
//   3. "subscribe receives pool events"
//      Call resize to size 3.
//      Assert event: {event: "pool", action: "resize", size: 3}.
//      Resize back to 2 for the rest of the flow.
//      Assert another pool event for the resize back.
//
//   4. "filter by sessions"
//      Close previous subscription. Open new subscription with sessions: [s1].
//      Send a followup to s2 (should NOT produce events on this subscription).
//      Send a followup to s1.
//      Assert: only events for s1 arrive. No events for s2.
//      Wait for both to become idle.
//
//   5. "filter by events"
//      Close subscription. Open new with events: ["status"].
//      Start a new session s3.
//      Assert: receive status change events only. No "created" event should appear
//      on this subscription (created events are filtered out).
//      Wait for s3 to become idle.
//
//   6. "filter by statuses"
//      Close subscription. Open new with statuses: ["idle"].
//      Send a followup to s1 (goes processing → idle).
//      Assert: only the transition TO idle arrives. The transition to "processing"
//      is filtered out.
//      Wait for s1 to become idle.
//
//   7. "filters are ANDed"
//      Close subscription. Open new with sessions: [s1], statuses: ["idle"].
//      Send followup to s1 and s2.
//      Assert: only the event where s1 transitions to idle arrives.
//      No events for s2, no "processing" events for s1.
//
//   8. "re-subscribe replaces filters"
//      Open subscription with sessions: [s1].
//      Send another subscribe on the SAME connection with sessions: [s2].
//      Send followup to s1 and s2.
//      Assert: only events for s2 arrive (filters were replaced, not merged).
//      Wait for both idle.
//
//   9. "updated event — priority change"
//      Open subscription with events: ["updated"].
//      Call set-priority on s1 with priority 5.
//      Assert event: {event: "updated", sessionId: s1, changes: {priority: 5}}.
//
//  10. "updated event — pin/unpin"
//      Pin s1.
//      Assert event: {event: "updated", sessionId: s1, changes: {pinned: true}}.
//      Unpin s1.
//      Assert event: {event: "updated", sessionId: s1, changes: {pinned: false}}.
//
//  11. "updated event — fields filter"
//      Close subscription. Open new with events: ["updated"], fields: ["priority"].
//      Call set-priority on s1 (should trigger event).
//      Pin s1 (should NOT trigger event — "pinned" not in fields filter).
//      Assert: only the priority updated event arrives.
//      Unpin s1 for cleanup.
//
//  12. "archived and unarchived events"
//      Open subscription with events: ["archived", "unarchived"].
//      Archive s1.
//      Assert event: {event: "archived", sessionId: s1}.
//      Unarchive s1.
//      Assert event: {event: "unarchived", sessionId: s1}.
//
//  13. "multiple concurrent subscribers"
//      Open two separate subscription connections: sub_a with sessions: [s1],
//      sub_b with sessions: [s2].
//      Send followup to both s1 and s2.
//      Assert: sub_a only receives s1 events, sub_b only receives s2 events.
//      Both streams are independent.

import "testing"

func TestSubscribe(t *testing.T) {
	t.Skip("not yet implemented")
}
