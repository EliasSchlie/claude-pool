# Integration Tests

## Philosophy

These tests run against **real Claude Code sessions** through the CLI and socket API.
No mocking — these validate end-to-end behavior with real processes. Pure logic tests
(parsing, filtering, state transitions) belong in unit tests co-located with the
source (`internal/*/`).

Every test pool uses `--model haiku` to keep API costs low.

**Save tokens:** When you need a session to stay busy (processing state), don't ask
the model to write a long essay — have it run a slow bash command like `sleep 60`.
This keeps the session processing without burning tokens on LLM output. Only use real
prompts when you actually need to verify the content of the response.

## Two Test Modes

### CLI-based tests (most tests)

Operations go through the CLI binary as subprocess calls — the same way real users
and Claude sessions interact with pools. The test helpers (`cliPool`) build the CLI
binary, start the daemon, and run `claude-pool-cli` commands.

Files: `pool_test.go`, `session_test.go`, `slots_test.go`, `offload_test.go`, `parent_child_test.go`

### Socket-based tests (API-only features)

Attach and subscribe are API-only features not exposed in the CLI. These tests use
the socket API directly via `testPool`.

Files: `attach_test.go`, `subscribe_test.go`

## Design: Flow-Based Tests

Tests are organized as **user flows**, not isolated unit assertions. Each test file
tells a story: it initializes a pool, builds up state through a sequence of commands,
and asserts at each step.

Why flows:
- **Setup cost is real.** Spinning up a pool takes 10-30s. Per-assertion setup would take hours.
- **State is the point.** Most interesting behavior depends on prior state.
- **Failures are locatable.** Each step is a named `t.Run` subtest.

## Claude Code `cd` Constraint

Sessions can only `cd` into **subdirectories** of their spawn directory. All cwd-related
test prompts must use relative subdirectories.

## Structure

| File | Pool size | Mode | What it covers |
|------|-----------|------|----------------|
| `pool_test.go` | 2 | CLI | Init, ping, config, resize, health, destroy, re-init with restore |
| `session_test.go` | 3 | CLI | Start, wait, capture, followup, info, set (metadata), stop, --block, prefix resolution |
| `slots_test.go` | 2 | CLI | Queue, set priority, set pinned, eviction order, LRU |
| `offload_test.go` | 2 | CLI | Eviction→offload, capture offloaded, followup restores, process death, archive/unarchive lifecycle |
| `parent_child_test.go` | 3 | CLI | --parent flag, env auto-detection, --parent none, ls filtering, verbosity, recursive archive |
| `attach_test.go` | 2 | Socket | Attach pipe, pendingInput, keystroke/submit, eviction closes pipe, re-attach |
| `subscribe_test.go` | 2 | Socket | Event stream, filters, re-subscribe, updated events |

Shared infrastructure:

| File | Purpose |
|------|---------|
| `helpers_test.go` | Both pool types (CLI + socket), assertion helpers, waiting helpers |
| `../testutil/testutil.go` | TestMain utilities: find repo root, build binary, setup run dir |

## Running

```bash
# All integration tests
go test ./tests/integration/ -v -timeout 10m

# Single flow
go test ./tests/integration/ -v -run TestSession -timeout 5m
```

## Code Style

### CLI tests

Use `pool.run("command", "--flag", "value")` or `pool.runJSON(...)` for all CLI operations.
Every command and its flags should be visible in the test.

### Socket tests

Use `pool.send(Msg{...})` directly for all protocol commands. Every request and response
should be visible — no hidden abstractions.

### Comments

Only comment when the intent isn't obvious. The `t.Run` name describes the step.
Inline comments explain non-obvious setup, preconditions, or why an assertion matters.

## Adding Tests

1. If your test fits naturally into an existing flow, add a `t.Run` subtest at the
   appropriate point in the sequence.
2. If it needs fundamentally different pool state, create a new file with its own pool.
3. Keep flows linear — each subtest depends only on prior subtests in the same flow.
