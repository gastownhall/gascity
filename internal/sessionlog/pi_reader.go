package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadPiFile reads a Pi Coding Agent native JSONL session file and converts it
// to the standard Session format used by gc session logs.
func ReadPiFile(path string, tailCompactions int) (*Session, error) {
	entries, sessionID, diagnostics, err := parsePiFileDetailed(path)
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		sessionID = piSessionID(path)
	}

	messages := piActiveBranch(entries, sessionID)
	orphanedToolUseIDs := findOrphanedToolUses(messages, collectAllToolResultIDs(messages))
	sess := &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: orphanedToolUseIDs,
		Diagnostics:        diagnostics,
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

// FindPiSessionFile searches Pi JSONL session directories for the most
// recently modified session whose header cwd matches workDir.
func FindPiSessionFile(searchPaths []string, workDir string) string {
	workDir = cleanPiWorkDir(workDir)
	if workDir == "" {
		return ""
	}

	var (
		bestPath string
		bestTime time.Time
	)
	for _, root := range mergePiSearchPaths(searchPaths) {
		path := findPiSessionFileIn(root, workDir)
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

func findPiSessionFileIn(root, workDir string) string {
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
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
			return nil
		}
		if cleanPiWorkDir(piSessionCWD(path)) != workDir {
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

func parsePiFileDetailed(path string) ([]piEntry, string, SessionDiagnostics, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", SessionDiagnostics{}, fmt.Errorf("opening pi session file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var (
		entries                   []piEntry
		diagnostics               SessionDiagnostics
		sessionID                 string
		lastNonEmptyLineMalformed bool
	)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry piEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		entry.Raw = append(json.RawMessage(nil), line...)
		if entry.Type == "session" && sessionID == "" {
			sessionID = strings.TrimSpace(entry.ID)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", SessionDiagnostics{}, fmt.Errorf("scanning pi session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed
	return entries, sessionID, diagnostics, nil
}

func piActiveBranch(entries []piEntry, sessionID string) []*Entry {
	nodeByID := make(map[string]piEntry, len(entries))
	var leafID string
	for _, entry := range entries {
		if entry.Type == "session" || strings.TrimSpace(entry.ID) == "" {
			continue
		}
		nodeByID[entry.ID] = entry
		leafID = entry.ID
	}
	if leafID == "" {
		return nil
	}

	var branch []piEntry
	seen := map[string]bool{}
	for id := leafID; id != ""; {
		if seen[id] {
			break
		}
		seen[id] = true
		entry, ok := nodeByID[id]
		if !ok {
			break
		}
		branch = append(branch, entry)
		id = strings.TrimSpace(entry.ParentID)
	}
	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}

	out := make([]*Entry, 0, len(branch))
	for _, entry := range branch {
		converted := convertPiEntry(entry, sessionID)
		if converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

func convertPiEntry(entry piEntry, sessionID string) *Entry {
	switch entry.Type {
	case "message":
		return convertPiMessage(entry, sessionID)
	case "compaction":
		return &Entry{
			UUID:       strings.TrimSpace(entry.ID),
			ParentUUID: strings.TrimSpace(entry.ParentID),
			Type:       "system",
			Subtype:    "compact_boundary",
			Message:    mustMarshal(MessageContent{Role: "system", Content: mustMarshal(strings.TrimSpace(entry.Summary))}),
			Timestamp:  parsePiTimestamp(entry.Timestamp),
			SessionID:  sessionID,
			Raw:        cloneRawJSON(entry.Raw),
			CompactMetadata: &CompactMeta{
				Trigger:   "pi",
				PreTokens: entry.TokensBefore,
			},
		}
	case "custom_message":
		content := normalizePiContent(entry.Content)
		return &Entry{
			UUID:       strings.TrimSpace(entry.ID),
			ParentUUID: strings.TrimSpace(entry.ParentID),
			Type:       "system",
			Message:    mustMarshal(MessageContent{Role: "system", Content: content}),
			Timestamp:  parsePiTimestamp(entry.Timestamp),
			SessionID:  sessionID,
			Raw:        cloneRawJSON(entry.Raw),
		}
	default:
		return nil
	}
}

func convertPiMessage(entry piEntry, sessionID string) *Entry {
	role := strings.TrimSpace(entry.Message.Role)
	if role == "" {
		role = "assistant"
	}
	convertedType := piEntryTypeForRole(role)
	blocks := piMessageBlocks(entry.Message)
	message := mustMarshal(MessageContent{Role: piMessageRole(role), Content: piMessageContent(entry.Message.Content, blocks)})
	return &Entry{
		UUID:       strings.TrimSpace(entry.ID),
		ParentUUID: strings.TrimSpace(entry.ParentID),
		Type:       convertedType,
		Message:    message,
		ToolUseID:  strings.TrimSpace(entry.Message.ToolCallID),
		Timestamp:  firstPiTimestamp(entry.Message.Timestamp, parsePiTimestamp(entry.Timestamp)),
		SessionID:  sessionID,
		Raw:        cloneRawJSON(entry.Raw),
	}
}

func piEntryTypeForRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return "user"
	case "toolresult":
		return "tool_result"
	case "custom", "bashexecution":
		return "system"
	default:
		return "assistant"
	}
}

func piMessageRole(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "toolResult") {
		return "user"
	}
	if strings.EqualFold(strings.TrimSpace(role), "bashExecution") {
		return "system"
	}
	return strings.ToLower(strings.TrimSpace(role))
}

func piMessageContent(raw json.RawMessage, blocks []ContentBlock) json.RawMessage {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return mustMarshal(blocks[0].Text)
	}
	if len(blocks) > 0 {
		return mustMarshal(blocks)
	}
	return normalizePiContent(raw)
}

func normalizePiContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return mustMarshal("")
	}
	return cloneRawJSON(raw)
}

