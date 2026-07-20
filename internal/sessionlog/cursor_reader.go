package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReadCursorFile reads a Cursor hook/stream JSONL capture and converts it to
// the standard Session format used by GC session logs.
func ReadCursorFile(path string, _ int) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	var messages []*Entry
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool
	sessionID := ""
	lastUUID := ""
	toolNames := make(map[string]string)
	sawPartialAssistant := false
	syntheticIDs := newStableSyntheticEntryIDSequence("cursor")

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rawLine := append(json.RawMessage(nil), line...)
		var event cursorEvent
		if err := json.Unmarshal(line, &event); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if sessionID == "" {
			sessionID = cursorSessionIDFromEvent(event)
		}
		if cursorShouldSkipAssistantFrame(event, &sawPartialAssistant) {
			continue
		}

		recordIDs := syntheticIDs.ForRecord(rawLine)
		entries := cursorEntriesFromEvent(event, rawLine, toolNames)
		assignCursorRecordSyntheticEntryIDs(event, rawLine, recordIDs, entries)
		for _, entry := range entries {
			if entry == nil {
				continue
			}
			entry.RawRecordID = recordIDs.RawRecordID()
			entry.ParentUUID = lastUUID
			lastUUID = entry.UUID
			messages = append(messages, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning cursor hook capture: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &Session{
		ID:          sessionID,
		Messages:    messages,
		Diagnostics: diagnostics,
	}, nil
}

type cursorEvent struct {
	ID                json.RawMessage `json:"id"`
	HookEventName     string          `json:"hook_event_name"`
	EventName         string          `json:"event_name"`
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	ConversationID    string          `json:"conversation_id"`
	SessionID         string          `json:"session_id"`
	SessionIDCamel    string          `json:"sessionId"`
	GenerationID      string          `json:"generation_id"`
	GenerationIDCamel string          `json:"generationId"`
	CallID            string          `json:"call_id"`
	CallIDCamel       string          `json:"callId"`
	ToolCallID        string          `json:"tool_call_id"`
	ToolCallIDCamel   string          `json:"toolCallId"`
	ToolUseID         string          `json:"tool_use_id"`
	ToolUseIDCamel    string          `json:"toolUseId"`
	ModelCallID       string          `json:"model_call_id"`
	Timestamp         string          `json:"timestamp"`
	CreatedAt         string          `json:"created_at"`
	TimestampMS       *int64          `json:"timestamp_ms"`
	TimestampMSCamel  *int64          `json:"timestampMs"`
	Model             string          `json:"model"`
	Prompt            string          `json:"prompt"`
	Text              string          `json:"text"`
	Response          string          `json:"response"`
	Command           string          `json:"command"`
	CWD               string          `json:"cwd"`
	WorkingDir        string          `json:"working_dir"`
	WorkingDirCamel   string          `json:"workingDir"`
	FilePath          string          `json:"file_path"`
	FilePathCamel     string          `json:"filePath"`
	Path              string          `json:"path"`
	OldText           string          `json:"old_text"`
	OldTextCamel      string          `json:"oldText"`
	OldString         string          `json:"old_string"`
	OldStringCamel    string          `json:"oldString"`
	NewText           string          `json:"new_text"`
	NewTextCamel      string          `json:"newText"`
	NewString         string          `json:"new_string"`
	NewStringCamel    string          `json:"newString"`
	Stdout            string          `json:"stdout"`
	Stderr            string          `json:"stderr"`
	Output            json.RawMessage `json:"output"`
	Result            json.RawMessage `json:"result"`
	Content           json.RawMessage `json:"content"`
	Message           json.RawMessage `json:"message"`
	Input             json.RawMessage `json:"input"`
	Args              json.RawMessage `json:"args"`
	Arguments         json.RawMessage `json:"arguments"`
	ToolCall          json.RawMessage `json:"tool_call"`
	ToolCallCamel     json.RawMessage `json:"toolCall"`
	ToolName          string          `json:"tool_name"`
	ToolNameCamel     string          `json:"toolName"`
	Name              string          `json:"name"`
	ExitCode          *int            `json:"exit_code"`
	ExitCodeCamel     *int            `json:"exitCode"`
	IsError           bool            `json:"is_error"`
	IsErrorCamel      bool            `json:"isError"`
	Success           *bool           `json:"success"`
}

func cursorEntriesFromEvent(event cursorEvent, rawLine json.RawMessage, toolNames map[string]string) []*Entry {
	switch cursorFrameType(event) {
	case "user":
		if entry := cursorMessageEntry(event, rawLine, "user"); entry != nil {
			return []*Entry{entry}
		}
	case "assistant":
		if entry := cursorMessageEntry(event, rawLine, "assistant"); entry != nil {
			return []*Entry{entry}
		}
	case "toolcall":
		return cursorToolCallEntries(event, rawLine, toolNames)
	case "result":
		if entry := cursorResultEntry(event, rawLine); entry != nil {
			return []*Entry{entry}
		}
	case "system":
		return nil
	}

	switch cursorEventKind(event) {
	case "beforesubmitprompt", "promptsubmit", "userprompt":
		if text := strings.TrimSpace(event.Prompt); text != "" {
			return []*Entry{cursorTextEntry(event, rawLine, "user", text)}
		}
	case "afteragentresponse", "agentresponse", "assistantmessage":
		if text := cursorEventText(event); text != "" {
			return []*Entry{cursorTextEntry(event, rawLine, "assistant", text)}
		}
	case "beforeshellexecution":
		callID := cursorToolCallID(event, rawLine)
		toolNames[callID] = "shell"
		return []*Entry{cursorToolUseEntry(event, rawLine, callID, "shell", cursorShellInput(event))}
	case "aftershellexecution":
		callID := cursorToolCallID(event, rawLine)
		return []*Entry{cursorToolResultEntry(event, rawLine, callID, firstNonEmpty(toolNames[callID], "shell"), cursorShellResult(event), cursorEventIsError(event))}
	case "afterfileedit":
		callID := cursorToolCallID(event, rawLine)
		toolNames[callID] = "edit"
		return []*Entry{
			cursorToolUseEntry(event, rawLine, callID, "edit", cursorEditInput(event)),
			cursorToolResultEntry(event, rawLine, callID, "edit", cursorEditResult(event), cursorEventIsError(event)),
		}
	case "pretooluse", "beforetooluse", "beforemcpexecution":
		callID := cursorToolCallID(event, rawLine)
		name := cursorToolName(event)
		toolNames[callID] = name
		return []*Entry{cursorToolUseEntry(event, rawLine, callID, name, cursorGenericToolInput(event))}
	case "posttooluse", "aftertooluse", "posttoolusefailure", "aftermcpexecution":
		callID := cursorToolCallID(event, rawLine)
		name := firstNonEmpty(cursorToolName(event), toolNames[callID], "tool")
		return []*Entry{cursorToolResultEntry(event, rawLine, callID, name, cursorGenericToolResult(event), cursorEventIsError(event))}
	}
	return nil
}

func cursorMessageEntry(event cursorEvent, rawLine json.RawMessage, role string) *Entry {
	message := cloneRawJSON(event.Message)
	if len(message) == 0 || string(message) == "null" {
		if text := cursorEventText(event); text != "" {
			message = mustMarshal(MessageContent{Role: role, Content: mustMarshal(text)})
		}
	}
	if len(message) == 0 || string(message) == "null" {
		return nil
	}
	return &Entry{
		UUID:      cursorEntryID(event, rawLine, ""),
		Type:      role,
		Timestamp: cursorEventTimestamp(event),
		SessionID: cursorSessionIDFromEvent(event),
		Message:   message,
		Raw:       rawLine,
	}
}

func cursorTextEntry(event cursorEvent, rawLine json.RawMessage, role, text string) *Entry {
	return &Entry{
		UUID:      cursorEntryID(event, rawLine, ""),
		Type:      role,
		Timestamp: cursorEventTimestamp(event),
		SessionID: cursorSessionIDFromEvent(event),
		Message:   mustMarshal(MessageContent{Role: role, Content: mustMarshal(text)}),
		Raw:       rawLine,
	}
}

type cursorToolCall struct {
	ID     string
	Name   string
	Args   json.RawMessage
	Result json.RawMessage
}

func cursorToolCallEntries(event cursorEvent, rawLine json.RawMessage, toolNames map[string]string) []*Entry {
	call, ok := cursorToolCallFromEvent(event, rawLine)
	if !ok {
		return nil
	}
	switch cursorFrameSubtype(event) {
	case "started", "start", "pending":
		toolNames[call.ID] = call.Name
		return []*Entry{cursorToolUseEntry(event, rawLine, call.ID, call.Name, cursorToolCallInput(call))}
	case "completed", "complete", "success", "failed", "error":
		name := firstNonEmpty(call.Name, toolNames[call.ID], "tool")
		content := cursorToolCallResult(call)
		return []*Entry{cursorToolResultEntry(event, rawLine, call.ID, name, content, cursorEventIsError(event) || cursorToolCallResultIsError(call, content))}
	default:
		if len(call.Result) > 0 {
			name := firstNonEmpty(call.Name, toolNames[call.ID], "tool")
			content := cursorToolCallResult(call)
			return []*Entry{cursorToolResultEntry(event, rawLine, call.ID, name, content, cursorEventIsError(event) || cursorToolCallResultIsError(call, content))}
		}
		toolNames[call.ID] = call.Name
		return []*Entry{cursorToolUseEntry(event, rawLine, call.ID, call.Name, cursorToolCallInput(call))}
	}
}

func cursorToolCallFromEvent(event cursorEvent, rawLine json.RawMessage) (cursorToolCall, bool) {
	raw := firstNonNilRaw(event.ToolCall, event.ToolCallCamel)
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return cursorToolCall{}, false
	}
	for _, candidate := range []struct {
		key  string
		name string
	}{
		{key: "readToolCall", name: "Read"},
		{key: "writeToolCall", name: "Write"},
		{key: "editToolCall", name: "Edit"},
		{key: "deleteToolCall", name: "Delete"},
	} {
		if call := kiroRawObject(firstKiroRawField(object, candidate.key)); len(call) > 0 {
			nativeID := firstNonEmpty(
				kiroStringField(call, "toolCallId", "tool_call_id", "callId", "call_id", "id"),
				cursorNativeEntryID(event),
			)
			id := nativeID
			if id == "" {
				id = cursorToolCallID(event, rawLine)
			}
			return cursorToolCall{
				ID:     id,
				Name:   candidate.name,
				Args:   firstKiroRawField(call, "args", "arguments", "input"),
				Result: firstKiroRawField(call, "result", "output"),
			}, true
		}
	}
	if call := kiroRawObject(firstKiroRawField(object, "function")); len(call) > 0 {
		nativeID := firstNonEmpty(
			kiroStringField(call, "toolCallId", "tool_call_id", "callId", "call_id", "id"),
			cursorNativeEntryID(event),
		)
		id := nativeID
		if id == "" {
			id = cursorToolCallID(event, rawLine)
		}
		return cursorToolCall{
			ID:     id,
			Name:   firstNonEmpty(kiroStringField(call, "name"), "tool"),
			Args:   firstKiroRawField(call, "arguments", "args", "input"),
			Result: firstKiroRawField(call, "result", "output"),
		}, true
	}
	return cursorToolCall{}, false
}

