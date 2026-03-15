package pool

import "testing"

func TestParseBufferInput(t *testing.T) {
	t.Run("simple prompt with text", func(t *testing.T) {
		buf := []byte("some output\n❯ hello world\n")
		got := parseBufferInput(buf)
		if got != "hello world" {
			t.Fatalf("expected %q, got %q", "hello world", got)
		}
	})

	t.Run("empty prompt", func(t *testing.T) {
		buf := []byte("some output\n❯ \n")
		got := parseBufferInput(buf)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("no prompt char", func(t *testing.T) {
		buf := []byte("just some output\nno prompt here\n")
		got := parseBufferInput(buf)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("multiple prompts takes last", func(t *testing.T) {
		buf := []byte("❯ old input\nresponse...\n❯ current input\n")
		got := parseBufferInput(buf)
		if got != "current input" {
			t.Fatalf("expected %q, got %q", "current input", got)
		}
	})

	t.Run("prompt with ANSI color codes", func(t *testing.T) {
		buf := []byte("output\n\x1b[38;5;12m❯\x1b[0m typed text\n")
		got := parseBufferInput(buf)
		if got != "typed text" {
			t.Fatalf("expected %q, got %q", "typed text", got)
		}
	})

	t.Run("prompt on last line without trailing newline", func(t *testing.T) {
		buf := []byte("output\n❯ partial")
		got := parseBufferInput(buf)
		if got != "partial" {
			t.Fatalf("expected %q, got %q", "partial", got)
		}
	})

	t.Run("empty buffer", func(t *testing.T) {
		got := parseBufferInput(nil)
		if got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("redrawn prompt takes latest", func(t *testing.T) {
		buf := []byte("❯ he❯ hello\n")
		got := parseBufferInput(buf)
		if got != "hello" {
			t.Fatalf("expected %q, got %q", "hello", got)
		}
	})

	// Prevents: false positive pendingInput from TUI status bar artifacts
	// when \r separates prompt and status bar on the same "line"
	t.Run("CR-separated status bar not detected as input", func(t *testing.T) {
		buf := []byte("❯ \r────────────────\r~/.cache/cpt/workdir\r  13%  Haiku 4.5\r")
		got := parseBufferInput(buf)
		if got != "" {
			t.Fatalf("expected empty (TUI artifact), got %q", got)
		}
	})

	// Prevents: missing input detection when prompt line ends with \r instead of \n
	t.Run("prompt with CR line ending", func(t *testing.T) {
		buf := []byte("output\r❯ typed text\r")
		got := parseBufferInput(buf)
		if got != "typed text" {
			t.Fatalf("expected %q, got %q", "typed text", got)
		}
	})
}
