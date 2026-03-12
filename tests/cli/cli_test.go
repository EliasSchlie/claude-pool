package cli

// TestCLI — Basic CLI commands flow
//
// Pool size: 2
//
// Verifies the CLI binary correctly translates user commands into socket API calls
// and formats output. Not a re-test of pool logic — just the CLI layer.
//
// Flow:
//
//   1. "ping"
//      Run: claude-pool ping
//      Assert: exit code 0, output contains "pong" or similar success indicator.
//
//   2. "health"
//      Run: claude-pool health
//      Assert: exit code 0, output shows slot count, session states.
//      Run: claude-pool health --json
//      Assert: valid JSON with type "health".
//
//   3. "start and wait"
//      Run: claude-pool start --prompt "respond with exactly: cli-test"
//      Assert: exit code 0, output contains sessionId.
//      Parse sessionId from output.
//      Run: claude-pool wait --session <id> --timeout 120000
//      Assert: exit code 0, output contains "cli-test".
//
//   4. "info"
//      Run: claude-pool info --session <id>
//      Assert: exit code 0, output shows session details (status, cwd, etc.).
//      Run: claude-pool info --session <id> --json
//      Assert: valid JSON with type "session", status "idle".
//
//   5. "ls"
//      Run: claude-pool ls
//      Assert: exit code 0, output lists sessions.
//      Run: claude-pool ls --all --json
//      Assert: valid JSON with type "sessions", array includes our session.
//
//   6. "capture"
//      Run: claude-pool capture --session <id>
//      Assert: exit code 0, output is non-empty.
//      Run: claude-pool capture --session <id> --format jsonl-full
//      Assert: exit code 0, output is longer than default.
//
//   7. "followup"
//      Run: claude-pool followup --session <id> --prompt "respond with exactly: followup-cli"
//      Assert: exit code 0.
//      Run: claude-pool wait --session <id> --timeout 120000
//      Assert: output contains "followup-cli".
//
//   8. "set-priority"
//      Run: claude-pool set-priority --session <id> --priority 5
//      Assert: exit code 0.
//      Run: claude-pool info --session <id> --json
//      Assert: priority is 5.
//
//   9. "pin and unpin"
//      Run: claude-pool pin --session <id> --duration 60
//      Assert: exit code 0.
//      Run: claude-pool info --session <id> --json
//      Assert: pinned is true.
//      Run: claude-pool unpin --session <id>
//      Assert: exit code 0, pinned is false.
//
//  10. "offload and archive"
//      Run: claude-pool offload --session <id>
//      Assert: exit code 0.
//      Run: claude-pool info --session <id> --json
//      Assert: status "offloaded".
//      Run: claude-pool archive --session <id>
//      Assert: exit code 0.
//      Run: claude-pool info --session <id> --json
//      Assert: status "archived".
//
//  11. "error exit codes"
//      Run: claude-pool info --session nonexistent
//      Assert: exit code non-zero, stderr contains error message.
//      Run: claude-pool followup --session <archived-id> --prompt "test"
//      Assert: exit code non-zero (archived, can't followup).
//
//  12. "destroy without confirm"
//      Run: claude-pool destroy
//      Assert: exit code non-zero (missing --confirm).
//
//  13. "destroy with confirm"
//      Run: claude-pool destroy --confirm
//      Assert: exit code 0.

import "testing"

func TestCLI(t *testing.T) {
	t.Skip("not yet implemented")
}
