---
name: claude-pool
description: Use when you need to manage pools of Claude Code sessions — parallel agents, background workers, or batch processing.
---

# claude-pool — Session Pool Management

Use `claude-pool` to manage pools of Claude Code sessions. Pools handle spawning, queuing, offloading, and restoring sessions automatically.

## When to Use

- **Use claude-pool** for: parallel sub-agents, background tasks, batch processing, any workflow needing multiple Claude sessions
- **Use direct Claude** for: single-session work that doesn't need concurrency

## Quick Reference

```bash
# Pool lifecycle
claude-pool init 4                          # Initialize pool with 4 slots
claude-pool health                          # Pool health report
claude-pool resize 8                        # Grow/shrink pool
claude-pool destroy                         # Tear down pool

# Start sessions
claude-pool start "your prompt here"        # Start new session (may queue if full)
claude-pool start "prompt" --block          # Start + wait for result

# Monitor and interact
claude-pool wait <sessionId>                # Wait for session to become idle
claude-pool wait                            # Wait for any owned session
claude-pool capture <sessionId>             # Get output immediately
claude-pool ls                              # List your sessions
claude-pool ls --tree                       # Show parent-child hierarchy
claude-pool info <sessionId>                # Full session details

# Follow-up and control
claude-pool followup <sessionId> "prompt"   # Send follow-up to idle session
claude-pool stop <sessionId>                # Interrupt processing session
claude-pool offload <sessionId>             # Free slot (session preserved)
claude-pool archive <sessionId>             # Mark as done

# Pin to prevent auto-offload
claude-pool pin <sessionId> 300             # Pin for 300s (default 120s)
claude-pool unpin <sessionId>
```

## Workflow Example

```bash
# 1. Start parallel tasks
S1=$(claude-pool start "review src/auth.go for security issues" --json | jq -r .sessionId)
S2=$(claude-pool start "write tests for src/api/handlers.go" --json | jq -r .sessionId)

# 2. Wait for both
claude-pool wait "$S1"
claude-pool wait "$S2"

# 3. Get results
claude-pool capture "$S1"
claude-pool capture "$S2"

# 4. Clean up
claude-pool archive "$S1"
claude-pool archive "$S2"
```

## Output Formats

Use `--format` with `wait`, `capture`, `start --block`:

| Format | Description |
|--------|-------------|
| `jsonl-short` | Assistant messages since last prompt **(default)** |
| `jsonl-last` | Last assistant message only |
| `jsonl-long` | Full JSONL, repetitive fields stripped |
| `buffer-full` | Raw terminal scrollback (live sessions only) |

## Pool Selection

```bash
claude-pool --pool work start "..."     # Target a named pool
claude-pool --pool default ls           # Default pool
claude-pool pools                       # List known pools
```

## Parent-Child Sessions

Sessions spawned from within a pool session automatically track their parent. `ls` shows only your direct children by default. Use `--all` to see everything.

## Session States

| State | Meaning |
|-------|---------|
| `queued` | Waiting for a slot |
| `idle` | Ready for input |
| `processing` | Working |
| `offloaded` | Saved, not in a slot |
| `archived` | Done, hidden from `ls` |
