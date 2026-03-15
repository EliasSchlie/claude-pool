# Plan: CLI-First Integration Tests

## Checklist

### Phase 1: Write Tests (no implementation changes)

- [ ] Delete `tests/cli/` directory entirely
- [ ] Rewrite `tests/integration/helpers_test.go`:
  - Build daemon + CLI binaries in TestMain
  - `setupCLIPool(t, size)`: runs `init` via CLI (starts daemon), returns `cliPool`
  - `cliPool.run(args...)`: run CLI command → stdout/stderr/exitCode
  - `cliPool.runJSON(args...)`: run with `--json`, parse output
  - `cliPool.waitForStatus(sessionID, status, timeout)`: poll `info --json`
  - `cliPool.waitForIdle(sessionID, timeout)`: use `wait` CLI command
  - `cliPool.waitForIdleCount(n, timeout)`: poll `health --json`
  - Keep socket-based helpers for attach/subscribe tests only
  - Cleanup: `destroy --confirm` via CLI
- [ ] Rewrite `pool_test.go` → CLI:
  - init, ping, health, config read/set, resize, destroy, re-init with/without restore
  - Drop "ping before init" (daemon not running yet in CLI model)
  - Drop "config before init" (same reason)
- [ ] Rewrite `session_test.go` → CLI:
  - start, wait, info, set (metadata via --meta), capture (all source/turns/detail combos), followup, stop
  - Drop `followup --force` (removed from spec)
  - Drop `input` (now `debug input`, tested separately or in debug test)
  - Use `set --meta key=value` instead of `set-metadata` API
  - Test `--block` on start
- [ ] Rewrite `slots_test.go` → CLI:
  - Queue behavior, `set --priority`, `set --pinned`, eviction order, LRU
  - Drop `pin without sessionId` (spec requires `--session`)
  - Drop `followup --force on queued` (no force in spec)
  - Use `set --pinned <seconds>` and `set --pinned false` instead of pin/unpin
- [ ] Rewrite `offload_test.go` → CLI:
  - No explicit `offload` command — trigger via eviction (start session when pool full)
  - Test capture jsonl on offloaded (works), capture buffer on offloaded (errors)
  - Test followup restores offloaded
  - Test process death → offloaded (kill PID from info)
  - Test archive/unarchive lifecycle
  - Drop `offload pinned auto-unpins` (no offload command)
  - Drop `offload non-idle errors` (no offload command)
- [ ] Rewrite `parent_child_test.go` → CLI:
  - `--parent` flag on start, info shows children, ls with `--parent` filter
  - `--verbosity nested/full` instead of `--tree`
  - archive with unarchived children errors, `archive --recursive`
  - Test `--parent none` to disable auto-detection
- [ ] Keep `attach_test.go` as socket-based (API-only feature)
  - But use CLI `init` for setup to keep consistent
- [ ] Keep `subscribe_test.go` as socket-based (API-only feature)
  - But use CLI `init` for setup to keep consistent
- [ ] Update `tests/integration/CLAUDE.md` to reflect CLI-based approach

### Spec-to-test mapping (must be covered)

#### CLI Commands → Test Coverage:
| Command | Test file(s) |
|---------|-------------|
| `start --prompt --parent --block` | session, slots, parent_child |
| `followup --session --prompt --block` | session, offload |
| `wait --session --parent --timeout --source --turns --detail` | session |
| `capture --session --source --turns --detail` | session, offload |
| `stop --session` | session, slots |
| `ls --parent --status --archived --verbosity` | parent_child, offload |
| `info --session --verbosity` | session, parent_child |
| `archive --session --recursive` | offload, parent_child |
| `unarchive --session` | offload |
| `set --session --priority --pinned --meta` | session (meta), slots (priority, pinned) |
| `init --size --flags --dir --no-restore` | pool |
| `health` | pool |
| `resize --size` | pool |
| `config --set` | pool |
| `destroy --confirm` | pool |
| `ping` | pool |
| `pools` | pool |
| `debug input` | session (moved from main flow) |
| `debug capture` | (optional, low priority) |
| `debug slots` | (optional, low priority) |
| `debug logs` | (optional, low priority) |

#### Key Spec Behaviors → Test Coverage:
| Behavior | Test |
|----------|------|
| Session states: queued/idle/processing/offloaded/error/archived | slots, session, offload |
| Eviction: priority → LRU → pinned protected | slots |
| Processing sessions never evicted | slots |
| Output capture: jsonl default, buffer requires live session | session, offload |
| Turns/detail filtering | session |
| Parent auto-detection from CLAUDE_POOL_SESSION_ID env | parent_child |
| `--parent none` disables auto-detection | parent_child |
| ls deduplication (nested/full) | parent_child |
| Archive errors with unarchived children | parent_child |
| Archive --recursive | parent_child |
| Followup errors on busy/queued/archived | session, slots |
| Stop cancels queued, interrupts processing | slots, session |
| Prefix resolution for session IDs | session |
| Destroy without --confirm errors | pool |
| Init errors if already running | pool |
| Re-init restores sessions | pool |
| Re-init with --no-restore | pool |
| Config persistence across restarts | pool |
| Resize up/down | pool |
| Pinned sessions protected from resize eviction | pool |
| `--json` flag produces valid JSON | session (implicitly via runJSON) |
| Human-readable output (non-json mode) | session |
| Attach (API-only) | attach |
| Subscribe (API-only) | subscribe |

### Phase 2: Implement CLI (no test changes unless obvious bugs)

- [ ] Implement `init` in CLI (start daemon, write config, register pool)
- [ ] Implement `pools` in CLI (read registry)
- [ ] Implement `set` command (unified priority/pinned/meta)
- [ ] Implement `stop` command
- [ ] Implement `unarchive` command
- [ ] Implement `resize` command
- [ ] Implement `config` command
- [ ] Implement `debug input/capture/slots/logs` commands
- [ ] Update `start` for spec compliance (--block, --parent behavior)
- [ ] Update `wait` for spec compliance (--parent filter)
- [ ] Update `ls` for spec compliance (--parent, --status, --verbosity, no --all/--tree)
- [ ] Update `followup` (remove --force)
- [ ] Update field names (parent vs parentId)
- [ ] Run all tests → fix implementation until green
- [ ] Remove old commands from CLI (pin, unpin, set-priority, offload)

### Phase 3: Manual Verification

- [ ] Test every CLI command by hand
- [ ] Verify `--json` vs human output for each command
- [ ] Verify error messages are helpful
- [ ] Test parent auto-detection from CLAUDE_POOL_SESSION_ID
- [ ] Test prefix resolution
- [ ] Test edge cases (empty pool, all archived, etc.)
