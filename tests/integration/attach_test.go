package integration

// TestAttach — Attach pipe workflow flow
//
// Pool size: 2
//
// This flow tests raw PTY attach: connecting to a session's terminal,
// typing detection, submitting prompts through the pipe, API followup
// while attached, and pipe lifecycle (offload closes pipe, re-attach
// after reload).
//
// Flow:
//
//   1.  "pin fresh session"
//   2.  "attach to idle session"
//   3.  "keystrokes detected as typing"
//   4.  "clearing input returns to idle"
//   5.  "submit via attach triggers processing and completes"
//   6.  "followup via API while attached"
//   7.  "offload closes attach pipe"
//   8.  "attach fails on offloaded session"
//   9.  "re-pin and re-attach after offload"

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
	t.Cleanup(func() {
		if attachConn != nil {
			attachConn.Close()
		}
	})

	t.Run("pin fresh session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "pin"})
		assertNotError(t, resp)
		assertType(t, resp, "ok")

		s1 = strVal(resp, "sessionId")
		assertNonEmpty(t, "sessionId", s1)

		pool.awaitStatus(s1, "idle", 30*time.Second)
	})

	t.Run("attach to idle session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "attached")

		socketPath := strVal(resp, "socketPath")
		assertNonEmpty(t, "socketPath", socketPath)

		var err error
		attachConn, err = net.Dial("unix", socketPath)
		if err != nil {
			t.Fatalf("connect to attach socket: %v", err)
		}
		drainAttach(attachConn)
	})

	t.Run("keystrokes detected as typing", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("hello")); err != nil {
			t.Fatalf("write to attach socket: %v", err)
		}

		pool.awaitStatus(s1, "typing", 10*time.Second)
	})

	t.Run("clearing input returns to idle", func(t *testing.T) {
		// Ctrl-U clears the input line
		if _, err := attachConn.Write([]byte("\x15")); err != nil {
			t.Fatalf("write Ctrl-U: %v", err)
		}

		pool.awaitStatus(s1, "idle", 10*time.Second)
	})

	t.Run("submit via attach triggers processing and completes", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("respond with exactly: attach-test-output\r")); err != nil {
			t.Fatalf("write prompt: %v", err)
		}

		pool.awaitStatus(s1, "processing", 15*time.Second)
		pool.awaitStatus(s1, "idle", 30*time.Second)

		resp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-last"})
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

		// Attach pipe should receive terminal output from the API-driven followup
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

		captureResp := pool.send(Msg{"type": "capture", "sessionId": s1, "format": "jsonl-last"})
		assertNotError(t, captureResp)
		assertContains(t, strVal(captureResp, "content"), "followup-while-attached")
	})

	t.Run("offload closes attach pipe", func(t *testing.T) {
		// Offload auto-unpins per protocol
		resp := pool.send(Msg{"type": "offload", "sessionId": s1})
		assertNotError(t, resp)

		buf := make([]byte, 1)
		attachConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := attachConn.Read(buf)
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("expected EOF or closed after offload, got: %v", err)
		}
	})

	t.Run("attach fails on offloaded session", func(t *testing.T) {
		resp := pool.send(Msg{"type": "attach", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("re-pin and re-attach after offload", func(t *testing.T) {
		resp := pool.send(Msg{"type": "pin", "sessionId": s1})
		assertNotError(t, resp)

		pool.awaitStatus(s1, "idle", 30*time.Second)

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

		// Verify pipe is alive by writing a character and reading the echo
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

		// Best-effort cleanup
		newConn.Write([]byte("\x15"))
	})
}

// drainAttach reads and discards any pending output from an attach pipe.
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
