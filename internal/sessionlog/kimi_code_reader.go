package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The durable Kimi Code wire is an event journal, not the legacy context
// message list. Assistant content and tool results live in loop events; prompt
// envelopes duplicate append_message and must not become a second user row.
type kimiCodeRecord struct {
	Type           string            `json:"type"`
	Time           int64             `json:"time"`
	Message        kimiCodeMessage   `json:"message"`
	Event          kimiCodeLoopEvent `json:"event"`
	Target         string            `json:"target"`
	Summary        json.RawMessage   `json:"summary"`
	ContextSummary string            `json:"contextSummary"`
}

type kimiCodeMessage struct {
	ID         string             `json:"id"`
	Role       string             `json:"role"`
	Content    json.RawMessage    `json:"content"`
	ToolCalls  []kimiCodeToolCall `json:"toolCalls"`
	ToolCallID string             `json:"toolCallId"`
	IsError    bool               `json:"isError"`
}

type kimiCodeToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type kimiCodeLoopEvent struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	Part       kimiCodePart    `json:"part"`
	Result     struct {
		Output  json.RawMessage `json:"output"`
		IsError bool            `json:"isError"`
	} `json:"result"`
}

type kimiCodePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Think string `json:"think"`
}

func readKimiCodeWire(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), maxTailTokenBytes)
	sess := &Session{ID: kimiSessionID(path)}
	ids := newStableSyntheticEntryIDSequence("kimi-code")
	var parent string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record kimiCodeRecord
		if err := json.Unmarshal(line, &record); err != nil {
			sess.Diagnostics.MalformedLineCount++
			sess.Diagnostics.MalformedTail = true
			continue
		}
		sess.Diagnostics.MalformedTail = false
		entry := kimiCodeEntry(record, ids.ForRecord(line).ID(""))
		if entry == nil {
			continue
		}
		entry.SessionID = sess.ID
		entry.ParentUUID = parent
		parent = entry.UUID
		if record.Time != 0 {
			entry.Timestamp = time.UnixMilli(record.Time).UTC()
		}
		entry.Raw = append(json.RawMessage(nil), line...)
		sess.Messages = append(sess.Messages, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning kimi code wire: %w", err)
	}
	sess.OrphanedToolUseIDs = findOrphanedToolUses(sess.Messages, collectAllToolResultIDs(sess.Messages))
	if len(sess.OrphanedToolUseIDs) == 0 {
		sess.OrphanedToolUseIDs = nil
	}
	return sess, nil
}

func kimiCodeEntry(record kimiCodeRecord, id string) *Entry {
	switch record.Type {
	case "context.append_message":
		m := record.Message
		if m.ID != "" {
			id = m.ID
		}
		switch m.Role {
		case "user", "assistant", "system":
			content := kimiMessageContent(m.Content)
			if len(m.ToolCalls) > 0 {
				blocks := kimiContentBlocks(m.Content)
				for _, call := range m.ToolCalls {
					blocks = append(blocks, ContentBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: call.Args})
				}
				content = mustMarshal(blocks)
			}
			return &Entry{UUID: id, Type: m.Role, Message: mustMarshal(MessageContent{Role: m.Role, Content: content})}
		case "tool":
			return kimiCodeToolResult(id, m.ToolCallID, m.Content, m.IsError)
		}
	case "context.append_loop_event":
		return kimiCodeLoopEntry(record.Event, id)
	case "turn.ended":
		return &Entry{UUID: id, Type: "system", Subtype: "turn_duration"}
	case "context.apply_compaction":
		content := record.Summary
		if record.ContextSummary != "" {
			content = mustMarshal(record.ContextSummary)
		}
		var summary kimiCodeMessage
		if json.Unmarshal(content, &summary) == nil && len(summary.Content) > 0 {
			content = summary.Content
		}
		return &Entry{UUID: id, Type: "system", Subtype: "compact_boundary", Message: mustMarshal(MessageContent{Role: "system", Content: content})}
	}
	return nil
}

func kimiCodeLoopEntry(event kimiCodeLoopEvent, id string) *Entry {
	if event.UUID != "" {
		id = event.UUID
	}
	var block ContentBlock
	switch event.Type {
	case "content.part":
		switch event.Part.Type {
		case "text":
			block = ContentBlock{Type: "text", Text: event.Part.Text}
		case "think":
			block = ContentBlock{Type: "thinking", Thinking: event.Part.Think}
		default:
			return nil
		}
	case "tool.call":
		if strings.TrimSpace(event.ToolCallID) == "" || strings.TrimSpace(event.Name) == "" {
			return nil
		}
		block = ContentBlock{Type: "tool_use", ID: event.ToolCallID, Name: event.Name, Input: event.Args}
	case "tool.result":
		return kimiCodeToolResult(id, event.ToolCallID, event.Result.Output, event.Result.IsError)
	default:
		return nil
	}
	return &Entry{UUID: id, Type: "assistant", Message: mustMarshal(MessageContent{Role: "assistant", Content: mustMarshal([]ContentBlock{block})})}
}

func kimiCodeToolResult(id, toolCallID string, output json.RawMessage, isError bool) *Entry {
	block := ContentBlock{Type: "tool_result", ToolUseID: toolCallID, Content: output, IsError: isError}
	return &Entry{UUID: id, Type: "result", ToolUseID: toolCallID, Message: mustMarshal(MessageContent{Role: "user", Content: mustMarshal([]ContentBlock{block})})}
}

// Kimi's step.end finishes an LLM call, not necessarily the turn. Only the
// explicit turn terminal record establishes idle; a tool result, retry, or
// streamed content keeps the session busy even after a long-running call.
func kimiCodeTailActivity(entryType string, line []byte) string {
	switch entryType {
	case "turn.ended":
		return "idle"
	case "turn.prompt", "turn.steer", "llm.request", "turn.step.retrying":
		return "in-turn"
	case "context.append_loop_event":
		var record kimiCodeRecord
		if json.Unmarshal(line, &record) != nil {
			return ""
		}
		switch record.Event.Type {
		case "step.begin", "step.end", "content.part", "tool.call", "tool.result":
			return "in-turn"
		}
	case "turn.cancel":
		var record kimiCodeRecord
		if json.Unmarshal(line, &record) == nil && record.Target == "active" {
			return "idle"
		}
	}
	return ""
}
