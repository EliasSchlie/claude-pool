# Claude Pool

Managed pool of Claude Code sessions — daemon, CLI, and socket API.

Claude Pool runs a persistent daemon that manages a pool of pre-started Claude Code sessions. Sessions are automatically kept alive, offloaded when idle, and restored on demand. Any number of clients can connect to the same pool simultaneously.

## Status

🚧 **Early development** — extracting from [Open Cockpit](https://github.com/EliasSchlie/open-cockpit).

## Architecture

```
┌─────────────────────────────────────────────┐
│  Clients (connect via Unix socket)          │
│  • claude-pool CLI                          │
│  • Open Cockpit (Electron app)              │
│  • Python package (planned)                 │
│  • Any tool speaking newline-delimited JSON │
├═══════════════════ socket ══════════════════┤
│  Claude Pool daemon                         │
│  • Pool lifecycle (init/resize/destroy)     │
│  • Slot management + LRU eviction           │
│  • PTY daemon orchestration                 │
│  • Session offload/restore/archive          │
│  • Named pools                              │
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

# Named pools
claude-pool --pool=work pool init 3
claude-pool --pool=work start "review the PR"

# Observe sessions
claude-pool ls
claude-pool screen @2
claude-pool watch @2

# Interact
claude-pool followup @1 "add tests"
claude-pool wait @1
claude-pool result @1
```

## Protocol

Newline-delimited JSON over Unix socket. See [docs/protocol.md](docs/protocol.md).

```
-> {"type":"pool-start","prompt":"fix the bug","id":1}
<- {"type":"started","sessionId":"abc-123","slotIndex":0,"id":1}
```

## License

MIT
