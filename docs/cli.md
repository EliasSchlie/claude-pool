# CLI Reference

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--pool <name>` | `default` | Target a named pool |

## Daemon

```bash
claude-pool daemon start           # Start daemon (foreground)
claude-pool daemon start -d        # Start daemon (background)
claude-pool daemon stop            # Stop daemon
claude-pool daemon status          # Check if running
```

## Pool Management

```bash
claude-pool pool init [size]       # Initialize pool (default 5)
claude-pool pool status            # Health report
claude-pool pool resize <size>     # Resize pool
claude-pool pool destroy           # Destroy pool and kill all sessions
claude-pool pool clean             # Offload + archive all idle sessions
```

## Session Lifecycle

```bash
claude-pool start <prompt>                  # Start new session
claude-pool start <prompt> --block          # Start + wait for result
claude-pool followup <target> <prompt>      # Send to idle session
claude-pool followup <target> <prompt> --block  # Send + wait for result
claude-pool resume <target>                 # Resume offloaded session
claude-pool wait [target]                   # Wait for idle (any if no target)
claude-pool result <target>                 # Get output (must be idle)
claude-pool capture <target>                # Get live terminal content
claude-pool stop <target>                   # Interrupt running session
```

## Observing

```bash
claude-pool ls                     # List sessions (table)
claude-pool ls --idle              # Only idle sessions
claude-pool ls --processing        # Only processing sessions
claude-pool ls --all               # Include archived
claude-pool ls --json              # Raw JSON

claude-pool screen <target>        # Terminal output (ANSI-stripped)
claude-pool screen <target> --raw  # With ANSI codes
claude-pool watch <target> [interval]  # Follow output (default 2s)
claude-pool log <target> [lines]   # Conversation turns (default 20)
```

## Session Management

```bash
claude-pool pin <target> [seconds]     # Prevent offload (default 120s)
claude-pool unpin <target>             # Allow offload
claude-pool archive <target>           # Archive session
claude-pool unarchive <target>         # Restore from archive
claude-pool intention <target>         # Read intention
claude-pool intention <target> "text"  # Write intention
```

## Low-level

```bash
claude-pool input <target> <data>      # Send raw input
claude-pool key <target> <key>         # Send named key (enter, ctrl-c, etc.)
claude-pool type <target> <text>       # Type text (interprets escapes)
claude-pool slot read <index>          # Read slot buffer
claude-pool slot write <index> <data>  # Write to slot
claude-pool slot status <index>        # Slot details
```

## Session Terminals (shell tabs)

```bash
claude-pool term ls <target>                   # List tabs
claude-pool term read <target> <tabIndex>      # Read tab content
claude-pool term write <target> <tabIndex> <data>  # Write to tab
claude-pool term open <target> [cwd]           # Open new shell tab
claude-pool term close <target> <tabIndex>     # Close tab
claude-pool term run <target> <tabIndex> <cmd> # Run command, return output
claude-pool term exec <target> <cmd>           # Ephemeral: open → run → close
```

## Targeting Sessions

| Format | Example | Description |
|--------|---------|-------------|
| Full UUID | `2947bf12-d307-...` | Exact session ID |
| Prefix | `2947b` | Auto-resolves if unique |
| `@N` | `@0`, `@3` | Pool slot index |
