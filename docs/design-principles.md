# Design Principles

Design decisions and implementation details. Invariants live in [SPEC.md](../SPEC.md).

**This is not a 1:1 copy of Open Cockpit's pool logic.** We're taking lessons learned from Open Cockpit and designing a cleaner system from the requirements up. Open Cockpit code is a reference, not a blueprint.

---

## Design Decisions (strong defaults, debatable with good reason)

1. **One daemon per pool.** Each pool runs a single daemon process that owns PTYs directly (via `creack/pty` in-process). On restart, the daemon re-adopts orphaned PTY processes by checking PIDs from pool.json.

2. **Each pool can run its own code version.** Different pools may run different versions of claude-pool. This enables safe testing of new versions alongside a stable production pool.

3. **CLI is a separate package.** The CLI is a thin router that resolves pool names to socket connections (local or remote). It ships independently from the daemon. This keeps the daemon minimal and enables remote pool access without installing the full daemon.

4. **Pool registry for multi-pool access.** A shared registry (`~/.claude-pool/pools.json`) maps pool names to socket paths (local) or connection strings (remote). CLI and Open Cockpit both read this registry. Creating a pool automatically updates the registry.

5. **CLI commands default to the `default` pool.** If `--pool` is not specified, all commands target the `default` entry in the registry.

6. **Hooks are project-local and self-contained.** The pool daemon writes `.claude/hooks.json` + hook scripts into the pool directory on `init`. Sessions spawn there, so Claude Code loads the hooks automatically. Hook scripts read `CLAUDE_POOL_HOME` and `CLAUDE_POOL_SESSION_ID` environment variables set by the daemon. No plugins, no global hooks — each pool is fully self-contained.

7. **Sugar operations live in the daemon.** High-level operations (start, followup, wait, pin) that coordinate multiple internal steps (slot claiming, LRU eviction, offload/restore, queueing) are handled server-side. Clients send one request, get one response.

8. **Write locking prevents races.** All pool state mutations go through a mutex. Multiple concurrent clients cannot corrupt pool.json.

9. **Pool config is the single source for spawn settings.** Each pool has a `config.json` with flags, size, etc. All spawn operations (init, resize, reconciliation) read flags from config. `config set` updates the config — affects future spawns only. No per-command flag overrides in the protocol.

10. **Requests queue when slots are full.** If all slots are busy and no idle session can be offloaded, `start` queues the request FIFO. The request gets an internal session ID immediately. When a slot becomes available, the queued request is executed.

11. **Sessions are loaded, offloaded, or archived.** Sessions are either in a slot (loaded), not in a slot (offloaded), or soft-deleted (archived). Dead/error sessions are treated like offloaded — `followup` auto-resumes them. To load an offloaded session: send `followup` (auto-resumes) or `pin` (triggers priority load). To offload: `offload` or automatic LRU eviction. To archive: `archive` (stops active sessions first, errors if unarchived children unless `recursive`). Archived sessions are hidden from `ls` by default (use `archived: true` flag), auto-cleaned after 30 days. `unarchive` restores to offloaded state.

12. **Attach requires a live session.** Attach fails for offloaded/queued sessions. To attach an offloaded session: pin it (triggers priority load) → wait for it to become live → attach. The raw pipe closes automatically when the session is offloaded or dies.

13. **Automatic slot management.** The pool decides when to offload sessions via LRU eviction when slots are needed. Clients can manually offload individual sessions. No bulk "clean" command.

14. **Session priority affects eviction order.** Each session has a numeric priority (default 0, range unbounded — negative allowed). LRU eviction prefers lower-priority sessions first, then oldest within the same priority. Priority is set per-session, not per-message. Does not affect processing speed or queue order.

15. **Pool config survives destroy.** `destroy` kills all sessions and the daemon exits, but the pool directory, config.json, and pools.json registry entry persist. The pool can be re-initialized with `init`. Only manually deleting the pool directory truly removes it.

---

## Implementation Details (flexible, change freely)

