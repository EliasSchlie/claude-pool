package integration

// TestSubscribe — Event stream, filters, re-subscribe, and updated events flow
//
// Pool size: 2
//
// This flow tests the subscribe system: receiving events, applying filters, dynamically
// updating subscriptions, and verifying the "updated" event type for property changes.
//
// Flow:
//
//   1.  "subscribe receives status events"
//   2.  "subscribe receives created events with parentId"
//   3.  "subscribe receives pool events"
//   4.  "filter by sessions"
//   5.  "filter by events"
//   6.  "filter by statuses"
//   7.  "filters are ANDed"
//   8.  "re-subscribe replaces filters"
//   9.  "updated event — priority change"
//  10.  "updated event — pin/unpin"
//  11.  "updated event — fields filter"
//  12.  "archived and unarchived events"
//  13.  "multiple concurrent subscribers"

import (
	"testing"
	"time"
)

func TestSubscribe(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string

	t.Run("subscribe receives status events", func(t *testing.T) {
		sub := pool.subscribe(Msg{})

		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: sub1"})
		assertNotError(t, resp)
		s1 = strVal(resp, "sessionId")

		// Collect events until we see idle
		var sawCreated, sawIdle bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(15 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "sessionId") != s1 {
				continue
			}
			if strVal(ev, "event") == "created" {
				sawCreated = true
			}
			if strVal(ev, "event") == "status" && strVal(ev, "status") == "idle" {
				sawIdle = true
				break
			}
		}
		if !sawCreated {
			t.Fatal("expected to see created event for s1")
		}
		if !sawIdle {
			t.Fatal("expected to see status=idle event for s1")
		}
	})

	t.Run("subscribe receives created events with parentId", func(t *testing.T) {
		sub := pool.subscribe(Msg{"sessions": []string{}, "events": []string{"created"}})

		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: sub2", "parentId": s1})
		assertNotError(t, resp)
		s2 = strVal(resp, "sessionId")

		// Look for created event with parentId
		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(10 * time.Second)
			if !ok {
				t.Fatal("expected created event for s2")
			}
			if strVal(ev, "event") == "created" && strVal(ev, "sessionId") == s2 {
				if strVal(ev, "parentId") != s1 {
					t.Fatalf("expected parentId %s, got %q", s1, strVal(ev, "parentId"))
				}
				break
			}
		}

		pool.awaitStatus(s2, "idle", 120*time.Second)
	})

	t.Run("subscribe receives pool events", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"pool"}})

		pool.send(Msg{"type": "resize", "size": 3})

		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected pool event for resize")
		}
		if strVal(ev, "event") != "pool" {
			t.Fatalf("expected pool event, got %q", strVal(ev, "event"))
		}

		// Resize back
		pool.send(Msg{"type": "resize", "size": 2})
		sub.drain()
	})

	t.Run("filter by sessions", func(t *testing.T) {
		sub := pool.subscribe(Msg{"sessions": []string{s1}})

		// Touch s2 — should NOT produce events on this subscription
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: filtered"})

		// Touch s1
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: visible"})

		// Collect events — only s1 events should appear
		var sawS1, sawS2 bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(5 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "sessionId") == s1 {
				sawS1 = true
			}
			if strVal(ev, "sessionId") == s2 {
				sawS2 = true
			}
		}
		if !sawS1 {
			t.Fatal("expected events for s1")
		}
		if sawS2 {
			t.Fatal("should not receive events for s2 with session filter")
		}

		pool.awaitStatus(s1, "idle", 120*time.Second)
		pool.awaitStatus(s2, "idle", 120*time.Second)
	})

	t.Run("filter by events", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"status"}})

		// Archive s2 to free a slot, start a new session
		pool.send(Msg{"type": "offload", "sessionId": s2})
		pool.awaitStatus(s2, "offloaded", 10*time.Second)

		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: eventfilter"})
		assertNotError(t, r)
		s3 := strVal(r, "sessionId")

		// Should receive status events but NOT created events
		var sawStatus, sawCreated bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(10 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "event") == "status" {
				sawStatus = true
			}
			if strVal(ev, "event") == "created" {
				sawCreated = true
			}
			// Stop once we see idle for s3
			if strVal(ev, "event") == "status" && strVal(ev, "sessionId") == s3 && strVal(ev, "status") == "idle" {
				break
			}
		}
		if !sawStatus {
			t.Fatal("expected status events")
		}
		if sawCreated {
			t.Fatal("should not receive created events with events=[status] filter")
		}

		// Restore s2 for later steps
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: back"})
		pool.awaitStatus(s2, "idle", 120*time.Second)

		// Archive s3 to stay within capacity
		pool.send(Msg{"type": "archive", "sessionId": s3})
	})

	t.Run("filter by statuses", func(t *testing.T) {
		sub := pool.subscribe(Msg{"statuses": []string{"idle"}})

		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: statusfilter"})

		// Should only see transitions TO idle, not to processing
		var sawIdle, sawProcessing bool
		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(15 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "status") == "idle" {
				sawIdle = true
				break
			}
			if strVal(ev, "status") == "processing" {
				sawProcessing = true
			}
		}
		if !sawIdle {
			t.Fatal("expected idle status event")
		}
		if sawProcessing {
			t.Fatal("should not receive processing events with statuses=[idle] filter")
		}
	})

	t.Run("filters are ANDed", func(t *testing.T) {
		sub := pool.subscribe(Msg{"sessions": []string{s1}, "statuses": []string{"idle"}})

		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: and1"})
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: and2"})

		// Only s1→idle should arrive
		var matched bool
		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(15 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "sessionId") == s2 {
				t.Fatal("should not receive events for s2")
			}
			if strVal(ev, "status") == "processing" {
				t.Fatal("should not receive processing events")
			}
			if strVal(ev, "sessionId") == s1 && strVal(ev, "status") == "idle" {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatal("expected s1 idle event")
		}

		pool.awaitStatus(s1, "idle", 120*time.Second)
		pool.awaitStatus(s2, "idle", 120*time.Second)
	})

	t.Run("re-subscribe replaces filters", func(t *testing.T) {
		sub := pool.subscribe(Msg{"sessions": []string{s1}})

		// Re-subscribe on same connection with different filter
		sub.resubscribe(Msg{"sessions": []string{s2}})

		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: old"})
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: new"})

		// Should only see s2 events (filters were replaced)
		var sawS1, sawS2 bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(5 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "sessionId") == s1 {
				sawS1 = true
			}
			if strVal(ev, "sessionId") == s2 {
				sawS2 = true
			}
		}
		if sawS1 {
			t.Fatal("should not receive s1 events after re-subscribe")
		}
		if !sawS2 {
			t.Fatal("expected s2 events after re-subscribe")
		}

		pool.awaitStatus(s1, "idle", 120*time.Second)
		pool.awaitStatus(s2, "idle", 120*time.Second)
	})

	t.Run("updated event — priority change", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"updated"}})

		pool.send(Msg{"type": "set-priority", "sessionId": s1, "priority": 5})

		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for priority change")
		}
		if strVal(ev, "event") != "updated" {
			t.Fatalf("expected updated event, got %q", strVal(ev, "event"))
		}
		if strVal(ev, "sessionId") != s1 {
			t.Fatalf("expected sessionId %s, got %q", s1, strVal(ev, "sessionId"))
		}

		changes, _ := ev["changes"].(map[string]any)
		if numVal(changes, "priority") != 5 {
			t.Fatalf("expected priority 5 in changes, got %v", changes["priority"])
		}

		// Reset priority
		pool.send(Msg{"type": "set-priority", "sessionId": s1, "priority": 0})
		sub.drain()
	})

	t.Run("updated event — pin/unpin", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"updated"}})

		pool.send(Msg{"type": "pin", "sessionId": s1})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for pin")
		}
		changes, _ := ev["changes"].(map[string]any)
		if !boolVal(changes, "pinned") {
			t.Fatal("expected pinned=true in changes")
		}

		pool.send(Msg{"type": "unpin", "sessionId": s1})
		ev, ok = sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for unpin")
		}
		changes, _ = ev["changes"].(map[string]any)
		if boolVal(changes, "pinned") {
			t.Fatal("expected pinned=false in changes")
		}
	})

	t.Run("updated event — fields filter", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"updated"}, "fields": []string{"priority"}})

		// Priority change should trigger event
		pool.send(Msg{"type": "set-priority", "sessionId": s1, "priority": 3})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for priority change")
		}
		if strVal(ev, "event") != "updated" {
			t.Fatalf("expected updated event, got %q", strVal(ev, "event"))
		}

		// Pin should NOT trigger event (pinned not in fields filter)
		pool.send(Msg{"type": "pin", "sessionId": s1})
		_, ok = sub.nextWithin(2 * time.Second)
		if ok {
			t.Fatal("should not receive updated event for pin when fields=[priority]")
		}

		pool.send(Msg{"type": "unpin", "sessionId": s1})
		pool.send(Msg{"type": "set-priority", "sessionId": s1, "priority": 0})
	})

	t.Run("archived and unarchived events", func(t *testing.T) {
		sub := pool.subscribe(Msg{"events": []string{"archived", "unarchived"}})

		// Offload first so archive works
		pool.send(Msg{"type": "offload", "sessionId": s1})
		pool.awaitStatus(s1, "offloaded", 10*time.Second)

		pool.send(Msg{"type": "archive", "sessionId": s1})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected archived event")
		}
		if strVal(ev, "event") != "archived" {
			t.Fatalf("expected archived event, got %q", strVal(ev, "event"))
		}
		if strVal(ev, "sessionId") != s1 {
			t.Fatalf("expected sessionId %s, got %q", s1, strVal(ev, "sessionId"))
		}

		pool.send(Msg{"type": "unarchive", "sessionId": s1})
		ev, ok = sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected unarchived event")
		}
		if strVal(ev, "event") != "unarchived" {
			t.Fatalf("expected unarchived event, got %q", strVal(ev, "event"))
		}

		// Restore s1 for the next test
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: back"})
		pool.awaitStatus(s1, "idle", 120*time.Second)
	})

	t.Run("multiple concurrent subscribers", func(t *testing.T) {
		sub1 := pool.subscribe(Msg{"sessions": []string{s1}})
		sub2 := pool.subscribe(Msg{"sessions": []string{s2}})

		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: multi1"})
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: multi2"})

		// sub1 should only see s1, sub2 should only see s2
		var sub1SawS1, sub1SawS2 bool
		for i := 0; i < 10; i++ {
			ev, ok := sub1.nextWithin(5 * time.Second)
			if !ok {
				break
			}
			sid := strVal(ev, "sessionId")
			if sid == s1 {
				sub1SawS1 = true
			}
			if sid == s2 {
				sub1SawS2 = true
			}
		}

		var sub2SawS1, sub2SawS2 bool
		for i := 0; i < 10; i++ {
			ev, ok := sub2.nextWithin(5 * time.Second)
			if !ok {
				break
			}
			sid := strVal(ev, "sessionId")
			if sid == s1 {
				sub2SawS1 = true
			}
			if sid == s2 {
				sub2SawS2 = true
			}
		}

		if !sub1SawS1 {
			t.Fatal("sub1 should receive s1 events")
		}
		if sub1SawS2 {
			t.Fatal("sub1 should NOT receive s2 events")
		}
		if !sub2SawS2 {
			t.Fatal("sub2 should receive s2 events")
		}
		if sub2SawS1 {
			t.Fatal("sub2 should NOT receive s1 events")
		}
	})
}
