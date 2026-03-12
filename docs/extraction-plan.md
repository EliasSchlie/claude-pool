# Implementation Plan

Written in Go. Taking lessons from Open Cockpit's pool implementation, but designing a cleaner system from requirements up. Open Cockpit code is a reference for patterns and edge cases, not a blueprint.

## What to reference from Open Cockpit

From `open-cockpit/src/` (Node.js — used as behavioral reference, not code to port):

| File | Relevance | What to learn |
|------|-----------|---------------|
| `pool-manager.js` | High | Offloading flow, slot claiming, LRU eviction logic, session spawning order |
| `session-discovery.js` | High | Idle detection via signal files, typing detection via buffer polling, consecutive-miss threshold |
| `api-handlers.js` | Medium | Handler patterns, error responses, timing dance for prompt delivery |
| `pool.js` | Medium | pool.json structure, slot selection heuristics |
| `api-server.js` | Medium | Unix socket connection handling, newline-delimited JSON parsing |

**Not referenced** (different scope):
- `pty-daemon.js`, `daemon-client.js` — We use in-process PTY, not a separate daemon
- `main.js`, `renderer.js`, UI modules — Electron-specific
- Terminal tab management — claude-term's scope
- Intention files — Open Cockpit's scope

## Go Dependencies

| Package | Purpose |
|---------|---------|
| `creack/pty` | PTY spawn, read/write, window resize |
| stdlib `net` | Unix domain sockets (API server + attach sockets) |
| stdlib `encoding/json` | JSON protocol (marshal/unmarshal) |
| stdlib `os/exec` | Spawning Claude Code processes |
| stdlib `sync` | Mutex for pool state |
| stdlib `bufio` | Newline-delimited JSON scanning |

No frameworks. The stdlib covers almost everything.

## Source Layout

```
cmd/
  claude-pool/
    main.go              # Daemon entry point
internal/
  daemon/
    daemon.go            # Lifecycle: start, signal handling, graceful shutdown
  pool/
    pool.go              # Pool state (sessions, queue, mappings)
    manager.go           # Business logic (start, followup, pin, offload, eviction)
    config.go            # config.json read/write
    queue.go             # FIFO request queue
  pty/
    manager.go           # PTY spawn, kill, buffer capture, re-adopt on restart
    timing.go            # Timing dance (Escape → Ctrl-U → type → poll → Enter)
    ansi.go              # ANSI escape stripping
  api/
    server.go            # Unix socket listener, connection handling
    handlers.go          # Command dispatch → pool manager
    protocol.go          # Request/response types, JSON marshaling
  attach/
    server.go            # Per-session raw PTY pipe sockets
  discovery/
    discovery.go         # Session state detection (signal files, JSONL)
    typing.go            # Typing detection (buffer polling)
  paths/
    paths.go             # Pool name → all file paths
hooks/
  idle.sh               # Idle signal hook
  stop.sh               # Stop hook
```

## Separate package: CLI

The CLI ships as `claude-pool-cli` (separate repo). It's a thin Go binary:
- Reads `~/.claude-pool/pools.json` registry
- Resolves `--pool` flag to socket connection
- Sends JSON, pretty-prints responses
- Supports local Unix sockets and remote connections (SSH tunneling)

## Build & Distribution

Single static binary per platform. No runtime dependencies.

```bash
# Build
go build -o claude-pool ./cmd/claude-pool

# Cross-compile
GOOS=linux GOARCH=amd64 go build -o claude-pool-linux ./cmd/claude-pool
```

## Implementation Phases

### Phase 1: Core daemon
1. Pool state management (pool.go, manager.go, config.go)
2. PTY management (spawn, kill, buffer capture)
3. API server (socket, handlers, protocol)
4. Session discovery (idle detection via signal files)
5. Basic commands: init, start, wait, result, followup, ls, info, health
6. Timing dance for prompt delivery
7. Hooks (idle signal, stop)

### Phase 2: Advanced features
1. Offload/restore flow
2. Claim command (fresh session access)
3. LRU eviction with priority
4. Queue management
5. Pin/unpin
6. Attach server (raw PTY pipe sockets)
7. Reconciliation loop

### Phase 3: CLI (separate package)
1. Pool registry (`~/.claude-pool/pools.json`)
2. Local socket connections
3. Remote pool support (SSH tunneling)
4. Pretty-printed output, `--json` flag for programmatic use

### Phase 4: Wire Open Cockpit
1. Open Cockpit reads pool registry, connects to pool sockets as a client
2. Remove pool logic from Open Cockpit
3. Pool UI goes through socket API
4. Attach feeds xterm.js via raw pipe sockets

### Phase 5: Extract claude-term (separate project)
1. Per-session persistent terminal tabs
2. Independent from claude-pool
3. Open Cockpit depends on both

## Key Implementation Notes

### Timing dance (from OC reference)
The sequence for safely delivering a prompt to an idle Claude session:
1. Send Escape (clear any partial input)
2. Send Ctrl-U (clear line)
3. Type the prompt text
4. Poll terminal buffer until prompt text appears
5. Send Enter

This must be reimplemented in Go using `creack/pty` read/write on the `*os.File`.

### PTY re-adoption on restart
On daemon restart, read PIDs from pool.json → check if processes are still alive → re-open `/proc/<pid>/fd/0` (Linux) or use `os.FindProcess` → reconnect PTY I/O.

### Session state detection
Claude Code writes signal files when idle. The daemon watches these files + polls the JSONL transcript for processing state. Typing detection polls the terminal buffer for un-submitted input (with consecutive-miss threshold to avoid false positives).

## Open Questions

- [ ] Should the daemon auto-start when the CLI connects? (Like Docker)
- [ ] How to handle migration for existing Open Cockpit users?
- [ ] Should hooks ship as a claude-pool plugin or integrate with Open Cockpit's existing plugin?
- [ ] Pool registry format — should remote connections use SSH tunnel strings, TCP addresses, or something else?
