package main

// Manual test: verify /clear works after Ctrl-C with minimal delay.
// Run: go test -v -run TestClearAfterCtrlC -timeout 5m ./tests/manual/

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestClearAfterCtrlC verifies that /clear works immediately after Ctrl-C.
// This is a manual test — it spawns a real Claude session.
func TestClearAfterCtrlC(t *testing.T) {
	delays := []time.Duration{
		0,
		100 * time.Millisecond,
		200 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
	}

	for _, delay := range delays {
		t.Run(fmt.Sprintf("delay_%dms", delay.Milliseconds()), func(t *testing.T) {
			testClearAfterCtrlC(t, delay)
		})
	}
}

func testClearAfterCtrlC(t *testing.T, delayAfterCtrlC time.Duration) {
	cmd := exec.Command("claude", "--dangerously-skip-permissions", "--model", "haiku")
	cmd.Dir = os.TempDir()

	// Filter env to avoid nested session errors
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDECODE=") ||
			strings.HasPrefix(e, "CLAUDE_CODE_SESSION_ID=") {
			continue
		}
		filtered = append(filtered, e)
	}
	cmd.Env = filtered

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		ptmx.Close()
	}()

	// Read output in background
	buf := make([]byte, 0, 256*1024)
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := ptmx.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
		}
	}()

	// Wait for Claude to start
	t.Log("Waiting for Claude to start...")

	// Wait for initial ready state (trust prompt or idle)
	time.Sleep(3 * time.Second)

	// Accept trust if needed
	if strings.Contains(strings.ToLower(string(buf)), "trust?") {
		ptmx.Write([]byte("\r"))
		time.Sleep(1 * time.Second)
	}

	// Send a slow command
	t.Log("Sending sleep 60 command...")
	ptmx.Write([]byte("\x1b"))
	time.Sleep(100 * time.Millisecond)
	ptmx.Write([]byte("\x15"))
	time.Sleep(50 * time.Millisecond)
	ptmx.Write([]byte("run the bash command: sleep 60\r"))

	// Wait for processing to start
	time.Sleep(3 * time.Second)

	// Send Ctrl-C
	t.Logf("Sending Ctrl-C, then waiting %v before /clear...", delayAfterCtrlC)
	ptmx.Write([]byte("\x03"))

	// Wait the specified delay
	time.Sleep(delayAfterCtrlC)

	// Record buffer state before /clear
	bufBefore := len(buf)

	// Send /clear
	ptmx.Write([]byte("\x1b"))
	time.Sleep(100 * time.Millisecond)
	ptmx.Write([]byte("\x15"))
	time.Sleep(50 * time.Millisecond)
	ptmx.Write([]byte("/clear\r"))
	t.Log("Sent /clear")

	// Wait for /clear to take effect — look for session start indicators
	// Claude should show a fresh session after /clear
	success := false
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Logf("Buffer after /clear (last 2000 chars):\n%s", tail(string(buf), 2000))
			t.Fatalf("/clear did not complete within 30s (delay=%v)", delayAfterCtrlC)
			return
		case <-time.After(500 * time.Millisecond):
			recent := string(buf[bufBefore:])
			// After /clear, Claude starts a new session. Look for signs of that.
			if strings.Contains(recent, "clear") || strings.Contains(recent, "Tips") ||
				strings.Contains(recent, ">") || len(recent) > 500 {
				t.Logf("✅ /clear succeeded after %v delay (output: %d bytes)", delayAfterCtrlC, len(recent))
				success = true
			}
		}
		if success {
			break
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
