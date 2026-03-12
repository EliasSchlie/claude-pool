package integration

// helpers_test.go — Shared test infrastructure
//
// Provides:
//   - setupPool(t, size): starts a daemon with a temp pool directory, configures
//     --model haiku and --dangerously-skip-permissions, calls init with the given
//     size, registers t.Cleanup to destroy+kill daemon. Returns a *testPool.
//   - testPool: wraps a socket connection with typed methods for every protocol
//     command. Methods send JSON, parse the response, and fail the test on errors.
//   - Assertion helpers: assertStatus, assertError, assertSessionCount, etc.
//
// The testPool methods mirror the protocol 1:1:
//   - pool.ping() → pong
//   - pool.init(size) / pool.initNoRestore(size)
//   - pool.resize(size)
//   - pool.health() → healthReport
//   - pool.destroy()
//   - pool.config() / pool.configSet(fields)
//   - pool.start(prompt) → sessionId
//   - pool.startWithParent(prompt, parentId) → sessionId
//   - pool.followup(sessionId, prompt) → status
//   - pool.followupForce(sessionId, prompt) → status
//   - pool.wait(sessionId) → content
//   - pool.waitAny() → (sessionId, content)
//   - pool.waitFormat(sessionId, format) → content
//   - pool.capture(sessionId) → content
//   - pool.captureFormat(sessionId, format) → content
//   - pool.input(sessionId, data)
//   - pool.offload(sessionId)
//   - pool.ls() → []session
//   - pool.lsAll() → []session
//   - pool.lsTree() → []session
//   - pool.lsArchived() → []session
//   - pool.info(sessionId) → session
//   - pool.pin(sessionId) → (sessionId, status)
//   - pool.pinFresh() → (sessionId, status)
//   - pool.unpin(sessionId)
//   - pool.stop(sessionId)
//   - pool.archive(sessionId)
//   - pool.archiveRecursive(sessionId)
//   - pool.unarchive(sessionId)
//   - pool.setPriority(sessionId, priority)
//   - pool.attach(sessionId) → socketPath
//   - pool.subscribe(opts) → *subscription (reads events via sub.next())
//
// All methods that expect success call t.Fatal on protocol errors.
// For testing error cases, use pool.send(msg) directly and inspect the response.
//
// Socket communication:
//   - newConn(socketPath) opens a new connection (for subscribe, concurrent clients)
//   - pool.send(msg) → response: raw JSON round-trip on the default connection
//   - pool.sendOn(conn, msg) → response: raw JSON round-trip on a specific connection
//
// Timeouts:
//   - waitIdle(sessionId): calls wait with a 2-minute timeout, fails test if exceeded
//   - All operations have a default 30s timeout for non-wait commands

import "testing"

func TestHelpersSanity(t *testing.T) {
	t.Skip("helpers only — no runnable tests")
}
