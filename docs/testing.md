# Testing Strategy

## Philosophy

**No mocking.** Tests run against real Claude Code sessions through the full socket API. This catches integration issues that unit tests with mocks would miss — the pool's value is in orchestrating real processes, so that's what we test.

## Test Pool Config

All tests use a pool configured with `--model haiku` to minimize API costs:

```go
config set flags "--dangerously-skip-permissions --model haiku"
```

## Framework

Go's built-in `testing` package with `t.Run` subtests. Tests live in `tests/integration/`.

Each test:
1. Creates a temp pool directory
2. Starts a daemon
3. Runs assertions through the socket API
4. Tears down (destroy + cleanup)

## Test Structure

Tests are organized as flow-based integration tests. See [tests/integration/CLAUDE.md](../tests/integration/CLAUDE.md) for the full philosophy, file listing, and guidelines for adding new tests.

## Running

```bash
# All integration tests
go test ./tests/integration/ -v -timeout 10m

# Single flow
go test ./tests/integration/ -v -run TestSession -timeout 5m
```
