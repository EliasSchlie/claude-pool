package pool

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Session states.
const (
	StatusQueued     = "queued"
	StatusIdle       = "idle"
	StatusProcessing = "processing"
	StatusOffloaded  = "offloaded"
	StatusError      = "error"
	StatusArchived   = "archived"
)

// maxSpawnAttempts is the number of consecutive spawn failures before a session
// is marked as error. SPEC: "After repeated failures (implementation decides
// the threshold), the session is marked error."
const maxSpawnAttempts = 3

// SessionMetadata holds user-defined session labels.
type SessionMetadata struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

// Session represents a managed Claude Code session — pure identity and lifecycle.
// Process ownership lives on the Slot.
type Session struct {
	ID           string
	ClaudeUUID   string
	Status       string
	ParentID     string
	Priority     float64
	Pinned       bool
	PinExpiry    time.Time
	SpawnCwd     string
	Cwd          string
	CreatedAt    time.Time
	LastUsedAt   time.Time // updated on prompt delivery, used for LRU eviction
	PendingInput string    // surfaced from slot-level detection
	Metadata     SessionMetadata

	// SlotIndex is the index of the slot hosting this session (-1 when not loaded).
	SlotIndex int

	// Internal: pending prompt for queued sessions
	PendingPrompt string
	PendingForce  bool
	PendingResume string // Claude UUID to /resume before delivering PendingPrompt

	// Internal: spawn retry tracking
	SpawnAttempts int // consecutive spawn failures (reset on success)

	// Internal: for offloaded/archived metadata persistence
	Flags string // flags used to spawn this session
}

// ClearPending cancels all pending work (prompt, force, resume).
// Used by stop to ensure idle transitions don't deliver stale prompts.
func (s *Session) ClearPending() {
	s.PendingPrompt = ""
	s.PendingForce = false
	s.PendingResume = ""
}

// IsLoaded returns true if the session is loaded in a slot.
func (s *Session) IsLoaded() bool {
	return s.SlotIndex >= 0
}

// IsLive returns true if the session is loaded and active.
func (s *Session) IsLive() bool {
	return s.Status == StatusIdle || s.Status == StatusProcessing
}

// Verbosity levels for session serialization (SPEC: Session Object table).
const (
	VerbosityFlat   = "flat"
	VerbosityNested = "nested"
	VerbosityFull   = "full"
)

// ToMsg converts a session to a protocol message at the given verbosity level.
func (s *Session) ToMsg(verbosity string, slotPID int) map[string]any {
	m := map[string]any{
		"sessionId": s.ID,
		"status":    s.Status,
	}

	if verbosity == VerbosityFull {
		m["priority"] = s.Priority
		m["pinned"] = s.Pinned
		if s.IsLoaded() {
			m["pendingInput"] = s.PendingInput
		}
		m["parent"] = s.ParentID
		m["cwd"] = s.Cwd
		if s.ClaudeUUID != "" {
			m["claudeUUID"] = s.ClaudeUUID
		} else {
			m["claudeUUID"] = nil
		}
		m["spawnCwd"] = s.SpawnCwd
		m["createdAt"] = s.CreatedAt.UTC().Format(time.RFC3339)
		if slotPID != 0 {
			m["pid"] = float64(slotPID)
		} else {
			m["pid"] = nil
		}
		meta := map[string]any{}
		for k, v := range s.Metadata.Tags {
			meta[k] = v
		}
		if s.Metadata.Name != "" {
			meta["name"] = s.Metadata.Name
		}
		if s.Metadata.Description != "" {
			meta["description"] = s.Metadata.Description
		}
		m["metadata"] = meta
	} else {
		if s.Priority != 0 {
			m["priority"] = s.Priority
		}
		if s.Pinned {
			m["pinned"] = s.Pinned
		}
		if s.IsLoaded() && s.PendingInput != "" {
			m["pendingInput"] = s.PendingInput
		}
	}

	return m
}

// IsChildOf returns true if this session's parent matches the given session.
func (s *Session) IsChildOf(parent *Session) bool {
	if s.ParentID == "" {
		return false
	}
	return s.ParentID == parent.ID || (parent.ClaudeUUID != "" && s.ParentID == parent.ClaudeUUID)
}

// ToMsgWithChildren converts a session to a protocol message with recursive children.
func (s *Session) ToMsgWithChildren(allSessions map[string]*Session, verbosity string, pidLookup func(string) int) map[string]any {
	m := s.ToMsg(verbosity, pidLookup(s.ID))
	if verbosity == VerbosityNested || verbosity == VerbosityFull {
		children := make([]any, 0)
		for _, other := range allSessions {
			if other.IsChildOf(s) && other.Status != StatusArchived {
				children = append(children, other.ToMsgWithChildren(allSessions, verbosity, pidLookup))
			}
		}
		m["children"] = children
	}
	return m
}

func generateSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:12]
}
