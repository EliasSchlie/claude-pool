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
Each pool is a self-contained directory. The daemon takes `--pool-dir <path>` to specify where to operate. Named pools default to `~/.claude-pool/<name>/`. The pool directory contains config, state, logs, socket, hook scripts, and a `.claude/` folder with hooks that sessions inherit automatically.

### PTY Manager
Owns all terminal instances in-process via `creack/pty`. On daemon restart, re-adopts orphaned PTY processes by checking PIDs from pool.json.

### API Server
Listens on `~/.claude-pool/<name>/api.sock`. Accepts newline-delimited JSON. Routes requests to pool manager.

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
  "default": { "socket": "~/.claude-pool/default/api.sock" },
  "work": { "socket": "~/.claude-pool/work/api.sock" },
  "vps": { "socket": "ssh://user@vps/home/user/.claude-pool/default/api.sock" }
}
```

CLI and Open Cockpit both read this registry. No routing logic duplication — it's a simple name → connection lookup.

### Remote Pools (SSH Tunnel)

Remote pools use SSH tunnels. The CLI automatically forwards the remote Unix socket:

```bash
# CLI transparently handles this:
ssh -L /tmp/pool-vps.sock:/home/user/.claude-pool/default/api.sock user@vps

# Registry entry:
"vps": { "socket": "ssh://user@vps/home/user/.claude-pool/default/api.sock" }
```

Full protocol support including subscribe (persistent event stream) works over the tunnel. Zero additional infra — uses existing SSH auth, encrypted, works through NAT/firewalls.

## Key Design Decisions

1. **Sessions, not slots.** Clients use internal session IDs. The pool manages slots transparently — free to move sessions between slots without breaking clients.
2. **Socket as the only client interface.** No client reads pool files directly.
3. **Single daemon per pool.** One process owns everything. On restart, re-adopts orphaned PTY processes.
4. **Automatic slot management.** LRU eviction when slots are needed. No manual offload command.
5. **Uniform pools.** All sessions run the same flags. Different flags = different pool.
6. **Config-driven spawning.** `config.json` drives all spawn operations. Changes affect future spawns only.
7. **Each pool can run its own code version.** Safe testing of new versions alongside stable pools.
8. **CLI is a separate package.** Thin router resolving pool names to socket connections. Keeps daemon minimal.
9. **Sugar operations live in the daemon.** High-level ops (start, followup, wait, set) coordinate multiple internal steps server-side.
10. **Pool config survives destroy.** Directory + config persist. Only manual deletion fully removes a pool.

## Implementation Details

- Written in Go. Single static binary, no runtime dependencies. PTY via `creack/pty`, sockets and JSON via stdlib.
- Newline-delimited JSON protocol over Unix sockets. Socket permissions `0600` (owner-only).
- Offloaded sessions stored as `meta.json` (JSONL transcripts are the persistent record).
- Default flags: `--dangerously-skip-permissions`.
- Pending input detection: polls terminal buffer for un-submitted text (consecutive-miss threshold).
- Lock discipline: hold mutex only for in-memory state mutations. Never across I/O, process spawning, or network calls.
- Slot states are internal — consumers never see them (invariant #5). Slot errors are recycled automatically.

## Hooks

Hooks tell the pool daemon when sessions change state (idle, processing, etc.). Two-layer design: a global hook-runner installed once, and pool-local scripts deployed on every `init`.

### Global install (`claude-pool install`)

- Writes `~/.claude-pool/hook-runner.sh` — a thin entry point that delegates to pool-local scripts
- Registers hook entries in `~/.claude/settings.json` for all relevant events (SessionStart, Stop, PreToolUse, PermissionRequest, PostToolUse, UserPromptSubmit)
- Installs the Claude Code skill to `~/.claude/skills/claude-pool/SKILL.md`
- Run once per machine. `claude-pool uninstall` reverses everything.

### Pool-local scripts (`init`)

- Each `init` deploys hook scripts (`common.sh`, `idle-signal.sh`, `session-pid-map.sh`) to `<pool-dir>/hooks/`
- The global hook-runner checks `$CLAUDE_POOL_DIR` (set by the daemon on pool sessions) and delegates to the pool's scripts. Non-pool sessions exit silently.
- Scripts write to signal files in the pool directory for idle detection and PID tracking

### Why two layers

Different pools (or different branches under test) can run different hook versions independently. The global `settings.json` entries point to the fixed hook-runner, which dispatches to whichever pool owns the session. No version conflicts, no race conditions between concurrent pools.

## Pool Directory Structure

```
~/.claude-pool/<name>/
  config.json            # Pool configuration (flags, size, dir, keepFresh)
  pool.json              # Pool state (sessions, queue, mappings)
  api.sock               # Daemon socket
  daemon.pid             # Daemon PID
  daemon.log             # Single JSONL log file (all categories, 30-day retention)
  offloaded/             # Offloaded session metadata
  archived/              # Archived sessions (auto-cleaned after 30 days)
  session-pids/          # PID → internal ID mapping
  idle-signals/          # Session idle signal files
  hooks/                 # Pool-local hook scripts (deployed by init)
```

Global (not per-pool):

```
~/.claude-pool/
  pools.json             # Pool name → socket path (auto-updated)
  hook-runner.sh         # Global hook entry point (delegates to pool-local scripts)
  hooks/                 # Global hook script templates
  default/               # Default pool
  work/                  # Named pool
```

Deleting `~/.claude-pool/<name>/` completely removes that pool with zero side effects.

## What Claude Pool Does NOT Do

- **Terminal tabs / persistent shells** — that's claude-term (separate project)
- **Intention files** — that's Open Cockpit's domain
- **Terminal rendering** — clients render however they want
- **Non-pool session discovery** — Open Cockpit handles all-device session browsing
- **Authentication** — socket is `0600` (owner-only). Network auth is future work.
