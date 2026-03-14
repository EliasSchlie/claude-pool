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
	StatusTyping     = "typing"
	StatusProcessing = "processing"
	StatusOffloaded  = "offloaded"
	StatusDead       = "dead"
	StatusError      = "error"
	StatusArchived   = "archived"
)

// Session represents a managed Claude Code session.
type Session struct {
	ID         string
	ClaudeUUID string
	Status     string
	ParentID   string
	Priority   float64
	Pinned     bool
	PinExpiry  time.Time // TODO: not yet enforced — no goroutine checks for expiration
	SpawnCwd   string
	Cwd        string
	CreatedAt  time.Time
	LastUsedAt time.Time // updated on prompt delivery, used for LRU eviction
	PID        int

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
	case StatusFresh, StatusIdle, StatusTyping, StatusProcessing:
		return true
	}
	return false
}

// IsBusy returns true if the session is processing or queued.
func (s *Session) IsBusy() bool {
	return s.Status == StatusProcessing || s.Status == StatusQueued
}

// ToMsg converts a session to a protocol message.
func (s *Session) ToMsg() map[string]any {
	// Fresh is internal — externally exposed as idle (or processing if
	// a prompt is pending delivery).
	status := s.Status
	if status == StatusFresh {
		if s.PendingPrompt != "" {
			status = StatusProcessing
		} else {
			status = StatusIdle
		}
	}
	m := map[string]any{
		"sessionId": s.ID,
		"status":    status,
		"priority":  s.Priority,
		"pinned":    s.Pinned,
		"createdAt": s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.ClaudeUUID != "" {
		m["claudeUUID"] = s.ClaudeUUID
	}
	if s.ParentID != "" {
		m["parentId"] = s.ParentID
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
