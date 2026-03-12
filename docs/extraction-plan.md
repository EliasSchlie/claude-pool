# Extraction Plan

Taking lessons from Open Cockpit's pool implementation, but designing a cleaner system from requirements up. Open Cockpit code is a reference, not a blueprint.

## What to reference from Open Cockpit

From `open-cockpit/src/`:

| File | Relevance | Notes |
|------|-----------|-------|
| `pool.js` | High | Pure data layer — good patterns for pool.json read/write, slot selection |
| `pool-manager.js` | High | Core logic for offloading, slot claiming, LRU eviction, session spawning. Needs significant refactoring (remove UI callbacks, absorb PTY management, drop slot-index-based APIs). |
| `pool-lock.js` | High | Async mutex — reuse directly |
| `api-server.js` | High | Unix socket server — adapt socket path from pool directory |
| `api-handlers.js` | Medium | Handler pattern is good. Strip UI handlers, strip slot/term commands, strip intentions. |
| `session-discovery.js` | High | Idle detection, status tracking — adapt paths for pool directory |
| `secure-fs.js` | Medium | Secure file writing |
| `platform.js` | Medium | Cross-platform path resolution |

**Not referenced** (different scope):
- `pty-daemon.js`, `daemon-client.js` — We use in-process PTY management, not a separate daemon
- `main.js`, `renderer.js`, UI modules — Electron-specific
- Terminal tab management — That's claude-term's scope
- Intention files — That's Open Cockpit's scope

## New files to create

| File | Purpose |
|------|---------|
| `src/daemon.js` | Single daemon entry point. Owns PTYs in-process, runs API server, reconciliation loop. |
| `src/pty-manager.js` | In-process PTY management (spawn, kill, read buffer, re-adopt on restart). Replaces the old two-daemon architecture. |
| `src/paths.js` | Centralized path resolution. Pool name → all paths (config.json, pool.json, socket, logs/, offloaded/). |
| `src/pool.js` | Pool state data layer (adapted from Open Cockpit). |
| `src/pool-manager.js` | Pool business logic (adapted). Session-ID-only interface, no slot indices exposed. |
| `src/pool-lock.js` | Async mutex (direct reuse). |
| `src/api-server.js` | Unix socket server (adapted). |
| `src/api-handlers.js` | Command handlers — pool ops, session ops, attach. No UI, no terminals, no intentions. |
| `src/session-discovery.js` | Session state detection (adapted). |
| `src/attach-server.js` | Per-session raw PTY pipe sockets for terminal attachment. |
| `hooks/` | Hook scripts with `CLAUDE_POOL_HOME` env var routing. |

## Separate package: CLI

The CLI ships as `claude-pool-cli` (separate repo or package). It's a thin client:
- Reads `~/.claude-pool/pools.json` registry
- Resolves `--pool` flag to socket connection
- Sends JSON, pretty-prints responses
- Supports local Unix sockets and remote connections (SSH tunneling)

## Migration Strategy

### Phase 1: Build claude-pool daemon
1. Design from requirements (this doc + design principles), not by copying
2. Reference Open Cockpit code for patterns, not structure
3. All sessions addressed by Claude UUID — no slot indices in API
4. Single daemon per pool — PTYs in-process
5. Pool config.json for flags (default: `--dangerously-skip-permissions`)
6. Hooks with `CLAUDE_POOL_HOME` routing
7. Attach via session-level raw pipe sockets

### Phase 2: Build CLI (separate package)
1. Pool registry (`~/.claude-pool/pools.json`)
2. Local socket connections
3. Remote pool support (SSH tunneling)
4. Pretty-printed output, `--json` flag for programmatic use

### Phase 3: Wire Open Cockpit
1. Open Cockpit reads pool registry, connects to pool sockets as a client
2. Remove pool logic from Open Cockpit
3. Pool UI goes through socket API
4. Attach feeds xterm.js via raw pipe sockets

### Phase 4: Extract claude-term (separate project)
1. Per-session persistent terminal tabs
2. Independent from claude-pool
3. Open Cockpit depends on both

## Open Questions

- [ ] Should the daemon auto-start when the CLI connects? (Like Docker)
- [ ] How to handle migration for existing Open Cockpit users?
- [ ] Should hooks ship as a claude-pool plugin or integrate with Open Cockpit's existing plugin?
- [ ] Pool registry format — should remote connections use SSH tunnel strings, TCP addresses, or something else?
- [ ] Should `pool-start` block until the Claude UUID is discovered, or return immediately with a pending ID?
