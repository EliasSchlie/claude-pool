package integration

// helpers_test.go — Shared test infrastructure
//
// Provides:
//   - setupPool(t, size): starts a daemon with a temp pool directory, configures
//     --model haiku and --dangerously-skip-permissions, calls init with the given
//     size, registers t.Cleanup to destroy+kill daemon. Returns a *testPool.
//   - testPool: wraps a socket connection with typed methods for every protocol
//     command. Methods send JSON, parse the response, and fail the test on errors.
//   - Assertion helpers: assertStatus, assertError, assertSessionCount, etc.
//
// Socket communication:
//   - newConn(socketPath) opens a new connection (for subscribe, concurrent clients)
//   - pool.send(msg) → response: raw JSON round-trip on the default connection
//   - pool.sendOn(conn, msg) → response: raw JSON round-trip on a specific connection
//
// Timeouts:
//   - waitIdle(sessionId): calls wait with a 2-minute timeout, fails test if exceeded
//   - All operations have a default 30s timeout for non-wait commands

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
	"testing"
	"time"
)

// --------------------------------------------------------------------
// JSON message types
// --------------------------------------------------------------------

// Msg is a generic JSON message for the protocol. We use map[string]any for
// flexibility — the protocol has many command shapes and we don't want to
// maintain parallel struct hierarchies in test code.
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
	mu         sync.Mutex // protects conn/scanner for sequential sends
	daemon     *exec.Cmd
	nextID     int
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
		// Best-effort destroy
		p.sendRaw(Msg{"type": "destroy", "confirm": true})
		p.conn.Close()
		// Kill daemon if still running
		daemon.Process.Kill()
		daemon.Wait()
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

func (p *testPool) send(msg Msg) Msg {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	p.nextID++
	msg["id"] = p.nextID
	return p.sendOnLocked(p.conn, p.scanner, msg)
}

// sendRaw sends without expecting a response (best-effort).
func (p *testPool) sendRaw(msg Msg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, _ := json.Marshal(msg)
	data = append(data, '\n')
	p.conn.Write(data)
}

func (p *testPool) sendOnLocked(conn net.Conn, scanner *bufio.Scanner, msg Msg) Msg {
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

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
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
func (p *testPool) sendOn(conn net.Conn, scanner *bufio.Scanner, msg Msg) Msg {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	msg["id"] = p.nextID
	return p.sendOnLocked(conn, scanner, msg)
}

// --------------------------------------------------------------------
// Protocol commands — success path (fail test on error)
// --------------------------------------------------------------------

func (p *testPool) ping() Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "ping"}), "pong")
}

func (p *testPool) initPool(size int) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "init", "size": size}), "pool")
}

func (p *testPool) initNoRestore(size int) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "init", "size": size, "noRestore": true}), "pool")
}

func (p *testPool) resize(size int) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "resize", "size": size}), "pool")
}

func (p *testPool) health() Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "health"}), "health")
}

func (p *testPool) destroy() Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "destroy", "confirm": true}), "ok")
}

func (p *testPool) config() Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "config"}), "config")
}

func (p *testPool) configSet(fields Msg) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "config", "set": fields}), "config")
}

func (p *testPool) start(prompt string) string {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "start", "prompt": prompt}), "started")
	sid := strVal(resp, "sessionId")
	if sid == "" {
		p.t.Fatal("start: empty sessionId")
	}
	return sid
}

func (p *testPool) startWithParent(prompt, parentID string) string {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "start", "prompt": prompt, "parentId": parentID}), "started")
	sid := strVal(resp, "sessionId")
	if sid == "" {
		p.t.Fatal("start: empty sessionId")
	}
	return sid
}

func (p *testPool) startRaw(prompt string) Msg {
	p.t.Helper()
	return p.send(Msg{"type": "start", "prompt": prompt})
}

func (p *testPool) followup(sessionID, prompt string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{
		"type": "followup", "sessionId": sessionID, "prompt": prompt,
	}), "started")
}

func (p *testPool) followupForce(sessionID, prompt string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{
		"type": "followup", "sessionId": sessionID, "prompt": prompt, "force": true,
	}), "started")
}

func (p *testPool) followupRaw(sessionID, prompt string) Msg {
	p.t.Helper()
	return p.send(Msg{"type": "followup", "sessionId": sessionID, "prompt": prompt})
}

// wait calls the wait command with a 2-minute timeout.
func (p *testPool) wait(sessionID string) Msg {
	p.t.Helper()
	return p.waitWithTimeout(sessionID, 120000)
}

func (p *testPool) waitWithTimeout(sessionID string, timeoutMs int) Msg {
	p.t.Helper()
	msg := Msg{"type": "wait", "timeout": timeoutMs}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	// Use a longer read deadline for wait (it long-polls)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	msg["id"] = p.nextID

	data, _ := json.Marshal(msg)
	data = append(data, '\n')

	p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := p.conn.Write(data); err != nil {
		p.t.Fatalf("write: %v", err)
	}

	waitDuration := time.Duration(timeoutMs+30000) * time.Millisecond
	p.conn.SetReadDeadline(time.Now().Add(waitDuration))
	if !p.scanner.Scan() {
		if err := p.scanner.Err(); err != nil {
			p.t.Fatalf("wait read: %v", err)
		}
		p.t.Fatal("connection closed during wait")
	}

	var resp Msg
	if err := json.Unmarshal(p.scanner.Bytes(), &resp); err != nil {
		p.t.Fatalf("unmarshal wait response: %v", err)
	}
	return resp
}

