# Socket Protocol

Implementation details for the socket API. For what the API exposes and why, see [SPEC.md](../SPEC.md). If this document contradicts the spec, the spec wins.

## Transport

Unix domain socket at `~/.claude-pool/<name>/api.sock`. Newline-delimited JSON. Each message is one JSON object + `\n`. Requests may include `id` — response echoes it back.

```
-> {"type":"ping","id":1}\n
<- {"type":"pong","id":1}\n
```

All errors: `{ type: "error", error: "human-readable message", id: <echoed> }`.

---

## Output Capture Implementation

See SPEC.md for parameter definitions (`source`, `turns`, `detail`). This section covers how they're implemented.

### JSONL transcript structure

Claude Code's JSONL transcripts use these entry types:

| Entry `type` | Meaning |
|-------------|---------|
| `user` | User prompt OR tool result (distinguished by content blocks) |
| `assistant` | Assistant response OR tool use (distinguished by content blocks) |
| `progress` | Hook execution, internal events |
| `system` | System messages |
| `file-history-snapshot` | File state snapshots |

Tool calls are **not** separate entry types. A `tool_use` block appears inside an `assistant` entry's `message.content` array. A `tool_result` block appears inside a `user` entry's `message.content` array.

### Turn boundaries

A turn starts at a user prompt (a `type: "user"` entry where `message.content` contains a `text` block, not a `tool_result` block) and includes everything until the next user prompt. For buffer source, turn boundaries are detected from the JSONL transcript — `turns: 1` returns terminal output since the last user prompt was sent.

### Detail filtering

| Value | Entries included | Content filtering |
|-------|-----------------|-------------------|
| `"last"` | User prompts + final assistant entry with text, per turn. | Exclude tool_use blocks. Exclude tool_result user entries. |
| `"assistant"` | User prompts + all assistant entries that contain text. | Exclude tool_use blocks. Exclude tool_result user entries. |
| `"tools"` | All user entries (prompts + tool results) + all assistant entries. | Keep all content blocks. Strip metadata fields (see below). |
| `"raw"` | All entries unfiltered (including progress, system, etc.). | No filtering. |

### Metadata stripping (`detail: "tools"`)

**Stripped from all entries:** `parentUuid`, `isSidechain`, `version`, `gitBranch`, `requestId`, `uuid`, `timestamp`, `cwd`, `sessionId`, `userType`

**Stripped from `message` objects:** `model`, `id`, `usage`, `stop_reason`, `stop_sequence`

**Kept:** `type`, `message.role`, `message.content`

Principle: keep conversation content, strip everything else.

### Output format

JSONL source: `content` is always JSONL (one JSON object per line).

```jsonl
{"type":"user","content":"What is 2+2?"}
{"type":"assistant","content":"4"}
```

Buffer source: `content` is plain text (ANSI escape sequences stripped).

Empty content (session stopped before output, no assistant message in requested turns) returns an empty string — not an error.

---

## Command Behavior Notes

Field tables and basic behavior are in SPEC.md. This section documents per-state behavior and implementation details that go beyond the spec.

### `init`

Response: `{ type: "health", health }` — same format as `health` command.

If previous session state exists, `init` restores sessions that were **live** (idle or processing) when the pool last shut down via `/resume`. Excess sessions beyond `size` stay offloaded. `noRestore: true` ignores previous state.

### `resize`

When shrinking: enqueues "kill slot" tokens at the front of the internal queue. When a slot becomes available, a kill token consumes it — the session is offloaded and the slot is permanently removed. Processing sessions finish naturally. Pinned sessions are never evicted. Queued requests stay in the queue behind kill tokens.

### `start`

Assigns internal session ID immediately. If a fresh slot is available, claims it and sends prompt (if provided). Otherwise queues — the queue processor asynchronously evicts the lowest-priority idle session (LRU within same priority). FIFO ordering.

### `followup`

Per-state behavior:
- **idle** → sends prompt immediately (timing dance: Escape, Ctrl-U, type, poll buffer, Enter)
- **offloaded** → queues for loading (→ `queued`), loads via `/resume <claudeUUID>`, sends prompt
- **processing/queued** → errors (stop first)
- **archived** → errors (unarchive first)

### `wait`

Long-polls until target session reaches `idle`. Returns immediately if already idle. Errors if offloaded (use `followup` to resume). On timeout: `{ type: "error", error: "timeout" }`. If session dies: `{ type: "error", error: "session died" }`.

### `capture`

- **JSONL source** works for any session with a Claude UUID (idle, processing, offloaded, archived, error, re-queued)
- **Buffer source** only works for live sessions (idle, processing)
- Sessions queued from scratch have no UUID — all sources error

### `stop`

**Synchronous** — session is guaranteed idle (or removed) when `ok` returns. Processing → Ctrl+C + wait. Queued → cancel (reverts to prior state or removes). Idle → no-op.

### `archive`

If processing → stopped first. If loaded (idle) → offloaded first. Has unarchived children → errors unless `recursive: true` (depth-first). Pinned → unpinned first. Idempotent on already-archived sessions.

### `set`

At least one of `priority`, `pinned`, or `metadata` required. Priority: unbounded, takes effect immediately for LRU. Pinned: duration in seconds, `false` to unpin, resets timer if already pinned. Metadata: merge semantics, `null` clears fields, `null` on `tags` key deletes it.

---

## UI-specific API

API-only — not exposed in the CLI. For user interfaces (e.g. Open Cockpit).

### `attach`

Creates a temporary Unix socket for raw PTY I/O. Response includes current PTY dimensions (`cols`, `rows`) so clients can create viewports at matching size before writing replay buffer. Multiple clients can attach simultaneously (broadcast). Pipe closes on offload/death/shutdown. Only works for live sessions. Attaching does not affect other operations.

### `pty-resize`

Sets a session's PTY dimensions (`cols`, `rows`). Triggers SIGWINCH on the underlying process. Only works on live sessions (idle, processing).

### `subscribe`

Persistent event stream. Filters (ANDed): `sessions`, `events`, `statuses`, `fields`.

Event types: `status` (state change), `created` (new session), `updated` (property change — cwd, priority, pinned, pendingInput, metadata), `archived`, `unarchived`, `pool` (init/resize/destroy).

Re-subscribing on same connection replaces filters. Multiple connections get independent streams.

---

## Debug Commands

### `input`

Raw bytes to session PTY. No timing dance. Only works for live sessions. Use `followup` for safe prompt delivery.
