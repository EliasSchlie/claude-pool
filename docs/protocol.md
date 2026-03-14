# Socket Protocol

> **Note:** This is a detailed reference for implementers. The ground truth for what the API exposes and why is [SPEC.md](../SPEC.md). If this document contradicts the spec, the spec wins.

## Transport

Unix domain socket at `~/.claude-pool/pools/<name>/api.sock`. Newline-delimited JSON. Each message is one JSON object + `\n`. Requests may include `id` — response echoes it back.

```
-> {"type":"ping","id":1}\n
<- {"type":"pong","id":1}\n
```

## Errors

All errors: `{ type: "error", error: "human-readable message", id: <echoed> }`.

## Session Identity

Sessions are addressed by **internal IDs** — short random strings (like `a7f2x9`) assigned at request time. The Claude UUID is discovered later and mapped 1:1. Use `info` to look up the Claude UUID for a given internal ID.

## Ownership

Sessions track their parent. Auto-detected via `CLAUDE_POOL_SESSION_ID` env var, or set via `parentId`. `ls` returns only owned sessions by default. `all: true` shows everything.

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a slot. Sessions enter this state from `start` (new session), or from `followup`/`pin` on an offloaded session that needs to be loaded. |
| `idle` | Finished processing, waiting for input |
| `processing` | Claude is working |
| `offloaded` | Not in a slot, can be resumed. Also the state after a process dies (error is logged, session becomes offloaded). |
| `error` | Repeatedly failed to load. Visible but cannot be loaded without explicit action. |
| `archived` | Session is done. Hidden from `ls` by default. Auto-cleaned after 30 days. |

**Important:** Offloaded sessions can become `queued` again. When `followup` or `pin` targets an offloaded session, the session transitions to `queued` while waiting for a slot to become available.

---

## Output Capture

Commands that return session output (`wait`, `capture`) accept three optional parameters:

### `source` — where to read from

| Value | Description | Requires live terminal? |
|-------|-------------|------------------------|
| `"jsonl"` (default) | Claude Code's JSONL transcript (via Claude UUID). Works for any session state where a UUID has been discovered — including offloaded, error, and archived sessions. | No |
| `"buffer"` | Raw terminal scrollback, ANSI stripped. Only works for live sessions (idle, processing). Errors for queued, offloaded, error, and archived sessions. | Yes |

### `turns` — how far back to look

Integer. Default: `1`.

- `1` — last turn only. (default)
- `N` — last N turns.
- `0` — entire history.

A **turn** is one user message and everything that follows until the next user message (assistant responses, tool calls, tool results). For buffer source, turn boundaries are detected from the JSONL transcript — `turns: 1` returns terminal output since the last user message was sent.

### `detail` — what to include per turn (JSONL only)

In Claude Code's JSONL transcripts, tool use and tool results are not separate entry types — `tool_use` blocks appear inside `type: "assistant"` entries, and `tool_result` blocks appear inside `type: "user"` entries. The `detail` parameter filters at both the entry level (which entries to include) and the content-block level (which blocks within an entry to keep).

| Value | Entries included | Content filtering |
|-------|-----------------|-------------------|
| `"last"` (default) | User prompts + final assistant entry with text, per turn. | Strip tool_use blocks. Exclude tool_result user entries. |
| `"assistant"` | User prompts + all assistant entries that contain text. | Strip tool_use blocks. Exclude tool_result user entries. |
| `"tools"` | All user entries (prompts + tool results) + all assistant entries. | Keep everything. Strip metadata (model, usage, timestamps). |
| `"raw"` | All entries unfiltered (including progress, system, etc.). | No filtering. |

For buffer source, `detail` is ignored — buffer output is always raw terminal text.

### Output format

For JSONL source, the `content` field is always JSONL (one JSON object per line), regardless of `detail` level. The `detail` parameter controls which entries are included and how they are filtered, not the output format.

Example — `source: "jsonl", turns: 2, detail: "last"`:
```jsonl
{"type":"user","content":"What is 2+2?"}
{"type":"assistant","content":"4"}
{"type":"user","content":"What is 3+3?"}
{"type":"assistant","content":"6"}
```

The same request with `detail: "tools"` would include all entries from those turns — including assistant entries with `tool_use` content blocks and user entries carrying `tool_result` blocks.

With `detail: "raw"`, entries are the original unmodified JSONL lines from Claude Code's transcript (including `progress`, `system`, `file-history-snapshot` entries, and all metadata fields like `model`, `usage`, `parentUuid`, `timestamp`, etc.).

