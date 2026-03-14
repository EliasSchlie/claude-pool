package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// captureOutput returns session output filtered by source/turns/detail params.
// TODO: implement — currently panics to fail tests explicitly.
func (m *Manager) captureOutput(s *Session, source string, turns int, detail string) string {
	panic("captureOutput not implemented — see capture API redesign")
}

func (m *Manager) captureContent(s *Session, format string) string {
	switch format {
	case "buffer-full":
		if proc := m.procs[s.ID]; proc != nil {
			return stripANSI(string(proc.Buffer()))
		}
		return ""
	case "buffer-last":
		if proc := m.procs[s.ID]; proc != nil {
			return extractLastSection(stripANSI(string(proc.Buffer())))
		}
		return ""
	case "jsonl-full":
		return m.readJSONL(s, false, false)
	case "jsonl-long":
		return m.readJSONL(s, true, false)
	case "jsonl-last":
		return m.readJSONLLast(s)
	case "jsonl-short":
		return m.readJSONL(s, false, true)
	default:
		return m.readJSONL(s, false, true)
	}
}

func (m *Manager) readJSONL(s *Session, sinceLastUser bool, shortOnly bool) string {
	if s.ClaudeUUID == "" {
		return ""
	}
	transcriptPath := m.findTranscript(s.ClaudeUUID)
	if transcriptPath == "" {
		return ""
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return ""
	}

	// Full raw JSONL — no filtering
	if !sinceLastUser && !shortOnly {
		return string(data)
	}

	// Determine start index
	startIdx := 0
	if sinceLastUser {
		for i, line := range lines {
			var msg map[string]any
			if json.Unmarshal([]byte(line), &msg) == nil {
				if msgType, _ := msg["type"].(string); msgType == "user" || msgType == "human" {
					startIdx = i
				}
			}
		}
	}

	var parts []string
	for _, line := range lines[startIdx:] {
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		msgType, _ := msg["type"].(string)

		if shortOnly {
			if msgType == "assistant" {
				if content := extractTextContent(msg); content != "" {
					parts = append(parts, content)
				}
			}
		} else {
			parts = append(parts, line)
		}
	}

	return strings.Join(parts, "\n")
}

func (m *Manager) readJSONLLast(s *Session) string {
	if s.ClaudeUUID == "" {
		return ""
	}
	transcriptPath := m.findTranscript(s.ClaudeUUID)
	if transcriptPath == "" {
		return ""
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var msg map[string]any
		if json.Unmarshal([]byte(lines[i]), &msg) != nil {
			continue
		}
		if msgType, _ := msg["type"].(string); msgType == "assistant" {
			if content := extractTextContent(msg); content != "" {
				return content
			}
		}
	}
	return ""
}

func (m *Manager) findTranscript(claudeUUID string) string {
	dirs := m.transcriptDirs
	if len(dirs) == 0 {
		home, _ := os.UserHomeDir()
		dirs = []string{filepath.Join(home, ".claude", "projects")}
	}
	// Claude stores transcripts at <dir>/<project-key>/<UUID>.jsonl
	for _, dir := range dirs {
		pattern := filepath.Join(dir, "*", claudeUUID+".jsonl")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// extractTextContent extracts text from a Claude JSONL assistant message.
// Format: { "type": "assistant", "message": { "content": [{ "type": "text", "text": "..." }] } }
func extractTextContent(msg map[string]any) string {
	// Content may be at msg["content"] (legacy) or msg["message"]["content"] (current)
	content := msg["content"]
	if message, ok := msg["message"].(map[string]any); ok {
		content = message["content"]
	}

	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		var parts []string
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func extractLastSection(buf string) string {
	lines := strings.Split(buf, "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return strings.Join(lines, "\n")
}

// stripANSI removes ANSI escape sequences from terminal output.
var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\[[0-9;]*[mGKHJ]|\x1b[()][0-9A-B]`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}
