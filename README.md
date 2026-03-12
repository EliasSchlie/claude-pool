# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API.

Claude Pool runs a persistent daemon that manages a pool of pre-started Claude Code sessions. Sessions are automatically kept alive, offloaded when idle, and restored on demand. Any number of clients can connect to the same pool simultaneously.

## Status

🚧 **Early development** — designing from requirements, referencing [Open Cockpit](https://github.com/EliasSchlie/open-cockpit) for patterns.

## Architecture

```
┌─────────────────────────────────────────────┐
│  Clients (connect via Unix socket)          │
│  • claude-pool CLI (separate package)       │
│  • Open Cockpit (Electron app)              │
│  • Python package (planned)                 │
│  • Any tool speaking newline-delimited JSON │
├═══════════════════ socket ══════════════════┤
│  Claude Pool daemon (one per pool)          │
│  • Pool lifecycle (init/resize/destroy)     │
│  • Session management + LRU eviction        │
│  • In-process PTY management                │
│  • Session offload/restore/archive          │
│  • Terminal attachment (raw PTY pipes)       │
└─────────────────────────────────────────────┘
```

See [docs/architecture.md](docs/architecture.md) for full details.

## Planned Usage

```bash
# Start the daemon
claude-pool daemon start

# Initialize a pool with 5 sessions
claude-pool pool init 5

# Send a prompt, get a response
claude-pool start "fix the login bug" --block

# Named pools (each fully independent)
claude-pool --pool=work pool init 3
claude-pool --pool=work start "review the PR"

# Observe sessions
claude-pool ls
claude-pool screen abc123
claude-pool watch abc123

# Interact
claude-pool followup abc123 "add tests"
claude-pool wait abc123
claude-pool result abc123

# Attach to live terminal
claude-pool attach abc123
```

## Protocol

Newline-delimited JSON over Unix socket. See [docs/protocol.md](docs/protocol.md) and [schema/protocol.json](schema/protocol.json).

```
-> {"type":"pool-start","prompt":"fix the bug","id":1}
<- {"type":"started","sessionId":"2947bf12-d307-...","id":1}
```

## Design Principles

See [docs/design-principles.md](docs/design-principles.md) for invariants and rules.

Key principles:
- **Pool isolation is absolute** — each pool is a separate daemon, separate directory, zero shared state
- **Sessions addressed by Claude UUID only** — no slot indices, no internal IDs
- **Pools are uniform** — all sessions run the same flags
- **Socket is the only interface** — no client reads pool files directly

## License

MIT
