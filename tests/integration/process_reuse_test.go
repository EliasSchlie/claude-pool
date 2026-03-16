package integration

// TestProcessReuseEviction — Verify eviction reuses slot process (CLI)
//
// Pool size: 1
//
// The spec requires that Claude processes are persistent — the pool never spawns
// throwaway processes. When a slot needs a new session, /clear resets context.
// PID should stay stable across eviction.
//
// Flow:
//   1. Start A → idle → record PID
//   2. Start B (evicts A) → idle → assert same PID

import (
	"testing"
	"time"
)

func TestProcessReuseEviction(t *testing.T) {
	pool := setupPool(t, 1)

	var origPID float64
	var s1, s2 string

	t.Run("start session and record PID", func(t *testing.T) {
		// Prevents: pool killing and respawning Claude processes on eviction
		// (spec: "the pool never spawns throwaway processes")
		r1 := pool.runJSON("start", "--prompt", "respond with exactly: alpha")
		s1 = strVal(r1, "sessionId")
		pool.waitForIdle(s1, 300*time.Second)

		info := pool.getSessionInfo(s1)
		origPID = info.PID
		if origPID <= 0 {
			t.Fatalf("expected live PID, got %v", origPID)
		}
	})

	t.Run("eviction reuses slot process", func(t *testing.T) {
		// Start session B — evicts A from the only slot
		r2 := pool.runJSON("start", "--prompt", "respond with exactly: bravo")
		s2 = strVal(r2, "sessionId")
		pool.waitForIdle(s2, 300*time.Second)

		// The slot process should have been reused via /clear, not killed+respawned
		info := pool.getSessionInfo(s2)
		if info.PID != origPID {
			t.Fatalf("slot process killed and respawned: original PID=%d, new PID=%d — "+
				"expected same PID (process reuse via /clear)", int(origPID), int(info.PID))
		}
	})
}

// TestProcessReuseResume — Verify resume reuses slot process (CLI)
//
// Pool size: 2
//
// When resuming an offloaded session, the pool sends /resume <uuid> to a cleared
// slot process. No new processes should be spawned.
//
// Flow:
//   1. Start A, B → idle → record PID set
//   2. Touch B, start C (evicts A) → assert C's PID in original set
//   3. Followup on offloaded A → assert A's PID in original set

func TestProcessReuseResume(t *testing.T) {
	pool := setupPool(t, 2)

	var s1, s2 string
	pidSet := map[int]bool{}

	t.Run("start sessions and record PIDs", func(t *testing.T) {
		r1 := pool.runJSON("start", "--prompt", "respond with exactly: alpha")
		s1 = strVal(r1, "sessionId")
		pool.waitForIdle(s1, 300*time.Second)

		r2 := pool.runJSON("start", "--prompt", "respond with exactly: bravo")
		s2 = strVal(r2, "sessionId")
		pool.waitForIdle(s2, 300*time.Second)

		info1 := pool.getSessionInfo(s1)
		info2 := pool.getSessionInfo(s2)

		pidSet[int(info1.PID)] = true
		pidSet[int(info2.PID)] = true
		if len(pidSet) != 2 {
			t.Fatalf("expected 2 distinct PIDs, got %v", pidSetKeys(pidSet))
		}
	})

	t.Run("eviction reuses slot process", func(t *testing.T) {
		// Prevents: pool spawning new processes when evicting sessions
		// Touch s2 so s1 is LRU, then start C — evicts s1
		pool.run("followup", "--session", s2, "--prompt", "respond with exactly: touched")
		pool.waitForIdle(s2, 300*time.Second)

		r3 := pool.runJSON("start", "--prompt", "respond with exactly: charlie")
		s3 := strVal(r3, "sessionId")
		pool.waitForStatus(s1, "offloaded", 15*time.Second)
		pool.waitForIdle(s3, 300*time.Second)

		info3 := pool.getSessionInfo(s3)
		if !pidSet[int(info3.PID)] {
			t.Fatalf("new session got PID %d not in original set %v — "+
				"expected process reuse via /clear", int(info3.PID), pidSetKeys(pidSet))
		}
	})

	t.Run("resume reuses slot process", func(t *testing.T) {
		// Prevents: pool spawning new processes when resuming offloaded sessions
		// (spec: "sends /resume <uuid> to restore it into the cleared slot")
		pool.run("followup", "--session", s1, "--prompt", "respond with exactly: restored")
		pool.waitForIdle(s1, 300*time.Second)

		info := pool.getSessionInfo(s1)
		if !pidSet[int(info.PID)] {
			t.Fatalf("resumed session got PID %d not in original set %v — "+
				"expected process reuse via /resume", int(info.PID), pidSetKeys(pidSet))
		}
	})
}

func pidSetKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
