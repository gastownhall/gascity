package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadOpenCodeFile reads an OpenCode session export JSON file and converts it
// to the standard Session format used by gc session logs.
func ReadOpenCodeFile(path string, tailCompactions int) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var export openCodeExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, err
	}

	sessionID := strings.TrimSpace(export.Info.ID)
	if sessionID == "" {
		sessionID = openCodeSessionID(path)
	}

	messages := make([]*Entry, 0, len(export.Messages))
	orphanedToolUseIDs := make(map[string]bool)
	var lastID string
	for idx, rawMessage := range export.Messages {
		entry := convertOpenCodeMessage(rawMessage, sessionID, idx, orphanedToolUseIDs)
		if entry == nil {
			continue
		}
		if entry.ParentUUID == "" {
			entry.ParentUUID = lastID
		}
		lastID = entry.UUID
		messages = append(messages, entry)
	}

	sess := &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: orphanedToolUseIDs,
	}
	if len(sess.OrphanedToolUseIDs) == 0 {
		sess.OrphanedToolUseIDs = nil
	}
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}
	return sess, nil
}

// FindOpenCodeSessionFile searches OpenCode JSON export directories for the
// most recently modified export whose embedded info.directory matches workDir.
func FindOpenCodeSessionFile(searchPaths []string, workDir string) string {
	workDir = cleanOpenCodeWorkDir(workDir)
	if workDir == "" {
		return ""
	}

	var (
		bestPath string
		bestTime time.Time
	)
	for _, root := range mergeOpenCodeSearchPaths(searchPaths) {
		path := findOpenCodeSessionFileIn(root, workDir)
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

func findOpenCodeSessionFileIn(root, workDir string) string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ""
	}

	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		if cleanOpenCodeWorkDir(openCodeExportDirectory(path)) != workDir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].path
}

func convertOpenCodeMessage(rawMessage json.RawMessage, sessionID string, idx int, orphanedToolUseIDs map[string]bool) *Entry {
	var message openCodeMessage
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		return nil
	}

	role := strings.TrimSpace(message.Info.Role)
	if role == "" {
		role = "assistant"
	}
	if role == "developer" {
		return nil
	}

	uuid := strings.TrimSpace(message.Info.ID)
	if uuid == "" {
		uuid = fmt.Sprintf("opencode-%d", idx)
	}
	ts := time.Time{}
	if message.Info.Time.Created > 0 {
		ts = time.UnixMilli(message.Info.Time.Created)
	}

	blocks := openCodeMessageBlocks(message.Parts, orphanedToolUseIDs)
	entry := &Entry{
		UUID:       uuid,
		ParentUUID: strings.TrimSpace(message.Info.ParentID),
		Type:       role,
		Timestamp:  ts,
		SessionID:  firstNonEmpty(message.Info.SessionID, sessionID),
		Raw:        cloneRawJSON(rawMessage),
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		entry.Message = mustMarshal(MessageContent{Role: role, Content: mustMarshal(blocks[0].Text)})
		return entry
	}
	entry.Message = mustMarshal(MessageContent{Role: role, Content: mustMarshal(blocks)})
	return entry
}

func openCodeMessageBlocks(parts []openCodePart, orphanedToolUseIDs map[string]bool) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part.Type)) {
		case "text":
			text := strings.TrimSpace(part.Text)
			if text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		case "reasoning":
			text := strings.TrimSpace(firstNonEmpty(part.Text, part.Summary))
			if text != "" {
				blocks = append(blocks, ContentBlock{Type: "thinking", Text: text})
			}
		case "tool":
			blocks = append(blocks, openCodeToolBlocks(part, orphanedToolUseIDs)...)
		case "interaction":
			blocks = append(blocks, openCodeInteractionBlock(part))
		}
		if interaction, ok := openCodePartMetadataInteraction(part.Metadata); ok {
			blocks = append(blocks, interaction)
		}
	}
	if len(blocks) == 0 {
		return []ContentBlock{{Type: "text"}}
	}
	return blocks
}

