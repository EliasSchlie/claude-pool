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

## Session Addressing

All sessions are addressed by their **Claude UUID** — the session UUID assigned by Claude Code. No slot indices, no terminal IDs, no internal identifiers.

Some commands accept an optional `sessionId`. When omitted, the daemon picks (e.g. `pool-wait` with no sessionId waits for any busy session).

## Commands

### Meta
| Command | Fields | Response |
|---------|--------|----------|
| `ping` | — | `{ type: "pong" }` |

### Pool Lifecycle
| Command | Fields | Response |
|---------|--------|----------|
| `pool-init` | `size?` (default 5), `flags?` (override config) | `{ type: "pool", pool }` |
| `pool-resize` | `size` | `{ type: "pool", pool }` |
| `pool-health` | — | `{ type: "health", health }` |
| `pool-read` | — | `{ type: "pool", pool }` |
| `pool-destroy` | — | `{ type: "ok" }` |
| `pool-config` | `set?` (fields to update) | `{ type: "config", config }` |

### Session Operations
| Command | Fields | Response |
|---------|--------|----------|
| `pool-start` | `prompt` | `{ type: "started", sessionId }` |
| `pool-resume` | `sessionId` | `{ type: "resumed", sessionId }` (may be new UUID) |
| `pool-followup` | `sessionId`, `prompt`, `force?` | `{ type: "started", sessionId }` |
| `pool-wait` | `sessionId?`, `timeout?` (ms, default 300000) | `{ type: "result", sessionId, buffer }` |
| `pool-capture` | `sessionId` | `{ type: "buffer", sessionId, buffer }` |
| `pool-result` | `sessionId` | `{ type: "result", sessionId, buffer }` (errors if running) |
| `pool-input` | `sessionId`, `data` | `{ type: "ok" }` |
| `pool-offload` | `sessionId` | `{ type: "ok" }` (errors if busy) |

**Behavior:**
- `pool-start` — Claims a fresh slot (offloads LRU idle if none available), sends prompt.
- `pool-resume` — Restores an offloaded session. The returned sessionId may differ (new Claude UUID).
- `pool-followup` — Sends prompt to idle session. Auto-resumes if offloaded. Errors if busy (unless `force`).
- `pool-wait` — Long-polls until idle. If no sessionId, waits for any busy session.
- `pool-result` — Returns buffer only if idle.
- `pool-offload` — Manually offload a specific session (snapshot + /clear). For when you're done with a session and want to free the slot without destroying it.
- Automatic offloading happens via LRU eviction when `pool-start` needs a slot.

### Session Management
| Command | Fields | Response |
|---------|--------|----------|
| `get-sessions` | — | `{ type: "sessions", sessions }` |
| `archive-session` | `sessionId` | `{ type: "ok" }` |
| `unarchive-session` | `sessionId` | `{ type: "ok" }` |
| `pin-session` | `sessionId`, `duration?` (seconds, default 120) | `{ type: "ok" }` |
| `unpin-session` | `sessionId` | `{ type: "ok" }` |
| `stop-session` | `sessionId` | `{ type: "ok" }` |

### Terminal Attachment
| Command | Fields | Response |
|---------|--------|----------|
| `attach` | `sessionId` | `{ type: "attached", socketPath }` |

`attach` returns a path to a temporary Unix socket for raw PTY I/O. Connect to it for live terminal streaming (bytes in = keystrokes, bytes out = terminal output). The pipe closes automatically when the session is offloaded or dies.

Only works for live sessions. Attach to an offloaded session → error. Resume first, then attach.

## Security

Socket permissions are set to `0600` (owner-only).
