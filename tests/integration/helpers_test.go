package integration

// helpers_test.go — Shared test infrastructure
//
// Two pool types for two kinds of tests:
//
//   cliPool — For tests that exercise the CLI (pool, session, slots, offload, parent_child).
//     Setup: build daemon + CLI binaries, run `init` via CLI, which starts the daemon.
//     Operations: run CLI commands as subprocesses, parse JSON output.
//     Waiting: poll `info --json` for status, use `wait` CLI command for idle.
//
//   testPool — For tests that need raw socket access (attach, subscribe).
//     Setup: start daemon directly, connect socket, send init.
//     Operations: send JSON messages on socket, read responses.
//     Kept because attach and subscribe are API-only features not exposed in the CLI.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliasSchlie/claude-pool/tests/testutil"
)

// Built once by TestMain and shared across all test functions.
var (
	daemonBinPath string
	cliBinPath    string
	runDir        string
)

func TestMain(m *testing.M) {
	repoRoot := testutil.FindRepoRoot()
	runDir = testutil.SetupRunDir(repoRoot, "integ")

	// Build daemon and CLI in parallel
	daemonBinPath = filepath.Join(runDir, "claude-pool")
	cliBinPath = filepath.Join(runDir, "claude-pool-cli")

	type buildResult struct {
		name string
		out  []byte
		err  error
	}
	ch := make(chan buildResult, 2)

	for _, target := range []struct{ name, bin, pkg string }{
		{"daemon", daemonBinPath, "./cmd/claude-pool"},
		{"CLI", cliBinPath, "./cmd/claude-pool-cli"},
	} {
		target := target
		go func() {
			build := exec.Command("go", "build", "-o", target.bin, target.pkg)
			build.Dir = repoRoot
			out, err := build.CombinedOutput()
			ch <- buildResult{target.name, out, err}
		}()
	}

	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "failed to build %s: %v\n%s\n", r.name, r.err, r.out)
			os.Exit(1)
		}
	}

	code := m.Run()
	if code == 0 {
		os.RemoveAll(runDir)
	} else {
		fmt.Fprintf(os.Stderr, "\n=== Test artifacts preserved at: %s ===\n", runDir)
	}
	os.Exit(code)
}

// --------------------------------------------------------------------
// JSON message types
// --------------------------------------------------------------------

type Msg = map[string]any

// SessionInfo holds parsed session fields from info/ls responses.
type SessionInfo struct {
	SessionID    string
	ClaudeUUID   string
	Status       string
	Parent       string
	Priority     float64
	Cwd          string
	SpawnCwd     string
	CreatedAt    string
	PID          float64
	Pinned       bool
	PendingInput string
	Metadata     map[string]string
	Children     []SessionInfo
}

func parseSession(t *testing.T, raw any) SessionInfo {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected session object, got %T", raw)
	}
	s := SessionInfo{
		SessionID:    strVal(m, "sessionId"),
		ClaudeUUID:   strVal(m, "claudeUUID"),
		Status:       strVal(m, "status"),
		Parent:       strVal(m, "parent"),
		Priority:     numVal(m, "priority"),
		Cwd:          strVal(m, "cwd"),
		SpawnCwd:     strVal(m, "spawnCwd"),
		CreatedAt:    strVal(m, "createdAt"),
		PID:          numVal(m, "pid"),
		Pinned:       boolVal(m, "pinned"),
		PendingInput: strVal(m, "pendingInput"),
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		s.Metadata = make(map[string]string, len(meta))
		for k, v := range meta {
			if sv, ok := v.(string); ok {
				s.Metadata[k] = sv
			}
		}
	}
	if kids, ok := m["children"].([]any); ok {
		for _, k := range kids {
			s.Children = append(s.Children, parseSession(t, k))
		}
	}
	return s
}

