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

// ReadKiroFile reads a Kiro ACP session JSONL file and converts it to the
// standard Session format used by GC session logs.
func ReadKiroFile(path string, _ int) (*Session, error) {
	return readKiroFile(path, "kiro")
}

func readKiroFile(path, syntheticPrefix string) (*Session, error) {
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
	syntheticIDs := newStableSyntheticEntryIDSequence(syntheticPrefix)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rawLine := append(json.RawMessage(nil), line...)
		var event kiroEvent
		if err := json.Unmarshal(line, &event); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if sessionID == "" {
			sessionID = kiroSessionIDFromEvent(event)
		}

		recordIDs := syntheticIDs.ForRecord(rawLine)
		entries := kiroEntriesFromEvent(event, rawLine, toolNames, recordIDs)
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
		return nil, fmt.Errorf("scanning kiro session file: %w", err)
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

type kiroEvent struct {
	ID        json.RawMessage `json:"id"`
	Type      string          `json:"type"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Message   json.RawMessage `json:"message"`
	Content   json.RawMessage `json:"content"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	CreatedAt string          `json:"created_at"`
}

func kiroEntriesFromEvent(event kiroEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) []*Entry {
	if strings.TrimSpace(event.Method) == "session/update" {
		return kiroEntriesFromACPUpdate(event, rawLine, toolNames, syntheticIDs)
	}
	return kiroEntriesFromNativeFrame(event, rawLine, toolNames, syntheticIDs)
}

func kiroEntriesFromACPUpdate(event kiroEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) []*Entry {
	params := kiroRawObject(event.Params)
	updateRaw := firstKiroRawField(params, "update")
	update := kiroRawObject(updateRaw)
	if len(update) == 0 {
		return nil
	}
	updateType := kiroNormalizeUpdateType(kiroStringField(update, "sessionUpdate", "session_update", "type"))
	ts := kiroEventTimestamp(event, update)
	switch updateType {
	case "agentmessagechunk":
		text := kiroTextFromUpdate(update)
		if text == "" {
			return nil
		}
		return []*Entry{{
			UUID:      kiroEntryID(event, syntheticIDs),
			Type:      "assistant",
			Timestamp: ts,
			Message:   kiroMessageWithBlocks("assistant", []ContentBlock{{Type: "text", Text: text}}),
			Raw:       rawLine,
		}}
	case "toolcall":
		callID := kiroStringField(update, "toolCallId", "tool_call_id", "id")
		if callID == "" {
			return nil
		}
		name := firstNonEmpty(kiroStringField(update, "title", "name"), kiroStringField(update, "kind"), "tool")
		toolNames[callID] = name
		input := kiroNeutralToolInput(name, firstKiroRawField(update, "rawInput", "raw_input", "input", "arguments", "args"))
		return []*Entry{{
			UUID:      kiroEntryID(event, syntheticIDs),
			Type:      "assistant",
			Timestamp: ts,
			Message: kiroMessageWithBlocks("assistant", []ContentBlock{{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: input,
			}}),
			Raw: rawLine,
		}}
	case "toolcallupdate":
		callID := kiroStringField(update, "toolCallId", "tool_call_id", "id")
		if callID == "" {
			return nil
		}
		content := kiroToolUpdateResultContent(update)
		if len(content) == 0 {
			return nil
		}
		isError := kiroToolUpdateIsError(update, content)
		return []*Entry{{
			UUID:      kiroEntryID(event, syntheticIDs),
			Type:      "tool_result",
			Timestamp: ts,
			ToolUseID: callID,
			Message: kiroMessageWithBlocks("tool", []ContentBlock{{
				Type:      "tool_result",
				ToolUseID: callID,
				Name:      toolNames[callID],
				Content:   content,
				IsError:   isError,
			}}),
			Raw: rawLine,
		}}
	case "turnend":
		return nil
	default:
		return nil
	}
}

func kiroEntriesFromNativeFrame(event kiroEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) []*Entry {
	frameType := kiroNormalizeUpdateType(event.Type)
	message := event.Message
	if len(message) == 0 || string(message) == "null" {
		message = event.Content
	}
	switch frameType {
	case "usermessage":
		text := kiroTextFromRaw(message)
		if text == "" {
			return nil
		}
		return []*Entry{{
			UUID:      kiroEntryID(event, syntheticIDs),
			Type:      "user",
			Timestamp: kiroEventTimestamp(event, nil),
			Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(text)}),
			Raw:       rawLine,
		}}
	case "assistantmessage":
		blocks := kiroNativeContentBlocks(message, toolNames)
		if len(blocks) == 0 {
			return nil
		}
		return []*Entry{{
			UUID:      kiroEntryID(event, syntheticIDs),
			Type:      "assistant",
			Timestamp: kiroEventTimestamp(event, nil),
			Message:   kiroMessageWithBlocks("assistant", blocks),
			Raw:       rawLine,
		}}
	case "toolresults", "toolresult":
		blocks := kiroNativeToolResultBlocks(message, toolNames)
		entries := make([]*Entry, 0, len(blocks))
		for offset, block := range blocks {
			if strings.TrimSpace(block.ToolUseID) == "" {
				continue
			}
			entryID := kiroEntryID(event, syntheticIDs)
			if len(blocks) > 1 {
				// A native record ID identifies the container, not any one of
				// its normalized child entries. Hash each child so an ID such
				// as x cannot fabricate x-0 and alias a real native x-0 record.
				entryID = syntheticIDs.ID(fmt.Sprintf("%d", offset))
			}
			entries = append(entries, &Entry{
				UUID:      entryID,
				Type:      "tool_result",
				Timestamp: kiroEventTimestamp(event, nil),
				ToolUseID: block.ToolUseID,
				Message:   kiroMessageWithBlocks("tool", []ContentBlock{block}),
				Raw:       rawLine,
			})
		}
		return entries
	default:
		return nil
	}
}

