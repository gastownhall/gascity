package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/google/shlex"
)

type structuredToolContext struct {
	Name  string
	Input *StructuredToolInput
}

func attachStructuredToolData(entries []HistoryEntry) []HistoryEntry {
	return attachStructuredToolDataWithContext(entries, entries)
}

// attachStructuredToolDataWithContext normalizes structured tool input and result
// data on entries. The tool_use -> context map is built from contextEntries — the
// full session — rather than from entries alone, so a tool_result on a paginated
// page whose matching tool_use falls off the page can still recover the tool name
// and input needed to type the result (command, diff, read range, task). When
// entries and contextEntries are the same slice (an un-paged load), the context
// pass runs once and behavior is unchanged.
func attachStructuredToolDataWithContext(entries, contextEntries []HistoryEntry) []HistoryEntry {
	contexts := make(map[string]structuredToolContext)
	recordToolUseContexts := func(src []HistoryEntry) {
		for entryIndex := range src {
			for blockIndex := range src[entryIndex].Blocks {
				block := &src[entryIndex].Blocks[blockIndex]
				if block.Kind != BlockKindToolUse || strings.TrimSpace(block.ToolUseID) == "" {
					continue
				}
				input := normalizeStructuredToolInput(block.Name, block.Input)
				block.StructuredInput = input
				contexts[block.ToolUseID] = structuredToolContext{
					Name:  block.Name,
					Input: input,
				}
			}
		}
	}
	recordToolUseContexts(contextEntries)
	if !sameHistoryEntries(entries, contextEntries) {
		// Distinct paged slice: the context pass populated the map (including
		// off-page tool_use) but set StructuredInput only on the context copies,
		// so run it over the returned page too — its own tool_use blocks must
		// carry StructuredInput, and an on-page tool_use wins for its own ID.
		recordToolUseContexts(entries)
	}
	for entryIndex := range entries {
		for blockIndex := range entries[entryIndex].Blocks {
			block := &entries[entryIndex].Blocks[blockIndex]
			if block.Kind != BlockKindToolResult {
				continue
			}
			content := structuredJSONText(block.Content)
			if content == "" {
				content = block.Text
			}
			block.StructuredResult = attachStructuredToolError(inferStructuredToolResult(*block, contexts[block.ToolUseID], content), *block, content)
		}
	}
	attachLinkedStdinCommands(entries)
	return entries
}

// sameHistoryEntries reports whether a and b are the same underlying slice, so
// the un-paged fast path (entries == contextEntries) does the tool_use context
// pass exactly once.
func sameHistoryEntries(a, b []HistoryEntry) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

func attachLinkedStdinCommands(entries []HistoryEntry) {
	shellCommands := make(map[string]string)
	for entryIndex := range entries {
		for blockIndex := range entries[entryIndex].Blocks {
			block := &entries[entryIndex].Blocks[blockIndex]
			if block.Kind != BlockKindToolResult || block.StructuredResult == nil || block.StructuredResult.Kind != "bash" {
				continue
			}
			taskID := strings.TrimSpace(block.StructuredResult.TaskID)
			command := strings.TrimSpace(block.StructuredResult.Command)
			if taskID != "" && command != "" {
				shellCommands[taskID] = command
			}
		}
	}
	if len(shellCommands) == 0 {
		return
	}
	for entryIndex := range entries {
		for blockIndex := range entries[entryIndex].Blocks {
			block := &entries[entryIndex].Blocks[blockIndex]
			if block.Kind != BlockKindToolUse || block.StructuredInput == nil || block.StructuredInput.Kind != "stdin" {
				continue
			}
			taskID := strings.TrimSpace(block.StructuredInput.TaskID)
			if taskID == "" || block.StructuredInput.LinkedCommand != "" {
				continue
			}
			if command := shellCommands[taskID]; command != "" {
				block.StructuredInput.LinkedCommand = command
			}
		}
	}
}

func normalizeStructuredToolInput(name string, raw json.RawMessage) *StructuredToolInput {
	if len(raw) == 0 {
		return nil
	}
	text := structuredJSONText(raw)
	out := &StructuredToolInput{}
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "apply_patch" {
		patch, filePath := editPatchFromRawInput(raw)
		if patch == "" {
			patch = text
			filePath = patchFilePath(text)
		}
		out.Kind = "patch"
		out.Patch = patch
		out.FilePath = filePath
		return out
	}
	if looksLikePatch(text) {
		out.Kind = "patch"
		out.Patch = text
		out.FilePath = patchFilePath(text)
		return out
	}
	if isTodoTool(lowerName, nil) {
		out.Kind = "todo"
		out.Todos = todoItemsFromRawField(raw, "todos")
		return out
	}
	if isQuestionTool(lowerName, nil) {
		out.Kind = "question"
		out.Question, out.Options = questionInputFields(raw)
		return out
	}
	if isPlanTool(lowerName, nil) {
		out.Kind = "plan"
		out.Plan, out.Explanation, out.Steps = planFieldsFromRaw(raw)
		return out
	}
	if isStdinTool(lowerName, nil) {
		out.Kind = "stdin"
		out.TaskID, out.Text = stdinInputFields(raw)
		return out
	}
	if isTaskTool(lowerName, nil) {
		out.Kind = "task"
		out.TaskID, out.TaskType, out.TaskStatus, out.Description = taskInputFields(raw)
		out.Prompt = firstNonEmptyString(out.Prompt, taskPromptField(raw))
		return out
	}
	if isWriteTool(lowerName) {
		filePath, content, language := writeInputFields(raw)
		if filePath != "" || content != "" {
			out.Kind = "write"
			out.FilePath = filePath
			out.Language = firstNonEmptyString(language, languageForPath(filePath))
			out.Text = content
			return out
		}
	}

	for _, field := range structuredJSONFields(raw) {
		switch normalizeStructuredFieldName(field.Name) {
		case "command":
			out.Command = firstNonEmptyString(out.Command, field.Value)
		case "linked_command":
			out.LinkedCommand = firstNonEmptyString(out.LinkedCommand, field.Value)
		case "code":
			out.Code = firstNonEmptyString(out.Code, field.Value)
		case "patch":
			out.Patch = firstNonEmptyString(out.Patch, field.Value)
		case "file_path":
			out.FilePath = firstNonEmptyString(out.FilePath, field.Value)
		case "language":
			out.Language = firstNonEmptyString(out.Language, field.Value)
		case "url":
			out.URL = firstNonEmptyString(out.URL, field.Value)
		case "prompt":
			out.Prompt = firstNonEmptyString(out.Prompt, field.Value)
		case "task_id":
			out.TaskID = firstNonEmptyString(out.TaskID, field.Value)
		case "task_type":
			out.TaskType = firstNonEmptyString(out.TaskType, field.Value)
		case "task_status":
			out.TaskStatus = firstNonEmptyString(out.TaskStatus, field.Value)
		case "description":
			out.Description = firstNonEmptyString(out.Description, field.Value)
		case "query":
			out.Query = firstNonEmptyString(out.Query, field.Value)
		case "pattern":
			out.Pattern = firstNonEmptyString(out.Pattern, field.Value)
		case "text":
			out.Text = firstNonEmptyString(out.Text, field.Value)
		default:
			// Unknown provider fields are not provider-neutral merely because
			// their names and values fit inside the generic argument shape.
			// Preserve provider-owned data on the raw transcript only.
			continue
		}
	}

	if patch, filePath := editPatchFromRawInput(raw); patch != "" && isEditTool(lowerName, out) {
		out.Kind = "patch"
		out.Patch = patch
		out.FilePath = firstNonEmptyString(out.FilePath, filePath)
		return out
	}
	if out.Command != "" {
		if derived := shellDerivedStructuredInput(out.Command); derived != nil {
			derived.Command = out.Command
			if len(out.Arguments) > 0 {
				derived.Arguments = append(derived.Arguments, out.Arguments...)
			}
			return derived
		}
	}
	if isGlobTool(lowerName, out) {
		out.Kind = "glob"
		return out
	}
	if isFetchTool(lowerName, out) {
		out.Kind = "fetch"
		return out
	}
	if isTodoTool(lowerName, out) {
		out.Kind = "todo"
		return out
	}
	if isPlanTool(lowerName, out) {
		out.Kind = "plan"
		out.Plan, out.Explanation, out.Steps = planFieldsFromRaw(raw)
		return out
	}
	if isQuestionTool(lowerName, out) {
		out.Kind = "question"
		out.Question, out.Options = questionInputFields(raw)
		return out
	}
	if isStdinTool(lowerName, out) {
		out.Kind = "stdin"
		if out.TaskID == "" || out.Text == "" {
			taskID, text := stdinInputFields(raw)
			out.TaskID = firstNonEmptyString(out.TaskID, taskID)
			out.Text = firstNonEmptyString(out.Text, text)
		}
		return out
	}
	if isTaskTool(lowerName, out) {
		out.Kind = "task"
		if out.TaskID == "" || out.TaskType == "" || out.TaskStatus == "" || out.Description == "" {
			taskID, taskType, taskStatus, description := taskInputFields(raw)
			out.TaskID = firstNonEmptyString(out.TaskID, taskID)
			out.TaskType = firstNonEmptyString(out.TaskType, taskType)
			out.TaskStatus = firstNonEmptyString(out.TaskStatus, taskStatus)
			out.Description = firstNonEmptyString(out.Description, description)
		}
		out.Prompt = firstNonEmptyString(out.Prompt, taskPromptField(raw))
		return out
	}
	if isSearchTool(lowerName, out) {
		out.Kind = "search"
		return out
	}

	switch {
	case out.Command != "":
		out.Kind = "command"
	case out.LinkedCommand != "" && out.Text != "":
		out.Kind = "stdin"
	case out.Code != "":
		out.Kind = "code"
	case out.Patch != "":
		out.Kind = "patch"
	case out.FilePath != "":
		out.Kind = "file"
		out.Language = firstNonEmptyString(out.Language, languageForPath(out.FilePath))
	case out.URL != "":
		out.Kind = "fetch"
	case out.Query != "" || out.Pattern != "":
		out.Kind = "search"
	case len(out.Todos) > 0:
		out.Kind = "todo"
	case out.Plan != "" || out.Explanation != "" || len(out.Steps) > 0:
		out.Kind = "plan"
	case out.Question != "" || len(out.Options) > 0:
		out.Kind = "question"
	case out.TaskID != "" || out.TaskType != "" || out.TaskStatus != "" || out.Description != "":
		out.Kind = "task"
	case out.Text != "":
		out.Kind = "text"
	case len(out.Arguments) > 0:
		out.Kind = "arguments"
	case text != "" && !structuredJSONContainer(raw):
		out.Kind = "text"
		out.Text = text
	}
	if out.Kind == "" {
		return nil
	}
	return out
}

