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

// ReadAmpFile reads an Amp --execute --stream-json JSONL capture and converts
// it to the standard Session format used by GC session logs.
func ReadAmpFile(path string, _ int) (*Session, error) {
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
	syntheticIDs := newStableSyntheticEntryIDSequence("amp")

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rawLine := append(json.RawMessage(nil), line...)
		var event ampEvent
		if err := json.Unmarshal(line, &event); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if sessionID == "" && strings.TrimSpace(event.SessionID) != "" {
			sessionID = strings.TrimSpace(event.SessionID)
		}

		recordIDs := syntheticIDs.ForRecord(rawLine)
		entries := ampEntriesFromEvent(event, rawLine, toolNames, recordIDs)
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
		return nil, fmt.Errorf("scanning amp stream JSON file: %w", err)
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

type ampEvent struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	Message         json.RawMessage `json:"message"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	CWD             string          `json:"cwd"`
	Result          string          `json:"result"`
	Error           string          `json:"error"`
	IsError         bool            `json:"is_error"`
	Usage           json.RawMessage `json:"usage"`
}

func ampEntriesFromEvent(event ampEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) []*Entry {
	switch strings.ToLower(strings.TrimSpace(event.Type)) {
	case "system":
		return nil
	case "assistant":
		entry := ampAssistantEntry(event, rawLine, toolNames, syntheticIDs)
		if entry == nil {
			return nil
		}
		return []*Entry{entry}
	case "user":
		return ampUserEntries(event, rawLine, toolNames, syntheticIDs)
	case "result":
		entry := ampResultEntry(event, rawLine, syntheticIDs)
		if entry == nil {
			return nil
		}
		return []*Entry{entry}
	default:
		return nil
	}
}

func ampAssistantEntry(event ampEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) *Entry {
	message := ampMessageObject(event.Message)
	blocks := ampAssistantContentBlocks(message.Content, toolNames)
	if len(blocks) == 0 {
		return nil
	}
	return &Entry{
		UUID:      syntheticIDs.ID("assistant"),
		Type:      "assistant",
		Message:   ampMessageWithMetadata("assistant", blocks, message),
		SessionID: strings.TrimSpace(event.SessionID),
		Raw:       rawLine,
	}
}

func ampUserEntries(event ampEvent, rawLine json.RawMessage, toolNames map[string]string, syntheticIDs stableSyntheticEntryIDSource) []*Entry {
	message := ampMessageObject(event.Message)
	rawBlocks := ampRawArray(message.Content)
	if len(rawBlocks) == 0 {
		if text := ampTextFromRaw(message.Content); text != "" {
			return []*Entry{{
				UUID:      syntheticIDs.ID("user"),
				Type:      "user",
				Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(text)}),
				SessionID: strings.TrimSpace(event.SessionID),
				Raw:       rawLine,
			}}
		}
		return nil
	}
	var textBlocks []ContentBlock
	var entries []*Entry
	for _, rawBlock := range rawBlocks {
		block := ampRawObject(rawBlock)
		switch strings.ToLower(strings.TrimSpace(ampStringField(block, "type"))) {
		case "text":
			if text := ampTextFromRaw(rawBlock); text != "" {
				textBlocks = append(textBlocks, ContentBlock{Type: "text", Text: text})
			}
		case "tool_result":
			callID := ampStringField(block, "tool_use_id", "toolUseId", "id")
			if callID == "" {
				continue
			}
			content := ampNeutralToolResult(firstAmpRawField(block, "content", "result", "output"))
			entries = append(entries, &Entry{
				UUID:      syntheticIDs.ID(fmt.Sprintf("tool_result:%s:%d", callID, len(entries))),
				Type:      "tool_result",
				ToolUseID: callID,
				SessionID: strings.TrimSpace(event.SessionID),
				Message: ampMessageWithBlocks("tool", []ContentBlock{{
					Type:      "tool_result",
					ToolUseID: callID,
					Name:      toolNames[callID],
					Content:   content,
					IsError:   ampBoolField(block, "is_error", "isError"),
				}}),
				Raw: rawLine,
			})
		}
	}
	if len(textBlocks) > 0 {
		entries = append([]*Entry{{
			UUID:      syntheticIDs.ID("user"),
			Type:      "user",
			Message:   ampMessageWithBlocks("user", textBlocks),
			SessionID: strings.TrimSpace(event.SessionID),
			Raw:       rawLine,
		}}, entries...)
	}
	return entries
}

func ampResultEntry(event ampEvent, rawLine json.RawMessage, syntheticIDs stableSyntheticEntryIDSource) *Entry {
	message := firstNonEmpty(strings.TrimSpace(event.Result), strings.TrimSpace(event.Error))
	kind := "result"
	category := strings.TrimSpace(event.Subtype)
	if category == "" && event.IsError {
		category = "error"
	}
	if message == "" && category == "" {
		return nil
	}
	return &Entry{
		UUID:      syntheticIDs.ID("result"),
		Type:      "system",
		Subtype:   "result",
		Message:   mustMarshal(MessageContent{Role: "system", Content: mustMarshal(message)}),
		SessionID: strings.TrimSpace(event.SessionID),
		SystemEvent: &SystemEvent{
			Kind:     kind,
			Category: category,
			Message:  message,
		},
		Raw: rawLine,
	}
}

type ampMessage struct {
	Content    json.RawMessage
	StopReason string
	Usage      json.RawMessage
}

func ampMessageObject(raw json.RawMessage) ampMessage {
	var object struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(raw, &object)
	return ampMessage{
		Content:    object.Content,
		StopReason: strings.TrimSpace(object.StopReason),
		Usage:      cloneRawJSON(object.Usage),
	}
}

func ampAssistantContentBlocks(raw json.RawMessage, toolNames map[string]string) []ContentBlock {
	rawBlocks := ampRawArray(raw)
	if len(rawBlocks) == 0 {
		if text := ampTextFromRaw(raw); text != "" {
			return []ContentBlock{{Type: "text", Text: text}}
		}
		return nil
	}
	blocks := make([]ContentBlock, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		object := ampRawObject(rawBlock)
		switch strings.ToLower(strings.TrimSpace(ampStringField(object, "type"))) {
		case "text":
			if text := ampTextFromRaw(rawBlock); text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		case "thinking":
			if thinking := ampStringField(object, "thinking", "text"); thinking != "" {
				blocks = append(blocks, ContentBlock{
					Type:      "thinking",
					Thinking:  thinking,
					Signature: ampStringField(object, "signature"),
				})
			}
		case "tool_use":
			callID := ampStringField(object, "id", "tool_use_id", "toolUseId")
			if callID == "" {
				continue
			}
			name := firstNonEmpty(ampStringField(object, "name"), "tool")
			toolNames[callID] = name
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: ampNeutralToolInput(name, firstAmpRawField(object, "input", "arguments", "args")),
			})
		}
	}
	return blocks
}

func ampMessageWithBlocks(role string, content []ContentBlock) json.RawMessage {
	return mustMarshal(MessageContent{
		Role:    role,
		Content: mustMarshal(content),
	})
}

func ampMessageWithMetadata(role string, content []ContentBlock, message ampMessage) json.RawMessage {
	payload := struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason,omitempty"`
		Usage      json.RawMessage `json:"usage,omitempty"`
	}{
		Role:       role,
		Content:    mustMarshal(content),
		StopReason: message.StopReason,
		Usage:      cloneRawJSON(message.Usage),
	}
	return mustMarshal(payload)
}

