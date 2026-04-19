package api

import (
	"encoding/json"
	"strings"

	"github.com/gastownhall/gascity/internal/worker"
)

// entryToTurn converts a provider transcript entry to a human-readable output turn.
func entryToTurn(e *worker.TranscriptEntry) outputTurn {
	turn := outputTurn{
		Role: e.Type,
	}
	if !e.Timestamp.IsZero() {
		turn.Timestamp = e.Timestamp.Format("2006-01-02T15:04:05Z07:00")
	}

	// Try plain string content (message is a JSON object with string content).
	if text := e.TextContent(); text != "" {
		turn.Text = text
		return turn
	}

	// Try structured content blocks — extract human-readable text.
	if blocks := e.ContentBlocks(); len(blocks) > 0 {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			case "tool_use":
				if b.Name != "" {
					parts = append(parts, "["+b.Name+"]")
				}
			case "tool_result":
				text := extractToolResultText(b.Content)
				if text != "" {
					if len(text) > 500 {
						text = text[:500] + "…"
					}
					parts = append(parts, "[result] "+text)
				}
			case "thinking":
				// Redact thinking blocks — internal model reasoning
				// should not be surfaced to the UI.
				parts = append(parts, "[thinking]")
			}
		}
		turn.Text = strings.Join(parts, "\n")
		return turn
	}

	// Claude JSONL double-encodes the message field as a JSON string
	// containing JSON. Unwrap and try again.
	turn.Text = unwrapDoubleEncoded(e.Message)
	return turn
}

func historyEntryToTurn(entry worker.HistoryEntry) outputTurn {
	turn := outputTurn{
		Role: entry.Kind,
	}
	if turn.Role == "" {
		turn.Role = string(entry.Actor)
	}
	if entry.Timestamp != nil {
		turn.Timestamp = entry.Timestamp.Format("2006-01-02T15:04:05Z07:00")
	}

	if len(entry.Blocks) > 0 {
		var parts []string
		for _, block := range entry.Blocks {
			switch block.Kind {
			case worker.BlockKindText:
				if block.Text != "" {
					parts = append(parts, block.Text)
				}
			case worker.BlockKindToolUse:
				if block.Name != "" {
					parts = append(parts, "["+block.Name+"]")
				}
			case worker.BlockKindToolResult:
				text := extractToolResultText(block.Content)
				if text != "" {
					if len(text) > 500 {
						text = text[:500] + "…"
					}
					parts = append(parts, "[result] "+text)
				}
			case worker.BlockKindThinking:
				parts = append(parts, "[thinking]")
			}
		}
		turn.Text = strings.Join(parts, "\n")
		if turn.Text != "" {
			return turn
		}
	}

	if strings.TrimSpace(entry.Text) != "" {
		turn.Text = entry.Text
		return turn
	}
	if turn.Text == "" {
		turn.Text = historyRawEntryText(entry.Provenance.Raw)
	}
	return turn
}

func historySnapshotTurns(snapshot *worker.HistorySnapshot) ([]outputTurn, []string) {
	if snapshot == nil {
		return nil, nil
	}
	turns := make([]outputTurn, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if !historyEntryVisibleInConversation(entry) {
			continue
		}
		turn := historyEntryToTurn(entry)
		if turn.Text == "" {
			continue
		}
		turns = append(turns, turn)
		ids = append(ids, entry.ID)
	}
	return turns, ids
}

func historyEntryVisibleInConversation(entry worker.HistoryEntry) bool {
	switch entry.Provenance.RawType {
	case "user", "assistant", "system", "result":
		return true
	}
	switch entry.Kind {
	case "user", "assistant", "system", "result":
		return true
	default:
		return false
	}
}

func historySnapshotRawMessages(snapshot *worker.HistorySnapshot) ([]json.RawMessage, []string) {
	if snapshot == nil {
		return nil, nil
	}
	rawMessages := make([]json.RawMessage, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if len(entry.Provenance.Raw) == 0 {
			continue
		}
		rawMessages = append(rawMessages, entry.Provenance.Raw)
		ids = append(ids, entry.ID)
	}
	return rawMessages, ids
}

func historySnapshotActivity(snapshot *worker.HistorySnapshot) string {
	if snapshot == nil {
		return ""
	}
	switch snapshot.TailState.Activity {
	case worker.TailActivityIdle:
		return "idle"
	case worker.TailActivityInTurn:
		return "in-turn"
	default:
		return ""
	}
}

// extractToolResultText extracts human-readable text from a tool_result
// Content field (json.RawMessage). The content can be a plain string or
// an array of content blocks (e.g., [{type:"text", text:"..."}]).
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	// Try array of content blocks.
	var blocks []worker.TranscriptContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// unwrapDoubleEncoded handles Claude's double-encoded message format
// where the "message" field is a JSON string containing a JSON object.
// Returns the human-readable content text, or "" if not parseable.
func unwrapDoubleEncoded(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err == nil {
		raw = []byte(inner)
	}
	var mc worker.TranscriptMessageContent
	if err := json.Unmarshal(raw, &mc); err != nil {
		return ""
	}
	// Try string content.
	var s string
	if err := json.Unmarshal(mc.Content, &s); err == nil && s != "" {
		return s
	}
	// Try array of content blocks.
	var blocks []worker.TranscriptContentBlock
	if err := json.Unmarshal(mc.Content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func historyRawEntryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var entry struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	return unwrapDoubleEncoded(entry.Message)
}
