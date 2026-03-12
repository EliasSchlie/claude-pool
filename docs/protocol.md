# Socket Protocol

**Machine-readable contract:** [`schema/protocol.json`](../schema/protocol.json) (JSON Schema). This doc is a human-friendly summary — the schema is the source of truth.

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

Sessions are addressed by **pool-assigned internal IDs** (not Claude UUIDs). Internal IDs are assigned immediately on `pool-start` — even while queued, before a Claude process exists. The Claude UUID is discovered later and mapped 1:1.

Use `get-session` to look up the Claude UUID for a session.

## Ownership

Every session has an optional `parentId` (the internal ID of the session that spawned it). The daemon auto-detects parents via the `CLAUDE_POOL_SESSION_ID` env var set on pool-spawned sessions. External callers can pass `parentId` explicitly.

`get-sessions` returns only sessions owned by the caller (children + descendants) by default. Set `all: true` to see all pool sessions.

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Request received, waiting for a slot |
| `starting` | Slot allocated, Claude spawning, UUID not yet known |
| `fresh` | Claude started, idle, never received a prompt |
| `idle` | Finished processing, waiting for input |
| `typing` | Input being sent (timing dance before Enter) |
| `processing` | Claude is working |
| `offloaded` | Snapshot saved, slot freed |
| `dead` | Process died |
| `error` | Slot error (crash during startup, etc.) |

## Commands

### Meta
| Command | Fields | Response |
|---------|--------|----------|
| `ping` | — | `{ type: "pong" }` |

### Pool Lifecycle
| Command | Fields | Response |
|---------|--------|----------|
| `pool-init` | `size?` (default 5), `flags?` | `{ type: "pool", pool }` |
| `pool-resize` | `size` | `{ type: "pool", pool }` |
| `pool-health` | — | `{ type: "health", health }` |
| `pool-read` | — | `{ type: "pool", pool }` |
| `pool-destroy` | — | `{ type: "ok" }` |
| `pool-config` | `set?` | `{ type: "config", config }` |

### Session Operations
| Command | Fields | Response |
|---------|--------|----------|
| `pool-start` | `prompt`, `parentId?` | `{ type: "started", sessionId, status }` |
| `pool-resume` | `sessionId` | `{ type: "resumed", sessionId, claudeUUID }` |
| `pool-followup` | `sessionId`, `prompt`, `force?` | `{ type: "started", sessionId }` |
| `pool-wait` | `sessionId?`, `timeout?` | `{ type: "result", sessionId, buffer }` |
| `pool-capture` | `sessionId` | `{ type: "buffer", sessionId, buffer }` |
| `pool-result` | `sessionId` | `{ type: "result", sessionId, buffer }` |
| `pool-input` | `sessionId`, `data` | `{ type: "ok" }` |
| `pool-offload` | `sessionId` | `{ type: "ok" }` |

**Queueing:** `pool-start` returns immediately with a `sessionId` and `status`. If status is `queued`, the session will be started FIFO when a slot becomes available. `pool-wait` works for queued sessions — it waits for the session to start, process, and become idle.

### Session Management
| Command | Fields | Response |
|---------|--------|----------|
| `get-sessions` | `all?` (default false) | `{ type: "sessions", sessions }` |
| `get-session` | `sessionId` | `{ type: "session", session }` |
| `archive-session` | `sessionId` | `{ type: "ok" }` |
| `unarchive-session` | `sessionId` | `{ type: "ok" }` |
| `pin-session` | `sessionId`, `duration?` (seconds) | `{ type: "ok" }` |
| `unpin-session` | `sessionId` | `{ type: "ok" }` |
| `stop-session` | `sessionId` | `{ type: "ok" }` |

**Pin behavior:** Pinning prevents auto-offload. If the session is offloaded or queued, pinning bumps it to load on the next available slot (priority over queue).

### Terminal Attachment
| Command | Fields | Response |
|---------|--------|----------|
| `attach` | `sessionId` | `{ type: "attached", socketPath }` |

Requires a live session. Pipe closes on offload/death. To attach to an offloaded session: pin → wait for live → attach.

## Security

Socket permissions are set to `0600` (owner-only).
