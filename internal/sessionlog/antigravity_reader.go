package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AntigravityHistoryEntry maps a historic conversation run to its local workspace.
type AntigravityHistoryEntry struct {
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversationId"`
	Timestamp      int64  `json:"timestamp"`
}

type agyLogEntry struct {
	StepIndex int           `json:"step_index"`
	Source    string        `json:"source"`
	Type      string        `json:"type"`
	Status    string        `json:"status"`
	CreatedAt string        `json:"created_at"`
	Content   string        `json:"content"`
	Thinking  string        `json:"thinking"`
	ToolCalls []agyToolCall `json:"tool_calls"`
}

type agyToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ReadAntigravityFile parses an agy trajectory JSONL log into standard Session turns.
func ReadAntigravityFile(path string, tailCompactions int) (*Session, error) {
	sess, err := readAntigravityFile(path, false)
	if err != nil {
		return nil, err
	}
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}
	return sess, nil
}

// ReadAntigravityFileRaw parses an agy trajectory JSONL log without display type filtering.
func ReadAntigravityFileRaw(path string, tailCompactions int) (*Session, error) {
	sess, err := readAntigravityFile(path, true)
	if err != nil {
		return nil, err
	}
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}
	return sess, nil
}

func readAntigravityFile(path string, rawMode bool) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	var messages []*Entry
	var diagnostics SessionDiagnostics

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	var lastNonEmptyLineMalformed bool
	var lastCallID string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw agyLogEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false

		entry := convertAgyEntry(raw, line, &lastCallID)
		if entry == nil {
			continue
		}
		if !rawMode && !displayTypes[entry.Type] {
			continue
		}
		messages = append(messages, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning antigravity session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))

	return &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: nil,
		Diagnostics:        diagnostics,
	}, nil
}

func convertAgyEntry(raw agyLogEntry, rawLine []byte, lastCallID *string) *Entry {
	ts, _ := time.Parse(time.RFC3339, raw.CreatedAt)
	uuid := fmt.Sprintf("agy-%d", raw.StepIndex)

	switch raw.Type {
	case "USER_INPUT":
		content := unwrapAgyContent(raw.Content)
		return &Entry{
			UUID:      uuid,
			Type:      "user",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "user",
				Content: mustMarshal(content),
			}),
			Raw: append(json.RawMessage(nil), rawLine...),
		}
	case "PLANNER_RESPONSE":
		var blocks []ContentBlock
		if raw.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: raw.Content})
		}
		if raw.Thinking != "" {
			blocks = append(blocks, ContentBlock{Type: "thinking", Text: raw.Thinking})
		}
		for i, tc := range raw.ToolCalls {
			callID := fmt.Sprintf("call-%d-%d", raw.StepIndex, i)
			*lastCallID = callID
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  tc.Name,
				Input: tc.Args,
			})
		}
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "assistant",
				Content: mustMarshal(blocks),
			}),
			Raw: append(json.RawMessage(nil), rawLine...),
		}
	case "GENERIC", "RUN_COMMAND", "READ_FILE", "WRITE_FILE", "BROWSE_WEB", "SEARCH_WEB":
		// Standard executions and generic models results translate to tool results.
		callID := *lastCallID
		if callID == "" {
			callID = fmt.Sprintf("call-%d-0", raw.StepIndex-1)
		}
		block := ContentBlock{
			Type:      "tool_result",
			ToolUseID: callID,
			Content:   mustMarshal(raw.Content),
		}
		return &Entry{
			UUID:      uuid,
			Type:      "result",
			Timestamp: ts,
			ToolUseID: callID,
			Message: mustMarshal(MessageContent{
				Role:    "user",
				Content: mustMarshal([]ContentBlock{block}),
			}),
			Raw: append(json.RawMessage(nil), rawLine...),
		}
	case "CONVERSATION_HISTORY":
		return &Entry{
			UUID:      uuid,
			Type:      "system",
			Subtype:   "init",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "system",
				Content: mustMarshal("Conversation History Initialized"),
			}),
			Raw: append(json.RawMessage(nil), rawLine...),
		}
	default:
		// Default system logs fallback to system turns.
		if raw.Content == "" {
			return nil
		}
		return &Entry{
			UUID:      uuid,
			Type:      "system",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "system",
				Content: mustMarshal(raw.Content),
			}),
			Raw: append(json.RawMessage(nil), rawLine...),
		}
	}
}

func unwrapAgyContent(s string) string {
	// Cleans initial wrapped JSON strings if any, or returns literal.
	var inner string
	if err := json.Unmarshal([]byte(s), &inner); err == nil {
		return inner
	}
	return s
}

// FindAntigravitySessionFileByID maps a conversation UUID directly into the nested brain layout.
func FindAntigravitySessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}

	// Check standard search bases (defaults to ~/.gemini/antigravity-cli/brain)
	for _, root := range mergeAntigravitySearchPaths(searchPaths) {
		path := filepath.Join(root, sessionID, ".system_generated", "logs", "transcript.jsonl")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// FindAntigravitySessionFile matches active workdirs against the global history index
// and returns the path of the most recently modified matching conversation's transcript.
func FindAntigravitySessionFile(searchPaths []string, workDir string) string {
	workDir = filepath.Clean(strings.TrimSpace(workDir))
	if workDir == "" {
		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	historyPath := filepath.Join(home, ".gemini", "antigravity-cli", "history.jsonl")
	f, err := os.Open(historyPath)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only file

	var bestID string
	var bestTime int64

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry AntigravityHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if filepath.Clean(entry.Workspace) == workDir {
			if entry.Timestamp > bestTime {
				bestTime = entry.Timestamp
				bestID = entry.ConversationID
			}
		}
	}

	if bestID == "" {
		return ""
	}
	return FindAntigravitySessionFileByID(searchPaths, workDir, bestID)
}
