# CLI Smoke Tests

## Philosophy

These tests verify the **CLI → daemon** path: arg parsing, env var propagation, and output formatting. They invoke the `claude-pool` CLI binary as a subprocess and assert on stdout/stderr/exit codes.

**Not a re-test of pool logic.** The API integration tests (`tests/integration/`) thoroughly test daemon behavior. These tests only verify that the CLI correctly translates user commands into API calls and that environment variables flow through to spawned sessions.

Every test pool uses `--model haiku` to keep API costs low.

**Save tokens:** Same as integration tests — use `sleep` commands for busy sessions, real prompts only when verifying response content.

## What These Tests Cover

- CLI arg parsing → correct socket API call
- `CLAUDE_POOL_SESSION_ID` env var auto-detection (parent-child without explicit `--parent`)
- Pool name resolution from registry (`--pool` flag, default pool)
- Output formatting (human-readable vs JSON `--json` mode)
- Exit codes (0 on success, non-zero on error)
- One real end-to-end test: Claude session spawns a child via CLI command

## Design

Same flow-based approach as integration tests. Each test file spins up a real daemon, runs CLI commands against it, and asserts on results.

The key difference: integration tests send raw JSON over the socket. CLI tests run the `claude-pool` binary and interact via stdin/stdout/exit codes — the way a real user (or Claude session) would.

## Structure

| File | What it covers |
|------|----------------|
| `cli_test.go` | Basic CLI commands: init, start, wait, ls, info, destroy |
| `env_test.go` | Env var propagation, parent-child auto-detection |
| `helpers_test.go` | Shared setup: build binaries, start daemon, run CLI commands |

## Running

```bash
# All CLI tests
go test ./tests/cli/ -v -timeout 10m

# Single flow
go test ./tests/cli/ -v -run TestCLI -timeout 5m
```

## Code Style

Use `pool.run("start", "--prompt", "hello")` to invoke the CLI. Every command and its output should be visible in the test.

Comments: same rules as integration tests — only comment *why*, not *what*.
