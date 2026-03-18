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

func mustContain(t *testing.T, label, s, substr string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(s), strings.ToLower(substr)) {
		t.Fatalf("%s should contain %q, got:\n%s", label, substr, truncateStr(s, 500))
	}
}

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

func assertTypeCount(t *testing.T, entries []map[string]any, msgType string, expected int) {
	t.Helper()
	got := countByType(entries, msgType)
	if got != expected {
		t.Fatalf("expected %d %q entries, got %d", expected, msgType, got)
	}
}

// baseEntry returns common metadata fields shared by all realistic JSONL entries.
func baseEntry() map[string]any {
	return map[string]any{
		"parentUuid":  nil,
		"isSidechain": false,
		"cwd":         "/tmp/test",
		"sessionId":   "fake-session-uuid",
		"version":     "2.1.72",
	}
}

// buildEntry creates a single realistic JSONL entry matching Claude Code's actual format.
func buildEntry(t *testing.T, entryType string, content any) string {
	t.Helper()
	entry := baseEntry()

	switch entryType {
	case "user":
		text, _ := content.(string)
		entry["userType"] = "external"
		entry["type"] = "user"
		entry["message"] = map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		}
		entry["uuid"] = "user-uuid-fake"
		entry["timestamp"] = "2026-03-14T10:00:00Z"

	case "assistant":
		text, _ := content.(string)
		entry["parentUuid"] = "user-uuid-fake"
		entry["type"] = "assistant"
		entry["message"] = map[string]any{
			"model": "claude-haiku-4-5-20251001",
			"id":    "msg_fake",
			"type":  "message",
			"role":  "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                100,
				"output_tokens":               50,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		}
		entry["requestId"] = "req_fake"
		entry["uuid"] = "assistant-uuid-fake"
		entry["timestamp"] = "2026-03-14T10:00:01Z"

	case "assistant_tool_use":
		toolName, _ := content.(string)
		entry["parentUuid"] = "user-uuid-fake"
		entry["type"] = "assistant"
		entry["message"] = map[string]any{
			"model": "claude-haiku-4-5-20251001",
			"id":    "msg_fake_tool",
			"type":  "message",
			"role":  "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_fake",
					"name":  toolName,
					"input": map[string]any{"command": "echo test"},
				},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens":  100,
				"output_tokens": 30,
			},
		}
		entry["requestId"] = "req_fake_tool"
		entry["uuid"] = "assistant-tool-uuid-fake"
		entry["timestamp"] = "2026-03-14T10:00:02Z"

	case "tool_result":
		// Tool result: top-level type=user, message.content contains tool_result block
		text, _ := content.(string)
		entry["parentUuid"] = "assistant-tool-uuid-fake"
		entry["userType"] = "external"
		entry["type"] = "user"
		entry["message"] = map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_fake",
					"content":     text,
				},
			},
		}
		entry["uuid"] = "tool-result-uuid-fake"
		entry["timestamp"] = "2026-03-14T10:00:03Z"

	case "progress":
		entry["parentUuid"] = "assistant-uuid-fake"
		entry["type"] = "progress"
		entry["data"] = map[string]any{
			"type":      "hook_progress",
			"hookEvent": "Stop",
		}
		entry["timestamp"] = "2026-03-14T10:00:04Z"

	case "system":
		entry["type"] = "system"
		entry["timestamp"] = "2026-03-14T10:00:00Z"

	default:
		t.Fatalf("buildEntry: unknown type %q", entryType)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("buildEntry: marshal error: %v", err)
	}
	return string(data)
}

// buildTranscript joins multiple JSONL entries into a transcript string.
func buildTranscript(t *testing.T, entries ...string) string {
	t.Helper()
	return strings.Join(entries, "\n")
}