func parseSessions(t *testing.T, resp Msg) []SessionInfo {
	t.Helper()
	raw, ok := resp["sessions"].([]any)
	if !ok {
		t.Fatalf("expected sessions array, got %T", resp["sessions"])
	}
	var sessions []SessionInfo
	for _, r := range raw {
		sessions = append(sessions, parseSession(t, r))
	}
	return sessions
}

var (
	strVal  = testutil.StrVal
	numVal  = testutil.NumVal
	boolVal = testutil.BoolVal
)

// ====================================================================
// CLI-based pool (for pool, session, slots, offload, parent_child tests)
// ====================================================================

type cliPool struct {
	t            *testing.T
	poolDir      string
	poolName     string
	registryPath string
	cliBin       string
	daemonBin    string
}

type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// setupCLIPool creates a pool via CLI init: starts the daemon, initializes
// the pool, and returns a cliPool ready for CLI commands.
func setupCLIPool(t *testing.T, size int) *cliPool {
	t.Helper()

	poolDir := filepath.Join(runDir, t.Name())
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("failed to create pool dir: %v", err)
	}

	poolName := "test"

	// Write registry so CLI can resolve pool name → socket
	registryDir := filepath.Join(poolDir, "registry")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatalf("failed to create registry dir: %v", err)
	}
	socketPath := filepath.Join(poolDir, "api.sock")
	registry := Msg{poolName: Msg{"socket": socketPath}}
	registryBytes, _ := json.Marshal(registry)
	registryPath := filepath.Join(registryDir, "pools.json")
	if err := os.WriteFile(registryPath, registryBytes, 0644); err != nil {
		t.Fatalf("failed to write registry: %v", err)
	}

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

	p := &cliPool{
		t:            t,
		poolDir:      poolDir,
		poolName:     poolName,
		registryPath: registryPath,
		cliBin:       cliBinPath,
		daemonBin:    daemonBinPath,
	}

	// Start daemon process
	daemon := exec.Command(daemonBinPath, "--pool-dir", poolDir)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}

	// Wait for socket to appear
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

	// Init via CLI
	initResult := p.run("init", "--size", fmt.Sprintf("%d", size))
	if initResult.ExitCode != 0 {
		daemon.Process.Kill()
		t.Fatalf("init failed: %s", initResult.Stderr)
	}

	// Wait for all initial sessions to become idle
	p.waitForIdleCount(size, 90*time.Second)

	t.Cleanup(func() {
		// Best-effort destroy via CLI
		p.run("destroy", "--confirm")
		daemon.Process.Kill()
		daemon.Wait()
	})

	return p
}

// setupCLIDaemon starts the daemon but does NOT init. For tests that need
// to control init timing (e.g., testing ping/config before init).
func setupCLIDaemon(t *testing.T, size int) *cliPool {
	t.Helper()

	poolDir := filepath.Join(runDir, t.Name())
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("failed to create pool dir: %v", err)
	}

	poolName := "test"
	socketPath := filepath.Join(poolDir, "api.sock")

	// Write registry
	registryDir := filepath.Join(poolDir, "registry")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatalf("failed to create registry dir: %v", err)
	}
	registry := Msg{poolName: Msg{"socket": socketPath}}
	registryBytes, _ := json.Marshal(registry)
	registryPath := filepath.Join(registryDir, "pools.json")
	if err := os.WriteFile(registryPath, registryBytes, 0644); err != nil {
		t.Fatalf("failed to write registry: %v", err)
	}

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

	p := &cliPool{
		t:            t,
		poolDir:      poolDir,
		poolName:     poolName,
		registryPath: registryPath,
		cliBin:       cliBinPath,
		daemonBin:    daemonBinPath,
	}

	p.startDaemon()

	t.Cleanup(func() {
		p.run("destroy", "--confirm")
		p.killDaemon()
	})

	return p
}

