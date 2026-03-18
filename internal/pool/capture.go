package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// transcriptEntry pairs a parsed JSONL entry with its original line.
type transcriptEntry struct {
	data map[string]any
	raw  string
}

// captureOutput returns session output filtered by source/turns/detail params.
func (m *Manager) captureOutput(s *Session, source string, turns int, detail string) string {
	if source == "buffer" {
		return m.captureBuffer(s, turns)
	}
	return m.captureJSONL(s, turns, detail)
}

// captureJSONL reads the JSONL transcript and filters by turns and detail level.
//
// Optimization: only parses JSON for lines within the requested turn range.
// For turns=1 (the default), this means scanning backwards from the end to find
// the last user prompt, then parsing only entries from that point forward.
func (m *Manager) captureJSONL(s *Session, turns int, detail string) string {
	lines := m.readTranscriptLines(s)
	if len(lines) == 0 {
		return ""
	}

	// Strip leading /clear entries — these are internal pool commands sent
	// during slot recycling, not real user prompts. Only strip from the
	// start of the transcript (later /clear commands could be user-initiated).
	lines = stripLeadingClear(lines)
	if len(lines) == 0 {
		return ""
	}

	// Find start line for requested turn range by scanning from the end.
	startLine := findTurnStart(lines, turns)

	// For raw detail, return lines directly — no JSON parsing needed.
	if detail == "raw" {
		return strings.Join(lines[startLine:], "\n")
	}

	// Parse only the selected lines.
	entries := parseLines(lines[startLine:])

	switch detail {
	case "tools":
		return filterTools(entries)
	case "assistant":
		return filterAssistantDetail(entries, false)
	default: // "last"
		return filterAssistantDetail(entries, true)
	}
}

// findTurnStart scans lines from the end to find where the Nth-from-last turn
// begins. Returns 0 if turns == 0 or fewer turns exist than requested.
func findTurnStart(lines []string, turns int) int {
	if turns == 0 {
		return 0
	}
	found := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var entry map[string]any
		if json.Unmarshal([]byte(lines[i]), &entry) != nil {
			continue
		}
		if isUserPrompt(entry) {
			found++
			if found >= turns {
				return i
			}
		}
	}
	return 0 // fewer turns than requested — return everything
}

// captureBuffer returns rendered terminal screen content for the requested turns.
// Uses the persistent vt10x terminal emulator (same one used for typing detection)
// to produce a clean snapshot — like what you'd see if you attached to the session.
// Turn boundaries are determined from the JSONL transcript.
func (m *Manager) captureBuffer(s *Session, turns int) string {
	sl := m.slotForSession(s)
	if sl == nil || sl.Term == nil {
		return ""
	}
	st := sl.Term
	buf := st.renderedScreen()
	if buf == "" || turns == 0 {
		return buf
	}

	// Find user prompt texts from JSONL to locate turn boundaries in the buffer.
	prompts := m.userPromptTexts(s)
	if len(prompts) == 0 {
		return buf
	}

	startIdx := 0
	if turns < len(prompts) {
		startIdx = len(prompts) - turns
	}

	// Find the prompt in the buffer and return everything from that point.
	pos := strings.Index(buf, prompts[startIdx])
	if pos < 0 {
		return buf
	}
	return buf[pos:]
}

// readTranscriptLines reads a session's JSONL transcript and returns raw lines
// without parsing JSON. Returns nil if the transcript doesn't exist or is empty.
func (m *Manager) readTranscriptLines(s *Session) []string {
	if s.ClaudeUUID == "" {
		return nil
	}
	path := m.findTranscript(s.ClaudeUUID)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Filter empty lines.
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// parseLines parses raw JSONL lines into transcript entries.
func parseLines(lines []string) []transcriptEntry {
	entries := make([]transcriptEntry, 0, len(lines))
	for _, line := range lines {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, transcriptEntry{data: entry, raw: line})
		}
	}
	return entries
}

