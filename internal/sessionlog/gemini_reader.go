package sessionlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// ReadGeminiFile reads a Gemini session JSON/JSONL file and converts it to the
// standard Session format used by GC session transcripts.
//
// Gemini stores sessions at ~/.gemini/tmp/<project>/chats/session-*.json or
// session-*.jsonl. Older files are single JSON objects with a linear messages[]
// array. Current CLI files are JSONL mutation streams with an initial session
// header, top-level message objects, and "$set.messages" snapshots.
func ReadGeminiFile(path string, _ int) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return readGeminiJSONLFile(path, data)
	}

	var raw struct {
		SessionID string            `json:"sessionId"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	sessionID := strings.TrimSpace(raw.SessionID)
	if sessionID == "" {
		sessionID = geminiSessionID(path)
	}

	var messages []*Entry
	syntheticIDs := newStableSyntheticEntryIDSequence("gemini")
	for _, rawMessage := range raw.Messages {
		entry := parseGeminiMessage(rawMessage, syntheticIDs.ForRecord(rawMessage))
		if entry == nil {
			continue
		}
		messages = append(messages, entry)
	}

	return &Session{
		ID:       sessionID,
		Messages: messages,
	}, nil
}

func readGeminiJSONLFile(path string, data []byte) (*Session, error) {
	type setPayload struct {
		Messages *[]json.RawMessage `json:"messages"`
	}
	type linePayload struct {
		SessionID string            `json:"sessionId"`
		Type      string            `json:"type"`
		Set       *setPayload       `json:"$set"`
		RawSet    *setPayload       `json:"set"`
		Messages  []json.RawMessage `json:"messages"`
	}

	sessionID := ""
	messages := make([]*Entry, 0)
	messageIndex := make(map[string]int)
	syntheticIDs := newStableSyntheticEntryIDSequence("gemini")
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool

	appendEntry := func(rawMessage json.RawMessage) {
		entry := parseGeminiMessage(rawMessage, syntheticIDs.ForRecord(rawMessage))
		if entry == nil {
			return
		}
		if idx, ok := messageIndex[entry.UUID]; ok {
			messages[idx] = entry
			return
		}
		messageIndex[entry.UUID] = len(messages)
		messages = append(messages, entry)
	}
	resetMessages := func(rawMessages []json.RawMessage) {
		messages = messages[:0]
		clear(messageIndex)
		syntheticIDs = newStableSyntheticEntryIDSequence("gemini")
		for _, rawMessage := range rawMessages {
			appendEntry(rawMessage)
		}
	}

	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var payload linePayload
		if err := json.Unmarshal(line, &payload); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if sessionID == "" {
			sessionID = strings.TrimSpace(payload.SessionID)
		}
		if payload.Set != nil && payload.Set.Messages != nil {
			resetMessages(*payload.Set.Messages)
			continue
		}
		if payload.RawSet != nil && payload.RawSet.Messages != nil {
			resetMessages(*payload.RawSet.Messages)
			continue
		}
		if len(payload.Messages) > 0 {
			resetMessages(payload.Messages)
			continue
		}
		if strings.TrimSpace(payload.Type) != "" {
			appendEntry(append(json.RawMessage(nil), line...))
		}
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed
	if sessionID == "" {
		sessionID = geminiSessionID(path)
	}
	return &Session{
		ID:          sessionID,
		Messages:    messages,
		Diagnostics: diagnostics,
	}, nil
}

func parseGeminiMessage(rawMessage json.RawMessage, syntheticID stableSyntheticEntryIDSource) *Entry {
	var message struct {
		ID           string              `json:"id"`
		Timestamp    string              `json:"timestamp"`
		Type         string              `json:"type"`
		Content      json.RawMessage     `json:"content"`
		Thoughts     []geminiThought     `json:"thoughts"`
		ToolCalls    []geminiToolCall    `json:"toolCalls"`
		Interactions []geminiInteraction `json:"interactions"`
		Model        string              `json:"model"`
	}
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		return nil
	}

	ts, _ := time.Parse(time.RFC3339Nano, message.Timestamp)
	uuid := strings.TrimSpace(message.ID)
	if uuid == "" {
		uuid = syntheticID.ID("")
	}

	switch message.Type {
	case "user":
		text := geminiContentText(message.Content)
		if text == "" {
			text = strings.TrimSpace(string(message.Content))
		}
		if interactionBlocks := geminiInteractionBlocks(message.Interactions); len(interactionBlocks) > 0 {
			content := make([]ContentBlock, 0, 1+len(interactionBlocks))
			if strings.TrimSpace(text) != "" {
				content = append(content, ContentBlock{Type: "text", Text: text})
			}
			content = append(content, interactionBlocks...)
			return &Entry{
				UUID:      uuid,
				Type:      "user",
				Timestamp: ts,
				Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(content)}),
				Raw:       append(json.RawMessage(nil), rawMessage...),
			}
		}
		return &Entry{
			UUID:      uuid,
			Type:      "user",
			Timestamp: ts,
			Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(text)}),
			Raw:       append(json.RawMessage(nil), rawMessage...),
		}
	case "info":
		text := strings.TrimSpace(geminiContentText(message.Content))
		if text == "" {
			text = strings.Trim(strings.TrimSpace(string(message.Content)), `"`)
		}
		return &Entry{
			UUID:      uuid,
			Type:      "system",
			Timestamp: ts,
			Message:   mustMarshal(MessageContent{Role: "system", Content: mustMarshal(text)}),
			Raw:       append(json.RawMessage(nil), rawMessage...),
		}
	case "error":
		text := strings.TrimSpace(geminiContentText(message.Content))
		if text == "" {
			text = strings.Trim(strings.TrimSpace(string(message.Content)), `"`)
		}
		if text == "" {
			text = "Gemini reported an error"
		}
		systemEvent := geminiSystemErrorEvent(text)
		return &Entry{
			UUID:        uuid,
			Type:        "system",
			Subtype:     systemEvent.Kind,
			SystemEvent: systemEvent,
			Timestamp:   ts,
			Message:     mustMarshal(MessageContent{Role: "system", Content: mustMarshal(text)}),
			Raw:         append(json.RawMessage(nil), rawMessage...),
		}
	case "gemini":
		content := make([]ContentBlock, 0, len(message.Thoughts)+1+len(message.ToolCalls)+len(message.Interactions))
		for _, thought := range message.Thoughts {
			text := strings.TrimSpace(thought.Description)
			subject := strings.TrimSpace(thought.Subject)
			if subject != "" && text != "" {
				text = subject + ": " + text
			} else if subject != "" {
				text = subject
			}
			if text == "" {
				continue
			}
			content = append(content, ContentBlock{Type: "thinking", Text: text})
		}

		if text := strings.TrimSpace(geminiContentText(message.Content)); text != "" {
			content = append(content, ContentBlock{Type: "text", Text: text})
		}

		for _, toolCall := range message.ToolCalls {
			content = append(content, ContentBlock{
				Type:  "tool_use",
				ID:    strings.TrimSpace(toolCall.ID),
				Name:  strings.TrimSpace(toolCall.Name),
				Input: toolCall.Args,
			})
			for _, result := range toolCall.Result {
				output := strings.TrimSpace(result.FunctionResponse.Response.Output)
				if output == "" {
					continue
				}
				content = append(content, ContentBlock{
					Type:      "tool_result",
					ToolUseID: firstNonEmpty(result.FunctionResponse.ID, toolCall.ID),
					Content:   geminiToolResultContent(output, toolCall.ResultDisplay),
					IsError:   geminiToolResultIsError(toolCall.Status, result.FunctionResponse.Response.Status),
				})
			}
		}

		content = append(content, geminiInteractionBlocks(message.Interactions)...)

		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "assistant",
				Content: mustMarshal(content),
			}),
			Raw: append(json.RawMessage(nil), rawMessage...),
		}
	default:
		return nil
	}
}

