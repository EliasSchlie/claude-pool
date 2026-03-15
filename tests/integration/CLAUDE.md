# Integration Tests

## Philosophy

These tests run against **real Claude Code sessions** through the CLI.
No mocking — these validate end-to-end behavior with real processes. Pure logic tests
(parsing, filtering, state transitions) belong in unit tests co-located with the
source (`internal/*/`).

Every test pool uses `--model haiku` to keep API costs low.

**Save tokens:** When you need a session to stay busy (processing state), don't ask
the model to write a long essay — have it run a slow bash command like `sleep 60`.
This keeps the session processing without burning tokens on LLM output. Only use real
prompts when you actually need to verify the content of the response.

## Test Setup

All tests use the **same pool type** (`pool`). Setup via `setupPool(t, size)` runs
`claude-pool-cli init` — the same command a real user would run. No manual daemon
start, no hand-written config, no synthetic registry.

**Isolation via `CLAUDE_POOL_HOME`:** Each test gets its own directory that mirrors
production's `~/.claude-pool/` structure:

```
~/.cache/claude-pool-tests/<run-id>/<TestName>/
  .claude-pool/          ← CLAUDE_POOL_HOME (registry, pool data, socket)
  workdir/               ← session spawn directory (--dir flag)
```

Two env vars drive isolation:
- `CLAUDE_POOL_HOME` — redirects all pool state (registry, pool dirs)
- `CLAUDE_POOL_DAEMON` — tells CLI where to find the daemon binary

### CLI tests (most tests)

Operations go through the CLI binary as subprocess calls — the same way real users
and Claude sessions interact with pools.

Files: `pool_test.go`, `session_test.go`, `slots_test.go`, `offload_test.go`, `parent_child_test.go`

### Socket tests (API-only features)

Attach and subscribe are API-only features not exposed in the CLI. These tests
set up the pool via CLI init (same as above), then call `pool.dial()` to open a
raw socket connection for API-only commands.

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

| File | Pool size | What it covers |
|------|-----------|----------------|
| `pool_test.go` | 2 | Init, ping, config, resize, health, destroy, re-init with restore |
| `session_test.go` | 3 | Start, wait, capture, followup, info, set (metadata), stop, --block, prefix resolution, --dir verification, wait-any, debug slots/logs |
| `slots_test.go` | 2 | Queue, set priority, set pinned, eviction order, LRU, pendingInput LRU reset, debug input/capture |
| `offload_test.go` | 2 | Eviction→offload, capture offloaded, followup restores, process death, archive/unarchive lifecycle |
| `parent_child_test.go` | 3 | --parent flag, env auto-detection, --parent none, ls filtering, verbosity, recursive archive |
| `attach_test.go` | 2 | Attach pipe, pendingInput, keystroke/submit, eviction closes pipe, re-attach, multiple simultaneous clients |
| `subscribe_test.go` | 2 | Event stream, filters, re-subscribe, updated events |
| `multi_pool_test.go` | 1+1 | Two pools in shared registry, session isolation, concurrent ops, destroy isolation (invariant #1) |

Shared infrastructure:

| File | Purpose |
|------|---------|
| `helpers_test.go` | Pool struct (CLI + socket), setup helpers, assertion helpers, waiting helpers |
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

Use `sc.send(Msg{...})` directly for API-only commands. CLI commands still go through
`pool.run(...)` / `pool.runJSON(...)`. Every request should be visible — no hidden
abstractions.

### Comments

Only comment when the intent isn't obvious. The `t.Run` name describes the step.
Inline comments explain non-obvious setup, preconditions, or why an assertion matters.

## Adding Tests

1. If your test fits naturally into an existing flow, add a `t.Run` subtest at the
   appropriate point in the sequence.
2. If it needs fundamentally different pool state, create a new file with its own pool.
3. Keep flows linear — each subtest depends only on prior subtests in the same flow.
