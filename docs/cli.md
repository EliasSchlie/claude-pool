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
claude-pool followup <sessionId> <prompt>     # Send to idle/offloaded/dead session
claude-pool followup <sessionId> <prompt> --block
claude-pool wait [sessionId]                  # Wait for idle (any owned if omitted)
claude-pool result <sessionId>                # Get output (must be idle)
claude-pool result <sessionId> --format jsonl-long
claude-pool capture <sessionId>               # Get live output
claude-pool capture <sessionId> --format buffer-full
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

## Output Formats

The `--format` flag is supported by `wait`, `capture`, `result`, and `start --block`:

| Format | Description | Requires live terminal |
|--------|-------------|----------------------|
| `jsonl-last` | Last assistant message only | No (reads JSONL transcript) |
| `jsonl-short` | All assistant messages since last user message **(default)** | No (reads JSONL transcript) |
| `jsonl-long` | Full JSONL since last user message, repetitive fields stripped | No (reads JSONL transcript) |
| `jsonl-full` | Complete unfiltered JSONL transcript | No (reads JSONL transcript) |
| `buffer-last` | Terminal buffer since last user message | Yes |
| `buffer-full` | Full terminal scrollback, ANSI stripped | Yes |

JSONL formats read from Claude Code's own transcript files (located via UUID), so they work for any session state. Buffer formats require a live terminal — they fail for offloaded/archived sessions.
