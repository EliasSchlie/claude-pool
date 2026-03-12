package pool

import (
	"net"
	"sync"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
)

// Manager is the core pool business logic. All state mutations go through its mutex.
type Manager struct {
	paths  *paths.Pool
	config *ConfigManager

	mu          sync.Mutex
	initialized bool
	done        chan struct{} // closed on destroy
}

func NewManager(p *paths.Pool, cfg *ConfigManager) *Manager {
	return &Manager{
		paths:  p,
		config: cfg,
		done:   make(chan struct{}),
	}
}

// Done returns a channel that's closed when the pool is destroyed.
func (m *Manager) Done() <-chan struct{} {
	return m.done
}

// Shutdown performs cleanup on daemon exit.
func (m *Manager) Shutdown() {
	// TODO: cleanup PTYs, save state
}

// Handle routes an API request to the appropriate handler.
func (m *Manager) Handle(conn net.Conn, req api.Msg) api.Msg {
	id := req["id"]
	msgType, _ := req["type"].(string)

	switch msgType {
	case "ping":
		return m.handlePing(id)
	case "config":
		return m.handleConfig(id, req)
	case "init":
		return m.handleInit(id, req)
	case "health":
		return m.handleHealth(id)
	case "destroy":
		return m.handleDestroy(id, req)
	default:
		return api.ErrorResponse(id, "unknown command: "+msgType)
	}
}

func (m *Manager) handlePing(id any) api.Msg {
	return api.Response(id, "pong")
}

func (m *Manager) handleConfig(id any, req api.Msg) api.Msg {
	if setMap, ok := req["set"].(map[string]any); ok {
		cfg, err := m.config.Update(setMap)
		if err != nil {
			return api.ErrorResponse(id, "config update failed: "+err.Error())
		}
		return api.ConfigResponse(id, configToMsg(cfg))
	}

	cfg, err := m.config.Load()
	if err != nil {
		return api.ErrorResponse(id, "config read failed: "+err.Error())
	}
	return api.ConfigResponse(id, configToMsg(cfg))
}

func (m *Manager) handleInit(id any, req api.Msg) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.initialized {
		return api.ErrorResponse(id, "pool already initialized")
	}

	// Read size from request or config
	cfg, _ := m.config.Load()
	size := cfg.Size
	if reqSize, ok := req["size"].(float64); ok {
		size = int(reqSize)
	}
	if size <= 0 {
		return api.ErrorResponse(id, "pool size must be positive")
	}

	m.initialized = true

	// TODO: spawn sessions
	sessions := make([]any, 0)

	return api.Response(id, "pool", api.Msg{
		"pool": api.Msg{
			"size":     float64(size),
			"sessions": sessions,
		},
	})
}

func (m *Manager) handleHealth(id any) api.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.initialized {
		return api.ErrorResponse(id, "pool not initialized")
	}

	// TODO: real health data
	return api.Response(id, "health", api.Msg{
		"health": api.Msg{
			"size":       float64(0),
			"counts":     api.Msg{"idle": float64(0)},
			"queueDepth": float64(0),
			"sessions":   []any{},
		},
	})
}

func (m *Manager) handleDestroy(id any, req api.Msg) api.Msg {
	confirm, _ := req["confirm"].(bool)
	if !confirm {
		return api.ErrorResponse(id, "destroy requires confirm: true")
	}

	m.mu.Lock()
	m.initialized = false
	m.mu.Unlock()

	// TODO: kill all sessions

	// Signal daemon to exit
	select {
	case <-m.done:
	default:
		close(m.done)
	}

	return api.OkResponse(id)
}

func configToMsg(cfg Config) api.Msg {
	m := api.Msg{
		"size": float64(cfg.Size),
	}
	if cfg.Flags != "" {
		m["flags"] = cfg.Flags
	}
	return m
}
