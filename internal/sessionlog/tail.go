package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TailMeta holds metadata extracted from the tail of a session file.
type TailMeta struct {
	Model        string
	ContextUsage *ContextUsage
	Activity     string // "idle", "in-turn", or "" (unknown)
}

// ContextUsage holds computed context usage data.
type ContextUsage struct {
	InputTokens   int `json:"input_tokens"`
	Percentage    int `json:"percentage"`
	ContextWindow int `json:"context_window"`
}

// tailChunkSize is how many bytes we read from the end of the file.
const tailChunkSize = 64 * 1024

// ExtractTailMeta reads the last portion of a session file to extract
// model and context usage without full DAG resolution. Returns nil (no
// error) if the file has no usable data.
func ExtractTailMeta(path string) (*TailMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	data, err := readTail(f, tailChunkSize)
	if err != nil {
		return nil, err
	}

	lines := splitLines(data)
	return extractFromLines(lines), nil
}

// ExtractTailMetaFromSearchPaths reads tail metadata only after verifying
// path resolves under one of the configured session-log search roots.
func ExtractTailMetaFromSearchPaths(searchPaths []string, path string) (*TailMeta, error) {
	safePath, err := validateSearchPathFile(searchPaths, path)
	if err != nil {
		return nil, err
	}
	return ExtractTailMeta(safePath)
}

func validateSearchPathFile(searchPaths []string, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty session log path")
	}
	cleanPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolving session log path: %w", err)
	}
	for _, root := range searchPaths {
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleanRoot, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(cleanRoot, cleanPath)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		return cleanPath, nil
	}
	return "", fmt.Errorf("session log path is outside configured search paths")
}

// readTail reads the last n bytes of r (or the whole thing if smaller).
func readTail(r io.ReadSeeker, n int64) ([]byte, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	offset := size - n
	if offset < 0 {
		offset = 0
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// splitLines splits data into JSONL lines. Partial lines from a mid-file
// read are tolerated — they fail json.Unmarshal silently in the caller.
func splitLines(data []byte) [][]byte {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	var lines [][]byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
	}
	return lines
}

// tailEntry is the minimal structure we decode from each JSONL line.
type tailEntry struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message"`
}

// messageStopReason extracts stop_reason from an assistant message.
type messageStopReason struct {
	StopReason string `json:"stop_reason"`
}

// InferActivity derives session activity state from a JSONL entry.
//
// Returns:
//   - "idle" — session finished its turn (end_turn stop reason or turn_duration system event)
//   - "in-turn" — session is actively processing (tool_use stop reason or user message)
//   - "" — unknown / insufficient data
func InferActivity(entryType, subtype string, message json.RawMessage) string {
	switch entryType {
	case "system":
		if subtype == "turn_duration" {
			return "idle"
		}
	case "assistant":
		if len(message) == 0 {
			return ""
		}
		raw := unwrapJSONString(message)
		var msg messageStopReason
		if err := json.Unmarshal(raw, &msg); err != nil {
			return ""
		}
		if msg.StopReason == "" {
			return "" // no stop_reason yet — streaming or partial entry
		}
		if msg.StopReason == "tool_use" {
			return "in-turn"
		}
		// Any other stop_reason (end_turn, stop_sequence, max_tokens, etc.)
		// means the assistant finished its turn.
		return "idle"
	case "user":
		if len(message) > 0 {
			// Check for interrupt messages — these end the turn, not start one.
			if isInterruptMessage(message) {
				return "idle"
			}
			return "in-turn"
		}
	}
	return ""
}

// InferActivityFromEntries walks entries backwards to find the last
// activity-defining entry. This mirrors the backwards-walk in
// extractFromLines but operates on parsed Entry values (for SSE streams).
func InferActivityFromEntries(entries []*Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] == nil {
			continue
		}
		if act := InferActivity(entries[i].Type, entries[i].Subtype, entries[i].Message); act != "" {
			return act
		}
	}
	return ""
}

// isInterruptMessage checks if a user message is an interrupt marker.
// Claude Code writes these when the user presses Escape/Ctrl-C mid-turn.
// The session is idle afterwards (waiting at the prompt), not starting a new turn.
func isInterruptMessage(message json.RawMessage) bool {
	raw := unwrapJSONString(message)
	// Try object form: {"content": [{"text": "..."}]} or {"content": "..."}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) == 0 {
		return false
	}
	// String content
	if msg.Content[0] == '"' {
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			return bytes.Contains([]byte(s), []byte("[Request interrupted by user]"))
		}
	}
	// Array content: [{"type":"text","text":"..."}]
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		for _, b := range blocks {
			if bytes.Contains([]byte(b.Text), []byte("[Request interrupted by user]")) {
				return true
			}
		}
	}
	return false
}

// unwrapJSONString handles JSONL files where the message field is stored
// as a JSON string (e.g. "message":"{\"role\":...}") rather than an
// object. If raw is a JSON string, it returns the unquoted inner bytes;
// otherwise returns raw unchanged.
func unwrapJSONString(raw json.RawMessage) json.RawMessage {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return json.RawMessage(s)
		}
	}
	return raw
}

// assistantMessage is the structure inside the "message" field for assistant entries.
type assistantMessage struct {
	Role  string `json:"role"`
	Model string `json:"model"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// extractFromLines walks lines backwards to find model, context usage, and activity.
func extractFromLines(lines [][]byte) *TailMeta {
	var (
		model     string
		lastUsage *assistantMessage
		activity  string
	)

	// Walk backwards — we want the last entries.
	for i := len(lines) - 1; i >= 0; i-- {
		var entry tailEntry
		if err := json.Unmarshal(lines[i], &entry); err != nil {
			continue
		}

		// Unwrap string-encoded JSON once for reuse.
		rawMsg := entry.Message
		if entry.Type == "assistant" && len(rawMsg) > 0 {
			rawMsg = unwrapJSONString(rawMsg)
		}

		// Infer activity from the last valid entry (first hit walking backwards).
		if activity == "" {
			activity = InferActivity(entry.Type, entry.Subtype, rawMsg)
		}

		// Check for assistant message with model/usage.
		if entry.Type == "assistant" && len(rawMsg) > 0 {
			var msg assistantMessage
			if err := json.Unmarshal(rawMsg, &msg); err != nil {
				continue
			}
			if model == "" && msg.Model != "" {
				model = msg.Model
			}
			if lastUsage == nil && msg.Usage != nil && msg.Usage.InputTokens > 0 {
				lastUsage = &msg
			}
		}

		// Once we have everything, stop scanning.
		if model != "" && lastUsage != nil && activity != "" {
			break
		}
	}

	if model == "" && lastUsage == nil && activity == "" {
		return nil
	}

	result := &TailMeta{Model: model, Activity: activity}

	if lastUsage != nil && lastUsage.Usage != nil {
		effectiveModel := model
		if effectiveModel == "" && lastUsage.Model != "" {
			effectiveModel = lastUsage.Model
		}

		contextWindow := ModelContextWindow(effectiveModel)
		if contextWindow > 0 {
			totalInput := lastUsage.Usage.InputTokens +
				lastUsage.Usage.CacheReadInputTokens +
				lastUsage.Usage.CacheCreationInputTokens

			pct := totalInput * 100 / contextWindow
			if pct > 100 {
				pct = 100
			}

			result.ContextUsage = &ContextUsage{
				InputTokens:   totalInput,
				Percentage:    pct,
				ContextWindow: contextWindow,
			}
		}
	}

	return result
}
