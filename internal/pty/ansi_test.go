package pty

import (
	"bytes"
	"testing"
)

func TestSanitizeReplay(t *testing.T) {
	t.Run("empty returns empty", func(t *testing.T) {
		got := SanitizeReplay(nil)
		if len(got) != 0 {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("prepends SGR reset", func(t *testing.T) {
		got := SanitizeReplay([]byte("hello"))
		want := []byte("\x1b[0mhello")
		if !bytes.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("preserves complete escape sequences", func(t *testing.T) {
		input := []byte("\x1b[31mred\x1b[0m")
		got := SanitizeReplay(input)
		want := append([]byte("\x1b[0m"), input...)
		if !bytes.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("orphaned CSI bytes preserved as harmless literal text", func(t *testing.T) {
		// "31m" without ESC[ prefix — terminal prints it as literal text
		input := []byte("31mhello")
		got := SanitizeReplay(input)
		want := append([]byte("\x1b[0m"), input...)
		if !bytes.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
