package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasSchlie/claude-pool/internal/paths"
	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// --- Test helpers ---

// mustContain fails the test if s does not contain substr (case-insensitive).
func mustContain(t *testing.T, label, s, substr string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(s), strings.ToLower(substr)) {
		t.Fatalf("%s should contain %q, got:\n%s", label, substr, truncateStr(s, 500))
	}
}

// mustNotContain fails the test if s contains substr (case-insensitive).
func mustNotContain(t *testing.T, label, s, substr string) {
	t.Helper()
	if strings.Contains(strings.ToLower(s), strings.ToLower(substr)) {
		t.Fatalf("%s should NOT contain %q, got:\n%s", label, substr, truncateStr(s, 500))
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// buildTranscript creates a fake JSONL transcript from a sequence of messages.
// Each entry is a (type, text) pair — e.g., ("user", "hello"), ("assistant", "hi").
func buildTranscript(t *testing.T, messages ...string) string {
	t.Helper()
	if len(messages)%2 != 0 {
		t.Fatal("buildTranscript requires (type, text) pairs")
	}
	var lines []string
	for i := 0; i < len(messages); i += 2 {
		msgType := messages[i]
		text := messages[i+1]

		var entry map[string]any
		switch msgType {
		case "assistant":
			entry = map[string]any{
				"type":  "assistant",
				"model": "claude-sonnet-4-20250514",
				"usage": map[string]any{
					"input_tokens":  100,
					"output_tokens": 50,
				},
				"message": map[string]any{
					"id":    "msg_fake",
					"model": "claude-sonnet-4-20250514",
					"usage": map[string]any{
						"input_tokens":  100,
						"output_tokens": 50,
					},
					"stop_reason": "end_turn",
					"content": []any{
						map[string]any{"type": "text", "text": text},
					},
				},
			}
		case "user", "human":
			entry = map[string]any{
				"type": msgType,
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": text},
					},
				},
			}
		default:
			entry = map[string]any{
				"type":    msgType,
				"content": text,
			}
		}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("buildTranscript: marshal error: %v", err)
		}
		lines = append(lines, string(data))
	}
	return strings.Join(lines, "\n")
}

// setupFakeTranscript writes a transcript to a temp directory and returns a
// Manager configured to find it via transcriptDirs. Fully isolated — no
// writes to ~/.claude/.
func setupFakeTranscript(t *testing.T, uuid, transcript string) (*Manager, *Session) {
	t.Helper()

	// Create <tmpdir>/project-key/<uuid>.jsonl — findTranscript globs */<uuid>.jsonl
	projectDir := filepath.Join(t.TempDir(), "test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("setupFakeTranscript: MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, uuid+".jsonl"), []byte(transcript), 0644); err != nil {
		t.Fatalf("setupFakeTranscript: WriteFile: %v", err)
	}

	m := &Manager{
		paths:          paths.New(t.TempDir()),
		sessions:       make(map[string]*Session),
		procs:          make(map[string]*ptyPkg.Process),
		pidToSID:       make(map[int]string),
		pipes:          make(map[string]*attachPipe),
		done:           make(chan struct{}),
		transcriptDirs: []string{filepath.Dir(projectDir)},
	}
	s := &Session{ID: "test-session", ClaudeUUID: uuid}
	return m, s
}

// --- Bug #1: jsonl-short must filter since last user message ---

func TestJsonlShortFiltersSinceLastUser(t *testing.T) {
	// SPEC: jsonl-short = "All assistant messages since last user message."
	//
	// Bug: captureContent dispatches jsonl-short as readJSONL(s, false, true)
	// — sinceLastUser=false. Should be readJSONL(s, true, true).

	transcript := buildTranscript(t,
		"user", "what is 2+2",
		"assistant", "4",
		"user", "what is 3+3",
		"assistant", "6",
	)

	m, s := setupFakeTranscript(t, "test-short-uuid", transcript)

	t.Run("readJSONL with sinceLastUser=true excludes earlier turns", func(t *testing.T) {
		result := m.readJSONL(s, true, true) // sinceLastUser=true, shortOnly=true
		mustContain(t, "result", result, "6")
		mustNotContain(t, "result", result, "4")
	})

	t.Run("captureContent jsonl-short excludes earlier turns", func(t *testing.T) {
		result := m.captureContent(s, "jsonl-short")
		mustContain(t, "jsonl-short", result, "6")
		mustNotContain(t, "jsonl-short", result, "4")
	})
}

