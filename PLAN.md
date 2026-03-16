# Process Reuse Implementation

## Goal
Replace kill+respawn with `/clear` + `/resume` so slot processes are never killed.

## Key Insight
After `/clear`, a process goes through the same startup cycle as a fresh spawn (SessionStart hook → idle signal). So a `/clear`ed process becomes a pre-warmed slot naturally.

## Changes

### 1. Session struct (`session.go`)
Add `PendingResume string` — Claude UUID to `/resume` before delivering PendingPrompt.

### 2. `offloadSessionLocked(s)` — rewrite
**Before:** Kill process, delete from maps.
**After:**
1. Close attach pipe
2. Save offload meta, mark session offloaded (PID=0)
3. Create new pre-warmed session (StatusFresh), assign process to it
4. Send `/clear` to process (async goroutine)
5. Start watchers on pre-warmed session
6. Idle signal watcher transitions pre-warmed → idle when /clear completes

### 3. `tryDequeue()` — rewrite
**Before:** Count active sessions, spawn new processes for deficit.
**After:** Find fresh/idle pre-warmed slots and transfer to queued sessions.
- Resume case (ClaudeUUID set): set `PendingResume`, transfer, StatusFresh
- New session case: transfer, deliver prompt if slot idle, else PendingPrompt

### 4. `watchIdleSignal` — add PendingResume stage
Before PendingPrompt check: if PendingResume set, deliver `/resume <uuid>`, stay processing.
Next idle signal delivers PendingPrompt (existing flow).

### 5. `transferProcess` — preserve existing ClaudeUUID
Only copy ClaudeUUID from source if target doesn't already have one.

### 6. `maintainFreshSlots()` — simplify
Don't spawn replacement after offload — offload now creates a pre-warmed slot.

### 7. `tryKillTokens` — shrink pool
After offloading, delete the created pre-warmed session too (to actually reduce count).

### 8. Keep `spawnSession` for:
- `handleInit` (daemon startup, no live processes exist)
- `tryReplaceDeadSessions` (actual process death)

### 9. Keep `Shutdown()`/`handleDestroy` killing processes (legitimate cleanup).
