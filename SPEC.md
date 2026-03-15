# Spec

> ⛔ **Protected.** Do not edit without explicit user permission. If you believe the spec needs a change, propose it to the user first. Implementation details not covered here are free to change — but anything that contradicts the spec must be flagged.

## Invariants

These are non-negotiable. Code that violates an invariant is a bug.

1. **Pool isolation is absolute.** Each pool runs its own daemon process, owns its own directory, and shares zero state with other pools. If one pool crashes, panics, corrupts its data, or runs buggy code — other pools are completely unaffected. No shared sockets, no shared files, no shared processes.

2. **Clients only interact through the socket.** All clients (CLI, Python, Open Cockpit, custom tools) talk to a pool exclusively through its socket API. No client should ever directly read pool.json, write to idle-signals/, or import pool internals. The socket is the boundary — inside is the pool's business, outside is the client's business.

3. **Pools are uniform.** All sessions in a pool run with the same flags and configuration. If you need different flags, create a different pool. No mixed configurations within a pool.

4. **Internal session IDs are the primary identifier.** Each session gets a pool-assigned internal ID (short random string) at request time — before a slot is allocated, before Claude starts. This ID is stable across the session's entire lifecycle: queued → live → offloaded → resumed. The Claude UUID is discovered later and mapped 1:1. Both are queryable, but the internal ID is what clients use.

5. **The consumer API is session-oriented.** Slots are an internal implementation detail. No consumer API command exposes slot indices, slot states, or slot-level operations. Clients think in sessions — the pool manages slots transparently.

---

## Model

A **session** is a logical unit of work — it has an ID, an owner, prompt history, and a lifecycle (queued → loaded/offloaded → archived). Sessions survive being unloaded and reloaded.

A **slot** is a physical resource — a running Claude Code process with a PTY. Slots host sessions. When a slot is needed elsewhere, the session is offloaded and the slot is reused.

### Session Object

Verbosity levels: `flat` (default), `nested`, `full`. Applied recursively to children.

| Field | Type | flat | nested | full | Description |
|-------|------|------|--------|------|-------------|
| `sessionId` | string | ✓ | ✓ | ✓ | Pool-assigned internal ID. Stable across the session's entire lifecycle. |
| `status` | string | ✓ | ✓ | ✓ | Current state (see below). |
| `priority` | number | ✓* | ✓* | ✓ | Eviction priority (default 0, lower = evicted first). |
| `pinned` | boolean | ✓* | ✓* | ✓ | Protected from LRU eviction (time-limited). |
| `pendingInput` | string | ✓* | ✓* | ✓ | Un-submitted text in terminal buffer. Only populated for loaded sessions. |
| `children` | array | | ✓ | ✓ | Child session objects, recursive. Same verbosity applied to each child. |
| `parent` | string | | | ✓ | Identifies who spawned this session. Any string — defaults to caller's Claude Code UUID when auto-detected. |
| `cwd` | string | | | ✓ | Current working directory. Updates as the session navigates. |
| `claudeUUID` | string or null | | | ✓ | Claude Code's session UUID. Null if not yet discovered. |
| `spawnCwd` | string | | | ✓ | Directory the session was originally started in. Never changes. |
| `createdAt` | string | | | ✓ | ISO 8601 timestamp. |
| `pid` | number or null | | | ✓ | OS process ID. Null if not loaded. |
| `metadata` | object | | | ✓ | Arbitrary key-value pairs. Set via config or at session creation. |

✓* = only shown if non-default

### Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a slot |
| `idle` | Finished processing, waiting for input |
| `processing` | Claude is working |
| `offloaded` | Not in a slot, can be resumed |
| `error` | Repeatedly failed to load (broken session) |
| `archived` | Done. Hidden from `ls` by default. Auto-cleaned after 30 days. |

Offloaded sessions can become `queued` again when targeted by `followup` or `pin`.

When a session's process dies, the session transitions to `offloaded` (not a separate state — the JSONL transcript still exists). The error is logged. On next `followup` or `pin`, the session is loaded normally.

When a session fails to load, the error is logged and loading is retried automatically. After repeated failures (implementation decides the threshold), the session is marked `error`. Error sessions are visible but cannot be loaded without explicit action (unarchive or implementation-specific reset).

### Output Capture

Commands that return session output (`wait`, `capture`, `start --block`, `followup --block`) accept three optional parameters.

**`source`** — where to read from

| Value | Description |
|-------|-------------|
| `jsonl` (default) | Claude Code's JSONL transcript. Works for any session with a known UUID, including offloaded and archived. Errors if session has no UUID (e.g. queued from scratch, never loaded). |
| `buffer` | Raw terminal scrollback, ANSI stripped. Requires a live session, errors otherwise. |

**`turns`** — how far back to look

| Value | Description |
|-------|-------------|
| `1` (default) | Last turn only. |
| `N` | Last N turns. |
| `0` | Entire history. |

