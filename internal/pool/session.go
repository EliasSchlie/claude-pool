package pool

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Session states.
const (
	StatusQueued     = "queued"
	StatusFresh      = "fresh" // Pre-warmed, not yet idle (startup in progress)
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

// Session represents a managed Claude Code session.
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
	PID          int
	PendingInput string // Un-submitted text detected on prompt line (attach pipe or buffer poll)
	Metadata     SessionMetadata

	// Internal: pool-owned pre-warmed session (can be claimed by start/pin)
	PreWarmed bool
	// Internal: process was recycled via /clear (not freshly spawned).
	// Used to distinguish "clearing" from "spawning" in health slot states.
	Recycled bool

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
// Used by stop to ensure watchIdleSignal doesn't deliver stale prompts.
func (s *Session) ClearPending() {
	s.PendingPrompt = ""
	s.PendingForce = false
	s.PendingResume = ""
}

// IsLive returns true if the session has a live terminal.
func (s *Session) IsLive() bool {
	switch s.Status {
	case StatusFresh, StatusIdle, StatusProcessing:
		return true
	}
	return false
}

// IsBusy returns true if the session is processing or queued.
func (s *Session) IsBusy() bool {
	return s.Status == StatusProcessing || s.Status == StatusQueued
}

// ExternalStatus returns the API-visible status. Fresh is internal —
// externally exposed as idle (or processing if a prompt is pending).
func (s *Session) ExternalStatus() string {
	if s.Status == StatusFresh {
		if s.PendingPrompt != "" {
			return StatusProcessing
		}
		return StatusIdle
	}
	return s.Status
}

// ToMsg converts a session to a protocol message.
// Verbosity levels for session serialization (SPEC: Session Object table).
const (
	VerbosityFlat   = "flat"
	VerbosityNested = "nested"
	VerbosityFull   = "full"
)

// ToMsg converts a session to a protocol message at the given verbosity level.
//
// Verbosity controls which fields are included (SPEC lines 29-47):
//   - flat:   sessionId, status, + priority/pinned/pendingInput only if non-default
//   - nested: same as flat + children
//   - full:   all fields always
func (s *Session) ToMsg(verbosity string) map[string]any {
	m := map[string]any{
		"sessionId": s.ID,
		"status":    s.ExternalStatus(),
	}

	if verbosity == VerbosityFull {
		// Full: always include all fields
		m["priority"] = s.Priority
		m["pinned"] = s.Pinned
		if s.IsLive() {
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
		if s.PID != 0 {
			m["pid"] = float64(s.PID)
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
		// Flat/nested: only ✓* fields when non-default
		if s.Priority != 0 {
			m["priority"] = s.Priority
		}
		if s.Pinned {
			m["pinned"] = s.Pinned
		}
		if s.IsLive() && s.PendingInput != "" {
			m["pendingInput"] = s.PendingInput
		}
	}

	return m
}

// ToMsgWithChildren converts a session to a protocol message with recursive children.
// Verbosity is applied recursively to all children.
// SPEC: children field only included in nested and full verbosity.
func (s *Session) ToMsgWithChildren(allSessions map[string]*Session, verbosity string) map[string]any {
	m := s.ToMsg(verbosity)
	if verbosity == VerbosityNested || verbosity == VerbosityFull {
		children := make([]any, 0)
		for _, other := range allSessions {
			if other.ParentID == s.ID && other.Status != StatusArchived {
				children = append(children, other.ToMsgWithChildren(allSessions, verbosity))
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
