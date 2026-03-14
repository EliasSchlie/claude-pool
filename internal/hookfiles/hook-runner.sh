#!/bin/bash
# claude-pool hook runner — registered globally in ~/.claude/settings.json.
#
# Why a runner instead of direct script paths?
#
# Each pool deploys its own hook scripts on init, so different pools (or
# different branches under test) can run different hook versions. But the
# global settings.json entries need a fixed command — they can't point to
# pool-specific paths because the pool dir varies.
#
# Solution: this runner is the fixed entry point. It delegates to the
# pool-local scripts via $CLAUDE_POOL_DIR (set by the daemon on pool
# sessions). For non-pool sessions, it exits silently.
#
# Usage (from settings.json):
#   bash ~/.claude-pool/hook-runner.sh idle-signal.sh write stop
#   bash ~/.claude-pool/hook-runner.sh session-pid-map.sh

[ -z "${CLAUDE_POOL_DIR:-}" ] && exit 0
exec bash "$CLAUDE_POOL_DIR/hooks/$1" "${@:2}"
