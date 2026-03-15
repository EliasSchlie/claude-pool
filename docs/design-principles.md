# Design Principles

Design decisions and implementation details. Invariants and API surface live in [SPEC.md](../SPEC.md).

**Not a 1:1 copy of Open Cockpit.** Lessons learned from OC, designed from requirements up. OC code is reference, not blueprint.

---

## Design Decisions (strong defaults, debatable with good reason)

1. **One daemon per pool.** Owns PTYs in-process (`creack/pty`). On restart, re-adopts orphaned PTY processes via PIDs from pool.json.
2. **Each pool can run its own code version.** Safe testing of new versions alongside stable pools.
3. **CLI is a separate package.** Thin router resolving pool names to socket connections. Keeps daemon minimal, enables remote access.
4. **Pool registry for multi-pool access.** `~/.claude-pool/pools.json` maps names to socket paths (local) or connection strings (remote). Auto-updated on pool creation.
5. **CLI defaults to the `default` pool.**
6. **Two-layer hook design.** Global hook-runner (`~/.claude-pool/hook-runner.sh`) registered once in `~/.claude/settings.json`, delegates to pool-local scripts in `<pool-dir>/hooks/`. Sessions set `CLAUDE_POOL_DIR` and `CLAUDE_POOL_SESSION_ID` env vars. Non-pool sessions exit silently.
7. **Sugar operations live in the daemon.** High-level ops (start, followup, wait, set) coordinate multiple internal steps server-side. One request, one response.
8. **Write locking prevents races.** All state mutations go through a mutex.
9. **Pool config is the single source for spawn settings.** `config.json` drives all spawn operations. No per-command flag overrides.
10. **Requests queue when slots are full.** FIFO. Internal session ID assigned immediately.
11. **Sessions are loaded, offloaded, or archived.** Process death → session becomes offloaded (error logged). Repeated load failures → session marked `error`. `followup` auto-resumes offloaded sessions.
12. **Attach requires a live session.** Use `followup` to resume offloaded sessions first.
13. **Automatic slot management.** LRU eviction when slots needed. No bulk "clean" command.
14. **Session priority affects eviction order.** Lower = evicted first, then oldest within same priority. Does not affect queue order or processing speed.
15. **Pool config survives destroy.** Directory + config persist. Only manual deletion fully removes a pool.

---

## Implementation Details (flexible, change freely)

16. Written in Go. Single static binary, no runtime dependencies. PTY via `creack/pty`, sockets and JSON via stdlib.
17. Newline-delimited JSON protocol over Unix sockets.
18. Reconciliation loop runs every 30 seconds.
19. Socket permissions `0600` (owner-only).
20. Offloaded sessions stored as `meta.json` (JSONL transcripts are the persistent record).
21. Default flags: `--dangerously-skip-permissions`.
22. Pending input detection: polls terminal buffer for un-submitted text (consecutive-miss threshold). Populates `pendingInput` session property.
23. Lock discipline: hold mutex only for in-memory state mutations. Never across I/O, process spawning, or network calls.

---

## Slot States (internal)

See [SPEC.md](../SPEC.md) for the slot state table. Slots are internal — consumers never see them (invariant #5). Slot errors are recycled automatically (kill, replace with fresh).

---

## Pool Directory Structure

```
~/.claude-pool/<name>/
  config.json            # Pool configuration (flags, size, keepFresh)
  pool.json              # Pool state (sessions, queue, mappings)
  api.sock               # Daemon socket
  daemon.pid             # Daemon PID
  daemon.log             # Single JSONL log file (all categories, 30-day retention)
  offloaded/             # Offloaded sessions
    <internalId>/
      meta.json
  archived/              # Archived sessions (auto-cleaned after 30 days)
    <internalId>/
      meta.json
  session-pids/          # PID → internal ID mapping
  idle-signals/          # Session idle signal files
  hooks/                 # Pool-local hook scripts (deployed by init)
```

Global (not per-pool):

```
~/.claude-pool/
  pools.json             # Pool name → socket path/connection string (auto-updated)
  hook-runner.sh         # Global hook entry point (delegates to pool-local scripts)
  hooks/                 # Global hook script templates
  default/               # Default pool
  work/                  # Named pool
  ...
```

Nothing lives outside the pool directory except the registry and global hooks. Deleting `~/.claude-pool/<name>/` completely removes that pool with zero side effects.
