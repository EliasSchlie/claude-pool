#!/bin/bash
# Deploy claude-pool plugin to local Claude Code cache.
# Rebuilds binaries and installs the plugin.
# Run after editing Go code, skills, or hooks. Then /reload-plugins in active sessions.
set -euo pipefail
SCRIPT_DIR=$(dirname "$(realpath "$0")")
cd "$SCRIPT_DIR"
make build
go run ./cmd/claude-pool install
