package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReadCodexFile reads a Codex JSONL session file and converts it to the
// standard Session format used by gc session logs.
//
// Codex entries use a different schema than Claude:
//   - session_meta: session initialization (skipped)
//   - event_msg: user messages, agent messages, reasoning, token counts
//   - response_item: messages, function calls, reasoning (preferred over event_msg)
//   - turn_context: per-turn configuration (skipped)
//
// Port of yepanywhere's CodexSessionReader.convertEntriesToMessages.
func ReadCodexFile(path string, _ int) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	var entries []codexEntry
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw codexRawEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if raw.Type == "" {
			continue
		}
		entries = append(entries, codexEntry{raw: raw, line: string(line)})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning codex session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	// Check if response_item entries contain user messages (preferred source).
	hasResponseItemUser := false
	for _, e := range entries {
		if e.raw.Type == "response_item" {
			var ri codexResponseItem
			if json.Unmarshal(e.raw.Payload, &ri) == nil && ri.Type == "message" && ri.Role == "user" {
				hasResponseItemUser = true
				break
			}
		}
	}

	var messages []*Entry
	idx := 0
	var lastUUID string

	for _, e := range entries {
		ts, _ := time.Parse(time.RFC3339Nano, e.raw.Timestamp)

		switch e.raw.Type {
		case "response_item":
			entry := convertResponseItem(e.raw.Payload, e.line, idx, ts)
			if entry != nil {
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++
			}

		case "event_msg":
			var em codexEventMsg
			if json.Unmarshal(e.raw.Payload, &em) != nil {
				continue
			}
			switch em.Type {
			case "user_message":
				if hasResponseItemUser {
					continue // prefer response_item user messages
				}
				entry := &Entry{
					UUID:      fmt.Sprintf("codex-event-%d", idx),
					Type:      "user",
					Timestamp: ts,
					Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(em.Message)}),
					Raw:       json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++

			case "agent_message":
				// Skip — response_item has the complete text.
				// Only include if no response_items exist.
				if hasResponseItemUser {
					continue
				}
				entry := &Entry{
					UUID:      fmt.Sprintf("codex-event-%d", idx),
					Type:      "assistant",
					Timestamp: ts,
					Message: mustMarshal(MessageContent{
						Role:    "assistant",
						Content: mustMarshal([]ContentBlock{{Type: "text", Text: em.Message}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++

			case "agent_reasoning":
				entry := &Entry{
					UUID:      fmt.Sprintf("codex-event-%d", idx),
					Type:      "assistant",
					Timestamp: ts,
					Message: mustMarshal(MessageContent{
						Role:    "assistant",
						Content: mustMarshal([]ContentBlock{{Type: "thinking", Text: em.Text}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++

			case "error", "stream_error", "turn_aborted":
				entry := &Entry{
					UUID:      fmt.Sprintf("codex-event-%d", idx),
					Type:      "system",
					Timestamp: ts,
					Message: mustMarshal(MessageContent{
						Role:    "system",
						Content: mustMarshal([]ContentBlock{{Type: "text", Text: codexErrorText(em)}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++

			default:
				if skipCodexEventMsgType(em.Type) {
					continue
				}
				entry := &Entry{
					UUID:      fmt.Sprintf("codex-event-%d", idx),
					Type:      "event_msg",
					Timestamp: ts,
					Raw:       json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
				idx++
			}
		}
	}

	return &Session{
		ID:          codexSessionID(path),
		Messages:    messages,
		Diagnostics: diagnostics,
	}, nil
}

func convertResponseItem(payload json.RawMessage, rawLine string, idx int, ts time.Time) *Entry {
	var ri codexResponseItem
	if json.Unmarshal(payload, &ri) != nil {
		return nil
	}

	uuid := fmt.Sprintf("codex-%d", idx)

	switch ri.Type {
	case "message":
		if ri.Role == "developer" {
			return nil
		}
		// Concatenate all text blocks.
		var fullText string
		for _, c := range ri.Content {
			fullText += c.Text
		}
		entryType := ri.Role
		if entryType == "" {
			entryType = "assistant"
		}
		return &Entry{
			UUID:      uuid,
			Type:      entryType,
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    ri.Role,
				Content: mustMarshal([]ContentBlock{{Type: "text", Text: fullText}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "reasoning":
		var summaryText string
		for _, s := range ri.Summary {
			summaryText += s.Text + "\n"
		}
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "assistant",
				Content: mustMarshal([]ContentBlock{{Type: "thinking", Text: summaryText}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "function_call", "custom_tool_call":
		callID := firstNonEmpty(ri.CallID, ri.ID)
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role: "assistant",
				Content: mustMarshal([]ContentBlock{{
					Type:  "tool_use",
					ID:    callID,
					Name:  ri.Name,
					Input: cloneRawJSON(ri.Input),
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "function_call_output", "custom_tool_call_output":
		callID := firstNonEmpty(ri.CallID, ri.ID)
		return &Entry{
			UUID:      uuid,
			Type:      "tool_result",
			Timestamp: ts,
			ToolUseID: callID,
			Message: mustMarshal(MessageContent{
				Role: "tool",
				Content: mustMarshal([]ContentBlock{{
					Type:      "tool_result",
					ToolUseID: callID,
					Content:   cloneRawJSON(ri.Output),
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "interaction":
		requestID := firstNonEmpty(ri.RequestID, ri.ID)
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role: "assistant",
				Content: mustMarshal([]ContentBlock{{
					Type:      "interaction",
					RequestID: requestID,
					Kind:      ri.Kind,
					State:     ri.State,
					Text:      ri.Text,
					Prompt:    ri.Prompt,
					Options:   append([]string(nil), ri.Options...),
					Action:    ri.Action,
					Metadata:  cloneRawJSON(ri.Metadata),
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}
	}

	return nil
}

func codexErrorText(em codexEventMsg) string {
	label := strings.TrimSpace(em.CodexErrorInfo)
	if label == "" {
		label = strings.TrimSpace(em.Type)
	}
	message := strings.TrimSpace(em.Message)
	switch {
	case label != "" && message != "":
		return label + ": " + message
	case message != "":
		return message
	default:
		return label
	}
}

func skipCodexEventMsgType(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "token_count",
		"exec_command_begin",
		"exec_command_end",
		"patch_apply_begin",
		"patch_apply_end",
		"task_started",
		"task_complete",
		"item_started",
		"item_completed",
		"context_compacted":
		return true
	default:
		return false
	}
}

func codexSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func cloneRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// Codex JSONL entry types.

type codexRawEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexEntry struct {
	raw  codexRawEntry
	line string
}

type codexEventMsg struct {
	Type           string `json:"type"`             // user_message, agent_message, agent_reasoning, token_count
	Message        string `json:"message"`          // for user_message, agent_message, error
	Text           string `json:"text"`             // for agent_reasoning
	CodexErrorInfo string `json:"codex_error_info"` // for usage_limit_exceeded and related errors
}

type codexResponseItem struct {
	Type      string             `json:"type"` // message, reasoning, function_call, custom_tool_call, function_call_output, custom_tool_call_output, interaction
	Role      string             `json:"role,omitempty"`
	Content   []codexTextContent `json:"content,omitempty"`
	Summary   []codexTextContent `json:"summary,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Input     json.RawMessage    `json:"input,omitempty"`
	Output    json.RawMessage    `json:"output,omitempty"`
	RequestID string             `json:"request_id,omitempty"`
	ID        string             `json:"id,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	State     string             `json:"state,omitempty"`
	Text      string             `json:"text,omitempty"`
	Prompt    string             `json:"prompt,omitempty"`
	Options   []string           `json:"options,omitempty"`
	Action    string             `json:"action,omitempty"`
	Metadata  json.RawMessage    `json:"metadata,omitempty"`
}

type codexTextContent struct {
	Text string `json:"text"`
}
