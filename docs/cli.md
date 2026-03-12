# CLI Reference

The CLI is a **separate package** (`claude-pool-cli`) that routes commands to pool daemons via their sockets. It reads `~/.claude-pool/pools.json` to resolve pool names to socket connections (local or remote).

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pool <name>` | `default` | Target a named pool from the registry |

## Daemon

```bash
claude-pool daemon start           # Start daemon (foreground)
claude-pool daemon start -d        # Start daemon (background)
claude-pool daemon stop            # Stop daemon
claude-pool daemon status          # Check if running
```

## Pool Management

```bash
claude-pool pool init [size]       # Initialize pool (default 5, flags from config)
claude-pool pool init 5 --flags="--dangerously-skip-permissions --model opus"
claude-pool pool status            # Health report
claude-pool pool resize <size>     # Resize pool (new slots use config flags)
claude-pool pool destroy           # Destroy pool and kill all sessions
claude-pool pool config            # Show current config
claude-pool pool config set flags "--dangerously-skip-permissions"
claude-pool pool config set size 8
```

## Session Lifecycle

```bash
claude-pool start <prompt>                  # Start new session
claude-pool start <prompt> --block          # Start + wait for result
claude-pool followup <sessionId> <prompt>   # Send to idle session
claude-pool followup <sessionId> <prompt> --block
claude-pool resume <sessionId>              # Resume offloaded session
claude-pool wait [sessionId]                # Wait for idle (any if no target)
claude-pool result <sessionId>              # Get output (must be idle)
claude-pool capture <sessionId>             # Get live terminal content
claude-pool stop <sessionId>                # Interrupt running session
claude-pool offload <sessionId>             # Manually offload idle session
```

## Observing

```bash
claude-pool ls                     # List sessions (table)
claude-pool ls --idle              # Only idle sessions
claude-pool ls --processing        # Only processing sessions
claude-pool ls --all               # Include archived
claude-pool ls --json              # Raw JSON

claude-pool screen <sessionId>     # Terminal output (ANSI-stripped)
claude-pool screen <sessionId> --raw
claude-pool watch <sessionId> [interval]   # Follow output (default 2s)
```

## Session Management

```bash
claude-pool pin <sessionId> [seconds]      # Prevent auto-offload (default 120s)
claude-pool unpin <sessionId>              # Allow auto-offload
claude-pool archive <sessionId>            # Archive session
claude-pool unarchive <sessionId>          # Restore from archive
```

## Terminal Attachment

```bash
claude-pool attach <sessionId>             # Attach to live terminal (raw PTY I/O)
```

Attach gives you a live terminal stream — like `docker attach`. Pipe closes when session is offloaded or dies.

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

## Targeting Sessions

| Format | Example | Description |
|--------|---------|-------------|
| Full UUID | `2947bf12-d307-...` | Exact Claude session UUID |
| Prefix | `2947b` | Auto-resolves if unique match |