A **turn** is one user prompt and everything that follows until the next user prompt.

**`detail`** — what to include per turn (`jsonl` source only, ignored for `buffer`)

| Value | Description |
|-------|-------------|
| `last` (default) | User prompt + final assistant response. No tool calls. |
| `assistant` | User prompt + all assistant text responses. No tool calls. |
| `tools` | User prompt + assistant responses + tool calls/results. No internal metadata. |
| `raw` | Everything unfiltered. |

If the session is still processing or was stopped early, there might not be any assistant message or tool messages, which could make it only return the user prompt.

---

## CLI

The CLI must be a thin wrapper over the socket API to make it easy to create other types of clients that interact with pools (e.g. python package,...) later.

These flags are available on every command:
- `--pool <name>` — Target pool (default: `default`).
- `--json` — Machine-readable JSON output.

### Interaction

**start** — Start a new session.
  `--prompt <text>` (required) — The prompt to send.
  `--parent <string>` — Identifies who spawned this session. Any string. If omitted and the caller is a Claude Code instance, defaults to that session's Claude Code UUID. If omitted from a regular terminal, the session has no parent. Use `--parent none` to explicitly create a session with no parent (disables auto-detection).
  `--block` — Wait for completion and print output. Accepts output flags: `--source`, `--turns`, `--detail` (see Output Capture).
  → `sessionId`: string — Pool-assigned session ID.
  → `status`: session state — Current state (see Session States).
  With `--block`, additionally:
  → `content`: string — Session output (see `wait`).

**followup** — Send a prompt to an existing session. Errors if session is busy (stop first), queued (stop first), or archived (unarchive first).
  `--session <id>` (required) — Target session.
  `--prompt <text>` (required) — The prompt to send.
  `--block` — See `start`.
  → `sessionId`, `status` — See `start`.
  With `--block`, additionally:
  → `content` — See `start`.

**wait** — Wait for a session to become idle, then return its output.
  `--session <id>` — Wait for this specific session. Overrides `--parent` if both set.
  `--parent <string>` — Wait for any busy session with this parent. See `start` for default behavior. Without `--session` or `--parent` (and no auto-detection), waits for any busy session with no parent.
  `--timeout <ms>` — Timeout in milliseconds (default: 300000).
  `--source`, `--turns`, `--detail` — See Output Capture.
  → `sessionId`: string — The session that became idle.
  → `content`: string — Session output (format depends on `--source` and `--detail`).

**capture** — Get session output immediately, regardless of state.
  `--session <id>` (required) — Target session.
  `--source`, `--turns`, `--detail` — See Output Capture.
  → `sessionId`, `content` — See `wait`.

**stop** — Cancel a queued request or interrupt a processing session. Errors if session is not processing or queued.
  `--session <id>` (required) — Target session.

**ls** — Only filters the top level — if a session appears as a child of another session, it's not repeated as a separate entry.
  `--parent <string>` — Filter top-level sessions by parent. See `start` for auto-detection. Without `--parent` and without auto-detection (non-Claude caller), shows all sessions. Use `--parent none` from a Claude session to show all.
  `--status <states>` — Filter by status (comma-separated, e.g. `idle,processing`).
  `--archived` — Include archived sessions. (excluded by default)
  `--verbosity flat|nested|full` — See Session Object. (default: `flat`)
  → `sessions`: array — List of session objects.

**info** — Get all details about a session.
  `--session <id>` (required) — Target session.
  `--verbosity flat|nested|full` — See Session Object. (default: `full`)
  → `session`: object — Session object.

### Lifecycle

**archive** — Mark session as done. Errors if session has unarchived children (use `--recursive`).
  `--session <id>` (required) — Target session.
  `--recursive` — Archive all descendants too.

**unarchive** — Restore archived session. Errors if session is not archived.
  `--session <id>` (required) — Target session.

**set** — Set session properties.
  `--session <id>` (required) — Target session.
  `--priority <number>` — Eviction priority (lower = evicted first, default: 0).
  `--pinned <seconds>` — Pin for this duration. Use `--pinned false` to unpin.
  `--meta <key>=<value>` — Set metadata. Repeatable for multiple keys.

### Pool

`pools` and `init` (when creating a new pool) operate directly on the filesystem and daemon processes — they are CLI-only and don't go through the socket API.

**init** — Initialize pool. Creates the pool if it doesn't exist, starts the daemon, and registers in `~/.claude-pool/pools.json`. If the pool already exists, uses existing config (flags override if provided). Errors if the pool is already running.
  `--size <n>` — Slot count. Falls back to config if omitted.
  `--flags <string>` — Claude CLI flags for all sessions. Updates config if provided.
  `--dir <path>` — Pool home directory (default: `~`).
  `--no-restore` — Skip restoring previous sessions.
  → Pool state after initialization (same as `health`).

