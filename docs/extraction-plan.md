# Extraction Plan

Migrating pool management from Open Cockpit into Claude Pool.

## Source Files to Migrate

From `open-cockpit/src/`:

| File | Destination | Changes needed |
|------|-------------|----------------|
| `pool.js` | `src/pool.js` | None — pure data layer, zero Electron deps |
| `pool-manager.js` | `src/pool-manager.js` | Remove UI callback registration (onIntentionChanged, etc). Make callbacks optional/no-op. |
| `pool-lock.js` | `src/pool-lock.js` | None — pure async mutex |
| `daemon-client.js` | `src/daemon-client.js` | Update socket path for new directory structure |
| `pty-daemon.js` | `src/pty-daemon.js` | Remove `ELECTRON_RUN_AS_NODE` requirement — run directly with Node |
| `api-server.js` | `src/api-server.js` | Update socket path |
| `api-handlers.js` | `src/api-handlers.js` | **Split**: keep pool/session/PTY handlers, remove UI handlers (show, hide, screenshot, ui-state, session-select, relaunch) |
| `session-discovery.js` | `src/session-discovery.js` | None — pure Node.js |
| `secure-fs.js` | `src/secure-fs.js` | None |
| `platform.js` | `src/platform.js` | None |

### New files to create

| File | Purpose |
|------|---------|
| `src/daemon.js` | Main daemon entry point (replaces main.js orchestration). Starts API server, PTY daemon, reconciliation loop. |
| `bin/claude-pool` | CLI entry point. Sends JSON to socket, pretty-prints responses. |

### NOT migrated (stays in Open Cockpit)

- `main.js` — Electron window management, IPC wiring
- `renderer.js` and all renderer modules — UI
- `dock-layout.js` — UI layout
- All window control handlers (show, hide, screenshot, etc.)
- Non-pool session discovery (browsing all Claude sessions on device)
- Plugin/hooks system

## Migration Strategy

### Phase 1: Copy + adapt
1. Copy source files into claude-pool repo
2. Adapt paths (`~/.open-cockpit/` → `~/.claude-pool/`)
3. Remove Electron-specific code
4. Create daemon entry point
5. Create CLI
6. Test standalone

### Phase 2: Wire Open Cockpit
1. Open Cockpit starts claude-pool daemon (if not running)
2. Open Cockpit connects to claude-pool socket as a client
3. Remove duplicated pool logic from Open Cockpit
4. Pool UI commands go through socket instead of in-process calls

### Phase 3: Named pools
1. Pool name parameter on daemon start
2. Separate directories per pool
3. CLI `--pool` flag
4. Open Cockpit pool selector

## Path Changes

| Current | New |
|---------|-----|
| `~/.open-cockpit/pool.json` | `~/.claude-pool/pools/default/pool.json` |
| `~/.open-cockpit/api.sock` | `~/.claude-pool/pools/default/api.sock` |
| `~/.open-cockpit/pty-daemon.sock` | `~/.claude-pool/pools/default/pty-daemon.sock` |
| `~/.open-cockpit/pty-daemon.pid` | `~/.claude-pool/pools/default/pty-daemon.pid` |
| `~/.open-cockpit/offloaded/` | `~/.claude-pool/offloaded/` |
| `~/.open-cockpit/session-pids/` | `~/.claude-pool/session-pids/` |

## Open Questions

- [ ] Should the daemon auto-start when the CLI is used? (Like Docker)
- [ ] Should pools share the offloaded directory or have separate ones?
- [ ] How to handle migration for existing Open Cockpit users? (symlinks? migration script?)
- [ ] Should the PTY daemon be shared across pools or one per pool?