func openCodeToolBlocks(part openCodePart, orphanedToolUseIDs map[string]bool) []ContentBlock {
	callID := firstNonEmpty(part.CallID, part.ID)
	toolName := strings.TrimSpace(part.Tool)
	state := decodeOpenCodeToolState(part.State)
	status := strings.ToLower(strings.TrimSpace(state.Status))
	input := cloneRawJSON(state.Input)
	if len(input) == 0 {
		input = cloneRawJSON(part.Input)
	}

	blocks := []ContentBlock{{
		Type:  "tool_use",
		ID:    callID,
		Name:  toolName,
		Input: input,
	}}
	if callID != "" {
		orphanedToolUseIDs[callID] = true
	}
	if status == "completed" || status == "error" || status == "failed" {
		result := ContentBlock{
			Type:      "tool_result",
			ToolUseID: callID,
			Content:   openCodeToolResultContent(state),
			IsError:   status == "error" || status == "failed",
		}
		blocks = append(blocks, result)
		if callID != "" {
			delete(orphanedToolUseIDs, callID)
		}
	}
	return blocks
}

func openCodeToolResultContent(state openCodeToolState) json.RawMessage {
	if len(state.Output) != 0 {
		return cloneRawJSON(state.Output)
	}
	if len(state.Error) != 0 {
		return cloneRawJSON(state.Error)
	}
	return nil
}

func openCodeInteractionBlock(part openCodePart) ContentBlock {
	return ContentBlock{
		Type:      "interaction",
		RequestID: firstNonEmpty(part.RequestID, part.ID, part.CallID),
		Kind:      strings.TrimSpace(part.Kind),
		State:     openCodeStateText(part.State),
		Text:      strings.TrimSpace(part.Text),
		Prompt:    strings.TrimSpace(part.Prompt),
		Options:   append([]string(nil), part.Options...),
		Action:    strings.TrimSpace(part.Action),
		Metadata:  cloneRawJSON(part.Metadata),
	}
}

func decodeOpenCodeToolState(raw json.RawMessage) openCodeToolState {
	var state openCodeToolState
	if len(raw) == 0 {
		return state
	}
	_ = json.Unmarshal(raw, &state)
	return state
}

func openCodeStateText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &state); err == nil {
		return strings.TrimSpace(state.Status)
	}
	return ""
}

func openCodePartMetadataInteraction(raw json.RawMessage) (ContentBlock, bool) {
	if len(raw) == 0 {
		return ContentBlock{}, false
	}
	var wrapper struct {
		Interaction *struct {
			RequestID string          `json:"request_id"`
			ID        string          `json:"id"`
			Kind      string          `json:"kind"`
			State     string          `json:"state"`
			Text      string          `json:"text"`
			Prompt    string          `json:"prompt"`
			Options   []string        `json:"options"`
			Action    string          `json:"action"`
			Metadata  json.RawMessage `json:"metadata"`
		} `json:"interaction"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Interaction == nil {
		return ContentBlock{}, false
	}
	interaction := wrapper.Interaction
	return ContentBlock{
		Type:      "interaction",
		RequestID: firstNonEmpty(interaction.RequestID, interaction.ID),
		Kind:      strings.TrimSpace(interaction.Kind),
		State:     strings.TrimSpace(interaction.State),
		Text:      strings.TrimSpace(interaction.Text),
		Prompt:    strings.TrimSpace(interaction.Prompt),
		Options:   append([]string(nil), interaction.Options...),
		Action:    strings.TrimSpace(interaction.Action),
		Metadata:  cloneRawJSON(interaction.Metadata),
	}, true
}

func openCodeExportDirectory(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var export struct {
		Info struct {
			Directory string `json:"directory"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &export); err != nil {
		return ""
	}
	return export.Info.Directory
}

func cleanOpenCodeWorkDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func openCodeSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func mergeOpenCodeSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultOpenCodeSearchPaths(), extraPaths)
}

// DefaultOpenCodeSearchPaths returns Gas City's default OpenCode transcript
// mirror directory.
func DefaultOpenCodeSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "share", "gascity", "opencode-transcripts")}
}

type openCodeExport struct {
	Info struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	} `json:"info"`
	Messages []json.RawMessage `json:"messages"`
}

type openCodeMessage struct {
	Info  openCodeMessageInfo `json:"info"`
	Parts []openCodePart      `json:"parts"`
}

type openCodeMessageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ParentID  string `json:"parentID"`
	Time      struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type openCodePart struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Summary   string          `json:"summary"`
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	State     json.RawMessage `json:"state"`
	RequestID string          `json:"request_id"`
	Kind      string          `json:"kind"`
	Prompt    string          `json:"prompt"`
	Options   []string        `json:"options"`
	Action    string          `json:"action"`
	Metadata  json.RawMessage `json:"metadata"`
}

type openCodeToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  json.RawMessage `json:"error"`
}
