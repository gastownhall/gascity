package sessionlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// This file reads the JSON-RPC capture transcripts gc itself writes for ACP
// sessions (internal/runtime/acp/capture.go, format v1). Each file holds one
// JSON object per line: a header {"gc_acp_capture":1,...} at every agent
// start, then {"ts","dir":"out"|"in","msg":<JSON-RPC message>} records and
// occasional {"dir":"meta","dropped":N} gap markers. The file is append-only,
// so entry ids derived from line positions and ACP message ids are stable as
// it grows.
//
// It is unrelated to acp_capture_reader.go, which reads the session files
// that ACP-speaking CLIs (grok, auggie, cursor) write themselves.

// acpCaptureFormatVersion is the newest capture format this reader accepts.
const acpCaptureFormatVersion = 1

const (
	acpMethodPrompt            = "session/prompt"
	acpMethodUpdate            = "session/update"
	acpMethodRequestPermission = "session/request_permission"

	acpDirOut = "out"
	acpDirIn  = "in"

	acpStopReasonEndTurn   = "end_turn"
	acpStopReasonCancelled = "cancelled" //nolint:misspell // ACP StopReason wire value
)

// Chunk run kinds; they also name the entry-id namespace of a run.
const (
	acpRunMessage = "message"
	acpRunThought = "thought"
	acpRunUser    = "user"
)

// IsACPCapturePath reports whether path lies under a city's ACP capture root
// (citylayout.ACPTranscriptsRoot). Such a file is always read with the ACP
// capture reader, whatever the session's provider is called: gc wrote it, so
// its format does not depend on which agent spoke ACP.
func IsACPCapturePath(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	clean := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
	marker := "/" + filepath.ToSlash(citylayout.ACPTranscriptsRoot) + "/"
	return strings.Contains(clean, marker)
}

// ReadACPCaptureFile reads a gc ACP capture transcript and converts it to the
// standard Session format. The first well-formed line must be a capture
// header. Lines that do not parse are skipped and counted; a torn final line
// sets Diagnostics.MalformedTail. tailCompactions > 0 attaches pagination
// metadata (captures have no compaction boundaries, so every entry is
// returned).
func ReadACPCaptureFile(path string, tailCompactions int) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	r := newACPCaptureReader()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)
	lineIndex := -1
	for scanner.Scan() {
		lineIndex++
		if err := r.consume(lineIndex, scanner.Bytes()); err != nil {
			return nil, fmt.Errorf("reading acp capture %q: %w", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning acp capture %q: %w", path, err)
	}
	sess := r.finish(path)
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}
	return sess, nil
}

// acpCaptureLine is one decoded capture line: a header or a record.
type acpCaptureLine struct {
	Version   int             `json:"gc_acp_capture"`
	TS        string          `json:"ts"`
	SessionID string          `json:"session_id"`
	Dir       string          `json:"dir"`
	Msg       json.RawMessage `json:"msg"`
}

func (l acpCaptureLine) isHeader() bool { return l.Version != 0 }

// acpRPCMessage is the subset of a JSON-RPC 2.0 message the reader needs.
type acpRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *acpRPCError    `json:"error"`
}

type acpRPCError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// idKey is the id's canonical JSON text, or "" for a notification.
func (m acpRPCMessage) idKey() string {
	id := bytes.TrimSpace(m.ID)
	if len(id) == 0 || string(id) == "null" {
		return ""
	}
	return string(id)
}

func (m acpRPCMessage) isResponse() bool {
	return m.Method == "" && m.idKey() != ""
}

// decodeACPCaptureLine parses one non-empty capture line. ok is false for a
// line that is not a JSON object.
func decodeACPCaptureLine(line []byte) (acpCaptureLine, acpRPCMessage, bool) {
	var rec acpCaptureLine
	if err := json.Unmarshal(line, &rec); err != nil {
		return acpCaptureLine{}, acpRPCMessage{}, false
	}
	var msg acpRPCMessage
	if len(rec.Msg) > 0 {
		_ = json.Unmarshal(rec.Msg, &msg) // a non-object msg is simply not a JSON-RPC message
	}
	return rec, msg, true
}