**health** — Pool status.
  → `health`: object — Slot count, session states, queue depth.

**resize** — Change slot count immediately and update config.
  `--size <n>` (required) — New slot count.

**config** — Read or update pool config. Default keys: `flags` (string, Claude CLI flags), `size` (integer, default slot count). Arbitrary key-value pairs can also be stored for session metadata.
  `--set <key>=<value>` — Update a config field. Omit to read.
  → `config`: object — Current config after any updates.

**destroy** — Kill all sessions, daemon exits. Returns success before the daemon shuts down. Pool directory, config, and registry entry persist — run `init` to restart.
  `--confirm` (required) — Safety guard.

**ping** — Health check.

**pools** — List all known pools from the registry (`~/.claude-pool/pools.json`). Shows status (running/stopped) by checking if the daemon is reachable. Pool data persists after `destroy` — run `init` to restart.
  → List of pool names, status, and config.

### Debug

Debug commands live under `debug <command>`.

**debug input** — Send raw bytes to a session's PTY. For sending keystrokes like Enter, Ctrl+C, or arbitrary text. Use `followup` for safe prompt delivery.
  `--session <id>` (required) — Target session.
  `--data <bytes>` (required) — Raw bytes to send.

**debug capture** — Capture raw terminal buffer from a slot. For inspecting what's happening in a slot regardless of session state or mapping.
  `--slot <index>` (required) — Target slot.
  `--raw` — Include ANSI escape codes (stripped by default).

**debug slots** — Show slot states and slot↔session mappings.

**debug logs** — Tail the pool's daemon log.
  `--lines <n>` — Number of lines to show (default: 50).
  `--follow` — Stream new log entries.

## Skill

The Claude Code skill (`claude-pool`) explains the CLI use in two levels:

**Main skill** — covers the commands Claude sessions use by default:
- `start`, `followup`, `wait`, `capture`, `stop`
- `ls`, `info`
- `archive`, `unarchive`
- `init`, `health`, `resize`, `config`, `destroy`, `ping`, `pools`

**Debug sub-skill** — covers everything else, for troubleshooting and advanced use:
- `set` (priority, pinned, metadata)
- All `debug` commands (`input`, `capture`, `slots`, `logs`)

The main skill should not mention `set` or debug commands.

---

## UI-specific API features

These are API-only — not exposed in the CLI. Needed by user interfaces (e.g. Open Cockpit) that render live terminal output.

**attach** — Get a temporary Unix socket for raw PTY I/O. Connect to it for live terminal streaming: bytes written = keystrokes, bytes read = terminal output. Multiple clients can attach simultaneously. The pipe closes when the session is offloaded or dies. Only works on live sessions (idle, processing). All other API commands continue to work normally on attached sessions.

**subscribe** — Open a persistent event stream on the socket connection. Filterable by session, event type, status transition, or property change (including `pendingInput`). Re-subscribing on the same connection replaces filters.

---

## Internals

### Eviction Policy

When a slot is needed and none are free:

1. Use a `fresh` slot first (no session to displace).
2. If no fresh slots, offload the lowest-priority idle session. Within the same priority, offload the session that has been idle the longest (LRU). Changes in `pendingInput` reset the LRU timestamp for that session (counts as recent use).
3. Pinned sessions are never evicted. If all idle sessions are pinned, the request queues until a slot frees up naturally.
4. Processing sessions are never interrupted for eviction — they finish naturally.

### Slot States

Slots are the physical resources that host sessions. Consumers never interact with slots directly (invariant #5).

| State | Meaning |
|-------|---------|
| `fresh` | Pre-warmed Claude process, never prompted. Ready for immediate use. |
| `loading` | Starting a new session or resuming an offloaded one. |
| `live` | Hosting an active session (idle or processing). |
| `error` | Crashed during startup or loading. Recycled automatically (killed, replaced with fresh). |

### Logging

Each pool logs to a single JSONL file (`daemon.log`) in its pool directory — one JSON object per line, appended. Entries older than 30 days are discarded.

Log categories (as a field, not separate files — the timeline matters for debugging):

| Category | What gets logged |
|----------|-----------------|
| `api` | Every request and response |
| `state` | Session state transitions (queued → processing → idle, etc.) |
| `slot` | Slot lifecycle: spawn, clear, resume, kill, error |
| `eviction` | Eviction decisions: which session, why, what triggered it |
| `error` | All errors |

Every log entry includes: timestamp, category, session ID (if applicable), and a human-readable message.

### Session Lifecycle Mechanics

All Claude sessions are persistent and headful — the pool never spawns throwaway processes. When a slot needs to host a new session, the pool sends `/clear` to reset the existing Claude process's context. When resuming an offloaded session, the pool sends `/resume <uuid>` to restore it into the cleared slot. This reuse model exists because spawning a new Claude CLI process kills bash command output for all other running Claude processes on the same machine.

