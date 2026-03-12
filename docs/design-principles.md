# Design Principles

Rules that govern all design decisions. Tiered by importance — higher tiers override lower tiers.

---

## 🔴 Invariants (never broken)

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, manages its own PTY daemon, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **The socket is the only interface.** All clients (CLI, Python, Open Cockpit, custom tools) interact with the pool exclusively through the socket API. No in-process imports, no shared memory, no backdoors. This guarantees that any client can be swapped, restarted, or crash without affecting the pool.

3. **Sessions survive daemon restarts.** The PTY daemon is a separate process. Restarting the pool daemon does not kill running sessions. State is recoverable from disk.

---

## 🟡 Design Decisions (strong defaults, debatable with good reason)

4. **Each pool can run its own code version.** The `--pool` flag selects which pool to interact with. Different pools may run different versions of claude-pool. This enables safe testing of new versions alongside a stable production pool.

5. **CLI commands default to the `default` pool.** If `--pool` is not specified, all commands target `~/.claude-pool/pools/default/`. Users who only need one pool never have to think about pool names.

6. **Hooks use environment variables for pool routing.** The pool daemon sets `CLAUDE_POOL_HOME` (and similar env vars) when spawning sessions. Hook scripts read these variables to determine which pool directory to write to. This keeps hooks pool-aware without hardcoding paths.

7. **Sugar operations live in the daemon.** High-level operations (start, followup, wait) that coordinate multiple internal steps (slot claiming, LRU eviction, offload/restore) are handled server-side. Clients send one request, get one response.

8. **Write locking prevents races.** All pool state mutations go through an async mutex. Multiple concurrent clients cannot corrupt pool.json.

---

## 🟢 Implementation Details (flexible, change freely)

9. Written in Node.js (because existing code is Node.js and PTY ecosystem is mature here).
10. Newline-delimited JSON protocol over Unix sockets.
11. Reconciliation loop runs every 30 seconds.
12. Socket permissions are `0600` (owner-only).
13. Offloaded sessions stored as `snapshot.log` + `meta.json`.

---

## Pool Directory Structure (consequence of invariant #1)

Each pool is fully self-contained:

```
~/.claude-pool/pools/<name>/
  pool.json              # Pool state
  api.sock               # Pool daemon socket
  pty-daemon.sock        # PTY daemon socket (owned by this pool)
  pty-daemon.pid         # PTY daemon PID
  offloaded/             # Offloaded sessions (per-pool, not shared)
    <sessionId>/
      snapshot.log
      meta.json
  session-pids/          # PID → session mapping
  idle-signals/          # Session idle signal files
  intentions/            # Session intention files
  daemon.pid             # Pool daemon PID
  daemon.log             # Pool daemon log
```

Nothing lives outside the pool directory. Deleting `~/.claude-pool/pools/foo/` completely removes that pool.
