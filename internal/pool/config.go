package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Config holds pool configuration (persisted as config.json).
// Known keys (flags, size, dir, keepFresh) are typed fields.
// Arbitrary extra keys are stored in Extra (SPEC: "for session metadata").
type Config struct {
	Flags     string         `json:"flags,omitempty"`
	Size      int            `json:"size,omitempty"`
	Dir       string         `json:"dir,omitempty"`       // Session spawn directory (defaults to pool dir)
	KeepFresh *int           `json:"keepFresh,omitempty"` // Target fresh slot count (nil = use default 1)
	Extra     map[string]any `json:"-"`                   // Arbitrary key-value pairs
}

// knownConfigKeys are the typed fields handled by Update.
var knownConfigKeys = map[string]bool{
	"flags": true, "size": true, "dir": true, "keepFresh": true,
}

// KeepFreshVal returns the effective keepFresh value (default: 1).
func (c Config) KeepFreshVal() int {
	if c.KeepFresh != nil {
		return *c.KeepFresh
	}
	return 1
}

// MarshalJSON merges typed fields with Extra into a single flat object.
func (c Config) MarshalJSON() ([]byte, error) {
	m := make(map[string]any)
	for k, v := range c.Extra {
		m[k] = v
	}
	if c.Flags != "" {
		m["flags"] = c.Flags
	}
	if c.Size != 0 {
		m["size"] = c.Size
	}
	if c.Dir != "" {
		m["dir"] = c.Dir
	}
	if c.KeepFresh != nil {
		m["keepFresh"] = *c.KeepFresh
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads typed fields and puts the rest into Extra.
func (c *Config) UnmarshalJSON(data []byte) error {
	// Decode known fields via alias to avoid recursion
	type Alias Config
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = Config(a)

	// Decode all keys to find extras
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if knownConfigKeys[k] {
			continue
		}
		if c.Extra == nil {
			c.Extra = make(map[string]any)
		}
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			c.Extra[k] = string(v)
		} else {
			c.Extra[k] = val
		}
	}
	return nil
}

// ConfigManager handles reading/writing config.json with a mutex.
type ConfigManager struct {
	path string
	mu   sync.Mutex
}

func NewConfigManager(path string) *ConfigManager {
	return &ConfigManager{path: path}
}

// Load reads config.json from disk. Returns zero Config if file doesn't exist.
func (cm *ConfigManager) Load() (Config, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.loadLocked()
}

func (cm *ConfigManager) loadLocked() (Config, error) {
	data, err := os.ReadFile(cm.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes config.json to disk.
func (cm *ConfigManager) Save(cfg Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.saveLocked(cfg)
}

func (cm *ConfigManager) saveLocked(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cm.path, append(data, '\n'), 0644)
}

// Update applies partial changes to the config. Known keys are typed;
// unknown keys go into Extra (SPEC: arbitrary key-value pairs).
func (cm *ConfigManager) Update(update map[string]any) (Config, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cfg, err := cm.loadLocked()
	if err != nil {
		return Config{}, err
	}

	if v, ok := update["size"]; ok {
		var n int
		switch val := v.(type) {
		case float64:
			n = int(val)
		case int:
			n = val
		default:
			return Config{}, fmt.Errorf("size must be a number")
		}
		if n < 1 {
			return Config{}, fmt.Errorf("size must be >= 1")
		}
		cfg.Size = n
	}
	if v, ok := update["flags"].(string); ok {
		cfg.Flags = v
	}
	if v, ok := update["dir"].(string); ok {
		cfg.Dir = v
	}
	if v, ok := update["keepFresh"]; ok {
		var n int
		switch val := v.(type) {
		case float64:
			n = int(val)
		case int:
			n = val
		default:
			return Config{}, fmt.Errorf("keepFresh must be a number")
		}
		if n < 0 {
			return Config{}, fmt.Errorf("keepFresh must be >= 0")
		}
		cfg.KeepFresh = &n
	}

	// Store unknown keys in Extra
	for k, v := range update {
		if knownConfigKeys[k] {
			continue
		}
		if cfg.Extra == nil {
			cfg.Extra = make(map[string]any)
		}
		cfg.Extra[k] = v
	}

	if err := cm.saveLocked(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
