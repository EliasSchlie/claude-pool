# Design Principles

Rules that govern all design decisions. Tiered by importance — higher tiers override lower tiers.

---

## 🔴 Invariants (never broken)

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **Clients only interact through the socket.** All clients (CLI, Python, Open Cockpit, custom tools) talk to a pool exclusively through its socket API. No client should ever directly read pool.json, write to idle-signals/, or import pool internals. The socket is the boundary — inside is the pool's business, outside is the client's business.

3. **Each pool has its own logs.** All logging for a pool goes into that pool's directory. When debugging, you look at one pool's logs — never need to grep through shared log files or correlate across pools.

---

## 🟡 Design Decisions (strong defaults, debatable with good reason)

4. **One daemon per pool.** Each pool runs a single daemon process that owns PTYs directly (via `node-pty` in-process). No separate PTY daemon. On restart, the daemon re-adopts orphaned PTY processes by checking PIDs from pool.json.

5. **Each pool can run its own code version.** The `--pool` flag selects which pool to interact with. Different pools may run different versions of claude-pool. This enables safe testing of new versions alongside a stable production pool.

6. **CLI commands default to the `default` pool.** If `--pool` is not specified, all commands target `~/.claude-pool/pools/default/`. Users who only need one pool never have to think about pool names.

7. **Hooks use environment variables for pool routing.** The pool daemon sets `CLAUDE_POOL_HOME` (and similar env vars) when spawning sessions. Hook scripts read these variables to determine which pool directory to write to. This keeps hooks pool-aware without hardcoding paths.

8. **Sugar operations live in the daemon.** High-level operations (start, followup, wait) that coordinate multiple internal steps (slot claiming, LRU eviction, offload/restore) are handled server-side. Clients send one request, get one response.

9. **Write locking prevents races.** All pool state mutations go through an async mutex. Multiple concurrent clients cannot corrupt pool.json.

---

## 🟢 Implementation Details (flexible, change freely)

10. Written in Node.js (because existing code is Node.js and PTY ecosystem is mature here).
11. Newline-delimited JSON protocol over Unix sockets.
12. Reconciliation loop runs every 30 seconds.
13. Socket permissions are `0600` (owner-only).
14. Offloaded sessions stored as `snapshot.log` + `meta.json`.

---

## Pool Directory Structure (consequence of invariant #1)

Each pool is fully self-contained:

```
~/.claude-pool/pools/<name>/
  pool.json              # Pool state
  api.sock               # Daemon socket
  daemon.pid             # Daemon PID
  logs/                  # All pool logs
    daemon.log           # Daemon output, lifecycle events
    error.log            # Errors and crashes
    api.log              # API requests/responses (optional, for debugging)
  offloaded/             # Offloaded sessions
    <sessionId>/
      snapshot.log
      meta.json
  session-pids/          # PID → session mapping
  idle-signals/          # Session idle signal files
  intentions/            # Session intention files
```

Nothing lives outside the pool directory. Deleting `~/.claude-pool/pools/foo/` completely removes that pool with zero side effects on anything else.