For buffer source, `content` is plain text (the raw terminal output for the requested turns, ANSI stripped).

### Empty content

If a session was interrupted (`stop`) before Claude produced any assistant output, or if there is no assistant message in the requested turns, capture might return an empty string. This is not an error — it reflects that no output was produced. Callers should handle empty content gracefully.

---

## Commands

### `ping`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| — | — | — | No fields |

**Response:** `{ type: "pong" }`

**Behavior:** Health check. Returns immediately. No side effects.

---

### `init`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `size` | integer ≥ 1 | No | Number of slots. Falls back to `config.json` if omitted. |
| `noRestore` | boolean | No | Skip restoring previously live sessions (default false). |

**Response:** `{ type: "pool", pool }` — full pool state after init.

**Behavior:** Initializes the pool daemon. Reads flags from `config.json`. Errors if pool already initialized (sessions are running). Updates `pools.json` registry.

If previous session state exists (from a prior run that was destroyed or crashed), `init` restores sessions that were **live** (idle or processing) when the pool last shut down. These sessions are loaded via `/resume` into available slots. Sessions that were already offloaded, error, or archived remain in their prior state. If there are more sessions to restore than `size` slots, excess sessions stay offloaded and can be loaded later via `followup` or `pin`.

If `noRestore: true`, previous session state is ignored — all slots are filled with fresh pre-warmed sessions instead.

If no previous state exists (first-time init), all slots get fresh pre-warmed sessions regardless of `noRestore`.

---

### `resize`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `size` | integer ≥ 0 | Yes | New slot count |

**Response:** `{ type: "pool", pool }`

**Behavior:** Grows or shrinks the pool. When growing: spawns new slots using flags from `config.json`. When shrinking: enqueues "kill slot" tokens at the front of the internal queue — one per slot to remove. When a slot becomes available (session finishes processing, becomes idle, etc.), a kill token consumes it: the session is offloaded and the slot is permanently removed. This means processing sessions finish naturally rather than being interrupted. Pinned sessions are never evicted by resize — if all remaining sessions are pinned, resize waits until pins expire or sessions are unpinned. Queued requests are never dropped — they stay in the queue behind the kill tokens.

---

### `health`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| — | — | — | No fields |

**Response:** `{ type: "health", health }` — per-session status, counts, PID liveness, queue depth.

**Behavior:** Returns pool health report. Always shows full pool (ignores ownership). Includes: total slots, sessions per state, queue depth, PID liveness checks.

---

### `destroy`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `confirm` | boolean | Yes | Must be `true`. Safety guard against accidental destruction. |

**Response:** `{ type: "ok" }`

**Behavior:** Kills all sessions (including processing ones), removes `pool.json` (runtime state), and the daemon process exits. The pool directory, `config.json`, and `pools.json` registry entry **persist** — the pool can be re-initialized later with `init`. To fully remove a pool, manually delete its directory and registry entry.

---

### `config`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `set` | object | No | Fields to update. Omit to read current config. |

**Response:** `{ type: "config", config }` — current config after any updates.

**Behavior:** Reads or updates `config.json`. Settable fields: `flags` (string, Claude CLI flags), `size` (integer, default pool size). Changes affect future spawns only — running sessions keep their original flags. Reading config has no side effects.

---

### `start`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prompt` | string | Yes | The prompt to send |
| `parentId` | sessionId | No | Explicit parent. Auto-detected from `CLAUDE_POOL_SESSION_ID` env var if omitted. |

**Response:** `{ type: "started", sessionId, status }` — internal ID + initial status.

**Behavior:** Assigns an internal session ID immediately and returns it. Then:
1. If a fresh (pre-warmed) slot is available → claims it, sends the prompt. Status: first visible state (e.g. `processing`).
2. Otherwise → queues the request. Status: `queued`. The queue processor runs asynchronously: if an evictable idle session exists, it offloads the lowest-priority one (LRU within same priority) to free a slot. If no session is evictable (all processing or pinned), the request waits until a slot frees naturally. Queued requests are served FIFO.

The `parentId` is recorded on the session. If the caller is itself a pool session (detected via `CLAUDE_POOL_SESSION_ID` env var), that ID is used as parent automatically.

Priority defaults to 0 for new sessions. Use `set-priority` to change it after creation.

---

### `followup`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `prompt` | string | Yes | The prompt to send |
| `force` | boolean | No | Send even if session is busy/queued (default false) |

