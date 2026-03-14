// Package hookfiles embeds hook scripts shared by global install and local pool deployment.
package hookfiles

import "embed"

// Global contains hook scripts for global installation (~/.claude-pool/hooks/).
// The common.sh in this set defers to local hooks if they exist in the pool dir.
//
//go:embed common.sh idle-signal.sh session-pid-map.sh
var Global embed.FS

// LocalScripts contains hook scripts for per-pool local deployment.
// The common.sh in this set does NOT defer — these ARE the local hooks.
// idle-signal.sh and session-pid-map.sh are shared (same file, different embed name).
//
//go:embed common-local.sh idle-signal.sh session-pid-map.sh
var LocalScripts embed.FS

// LocalSettings is the settings.json template for local hook deployment.
// Commands use ${CLAUDE_POOL_DIR} which the shell expands at runtime.
//
//go:embed settings-local.json
var LocalSettings []byte
