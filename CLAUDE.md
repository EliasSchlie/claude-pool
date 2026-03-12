# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API. Written in Go.

## Status

Early development — designing from requirements, referencing Open Cockpit (`~/projects/open-cockpit/`) for patterns. Not a 1:1 copy.

## ⚠️ Read First

**[docs/design-principles.md](docs/design-principles.md)** — Invariants, design decisions, and implementation details, tiered by importance. All code must respect the invariants.

## ⛔ Protected Files

These files require **explicit user permission** before any modification:
- `docs/design-principles.md` — Project invariants and rules
- `docs/protocol.md` — API contract (human-readable)
- `schema/protocol.json` — API contract (machine-readable)

## Architecture

- `cmd/claude-pool/` — Daemon entry point
- `internal/` — Daemon packages (pool, pty, api, attach, discovery, paths)
- `tests/integration/` — Integration tests (real Claude sessions, `--model haiku`)
- `schema/` — JSON Schema contract for the socket protocol (source of truth)
- `hooks/` — Claude Code hook scripts (pool-aware via env vars)
- `docs/` — Documentation

Key docs:
- [docs/design-principles.md](docs/design-principles.md) — **Invariants and rules** (read first)
- [docs/architecture.md](docs/architecture.md) — Component overview, multi-pool access
- [docs/protocol.md](docs/protocol.md) — Socket API summary
- [schema/protocol.json](schema/protocol.json) — Socket API contract (machine-readable)
- [docs/extraction-plan.md](docs/extraction-plan.md) — Implementation plan, OC reference notes
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

## Origin

Designed from requirements, not copied from Open Cockpit. Open Cockpit code is a reference for patterns and edge cases. See [docs/extraction-plan.md](docs/extraction-plan.md).
