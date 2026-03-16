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

## Quick Reference

- **Deploy plugin:** `./deploy-plugin.sh` (bumps version, copies to cache, then `/reload-plugins`)
- **Plugin test:** `claude --plugin-dir .` (loads skill + hooks for one session only)
- **Standalone install:** `./claude-pool install` (fallback — writes directly to `~/.claude/`)

## Architecture

- `.claude-plugin/` — Plugin manifest
- `skills/claude-pool/` — Plugin skill
- `hooks/` — Plugin hooks (hooks.json + hook-runner.sh)
- `cmd/claude-pool/` — Daemon entry point + install/uninstall commands
- `internal/` — Daemon packages (pool, pty, api, attach, discovery, paths, hookfiles)
- `tests/integration/` — Integration tests (real Claude sessions, `--model haiku`)
- `tests/manual/` — Manual testing directory (own `.claude/` hooks, independent per worktree)
- `schema/` — JSON Schema (must match SPEC.md)
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

## Origin

Designed from requirements, not copied from Open Cockpit. Open Cockpit code is a reference for patterns and edge cases.