func geminiSystemErrorEvent(message string) *SystemEvent {
	return &SystemEvent{
		Kind:     "error",
		Category: "provider_error",
		Message:  strings.TrimSpace(message),
	}
}

func geminiInteractionBlocks(interactions []geminiInteraction) []ContentBlock {
	if len(interactions) == 0 {
		return nil
	}
	blocks := make([]ContentBlock, 0, len(interactions))
	for _, interaction := range interactions {
		blocks = append(blocks, ContentBlock{
			Type:      "interaction",
			RequestID: firstNonEmpty(interaction.RequestID, interaction.ID),
			Kind:      strings.TrimSpace(interaction.Kind),
			State:     strings.TrimSpace(interaction.State),
			Text:      strings.TrimSpace(interaction.Text),
			Prompt:    strings.TrimSpace(interaction.Prompt),
			Options:   append([]string(nil), interaction.Options...),
			Action:    strings.TrimSpace(interaction.Action),
			Metadata:  cloneRawJSON(interaction.Metadata),
		})
	}
	return blocks
}

func geminiContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return strings.TrimSpace(plain)
	}

	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, part := range parts {
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			texts = append(texts, part.Text)
		}
		return strings.TrimSpace(strings.Join(texts, ""))
	}

	return ""
}

// FindGeminiSessionFile searches Gemini's tmp sessions directory
// (~/.gemini/tmp/<project>/chats/session-*.json*) for the most recently
// modified session matching workDir.
func FindGeminiSessionFile(searchPaths []string, workDir string) string {
	if workDir == "" {
		return ""
	}

	var (
		bestPath string
		bestTime time.Time
	)
	for _, root := range mergeGeminiSearchPaths(searchPaths) {
		path := findGeminiSessionFileIn(root, workDir)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestTime) {
			bestPath = path
			bestTime = info.ModTime()
		}
	}
	return bestPath
}