func kiroNativeContentBlocks(raw json.RawMessage, toolNames map[string]string) []ContentBlock {
	contentRaw := kiroMessageContentRaw(raw)
	if text := kiroTextFromRaw(contentRaw); text != "" && !strings.HasPrefix(strings.TrimSpace(string(contentRaw)), "[") {
		return []ContentBlock{{Type: "text", Text: text}}
	}
	rawBlocks := kiroRawArray(contentRaw)
	blocks := make([]ContentBlock, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		object := kiroRawObject(rawBlock)
		blockType := kiroNormalizeUpdateType(kiroStringField(object, "type"))
		switch blockType {
		case "text":
			if text := kiroTextFromRaw(rawBlock); text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		case "tooluse":
			callID := kiroStringField(object, "id", "toolUseId", "tool_use_id", "toolCallId", "tool_call_id")
			if callID == "" {
				continue
			}
			name := firstNonEmpty(kiroStringField(object, "name", "title"), kiroStringField(object, "kind"), "tool")
			toolNames[callID] = name
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: kiroNeutralToolInput(name, firstKiroRawField(object, "input", "rawInput", "arguments", "args")),
			})
		}
	}
	return blocks
}

func kiroNativeToolResultBlocks(raw json.RawMessage, toolNames map[string]string) []ContentBlock {
	contentRaw := kiroMessageContentRaw(raw)
	rawBlocks := kiroRawArray(contentRaw)
	if len(rawBlocks) == 0 {
		rawBlocks = []json.RawMessage{contentRaw}
	}
	blocks := make([]ContentBlock, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		object := kiroRawObject(rawBlock)
		if len(object) == 0 {
			continue
		}
		callID := kiroStringField(object, "toolUseId", "tool_use_id", "toolCallId", "tool_call_id", "id")
		if callID == "" {
			continue
		}
		resultRaw := firstKiroRawField(object, "content", "result", "rawOutput", "raw_output", "output")
		content := kiroNeutralToolResult(resultRaw)
		isError := kiroBoolField(object, "isError", "is_error") || kiroToolUpdateIsError(object, content)
		blocks = append(blocks, ContentBlock{
			Type:      "tool_result",
			ToolUseID: callID,
			Name:      toolNames[callID],
			Content:   content,
			IsError:   isError,
		})
	}
	return blocks
}

func kiroMessageContentRaw(raw json.RawMessage) json.RawMessage {
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return cloneRawJSON(raw)
	}
	if content := firstKiroRawField(object, "content"); len(content) > 0 {
		return content
	}
	return cloneRawJSON(raw)
}

func kiroToolUpdateResultContent(update map[string]json.RawMessage) json.RawMessage {
	if rawOutput := firstKiroRawField(update, "rawOutput", "raw_output", "output", "result"); len(rawOutput) > 0 && string(rawOutput) != "null" {
		return kiroNeutralToolResult(rawOutput)
	}
	if errorRaw := firstKiroRawField(update, "error"); len(errorRaw) > 0 && string(errorRaw) != "null" {
		return kiroNeutralErrorResult(errorRaw)
	}
	if content := firstKiroRawField(update, "content"); len(content) > 0 && string(content) != "null" {
		if diff := kiroNeutralDiffResult(content); len(diff) > 0 {
			return diff
		}
		return kiroNeutralToolResult(content)
	}
	status := strings.ToLower(strings.TrimSpace(kiroStringField(update, "status")))
	if status == "failed" || status == "error" {
		return mustMarshal(map[string]string{"error": "tool call failed"})
	}
	return nil
}

