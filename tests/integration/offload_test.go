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
//   1.  "start and wait for idle"
//   2.  "offload idle session"
//   3.  "stop on offloaded errors"
//   4.  "offload pinned session auto-unpins"
//   5.  "offload non-idle errors"
//   6.  "capture JSONL on offloaded session works"
//   7.  "capture buffer on offloaded session errors"
//   8.  "followup restores offloaded session"
//   9.  "process death transitions to offloaded"
//  10.  "wait returns error on session death"
//  11.  "archive idle session"
//  12.  "stop on archived errors"
//  13.  "archived session hidden from ls"
//  14.  "capture JSONL on archived session works"
//  15.  "capture buffer on archived session errors"
//  16.  "followup on archived errors"
//  17.  "pin on archived errors"
//  18.  "unarchive restores to offloaded"
//  19.  "unarchive on non-archived errors"
//  20.  "archive stops active session first"
//  21.  "archive is idempotent"
//  22.  "archive pinned session unpins first"
//  23.  "archive queued session cancels and archives"

import (
	"testing"
	"time"
)

func TestOffload(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string

	t.Run("start and wait for idle", func(t *testing.T) {
		r1 := pool.send(Msg{"type": "start", "prompt": "respond with exactly: offload test"})
		assertNotError(t, r1)
		s1 = strVal(r1, "sessionId")

		r2 := pool.send(Msg{"type": "start", "prompt": "respond with exactly: second"})
		assertNotError(t, r2)
		s2 = strVal(r2, "sessionId")

		pool.awaitStatus(s1, "idle", 60*time.Second)
		pool.awaitStatus(s2, "idle", 60*time.Second)
	})

	t.Run("offload idle session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "offload", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "offloaded")
		if session.PID != 0 {
			t.Fatalf("offloaded session should have no PID, got %v", session.PID)
		}
	})

	t.Run("stop on offloaded errors", func(t *testing.T) {
		// s1 is offloaded — nothing to stop
		resp := pool.send(Msg{"type": "stop", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("offload pinned session auto-unpins", func(t *testing.T) {
		pool.send(Msg{"type": "pin", "sessionId": s2})

		resp := pool.send(Msg{"type": "offload", "sessionId": s2})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s2})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "offloaded")
	})

	t.Run("offload non-idle errors", func(t *testing.T) {
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "run the bash command: sleep 60"})
		pool.awaitStatus(s2, "processing", 15*time.Second)

		resp := pool.send(Msg{"type": "offload", "sessionId": s2})
		assertError(t, resp)

		pool.send(Msg{"type": "stop", "sessionId": s2})
	})

	t.Run("capture JSONL on offloaded session works", func(t *testing.T) {
		// s1 is offloaded — JSONL capture reads from persisted transcript
		resp := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertNotError(t, resp)
		assertContains(t, strVal(resp, "content"), "offload test")

		// All JSONL detail levels should work on offloaded sessions
		for _, detail := range []string{"assistant", "tools", "raw"} {
			r := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "detail": detail})
			assertNotError(t, r)
			assertNonEmpty(t, "detail="+detail+" content", strVal(r, "content"))
		}
	})

	t.Run("capture buffer on offloaded session errors", func(t *testing.T) {
		// Buffer capture requires a live terminal
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 1})
		assertError(t, resp)

		resp = pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 0})
		assertError(t, resp)
	})

	t.Run("followup restores offloaded session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: restored"})
		assertNotError(t, resp)
		assertType(t, resp, "started")

		pool.awaitStatus(s1, "idle", 60*time.Second)

		capture := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertContains(t, strVal(capture, "content"), "restored")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "idle")
		if session.PID <= 0 {
			t.Fatalf("restored session should have a PID, got %v", session.PID)
		}
	})

	t.Run("process death transitions to offloaded", func(t *testing.T) {
		// s2 is idle with a live PID — kill it directly
		info := pool.send(Msg{"type": "info", "sessionId": s2})
		session := parseSession(t, info["session"])
		pid := int(session.PID)
		if pid <= 0 {
			t.Fatalf("expected live PID for s2, got %v", pid)
		}

		killPID(t, pid)

		s := pool.awaitStatus(s2, "offloaded", 15*time.Second)
		if s.PID != 0 {
			t.Fatalf("dead session should have PID 0, got %v", s.PID)
		}
	})

	t.Run("wait returns error on session death", func(t *testing.T) {
		// s1 is idle — start a long-running task, then kill the process mid-wait
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "run the bash command: sleep 120"})
		pool.awaitStatus(s1, "processing", 15*time.Second)

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		pid := int(session.PID)

		// Use a separate connection for the concurrent wait (main conn isn't thread-safe)
		waitConn, waitScanner := pool.newConn()
		ch := make(chan Msg, 1)
		go func() {
			ch <- pool.sendOn(waitConn, waitScanner,
				Msg{"type": "wait", "sessionId": s1, "timeout": 30000},
			)
		}()

		// Give wait a moment to register on the server before killing
		time.Sleep(500 * time.Millisecond)
		killPID(t, pid)

		assertError(t, <-ch)
	})

	t.Run("archive idle session", func(t *testing.T) {
		// s1 is offloaded after process death — archive directly
		pool.awaitStatus(s1, "offloaded", 10*time.Second)

		resp := pool.send(Msg{"type": "archive", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
	})

	t.Run("stop on archived errors", func(t *testing.T) {
		// s1 is archived — nothing to stop
		resp := pool.send(Msg{"type": "stop", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("archived session hidden from ls", func(t *testing.T) {
		// Default ls (no archived flag) should exclude archived sessions
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		if _, found := findSession(sessions, s1); found {
			t.Fatal("archived session should not appear in ls without archived flag")
		}

		// With archived: true, it should be visible
		lsArchived := pool.send(Msg{"type": "ls", "all": true, "archived": true})
		archivedSessions := parseSessions(t, lsArchived)
		s, found := findSession(archivedSessions, s1)
		if !found {
			t.Fatal("archived session should appear with archived: true")
		}
		assertStatus(t, s, "archived")
	})

	t.Run("capture JSONL on archived session works", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1})
		assertNotError(t, resp)
		assertNonEmpty(t, "archived capture", strVal(resp, "content"))
	})

	t.Run("capture buffer on archived session errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "buffer", "turns": 1})
		assertError(t, resp)
	})

	t.Run("followup on archived errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "nope"})
		assertError(t, resp)
	})

	t.Run("pin on archived errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "pin", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("unarchive restores to offloaded", func(t *testing.T) {
		resp := pool.send(Msg{"type": "unarchive", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "offloaded")

		// Should be visible in ls again
		lsResp := pool.send(Msg{"type": "ls", "all": true})
		sessions := parseSessions(t, lsResp)
		if _, found := findSession(sessions, s1); !found {
			t.Fatal("unarchived session should appear in ls")
		}
	})

	t.Run("unarchive on non-archived errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "unarchive", "sessionId": s2})
		assertError(t, resp)
	})

	t.Run("archive stops active session first", func(t *testing.T) {
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "run the bash command: sleep 60"})
		pool.awaitStatus(s2, "processing", 15*time.Second)

		resp := pool.send(Msg{"type": "archive", "sessionId": s2})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s2})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
	})

	t.Run("archive is idempotent", func(t *testing.T) {
		resp := pool.send(Msg{"type": "archive", "sessionId": s2})
		assertNotError(t, resp)
		assertType(t, resp, "ok")
	})

	// State: s1 offloaded, s2 archived

	t.Run("archive pinned session unpins first", func(t *testing.T) {
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: pin test"})
		pool.awaitStatus(s1, "idle", 60*time.Second)

		pool.send(Msg{"type": "pin", "sessionId": s1})

		resp := pool.send(Msg{"type": "archive", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
		if session.Pinned {
			t.Fatal("archived session should not still be pinned")
		}
	})

	// State: s1 archived, s2 archived, both slots free

	t.Run("archive queued session cancels and archives", func(t *testing.T) {
		pool.send(Msg{"type": "unarchive", "sessionId": s1})
		pool.send(Msg{"type": "unarchive", "sessionId": s2})
		pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: fill1"})
		pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "respond with exactly: fill2"})
		pool.awaitStatus(s1, "idle", 60*time.Second)
		pool.awaitStatus(s2, "idle", 60*time.Second)

		// Pin both so the new session can't evict them — forces queuing
		pool.send(Msg{"type": "pin", "sessionId": s1})
		pool.send(Msg{"type": "pin", "sessionId": s2})

		// Pool is full (2/2), both pinned — new session must queue
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: queued"})
		assertNotError(t, r)
		s3 := strVal(r, "sessionId")

		info := pool.send(Msg{"type": "info", "sessionId": s3})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "queued")

		resp := pool.send(Msg{"type": "archive", "sessionId": s3})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info = pool.send(Msg{"type": "info", "sessionId": s3})
		session = parseSession(t, info["session"])
		assertStatus(t, session, "archived")
	})
}
