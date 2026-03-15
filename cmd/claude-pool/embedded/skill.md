---
name: claude-pool
description: Use when you need to manage pools of Claude Code sessions — parallel agents, background workers, or batch processing.
---

# claude-pool — Session Pool Management

Use `claude-pool-cli` to manage pools of Claude Code sessions. Pools handle spawning, queuing, offloading, and restoring sessions automatically.

## When to Use

- **Use claude-pool** for: parallel sub-agents, background tasks, batch processing, any workflow needing multiple Claude sessions
- **Use direct Claude** for: single-session work that doesn't need concurrency

## Quick Reference

```bash
# Pool lifecycle
claude-pool-cli init --size 4                   # Initialize pool with 4 slots
claude-pool-cli health                          # Pool health report
claude-pool-cli resize --size 8                 # Grow/shrink pool
claude-pool-cli destroy --confirm               # Tear down pool

# Start sessions
claude-pool-cli start --prompt "your prompt"    # Start new session (may queue if full)
claude-pool-cli start --prompt "prompt" --block # Start + wait for result

# Monitor and interact
claude-pool-cli wait --session <id>             # Wait for session to become idle
claude-pool-cli wait                            # Wait for any owned session
claude-pool-cli capture --session <id>          # Get output immediately
claude-pool-cli ls                              # List your sessions
claude-pool-cli ls --verbosity nested           # Show parent-child hierarchy
claude-pool-cli info --session <id>             # Full session details

# Follow-up and control
claude-pool-cli followup --session <id> --prompt "prompt"  # Send to idle session
claude-pool-cli stop --session <id>             # Interrupt processing session
claude-pool-cli archive --session <id>          # Mark as done
claude-pool-cli archive --session <id> --recursive  # Archive with descendants
```

## Workflow Example

```bash
# 1. Start parallel tasks
S1=$(claude-pool-cli start --prompt "review src/auth.go for security issues" --json | jq -r .sessionId)
S2=$(claude-pool-cli start --prompt "write tests for src/api/handlers.go" --json | jq -r .sessionId)

# 2. Wait for both
claude-pool-cli wait --session "$S1"
claude-pool-cli wait --session "$S2"

# 3. Get results
claude-pool-cli capture --session "$S1"
claude-pool-cli capture --session "$S2"

# 4. Clean up
claude-pool-cli archive --session "$S1"
claude-pool-cli archive --session "$S2"
```

## Output Capture

Commands that return output (`wait`, `capture`, `start --block`, `followup --block`) accept three flags:

| Flag | Values | Default | Description |
|------|--------|---------|-------------|
| `--source` | `jsonl`, `buffer` | `jsonl` | Where to read from. `buffer` requires live session. |
| `--turns` | integer | `1` | How many turns back (`0` = all). |
| `--detail` | `last`, `assistant`, `tools`, `raw` | `last` | What to include per turn (JSONL only). |

## Pool Selection

```bash
claude-pool-cli --pool work start --prompt "..."  # Target a named pool
claude-pool-cli --pool default ls                  # Default pool
claude-pool-cli pools                              # List known pools
```

## Parent-Child Sessions

Sessions spawned from within a pool session automatically track their parent. `ls` shows only your direct children by default. Use `--parent none` from a Claude session to see all sessions.

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a slot |
| `idle` | Ready for input |
| `processing` | Working |
| `offloaded` | Saved, not in a slot |
| `error` | Repeatedly failed to load |
| `archived` | Done, hidden from `ls` |
