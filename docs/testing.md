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

## Test Categories

### Daemon lifecycle
- Start foreground/background, status, stop
- Re-start re-adopts live PIDs

### Pool lifecycle
- Init with size, resize up/down, destroy (config persists), health, config get/set

### Session basics
- `start --block`, `followup`, `wait`, `result`, `capture`
- All output formats: jsonl-last, jsonl-short, jsonl-long, jsonl-full, buffer-last, buffer-full

### Queue behavior
- Start when pool full → queues, slot frees → dequeues FIFO
- Stop cancels queued request

### Offload/restore
- Offload idle session, followup restores it
- Pin restores offloaded session
- JSONL formats work while offloaded, buffer formats error

### Archive/unarchive
- Archive stops active sessions first
- Errors on unarchived children (without recursive flag)
- Recursive archives descendants depth-first
- `ls --archived` shows them, default `ls` hides them
- Unarchive → offloaded state

### Pin/unpin
- Pin with sessionId prevents eviction
- Pin without sessionId allocates fresh session
- Unpin allows eviction

### Priority & eviction
- `set-priority` changes eviction order
- LRU evicts lower priority first, oldest within same priority

### Parent-child
- Start with parentId sets ownership
- `ls` returns owned children only
- `ls --tree` nests descendants
- `info` shows recursive children

### Subscribe
- Event stream delivers status changes
- Filters: sessions, events, statuses (ANDed)
- Re-subscribe on same connection replaces filters
- Multiple concurrent subscribers

### Attach
- Raw PTY I/O works for live sessions
- Errors for offloaded/queued
- Input/key/type low-level commands

### Screen/watch
- Terminal buffer output, ANSI stripping

### Edge cases
- Session prefix resolution (unique prefix matches)
- Ambiguous prefix → error
- Concurrent clients on same pool
- Followup on queued → error; followup with force → replaces prompt
