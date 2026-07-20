package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ErrAmbiguousPiSessionFile reports multiple Pi transcripts for one workdir.
var ErrAmbiguousPiSessionFile = errors.New("ambiguous pi session file")

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

	dag := BuildDag(piEntriesToStandard(entries, sessionID))
	messages := dag.ActiveBranch
	sess := &Session{
		ID:                 sessionID,
		Messages:           messages,
		OrphanedToolUseIDs: dag.OrphanedToolUseIDs,
		HasBranches:        dag.HasBranches,
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

// ResetPiInterruptedTurn removes Pi's interrupted tail before a replacement
// prompt is sent. Native transcript reset failures are returned; the optional
// mirror update is best effort and logs path-bearing diagnostics on failure.
func ResetPiInterruptedTurn(path, mirrorDir string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	truncated, changed := truncatePiSessionAfterLastUserMessage(data)
	perm := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if changed {
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, truncated, perm); err != nil {
			return err
		}
	}
	if strings.TrimSpace(mirrorDir) == "" {
		return nil
	}
	sessionID := safePiSessionID(piSessionIDFromData(data))
	if sessionID == "" {
		sessionID = safePiSessionID(piSessionID(path))
	}
	if sessionID == "" {
		return nil
	}
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		log.Printf("sessionlog: pi mirror reset mkdir_failed path=%q err=%v", mirrorDir, err)
		return nil
	}
	mirrorPath := filepath.Join(mirrorDir, sessionID+".jsonl")
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, mirrorPath, truncated, perm); err != nil {
		log.Printf("sessionlog: pi mirror reset write_failed path=%q err=%v", mirrorPath, err)
		return nil
	}
	return nil
}

// FindPiSessionFile searches Pi JSONL session directories for the most
// recently modified session whose header cwd matches workDir. If more than one
// matching session exists, it returns an empty path so callers do not guess
// which same-workdir transcript is safe to mutate.
func FindPiSessionFile(searchPaths []string, workDir string) string {
	path, err := FindPiSessionFileStrict(searchPaths, workDir)
	if err != nil {
		return ""
	}
	return path
}

// FindPiSessionFileStrict searches Pi JSONL session directories for one
// unambiguous session whose header cwd matches workDir.
func FindPiSessionFileStrict(searchPaths []string, workDir string) (string, error) {
	candidates := findPiSessionCandidates(searchPaths, workDir)
	switch len(candidates) {
	case 0:
		return "", nil
	case 1:
		return candidates[0].path, nil
	default:
		return "", ErrAmbiguousPiSessionFile
	}
}

// FindPiSessionFileByID searches Pi JSONL session directories for the session
// whose header cwd and provider session ID match the supplied values.
func FindPiSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	for _, candidate := range findPiSessionCandidates(searchPaths, workDir) {
		if candidate.sessionID == sessionID {
			return candidate.path
		}
	}
	return ""
}

type piSessionCandidate struct {
	path      string
	sessionID string
	modTime   time.Time
}

