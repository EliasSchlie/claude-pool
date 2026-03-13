// Package testutil provides shared helpers for test infrastructure.
// Used by TestMain in each test package — not for use inside individual tests.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FindRepoRoot walks up from cwd to find the directory containing go.mod.
func FindRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fmt.Fprintf(os.Stderr, "could not find repo root (no go.mod)\n")
			os.Exit(1)
		}
		dir = parent
	}
}

// BuildBinary runs `go build` for the given package and writes the binary to outputPath.
// Exits the process on failure (intended for TestMain, not individual tests).
func BuildBinary(repoRoot, outputPath, pkg string) {
	build := exec.Command("go", "build", "-o", outputPath, pkg)
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build %s: %v\n%s\n", pkg, err, out)
		os.Exit(1)
	}
}

// SetupRunDir creates a timestamped run directory for test artifacts at
// <repoRoot>/.run/<prefix>-<timestamp>/. Prunes old runs (>1 hour).
// On success the caller removes the run dir; on failure it stays for inspection.
//
// Keep paths short — Unix sockets have a 104-byte limit on macOS. The full
// path to api.sock must fit: <repoRoot>/.run/<prefix>-<ts>/<TestName>/api.sock
func SetupRunDir(repoRoot, prefix string) string {
	baseDir := filepath.Join(repoRoot, ".run")
	pruneOldRuns(baseDir, time.Hour)

	stamp := time.Now().Format(stampFormat)
	dir := filepath.Join(baseDir, fmt.Sprintf("%s-%s", prefix, stamp))
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create run dir: %v\n", err)
		os.Exit(1)
	}
	return dir
}

const stampFormat = "0102-150405"

// pruneOldRuns removes subdirectories of baseDir whose embedded timestamp
// is older than maxAge. Parses the timestamp from the directory name
// (format: <prefix>-MMDD-HHMMSS) rather than relying on mtime, which can
// be updated by writes to files inside the directory.
func pruneOldRuns(baseDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Extract timestamp after the last "-" prefix separator
		// Dir name format: <prefix>-MMDD-HHMMSS
		name := e.Name()
		if idx := strings.Index(name, "-"); idx >= 0 {
			stamp := name[idx+1:]
			t, err := time.Parse(stampFormat, stamp)
			if err != nil {
				continue
			}
			// Parse gives year 0 — set to current year for comparison
			t = t.AddDate(time.Now().Year(), 0, 0)
			if t.Before(cutoff) {
				os.RemoveAll(filepath.Join(baseDir, name))
			}
		}
	}
}