// acpChunkRun is an assistant/user message being reassembled from chunks.
type acpChunkRun struct {
	kind      string
	messageID string
	entry     *Entry
	text      strings.Builder
	raws      []json.RawMessage
}

// acpToolState tracks one tool call of the current agent process.
type acpToolState struct {
	entry    *Entry
	name     string
	input    json.RawMessage
	terminal bool
}

// acpPermission is an unanswered session/request_permission.
type acpPermission struct {
	requestID string
	line      int
}

// acpCaptureReader turns capture lines into entries. State that belongs to
// one agent process (JSON-RPC ids, open tools and permission requests) resets
// at every header.
type acpCaptureReader struct {
	messages    []*Entry
	diagnostics SessionDiagnostics
	// sessionID is the ACP session id; gcSessionID (from the first header)
	// stands in when the agent never reported one.
	sessionID   string
	gcSessionID string
	headerSeen  bool

	// Per agent process.
	prompts     map[string]bool // out session/prompt ids awaiting a response
	permissions map[string]acpPermission
	tools       map[string]*acpToolState
	toolOrder   []string

	run *acpChunkRun
	// turnAssistant is the last assistant entry of the current turn; the
	// prompt response stamps its stop reason and usage there.
	turnAssistant *Entry
	// messageIDSeen counts runs per kind+messageId so a reused ACP message id
	// (the field is unstable) still yields distinct entry ids.
	messageIDSeen map[string]int
}

func newACPCaptureReader() *acpCaptureReader {
	r := &acpCaptureReader{messageIDSeen: make(map[string]int)}
	r.resetProcess()
	return r
}

func (r *acpCaptureReader) resetProcess() {
	r.prompts = make(map[string]bool)
	r.permissions = make(map[string]acpPermission)
	r.tools = make(map[string]*acpToolState)
	r.toolOrder = nil
	r.turnAssistant = nil
}

func (r *acpCaptureReader) consume(lineIndex int, line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	rec, msg, ok := decodeACPCaptureLine(line)
	if !ok {
		r.diagnostics.MalformedLineCount++
		r.diagnostics.MalformedTail = true
		return nil
	}
	r.diagnostics.MalformedTail = false
	raw := append(json.RawMessage(nil), line...)
	ts := parseACPTimestamp(rec.TS)

	if rec.isHeader() {
		if rec.Version > acpCaptureFormatVersion {
			return fmt.Errorf("unsupported capture format version %d (newest supported %d)", rec.Version, acpCaptureFormatVersion)
		}
		r.header(lineIndex, rec, raw, ts)
		return nil
	}
	if !r.headerSeen {
		return fmt.Errorf("line %d: not a gc ACP capture (first record is not a capture header)", lineIndex+1)
	}
	switch rec.Dir {
	case acpDirOut:
		r.outgoing(lineIndex, msg, raw, ts)
	case acpDirIn:
		r.incoming(lineIndex, msg, raw, ts)
	}
	return nil
}

func (r *acpCaptureReader) header(lineIndex int, rec acpCaptureLine, raw json.RawMessage, ts time.Time) {
	r.closeRun()
	if !r.headerSeen {
		r.headerSeen = true
		r.gcSessionID = strings.TrimSpace(rec.SessionID)
		r.resetProcess()
		return
	}
	// A later header is an agent restart: the old process's requests can no
	// longer be answered.
	blocks := []ContentBlock{{Type: "text", Text: "Agent restarted"}}
	for _, key := range sortedPermissionKeys(r.permissions) {
		blocks = append(blocks, ContentBlock{
			Type:      "interaction",
			RequestID: r.permissions[key].requestID,
			Kind:      "approval",
			State:     "dismissed",
		})
	}
	r.append(&Entry{
		UUID:      acpLineEntryID(lineIndex, "restart"),
		Type:      "system",
		Timestamp: ts,
		Message:   kiroMessageWithBlocks("system", blocks),
		SystemEvent: &SystemEvent{
			Kind:    "agent_restart",
			Message: "Agent restarted",
		},
		Raw: raw,
	})
	r.resetProcess()
}

