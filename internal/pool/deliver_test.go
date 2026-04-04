package pool

import (
	"testing"
	"time"

	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// TestDeliverSlashCommandTimingGap verifies that slash commands (like /resume)
// wait for the PTY buffer before sending Enter.
//
// Bug: deliverSlotPrompt skipped waitForBufferContent for prompts starting with "/",
// causing the command text and Enter to arrive as a single PTY read. Claude Code
// treated "/resume <uuid>" as a user message instead of a slash command.
//
// The fix removes the HasPrefix("/") guard so all prompts wait for the buffer.
// The buffer wait adds at least one 50ms poll cycle, so we assert that slash
// command delivery takes measurably longer than the base delay sequence.
func TestDeliverSlashCommandTimingGap(t *testing.T) {
	proc, err := ptyPkg.Spawn(ptyPkg.SpawnOpts{
		Flags: "",
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Skipf("cannot spawn claude: %v", err)
	}
	defer proc.Kill()

	// Wait for Claude to start so the PTY buffer is active
	time.Sleep(3 * time.Second)

	m := &Manager{
		done:  make(chan struct{}),
		slots: []*Slot{},
	}
	sl := &Slot{
		Process: proc,
	}

	// Deliver a slash command with a short settle delay.
	// Base timing (no buffer wait): settle(10ms) + esc-wait(100ms) + ctrl-u-wait(50ms) = 160ms
	// With buffer wait: adds up to 200ms polling (at least one 50ms tick)
	cmd := "/resume test-uuid-1234"
	start := time.Now()
	m.deliverSlotPrompt(sl, cmd, 10*time.Millisecond)

	// deliverSlotPrompt spawns a goroutine. Wait for it to finish by
	// polling the Delivering channel it sets on the slot.
	time.Sleep(50 * time.Millisecond) // let goroutine start and set sl.Delivering
	ch := sl.Delivering
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("delivery goroutine timed out")
		}
	} else {
		// Goroutine might have already finished
		time.Sleep(500 * time.Millisecond)
	}

	elapsed := time.Since(start)

	// Without the fix (slash commands skip buffer wait):
	//   settle(10) + esc(100) + ctrl-u(50) + write + enter ≈ 165ms
	//
	// With the fix (buffer wait runs for all prompts):
	//   settle(10) + esc(100) + ctrl-u(50) + write + buffer-wait(50-200ms) + enter ≈ 215-365ms
	//
	// We use 200ms as the threshold: anything below means the buffer wait was skipped.
	minExpected := 200 * time.Millisecond
	if elapsed < minExpected {
		t.Errorf("slash command delivery completed in %v, expected >= %v\n"+
			"waitForBufferContent was likely skipped for slash commands.\n"+
			"This causes /resume and Enter to arrive as one PTY read,\n"+
			"making Claude treat it as a user message instead of a slash command.",
			elapsed.Round(time.Millisecond), minExpected)
	} else {
		t.Logf("slash command delivery took %v (includes buffer wait)", elapsed.Round(time.Millisecond))
	}
}
