package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/shlex"
)

// ReadCodexFile reads a Codex JSONL session file and converts it to the
// standard Session format used by gc session logs.
//
// Codex entries use a different schema than Claude:
//   - session_meta: session initialization (skipped)
//   - event_msg: user messages, agent messages, reasoning, token counts
//   - response_item: messages, function calls, reasoning (preferred over event_msg)
//   - turn_context: per-turn configuration (skipped)
//
// Port of yepanywhere's CodexSessionReader.convertEntriesToMessages.
func ReadCodexFile(path string, _ int) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	var entries []codexEntry
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw codexRawEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if raw.Type == "" {
			continue
		}
		entries = append(entries, codexEntry{raw: raw, line: string(line)})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning codex session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	// Check if response_item entries contain user messages (preferred source).
	hasResponseItemUser := false
	for _, e := range entries {
		if e.raw.Type == "response_item" {
			var ri codexResponseItem
			if json.Unmarshal(e.raw.Payload, &ri) == nil && ri.Type == "message" && ri.Role == "user" {
				hasResponseItemUser = true
				break
			}
		}
	}
	patchApplyResults := collectCodexPatchApplyResults(entries)

	var messages []*Entry
	var lastUUID string
	toolContexts := make(map[string]codexToolCallContext)
	responseItemIDs := newStableSyntheticEntryIDSequence("codex")
	eventMsgIDs := newStableSyntheticEntryIDSequence("codex-event")

	for _, e := range entries {
		ts, _ := time.Parse(time.RFC3339Nano, e.raw.Timestamp)

		switch e.raw.Type {
		case "response_item":
			entry := convertResponseItem(e.raw.Payload, e.line, ts, patchApplyResults, toolContexts, responseItemIDs.ForRecord([]byte(e.line)))
			if entry != nil {
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)
			}

		case "event_msg":
			var em codexEventMsg
			if json.Unmarshal(e.raw.Payload, &em) != nil {
				continue
			}
			eventID := eventMsgIDs.ForRecord([]byte(e.line))
			switch em.Type {
			case "user_message":
				if hasResponseItemUser {
					continue // prefer response_item user messages
				}
				entry := &Entry{
					UUID:      eventID.ID("event_msg:" + em.Type),
					Type:      "user",
					Timestamp: ts,
					Message:   mustMarshal(MessageContent{Role: "user", Content: mustMarshal(em.Message)}),
					Raw:       json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)

			case "agent_message":
				// Skip — response_item has the complete text.
				// Only include if no response_items exist.
				if hasResponseItemUser {
					continue
				}
				entry := &Entry{
					UUID:      eventID.ID("event_msg:" + em.Type),
					Type:      "assistant",
					Timestamp: ts,
					Message: mustMarshal(MessageContent{
						Role:    "assistant",
						Content: mustMarshal([]ContentBlock{{Type: "text", Text: em.Message}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)

			case "agent_reasoning":
				entry := &Entry{
					UUID:      eventID.ID("event_msg:" + em.Type),
					Type:      "assistant",
					Timestamp: ts,
					Message: mustMarshal(MessageContent{
						Role:    "assistant",
						Content: mustMarshal([]ContentBlock{{Type: "thinking", Text: em.Text}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)

			case "error", "stream_error", "turn_aborted":
				systemEvent := codexSystemEvent(em)
				entry := &Entry{
					UUID:        eventID.ID("event_msg:" + em.Type),
					Type:        "system",
					Subtype:     systemEvent.Kind,
					Timestamp:   ts,
					SystemEvent: systemEvent,
					Message: mustMarshal(MessageContent{
						Role:    "system",
						Content: mustMarshal([]ContentBlock{{Type: "text", Text: systemEvent.Message}}),
					}),
					Raw: json.RawMessage(e.line),
				}
				entry.ParentUUID = lastUUID
				lastUUID = entry.UUID
				messages = append(messages, entry)

			default:
				continue
			}
		}
	}

	return &Session{
		ID:          codexSessionID(path),
		Messages:    messages,
		Diagnostics: diagnostics,
	}, nil
}

func convertResponseItem(payload json.RawMessage, rawLine string, ts time.Time, patchApplyResults map[string]json.RawMessage, toolContexts map[string]codexToolCallContext, syntheticID stableSyntheticEntryIDSource) *Entry {
	var ri codexResponseItem
	if json.Unmarshal(payload, &ri) != nil {
		return nil
	}

	uuid := syntheticID.ID("response_item:" + ri.Type)

	switch ri.Type {
	case "message":
		if ri.Role == "developer" {
			return nil
		}
		entryType := ri.Role
		if entryType == "" {
			entryType = "assistant"
		}
		content := codexResponseContentBlocks(ri.Content)
		return &Entry{
			UUID:      uuid,
			Type:      entryType,
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    entryType,
				Content: mustMarshal(content),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "reasoning":
		text := codexTextContents(ri.Summary)
		if strings.TrimSpace(text) == "" {
			text = codexTextContents(ri.Content)
		}
		signature := ""
		if strings.TrimSpace(ri.EncryptedContent) != "" {
			signature = "encrypted"
		}
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role:    "assistant",
				Content: mustMarshal([]ContentBlock{{Type: "thinking", Text: text, Signature: signature}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "function_call", "custom_tool_call":
		callID := firstNonEmpty(ri.CallID, ri.ID)
		input := codexToolCallInput(ri.Name, ri.Input, ri.Arguments)
		if callID != "" {
			toolContexts[callID] = codexToolCallContextFromInput(ri.Name, input)
		}
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role: "assistant",
				Content: mustMarshal([]ContentBlock{{
					Type:  "tool_use",
					ID:    callID,
					Name:  ri.Name,
					Input: input,
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "web_search_call":
		callID := firstNonEmpty(ri.CallID, ri.ID)
		input := codexWebSearchInput(ri.Input, ri.Query, ri.Action)
		if callID != "" {
			toolContexts[callID] = codexToolCallContextFromInput("web_search", input)
		}
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role: "assistant",
				Content: mustMarshal([]ContentBlock{{
					Type:  "tool_use",
					ID:    callID,
					Name:  "web_search",
					Input: input,
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "function_call_output", "custom_tool_call_output":
		callID := firstNonEmpty(ri.CallID, ri.ID)
		context := toolContexts[callID]
		content := codexToolResultContent(ri.Output, patchApplyResults[callID], context)
		return &Entry{
			UUID:      uuid,
			Type:      "tool_result",
			Timestamp: ts,
			ToolUseID: callID,
			Message: mustMarshal(MessageContent{
				Role: "tool",
				Content: mustMarshal([]ContentBlock{{
					Type:      "tool_result",
					ToolUseID: callID,
					Content:   content,
					IsError:   codexToolResultIsError(ri.Output, content, context),
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}

	case "interaction":
		requestID := firstNonEmpty(ri.RequestID, ri.ID)
		return &Entry{
			UUID:      uuid,
			Type:      "assistant",
			Timestamp: ts,
			Message: mustMarshal(MessageContent{
				Role: "assistant",
				Content: mustMarshal([]ContentBlock{{
					Type:      "interaction",
					RequestID: requestID,
					Kind:      ri.Kind,
					State:     ri.State,
					Text:      ri.Text,
					Prompt:    ri.Prompt,
					Options:   append([]string(nil), ri.Options...),
					Action:    codexActionString(ri.Action),
					Metadata:  cloneRawJSON(ri.Metadata),
				}}),
			}),
			Raw: json.RawMessage(rawLine),
		}
	}

	return nil
}

type codexToolCallContext struct {
	Name                 string
	Command              string
	FilePath             string
	Paths                []string
	Pattern              string
	Query                string
	ReadStartLine        int
	ReadEndLine          int
	ReadStripLineNumbers bool
	GrepCount            bool
}

func collectCodexPatchApplyResults(entries []codexEntry) map[string]json.RawMessage {
	results := make(map[string]json.RawMessage)
	for _, e := range entries {
		if e.raw.Type != "event_msg" {
			continue
		}
		var em codexEventMsg
		if json.Unmarshal(e.raw.Payload, &em) != nil || em.Type != "patch_apply_end" || strings.TrimSpace(em.CallID) == "" {
			continue
		}
		if result := codexPatchApplyResultContent(em); len(result) > 0 {
			results[em.CallID] = result
		}
	}
	return results
}

func codexPatchApplyResultContent(em codexEventMsg) json.RawMessage {
	patch, filePath := codexPatchFromChanges(em.Changes)
	if patch == "" && strings.TrimSpace(em.Stdout) == "" && strings.TrimSpace(em.Stderr) == "" {
		return nil
	}
	payload := struct {
		Output   string `json:"output,omitempty"`
		Stderr   string `json:"stderr,omitempty"`
		Patch    string `json:"patch,omitempty"`
		FilePath string `json:"file_path,omitempty"`
	}{
		Output:   em.Stdout,
		Stderr:   em.Stderr,
		Patch:    patch,
		FilePath: filePath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return raw
}

func codexToolCallInput(name string, input json.RawMessage, arguments json.RawMessage) json.RawMessage {
	if len(input) > 0 && string(input) != "null" {
		return codexNeutralToolInput(name, input)
	}
	if len(arguments) == 0 || string(arguments) == "null" {
		return nil
	}
	var argumentString string
	if json.Unmarshal(arguments, &argumentString) == nil {
		argumentString = strings.TrimSpace(argumentString)
		if argumentString == "" {
			return nil
		}
		if json.Valid([]byte(argumentString)) {
			return codexNeutralToolInput(name, json.RawMessage(argumentString))
		}
		return mustMarshal(argumentString)
	}
	return codexNeutralToolInput(name, arguments)
}

func codexNeutralToolInput(name string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return codexNeutralToolInput(name, json.RawMessage(encoded))
		}
		return mustMarshal(encoded)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return cloneRawJSON(raw)
	}
	neutral := make(map[string]json.RawMessage, len(object)+3)
	for key, value := range object {
		neutral[codexNeutralToolInputKey(key)] = cloneRawJSON(value)
	}
	if command := jsonStringValue(neutral["command"]); command != "" && codexCanDeriveShellInput(name) {
		codexAddShellDerivedInput(neutral, command)
	}
	return mustMarshal(neutral)
}

func codexNeutralToolInputKey(key string) string {
	switch strings.TrimSpace(key) {
	case "cmd", "shellCommand", "shell_command":
		return "command"
	case "filePath", "filepath", "path", "file":
		return "file_path"
	case "oldString", "oldStr":
		return "old_string"
	case "newString", "newStr":
		return "new_string"
	case "exitCode":
		return "exit_code"
	case "statusCode", "code":
		return "status_code"
	case "codeText", "statusText":
		return "status_text"
	case "durationMs":
		return "duration_ms"
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
	case "answerMap":
		return "answer_map"
	default:
		return key
	}
}

func codexWebSearchInput(rawInput json.RawMessage, query string, action json.RawMessage) json.RawMessage {
	neutral := make(map[string]json.RawMessage)
	var object map[string]json.RawMessage
	if json.Unmarshal(rawInput, &object) == nil {
		for key, value := range object {
			normalizedKey := codexNeutralToolInputKey(key)
			switch normalizedKey {
			case "query":
				if strings.TrimSpace(query) == "" {
					query = jsonStringValue(value)
				}
			case "action":
				if len(action) == 0 || string(action) == "null" {
					action = cloneRawJSON(value)
				}
			default:
				neutral[normalizedKey] = cloneRawJSON(value)
			}
		}
	}
	if strings.TrimSpace(query) != "" {
		neutral["query"] = mustMarshal(strings.TrimSpace(query))
	}
	if actionText := codexActionString(action); actionText != "" {
		neutral["action"] = mustMarshal(actionText)
	}
	if len(neutral) == 0 {
		return nil
	}
	return mustMarshal(neutral)
}

func codexActionString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return strings.TrimSpace(string(raw))
}

func codexCanDeriveShellInput(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "exec_command", "shell", "bash", "terminal":
		return true
	default:
		return false
	}
}

func codexAddShellDerivedInput(neutral map[string]json.RawMessage, command string) {
	args, err := shlex.Split(command)
	if err != nil || len(args) == 0 {
		return
	}
	switch args[0] {
	case "cat":
		if filePath := codexLastNonOptionArg(args[1:]); filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
	case "sed":
		if filePath := codexLastNonOptionArg(args[1:]); filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
	case "nl":
		if filePath := codexNLInputFile(args); filePath != "" {
			neutral["file_path"] = mustMarshal(filePath)
		}
	case "rg", "grep":
		pattern, paths := codexGrepPatternAndPaths(args)
		if pattern != "" {
			neutral["pattern"] = mustMarshal(pattern)
		}
		if len(paths) == 1 {
			neutral["file_path"] = mustMarshal(paths[0])
		}
		if len(paths) > 0 {
			neutral["paths"] = mustMarshal(paths)
		}
	}
}

func codexLastNonOptionArg(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		arg := strings.TrimSpace(args[i])
		if arg == "" || arg == "|" || strings.HasPrefix(arg, "-") || codexLooksLikeSedAddress(arg) {
			continue
		}
		return arg
	}
	return ""
}

func codexNLInputFile(args []string) string {
	pipeIndex := len(args)
	for i, arg := range args {
		if arg == "|" {
			pipeIndex = i
			break
		}
	}
	return codexLastNonOptionArg(args[1:pipeIndex])
}

func codexGrepPatternAndPaths(args []string) (string, []string) {
	if len(args) < 2 {
		return "", nil
	}
	var pattern string
	var paths []string
	skipNext := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--" {
			if i+1 < len(args) && pattern == "" {
				pattern = args[i+1]
				paths = append(paths, args[i+2:]...)
			}
			break
		}
		if strings.HasPrefix(arg, "-") {
			if codexGrepFlagTakesValue(arg) && i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		if pattern == "" {
			pattern = arg
			continue
		}
		paths = append(paths, arg)
	}
	return pattern, paths
}

func codexGrepCountCommand(command string) bool {
	args, err := shlex.Split(command)
	if err != nil || len(args) == 0 {
		return false
	}
	if args[0] != "rg" && args[0] != "grep" {
		return false
	}
	for _, arg := range args[1:] {
		switch arg {
		case "-c", "--count", "--count-matches":
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c") && !strings.Contains(arg[1:], "C") {
			return true
		}
	}
	return false
}

func codexGrepFlagTakesValue(flag string) bool {
	switch flag {
	case "-e", "--regexp", "-g", "--glob", "-t", "--type", "-m", "--max-count", "-A", "--after-context", "-B", "--before-context", "-C", "--context":
		return true
	default:
		return false
	}
}

func codexLooksLikeSedAddress(value string) bool {
	value = strings.TrimSpace(strings.TrimSuffix(value, "p"))
	if value == "" {
		return false
	}
	start, end, hasComma := strings.Cut(value, ",")
	if !hasComma {
		return codexPositiveInt(start)
	}
	return codexPositiveInt(start) && codexPositiveInt(end)
}

func codexPositiveInt(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != "0"
}

func codexShellReadStripsLineNumbers(command string) bool {
	args, err := shlex.Split(command)
	return err == nil && len(args) > 0 && args[0] == "nl"
}

func codexStripShellReadLineNumbers(content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	trailingNewline := strings.HasSuffix(normalized, "\n")
	lines := strings.Split(strings.TrimRight(normalized, "\n"), "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		digitCount := 0
		for digitCount < len(trimmed) && trimmed[digitCount] >= '0' && trimmed[digitCount] <= '9' {
			digitCount++
		}
		if digitCount == 0 {
			continue
		}
		rest := trimmed[digitCount:]
		if rest == "" {
			lines[i] = ""
			continue
		}
		if rest[0] == '\t' || rest[0] == ' ' {
			lines[i] = strings.TrimLeft(rest, " \t")
		}
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out
}

func codexShellReadRange(command string) (int, int) {
	args, err := shlex.Split(command)
	if err != nil || len(args) == 0 {
		return 0, 0
	}
	for _, arg := range args {
		start, end, ok := codexParseSedAddress(arg)
		if ok {
			return start, end
		}
	}
	return 0, 0
}

func codexParseSedAddress(value string) (int, int, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "p")
	if value == "" {
		return 0, 0, false
	}
	startText, endText, hasComma := strings.Cut(value, ",")
	if !hasComma {
		line, ok := codexParsePositiveInt(startText)
		if !ok {
			return 0, 0, false
		}
		return line, line, true
	}
	start, ok := codexParsePositiveInt(startText)
	if !ok {
		return 0, 0, false
	}
	end, ok := codexParsePositiveInt(endText)
	if !ok {
		return 0, 0, false
	}
	return start, end, true
}

func codexParsePositiveInt(value string) (int, bool) {
	out, ok := codexParseNonNegativeInt(value)
	return out, ok && out > 0
}

func codexParseNonNegativeInt(value string) (int, bool) {
	var out int
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
		out = out*10 + int(r-'0')
	}
	return out, true
}

func codexCountLines(content string) int {
	content = strings.TrimRight(content, "\r\n")
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func codexIsAllASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func codexFirstWhitespaceDelimitedToken(line string) string {
	for i, r := range line {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return line[:i]
		}
	}
	return line
}

func codexCompactStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func codexToolCallContextFromInput(name string, input json.RawMessage) codexToolCallContext {
	context := codexToolCallContext{Name: strings.TrimSpace(name)}
	var object map[string]json.RawMessage
	if json.Unmarshal(input, &object) != nil || len(object) == 0 {
		return context
	}
	context.Command = jsonStringValue(object["command"])
	context.FilePath = jsonStringValue(object["file_path"])
	context.Pattern = jsonStringValue(object["pattern"])
	context.Query = jsonStringValue(object["query"])
	_ = json.Unmarshal(object["paths"], &context.Paths)
	context.Paths = codexCompactStringSlice(context.Paths)
	if context.Command != "" {
		context.ReadStartLine, context.ReadEndLine = codexShellReadRange(context.Command)
		context.ReadStripLineNumbers = codexShellReadStripsLineNumbers(context.Command)
		context.GrepCount = codexGrepCountCommand(context.Command)
	}
	return context
}

func codexToolResultContent(output json.RawMessage, patchResult json.RawMessage, context codexToolCallContext) json.RawMessage {
	if len(patchResult) > 0 {
		return codexToolResultContentWithPatch(output, patchResult)
	}
	return codexNeutralToolResult(output, context)
}

func codexToolResultIsError(output json.RawMessage, content json.RawMessage, context codexToolCallContext) bool {
	exitCode, hasExitCode := codexToolResultExitCode(output, content)
	if codexSearchNoMatch(content, context, exitCode, hasExitCode) {
		return false
	}
	if codexToolResultExplicitError(output) || codexToolResultExplicitError(content) {
		return true
	}
	if hasExitCode && exitCode != 0 {
		return true
	}
	text := firstNonEmpty(codexOutputText(content), codexOutputText(output))
	return codexTextLooksLikeError(text)
}

func codexToolResultExitCode(values ...json.RawMessage) (int, bool) {
	for _, raw := range values {
		if code, ok := codexExitCodeFromRaw(raw, 0); ok {
			return code, true
		}
	}
	return 0, false
}

func codexExitCodeFromRaw(raw json.RawMessage, depth int) (int, bool) {
	if len(raw) == 0 || depth > 4 {
		return 0, false
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		trimmed := strings.TrimSpace(encoded)
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			return codexExitCodeFromRaw(json.RawMessage(trimmed), depth+1)
		}
		return codexExitCodeFromText(encoded)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && len(object) > 0 {
		for _, key := range []string{"exit_code", "exitCode"} {
			if code, ok := codexIntValue(object[key]); ok {
				return code, true
			}
		}
		for _, key := range []string{"metadata", "result", "tool_result", "provider_result"} {
			if code, ok := codexExitCodeFromRaw(object[key], depth+1); ok {
				return code, true
			}
		}
	}
	return 0, false
}

func codexExitCodeFromText(text string) (int, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"Exit code:", "Process exited with code"} {
			if rest, ok := strings.CutPrefix(trimmed, prefix); ok {
				if code, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					return code, true
				}
			}
		}
	}
	return 0, false
}

func codexIntValue(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var intValue int
	if json.Unmarshal(raw, &intValue) == nil {
		return intValue, true
	}
	var floatValue float64
	if json.Unmarshal(raw, &floatValue) == nil {
		return int(floatValue), true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, err := strconv.Atoi(strings.TrimSpace(text))
		return value, err == nil
	}
	return 0, false
}

func codexToolResultExplicitError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		trimmed := strings.TrimSpace(encoded)
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			return codexToolResultExplicitError(json.RawMessage(trimmed))
		}
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return false
	}
	for _, key := range []string{"is_error", "isError"} {
		var value bool
		if json.Unmarshal(object[key], &value) == nil && value {
			return true
		}
	}
	status := strings.ToLower(strings.TrimSpace(firstNonEmpty(jsonStringValue(object["status"]), jsonStringValue(object["state"]))))
	if status == "failed" || status == "error" {
		return true
	}
	for _, key := range []string{"metadata", "result", "tool_result", "provider_result"} {
		if codexToolResultExplicitError(object[key]) {
			return true
		}
	}
	return false
}

func codexSearchNoMatch(content json.RawMessage, context codexToolCallContext, exitCode int, hasExitCode bool) bool {
	if !hasExitCode || exitCode != 1 || (strings.TrimSpace(context.Pattern) == "" && strings.TrimSpace(context.Query) == "") {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(content, &object) != nil || len(object) == 0 {
		return false
	}
	numFiles, hasNumFiles := codexIntValue(object["num_files"])
	numResults, hasNumResults := codexIntValue(object["num_results"])
	contentText := jsonStringValue(object["content"])
	return hasNumFiles && numFiles == 0 && (!hasNumResults || numResults == 0) && strings.TrimSpace(contentText) == ""
}

func codexTextLooksLikeError(text string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(trimmed, "error:") || strings.HasPrefix(trimmed, "fatal:") || strings.HasPrefix(trimmed, "failed:") {
			return true
		}
	}
	return false
}

func codexNeutralToolResult(raw json.RawMessage, context codexToolCallContext) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		trimmed := strings.TrimSpace(encoded)
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			return codexNeutralToolResult(json.RawMessage(trimmed), context)
		}
		return codexNeutralTextToolResult(encoded, context)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return cloneRawJSON(raw)
	}
	neutral := make(map[string]json.RawMessage, len(object)+3)
	for key, value := range object {
		neutral[codexNeutralToolResultKey(key)] = cloneRawJSON(value)
	}
	codexAddContextToResult(neutral, context)
	payload := codexNeutralResultPayload(neutral)
	codexAddReadResultFields(neutral, payload, context)
	codexAddSearchResultFields(neutral, payload, context)
	return mustMarshal(neutral)
}

func codexNeutralTextToolResult(text string, context codexToolCallContext) json.RawMessage {
	payload := strings.TrimPrefix(codexCommandOutputPayload(text), "\n")
	neutral := make(map[string]json.RawMessage, 12)
	if payload != "" {
		neutral["output"] = mustMarshal(payload)
	}
	codexAddContextToResult(neutral, context)
	codexAddReadResultFields(neutral, payload, context)
	codexAddSearchResultFields(neutral, payload, context)
	if len(neutral) == 0 {
		return mustMarshal(text)
	}
	return mustMarshal(neutral)
}

func codexNeutralResultPayload(neutral map[string]json.RawMessage) string {
	for _, key := range []string{"content", "output", "stdout", "stderr", "result", "text", "error"} {
		if value := jsonStringValue(neutral[key]); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func codexAddReadResultFields(neutral map[string]json.RawMessage, payload string, context codexToolCallContext) {
	if strings.TrimSpace(context.FilePath) == "" || strings.TrimSpace(context.Pattern) != "" || strings.TrimSpace(context.Query) != "" {
		return
	}
	content := payload
	if context.ReadStripLineNumbers {
		content = codexStripShellReadLineNumbers(content)
	}
	if content != "" {
		neutral["content"] = mustMarshal(content)
	}
	numLines := codexCountLines(content)
	if context.ReadStartLine > 0 && context.ReadEndLine >= context.ReadStartLine {
		neutral["start_line"] = mustMarshal(context.ReadStartLine)
		neutral["total_lines"] = mustMarshal(context.ReadEndLine)
		numLines = context.ReadEndLine - context.ReadStartLine + 1
	}
	if numLines > 0 {
		neutral["num_lines"] = mustMarshal(numLines)
	}
}

func codexAddSearchResultFields(neutral map[string]json.RawMessage, payload string, context codexToolCallContext) {
	if strings.TrimSpace(context.Pattern) == "" && strings.TrimSpace(context.Query) == "" {
		return
	}
	content := payload
	if content != "" {
		neutral["content"] = mustMarshal(content)
	}
	mode := codexSearchResultMode(content, context)
	if mode != "" {
		neutral["mode"] = mustMarshal(mode)
	}
	filenames := codexSearchResultFilenamesForMode(content, mode)
	counts, countTotal := codexSearchResultCountsForMode(content, mode)
	if len(filenames) == 0 && len(counts) > 0 {
		filenames = codexCountResultFilenames(counts)
	}
	if len(filenames) > 0 {
		neutral["filenames"] = mustMarshal(filenames)
	}
	neutral["num_files"] = mustMarshal(len(filenames))
	if len(counts) > 0 {
		neutral["counts"] = mustMarshal(counts)
	}
	resultItems := codexSearchResultItems(content, context)
	if len(resultItems) > 0 {
		neutral["result_items"] = mustMarshal(resultItems)
	}
	switch {
	case context.Query != "" && context.Pattern == "":
		if len(resultItems) > 0 {
			neutral["num_results"] = mustMarshal(len(resultItems))
		} else {
			neutral["num_results"] = mustMarshal(codexCountSearchResults(content, filenames))
		}
	case mode == "count":
		neutral["num_results"] = mustMarshal(countTotal)
	case strings.TrimSpace(content) == "":
		neutral["num_results"] = mustMarshal(0)
	}
	if numLines := codexCountLines(content); numLines > 0 {
		neutral["num_lines"] = mustMarshal(numLines)
	} else if strings.TrimSpace(content) == "" {
		neutral["num_lines"] = mustMarshal(0)
	}
}

type codexStructuredArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func codexSearchResultMode(content string, context codexToolCallContext) string {
	if context.Query != "" && context.Pattern == "" {
		return "query"
	}
	if context.GrepCount {
		return "count"
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if normalized == "" {
		return "files_with_matches"
	}
	if codexLooksLikeGrepCountOutput(normalized) {
		return "count"
	}
	for _, line := range strings.Split(normalized, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) >= 3 && codexIsAllASCIIDigits(parts[1]) {
			return "content"
		}
	}
	return "files_with_matches"
}

func codexSearchResultFilenamesForMode(content string, mode string) []string {
	if mode == "count" {
		counts, _ := codexSearchResultCountsForMode(content, mode)
		return codexCountResultFilenames(counts)
	}
	filenames := codexSearchResultFilenames(content)
	if len(filenames) > 0 || mode != "files_with_matches" {
		return filenames
	}
	seen := make(map[string]struct{})
	for _, line := range strings.Split(content, "\n") {
		filename := strings.TrimSpace(line)
		if filename == "" || strings.ContainsAny(filename, " \t") {
			continue
		}
		seen[filename] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	filenames = make([]string, 0, len(seen))
	for filename := range seen {
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)
	return filenames
}

func codexSearchResultFilenames(content string) []string {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var filename string
		if strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			filename = strings.TrimRight(codexFirstWhitespaceDelimitedToken(line), ":")
		} else {
			var ok bool
			filename, _, ok = strings.Cut(line, ":")
			if !ok {
				continue
			}
		}
		filename = strings.TrimSpace(filename)
		if filename == "" || strings.ContainsAny(filename, " \t") {
			continue
		}
		seen[filename] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	filenames := make([]string, 0, len(seen))
	for filename := range seen {
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)
	return filenames
}

type codexSearchResultItem struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

func codexSearchResultItems(content string, context codexToolCallContext) []codexSearchResultItem {
	if strings.TrimSpace(context.Query) == "" || strings.TrimSpace(context.Pattern) != "" {
		return nil
	}
	seen := make(map[string]struct{})
	var items []codexSearchResultItem
	for _, line := range strings.Split(codexCommandOutputPayload(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rawToken := codexFirstWhitespaceDelimitedToken(line)
		itemURL := strings.TrimRight(rawToken, ":")
		if !codexIsHTTPURL(itemURL) {
			continue
		}
		if _, ok := seen[itemURL]; ok {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, rawToken)), ":"))
		title = strings.TrimSpace(strings.TrimPrefix(title, "-"))
		items = append(items, codexSearchResultItem{
			Title: title,
			URL:   itemURL,
		})
		seen[itemURL] = struct{}{}
	}
	return items
}

func codexIsHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func codexSearchResultCountsForMode(content string, mode string) ([]codexStructuredArgument, int) {
	if mode != "count" {
		return nil, 0
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(codexCommandOutputPayload(content), "\r\n", "\n"))
	if normalized == "" {
		return nil, 0
	}
	counts := make([]codexStructuredArgument, 0)
	total := 0
	for _, line := range strings.Split(normalized, "\n") {
		name, value, ok := codexParseGrepCountLine(line)
		if !ok {
			continue
		}
		counts = append(counts, codexStructuredArgument{Name: name, Value: value})
		if parsed, parsedOK := codexParseNonNegativeInt(value); parsedOK {
			total += parsed
		}
	}
	return counts, total
}

func codexLooksLikeGrepCountOutput(content string) bool {
	lines := strings.Split(content, "\n")
	seen := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, _, ok := codexParseGrepCountLine(line); !ok {
			return false
		}
		seen = true
	}
	return seen
}

func codexParseGrepCountLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	if value, ok := codexParseNonNegativeInt(line); ok {
		return "matches", fmt.Sprintf("%d", value), true
	}
	name, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", "", false
	}
	count, countOK := codexParseNonNegativeInt(value)
	if !countOK {
		return "", "", false
	}
	return name, fmt.Sprintf("%d", count), true
}

func codexCountResultFilenames(counts []codexStructuredArgument) []string {
	if len(counts) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, count := range counts {
		name := strings.TrimSpace(count.Name)
		if name == "" || name == "matches" {
			continue
		}
		seen[name] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	filenames := make([]string, 0, len(seen))
	for filename := range seen {
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)
	return filenames
}

func codexCountSearchResults(content string, filenames []string) int {
	normalized := strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if normalized == "" {
		return len(filenames)
	}
	count := 0
	for _, line := range strings.Split(normalized, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count == 0 {
		return len(filenames)
	}
	return count
}

func codexNeutralToolResultKey(key string) string {
	switch strings.TrimSpace(key) {
	case "filePath", "filepath", "path", "file":
		return "file_path"
	case "diff", "fileDiff":
		return "patch"
	case "exitCode":
		return "exit_code"
	case "statusCode", "code":
		return "status_code"
	case "codeText", "statusText":
		return "status_text"
	case "durationMs":
		return "duration_ms"
	case "numFiles":
		return "num_files"
	case "numResults":
		return "num_results"
	case "isImage":
		return "is_image"
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
	case "answerMap":
		return "answer_map"
	default:
		return key
	}
}

func codexAddContextToResult(neutral map[string]json.RawMessage, context codexToolCallContext) {
	if context.FilePath != "" {
		neutral["file_path"] = mustMarshal(context.FilePath)
	}
	if context.Pattern != "" {
		neutral["pattern"] = mustMarshal(context.Pattern)
	}
	if context.Query != "" {
		neutral["query"] = mustMarshal(context.Query)
	}
}

func codexCommandOutputPayload(content string) string {
	if after, ok := strings.CutPrefix(content, "Output:\n"); ok {
		return after
	}
	if strings.TrimSpace(content) == "Output:" {
		return ""
	}
	before, after, ok := strings.Cut(content, "\nOutput:")
	if !ok {
		return content
	}
	if !strings.HasPrefix(strings.TrimSpace(before), "Command:") && !codexLooksLikeCommandOutputWrapper(before) {
		return content
	}
	return strings.TrimPrefix(after, "\n")
}

func codexLooksLikeCommandOutputWrapper(header string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(header, "\r\n", "\n"), "\n") {
		normalized := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(normalized, "chunk id:"):
			return true
		case strings.HasPrefix(normalized, "wall time:"):
			return true
		case strings.HasPrefix(normalized, "process exited with code "):
			return true
		case strings.HasPrefix(normalized, "exit code:"):
			return true
		case strings.HasPrefix(normalized, "exit code "):
			return true
		case strings.HasPrefix(normalized, "original token count:"):
			return true
		}
	}
	return false
}

func codexToolResultContentWithPatch(output json.RawMessage, patchResult json.RawMessage) json.RawMessage {
	var patchObject struct {
		Output   string `json:"output,omitempty"`
		Stderr   string `json:"stderr,omitempty"`
		Patch    string `json:"patch,omitempty"`
		FilePath string `json:"file_path,omitempty"`
	}
	if json.Unmarshal(patchResult, &patchObject) != nil {
		return cloneRawJSON(output)
	}
	patchObject.Output = firstNonEmpty(codexOutputText(output), patchObject.Output)
	raw, err := json.Marshal(patchObject)
	if err != nil {
		return cloneRawJSON(output)
	}
	return raw
}

func codexOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		trimmed := strings.TrimSpace(text)
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			return codexOutputText(json.RawMessage(trimmed))
		}
		return text
	}
	var object struct {
		Output string `json:"output"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Text   string `json:"text"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return strings.Join(codexNonEmptyStrings(object.Output, object.Stdout, object.Stderr, object.Text), "\n")
	}
	return ""
}

