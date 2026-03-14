#!/bin/bash
# Maps Claude process PID → session ID.
#
# Triggered by: SessionStart
# Input (stdin): JSON with session_id
# Output: $POOL_DIR/session-pids/<PID> containing the session ID

source "$(dirname "$0")/common.sh"

mkdir -p "$SESSION_PIDS_DIR"

input=""
read -t 1 -r input 2>/dev/null || true
session_id=$(json_get "$input" "session_id") || true

[ -z "$session_id" ] && exit 0

echo "$session_id" > "$SESSION_PIDS_DIR/$PPID"

# Clean up stale entries (PIDs that no longer exist)
for f in "$SESSION_PIDS_DIR"/*; do
    [ -f "$f" ] || continue
    pid=$(basename "$f")
    if ! kill -0 "$pid" 2>/dev/null; then
        rm -f "$f"
    fi
done

exit 0
