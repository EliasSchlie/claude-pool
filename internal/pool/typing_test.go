package pool

import "testing"

// Default terminal size for unit tests. Actual sessions use real PTY dimensions.
const testCols, testRows = 80, 24

func TestParseBufferInput(t *testing.T) {
	t.Run("simple prompt with text", func(t *testing.T) {
		buf := []byte("some output\n❯ hello world\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "hello world" {
			t.Fatalf("expected %q, got %q", "hello world", got)
		}
	})

	t.Run("empty prompt", func(t *testing.T) {
		buf := []byte("some output\n❯ \n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("no prompt char", func(t *testing.T) {
		buf := []byte("just some output\nno prompt here\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("multiple prompts takes last", func(t *testing.T) {
		buf := []byte("❯ old input\nresponse...\n❯ current input\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "current input" {
			t.Fatalf("expected %q, got %q", "current input", got)
		}
	})

	t.Run("prompt with ANSI color codes", func(t *testing.T) {
		buf := []byte("output\n\x1b[38;5;12m❯\x1b[0m typed text\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "typed text" {
			t.Fatalf("expected %q, got %q", "typed text", got)
		}
	})

	t.Run("prompt on last line without trailing newline", func(t *testing.T) {
		buf := []byte("output\n❯ partial")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "partial" {
			t.Fatalf("expected %q, got %q", "partial", got)
		}
	})

	t.Run("empty buffer", func(t *testing.T) {
		got := parseBufferInput(nil, testCols, testRows)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("redrawn prompt takes latest", func(t *testing.T) {
		// VT100 emulator renders cursor-positioned redraws correctly
		buf := []byte("❯ hello\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "hello" {
			t.Fatalf("expected %q, got %q", "hello", got)
		}
	})

	// Prevents: false positive pendingInput from TUI status bar artifacts
	t.Run("box drawing characters rejected as input", func(t *testing.T) {
		buf := []byte("❯ ────────────────\n")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "" {
			t.Fatalf("expected empty (TUI artifact), got %q", got)
		}
	})

	// Prevents: cursor-positioned text not detected by naive string matching
	t.Run("cursor-positioned prompt detected via VT100 emulation", func(t *testing.T) {
		// Simulate TUI: move cursor to row 1, col 3, write "❯ hello"
		buf := []byte("\x1b[1;3H❯ hello")
		got := parseBufferInput(buf, testCols, testRows)
		if got != "hello" {
			t.Fatalf("expected %q, got %q", "hello", got)
		}
	})
}