func inferStructuredToolResult(block HistoryBlock, context structuredToolContext, content string) *StructuredToolResult {
	if content == "" {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(block.Name, context.Name)))
	if isPythonTool(name, context.Input) {
		stdout, stderr, exitCode, interrupted, truncated, isImage := commandResultFields(block.Content, content)
		return &StructuredToolResult{
			Kind:        "python",
			Text:        content,
			Code:        firstNonEmptyString(inputCode(context.Input), resultCode(block.Content)),
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			Interrupted: interrupted,
			Truncated:   truncated,
			IsImage:     isImage,
		}
	}
	if isReadTool(name, context.Input) {
		var resultObject map[string]json.RawMessage
		_ = json.Unmarshal(block.Content, &resultObject)
		readObject := readResultObject(resultObject)
		normalizedContent := jsonStringField(readObject, "content")
		visibleContent := jsonStringField(resultObject, "content")
		readContent := firstNonEmptyString(normalizedContent, commandOutputPayload(visibleContent), commandOutputPayload(content))
		if normalizedContent == "" && shellReadStripsLineNumbers(inputCommand(context.Input)) {
			readContent = stripShellReadLineNumbers(readContent)
		}
		startLine, endLine := shellReadRange(inputCommand(context.Input))
		if value := jsonIntField(readObject, "start_line"); value != nil {
			startLine = *value
		}
		if value := jsonIntField(readObject, "total_lines"); value != nil {
			endLine = *value
		}
		numLines := countLines(readContent)
		if value := jsonIntField(readObject, "num_lines"); value != nil {
			numLines = *value
		} else if startLine > 0 && endLine >= startLine {
			numLines = endLine - startLine + 1
		}
		filePath := firstNonEmptyString(jsonStringField(readObject, "file_path"), inputFilePath(context.Input))
		return &StructuredToolResult{
			Kind:       "read",
			FilePath:   filePath,
			Language:   firstNonEmptyString(jsonStringField(readObject, "language"), inputLanguage(context.Input), languageForPath(filePath)),
			Content:    readContent,
			NumLines:   numLines,
			StartLine:  startLine,
			TotalLines: endLine,
		}
	}
	if isGlobTool(name, context.Input) {
		filenames, numFiles, durationMs, truncated := globResultFields(block.Content, content)
		if numFiles == 0 {
			numFiles = len(filenames)
		}
		globContent := commandOutputPayload(content)
		if len(filenames) > 0 && strings.HasPrefix(strings.TrimSpace(globContent), "{") {
			globContent = strings.Join(filenames, "\n") + "\n"
		}
		return &StructuredToolResult{
			Kind:       "glob",
			Filenames:  filenames,
			NumFiles:   numFiles,
			DurationMs: durationMs,
			Truncated:  truncated,
			Content:    globContent,
			NumLines:   countLines(globContent),
		}
	}
	if isFetchTool(name, context.Input) {
		fetch := fetchResultFields(block.Content, content)
		return &StructuredToolResult{
			Kind:       "fetch",
			Text:       fetch.Content,
			URL:        firstNonEmptyString(fetch.URL, inputURL(context.Input)),
			StatusCode: fetch.StatusCode,
			StatusText: fetch.StatusText,
			Bytes:      fetch.Bytes,
			DurationMs: fetch.DurationMs,
			Content:    fetch.Content,
			NumLines:   countLines(fetch.Content),
		}
	}
	if isTodoTool(name, context.Input) {
		oldTodos, newTodos := todoResultFields(block.Content)
		return &StructuredToolResult{
			Kind:     "todo",
			Text:     content,
			Content:  content,
			OldTodos: oldTodos,
			NewTodos: newTodos,
		}
	}
	if isPlanTool(name, context.Input) {
		plan, explanation, steps := planResultFields(block.Content)
		return &StructuredToolResult{
			Kind:        "plan",
			Text:        content,
			Content:     content,
			Plan:        plan,
			Explanation: explanation,
			Steps:       steps,
		}
	}
	if isQuestionTool(name, context.Input) {
		question, answer, options, answers, questions := questionResultFields(block.Content)
		return &StructuredToolResult{
			Kind:      "question",
			Text:      content,
			Content:   content,
			Question:  firstNonEmptyString(question, inputQuestion(context.Input)),
			Questions: questions,
			Answer:    answer,
			Options:   firstNonEmptyStringSlice(options, inputOptions(context.Input)),
			Answers:   answers,
		}
	}
	if isBashOutputTool(name) {
		bash := bashOutputResultFields(block.Content, content)
		return &StructuredToolResult{
			Kind:        "bash",
			Text:        firstNonEmptyString(bash.Stdout, bash.Stderr, content),
			Command:     bash.Command,
			TaskID:      bash.TaskID,
			TaskStatus:  bash.TaskStatus,
			Stdout:      bash.Stdout,
			Stderr:      bash.Stderr,
			ExitCode:    bash.ExitCode,
			StdoutLines: bash.StdoutLines,
			StderrLines: bash.StderrLines,
			Timestamp:   bash.Timestamp,
			Content:     firstNonEmptyString(bash.Stdout, bash.Stderr, content),
			NumLines:    countLines(firstNonEmptyString(bash.Stdout, bash.Stderr, content)),
		}
	}
	if isKillShellTool(name) {
		shell := killShellResultFields(block.Content, content)
		text := firstNonEmptyString(shell.Message, shell.Stdout, shell.Stderr, content)
		return &StructuredToolResult{
			Kind:       "bash",
			Text:       text,
			TaskID:     firstNonEmptyString(shell.TaskID, inputTaskID(context.Input)),
			TaskStatus: shell.TaskStatus,
			Stdout:     firstNonEmptyString(shell.Stdout, shell.Message),
			Stderr:     shell.Stderr,
			ExitCode:   shell.ExitCode,
			Content:    text,
			NumLines:   countLines(text),
		}
	}
	if isStdinTool(name, context.Input) {
		text := commandOutputPayload(content)
		if strings.TrimSpace(text) == "" {
			text = content
		}
		return &StructuredToolResult{
			Kind:     "stdin",
			Text:     text,
			TaskID:   inputTaskID(context.Input),
			Content:  text,
			NumLines: countLines(text),
		}
	}
	if isTaskTool(name, context.Input) {
		task := taskResultFields(block.Content, content)
		return &StructuredToolResult{
			Kind:              "task",
			Text:              firstNonEmptyString(task.Output, content),
			TaskID:            firstNonEmptyString(task.TaskID, inputTaskID(context.Input)),
			TaskType:          firstNonEmptyString(task.TaskType, inputTaskType(context.Input)),
			TaskStatus:        firstNonEmptyString(task.TaskStatus, inputTaskStatus(context.Input)),
			Description:       firstNonEmptyString(task.Description, inputTaskDescription(context.Input)),
			TotalDurationMs:   task.TotalDurationMs,
			TotalTokens:       task.TotalTokens,
			TotalToolUseCount: task.TotalToolUseCount,
			Output:            task.Output,
			Stdout:            task.Stdout,
			Stderr:            task.Stderr,
			ExitCode:          task.ExitCode,
			Content:           firstNonEmptyString(task.Output, content),
		}
	}
	if isSearchTool(name, context.Input) {
		var resultObject map[string]json.RawMessage
		_ = json.Unmarshal(block.Content, &resultObject)
		searchObject := searchResultObject(resultObject)
		hasNeutralSummary := hasAnyJSONField(searchObject, "mode", "num_files", "num_results", "counts", "filenames", "file_paths", "paths", "files", "result_items", "duration_ms", "durationMs", "applied_limit", "appliedLimit")
		searchContent := jsonStringField(searchObject, "content")
		if searchContent == "" && !hasNeutralSummary {
			searchContent = commandOutputPayload(firstNonEmptyString(jsonStringField(resultObject, "content"), content))
		}
		mode := firstNonEmptyString(jsonStringField(searchObject, "mode"), searchResultMode(searchContent, context.Input))
		filenames := jsonStringSliceField(searchObject, "filenames", "file_paths", "paths", "files")
		if len(filenames) == 0 && !hasNeutralSummary {
			filenames = searchResultFilenamesForMode(searchContent, mode)
		}
		counts := argumentListFromObjectField(searchObject, "counts")
		countTotal := structuredArgumentIntTotal(counts)
		if len(counts) == 0 && !hasNeutralSummary {
			counts, countTotal = searchResultCountsForMode(searchContent, mode)
		}
		if len(filenames) == 0 && len(counts) > 0 {
			filenames = countResultFilenames(counts)
		}
		kind := "grep"
		query := jsonStringField(searchObject, "query")
		numResults := 0
		if context.Input != nil && context.Input.Query != "" && context.Input.Pattern == "" {
			kind = "search"
			query = firstNonEmptyString(query, context.Input.Query)
			numResults = countSearchResults(searchContent, filenames)
		} else if mode == "count" {
			numResults = countTotal
		}
		if value := jsonIntField(searchObject, "num_results"); value != nil {
			numResults = *value
		}
		resultItems := searchResultItems(searchObject, searchContent, context.Input)
		if numResults == 0 && len(resultItems) > 0 {
			numResults = len(resultItems)
		}
		numFiles := len(filenames)
		if value := jsonIntField(searchObject, "num_files"); value != nil {
			numFiles = *value
		}
		numLines := countLines(searchContent)
		if value := jsonIntField(searchObject, "num_lines"); value != nil {
			numLines = *value
		}
		durationMs := 0
		if value := jsonIntField(searchObject, "duration_ms", "durationMs"); value != nil {
			durationMs = *value
		}
		appliedLimit := 0
		if value := jsonIntField(searchObject, "applied_limit", "appliedLimit"); value != nil {
			appliedLimit = *value
		}
		return &StructuredToolResult{
			Kind:         kind,
			Mode:         mode,
			Query:        query,
			Filenames:    filenames,
			NumFiles:     numFiles,
			NumResults:   numResults,
			Counts:       counts,
			DurationMs:   durationMs,
			AppliedLimit: appliedLimit,
			ResultItems:  resultItems,
			Content:      searchContent,
			NumLines:     numLines,
		}
	}
	if isCommandTool(name, context.Input) {
		stdout, stderr, exitCode, interrupted, truncated, isImage := commandResultFields(block.Content, content)
		taskID := taskResultFields(block.Content, content).TaskID
		text := firstNonEmptyString(stdout, stderr, content)
		return &StructuredToolResult{
			Kind:        "bash",
			Text:        text,
			Command:     inputCommand(context.Input),
			TaskID:      taskID,
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			Interrupted: interrupted,
			Truncated:   truncated,
			IsImage:     isImage,
		}
	}
	if isWriteTool(name) || (context.Input != nil && context.Input.Kind == "write") {
		write := writeResultFields(block.Content)
		writeContent := firstNonEmptyString(write.Content, commandOutputPayload(content))
		numLines := countLines(writeContent)
		if write.NumLines != 0 {
			numLines = write.NumLines
		}
		resultPatch, resultFile := explicitPatchFromRawResult(block.Content)
		patch := firstNonEmptyString(resultPatch, patchContent(content))
		patchHunks, filePaths := explicitPatchHunksFromRawResult(block.Content)
		if len(patchHunks) == 0 && patch != "" {
			patchHunks = parsePatchHunks(patch, firstNonEmptyString(resultFile, write.FilePath, inputFilePath(context.Input)))
			filePaths = patchHunkFilePaths(patchHunks)
		}
		filePath := firstNonEmptyString(write.FilePath, resultFile, patchFilePath(patch), inputFilePath(context.Input), firstString(filePaths))
		filePaths = addUniqueString(filePaths, filePath)
		return &StructuredToolResult{
			Kind:       "write",
			Text:       writeContent,
			FilePath:   filePath,
			FilePaths:  filePaths,
			Language:   firstNonEmptyString(write.Language, inputLanguage(context.Input), languageForPath(filePath)),
			Content:    writeContent,
			NumLines:   numLines,
			Patch:      patch,
			PatchHunks: patchHunks,
			StartLine:  write.StartLine,
			TotalLines: write.TotalLines,
		}
	}
	if name == "apply_patch" || isEditTool(name, context.Input) || (context.Input != nil && context.Input.Kind == "patch") || looksLikePatch(content) {
		resultPatch, resultFile := editPatchFromRawResult(block.Content)
		patch := firstNonEmptyString(resultPatch, patchContent(content))
		patchHunks, filePaths := editPatchHunksFromRawResult(block.Content)
		metadata := editMetadataFromRawResult(block.Content)
		resultContent := commandOutputPayload(content)
		if len(patchHunks) == 0 && patch != "" {
			patchHunks = parsePatchHunks(patch, firstNonEmptyString(resultFile, inputFilePath(context.Input)))
			filePaths = patchHunkFilePaths(patchHunks)
		}
		filePath := firstNonEmptyString(resultFile, patchFilePath(patch), patchFilePath(content), inputFilePath(context.Input), firstString(filePaths))
		filePaths = addUniqueString(filePaths, filePath)
		return &StructuredToolResult{
			Kind:         "edit",
			FilePath:     filePath,
			FilePaths:    filePaths,
			Patch:        patch,
			PatchHunks:   patchHunks,
			OldString:    metadata.OldString,
			NewString:    metadata.NewString,
			OriginalFile: metadata.OriginalFile,
			ReplaceAll:   metadata.ReplaceAll,
			UserModified: metadata.UserModified,
			Content:      resultContent,
		}
	}
	return &StructuredToolResult{
		Kind:    "text",
		Text:    content,
		Content: content,
	}
}

func structuredJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var textBlocks []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &textBlocks); err == nil {
		parts := make([]string, 0, len(textBlocks))
		for _, block := range textBlocks {
			switch {
			case block.Text != "":
				parts = append(parts, block.Text)
			case block.Content != "":
				parts = append(parts, block.Content)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	var object struct {
		Output  string `json:"output"`
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
		Text    string `json:"text"`
		Content string `json:"content"`
		Error   string `json:"error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		values := nonEmptyStrings(
			object.Output,
			object.Stdout,
			object.Stderr,
			object.Text,
			object.Content,
			object.Error,
			object.Result,
		)
		if len(values) > 0 {
			return strings.Join(values, "\n")
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

func structuredJSONFields(raw json.RawMessage) []StructuredArgument {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]StructuredArgument, 0, len(keys))
	for _, key := range keys {
		value, ok := structuredJSONScalar(object[key])
		if !ok {
			continue
		}
		fields = append(fields, StructuredArgument{
			Name:  key,
			Value: value,
		})
	}
	return fields
}

func structuredJSONScalar(raw json.RawMessage) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

func structuredJSONContainer(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	switch decoded.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

func structuredArgumentScalar(raw json.RawMessage) (string, bool) {
	value, ok := structuredJSONScalar(raw)
	if !ok {
		return "", false
	}
	if _, encodedContainer := decodeJSONStringContainer(value); encodedContainer {
		return "", false
	}
	return value, true
}

func readResultObject(object map[string]json.RawMessage) map[string]json.RawMessage {
	if len(object) == 0 {
		return object
	}
	for _, key := range []string{"tool_result", "provider_result"} {
		nestedRaw, ok := object[key]
		if !ok || len(nestedRaw) == 0 {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(nestedRaw, &nested) != nil || len(nested) == 0 {
			continue
		}
		if hasAnyJSONField(nested, "file_path", "content", "num_lines", "start_line", "total_lines", "language") {
			return nested
		}
	}
	return object
}

func searchResultObject(object map[string]json.RawMessage) map[string]json.RawMessage {
	if len(object) == 0 {
		return object
	}
	for _, key := range []string{"tool_result", "provider_result"} {
		nestedRaw, ok := object[key]
		if !ok || len(nestedRaw) == 0 {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(nestedRaw, &nested) != nil || len(nested) == 0 {
			continue
		}
		if hasAnyJSONField(nested, "query", "mode", "num_files", "num_results", "counts", "filenames", "file_paths", "paths", "files", "result_items", "duration_ms", "durationMs", "applied_limit", "appliedLimit", "content") {
			return nested
		}
	}
	return object
}

func normalizeStructuredFieldName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cmd", "command", "shell_command":
		return "command"
	case "linked_command", "linkedcommand", "parent_command", "parentcommand":
		return "linked_command"
	case "code", "python", "script":
		return "code"
	case "patch", "diff", "file_diff", "filediff":
		return "patch"
	case "file", "file_path", "filepath", "path":
		return "file_path"
	case "language", "lang":
		return "language"
	case "url", "uri", "href":
		return "url"
	case "prompt", "instruction", "instructions":
		return "prompt"
	case "task_id", "taskid", "session_id", "sessionid", "background_task_id", "backgroundtaskid", "background_task", "backgroundtask", "backgroundTaskId", "bash_id", "bashid", "shell_id", "shellid", "agent_id", "agentid":
		return "task_id"
	case "task_type", "tasktype", "task_kind", "taskkind", "subagent_type", "subagenttype", "agent_type", "agenttype":
		return "task_type"
	case "task_status", "taskstatus", "status", "state":
		return "task_status"
	case "description", "summary", "title":
		return "description"
	case "q", "query", "search_query":
		return "query"
	case "pattern", "regexp", "regex":
		return "pattern"
	case "content", "new_string", "newstring", "new_str", "old_string", "oldstring", "old_str", "replacement", "text":
		return "text"
	default:
		return name
	}
}

func looksLikePatch(text string) bool {
	return strings.Contains(text, "*** Begin Patch") || strings.Contains(text, "\n@@")
}

func patchFilePath(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

func patchContent(content string) string {
	if looksLikePatch(content) {
		return content
	}
	return ""
}

func editPatchFromRawInput(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	text := structuredJSONText(raw)
	if looksLikePatch(text) {
		return text, patchFilePath(text)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", ""
	}
	if patch := jsonStringField(object, "patch", "diff", "file_diff", "fileDiff"); patch != "" {
		return patch, firstNonEmptyString(jsonStringField(object, "file_path", "filePath", "path", "file"), patchFilePath(patch))
	}
	filePath := jsonStringField(object, "file_path", "filePath", "path", "file")
	if patch := editPatchFromEditArray(object, filePath); patch != "" {
		return patch, filePath
	}
	oldText := jsonStringField(object, "old_string", "oldString", "old_str", "oldStr", "old")
	newText := jsonStringField(object, "new_string", "newString", "new_str", "newStr", "replacement", "new")
	if oldText != "" || newText != "" {
		return buildUnifiedPatch(filePath, []editPatchHunk{{OldText: oldText, NewText: newText}}), filePath
	}
	content := jsonStringField(object, "content", "file_text", "fileText", "new_content", "newContent")
	if content != "" {
		return buildUnifiedPatch(filePath, []editPatchHunk{{NewText: content}}), filePath
	}
	return "", ""
}

// editPatchFromRawResult extracts a unified-diff patch and file path from a
// tool RESULT payload. It must only ever be passed result-side bytes
// (block.Content), never tool input: the structured contract requires that a
// result patch come from provider/result-side evidence and is never fabricated
// from input fields such as old_string/new_string or an apply_patch input. Keep
// this signature input-free so that invariant cannot regress unnoticed. See
// TestInferStructuredToolResultDoesNotFabricateEditPatchFromInput.
func editPatchFromRawResult(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", ""
	}
	if displayRaw, ok := object["resultDisplay"]; ok {
		if patch, filePath := editPatchFromResultDisplay(displayRaw); patch != "" {
			return patch, filePath
		}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if resultRaw, ok := object[key]; ok {
			if patch, filePath := editPatchFromRawResult(resultRaw); patch != "" {
				return patch, filePath
			}
		}
	}
	if patch, filePath := editPatchFromResultDisplay(raw); patch != "" {
		return patch, filePath
	}
	if patch, filePath := editPatchFromStructuredPatch(object); patch != "" {
		return patch, filePath
	}
	if patch := jsonStringField(object, "patch", "diff", "file_diff", "fileDiff"); patch != "" {
		return patch, firstNonEmptyString(jsonStringField(object, "file_path", "filePath", "path", "file"), patchFilePath(patch))
	}
	return "", ""
}

func explicitPatchFromRawResult(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", ""
	}
	if displayRaw, ok := object["resultDisplay"]; ok {
		if patch, filePath := explicitPatchFromResultDisplay(displayRaw); patch != "" {
			return patch, filePath
		}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if resultRaw, ok := object[key]; ok {
			if patch, filePath := explicitPatchFromRawResult(resultRaw); patch != "" {
				return patch, filePath
			}
		}
	}
	if patch, filePath := explicitPatchFromResultDisplay(raw); patch != "" {
		return patch, filePath
	}
	if patch, filePath := editPatchFromStructuredPatch(object); patch != "" {
		return patch, filePath
	}
	if patch := jsonStringField(object, "patch", "diff", "file_diff", "fileDiff"); patch != "" {
		return patch, firstNonEmptyString(jsonStringField(object, "file_path", "filePath", "path", "file"), patchFilePath(patch))
	}
	return "", ""
}

func editPatchHunksFromRawResult(raw json.RawMessage) ([]StructuredPatchHunk, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return nil, nil
	}
	if displayRaw, ok := object["resultDisplay"]; ok {
		if hunks, filePaths := patchHunksFromResultDisplay(displayRaw); len(hunks) > 0 {
			return hunks, filePaths
		}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if resultRaw, ok := object[key]; ok {
			if hunks, filePaths := editPatchHunksFromRawResult(resultRaw); len(hunks) > 0 {
				return hunks, filePaths
			}
		}
	}
	if hunks, filePaths := patchHunksFromResultDisplay(raw); len(hunks) > 0 {
		return hunks, filePaths
	}
	if hunks, filePaths := patchHunksFromStructuredPatch(object); len(hunks) > 0 {
		return hunks, filePaths
	}
	if patch := jsonStringField(object, "patch", "diff", "file_diff", "fileDiff"); patch != "" {
		filePath := firstNonEmptyString(jsonStringField(object, "file_path", "filePath", "path", "file"), patchFilePath(patch))
		hunks := parsePatchHunks(patch, filePath)
		return hunks, patchHunkFilePaths(hunks)
	}
	return nil, nil
}

func explicitPatchHunksFromRawResult(raw json.RawMessage) ([]StructuredPatchHunk, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return nil, nil
	}
	if displayRaw, ok := object["resultDisplay"]; ok {
		if hunks, filePaths := explicitPatchHunksFromResultDisplay(displayRaw); len(hunks) > 0 {
			return hunks, filePaths
		}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if resultRaw, ok := object[key]; ok {
			if hunks, filePaths := explicitPatchHunksFromRawResult(resultRaw); len(hunks) > 0 {
				return hunks, filePaths
			}
		}
	}
	if hunks, filePaths := explicitPatchHunksFromResultDisplay(raw); len(hunks) > 0 {
		return hunks, filePaths
	}
	if hunks, filePaths := patchHunksFromStructuredPatch(object); len(hunks) > 0 {
		return hunks, filePaths
	}
	if patch := jsonStringField(object, "patch", "diff", "file_diff", "fileDiff"); patch != "" {
		filePath := firstNonEmptyString(jsonStringField(object, "file_path", "filePath", "path", "file"), patchFilePath(patch))
		hunks := parsePatchHunks(patch, filePath)
		return hunks, patchHunkFilePaths(hunks)
	}
	return nil, nil
}

type editResultMetadata struct {
	OldString    string
	NewString    string
	OriginalFile string
	ReplaceAll   *bool
	UserModified *bool
}

func editMetadataFromRawResult(raw json.RawMessage) editResultMetadata {
	return editMetadataFromRawResultDepth(raw, 0)
}

func editMetadataFromRawResultDepth(raw json.RawMessage, depth int) editResultMetadata {
	if len(raw) == 0 || depth > 4 {
		return editResultMetadata{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return editMetadataFromRawResultDepth(json.RawMessage(encoded), depth+1)
		}
		return editResultMetadata{}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return editResultMetadata{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result", "edit", "editResult", "edit_result"} {
		if nested, ok := object[key]; ok {
			metadata := editMetadataFromRawResultDepth(nested, depth+1)
			if metadata.hasData() {
				return metadata
			}
		}
	}
	return editResultMetadata{
		OldString:    jsonStringField(object, "old_string", "oldString", "old_str", "oldStr"),
		NewString:    jsonStringField(object, "new_string", "newString", "new_str", "newStr"),
		OriginalFile: jsonStringField(object, "original_file", "originalFile"),
		ReplaceAll:   jsonBoolFieldPtr(object, "replace_all", "replaceAll"),
		UserModified: jsonBoolFieldPtr(object, "user_modified", "userModified"),
	}
}

func (m editResultMetadata) hasData() bool {
	return m.OldString != "" || m.NewString != "" || m.OriginalFile != "" || m.ReplaceAll != nil || m.UserModified != nil
}

func editPatchFromResultDisplay(raw json.RawMessage) (string, string) {
	var display map[string]json.RawMessage
	if json.Unmarshal(raw, &display) != nil || len(display) == 0 {
		return "", ""
	}
	filePath := jsonStringField(display, "file_path", "filePath", "fileName", "file")
	if patch := jsonStringField(display, "file_diff", "fileDiff", "patch", "diff"); patch != "" {
		return patch, firstNonEmptyString(filePath, patchFilePath(patch))
	}
	oldText := jsonStringField(display, "original_content", "originalContent", "old_content", "oldContent")
	newText := jsonStringField(display, "new_content", "newContent", "content")
	if oldText != "" || newText != "" {
		return buildUnifiedPatch(filePath, []editPatchHunk{{OldText: oldText, NewText: newText}}), filePath
	}
	return "", ""
}

func explicitPatchFromResultDisplay(raw json.RawMessage) (string, string) {
	var display map[string]json.RawMessage
	if json.Unmarshal(raw, &display) != nil || len(display) == 0 {
		return "", ""
	}
	filePath := jsonStringField(display, "file_path", "filePath", "fileName", "file")
	if patch := jsonStringField(display, "file_diff", "fileDiff", "patch", "diff"); patch != "" {
		return patch, firstNonEmptyString(filePath, patchFilePath(patch))
	}
	return "", ""
}

func patchHunksFromResultDisplay(raw json.RawMessage) ([]StructuredPatchHunk, []string) {
	var display map[string]json.RawMessage
	if json.Unmarshal(raw, &display) != nil || len(display) == 0 {
		return nil, nil
	}
	filePath := jsonStringField(display, "file_path", "filePath", "fileName", "file")
	if patch := jsonStringField(display, "file_diff", "fileDiff", "patch", "diff"); patch != "" {
		hunks := parsePatchHunks(patch, firstNonEmptyString(filePath, patchFilePath(patch)))
		return hunks, patchHunkFilePaths(hunks)
	}
	oldText := jsonStringField(display, "original_content", "originalContent", "old_content", "oldContent")
	newText := jsonStringField(display, "new_content", "newContent", "content")
	if oldText != "" || newText != "" {
		hunks := parsePatchHunks(buildUnifiedPatch(filePath, []editPatchHunk{{OldText: oldText, NewText: newText}}), filePath)
		return hunks, patchHunkFilePaths(hunks)
	}
	return nil, nil
}

func explicitPatchHunksFromResultDisplay(raw json.RawMessage) ([]StructuredPatchHunk, []string) {
	var display map[string]json.RawMessage
	if json.Unmarshal(raw, &display) != nil || len(display) == 0 {
		return nil, nil
	}
	filePath := jsonStringField(display, "file_path", "filePath", "fileName", "file")
	if patch := jsonStringField(display, "file_diff", "fileDiff", "patch", "diff"); patch != "" {
		hunks := parsePatchHunks(patch, firstNonEmptyString(filePath, patchFilePath(patch)))
		return hunks, patchHunkFilePaths(hunks)
	}
	return nil, nil
}

func editPatchFromEditArray(object map[string]json.RawMessage, filePath string) string {
	rawEdits, ok := object["edits"]
	if !ok {
		return ""
	}
	var edits []map[string]json.RawMessage
	if json.Unmarshal(rawEdits, &edits) != nil || len(edits) == 0 {
		return ""
	}
	hunks := make([]editPatchHunk, 0, len(edits))
	for _, edit := range edits {
		oldText := jsonStringField(edit, "old_string", "oldString", "old_str", "oldStr", "old")
		newText := jsonStringField(edit, "new_string", "newString", "new_str", "newStr", "replacement", "new")
		if oldText == "" && newText == "" {
			continue
		}
		hunks = append(hunks, editPatchHunk{OldText: oldText, NewText: newText})
	}
	if len(hunks) == 0 {
		return ""
	}
	return buildUnifiedPatch(filePath, hunks)
}

type editPatchHunk struct {
	OldText string
	NewText string
}

func buildUnifiedPatch(filePath string, hunks []editPatchHunk) string {
	if len(hunks) == 0 {
		return ""
	}
	from := firstNonEmptyString(filePath, "file")
	to := from
	if len(hunks) == 1 {
		if hunks[0].OldText == "" && hunks[0].NewText != "" {
			from = "/dev/null"
		}
		if hunks[0].OldText != "" && hunks[0].NewText == "" {
			to = "/dev/null"
		}
	}

	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(from)
	b.WriteString("\n+++ ")
	b.WriteString(to)
	for _, hunk := range hunks {
		b.WriteString("\n@@\n")
		appendPatchLines(&b, "-", hunk.OldText)
		appendPatchLines(&b, "+", hunk.NewText)
	}
	return b.String()
}

func appendPatchLines(b *strings.Builder, prefix, text string) {
	if text == "" {
		return
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
}

func jsonStringField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		if text := structuredJSONText(raw); strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func hasAnyJSONField(object map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		raw, ok := object[name]
		if ok && len(raw) > 0 && string(raw) != "null" {
			return true
		}
	}
	return false
}

func editPatchFromStructuredPatch(object map[string]json.RawMessage) (string, string) {
	rawPatch, ok := structuredPatchRaw(object)
	if !ok {
		return "", ""
	}
	hunks := decodeStructuredPatchHunks(rawPatch)
	if len(hunks) == 0 {
		return "", ""
	}
	filePath := jsonStringField(object, "file_path", "filePath", "path", "file")
	for _, hunk := range hunks {
		if strings.TrimSpace(hunk.FilePath) != "" {
			filePath = hunk.FilePath
			break
		}
	}
	var b strings.Builder
	from := firstNonEmptyString(filePath, "file")
	b.WriteString("--- ")
	b.WriteString(from)
	b.WriteString("\n+++ ")
	b.WriteString(from)
	for _, hunk := range hunks {
		b.WriteString("\n@@")
		if hunk.OldStart > 0 || hunk.NewStart > 0 {
			b.WriteString(" -")
			b.WriteString(formatPatchRange(hunk.OldStart, hunk.OldLines))
			b.WriteString(" +")
			b.WriteString(formatPatchRange(hunk.NewStart, hunk.NewLines))
			b.WriteString(" ")
		}
		b.WriteString("@@\n")
		for _, line := range hunk.Lines {
			if line == "" || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\\") {
				b.WriteString(line)
			} else {
				b.WriteString(" ")
				b.WriteString(line)
			}
			b.WriteString("\n")
		}
	}
	return b.String(), filePath
}

func patchHunksFromStructuredPatch(object map[string]json.RawMessage) ([]StructuredPatchHunk, []string) {
	rawPatch, ok := structuredPatchRaw(object)
	if !ok {
		return nil, nil
	}
	hunks := decodeStructuredPatchHunks(rawPatch)
	if len(hunks) == 0 {
		return nil, nil
	}
	filePath := jsonStringField(object, "file_path", "filePath", "path", "file")
	out := make([]StructuredPatchHunk, 0, len(hunks))
	for _, hunk := range hunks {
		hunkFilePath := firstNonEmptyString(hunk.FilePath, filePath)
		out = append(out, StructuredPatchHunk{
			FilePath: hunkFilePath,
			OldStart: hunk.OldStart,
			OldLines: hunk.OldLines,
			NewStart: hunk.NewStart,
			NewLines: hunk.NewLines,
			Lines:    normalizePatchLines(hunk.Lines),
		})
	}
	return out, patchHunkFilePaths(out)
}

func structuredPatchRaw(object map[string]json.RawMessage) (json.RawMessage, bool) {
	if rawPatch, ok := object["patch_hunks"]; ok {
		return rawPatch, true
	}
	if rawPatch, ok := object["structuredPatch"]; ok {
		return rawPatch, true
	}
	return nil, false
}

func decodeStructuredPatchHunks(raw json.RawMessage) []StructuredPatchHunk {
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil
	}
	out := make([]StructuredPatchHunk, 0, len(items))
	for _, item := range items {
		hunk := StructuredPatchHunk{
			FilePath: jsonStringField(item, "file_path", "filePath"),
			OldStart: intFieldValue(item, "old_start", "oldStart"),
			OldLines: intFieldValue(item, "old_lines", "oldLines"),
			NewStart: intFieldValue(item, "new_start", "newStart"),
			NewLines: intFieldValue(item, "new_lines", "newLines"),
			Lines:    normalizePatchLines(jsonStringSliceField(item, "lines")),
		}
		out = append(out, hunk)
	}
	return out
}

func intFieldValue(object map[string]json.RawMessage, names ...string) int {
	if value := jsonIntField(object, names...); value != nil {
		return *value
	}
	return 0
}

func parsePatchHunks(patch string, fallbackFilePath string) []StructuredPatchHunk {
	normalized := strings.ReplaceAll(patch, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]StructuredPatchHunk, 0)
	currentFilePath := fallbackFilePath
	nextOldStart := 1
	nextNewStart := 1
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if operation, filePath := patchFileOperation(line); filePath != "" {
			currentFilePath = filePath
			nextOldStart = 1
			nextNewStart = 1
			if operation == "add" || operation == "delete" {
				hunkLines := make([]string, 0)
				for i++; i < len(lines); i++ {
					current := lines[i]
					if current == "*** End Patch" || strings.HasPrefix(current, "@@") || patchLineFilePath(current) != "" {
						i--
						break
					}
					switch {
					case operation == "add" && strings.HasPrefix(current, "+"):
						hunkLines = append(hunkLines, current)
					case operation == "delete" && strings.HasPrefix(current, "-"):
						hunkLines = append(hunkLines, current)
					}
				}
				if len(hunkLines) == 0 {
					continue
				}
				oldStart, newStart := 1, 1
				if operation == "add" {
					oldStart = 0
				}
				if operation == "delete" {
					newStart = 0
				}
				out = append(out, StructuredPatchHunk{
					FilePath: currentFilePath,
					OldStart: oldStart,
					OldLines: countOldPatchLines(hunkLines),
					NewStart: newStart,
					NewLines: countNewPatchLines(hunkLines),
					Lines:    hunkLines,
				})
				nextOldStart = oldStart + countOldPatchLines(hunkLines)
				nextNewStart = newStart + countNewPatchLines(hunkLines)
			}
			continue
		}
		if filePath := patchLineFilePath(line); filePath != "" {
			currentFilePath = filePath
			nextOldStart = 1
			nextNewStart = 1
			continue
		}
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		oldStart, oldLines, newStart, newLines, hasRange := parsePatchHunkHeader(line)
		hunkLines := make([]string, 0)
		for i++; i < len(lines); i++ {
			current := lines[i]
			if current == "*** End Patch" || strings.HasPrefix(current, "@@") || patchLineFilePath(current) != "" {
				i--
				break
			}
			if current == "\\ No newline at end of file" {
				continue
			}
			switch {
			case strings.HasPrefix(current, " "), strings.HasPrefix(current, "-"), strings.HasPrefix(current, "+"):
				hunkLines = append(hunkLines, current)
			case current == "":
				if i != len(lines)-1 {
					hunkLines = append(hunkLines, " ")
				}
			}
		}
		if len(hunkLines) == 0 {
			continue
		}
		if !hasRange {
			oldStart = nextOldStart
			oldLines = countOldPatchLines(hunkLines)
			newStart = nextNewStart
			newLines = countNewPatchLines(hunkLines)
		}
		out = append(out, StructuredPatchHunk{
			FilePath: currentFilePath,
			OldStart: oldStart,
			OldLines: oldLines,
			NewStart: newStart,
			NewLines: newLines,
			Lines:    hunkLines,
		})
		nextOldStart = oldStart + oldLines
		nextNewStart = newStart + newLines
	}
	return out
}

func patchFileOperation(line string) (string, string) {
	line = strings.TrimSpace(line)
	for _, op := range []struct {
		prefix string
		name   string
	}{
		{prefix: "*** Add File:", name: "add"},
		{prefix: "*** Delete File:", name: "delete"},
	} {
		if rest, ok := strings.CutPrefix(line, op.prefix); ok {
			return op.name, cleanPatchFilePath(rest)
		}
	}
	return "", ""
}

func parsePatchHunkHeader(line string) (int, int, int, int, bool) {
	parts := strings.Fields(line)
	var oldStart, oldLines, newStart, newLines int
	var hasOld, hasNew bool
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, "-"):
			oldStart, oldLines, hasOld = parsePatchRange(strings.TrimPrefix(part, "-"))
		case strings.HasPrefix(part, "+"):
			newStart, newLines, hasNew = parsePatchRange(strings.TrimPrefix(part, "+"))
		}
	}
	return oldStart, oldLines, newStart, newLines, hasOld && hasNew
}

func parsePatchRange(value string) (int, int, bool) {
	startText, linesText, hasComma := strings.Cut(value, ",")
	start, ok := parsePatchRangeInt(startText)
	if !ok {
		return 0, 0, false
	}
	if !hasComma {
		return start, 1, true
	}
	lines, ok := parsePatchRangeInt(linesText)
	if !ok {
		return 0, 0, false
	}
	return start, lines, true
}

func parsePatchRangeInt(value string) (int, bool) {
	if value == "0" {
		return 0, true
	}
	return parsePositiveInt(value)
}

func patchLineFilePath(line string) string {
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "Index:"} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return cleanPatchFilePath(rest)
		}
	}
	for _, prefix := range []string{"--- ", "+++ "} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			filePath := cleanPatchFilePath(rest)
			if filePath != "" && filePath != "/dev/null" {
				return filePath
			}
		}
	}
	return ""
}

func cleanPatchFilePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = firstWhitespaceDelimitedToken(value)
	if value == "/dev/null" {
		return value
	}
	value = strings.TrimPrefix(value, "a/")
	value = strings.TrimPrefix(value, "b/")
	return value
}

func normalizePatchLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\\") {
			out = append(out, line)
			continue
		}
		out = append(out, " "+line)
	}
	return out
}

func patchHunkFilePaths(hunks []StructuredPatchHunk) []string {
	var out []string
	for _, hunk := range hunks {
		out = addUniqueString(out, hunk.FilePath)
	}
	return out
}

func countOldPatchLines(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-") {
			count++
		}
	}
	return count
}

func countNewPatchLines(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "+") {
			count++
		}
	}
	return count
}

func formatPatchRange(start, lines int) string {
	if start <= 0 {
		start = 1
	}
	if lines <= 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if lines == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, lines)
}

func inputFilePath(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.FilePath
}

func inputLanguage(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Language
}

func inputCode(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Code
}

func inputCommand(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Command
}

func inputURL(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.URL
}

func inputQuestion(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Question
}

func inputOptions(input *StructuredToolInput) []string {
	if input == nil {
		return nil
	}
	return input.Options
}

func inputTaskID(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.TaskID
}

func inputTaskType(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.TaskType
}

func inputTaskStatus(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.TaskStatus
}

func inputTaskDescription(input *StructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Description
}

func taskPromptField(raw json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return ""
	}
	return jsonLiteralStringField(object, "prompt", "instruction", "instructions")
}

func writeInputFields(raw json.RawMessage) (filePath string, content string, language string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", ""
	}
	filePath = jsonStringField(object, "file_path", "filePath", "path", "file")
	language = jsonStringField(object, "language", "lang")
	content = firstNonEmptyString(
		jsonStringField(object, "content"),
		jsonStringField(object, "file_text", "fileText"),
		jsonStringField(object, "new_content", "newContent"),
		jsonStringField(object, "text"),
	)
	return filePath, content, language
}

type writeResultData struct {
	FilePath   string
	Content    string
	Language   string
	NumLines   int
	StartLine  int
	TotalLines int
}

func writeResultFields(raw json.RawMessage) writeResultData {
	return writeResultFieldsDepth(raw, 0)
}

func writeResultFieldsDepth(raw json.RawMessage, depth int) writeResultData {
	if len(raw) == 0 || depth > 4 {
		return writeResultData{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return writeResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return writeResultData{Content: encoded}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return writeResultData{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result", "write", "writeResult", "write_result"} {
		if nested, ok := object[key]; ok {
			if data := writeResultFieldsDepth(nested, depth+1); data.hasData() {
				return data
			}
		}
	}
	data := writeResultData{
		FilePath: jsonStringField(object, "file_path", "filePath", "path", "file"),
		Content: firstNonEmptyString(
			jsonStringField(object, "content"),
			jsonStringField(object, "file_text", "fileText"),
			jsonStringField(object, "new_content", "newContent"),
			jsonStringField(object, "text"),
			jsonStringField(object, "output"),
			jsonStringField(object, "result"),
		),
		Language: jsonStringField(object, "language", "lang"),
	}
	if value := jsonIntField(object, "num_lines", "numLines"); value != nil {
		data.NumLines = *value
	}
	if value := jsonIntField(object, "start_line", "startLine"); value != nil {
		data.StartLine = *value
	}
	if value := jsonIntField(object, "total_lines", "totalLines"); value != nil {
		data.TotalLines = *value
	}
	if fileRaw, ok := object["file"]; ok && jsonObjectField(fileRaw) {
		if fileData := writeResultFieldsDepth(fileRaw, depth+1); fileData.hasData() {
			data = mergeWriteResultData(data, fileData)
		}
	}
	return data
}

func jsonObjectField(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && len(object) > 0
}

func mergeWriteResultData(base, override writeResultData) writeResultData {
	if override.FilePath != "" {
		base.FilePath = override.FilePath
	}
	if override.Content != "" {
		base.Content = override.Content
	}
	if override.Language != "" {
		base.Language = override.Language
	}
	if override.NumLines != 0 {
		base.NumLines = override.NumLines
	}
	if override.StartLine != 0 {
		base.StartLine = override.StartLine
	}
	if override.TotalLines != 0 {
		base.TotalLines = override.TotalLines
	}
	return base
}

func (data writeResultData) hasData() bool {
	return data.FilePath != "" || data.Content != "" || data.Language != "" || data.NumLines != 0 || data.StartLine != 0 || data.TotalLines != 0
}

func stdinInputFields(raw json.RawMessage) (taskID string, text string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", structuredJSONText(raw)
	}
	taskID = jsonLiteralStringField(object, "task_id", "taskId", "session_id", "sessionId", "shell_id", "shellId", "bash_id", "bashId", "id")
	text = firstNonEmptyString(
		jsonStringField(object, "content"),
		jsonStringField(object, "text"),
		jsonStringField(object, "input"),
		jsonStringField(object, "chars"),
		jsonStringField(object, "data"),
	)
	return taskID, text
}

func taskInputFields(raw json.RawMessage) (taskID string, taskType string, taskStatus string, description string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", "", ""
	}
	taskID = jsonLiteralStringField(object, "task_id", "taskId", "session_id", "sessionId", "backgroundTaskId", "background_task_id", "bash_id", "bashId", "shell_id", "shellId", "agent_id", "agentId", "id")
	taskType = jsonLiteralStringField(object, "task_type", "taskType", "task_kind", "taskKind", "subagent_type", "subagentType", "agent_type", "agentType", "type", "kind")
	taskStatus = jsonLiteralStringField(object, "task_status", "taskStatus", "status", "state")
	description = jsonLiteralStringField(object, "description", "summary", "title")
	return taskID, taskType, taskStatus, description
}

type taskResultData struct {
	TaskID            string
	TaskType          string
	TaskStatus        string
	Description       string
	Output            string
	Stdout            string
	Stderr            string
	ExitCode          *int
	TotalDurationMs   int
	TotalTokens       int
	TotalToolUseCount int
}

type bashOutputResultData struct {
	TaskID      string
	Command     string
	TaskStatus  string
	Stdout      string
	Stderr      string
	ExitCode    *int
	StdoutLines int
	StderrLines int
	Timestamp   string
}

type killShellResultData struct {
	TaskID     string
	TaskStatus string
	Message    string
	Stdout     string
	Stderr     string
	ExitCode   *int
}

func bashOutputResultFields(raw json.RawMessage, content string) bashOutputResultData {
	data := bashOutputResultFieldsDepth(raw, 0)
	if data.Stdout == "" && data.Stderr == "" {
		data.Stdout = commandOutputPayload(content)
	}
	return data
}

func killShellResultFields(raw json.RawMessage, content string) killShellResultData {
	data := killShellResultFieldsDepth(raw, 0)
	if data.Message == "" && data.Stdout == "" && data.Stderr == "" {
		data.Message = commandOutputPayload(content)
	}
	return data
}

func killShellResultFieldsDepth(raw json.RawMessage, depth int) killShellResultData {
	if len(raw) == 0 || depth > 4 {
		return killShellResultData{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return killShellResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return killShellResultData{Message: encoded}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return killShellResultData{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result", "shell", "killShell", "kill_shell"} {
		if nested, ok := object[key]; ok {
			data := killShellResultFieldsDepth(nested, depth+1)
			if data.TaskID != "" || data.TaskStatus != "" || data.Message != "" || data.Stdout != "" || data.Stderr != "" || data.ExitCode != nil {
				return data
			}
		}
	}
	return killShellResultData{
		TaskID:     jsonLiteralStringField(object, "task_id", "taskId", "shell_id", "shellId", "bash_id", "bashId"),
		TaskStatus: jsonLiteralStringField(object, "task_status", "taskStatus", "status", "state"),
		Message:    jsonStringField(object, "message", "content", "output", "result", "text"),
		Stdout:     jsonStringField(object, "stdout"),
		Stderr:     jsonStringField(object, "stderr", "error"),
		ExitCode:   jsonIntField(object, "exit_code", "exitCode"),
	}
}

func bashOutputResultFieldsDepth(raw json.RawMessage, depth int) bashOutputResultData {
	if len(raw) == 0 || depth > 4 {
		return bashOutputResultData{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return bashOutputResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return bashOutputResultData{Stdout: encoded}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return bashOutputResultData{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result", "bash", "bashOutput", "bash_output"} {
		if nested, ok := object[key]; ok {
			data := bashOutputResultFieldsDepth(nested, depth+1)
			if data.TaskID != "" || data.Command != "" || data.TaskStatus != "" || data.Stdout != "" || data.Stderr != "" || data.ExitCode != nil || data.StdoutLines != 0 || data.StderrLines != 0 || data.Timestamp != "" {
				return data
			}
		}
	}
	data := bashOutputResultData{
		TaskID:     jsonLiteralStringField(object, "task_id", "taskId", "backgroundTaskId", "background_task_id", "bash_id", "bashId", "shell_id", "shellId"),
		Command:    jsonLiteralStringField(object, "command"),
		TaskStatus: jsonLiteralStringField(object, "task_status", "taskStatus", "status", "state"),
		Stdout:     firstNonEmptyString(jsonStringField(object, "stdout"), jsonStringField(object, "output")),
		Stderr:     jsonStringField(object, "stderr", "error"),
		ExitCode:   jsonIntField(object, "exit_code", "exitCode"),
		Timestamp:  jsonLiteralStringField(object, "timestamp"),
	}
	if stdoutLines := jsonIntField(object, "stdout_lines", "stdoutLines"); stdoutLines != nil {
		data.StdoutLines = *stdoutLines
	}
	if stderrLines := jsonIntField(object, "stderr_lines", "stderrLines"); stderrLines != nil {
		data.StderrLines = *stderrLines
	}
	return data
}

func taskResultFields(raw json.RawMessage, content string) taskResultData {
	data := taskResultFieldsDepth(raw, 0)
	if notification := taskNotificationFields(content); notification.TaskID != "" || notification.TaskStatus != "" || notification.Description != "" || notification.Output != "" || notification.ExitCode != nil {
		data.TaskID = firstNonEmptyString(data.TaskID, notification.TaskID)
		data.TaskStatus = firstNonEmptyString(data.TaskStatus, notification.TaskStatus)
		data.Description = firstNonEmptyString(data.Description, notification.Description)
		data.Output = firstNonEmptyString(data.Output, notification.Output)
		if data.ExitCode == nil {
			data.ExitCode = notification.ExitCode
		}
	}
	if taskID := shellSessionIDFromText(content); taskID != "" {
		data.TaskID = firstNonEmptyString(data.TaskID, taskID)
	}
	if data.Output == "" {
		output := commandOutputPayload(content)
		if !strings.HasPrefix(strings.TrimSpace(output), "{") {
			data.Output = output
		}
	}
	if data.Stdout == "" {
		data.Stdout = data.Output
	}
	return data
}

func shellSessionIDFromText(content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		for _, marker := range []string{
			"process running with session id",
			"session id",
			"session",
		} {
			index := strings.Index(lower, marker)
			if index < 0 {
				continue
			}
			after := strings.TrimSpace(line[index+len(marker):])
			after = strings.TrimLeft(after, " :#")
			token := firstWhitespaceDelimitedToken(after)
			token = strings.TrimRightFunc(token, func(r rune) bool {
				return (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_'
			})
			if token != "" {
				return token
			}
		}
	}
	return ""
}

func taskResultFieldsDepth(raw json.RawMessage, depth int) taskResultData {
	if len(raw) == 0 || depth > 4 {
		return taskResultData{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return taskResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return taskResultData{Output: encoded}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return taskResultData{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result", "task", "taskResult", "task_result"} {
		if nested, ok := object[key]; ok {
			data := taskResultFieldsDepth(nested, depth+1)
			if data.TaskID != "" || data.TaskType != "" || data.TaskStatus != "" || data.Description != "" || data.Output != "" || data.Stdout != "" || data.Stderr != "" || data.ExitCode != nil || data.TotalDurationMs != 0 || data.TotalTokens != 0 || data.TotalToolUseCount != 0 {
				return data
			}
		}
	}
	data := taskResultData{
		TaskID:      jsonLiteralStringField(object, "task_id", "taskId", "backgroundTaskId", "background_task_id", "bash_id", "bashId", "agent_id", "agentId"),
		TaskType:    jsonLiteralStringField(object, "task_type", "taskType", "task_kind", "taskKind", "subagent_type", "subagentType", "agent_type", "agentType"),
		TaskStatus:  jsonLiteralStringField(object, "task_status", "taskStatus", "status", "state"),
		Description: jsonLiteralStringField(object, "description", "summary", "title"),
		Output:      firstNonEmptyString(jsonStringField(object, "output"), jsonStringField(object, "content"), jsonStringField(object, "result"), jsonStringField(object, "text")),
		Stdout:      jsonStringField(object, "stdout"),
		Stderr:      jsonStringField(object, "stderr", "error"),
		ExitCode:    jsonIntField(object, "exit_code", "exitCode"),
	}
	if totalDurationMs := jsonIntField(object, "total_duration_ms", "totalDurationMs"); totalDurationMs != nil {
		data.TotalDurationMs = *totalDurationMs
	}
	if totalTokens := jsonIntField(object, "total_tokens", "totalTokens"); totalTokens != nil {
		data.TotalTokens = *totalTokens
	}
	if totalToolUseCount := jsonIntField(object, "total_tool_use_count", "totalToolUseCount"); totalToolUseCount != nil {
		data.TotalToolUseCount = *totalToolUseCount
	}
	if data.TaskID == "" && !hasProviderEnvelopeFields(object) {
		data.TaskID = jsonLiteralStringField(object, "id")
	}
	if data.TaskType == "" && !hasProviderEnvelopeFields(object) {
		data.TaskType = jsonLiteralStringField(object, "type", "kind")
	}
	if data.Output == "" && data.Stdout != "" {
		data.Output = data.Stdout
	}
	return data
}

func hasProviderEnvelopeFields(object map[string]json.RawMessage) bool {
	for _, key := range []string{"message", "uuid", "parentUuid", "toolUseID", "tool_use_id", "sourceToolAssistantUUID", "sessionId"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func taskNotificationFields(content string) taskResultData {
	if !strings.Contains(content, "<task-notification>") {
		return taskResultData{}
	}
	description := xmlTagValue(content, "summary")
	return taskResultData{
		TaskID:      xmlTagValue(content, "task-id"),
		TaskStatus:  xmlTagValue(content, "status"),
		Description: description,
		Output:      xmlTagValue(content, "output"),
		ExitCode:    taskExitCodeFromText(description),
	}
}

func xmlTagValue(content, tag string) string {
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"
	_, after, ok := strings.Cut(content, startTag)
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(after, endTag)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func taskExitCodeFromText(text string) *int {
	before, after, ok := strings.Cut(text, "exit code ")
	if !ok || !strings.Contains(before, "(") {
		return nil
	}
	value := strings.TrimRightFunc(after, func(r rune) bool {
		return r < '0' || r > '9'
	})
	value = firstWhitespaceDelimitedToken(value)
	out, ok := parseNonNegativeInt(value)
	if !ok {
		return nil
	}
	return &out
}

func resultCode(raw json.RawMessage) string {
	var object struct {
		Code   string `json:"code"`
		Script string `json:"script"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &object) == nil {
		return firstNonEmptyString(object.Code, object.Script)
	}
	return ""
}

func isPythonTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "code" {
		return true
	}
	switch name {
	case "python", "python_execution", "pythonexecution":
		return true
	default:
		return false
	}
}

func isCommandTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "command" {
		return true
	}
	switch name {
	case "bash", "shell", "sh", "run_command", "exec_command", "shell_command", "terminal", "terminal.exec":
		return true
	default:
		return false
	}
}

func isReadTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "file" {
		return true
	}
	switch name {
	case "read", "read_file", "view", "cat", "open_file":
		return true
	default:
		return false
	}
}

func isGlobTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "glob" {
		return true
	}
	return name == "glob" || strings.Contains(name, "glob")
}

func isFetchTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "fetch" {
		return true
	}
	switch name {
	case "webfetch", "web_fetch", "fetch", "fetch_url", "web.fetch":
		return true
	default:
		return strings.Contains(name, "fetch")
	}
}

func isTodoTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "todo" {
		return true
	}
	switch name {
	case "todowrite", "todo_write", "todo":
		return true
	default:
		return strings.Contains(name, "todo")
	}
}

func isPlanTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "plan" {
		return true
	}
	switch name {
	case "exitplanmode", "exit_plan_mode", "update_plan", "updateplan", "plan":
		return true
	default:
		return strings.Contains(name, "plan")
	}
}

func isQuestionTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "question" {
		return true
	}
	switch name {
	case "askuserquestion", "ask_user_question", "ask_user", "question":
		return true
	default:
		return strings.Contains(name, "question")
	}
}

func isStdinTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "stdin" {
		return true
	}
	switch name {
	case "writestdin", "write_stdin", "stdin", "send_stdin":
		return true
	default:
		return false
	}
}

func isTaskTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "task" {
		return true
	}
	switch name {
	case "task", "taskoutput", "task_output", "taskcreate", "task_create", "taskget", "task_get",
		"tasklist", "task_list", "taskupdate", "task_update", "taskstop", "task_stop",
		"bashoutput", "bash_output":
		return true
	default:
		return false
	}
}

func isBashOutputTool(name string) bool {
	switch name {
	case "bashoutput", "bash_output":
		return true
	default:
		return false
	}
}

func isKillShellTool(name string) bool {
	switch name {
	case "killshell", "kill_shell", "shellkill", "shell_kill":
		return true
	default:
		return false
	}
}

func isEditTool(name string, _ *StructuredToolInput) bool {
	if name == "" {
		return false
	}
	switch name {
	case "edit", "write", "write_file", "writefile", "multi_edit", "multiedit",
		"create_file", "replace", "search_replace", "searchreplace", "str_replace", "str_replace_editor":
		return true
	default:
		return strings.Contains(name, "edit") || strings.Contains(name, "write")
	}
}

func isWriteTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write", "write_file", "writefile", "create_file", "createfile":
		return true
	default:
		return false
	}
}

func isSearchTool(name string, input *StructuredToolInput) bool {
	if input != nil && input.Kind == "search" {
		return true
	}
	if isEditTool(name, input) {
		return false
	}
	return strings.Contains(name, "grep") ||
		strings.Contains(name, "search") ||
		name == "rg"
}