func (r *acpCaptureReader) outgoing(lineIndex int, msg acpRPCMessage, raw json.RawMessage, ts time.Time) {
	switch {
	case msg.Method == acpMethodPrompt && msg.idKey() != "":
		r.closeRun()
		r.turnAssistant = nil
		r.prompts[msg.idKey()] = true
		params := kiroRawObject(msg.Params)
		r.noteSessionID(kiroStringField(params, "sessionId"))
		r.append(&Entry{
			UUID:      acpLineEntryID(lineIndex, "prompt"),
			Type:      "user",
			Timestamp: ts,
			Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(acpFlattenPrompt(firstKiroRawField(params, "prompt")))}),
			Raw:       raw,
		})
	case msg.isResponse():
		perm, ok := r.permissions[msg.idKey()]
		if !ok {
			return
		}
		delete(r.permissions, msg.idKey())
		r.closeRun()
		state, action := "dismissed", ""
		if msg.Error == nil {
			outcome := kiroRawObject(firstKiroRawField(kiroRawObject(msg.Result), "outcome"))
			if kiroStringField(outcome, "outcome") == "selected" {
				state, action = "resolved", kiroStringField(outcome, "optionId")
			}
		}
		r.append(&Entry{
			UUID:      acpLineEntryID(lineIndex, "permission-outcome"),
			Type:      "system",
			Timestamp: ts,
			Message: kiroMessageWithBlocks("system", []ContentBlock{{
				Type:      "interaction",
				RequestID: perm.requestID,
				Kind:      "approval",
				State:     state,
				Action:    action,
			}}),
			Raw: raw,
		})
	}
}

func (r *acpCaptureReader) incoming(lineIndex int, msg acpRPCMessage, raw json.RawMessage, ts time.Time) {
	switch {
	case msg.Method == acpMethodUpdate:
		r.update(lineIndex, msg, raw, ts)
	case msg.Method == acpMethodRequestPermission && msg.idKey() != "":
		r.permissionRequest(lineIndex, msg, raw, ts)
	case msg.isResponse():
		if r.prompts[msg.idKey()] {
			delete(r.prompts, msg.idKey())
			r.turnEnd(lineIndex, msg, raw, ts)
			return
		}
		if result := kiroRawObject(msg.Result); len(result) > 0 {
			r.noteSessionID(kiroStringField(result, "sessionId"))
		}
	}
}

func (r *acpCaptureReader) update(lineIndex int, msg acpRPCMessage, raw json.RawMessage, ts time.Time) {
	params := kiroRawObject(msg.Params)
	r.noteSessionID(kiroStringField(params, "sessionId"))
	update := kiroRawObject(firstKiroRawField(params, "update"))
	if len(update) == 0 {
		return
	}
	switch kiroNormalizeUpdateType(kiroStringField(update, "sessionUpdate")) {
	case "agentmessagechunk":
		r.chunk(lineIndex, acpRunMessage, update, raw, ts)
	case "agentthoughtchunk":
		r.chunk(lineIndex, acpRunThought, update, raw, ts)
	case "usermessagechunk":
		// gc already recorded the prompt it sent; an agent echoing it back
		// while that prompt is in flight is not a second user message.
		if len(r.prompts) > 0 {
			return
		}
		r.chunk(lineIndex, acpRunUser, update, raw, ts)
	case "toolcall":
		r.toolCall(lineIndex, update, raw, ts)
	case "toolcallupdate":
		r.toolCallUpdate(lineIndex, update, raw, ts)
	}
}

