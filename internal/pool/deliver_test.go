package pool

import (
	"testing"
	"time"

	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// TestDeliverSlashCommandWaitsForRenderedContent verifies that slash commands
// (like /resume) wait for the command text to appear in the rendered terminal
// screen before sending Enter.
//
// Bug: deliverSlotPrompt skipped waitForBufferContent for prompts starting with "/",
// causing the command text and Enter to arrive as a single PTY read. Claude Code
// treated "/resume <uuid>" as a user message instead of a slash command.
//
// The fix uses waitForRenderedContent (rendered screen + raw buffer, 2s timeout)
// for all prompts. The rendered screen strips ANSI sequences, so it can detect
// the command text even when Claude's TUI renders it with cursor positioning.
func TestDeliverSlashCommandWaitsForRenderedContent(t *testing.T) {
	proc, err := ptyPkg.Spawn(ptyPkg.SpawnOpts{
		Flags: "",
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Skipf("cannot spawn claude: %v", err)
	}
	defer proc.Kill()

	// Wait for Claude to start so the terminal emulator is active
	time.Sleep(3 * time.Second)

	m := &Manager{
		done:  make(chan struct{}),
		slots: []*Slot{},
	}
	sl := &Slot{
		Process: proc,
	}
	sl.Term = m.newSlotTerm(sl)

	// Deliver a slash command with a short settle delay
	cmd := "/resume test-uuid-1234"
	start := time.Now()
	m.deliverSlotPrompt(sl, cmd, 10*time.Millisecond)

	// Wait for the delivery goroutine to complete
	time.Sleep(50 * time.Millisecond)
	ch := sl.Delivering
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("delivery goroutine timed out")
		}
	} else {
		time.Sleep(500 * time.Millisecond)
	}

	elapsed := time.Since(start)

	// With the fix, waitForRenderedContent checks the rendered screen.
	// If the text appears in the rendered screen (Claude rendered the input),
	// Enter is sent quickly. If not, it waits up to 2s.
	//
	// Either way, there must be a measurable delay (> 200ms) between writing
	// the command and sending Enter, ensuring they arrive as separate PTY reads.
	minExpected := 200 * time.Millisecond
	if elapsed < minExpected {
		t.Errorf("slash command delivery completed in %v, expected >= %v\n"+
			"waitForRenderedContent was likely skipped for slash commands.\n"+
			"This causes /resume and Enter to arrive as one PTY read,\n"+
			"making Claude treat it as a user message instead of a slash command.",
			elapsed.Round(time.Millisecond), minExpected)
	} else {
		t.Logf("slash command delivery took %v (includes rendered content wait)", elapsed.Round(time.Millisecond))
	}
}
