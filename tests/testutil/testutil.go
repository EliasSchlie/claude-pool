// Package testutil provides shared helpers for test infrastructure.
// Used by TestMain in each test package — not for use inside individual tests.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