// startDaemon starts (or restarts) the daemon process and waits for the socket.
func (p *cliPool) startDaemon() {
	p.t.Helper()
	socketPath := filepath.Join(p.poolDir, "api.sock")

	daemon := exec.Command(p.daemonBin, "--pool-dir", p.poolDir)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		p.t.Fatalf("failed to start daemon: %v", err)
	}

	// Store for cleanup
	p.setDaemon(daemon)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	daemon.Process.Kill()
	p.t.Fatalf("daemon socket never appeared at %s", socketPath)
}

// daemon tracking — stored separately since cliPool is value-oriented
var daemonProcs sync.Map // poolDir → *exec.Cmd

func (p *cliPool) setDaemon(cmd *exec.Cmd) {
	daemonProcs.Store(p.poolDir, cmd)
}

func (p *cliPool) killDaemon() {
	if v, ok := daemonProcs.Load(p.poolDir); ok {
		cmd := v.(*exec.Cmd)
		cmd.Process.Kill()
		cmd.Wait()
		daemonProcs.Delete(p.poolDir)
	}
}

// awaitSocketGone polls until the socket file disappears.
func (p *cliPool) awaitSocketGone(timeout time.Duration) {
	p.t.Helper()
	socketPath := filepath.Join(p.poolDir, "api.sock")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("socket still exists after %v", timeout)
}

// --- CLI execution ---

// run executes a CLI command with --pool prepended.
func (p *cliPool) run(args ...string) cmdResult {
	p.t.Helper()
	return p.execCLI(nil, args...)
}

// runJSON executes a CLI command with --json and parses stdout.
func (p *cliPool) runJSON(args ...string) Msg {
	p.t.Helper()
	result := p.run(append(args, "--json")...)
	if result.ExitCode != 0 {
		p.t.Fatalf("CLI exited %d: stderr=%s stdout=%s", result.ExitCode, result.Stderr, result.Stdout)
	}
	var msg Msg
	if err := json.Unmarshal([]byte(result.Stdout), &msg); err != nil {
		p.t.Fatalf("parse CLI JSON: %v\nstdout: %s", err, result.Stdout)
	}
	return msg
}

// runInSession executes a CLI command with CLAUDE_POOL_SESSION_ID set.
func (p *cliPool) runInSession(sessionID string, args ...string) cmdResult {
	p.t.Helper()
	return p.execCLI([]string{
		fmt.Sprintf("CLAUDE_POOL_SESSION_ID=%s", sessionID),
	}, args...)
}

// runInSessionJSON is runInSession + JSON parsing.
func (p *cliPool) runInSessionJSON(sessionID string, args ...string) Msg {
	p.t.Helper()
	result := p.runInSession(sessionID, append(args, "--json")...)
	if result.ExitCode != 0 {
		p.t.Fatalf("CLI exited %d: stderr=%s stdout=%s", result.ExitCode, result.Stderr, result.Stdout)
	}
	var msg Msg
	if err := json.Unmarshal([]byte(result.Stdout), &msg); err != nil {
		p.t.Fatalf("parse CLI JSON: %v\nstdout: %s", err, result.Stdout)
	}
	return msg
}