**Response:** `{ type: "started", sessionId, status }`

**Behavior:** Sends a follow-up prompt to an existing session.
- If session is **idle** → sends the prompt immediately. Handles the timing dance (Escape, Ctrl-U, type text, poll buffer, Enter).
- If session is **offloaded** → queues the session for loading (transitions to `queued`). Once a slot is available, loads via `/resume <claudeUUID>`, waits for ready, sends the prompt. Session's internal ID stays the same.
- If session is **processing** → errors, unless `force: true` (sends the prompt anyway, useful for interrupt-and-redirect).
- If session is **queued** → errors, unless `force: true` (replaces the pending prompt). Use `stop` to cancel a queued request before sending a new followup.
- If session is **error** → errors, unless `force: true` (resets retry counter, attempts loading again).
- If session is **archived** → errors. Unarchive first.

---

### `wait`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | No | Target session. If omitted, waits for any owned busy session. |
| `timeout` | integer | No | Timeout in ms (default 300000 = 5 min) |
| `source` | string | No | `"jsonl"` (default) or `"buffer"`. See Output Capture. |
| `turns` | integer | No | How many turns back (default 1, 0 = all). See Output Capture. |
| `detail` | string | No | `"last"` (default), `"assistant"`, `"tools"`, or `"raw"`. See Output Capture. |

**Response:** `{ type: "result", sessionId, content }` — session output when it becomes idle.

**Behavior:** Long-polls until the target session reaches `idle` state.
- If session is **queued** → waits through: queue → slot allocation → processing → idle.
- If session is **processing** → waits until idle.
- If session is **idle** → returns immediately with current output.
- If session is **offloaded** → errors (nothing to wait for — use `followup` to resume).
- If no `sessionId` → waits for any owned session that is currently `queued` or `processing` to become idle. Returns the first one that completes. Errors immediately if no owned sessions are busy.
- On timeout → returns `{ type: "error", error: "timeout" }`.
- If session dies while waiting → session transitions to offloaded, returns `{ type: "error", error: "session died" }`.

---

### `capture`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `source` | string | No | `"jsonl"` (default) or `"buffer"`. See Output Capture. |
| `turns` | integer | No | How many turns back (default 1, 0 = all). See Output Capture. |
| `detail` | string | No | `"last"` (default), `"assistant"`, `"tools"`, or `"raw"`. See Output Capture. |

**Response:** `{ type: "result", sessionId, content }` — session output.

**Behavior:** Returns the current session output, regardless of session state.
- **JSONL source** works for any session with a known Claude UUID: **idle**, **processing**, **queued** (if re-queued from offloaded), **offloaded**, **error**, **archived**. Reads from Claude Code's transcript files.
- **Buffer source** only works for live sessions (**idle**, **processing**). Errors for all other states.
- **Queued** sessions: JSONL source works if the session has a UUID (re-queued from offloaded). Buffer source errors (no live terminal). Sessions queued from scratch (never spawned) have no UUID, so all sources error.

---

### `input`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `data` | string | Yes | Raw bytes to send to the terminal |

**Response:** `{ type: "ok" }`

**Behavior:** Sends raw bytes directly to the session's PTY input. No timing dance, no buffer polling — just raw write.
- Works for any session with a live terminal (**idle**, **processing**).
- Errors if session has no live terminal: **queued**, **offloaded**, **error**, **archived**.
- Use this for sending keystrokes like `\r` (Enter), `\x03` (Ctrl+C), `\x1b` (Escape), or arbitrary text.
- Does NOT handle the timing dance. For safe prompt delivery, use `followup` instead.

---

### `offload`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "ok" }`

**Behavior:** Manually offload a session to free its slot. Saves session metadata, frees the slot's resources, and transitions the session to `offloaded`. The JSONL transcript (maintained by Claude Code itself) serves as the persistent record and is always accessible via the session's Claude UUID.

- Only works for **idle** sessions. Errors if **processing**, **queued** (nothing to offload), or already **offloaded**.
- If session is **pinned** → automatically unpinned before offloading.

---

### `ls`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `all` | boolean | No | Show all pool sessions (default false = owned only) |
| `tree` | boolean | No | Show descendants as nested tree (default false = flat list of direct children) |
| `archived` | boolean | No | Include archived sessions (default false = hidden) |

**Response:** `{ type: "sessions", sessions }` — array of session info objects.

