# Implementation Tasks

Dependency-ordered task list for building the daemon. Each task builds on the previous ones.
The goal: pass all integration tests (`go test ./tests/integration/ -v -timeout 10m`).

## Layer 1: Foundation (no tests pass yet)

### 1. `internal/paths/paths.go` — Path resolution
Pool dir → all file paths (config.json, pool.json, api.sock, daemon.log, offloaded/, archived/, idle-signals/, session-pids/).
`--pool-dir` flag support in main.go.

### 2. `internal/pool/config.go` — Config read/write
Read/write `config.json` (flags, size). `config` and `config set` commands.

### 3. `internal/api/server.go` + `internal/api/protocol.go` — Socket server
Unix socket listener, newline-delimited JSON, connection handling, request routing.
`ping` → `pong` works.

### 4. `cmd/claude-pool/main.go` — Daemon entry point
Parse `--pool-dir`, start API server, signal handling, graceful shutdown.
**Milestone: `TestPool` steps 1-3 pass** (ping, config, config set).

## Layer 2: Pool Init & Session Spawn

### 5. `internal/pool/pool.go` — Pool state
In-memory pool state: sessions map, queue, slot tracking. Load/save `pool.json`.
Mutex for all state mutations.

### 6. `internal/pty/manager.go` — PTY spawn
Spawn Claude Code process via `creack/pty`. Buffer capture (ring buffer for terminal output).
Process lifecycle (start, kill, re-adopt on restart).

### 7. `internal/pool/manager.go` — Pool manager (init, basic commands)
`init`: create slots, spawn sessions. `health`: slot counts, PID liveness.
`info`: session details. `ls`: list sessions with ownership filtering.
**Milestone: `TestPool` steps 4-6 pass** (init, init error, health).

## Layer 3: Hooks & Session Discovery

### 8. `hooks/` — Hook scripts + `.claude/hooks.json`
Idle signal hook (writes to `idle-signals/<sessionId>`).
Stop hook. Template `hooks.json` for project-local hooks.
`init` writes hooks into pool directory.

### 9. `internal/discovery/discovery.go` — Session state detection
Idle detection via signal files. Status tracking (processing/idle). Process death → session offloaded, slot recycled.
JSONL transcript reading for Claude UUID discovery.
**Milestone: sessions actually reach `idle` status after processing**.

## Layer 4: Prompt Delivery & Wait

### 10. `internal/pty/timing.go` — Timing dance
Escape → Ctrl-U → type prompt → poll buffer → Enter.
Safe prompt delivery to idle sessions.

### 11. `internal/pool/manager.go` — start, followup, wait, stop, capture
`start`: spawn or claim slot, deliver prompt via timing dance.
`followup`: deliver prompt to existing session (with force option).
`wait`: block until session idle, return content.
`stop`: Ctrl-C, synchronous (idle on return). Cancel queued.
`capture`: read JSONL transcript or terminal buffer.
**Milestone: `TestSession` steps 1-11 pass** (start, wait, info, capture, followup).

## Layer 5: Offload, Restore, Archive

### 12. `internal/pool/manager.go` — offload, restore, archive, unarchive
`offload`: save meta, kill PTY, free slot.
`restore`: re-spawn from JSONL transcript (Claude `--resume`).
`archive`/`unarchive`: move between offloaded↔archived. Recursive archive.
**Milestone: `TestOffload` passes**.

## Layer 6: Slots, Queue, Eviction

### 13. `internal/pool/queue.go` — Request queue
FIFO queue for when all slots are busy. Dequeue on slot free.

### 14. `internal/pool/manager.go` — eviction, set (priority, pinned, metadata)
LRU eviction (priority-aware). `set` command for priority, pinned, metadata.
Pinned sessions: prevent eviction for a duration.
`resize`: add/remove slots, graceful shrink.
**Milestone: `TestSlots` passes**.

## Layer 7: Subscribe & Events

### 15. `internal/api/subscribe.go` — Event stream
Persistent connections. Event types: status, created, updated, pool, archived, unarchived.
Filters: sessions, events, statuses, fields.
**Milestone: `TestSubscribe` passes**.

## Layer 8: Parent-Child & Remaining

### 16. `internal/pool/manager.go` — parent-child ownership
`parentId` on start/pin. `ls` ownership filtering (default: caller's children).
`ls` with `verbosity: nested`: nested descendants. `info` recursive children.
`archive` with children checks. Recursive archive.
**Milestone: `TestParentChild` passes**.

### 17. `internal/pool/manager.go` — destroy, re-init, resize
`destroy`: kill all sessions, daemon exits. Config/pool.json persist.
Re-init: restore sessions from pool.json. `noRestore` flag.
**Milestone: `TestPool` all steps pass**.

## Layer 9: Polish

### 18. Reconciliation loop
Recycle error slots, detect process death (session → offloaded, slot → recycled), periodic health checks (every 30s).

### 19. `internal/attach/server.go` — Attach server
Per-session raw PTY pipe sockets. Multiple concurrent clients.

### 20. `internal/pty/ansi.go` — ANSI stripping
Clean terminal output for buffer capture.

### 21. Input command
Raw byte injection into session PTY. Error for offloaded sessions.
**Milestone: all integration tests pass**.