func (r *acpCaptureReader) chunk(lineIndex int, kind string, update map[string]json.RawMessage, raw json.RawMessage, ts time.Time) {
	content := kiroRawObject(firstKiroRawField(update, "content"))
	if kiroStringField(content, "type") != "text" {
		return
	}
	text := jsonStringValue(content["text"]) // untrimmed: chunk boundaries fall mid-sentence
	messageID := kiroStringField(update, "messageId")
	if r.run != nil && r.run.kind == kind && r.run.messageID == messageID {
		r.run.text.WriteString(text)
		r.run.raws = append(r.run.raws, raw)
		return
	}
	r.closeRun()
	id := acpLineEntryID(lineIndex, kind)
	if messageID != "" {
		key := kind + "\x00" + messageID
		id = acpMessageEntryID(kind, messageID, r.messageIDSeen[key])
		r.messageIDSeen[key]++
	}
	entryType := "assistant"
	if kind == acpRunUser {
		entryType = "user"
	}
	run := &acpChunkRun{kind: kind, messageID: messageID, entry: &Entry{UUID: id, Type: entryType, Timestamp: ts}}
	run.text.WriteString(text)
	run.raws = append(run.raws, raw)
	r.run = run
	r.append(run.entry)
}

// closeRun finalizes the open chunk run's message and raw payload.
func (r *acpCaptureReader) closeRun() {
	run := r.run
	if run == nil {
		return
	}
	r.run = nil
	text := run.text.String()
	switch run.kind {
	case acpRunUser:
		run.entry.Message = mustMarshal(MessageContent{Role: "user", Content: mustMarshal(text)})
	case acpRunThought:
		run.entry.Message = kiroMessageWithBlocks("assistant", []ContentBlock{{Type: "thinking", Thinking: text}})
	default:
		run.entry.Message = kiroMessageWithBlocks("assistant", []ContentBlock{{Type: "text", Text: text}})
	}
	if len(run.raws) == 1 {
		run.entry.Raw = run.raws[0]
		return
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, raw := range run.raws {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')
	run.entry.Raw = buf.Bytes()
}

func (r *acpCaptureReader) toolCall(lineIndex int, update map[string]json.RawMessage, raw json.RawMessage, ts time.Time) {
	callID := kiroStringField(update, "toolCallId")
	if callID == "" {
		return
	}
	r.closeRun()
	name := firstNonEmpty(kiroStringField(update, "title"), kiroStringField(update, "kind"), "tool")
	input := kiroNeutralToolInput(name, firstKiroRawField(update, "rawInput"))
	entry := &Entry{
		UUID:      acpLineEntryID(lineIndex, "tool-use"),
		Type:      "assistant",
		Timestamp: ts,
		Message:   acpToolUseMessage(callID, name, input),
		Raw:       raw,
	}
	state := &acpToolState{entry: entry, name: name, input: input}
	if _, known := r.tools[callID]; !known {
		r.toolOrder = append(r.toolOrder, callID)
	}
	r.tools[callID] = state
	r.append(entry)
	r.toolResult(lineIndex, callID, state, update, raw, ts)
}

func (r *acpCaptureReader) toolCallUpdate(lineIndex int, update map[string]json.RawMessage, raw json.RawMessage, ts time.Time) {
	callID := kiroStringField(update, "toolCallId")
	if callID == "" {
		return
	}
	state := r.tools[callID]
	if state != nil && !state.terminal {
		// Agents often send the tool's input or a better title after the
		// initial tool_call; fold them into the tool_use entry.
		name := firstNonEmpty(kiroStringField(update, "title"), state.name)
		input := state.input
		if rawInput := firstKiroRawField(update, "rawInput"); len(rawInput) > 0 && string(rawInput) != "null" {
			input = kiroNeutralToolInput(name, rawInput)
		}
		if name != state.name || !bytes.Equal(input, state.input) {
			state.name, state.input = name, input
			state.entry.Message = acpToolUseMessage(callID, name, input)
		}
	}
	r.toolResult(lineIndex, callID, state, update, raw, ts)
}

// toolResult emits the result entry when update carries a terminal status.
func (r *acpCaptureReader) toolResult(lineIndex int, callID string, state *acpToolState, update map[string]json.RawMessage, raw json.RawMessage, ts time.Time) {
	status := strings.ToLower(kiroStringField(update, "status"))
	if status != "completed" && status != "failed" {
		return
	}
	if state != nil {
		if state.terminal {
			return
		}
		state.terminal = true
	}
	r.closeRun()
	content := kiroToolUpdateResultContent(update)
	if len(content) == 0 {
		content = mustMarshal(map[string]string{"status": status})
	}
	name := ""
	if state != nil {
		name = state.name
	}
	r.append(&Entry{
		UUID:      acpLineEntryID(lineIndex, "tool-result"),
		Type:      "tool_result",
		Timestamp: ts,
		ToolUseID: callID,
		Message: kiroMessageWithBlocks("tool", []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: callID,
			Name:      name,
			Content:   content,
			IsError:   kiroToolUpdateIsError(update, content),
		}}),
		Raw: raw,
	})
}