// setupFakeTranscript writes a transcript to a temp directory and returns a
// Manager configured to find it via transcriptDirs.
func setupFakeTranscript(t *testing.T, uuid, transcript string) (*Manager, *Session) {
	t.Helper()

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

// parseCaptureLines splits JSONL content and parses each line.
func parseCaptureLines(t *testing.T, content string) []map[string]any {
	t.Helper()
	if content == "" {
		return nil
	}
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("parseCaptureLines: invalid JSON line: %v\nline: %s", err, line)
		}
		result = append(result, msg)
	}
	return result
}

func countByType(entries []map[string]any, msgType string) int {
	n := 0
	for _, e := range entries {
		if typ, _ := e["type"].(string); typ == msgType {
			n++
		}
	}
	return n
}

// hasContentBlockType checks if an entry's message.content contains a block of the given type.
func hasContentBlockType(entry map[string]any, blockType string) bool {
	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return false
	}
	content, _ := msg["content"].([]any)
	for _, c := range content {
		if block, ok := c.(map[string]any); ok {
			if typ, _ := block["type"].(string); typ == blockType {
				return true
			}
		}
	}
	return false
}

// ============================================================
// New capture API tests: captureOutput(s, source, turns, detail)
//
// These tests will fail until captureOutput is implemented.
// ============================================================

// --- Simple two-turn transcript (no tool calls) ---

func twoTurnTranscript(t *testing.T) string {
	return buildTranscript(t,
		buildEntry(t, "user", "what is 2+2"),
		buildEntry(t, "assistant", "4"),
		buildEntry(t, "user", "what is 3+3"),
		buildEntry(t, "assistant", "6"),
	)
}

// --- Tool-use turn transcript ---
// Turn 1: user asks, assistant calls bash, gets result, responds with final text
// Turn 2: simple follow-up

func toolUseTranscript(t *testing.T) string {
	return buildTranscript(t,
		buildEntry(t, "user", "list the files"),
		buildEntry(t, "assistant_tool_use", "Bash"),
		buildEntry(t, "tool_result", "file1.txt\nfile2.txt"),
		buildEntry(t, "assistant", "I found two files: file1.txt and file2.txt"),
		buildEntry(t, "user", "thanks"),
		buildEntry(t, "assistant", "you are welcome"),
	)
}

// --- Transcript with progress/system entries ---

func noisyTranscript(t *testing.T) string {
	return buildTranscript(t,
		buildEntry(t, "system", ""),
		buildEntry(t, "user", "hello"),
		buildEntry(t, "progress", ""),
		buildEntry(t, "assistant", "hi there"),
	)
}

// === turns parameter ===

func TestCaptureTurns1(t *testing.T) {
	m, s := setupFakeTranscript(t, "turns-1", twoTurnTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "last")
	entries := parseCaptureLines(t, result)

	// Last turn only: "what is 3+3" → "6"
	assertTypeCount(t, entries, "user", 1)
	assertTypeCount(t, entries, "assistant", 1)
	mustContain(t, "result", result, "3+3")
	mustContain(t, "result", result, "6")
	mustNotContain(t, "result", result, "2+2")
}

func TestCaptureTurns2(t *testing.T) {
	transcript := buildTranscript(t,
		buildEntry(t, "user", "turn one"),
		buildEntry(t, "assistant", "response one"),
		buildEntry(t, "user", "turn two"),
		buildEntry(t, "assistant", "response two"),
		buildEntry(t, "user", "turn three"),
		buildEntry(t, "assistant", "response three"),
	)

	m, s := setupFakeTranscript(t, "turns-2", transcript)
	result := m.captureOutput(s, "jsonl", 2, "last")

	mustContain(t, "result", result, "turn two")
	mustContain(t, "result", result, "response two")
	mustContain(t, "result", result, "turn three")
	mustContain(t, "result", result, "response three")
	mustNotContain(t, "result", result, "turn one")
	mustNotContain(t, "result", result, "response one")
}

