# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API.

## Status

Early development — designing from requirements, referencing Open Cockpit (`~/projects/open-cockpit/`) for patterns. Not a 1:1 copy.

## ⚠️ Read First

**[docs/design-principles.md](docs/design-principles.md)** — Invariants, design decisions, and implementation details, tiered by importance. All code must respect the invariants.

## Architecture

- `src/` — Daemon source (pool manager, PTY manager, API server, session discovery, attach server)
- `schema/` — JSON Schema contract for the socket protocol (source of truth)
- `hooks/` — Claude Code hook scripts (pool-aware via env vars)
- `docs/` — Documentation

Key docs:
- [docs/design-principles.md](docs/design-principles.md) — **Invariants and rules** (read first)
- [docs/architecture.md](docs/architecture.md) — Component overview, multi-pool access
- [docs/protocol.md](docs/protocol.md) — Socket API summary
- [schema/protocol.json](schema/protocol.json) — Socket API contract (machine-readable)
- [docs/extraction-plan.md](docs/extraction-plan.md) — What to reference from Open Cockpit

## Scope

Claude Pool manages pools of Claude sessions: spawn, offload, restore, prompt, wait, attach.

**Not in scope:** Terminal tabs (claude-term), intention files (Open Cockpit), UI (Open Cockpit), non-pool session discovery (Open Cockpit).

## Related Projects

- **CLI** — Separate package (`claude-pool-cli`). Thin router that resolves pool names from registry to socket connections.
- **claude-term** — Separate project. Persistent terminal tabs for Claude sessions. Independent from claude-pool.
- **Open Cockpit** — Electron app. Depends on claude-pool (via socket) and claude-term. Human interface.

## Origin

Designed from requirements, not copied from Open Cockpit. Open Cockpit code is a reference for patterns and edge cases. See [docs/extraction-plan.md](docs/extraction-plan.md).
