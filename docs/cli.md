# CLI Reference

The CLI is a **separate package** (`claude-pool-cli`). It routes commands to pool daemons via their sockets, reading `~/.claude-pool/pools.json` to resolve pool names.

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pool <name>` | `default` | Target a named pool from the registry |
| `--all` | false | Show all pool sessions, not just owned ones |
| `--json` | false | Raw JSON output |

## Daemon

```bash
claude-pool daemon start           # Start daemon (foreground)
claude-pool daemon start -d        # Start daemon (background)
claude-pool daemon stop            # Stop daemon
claude-pool daemon status          # Check if running
```

## Pool

```bash
claude-pool init [size]            # Initialize pool (size optional, falls back to config)
claude-pool resize <size>          # Resize pool (shrinks by offloading idle first)
claude-pool destroy                # Stop all sessions, daemon exits (config persists)
claude-pool health                 # Health report
claude-pool config                 # Show current config
claude-pool config set flags "--dangerously-skip-permissions"
claude-pool config set size 8
```

## Sessions

```bash
claude-pool start <prompt>                    # Start new session (may queue)
claude-pool start <prompt> --block            # Start + wait for result
claude-pool followup <sessionId> <prompt>     # Send to idle/offloaded session
claude-pool followup <sessionId> <prompt> --block
claude-pool wait [sessionId]                  # Wait for idle (any owned if omitted)
claude-pool result <sessionId>                # Get output (must be idle)
claude-pool result <sessionId> --source jsonl --detail tools
claude-pool capture <sessionId>               # Get live output
claude-pool capture <sessionId> --source buffer --turns 0
claude-pool stop <sessionId>                  # Interrupt or cancel queued request
claude-pool offload <sessionId>               # Manually offload idle session

claude-pool archive <sessionId>               # Soft-delete session (stops if active)
claude-pool archive <sessionId> --recursive   # Archive session + all descendants
claude-pool unarchive <sessionId>             # Restore archived session (→ offloaded)

claude-pool ls                                # List owned direct children
claude-pool ls --tree                         # List with nested descendants
claude-pool ls --all                          # List all pool sessions
claude-pool ls --archived                     # Include archived sessions
claude-pool ls --idle --processing --queued   # Filter by status
claude-pool info <sessionId>                  # Full session details (includes children + UUID)

claude-pool screen <sessionId>                # Terminal output (ANSI-stripped)
claude-pool screen <sessionId> --raw          # With ANSI codes
claude-pool watch <sessionId> [interval]      # Follow output (default 2s)

claude-pool pin <sessionId> [seconds]         # Prevent auto-offload + priority load (default 120s)
claude-pool pin                               # Allocate + pin fresh session
claude-pool unpin <sessionId>                 # Allow auto-offload
claude-pool set-priority <sessionId> <number> # Set eviction priority (lower = evicted first)
claude-pool attach <sessionId>                # Attach to live terminal (raw PTY I/O)
claude-pool subscribe                         # Stream pool events (status changes, etc.)
claude-pool subscribe --events status_change,session_created
claude-pool subscribe --statuses idle,processing
claude-pool subscribe --sessions a7f,b3k
```

## Low-level

```bash
claude-pool input <sessionId> <data>       # Send raw input
claude-pool key <sessionId> <key>          # Send named key (enter, ctrl-c, etc.)
claude-pool type <sessionId> <text>        # Type text (interprets escapes)
```

## Pool Registry

```bash
claude-pool pools                          # List known pools
claude-pool pools add <name> <socket-path> # Add local pool
claude-pool pools add <name> ssh://...     # Add remote pool
claude-pool pools remove <name>            # Remove from registry
```

## Session Targeting

| Format | Example | Description |
|--------|---------|-------------|
| Full internal ID | `a7f2x9` | Pool-assigned session ID |
| Prefix | `a7f` | Auto-resolves if unique match |

## Output Capture

Commands that return session output (`wait`, `capture`, `result`, `start --block`) accept three flags:

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
