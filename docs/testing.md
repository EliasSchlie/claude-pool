# Testing Strategy

## Philosophy

**No mocking.** Tests run against real Claude Code sessions through the full socket API. This catches integration issues that unit tests with mocks would miss — the pool's value is in orchestrating real processes, so that's what we test.

## Test Pool Config

All tests use a pool configured with `--model haiku` to minimize API costs:

```go
config set flags "--dangerously-skip-permissions --model haiku"
```

## Framework

Go's built-in `testing` package with `t.Run` subtests.

## Test Layers

### API Integration Tests (`tests/integration/`)

Test the daemon's behavior directly through the socket API. Raw JSON over Unix socket — no CLI involved. This is the bulk of testing.

See [tests/integration/CLAUDE.md](../tests/integration/CLAUDE.md) for philosophy, file listing, and guidelines.

### CLI Smoke Tests (`tests/cli/`)

Test the CLI → daemon path: arg parsing, env var propagation (`CLAUDE_POOL_SESSION_ID`), output formatting, exit codes. Invoke the `claude-pool` CLI binary as a subprocess. Not a re-test of pool logic.

See [tests/cli/CLAUDE.md](../tests/cli/CLAUDE.md) for philosophy and file listing.

## Claude Code Constraints

**`cd` only works downward.** Claude Code sessions can `cd` into subdirectories of their spawn directory, but cannot `cd` to directories above it (e.g., `/tmp/`, `~/other-project/`). The Bash tool silently resets cwd to the spawn directory when asked to go higher. This means:

- Tests that verify cwd changes must use relative subdirectories, never absolute paths outside the spawn dir.
- `cwd` tracking (via process inspection) will only ever show paths at or below `spawnCwd`.

## Running

```bash
# All tests
go test ./tests/... -v -timeout 15m

# API integration tests only
go test ./tests/integration/ -v -timeout 10m

# CLI smoke tests only
go test ./tests/cli/ -v -timeout 10m

# Single flow
go test ./tests/integration/ -v -run TestSession -timeout 5m
```
