package cli

// helpers_test.go — Shared CLI test infrastructure
//
// Similar to integration test helpers but operates at the CLI level:
//   - setupCLIPool: build both daemon and CLI binaries, start daemon, verify connectivity
//   - run: execute CLI commands as subprocesses, capture stdout/stderr/exit code
//   - runJSON: like run but parses stdout as JSON
//   - assertion helpers reused from integration tests where possible

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type Msg = map[string]any

type cliPool struct {
	t          *testing.T
	poolDir    string
	socketPath string
	daemonBin  string
	cliBin     string
	daemon     *exec.Cmd
	poolName   string
}

type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// setupCLIPool builds both binaries, starts the daemon, calls init via socket,
// and returns a cliPool ready for CLI commands.
func setupCLIPool(t *testing.T, size int) *cliPool {
	t.Helper()

	repoRoot := findRepoRoot(t)

	// Build daemon binary
	binDir := t.TempDir()
	daemonBin := filepath.Join(binDir, "claude-pool")
	build := exec.Command("go", "build", "-o", daemonBin, "./cmd/claude-pool")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build daemon: %v\n%s", err, out)
	}

	// Build CLI binary
	cliBin := filepath.Join(binDir, "claude-pool-cli")
	build = exec.Command("go", "build", "-o", cliBin, "./cmd/claude-pool-cli")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build CLI: %v\n%s", err, out)
	}

	// Create temp pool directory and registry
	poolDir := t.TempDir()
	socketPath := filepath.Join(poolDir, "api.sock")
	poolName := "test"

	// Write pool config
	configPath := filepath.Join(poolDir, "config.json")
	config := Msg{
		"flags": "--dangerously-skip-permissions --model haiku",
		"size":  size,
	}
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Write registry so CLI can resolve pool name → socket
	registryDir := t.TempDir()
	registry := Msg{
		poolName: Msg{"socket": socketPath},
	}
	registryBytes, _ := json.Marshal(registry)
	registryPath := filepath.Join(registryDir, "pools.json")
	if err := os.WriteFile(registryPath, registryBytes, 0644); err != nil {
		t.Fatalf("failed to write registry: %v", err)
	}

	// Start daemon
	daemon := exec.Command(daemonBin, "--pool-dir", poolDir)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}

	// Wait for socket
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(socketPath); err != nil {
		daemon.Process.Kill()
		t.Fatalf("daemon socket never appeared at %s", socketPath)
	}

	p := &cliPool{
		t:          t,
		poolDir:    poolDir,
		socketPath: socketPath,
		daemonBin:  daemonBin,
		cliBin:     cliBin,
		daemon:     daemon,
		poolName:   poolName,
	}

	// Cleanup
	t.Cleanup(func() {
		// Best-effort destroy via socket
		if conn, err := net.Dial("unix", socketPath); err == nil {
			data, _ := json.Marshal(Msg{"type": "destroy", "confirm": true})
			conn.Write(append(data, '\n'))
			conn.Close()
		}
		daemon.Process.Kill()
		daemon.Wait()
	})

	// Init via socket (CLI init may not exist yet)
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		daemon.Process.Kill()
		t.Fatalf("failed to connect: %v", err)
	}
	initMsg, _ := json.Marshal(Msg{"type": "init", "size": size})
	conn.Write(append(initMsg, '\n'))
	scanner := bufio.NewScanner(conn)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if scanner.Scan() {
		var resp Msg
		json.Unmarshal(scanner.Bytes(), &resp)
		if resp["type"] == "error" {
			conn.Close()
			t.Fatalf("init failed: %v", resp["error"])
		}
	}
	conn.Close()

	// Set env for CLI commands to find the registry
	p.t.Setenv("CLAUDE_POOL_REGISTRY", registryPath)

	return p
}

// run executes a CLI command and returns stdout, stderr, and exit code.
func (p *cliPool) run(args ...string) cmdResult {
	p.t.Helper()
	fullArgs := append([]string{"--pool", p.poolName}, args...)
	cmd := exec.Command(p.cliBin, fullArgs...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CLAUDE_POOL_REGISTRY=%s", os.Getenv("CLAUDE_POOL_REGISTRY")),
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			p.t.Fatalf("failed to run CLI: %v", err)
		}
	}

	return cmdResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
}

// runJSON executes a CLI command with --json and parses stdout.
func (p *cliPool) runJSON(args ...string) Msg {
	p.t.Helper()
	result := p.run(append(args, "--json")...)
	if result.ExitCode != 0 {
		p.t.Fatalf("CLI exited %d: %s", result.ExitCode, result.Stderr)
	}
	var msg Msg
	if err := json.Unmarshal([]byte(result.Stdout), &msg); err != nil {
		p.t.Fatalf("failed to parse CLI JSON output: %v\nstdout: %s", err, result.Stdout)
	}
	return msg
}

// runInSession executes a CLI command with CLAUDE_POOL_SESSION_ID set,
// simulating a call from within a pool session.
func (p *cliPool) runInSession(sessionID string, args ...string) cmdResult {
	p.t.Helper()
	fullArgs := append([]string{"--pool", p.poolName}, args...)
	cmd := exec.Command(p.cliBin, fullArgs...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CLAUDE_POOL_REGISTRY=%s", os.Getenv("CLAUDE_POOL_REGISTRY")),
		fmt.Sprintf("CLAUDE_POOL_SESSION_ID=%s", sessionID),
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			p.t.Fatalf("failed to run CLI: %v", err)
		}
	}

	return cmdResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
}

// runInSessionJSON is runInSession + JSON parsing.
func (p *cliPool) runInSessionJSON(sessionID string, args ...string) Msg {
	p.t.Helper()
	result := p.runInSession(sessionID, append(args, "--json")...)
	if result.ExitCode != 0 {
		p.t.Fatalf("CLI exited %d: %s", result.ExitCode, result.Stderr)
	}
	var msg Msg
	if err := json.Unmarshal([]byte(result.Stdout), &msg); err != nil {
		p.t.Fatalf("failed to parse CLI JSON output: %v\nstdout: %s", err, result.Stdout)
	}
	return msg
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod)")
		}
		dir = parent
	}
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
