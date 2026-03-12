# Integration Tests

## Philosophy

These tests run against **real Claude Code sessions** through the full socket API.
No mocking, no stubs, no fakes. The pool's value is orchestrating real processes —
so that's what we test.

Every test pool uses `--model haiku` to keep API costs low.

**Save tokens:** When you need a session to stay busy (processing state), don't ask
the model to write a long essay — have it run a slow bash command like `sleep 60`.
This keeps the session processing without burning tokens on LLM output. Only use real
prompts when you actually need to verify the content of the response.

## Design: Flow-Based Tests

Tests are organized as **user flows**, not isolated unit assertions. Each test file
tells a story: it initializes a pool, builds up state through a sequence of commands,
and asserts at each step. This mirrors how the pool is actually used.

Why flows instead of isolated tests:
- **Setup cost is real.** Spinning up a pool with real Claude sessions takes 10-30s.
  Doing that per assertion would make the suite take hours.
- **State is the point.** Most interesting behavior depends on prior state (eviction
  needs a full pool, archive needs children, restore needs a prior run). Building
  state naturally through a flow is more reliable than trying to synthetically
  recreate it.
- **Failures are still locatable.** Each step is a named `t.Run` subtest, so when
  something fails you see exactly which checkpoint broke.

Tradeoff: a failure mid-flow can cascade. We accept this — fixing the first failure
fixes the cascade, and the alternative (isolated tests) would be 10x slower and
require duplicating all the state-building logic.

## Structure

Each test file = one pool, one flow, multiple subtests:

| File | Pool size | What it covers |
|------|-----------|----------------|
| `pool_test.go` | 2 | Init, config, resize, health, destroy, re-init with restore |
| `session_test.go` | 3 | Start, wait, capture, followup, output formats, input |
| `slots_test.go` | 2 | Queue, priority, pin/eviction, queued-session behavior |
| `offload_test.go` | 2 | Offload, capture while offloaded, restore, archive lifecycle |
| `parent_child_test.go` | 3 | Ownership, ls/tree, info, recursive archive |
| `subscribe_test.go` | 2 | Event stream, filters, re-subscribe, updated events |

Shared infrastructure:

| File | Purpose |
|------|---------|
| `helpers_test.go` | Pool setup/teardown, socket client, assertion helpers |

## Running

```bash
# All integration tests
go test ./tests/integration/ -v -timeout 10m

# Single flow
go test ./tests/integration/ -v -run TestSession -timeout 5m
```

The `-timeout` is important — real Claude sessions can take time.

## Code Style

### Protocol calls

Use `pool.send(Msg{...})` directly for all protocol commands. No wrapper methods.
Every request and response should be visible in the test — the reader should see
exactly what JSON goes over the wire without tracing through helpers.

### Comments

Only comment when the intent isn't obvious from the code. Don't narrate what
the code already says — comment *why* something is done, not *what*.

Good:
```go
// Offload s1 so we can test that input errors on sessions without a live terminal
offloadResp := pool.send(Msg{"type": "offload", "sessionId": s1})
```

```go
// s2 should still be processing — followup must error without force
resp := pool.send(Msg{"type": "followup", "sessionId": s2, "prompt": "ignore"})
```

Bad:
```go
// Send a start command
resp := pool.send(Msg{"type": "start", "prompt": "hello"})
// Check that there's no error
assertNotError(t, resp)
```

The `t.Run` name describes the step. Inline comments explain non-obvious
setup, preconditions, or why a particular assertion matters.

## Adding Tests

1. If your test fits naturally into an existing flow, add a `t.Run` subtest at the
   appropriate point in the sequence.
2. If it needs fundamentally different pool state (different size, special config),
   create a new file with its own pool.
3. Keep flows linear — don't branch. Each subtest should depend only on the state
   built by previous subtests in the same flow.
