// Package hookfiles embeds hook scripts for pool deployment and global install.
//
// Hook architecture: each pool deploys its own scripts on init (pool-dir/hooks/).
// The global install only writes a thin hook-runner.sh that delegates to
// $CLAUDE_POOL_DIR/hooks/ at runtime. This keeps pools fully self-contained —
// different pools can run different hook versions independently.
package hookfiles

import "embed"

// Scripts contains hook scripts deployed to each pool directory on init.
//
//go:embed common.sh idle-signal.sh session-pid-map.sh
var Scripts embed.FS

// HookRunner is the thin global entry point installed to ~/.claude-pool/hook-runner.sh.
// It delegates to pool-local scripts via $CLAUDE_POOL_DIR, exiting silently for
// non-pool sessions.
//
//go:embed hook-runner.sh
var HookRunner []byte