// FindGeminiSessionFileByID searches Gemini's tmp sessions directory for a
// transcript whose stored sessionId exactly matches sessionID.
func FindGeminiSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if workDir == "" || sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}

	var (
		bestPath string
		bestTime time.Time
	)
	for _, root := range mergeGeminiSearchPaths(searchPaths) {
		for _, path := range geminiSessionCandidatesIn(root, workDir) {
			if geminiSessionIDFromFile(path) != sessionID {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if bestPath == "" || info.ModTime().After(bestTime) {
				bestPath = path
				bestTime = info.ModTime()
			}
		}
	}
	return bestPath
}

func findGeminiSessionFileIn(root, workDir string) string {
	var (
		bestPath string
		bestTime time.Time
	)
	for _, path := range geminiSessionCandidatesIn(root, workDir) {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestTime) {
			bestPath = path
			bestTime = info.ModTime()
		}
	}
	return bestPath
}

func geminiSessionCandidatesIn(root, workDir string) []string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var candidates []string
	if candidate := geminiProjectDir(root, workDir); candidate != "" {
		candidates = append(candidates, candidate)
	}

	if geminiProjectRootMatches(root, workDir) {
		candidates = append(candidates, root)
	}

	entries, err := os.ReadDir(root)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if geminiProjectRootMatches(dir, workDir) {
				candidates = append(candidates, dir)
			}
		}
	}

	candidates = uniqueStrings(candidates)

	var paths []string
	for _, candidate := range candidates {
		paths = append(paths, geminiSessionsInChats(filepath.Join(candidate, "chats"))...)
	}

	return paths
}

func geminiProjectDir(root, workDir string) string {
	projectsPath := filepath.Join(filepath.Dir(root), "projects.json")
	data, err := os.ReadFile(projectsPath)
	if err != nil {
		return ""
	}

	var projects struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return ""
	}

	dirName := strings.TrimSpace(projects.Projects[workDir])
	if dirName == "" {
		for projectRoot, mappedDirName := range projects.Projects {
			if pathutil.SamePath(projectRoot, workDir) {
				dirName = strings.TrimSpace(mappedDirName)
				break
			}
		}
	}
	if dirName == "" {
		return ""
	}
	return filepath.Join(root, dirName)
}

