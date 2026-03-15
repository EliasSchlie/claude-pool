package integration

// TestOffload — Offload, capture, restore, and archive lifecycle flow (CLI)
//
// Pool size: 2
//
// Tests session lifecycle when sessions are offloaded (via eviction, not explicit
// command — offload is not a CLI command), captured in various states, restored
// via followup, and archived/unarchived.
//
// Offloading is triggered by starting new sessions when the pool is full — the
// LRU idle session gets displaced. This tests real-world behavior rather than
// relying on a debug command.
//
// Flow:
//
//   1.  "start and wait for idle"
//   2.  "eviction offloads LRU session"
//   3.  "stop on offloaded errors"
//   4.  "capture JSONL on offloaded works"
//   5.  "capture buffer on offloaded errors"
//   6.  "followup restores offloaded session"
//   7.  "process death transitions to offloaded"
//   8.  "archive idle session"
//   9.  "stop on archived errors"
//  10.  "archived session hidden from ls"
//  11.  "capture JSONL on archived works"
//  12.  "capture buffer on archived errors"
//  13.  "followup on archived errors"
//  14.  "unarchive restores to offloaded"
//  15.  "unarchive on non-archived errors"
//  16.  "archive stops active session first"
//  17.  "archive is idempotent"
//  18.  "archive queued session cancels and archives"

import (
	"testing"
	"time"
)

