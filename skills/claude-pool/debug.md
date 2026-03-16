# Debug & Advanced Commands

For troubleshooting pool issues or tuning session eviction behavior.

## Session Properties

```bash
claude-pool-cli set --session <id> --priority 5         # Eviction priority (lower = evicted first, default: 0)
claude-pool-cli set --session <id> --pinned 3600         # Pin for N seconds (protected from eviction)
claude-pool-cli set --session <id> --pinned false        # Unpin
claude-pool-cli set --session <id> --meta env=staging    # Set metadata (repeatable)
```

## Debug Commands

All under `claude-pool-cli debug <command>`.

```bash
claude-pool-cli debug slots                              # Show slot states + slot↔session mappings
claude-pool-cli debug logs                               # Tail daemon log (last 50 lines)
claude-pool-cli debug logs --lines 200 --follow          # More lines + stream
claude-pool-cli debug input --session <id> --data "text" # Send raw bytes to PTY (use followup for prompts)
claude-pool-cli debug capture --slot 0                   # Raw terminal buffer from slot (ANSI stripped)
claude-pool-cli debug capture --slot 0 --raw             # With ANSI escape codes
```