// waitIdle waits for a specific session to become idle. Fails test on error/timeout.
func (p *testPool) waitIdle(sessionID string) string {
	p.t.Helper()
	resp := p.wait(sessionID)
	if resp["type"] == "error" {
		p.t.Fatalf("waitIdle(%s): %v", sessionID, resp["error"])
	}
	return strVal(resp, "content")
}

// waitAny waits for any owned busy session. Returns (sessionId, content).
func (p *testPool) waitAny() (string, string) {
	p.t.Helper()
	resp := p.wait("")
	if resp["type"] == "error" {
		p.t.Fatalf("waitAny: %v", resp["error"])
	}
	return strVal(resp, "sessionId"), strVal(resp, "content")
}

func (p *testPool) waitFormat(sessionID, format string) string {
	p.t.Helper()
	msg := Msg{"type": "wait", "sessionId": sessionID, "format": format, "timeout": 120000}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	msg["id"] = p.nextID

	data, _ := json.Marshal(msg)
	data = append(data, '\n')

	p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	p.conn.Write(data)
	p.conn.SetReadDeadline(time.Now().Add(150 * time.Second))
	if !p.scanner.Scan() {
		p.t.Fatalf("waitFormat read failed: %v", p.scanner.Err())
	}
	var resp Msg
	json.Unmarshal(p.scanner.Bytes(), &resp)
	if resp["type"] == "error" {
		p.t.Fatalf("waitFormat(%s, %s): %v", sessionID, format, resp["error"])
	}
	return strVal(resp, "content")
}

func (p *testPool) capture(sessionID string) string {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "capture", "sessionId": sessionID}), "result")
	return strVal(resp, "content")
}

func (p *testPool) captureFormat(sessionID, format string) string {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{
		"type": "capture", "sessionId": sessionID, "format": format,
	}), "result")
	return strVal(resp, "content")
}

func (p *testPool) captureRaw(sessionID, format string) Msg {
	p.t.Helper()
	return p.send(Msg{"type": "capture", "sessionId": sessionID, "format": format})
}

func (p *testPool) input(sessionID, data string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{
		"type": "input", "sessionId": sessionID, "data": data,
	}), "ok")
}

func (p *testPool) offload(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "offload", "sessionId": sessionID}), "ok")
}

func (p *testPool) ls() []SessionInfo {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "ls"}), "sessions")
	return parseSessions(p.t, resp)
}

func (p *testPool) lsAll() []SessionInfo {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "ls", "all": true}), "sessions")
	return parseSessions(p.t, resp)
}

func (p *testPool) lsTree() []SessionInfo {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "ls", "tree": true}), "sessions")
	return parseSessions(p.t, resp)
}

func (p *testPool) lsArchived() []SessionInfo {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "ls", "archived": true}), "sessions")
	return parseSessions(p.t, resp)
}

func (p *testPool) info(sessionID string) SessionInfo {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "info", "sessionId": sessionID}), "session")
	return parseSession(p.t, resp["session"])
}

func (p *testPool) infoRaw(sessionID string) Msg {
	p.t.Helper()
	return p.send(Msg{"type": "info", "sessionId": sessionID})
}

func (p *testPool) pin(sessionID string) (string, string) {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "pin", "sessionId": sessionID}), "ok")
	return strVal(resp, "sessionId"), strVal(resp, "status")
}

func (p *testPool) pinFresh() (string, string) {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "pin"}), "ok")
	return strVal(resp, "sessionId"), strVal(resp, "status")
}

func (p *testPool) unpin(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "unpin", "sessionId": sessionID}), "ok")
}

func (p *testPool) stop(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "stop", "sessionId": sessionID}), "ok")
}

func (p *testPool) archive(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "archive", "sessionId": sessionID}), "ok")
}

func (p *testPool) archiveRecursive(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{
		"type": "archive", "sessionId": sessionID, "recursive": true,
	}), "ok")
}

func (p *testPool) archiveRaw(sessionID string) Msg {
	p.t.Helper()
	return p.send(Msg{"type": "archive", "sessionId": sessionID})
}

func (p *testPool) unarchive(sessionID string) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{"type": "unarchive", "sessionId": sessionID}), "ok")
}

func (p *testPool) setPriority(sessionID string, priority float64) Msg {
	p.t.Helper()
	return p.expectOK(p.send(Msg{
		"type": "set-priority", "sessionId": sessionID, "priority": priority,
	}), "ok")
}

func (p *testPool) attach(sessionID string) string {
	p.t.Helper()
	resp := p.expectOK(p.send(Msg{"type": "attach", "sessionId": sessionID}), "attached")
	return strVal(resp, "socketPath")
}

// --------------------------------------------------------------------
// Subscribe
// --------------------------------------------------------------------

type subscription struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

func (p *testPool) subscribe(opts Msg) *subscription {
	p.t.Helper()
	opts["type"] = "subscribe"
	conn, scanner := p.newConn()

	// Send subscribe request
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

// nextWithin reads the next event within a custom timeout.
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

// drain reads all pending events (non-blocking drain with short timeout).
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

func (p *testPool) expectOK(resp Msg, expectedType string) Msg {
	p.t.Helper()
	if resp["type"] == "error" {
		p.t.Fatalf("expected %s, got error: %v", expectedType, resp["error"])
	}
	if resp["type"] != expectedType {
		p.t.Fatalf("expected type %q, got %q: %v", expectedType, resp["type"], resp)
	}
	return resp
}

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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
