package pool

import (
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// Slot states. SPEC: Slot States table.
const (
	SlotSpawning   = "spawning"
	SlotFresh      = "fresh"
	SlotClearing   = "clearing"
	SlotResuming   = "resuming"
	SlotIdle       = "idle"
	SlotProcessing = "processing"
	SlotCrashed    = "crashed"
)

// Slot is a physical resource — a running Claude Code process with a PTY.
// Count = pool size. Created at init, persists until destroy.
// SPEC: "A slot is a physical resource — a running Claude Code process with a PTY."
type Slot struct {
	Index   int
	State   string
	Process *ptyPkg.Process
	Term    *sessionTerm
	Pipe    *attachPipe

	// Delivering is closed when an in-flight deliverPrompt completes.
	Delivering chan struct{}

	// SessionID is the session currently loaded in this slot ("" when unoccupied).
	SessionID string

	// ClearQueue holds remaining steps for the multi-step clear workflow
	// (/clear → /update-plugins → /clear). Popped one at a time as each
	// step completes (detected by the typing poller).
	ClearQueue []string

	// PendingInput is detected at the slot level by the typing poller,
	// then surfaced to the session.
	PendingInput string
}

// IsOccupied returns true if a session is loaded in this slot.
func (sl *Slot) IsOccupied() bool {
	return sl.SessionID != ""
}

// IsLive returns true if the slot has an active process.
func (sl *Slot) IsLive() bool {
	switch sl.State {
	case SlotFresh, SlotClearing, SlotResuming, SlotIdle, SlotProcessing:
		return true
	}
	return false
}

// IsFresh returns true if the slot is ready for immediate use.
func (sl *Slot) IsFresh() bool {
	return sl.State == SlotFresh
}

// PID returns the process ID or 0 if no process.
func (sl *Slot) PID() int {
	if sl.Process == nil {
		return 0
	}
	return sl.Process.PID()
}