func cursorToolCallInput(call cursorToolCall) json.RawMessage {
	switch strings.ToLower(strings.TrimSpace(call.Name)) {
	case "read":
		neutral := make(map[string]json.RawMessage)
		if filePath := kiroStringField(kiroRawObject(call.Args), "path", "file_path", "filePath", "file"); filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
		if len(neutral) > 0 {
			return mustMarshal(neutral)
		}
	case "write":
		neutral := make(map[string]json.RawMessage)
		args := kiroRawObject(call.Args)
		if filePath := kiroStringField(args, "path", "file_path", "filePath", "file"); filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
		if content := cursorStringField(args, "fileText", "file_text", "content", "text"); content != "" {
			neutral["content"] = mustMarshal(content)
		}
		if len(neutral) > 0 {
			return mustMarshal(neutral)
		}
	}
	return copilotNeutralObject(call.Args, cursorNeutralInputKey, call.Name)
}

func cursorToolCallResult(call cursorToolCall) json.RawMessage {
	success := cursorSuccessPayload(call.Result)
	switch strings.ToLower(strings.TrimSpace(call.Name)) {
	case "read":
		return cursorReadResult(call.Args, success)
	case "write":
		return cursorWriteResult(call.Args, success)
	default:
		if len(success) > 0 {
			return copilotNeutralObject(success, cursorNeutralResultKey, "")
		}
		return copilotNeutralObject(call.Result, cursorNeutralResultKey, "")
	}
}

