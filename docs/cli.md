# CLI Reference

The CLI is a **separate package** (`claude-pool-cli`). It routes commands to pool daemons via their sockets, reading `~/.claude-pool/pools.json` to resolve pool names.

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pool <name>` | `default` | Target a named pool from the registry |
| `--json` | false | Machine-readable JSON output |

## Pool Lifecycle

`pools` and `init` (when creating a new pool) operate directly on the filesystem and daemon processes — they are CLI-only and don't go through the socket API.

```bash
claude-pool-cli init --size 4                    # Initialize pool with 4 slots (starts daemon, registers in pools.json)
claude-pool-cli init --flags "--dangerously-skip-permissions"  # Set Claude CLI flags
claude-pool-cli init --dir ~/projects             # Set pool home directory
claude-pool-cli init --keep-fresh 2              # Maintain 2 fresh slots
claude-pool-cli init --no-restore                # Skip restoring previous sessions
claude-pool-cli health                           # Pool health report
claude-pool-cli resize --size 8                  # Change slot count
claude-pool-cli config                           # Read current config
claude-pool-cli config --set flags="--dangerously-skip-permissions"
claude-pool-cli config --set size=8
claude-pool-cli destroy --confirm                # Kill all sessions, daemon exits (config persists)
claude-pool-cli ping                             # Health check
claude-pool-cli pools                            # List known pools from registry
```

## Interaction

```bash
# Start sessions
claude-pool-cli start --prompt "fix the login bug"           # Start new session (may queue)
claude-pool-cli start --prompt "fix the bug" --block         # Start + wait for result
claude-pool-cli start --prompt "..." --parent none           # No parent (disable auto-detection)

# Follow-up
claude-pool-cli followup --session <id> --prompt "add tests" # Send to idle/offloaded session
claude-pool-cli followup --session <id> --prompt "..." --block

# Wait for completion
claude-pool-cli wait --session <id>              # Wait for specific session
claude-pool-cli wait                             # Wait for any owned busy session
claude-pool-cli wait --parent none               # Wait for any session with no parent
claude-pool-cli wait --timeout 60000             # Custom timeout (ms, default 300000)

# Get output
claude-pool-cli capture --session <id>           # Get output immediately
claude-pool-cli capture --session <id> --source buffer  # Raw terminal output (live only)
claude-pool-cli capture --session <id> --turns 0 --detail tools  # Full history with tool calls

# Control
claude-pool-cli stop --session <id>              # Interrupt processing or cancel queued
```

## Session Management

```bash
# List sessions
claude-pool-cli ls                               # List owned direct children (flat)
claude-pool-cli ls --verbosity nested            # Show descendants as nested tree
claude-pool-cli ls --verbosity full              # All fields including children
claude-pool-cli ls --parent none                 # Show all sessions (bypass auto-filter)
claude-pool-cli ls --status idle,processing      # Filter by status
claude-pool-cli ls --archived                    # Include archived sessions

# Session details
claude-pool-cli info --session <id>              # Full session details (default verbosity: full)
claude-pool-cli info --session <id> --verbosity flat  # Minimal fields

# Lifecycle
claude-pool-cli archive --session <id>           # Mark as done
claude-pool-cli archive --session <id> --recursive  # Archive with all descendants
claude-pool-cli unarchive --session <id>         # Restore archived session (→ offloaded)
```

## Debug Commands

Debug commands are for troubleshooting — not part of normal workflows.

```bash
claude-pool-cli debug input --session <id> --data "\x03"  # Send raw bytes (e.g. Ctrl+C)
claude-pool-cli debug capture --slot 0                      # Raw terminal buffer from slot
claude-pool-cli debug capture --slot 0 --raw                # With ANSI escape codes
claude-pool-cli debug slots                                 # Show slot states + mappings
claude-pool-cli debug logs                                  # Tail daemon log (last 50 lines)
claude-pool-cli debug logs --lines 100 --follow             # Stream new entries
```

## Session Targeting

| Format | Example | Description |
|--------|---------|-------------|
| Full internal ID | `a7f2x9` | Pool-assigned session ID |
| Prefix | `a7f` | Auto-resolves if unique match |

## Output Capture

Commands that return session output (`wait`, `capture`, `start --block`, `followup --block`) accept three flags:

### `--source` — where to read from

| Value | Description | Requires live terminal? |
|-------|-------------|------------------------|
| `jsonl` **(default)** | Claude Code's JSONL transcript (via UUID). Works for any session with a known UUID — including offloaded and archived. | No |
| `buffer` | Raw terminal scrollback, ANSI stripped. | Yes |

### `--turns` — how far back to look

Integer. Default: `1`.

- `1` — last turn only (default)
- `N` — last N turns
- `0` — entire history

### `--detail` — what to include per turn (JSONL only)

| Value | Description |
|-------|-------------|
| `last` **(default)** | User prompt + final assistant response per turn. No tool calls. |
| `assistant` | User prompt + all assistant text responses per turn. No tool calls. |
| `tools` | User prompt + assistant responses + tool calls/results. No internal metadata. |
| `raw` | Everything unfiltered. |

For buffer source, `--detail` is ignored — buffer is always raw terminal text.