func TestCaptureTurns0(t *testing.T) {
	m, s := setupFakeTranscript(t, "turns-0", twoTurnTranscript(t))
	result := m.captureOutput(s, "jsonl", 0, "last")

	mustContain(t, "result", result, "2+2")
	mustContain(t, "result", result, "3+3")
	assertTypeCount(t, parseCaptureLines(t, result), "user", 2)
}

func TestCaptureTurnsExceedsAvailable(t *testing.T) {
	// turns: 10 but only 2 turns — returns everything available
	m, s := setupFakeTranscript(t, "turns-exceed", twoTurnTranscript(t))
	result := m.captureOutput(s, "jsonl", 10, "last")
	assertTypeCount(t, parseCaptureLines(t, result), "user", 2)
}

// === detail: "last" ===

func TestCaptureDetailLast(t *testing.T) {
	// Last turn of toolUseTranscript is "thanks" → "you are welcome" (no tool calls)
	m, s := setupFakeTranscript(t, "detail-last", toolUseTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "last")
	entries := parseCaptureLines(t, result)

	mustContain(t, "result", result, "thanks")
	mustContain(t, "result", result, "you are welcome")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (user + assistant), got %d", len(entries))
	}
}

func TestCaptureDetailLastToolTurn(t *testing.T) {
	// Both turns: turn 1 has tool calls, detail "last" returns final assistant per turn
	m, s := setupFakeTranscript(t, "detail-last-tool", toolUseTranscript(t))
	result := m.captureOutput(s, "jsonl", 2, "last")

	// Turn 1: user + final assistant ("I found two files..."), NOT the tool_use assistant
	mustContain(t, "result", result, "list the files")
	mustContain(t, "result", result, "I found two files")

	entries := parseCaptureLines(t, result)
	// 2 turns × (user + last assistant) = 4 entries
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (2 turns × user+assistant), got %d", len(entries))
	}
	for _, e := range entries {
		if hasContentBlockType(e, "tool_use") {
			t.Fatal("detail 'last' should not include tool_use content blocks")
		}
		if hasContentBlockType(e, "tool_result") {
			t.Fatal("detail 'last' should not include tool_result user entries")
		}
	}
}

// === detail: "assistant" ===

// multiAssistantTranscript has a turn where the assistant gives text, calls a tool,
// then gives more text — testing that "assistant" returns all text blocks while
// "last" returns only the final one.
func multiAssistantTranscript(t *testing.T) string {
	return buildTranscript(t,
		buildEntry(t, "user", "check the logs"),
		buildEntry(t, "assistant", "let me look at the logs"),
		buildEntry(t, "assistant_tool_use", "Bash"),
		buildEntry(t, "tool_result", "ERROR: connection refused"),
		buildEntry(t, "assistant", "the logs show a connection error"),
	)
}

func TestCaptureDetailAssistantReturnsAllTextBlocks(t *testing.T) {
	// Key distinction from "last": "assistant" returns ALL assistant text blocks,
	// not just the final one. In a tool-use turn, there are typically multiple.
	m, s := setupFakeTranscript(t, "detail-asst-multi", multiAssistantTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "assistant")
	entries := parseCaptureLines(t, result)

	// Both assistant text messages should be present
	mustContain(t, "result", result, "let me look at the logs")
	mustContain(t, "result", result, "the logs show a connection error")

	// But tool_use and tool_result should be excluded
	mustNotContain(t, "result", result, "connection refused")
	for _, e := range entries {
		if hasContentBlockType(e, "tool_use") {
			t.Fatal("detail 'assistant' should not include tool_use blocks")
		}
		if hasContentBlockType(e, "tool_result") {
			t.Fatal("detail 'assistant' should not include tool_result entries")
		}
	}

	// User prompt present
	mustContain(t, "result", result, "check the logs")
}