func (p *cliPool) execCLI(extraEnv []string, args ...string) cmdResult {
	p.t.Helper()
	fullArgs := append([]string{"--pool", p.poolName}, args...)
	cmd := exec.Command(p.cliBin, fullArgs...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CLAUDE_POOL_REGISTRY=%s", p.registryPath),
	)
	cmd.Env = append(cmd.Env, extraEnv...)

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

// --- Waiting helpers (CLI-based) ---

// waitForStatus polls `info --json` until the session reaches the target status.
func (p *cliPool) waitForStatus(sessionID, target string, timeout time.Duration) SessionInfo {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result := p.run("info", "--session", sessionID, "--json")
		if result.ExitCode == 0 {
			var resp Msg
			if err := json.Unmarshal([]byte(result.Stdout), &resp); err == nil {
				if session, ok := resp["session"].(map[string]any); ok {
					info := parseSession(p.t, session)
					if info.Status == target {
						return info
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Final check for error message
	result := p.run("info", "--session", sessionID, "--json")
	p.t.Fatalf("waitForStatus(%s, %q): timed out after %v\nlast info: %s", sessionID, target, timeout, result.Stdout)
	return SessionInfo{}
}

// waitForIdle uses the CLI wait command.
func (p *cliPool) waitForIdle(sessionID string, timeout time.Duration) Msg {
	p.t.Helper()
	result := p.run("wait", "--session", sessionID, "--timeout", fmt.Sprintf("%d", timeout.Milliseconds()), "--json")
	if result.ExitCode != 0 {
		p.t.Fatalf("wait for %s failed (exit %d): %s", sessionID, result.ExitCode, result.Stderr)
	}
	var resp Msg
	if err := json.Unmarshal([]byte(result.Stdout), &resp); err != nil {
		p.t.Fatalf("parse wait JSON: %v\nstdout: %s", err, result.Stdout)
	}
	return resp
}

// waitForIdleCount polls health until at least n sessions are idle.
func (p *cliPool) waitForIdleCount(n int, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result := p.run("health", "--json")
		if result.ExitCode == 0 {
			var resp Msg
			if err := json.Unmarshal([]byte(result.Stdout), &resp); err == nil {
				if health, ok := resp["health"].(map[string]any); ok {
					if counts, ok := health["counts"].(map[string]any); ok {
						if numVal(counts, "idle") >= float64(n) {
							return
						}
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.t.Fatalf("waitForIdleCount(%d): timed out after %v", n, timeout)
}

// waitForPoolSize polls health until the pool reaches the target slot count.
func (p *cliPool) waitForPoolSize(target int, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result := p.run("health", "--json")
		if result.ExitCode == 0 {
			var resp Msg
			if err := json.Unmarshal([]byte(result.Stdout), &resp); err == nil {
				if health, ok := resp["health"].(map[string]any); ok {
					if int(numVal(health, "size")) == target {
						return
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.t.Fatalf("waitForPoolSize(%d): timed out after %v", target, timeout)
}

// getSessionInfo runs info --json and returns parsed session.
func (p *cliPool) getSessionInfo(sessionID string) SessionInfo {
	p.t.Helper()
	resp := p.runJSON("info", "--session", sessionID)
	session, ok := resp["session"].(map[string]any)
	if !ok {
		p.t.Fatalf("expected session object in info response")
	}
	return parseSession(p.t, session)
}

// getHealth runs health --json and returns the health object.
func (p *cliPool) getHealth() Msg {
	p.t.Helper()
	resp := p.runJSON("health")
	health, ok := resp["health"].(map[string]any)
	if !ok {
		p.t.Fatalf("expected health object")
	}
	return health
}

// listSessions runs ls --json with optional args and returns parsed sessions.
func (p *cliPool) listSessions(args ...string) []SessionInfo {
	p.t.Helper()
	fullArgs := append([]string{"ls"}, args...)
	resp := p.runJSON(fullArgs...)
	return parseSessions(p.t, resp)
}

// ====================================================================
// Socket-based pool (for attach and subscribe tests)
// ====================================================================

type testPool struct {
	t          *testing.T
	dir        string
	binPath    string
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
	daemon     *exec.Cmd
	nextID     atomic.Int64
}

// setupPool builds the daemon, starts it, calls init, and returns a connected testPool.
func setupPool(t *testing.T, size int) *testPool {
	t.Helper()
	p := setupDaemon(t, size)

	resp := p.send(Msg{"type": "init", "size": size})
	if resp["type"] == "error" {
		t.Fatalf("init failed: %v", resp["error"])
	}

	return p
}

// setupDaemon starts a daemon but does NOT call init.
func setupDaemon(t *testing.T, size int) *testPool {
	t.Helper()

	poolDir := filepath.Join(runDir, t.Name())
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("failed to create pool dir: %v", err)
	}
	socketPath := filepath.Join(poolDir, "api.sock")

	configPath := filepath.Join(poolDir, "config.json")
	config := Msg{
		"flags": "--dangerously-skip-permissions --model haiku",
		"size":  size,
	}
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	p := &testPool{
		t:          t,
		dir:        poolDir,
		binPath:    daemonBinPath,
		socketPath: socketPath,
	}

	p.startDaemon()

	t.Cleanup(func() {
		p.send(Msg{"type": "destroy", "confirm": true})
		p.conn.Close()
		if p.daemon != nil && p.daemon.Process != nil {
			p.daemon.Process.Kill()
		}
	})

	return p
}

func (p *testPool) startDaemon() {
	p.t.Helper()

	daemon := exec.Command(p.binPath, "--pool-dir", p.dir)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		p.t.Fatalf("failed to start daemon: %v", err)
	}
	p.daemon = daemon

	daemonDied := make(chan struct{})
	go func() {
		daemon.Wait()
		close(daemonDied)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.socketPath); err == nil {
			break
		}
		select {
		case <-daemonDied:
			p.t.Fatalf("daemon exited before socket appeared")
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(p.socketPath); err != nil {
		daemon.Process.Kill()
		p.t.Fatalf("daemon socket never appeared at %s", p.socketPath)
	}

	conn, err := net.Dial("unix", p.socketPath)
	if err != nil {
		daemon.Process.Kill()
		p.t.Fatalf("failed to connect to daemon socket: %v", err)
	}
	p.conn = conn
	p.scanner = bufio.NewScanner(conn)
}

func (p *testPool) send(msg Msg) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(p.conn, p.scanner, msg, 10*time.Second)
}

func (p *testPool) sendLong(msg Msg, readTimeout time.Duration) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(p.conn, p.scanner, msg, readTimeout)
}

func (p *testPool) doSend(conn net.Conn, scanner *bufio.Scanner, msg Msg, readTimeout time.Duration) Msg {
	p.t.Helper()

	data, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(data); err != nil {
		p.t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(readTimeout))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			p.t.Fatalf("read: %v", err)
		}
		p.t.Fatal("connection closed while reading response")
	}

	var resp Msg
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		p.t.Fatalf("unmarshal response: %v\nraw: %s", err, scanner.Text())
	}
	return resp
}

func (p *testPool) newConn() (net.Conn, *bufio.Scanner) {
	p.t.Helper()
	conn, err := net.Dial("unix", p.socketPath)
	if err != nil {
		p.t.Fatalf("failed to open new connection: %v", err)
	}
	p.t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewScanner(conn)
}

func (p *testPool) sendOn(conn net.Conn, scanner *bufio.Scanner, msg Msg) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(conn, scanner, msg, 10*time.Second)
}

// awaitStatus uses subscribe to wait for a session to reach a target status.
func (p *testPool) awaitStatus(sessionID, target string, timeout time.Duration) SessionInfo {
	p.t.Helper()

	sub := p.subscribe(Msg{
		"sessions": []string{sessionID},
		"events":   []string{"status"},
		"statuses": []string{target},
	})
	defer sub.close()

	resp := p.send(Msg{"type": "info", "sessionId": sessionID})
	if resp["type"] == "error" {
		p.t.Fatalf("awaitStatus info failed: %v", resp["error"])
	}
	info := parseSession(p.t, resp["session"])
	if info.Status == target {
		return info
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev, ok := sub.nextWithin(time.Until(deadline))
		if !ok {
			break
		}
		if strVal(ev, "sessionId") == sessionID && strVal(ev, "status") == target {
			resp = p.send(Msg{"type": "info", "sessionId": sessionID})
			if resp["type"] == "error" {
				p.t.Fatalf("awaitStatus info failed: %v", resp["error"])
			}
			return parseSession(p.t, resp["session"])
		}
	}

	resp = p.send(Msg{"type": "info", "sessionId": sessionID})
	info = parseSession(p.t, resp["session"])
	p.t.Fatalf("awaitStatus(%s, %q): timed out after %v, last status was %q",
		sessionID, target, timeout, info.Status)
	return SessionInfo{}
}

func (p *testPool) awaitIdleCount(n int, timeout time.Duration) {
	p.t.Helper()

	sub := p.subscribe(Msg{"events": []string{"status"}, "statuses": []string{"idle"}})
	defer sub.close()

	resp := p.send(Msg{"type": "health"})
	health, _ := resp["health"].(map[string]any)
	counts, _ := health["counts"].(map[string]any)
	if int(numVal(counts, "idle")) >= n {
		return
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ok := sub.nextWithin(time.Until(deadline))
		if !ok {
			break
		}
		resp = p.send(Msg{"type": "health"})
		health, _ = resp["health"].(map[string]any)
		counts, _ = health["counts"].(map[string]any)
		if int(numVal(counts, "idle")) >= n {
			return
		}
	}
	p.t.Fatalf("awaitIdleCount(%d): timed out after %v", n, timeout)
}

func (p *testPool) awaitPoolSize(target int, timeout time.Duration) {
	p.t.Helper()

	sub := p.subscribe(Msg{"events": []string{"pool"}})
	defer sub.close()

	resp := p.send(Msg{"type": "health"})
	health, _ := resp["health"].(map[string]any)
	if int(numVal(health, "size")) == target {
		return
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ok := sub.nextWithin(time.Until(deadline))
		if !ok {
			break
		}
		resp = p.send(Msg{"type": "health"})
		health, _ = resp["health"].(map[string]any)
		if int(numVal(health, "size")) == target {
			return
		}
	}
	p.t.Fatalf("awaitPoolSize(%d): timed out after %v", target, timeout)
}

func (p *testPool) awaitPendingInputSet(sessionID string, timeout time.Duration) string {
	p.t.Helper()
	return p.awaitPendingInput(sessionID, func(v string) bool { return v != "" }, timeout)
}

func (p *testPool) awaitPendingInputClear(sessionID string, timeout time.Duration) {
	p.t.Helper()
	p.awaitPendingInput(sessionID, func(v string) bool { return v == "" }, timeout)
}

func (p *testPool) awaitPendingInput(sessionID string, match func(string) bool, timeout time.Duration) string {
	p.t.Helper()

	sub := p.subscribe(Msg{
		"sessions": []string{sessionID},
		"events":   []string{"updated"},
		"fields":   []string{"pendingInput"},
	})
	defer sub.close()

	resp := p.send(Msg{"type": "info", "sessionId": sessionID})
	if resp["type"] == "error" {
		p.t.Fatalf("awaitPendingInput info failed: %v", resp["error"])
	}
	info := parseSession(p.t, resp["session"])
	if match(info.PendingInput) {
		return info.PendingInput
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev, ok := sub.nextWithin(time.Until(deadline))
		if !ok {
			break
		}
		val := strVal(ev, "pendingInput")
		if match(val) {
			return val
		}
	}

	resp = p.send(Msg{"type": "info", "sessionId": sessionID})
	info = parseSession(p.t, resp["session"])
	p.t.Fatalf("awaitPendingInput(%s): timed out after %v, pendingInput was %q",
		sessionID, timeout, info.PendingInput)
	return ""
}

// --- Subscribe ---

type subscription struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

func (s *subscription) close() { s.conn.Close() }

func (p *testPool) subscribe(opts Msg) *subscription {
	p.t.Helper()
	opts["type"] = "subscribe"
	conn, scanner := p.newConn()

	data, _ := json.Marshal(opts)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(data); err != nil {
		p.t.Fatalf("subscribe write: %v", err)
	}

	return &subscription{t: p.t, conn: conn, scanner: scanner}
}

func (s *subscription) resubscribe(opts Msg) {
	s.t.Helper()
	opts["type"] = "subscribe"
	data, _ := json.Marshal(opts)
	data = append(data, '\n')
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := s.conn.Write(data); err != nil {
		s.t.Fatalf("resubscribe write: %v", err)
	}
}

func (s *subscription) next() Msg {
	s.t.Helper()
	s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			s.t.Fatalf("subscribe read: %v", err)
		}
		s.t.Fatal("subscribe connection closed")
	}
	var event Msg
	if err := json.Unmarshal(s.scanner.Bytes(), &event); err != nil {
		s.t.Fatalf("unmarshal event: %v\nraw: %s", err, s.scanner.Text())
	}
	return event
}

func (s *subscription) nextWithin(timeout time.Duration) (Msg, bool) {
	s.t.Helper()
	s.conn.SetReadDeadline(time.Now().Add(timeout))
	if !s.scanner.Scan() {
		return nil, false
	}
	var event Msg
	if err := json.Unmarshal(s.scanner.Bytes(), &event); err != nil {
		s.t.Fatalf("unmarshal event: %v", err)
	}
	return event, true
}

func (s *subscription) drain() []Msg {
	s.t.Helper()
	var events []Msg
	for {
		ev, ok := s.nextWithin(500 * time.Millisecond)
		if !ok {
			break
		}
		events = append(events, ev)
	}
	return events
}

// --- Process helpers ---

func killPID(t *testing.T, pid int) {
	t.Helper()
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill process %d: %v", pid, err)
	}
}

// --- Assertion helpers ---

func assertStatus(t *testing.T, s SessionInfo, expected string) {
	t.Helper()
	if s.Status != expected {
		t.Fatalf("session %s: expected status %q, got %q", s.SessionID, expected, s.Status)
	}
}

func assertError(t *testing.T, resp Msg) {
	t.Helper()
	if resp["type"] != "error" {
		t.Fatalf("expected error response, got type %q: %v", resp["type"], resp)
	}
}

func assertNotError(t *testing.T, resp Msg) {
	t.Helper()
	if resp["type"] == "error" {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
}

func assertType(t *testing.T, resp Msg, expected string) {
	t.Helper()
	if resp["type"] != expected {
		t.Fatalf("expected type %q, got %q: %v", expected, resp["type"], resp)
	}
}

func assertContains(t *testing.T, content, substr string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(content), strings.ToLower(substr)) {
		t.Fatalf("expected content to contain %q, got:\n%s", substr, truncate(content, 500))
	}
}

func assertNonEmpty(t *testing.T, label, value string) {
	t.Helper()
	if value == "" {
		t.Fatalf("expected %s to be non-empty", label)
	}
}

func assertSessionCount(t *testing.T, sessions []SessionInfo, expected int) {
	t.Helper()
	if len(sessions) != expected {
		ids := make([]string, len(sessions))
		for i, s := range sessions {
			ids[i] = fmt.Sprintf("%s(%s)", s.SessionID, s.Status)
		}
		t.Fatalf("expected %d sessions, got %d: %v", expected, len(sessions), ids)
	}
}

func assertHasChild(t *testing.T, parent SessionInfo, childID string) {
	t.Helper()
	for _, c := range parent.Children {
		if c.SessionID == childID {
			return
		}
	}
	t.Fatalf("expected session %s to have child %s", parent.SessionID, childID)
}

func assertExitError(t *testing.T, result cmdResult) {
	t.Helper()
	if result.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got 0\nstdout: %s", result.Stdout)
	}
}

func assertExitOK(t *testing.T, result cmdResult) {
	t.Helper()
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d\nstderr: %s", result.ExitCode, result.Stderr)
	}
}

func findSession(sessions []SessionInfo, id string) (SessionInfo, bool) {
	for _, s := range sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return SessionInfo{}, false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
