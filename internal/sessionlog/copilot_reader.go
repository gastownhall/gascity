package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// ReadCopilotFile reads a GitHub Copilot CLI session-state events.jsonl file
// and converts it to the standard Session format used by GC session logs.
func ReadCopilotFile(path string, _ int) (*Session, error) {
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
	emittedToolUse := make(map[string]bool)
	toolNames := make(map[string]string)
	syntheticIDs := newStableSyntheticEntryIDSequence("copilot")

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event copilotEvent
		if err := json.Unmarshal(line, &event); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if strings.TrimSpace(event.Type) == "" {
			continue
		}
		rawLine := append(json.RawMessage(nil), line...)
		syntheticID := syntheticIDs.ForRecord(rawLine)
		ts := copilotEventTimestamp(event)
		if sessionID == "" {
			sessionID = copilotSessionIDFromEvent(event)
		}

		var entry *Entry
		switch event.Type {
		case "session.start", "session.resume":
			continue
		case "user.message":
			entry = copilotMessageEntry(event, rawLine, "user", ts, syntheticID)
		case "system.message":
			entry = copilotMessageEntry(event, rawLine, "system", ts, syntheticID)
		case "assistant.message":
			entry = copilotAssistantMessageEntry(event, rawLine, ts, emittedToolUse, toolNames, syntheticID)
		case "tool.execution_start":
			entry = copilotToolStartEntry(event, rawLine, ts, emittedToolUse, toolNames, syntheticID)
		case "tool.execution_complete":
			entry = copilotToolCompleteEntry(event, rawLine, ts, toolNames, syntheticID)
		default:
			continue
		}
		if entry == nil {
			continue
		}
		entry.ParentUUID = lastUUID
		lastUUID = entry.UUID
		messages = append(messages, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning copilot session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	if sessionID == "" {
		sessionID = copilotSessionIDFromPath(path)
	}
	return &Session{
		ID:          sessionID,
		Messages:    messages,
		Diagnostics: diagnostics,
	}, nil
}

type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	CreatedAt string          `json:"created_at"`
	SessionID string          `json:"sessionId"`
}

func copilotMessageEntry(event copilotEvent, rawLine json.RawMessage, role string, ts time.Time, syntheticID stableSyntheticEntryIDSource) *Entry {
	text := copilotTextFromData(event.Data)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &Entry{
		UUID:      copilotEntryID(event, syntheticID),
		Type:      role,
		Timestamp: ts,
		Message:   mustMarshal(MessageContent{Role: role, Content: mustMarshal(text)}),
		Raw:       rawLine,
	}
}

func copilotAssistantMessageEntry(event copilotEvent, rawLine json.RawMessage, ts time.Time, emittedToolUse map[string]bool, toolNames map[string]string, syntheticID stableSyntheticEntryIDSource) *Entry {
	content := make([]ContentBlock, 0, 1)
	if text := strings.TrimSpace(copilotTextFromData(event.Data)); text != "" {
		content = append(content, ContentBlock{Type: "text", Text: text})
	}
	for _, request := range copilotToolRequests(event.Data) {
		if request.ID == "" {
			continue
		}
		toolNames[request.ID] = request.Name
		emittedToolUse[request.ID] = true
		content = append(content, ContentBlock{
			Type:  "tool_use",
			ID:    request.ID,
			Name:  request.Name,
			Input: copilotNeutralToolInput(request.Name, request.Arguments),
		})
	}
	if len(content) == 0 {
		return nil
	}
	return &Entry{
		UUID:      copilotEntryID(event, syntheticID),
		Type:      "assistant",
		Timestamp: ts,
		Message:   copilotMessageWithMetadata("assistant", content, event.Data),
		Raw:       rawLine,
	}
}

func copilotToolStartEntry(event copilotEvent, rawLine json.RawMessage, ts time.Time, emittedToolUse map[string]bool, toolNames map[string]string, syntheticID stableSyntheticEntryIDSource) *Entry {
	object := copilotDataObject(event.Data)
	callID := copilotStringField(object, "toolCallId", "tool_call_id", "callId", "call_id", "id")
	if callID == "" {
		return nil
	}
	name := copilotStringField(object, "toolName", "tool_name", "name", "tool")
	if name != "" {
		toolNames[callID] = name
	}
	if emittedToolUse[callID] {
		return nil
	}
	emittedToolUse[callID] = true
	return &Entry{
		UUID:      copilotEntryID(event, syntheticID),
		Type:      "assistant",
		Timestamp: ts,
		Message: mustMarshal(MessageContent{
			Role: "assistant",
			Content: mustMarshal([]ContentBlock{{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: copilotNeutralToolInput(name, firstCopilotRawField(object, "arguments", "args", "input", "parameters")),
			}}),
		}),
		Raw: rawLine,
	}
}

