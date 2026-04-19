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

// ReadGeminiFile reads a Gemini session JSON file and converts it to the
// standard Session format used by GC session transcripts.
//
// Gemini stores sessions at ~/.gemini/tmp/<project>/chats/session-*.json as a
// single JSON object with a linear messages[] array.
func ReadGeminiFile(path string, _ int) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
	for idx, rawMessage := range raw.Messages {
		entry := parseGeminiMessage(rawMessage, idx)
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

func parseGeminiMessage(rawMessage json.RawMessage, idx int) *Entry {
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
		uuid = deterministicGeminiID(rawMessage, idx)
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
					Content:   mustMarshal(output),
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
// (~/.gemini/tmp/<project>/chats/session-*.json) for the most recently
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

func findGeminiSessionFileIn(root, workDir string) string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ""
	}

	var candidates []string
	if candidate := geminiProjectDir(root, workDir); candidate != "" {
		candidates = append(candidates, candidate)
	}

	if geminiProjectRoot(root) == workDir {
		candidates = append(candidates, root)
	}

	entries, err := os.ReadDir(root)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if geminiProjectRoot(dir) == workDir {
				candidates = append(candidates, dir)
			}
		}
	}

	candidates = uniqueStrings(candidates)

	var (
		bestPath string
		bestTime time.Time
	)
	for _, candidate := range candidates {
		path := newestGeminiSessionInChats(filepath.Join(candidate, "chats"))
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

func newestGeminiSessionInChats(chatsDir string) string {
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
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
		if !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(chatsDir, entry.Name())
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
		return ""
	}
	return files[0].path
}

func geminiSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func deterministicGeminiID(_ json.RawMessage, idx int) string {
	return fmt.Sprintf("gemini-%d", idx)
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
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Result []struct {
		FunctionResponse struct {
			ID       string `json:"id"`
			Response struct {
				Output string `json:"output"`
			} `json:"response"`
		} `json:"functionResponse"`
	} `json:"result"`
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
