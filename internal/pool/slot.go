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

// cleanup closes the pipe, stops the terminal, and kills the process.
// Must be called with m.mu held.
func (sl *Slot) cleanup(m *Manager) {
	if sl.Pipe != nil {
		sl.Pipe.Close()
		sl.Pipe = nil
	}
	m.stopSlotTerm(sl)
	if sl.Process != nil {
		sl.Process.Kill()
		sl.Process.Close()
		sl.Process = nil
	}
	sl.ClearQueue = nil
}

// PID returns the process ID or 0 if no process.
func (sl *Slot) PID() int {
	if sl.Process == nil {
		return 0
	}
	return sl.Process.PID()
}