func TestCaptureDetailLastVsAssistant(t *testing.T) {
	// Direct comparison: "last" returns 1 assistant, "assistant" returns 2
	m, s := setupFakeTranscript(t, "last-vs-asst", multiAssistantTranscript(t))

	lastResult := m.captureOutput(s, "jsonl", 1, "last")
	asstResult := m.captureOutput(s, "jsonl", 1, "assistant")

	lastEntries := parseCaptureLines(t, lastResult)
	asstEntries := parseCaptureLines(t, asstResult)

	// "last" should have fewer assistant entries than "assistant"
	lastAssistantCount := countByType(lastEntries, "assistant")
	asstAssistantCount := countByType(asstEntries, "assistant")

	if lastAssistantCount != 1 {
		t.Fatalf("detail 'last' should return 1 assistant entry, got %d", lastAssistantCount)
	}
	if asstAssistantCount != 2 {
		t.Fatalf("detail 'assistant' should return 2 assistant entries, got %d", asstAssistantCount)
	}

	// "last" should only have the final text
	mustNotContain(t, "last result", lastResult, "let me look")
	mustContain(t, "last result", lastResult, "connection error")

	// "assistant" should have both
	mustContain(t, "assistant result", asstResult, "let me look")
	mustContain(t, "assistant result", asstResult, "connection error")
}

func TestCaptureDetailAssistantExcludesToolUseOnly(t *testing.T) {
	// toolUseTranscript turn 1 has a tool_use-only assistant entry (no text).
	// "assistant" should exclude it entirely.
	m, s := setupFakeTranscript(t, "detail-asst-toolonly", toolUseTranscript(t))
	result := m.captureOutput(s, "jsonl", 2, "assistant")
	entries := parseCaptureLines(t, result)

	mustContain(t, "result", result, "I found two files")
	mustContain(t, "result", result, "list the files")

	for _, e := range entries {
		if hasContentBlockType(e, "tool_use") {
			t.Fatal("detail 'assistant' should not include tool_use-only assistant entries")
		}
		if hasContentBlockType(e, "tool_result") {
			t.Fatal("detail 'assistant' should not include tool_result user entries")
		}
	}
}

// === detail: "tools" ===

func TestCaptureDetailTools(t *testing.T) {
	// Both turns: all user + assistant entries, metadata stripped
	m, s := setupFakeTranscript(t, "detail-tools", toolUseTranscript(t))
	result := m.captureOutput(s, "jsonl", 2, "tools")
	entries := parseCaptureLines(t, result)

	// All user entries (prompts + tool results) and all assistant entries
	mustContain(t, "result", result, "list the files")
	mustContain(t, "result", result, "Bash")
	mustContain(t, "result", result, "file1.txt")
	mustContain(t, "result", result, "I found two files")

	// Metadata stripped
	mustNotContain(t, "result", result, "stop_reason")
	mustNotContain(t, "result", result, "input_tokens")
	mustNotContain(t, "result", result, "requestId")
	mustNotContain(t, "result", result, "parentUuid")

	assertTypeCount(t, entries, "progress", 0)
}

// === detail: "raw" ===

func TestCaptureDetailRaw(t *testing.T) {
	m, s := setupFakeTranscript(t, "detail-raw", noisyTranscript(t))
	result := m.captureOutput(s, "jsonl", 0, "raw")
	entries := parseCaptureLines(t, result)

	// Everything unfiltered
	assertTypeCount(t, entries, "progress", 1)
	assertTypeCount(t, entries, "system", 1)
	mustContain(t, "result", result, "stop_reason")
	mustContain(t, "result", result, "input_tokens")
}

func TestCaptureDetailRawPreservesMetadata(t *testing.T) {
	m, s := setupFakeTranscript(t, "detail-raw-meta", toolUseTranscript(t))
	result := m.captureOutput(s, "jsonl", 0, "raw")

	mustContain(t, "result", result, "parentUuid")
	mustContain(t, "result", result, "requestId")
	mustContain(t, "result", result, "isSidechain")
	mustContain(t, "result", result, "stop_reason")
}

// === Output is always JSONL ===