func (r *acpCaptureReader) permissionRequest(lineIndex int, msg acpRPCMessage, raw json.RawMessage, ts time.Time) {
	r.closeRun()
	params := kiroRawObject(msg.Params)
	toolCall := kiroRawObject(firstKiroRawField(params, "toolCall"))
	var options []string
	for _, rawOption := range kiroRawArray(firstKiroRawField(params, "options")) {
		if id := kiroStringField(kiroRawObject(rawOption), "optionId"); id != "" {
			options = append(options, id)
		}
	}
	entryID := acpLineEntryID(lineIndex, "permission")
	var metadata json.RawMessage
	if callID := kiroStringField(toolCall, "toolCallId"); callID != "" {
		metadata = mustMarshal(map[string]string{"tool_call_id": callID})
	}
	r.permissions[msg.idKey()] = acpPermission{requestID: entryID, line: lineIndex}
	r.append(&Entry{
		UUID:      entryID,
		Type:      "assistant",
		Timestamp: ts,
		Message: kiroMessageWithBlocks("assistant", []ContentBlock{{
			Type:      "interaction",
			RequestID: entryID,
			Kind:      "approval",
			State:     "pending",
			Prompt:    firstNonEmpty(kiroStringField(toolCall, "title"), "Permission requested"),
			Options:   options,
			Metadata:  metadata,
		}}),
		Raw: raw,
	})
}

// turnEnd handles the response to a session/prompt: it stamps the stop reason
// and usage on the turn's last assistant entry and records turns that did not
// end normally as system entries.
func (r *acpCaptureReader) turnEnd(lineIndex int, msg acpRPCMessage, raw json.RawMessage, ts time.Time) {
	r.closeRun()
	defer func() { r.turnAssistant = nil }()
	if msg.Error != nil {
		message := firstNonEmpty(strings.TrimSpace(msg.Error.Message), "Agent returned an error")
		r.append(&Entry{
			UUID:      acpLineEntryID(lineIndex, "turn-error"),
			Type:      "system",
			Timestamp: ts,
			Message:   kiroMessageWithBlocks("system", []ContentBlock{{Type: "text", Text: message}}),
			SystemEvent: &SystemEvent{
				Kind:     "error",
				Category: "provider_error",
				Code:     strings.TrimSpace(string(msg.Error.Code)),
				Message:  message,
			},
			Raw: raw,
		})
		return
	}
	result := kiroRawObject(msg.Result)
	stopReason := kiroStringField(result, "stopReason")
	if r.turnAssistant != nil {
		r.turnAssistant.Message = acpStampTurnEnd(r.turnAssistant.Message, stopReason, acpUsage(firstKiroRawField(result, "usage")))
	}
	if stopReason == "" || stopReason == acpStopReasonEndTurn {
		return
	}
	kind, text := "result", "Turn stopped: "+stopReason
	if stopReason == acpStopReasonCancelled {
		kind, text = "turn_aborted", "Turn canceled"
	}
	r.append(&Entry{
		UUID:      acpLineEntryID(lineIndex, "turn-end"),
		Type:      "system",
		Timestamp: ts,
		Message:   kiroMessageWithBlocks("system", []ContentBlock{{Type: "text", Text: text}}),
		SystemEvent: &SystemEvent{
			Kind:     kind,
			Category: stopReason,
			Message:  text,
		},
		Raw: raw,
	})
}

