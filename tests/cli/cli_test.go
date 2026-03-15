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
//      Run: claude-pool capture --session <id> --source jsonl --turns 0 --detail raw
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

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {
	pool := setupCLIPool(t, 2)

	var sessionID string

	t.Run("ping", func(t *testing.T) {
		result := pool.run("ping")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if !strings.Contains(strings.ToLower(result.Stdout), "pong") {
			t.Fatalf("expected pong in output, got: %s", result.Stdout)
		}
	})

	t.Run("health", func(t *testing.T) {
		result := pool.run("health")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if result.Stdout == "" {
			t.Fatal("expected non-empty health output")
		}

		resp := pool.runJSON("health")
		if resp["type"] != "health" {
			t.Fatalf("expected type health, got %v", resp["type"])
		}
		if resp["health"] == nil {
			t.Fatal("expected health field in response")
		}
	})

	t.Run("start and wait", func(t *testing.T) {
		// Use --json to reliably parse sessionId
		startResp := pool.runJSON("start", "--prompt", "respond with exactly: cli-test")
		sessionID = strVal(startResp, "sessionId")
		if sessionID == "" {
			t.Fatalf("no sessionId in start response: %v", startResp)
		}

		result := pool.run("wait", "--session", sessionID, "--timeout", "120000")
		if result.ExitCode != 0 {
			t.Fatalf("wait exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if !strings.Contains(strings.ToLower(result.Stdout), "cli-test") {
			t.Fatalf("expected output to contain 'cli-test', got: %s", result.Stdout)
		}
	})

	t.Run("info", func(t *testing.T) {
		result := pool.run("info", "--session", sessionID)
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if result.Stdout == "" {
			t.Fatal("expected non-empty info output")
		}

		resp := pool.runJSON("info", "--session", sessionID)
		if resp["type"] != "session" {
			t.Fatalf("expected type session, got %v", resp["type"])
		}
		session, ok := resp["session"].(map[string]any)
		if !ok {
			t.Fatalf("expected session object, got %T", resp["session"])
		}
		if strVal(session, "status") != "idle" {
			t.Fatalf("expected idle, got %s", strVal(session, "status"))
		}
	})

	t.Run("ls", func(t *testing.T) {
		result := pool.run("ls", "--all")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if result.Stdout == "" {
			t.Fatal("expected non-empty ls output")
		}

		resp := pool.runJSON("ls", "--all")
		if resp["type"] != "sessions" {
			t.Fatalf("expected type sessions, got %v", resp["type"])
		}
		sessions, ok := resp["sessions"].([]any)
		if !ok {
			t.Fatalf("expected sessions array, got %T", resp["sessions"])
		}

		// Should include our session
		found := false
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			if strVal(sm, "sessionId") == sessionID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("session %s not found in ls output", sessionID)
		}
	})

	t.Run("capture", func(t *testing.T) {
		result := pool.run("capture", "--session", sessionID)
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		defaultOutput := result.Stdout
		if defaultOutput == "" {
			t.Fatal("expected non-empty capture output")
		}

		result = pool.run("capture", "--session", sessionID, "--source", "jsonl", "--turns", "0", "--detail", "raw")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if len(result.Stdout) <= len(defaultOutput) {
			t.Fatalf("expected raw/all-turns output to be longer than default\ndefault (%d bytes): %s\nraw (%d bytes): %s",
				len(defaultOutput), truncate(defaultOutput, 200),
				len(result.Stdout), truncate(result.Stdout, 200))
		}
	})

	t.Run("followup", func(t *testing.T) {
		result := pool.run("followup", "--session", sessionID, "--prompt", "respond with exactly: followup-cli")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		result = pool.run("wait", "--session", sessionID, "--timeout", "120000")
		if result.ExitCode != 0 {
			t.Fatalf("wait exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
		if !strings.Contains(strings.ToLower(result.Stdout), "followup-cli") {
			t.Fatalf("expected 'followup-cli' in output, got: %s", result.Stdout)
		}
	})

	t.Run("set-priority", func(t *testing.T) {
		result := pool.run("set-priority", "--session", sessionID, "--priority", "5")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		resp := pool.runJSON("info", "--session", sessionID)
		session, _ := resp["session"].(map[string]any)
		priority := session["priority"].(float64)
		if priority != 5 {
			t.Fatalf("expected priority 5, got %v", priority)
		}
	})

	t.Run("pin and unpin", func(t *testing.T) {
		result := pool.run("pin", "--session", sessionID, "--duration", "60")
		if result.ExitCode != 0 {
			t.Fatalf("pin exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		resp := pool.runJSON("info", "--session", sessionID)
		session, _ := resp["session"].(map[string]any)
		if pinned, _ := session["pinned"].(bool); !pinned {
			t.Fatal("expected pinned to be true after pin")
		}

		result = pool.run("unpin", "--session", sessionID)
		if result.ExitCode != 0 {
			t.Fatalf("unpin exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		resp = pool.runJSON("info", "--session", sessionID)
		session, _ = resp["session"].(map[string]any)
		if pinned, _ := session["pinned"].(bool); pinned {
			t.Fatal("expected pinned to be false after unpin")
		}
	})

	t.Run("offload and archive", func(t *testing.T) {
		result := pool.run("offload", "--session", sessionID)
		if result.ExitCode != 0 {
			t.Fatalf("offload exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		resp := pool.runJSON("info", "--session", sessionID)
		session, _ := resp["session"].(map[string]any)
		if strVal(session, "status") != "offloaded" {
			t.Fatalf("expected offloaded, got %s", strVal(session, "status"))
		}

		result = pool.run("archive", "--session", sessionID)
		if result.ExitCode != 0 {
			t.Fatalf("archive exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}

		resp = pool.runJSON("info", "--session", sessionID)
		session, _ = resp["session"].(map[string]any)
		if strVal(session, "status") != "archived" {
			t.Fatalf("expected archived, got %s", strVal(session, "status"))
		}
	})

	t.Run("error exit codes", func(t *testing.T) {
		result := pool.run("info", "--session", "nonexistent")
		if result.ExitCode == 0 {
			t.Fatal("expected non-zero exit code for nonexistent session")
		}
		if result.Stderr == "" {
			t.Fatal("expected error message on stderr")
		}

		result = pool.run("followup", "--session", sessionID, "--prompt", "test")
		if result.ExitCode == 0 {
			t.Fatal("expected non-zero exit code for followup on archived session")
		}
	})

	t.Run("destroy without confirm", func(t *testing.T) {
		result := pool.run("destroy")
		if result.ExitCode == 0 {
			t.Fatal("expected non-zero exit code without --confirm")
		}
	})

	t.Run("destroy with confirm", func(t *testing.T) {
		result := pool.run("destroy", "--confirm")
		if result.ExitCode != 0 {
			t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
		}
	})
}

// TestCLIOutputFormat — Verify human-readable vs JSON output modes are distinct
//
// Pool size: 1
//
// This tests something the integration tests can't: that the CLI formats output
// differently based on --json flag. Integration tests always use raw socket JSON.
func TestCLIOutputFormat(t *testing.T) {
	pool := setupCLIPool(t, 1)

	t.Run("ping human vs json", func(t *testing.T) {
		human := pool.run("ping")
		if human.ExitCode != 0 {
			t.Fatalf("exit code %d", human.ExitCode)
		}

		jsonResult := pool.run("ping", "--json")
		if jsonResult.ExitCode != 0 {
			t.Fatalf("exit code %d", jsonResult.ExitCode)
		}

		// Human output should be short and readable
		if !strings.Contains(human.Stdout, "pong") {
			t.Fatalf("expected 'pong' in human output, got: %s", human.Stdout)
		}

		// JSON output should be valid JSON
		var resp map[string]any
		if err := json.Unmarshal([]byte(jsonResult.Stdout), &resp); err != nil {
			t.Fatalf("JSON output not valid: %v\nstdout: %s", err, jsonResult.Stdout)
		}
		if resp["type"] != "pong" {
			t.Fatalf("expected type pong in JSON, got %v", resp["type"])
		}
	})

	t.Run("health human vs json", func(t *testing.T) {
		human := pool.run("health")
		jsonResult := pool.run("health", "--json")

		if human.ExitCode != 0 || jsonResult.ExitCode != 0 {
			t.Fatalf("unexpected exit codes: human=%d json=%d", human.ExitCode, jsonResult.ExitCode)
		}

		// JSON output should parse as valid JSON with type field
		var resp map[string]any
		if err := json.Unmarshal([]byte(jsonResult.Stdout), &resp); err != nil {
			t.Fatalf("JSON output not valid: %v", err)
		}
		if resp["type"] != "health" {
			t.Fatalf("expected type health, got %v", resp["type"])
		}

		// Human output should NOT be raw JSON with "type" field
		// (it should print just the health object, not the wrapper)
		if strings.Contains(human.Stdout, `"type"`) {
			t.Fatalf("human output looks like raw JSON: %s", human.Stdout)
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