func TestCaptureOutputIsJSONL(t *testing.T) {
	m, s := setupFakeTranscript(t, "always-jsonl", twoTurnTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "last")
	entries := parseCaptureLines(t, result)

	if len(entries) != 2 {
		t.Fatalf("expected 2 JSONL entries, got %d", len(entries))
	}
	if typ, _ := entries[0]["type"].(string); typ != "user" {
		t.Fatalf("first entry should be type=user, got %q", typ)
	}
	if typ, _ := entries[1]["type"].(string); typ != "assistant" {
		t.Fatalf("second entry should be type=assistant, got %q", typ)
	}
}

// === Metadata stripping for last/assistant ===

func TestCaptureDetailLastStripsMetadata(t *testing.T) {
	m, s := setupFakeTranscript(t, "last-strips-meta", twoTurnTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "last")

	mustNotContain(t, "result", result, "stop_reason")
	mustNotContain(t, "result", result, "input_tokens")
	mustNotContain(t, "result", result, "requestId")
	mustNotContain(t, "result", result, "parentUuid")
	mustNotContain(t, "result", result, "isSidechain")
	mustNotContain(t, "result", result, "sessionId")

	// Content should still be present
	mustContain(t, "result", result, "3+3")
	mustContain(t, "result", result, "6")
}

func TestCaptureDetailAssistantStripsMetadata(t *testing.T) {
	m, s := setupFakeTranscript(t, "asst-strips-meta", multiAssistantTranscript(t))
	result := m.captureOutput(s, "jsonl", 1, "assistant")

	mustNotContain(t, "result", result, "stop_reason")
	mustNotContain(t, "result", result, "input_tokens")
	mustNotContain(t, "result", result, "parentUuid")
	mustNotContain(t, "result", result, "sessionId")

	// Content should still be present
	mustContain(t, "result", result, "let me look at the logs")
	mustContain(t, "result", result, "connection error")
}

// === Edge cases ===

func TestCaptureEmptyTranscript(t *testing.T) {
	m, s := setupFakeTranscript(t, "empty", "")
	result := m.captureOutput(s, "jsonl", 1, "last")
	if result != "" {
		t.Fatalf("expected empty for empty transcript, got: %s", result)
	}
}

func TestCaptureNoAssistantYet(t *testing.T) {
	transcript := buildTranscript(t,
		buildEntry(t, "user", "still waiting"),
	)
	m, s := setupFakeTranscript(t, "no-asst", transcript)
	result := m.captureOutput(s, "jsonl", 1, "last")
	entries := parseCaptureLines(t, result)

	assertTypeCount(t, entries, "user", 1)
}

func TestCaptureNoUUID(t *testing.T) {
	m, s := setupFakeTranscript(t, "no-uuid", twoTurnTranscript(t))
	s.ClaudeUUID = ""
	result := m.captureOutput(s, "jsonl", 1, "last")
	if result != "" {
		t.Fatalf("expected empty for session without UUID, got: %s", result)
	}
}

// === Helper function tests ===

func TestExtractTextContent(t *testing.T) {
	t.Run("message.content array", func(t *testing.T) {
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

	t.Run("legacy content string", func(t *testing.T) {
		msg := map[string]any{"type": "assistant", "content": "legacy text"}
		if got := extractTextContent(msg); got != "legacy text" {
			t.Fatalf("expected 'legacy text', got %q", got)
		}
	})

	t.Run("skips tool_use blocks", func(t *testing.T) {
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

	t.Run("empty content", func(t *testing.T) {
		msg := map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{}},
		}
		if got := extractTextContent(msg); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}

func TestExtractLastSection(t *testing.T) {
	t.Run("short buffer", func(t *testing.T) {
		buf := "line1\nline2\nline3"
		if got := extractLastSection(buf); got != buf {
			t.Fatalf("expected full buffer, got %q", got)
		}
	})

	t.Run("long buffer truncated to 50 lines", func(t *testing.T) {
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
