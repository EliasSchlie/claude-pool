#!/bin/bash
# shellcheck disable=SC2034  # variables are used by sourcing scripts
# Shared constants and helpers for claude-pool hooks (local deployment).
#
# Usage: source "$(dirname "$0")/common.sh"
#
# This is the local-deploy variant — no deferral check, because these hooks
# ARE the local hooks. Silently exits for non-pool sessions only.

set -euo pipefail
umask 077

# Guard: exit silently for non-pool sessions
if [ -z "${CLAUDE_POOL_DIR:-}" ]; then
    exit 0
fi

POOL_DIR="$CLAUDE_POOL_DIR"
SESSION_PIDS_DIR="$POOL_DIR/session-pids"
SIGNAL_DIR="$POOL_DIR/idle-signals"

# Extract a JSON string value using sed (avoids python3 startup overhead).
json_get() {
    echo "$1" | sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}
