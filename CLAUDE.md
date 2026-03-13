# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API. Written in Go.

## Status

Early development — designing from requirements, referencing Open Cockpit (`~/projects/open-cockpit/`) for patterns. Not a 1:1 copy.

## ⚠️ Read First

**[SPEC.md](SPEC.md)** — Product contract (invariants, API surface). All code must respect it.

## ⛔ Protected Files

These files require **explicit user permission** before any modification:
- `SPEC.md` — Invariants + API surface contract
- `tests/integration/` — Integration tests. Propose changes and get approval first.

## Architecture

- `cmd/claude-pool/` — Daemon entry point
- `internal/` — Daemon packages (pool, pty, api, attach, discovery, paths)
- `tests/integration/` — Integration tests (real Claude sessions, `--model haiku`)
- `tests/manual/` — Manual testing directory (own `.claude/` hooks, independent per worktree)
- `schema/` — JSON Schema (must match SPEC.md)
- `hooks/` — Claude Code hook scripts (project-local, written into pool dir on init)
- `docs/` — Documentation

Key docs:
- [SPEC.md](SPEC.md) — **Product contract** (read first)
- [docs/protocol.md](docs/protocol.md) — Full protocol reference (field tables, JSON shapes, per-state behavior)
- [docs/architecture.md](docs/architecture.md) — Component overview, multi-pool access
- [docs/testing.md](docs/testing.md) — Testing strategy (no mocking, real sessions, haiku model)
- [docs/extraction-plan.md](docs/extraction-plan.md) — Implementation plan, OC reference notes

## Scope

Claude Pool manages pools of Claude sessions: spawn, offload, restore, prompt, wait, attach.

**Not in scope:** Terminal tabs (claude-term), intention files (Open Cockpit), UI (Open Cockpit), non-pool session discovery (Open Cockpit).

## Related Projects

- **CLI** — Separate package (`claude-pool-cli`). Thin router that resolves pool names from registry to socket connections.
- **claude-term** — Separate project. Persistent terminal tabs for Claude sessions. Independent from claude-pool.
- **Open Cockpit** — Electron app. Depends on claude-pool (via socket) and claude-term. Human interface.

## Testing

When a bug is found in production that wasn't caught by integration tests, figure out which existing flow should have caught it and add a `t.Run` step at the right point in the sequence. If it doesn't fit naturally into any existing flow (different pool config needed, flow would get too long, fundamentally different scenario), propose a new flow file to the user. See [tests/integration/CLAUDE.md](tests/integration/CLAUDE.md) for test structure and philosophy.

## Origin

Designed from requirements, not copied from Open Cockpit. Open Cockpit code is a reference for patterns and edge cases. See [docs/extraction-plan.md](docs/extraction-plan.md).
