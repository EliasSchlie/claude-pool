// Package pool implements the core pool business logic.
//
// Locking: A single mutex (Manager.mu) guards all state. It is held across
// some I/O (file writes, process spawning). This is intentional — at the
// expected scale (single user, small pools, few concurrent clients) the
// simplicity of a single lock outweighs the contention cost. If concurrent
// load grows significantly, consider a channel-based work queue instead.
package pool

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// Manager is the core pool business logic. All state mutations go through its mutex.
type Manager struct {
	paths    *paths.Pool
	poolName string // Pool name (directory basename), included in health/init responses
	config   *ConfigManager
	hub      *api.SubscriberHub

	// connAcceptedAt returns when a connection was accepted by the server.
	connAcceptedAt func(net.Conn) time.Time

	mu               sync.Mutex
	initialized      bool
	poolSize         int
	sessions         map[string]*Session
	procs            map[string]*ptyPkg.Process
	pidToSID         map[int]string
	pipes            map[string]*attachPipe   // sessionID → attach pipe
	delivering       map[string]chan struct{} // sessionID → closed when in-flight deliverPrompt completes
	terms            map[string]*sessionTerm  // sessionID → persistent headless terminal emulator
	bufferPollSignal chan struct{}            // signals typing poller to re-check immediately
	queue            []*Session
	killTokens       int
	done             chan struct{}
	statusNotify     chan struct{} // closed on every broadcastStatus; waiters select on it for zero-latency status detection
	transcriptDirs   []string      // override transcript search dirs (for testing; empty = default ~/.claude/projects)
}

func NewManager(p *paths.Pool, cfg *ConfigManager) *Manager {
	return &Manager{
		paths:            p,
		poolName:         filepath.Base(p.Root),
		config:           cfg,
		hub:              api.NewSubscriberHub(),
		sessions:         make(map[string]*Session),
		procs:            make(map[string]*ptyPkg.Process),
		pidToSID:         make(map[int]string),
		pipes:            make(map[string]*attachPipe),
		delivering:       make(map[string]chan struct{}),
		terms:            make(map[string]*sessionTerm),
		bufferPollSignal: make(chan struct{}, 1),
		done:             make(chan struct{}),
		statusNotify:     make(chan struct{}),
	}
}

// SetConnAcceptedAt provides the function that returns when a connection was accepted.
func (m *Manager) SetConnAcceptedAt(fn func(net.Conn) time.Time) {
	m.connAcceptedAt = fn
}

// Done returns a channel that's closed when the pool is destroyed.
func (m *Manager) Done() <-chan struct{} {
	return m.done
}

// HandleDisconnect cleans up subscriber state when a connection closes.
func (m *Manager) HandleDisconnect(conn net.Conn) {
	m.hub.RemoveByConn(conn)
}

// Shutdown performs cleanup on daemon exit. Kills all processes —
// this is the one place where process killing is always correct.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pipe := range m.pipes {
		pipe.Close()
	}
	for sid, proc := range m.procs {
		log.Printf("[shutdown] killing session %s (PID %d)", sid, proc.PID())
		proc.Kill()
		proc.Close()
	}
}

// Handle routes an API request to the appropriate handler.
func (m *Manager) Handle(conn net.Conn, req api.Msg) api.Msg {
	id := req["id"]
	msgType, _ := req["type"].(string)
	log.Printf("[api] handling %s (id=%v)", msgType, id)

	switch msgType {
	case "ping":
		return api.Response(id, "pong")
	case "config":
		return m.handleConfig(id, req)
	case "init":
		return m.handleInit(id, req)
	case "health":
		return m.handleHealth(id)
	case "destroy":
		return m.handleDestroy(id, req)
	case "start":
		return m.handleStart(id, req)
	case "followup":
		return m.handleFollowup(id, req)
	case "wait":
		return m.handleWait(id, req)
	case "stop":
		return m.handleStop(id, req)
	case "capture":
		return m.handleCapture(id, req)
	case "info":
		return m.handleInfo(id, req)
	case "ls":
		return m.handleLs(id, req)
	case "offload":
		return m.handleOffload(id, req)
	case "archive":
		return m.handleArchive(id, req)
	case "unarchive":
		return m.handleUnarchive(id, req)
	case "set":
		return m.handleSet(id, req)
	case "pin":
		return m.handlePin(id, req)
	case "unpin":
		return m.handleUnpin(id, req)
	case "set-priority":
		return m.handleSetPriority(id, req)
	case "set-metadata":
		return m.handleSetMetadata(id, req)
	case "resize":
		return m.handleResize(id, req)
	case "input":
		return m.handleInput(id, req)
	case "debug-slots":
		return m.handleDebugSlots(id)
	case "debug-capture":
		return m.handleDebugCapture(id, req)
	case "debug-logs":
		return m.handleDebugLogs(id, req)
	case "attach":
		return m.handleAttach(id, req)
	case "pty-resize":
		return m.handlePtyResize(id, req)
	case "subscribe":
		m.handleSubscribe(conn, req)
		return nil
	default:
		return api.ErrorResponse(id, "unknown command: "+msgType)
	}
}

// --- Broadcasting ---

func (m *Manager) broadcastStatus(s *Session, prevStatus string) {
	// Wake internal waiters (handleWait, waitForSessionReady) on every
	// status change — even suppressed ones (e.g. fresh).
	close(m.statusNotify)
	m.statusNotify = make(chan struct{})

	if s.Status == StatusFresh {
		return // Don't broadcast internal fresh state to external subscribers
	}
	// Don't expose "fresh" as a prevStatus to external consumers
	if prevStatus == StatusFresh {
		prevStatus = StatusIdle
	}
	m.hub.Broadcast(api.Msg{
		"type":       "event",
		"event":      "status",
		"sessionId":  s.ID,
		"status":     s.ExternalStatus(),
		"prevStatus": prevStatus,
	})
}

func (m *Manager) broadcastEvent(event api.Msg) {
	m.hub.Broadcast(event)
}

// --- Utilities ---

func configToMsg(cfg Config) api.Msg {
	m := api.Msg{
		"size":      float64(cfg.Size),
		"keepFresh": float64(cfg.KeepFreshVal()),
	}
	if cfg.Flags != "" {
		m["flags"] = cfg.Flags
	}
	return m
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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

// metadataFromMap extracts SessionMetadata from a persisted map (pool.json / offload meta).
func metadataFromMap(m map[string]any) SessionMetadata {
	raw, _ := m["metadata"].(map[string]any)
	if raw == nil {
		return SessionMetadata{}
	}
	md := SessionMetadata{
		Name:        strVal(raw, "name"),
		Description: strVal(raw, "description"),
	}
	if tags, ok := raw["tags"].(map[string]any); ok {
		md.Tags = make(map[string]string, len(tags))
		for k, v := range tags {
			if sv, ok := v.(string); ok {
				md.Tags[k] = sv
			}
		}
	}
	return md
}

func isPidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func getCwd(pid int) string {
	// Try /proc (Linux)
	link := fmt.Sprintf("/proc/%d/cwd", pid)
	if target, err := os.Readlink(link); err == nil {
		return target
	}
	// Fallback: lsof (macOS — no /proc)
	out, err := exec.Command("lsof", "-a", "-p", fmt.Sprintf("%d", pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}
