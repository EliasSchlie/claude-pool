package integration

// helpers_test.go — Shared test infrastructure
//
// Single pool type for all tests:
//
//   pool — All operations go through CLI commands, just like production.
//     Setup: build daemon + CLI binaries, run `init` via CLI (which starts the daemon).
//     Operations: run CLI commands as subprocesses, parse JSON output.
//     Waiting: poll `info --json` for status, use `wait` CLI command for idle.
//
//   For API-only features (attach, subscribe), pool.dial() opens a raw socket
//   connection to the daemon for direct JSON messaging.
//
// Isolation:
//
//   Each test gets its own directory under ~/.cache/claude-pool-tests/:
//     <testDir>/.claude-pool/   ← CLAUDE_POOL_HOME (registry, pool data)
//     <testDir>/workdir/        ← session spawn directory
//
//   CLAUDE_POOL_HOME mirrors the production ~/.claude-pool/ structure.
//   CLAUDE_POOL_DAEMON tells the CLI where to find the daemon binary.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if code == 0 && os.Getenv("CLAUDE_POOL_TEST_KEEP") == "" {
		os.RemoveAll(runDir)
	} else if code == 0 {
		fmt.Fprintf(os.Stderr, "\n=== CLAUDE_POOL_TEST_KEEP: artifacts at %s ===\n", runDir)
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
// Pool — all operations go through CLI commands
// ====================================================================

type pool struct {
	t       *testing.T
	name    string
	homeDir string // CLAUDE_POOL_HOME
	workDir string // session spawn directory
}

type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// newPool creates the directory structure but does NOT init.
// For tests that need to control init timing (e.g., TestPool).
func newPool(t *testing.T) *pool {
	t.Helper()

	testDir := filepath.Join(runDir, t.Name())
	cpHome := filepath.Join(testDir, ".claude-pool")
	workDir := filepath.Join(testDir, "workdir")

	if err := os.MkdirAll(cpHome, 0755); err != nil {
		t.Fatalf("failed to create .claude-pool dir: %v", err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workdir: %v", err)
	}

	p := &pool{
		t:       t,
		name:    "test",
		homeDir: cpHome,
		workDir: workDir,
	}

	t.Cleanup(func() {
		p.run("destroy", "--confirm")
	})

	return p
}

// newNamedPool creates a pool with a specific name and shared homeDir.
// For tests that run multiple pools in the same CLAUDE_POOL_HOME (e.g., TestMultiPool).
func newNamedPool(t *testing.T, name, homeDir, workDir string) *pool {
	t.Helper()

	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create workdir: %v", err)
	}

	p := &pool{
		t:       t,
		name:    name,
		homeDir: homeDir,
		workDir: workDir,
	}

	t.Cleanup(func() {
		p.run("destroy", "--confirm")
	})

	return p
}

// setupPool creates a pool via CLI init and waits for all slots to become idle.
func setupPool(t *testing.T, size int) *pool {
	t.Helper()

	p := newPool(t)

	result := p.run("init", "--size", fmt.Sprintf("%d", size),
		"--dir", p.workDir,
		"--flags", "--dangerously-skip-permissions --model haiku")
	if result.ExitCode != 0 {
		t.Fatalf("init failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	p.waitForIdleCount(size, 90*time.Second)
	return p
}

// --- CLI execution ---

func (p *pool) cliEnv() []string {
	return append(os.Environ(),
		"CLAUDE_POOL_HOME="+p.homeDir,
		"CLAUDE_POOL_DAEMON="+daemonBinPath,
	)
}

// run executes a CLI command with --pool prepended.
func (p *pool) run(args ...string) cmdResult {
	p.t.Helper()
	return p.execCLI(nil, args...)
}

// runJSON executes a CLI command with --json and parses stdout.
func (p *pool) runJSON(args ...string) Msg {
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
func (p *pool) runInSession(sessionID string, args ...string) cmdResult {
	p.t.Helper()
	return p.execCLI([]string{
		fmt.Sprintf("CLAUDE_POOL_SESSION_ID=%s", sessionID),
	}, args...)
}

// runInSessionJSON is runInSession + JSON parsing.
func (p *pool) runInSessionJSON(sessionID string, args ...string) Msg {
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

func (p *pool) execCLI(extraEnv []string, args ...string) cmdResult {
	p.t.Helper()
	fullArgs := append([]string{"--pool", p.name}, args...)
	cmd := exec.Command(cliBinPath, fullArgs...)
	cmd.Env = append(p.cliEnv(), extraEnv...)

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
func (p *pool) waitForStatus(sessionID, target string, timeout time.Duration) SessionInfo {
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
	result := p.run("info", "--session", sessionID, "--json")
	p.t.Fatalf("waitForStatus(%s, %q): timed out after %v\nlast info: %s", sessionID, target, timeout, result.Stdout)
	return SessionInfo{}
}

// waitForIdle uses the CLI wait command.
func (p *pool) waitForIdle(sessionID string, timeout time.Duration) Msg {
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
func (p *pool) waitForIdleCount(n int, timeout time.Duration) {
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
func (p *pool) waitForPoolSize(target int, timeout time.Duration) {
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

// waitForPendingInput polls info until pendingInput matches.
func (p *pool) waitForPendingInput(sessionID string, match func(string) bool, timeout time.Duration) string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info := p.getSessionInfo(sessionID)
		if match(info.PendingInput) {
			return info.PendingInput
		}
		time.Sleep(200 * time.Millisecond)
	}
	info := p.getSessionInfo(sessionID)
	p.t.Fatalf("waitForPendingInput(%s): timed out after %v, pendingInput was %q",
		sessionID, timeout, info.PendingInput)
	return ""
}

// awaitSocketGone polls until the pool's socket file disappears.
func (p *pool) awaitSocketGone(timeout time.Duration) {
	p.t.Helper()
	socketPath := filepath.Join(p.homeDir, p.name, "api.sock")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("socket still exists after %v", timeout)
}

// --- Convenience helpers ---

// getSessionInfo runs info --json and returns parsed session.
func (p *pool) getSessionInfo(sessionID string) SessionInfo {
	p.t.Helper()
	resp := p.runJSON("info", "--session", sessionID)
	session, ok := resp["session"].(map[string]any)
	if !ok {
		p.t.Fatalf("expected session object in info response")
	}
	return parseSession(p.t, session)
}

// getHealth runs health --json and returns the health object.
func (p *pool) getHealth() Msg {
	p.t.Helper()
	resp := p.runJSON("health")
	health, ok := resp["health"].(map[string]any)
	if !ok {
		p.t.Fatalf("expected health object")
	}
	return health
}

// listSessions runs ls --json with optional args and returns parsed sessions.
func (p *pool) listSessions(args ...string) []SessionInfo {
	p.t.Helper()
	fullArgs := append([]string{"ls"}, args...)
	resp := p.runJSON(fullArgs...)
	return parseSessions(p.t, resp)
}

// ====================================================================
// Socket connection — for API-only features (attach, subscribe)
// ====================================================================

type socketConn struct {
	t          *testing.T
	conn       net.Conn
	scanner    *bufio.Scanner
	socketPath string
	nextID     atomic.Int64
}

// dial opens a socket connection to the pool's API socket.
func (p *pool) dial() *socketConn {
	p.t.Helper()
	socketPath := filepath.Join(p.homeDir, p.name, "api.sock")
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		p.t.Fatalf("failed to connect to socket %s: %v", socketPath, err)
	}
	sc := &socketConn{
		t:          p.t,
		conn:       conn,
		scanner:    bufio.NewScanner(conn),
		socketPath: socketPath,
	}
	p.t.Cleanup(func() { conn.Close() })
	return sc
}

func (sc *socketConn) send(msg Msg) Msg {
	sc.t.Helper()
	msg["id"] = int(sc.nextID.Add(1))
	return sc.doSend(sc.conn, sc.scanner, msg, 10*time.Second)
}

func (sc *socketConn) sendLong(msg Msg, readTimeout time.Duration) Msg {
	sc.t.Helper()
	msg["id"] = int(sc.nextID.Add(1))
	return sc.doSend(sc.conn, sc.scanner, msg, readTimeout)
}

func (sc *socketConn) doSend(conn net.Conn, scanner *bufio.Scanner, msg Msg, readTimeout time.Duration) Msg {
	sc.t.Helper()

	data, err := json.Marshal(msg)
	if err != nil {
		sc.t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(data); err != nil {
		sc.t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(readTimeout))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			sc.t.Fatalf("read: %v", err)
		}
		sc.t.Fatal("connection closed while reading response")
	}

	var resp Msg
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		sc.t.Fatalf("unmarshal response: %v\nraw: %s", err, scanner.Text())
	}
	return resp
}

func (sc *socketConn) newConn() (net.Conn, *bufio.Scanner) {
	sc.t.Helper()
	conn, err := net.Dial("unix", sc.socketPath)
	if err != nil {
		sc.t.Fatalf("failed to open new connection: %v", err)
	}
	sc.t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewScanner(conn)
}

func (sc *socketConn) sendOn(conn net.Conn, scanner *bufio.Scanner, msg Msg) Msg {
	sc.t.Helper()
	msg["id"] = int(sc.nextID.Add(1))
	return sc.doSend(conn, scanner, msg, 10*time.Second)
}

// --- Subscribe ---

type subscription struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

func (s *subscription) close() { s.conn.Close() }

func (sc *socketConn) subscribe(opts Msg) *subscription {
	sc.t.Helper()
	opts["type"] = "subscribe"
	conn, scanner := sc.newConn()

	data, _ := json.Marshal(opts)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(data); err != nil {
		sc.t.Fatalf("subscribe write: %v", err)
	}

	return &subscription{t: sc.t, conn: conn, scanner: scanner}
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