func (r *acpCaptureReader) append(entry *Entry) {
	if n := len(r.messages); n > 0 {
		entry.ParentUUID = r.messages[n-1].UUID
	}
	entry.SessionID = r.sessionID
	if entry.Type == "assistant" {
		r.turnAssistant = entry
	}
	r.messages = append(r.messages, entry)
}

// noteSessionID records the first ACP session id the agent reported.
func (r *acpCaptureReader) noteSessionID(id string) {
	if r.sessionID == "" {
		r.sessionID = strings.TrimSpace(id)
	}
}

func (r *acpCaptureReader) finish(path string) *Session {
	r.closeRun()
	var orphans map[string]bool
	for _, callID := range r.toolOrder {
		if state := r.tools[callID]; state != nil && !state.terminal {
			if orphans == nil {
				orphans = make(map[string]bool)
			}
			orphans[callID] = true
		}
	}
	id := firstNonEmpty(r.sessionID, r.gcSessionID)
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &Session{
		ID:                 id,
		Messages:           r.messages,
		OrphanedToolUseIDs: orphans,
		Diagnostics:        r.diagnostics,
	}
}

func acpToolUseMessage(callID, name string, input json.RawMessage) json.RawMessage {
	return kiroMessageWithBlocks("assistant", []ContentBlock{{
		Type:  "tool_use",
		ID:    callID,
		Name:  name,
		Input: input,
	}})
}

// acpFlattenPrompt renders ACP prompt content blocks as the user's text.
// Embedded resources contribute their text (or URI), links their URI.
func acpFlattenPrompt(raw json.RawMessage) string {
	var parts []string
	for _, rawBlock := range kiroRawArray(raw) {
		block := kiroRawObject(rawBlock)
		switch kiroStringField(block, "type") {
		case "text":
			if text := jsonStringValue(block["text"]); text != "" {
				parts = append(parts, text)
			}
		case "resource_link":
			if link := firstNonEmpty(kiroStringField(block, "uri"), kiroStringField(block, "name")); link != "" {
				parts = append(parts, link)
			}
		case "resource":
			resource := kiroRawObject(firstKiroRawField(block, "resource"))
			if text := firstNonEmpty(jsonStringValue(resource["text"]), kiroStringField(resource, "uri")); text != "" {
				parts = append(parts, text)
			}
		case "image":
			parts = append(parts, "[image]")
		case "audio":
			parts = append(parts, "[audio]")
		}
	}
	return strings.Join(parts, "\n\n")
}

