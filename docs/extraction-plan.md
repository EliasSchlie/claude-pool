# Extraction Plan

Migrating pool management from Open Cockpit into Claude Pool.

## Source Files to Migrate

From `open-cockpit/src/`:

| File | Destination | Changes needed |
|------|-------------|----------------|
| `pool.js` | `src/pool.js` | None — pure data layer, zero Electron deps |
| `pool-manager.js` | `src/pool-manager.js` | Remove UI callback registration (onIntentionChanged, etc). Make callbacks optional/no-op. All paths parameterized by pool directory. |
| `pool-lock.js` | `src/pool-lock.js` | None — pure async mutex |
| `daemon-client.js` | `src/daemon-client.js` | Socket path from pool directory |
| `pty-daemon.js` | `src/pty-daemon.js` | Remove `ELECTRON_RUN_AS_NODE` requirement — run directly with Node. Socket/PID paths from pool directory. |
| `api-server.js` | `src/api-server.js` | Socket path from pool directory |
| `api-handlers.js` | `src/api-handlers.js` | **Split**: keep pool/session/PTY handlers, remove UI handlers (show, hide, screenshot, ui-state, session-select, relaunch) |
| `session-discovery.js` | `src/session-discovery.js` | Read idle-signals/session-pids from pool directory |
| `secure-fs.js` | `src/secure-fs.js` | None |
| `platform.js` | `src/platform.js` | None |

### New files to create

| File | Purpose |
|------|---------|
| `src/daemon.js` | Main daemon entry point. Receives pool name, resolves pool directory, starts API server, PTY daemon, reconciliation loop. |
| `src/paths.js` | All path resolution centralized. Takes pool name → returns all paths (pool.json, sockets, offloaded/, etc.) |
| `bin/claude-pool` | CLI entry point. Parses `--pool` flag, connects to correct socket, sends JSON, pretty-prints responses. |
| `hooks/` | Hook scripts migrated from Open Cockpit, adapted to read `CLAUDE_POOL_HOME` env var for pool routing. |

### NOT migrated (stays in Open Cockpit)

- `main.js` — Electron window management, IPC wiring
- `renderer.js` and all renderer modules — UI
- `dock-layout.js` — UI layout
- All window control handlers (show, hide, screenshot, etc.)
- Non-pool session discovery (browsing all Claude sessions on device)

## Hooks Isolation

**Problem:** Claude Code hooks are installed globally (one plugin per installation). But each pool needs hooks that write to its own directory.

**Solution:** Environment variable routing.

1. When the pool daemon spawns a Claude session, it sets env vars:
   ```
   CLAUDE_POOL_HOME=~/.claude-pool/pools/mypool
   CLAUDE_POOL_NAME=mypool
   ```
2. Hook scripts read `CLAUDE_POOL_HOME` to determine where to write (idle signals, session PIDs, intentions).
3. If `CLAUDE_POOL_HOME` is not set, hooks fall back to Open Cockpit behavior (for non-pool sessions).

This means hooks ship with claude-pool (not Open Cockpit), and Open Cockpit installs them or delegates to claude-pool's plugin.

## Migration Strategy

### Phase 1: Extract + independent pools from day one
1. Copy source files into claude-pool repo
2. All paths parameterized by pool name (no hardcoded paths)
3. Each pool gets its own directory under `~/.claude-pool/pools/<name>/`
4. Each pool runs its own daemon + PTY daemon (invariant #1)
5. Create daemon entry point
6. Create CLI with `--pool` flag
7. Migrate hooks with `CLAUDE_POOL_HOME` routing
8. Test standalone

### Phase 2: Wire Open Cockpit
1. Open Cockpit discovers running pools (scan `~/.claude-pool/pools/*/api.sock`)
2. Open Cockpit connects as a socket client
3. Remove duplicated pool logic from Open Cockpit
4. Pool UI commands go through socket

### Phase 3: Polish
1. Daemon auto-start on CLI use
2. `npm install -g claude-pool`
3. Migration tool for existing Open Cockpit users

## Path Layout (per-pool, fully self-contained)

```
~/.claude-pool/pools/<name>/
  pool.json              # Pool state
  api.sock               # Pool daemon socket
  pty-daemon.sock        # PTY daemon socket
  pty-daemon.pid         # PTY daemon PID
  daemon.pid             # Pool daemon PID
  daemon.log             # Pool daemon log
  offloaded/             # Offloaded sessions
    <sessionId>/
      snapshot.log
      meta.json
  session-pids/          # PID → session mapping
  idle-signals/          # Session idle signal files
  intentions/            # Session intention files
```

Deleting `~/.claude-pool/pools/<name>/` completely removes that pool with zero side effects.

## Resolved Questions

- ~~Should pools share the offloaded directory?~~ **No.** Each pool owns its offloaded sessions. (invariant #1)
- ~~Should the PTY daemon be shared across pools?~~ **No.** One PTY daemon per pool. (invariant #1)
- ~~How to handle hooks for different pools?~~ **Env var routing.** `CLAUDE_POOL_HOME` set by daemon when spawning sessions.

## Open Questions

- [ ] Should the daemon auto-start when the CLI is used? (Like Docker)
- [ ] How to handle migration for existing Open Cockpit users? (symlinks? migration script? detect and offer?)
- [ ] Should hooks ship as a claude-pool plugin or integrate with Open Cockpit's existing plugin?
- [ ] How does Open Cockpit discover which pools exist? (scan directory? config file? registry?)