func cursorToolCallResultIsError(call cursorToolCall, content json.RawMessage) bool {
	resultObject := kiroRawObject(call.Result)
	if len(resultObject) > 0 {
		if raw := firstKiroRawField(resultObject, "error", "failure"); len(raw) > 0 && string(raw) != "null" {
			return true
		}
	}
	contentObject := kiroRawObject(content)
	if len(contentObject) == 0 {
		return false
	}
	if kiroBoolField(contentObject, "is_error", "isError") {
		return true
	}
	if exitCode := kiroIntField(contentObject, "exit_code", "exitCode"); exitCode != nil && *exitCode != 0 {
		return true
	}
	return false
}

func cursorReadResult(args, success json.RawMessage) json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	argObject := kiroRawObject(args)
	successObject := kiroRawObject(success)
	if filePath := firstNonEmpty(
		kiroStringField(argObject, "path", "file_path", "filePath", "file"),
		kiroStringField(successObject, "path", "file_path", "filePath", "file"),
	); filePath != "" {
		neutral["file_path"] = mustMarshal(filePath)
	}
	if content := cursorStringField(successObject, "content", "text"); content != "" {
		neutral["content"] = mustMarshal(content)
	}
	if totalLines := kiroIntField(successObject, "totalLines", "total_lines", "numLines", "num_lines"); totalLines != nil {
		neutral["total_lines"] = mustMarshal(*totalLines)
	}
	if totalChars := kiroIntField(successObject, "totalChars", "total_chars", "bytes"); totalChars != nil {
		neutral["bytes"] = mustMarshal(*totalChars)
	}
	if exceeded := kiroBoolField(successObject, "exceededLimit", "exceeded_limit", "truncated"); exceeded {
		neutral["truncated"] = mustMarshal(true)
	}
	if len(neutral) == 0 {
		return copilotNeutralObject(success, cursorNeutralResultKey, "")
	}
	return mustMarshal(neutral)
}

