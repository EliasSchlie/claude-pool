# Architecture

## Overview

Claude Pool is a daemon that manages a pool of Claude Code terminal sessions. It handles session lifecycle, slot allocation, offloading/restoring, and exposes everything through a Unix socket API.

```
claude-pool daemon
  ├── API server (Unix socket, newline-delimited JSON)
  ├── Pool manager (slot allocation, LRU eviction, offload/restore)
  ├── PTY daemon (separate process, manages terminal instances)
  ├── Session discovery (idle detection, status tracking)
  └── Reconciliation loop (auto-restart dead slots, periodic health checks)
```

## Components

### Pool Manager
Core business logic. Manages pool.json, slot allocation, offloading, archiving, session restoration. All state mutations go through `withPoolLock()` to prevent races.

### PTY Daemon
Standalone subprocess that owns all PTY instances. Communicates via its own Unix socket (`pty-daemon.sock`). Survives daemon restarts — sessions stay alive even if the pool daemon is restarted.

### API Server
Listens on `~/.claude-pool/pools/<name>/api.sock`. Accepts newline-delimited JSON. Routes requests to pool manager. Any number of clients can connect simultaneously.

### Session Discovery
Detects session state (idle, processing, dead) by reading Claude Code's signal files and JSONL transcripts. Caches results for performance.

### Reconciliation Loop
Runs every 30s. Restarts dead/error slots, kills orphaned processes, maintains pool health.

## Directory Structure

```
~/.claude-pool/
  pools/
    default/
      pool.json          # Pool state (slots, sessions, settings)
      api.sock           # API socket
      pty-daemon.sock    # PTY daemon socket
      pty-daemon.pid     # PTY daemon PID
    work/                # Named pool example
      ...
  offloaded/
    <sessionId>/
      snapshot.log       # Terminal output at offload time
      meta.json          # Session metadata
  session-pids/          # PID → session mapping for auto-detection
```

## Key Design Decisions

### Socket as the only interface
All clients (CLI, Open Cockpit, Python, custom tools) use the same socket API. No in-process API, no shared libraries required. This means:
- Clients can be in any language
- Daemon can restart without killing clients
- Multiple clients can connect simultaneously
- Clean error boundaries — daemon bugs don't crash clients

### PTY daemon as separate process
The PTY daemon is a separate process from the pool daemon. This means:
- Sessions survive pool daemon restarts
- PTY management is isolated from business logic
- Multiple pool daemons could theoretically share a PTY daemon

### Offload/restore model
Idle sessions are offloaded (snapshot saved, `/clear` sent) to free slots. On restore, Claude's internal `/resume` command rehydrates the conversation. This allows managing more sessions than pool slots.

### Write locking
All pool.json mutations use an async mutex (`withPoolLock()`). This prevents concurrent read-modify-write races when multiple clients issue commands simultaneously.

## What Claude Pool Does NOT Do

- **Terminal rendering** — clients render however they want (xterm.js, plain text, etc.)
- **UI** — that's Open Cockpit's job
- **Non-pool session management** — Claude Pool only manages its own pool sessions. Open Cockpit handles discovery of all Claude sessions on the device.
- **Authentication** — socket is `0600` (owner-only). Network access would need a future auth layer.
