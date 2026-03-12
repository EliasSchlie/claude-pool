package integration

// helpers_test.go — Shared test infrastructure
//
// Keeps only what genuinely reduces noise without hiding protocol details:
//   - setupPool: complex setup/teardown (build binary, start daemon, connect, init)
//   - send / sendOn: socket boilerplate (marshal, write, read, unmarshal)
//   - awaitStatus: poll info until session reaches target state (eliminates timing races)
//   - parseSession / parseSessions: JSON map → typed struct
//   - subscription: subscribe has different mechanics (persistent stream)
//   - assertion helpers: assertStatus, assertError, assertContains, etc.
//
// Tests use pool.send(Msg{...}) directly for all protocol commands.
// Every request and response is visible in the test — no hidden abstractions.

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
)

// --------------------------------------------------------------------
// JSON message types
// --------------------------------------------------------------------

// Msg is a generic JSON message. We use map[string]any for flexibility —
// the protocol has many command shapes and we don't want parallel struct
// hierarchies in test code.
type Msg = map[string]any

// SessionInfo holds parsed session fields from info/ls responses.
type SessionInfo struct {
	SessionID  string
	ClaudeUUID string
	Status     string
	ParentID   string
	Priority   float64
	Cwd        string
	SpawnCwd   string
	CreatedAt  string
	PID        float64
	Pinned     bool
	Children   []SessionInfo
}

func parseSession(t *testing.T, raw any) SessionInfo {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected session object, got %T", raw)
	}
	s := SessionInfo{
		SessionID:  strVal(m, "sessionId"),
		ClaudeUUID: strVal(m, "claudeUUID"),
		Status:     strVal(m, "status"),
		ParentID:   strVal(m, "parentId"),
		Priority:   numVal(m, "priority"),
		Cwd:        strVal(m, "cwd"),
		SpawnCwd:   strVal(m, "spawnCwd"),
		CreatedAt:  strVal(m, "createdAt"),
		PID:        numVal(m, "pid"),
		Pinned:     boolVal(m, "pinned"),
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

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func numVal(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func boolVal(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// --------------------------------------------------------------------
// Test pool
// --------------------------------------------------------------------

type testPool struct {
	t          *testing.T
	dir        string // temp pool directory
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
	daemon     *exec.Cmd
	nextID     atomic.Int64
}

// setupPool builds the daemon binary, starts it with a temp pool directory,
// configures flags, calls init, and returns a connected testPool.
// t.Cleanup tears everything down.
func setupPool(t *testing.T, size int) *testPool {
	t.Helper()

	// Build the daemon binary
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "claude-pool")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/claude-pool")
	build.Dir = findRepoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build daemon: %v\n%s", err, out)
	}

	// Create temp pool directory
	poolDir := t.TempDir()
	socketPath := filepath.Join(poolDir, "api.sock")

	// Write initial config
	configPath := filepath.Join(poolDir, "config.json")
	config := Msg{
		"flags": "--dangerously-skip-permissions --model haiku",
		"size":  size,
	}
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	daemon := exec.Command(binPath, "--pool-dir", poolDir)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}

	// Single goroutine to detect daemon death
	daemonDied := make(chan struct{})
	go func() {
		daemon.Wait()
		close(daemonDied)
	}()

	// Wait for socket to appear (fast-fail if daemon exits early)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		select {
		case <-daemonDied:
			t.Fatalf("daemon exited before socket appeared")
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(socketPath); err != nil {
		daemon.Process.Kill()
		t.Fatalf("daemon socket never appeared at %s", socketPath)
	}

	// Connect
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		daemon.Process.Kill()
		t.Fatalf("failed to connect to daemon socket: %v", err)
	}

	p := &testPool{
		t:          t,
		dir:        poolDir,
		socketPath: socketPath,
		conn:       conn,
		scanner:    bufio.NewScanner(conn),
		daemon:     daemon,
	}

	// Cleanup: destroy pool, kill daemon
	t.Cleanup(func() {
		p.sendRaw(Msg{"type": "destroy", "confirm": true})
		p.conn.Close()
		daemon.Process.Kill()
		// daemon.Wait() already called by goroutine above
	})

	// Init the pool
	resp := p.send(Msg{"type": "init", "size": size})
	if resp["type"] == "error" {
		t.Fatalf("init failed: %v", resp["error"])
	}

	return p
}

// findRepoRoot walks up from cwd to find go.mod.
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

// --------------------------------------------------------------------
// Socket communication
// --------------------------------------------------------------------

// send sends a JSON message on the default connection and reads the response.
// Auto-assigns an incrementing id field.
func (p *testPool) send(msg Msg) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(p.conn, p.scanner, msg, 30*time.Second)
}

// sendLong sends a message with a custom read timeout (for long-polling commands like wait).
func (p *testPool) sendLong(msg Msg, readTimeout time.Duration) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(p.conn, p.scanner, msg, readTimeout)
}

// sendRaw sends without expecting a response (best-effort, for cleanup).
func (p *testPool) sendRaw(msg Msg) {
	data, _ := json.Marshal(msg)
	data = append(data, '\n')
	p.conn.Write(data)
}

func (p *testPool) doSend(conn net.Conn, scanner *bufio.Scanner, msg Msg, readTimeout time.Duration) Msg {
	p.t.Helper()

	data, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
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

// newConn opens a separate socket connection (for subscribe, concurrent clients).
// Registered for cleanup automatically.
func (p *testPool) newConn() (net.Conn, *bufio.Scanner) {
	p.t.Helper()
	conn, err := net.Dial("unix", p.socketPath)
	if err != nil {
		p.t.Fatalf("failed to open new connection: %v", err)
	}
	p.t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewScanner(conn)
}

// sendOn sends a message on a specific connection and reads the response.
// Safe to call concurrently with sends on other connections (no shared mutex).
func (p *testPool) sendOn(conn net.Conn, scanner *bufio.Scanner, msg Msg) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(conn, scanner, msg, 30*time.Second)
}

// --------------------------------------------------------------------
// State helpers
// --------------------------------------------------------------------

// awaitStatus polls info until the session reaches the target status.
// Eliminates timing races — use this instead of assuming a session is in
// a particular state after sending a command.
func (p *testPool) awaitStatus(sessionID, target string, timeout time.Duration) SessionInfo {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := p.send(Msg{"type": "info", "sessionId": sessionID})
		if resp["type"] == "error" {
			p.t.Fatalf("awaitStatus info failed: %v", resp["error"])
		}
		info := parseSession(p.t, resp["session"])
		if info.Status == target {
			return info
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Final check with the actual status for a useful error message
	resp := p.send(Msg{"type": "info", "sessionId": sessionID})
	info := parseSession(p.t, resp["session"])
	p.t.Fatalf("awaitStatus(%s, %q): timed out after %v, last status was %q",
		sessionID, target, timeout, info.Status)
	return SessionInfo{} // unreachable
}

// --------------------------------------------------------------------
// Subscribe
// --------------------------------------------------------------------

type subscription struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

// subscribe opens a persistent event stream on a new connection.
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

// resubscribe sends a new subscribe on the same connection (replaces filters).
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

// next reads the next event from the subscription stream. Times out after 10s.
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

// nextWithin reads the next event within a custom timeout. Returns false on timeout.
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

// drain reads all pending events (non-blocking with short timeout).
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

// --------------------------------------------------------------------
// Assertion helpers
// --------------------------------------------------------------------

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