// --- Bug #3: jsonl-long must strip repetitive fields ---

func TestJsonlLongStripsFields(t *testing.T) {
	// SPEC: jsonl-long = "Full JSONL since last user message, repetitive
	// fields stripped."
	//
	// Bug: readJSONL with shortOnly=false returns raw JSONL unchanged —
	// no field stripping.

	transcript := buildTranscript(t,
		"user", "hello",
		"assistant", "world",
	)

	m, s := setupFakeTranscript(t, "test-long-uuid", transcript)

	t.Run("jsonl-long should strip model and usage fields", func(t *testing.T) {
		longResult := m.captureContent(s, "jsonl-long")
		fullResult := m.captureContent(s, "jsonl-full")

		if strings.Contains(longResult, `"stop_reason"`) {
			t.Error("jsonl-long should strip 'stop_reason' from messages")
		}

		if len(longResult) >= len(fullResult) {
			t.Errorf("jsonl-long (%d bytes) should be smaller than jsonl-full (%d bytes) after stripping repetitive fields",
				len(longResult), len(fullResult))
		}
	})

	t.Run("jsonl-long preserves content", func(t *testing.T) {
		longResult := m.captureContent(s, "jsonl-long")
		mustContain(t, "jsonl-long", longResult, "world")
	})
}

// --- Verify jsonl-full is unfiltered ---

func TestJsonlFullReturnsEverything(t *testing.T) {
	transcript := buildTranscript(t,
		"user", "first",
		"assistant", "one",
		"user", "second",
		"assistant", "two",
	)

	m, s := setupFakeTranscript(t, "test-full-uuid", transcript)
	result := m.captureContent(s, "jsonl-full")

	mustContain(t, "jsonl-full", result, "one")
	mustContain(t, "jsonl-full", result, "two")
	mustContain(t, "jsonl-full", result, "first")
}

// --- jsonl-last returns only the last assistant message ---

func TestJsonlLastReturnsOnlyLast(t *testing.T) {
	transcript := buildTranscript(t,
		"user", "q1",
		"assistant", "answer one",
		"user", "q2",
		"assistant", "answer two",
	)

	m, s := setupFakeTranscript(t, "test-last-uuid", transcript)
	result := m.captureContent(s, "jsonl-last")

	mustContain(t, "jsonl-last", result, "answer two")
	mustNotContain(t, "jsonl-last", result, "answer one")
}

// --- extractTextContent ---

func TestExtractTextContent(t *testing.T) {
	t.Run("current format with message.content array", func(t *testing.T) {
		msg := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "hello"},
					map[string]any{"type": "text", "text": " world"},
				},
			},
		}
		if got := extractTextContent(msg); got != "hello world" {
			t.Fatalf("expected 'hello world', got %q", got)
		}
	})

	t.Run("legacy format with top-level content string", func(t *testing.T) {
		msg := map[string]any{"type": "assistant", "content": "legacy text"}
		if got := extractTextContent(msg); got != "legacy text" {
			t.Fatalf("expected 'legacy text', got %q", got)
		}
	})

	t.Run("skips non-text content blocks", func(t *testing.T) {
		msg := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "tool_use", "name": "bash"},
					map[string]any{"type": "text", "text": "result"},
				},
			},
		}
		if got := extractTextContent(msg); got != "result" {
			t.Fatalf("expected 'result', got %q", got)
		}
	})

	t.Run("empty content returns empty string", func(t *testing.T) {
		msg := map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{}},
		}
		if got := extractTextContent(msg); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})
}

func TestExtractLastSection(t *testing.T) {
	t.Run("short buffer returned as-is", func(t *testing.T) {
		buf := "line1\nline2\nline3"
		if got := extractLastSection(buf); got != buf {
			t.Fatalf("expected full buffer, got %q", got)
		}
	})

	t.Run("long buffer truncated to last 50 lines", func(t *testing.T) {
		var lines []string
		for i := 0; i < 100; i++ {
			lines = append(lines, "line")
		}
		buf := strings.Join(lines, "\n")
		gotLines := strings.Split(extractLastSection(buf), "\n")
		if len(gotLines) != 50 {
			t.Fatalf("expected 50 lines, got %d", len(gotLines))
		}
	})
}
