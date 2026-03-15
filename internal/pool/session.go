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
	PinExpiry    time.Time // TODO: not yet enforced — no goroutine checks for expiration
	SpawnCwd     string
	Cwd          string
	CreatedAt    time.Time
	LastUsedAt   time.Time // updated on prompt delivery, used for LRU eviction
	PID          int
	PendingInput string // Un-submitted text in terminal buffer (attach pipe)
	Metadata     SessionMetadata

	// Internal: pool-owned pre-warmed session (can be claimed by start/pin)
	PreWarmed bool

	// Internal: pending prompt for queued sessions
	PendingPrompt string
	PendingForce  bool

	// Internal: for offloaded/archived metadata persistence
	Flags string // flags used to spawn this session
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
func (s *Session) ToMsg() map[string]any {
	m := map[string]any{
		"sessionId": s.ID,
		"status":    s.ExternalStatus(),
		"priority":  s.Priority,
		"pinned":    s.Pinned,
		"createdAt": s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.ClaudeUUID != "" {
		m["claudeUUID"] = s.ClaudeUUID
	}
	if s.ParentID != "" {
		m["parent"] = s.ParentID
	}
	if s.Cwd != "" {
		m["cwd"] = s.Cwd
	}
	if s.SpawnCwd != "" {
		m["spawnCwd"] = s.SpawnCwd
	}
	if s.PID != 0 {
		m["pid"] = float64(s.PID)
	}
	// Always include pendingInput for loaded sessions (empty string = nothing typed)
	if s.IsLive() {
		m["pendingInput"] = s.PendingInput
	}
	// Metadata: flat key-value pairs (spec: "Arbitrary key-value pairs")
	// Tags are the primary storage; Name/Description included for backward compat.
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
	return m
}

// ToMsgWithChildren converts a session to a protocol message with recursive children.
func (s *Session) ToMsgWithChildren(allSessions map[string]*Session) map[string]any {
	m := s.ToMsg()
	children := make([]any, 0)
	for _, other := range allSessions {
		if other.ParentID == s.ID && other.Status != StatusArchived {
			children = append(children, other.ToMsgWithChildren(allSessions))
		}
	}
	m["children"] = children
	return m
}

func generateSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:12]
}