// acpUsage translates the (unstable) ACP prompt-response usage object to the
// snake_case token keys transcript consumers read. It returns nil when the
// agent reported none.
func acpUsage(raw json.RawMessage) map[string]int {
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return nil
	}
	fields := []struct{ from, to string }{
		{"inputTokens", "input_tokens"},
		{"outputTokens", "output_tokens"},
		{"thoughtTokens", "reasoning_tokens"},
		{"cachedReadTokens", "cache_read_input_tokens"},
		{"cachedWriteTokens", "cache_creation_input_tokens"},
	}
	usage := make(map[string]int)
	for _, field := range fields {
		if value := kiroIntField(object, field.from); value != nil && *value > 0 {
			usage[field.to] = *value
		}
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

// acpStampTurnEnd adds stop_reason and usage to an entry's message object.
func acpStampTurnEnd(message json.RawMessage, stopReason string, usage map[string]int) json.RawMessage {
	if stopReason == "" && usage == nil {
		return message
	}
	object := kiroRawObject(message)
	if object == nil {
		return message
	}
	if stopReason != "" {
		object["stop_reason"] = mustMarshal(stopReason)
	}
	if usage != nil {
		object["usage"] = mustMarshal(usage)
	}
	return mustMarshal(object)
}

// acpLineEntryID names an entry after the capture line that opened it. Line
// positions never move in an append-only file.
func acpLineEntryID(lineIndex int, part string) string {
	return "acp-l" + strconv.Itoa(lineIndex) + "-" + part
}

// acpMessageEntryID names a reassembled entry after its ACP messageId; the
// occurrence count separates runs that reuse one id.
func acpMessageEntryID(kind, messageID string, occurrence int) string {
	sum := sha256.Sum256([]byte(messageID))
	id := "acp-" + kind + "-" + hex.EncodeToString(sum[:12])
	if occurrence > 0 {
		id += "-" + strconv.Itoa(occurrence+1)
	}
	return id
}

func parseACPTimestamp(value string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return ts
}

// sortedPermissionKeys orders open permission requests by the line that
// opened them, so restart dismissals are deterministic.
func sortedPermissionKeys(permissions map[string]acpPermission) []string {
	keys := make([]string, 0, len(permissions))
	for key := range permissions {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return permissions[keys[i]].line < permissions[keys[j]].line })
	return keys
}

// extractACPCaptureTailMeta derives tail activity from a capture: "in-turn"
// while the last session/prompt gc sent has no response, "idle" once it has
// one or the agent restarted, "" when the file holds no header. It scans
// backward from the end and widens the window until it reaches a deciding
// record, so a long turn does not hide its prompt.
func extractACPCaptureTailMeta(f io.ReadSeeker) (*TailMeta, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	for window := int64(tailChunkSize); ; window *= 4 {
		data, startsMidLine, truncated, err := readTailWindowAt(f, size, window)
		if err != nil {
			return nil, err
		}
		lines := bytes.Split(data, []byte{'\n'})
		if startsMidLine && len(lines) > 0 {
			lines = lines[1:]
		}
		meta, decided := acpCaptureActivity(lines)
		if decided || !truncated {
			return meta, nil
		}
	}
}

func acpCaptureActivity(lines [][]byte) (*TailMeta, bool) {
	meta := &TailMeta{}
	answered := make(map[string]bool)
	sawLine := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		rec, msg, ok := decodeACPCaptureLine(line)
		if !ok {
			if !sawLine {
				meta.MalformedTail = true
			}
			sawLine = true
			continue
		}
		sawLine = true
		if rec.isHeader() {
			meta.Activity = "idle"
			return meta, true
		}
		switch {
		case rec.Dir == acpDirIn && msg.isResponse():
			answered[msg.idKey()] = true
		case rec.Dir == acpDirOut && msg.Method == acpMethodPrompt && msg.idKey() != "":
			if answered[msg.idKey()] {
				meta.Activity = "idle"
			} else {
				meta.Activity = "in-turn"
			}
			return meta, true
		}
	}
	return meta, false
}

// acpCaptureFamily is the reader family of a gc ACP capture file. It is not a
// provider family: ProviderFamily never returns it.
const acpCaptureFamily = "gc-acp-capture"

// transcriptFamily picks the reader for a transcript file: a gc ACP capture
// by location, otherwise the provider's family.
func transcriptFamily(provider, path string) string {
	if IsACPCapturePath(path) {
		return acpCaptureFamily
	}
	return ProviderFamily(provider)
}
