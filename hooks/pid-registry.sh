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
[ -d "$REGISTRY_DIR" ] || mkdir -p "$REGISTRY_DIR"

input=""
read -t 1 -r input 2>/dev/null || true

# Extract session_id using sed (avoids python3 startup overhead)
session_id=$(echo "$input" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || true

[ -z "$session_id" ] && exit 0

# $PPID is the Claude process that spawned this hook
echo "$session_id" > "$REGISTRY_DIR/$PPID"

# Write to pool-local session-pids. Serves as fallback UUID source when
# SessionStart hooks don't fire (concurrent spawn race). Cleared by the
# daemon on slot clear, so stale UUIDs don't persist across sessions.
if [ -n "${CLAUDE_POOL_DIR:-}" ]; then
    [ -d "$CLAUDE_POOL_DIR/session-pids" ] || mkdir -p "$CLAUDE_POOL_DIR/session-pids"
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