**Behavior:** Lists sessions. Each session includes: `sessionId`, `claudeUUID` (null if not yet discovered), `status`, `parentId`, `priority`, `cwd`, `spawnCwd`, `createdAt`, `pid`, `pinned`, `children` (array of child sessions, populated when `tree: true`).
- Default: returns direct children of the caller (excludes archived).
- `tree: true`: returns children with their descendants nested recursively (each child has its own `children` array populated).
- `all: true`: returns every session in the pool (flat list).
- `all: true` + `tree: true`: returns every session in the pool as a nested tree rooted at top-level sessions.
- `archived: true`: includes archived sessions in the results.
- Includes all non-archived states by default: queued, idle, processing, offloaded, error.

---

### `info`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "session", session }` — a session object (see below).

**Behavior:** Returns a **session object** with all information about the session:

```json
{
  "sessionId": "a7f2x9",
  "claudeUUID": "2947bf12-d307-4a1c-...",
  "status": "idle",
  "parentId": null,
  "priority": 0,
  "cwd": "/Users/me/project",
  "spawnCwd": "/Users/me",
  "createdAt": "2026-03-12T14:30:00Z",
  "pid": 12345,
  "pinned": false,
  "pendingInput": "",
  "children": [
    {
      "sessionId": "b3k9m2",
      "status": "processing",
      "children": [
        { "sessionId": "c1p4q7", "status": "idle", "children": [], ... }
      ],
      ...
    }
  ]
}
```

`cwd` is the session's **current** working directory — it changes as Claude `cd`s around. For live sessions, detected via process inspection (`lsof`/`/proc`). For offloaded sessions, falls back to the last known cwd from the JSONL transcript. `spawnCwd` is the directory the session was originally spawned in (never changes).

`pinned` indicates whether the session is currently pinned (prevents LRU eviction).

`pendingInput` contains any un-submitted text detected in the session's terminal buffer. Empty string if nothing typed. Only populated for loaded sessions. Changes to `pendingInput` reset the session's LRU timestamp.

`children` contains direct child session objects, each of which is a full session object with its own `children` — recursively. This gives you the full subtree rooted at this session.

Works for any state including offloaded, error, and archived. This is the primary way to look up a session's Claude UUID from its internal ID.

---

### `pin`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | No | Target session. If omitted, pins a fresh pre-warmed session and returns its sessionId. |
| `parentId` | sessionId | No | Only used when no sessionId (fresh session mode). Auto-detected from env if omitted. |
| `duration` | integer | No | Pin duration in seconds (default 120) |

**Response:** `{ type: "ok", sessionId, status }` — sessionId is the target (or newly allocated) session. Status is its current state.

**Behavior:** Prevents automatic LRU eviction for the specified duration.
- If **no sessionId** → allocates a fresh pre-warmed session, pins it, and returns its sessionId. If no fresh slot is available, offloads the lowest-priority idle session. If all slots are busy, queues the request (status: `queued`). Use `set-priority` after to adjust priority if needed.
- If session is **live** (idle/processing) → marks as pinned. LRU eviction skips it.
- If session is **offloaded** → transitions to `queued` for priority loading. The session jumps to the front of the load queue and is loaded on the next available slot (may offload an unpinned session to make room).
- If session is **error** → errors. Error sessions have repeatedly failed to load.
- If session is **queued** → bumps to front of queue.
- If session is **archived** → errors. Unarchive first.
- Pin expires after `duration` seconds. After expiration, session is eligible for eviction again.
- Pinning an already-pinned session resets the timer.

---

### `unpin`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "ok" }`

**Behavior:** Removes the pin, making the session eligible for LRU eviction again. No-op if session isn't pinned.

---

### `stop`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "ok" }`

**Behavior:** Interrupts or cancels a session's current operation. **Synchronous** — the session is guaranteed to be idle (or removed) when `ok` is returned.
- If session is **processing** → sends Ctrl+C (`\x03`) to the PTY, waits for the session to reach idle, then returns `ok`. The caller can immediately send a `followup` without needing to `wait`.
- If session is **queued** → cancels the queued request. The session transitions back to its prior state: `offloaded` if it was being loaded, removed entirely if it was a new `start` that never got a slot.
- If session is **idle** → no-op (already not processing).
- If session is **offloaded**, **error**, **archived** → errors (nothing to stop).

---

### `archive`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `recursive` | boolean | No | Archive all descendants too (default false) |

**Response:** `{ type: "ok" }`

**Behavior:** Archives a session — marks it as done. Archived sessions are hidden from `ls` by default and auto-cleaned after 30 days.

