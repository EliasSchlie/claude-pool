#!/bin/bash
# shellcheck disable=SC2034  # variables are used by sourcing scripts
# Shared constants and helpers for claude-pool hooks.
#
# Usage: source "$(dirname "$0")/common.sh"
#
# Silently exits if CLAUDE_POOL_DIR is not set — this happens when the hook
# fires in a non-pool Claude session (hooks are global but only act on pool sessions).

set -euo pipefail
umask 077

# Guard: exit silently for non-pool sessions
if [ -z "${CLAUDE_POOL_DIR:-}" ]; then
    exit 0
fi

# Defer to local hooks if deployed (avoids double-firing with global hooks)
if [ -f "$CLAUDE_POOL_DIR/.claude/settings.json" ]; then
    exit 0
fi

POOL_DIR="$CLAUDE_POOL_DIR"
SESSION_PIDS_DIR="$POOL_DIR/session-pids"
SIGNAL_DIR="$POOL_DIR/idle-signals"

# Extract a JSON string value using sed (avoids python3 startup overhead).
json_get() {
    echo "$1" | sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}
