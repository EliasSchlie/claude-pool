# Socket Protocol

> ⛔ **Protected file.** No changes without explicit user permission. `schema/protocol.json` is the machine-readable version.

## Transport

Unix domain socket at `~/.claude-pool/pools/<name>/api.sock`. Newline-delimited JSON. Each message is one JSON object + `\n`. Requests may include `id` — response echoes it back.

```
-> {"type":"ping","id":1}\n
<- {"type":"pong","id":1}\n
```

## Errors

All errors: `{ type: "error", error: "human-readable message", id: <echoed> }`.

## Session Identity

Sessions are addressed by **internal IDs** — short random strings (like `a7f2x9`) assigned at request time. The Claude UUID is discovered later and mapped 1:1. Use `info` to look up the Claude UUID.

## Ownership

Sessions track their parent. Auto-detected via `CLAUDE_POOL_SESSION_ID` env var, or set via `parentId`. `ls` returns only owned sessions by default. `all: true` shows everything.

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Request received, waiting for a slot |
| `idle` | Finished processing, waiting for input |
| `typing` | Un-submitted input detected in terminal buffer |
| `processing` | Claude is working |
| `offloaded` | Snapshot saved, slot freed |
| `dead` | Process died |
| `error` | Slot error (crash during startup, etc.) |

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

**Response:** `{ type: "pool", pool }` — full pool state after init.