func searchResultMode(content string, input *StructuredToolInput) string {
	if input != nil && input.Query != "" && input.Pattern == "" {
		return "query"
	}
	if input != nil && grepCountCommand(input.Command) {
		return "count"
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if normalized == "" {
		return "files_with_matches"
	}
	if looksLikeGrepCountOutput(normalized) {
		return "count"
	}
	for _, line := range strings.Split(normalized, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) >= 3 && isAllASCIIDigits(parts[1]) {
			return "content"
		}
	}
	return "files_with_matches"
}

func grepCountCommand(command string) bool {
	args, err := shlex.Split(shellCommandForClassification(command))
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

func shellDerivedStructuredInput(command string) *StructuredToolInput {
	args, err := shlex.Split(shellCommandForClassification(command))
	if err != nil || len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "cat":
		if len(args) == 2 && !strings.HasPrefix(args[1], "-") {
			return fileStructuredInput(args[1])
		}
	case "sed":
		if len(args) == 4 && args[1] == "-n" && looksLikeSedAddress(args[2]) {
			return fileStructuredInput(args[3])
		}
	case "nl":
		if len(args) == 7 && args[1] == "-ba" && args[3] == "|" && args[4] == "sed" && args[5] == "-n" && looksLikeSedAddress(args[6]) {
			return fileStructuredInput(args[2])
		}
	case "rg", "grep":
		if shellArgsContainCompoundOperator(args) {
			return nil
		}
		pattern, paths := grepPatternAndPaths(args)
		if pattern == "" {
			return nil
		}
		input := &StructuredToolInput{
			Kind:    "search",
			Pattern: pattern,
		}
		if len(paths) == 1 {
			input.FilePath = paths[0]
		}
		for _, path := range paths {
			input.Arguments = append(input.Arguments, StructuredArgument{Name: "path", Value: path})
		}
		return input
	}
	return nil
}

func shellArgsContainCompoundOperator(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "|", "&&", "||", ";":
			return true
		}
	}
	return false
}

func shellCommandForClassification(command string) string {
	normalized := strings.TrimSpace(command)
	for range 3 {
		args, err := shlex.Split(normalized)
		if err != nil || len(args) == 0 {
			break
		}
		prefixLen := shellLauncherPrefixLength(args)
		if prefixLen == 0 || len(args) <= prefixLen {
			break
		}
		normalized = strings.TrimSpace(strings.Join(args[prefixLen:], " "))
	}
	return normalized
}

func shellLauncherPrefixLength(args []string) int {
	if len(args) < 3 {
		return 0
	}
	if executableName(args[0]) == "env" && len(args) >= 4 && isShellExecutable(args[1]) && args[2] == "-lc" {
		return 3
	}
	if isShellExecutable(args[0]) && args[1] == "-lc" {
		return 2
	}
	return 0
}

func isShellExecutable(token string) bool {
	switch executableName(token) {
	case "bash", "sh", "zsh", "dash":
		return true
	default:
		return false
	}
}

