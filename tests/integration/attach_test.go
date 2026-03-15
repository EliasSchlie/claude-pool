package integration

// TestAttach — Attach pipe workflow flow (API-only)
//
// Pool size: 2
//
// This flow tests raw PTY attach: connecting to a session's terminal,
// pendingInput detection, submitting prompts through the pipe, API followup
// while attached, and pipe lifecycle (eviction closes pipe, re-attach
// after restore).
//
// Attach is an API-only feature (not exposed in CLI), so this test uses
// the socket API directly.
//
// Flow:
//
//   1.  "start and pin session"
//   2.  "attach to idle session"
//   3.  "new client receives buffer replay"
//   4.  "keystrokes populate pendingInput"
//   5.  "clearing input clears pendingInput"
//   6.  "submit via attach triggers processing and completes"
//   7.  "followup via API while attached"
//   8.  "eviction closes attach pipe"
//   9.  "attach fails on offloaded session"
//  10.  "restore and re-attach"

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestAttach(t *testing.T) {
	pool := setupPool(t, 2)

	var s1 string
	var attachConn net.Conn
	var attachSocketPath string
	t.Cleanup(func() {
		if attachConn != nil {
			attachConn.Close()
		}
	})

	t.Run("start and pin session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "start", "prompt": "respond with exactly: attach-init"})
		assertNotError(t, resp)
		s1 = strVal(resp, "sessionId")
		pool.awaitStatus(s1, "idle", 60*time.Second)

		// Pin to prevent eviction during attach tests
		pinResp := pool.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
		assertNotError(t, pinResp)
	})

	t.Run("attach to idle session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "attached")

		attachSocketPath = strVal(resp, "socketPath")
		assertNonEmpty(t, "socketPath", attachSocketPath)

		var err error
		attachConn, err = net.Dial("unix", attachSocketPath)
		if err != nil {
			t.Fatalf("connect to attach socket: %v", err)
		}
		drainAttach(attachConn)
	})

	t.Run("new client receives buffer replay", func(t *testing.T) {
		time.Sleep(3 * time.Second)
		drainAttach(attachConn)

		secondConn, err := net.Dial("unix", attachSocketPath)
		if err != nil {
			t.Fatalf("connect second client: %v", err)
		}
		defer secondConn.Close()

		buf := make([]byte, 65536)
		secondConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := secondConn.Read(buf)
		if err != nil {
			t.Fatalf("expected buffer replay on connect, got error: %v", err)
		}
		if n == 0 {
			t.Fatal("expected non-empty buffer replay on connect")
		}
	})

	t.Run("keystrokes populate pendingInput", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("hello")); err != nil {
			t.Fatalf("write to attach socket: %v", err)
		}
		pool.awaitPendingInputSet(s1, 10*time.Second)
	})

	t.Run("clearing input clears pendingInput", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("\x15")); err != nil {
			t.Fatalf("write Ctrl-U: %v", err)
		}
		pool.awaitPendingInputClear(s1, 10*time.Second)
	})

	t.Run("submit via attach triggers processing and completes", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("respond with exactly: attach-test-output\r")); err != nil {
			t.Fatalf("write prompt: %v", err)
		}

		pool.awaitStatus(s1, "processing", 15*time.Second)
		pool.awaitStatus(s1, "idle", 30*time.Second)

		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "detail": "last"})
		assertNotError(t, resp)
		assertContains(t, strVal(resp, "content"), "attach-test-output")
	})

	t.Run("followup via API while attached", func(t *testing.T) {
		drainAttach(attachConn)

		resp := pool.send(Msg{
			"type": "followup", "sessionId": s1,
			"prompt": "respond with exactly: followup-while-attached",
		})
		assertNotError(t, resp)

		pool.awaitStatus(s1, "processing", 15*time.Second)

		buf := make([]byte, 8192)
		attachConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := attachConn.Read(buf)
		if err != nil {
			t.Fatalf("attach pipe broke during API followup: %v", err)
		}
		if n == 0 {
			t.Fatal("expected terminal output from followup on attach pipe")
		}

		pool.awaitStatus(s1, "idle", 30*time.Second)

		captureResp := pool.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "detail": "last"})
		assertNotError(t, captureResp)
		assertContains(t, strVal(captureResp, "content"), "followup-while-attached")
	})

	t.Run("eviction closes attach pipe", func(t *testing.T) {
		// Unpin s1 so it can be evicted
		pool.send(Msg{"type": "set", "sessionId": s1, "pinned": false})

		// Start a new session to use the other fresh slot
		r := pool.send(Msg{"type": "start", "prompt": "respond with exactly: filler"})
		assertNotError(t, r)
		sNew := strVal(r, "sessionId")
		pool.awaitStatus(sNew, "idle", 60*time.Second)

		// Now both slots are full (s1 + sNew). Start another → s1 (LRU) gets evicted.
		r2 := pool.send(Msg{"type": "start", "prompt": "respond with exactly: evict-trigger"})
		assertNotError(t, r2)

		pool.awaitStatus(s1, "offloaded", 15*time.Second)

		// Drain residual PTY output before expecting EOF
		buf := make([]byte, 4096)
		attachConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, err := attachConn.Read(buf)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("expected EOF or closed after eviction, got: %v", err)
				}
				break
			}
		}
	})

	t.Run("attach fails on offloaded session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "attach", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("restore and re-attach", func(t *testing.T) {
		// Restore s1 via followup (triggers load from offloaded state)
		resp := pool.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: restored"})
		assertNotError(t, resp)
		pool.awaitStatus(s1, "idle", 60*time.Second)

		// Pin to keep it loaded
		pool.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})

		attachResp := pool.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, attachResp)
		assertType(t, attachResp, "attached")

		newPath := strVal(attachResp, "socketPath")
		assertNonEmpty(t, "new socketPath", newPath)

		newConn, err := net.Dial("unix", newPath)
		if err != nil {
			t.Fatalf("connect to new attach socket: %v", err)
		}
		defer newConn.Close()

		if _, err := newConn.Write([]byte("x")); err != nil {
			t.Fatalf("write to re-attached socket: %v", err)
		}
		buf := make([]byte, 4096)
		newConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := newConn.Read(buf)
		if err != nil {
			t.Fatalf("read from re-attached socket: %v", err)
		}
		if n == 0 {
			t.Fatal("expected output from re-attached session")
		}

		newConn.Write([]byte("\x15"))
	})
}

func drainAttach(conn net.Conn) {
	buf := make([]byte, 8192)
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
	}
	conn.SetReadDeadline(time.Time{})
}
