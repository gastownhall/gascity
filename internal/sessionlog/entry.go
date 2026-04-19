// Package sessionlog reads agent JSONL session files.
//
// Supports multiple session file formats:
//   - Claude: ~/.claude/projects/{slug}/{id}.jsonl (DAG with uuid/parentUuid)
//   - Codex: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl (flat, cwd in session_meta)
//
// Claude files form a DAG — each entry has a uuid and parentUuid. This
// package resolves the DAG to find the active conversation branch,
// pairs tool_use with tool_result, handles compact boundaries for
// pagination, and provides a structured read API.
//
// This is the observation layer (like kubectl logs). The event bus is
// the control-plane layer (like kubectl get events). They serve
// different purposes and should not be conflated.
package sessionlog

import (
	"encoding/json"
	"time"
)

// Entry is a single line from a Claude JSONL session file. Only the
// fields needed for DAG resolution, message classification, and tool
// pairing are decoded. The full JSON is preserved in Raw for consumers
// that need provider-specific fields.
type Entry struct {
	// Identity
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`

	// Classification
	Type    string `json:"type"`    // user, assistant, system, tool_use, tool_result, progress, result, file-history-snapshot
	Subtype string `json:"subtype"` // compact_boundary, init, status, etc. (system entries only)

	// Content
	Message json.RawMessage `json:"message"` // {role, content} for user/assistant

	// Tool pairing
	ToolUseID string `json:"toolUseID,omitempty"` // tool_use block ID (for tool_result pairing)

	// Compact boundary
	LogicalParentUUID string       `json:"logicalParentUuid,omitempty"` // bridges DAG across compaction
	CompactMetadata   *CompactMeta `json:"compactMetadata,omitempty"`
	IsCompactSummary  bool         `json:"isCompactSummary,omitempty"`

	// Metadata
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId,omitempty"`

	// Raw preserves the full JSON line for pass-through to API consumers.
	Raw json.RawMessage `json:"-"`
}

// CompactMeta carries context-compaction metadata.
type CompactMeta struct {
	Trigger   string `json:"trigger"`
	PreTokens int    `json:"preTokens"`
}

// ContentBlock is a block within a message's content array.
type ContentBlock struct {
	Type      string          `json:"type"` // text, tool_use, tool_result, interaction, thinking, image
	ID        string          `json:"id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	State     string          `json:"state,omitempty"`
	Text      string          `json:"text,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Action    string          `json:"action,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content
	IsError   bool            `json:"is_error,omitempty"`
}

// MessageContent is the structure inside a user or assistant message.
type MessageContent struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

// IsCompactBoundary returns true if this entry marks a context compaction.
func (e *Entry) IsCompactBoundary() bool {
	return e.Type == "system" && e.Subtype == "compact_boundary"
}

// ContentBlocks parses the message content as a slice of ContentBlock.
// Returns nil if the message is empty or content is a plain string.
func (e *Entry) ContentBlocks() []ContentBlock {
	if len(e.Message) == 0 {
		return nil
	}
	var mc MessageContent
	if err := json.Unmarshal(e.Message, &mc); err != nil {
		return nil
	}
	if len(mc.Content) == 0 {
		return nil
	}
	// Try array of blocks first.
	var blocks []ContentBlock
	if err := json.Unmarshal(mc.Content, &blocks); err == nil {
		return blocks
	}
	return nil
}

// TextContent returns the message content as a plain string.
// Returns "" if the content is an array of blocks or not a message.
func (e *Entry) TextContent() string {
	if len(e.Message) == 0 {
		return ""
	}
	var mc MessageContent
	if err := json.Unmarshal(e.Message, &mc); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(mc.Content, &s); err != nil {
		return ""
	}
	return s
}