func copilotToolCompleteEntry(event copilotEvent, rawLine json.RawMessage, ts time.Time, toolNames map[string]string, syntheticID stableSyntheticEntryIDSource) *Entry {
	object := copilotDataObject(event.Data)
	callID := copilotStringField(object, "toolCallId", "tool_call_id", "callId", "call_id", "id")
	if callID == "" {
		return nil
	}
	content := copilotToolResultContent(object)
	isError := copilotToolResultIsError(object, content)
	return &Entry{
		UUID:      copilotEntryID(event, syntheticID),
		Type:      "tool_result",
		Timestamp: ts,
		ToolUseID: callID,
		Message: mustMarshal(MessageContent{
			Role: "tool",
			Content: mustMarshal([]ContentBlock{{
				Type:      "tool_result",
				ToolUseID: callID,
				Name:      toolNames[callID],
				Content:   content,
				IsError:   isError,
			}}),
		}),
		Raw: rawLine,
	}
}

func copilotMessageWithMetadata(role string, content []ContentBlock, rawData json.RawMessage) json.RawMessage {
	object := copilotDataObject(rawData)
	message := struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model,omitempty"`
		Usage   json.RawMessage `json:"usage,omitempty"`
	}{
		Role:    role,
		Content: mustMarshal(content),
		Model:   copilotStringField(object, "model", "selectedModel", "selected_model"),
		Usage:   firstCopilotRawField(object, "usage", "tokens"),
	}
	return mustMarshal(message)
}

type copilotToolRequest struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

func copilotToolRequests(rawData json.RawMessage) []copilotToolRequest {
	object := copilotDataObject(rawData)
	rawRequests := firstCopilotRawField(object, "toolRequests", "tool_requests", "tools")
	if len(rawRequests) == 0 {
		return nil
	}
	var requests []map[string]json.RawMessage
	if err := json.Unmarshal(rawRequests, &requests); err != nil {
		return nil
	}
	out := make([]copilotToolRequest, 0, len(requests))
	for _, request := range requests {
		out = append(out, copilotToolRequest{
			ID:        copilotStringField(request, "toolCallId", "tool_call_id", "callId", "call_id", "id"),
			Name:      copilotStringField(request, "name", "toolName", "tool_name", "tool"),
			Arguments: firstCopilotRawField(request, "arguments", "args", "input", "parameters"),
		})
	}
	return out
}

func copilotToolResultContent(data map[string]json.RawMessage) json.RawMessage {
	if errorRaw := firstCopilotRawField(data, "error"); len(errorRaw) > 0 && string(errorRaw) != "null" {
		return copilotNeutralErrorResult(errorRaw)
	}
	for _, key := range []string{"result", "output", "content", "stdout", "stderr"} {
		if raw := firstCopilotRawField(data, key); len(raw) > 0 && string(raw) != "null" {
			return copilotNeutralToolResult(raw)
		}
	}
	return mustMarshal("")
}

func copilotToolResultIsError(data map[string]json.RawMessage, content json.RawMessage) bool {
	if value, ok := copilotBoolField(data, "success"); ok && !value {
		return true
	}
	if value, ok := copilotBoolField(data, "is_error", "isError"); ok && value {
		return true
	}
	if len(firstCopilotRawField(data, "error")) > 0 {
		return true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(content, &object) == nil {
		if value, ok := copilotBoolField(object, "is_error", "isError"); ok && value {
			return true
		}
		if exitCode := copilotIntField(object, "exit_code", "exitCode"); exitCode != nil && *exitCode != 0 {
			return true
		}
	}
	return false
}

func copilotNeutralToolInput(name string, raw json.RawMessage) json.RawMessage {
	return copilotNeutralObject(raw, copilotNeutralInputKey, strings.TrimSpace(name))
}

func copilotNeutralToolResult(raw json.RawMessage) json.RawMessage {
	return copilotNeutralObject(raw, copilotNeutralResultKey, "")
}

func copilotNeutralErrorResult(raw json.RawMessage) json.RawMessage {
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return mustMarshal(map[string]string{"error": strings.TrimSpace(message)})
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return copilotNeutralToolResult(raw)
	}
	neutral := make(map[string]json.RawMessage)
	if text := copilotStringField(object, "message", "error", "text", "content"); text != "" {
		neutral["error"] = mustMarshal(text)
	}
	if code := copilotStringField(object, "code", "type"); code != "" {
		neutral["code"] = mustMarshal(code)
	}
	if len(neutral) == 0 {
		return copilotNeutralToolResult(raw)
	}
	return mustMarshal(neutral)
}