func ampNeutralToolInput(name string, raw json.RawMessage) json.RawMessage {
	return copilotNeutralObject(raw, ampNeutralInputKey, strings.TrimSpace(name))
}

func ampNeutralToolResult(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return copilotNeutralObject(json.RawMessage(encoded), ampNeutralResultKey, "")
		}
		return mustMarshal(encoded)
	}
	return copilotNeutralObject(raw, ampNeutralResultKey, "")
}

func ampNeutralInputKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "session_id", "sessionid", "parent_tool_use_id", "parenttooluseid":
		return ""
	default:
		return copilotNeutralInputKey(key)
	}
}

func ampNeutralResultKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "session_id", "sessionid", "parent_tool_use_id", "parenttooluseid", "tool_use_id", "tooluseid":
		return ""
	default:
		return copilotNeutralResultKey(key)
	}
}

func ampTextFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	object := ampRawObject(raw)
	if len(object) > 0 {
		return firstNonEmpty(
			ampStringField(object, "text"),
			ampStringField(object, "content"),
			ampStringField(object, "message"),
		)
	}
	return ""
}

func ampRawObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func ampRawArray(raw json.RawMessage) []json.RawMessage {
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) != nil {
		return nil
	}
	return array
}

func firstAmpRawField(object map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, name := range names {
		if raw, ok := object[name]; ok && len(raw) > 0 {
			return cloneRawJSON(raw)
		}
	}
	return nil
}

