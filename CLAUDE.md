# Claude Pool

Managed pool of Claude Code sessions — daemon, CLI, and socket API.

## Status

Early development — extracting from [Open Cockpit](https://github.com/EliasSchlie/open-cockpit). No runnable code yet.

## ⚠️ Read First

**[docs/design-principles.md](docs/design-principles.md)** — Invariants, design decisions, and implementation details, tiered by importance. All code must respect the invariants.

## Architecture

- `src/` — Daemon source (pool manager, PTY daemon, API server, session discovery)
- `bin/` — CLI entry point
- `docs/` — Documentation

Key docs:
- [docs/design-principles.md](docs/design-principles.md) — **Invariants and rules** (read first)
- [docs/architecture.md](docs/architecture.md) — Component overview, directory structure
- [docs/protocol.md](docs/protocol.md) — Socket API specification (all commands)
- [docs/cli.md](docs/cli.md) — CLI reference
- [docs/extraction-plan.md](docs/extraction-plan.md) — Migration plan from Open Cockpit

## Origin

Pool logic is being extracted from Open Cockpit (`~/projects/open-cockpit/`). See extraction plan for file mapping.
