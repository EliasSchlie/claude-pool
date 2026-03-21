# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API. Written in Go.

## Status

Active development. Go daemon + CLI, distributed as a Claude Code plugin.

## ⚠️ Read First

**[SPEC.md](SPEC.md)** — Product contract (invariants, API surface). All code must respect it.

## ⛔ Protected Files

These files require **explicit user permission** before any modification:
- `SPEC.md` — Invariants + API surface contract
- `tests/integration/` — Integration tests. Propose changes and get approval first.

## Quick Reference

- **Build:** `make build` (compiles binaries to `bin/`)
- **Unit tests:** `go test ./internal/...`
- **Integration tests:** `go test ./tests/integration/ -v -timeout 10m` (real Claude sessions, costs tokens)
- **Install:** `make install` (build + symlink to `~/.local/bin/`)
- **Deploy plugin:** `./deploy-plugin.sh` (rebuilds binaries, bumps version, copies to cache, then `/reload-plugins`)
- **Restart pool:** `claude-pool-cli destroy --confirm && claude-pool-cli init --size N` (needed after code/hook changes)
- **Plugin test:** `claude --plugin-dir .` (loads skill + hooks for one session only)
- **Standalone install:** `./claude-pool install` (fallback — writes directly to `~/.claude/`)
- ⚠️ Don't use both plugin AND standalone install — hooks fire twice. Run `claude-pool uninstall` before switching to plugin.

## Architecture

### Core model

- **Slot** (`slot.go`) — physical resource: process + PTY + terminal emulator. Count = pool size. States: spawning, fresh, clearing, resuming, idle, processing, crashed. Owns `Process`, `Term`, `Pipe`, `ClearQueue`.
- **Session** (`session.go`) — logical identity: ID, parent, metadata, priority, pinned. Loaded into slots via `bindSession`/`unbindSession`. States: queued, idle, processing, offloaded, error, archived.
- **Manager** (`manager.go`) — holds `[]*Slot` and `map[string]*Session`. Single mutex.

### File layout

- `.claude-plugin/` — Plugin manifest
- `skills/claude-pool/` — Plugin skill
- `hooks/` — Plugin hooks (SessionStart lifecycle signals + PreToolUse PID registry)
- `cmd/claude-pool/` — Daemon entry point + install/uninstall commands
- `cmd/claude-pool-cli/` — CLI entry point (thin router, resolves pool from registry)
- `internal/pool/` — Core: `slot.go`, `session.go`, `manager.go`, `lifecycle.go`, `typing.go`, `config.go`, `capture.go`, `persistence.go`, `attach.go`, handlers split by domain (`handlers_session.go`, `handlers_pool.go`, `handlers_query.go`, `handlers_lifecycle.go`, `handlers_attach.go`)
- `internal/` — Other packages: api, hookfiles, paths, pty
- `tests/integration/` — Integration tests (real Claude sessions, `--model haiku`)
- `tests/manual/` — Manual testing directory (own `.claude/` hooks, independent per worktree)
- `schema/` — JSON Schema (`protocol.json` — must match SPEC.md, validated by tests)
- `docs/` — Documentation

Key docs:
- [SPEC.md](SPEC.md) — **Product contract** (read first)
- [docs/architecture.md](docs/architecture.md) — Components, design decisions, hooks, directory structure
- [docs/protocol.md](docs/protocol.md) — Socket API implementation details (output capture, per-state behavior)
- [docs/testing.md](docs/testing.md) — Testing strategy (no mocking, real sessions, haiku model)

## Scope

Claude Pool manages pools of Claude sessions: spawn, offload, restore, prompt, wait, attach.

**Not in scope:** Terminal tabs (claude-term), intention files (Open Cockpit), UI (Open Cockpit), non-pool session discovery (Open Cockpit).

## Related Projects

- **CLI** — Separate package (`claude-pool-cli`). Thin router that resolves pool names from registry to socket connections.
- **claude-term** — Separate project. Persistent terminal tabs for Claude sessions. Independent from claude-pool.
- **Open Cockpit** — Electron app. Depends on claude-pool (via socket) and claude-term. Human interface.

## Testing

When a bug is found in production that wasn't caught by integration tests, figure out which existing flow should have caught it and add a `t.Run` step at the right point in the sequence. If it doesn't fit naturally into any existing flow (different pool config needed, flow would get too long, fundamentally different scenario), propose a new flow file to the user. See [tests/integration/CLAUDE.md](tests/integration/CLAUDE.md) for test structure and philosophy.

## Idle Detection

Two complementary systems detect slot state transitions:

1. **Screen content monitoring** (`typing.go` → `pollBufferInput`, runs on **slots**):
   - Content above prompt changing → slot processing
   - Content stable 2s → slot idle (calls `transitionSlotToIdle()`)
   - PTY silent 2s → slot idle (fallback)
   - Pending input detection → surfaced to session via `session.PendingInput`

2. **Hook signal watcher** (`lifecycle.go` → `watchIdleSignal`):
   - Handles `session-start`/`session-clear` triggers only (spawning/clearing → ready)
   - All other signal triggers ignored

Don't add new hooks for idle detection — extend `pollBufferInput` instead.

### Clear workflow

`clearSlot()` runs: `/clear` → `/update-plugins` → `/clear`. Each step is delivered when the previous completes (detected by `pollBufferInput`). The slot stays in `SlotClearing` until the queue drains, then becomes `SlotFresh`.

## Go

Module: `github.com/EliasSchlie/claude-pool` — Go 1.23, deps: `creack/pty`, `x/term`.