// userPromptTexts extracts user prompt text strings from the JSONL transcript.
func (m *Manager) userPromptTexts(s *Session) []string {
	lines := stripLeadingClear(m.readTranscriptLines(s))
	entries := parseLines(lines)
	var prompts []string
	for _, e := range entries {
		if isUserPrompt(e.data) {
			if text := extractTextContent(e.data); text != "" {
				prompts = append(prompts, text)
			}
		}
	}
	return prompts
}

// stripLeadingClear removes /clear user prompts (and everything before them)
// from the start of the transcript. These are pool-internal commands from slot
// recycling, not real user prompts. Only strips up to the first assistant
// response — later /clear commands are preserved (could be user-initiated).
func stripLeadingClear(lines []string) []string {
	// Scan until the first assistant response (real content). Track /clear
	// entries found in the preamble — internal messages (progress, system,
	// local-command-caveat) may appear before /clear.
	lastClearIdx := -1
	for i, line := range lines {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		typ, _ := entry["type"].(string)
		if typ == "assistant" {
			break // real content started
		}
		if typ == "user" && isClearCommand(extractTextContent(entry)) {
			lastClearIdx = i
		}
	}
	if lastClearIdx < 0 {
		return lines // no leading /clear found
	}
	remaining := lines[lastClearIdx+1:]
	if len(remaining) == 0 {
		return nil
	}
	return remaining
}

// isClearCommand returns true if the text is a /clear slash command.
// Claude Code may wrap it as plain "/clear" or "<command-name>/clear</command-name>".
func isClearCommand(text string) bool {
	text = strings.TrimSpace(text)
	return text == "/clear" || strings.Contains(text, "/clear</command-name>")
}

// --- Turn boundary detection ---

// isUserPrompt returns true if the entry is a user prompt (has text content, not tool_result).
// Claude Code writes user prompts with message.content as a plain string (not an array of
// content blocks), while tool results use an array with tool_result blocks.
func isUserPrompt(entry map[string]any) bool {
	if typ, _ := entry["type"].(string); typ != "user" {
		return false
	}
	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return false
	}
	switch content := msg["content"].(type) {
	case string:
		return true
	case []any:
		for _, c := range content {
			if block, ok := c.(map[string]any); ok {
				if t, _ := block["type"].(string); t == "text" {
					return true
				}
			}
		}
	}
	return false
}

// hasBlockType checks if an entry's message.content contains a block of the given type.
func hasBlockType(entry map[string]any, blockType string) bool {
	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return false
	}
	content, _ := msg["content"].([]any)
	for _, c := range content {
		if block, ok := c.(map[string]any); ok {
			if t, _ := block["type"].(string); t == blockType {
				return true
			}
		}
	}
	return false
}

// hasTextContent returns true if an entry contains text content blocks.
func hasTextContent(entry map[string]any) bool {
	return hasBlockType(entry, "text")
}

// --- Detail filters ---

// filterTools includes only user and assistant entries, with metadata stripped.
func filterTools(entries []transcriptEntry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		typ, _ := e.data["type"].(string)
		if typ != "user" && typ != "assistant" {
			continue
		}
		lines = append(lines, marshalEntry(e, true, false))
	}
	return strings.Join(lines, "\n")
}

