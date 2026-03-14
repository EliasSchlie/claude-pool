// Package hooks embeds hook scripts for deployment into pool directories.
package hooks

import "embed"

//go:embed all:files
var Files embed.FS