func kiroNeutralToolInput(name string, raw json.RawMessage) json.RawMessage {
	return copilotNeutralObject(raw, kiroNeutralInputKey, strings.TrimSpace(name))
}

func kiroNeutralToolResult(raw json.RawMessage) json.RawMessage {
	return copilotNeutralObject(raw, kiroNeutralResultKey, "")
}

func kiroNeutralErrorResult(raw json.RawMessage) json.RawMessage {
	return copilotNeutralErrorResult(raw)
}

func kiroNeutralDiffResult(raw json.RawMessage) json.RawMessage {
	rawBlocks := kiroRawArray(raw)
	if len(rawBlocks) == 0 {
		rawBlocks = []json.RawMessage{raw}
	}
	neutral := make(map[string]json.RawMessage)
	for _, rawBlock := range rawBlocks {
		object := kiroRawObject(rawBlock)
		if len(object) == 0 || kiroNormalizeUpdateType(kiroStringField(object, "type")) != "diff" {
			continue
		}
		filePath := kiroStringField(object, "path", "filePath", "file_path", "file")
		oldText := kiroStringField(object, "oldText", "old_text", "oldString", "old_string", "old")
		newText := kiroStringField(object, "newText", "new_text", "newString", "new_string", "new")
		if filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
		if oldText != "" {
			neutral["old_string"] = mustMarshal(oldText)
		}
		if newText != "" {
			neutral["new_string"] = mustMarshal(newText)
		}
		if oldText != "" || newText != "" {
			neutral["patch"] = mustMarshal(kiroBuildUnifiedPatch(filePath, oldText, newText))
		}
	}
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func kiroBuildUnifiedPatch(filePath, oldText, newText string) string {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	if strings.TrimSpace(filePath) != "" {
		b.WriteString("*** Update File: ")
		b.WriteString(strings.TrimSpace(filePath))
		b.WriteString("\n")
	}
	b.WriteString("@@\n")
	for _, line := range kiroPatchLines("-", oldText) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, line := range kiroPatchLines("+", newText) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("*** End Patch")
	return b.String()
}

func kiroPatchLines(prefix, text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, prefix+part)
	}
	return out
}

func kiroNeutralInputKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "sessionupdate", "session_update", "toolcallid", "tool_call_id", "rawinput", "raw_input":
		return ""
	case "oldtext", "old_text":
		return "old_string"
	case "newtext", "new_text":
		return "new_string"
	case "cwd":
		return "working_dir"
	case "content":
		return "content"
	default:
		return copilotNeutralInputKey(key)
	}
}

func kiroNeutralResultKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "sessionupdate", "session_update", "toolcallid", "tool_call_id", "rawoutput", "raw_output", "rawinput", "raw_input":
		return ""
	case "oldtext", "old_text":
		return "old_string"
	case "newtext", "new_text":
		return "new_string"
	case "numlines", "num_lines":
		return "num_lines"
	case "startline", "start_line":
		return "start_line"
	case "totallines", "total_lines":
		return "total_lines"
	default:
		return copilotNeutralResultKey(key)
	}
}

