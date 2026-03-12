# Architecture

## Overview

Claude Pool is a daemon that manages a pool of Claude Code terminal sessions. It handles session lifecycle, slot allocation, offloading/restoring, and exposes everything through a Unix socket API.

Each pool runs as a single daemon process that owns its PTYs directly.

```
claude-pool daemon
  ├── API server (Unix socket, newline-delimited JSON)
  ├── Pool manager (slot allocation, LRU eviction, offload/restore)
  ├── PTY manager (in-process, owns terminal instances via node-pty)
  ├── Session discovery (idle detection, status tracking)
  └── Reconciliation loop (auto-restart dead slots, periodic health checks)
```

## Components

### Pool Manager
Core business logic. Manages pool.json, slot allocation, offloading, archiving, session restoration. All state mutations go through `withPoolLock()` to prevent races.

### PTY Manager
Owns all terminal instances in-process via `node-pty`. On daemon restart, re-adopts orphaned PTY processes by checking PIDs from pool.json (if still alive, reconnects; if dead, respawns).

### API Server
Listens on `~/.claude-pool/pools/<name>/api.sock`. Accepts newline-delimited JSON. Routes requests to pool manager. Any number of clients can connect simultaneously.

### Session Discovery
Detects session state (idle, processing, dead) by reading Claude Code's signal files and JSONL transcripts. Caches results for performance.

### Reconciliation Loop
Runs every 30s. Restarts dead/error slots, kills orphaned processes, maintains pool health.

## Directory Structure

See [design-principles.md](design-principles.md) for the canonical directory layout. Each pool is fully self-contained under `~/.claude-pool/pools/<name>/`.

## Key Design Decisions

### Socket as the only client interface
All clients (CLI, Open Cockpit, Python, custom tools) use the same socket API. No client should read pool files directly. This means:
- Clients can be in any language
- Multiple clients can connect simultaneously
- Clean error boundaries — daemon bugs don't crash clients, client bugs don't corrupt pool state

### Single daemon per pool
One process per pool owns everything: API server, PTY instances, pool state. Simpler than a two-daemon setup. On restart, the daemon re-adopts orphaned PTY processes by checking saved PIDs.

### Offload/restore model
Idle sessions are offloaded (snapshot saved, `/clear` sent) to free slots. On restore, Claude's internal `/resume` command rehydrates the conversation. This allows managing more sessions than pool slots.

### Write locking
All pool.json mutations use an async mutex (`withPoolLock()`). This prevents concurrent read-modify-write races when multiple clients issue commands simultaneously.

## What Claude Pool Does NOT Do

- **Terminal rendering** — clients render however they want (xterm.js, plain text, etc.)
- **UI** — that's Open Cockpit's job
- **Non-pool session management** — Claude Pool only manages its own pool sessions. Open Cockpit handles discovery of all Claude sessions on the device.
- **Authentication** — socket is `0600` (owner-only). Network access would need a future auth layer.