func findPiSessionCandidates(searchPaths []string, workDir string) []piSessionCandidate {
	workDir = cleanPiWorkDir(workDir)
	if workDir == "" {
		return nil
	}

	var candidates []piSessionCandidate
	for _, root := range mergePiSearchPaths(searchPaths) {
		candidates = append(candidates, findPiSessionCandidatesIn(root, workDir)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	return candidates
}

func findPiSessionCandidatesIn(root, workDir string) []piSessionCandidate {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var candidates []piSessionCandidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
			return nil
		}
		sessionID, cwd := piSessionHeader(path)
		if cleanPiWorkDir(cwd) != workDir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if sessionID == "" {
			sessionID = piSessionID(path)
		}
		candidates = append(candidates, piSessionCandidate{path: path, sessionID: sessionID, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil
	}
	return candidates
}

func parsePiFileDetailed(path string) ([]piEntry, string, SessionDiagnostics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", SessionDiagnostics{}, fmt.Errorf("opening pi session file: %w", err)
	}

	lines, diagnostics := parsePiEntryLines(data)
	entries := make([]piEntry, 0, len(lines))
	var sessionID string
	for _, line := range lines {
		entry := line.Entry
		if entry.Type == "session" && sessionID == "" {
			sessionID = strings.TrimSpace(entry.ID)
		}
		entries = append(entries, entry)
	}
	return entries, sessionID, diagnostics, nil
}

func piEntriesToStandard(entries []piEntry, sessionID string) []*Entry {
	out := make([]*Entry, 0, len(entries))
	for _, entry := range entries {
		converted := convertPiEntry(entry, sessionID)
		if converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

func truncatePiSessionAfterLastUserMessage(data []byte) ([]byte, bool) {
	lines, diagnostics := parsePiEntryLines(data)
	lastUser := -1
	for i, line := range lines {
		if piEntryRole(line.Entry, "user") {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return data, false
	}
	if completedEnd, ok := piCompletedTurnEndAfterUser(lines, lastUser); ok {
		if diagnostics.MalformedTail {
			return data[:completedEnd], true
		}
		return data, false
	}
	cutOffset := lines[lastUser].Start
	if cutOffset >= len(data) {
		return data, false
	}
	return data[:cutOffset], true
}

type piEntryLine struct {
	Entry piEntry
	Start int
	End   int
}

func parsePiEntryLines(data []byte) ([]piEntryLine, SessionDiagnostics) {
	var lines []piEntryLine
	offset := 0
	var diagnostics SessionDiagnostics
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			entry, ok := parsePiEntryLine(trimmed)
			if ok {
				lines = append(lines, piEntryLine{
					Entry: entry,
					Start: offset,
					End:   offset + len(line),
				})
				diagnostics.MalformedTail = false
			} else {
				diagnostics.MalformedLineCount++
				diagnostics.MalformedTail = true
			}
		}
		offset += len(line)
	}
	return lines, diagnostics
}

func parsePiEntryLine(line []byte) (piEntry, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return piEntry{}, false
	}
	var entry piEntry
	if err := json.Unmarshal(trimmed, &entry); err != nil {
		return piEntry{}, false
	}
	entry.Raw = append(json.RawMessage(nil), trimmed...)
	return entry, true
}

func piEntryRole(entry piEntry, role string) bool {
	return strings.EqualFold(strings.TrimSpace(entry.Type), "message") &&
		strings.EqualFold(strings.TrimSpace(entry.Message.Role), role)
}

func piCompletedTurnEndAfterUser(lines []piEntryLine, lastUser int) (int, bool) {
	openToolCalls := map[string]struct{}{}
	for i := lastUser + 1; i < len(lines); i++ {
		entry := lines[i].Entry
		if !strings.EqualFold(strings.TrimSpace(entry.Type), "message") {
			continue
		}
		switch {
		case piEntryRole(entry, "assistant"):
			for _, toolCallID := range piAssistantToolCallIDs(entry) {
				openToolCalls[toolCallID] = struct{}{}
			}
			if len(openToolCalls) == 0 && strings.TrimSpace(entry.Message.StopReason) != "" {
				return lines[i].End, true
			}
		case piEntryRole(entry, "toolResult"):
			toolCallID := strings.TrimSpace(entry.Message.ToolCallID)
			if toolCallID != "" {
				delete(openToolCalls, toolCallID)
			}
		}
	}
	return 0, false
}

func piAssistantToolCallIDs(entry piEntry) []string {
	if !piEntryRole(entry, "assistant") {
		return nil
	}
	blocks := piMessageBlocks(entry.Message)
	ids := make([]string, 0, len(blocks))
	for i, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		id := strings.TrimSpace(block.ID)
		if id == "" {
			// Missing tool IDs are intentionally unmatched by toolResult entries
			// without toolCallId, so the turn remains incomplete and is truncated.
			id = fmt.Sprintf("line-tool-call-%d", i)
		}
		ids = append(ids, id)
	}
	return ids
}

func piSessionIDFromData(data []byte) string {
	lines, _ := parsePiEntryLines(data)
	for _, line := range lines {
		if line.Entry.Type == "session" {
			return strings.TrimSpace(line.Entry.ID)
		}
	}
	return ""
}

func safePiSessionID(sessionID string) string {
	// Keep this filename contract in sync with safeSessionID in the managed Pi
	// hook overlay.
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '.' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
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
	case "custom", "bashexecution", "pythonexecution":
		return "system"
	default:
		return "assistant"
	}
}

