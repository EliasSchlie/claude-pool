# Socket Protocol

> ⛔ **Protected file.** No changes without explicit user permission. This is the API contract — `schema/protocol.json` is the machine-readable version.

## Transport

Unix domain socket at `~/.claude-pool/pools/<name>/api.sock`.

Newline-delimited JSON: each message is one JSON object followed by `\n`. Clients send a request, daemon sends a response. Requests may include an `id` field — the response echoes it back for correlation.

```
-> {"type":"ping","id":1}\n
<- {"type":"pong","id":1}\n
```

## Error Handling

Errors return `{ type: "error", error: "message" }`.

## Session Identity

Sessions are addressed by **pool-assigned internal IDs** (short random strings like `a7f2x9`). Assigned immediately on `pool-start` — even while queued. The Claude UUID is discovered later and mapped 1:1.

Use `get-session` to look up the Claude UUID for a session.

## Ownership

Every session has an optional `parentId` (the internal ID of the session that spawned it). Auto-detected via `CLAUDE_POOL_SESSION_ID` env var, or set explicitly via `parentId` field.

`get-sessions` returns only sessions owned by the caller by default. Set `all: true` for all pool sessions.

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

Internal states (`fresh`, `starting`) are not exposed via API.

## Commands

### Meta
| Command | Fields | Response |
|---------|--------|----------|
| `ping` | — | `{ type: "pong" }` |

### Pool Lifecycle
| Command | Fields | Response |
|---------|--------|----------|
| `pool-init` | `size?` (default from config) | `{ type: "pool", pool }` |
| `pool-resize` | `size` | `{ type: "pool", pool }` |
| `pool-health` | — | `{ type: "health", health }` |
| `pool-read` | — | `{ type: "pool", pool }` |
| `pool-destroy` | — | `{ type: "ok" }` |
| `pool-config` | `set?` | `{ type: "config", config }` |

All spawn settings (flags, default size) come from `config.json`. Use `pool-config set` to change them.

### Session Operations
| Command | Fields | Response |
|---------|--------|----------|
| `pool-start` | `prompt`, `parentId?`, `priority?` | `{ type: "started", sessionId, status }` |
| `pool-followup` | `sessionId`, `prompt`, `force?` | `{ type: "started", sessionId }` |
| `pool-wait` | `sessionId?`, `timeout?` (ms, default 300000) | `{ type: "result", sessionId, buffer }` |
| `pool-capture` | `sessionId` | `{ type: "buffer", sessionId, buffer }` |
| `pool-result` | `sessionId` | `{ type: "result", sessionId, buffer }` |
| `pool-input` | `sessionId`, `data` | `{ type: "ok" }` |
| `pool-offload` | `sessionId` | `{ type: "ok" }` |

**Behavior:**
- `pool-start` — Returns internal session ID + status immediately. Status is `queued` if no slot available, otherwise the session begins processing. Parent auto-detected from `CLAUDE_POOL_SESSION_ID` env var, or set via `parentId`.
- `pool-followup` — Sends prompt to idle session. **Auto-resumes offloaded sessions.** Errors if busy (unless `force`).
- `pool-wait` — Long-polls until session becomes idle. Works for queued sessions (waits through queue → process → idle). If no sessionId, waits for any owned busy session.
- `pool-result` — Returns buffer only if idle. Errors if running, queued, or offloaded.
- `pool-capture` — Returns buffer even while running. Errors if not in a slot (queued/offloaded).
- `pool-offload` — Manually offload an idle session (snapshot + /clear). Errors if busy.
- Automatic offloading happens via LRU eviction (lower priority first, then oldest).

### Session Management
| Command | Fields | Response |
|---------|--------|----------|
| `get-sessions` | `all?` (default false) | `{ type: "sessions", sessions }` |
| `get-session` | `sessionId` | `{ type: "session", session }` |
| `pin-session` | `sessionId`, `duration?` (seconds, default 120) | `{ type: "ok" }` |
| `unpin-session` | `sessionId` | `{ type: "ok" }` |
| `stop-session` | `sessionId` | `{ type: "ok" }` |
| `set-priority` | `sessionId`, `priority` (number, default 0) | `{ type: "ok" }` |

**Pin behavior:** Prevents auto-offload. If the session is offloaded, pinning triggers priority load on next available slot (jumps the queue). If queued, bumps to front.

**Stop:** Sends Ctrl+C to interrupt a running session. Transitions from `processing` → `idle`.

**Priority:** Numeric (default 0, unbounded). Affects LRU eviction order only — lower priority evicted first. Does not affect queue order or processing speed.

### Terminal Attachment
| Command | Fields | Response |
|---------|--------|----------|
| `attach` | `sessionId` | `{ type: "attached", socketPath }` |

Returns path to a temporary Unix socket for raw PTY I/O. Pipe closes when session is offloaded or dies. **Requires live session** — errors if offloaded/queued. To attach offloaded: pin → wait for live → attach.
