package integration

// TestSubscribe — Event stream, filters, re-subscribe, and updated events flow (API-only)
//
// Pool size: 2
//
// This flow tests the subscribe system: receiving events, applying filters, dynamically
// updating subscriptions, and verifying the "updated" event type for property changes.
//
// Subscribe is an API-only feature (not exposed in CLI). The pool is created
// via CLI init, then pool.dial() opens a socket connection for API commands.
//
// Flow:
//
//   1.  "subscribe receives status events"
//   2.  "subscribe receives created events with parent"
//   3.  "subscribe receives pool events"
//   4.  "filter by sessions"
//   5.  "filter by events"
//   6.  "filter by statuses"
//   7.  "filters are ANDed"
//   8.  "re-subscribe replaces filters"
//   9.  "updated event — priority change"
//  10.  "updated event — pin/unpin"
//  11.  "updated event — cwd change"
//  12.  "updated event — fields filter"
//  13.  "archived and unarchived events"
//  14.  "multiple concurrent subscribers"

import (
	"testing"
	"time"
)

func TestSubscribe(t *testing.T) {
	p := setupPool(t, 2)
	sc := p.dial()

	var s1, s2 string

	t.Run("subscribe receives status events", func(t *testing.T) {
		sub := sc.subscribe(Msg{})

		resp := p.runJSON("start", "--prompt", "respond with exactly: sub1")
		s1 = strVal(resp, "sessionId")

		var sawCreated, sawIdle bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(30 * time.Second)
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
			t.Fatal("expected created event for s1")
		}
		if !sawIdle {
			t.Fatal("expected status=idle event for s1")
		}
	})

	t.Run("subscribe receives created events with parent", func(t *testing.T) {
		sub := sc.subscribe(Msg{"sessions": []string{}, "events": []string{"created"}})

		resp := p.runJSON("start", "--prompt", "respond with exactly: sub2", "--parent", s1)
		s2 = strVal(resp, "sessionId")

		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(10 * time.Second)
			if !ok {
				t.Fatal("expected created event for s2")
			}
			if strVal(ev, "event") == "created" && strVal(ev, "sessionId") == s2 {
				if strVal(ev, "parent") != s1 {
					t.Fatalf("expected parent %s, got %q", s1, strVal(ev, "parent"))
				}
				break
			}
		}

		p.waitForStatus(s2, "idle", 60*time.Second)
	})

	t.Run("subscribe receives pool events", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"pool"}})
		defer sub.close()

		sc.send(Msg{"type": "resize", "size": 3})

		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected pool event for resize")
		}
		if strVal(ev, "event") != "pool" {
			t.Fatalf("expected pool event, got %q", strVal(ev, "event"))
		}

		sc.send(Msg{"type": "resize", "size": 2})
		sub.drain()
	})

	t.Run("filter by sessions", func(t *testing.T) {
		sub := sc.subscribe(Msg{"sessions": []string{s1}})

		p.run("followup", "--session", s2, "--prompt", "respond with exactly: filtered")
		p.run("followup", "--session", s1, "--prompt", "respond with exactly: visible")

		var sawS1, sawS2 bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(10 * time.Second)
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

		p.waitForStatus(s1, "idle", 60*time.Second)
		p.waitForStatus(s2, "idle", 60*time.Second)
	})

	t.Run("filter by events", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"status"}})

		sc.send(Msg{"type": "offload", "sessionId": s2})
		p.waitForStatus(s2, "offloaded", 10*time.Second)

		r := p.runJSON("start", "--prompt", "respond with exactly: eventfilter")
		s3 := strVal(r, "sessionId")

		var sawStatus, sawCreated bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(30 * time.Second)
			if !ok {
				break
			}
			if strVal(ev, "event") == "status" {
				sawStatus = true
			}
			if strVal(ev, "event") == "created" {
				sawCreated = true
			}
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

		p.run("followup", "--session", s2, "--prompt", "respond with exactly: back")
		p.waitForStatus(s2, "idle", 60*time.Second)
		p.run("archive", "--session", s3)
	})

	t.Run("filter by statuses", func(t *testing.T) {
		sub := sc.subscribe(Msg{"statuses": []string{"idle"}})

		p.run("followup", "--session", s1, "--prompt", "respond with exactly: statusfilter")

		var sawIdle, sawProcessing bool
		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(30 * time.Second)
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
		sub := sc.subscribe(Msg{"sessions": []string{s1}, "statuses": []string{"idle"}})

		p.run("followup", "--session", s1, "--prompt", "respond with exactly: and1")
		p.run("followup", "--session", s2, "--prompt", "respond with exactly: and2")

		var matched bool
		for i := 0; i < 10; i++ {
			ev, ok := sub.nextWithin(30 * time.Second)
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

		p.waitForStatus(s1, "idle", 60*time.Second)
		p.waitForStatus(s2, "idle", 60*time.Second)
	})

	t.Run("re-subscribe replaces filters", func(t *testing.T) {
		sub := sc.subscribe(Msg{"sessions": []string{s1}})
		defer sub.close()

		sub.resubscribe(Msg{"sessions": []string{s2}})

		p.run("followup", "--session", s1, "--prompt", "respond with exactly: old")
		p.run("followup", "--session", s2, "--prompt", "respond with exactly: new")

		var sawS1, sawS2 bool
		for i := 0; i < 20; i++ {
			ev, ok := sub.nextWithin(10 * time.Second)
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

		p.waitForStatus(s1, "idle", 60*time.Second)
		p.waitForStatus(s2, "idle", 60*time.Second)
	})

	t.Run("updated event — priority change", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"updated"}})

		sc.send(Msg{"type": "set", "sessionId": s1, "priority": 5})

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

		sc.send(Msg{"type": "set", "sessionId": s1, "priority": 0})
		sub.drain()
	})

	t.Run("updated event — pin/unpin", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"updated"}})

		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for pin")
		}
		changes, _ := ev["changes"].(map[string]any)
		if !boolVal(changes, "pinned") {
			t.Fatal("expected pinned=true in changes")
		}

		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": false})
		ev, ok = sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for unpin")
		}
		changes, _ = ev["changes"].(map[string]any)
		if boolVal(changes, "pinned") {
			t.Fatal("expected pinned=false in changes")
		}
	})

	t.Run("updated event — cwd change", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"updated"}, "fields": []string{"cwd"}})

		p.run("followup", "--session", s1, "--prompt", "run these bash commands: mkdir -p cwd_sub_test && cd cwd_sub_test")
		p.waitForIdle(s1, 60*time.Second)

		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for cwd change")
		}
		if strVal(ev, "event") != "updated" {
			t.Fatalf("expected updated event, got %q", strVal(ev, "event"))
		}
		changes, _ := ev["changes"].(map[string]any)
		assertContains(t, strVal(changes, "cwd"), "cwd_sub_test")
	})

	t.Run("updated event — fields filter", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"updated"}, "fields": []string{"priority"}})

		sc.send(Msg{"type": "set", "sessionId": s1, "priority": 3})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected updated event for priority change")
		}
		if strVal(ev, "event") != "updated" {
			t.Fatalf("expected updated event, got %q", strVal(ev, "event"))
		}

		// Pin should NOT trigger event (pinned not in fields filter)
		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
		_, ok = sub.nextWithin(2 * time.Second)
		if ok {
			t.Fatal("should not receive updated event for pin when fields=[priority]")
		}

		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": false})
		sc.send(Msg{"type": "set", "sessionId": s1, "priority": 0})
	})

	t.Run("archived and unarchived events", func(t *testing.T) {
		sub := sc.subscribe(Msg{"events": []string{"archived", "unarchived"}})

		sc.send(Msg{"type": "offload", "sessionId": s2})
		p.waitForStatus(s2, "offloaded", 10*time.Second)

		sc.send(Msg{"type": "archive", "sessionId": s2})
		ev, ok := sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected archived event")
		}
		if strVal(ev, "event") != "archived" {
			t.Fatalf("expected archived event, got %q", strVal(ev, "event"))
		}
		if strVal(ev, "sessionId") != s2 {
			t.Fatalf("expected sessionId %s, got %q", s2, strVal(ev, "sessionId"))
		}

		sc.send(Msg{"type": "unarchive", "sessionId": s2})
		ev, ok = sub.nextWithin(10 * time.Second)
		if !ok {
			t.Fatal("expected unarchived event")
		}
		if strVal(ev, "event") != "unarchived" {
			t.Fatalf("expected unarchived event, got %q", strVal(ev, "event"))
		}

		p.run("followup", "--session", s2, "--prompt", "respond with exactly: back")
		p.waitForStatus(s2, "idle", 60*time.Second)
	})

	t.Run("multiple concurrent subscribers", func(t *testing.T) {
		sub1 := sc.subscribe(Msg{"sessions": []string{s1}})
		sub2 := sc.subscribe(Msg{"sessions": []string{s2}})

		p.run("followup", "--session", s1, "--prompt", "respond with exactly: multi1")
		p.run("followup", "--session", s2, "--prompt", "respond with exactly: multi2")

		var sub1SawS1, sub1SawS2 bool
		for i := 0; i < 10; i++ {
			ev, ok := sub1.nextWithin(10 * time.Second)
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
			ev, ok := sub2.nextWithin(10 * time.Second)
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
