# Spec

> ⛔ **Protected.** No changes without explicit user permission. Flag contradictions — don't silently work around them.

## Invariants

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **Clients only interact through the socket.** All clients (CLI, Python, Open Cockpit, custom tools) talk to a pool exclusively through its socket API. No client should ever directly read pool.json, write to idle-signals/, or import pool internals. The socket is the boundary — inside is the pool's business, outside is the client's business.

3. **Each pool has its own logs.** All logging for a pool goes into that pool's directory. When debugging, you look at one pool's logs — never need to grep through shared log files or correlate across pools. Log everything by default (state changes, API requests, errors, lifecycle events). Retain logs for at least 24 hours.

4. **Pools are uniform.** All sessions in a pool run with the same flags and configuration. If you need different flags, create a different pool. No mixed configurations within a pool.

5. **Internal session IDs are the primary identifier.** Each session gets a pool-assigned internal ID (short random string) at request time — before a slot is allocated, before Claude starts. This ID is stable across the session's entire lifecycle: queued → live → offloaded → resumed. The Claude UUID is discovered later and mapped 1:1. Both are queryable, but the internal ID is what clients use.

6. **Sessions have owners.** Every session tracks its parent (the session or external caller that spawned it). By default, API queries return only sessions owned by the caller (direct children + their descendants). An explicit flag (`all: true`) shows all pool sessions. This prevents sessions from interfering with each other's sub-agents.

7. **The consumer API is session-oriented.** Slots are an internal implementation detail. No consumer API command exposes slot indices, slot states, or slot-level operations. Clients think in sessions — the pool manages slots transparently.

---

## Model

A **session** is a logical unit of work — it has an ID, an owner, prompt history, and a lifecycle (queued → active → offloaded → archived). Sessions survive being unloaded and reloaded.

A **slot** is a physical resource — a running Claude Code process with a PTY. Slots host sessions. When a slot is needed elsewhere, the session is offloaded and the slot is reused.

### Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a slot |
| `idle` | Finished processing, waiting for input |
| `typing` | Un-submitted input detected in a fresh slot's terminal buffer |
| `processing` | Claude is working |
| `offloaded` | Not in a slot, can be resumed |
| `error` | Repeatedly failed to load (broken session) |
| `archived` | Done. Hidden from `ls` by default. Auto-cleaned after 30 days. |

Offloaded sessions can become `queued` again when targeted by `followup` or `pin`.

When a session's process dies, the session transitions to `offloaded` (not a separate state — the JSONL transcript still exists). The error is logged. On next `followup` or `pin`, the session is loaded normally.

When a session fails to load, the error is logged and loading is retried automatically. After repeated failures (implementation decides the threshold), the session is marked `error`. Error sessions are visible but cannot be loaded without explicit action (`followup` with `force: true` resets the retry counter and attempts loading again).

### Output Formats

Commands that return session output (`wait`, `capture`) accept a `format` field:

| Format | Description | Requires live terminal? |
|--------|-------------|------------------------|
| `jsonl-last` | Last assistant message only. | No |
| `jsonl-short` | All assistant messages since last user message. **Default.** | No |
| `jsonl-long` | Full JSONL since last user message, repetitive fields stripped. | No |
| `jsonl-full` | Complete JSONL transcript, unfiltered. | No |
| `buffer-last` | Terminal buffer since last user message. | Yes |
| `buffer-full` | Full terminal scrollback, ANSI stripped. | Yes |

JSONL formats read from Claude Code's transcript files (via Claude UUID). Work for any session with a known UUID — including offloaded and archived. Buffer formats require a live terminal (idle, typing, processing only).

Empty content is valid — if a session was stopped before producing output, JSONL formats might return an empty string.

---

## Consumer API

Transport: Unix domain socket, newline-delimited JSON. See [docs/protocol.md](docs/protocol.md) for full field-level details.

### Pool Management