func piMessageBlocks(message piMessage) []ContentBlock {
	if strings.EqualFold(strings.TrimSpace(message.Role), "toolResult") {
		return []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: strings.TrimSpace(message.ToolCallID),
			Name:      strings.TrimSpace(message.ToolName),
			Content:   cloneRawJSON(message.Content),
			IsError:   message.IsError,
		}}
	}

	var text string
	if err := json.Unmarshal(message.Content, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: text}}
	}

	var parts []piContentBlock
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		return nil
	}
	blocks := make([]ContentBlock, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part.Type)) {
		case "text":
			text := strings.TrimSpace(part.Text)
			if text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		case "thinking":
			text := strings.TrimSpace(firstNonEmpty(part.Thinking, part.Text))
			if text != "" {
				blocks = append(blocks, ContentBlock{Type: "thinking", Text: text})
			}
		case "toolcall":
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    strings.TrimSpace(part.ID),
				Name:  strings.TrimSpace(part.Name),
				Input: cloneRawJSON(part.Arguments),
			})
		case "interaction":
			blocks = append(blocks, ContentBlock{
				Type:      "interaction",
				ID:        strings.TrimSpace(part.ID),
				RequestID: strings.TrimSpace(part.RequestID),
				Kind:      strings.TrimSpace(part.Kind),
				State:     strings.TrimSpace(part.State),
				Text:      strings.TrimSpace(part.Text),
				Prompt:    strings.TrimSpace(part.Prompt),
				Options:   append([]string(nil), part.Options...),
				Action:    strings.TrimSpace(part.Action),
				Metadata:  cloneRawJSON(part.Metadata),
			})
		case "image":
			blocks = append(blocks, ContentBlock{Type: "image"})
		}
	}
	return blocks
}

func firstPiTimestamp(millis int64, fallback time.Time) time.Time {
	if millis > 0 {
		return time.UnixMilli(millis)
	}
	return fallback
}

func parsePiTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func piSessionCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return ""
	}
	var header struct {
		Type string `json:"type"`
		CWD  string `json:"cwd"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return ""
	}
	if header.Type != "session" {
		return ""
	}
	return header.CWD
}

func cleanPiWorkDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func piSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// DefaultPiSearchPaths returns the default Pi session directory.
func DefaultPiSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".pi", "agent", "sessions")}
}

type piEntry struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	ParentID         string          `json:"parentId"`
	Timestamp        string          `json:"timestamp"`
	CWD              string          `json:"cwd"`
	Message          piMessage       `json:"message"`
	Summary          string          `json:"summary"`
	TokensBefore     int             `json:"tokensBefore"`
	CustomType       string          `json:"customType"`
	Content          json.RawMessage `json:"content"`
	FirstKeptEntryID string          `json:"firstKeptEntryId"`
	Raw              json.RawMessage `json:"-"`
}

type piMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Timestamp  int64           `json:"timestamp"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
}

type piContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	RequestID string          `json:"request_id"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Prompt    string          `json:"prompt"`
	Options   []string        `json:"options"`
	Action    string          `json:"action"`
	Metadata  json.RawMessage `json:"metadata"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
