# Architecture

## Overview

Claude Pool is a daemon that manages a pool of Claude Code sessions. It handles session lifecycle, slot allocation, offloading/restoring, and exposes everything through a Unix socket API.

Each pool runs as a single daemon process that owns its PTYs directly. Sessions are addressed exclusively by their Claude UUID.

```
claude-pool daemon
  ├── API server (Unix socket, newline-delimited JSON)
  ├── Pool manager (session allocation, LRU eviction, offload/restore)
  ├── PTY manager (in-process, owns terminal instances via node-pty)
  ├── Attach server (per-session raw PTY pipe sockets)
  ├── Session discovery (idle detection, status tracking)
  └── Reconciliation loop (auto-restart dead sessions, periodic health checks)
```

## Components

### Pool Manager
Core business logic. Manages pool.json, session allocation, offloading, archiving, session restoration. All state mutations go through `withPoolLock()` to prevent races. External interface uses Claude UUIDs only — slot indices are an internal implementation detail.

### PTY Manager
Owns all terminal instances in-process via `node-pty`. On daemon restart, re-adopts orphaned PTY processes by checking PIDs from pool.json.

### API Server
Listens on `~/.claude-pool/pools/<name>/api.sock`. Accepts newline-delimited JSON. Routes requests to pool manager.

### Attach Server
When a client requests `attach`, creates a temporary Unix socket for raw PTY I/O. The pipe closes when the session is offloaded or dies. Multiple clients can attach to the same session simultaneously (broadcast).

### Session Discovery
Detects session state (idle, processing, dead) by reading Claude Code's signal files and JSONL transcripts. Caches results for performance.

### Reconciliation Loop
Runs every 30s. Restarts dead/error sessions, kills orphaned processes, maintains pool health.

## Multi-Pool Access

Pools are discovered via a shared registry (`~/.claude-pool/pools.json`):

```json
{
  "default": { "socket": "~/.claude-pool/pools/default/api.sock" },
  "work": { "socket": "~/.claude-pool/pools/work/api.sock" },
  "vps": { "socket": "ssh://user@vps/home/user/.claude-pool/pools/default/api.sock" }
}
```

CLI and Open Cockpit both read this registry. No routing logic duplication — it's a simple name → connection lookup.

## Key Design Decisions

### Sessions, not slots
Clients never see or think about slots. The pool manages slots internally (which physical PTY holds which session). Clients use Claude UUIDs. This means the pool is free to move sessions between slots, change its internal allocation strategy, etc. without breaking clients.

### Socket as the only client interface
All clients use the same socket API. No client reads pool files directly.

### Single daemon per pool
One process per pool owns everything: API server, PTY instances, pool state. On restart, re-adopts orphaned PTY processes.

### Automatic slot management
The pool decides when to offload sessions (LRU eviction when slots are needed for `pool-start`). Clients can manually offload specific sessions, but there's no bulk "clean" operation.

### Uniform pools
All sessions in a pool run with the same flags. Different flags = different pool.

### Config-driven spawning
Pool config.json stores flags and settings. `pool init` and `pool resize` read from config. Changes to config affect future spawns only.

## What Claude Pool Does NOT Do

- **Terminal tabs / persistent shells** — that's claude-term (separate project)
- **Intention files** — that's Open Cockpit's domain
- **Terminal rendering** — clients render however they want
- **Non-pool session discovery** — Open Cockpit handles all-device session browsing
- **Authentication** — socket is `0600` (owner-only). Network auth is future work.
