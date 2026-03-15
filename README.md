# Claude Pool

Managed pool of Claude Code sessions — daemon and socket API.

Claude Pool runs a persistent daemon that manages a pool of pre-started Claude Code sessions. Sessions are automatically kept alive, offloaded when idle, and restored on demand. Any number of clients can connect to the same pool simultaneously.

## Status

🚧 **In development** — core functionality working, integration-tested with real Claude sessions.

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
│  • In-process PTY management (creack/pty)   │
│  • Session offload/restore                  │
│  • Terminal attachment (raw PTY pipes)       │
└─────────────────────────────────────────────┘
```

See [docs/architecture.md](docs/architecture.md) for full details.

## Usage

```bash
# Initialize a pool with 5 slots (starts daemon, registers pool)
claude-pool-cli init --size 5

# Send a prompt, get a response
claude-pool-cli start --prompt "fix the login bug" --block

# Named pools (each fully independent)
claude-pool-cli --pool work init --size 3
claude-pool-cli --pool work start --prompt "review the PR"

# Observe sessions
claude-pool-cli ls
claude-pool-cli info --session abc123

# Interact
claude-pool-cli followup --session abc123 --prompt "add tests"
claude-pool-cli wait --session abc123
claude-pool-cli capture --session abc123
```

## Protocol

Newline-delimited JSON over Unix socket. See [SPEC.md](SPEC.md) for the API surface and [docs/protocol.md](docs/protocol.md) for full protocol details.

```
-> {"type":"start","prompt":"fix the bug","id":1}
<- {"type":"started","sessionId":"a7f2x9","status":"processing","id":1}
```

## Design Principles

See [SPEC.md](SPEC.md) for invariants and protocol. See [docs/design-principles.md](docs/design-principles.md) for design decisions.

Key principles:
- **Pool isolation is absolute** — each pool is a separate daemon, separate directory, zero shared state
- **Internal session IDs** — pool-assigned short random strings, stable across lifecycle
- **Pools are uniform** — all sessions run the same flags
- **Socket is the only interface** — no client reads pool files directly

## License

MIT