func codexPatchFromChanges(changes map[string]codexPatchChange) (string, string) {
	if len(changes) == 0 {
		return "", ""
	}
	paths := make([]string, 0, len(changes))
	for path, change := range changes {
		if strings.TrimSpace(change.UnifiedDiff) == "" {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return "", ""
	}
	sort.Strings(paths)

	var b strings.Builder
	for i, path := range paths {
		if i > 0 {
			b.WriteString("\n")
		}
		change := changes[path]
		b.WriteString("--- ")
		b.WriteString(path)
		b.WriteString("\n+++ ")
		if strings.TrimSpace(change.MovePath) != "" {
			b.WriteString(change.MovePath)
		} else {
			b.WriteString(path)
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(change.UnifiedDiff, "\n"))
		b.WriteString("\n")
	}
	if len(paths) == 1 {
		return b.String(), paths[0]
	}
	return b.String(), ""
}

func codexNonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func codexSystemEvent(em codexEventMsg) *SystemEvent {
	kind := "error"
	var category string
	code := strings.TrimSpace(em.CodexErrorInfo)
	message := strings.TrimSpace(em.Message)
	switch strings.TrimSpace(em.Type) {
	case "stream_error":
		category = "stream_error"
		if message == "" {
			message = "Provider stream error"
		}
	case "turn_aborted":
		kind = "turn_aborted"
		category = "turn_aborted"
		if message == "" {
			message = "Turn aborted"
		}
	default:
		category = codexErrorCategory(code)
		if message == "" {
			message = "Provider reported an error"
		}
	}
	return &SystemEvent{
		Kind:     kind,
		Category: category,
		Code:     code,
		Message:  message,
	}
}

func codexErrorCategory(code string) string {
	switch {
	case strings.Contains(strings.ToLower(strings.TrimSpace(code)), "usage_limit"):
		return "usage_limit"
	case strings.TrimSpace(code) != "":
		return "provider_error"
	default:
		return "provider_error"
	}
}

func codexSessionID(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func cloneRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// Codex JSONL entry types.

type codexRawEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexEntry struct {
	raw  codexRawEntry
	line string
}

type codexEventMsg struct {
	Type           string                      `json:"type"`             // user_message, agent_message, agent_reasoning, token_count
	Message        string                      `json:"message"`          // for user_message, agent_message, error
	Text           string                      `json:"text"`             // for agent_reasoning
	CodexErrorInfo string                      `json:"codex_error_info"` // for usage_limit_exceeded and related errors
	CallID         string                      `json:"call_id,omitempty"`
	Stdout         string                      `json:"stdout,omitempty"`
	Stderr         string                      `json:"stderr,omitempty"`
	Changes        map[string]codexPatchChange `json:"changes,omitempty"`
}

type codexPatchChange struct {
	Type        string `json:"type,omitempty"`
	UnifiedDiff string `json:"unified_diff,omitempty"`
	MovePath    string `json:"move_path,omitempty"`
}

type codexResponseItem struct {
	Type             string              `json:"type"` // message, reasoning, function_call, custom_tool_call, function_call_output, custom_tool_call_output, interaction
	Role             string              `json:"role,omitempty"`
	Content          []codexContentBlock `json:"content,omitempty"`
	Summary          []codexContentBlock `json:"summary,omitempty"`
	CallID           string              `json:"call_id,omitempty"`
	Name             string              `json:"name,omitempty"`
	Input            json.RawMessage     `json:"input,omitempty"`
	Arguments        json.RawMessage     `json:"arguments,omitempty"`
	Output           json.RawMessage     `json:"output,omitempty"`
	Query            string              `json:"query,omitempty"`
	RequestID        string              `json:"request_id,omitempty"`
	ID               string              `json:"id,omitempty"`
	Kind             string              `json:"kind,omitempty"`
	State            string              `json:"state,omitempty"`
	Text             string              `json:"text,omitempty"`
	Prompt           string              `json:"prompt,omitempty"`
	Options          []string            `json:"options,omitempty"`
	Action           json.RawMessage     `json:"action,omitempty"`
	Metadata         json.RawMessage     `json:"metadata,omitempty"`
	EncryptedContent string              `json:"encrypted_content,omitempty"`
}

type codexContentBlock struct {
	Type     string `json:"type,omitempty"`
	Text     string `json:"text,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

func codexTextContents(contents []codexContentBlock) string {
	if len(contents) == 0 {
		return ""
	}
	var text strings.Builder
	for _, content := range contents {
		if content.Text == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(content.Text)
	}
	return text.String()
}

func codexResponseContentBlocks(contents []codexContentBlock) []ContentBlock {
	if len(contents) == 0 {
		return []ContentBlock{{Type: "text"}}
	}
	blocks := make([]ContentBlock, 0, len(contents))
	var pendingText strings.Builder
	flushText := func() {
		if pendingText.Len() == 0 {
			return
		}
		blocks = append(blocks, ContentBlock{Type: "text", Text: pendingText.String()})
		pendingText.Reset()
	}
	for _, content := range contents {
		switch strings.ToLower(strings.TrimSpace(content.Type)) {
		case "input_image", "image":
			flushText()
			imageURL := strings.TrimSpace(content.ImageURL)
			if strings.HasPrefix(strings.ToLower(imageURL), "data:") {
				imageURL = ""
			}
			blocks = append(blocks, ContentBlock{
				Type:     "image",
				FilePath: strings.TrimSpace(content.FilePath),
				ImageURL: imageURL,
				MIMEType: strings.TrimSpace(content.MIMEType),
			})
		default:
			if content.Text == "" {
				continue
			}
			if pendingText.Len() > 0 {
				pendingText.WriteByte('\n')
			}
			pendingText.WriteString(content.Text)
		}
	}
	flushText()
	if len(blocks) == 0 {
		return []ContentBlock{{Type: "text"}}
	}
	return blocks
}
