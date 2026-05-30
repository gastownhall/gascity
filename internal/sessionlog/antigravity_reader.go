package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
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
	StepIndex    int              `json:"step_index"`
	Source       string           `json:"source"`
	Type         string           `json:"type"`
	Status       string           `json:"status"`
	CreatedAt    string           `json:"created_at"`
	Content      string           `json:"content"`
	Thinking     string           `json:"thinking"`
	ToolCalls    []agyToolCall    `json:"tool_calls"`
	Interactions []agyInteraction `json:"interactions"`
	ToolCallID   string           `json:"tool_call_id"`
	ToolCallIDJS string           `json:"toolCallId"`
	CallID       string           `json:"call_id"`
}

type agyToolCall struct {
	ID           string          `json:"id"`
	ToolCallID   string          `json:"tool_call_id"`
	ToolCallIDJS string          `json:"toolCallId"`
	CallID       string          `json:"call_id"`
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args"`
}

// agyInteraction mirrors the gemini-family interaction record carried on agy
// trajectory entries that pause for an approval or other human decision.
type agyInteraction struct {
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

// ReadAntigravityFilePage parses an agy trajectory JSONL log and applies
// message-ID pagination using the stable agy-N entry IDs emitted by the reader.
func ReadAntigravityFilePage(path string, tailCompactions int, beforeMessageID, afterMessageID string) (*Session, error) {
	sess, err := readAntigravityFile(path, false)
	if err != nil {
		return nil, err
	}
	paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, beforeMessageID, afterMessageID)
	sess.Messages = paginated
	sess.Pagination = info
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

// ReadAntigravityFileRawPage parses an agy trajectory JSONL log without
// display type filtering and applies message-ID pagination.
func ReadAntigravityFileRawPage(path string, tailCompactions int, beforeMessageID, afterMessageID string) (*Session, error) {
	sess, err := readAntigravityFile(path, true)
	if err != nil {
		return nil, err
	}
	paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, beforeMessageID, afterMessageID)
	sess.Messages = paginated
	sess.Pagination = info
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
	var lastUUID string
	var pendingCallIDs []string

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

		entry := convertAgyEntry(raw, line, &pendingCallIDs)
		if entry == nil {
			continue
		}
		if !rawMode && !displayTypes[entry.Type] {
			continue
		}
		if entry.ParentUUID == "" {
			entry.ParentUUID = lastUUID
		}
		lastUUID = entry.UUID
		messages = append(messages, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning antigravity session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	sessionID := antigravitySessionID(path)
	orphanedToolUseIDs := findOrphanedToolUses(messages, collectAllToolResultIDs(messages))
	if len(orphanedToolUseIDs) == 0 {
		orphanedToolUseIDs = nil
	}

	return &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: orphanedToolUseIDs,
		Diagnostics:        diagnostics,
	}, nil
}

func convertAgyEntry(raw agyLogEntry, rawLine []byte, pendingCallIDs *[]string) *Entry {
	ts, _ := time.Parse(time.RFC3339, raw.CreatedAt)
	uuid := fmt.Sprintf("agy-%d", raw.StepIndex)

	switch raw.Type {
	case "USER_INPUT":
		content := unwrapAgyContent(raw.Content)
		if interactionBlocks := agyInteractionBlocks(raw.Interactions); len(interactionBlocks) > 0 {
			blocks := make([]ContentBlock, 0, 1+len(interactionBlocks))
			if content != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: content})
			}
			blocks = append(blocks, interactionBlocks...)
			return &Entry{
				UUID:      uuid,
				Type:      "user",
				Timestamp: ts,
				Message: mustMarshal(MessageContent{
					Role:    "user",
					Content: mustMarshal(blocks),
				}),
				Raw: append(json.RawMessage(nil), rawLine...),
			}
		}
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
		*pendingCallIDs = (*pendingCallIDs)[:0]
		for i, tc := range raw.ToolCalls {
			callID := agyToolCallID(tc, fmt.Sprintf("call-%d-%d", raw.StepIndex, i))
			*pendingCallIDs = append(*pendingCallIDs, callID)
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  tc.Name,
				Input: tc.Args,
			})
		}
		blocks = append(blocks, agyInteractionBlocks(raw.Interactions)...)
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
		callID := consumeAgyPendingCallID(pendingCallIDs, agyResultCallID(raw))
		if callID == "" {
			return agySystemEntry(raw, rawLine, ts, uuid)
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
		return agySystemEntry(raw, rawLine, ts, uuid)
	}
}