func executableName(token string) string {
	normalized := strings.ReplaceAll(token, `\`, "/")
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		normalized = normalized[idx+1:]
	}
	return strings.ToLower(normalized)
}

func fileStructuredInput(filePath string) *StructuredToolInput {
	return &StructuredToolInput{
		Kind:     "file",
		FilePath: filePath,
		Language: languageForPath(filePath),
	}
}

func languageForPath(path string) string {
	fileName := path
	if idx := strings.LastIndexAny(fileName, `/\`); idx >= 0 {
		fileName = fileName[idx+1:]
	}
	lower := strings.ToLower(fileName)
	switch lower {
	case "dockerfile":
		return "dockerfile"
	case "makefile":
		return "makefile"
	}
	ext := ""
	if idx := strings.LastIndex(lower, "."); idx >= 0 && idx < len(lower)-1 {
		ext = lower[idx+1:]
	}
	switch ext {
	case "ts":
		return "typescript"
	case "tsx":
		return "tsx"
	case "js", "mjs", "cjs":
		return "javascript"
	case "jsx":
		return "jsx"
	case "py":
		return "python"
	case "rs":
		return "rust"
	case "go":
		return "go"
	case "java":
		return "java"
	case "c", "h":
		return "c"
	case "cc", "cpp", "cxx", "hpp", "hh":
		return "cpp"
	case "json":
		return "json"
	case "yml", "yaml":
		return "yaml"
	case "toml":
		return "toml"
	case "md", "markdown", "mdx":
		return "markdown"
	case "html", "htm":
		return "html"
	case "css":
		return "css"
	case "scss":
		return "scss"
	case "sql":
		return "sql"
	case "sh", "bash", "zsh":
		return "bash"
	case "diff", "patch":
		return "diff"
	case "txt":
		return "text"
	default:
		return ""
	}
}

func grepPatternAndPaths(args []string) (string, []string) {
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
			if flagTakesValue(arg) && i+1 < len(args) {
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

func flagTakesValue(flag string) bool {
	switch flag {
	case "-e", "--regexp", "-g", "--glob", "-t", "--type", "-m", "--max-count", "-A", "--after-context", "-B", "--before-context", "-C", "--context":
		return true
	default:
		return false
	}
}

func commandOutputPayload(content string) string {
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
	if !strings.HasPrefix(strings.TrimSpace(before), "Command:") && !looksLikeCommandOutputWrapper(before) {
		return content
	}
	return strings.TrimPrefix(after, "\n")
}

func looksLikeCommandOutputWrapper(header string) bool {
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

func shellReadStripsLineNumbers(command string) bool {
	args, err := shlex.Split(shellCommandForClassification(command))
	return err == nil && len(args) > 0 && args[0] == "nl"
}

func stripShellReadLineNumbers(content string) string {
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

func shellReadRange(command string) (int, int) {
	args, err := shlex.Split(shellCommandForClassification(command))
	if err != nil || len(args) == 0 {
		return 0, 0
	}
	for _, arg := range args {
		start, end, ok := parseSedAddress(arg)
		if ok {
			return start, end
		}
	}
	return 0, 0
}

func looksLikeSedAddress(value string) bool {
	_, _, ok := parseSedAddress(value)
	return ok
}

func parseSedAddress(value string) (int, int, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "p")
	if value == "" {
		return 0, 0, false
	}
	startText, endText, hasComma := strings.Cut(value, ",")
	if !hasComma {
		line, ok := parsePositiveInt(startText)
		if !ok {
			return 0, 0, false
		}
		return line, line, true
	}
	start, ok := parsePositiveInt(startText)
	if !ok {
		return 0, 0, false
	}
	end, ok := parsePositiveInt(endText)
	if !ok {
		return 0, 0, false
	}
	return start, end, true
}

func parsePositiveInt(value string) (int, bool) {
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
	if out <= 0 {
		return 0, false
	}
	return out, true
}

func parseNonNegativeInt(value string) (int, bool) {
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

func searchResultFilenames(content string) []string {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var filename string
		if strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			filename = strings.TrimRight(firstWhitespaceDelimitedToken(line), ":")
		} else {
			var ok bool
			filename, _, ok = strings.Cut(line, ":")
			if !ok {
				continue
			}
		}
		filename = strings.TrimSpace(filename)
		if filename == "" || strings.Contains(filename, " ") {
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

func searchResultItems(object map[string]json.RawMessage, content string, input *StructuredToolInput) []StructuredSearchResultItem {
	if items := searchResultItemsFromRaw(object["result_items"]); len(items) > 0 {
		return items
	}
	if input == nil || strings.TrimSpace(input.Query) == "" || strings.TrimSpace(input.Pattern) != "" {
		return nil
	}
	return searchResultItemsFromURLLines(content)
}

func searchResultItemsFromRaw(raw json.RawMessage) []StructuredSearchResultItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []StructuredSearchResultItem
	if json.Unmarshal(raw, &items) == nil {
		return compactSearchResultItems(items)
	}
	return nil
}

func searchResultItemsFromURLLines(content string) []StructuredSearchResultItem {
	seen := make(map[string]struct{})
	var items []StructuredSearchResultItem
	for _, line := range strings.Split(commandOutputPayload(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		token := strings.TrimRight(firstWhitespaceDelimitedToken(line), ":")
		if !isHTTPURL(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, firstWhitespaceDelimitedToken(line))), ":"))
		title = strings.TrimSpace(strings.TrimPrefix(title, "-"))
		items = append(items, StructuredSearchResultItem{
			Title: title,
			URL:   token,
		})
		seen[token] = struct{}{}
	}
	return items
}

func compactSearchResultItems(items []StructuredSearchResultItem) []StructuredSearchResultItem {
	out := make([]StructuredSearchResultItem, 0, len(items))
	seen := make(map[string]struct{})
	for _, item := range items {
		item.Title = strings.TrimSpace(item.Title)
		item.URL = strings.TrimSpace(item.URL)
		item.Snippet = strings.TrimSpace(item.Snippet)
		if item.URL == "" && item.Title == "" && item.Snippet == "" {
			continue
		}
		key := firstNonEmptyString(item.URL, item.Title+"\x00"+item.Snippet)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func isHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func searchResultFilenamesForMode(content string, mode string) []string {
	if mode == "count" {
		counts, _ := searchResultCountsForMode(content, mode)
		return countResultFilenames(counts)
	}
	filenames := searchResultFilenames(content)
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

func searchResultCountsForMode(content string, mode string) ([]StructuredArgument, int) {
	if mode != "count" {
		return nil, 0
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(commandOutputPayload(content), "\r\n", "\n"))
	if normalized == "" {
		return nil, 0
	}
	counts := make([]StructuredArgument, 0)
	total := 0
	for _, line := range strings.Split(normalized, "\n") {
		name, value, ok := parseGrepCountLine(line)
		if !ok {
			continue
		}
		counts = append(counts, StructuredArgument{Name: name, Value: value})
		parsed, parsedOK := parseNonNegativeInt(value)
		if parsedOK {
			total += parsed
		}
	}
	return counts, total
}

func looksLikeGrepCountOutput(content string) bool {
	lines := strings.Split(content, "\n")
	seen := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, _, ok := parseGrepCountLine(line); !ok {
			return false
		}
		seen = true
	}
	return seen
}

func parseGrepCountLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	if value, ok := parseNonNegativeInt(line); ok {
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
	count, countOK := parseNonNegativeInt(value)
	if !countOK {
		return "", "", false
	}
	return name, fmt.Sprintf("%d", count), true
}

func countResultFilenames(counts []StructuredArgument) []string {
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

func structuredArgumentIntTotal(counts []StructuredArgument) int {
	total := 0
	for _, count := range counts {
		value, ok := parseNonNegativeInt(strings.TrimSpace(count.Value))
		if ok {
			total += value
		}
	}
	return total
}

func countSearchResults(content string, filenames []string) int {
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

func globResultFields(raw json.RawMessage, content string) (filenames []string, numFiles int, durationMs int, truncated bool) {
	filenames, numFiles, durationMs, truncated = globResultFieldsDepth(raw, 0)
	if len(filenames) == 0 {
		filenames = searchResultFilenamesForMode(commandOutputPayload(content), "files_with_matches")
	}
	if numFiles == 0 {
		numFiles = len(filenames)
	}
	return filenames, numFiles, durationMs, truncated
}

func globResultFieldsDepth(raw json.RawMessage, depth int) (filenames []string, numFiles int, durationMs int, truncated bool) {
	if len(raw) == 0 || depth > 4 {
		return nil, 0, 0, false
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return globResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return nil, 0, 0, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return nil, 0, 0, false
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			filenames, numFiles, durationMs, truncated = globResultFieldsDepth(nested, depth+1)
			if len(filenames) > 0 || numFiles != 0 || durationMs != 0 || truncated {
				return filenames, numFiles, durationMs, truncated
			}
		}
	}
	filenames = jsonStringSliceField(object, "filenames", "files", "paths")
	if numFilesPtr := jsonIntField(object, "num_files", "numFiles", "count"); numFilesPtr != nil {
		numFiles = *numFilesPtr
	}
	if durationMsPtr := jsonIntField(object, "duration_ms", "durationMs"); durationMsPtr != nil {
		durationMs = *durationMsPtr
	}
	truncated = jsonBoolField(object, "truncated")
	return filenames, numFiles, durationMs, truncated
}

type fetchResultData struct {
	URL        string
	StatusCode int
	StatusText string
	Bytes      int
	DurationMs int
	Content    string
}

func fetchResultFields(raw json.RawMessage, content string) fetchResultData {
	data := fetchResultFieldsDepth(raw, 0)
	if data.Content == "" {
		data.Content = commandOutputPayload(content)
		if strings.HasPrefix(strings.TrimSpace(data.Content), "{") {
			data.Content = ""
		}
	}
	return data
}

func fetchResultFieldsDepth(raw json.RawMessage, depth int) fetchResultData {
	if len(raw) == 0 || depth > 4 {
		return fetchResultData{}
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return fetchResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return fetchResultData{Content: encoded}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return fetchResultData{}
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			data := fetchResultFieldsDepth(nested, depth+1)
			if data.URL != "" || data.StatusCode != 0 || data.StatusText != "" || data.Bytes != 0 || data.DurationMs != 0 || data.Content != "" {
				return data
			}
		}
	}
	data := fetchResultData{
		URL:        jsonStringField(object, "url", "uri", "href"),
		StatusText: jsonStringField(object, "code_text", "codeText", "status_text", "statusText"),
		Content:    firstNonEmptyString(jsonStringField(object, "result"), jsonStringField(object, "content"), jsonStringField(object, "text"), jsonStringField(object, "output")),
	}
	if statusCode := jsonIntField(object, "status_code", "statusCode", "code"); statusCode != nil {
		data.StatusCode = *statusCode
	}
	if bytesValue := jsonIntField(object, "bytes", "byte_count", "byteCount"); bytesValue != nil {
		data.Bytes = *bytesValue
	}
	if durationMs := jsonIntField(object, "duration_ms", "durationMs"); durationMs != nil {
		data.DurationMs = *durationMs
	}
	return data
}

func questionInputFields(raw json.RawMessage) (question string, options []string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", nil
	}
	return jsonLiteralStringField(object, "question", "prompt"), jsonStringSliceField(object, "options", "choices")
}

func questionResultFields(raw json.RawMessage) (question string, answer string, options []string, answers []StructuredArgument, questions []StructuredQuestion) {
	return questionResultFieldsDepth(raw, 0)
}

func questionResultFieldsDepth(raw json.RawMessage, depth int) (question string, answer string, options []string, answers []StructuredArgument, questions []StructuredQuestion) {
	if len(raw) == 0 || depth > 4 {
		return "", "", nil, nil, nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return questionResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return "", "", nil, nil, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", nil, nil, nil
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			question, answer, options, answers, questions = questionResultFieldsDepth(nested, depth+1)
			if question != "" || answer != "" || len(options) > 0 || len(answers) > 0 || len(questions) > 0 {
				return question, answer, options, answers, questions
			}
		}
	}
	question = jsonLiteralStringField(object, "question", "prompt")
	answer = jsonLiteralStringField(object, "answer", "response", "choice", "result")
	options = jsonStringSliceField(object, "options", "choices")
	answers = argumentListFromObjectField(object, "answers", "answer_map", "answerMap")
	questions = structuredQuestionsFromRaw(object["questions"])
	if question == "" && len(questions) > 0 {
		question = questions[0].Question
	}
	if len(options) == 0 && len(questions) > 0 {
		options = structuredQuestionOptionLabels(questions[0].Options)
	}
	return question, answer, options, answers, questions
}

func structuredQuestionsFromRaw(raw json.RawMessage) []StructuredQuestion {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil
	}
	out := make([]StructuredQuestion, 0, len(items))
	for _, object := range items {
		question := StructuredQuestion{
			Question:    jsonLiteralStringField(object, "question", "prompt"),
			Header:      jsonLiteralStringField(object, "header", "title"),
			Options:     structuredQuestionOptionsFromRaw(object["options"]),
			MultiSelect: jsonBoolField(object, "multi_select", "multiSelect"),
		}
		if question.Question != "" || question.Header != "" || len(question.Options) > 0 {
			out = append(out, question)
		}
	}
	return out
}

func structuredQuestionOptionsFromRaw(raw json.RawMessage) []StructuredQuestionOption {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var objects []map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) == nil && len(objects) > 0 {
		out := make([]StructuredQuestionOption, 0, len(objects))
		for _, object := range objects {
			option := StructuredQuestionOption{
				Label:       jsonLiteralStringField(object, "label", "value", "text"),
				Description: jsonLiteralStringField(object, "description", "detail"),
			}
			if option.Label != "" || option.Description != "" {
				out = append(out, option)
			}
		}
		return out
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
		out := make([]StructuredQuestionOption, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				out = append(out, StructuredQuestionOption{Label: value})
			}
		}
		return out
	}
	return nil
}

func structuredQuestionOptionLabels(options []StructuredQuestionOption) []string {
	if len(options) == 0 {
		return nil
	}
	out := make([]string, 0, len(options))
	for _, option := range options {
		if option.Label != "" {
			out = append(out, option.Label)
		}
	}
	return out
}

func argumentListFromObjectField(object map[string]json.RawMessage, names ...string) []StructuredArgument {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		if args := argumentListFromRaw(raw); len(args) > 0 {
			return args
		}
	}
	return nil
}

func argumentListFromRaw(raw json.RawMessage) []StructuredArgument {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && len(object) > 0 {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]StructuredArgument, 0, len(keys))
		for _, key := range keys {
			if value, ok := structuredArgumentScalar(object[key]); ok && strings.TrimSpace(value) != "" {
				out = append(out, StructuredArgument{Name: key, Value: value})
			}
		}
		return out
	}
	var items []struct {
		Name  string `json:"name"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]StructuredArgument, 0, len(items))
	for _, item := range items {
		arg := StructuredArgument{
			Name:  firstNonEmptyString(strings.TrimSpace(item.Name), strings.TrimSpace(item.Key)),
			Value: strings.TrimSpace(item.Value),
		}
		if _, encodedContainer := decodeJSONStringContainer(arg.Value); encodedContainer {
			continue
		}
		if arg.Name != "" || arg.Value != "" {
			out = append(out, arg)
		}
	}
	return out
}

