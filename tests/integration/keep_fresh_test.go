package integration

// TestKeepFresh — Fresh slot maintenance behavior (CLI)
//
// Pool size: 3
//
// Tests that the pool proactively offloads idle sessions to maintain
// a target number of fresh slots. Uses keepFresh=1 (the spec default).
//
// Flow:
//
//   1.  "init with keepFresh=1"
//   2.  "fresh slots maintained after sessions go idle"
//   3.  "keepFresh respects pins"
//   4.  "keepFresh=0 disables proactive offloading"
//   5.  "config update triggers fresh slot maintenance"
//   6.  "processing sessions block keepFresh"

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeepFresh(t *testing.T) {
	testDir := filepath.Join(runDir, t.Name())
	cpHome := filepath.Join(testDir, ".claude-pool")
	if err := os.MkdirAll(cpHome, 0755); err != nil {
		t.Fatalf("failed to create .claude-pool dir: %v", err)
	}
	pool := newNamedPool(t, "test", cpHome, filepath.Join(testDir, "workdir"))

	t.Run("init with keepFresh=1", func(t *testing.T) {
		result := pool.run("init", "--size", "3",
			"--dir", pool.workDir,
			"--keep-fresh", "1",
			"--flags", "--dangerously-skip-permissions --model haiku")
		assertExitOK(t, result)

		pool.waitForSlotsReady(3, 90*time.Second)

		// Verify config has keepFresh=1
		resp := pool.runJSON("config")
		cfg, _ := resp["config"].(map[string]any)
		assertNumVal(t, cfg, "keepFresh", 1)
	})

	t.Run("fresh slots maintained after sessions go idle", func(t *testing.T) {
		// Start 3 sessions sequentially — order determines LRU.
		// s0 finishes first → oldest idle → should be evicted by keepFresh.
		var sessions []string
		for i := 0; i < 3; i++ {
			resp := pool.runJSON("start", "--prompt", fmt.Sprintf("respond with exactly: keep-fresh-%d", i))
			sid := strVal(resp, "sessionId")
			sessions = append(sessions, sid)
			pool.waitForIdle(sid, 300*time.Second)
		}

		// All 3 are idle. With keepFresh=1, the pool should proactively offload
		// the LRU session (sessions[0]) to free a slot.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			info := pool.getSessionInfo(sessions[0])
			if info.Status == "offloaded" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Verify the LRU session (sessions[0]) was offloaded
		info0 := pool.getSessionInfo(sessions[0])
		assertStatus(t, info0, "offloaded")

		// Verify the other two are still idle (not offloaded)
		info1 := pool.getSessionInfo(sessions[1])
		assertStatus(t, info1, "idle")
		info2 := pool.getSessionInfo(sessions[2])
		assertStatus(t, info2, "idle")

		// Verify health shows 1 fresh slot. The recycled process goes through
		// clearing → fresh (SPEC: slot states), so wait for the transition.
		pool.waitForSlotCondition("fresh>=1", func(slots Msg) bool {
			return numVal(slots, "fresh") >= 1
		}, 30*time.Second)
	})

	t.Run("keepFresh respects pins", func(t *testing.T) {
		// State: 2 idle, 1 offloaded, 1 fresh. Pin both idle sessions.
		sessions := pool.listSessions("--status", "idle")
		idleBefore := len(sessions)
		if idleBefore < 2 {
			t.Fatalf("expected at least 2 idle sessions, got %d", idleBefore)
		}
		for _, s := range sessions {
			pool.run("set", "--session", s.SessionID, "--pinned", "300")
		}

		// Request keepFresh=2 — pool would need to offload an idle session,
		// but all idle sessions are pinned, so it can't.
		pool.run("config", "--set", "keepFresh=2")

		// Wait — proactive offloading should NOT happen on pinned sessions
		time.Sleep(5 * time.Second)

		health := pool.getHealth()
		slots, _ := health["slots"].(map[string]any)
		idleAfter := numVal(slots, "idle")
		if int(idleAfter) != idleBefore {
			t.Fatalf("pinned idle sessions were offloaded: had %d, now have %v", idleBefore, idleAfter)
		}

		// Unpin all
		sessions = pool.listSessions("--status", "idle")
		for _, s := range sessions {
			pool.run("set", "--session", s.SessionID, "--pinned", "false")
		}

		// Restore keepFresh
		pool.run("config", "--set", "keepFresh=1")
	})

	t.Run("keepFresh=0 disables proactive offloading", func(t *testing.T) {
		pool.run("config", "--set", "keepFresh=0")

		// Archive everything and start fresh
		pool.archiveAll()

		// Start 3 sessions to fill all slots
		var sids []string
		for i := 0; i < 3; i++ {
			resp := pool.runJSON("start", "--prompt", fmt.Sprintf("respond with exactly: no-fresh-%d", i))
			sids = append(sids, strVal(resp, "sessionId"))
		}
		for _, sid := range sids {
			pool.waitForIdle(sid, 300*time.Second)
		}

		// With keepFresh=0, no proactive offloading — all 3 should remain idle
		time.Sleep(5 * time.Second)

		health := pool.getHealth()
		slots, _ := health["slots"].(map[string]any)
		assertNumVal(t, slots, "idle", 3)
		assertNumVal(t, slots, "fresh", 0)
	})

	t.Run("config update triggers fresh slot maintenance", func(t *testing.T) {
		// Pool is full (3 idle, 0 fresh, keepFresh=0). Set keepFresh=1 — should trigger offload.
		pool.run("config", "--set", "keepFresh=1")

		pool.waitForSlotCondition("fresh>=1", func(slots Msg) bool {
			return numVal(slots, "fresh") >= 1
		}, 30*time.Second)
	})

	t.Run("processing sessions block keepFresh", func(t *testing.T) {
		// SPEC: "best-effort — if all loaded sessions are pinned or processing,
		// the pool can't free anything."
		pool.run("config", "--set", "keepFresh=0")

		// Clean slate: archive everything, start 2 sessions
		pool.archiveAll()

		proc := pool.startSession("respond with exactly: kf-proc")
		idle := pool.startSession("respond with exactly: kf-idle")

		// Put one into processing
		pool.run("followup", "--session", proc, "--prompt", "run the bash command: sleep 60")
		pool.waitForStatus(proc, "processing", 15*time.Second)

		// keepFresh=3 wants all slots fresh — can offload idle but NOT processing
		pool.run("config", "--set", "keepFresh=3")
		time.Sleep(5 * time.Second)

		// idle session should be offloaded
		infoIdle := pool.getSessionInfo(idle)
		assertStatus(t, infoIdle, "offloaded")

		// Processing session must survive
		infoProc := pool.getSessionInfo(proc)
		assertStatus(t, infoProc, "processing")

		// Cleanup: restore keepFresh before stopping so the session stays idle.
		// SPEC: "After any session becomes idle, the pool checks whether the
		// number of fresh slots is below keepFresh." With keepFresh=3 still
		// active, stop → idle → immediate offload (never catches idle).
		pool.run("config", "--set", "keepFresh=1")
		pool.run("stop", "--session", proc)
		pool.waitForStatus(proc, "idle", 15*time.Second)
	})
}
