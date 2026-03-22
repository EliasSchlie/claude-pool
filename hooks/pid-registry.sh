#!/bin/bash
# Maps Claude process PID → session UUID for parent auto-detection.
#
# Triggered by: PreToolUse (Bash)
# Input (stdin): JSON with session_id
# Output: ~/.claude-pool/pid-registry/<PID> containing the session UUID
#
# NOT gated on CLAUDE_POOL_DIR — fires for ALL Claude sessions so that
# any Claude session calling the pool CLI can be identified as a parent.

set -euo pipefail

REGISTRY_DIR="${HOME}/.claude-pool/pid-registry"
mkdir -p "$REGISTRY_DIR"

input=""
read -t 1 -r input 2>/dev/null || true

# Extract session_id using sed (avoids python3 startup overhead)
session_id=$(echo "$input" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || true

[ -z "$session_id" ] && exit 0

# $PPID is the Claude process that spawned this hook
echo "$session_id" > "$REGISTRY_DIR/$PPID"

# Also write to pool-local session-pids if in a pool session.
# This is the authoritative UUID for the session's actual work
# (unlike SessionStart which fires during /clear with intermediate UUIDs).
if [ -n "${CLAUDE_POOL_DIR:-}" ]; then
    mkdir -p "$CLAUDE_POOL_DIR/session-pids"
    echo "$session_id" > "$CLAUDE_POOL_DIR/session-pids/$PPID"
fi

# Clean up stale entries ~1 in 20 invocations (avoid hot-path overhead)
if (( RANDOM % 20 == 0 )); then
    for f in "$REGISTRY_DIR"/*; do
        [ -f "$f" ] || continue
        pid=$(basename "$f")
        if ! kill -0 "$pid" 2>/dev/null; then
            rm -f "$f"
        fi
    done
fi

exit 0