func planFieldsFromRaw(raw json.RawMessage) (plan string, explanation string, steps []StructuredPlanStep) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", nil
	}
	plan = jsonLiteralStringField(object, "plan")
	explanation = jsonLiteralStringField(object, "explanation", "summary", "reason")
	steps = planStepsFromObjectField(object, "steps", "plan")
	return plan, explanation, steps
}

func planResultFields(raw json.RawMessage) (plan string, explanation string, steps []StructuredPlanStep) {
	return planResultFieldsDepth(raw, 0)
}

func planResultFieldsDepth(raw json.RawMessage, depth int) (plan string, explanation string, steps []StructuredPlanStep) {
	if len(raw) == 0 || depth > 4 {
		return "", "", nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return planResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return "", "", nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", nil
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			plan, explanation, steps = planResultFieldsDepth(nested, depth+1)
			if plan != "" || explanation != "" || len(steps) > 0 {
				return plan, explanation, steps
			}
		}
	}
	return planFieldsFromRaw(raw)
}

func jsonLiteralStringField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return strings.TrimSpace(value)
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&number) == nil {
			return strings.TrimSpace(number.String())
		}
	}
	return ""
}

func planStepsFromObjectField(object map[string]json.RawMessage, names ...string) []StructuredPlanStep {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		steps := planStepsFromRaw(raw)
		if len(steps) > 0 {
			return steps
		}
	}
	return nil
}

func planStepsFromRaw(raw json.RawMessage) []StructuredPlanStep {
	var rawItems []json.RawMessage
	if json.Unmarshal(raw, &rawItems) != nil {
		return nil
	}
	out := make([]StructuredPlanStep, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var text string
		if json.Unmarshal(rawItem, &text) == nil {
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, StructuredPlanStep{Step: text})
			}
			continue
		}
		var item struct {
			Step   string `json:"step"`
			Text   string `json:"text"`
			Title  string `json:"title"`
			Status string `json:"status"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		step := StructuredPlanStep{
			Step:   firstNonEmptyString(strings.TrimSpace(item.Step), strings.TrimSpace(item.Text), strings.TrimSpace(item.Title)),
			Status: strings.TrimSpace(item.Status),
		}
		if step.Step != "" || step.Status != "" {
			out = append(out, step)
		}
	}
	return out
}

func todoResultFields(raw json.RawMessage) (oldTodos []StructuredTodoItem, newTodos []StructuredTodoItem) {
	return todoResultFieldsDepth(raw, 0)
}

func todoResultFieldsDepth(raw json.RawMessage, depth int) (oldTodos []StructuredTodoItem, newTodos []StructuredTodoItem) {
	if len(raw) == 0 || depth > 4 {
		return nil, nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return todoResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return nil, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return nil, nil
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			oldTodos, newTodos = todoResultFieldsDepth(nested, depth+1)
			if len(oldTodos) > 0 || len(newTodos) > 0 {
				return oldTodos, newTodos
			}
		}
	}
	return todoItemsFromObjectField(object, "oldTodos", "old_todos"), todoItemsFromObjectField(object, "newTodos", "new_todos")
}

func todoItemsFromRawField(raw json.RawMessage, names ...string) []StructuredTodoItem {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return todoItemsFromObjectField(object, names...)
}

func todoItemsFromObjectField(object map[string]json.RawMessage, names ...string) []StructuredTodoItem {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var items []struct {
			ID          string `json:"id"`
			Content     string `json:"content"`
			Status      string `json:"status"`
			ActiveForm  string `json:"activeForm"`
			ActiveForm2 string `json:"active_form"`
			Priority    string `json:"priority"`
		}
		if json.Unmarshal(raw, &items) != nil {
			continue
		}
		out := make([]StructuredTodoItem, 0, len(items))
		for _, item := range items {
			todo := StructuredTodoItem{
				ID:         strings.TrimSpace(item.ID),
				Content:    strings.TrimSpace(item.Content),
				Status:     strings.TrimSpace(item.Status),
				ActiveForm: firstNonEmptyString(item.ActiveForm, item.ActiveForm2),
				Priority:   strings.TrimSpace(item.Priority),
			}
			if todo.ID != "" || todo.Content != "" || todo.Status != "" || todo.ActiveForm != "" || todo.Priority != "" {
				out = append(out, todo)
			}
		}
		return out
	}
	return nil
}

func firstWhitespaceDelimitedToken(line string) string {
	for i, r := range line {
		if r == ' ' || r == '\t' {
			return line[:i]
		}
	}
	return line
}

func isAllASCIIDigits(value string) bool {
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

func commandResultFields(raw json.RawMessage, content string) (stdout string, stderr string, exitCode *int, interrupted bool, truncated bool, isImage bool) {
	stdout, stderr, exitCode, interrupted, truncated, isImage = commandResultFieldsDepth(raw, 0)
	if exitCode == nil {
		if parsed, ok := commandWrapperExitCode(firstNonEmptyString(stdout, stderr, content)); ok {
			exitCode = &parsed
		}
	}
	if stdout != "" {
		stdout = commandOutputPayload(stdout)
	}
	if stderr != "" {
		stderr = commandOutputPayload(stderr)
	}
	if stdout == "" && stderr == "" {
		stdout = content
	}
	return stdout, stderr, exitCode, interrupted, truncated, isImage
}

func commandWrapperExitCode(content string) (int, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{
			"Process exited with code",
			"Exit code:",
			"Exit code",
		} {
			if value, ok := valueAfterCaseInsensitivePrefix(line, prefix); ok {
				fields := strings.Fields(strings.TrimSpace(value))
				if len(fields) == 0 {
					continue
				}
				if parsed, parsedOK := parseNonNegativeInt(strings.Trim(fields[0], ":")); parsedOK {
					return parsed, true
				}
			}
		}
	}
	return 0, false
}

func valueAfterCaseInsensitivePrefix(value string, prefix string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
		return "", false
	}
	return strings.TrimSpace(value[len(prefix):]), true
}

func commandResultFieldsDepth(raw json.RawMessage, depth int) (stdout string, stderr string, exitCode *int, interrupted bool, truncated bool, isImage bool) {
	if len(raw) == 0 || depth > 4 {
		return "", "", nil, false, false, false
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return commandResultFieldsDepth(json.RawMessage(encoded), depth+1)
		}
		return "", "", nil, false, false, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return "", "", nil, false, false, false
	}
	for _, key := range []string{"tool_result", "toolUseResult", "provider_result"} {
		if nested, ok := object[key]; ok {
			stdout, stderr, exitCode, interrupted, truncated, isImage = commandResultFieldsDepth(nested, depth+1)
			if stdout != "" || stderr != "" || exitCode != nil || interrupted || truncated || isImage {
				return stdout, stderr, exitCode, interrupted, truncated, isImage
			}
		}
	}
	stdout = firstNonEmptyString(
		jsonStringField(object, "stdout"),
		jsonStringField(object, "output"),
		jsonStringField(object, "text"),
		jsonStringField(object, "result"),
	)
	stderr = jsonStringField(object, "stderr", "error")
	exitCode = jsonIntField(object, "exit_code", "exitCode")
	if exitCode == nil {
		if metadata, ok := object["metadata"]; ok {
			var metadataObject map[string]json.RawMessage
			if json.Unmarshal(metadata, &metadataObject) == nil {
				exitCode = jsonIntField(metadataObject, "exit_code", "exitCode")
			}
		}
	}
	interrupted = jsonBoolField(object, "interrupted") || jsonBoolField(object, "canceled")
	truncated = jsonBoolField(object, "truncated")
	isImage = jsonBoolField(object, "is_image") || jsonBoolField(object, "isImage")
	return stdout, stderr, exitCode, interrupted, truncated, isImage
}

func jsonIntField(object map[string]json.RawMessage, names ...string) *int {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value int
		if json.Unmarshal(raw, &value) == nil {
			return &value
		}
	}
	return nil
}

func jsonBoolField(object map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil && value {
			return true
		}
	}
	return false
}

func jsonBoolFieldPtr(object map[string]json.RawMessage, names ...string) *bool {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err == nil {
			return &value
		}
	}
	return nil
}

func jsonStringSliceField(object map[string]json.RawMessage, names ...string) []string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var values []string
		if json.Unmarshal(raw, &values) == nil {
			return compactStringSlice(values)
		}
		var rawValues []json.RawMessage
		if json.Unmarshal(raw, &rawValues) == nil {
			out := make([]string, 0, len(rawValues))
			for _, value := range rawValues {
				if text, ok := structuredJSONScalar(value); ok && strings.TrimSpace(text) != "" {
					out = append(out, text)
				}
			}
			return out
		}
	}
	return nil
}

func countLines(content string) int {
	content = strings.TrimRight(content, "\r\n")
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstString(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyStringSlice(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func compactStringSlice(values []string) []string {
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

func addUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