// filterAssistantDetail handles both "last" and "assistant" detail levels.
// Groups entries into turns, then per turn:
//   - includes the user prompt
//   - "last" (lastOnly=true): includes only the final assistant entry with text
//   - "assistant" (lastOnly=false): includes all assistant entries with text
//
// Tool_use content blocks are stripped from included assistant entries.
// Tool_result user entries are excluded.
func filterAssistantDetail(entries []transcriptEntry, lastOnly bool) string {
	// Group entries into turns. A turn starts at a user prompt.
	var turns [][]transcriptEntry
	for _, e := range entries {
		if isUserPrompt(e.data) {
			turns = append(turns, nil)
		}
		if len(turns) > 0 {
			turns[len(turns)-1] = append(turns[len(turns)-1], e)
		}
	}

	lines := make([]string, 0, len(entries))
	for _, t := range turns {
		// User prompt — strip metadata for clean output.
		if len(t) > 0 && isUserPrompt(t[0].data) {
			lines = append(lines, marshalEntry(t[0], true, false))
		}

		if lastOnly {
			// Last assistant with text content
			for i := len(t) - 1; i >= 0; i-- {
				typ, _ := t[i].data["type"].(string)
				if typ == "assistant" && hasTextContent(t[i].data) {
					lines = append(lines, marshalEntry(t[i], true, true))
					break
				}
			}
		} else {
			// All assistant entries with text content
			for _, e := range t {
				typ, _ := e.data["type"].(string)
				if typ == "assistant" && hasTextContent(e.data) {
					lines = append(lines, marshalEntry(e, true, true))
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// --- Metadata stripping ---

// Fields stripped from entries for detail="tools".
var entryMetadataFields = map[string]bool{
	"parentUuid": true, "isSidechain": true, "version": true, "gitBranch": true,
	"requestId": true, "uuid": true, "timestamp": true, "cwd": true,
	"sessionId": true, "userType": true, "entrypoint": true,
	"permissionMode": true, "promptId": true,
}

// Fields stripped from message objects for detail="tools".
var messageMetadataFields = map[string]bool{
	"model": true, "id": true, "usage": true, "stop_reason": true, "stop_sequence": true,
}

// stripEntryMetadata returns a shallow copy of the entry with metadata fields removed.
func stripEntryMetadata(entry map[string]any) map[string]any {
	result := make(map[string]any, len(entry))
	for k, v := range entry {
		if !entryMetadataFields[k] {
			result[k] = v
		}
	}
	if msg, ok := result["message"].(map[string]any); ok {
		newMsg := make(map[string]any, len(msg))
		for k, v := range msg {
			if !messageMetadataFields[k] {
				newMsg[k] = v
			}
		}
		result["message"] = newMsg
	}
	return result
}

// marshalEntry serializes an entry with optional metadata stripping and tool_use removal.
// Falls back to the raw JSONL line on marshal error.
func marshalEntry(e transcriptEntry, stripMeta, stripToolUse bool) string {
	data := e.data
	if stripMeta {
		data = stripEntryMetadata(data)
	}
	if stripToolUse {
		data = removeToolUseBlocks(data)
	}
	if out, err := json.Marshal(data); err == nil {
		return string(out)
	}
	return e.raw
}

// removeToolUseBlocks returns a copy of the entry with tool_use content blocks removed.
// Returns the input unchanged if no tool_use blocks are present.
func removeToolUseBlocks(entry map[string]any) map[string]any {
	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return entry
	}
	content, _ := msg["content"].([]any)

	// Single pass: filter and detect changes.
	filtered := make([]any, 0, len(content))
	for _, c := range content {
		if block, ok := c.(map[string]any); ok {
			if t, _ := block["type"].(string); t == "tool_use" {
				continue
			}
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == len(content) {
		return entry // nothing removed
	}

	// Copy entry and message with filtered content.
	result := make(map[string]any, len(entry))
	for k, v := range entry {
		result[k] = v
	}
	newMsg := make(map[string]any, len(msg))
	for k, v := range msg {
		newMsg[k] = v
	}
	newMsg["content"] = filtered
	result["message"] = newMsg
	return result
}

// --- Shared helpers ---

func (m *Manager) findTranscript(claudeUUID string) string {
	dirs := m.transcriptDirs
	if len(dirs) == 0 {
		home, _ := os.UserHomeDir()
		dirs = []string{filepath.Join(home, ".claude", "projects")}
	}
	for _, dir := range dirs {
		pattern := filepath.Join(dir, "*", claudeUUID+".jsonl")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// extractTextContent extracts text from a Claude JSONL message entry.
// Handles both legacy (content string) and current (message.content array) formats.
func extractTextContent(msg map[string]any) string {
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

// extractLastSection returns the last 50 lines of a buffer string.
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
