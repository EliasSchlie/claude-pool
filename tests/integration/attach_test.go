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
// Attach is an API-only feature (not exposed in CLI). The pool is created
// via CLI init, then pool.dial() opens a socket connection for API commands.
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
//  11.  "multiple clients read simultaneously"
//  12.  "buffer-based pendingInput detection without attach"
//  13.  "buffer-based pendingInput clears on Ctrl-U"
//  14.  "process death closes attach pipe"
//  15.  "attach response includes dimensions"
//  16.  "pty-resize changes dimensions"
//  17.  "pty-resize on non-live session errors"
//  18.  "attach to promptless session and submit"

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestAttach(t *testing.T) {
	p := setupPool(t, 2)
	sc := p.dial()

	var s1 string
	var attachConn net.Conn
	var attachSocketPath string
	t.Cleanup(func() {
		if attachConn != nil {
			attachConn.Close()
		}
	})

	t.Run("start and pin session", func(t *testing.T) {
		resp := p.runJSON("start", "--prompt", "respond with exactly: attach-init")
		s1 = strVal(resp, "sessionId")
		p.waitForStatus(s1, "idle", 60*time.Second)

		// Pin to prevent eviction during attach tests
		pinResp := sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
		assertNotError(t, pinResp)
	})

	t.Run("attach to idle session", func(t *testing.T) {
		resp := sc.send(Msg{"type": "attach", "sessionId": s1})
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
		p.waitForPendingInput(s1, func(v string) bool { return v != "" }, 10*time.Second)
	})

	t.Run("clearing input clears pendingInput", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("\x15")); err != nil {
			t.Fatalf("write Ctrl-U: %v", err)
		}
		p.waitForPendingInput(s1, func(v string) bool { return v == "" }, 10*time.Second)
	})

	t.Run("submit via attach triggers processing and completes", func(t *testing.T) {
		if _, err := attachConn.Write([]byte("respond with exactly: attach-test-output\r")); err != nil {
			t.Fatalf("write prompt: %v", err)
		}

		p.waitForStatus(s1, "processing", 15*time.Second)
		p.waitForStatus(s1, "idle", 30*time.Second)

		resp := sc.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "detail": "last"})
		assertNotError(t, resp)
		assertContains(t, strVal(resp, "content"), "attach-test-output")
	})

	t.Run("followup via API while attached", func(t *testing.T) {
		drainAttach(attachConn)

		resp := sc.send(Msg{
			"type": "followup", "sessionId": s1,
			"prompt": "respond with exactly: followup-while-attached",
		})
		assertNotError(t, resp)

		p.waitForStatus(s1, "processing", 15*time.Second)

		buf := make([]byte, 8192)
		attachConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := attachConn.Read(buf)
		if err != nil {
			t.Fatalf("attach pipe broke during API followup: %v", err)
		}
		if n == 0 {
			t.Fatal("expected terminal output from followup on attach pipe")
		}

		p.waitForStatus(s1, "idle", 30*time.Second)

		captureResp := sc.send(Msg{"type": "capture", "sessionId": s1, "source": "jsonl", "detail": "last"})
		assertNotError(t, captureResp)
		assertContains(t, strVal(captureResp, "content"), "followup-while-attached")
	})

	t.Run("eviction closes attach pipe", func(t *testing.T) {
		// Unpin s1 so it can be evicted
		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": false})

		// Start a new session to use the other fresh slot
		r := sc.send(Msg{"type": "start", "prompt": "respond with exactly: filler"})
		assertNotError(t, r)
		sNew := strVal(r, "sessionId")
		p.waitForStatus(sNew, "idle", 60*time.Second)

		// Now both slots are full (s1 + sNew). Start another → s1 (LRU) gets evicted.
		r2 := sc.send(Msg{"type": "start", "prompt": "respond with exactly: evict-trigger"})
		assertNotError(t, r2)

		p.waitForStatus(s1, "offloaded", 15*time.Second)

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
		resp := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertError(t, resp)
	})

	t.Run("restore and re-attach", func(t *testing.T) {
		resp := sc.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: restored"})
		assertNotError(t, resp)
		p.waitForStatus(s1, "idle", 60*time.Second)

		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})

		attachResp := sc.send(Msg{"type": "attach", "sessionId": s1})
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

	t.Run("multiple clients read simultaneously", func(t *testing.T) {
		// s1 is restored and pinned from previous step — get its attach socket
		attachResp := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, attachResp)
		sockPath := strVal(attachResp, "socketPath")

		conn1, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("connect client 1: %v", err)
		}
		defer conn1.Close()

		conn2, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("connect client 2: %v", err)
		}
		defer conn2.Close()

		drainAttach(conn1)
		drainAttach(conn2)

		// Send a followup — both clients should receive terminal output
		followupResp := sc.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: multi-client-test"})
		assertNotError(t, followupResp)
		p.waitForStatus(s1, "processing", 15*time.Second)

		// Read from both clients concurrently
		type readResult struct {
			n   int
			err error
		}
		var wg sync.WaitGroup
		r1, r2 := make(chan readResult, 1), make(chan readResult, 1)

		wg.Add(2)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8192)
			conn1.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := conn1.Read(buf)
			r1 <- readResult{n, err}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 8192)
			conn2.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := conn2.Read(buf)
			r2 <- readResult{n, err}
		}()

		res1 := <-r1
		res2 := <-r2
		wg.Wait()

		if res1.err != nil {
			t.Fatalf("client 1 read failed: %v", res1.err)
		}
		if res2.err != nil {
			t.Fatalf("client 2 read failed: %v", res2.err)
		}
		if res1.n == 0 || res2.n == 0 {
			t.Fatalf("both clients should receive output: client1=%d bytes, client2=%d bytes", res1.n, res2.n)
		}

		p.waitForStatus(s1, "idle", 30*time.Second)
	})

	t.Run("buffer-based pendingInput detection without attach", func(t *testing.T) {
		// s1 is idle and pinned. The multi-client connections from step 11 are
		// closed (defer). Wait briefly for the server to clean up the pipe.
		time.Sleep(500 * time.Millisecond)

		// Write chars via `input` (raw PTY write, not attach pipe). The buffer
		// poller must detect them and populate pendingInput.
		resp := sc.send(Msg{"type": "input", "sessionId": s1, "data": "buffer_typing_test"})
		assertNotError(t, resp)

		val := p.waitForPendingInput(s1, func(v string) bool { return v != "" }, 10*time.Second)
		assertContains(t, val, "buffer_typing_test")
	})

	t.Run("buffer-based pendingInput clears on Ctrl-U", func(t *testing.T) {
		resp := sc.send(Msg{"type": "input", "sessionId": s1, "data": "\x15"})
		assertNotError(t, resp)

		p.waitForPendingInput(s1, func(v string) bool { return v == "" }, 10*time.Second)
	})

	t.Run("process death closes attach pipe", func(t *testing.T) {
		// SPEC: "The pipe closes when the session is offloaded or dies."
		// Offload path tested in "eviction closes attach pipe". This tests death.
		attachResp := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, attachResp)
		sockPath := strVal(attachResp, "socketPath")

		deathConn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("connect to attach socket: %v", err)
		}
		defer deathConn.Close()
		drainAttach(deathConn)

		info := p.getSessionInfo(s1)
		killPID(t, int(info.PID))

		// Pipe should close with EOF
		buf := make([]byte, 4096)
		deathConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, err := deathConn.Read(buf)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("expected EOF after process death, got: %v", err)
				}
				break
			}
		}

		p.waitForStatus(s1, "offloaded", 15*time.Second)

		// Restore s1 for subsequent tests
		sc.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: death-restored"})
		p.waitForStatus(s1, "idle", 60*time.Second)
		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
	})

	t.Run("attach response includes dimensions", func(t *testing.T) {
		// s1 is still pinned and live from previous steps
		resp := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, resp)
		assertType(t, resp, "attached")

		cols := numVal(resp, "cols")
		rows := numVal(resp, "rows")
		if cols <= 0 {
			t.Fatalf("expected positive cols, got %v", cols)
		}
		if rows <= 0 {
			t.Fatalf("expected positive rows, got %v", rows)
		}
	})

	t.Run("pty-resize changes dimensions", func(t *testing.T) {
		// Resize to a known size
		resp := sc.send(Msg{"type": "pty-resize", "sessionId": s1, "cols": 120, "rows": 40})
		assertNotError(t, resp)

		// Verify via attach response
		attachResp := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, attachResp)

		assertNumVal(t, attachResp, "cols", 120)
		assertNumVal(t, attachResp, "rows", 40)

		// Resize to a different size to confirm it's not a cached value
		resp2 := sc.send(Msg{"type": "pty-resize", "sessionId": s1, "cols": 200, "rows": 50})
		assertNotError(t, resp2)

		attachResp2 := sc.send(Msg{"type": "attach", "sessionId": s1})
		assertNotError(t, attachResp2)

		assertNumVal(t, attachResp2, "cols", 200)
		assertNumVal(t, attachResp2, "rows", 50)
	})

	t.Run("pty-resize on non-live session errors", func(t *testing.T) {
		// Unpin s1 and archive it (archive offloads idle sessions first per spec)
		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": false})
		p.run("archive", "--session", s1)
		p.waitForStatus(s1, "archived", 15*time.Second)

		resp := sc.send(Msg{"type": "pty-resize", "sessionId": s1, "cols": 80, "rows": 24})
		assertError(t, resp)

		// Restore s1 for subsequent tests
		p.run("unarchive", "--session", s1)
		restoreResp := sc.send(Msg{"type": "followup", "sessionId": s1, "prompt": "respond with exactly: resize-restored"})
		assertNotError(t, restoreResp)
		p.waitForStatus(s1, "idle", 60*time.Second)
		sc.send(Msg{"type": "set", "sessionId": s1, "pinned": 300})
	})

	t.Run("attach to promptless session and submit", func(t *testing.T) {
		// Start a session without prompt — should be idle with a live slot
		startResp := sc.send(Msg{"type": "start"})
		assertNotError(t, startResp)
		sPromptless := strVal(startResp, "sessionId")

		p.waitForStatus(sPromptless, "idle", 60*time.Second)

		// Attach to the promptless session
		aResp := sc.send(Msg{"type": "attach", "sessionId": sPromptless})
		assertNotError(t, aResp)
		assertType(t, aResp, "attached")

		sockPath := strVal(aResp, "socketPath")
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("connect to promptless attach socket: %v", err)
		}
		defer conn.Close()
		drainAttach(conn)

		// Type into the TUI via attach pipe
		if _, err := conn.Write([]byte("respond with exactly: attached-prompt")); err != nil {
			t.Fatalf("write to promptless session: %v", err)
		}
		p.waitForPendingInput(sPromptless, func(v string) bool { return v != "" }, 10*time.Second)

		// Submit via Enter
		if _, err := conn.Write([]byte("\r")); err != nil {
			t.Fatalf("write Enter: %v", err)
		}
		p.waitForStatus(sPromptless, "processing", 15*time.Second)
		p.waitForStatus(sPromptless, "idle", 60*time.Second)

		// Verify the prompt was processed
		captureResp := sc.send(Msg{"type": "capture", "sessionId": sPromptless, "source": "jsonl", "detail": "last"})
		assertNotError(t, captureResp)
		assertContains(t, strVal(captureResp, "content"), "attached-prompt")

		p.run("archive", "--session", sPromptless)
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