func TestOffload(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string

	t.Run("start and wait for idle", func(t *testing.T) {
		r1 := pool.runJSON("start", "--prompt", "respond with exactly: offload test")
		s1 = strVal(r1, "sessionId")

		r2 := pool.runJSON("start", "--prompt", "respond with exactly: second")
		s2 = strVal(r2, "sessionId")

		pool.waitForIdle(s1, 300*time.Second)
		pool.waitForIdle(s2, 300*time.Second)
	})

	t.Run("eviction offloads LRU session", func(t *testing.T) {
		// Pool is full (2/2). Starting a third session should evict s1 (LRU).
		// Touch s2 to make it more recently used.
		pool.run("followup", "--session", s2, "--prompt", "respond with exactly: touched")
		pool.waitForIdle(s2, 300*time.Second)

		r3 := pool.runJSON("start", "--prompt", "respond with exactly: eviction")
		s3 := strVal(r3, "sessionId")

		pool.waitForStatus(s1, "offloaded", 15*time.Second)
		pool.waitForIdle(s3, 300*time.Second)

		info := pool.getSessionInfo(s1)
		assertStatus(t, info, "offloaded")
		if info.PID != 0 {
			t.Fatalf("offloaded session should have no PID, got %v", info.PID)
		}

		pool.run("archive", "--session", s3)
	})

	t.Run("stop on offloaded errors", func(t *testing.T) {
		result := pool.run("stop", "--session", s1)
		assertExitError(t, result)
	})

	t.Run("capture JSONL on offloaded works", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s1)
		assertContains(t, strVal(resp, "content"), "offload test")

		for _, detail := range []string{"assistant", "tools", "raw"} {
			r := pool.runJSON("capture", "--session", s1, "--source", "jsonl", "--detail", detail)
			assertNonEmpty(t, "detail="+detail, strVal(r, "content"))
		}
	})

	t.Run("capture buffer on offloaded errors", func(t *testing.T) {
		result := pool.run("capture", "--session", s1, "--source", "buffer", "--turns", "1")
		assertExitError(t, result)

		result = pool.run("capture", "--session", s1, "--source", "buffer", "--turns", "0")
		assertExitError(t, result)
	})

	t.Run("followup restores offloaded session", func(t *testing.T) {
		resp := pool.runJSON("followup", "--session", s1, "--prompt", "respond with exactly: restored")
		if strVal(resp, "sessionId") != s1 {
			t.Fatalf("expected sessionId %s", s1)
		}

		pool.waitForIdle(s1, 300*time.Second)

		capture := pool.runJSON("capture", "--session", s1)
		assertContains(t, strVal(capture, "content"), "restored")

		info := pool.getSessionInfo(s1)
		assertStatus(t, info, "idle")
		if info.PID <= 0 {
			t.Fatalf("restored session should have a PID, got %v", info.PID)
		}
	})

	t.Run("process death transitions to offloaded", func(t *testing.T) {
		info := pool.getSessionInfo(s2)
		pid := int(info.PID)
		if pid <= 0 {
			t.Fatalf("expected live PID for s2, got %v", pid)
		}

		killPID(t, pid)
		pool.waitForStatus(s2, "offloaded", 15*time.Second)
	})

	t.Run("archive idle session", func(t *testing.T) {
		result := pool.run("archive", "--session", s2)
		assertExitOK(t, result)

		info := pool.getSessionInfo(s2)
		assertStatus(t, info, "archived")
	})

	t.Run("stop on archived errors", func(t *testing.T) {
		result := pool.run("stop", "--session", s2)
		assertExitError(t, result)
	})

	t.Run("archived session hidden from ls", func(t *testing.T) {
		sessions := pool.listSessions()
		if _, found := findSession(sessions, s2); found {
			t.Fatal("archived session should not appear in default ls")
		}

		archivedSessions := pool.listSessions("--archived")
		s, found := findSession(archivedSessions, s2)
		if !found {
			t.Fatal("archived session should appear with --archived")
		}
		assertStatus(t, s, "archived")
	})

	t.Run("capture JSONL on archived works", func(t *testing.T) {
		resp := pool.runJSON("capture", "--session", s2)
		assertNonEmpty(t, "archived capture", strVal(resp, "content"))
	})

	t.Run("capture buffer on archived errors", func(t *testing.T) {
		result := pool.run("capture", "--session", s2, "--source", "buffer", "--turns", "1")
		assertExitError(t, result)
	})

	t.Run("followup on archived errors", func(t *testing.T) {
		result := pool.run("followup", "--session", s2, "--prompt", "nope")
		assertExitError(t, result)
	})

	t.Run("unarchive restores to offloaded", func(t *testing.T) {
		result := pool.run("unarchive", "--session", s2)
		assertExitOK(t, result)

		info := pool.getSessionInfo(s2)
		assertStatus(t, info, "offloaded")

		sessions := pool.listSessions()
		if _, found := findSession(sessions, s2); !found {
			t.Fatal("unarchived session should appear in ls")
		}
	})

	t.Run("unarchive on non-archived errors", func(t *testing.T) {
		result := pool.run("unarchive", "--session", s1)
		assertExitError(t, result)
	})

	t.Run("archive stops active session first", func(t *testing.T) {
		pool.run("followup", "--session", s1, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(s1, "processing", 15*time.Second)

		result := pool.run("archive", "--session", s1)
		assertExitOK(t, result)

		info := pool.getSessionInfo(s1)
		assertStatus(t, info, "archived")
	})

	t.Run("archive is idempotent", func(t *testing.T) {
		result := pool.run("archive", "--session", s1)
		assertExitOK(t, result)
	})

	t.Run("archive queued session cancels and archives", func(t *testing.T) {
		pool.run("unarchive", "--session", s1)
		pool.run("unarchive", "--session", s2)
		pool.run("followup", "--session", s1, "--prompt", "respond with exactly: fill1")
		pool.run("followup", "--session", s2, "--prompt", "respond with exactly: fill2")
		pool.waitForIdle(s1, 300*time.Second)
		pool.waitForIdle(s2, 300*time.Second)

		// Pin both so new session can't evict them — forces queuing
		pool.run("set", "--session", s1, "--pinned", "300")
		pool.run("set", "--session", s2, "--pinned", "300")

		resp := pool.runJSON("start", "--prompt", "respond with exactly: queued")
		s3 := strVal(resp, "sessionId")

		info := pool.getSessionInfo(s3)
		assertStatus(t, info, "queued")

		result := pool.run("archive", "--session", s3)
		assertExitOK(t, result)

		info = pool.getSessionInfo(s3)
		assertStatus(t, info, "archived")
	})
}