func cursorWriteResult(args, success json.RawMessage) json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	argObject := kiroRawObject(args)
	successObject := kiroRawObject(success)
	if filePath := firstNonEmpty(
		kiroStringField(successObject, "path", "file_path", "filePath", "file"),
		kiroStringField(argObject, "path", "file_path", "filePath", "file"),
	); filePath != "" {
		neutral["file_path"] = mustMarshal(filePath)
	}
	if content := cursorStringField(argObject, "fileText", "file_text", "content", "text"); content != "" {
		neutral["content"] = mustMarshal(content)
	}
	if linesCreated := kiroIntField(successObject, "linesCreated", "lines_created", "numLines", "num_lines"); linesCreated != nil {
		neutral["num_lines"] = mustMarshal(*linesCreated)
	}
	if fileSize := kiroIntField(successObject, "fileSize", "file_size", "bytes"); fileSize != nil {
		neutral["bytes"] = mustMarshal(*fileSize)
	}
	if len(neutral) == 0 {
		return copilotNeutralObject(success, cursorNeutralResultKey, "")
	}
	return mustMarshal(neutral)
}

func cursorSuccessPayload(raw json.RawMessage) json.RawMessage {
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return cloneRawJSON(raw)
	}
	if success := firstKiroRawField(object, "success"); len(success) > 0 && string(success) != "null" {
		return success
	}
	if errorRaw := firstKiroRawField(object, "error", "failure"); len(errorRaw) > 0 && string(errorRaw) != "null" {
		return errorRaw
	}
	return cloneRawJSON(raw)
}

