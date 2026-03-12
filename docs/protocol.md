# Socket Protocol

## Transport

Unix domain socket at `~/.claude-pool/pools/<name>/api.sock`.

Newline-delimited JSON: each message is one JSON object followed by `\n`. Clients send a request, daemon sends a response. Requests may include an `id` field — the response echoes it back for correlation.

```
-> {"type":"ping","id":1}\n
<- {"type":"pong","id":1}\n
```

## Error Handling

Errors return `{ type: "error", error: "message" }`.

## Addressing Modes

Three ways to target a session, at different abstraction levels:

| Mode | When to use | Example |
|------|-------------|---------|
| `sessionId` | Normal interaction | `{"type":"pool-capture","sessionId":"abc-123"}` |
| `slotIndex` | Debugging, error recovery | `{"type":"slot-read","slotIndex":3}` |
| `termId` | Low-level PTY access | `{"type":"pty-read","termId":63}` |

Commands accepting `sessionId` also accept `slotIndex` as alternative. When both provided, `slotIndex` takes precedence.

## Validation

- `sessionId` must match `/^[a-f0-9-]+$/i` (prevents path traversal)
- `slotIndex` must be a finite number
- `termId` must be a finite number

---

## Commands

### Meta

| Command | Fields | Response |
|---------|--------|----------|
| `ping` | — | `{ type: "pong" }` |

### Pool Lifecycle

| Command | Fields | Response |
|---------|--------|----------|
| `pool-init` | `size` (default 5) | `{ type: "pool", pool }` |
| `pool-resize` | `size` | `{ type: "pool", pool }` |
| `pool-health` | — | `{ type: "health", health }` |
| `pool-read` | — | `{ type: "pool", pool }` |
| `pool-destroy` | — | `{ type: "ok" }` |
| `pool-get-flags` | — | `{ type: "flags", flags }` |
| `pool-set-flags` | `flags` | `{ type: "flags", flags }` |
| `pool-get-min-fresh` | — | `{ type: "min-fresh", minFreshSlots }` |
| `pool-set-min-fresh` | `minFreshSlots` | `{ type: "min-fresh", minFreshSlots }` |

### Session Operations (high-level)

| Command | Fields | Response |
|---------|--------|----------|
| `pool-start` | `prompt`, `parentSessionId?` | `{ type: "started", sessionId, termId, slotIndex }` |
| `pool-resume` | `sessionId` | `{ type: "resumed", sessionId, termId, slotIndex }` |
| `pool-followup` | `sessionId`, `prompt`, `force?` | `{ type: "started", sessionId, termId, slotIndex }` |
| `pool-wait` | `sessionId?` or `slotIndex?`, `timeout?` (ms, default 300000) | `{ type: "result", sessionId, buffer }` |
| `pool-capture` | `sessionId` or `slotIndex` | `{ type: "buffer", sessionId, slotIndex, buffer }` |
| `pool-result` | `sessionId` or `slotIndex` | `{ type: "result", sessionId, slotIndex, buffer }` |
| `pool-input` | (`sessionId` or `slotIndex`), `data` | `{ type: "ok" }` |
| `pool-clean` | — | `{ type: "cleaned", count }` |

**Behavior:**
- `pool-start` — Claims a fresh slot (offloads LRU idle if none available), sends prompt, marks busy.
- `pool-resume` — Restores an offloaded/archived session into a fresh slot. Session ID may change after `/resume`.
- `pool-followup` — Sends prompt to idle session. Errors if busy (unless `force: true`). Auto-resumes if not live.
- `pool-wait` — Long-polls until session becomes idle. If no session specified, waits for any busy session.
- `pool-result` — Returns buffer only if session is idle. Errors if still running.
- `pool-clean` — Offloads all idle sessions (snapshot + `/clear`), then archives them.

### Session Metadata

| Command | Fields | Response |
|---------|--------|----------|
| `get-sessions` | — | `{ type: "sessions", sessions }` |
| `read-intention` | `sessionId` | `{ type: "intention", content }` |
| `write-intention` | `sessionId`, `content` | `{ type: "ok" }` |
| `archive-session` | `sessionId` | `{ type: "ok" }` |
| `unarchive-session` | `sessionId` | `{ type: "ok" }` |
| `pin-session` | `sessionId`, `duration?` (seconds, default 120) | `{ type: "ok" }` |
| `unpin-session` | `sessionId` | `{ type: "ok" }` |
| `stop-session` | `sessionId` | `{ type: "ok" }` |

### Session Terminals (per-session shell tabs)

| Command | Fields | Response |
|---------|--------|----------|
| `session-terminals` | `sessionId` | `{ type: "terminals", terminals }` |
| `session-term-read` | `sessionId`, `tabIndex` | `{ type: "buffer", termId, buffer }` |
| `session-term-write` | `sessionId`, `tabIndex`, `data` | `{ type: "ok" }` |
| `session-term-open` | `sessionId`, `cwd?` | `{ type: "spawned", termId, tabIndex }` |
| `session-term-close` | `sessionId`, `tabIndex` | `{ type: "ok" }` |
| `session-term-run` | `sessionId`, `tabIndex`, `command`, `timeout?` (ms, default 30000) | `{ type: "output", output, termId }` |

### Slot Access (by index, for debugging)

| Command | Fields | Response |
|---------|--------|----------|
| `slot-read` | `slotIndex` | `{ type: "buffer", slotIndex, sessionId, buffer }` |
| `slot-write` | `slotIndex`, `data` | `{ type: "ok" }` |
| `slot-status` | `slotIndex` | `{ type: "slot", slot }` |

### PTY Access (low-level)

| Command | Fields | Response |
|---------|--------|----------|
| `pty-list` | — | `{ type: "ptys", ptys }` |
| `pty-read` | `termId` | `{ type: "buffer", buffer }` |
| `pty-write` | `termId`, `data` | `{ type: "ok" }` |
| `pty-spawn` | `cwd`, `cmd?`, `args?` | `{ type: "spawned", termId, pid }` |
| `pty-kill` | `termId` | `{ type: "ok" }` |
