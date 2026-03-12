# Design Principles

Rules that govern all design decisions. Tiered by importance — higher tiers override lower tiers.

**This is not a 1:1 copy of Open Cockpit's pool logic.** We're taking lessons learned from Open Cockpit and designing a cleaner system from the requirements up. Open Cockpit code is a reference, not a blueprint.

---

## 🔴 Invariants (never broken)

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **Clients only interact through the socket.** All clients (CLI, Python, Open Cockpit, custom tools) talk to a pool exclusively through its socket API. No client should ever directly read pool.json, write to idle-signals/, or import pool internals. The socket is the boundary — inside is the pool's business, outside is the client's business.

3. **Each pool has its own logs.** All logging for a pool goes into that pool's directory. When debugging, you look at one pool's logs — never need to grep through shared log files or correlate across pools.

4. **Pools are uniform.** All sessions in a pool run with the same flags and configuration. If you need different flags, create a different pool. No mixed configurations within a pool.

5. **Sessions are addressed by Claude UUID only.** No slot indices, no internal IDs, no terminal IDs in the API. The Claude session UUID is the only identifier clients use. The pool manages slots internally — clients never see or think about slots.

---

## 🟡 Design Decisions (strong defaults, debatable with good reason)

6. **One daemon per pool.** Each pool runs a single daemon process that owns PTYs directly (via `node-pty` in-process). On restart, the daemon re-adopts orphaned PTY processes by checking PIDs from pool.json.

7. **Each pool can run its own code version.** Different pools may run different versions of claude-pool. This enables safe testing of new versions alongside a stable production pool.

8. **CLI is a separate package.** The CLI is a thin router that resolves pool names to socket connections (local or remote). It ships independently from the daemon. This keeps the daemon minimal and enables remote pool access without installing the full daemon.

9. **Pool registry for multi-pool access.** A shared registry (`~/.claude-pool/pools.json`) maps pool names to socket paths (local) or connection strings (remote). CLI and Open Cockpit both read this registry — no duplicated routing logic.

10. **CLI commands default to the `default` pool.** If `--pool` is not specified, all commands target the `default` entry in the registry.

11. **Hooks use environment variables for pool routing.** The pool daemon sets `CLAUDE_POOL_HOME` (and similar env vars) when spawning sessions. Hook scripts read these variables to determine which pool directory to write to.

12. **Sugar operations live in the daemon.** High-level operations (start, followup, wait) that coordinate multiple internal steps (slot claiming, LRU eviction, offload/restore) are handled server-side. Clients send one request, get one response.

13. **Write locking prevents races.** All pool state mutations go through an async mutex. Multiple concurrent clients cannot corrupt pool.json.

14. **Pool config drives spawning.** Each pool has a `config.json` with flags, size, etc. `pool init` and `pool resize` read flags from config. `pool config set` updates the config — affects future spawns only, never running sessions.

15. **Automatic slot management only.** The pool decides when to offload/restore sessions (LRU eviction when slots are needed). There is no manual "clean all" command. Clients can offload individual sessions explicitly if needed.

16. **Attach is session-level.** Terminal attachment targets a session ID, not a slot. The daemon provides a raw pipe (separate socket) for live terminal I/O. If the session is offloaded, the pipe closes automatically. Clients reconnect after resume if needed.

---

## 🟢 Implementation Details (flexible, change freely)

17. Written in Node.js (because existing code is Node.js and PTY ecosystem is mature here).
18. Newline-delimited JSON protocol over Unix sockets.
19. Reconciliation loop runs every 30 seconds.
20. Socket permissions are `0600` (owner-only).
21. Offloaded sessions stored as `snapshot.log` + `meta.json`.
22. Default flags: `--dangerously-skip-permissions`.

---

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
  pool.json              # Pool state (sessions, slots)
  api.sock               # Daemon socket
  daemon.pid             # Daemon PID
  logs/                  # All pool logs
    daemon.log           # Daemon output, lifecycle events
    error.log            # Errors and crashes
    api.log              # API requests/responses (optional, for debugging)
  offloaded/             # Offloaded sessions
    <claudeUUID>/
      snapshot.log
      meta.json
  session-pids/          # PID → Claude UUID mapping
  idle-signals/          # Session idle signal files
```

Global registry (not per-pool):

```
~/.claude-pool/
  pools.json             # Pool name → socket path/connection string
  pools/
    default/             # Default pool
    work/                # Named pool
    ...
```

Nothing lives outside the pool directory except the registry. Deleting `~/.claude-pool/pools/foo/` completely removes that pool with zero side effects.