16. Written in Go. Single static binary, no runtime dependencies. PTY via `creack/pty`, sockets and JSON via stdlib.
17. Newline-delimited JSON protocol over Unix sockets.
18. Reconciliation loop runs every 30 seconds.
19. Socket permissions are `0600` (owner-only) — only the Unix user who owns the socket file can connect. Other users on the same machine cannot access the pool.
20. Offloaded sessions stored as `meta.json` (no terminal snapshot — JSONL transcripts are the persistent record, read via Claude UUID).
21. Default flags: `--dangerously-skip-permissions`.
22. Typing detection: polls terminal buffer for un-submitted input text (reference: Open Cockpit `session-discovery.js` terminal input polling with consecutive-miss threshold).
23. Lock discipline: hold the mutex only for in-memory state mutations. Never hold it across I/O, process spawning, or network calls.

---

## Session States (API-visible)

| State | Meaning |
|-------|---------|
| `queued` | Request received, waiting for a slot |
| `idle` | Finished processing, waiting for input |
| `typing` | Un-submitted input detected in terminal buffer |
| `processing` | Claude is working on a response |
| `offloaded` | Slot freed, JSONL transcript accessible via UUID |
| `dead` | Process died unexpectedly |
| `error` | Slot error (crash during startup, etc.) |
| `archived` | Soft-deleted, hidden from ls by default, auto-cleaned after 30 days |

Internal-only states (not exposed via API): `fresh` (pre-warmed slot, never prompted), `starting` (Claude process spawning). These are slot management details — clients see `queued` until the session is ready, then the first real state.

## Session Identity

```
Internal ID (pool-assigned, short random)  ←→  Claude UUID (discovered after spawn)
       "a7f2x9"                            ←→  "2947bf12-d307-4a1c-..."
```

- Internal ID assigned immediately on `start` or `pin` (even while queued)
- Claude UUID discovered via PID mapping after Claude process starts
- Both are queryable via `info`
- All API commands accept internal ID
- Claude UUID needed for external operations (transcripts, `/resume`, etc.)

## Parent-Child Ownership

```
External caller (no parent)
  └── session a7f2x9 (parent: null, spawned by external CLI)
       ├── session b3k9m2 (parent: a7f2x9)
       │    └── session c1p4q7 (parent: b3k9m2)
       └── session d8w2n5 (parent: a7f2x9)
```

- `CLAUDE_POOL_SESSION_ID` env var set on spawned sessions (auto-detection)
- `parentId` field in `start` / `pin` for explicit override
- `ls` default: returns sessions owned by caller (direct children)
- `ls` with `tree: true`: returns children with nested descendants
- `ls` with `all: true`: returns all pool sessions
- `info` includes a `children` array — direct child sessions

## Session Priority

Numeric value (default 0, unbounded — negative allowed). Affects LRU eviction order only:
- Lower priority evicted first
- Within same priority, oldest evicted first
- Does not affect queue order or processing speed
- Set via `set-priority` command, can be changed anytime

## Scope — what claude-pool does NOT do

- **Terminal tabs / persistent shells** — that's claude-term (separate project). Claude-pool works without claude-term.
- **Intention files** — that's an Open Cockpit / UI feature. The pool doesn't know about intentions.
- **Terminal rendering** — clients render however they want. Pool provides raw buffer data.
- **Non-pool session discovery** — Open Cockpit handles browsing all Claude sessions on the device.
- **Authentication** — socket is `0600` (owner-only). Network auth is a future concern.

---

## Pool Directory Structure (consequence of invariant #1)

Each pool is fully self-contained:

```
~/.claude-pool/pools/<name>/
  config.json            # Pool configuration (flags, size)
  pool.json              # Pool state (sessions, queue, mappings)
  api.sock               # Daemon socket
  daemon.pid             # Daemon PID
  logs/                  # All pool logs
    daemon.log           # Daemon output, lifecycle events
    error.log            # Errors and crashes
    api.log              # API requests/responses (optional, for debugging)
  offloaded/             # Offloaded sessions
    <internalId>/
      meta.json
  archived/              # Archived sessions (auto-cleaned after 30 days)
    <internalId>/
      meta.json
  session-pids/          # PID → internal ID mapping
  idle-signals/          # Session idle signal files
```

Global registry (not per-pool):

```
~/.claude-pool/
  pools.json             # Pool name → socket path/connection string (auto-updated)
  pools/
    default/             # Default pool
    work/                # Named pool
    ...
```

Nothing lives outside the pool directory except the registry. Deleting `~/.claude-pool/pools/foo/` completely removes that pool with zero side effects.
