package sessionlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// geminiScanBufferCap caps one JSONL line while scanning gemini chat
// recordings. The default scanner buffer is 64KB; gemini records can be
// large (tool results accumulate into one re-appended message, and a
// $set.messages patch carries the entire conversation). Variable so tests
// can lower it to pin the oversized-line tolerance cheaply.
var geminiScanBufferCap = 50 * 1024 * 1024

// geminiScanInitialBuffer returns the initial scanner buffer capacity for
// gemini JSONL scans, clamped to geminiScanBufferCap: bufio.Scanner's
// effective line cap is the LARGER of the max and the initial capacity, so
// the initial allocation must never exceed the configured cap.
func geminiScanInitialBuffer() int {
	const initial = 256 * 1024
	if geminiScanBufferCap < initial {
		return geminiScanBufferCap
	}
	return initial
}

// geminiSetPatch is the {"$set":{...}} metadata-patch line gemini-cli
// >=0.45 appends to JSONL chat recordings. Only the messages field matters
// for replay: a non-nil array means the CLI rewrote the whole conversation
// (updateMessagesFromHistory) and accumulated state must be replaced.
type geminiSetPatch struct {
	Set *struct {
		Messages []json.RawMessage `json:"messages"`
	} `json:"$set"`
}

// ReadGeminiFile reads a Gemini session recording and converts it to the
// standard Session format used by GC session transcripts.
//
// Legacy gemini-cli stores sessions at
// ~/.gemini/tmp/<project>/chats/session-*.json as a single JSON object with
// a linear messages[] array. gemini-cli >=0.45 writes
// chats/session-*.jsonl instead: the first line is the metadata object,
// subsequent lines are message records (re-appended when a message is
// updated, e.g. once its token counts arrive — the last occurrence wins),
// {"$set":...} metadata patch lines — skipped, except a non-nil
// $set.messages array, which REPLACES the accumulated conversation (the
// CLI's updateMessagesFromHistory writes one on compaction/context-scrub
// and on initialize() after resume) — and {"$rewindTo":"<id>"} rewind
// records, which delete the target message and everything after it
// (matching gemini-cli's own replay).
func ReadGeminiFile(path string, _ int) (*Session, error) {
	if strings.HasSuffix(path, ".jsonl") {
		return readGeminiJSONLFile(path)
	}
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

// readGeminiJSONLFile parses the gemini-cli >=0.45 JSONL chat recording:
// line one is the metadata object (sessionId source), message records carry
// an id and are deduplicated last-occurrence-wins in their original
// position, and {"$set":...} patches plus malformed lines are tolerated.
// Two record kinds mirror gemini-cli's replay (loadConversationRecord):
// {"$rewindTo":"<id>"} deletes the target message and every later message
// (an unknown target clears the whole conversation so far), and a non-nil
// {"$set":{"messages":[...]}} array clears the accumulated state and
// replays the embedded messages in array order. An oversized line (a
// whole-conversation $set patch grows with conversation size and can exceed
// geminiScanBufferCap) stops the scan softly: everything parsed before it
// is returned with nil error, matching the tolerate-malformed-lines posture
// — gemini-cli's own replay and the legacy path have no per-line cap.
func readGeminiJSONLFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, geminiScanInitialBuffer()), geminiScanBufferCap)

	sessionID := ""
	var messages []*Entry
	// byID maps a message id to its index in messages so re-appended
	// records of one message replace the original in place.
	byID := make(map[string]int)
	idx := 0
	// appendMsg routes one message record (top-level line or $set-embedded
	// object) through parse + last-occurrence-wins dedup.
	appendMsg := func(raw json.RawMessage, id string) {
		entry := parseGeminiMessage(raw, idx)
		if entry == nil {
			return
		}
		if i, seen := byID[id]; seen {
			messages[i] = entry
			return
		}
		byID[id] = len(messages)
		messages = append(messages, entry)
		idx++
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
			RewindTo  string `json:"$rewindTo"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if sessionID == "" && strings.TrimSpace(probe.SessionID) != "" {
			sessionID = strings.TrimSpace(probe.SessionID)
		}
		if probe.RewindTo != "" {
			if i, ok := byID[probe.RewindTo]; ok {
				for id, idx := range byID {
					if idx >= i {
						delete(byID, id)
					}
				}
				messages = messages[:i]
			} else {
				messages = nil
				byID = make(map[string]int)
			}
			continue
		}
		if probe.ID == "" {
			// Metadata object or {"$set":...} patch line. A non-nil
			// $set.messages array replaces the whole conversation.
			var patch geminiSetPatch
			if err := json.Unmarshal(line, &patch); err == nil && patch.Set != nil && patch.Set.Messages != nil {
				messages = nil
				byID = make(map[string]int)
				for _, raw := range patch.Set.Messages {
					var embedded struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(raw, &embedded); err != nil || embedded.ID == "" {
						continue
					}
					appendMsg(append(json.RawMessage(nil), raw...), embedded.ID)
				}
			}
			continue
		}
		appendMsg(append(json.RawMessage(nil), line...), probe.ID)
	}
	// bufio.ErrTooLong is a soft stop (see the doc comment); any other scan
	// error still fails the read.
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, fmt.Errorf("scanning gemini session file: %w", err)
	}

	if sessionID == "" {
		sessionID = geminiSessionID(path)
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
// (~/.gemini/tmp/<project>/chats/session-*.json legacy recordings and
// session-*.jsonl gemini-cli >=0.45 recordings) for the most recently
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
		// Accept both the legacy single-JSON sessions (session-*.json) and
		// the gemini-cli >=0.45 JSONL sessions (session-*.jsonl). The
		// "session-" prefix excludes subagent <id>.jsonl recordings.
		if !strings.HasPrefix(entry.Name(), "session-") ||
			(!strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".jsonl")) {
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
