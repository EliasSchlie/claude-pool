package integration

// helpers_test.go — Shared test infrastructure
//
// Keeps only what genuinely reduces noise without hiding protocol details:
//   - setupPool / setupDaemon: complex setup/teardown (build, start, connect)
//   - send / sendOn: socket boilerplate (marshal, write, read, unmarshal)
//   - awaitStatus / awaitPoolSize / awaitIdleCount: subscribe + wait for target state
//   - parseSession / parseSessions: JSON map → typed struct
//   - subscription: subscribe has different mechanics (persistent stream)
//   - assertion helpers: assertStatus, assertError, assertContains, assertHasChild, etc.
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

	"github.com/EliasSchlie/claude-pool/tests/testutil"
)

// daemonBinPath is built once by TestMain and shared across all test functions.
// runDir holds pool directories for the current run — preserved on failure.
var (
	daemonBinPath string
	runDir        string
)

func TestMain(m *testing.M) {
	repoRoot := testutil.FindRepoRoot()
	runDir = testutil.SetupRunDir(repoRoot, "integ")

	daemonBinPath = filepath.Join(runDir, "claude-pool")
	testutil.BuildBinary(repoRoot, daemonBinPath, "./cmd/claude-pool")

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

var (
	strVal  = testutil.StrVal
	numVal  = testutil.NumVal
	boolVal = testutil.BoolVal
)

// --------------------------------------------------------------------
// Test pool
// --------------------------------------------------------------------

type testPool struct {
	t          *testing.T
	dir        string // temp pool directory
	binPath    string // daemon binary path
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
	daemon     *exec.Cmd
	nextID     atomic.Int64
}

// setupPool builds the daemon binary, starts it, calls init, and returns a connected testPool.
// Most tests should use this. Use setupDaemon if you need to control init yourself.
func setupPool(t *testing.T, size int) *testPool {
	t.Helper()
	p := setupDaemon(t, size)

	resp := p.send(Msg{"type": "init", "size": size})
	if resp["type"] == "error" {
		t.Fatalf("init failed: %v", resp["error"])
	}

	return p
}

// setupDaemon starts a daemon with a temp pool directory and connects — but does
// NOT call init. Use this when the test flow needs to control init timing
// (e.g., pool_test.go tests ping/config before init).
// The daemon binary is built once by TestMain and shared across all tests.
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

// startDaemon starts (or restarts) the daemon process and connects to its socket.
// Used by setupDaemon and by tests that need to restart the daemon after destroy.
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

// --------------------------------------------------------------------
// Socket communication
// --------------------------------------------------------------------

// send sends a JSON message on the default connection and reads the response.
// Auto-assigns an incrementing id field.
func (p *testPool) send(msg Msg) Msg {
	p.t.Helper()
	msg["id"] = int(p.nextID.Add(1))
	return p.doSend(p.conn, p.scanner, msg, 10*time.Second)
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
	return p.doSend(conn, scanner, msg, 10*time.Second)
}

// --------------------------------------------------------------------
// State helpers
// --------------------------------------------------------------------

// awaitSocketGone polls until the socket file disappears (daemon exited).
func (p *testPool) awaitSocketGone(timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.socketPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("socket %s still exists after %v", p.socketPath, timeout)
}

// awaitStatus subscribes to status events for a specific session and waits
// until it reaches the target status. Uses subscribe rather than polling —
// same as a production client would.
//
// Subscribes BEFORE checking current state to avoid the TOCTOU race where
// a transition fires between the check and the subscribe registration.
func (p *testPool) awaitStatus(sessionID, target string, timeout time.Duration) SessionInfo {
	p.t.Helper()

	// Subscribe first — ensures no events are missed between check and listen
	sub := p.subscribe(Msg{
		"sessions": []string{sessionID},
		"events":   []string{"status"},
		"statuses": []string{target},
	})
	defer sub.close()

	// Now check — if already at target, done
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
			// Fetch full info to return
			resp = p.send(Msg{"type": "info", "sessionId": sessionID})
			if resp["type"] == "error" {
				p.t.Fatalf("awaitStatus info failed: %v", resp["error"])
			}
			return parseSession(p.t, resp["session"])
		}
	}

	// Timeout — get actual status for the error message
	resp = p.send(Msg{"type": "info", "sessionId": sessionID})
	info = parseSession(p.t, resp["session"])
	p.t.Fatalf("awaitStatus(%s, %q): timed out after %v, last status was %q",
		sessionID, target, timeout, info.Status)
	return SessionInfo{} // unreachable
}

// awaitPoolSize subscribes to pool events and waits until the pool reaches
// the target slot count. Uses subscribe rather than polling — same as a
// production client would.
//
// Subscribes BEFORE checking current state to avoid the TOCTOU race.
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

// awaitIdleCount subscribes to status events and waits until at least n
// sessions are idle. Uses subscribe rather than polling.
//
// Subscribes BEFORE checking current state to avoid the TOCTOU race.
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

// --------------------------------------------------------------------
// Subscribe
// --------------------------------------------------------------------

type subscription struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

// close releases the subscription connection. Call this when done waiting
// to avoid accumulating zombie connections over a long flow.
func (s *subscription) close() {
	s.conn.Close()
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

func assertHasChild(t *testing.T, parent SessionInfo, childID string) {
	t.Helper()
	for _, c := range parent.Children {
		if c.SessionID == childID {
			return
		}
	}
	t.Fatalf("expected session %s to have child %s", parent.SessionID, childID)
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
