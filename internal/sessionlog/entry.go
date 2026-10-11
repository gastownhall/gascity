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
	"strings"
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
	Message     json.RawMessage `json:"message"` // {role, content} for user/assistant
	SystemEvent *SystemEvent    `json:"systemEvent,omitempty"`

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
	Raw         json.RawMessage `json:"-"`
	RawRecordID string          `json:"-"`
}

// SystemEvent carries provider-neutral system event metadata extracted from a
// provider transcript event.
type SystemEvent struct {
	Kind     string `json:"kind,omitempty"`
	Category string `json:"category,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
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
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Action    string          `json:"action,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content
	IsError   bool            `json:"is_error,omitempty"`
	FilePath  string          `json:"file_path,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	MIMEType  string          `json:"mime_type,omitempty"`
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

// ToolResultEvidence returns provider-neutral tool-result evidence carried
// outside the message content by provider transcript formats.
func (e *Entry) ToolResultEvidence() json.RawMessage {
	if e == nil || len(e.Raw) == 0 {
		return nil
	}
	var rawEntry struct {
		ToolUseResult json.RawMessage `json:"toolUseResult"`
	}
	if err := json.Unmarshal(e.Raw, &rawEntry); err != nil || len(rawEntry.ToolUseResult) == 0 || string(rawEntry.ToolUseResult) == "null" {
		return nil
	}
	return neutralClaudeToolResultEvidence(rawEntry.ToolUseResult)
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

func neutralClaudeToolResultEvidence(raw json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return nil
	}
	neutral := make(map[string]json.RawMessage)
	copyClaudeReadFileEvidence(neutral, object)
	copyClaudeSearchResultEvidence(neutral, object)
	copyClaudeQuestionEvidence(neutral, object)
	copyStringField(neutral, object, "file_path", "filePath", "file_path", "path", "file")
	copyStringField(neutral, object, "stdout", "stdout")
	copyStringField(neutral, object, "stderr", "stderr", "error")
	copyStringField(neutral, object, "status", "status", "state")
	copyStringField(neutral, object, "command", "command")
	copyStringField(neutral, object, "description", "description", "summary", "title")
	copyStringField(neutral, object, "mode", "mode")
	copyStringField(neutral, object, "content", "content")
	copyStringField(neutral, object, "output", "output", "result", "content", "text", "message")
	copyStringField(neutral, object, "old_string", "oldString", "old_string", "oldStr", "old_str")
	copyStringField(neutral, object, "new_string", "newString", "new_string", "newStr", "new_str")
	copyStringField(neutral, object, "original_file", "originalFile", "original_file")
	copyStringField(neutral, object, "query", "query")
	copyStringField(neutral, object, "task_id", "taskId", "task_id", "backgroundTaskId", "background_task_id", "bashId", "bash_id", "shellId", "shell_id", "agentId", "agent_id")
	copyStringField(neutral, object, "task_type", "taskType", "task_type", "taskKind", "task_kind", "subagentType", "subagent_type", "agentType", "agent_type")
	copyStringField(neutral, object, "task_status", "taskStatus", "task_status", "status", "state")
	copyStringField(neutral, object, "question", "question", "prompt")
	copyStringField(neutral, object, "answer", "answer", "response", "choice")
	copyStringField(neutral, object, "plan", "plan")
	copyStringField(neutral, object, "explanation", "explanation", "reason")
	copyStringField(neutral, object, "url", "url", "uri", "href")
	copyStringField(neutral, object, "timestamp", "timestamp")
	copyStringField(neutral, object, "status_text", "codeText", "statusText", "status_text")
	copyIntField(neutral, object, "exit_code", "exitCode", "exit_code")
	copyIntField(neutral, object, "status_code", "code", "statusCode", "status_code")
	copyIntField(neutral, object, "bytes", "bytes")
	copyIntField(neutral, object, "duration_ms", "durationMs", "duration_ms")
	copyIntField(neutral, object, "total_duration_ms", "totalDurationMs", "total_duration_ms")
	copyIntField(neutral, object, "total_tokens", "totalTokens", "total_tokens")
	copyIntField(neutral, object, "total_tool_use_count", "totalToolUseCount", "total_tool_use_count")
	copyDurationSecondsField(neutral, object)
	copyIntField(neutral, object, "num_files", "numFiles", "num_files", "count")
	copyIntField(neutral, object, "num_lines", "numLines", "num_lines")
	copyIntField(neutral, object, "applied_limit", "appliedLimit", "applied_limit")
	copyIntField(neutral, object, "stdout_lines", "stdoutLines", "stdout_lines")
	copyIntField(neutral, object, "stderr_lines", "stderrLines", "stderr_lines")
	copyBoolField(neutral, object, "truncated", "truncated")
	copyBoolField(neutral, object, "replace_all", "replaceAll", "replace_all")
	copyBoolField(neutral, object, "user_modified", "userModified", "user_modified")
	copyRawField(neutral, object, "filenames", "filenames", "files", "paths")
	copyRawField(neutral, object, "old_todos", "oldTodos", "old_todos")
	copyRawField(neutral, object, "new_todos", "newTodos", "new_todos")
	copyRawField(neutral, object, "options", "options", "choices")
	copyRawField(neutral, object, "answers", "answers", "answerMap", "answer_map")
	copyRawField(neutral, object, "steps", "steps")
	if rawPatch, ok := object["structuredPatch"]; ok {
		if patchHunks := neutralPatchHunks(rawPatch, jsonStringValue(neutral["file_path"])); len(patchHunks) > 0 {
			neutral["patch_hunks"] = mustMarshal(patchHunks)
		}
	}
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func copyClaudeReadFileEvidence(neutral map[string]json.RawMessage, object map[string]json.RawMessage) {
	raw, ok := object["file"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil || len(file) == 0 {
		return
	}
	copyStringField(neutral, file, "file_path", "filePath", "file_path", "path", "file")
	copyStringField(neutral, file, "content", "content", "text")
	copyStringField(neutral, file, "language", "language", "lang")
	copyIntField(neutral, file, "num_lines", "numLines", "num_lines")
	copyIntField(neutral, file, "start_line", "startLine", "start_line")
	copyIntField(neutral, file, "total_lines", "totalLines", "total_lines")
}

type neutralSearchResultItem struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

type neutralQuestionOption struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

type neutralQuestion struct {
	Question    string                  `json:"question,omitempty"`
	Header      string                  `json:"header,omitempty"`
	Options     []neutralQuestionOption `json:"options,omitempty"`
	MultiSelect bool                    `json:"multi_select,omitempty"`
}

func copyClaudeQuestionEvidence(neutral map[string]json.RawMessage, object map[string]json.RawMessage) {
	questions := neutralClaudeQuestions(object["questions"])
	if len(questions) > 0 {
		neutral["questions"] = mustMarshal(questions)
	}
}

func neutralClaudeQuestions(raw json.RawMessage) []neutralQuestion {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rawQuestions []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawQuestions); err != nil || len(rawQuestions) == 0 {
		return nil
	}
	out := make([]neutralQuestion, 0, len(rawQuestions))
	for _, object := range rawQuestions {
		question := neutralQuestion{
			Question:    strings.TrimSpace(jsonStringValue(object["question"])),
			Header:      strings.TrimSpace(jsonStringValue(object["header"])),
			Options:     neutralClaudeQuestionOptions(object["options"]),
			MultiSelect: jsonBoolValue(object["multiSelect"]) || jsonBoolValue(object["multi_select"]),
		}
		if question.Question != "" || question.Header != "" || len(question.Options) > 0 {
			out = append(out, question)
		}
	}
	return out
}

func neutralClaudeQuestionOptions(raw json.RawMessage) []neutralQuestionOption {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rawOptions []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawOptions); err != nil || len(rawOptions) == 0 {
		return nil
	}
	out := make([]neutralQuestionOption, 0, len(rawOptions))
	for _, object := range rawOptions {
		option := neutralQuestionOption{
			Label:       strings.TrimSpace(jsonStringValue(object["label"])),
			Description: strings.TrimSpace(jsonStringValue(object["description"])),
		}
		if option.Label != "" || option.Description != "" {
			out = append(out, option)
		}
	}
	return out
}

func copyClaudeSearchResultEvidence(neutral map[string]json.RawMessage, object map[string]json.RawMessage) {
	items := neutralClaudeSearchResultItems(object["results"])
	if len(items) > 0 {
		neutral["result_items"] = mustMarshal(items)
	}
}

func neutralClaudeSearchResultItems(raw json.RawMessage) []neutralSearchResultItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil || len(rawItems) == 0 {
		return nil
	}
	out := make([]neutralSearchResultItem, 0, len(rawItems))
	seen := make(map[string]struct{})
	for _, rawItem := range rawItems {
		var itemObject map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &itemObject); err != nil || len(itemObject) == 0 {
			continue
		}
		if item := neutralClaudeSearchResultItem(itemObject); item.URL != "" || item.Title != "" {
			key := item.URL + "\x00" + item.Title + "\x00" + item.Snippet
			if _, ok := seen[key]; !ok {
				out = append(out, item)
				seen[key] = struct{}{}
			}
		}
		for _, nested := range neutralClaudeSearchResultContentItems(itemObject["content"]) {
			key := nested.URL + "\x00" + nested.Title + "\x00" + nested.Snippet
			if _, ok := seen[key]; ok {
				continue
			}
			out = append(out, nested)
			seen[key] = struct{}{}
		}
	}
	return out
}

func neutralClaudeSearchResultContentItems(raw json.RawMessage) []neutralSearchResultItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rawContent []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawContent); err != nil || len(rawContent) == 0 {
		return nil
	}
	out := make([]neutralSearchResultItem, 0, len(rawContent))
	for _, itemObject := range rawContent {
		if item := neutralClaudeSearchResultItem(itemObject); item.URL != "" || item.Title != "" {
			out = append(out, item)
		}
	}
	return out
}

func neutralClaudeSearchResultItem(object map[string]json.RawMessage) neutralSearchResultItem {
	return neutralSearchResultItem{
		Title:   strings.TrimSpace(jsonStringValue(object["title"])),
		URL:     strings.TrimSpace(jsonStringValue(object["url"])),
		Snippet: firstNonEmpty(strings.TrimSpace(jsonStringValue(object["snippet"])), strings.TrimSpace(jsonStringValue(object["description"]))),
	}
}

func copyStringField(dst map[string]json.RawMessage, src map[string]json.RawMessage, dstName string, srcNames ...string) {
	for _, name := range srcNames {
		value := jsonStringValue(src[name])
		if strings.TrimSpace(value) == "" {
			continue
		}
		dst[dstName] = mustMarshal(value)
		return
	}
}

func copyIntField(dst map[string]json.RawMessage, src map[string]json.RawMessage, dstName string, srcNames ...string) {
	for _, name := range srcNames {
		value, ok := jsonIntValue(src[name])
		if !ok {
			continue
		}
		dst[dstName] = mustMarshal(value)
		return
	}
}

func copyBoolField(dst map[string]json.RawMessage, src map[string]json.RawMessage, dstName string, srcNames ...string) {
	for _, name := range srcNames {
		raw, ok := src[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		dst[dstName] = mustMarshal(value)
		return
	}
}

func copyDurationSecondsField(dst map[string]json.RawMessage, src map[string]json.RawMessage) {
	if _, ok := dst["duration_ms"]; ok {
		return
	}
	value, ok := jsonFloatValue(src["durationSeconds"])
	if !ok {
		value, ok = jsonFloatValue(src["duration_seconds"])
	}
	if !ok {
		return
	}
	dst["duration_ms"] = mustMarshal(int(value * 1000))
}

func copyRawField(dst map[string]json.RawMessage, src map[string]json.RawMessage, dstName string, srcNames ...string) {
	for _, name := range srcNames {
		raw, ok := src[name]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		dst[dstName] = append(json.RawMessage(nil), raw...)
		return
	}
}

func jsonStringValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return ""
}

func jsonIntValue(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, true
	}
	return 0, false
}

func jsonFloatValue(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, true
	}
	return 0, false
}

func jsonBoolValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return false
}

type neutralPatchHunk struct {
	FilePath string   `json:"file_path,omitempty"`
	OldStart int      `json:"old_start,omitempty"`
	OldLines int      `json:"old_lines,omitempty"`
	NewStart int      `json:"new_start,omitempty"`
	NewLines int      `json:"new_lines,omitempty"`
	Lines    []string `json:"lines,omitempty"`
}

func neutralPatchHunks(raw json.RawMessage, filePath string) []neutralPatchHunk {
	var hunks []struct {
		FilePath string   `json:"filePath"`
		OldStart int      `json:"oldStart"`
		OldLines int      `json:"oldLines"`
		NewStart int      `json:"newStart"`
		NewLines int      `json:"newLines"`
		Lines    []string `json:"lines"`
	}
	if err := json.Unmarshal(raw, &hunks); err != nil || len(hunks) == 0 {
		return nil
	}
	out := make([]neutralPatchHunk, 0, len(hunks))
	for _, hunk := range hunks {
		out = append(out, neutralPatchHunk{
			FilePath: firstNonEmpty(hunk.FilePath, filePath),
			OldStart: hunk.OldStart,
			OldLines: hunk.OldLines,
			NewStart: hunk.NewStart,
			NewLines: hunk.NewLines,
			Lines:    append([]string(nil), hunk.Lines...),
		})
	}
	return out
}