func copilotNeutralObject(raw json.RawMessage, normalizeKey func(string) string, toolName string) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return copilotNeutralObject(json.RawMessage(encoded), normalizeKey, toolName)
		}
		return mustMarshal(encoded)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return cloneRawJSON(raw)
	}
	neutral := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		normalizedKey := normalizeKey(key)
		switch normalizedKey {
		case "":
			continue
		case "provider_result":
			copilotMergeNeutralObject(neutral, copilotNeutralToolResult(value))
		case "patch_hunks":
			if hunks := neutralPatchHunks(value, jsonStringValue(neutral["file_path"])); len(hunks) > 0 {
				neutral["patch_hunks"] = mustMarshal(hunks)
			}
		case "error":
			copilotCopyError(neutral, value)
		default:
			neutral[normalizedKey] = cloneRawJSON(value)
		}
	}
	if command := firstNonEmpty(copilotStringField(neutral, "command"), copilotCommandFromToolName(toolName, neutral)); command != "" {
		neutral["command"] = mustMarshal(command)
	}
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func copilotNeutralInputKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "toolcallid", "tool_call_id", "callid", "call_id", "id", "toolname", "tool_name", "mcpservername", "mcp_server_name", "mcptoolname", "mcp_tool_name":
		return ""
	case "cmd", "command", "commandtorun", "command_to_run", "shellcommand", "shell_command":
		return "command"
	case "workingdir", "working_dir", "cwd":
		return "working_dir"
	case "filepath", "file_path", "path", "file":
		return "file_path"
	case "oldstring", "old_string", "oldstr", "old_str", "old":
		return "old_string"
	case "newstring", "new_string", "newstr", "new_str", "new", "replacement":
		return "new_string"
	case "originalfile", "original_file":
		return "original_file"
	case "replaceall", "replace_all":
		return "replace_all"
	case "usermodified", "user_modified":
		return "user_modified"
	default:
		return key
	}
}

func copilotNeutralResultKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "toolcallid", "tool_call_id", "callid", "call_id", "id", "model", "interactionid", "interaction_id", "tooltelemetry", "tool_telemetry":
		return ""
	case "resultdisplay", "result_display":
		return "provider_result"
	case "detailedcontent", "detailed_content":
		return "content"
	case "message":
		return "content"
	case "exitcode", "exit_code":
		return "exit_code"
	case "filepath", "file_path", "path", "file":
		return "file_path"
	case "filediff", "file_diff", "diff":
		return "patch"
	case "structuredpatch", "structured_patch", "patchhunks", "patch_hunks":
		return "patch_hunks"
	case "oldstring", "old_string", "oldstr", "old_str":
		return "old_string"
	case "newstring", "new_string", "newstr", "new_str":
		return "new_string"
	case "originalfile", "original_file":
		return "original_file"
	case "replaceall", "replace_all":
		return "replace_all"
	case "usermodified", "user_modified":
		return "user_modified"
	case "iserror", "is_error":
		return "is_error"
	default:
		return copilotNeutralInputKey(key)
	}
}

func copilotMergeNeutralObject(target map[string]json.RawMessage, raw json.RawMessage) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	for key, value := range object {
		if _, exists := target[key]; exists {
			continue
		}
		target[key] = cloneRawJSON(value)
	}
}

func copilotCopyError(neutral map[string]json.RawMessage, raw json.RawMessage) {
	var message string
	if json.Unmarshal(raw, &message) == nil {
		if strings.TrimSpace(message) != "" {
			neutral["error"] = mustMarshal(strings.TrimSpace(message))
		}
		return
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		neutral["error"] = cloneRawJSON(raw)
		return
	}
	if text := copilotStringField(object, "message", "error", "text", "content"); text != "" {
		neutral["error"] = mustMarshal(text)
	}
	if code := copilotStringField(object, "code", "type"); code != "" {
		neutral["code"] = mustMarshal(code)
	}
}

func copilotCommandFromToolName(toolName string, object map[string]json.RawMessage) string {
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	if !strings.Contains(toolName, "terminal") && !strings.Contains(toolName, "shell") && !strings.Contains(toolName, "bash") {
		return ""
	}
	return firstNonEmpty(
		copilotStringField(object, "command"),
		copilotStringField(object, "cmd"),
		copilotStringField(object, "command_to_run"),
	)
}

func copilotTextFromData(raw json.RawMessage) string {
	object := copilotDataObject(raw)
	for _, key := range []string{"content", "text", "message", "output"} {
		rawValue := firstCopilotRawField(object, key)
		if len(rawValue) == 0 {
			continue
		}
		if text := copilotTextFromRaw(rawValue); text != "" {
			return text
		}
	}
	return ""
}

func copilotTextFromRaw(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, strings.TrimSpace(block.Text))
			}
		}
		return strings.Join(parts, "\n")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		return firstNonEmpty(
			copilotStringField(object, "content"),
			copilotStringField(object, "text"),
			copilotStringField(object, "message"),
		)
	}
	return ""
}

func copilotDataObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func firstCopilotRawField(object map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, name := range names {
		if raw, ok := object[name]; ok && len(raw) > 0 {
			return cloneRawJSON(raw)
		}
	}
	return nil
}

func copilotStringField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		if value := jsonStringValue(raw); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func copilotBoolField(object map[string]json.RawMessage, names ...string) (bool, bool) {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
	}
	return false, false
}

func copilotIntField(object map[string]json.RawMessage, names ...string) *int {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value int
		if json.Unmarshal(raw, &value) == nil {
			return &value
		}
	}
	return nil
}

func copilotEventTimestamp(event copilotEvent) time.Time {
	for _, value := range []string{event.Timestamp, event.CreatedAt} {
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return ts
		}
	}
	object := copilotDataObject(event.Data)
	for _, key := range []string{"timestamp", "created_at", "createdAt"} {
		if ts, err := time.Parse(time.RFC3339Nano, copilotStringField(object, key)); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func copilotEntryID(event copilotEvent, syntheticID stableSyntheticEntryIDSource) string {
	if strings.TrimSpace(event.ID) != "" {
		return strings.TrimSpace(event.ID)
	}
	return syntheticID.ID("")
}

func copilotSessionIDFromEvent(event copilotEvent) string {
	if strings.TrimSpace(event.SessionID) != "" {
		return strings.TrimSpace(event.SessionID)
	}
	object := copilotDataObject(event.Data)
	return copilotStringField(object, "sessionId", "session_id", "id")
}

func copilotSessionIDFromPath(path string) string {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "." || dir == string(filepath.Separator) {
		return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return dir
}

// DefaultCopilotSearchPaths returns the default search paths for Copilot CLI
// session-state event logs (~/.copilot/session-state).
func DefaultCopilotSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".copilot", "session-state")}
}

// FindCopilotSessionFileByID resolves a Copilot session-state events.jsonl file
// by the session directory name.
func FindCopilotSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	for _, root := range mergeCopilotSearchPaths(searchPaths) {
		path := filepath.Join(root, sessionID, "events.jsonl")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if strings.TrimSpace(workDir) != "" && !copilotSessionCWDMatches(path, workDir) {
			continue
		}
		return path
	}
	return ""
}

// FindCopilotSessionFile searches Copilot's session-state directories for the
// most recently modified events.jsonl whose workspace cwd matches workDir.
func FindCopilotSessionFile(searchPaths []string, workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	var candidates []sessionFileCandidate
	for _, root := range mergeCopilotSearchPaths(searchPaths) {
		candidates = append(candidates, copilotSessionCandidates(root)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		if copilotSessionCWDMatches(candidate.path, workDir) {
			return candidate.path
		}
	}
	return ""
}

type sessionFileCandidate struct {
	path    string
	modTime time.Time
}

func copilotSessionCandidates(root string) []sessionFileCandidate {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	var candidates []sessionFileCandidate
	if path := filepath.Join(root, "events.jsonl"); copilotEventsFileExists(path) {
		if info, err := os.Stat(path); err == nil {
			candidates = append(candidates, sessionFileCandidate{path: path, modTime: info.ModTime()})
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return candidates
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "events.jsonl")
		if !copilotEventsFileExists(path) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		candidates = append(candidates, sessionFileCandidate{path: path, modTime: info.ModTime()})
	}
	return candidates
}

func copilotEventsFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copilotSessionCWDMatches(path, workDir string) bool {
	cwd := copilotSessionCWD(path)
	if cwd == "" || workDir == "" {
		return false
	}
	return pathutil.SamePath(cwd, workDir)
}

func copilotSessionCWD(path string) string {
	if cwd := copilotSessionStartCWD(path); cwd != "" {
		return cwd
	}
	return copilotWorkspaceYAMLCWD(filepath.Join(filepath.Dir(path), "workspace.yaml"))
}

func copilotSessionStartCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event copilotEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Type != "session.start" {
			continue
		}
		data := copilotDataObject(event.Data)
		contextRaw := firstCopilotRawField(data, "context")
		var context map[string]json.RawMessage
		if json.Unmarshal(contextRaw, &context) == nil {
			if cwd := copilotStringField(context, "cwd", "workingDir", "working_dir"); cwd != "" {
				return cwd
			}
		}
		if cwd := copilotStringField(data, "cwd", "workingDir", "working_dir"); cwd != "" {
			return cwd
		}
	}
	return ""
}

func copilotWorkspaceYAMLCWD(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cwd:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "cwd:"))
		value = strings.Trim(value, `"'`)
		return strings.TrimSpace(value)
	}
	return ""
}

func mergeCopilotSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultCopilotSearchPaths(), extraPaths)
}
