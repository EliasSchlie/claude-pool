# Design Principles

> ⛔ **Protected file.** No changes without explicit user permission.

Rules that govern all design decisions. Tiered by importance — higher tiers override lower tiers.

**This is not a 1:1 copy of Open Cockpit's pool logic.** We're taking lessons learned from Open Cockpit and designing a cleaner system from the requirements up. Open Cockpit code is a reference, not a blueprint.

---

## 🔴 Invariants (never broken)

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **Clients only interact through the socket.** All clients (CLI, Python, Open Cockpit, custom tools) talk to a pool exclusively through its socket API. No client should ever directly read pool.json, write to idle-signals/, or import pool internals. The socket is the boundary — inside is the pool's business, outside is the client's business.

3. **Each pool has its own logs.** All logging for a pool goes into that pool's directory. When debugging, you look at one pool's logs — never need to grep through shared log files or correlate across pools.

4. **Pools are uniform.** All sessions in a pool run with the same flags and configuration. If you need different flags, create a different pool. No mixed configurations within a pool.

5. **Internal session IDs are the primary identifier.** Each session gets a pool-assigned internal ID (short random string) at request time — before a slot is allocated, before Claude starts. This ID is stable across the session's entire lifecycle: queued → live → offloaded → resumed. The Claude UUID is discovered later and mapped 1:1. Both are queryable, but the internal ID is what clients use.

6. **Sessions have owners.** Every session tracks its parent (the session or external caller that spawned it). By default, API queries return only sessions owned by the caller (direct children + their descendants). An explicit flag (`all: true`) shows all pool sessions. This prevents sessions from interfering with each other's sub-agents.

---

## 🟡 Design Decisions (strong defaults, debatable with good reason)

7. **One daemon per pool.** Each pool runs a single daemon process that owns PTYs directly (via `creack/pty` in-process). On restart, the daemon re-adopts orphaned PTY processes by checking PIDs from pool.json.

8. **Each pool can run its own code version.** Different pools may run different versions of claude-pool. This enables safe testing of new versions alongside a stable production pool.

9. **CLI is a separate package.** The CLI is a thin router that resolves pool names to socket connections (local or remote). It ships independently from the daemon. This keeps the daemon minimal and enables remote pool access without installing the full daemon.

10. **Pool registry for multi-pool access.** A shared registry (`~/.claude-pool/pools.json`) maps pool names to socket paths (local) or connection strings (remote). CLI and Open Cockpit both read this registry. Creating a pool automatically updates the registry.

11. **CLI commands default to the `default` pool.** If `--pool` is not specified, all commands target the `default` entry in the registry.

12. **Hooks use environment variables for pool routing.** The pool daemon sets `CLAUDE_POOL_HOME` and `CLAUDE_POOL_SESSION_ID` when spawning sessions. Hook scripts read `CLAUDE_POOL_HOME` to determine which pool directory to write to. `CLAUDE_POOL_SESSION_ID` identifies the session for parent-child tracking.

13. **Sugar operations live in the daemon.** High-level operations (start, followup, wait, pin) that coordinate multiple internal steps (slot claiming, LRU eviction, offload/restore, queueing) are handled server-side. Clients send one request, get one response.

14. **Write locking prevents races.** All pool state mutations go through a mutex. Multiple concurrent clients cannot corrupt pool.json.

15. **Pool config is the single source for spawn settings.** Each pool has a `config.json` with flags, size, etc. All spawn operations (init, resize, reconciliation) read flags from config. `config set` updates the config — affects future spawns only. No per-command flag overrides in the protocol.

16. **Requests queue when slots are full.** If all slots are busy and no idle session can be offloaded, `start` queues the request FIFO. The request gets an internal session ID immediately. When a slot becomes available, the queued request is executed.

17. **Sessions are loaded or offloaded — nothing else.** No "archive" concept. Sessions are either in a slot (loaded) or not (offloaded). Dead/error sessions are treated like offloaded — `followup` auto-resumes them. To load an offloaded session: send `followup` (auto-resumes) or `pin` (triggers priority load). To offload: `offload` or automatic LRU eviction.

18. **Attach requires a live session.** Attach fails for offloaded/queued sessions. To attach an offloaded session: pin it (triggers priority load) → wait for it to become live → attach. The raw pipe closes automatically when the session is offloaded or dies.

19. **Automatic slot management.** The pool decides when to offload sessions via LRU eviction when slots are needed. Clients can manually offload individual sessions. No bulk "clean" command.

20. **Session priority affects eviction order.** Each session has a numeric priority (default 0, range unbounded — negative allowed). LRU eviction prefers lower-priority sessions first, then oldest within the same priority. Priority is set per-session, not per-message. Does not affect processing speed or queue order.

21. **Pool config survives destroy.** `destroy` kills all sessions and the daemon exits, but the pool directory, config.json, and pools.json registry entry persist. The pool can be re-initialized with `init`. Only manually deleting the pool directory truly removes it.

---

## 🟢 Implementation Details (flexible, change freely)

22. Written in Go. Single static binary, no runtime dependencies. PTY via `creack/pty`, sockets and JSON via stdlib.
23. Newline-delimited JSON protocol over Unix sockets.
24. Reconciliation loop runs every 30 seconds.
25. Socket permissions are `0600` (owner-only) — only the Unix user who owns the socket file can connect. Other users on the same machine cannot access the pool.
26. Offloaded sessions stored as `snapshot.log` + `meta.json`.
27. Default flags: `--dangerously-skip-permissions`.
28. Typing detection: polls terminal buffer for un-submitted input text (reference: Open Cockpit `session-discovery.js` terminal input polling with consecutive-miss threshold).

---

## Session States (API-visible)

| State | Meaning |
|-------|---------|
| `queued` | Request received, waiting for a slot |
| `idle` | Finished processing, waiting for input |
| `typing` | Un-submitted input detected in terminal buffer |
| `processing` | Claude is working on a response |
| `offloaded` | Snapshot saved, slot freed |
| `dead` | Process died unexpectedly |
| `error` | Slot error (crash during startup, etc.) |

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
      snapshot.log
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