- If session is **live** (idle/processing) → stops the session first (sends Ctrl+C if processing, waits for idle), then offloads it, then archives. The slot is freed.
- If session is **offloaded** → transitions to `archived`.
- If session is **error** → transitions to `archived`.
- If session is **queued** → cancels the queued request, then archives.
- If session is **already archived** → no-op.
- If session has **unarchived children** → errors, unless `recursive: true`. With `recursive: true`, archives all descendants depth-first (children before parents, recursively) before archiving the target session.
- Pinned sessions are unpinned before archiving.
- Archived session metadata is auto-cleaned after 30 days.

---

### `unarchive`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "ok" }`

**Behavior:** Restores an archived session to `offloaded` state. The session becomes visible in `ls` again and can be resumed via `followup` or `pin`.
- Only works for **archived** sessions. Errors for any other state.

---

### `set-priority`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `priority` | number | Yes | New priority value |

**Response:** `{ type: "ok" }`

**Behavior:** Sets the session's eviction priority. Lower values are evicted first. Default is 0. Unbounded (can be negative or very large).
- Takes effect immediately for LRU eviction decisions.
- Does NOT affect queue order (queue is always FIFO).
- Does NOT affect processing speed.
- Works for any session state.

---

### `attach`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "attached", socketPath }` — path to a temporary Unix socket.

**Behavior:** Creates a temporary Unix socket for raw PTY I/O.
- Connect to `socketPath` for live terminal streaming: bytes you write = keystrokes to PTY, bytes you read = terminal output.
- Multiple clients can attach simultaneously (output is broadcast to all).
- The pipe closes automatically when the session is offloaded, dies, or the daemon shuts down. Client receives EOF.
- **Only works for live sessions** (idle, processing). Errors if queued, offloaded, error, archived.
- To attach to an offloaded session: `pin` it (triggers load, transitions to queued) → `wait` for it to become live → `attach`.
- The temporary socket is cleaned up when all clients disconnect.
- **Attaching does not affect other operations.** All API commands continue to work normally on attached sessions — `followup`, `stop`, `offload`, etc. Attachment is purely additive.

---

### `subscribe`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessions` | string[] | No | Only events for these sessionIds. Omit = all sessions. |
| `events` | string[] | No | Only these event types. Omit = all events. |
| `statuses` | string[] | No | Only `status` events transitioning TO these states. Omit = all transitions. |
| `fields` | string[] | No | Only `updated` events for these fields. Omit = all fields. |

**Response:** Stream of events, one JSON object per line. The connection stays open.

**Behavior:** Opens a persistent event stream. The daemon pushes events matching the filters:

```json
{"type":"event","event":"status","sessionId":"a7f2x9","status":"idle","prevStatus":"processing"}
{"type":"event","event":"created","sessionId":"b3k9m2","status":"queued","parentId":"a7f2x9"}
{"type":"event","event":"updated","sessionId":"a7f2x9","changes":{"cwd":"/Users/me/other-project"}}
{"type":"event","event":"updated","sessionId":"a7f2x9","changes":{"priority":5}}
{"type":"event","event":"archived","sessionId":"b3k9m2"}
{"type":"event","event":"pool","action":"resize","size":5}
```

Event types:
- `status` — session changed state. Includes `sessionId`, `status`, `prevStatus`.
- `created` — new session added to pool. Includes `sessionId`, `status`, `parentId`.
- `updated` — session property changed (not status). Includes `sessionId` and `changes` object with the changed fields and their new values. Tracked fields: `cwd`, `priority`, `pinned`, `pendingInput`. Filter with the `fields` param to only receive updates for specific properties.
- `archived` — session archived. Includes `sessionId`.
- `unarchived` — session unarchived. Includes `sessionId`.
- `pool` — pool-level event (init, resize, destroy). Includes `action` and relevant details.

Filters are ANDed: an event must match all specified filters. For example, `{ sessions: ["a7f2x9"], statuses: ["idle"] }` only fires when session `a7f2x9` transitions to idle.

**Re-subscribing:** Sending another `subscribe` on the same connection replaces the active subscription's filters. This lets you dynamically add/remove session IDs or change event filters without disconnecting (which could cause missed events). For example, when a new session is created, update your subscription to include it.

**Multiple subscribers:** Each socket connection is independent. Multiple clients (or multiple connections from the same client) can subscribe simultaneously with different filters. Each gets its own event stream.

The stream continues until the client disconnects or the daemon shuts down.
