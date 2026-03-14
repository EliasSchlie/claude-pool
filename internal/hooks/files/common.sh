#!/bin/bash
# shellcheck disable=SC2034  # variables are used by sourcing scripts
# Shared constants and helpers for claude-pool hooks.
#
# Usage: source "$(dirname "$0")/common.sh"

set -euo pipefail
umask 077

# CLAUDE_POOL_DIR is set by the daemon when spawning sessions.
POOL_DIR="${CLAUDE_POOL_DIR:?CLAUDE_POOL_DIR not set}"
SESSION_PIDS_DIR="$POOL_DIR/session-pids"
SIGNAL_DIR="$POOL_DIR/idle-signals"

# Extract a JSON string value using sed (avoids python3 startup overhead).
json_get() {
    echo "$1" | sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}