**Behavior:** Creates a new pool with `size` pre-warmed Claude sessions. Reads flags from `config.json`. Errors if pool already initialized. Each slot spawns a Claude process with the configured flags. Sessions start in internal `fresh` state (API won't expose this — they appear once they reach a visible state). Updates `pools.json` registry.

---

### `resize`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `size` | integer ≥ 0 | Yes | New slot count |

**Response:** `{ type: "pool", pool }`

**Behavior:** Grows or shrinks the pool. When growing: spawns new slots using flags from `config.json`. When shrinking: kills excess slots, preferring to kill in order: queued requests first, then idle sessions (lowest priority first, then oldest), then processing sessions (same priority order). Size 0 kills all sessions but keeps the pool alive (unlike `destroy`).

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
| — | — | — | No fields |

**Response:** `{ type: "ok" }`

**Behavior:** Kills all sessions (including processing ones), removes `pool.json`, removes the pool directory entry from `pools.json` registry. The daemon process exits. Irreversible.

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
| `priority` | number | No | Eviction priority (default 0). Lower = evicted first. |

**Response:** `{ type: "started", sessionId, status }` — internal ID + initial status.

**Behavior:** Assigns an internal session ID immediately and returns it. Then:
1. If a fresh (pre-warmed) slot is available → claims it, sends the prompt. Status: first visible state (e.g. `processing`).
2. If no fresh slot but an idle session exists → offloads the lowest-priority idle session (LRU within same priority), claims the freed slot, sends the prompt.
3. If all slots are busy → queues the request. Status: `queued`. The request is executed FIFO when a slot becomes available.

The `parentId` is recorded on the session. If the caller is itself a pool session (detected via `CLAUDE_POOL_SESSION_ID` env var), that ID is used as parent automatically.

---

### `followup`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `prompt` | string | Yes | The prompt to send |
| `force` | boolean | No | Send even if session is busy (default false) |

**Response:** `{ type: "started", sessionId }`

**Behavior:** Sends a follow-up prompt to an existing session.
- If session is **idle** → sends the prompt immediately. Handles the timing dance (Escape, Ctrl-U, type text, poll buffer, Enter).
- If session is **offloaded** → auto-resumes (claims a fresh slot, sends `/resume <claudeUUID>`, waits for session to be ready, then sends the prompt). Session's internal ID stays the same, but Claude UUID may change.
- If session is **processing** → errors, unless `force: true` (sends the prompt anyway, useful for interrupt-and-redirect).
- If session is **queued** → errors (session hasn't started yet).
- If session is **typing** → errors, unless `force: true`.
- If session is **dead/error** → errors.

---

### `wait`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | No | Target session. If omitted, waits for any owned busy session. |
| `timeout` | integer | No | Timeout in ms (default 300000 = 5 min) |

**Response:** `{ type: "result", sessionId, buffer }` — terminal buffer content when session becomes idle.

**Behavior:** Long-polls until the target session reaches `idle` state.
- If session is **queued** → waits through: queue → slot allocation → processing → idle.
- If session is **processing** → waits until idle.
- If session is **idle** → returns immediately with current buffer.
- If session is **offloaded** → errors (nothing to wait for — use `followup` to resume).
- If no `sessionId` → waits for any owned session that is currently `queued` or `processing` to become idle. Returns the first one that completes.
- On timeout → returns `{ type: "error", error: "timeout" }`.
- If session dies while waiting → returns `{ type: "error", error: "session died" }`.

---

### `capture`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "buffer", sessionId, buffer }` — raw terminal buffer content.

**Behavior:** Returns the current terminal buffer content, regardless of session state.
- Works for **idle**, **typing**, **processing** sessions (anything with a live terminal).
- Errors if session is **queued** (no terminal yet) or **offloaded** (terminal freed).
- Buffer is the full terminal scrollback rendered to plain text (ANSI stripped).

---

### `result`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "result", sessionId, buffer }`

**Behavior:** Like `capture` but only succeeds when the session is **idle**. This guarantees the buffer contains a complete response, not a partial one.
- If session is **idle** → returns buffer.
- If session is **processing**, **typing**, **queued**, **offloaded**, **dead**, **error** → errors with the current status in the error message.

---

### `input`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `data` | string | Yes | Raw bytes to send to the terminal |

**Response:** `{ type: "ok" }`

**Behavior:** Sends raw bytes directly to the session's PTY input. No timing dance, no buffer polling — just raw write.
- Works for any session with a live terminal (**idle**, **typing**, **processing**).
- Errors if **queued** or **offloaded** (no terminal).
- Use this for sending keystrokes like `\r` (Enter), `\x03` (Ctrl+C), `\x1b` (Escape), or arbitrary text.
- Does NOT handle the timing dance. For safe prompt delivery, use `followup` instead.

---

### `offload`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "ok" }`

**Behavior:** Manually offload a session to free its slot.
1. Captures a snapshot of the terminal buffer → saves to `offloaded/<internalId>/snapshot.log`.
2. Saves session metadata → `offloaded/<internalId>/meta.json`.
3. Sends `/clear` to the Claude session (frees Claude's internal state).
4. Kills the slot's PTY process.
5. Session transitions to `offloaded`.

- Only works for **idle** sessions. Errors if **processing**, **typing**, **queued** (nothing to offload), or already **offloaded**.
- If session is **pinned** → errors (unpin first).

---

### `ls`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `all` | boolean | No | Show all pool sessions (default false = owned only) |

**Response:** `{ type: "sessions", sessions }` — array of session info objects.

**Behavior:** Lists sessions. Each session includes: `sessionId`, `claudeUUID` (null if not yet discovered), `status`, `parentId`, `priority`, `cwd`, `createdAt`, `pid`.
- Default: returns sessions where the caller is the parent (direct children + their descendants, recursively).
- `all: true`: returns every session in the pool.
- Includes all states: queued, idle, typing, processing, offloaded, dead, error.

---

### `info`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |

**Response:** `{ type: "session", session }` — full session details.

**Behavior:** Returns complete information for a single session: internal ID, Claude UUID (null if pending), status, parent, priority, cwd, createdAt, pid. Works for any state including offloaded and dead.

---

### `pin`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sessionId` | sessionId | Yes | Target session |
| `duration` | integer | No | Pin duration in seconds (default 120) |

**Response:** `{ type: "ok" }`

**Behavior:** Prevents automatic LRU eviction for the specified duration.
- If session is **live** (idle/typing/processing) → marks as pinned. LRU eviction skips it.
- If session is **offloaded** → triggers priority load. The session jumps to the front of the load queue and is loaded on the next available slot (may offload an unpinned session to make room).
- If session is **queued** → bumps to front of queue.
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

**Behavior:** Interrupts a running session by sending Ctrl+C (`\x03`) to the PTY.
- If session is **processing** → sends Ctrl+C. Claude should stop its current work and return to idle. Session transitions to `idle` once Claude's stop hook fires.
- If session is **idle**, **typing** → no-op (already not processing).
- If session is **queued** → removes from queue. Session transitions to... actually, this needs thought. Probably errors — use a different command to cancel queued requests.
- If session is **offloaded** → errors (nothing to stop).

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
- **Only works for live sessions** (idle, typing, processing). Errors if queued, offloaded, dead, error.
- To attach to an offloaded session: `pin` it (triggers load) → wait for it to become live → `attach`.
- The temporary socket is cleaned up when all clients disconnect.
