#!/bin/bash
# claude-pool hook runner (plugin version).
# Delegates to pool-local scripts via $CLAUDE_POOL_DIR.
# For non-pool sessions, exits silently.
[ -z "${CLAUDE_POOL_DIR:-}" ] && exit 0
exec bash "$CLAUDE_POOL_DIR/hooks/$1" "${@:2}"
