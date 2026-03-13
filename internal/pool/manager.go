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
	"strings"
	"sync"
	"syscall"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// Manager is the core pool business logic. All state mutations go through its mutex.
type Manager struct {
	paths  *paths.Pool
	config *ConfigManager
	hub    *api.SubscriberHub

	mu          sync.Mutex
	initialized bool
	poolSize    int
	sessions    map[string]*Session
	procs       map[string]*ptyPkg.Process
	pidToSID    map[int]string
	pipes       map[string]*attachPipe // sessionID → attach pipe
	queue       []*Session
	killTokens  int
	done        chan struct{}
}

func NewManager(p *paths.Pool, cfg *ConfigManager) *Manager {
	return &Manager{
		paths:    p,
		config:   cfg,
		hub:      api.NewSubscriberHub(),
		sessions: make(map[string]*Session),
		procs:    make(map[string]*ptyPkg.Process),
		pidToSID: make(map[int]string),
		pipes:    make(map[string]*attachPipe),
		done:     make(chan struct{}),
	}
}

// Done returns a channel that's closed when the pool is destroyed.
func (m *Manager) Done() <-chan struct{} {
	return m.done
}

// Shutdown performs cleanup on daemon exit.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pipe := range m.pipes {
		pipe.Close()
	}
	for sid, proc := range m.procs {
		log.Printf("killing session %s (PID %d)", sid, proc.PID())
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
	case "pin":
		return m.handlePin(id, req)
	case "unpin":
		return m.handleUnpin(id, req)
	case "set-priority":
		return m.handleSetPriority(id, req)
	case "resize":
		return m.handleResize(id, req)
	case "input":
		return m.handleInput(id, req)
	case "attach":
		return m.handleAttach(id, req)
	case "subscribe":
		m.handleSubscribe(conn, req)
		return nil
	default:
		return api.ErrorResponse(id, "unknown command: "+msgType)
	}
}

// --- Broadcasting ---

func (m *Manager) broadcastStatus(s *Session, prevStatus string) {
	if s.Status == StatusFresh {
		return // Don't broadcast internal fresh state
	}
	// Don't expose "fresh" as a prevStatus to external consumers
	if prevStatus == StatusFresh {
		prevStatus = StatusIdle
	}
	m.hub.Broadcast(api.Msg{
		"type":       "event",
		"event":      "status",
		"sessionId":  s.ID,
		"status":     s.Status,
		"prevStatus": prevStatus,
	})
}

func (m *Manager) broadcastEvent(event api.Msg) {
	m.hub.Broadcast(event)
}

// --- Utilities ---

func configToMsg(cfg Config) api.Msg {
	m := api.Msg{
		"size": float64(cfg.Size),
	}
	if cfg.Flags != "" {
		m["flags"] = cfg.Flags
	}
	return m
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
