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
//   3.  "offload pinned session errors"
//   4.  "offload non-idle errors"
//   5.  "capture JSONL on offloaded session works"
//   6.  "capture buffer on offloaded session errors"
//   7.  "followup restores offloaded session"
//   8.  "archive idle session"
//   9.  "archived session hidden from ls"
//  10.  "capture JSONL on archived session works"
//  11.  "capture buffer on archived session errors"
//  12.  "followup on archived errors"
//  13.  "pin on archived errors"
//  14.  "unarchive restores to offloaded"
//  15.  "unarchive on non-archived errors"
//  16.  "archive stops active session first"
//  17.  "archive is idempotent"

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
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-short"})
		assertNotError(t, resp)
		assertContains(t, strVal(resp, "content"), "offload test")

		for _, format := range []string{"jsonl-last", "jsonl-long", "jsonl-full"} {
			r := pool.send(Msg{"type": "capture", "sessionId": s1, "format": format})
			assertNotError(t, r)
			assertNonEmpty(t, format+" content", strVal(r, "content"))
		}
	})

	t.Run("capture buffer on offloaded session errors", func(t *testing.T) {
		// Buffer capture requires a live terminal
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-last"})
		assertError(t, resp)

		resp = pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-full"})
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

	t.Run("archive idle session", func(t *testing.T) {
		// Offload first so it frees the slot, then archive
		pool.send(Msg{"type": "offload", "sessionId": s1})
		pool.awaitStatus(s1, "offloaded", 10*time.Second)

		resp := pool.send(Msg{"type": "archive", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		info := pool.send(Msg{"type": "info", "sessionId": s1})
		session := parseSession(t, info["session"])
		assertStatus(t, session, "archived")
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
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-short"})
		assertNotError(t, resp)
		assertNonEmpty(t, "archived capture", strVal(resp, "content"))
	})

	t.Run("capture buffer on archived session errors", func(t *testing.T) {
		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "buffer-last"})
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
}
