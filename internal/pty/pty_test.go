package pty

import "testing"

func TestRingBufferTail(t *testing.T) {
	t.Run("tail smaller than content", func(t *testing.T) {
		rb := NewRingBuffer(64)
		rb.Write([]byte("hello world"))
		got := string(rb.Tail(5))
		if got != "world" {
			t.Fatalf("expected %q, got %q", "world", got)
		}
	})

	t.Run("tail equals content", func(t *testing.T) {
		rb := NewRingBuffer(64)
		rb.Write([]byte("hello"))
		got := string(rb.Tail(5))
		if got != "hello" {
			t.Fatalf("expected %q, got %q", "hello", got)
		}
	})

	t.Run("tail larger than content", func(t *testing.T) {
		rb := NewRingBuffer(64)
		rb.Write([]byte("hi"))
		got := string(rb.Tail(100))
		if got != "hi" {
			t.Fatalf("expected %q, got %q", "hi", got)
		}
	})

	t.Run("tail after wrap", func(t *testing.T) {
		rb := NewRingBuffer(8)
		rb.Write([]byte("abcdefghij")) // wraps: buffer has "ijcdefgh", logical = "cdefghij"
		got := string(rb.Tail(4))
		if got != "ghij" {
			t.Fatalf("expected %q, got %q", "ghij", got)
		}
	})

	t.Run("tail spanning wrap boundary", func(t *testing.T) {
		rb := NewRingBuffer(8)
		rb.Write([]byte("abcdefghij")) // logical = "cdefghij"
		got := string(rb.Tail(8))
		if got != "cdefghij" {
			t.Fatalf("expected %q, got %q", "cdefghij", got)
		}
	})

	t.Run("empty buffer", func(t *testing.T) {
		rb := NewRingBuffer(64)
		got := rb.Tail(10)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}
