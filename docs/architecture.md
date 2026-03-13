# Architecture

## Overview

Claude Pool is a daemon that manages a pool of Claude Code sessions. It handles session lifecycle, slot allocation, offloading/restoring, and exposes everything through a Unix socket API.

Each pool runs as a single daemon process that owns its PTYs directly. Sessions are addressed by pool-assigned internal IDs (short random strings).

```
claude-pool daemon
  ├── API server (Unix socket, newline-delimited JSON)
  ├── Pool manager (session allocation, queue, LRU eviction, offload/restore)
  ├── PTY manager (in-process, owns terminal instances via creack/pty)
  ├── Attach server (per-session raw PTY pipe sockets)
  ├── Session discovery (idle detection, status tracking)
  └── Reconciliation loop (recycle error slots, periodic health checks)
```

## Components

### Pool Manager
Core business logic. Manages pool.json, session allocation, queueing, offloading, session restoration. All state mutations go through a mutex to prevent races. External interface uses internal session IDs — slot indices are an implementation detail.

### Pool Directory
Each pool is a self-contained directory. The daemon takes `--pool-dir <path>` to specify where to operate. Named pools default to `~/.claude-pool/pools/<name>/`. The pool directory contains config, state, logs, socket, hook scripts, and a `.claude/` folder with hooks that sessions inherit automatically.

### PTY Manager
Owns all terminal instances in-process via `creack/pty`. On daemon restart, re-adopts orphaned PTY processes by checking PIDs from pool.json.

### API Server
Listens on `~/.claude-pool/pools/<name>/api.sock`. Accepts newline-delimited JSON. Routes requests to pool manager.

### Attach Server
When a client requests `attach`, creates a temporary Unix socket for raw PTY I/O. The pipe closes when the session is offloaded or dies. Multiple clients can attach to the same session simultaneously (broadcast).

### Session Discovery
Detects session state (idle, processing) by reading Claude Code's signal files and JSONL transcripts. Process death is detected here — session transitions to offloaded, slot gets recycled. Caches results for performance.

### Reconciliation Loop
Runs every 30s. Recycles error slots, kills orphaned processes, maintains pool health.

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

### Remote Pools (SSH Tunnel)

Remote pools use SSH tunnels. The CLI automatically forwards the remote Unix socket:

```bash
# CLI transparently handles this:
ssh -L /tmp/pool-vps.sock:/home/user/.claude-pool/pools/default/api.sock user@vps

# Registry entry:
"vps": { "socket": "ssh://user@vps/home/user/.claude-pool/pools/default/api.sock" }
```

Full protocol support including subscribe (persistent event stream) works over the tunnel. Zero additional infra — uses existing SSH auth, encrypted, works through NAT/firewalls.

### Daemon Auto-Start

When the CLI connects and finds no running daemon (socket missing), it automatically spawns one — same pattern as Docker, `gpg-agent`, `ssh-agent`. Explicit `claude-pool daemon start/stop` still available for manual control.

## Key Design Decisions

### Sessions, not slots
Clients never see or think about slots. The pool manages slots internally (which physical PTY holds which session). Clients use internal session IDs. This means the pool is free to move sessions between slots, change its internal allocation strategy, etc. without breaking clients.

### Socket as the only client interface
All clients use the same socket API. No client reads pool files directly.

### Single daemon per pool
One process per pool owns everything: API server, PTY instances, pool state. On restart, re-adopts orphaned PTY processes.

### Automatic slot management
The pool decides when to offload sessions (LRU eviction when slots are needed for `start`). Clients can manually offload specific sessions, but there's no bulk "clean" operation.

### Uniform pools
All sessions in a pool run with the same flags. Different flags = different pool.

### Config-driven spawning
Pool config.json stores flags and settings. `init` and `resize` read from config. Changes to config affect future spawns only.

## Hooks

Hooks tell the pool daemon when sessions change state (idle, processing, etc.). They're project-local Claude Code hooks that live in the pool directory's `.claude/` folder.

- `init` writes `.claude/hooks.json` + hook scripts into the pool directory
- Sessions spawn with cwd inside the pool directory → Claude Code loads the hooks automatically
- Hooks write to signal files in the pool directory (using `CLAUDE_POOL_HOME` env var)
- No plugins, no global hook pollution — hooks only affect sessions in that pool
- Self-contained: deleting the pool directory removes everything including hooks

## What Claude Pool Does NOT Do

- **Terminal tabs / persistent shells** — that's claude-term (separate project)
- **Intention files** — that's Open Cockpit's domain
- **Terminal rendering** — clients render however they want
- **Non-pool session discovery** — Open Cockpit handles all-device session browsing
- **Authentication** — socket is `0600` (owner-only). Network auth is future work.
