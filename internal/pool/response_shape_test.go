package pool

import (
	"testing"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// ============================================================
// Response shape tests
//
// Validates that API responses match SPEC.md's contracts:
// - ToMsg respects verbosity levels (SPEC lines 29-47)
// - init response = health response is tested in integration
//   tests (pool_test.go) since handleInit has side effects
// ============================================================

// --- ToMsg verbosity: flat ---

// Prevents: flat verbosity leaking full-only fields like claudeUUID, parent, cwd, etc.
func TestToMsgFlatOmitsFullFields(t *testing.T) {
	s := &Session{
		ID:         "test123",
		Status:     StatusIdle,
		ClaudeUUID: "uuid-abc",
		ParentID:   "parent-xyz",
		Cwd:        "/some/path",
		SpawnCwd:   "/spawn/path",
		CreatedAt:  time.Now(),
		PID:        9999,
		Metadata:   SessionMetadata{Name: "test"},
	}

	msg := s.ToMsg(VerbosityFlat)

	// flat MUST have sessionId and status
	if _, ok := msg["sessionId"]; !ok {
		t.Error("flat: missing sessionId")
	}
	if _, ok := msg["status"]; !ok {
		t.Error("flat: missing status")
	}

	// flat MUST NOT have full-only fields
	fullOnlyFields := []string{"claudeUUID", "parent", "cwd", "spawnCwd", "createdAt", "pid", "metadata"}
	for _, field := range fullOnlyFields {
		if _, ok := msg[field]; ok {
			t.Errorf("flat: should not include %q (full-only field)", field)
		}
	}
}

// Prevents: flat verbosity always including priority/pinned/pendingInput
// instead of only when non-default (SPEC: "✓* = only shown if non-default")
func TestToMsgFlatConditionalFields(t *testing.T) {
	t.Run("default values omitted", func(t *testing.T) {
		s := &Session{
			ID:       "test123",
			Status:   StatusIdle,
			Priority: 0,
			Pinned:   false,
		}
		msg := s.ToMsg(VerbosityFlat)

		if _, ok := msg["priority"]; ok {
			t.Error("flat: priority=0 (default) should be omitted")
		}
		if _, ok := msg["pinned"]; ok {
			t.Error("flat: pinned=false (default) should be omitted")
		}
		if _, ok := msg["pendingInput"]; ok {
			t.Error("flat: pendingInput should be omitted when session is not live or input is empty")
		}
	})

	t.Run("non-default values included", func(t *testing.T) {
		s := &Session{
			ID:           "test123",
			Status:       StatusIdle,
			Priority:     5,
			Pinned:       true,
			PendingInput: "some text",
		}
		msg := s.ToMsg(VerbosityFlat)

		if _, ok := msg["priority"]; !ok {
			t.Error("flat: priority=5 (non-default) should be included")
		}
		if _, ok := msg["pinned"]; !ok {
			t.Error("flat: pinned=true (non-default) should be included")
		}
		if _, ok := msg["pendingInput"]; !ok {
			t.Error("flat: pendingInput with text should be included for live session")
		}
	})
}

// --- ToMsg verbosity: full ---

// Prevents: full verbosity missing fields that the spec requires
func TestToMsgFullIncludesAllFields(t *testing.T) {
	s := &Session{
		ID:         "test123",
		Status:     StatusIdle,
		ClaudeUUID: "uuid-abc",
		ParentID:   "parent-xyz",
		Cwd:        "/some/path",
		SpawnCwd:   "/spawn/path",
		CreatedAt:  time.Now(),
		PID:        9999,
		Priority:   0,
		Pinned:     false,
		Metadata:   SessionMetadata{Name: "test"},
	}

	msg := s.ToMsg(VerbosityFull)

	requiredFields := []string{
		"sessionId", "status", "priority", "pinned",
		"parent", "cwd", "claudeUUID", "spawnCwd", "createdAt", "pid", "metadata",
	}
	for _, field := range requiredFields {
		if _, ok := msg[field]; !ok {
			t.Errorf("full: missing required field %q", field)
		}
	}
}

// --- buildHealthResponse does not deadlock when lock is held ---

// Prevents: buildHealthResponse re-acquiring m.mu (which would deadlock
// when called from handleInit, which already holds the lock).
func TestBuildHealthResponseWithLockHeld(t *testing.T) {
	m := newTestManager(t)
	m.initialized = true
	m.poolSize = 2

	m.sessions["a"] = &Session{ID: "a", Status: StatusIdle, CreatedAt: time.Now(), PID: 123}
	m.sessions["b"] = &Session{ID: "b", Status: StatusOffloaded, CreatedAt: time.Now()}

	done := make(chan api.Msg, 1)
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		done <- m.buildHealthResponse(1)
	}()

	select {
	case resp := <-done:
		health, ok := resp["health"].(api.Msg)
		if !ok {
			t.Fatal("missing health object in response")
		}
		if numVal(health, "size") != 2 {
			t.Errorf("expected size 2, got %v", health["size"])
		}
		if _, ok := health["counts"]; !ok {
			t.Error("missing counts")
		}
		if _, ok := health["queueDepth"]; !ok {
			t.Error("missing queueDepth")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: buildHealthResponse blocked while lock held")
	}
}

// --- helper ---

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		paths:    paths.New(t.TempDir()),
		sessions: make(map[string]*Session),
		procs:    make(map[string]*ptyPkg.Process),
		pidToSID: make(map[int]string),
		pipes:    make(map[string]*attachPipe),
		done:     make(chan struct{}),
	}
}