func piMessageRole(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "toolResult") {
		return "user"
	}
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "bashexecution", "pythonexecution":
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
	switch strings.ToLower(strings.TrimSpace(message.Role)) {
	case "bashexecution":
		return []ContentBlock{piExecutionResultBlock("bash", message)}
	case "pythonexecution":
		return []ContentBlock{piExecutionResultBlock("python", message)}
	}

	if strings.EqualFold(strings.TrimSpace(message.Role), "toolResult") {
		return []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: strings.TrimSpace(message.ToolCallID),
			Name:      strings.TrimSpace(message.ToolName),
			Content:   piToolResultContent(message.Content),
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
				Input: piNeutralToolObject(part.Arguments),
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
			imageURL := strings.TrimSpace(part.ImageURL)
			if strings.HasPrefix(strings.ToLower(imageURL), "data:") {
				imageURL = ""
			}
			blocks = append(blocks, ContentBlock{
				Type:     "image",
				FilePath: strings.TrimSpace(part.FilePath),
				ImageURL: imageURL,
				MIMEType: strings.TrimSpace(firstNonEmpty(part.MIMEType, part.MediaType)),
			})
		}
	}
	return blocks
}

type piExecutionResultContent struct {
	Command     string `json:"command,omitempty"`
	Code        string `json:"code,omitempty"`
	Output      string `json:"output,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Canceled    bool   `json:"canceled,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

func piExecutionResultBlock(name string, message piMessage) ContentBlock {
	content := piExecutionResultContent{
		Command:     strings.TrimSpace(message.Command),
		Code:        strings.TrimSpace(message.Code),
		Output:      strings.TrimSpace(message.Output),
		ExitCode:    message.ExitCode,
		Interrupted: message.Canceled || message.Interrupted,
		Canceled:    message.Canceled,
		Truncated:   message.Truncated,
	}
	if content.Output == "" {
		content.Output = structuredPiMessageText(message.Content)
	}
	isError := content.Interrupted
	if message.ExitCode != nil && *message.ExitCode != 0 {
		isError = true
	}
	return ContentBlock{
		Type:    "tool_result",
		Name:    name,
		Content: mustMarshal(content),
		IsError: isError,
	}
}

func piToolResultContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return mustMarshal(blocks)
	}
	return piNeutralToolObject(raw)
}

func piNeutralToolObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return piNeutralToolObject(json.RawMessage(encoded))
		}
		return mustMarshal(encoded)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return cloneRawJSON(raw)
	}
	neutral := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		neutral[piNeutralToolKey(key)] = cloneRawJSON(value)
	}
	return mustMarshal(neutral)
}

func piNeutralToolKey(key string) string {
	switch strings.TrimSpace(key) {
	case "filePath", "filepath", "path", "file":
		return "file_path"
	case "oldString", "oldStr":
		return "old_string"
	case "newString", "newStr":
		return "new_string"
	case "exitCode":
		return "exit_code"
	case "durationMs":
		return "duration_ms"
	case "statusCode", "code":
		return "status_code"
	case "codeText", "statusText":
		return "status_text"
	case "numFiles":
		return "num_files"
	case "numResults":
		return "num_results"
	case "taskId", "backgroundTaskId", "bashId", "agentId":
		return "task_id"
	case "taskType", "taskKind", "subagentType", "agentType":
		return "task_type"
	case "taskStatus":
		return "task_status"
	case "oldTodos":
		return "old_todos"
	case "newTodos":
		return "new_todos"
	default:
		return key
	}
}

func structuredPiMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var object struct {
		Output  string `json:"output"`
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return strings.Join(nonEmptyPiStrings(
			object.Output,
			object.Stdout,
			object.Stderr,
			object.Text,
			object.Content,
		), "\n")
	}
	return ""
}

func nonEmptyPiStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
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

func piSessionHeader(path string) (string, string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return "", ""
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		CWD  string `json:"cwd"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return "", ""
	}
	if header.Type != "session" {
		return "", ""
	}
	return strings.TrimSpace(header.ID), header.CWD
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
	Role        string          `json:"role"`
	Content     json.RawMessage `json:"content"`
	Timestamp   int64           `json:"timestamp"`
	StopReason  string          `json:"stopReason"`
	ToolCallID  string          `json:"toolCallId"`
	ToolName    string          `json:"toolName"`
	IsError     bool            `json:"isError"`
	Command     string          `json:"command"`
	Code        string          `json:"code"`
	Output      string          `json:"output"`
	ExitCode    *int            `json:"exitCode"`
	Canceled    bool            `json:"canceled"`
	Interrupted bool            `json:"interrupted"`
	Truncated   bool            `json:"truncated"`
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
	FilePath  string          `json:"file_path"`
	ImageURL  string          `json:"image_url"`
	MIMEType  string          `json:"mime_type"`
	MediaType string          `json:"media_type"`
}