func cursorResultEntry(event cursorEvent, rawLine json.RawMessage) *Entry {
	message := firstNonEmpty(jsonStringValue(event.Result), cursorEventText(event))
	if message == "" && event.Subtype == "" {
		return nil
	}
	return &Entry{
		UUID:      cursorEntryID(event, rawLine, "result"),
		Type:      "system",
		Subtype:   "result",
		Timestamp: cursorEventTimestamp(event),
		SessionID: cursorSessionIDFromEvent(event),
		Message:   mustMarshal(MessageContent{Role: "system", Content: mustMarshal(message)}),
		SystemEvent: &SystemEvent{
			Kind:     "result",
			Category: strings.TrimSpace(event.Subtype),
			Message:  message,
		},
		Raw: rawLine,
	}
}

func cursorToolUseEntry(event cursorEvent, rawLine json.RawMessage, callID, name string, input json.RawMessage) *Entry {
	return &Entry{
		UUID:      cursorEntryID(event, rawLine, "use"),
		Type:      "assistant",
		Timestamp: cursorEventTimestamp(event),
		SessionID: cursorSessionIDFromEvent(event),
		Message: kiroMessageWithBlocks("assistant", []ContentBlock{{
			Type:  "tool_use",
			ID:    callID,
			Name:  name,
			Input: input,
		}}),
		Raw: rawLine,
	}
}

func cursorToolResultEntry(event cursorEvent, rawLine json.RawMessage, callID, name string, content json.RawMessage, isError bool) *Entry {
	return &Entry{
		UUID:      cursorEntryID(event, rawLine, "result"),
		Type:      "tool_result",
		Timestamp: cursorEventTimestamp(event),
		SessionID: cursorSessionIDFromEvent(event),
		ToolUseID: callID,
		Message: kiroMessageWithBlocks("tool", []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: callID,
			Name:      name,
			Content:   content,
			IsError:   isError,
		}}),
		Raw: rawLine,
	}
}

func cursorShellInput(event cursorEvent) json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	if command := strings.TrimSpace(event.Command); command != "" {
		neutral["command"] = mustMarshal(command)
	}
	if workingDir := cursorWorkingDir(event); workingDir != "" {
		neutral["working_dir"] = mustMarshal(workingDir)
	}
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func cursorShellResult(event cursorEvent) json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	if command := strings.TrimSpace(event.Command); command != "" {
		neutral["command"] = mustMarshal(command)
	}
	if strings.TrimSpace(event.Stdout) != "" {
		neutral["stdout"] = mustMarshal(event.Stdout)
	}
	if strings.TrimSpace(event.Stderr) != "" {
		neutral["stderr"] = mustMarshal(event.Stderr)
	}
	if exitCode := cursorExitCode(event); exitCode != nil {
		neutral["exit_code"] = mustMarshal(*exitCode)
	}
	if len(neutral) == 0 {
		return cursorGenericToolResult(event)
	}
	return mustMarshal(neutral)
}

