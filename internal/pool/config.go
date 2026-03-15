package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Config holds pool configuration (persisted as config.json).
type Config struct {
	Flags string `json:"flags,omitempty"`
	Size  int    `json:"size,omitempty"`
	Dir   string `json:"dir,omitempty"` // Session spawn directory (defaults to pool dir)
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

// Update applies partial changes to the config. Only non-zero fields in the
// update are applied. Returns the updated config.
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

	if err := cm.saveLocked(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