func geminiProjectRoot(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".project_root"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func geminiProjectRootMatches(dir, workDir string) bool {
	projectRoot := geminiProjectRoot(dir)
	if projectRoot == "" || workDir == "" {
		return false
	}
	return pathutil.SamePath(projectRoot, workDir)
}

func geminiSessionsInChats(chatsDir string) []string {
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return nil
	}

	type candidate struct {
		path    string
		modTime time.Time
	}
	var files []candidate
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "session-") || (!strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		path := filepath.Join(chatsDir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, candidate{path: path, modTime: info.ModTime()})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	if len(files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths
}

func geminiSessionIDFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var header struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(line, &header); err != nil {
				return ""
			}
			return strings.TrimSpace(header.SessionID)
		}
		return ""
	}
	var header struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return ""
	}
	return strings.TrimSpace(header.SessionID)
}

func geminiSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

type geminiThought struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

type geminiToolCall struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Args          json.RawMessage `json:"args"`
	ResultDisplay json.RawMessage `json:"resultDisplay"`
	Result        []struct {
		FunctionResponse struct {
			ID       string `json:"id"`
			Response struct {
				Output string `json:"output"`
				Status string `json:"status"`
			} `json:"response"`
		} `json:"functionResponse"`
	} `json:"result"`
}

func geminiToolResultIsError(statuses ...string) bool {
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "error", "failed", "failure", "canceled", "interrupted", "rejected", "denied":
			return true
		}
	}
	return false
}

func geminiToolResultContent(output string, resultDisplay json.RawMessage) json.RawMessage {
	if len(resultDisplay) == 0 {
		return mustMarshal(output)
	}
	normalized := map[string]json.RawMessage{
		"output": mustMarshal(output),
	}
	if filePath, patch := geminiResultDisplayPatch(resultDisplay); patch != "" {
		normalized["file_path"] = mustMarshal(filePath)
		normalized["patch"] = mustMarshal(patch)
		return mustMarshal(normalized)
	}
	normalized["content"] = cloneRawJSON(resultDisplay)
	return mustMarshal(normalized)
}

func geminiResultDisplayPatch(raw json.RawMessage) (string, string) {
	var display map[string]json.RawMessage
	if err := json.Unmarshal(raw, &display); err != nil || len(display) == 0 {
		return "", ""
	}
	filePath := firstNonEmpty(
		geminiStringField(display, "filePath", "file_path", "fileName", "file"),
		geminiPatchFilePath(geminiStringField(display, "fileDiff", "file_diff", "patch", "diff")),
	)
	if patch := geminiStringField(display, "fileDiff", "file_diff", "patch", "diff"); patch != "" {
		return filePath, patch
	}
	oldContent := geminiStringField(display, "originalContent", "original_content", "oldContent", "old_content")
	newContent := geminiStringField(display, "newContent", "new_content", "content")
	if oldContent == "" && newContent == "" {
		return "", ""
	}
	return filePath, geminiUnifiedPatch(filePath, oldContent, newContent)
}

func geminiStringField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func geminiPatchFilePath(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Index: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Index: "))
		}
		if strings.HasPrefix(line, "+++ ") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			path = strings.TrimSuffix(path, "\tWritten")
			path = strings.TrimSuffix(path, "\tModified")
			path = strings.TrimSuffix(path, "\tNew")
			if path != "/dev/null" {
				return strings.TrimSpace(path)
			}
		}
	}
	return ""
}

func geminiUnifiedPatch(filePath, oldContent, newContent string) string {
	from := firstNonEmpty(filePath, "file")
	to := from
	if oldContent == "" && newContent != "" {
		from = "/dev/null"
	}
	if oldContent != "" && newContent == "" {
		to = "/dev/null"
	}
	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(from)
	b.WriteString("\n+++ ")
	b.WriteString(to)
	b.WriteString("\n@@\n")
	geminiAppendPatchLines(&b, "-", oldContent)
	geminiAppendPatchLines(&b, "+", newContent)
	return b.String()
}

func geminiAppendPatchLines(b *strings.Builder, prefix, text string) {
	if text == "" {
		return
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for idx, line := range lines {
		if idx == len(lines)-1 && line == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

type geminiInteraction struct {
	RequestID string          `json:"request_id"`
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Text      string          `json:"text"`
	Prompt    string          `json:"prompt"`
	Options   []string        `json:"options"`
	Action    string          `json:"action"`
	Metadata  json.RawMessage `json:"metadata"`
}