func cursorEditInput(event cursorEvent) json.RawMessage {
	neutral := cursorEditFields(event)
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func cursorEditResult(event cursorEvent) json.RawMessage {
	neutral := cursorEditFields(event)
	filePath := jsonStringValue(neutral["file_path"])
	oldText := jsonStringValue(neutral["old_string"])
	newText := jsonStringValue(neutral["new_string"])
	if oldText != "" || newText != "" {
		neutral["patch"] = mustMarshal(kiroBuildUnifiedPatch(filePath, oldText, newText))
	}
	if len(neutral) == 0 {
		return cursorGenericToolResult(event)
	}
	return mustMarshal(neutral)
}

func cursorEditFields(event cursorEvent) map[string]json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	if filePath := cursorFilePath(event); filePath != "" {
		neutral["file_path"] = mustMarshal(filePath)
	}
	if oldText := firstNonEmpty(event.OldText, event.OldTextCamel, event.OldString, event.OldStringCamel); oldText != "" {
		neutral["old_string"] = mustMarshal(oldText)
	}
	if newText := firstNonEmpty(event.NewText, event.NewTextCamel, event.NewString, event.NewStringCamel); newText != "" {
		neutral["new_string"] = mustMarshal(newText)
	}
	return neutral
}

func cursorGenericToolInput(event cursorEvent) json.RawMessage {
	return copilotNeutralObject(firstNonNilRaw(event.Input, event.Args, event.Arguments, event.Content), cursorNeutralInputKey, cursorToolName(event))
}

func cursorGenericToolResult(event cursorEvent) json.RawMessage {
	return copilotNeutralObject(firstNonNilRaw(event.Result, event.Output, event.Content, event.Message), cursorNeutralResultKey, "")
}

func cursorNeutralInputKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "hook_event_name", "hookeventname", "event_name", "eventname", "conversation_id", "conversationid", "session_id", "sessionid", "generation_id", "generationid":
		return ""
	case "filetext", "file_text":
		return "content"
	default:
		return copilotNeutralInputKey(key)
	}
}

func cursorNeutralResultKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "hook_event_name", "hookeventname", "event_name", "eventname", "conversation_id", "conversationid", "session_id", "sessionid", "generation_id", "generationid":
		return ""
	case "oldtext", "old_text":
		return "old_string"
	case "newtext", "new_text":
		return "new_string"
	case "filetext", "file_text":
		return "content"
	case "totallines", "total_lines":
		return "total_lines"
	case "totalchars", "total_chars", "filesize", "file_size":
		return "bytes"
	case "exceededlimit", "exceeded_limit":
		return "truncated"
	case "linescreated", "lines_created":
		return "num_lines"
	default:
		return copilotNeutralResultKey(key)
	}
}

func cursorFrameType(event cursorEvent) string {
	return cursorNormalizeType(event.Type)
}

func cursorFrameSubtype(event cursorEvent) string {
	return cursorNormalizeType(event.Subtype)
}

func cursorNormalizeType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, "/", "")
	return value
}

func cursorEventKind(event cursorEvent) string {
	value := firstNonEmpty(event.HookEventName, event.EventName, event.Type)
	return cursorNormalizeType(value)
}

func cursorEventText(event cursorEvent) string {
	if text := firstNonEmpty(event.Text, event.Response); text != "" {
		return text
	}
	if text := kiroTextFromRaw(event.Content); text != "" {
		return text
	}
	return kiroTextFromRaw(event.Message)
}

func cursorNativeEntryID(event cursorEvent) string {
	if len(event.ID) > 0 {
		if value := jsonStringValue(event.ID); value != "" {
			return value
		}
	}
	return ""
}

func cursorToolCallID(event cursorEvent, rawLine json.RawMessage) string {
	if callID := firstNonEmpty(event.ModelCallID, event.CallID, event.CallIDCamel, event.ToolCallID, event.ToolCallIDCamel, event.ToolUseID, event.ToolUseIDCamel); callID != "" {
		return callID
	}
	if entryID := cursorNativeEntryID(event); entryID != "" {
		return entryID
	}
	// Cursor assigns generation IDs to every hook in one user-message
	// generation, so they cannot safely identify or correlate individual tool
	// calls. An unmatched record-local ID is preferable to a false association
	// that attaches a result to the wrong tool input.
	return stableSyntheticEntryID("cursor-tool", rawLine, "")
}

