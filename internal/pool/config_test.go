package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigExtraRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cm := NewConfigManager(path)

	t.Run("set and read arbitrary keys", func(t *testing.T) {
		cfg, err := cm.Update(map[string]any{
			"flags":   "--test",
			"size":    float64(2),
			"custom1": "hello",
			"custom2": float64(42),
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Extra["custom1"] != "hello" {
			t.Fatalf("expected custom1='hello', got %v", cfg.Extra["custom1"])
		}
		if cfg.Extra["custom2"] != float64(42) {
			t.Fatalf("expected custom2=42, got %v", cfg.Extra["custom2"])
		}
	})

	t.Run("arbitrary keys persist across load", func(t *testing.T) {
		cfg, err := cm.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Extra["custom1"] != "hello" {
			t.Fatalf("expected custom1='hello' after reload, got %v", cfg.Extra["custom1"])
		}
		if cfg.Flags != "--test" {
			t.Fatalf("expected flags='--test', got %q", cfg.Flags)
		}
	})

	t.Run("known keys not duplicated in Extra", func(t *testing.T) {
		cfg, _ := cm.Load()
		if _, exists := cfg.Extra["flags"]; exists {
			t.Fatal("'flags' should not appear in Extra")
		}
		if _, exists := cfg.Extra["size"]; exists {
			t.Fatal("'size' should not appear in Extra")
		}
	})

	t.Run("MarshalJSON produces flat object", func(t *testing.T) {
		cfg, _ := cm.Load()
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		if m["flags"] != "--test" {
			t.Fatalf("expected flags in JSON, got %v", m["flags"])
		}
		if m["custom1"] != "hello" {
			t.Fatalf("expected custom1 in JSON, got %v", m["custom1"])
		}
	})

	t.Run("ToMap includes extras", func(t *testing.T) {
		cfg, _ := cm.Load()
		m := cfg.ToMap()
		if m["custom1"] != "hello" {
			t.Fatalf("expected custom1 in ToMap, got %v", m["custom1"])
		}
		if m["flags"] != "--test" {
			t.Fatalf("expected flags in ToMap, got %v", m["flags"])
		}
	})

	t.Run("file on disk has extra keys", func(t *testing.T) {
		data, _ := os.ReadFile(path)
		var m map[string]any
		json.Unmarshal(data, &m)
		if m["custom1"] != "hello" {
			t.Fatalf("expected custom1 in file, got %v", m["custom1"])
		}
	})
}

func TestConfigToMsg(t *testing.T) {
	kf := 2
	cfg := Config{
		Flags:     "--test",
		Size:      3,
		KeepFresh: &kf,
		Extra:     map[string]any{"myKey": "myVal"},
	}
	m := configToMsg(cfg)
	if m["flags"] != "--test" {
		t.Fatalf("expected flags, got %v", m["flags"])
	}
	if m["myKey"] != "myVal" {
		t.Fatalf("expected myKey in configToMsg, got %v", m["myKey"])
	}
	// size and keepFresh should always be present as float64
	if m["size"] != float64(3) {
		t.Fatalf("expected size=3, got %v", m["size"])
	}
	if m["keepFresh"] != float64(2) {
		t.Fatalf("expected keepFresh=2, got %v", m["keepFresh"])
	}
}
