#!/bin/bash
# Deploy claude-pool plugin to local Claude Code cache.
# Rebuilds binaries and installs the plugin.
# Run after editing Go code, skills, or hooks. Then /reload-plugins in active sessions.
set -euo pipefail
SCRIPT_DIR=$(dirname "$(realpath "$0")")
cd "$SCRIPT_DIR"
make build
go run ./cmd/claude-pool install

# Verify deployed hooks match source
echo ""
echo "Verifying deployed hooks..."
CACHE_BASE="$HOME/.claude/plugins/cache/local-tools/claude-pool"
CACHE_DIR=$(find "$CACHE_BASE" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)
if [ -z "$CACHE_DIR" ]; then
    echo "  ⚠️  No cached version found — skipping verification"
    exit 0
fi

ERRORS=0
for f in hooks/*; do
    fname=$(basename "$f")
    if ! diff -q "$f" "$CACHE_DIR/hooks/$fname" >/dev/null 2>&1; then
        echo "  ❌ hooks/$fname differs from cached version"
        ERRORS=$((ERRORS + 1))
    fi
done

if [ "$ERRORS" -gt 0 ]; then
    echo "  ❌ $ERRORS hook file(s) diverged — cache may be stale!"
    exit 1
else
    echo "  ✅ All hook files match source"
fi
