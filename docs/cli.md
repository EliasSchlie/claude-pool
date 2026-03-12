# CLI Reference

The CLI is a **separate package** (`claude-pool-cli`). It routes commands to pool daemons via their sockets, reading `~/.claude-pool/pools.json` to resolve pool names.

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pool <name>` | `default` | Target a named pool from the registry |
| `--all` | false | Show all pool sessions, not just owned ones |

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
claude-pool resize <size>          # Resize pool (new slots use config flags)
claude-pool destroy                # Destroy pool and kill all sessions
claude-pool health                 # Health report
claude-pool config                 # Show current config
claude-pool config set flags "--dangerously-skip-permissions"
claude-pool config set size 8
```

## Sessions

```bash
claude-pool start <prompt>                    # Start new session (may queue)
claude-pool start <prompt> --block            # Start + wait for result
claude-pool start <prompt> --priority 5       # Start with eviction priority
claude-pool followup <sessionId> <prompt>     # Send to idle/offloaded session
claude-pool followup <sessionId> <prompt> --block
claude-pool wait [sessionId]                  # Wait for idle (any owned if omitted)
claude-pool result <sessionId>                # Get output (must be idle)
claude-pool capture <sessionId>               # Get live terminal content
claude-pool stop <sessionId>                  # Interrupt running session (Ctrl+C)
claude-pool offload <sessionId>               # Manually offload idle session

claude-pool ls                                # List owned sessions
claude-pool ls --all                          # List all pool sessions
claude-pool ls --idle --processing --queued   # Filter by status
claude-pool ls --json                         # Raw JSON
claude-pool info <sessionId>                  # Full session details

claude-pool screen <sessionId>                # Terminal output (ANSI-stripped)
claude-pool screen <sessionId> --raw          # With ANSI codes
claude-pool watch <sessionId> [interval]      # Follow output (default 2s)

claude-pool pin <sessionId> [seconds]         # Prevent auto-offload + priority load (default 120s)
claude-pool unpin <sessionId>                 # Allow auto-offload
claude-pool priority <sessionId> <number>     # Set eviction priority (lower = evicted first)
claude-pool attach <sessionId>                # Attach to live terminal (raw PTY I/O)
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