func kiroToolUpdateIsError(update map[string]json.RawMessage, content json.RawMessage) bool {
	status := strings.ToLower(strings.TrimSpace(kiroStringField(update, "status", "state")))
	if status == "failed" || status == "error" {
		return true
	}
	if kiroBoolField(update, "isError", "is_error") {
		return true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(content, &object) == nil {
		if kiroBoolField(object, "is_error", "isError") {
			return true
		}
		if exitCode := kiroIntField(object, "exit_code", "exitCode"); exitCode != nil && *exitCode != 0 {
			return true
		}
		if strings.TrimSpace(kiroStringField(object, "error")) != "" {
			return true
		}
	}
	return false
}

func kiroTextFromUpdate(update map[string]json.RawMessage) string {
	for _, key := range []string{"content", "text", "message", "delta"} {
		raw := firstKiroRawField(update, key)
		if len(raw) == 0 {
			continue
		}
		if text := kiroTextFromRaw(raw); text != "" {
			return text
		}
	}
	return ""
}

func kiroTextFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	object := kiroRawObject(raw)
	if len(object) > 0 {
		return firstNonEmpty(
			kiroStringField(object, "text"),
			kiroStringField(object, "content"),
			kiroStringField(object, "message"),
		)
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if text := firstNonEmpty(kiroStringField(block, "text"), kiroStringField(block, "content")); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func kiroMessageWithBlocks(role string, content []ContentBlock) json.RawMessage {
	return mustMarshal(MessageContent{
		Role:    role,
		Content: mustMarshal(content),
	})
}

func kiroRawObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func kiroRawArray(raw json.RawMessage) []json.RawMessage {
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) != nil {
		return nil
	}
	return array
}

func firstKiroRawField(object map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, name := range names {
		if raw, ok := object[name]; ok && len(raw) > 0 {
			return cloneRawJSON(raw)
		}
	}
	return nil
}

func kiroStringField(object map[string]json.RawMessage, names ...string) string {
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

func kiroBoolField(object map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return false
}

func kiroIntField(object map[string]json.RawMessage, names ...string) *int {
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

func kiroEventTimestamp(event kiroEvent, update map[string]json.RawMessage) time.Time {
	for _, value := range []string{event.Timestamp, event.CreatedAt, kiroStringField(update, "timestamp", "created_at", "createdAt")} {
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func kiroEntryID(event kiroEvent, syntheticIDs stableSyntheticEntryIDSource) string {
	if len(event.ID) > 0 {
		if value := jsonStringValue(event.ID); value != "" {
			return value
		}
	}
	return syntheticIDs.ID("")
}

func kiroSessionIDFromEvent(event kiroEvent) string {
	if strings.TrimSpace(event.SessionID) != "" {
		return strings.TrimSpace(event.SessionID)
	}
	params := kiroRawObject(event.Params)
	if sessionID := kiroStringField(params, "sessionId", "session_id", "id"); sessionID != "" {
		return sessionID
	}
	return ""
}

func kiroNormalizeUpdateType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, "/", "")
	return value
}

// DefaultKiroSearchPaths returns the default search paths for Kiro ACP
// session JSONL files (~/.kiro/sessions/cli).
func DefaultKiroSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".kiro", "sessions", "cli")}
}

// FindKiroSessionFileByID resolves a Kiro ACP session JSONL file by session ID.
func FindKiroSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	for _, root := range mergeKiroSearchPaths(searchPaths) {
		path := filepath.Join(root, sessionID+".jsonl")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if strings.TrimSpace(workDir) != "" && !kiroSessionCWDMatches(path, workDir) {
			continue
		}
		return path
	}
	return ""
}

// FindKiroSessionFile searches Kiro ACP session directories for the newest
// JSONL session whose recorded cwd matches workDir.
func FindKiroSessionFile(searchPaths []string, workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	var candidates []kiroSessionFileCandidate
	for _, root := range mergeKiroSearchPaths(searchPaths) {
		candidates = append(candidates, kiroSessionCandidates(root)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		if kiroSessionCWDMatches(candidate.path, workDir) {
			return candidate.path
		}
	}
	return ""
}

type kiroSessionFileCandidate struct {
	path    string
	modTime time.Time
}

func kiroSessionCandidates(root string) []kiroSessionFileCandidate {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var candidates []kiroSessionFileCandidate
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		candidates = append(candidates, kiroSessionFileCandidate{path: path, modTime: info.ModTime()})
	}
	return candidates
}

func kiroSessionCWDMatches(path, workDir string) bool {
	cwd := kiroSessionCWD(path)
	if cwd == "" || workDir == "" {
		return false
	}
	return pathutil.SamePath(cwd, workDir)
}

func kiroSessionCWD(path string) string {
	if cwd := kiroSidecarCWD(strings.TrimSuffix(path, filepath.Ext(path)) + ".json"); cwd != "" {
		return cwd
	}
	return kiroJSONLCWD(path)
}

func kiroSidecarCWD(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var raw json.RawMessage = data
	return kiroCWDFromRawJSON(raw)
}

func kiroJSONLCWD(path string) string {
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
		raw := append(json.RawMessage(nil), line...)
		if cwd := kiroCWDFromRawJSON(raw); cwd != "" {
			return cwd
		}
	}
	return ""
}

func kiroCWDFromRawJSON(raw json.RawMessage) string {
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return ""
	}
	if cwd := kiroStringField(object, "cwd", "workingDir", "working_dir", "workDir", "work_dir", "directory"); cwd != "" {
		return cwd
	}
	for _, key := range []string{"params", "context", "workspace", "project", "metadata", "data"} {
		if cwd := kiroCWDFromRawJSON(firstKiroRawField(object, key)); cwd != "" {
			return cwd
		}
	}
	return ""
}

func mergeKiroSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultKiroSearchPaths(), extraPaths)
}