func agyToolCallID(tc agyToolCall, fallback string) string {
	return firstTrimmedNonEmpty(tc.ID, tc.ToolCallID, tc.ToolCallIDJS, tc.CallID, fallback)
}

func agyResultCallID(raw agyLogEntry) string {
	return firstTrimmedNonEmpty(raw.ToolCallID, raw.ToolCallIDJS, raw.CallID)
}

func consumeAgyPendingCallID(pendingCallIDs *[]string, preferred string) string {
	if preferred != "" {
		for i, callID := range *pendingCallIDs {
			if callID == preferred {
				*pendingCallIDs = append((*pendingCallIDs)[:i], (*pendingCallIDs)[i+1:]...)
				return preferred
			}
		}
		return ""
	}
	if len(*pendingCallIDs) == 0 {
		return ""
	}
	callID := (*pendingCallIDs)[0]
	*pendingCallIDs = (*pendingCallIDs)[1:]
	return callID
}

func firstTrimmedNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func agySystemEntry(raw agyLogEntry, rawLine []byte, ts time.Time, uuid string) *Entry {
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

func unwrapAgyContent(s string) string {
	// Cleans initial wrapped JSON strings if any, or returns literal.
	var inner string
	if err := json.Unmarshal([]byte(s), &inner); err == nil {
		return inner
	}
	return s
}

// agyInteractionBlocks converts agy interaction records into canonical
// interaction content blocks so the worker layer can normalize pending and
// resolved human-decision state from the trajectory transcript.
func agyInteractionBlocks(interactions []agyInteraction) []ContentBlock {
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

// antigravitySessionID derives the stable conversation identity for an agy
// transcript. Real agy trajectories live at
// <brain>/<conversationID>/.system_generated/logs/transcript.jsonl, so the
// conversation directory — not the constant "transcript" file name — is the
// session identity. Flat fixture paths fall back to the file base name.
func antigravitySessionID(path string) string {
	logsDir := filepath.Dir(path)
	if filepath.Base(logsDir) == "logs" {
		genDir := filepath.Dir(logsDir)
		if filepath.Base(genDir) == ".system_generated" {
			convID := filepath.Base(filepath.Dir(genDir))
			if convID != "" && convID != "." && convID != string(filepath.Separator) {
				return convID
			}
		}
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// FindAntigravitySessionFileByID maps a conversation UUID directly into the
// nested brain layout. workDir is accepted for signature symmetry with the
// other provider keyed lookups; the agy conversation id is globally unique, so
// it is not needed to disambiguate the transcript path.
func FindAntigravitySessionFileByID(searchPaths []string, workDir, sessionID string) string {
	_ = workDir

	sessionID = safeAntigravitySessionDirName(sessionID)
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

	var bestID string
	var bestTime int64

	// The history index lives alongside the brain directory
	// (~/.gemini/antigravity-cli/history.jsonl next to .../brain). Honor any
	// configured search paths so discovery is hermetic, while the default path
	// still resolves the real home-directory index.
	for _, brainRoot := range mergeAntigravitySearchPaths(searchPaths) {
		bestID, bestTime = scanAntigravityHistory(filepath.Join(filepath.Dir(brainRoot), "history.jsonl"), workDir, bestID, bestTime)
	}

	if bestID == "" {
		return ""
	}
	return FindAntigravitySessionFileByID(searchPaths, workDir, bestID)
}

// scanAntigravityHistory reads one history index file and returns the
// conversation id with the newest timestamp matching workDir, preserving any
// better match already found in a prior index.
func scanAntigravityHistory(historyPath, workDir, bestID string, bestTime int64) (string, int64) {
	f, err := os.Open(historyPath)
	if err != nil {
		return bestID, bestTime
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)
	for scanner.Scan() {
		var entry AntigravityHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if filepath.Clean(entry.Workspace) == workDir && entry.Timestamp > bestTime {
			bestTime = entry.Timestamp
			bestID = entry.ConversationID
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("sessionlog: antigravity history scan failed path=%q err=%v", historyPath, err)
	}
	return bestID, bestTime
}

func safeAntigravitySessionDirName(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return filepath.Base(sessionID)
}
