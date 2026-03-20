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

## Install

**Prerequisites:** Go 1.23+, [Claude Code](https://github.com/anthropics/claude-code) installed and on `$PATH`.

```bash
git clone https://github.com/EliasSchlie/claude-pool
cd claude-pool
make install        # builds + symlinks to ~/.local/bin/
claude-pool install # installs global hooks + Claude Code skill (run once per machine)
```

`make install` symlinks `claude-pool` (daemon) and `claude-pool-cli` (CLI) into `~/.local/bin/`. Ensure that directory is on your `$PATH`.

`claude-pool install` writes the global hook runner into `~/.claude-pool/hook-runner.sh`, registers `SessionStart` and `PreToolUse` hooks in `~/.claude/settings.json`, and installs the Claude Code skill at `~/.claude/skills/claude-pool/SKILL.md`. Run `claude-pool uninstall` to reverse.

## Quick Start

```bash
# 1. Initialize a pool (starts daemon, creates 3 slots)
claude-pool-cli init --size 3

# 2. Run a task and wait for the result
claude-pool-cli start --prompt "summarize this repo" --block

# 3. List sessions
claude-pool-cli ls

# 4. Send a follow-up to an existing session
claude-pool-cli followup --session abc123 --prompt "add tests" --block

# 5. Tear down when done
claude-pool-cli destroy --confirm
```

## Usage

### Sending Prompts

```bash
# Start a session and block until done (prints output)
claude-pool-cli start --prompt "fix the login bug" --block

# Start a session in the background; get the session ID
claude-pool-cli start --prompt "refactor auth module"
# → {"sessionId":"a7f2x9","status":"processing"}

# Follow up on an existing session
claude-pool-cli followup --session a7f2x9 --prompt "add error handling" --block

# Wait for any busy session (auto-detects parent when called from Claude Code)
claude-pool-cli wait --timeout 60000
```

### Observing Sessions

```bash
claude-pool-cli ls                          # list active sessions
claude-pool-cli ls --status idle,processing # filter by state
claude-pool-cli ls --verbosity nested       # include child sessions
claude-pool-cli info --session abc123       # full session details
claude-pool-cli capture --session abc123    # get output immediately
```

### Pool Management

```bash
claude-pool-cli health                      # pool status and slot counts
claude-pool-cli resize --size 5             # grow/shrink slot count
claude-pool-cli config --set flags="--model claude-opus-4-5"
claude-pool-cli pools                       # list all known pools
```

### Named Pools

Each pool is fully independent — its own daemon, directory, and config:

```bash
claude-pool-cli --pool work init --size 3
claude-pool-cli --pool work start --prompt "review the PR"
claude-pool-cli --pool scratch init --size 1 --flags "--model claude-haiku-3-5"
```

### Attaching to a Session (Live Terminal)

```bash
# Start without a prompt to claim a slot interactively
claude-pool-cli start
# → {"sessionId":"x9k2m1","status":"idle"}

# Attach for live PTY I/O (like SSH into the session)
claude-pool-cli attach --session x9k2m1
# Disconnect with ~. (tilde-dot on a fresh line)
```

### Session Lifecycle

```bash
claude-pool-cli stop --session abc123       # interrupt or cancel
claude-pool-cli archive --session abc123    # mark done, hide from ls
claude-pool-cli archive --session abc123 --recursive  # include children
claude-pool-cli unarchive --session abc123  # restore archived session
```

### Debug

```bash
claude-pool-cli debug logs --follow         # tail daemon log
claude-pool-cli debug slots                 # slot states and session mapping
claude-pool-cli debug input --session abc123 --data $'\x03'  # send Ctrl+C
```

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a free slot |
| `idle` | Ready — waiting for a prompt |
| `processing` | Claude is working |
| `offloaded` | Not in a slot; resumes automatically on next `followup` |
| `error` | Repeatedly failed to load |
| `archived` | Done. Hidden from `ls` by default. Auto-cleaned after 30 days. |

Offloaded sessions re-queue automatically when targeted by `followup` — no manual intervention needed.

## Output Capture

`start --block`, `followup --block`, `wait`, and `capture` all accept these flags:

| Flag | Values | Default | Description |
|------|--------|---------|-------------|
| `--source` | `jsonl`, `buffer` | `jsonl` | Where to read from — JSONL transcript or raw terminal scrollback |
| `--turns` | `1`, `N`, `0` | `1` | How many turns back (`0` = full history) |
| `--detail` | `last`, `assistant`, `tools`, `raw` | `last` | How much of each turn to include |

Example — get the full conversation including tool calls:

```bash
claude-pool-cli wait --session abc123 --turns 0 --detail tools
```

## Protocol

Newline-delimited JSON over Unix socket at `~/.claude-pool/<name>/api.sock`. See [SPEC.md](SPEC.md) for the full API surface and [docs/protocol.md](docs/protocol.md) for protocol details.

```
-> {"type":"start","prompt":"fix the bug","id":1}
<- {"type":"started","sessionId":"a7f2x9","status":"processing","id":1}

-> {"type":"ping","id":2}
<- {"type":"pong","id":2}
```

All errors: `{"type":"error","error":"human-readable message","id":<echoed>}`.

Direct socket access (no CLI needed):

```bash
echo '{"type":"health"}' | nc -U ~/.claude-pool/default/api.sock
```

## Remote Pools

Pools on remote machines are accessed via SSH tunnel. Register once, then use normally:

```bash
# Add to ~/.claude-pool/pools.json:
# "vps": { "socket": "ssh://user@vps/home/user/.claude-pool/default/api.sock" }

claude-pool-cli --pool vps ls
```

The CLI transparently forwards the Unix socket over SSH — no additional infrastructure required.

## Design Principles

See [SPEC.md](SPEC.md) for invariants and protocol. See [docs/design-principles.md](docs/design-principles.md) for design decisions.

Key principles:
- **Pool isolation is absolute** — each pool is a separate daemon, separate directory, zero shared state
- **Internal session IDs** — pool-assigned short random strings, stable across the full lifecycle
- **Pools are uniform** — all sessions run the same flags; different flags = different pool
- **Socket is the only interface** — no client reads pool files directly

## License

MIT