func cursorEntryID(event cursorEvent, rawLine json.RawMessage, part string) string {
	if part == "" {
		if entryID := cursorNativeEntryID(event); entryID != "" {
			return entryID
		}
	}
	return stableSyntheticEntryID("cursor", rawLine, part)
}

func assignCursorRecordSyntheticEntryIDs(event cursorEvent, rawLine json.RawMessage, syntheticIDs stableSyntheticEntryIDSource, entries []*Entry) {
	baseIDs := newStableSyntheticEntryIDSource("cursor", rawLine)
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		for _, part := range []string{"", "use", "result"} {
			if part == "" && cursorNativeEntryID(event) != "" {
				continue
			}
			if entry.UUID == baseIDs.ID(part) {
				entry.UUID = syntheticIDs.ID(part)
				break
			}
		}
	}
}

func cursorToolName(event cursorEvent) string {
	return firstNonEmpty(event.ToolName, event.ToolNameCamel, event.Name, "tool")
}

func cursorWorkingDir(event cursorEvent) string {
	return firstNonEmpty(event.CWD, event.WorkingDir, event.WorkingDirCamel)
}

func cursorFilePath(event cursorEvent) string {
	return firstNonEmpty(event.FilePath, event.FilePathCamel, event.Path)
}

func cursorExitCode(event cursorEvent) *int {
	if event.ExitCode != nil {
		return event.ExitCode
	}
	return event.ExitCodeCamel
}

func cursorEventIsError(event cursorEvent) bool {
	if event.IsError || event.IsErrorCamel {
		return true
	}
	if event.Success != nil && !*event.Success {
		return true
	}
	if exitCode := cursorExitCode(event); exitCode != nil && *exitCode != 0 {
		return true
	}
	return cursorEventKind(event) == "posttoolusefailure"
}

func cursorEventTimestamp(event cursorEvent) time.Time {
	for _, value := range []string{event.Timestamp, event.CreatedAt} {
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return ts
		}
	}
	for _, value := range []*int64{event.TimestampMS, event.TimestampMSCamel} {
		if value != nil && *value > 0 {
			return time.UnixMilli(*value).UTC()
		}
	}
	return time.Time{}
}

func cursorSessionIDFromEvent(event cursorEvent) string {
	return firstNonEmpty(event.ConversationID, event.SessionID, event.SessionIDCamel)
}

func firstNonNilRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return cloneRawJSON(value)
		}
	}
	return nil
}

func cursorStringField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		if value := jsonStringValue(raw); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func cursorShouldSkipAssistantFrame(event cursorEvent, sawPartialAssistant *bool) bool {
	if cursorFrameType(event) != "assistant" {
		return false
	}
	hasTimestamp := event.TimestampMS != nil || event.TimestampMSCamel != nil
	if hasTimestamp && strings.TrimSpace(event.ModelCallID) != "" {
		return true
	}
	if hasTimestamp {
		if sawPartialAssistant != nil {
			*sawPartialAssistant = true
		}
		return false
	}
	return sawPartialAssistant != nil && *sawPartialAssistant
}

// DefaultCursorSearchPaths intentionally returns no local default. Cursor
// exposes hooks and stream output, but GC should only discover Cursor JSONL
// from configured capture paths until a stable native transcript store is
// supported.
func DefaultCursorSearchPaths() []string {
	return nil
}

// FindCursorSessionFileByID resolves a captured Cursor hook/stream JSONL file
// by session ID when one has been written into configured transcript search
// paths.
func FindCursorSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	return findCapturedACPSessionFileByID(searchPaths, DefaultCursorSearchPaths(), workDir, sessionID)
}

// FindCursorSessionFile searches configured Cursor capture directories for the
// newest hook/stream JSONL file whose recorded cwd matches workDir.
func FindCursorSessionFile(searchPaths []string, workDir string) string {
	return findCapturedACPSessionFile(searchPaths, DefaultCursorSearchPaths(), workDir)
}
