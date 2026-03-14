# Testing Strategy

## Philosophy

Test at the right level. Unit tests for pure logic (fast, pinpoint failures). Integration tests with real Claude sessions for end-to-end behavior (slow, high confidence). Integration tests must never mock the daemon or Claude — the pool's value is orchestrating real processes.

## Framework

Go's built-in `testing` package with `t.Run` subtests.

## Test Layers

### Unit Tests (`internal/*/`)

Co-located with source (Go convention, `package pool`). For pure logic that doesn't need a running daemon: JSONL parsing/filtering, eviction ordering, state transitions, data transformations. Fast (~ms), run on every change.

```bash
go test ./internal/pool/ -v
```

### API Integration Tests (`tests/integration/`)

Test the daemon's behavior through the socket API with real Claude sessions. Raw JSON over Unix socket — no CLI involved. Flow-based: each file initializes a pool and walks through a sequence of operations.

See [tests/integration/CLAUDE.md](../tests/integration/CLAUDE.md) for philosophy, file listing, and guidelines.

### CLI Smoke Tests (`tests/cli/`)

Test the CLI → daemon path: arg parsing, env var propagation (`CLAUDE_POOL_SESSION_ID`), output formatting, exit codes. Invoke the `claude-pool` CLI binary as a subprocess. Not a re-test of pool logic.

See [tests/cli/CLAUDE.md](../tests/cli/CLAUDE.md) for philosophy and file listing.

## Test Pool Config

Integration tests use `--model haiku` to minimize API costs:

```go
config set flags "--dangerously-skip-permissions --model haiku"
```

## Claude Code Constraints

**`cd` only works downward.** Claude Code sessions can `cd` into subdirectories of their spawn directory, but cannot `cd` to directories above it (e.g., `/tmp/`, `~/other-project/`). The Bash tool silently resets cwd to the spawn directory when asked to go higher. This means:

- Tests that verify cwd changes must use relative subdirectories, never absolute paths outside the spawn dir.
- `cwd` tracking (via process inspection) will only ever show paths at or below `spawnCwd`.

## Running

```bash
# All tests
go test ./... -v -timeout 15m

# Unit tests only
go test ./internal/... -v

# Integration tests only
go test ./tests/integration/ -v -timeout 10m

# CLI smoke tests only
go test ./tests/cli/ -v -timeout 10m

# Single flow
go test ./tests/integration/ -v -run TestSession -timeout 5m
```
