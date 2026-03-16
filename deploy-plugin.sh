#!/bin/bash
# Deploy claude-pool plugin to local Claude Code cache.
# Delegates to the Go install command (single source of truth for versioning).
# Run after editing skills/hooks. Then /reload-plugins in active sessions.
set -euo pipefail
SCRIPT_DIR=$(dirname "$(realpath "$0")")
cd "$SCRIPT_DIR"
go run ./cmd/claude-pool install