func ampStringField(object map[string]json.RawMessage, names ...string) string {
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

func ampBoolField(object map[string]json.RawMessage, names ...string) bool {
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

// DefaultAmpSearchPaths intentionally returns no local default. Amp documents
// stream-json output, not a stable retrospective local transcript store; GC
// should discover Amp JSONL only from configured capture paths.
func DefaultAmpSearchPaths() []string {
	return nil
}

// FindAmpSessionFileByID resolves a captured Amp stream JSONL file by session
// ID when one has been written into the configured transcript search paths.
func FindAmpSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	for _, root := range mergeAmpSearchPaths(searchPaths) {
		for _, path := range []string{
			filepath.Join(root, sessionID+".jsonl"),
			filepath.Join(root, sessionID, "stream.jsonl"),
			filepath.Join(root, sessionID, "events.jsonl"),
		} {
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			if strings.TrimSpace(workDir) != "" && !ampSessionCWDMatches(path, workDir) {
				continue
			}
			return path
		}
	}
	return ""
}

// FindAmpSessionFile searches configured Amp capture directories for the
// newest stream JSONL file whose init cwd matches workDir.
func FindAmpSessionFile(searchPaths []string, workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	var candidates []ampSessionFileCandidate
	for _, root := range mergeAmpSearchPaths(searchPaths) {
		candidates = append(candidates, ampSessionCandidates(root)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		if ampSessionCWDMatches(candidate.path, workDir) {
			return candidate.path
		}
	}
	return ""
}

type ampSessionFileCandidate struct {
	path    string
	modTime time.Time
}

func ampSessionCandidates(root string) []ampSessionFileCandidate {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	var candidates []ampSessionFileCandidate
	appendCandidate := func(path string) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return
		}
		candidates = append(candidates, ampSessionFileCandidate{path: path, modTime: info.ModTime()})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !entry.IsDir() {
			appendCandidate(path)
			continue
		}
		childEntries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, child := range childEntries {
			if child.IsDir() {
				continue
			}
			appendCandidate(filepath.Join(path, child.Name()))
		}
	}
	return candidates
}

func ampSessionCWDMatches(path, workDir string) bool {
	cwd := ampSessionCWD(path)
	if cwd == "" || workDir == "" {
		return false
	}
	return pathutil.SamePath(cwd, workDir)
}

func ampSessionCWD(path string) string {
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
		var event ampEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(event.Type)) == "system" && strings.TrimSpace(event.CWD) != "" {
			return strings.TrimSpace(event.CWD)
		}
	}
	return ""
}

func mergeAmpSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultAmpSearchPaths(), extraPaths)
}