- **`ping`** — Health check. Returns immediately.
- **`init`** — Initialize the pool. Reads flags from `config.json`. Restores previously live sessions by default (skip with `noRestore`). Errors if already initialized.
- **`resize`** — Change slot count. Growing spawns new slots. Shrinking uses kill tokens — processing sessions finish naturally, pinned sessions are never evicted, queued requests are never dropped.
- **`health`** — Pool health report. Shows all sessions regardless of ownership.
- **`destroy`** — Kill all sessions, daemon exits. Pool directory and config persist — can re-init later. Requires `confirm: true`.
- **`config`** — Read or update `config.json`. Changes affect future spawns only.

### Session Lifecycle

- **`start`** — Send a prompt to a new session. Returns an internal ID immediately. Claims a fresh slot if available, evicts an idle session if needed (see Eviction Policy), or queues the request FIFO.
- **`followup`** — Send a follow-up prompt to an existing session. Auto-resumes offloaded sessions (queues for loading). Errors on busy sessions unless `force: true`.
- **`stop`** — Interrupt or cancel. **Synchronous** — session is guaranteed idle (or removed) when `ok` returns. Cancels queued requests, sends Ctrl+C to processing sessions.
- **`offload`** — Manually free a session's slot. Only works on idle sessions. Unpins if pinned.
- **`archive`** — Mark a session as done. Stops live sessions first. Errors if unarchived children exist unless `recursive: true`. Auto-cleaned after 30 days.
- **`unarchive`** — Restore an archived session to offloaded state.

### Session Information

- **`ls`** — List sessions. Default: owned sessions only. `all: true` for everything. `tree: true` for nested descendants. `archived: true` to include archived.
- **`info`** — Full session details including Claude UUID, cwd, priority, pin status, and recursive children tree.
- **`wait`** — Long-poll until a session becomes idle. Returns session output. Without a sessionId, waits for any owned busy session.
- **`capture`** — Return session output immediately, regardless of state. JSONL formats work for any session with a UUID; buffer formats require a live terminal.

### Session Control

- **`pin`** — Prevent LRU eviction for a duration (default 120s). Without a sessionId, allocates a fresh session. Offloaded sessions get priority loading (jump to front of queue).
- **`unpin`** — Remove pin, make session eligible for eviction again.
- **`set-priority`** — Set eviction priority (default 0, lower = evicted first). Does NOT affect queue order or processing speed.
- **`input`** — Send raw bytes to a session's PTY. No timing dance — use `followup` for safe prompt delivery.
- **`attach`** — Get a temporary Unix socket for raw PTY I/O (live terminal streaming). Only works for live sessions. Multiple clients can attach simultaneously. Attaching does not affect other operations — all API commands continue to work normally on attached sessions.

### Events

- **`subscribe`** — Open a persistent event stream. Filterable by session, event type, status transition, or property change. Re-subscribing on the same connection replaces filters.

---

## CLI

The CLI is a separate package — a thin router that resolves pool names to socket connections. Each API command maps 1:1 to a CLI subcommand.

- `--pool <name>` selects the pool (default: `default`).
- Commands that send a prompt (`start`, `followup`) accept `--block`, which sends the command then automatically waits and prints the output.

---

## Internals

### Eviction Policy

When a slot is needed and none are free:

1. Use a `fresh` slot first (no session to displace).
2. If no fresh slots, offload the lowest-priority idle session. Within the same priority, offload the session that has been idle the longest (LRU).
3. Pinned sessions are never evicted. If all idle sessions are pinned, the request queues until a slot frees up naturally.
4. Processing sessions are never interrupted for eviction — they finish naturally.

### Slot States

Slots are the physical resources that host sessions. Consumers never interact with slots directly (invariant #7).

| State | Meaning |
|-------|---------|
| `fresh` | Pre-warmed Claude process, never prompted. Ready for immediate use. |
| `loading` | Starting a new session or resuming an offloaded one. |
| `live` | Hosting an active session (idle, typing, or processing). |
| `error` | Crashed during startup or loading. Recycled automatically (killed, replaced with fresh). |

### Debug API

Separate from the consumer API. Provides direct access to internal pool state for debugging:

- Attach to specific slots (by index) for raw PTY access.
- View slot states and slot↔session mappings.
