package worker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestInferStructuredToolResultNormalizesPythonExecution(t *testing.T) {
	exitCode := 0
	raw := mustMarshalStructuredToolTest(t, struct {
		Code      string `json:"code"`
		Output    string `json:"output"`
		ExitCode  *int   `json:"exitCode"`
		Truncated bool   `json:"truncated"`
		Canceled  bool   `json:"canceled"`
	}{
		Code:      "print('hello')",
		Output:    "hello",
		ExitCode:  &exitCode,
		Truncated: true,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "python",
		Content: raw,
	}

	got := inferStructuredToolResult(block, structuredToolContext{}, "hello")
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "python" {
		t.Fatalf("Kind = %q, want python", got.Kind)
	}
	if got.Code != "print('hello')" {
		t.Fatalf("Code = %q, want python source", got.Code)
	}
	if got.Stdout != "hello" {
		t.Fatalf("Stdout = %q, want hello", got.Stdout)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0", got.ExitCode)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if got.Interrupted {
		t.Fatal("Interrupted = true, want false")
	}
}

func TestStructuredToolErrorClassifiesUserRejectionWithReason(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "Error: <tool_use_error>The user doesn't want to proceed with this tool use. The user provided the following reason for the rejection: too risky</tool_use_error>")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Edit",
		Content: raw,
		IsError: true,
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, structuredToolContext{Name: "Edit"}, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil || got.Error == nil {
		t.Fatalf("structured error = nil, result = %+v", got)
	}
	if got.Error.Category != "user_rejection_with_reason" {
		t.Fatalf("error category = %q, want user_rejection_with_reason; error = %+v", got.Error.Category, got.Error)
	}
	if got.Error.UserReason != "too risky" {
		t.Fatalf("error user reason = %q, want too risky; error = %+v", got.Error.UserReason, got.Error)
	}
	if got.Error.Message == "" || strings.Contains(got.Error.Message, "<tool_use_error>") || strings.HasPrefix(got.Error.Message, "Error:") {
		t.Fatalf("error message = %q, want cleaned provider-neutral message", got.Error.Message)
	}
}

func TestStructuredToolErrorClassifiesNonzeroExit(t *testing.T) {
	exitCode := 2
	raw := mustMarshalStructuredToolTest(t, struct {
		ExitCode *int `json:"exitCode"`
	}{ExitCode: &exitCode})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Bash",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "Bash",
		Input: &StructuredToolInput{
			Kind:    "command",
			Command: "npm test",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil || got.Error == nil {
		t.Fatalf("structured error = nil, result = %+v", got)
	}
	if got.Error.Category != "command_failure" || got.Error.Message != "Exit code 2" {
		t.Fatalf("error = %+v, want command_failure with exit code message", got.Error)
	}
}

func TestStructuredToolErrorClassifiesFileError(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "<tool_use_error>File has been modified since read</tool_use_error>")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Edit",
		Content: raw,
		IsError: true,
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, structuredToolContext{Name: "Edit"}, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil || got.Error == nil {
		t.Fatalf("structured error = nil, result = %+v", got)
	}
	if got.Error.Category != "file_error" || got.Error.Message != "File has been modified since read" {
		t.Fatalf("error = %+v, want file_error with cleaned message", got.Error)
	}
}

func TestInferStructuredToolResultNormalizesSearchFilenames(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "cmd/gc/dashboard/web/src/panels/crew.ts:230:logButton\ninternal/api/session_structured_types.go:351:inferStructuredToolResult\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "rg",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "rg",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "structured",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "grep" {
		t.Fatalf("Kind = %q, want grep", got.Kind)
	}
	if got.Mode != "content" {
		t.Fatalf("Mode = %q, want content", got.Mode)
	}
	wantFiles := []string{
		"cmd/gc/dashboard/web/src/panels/crew.ts",
		"internal/api/session_structured_types.go",
	}
	if len(got.Filenames) != len(wantFiles) {
		t.Fatalf("Filenames = %#v, want %#v", got.Filenames, wantFiles)
	}
	for i, want := range wantFiles {
		if got.Filenames[i] != want {
			t.Fatalf("Filenames[%d] = %q, want %q; all = %#v", i, got.Filenames[i], want, got.Filenames)
		}
	}
	if got.NumFiles != 2 {
		t.Fatalf("NumFiles = %d, want 2", got.NumFiles)
	}
	if got.NumLines != 2 {
		t.Fatalf("NumLines = %d, want 2", got.NumLines)
	}
}

func TestInferStructuredToolResultNormalizesFilesWithMatchesSearch(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "README.md\nsrc/app.ts\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "rg",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "rg",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "needle",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Mode != "files_with_matches" {
		t.Fatalf("Mode = %q, want files_with_matches; result = %+v", got.Mode, got)
	}
	if got.NumFiles != 2 {
		t.Fatalf("NumFiles = %d, want 2; result = %+v", got.NumFiles, got)
	}
	for _, want := range []string{"README.md", "src/app.ts"} {
		if !stringSliceContains(got.Filenames, want) {
			t.Fatalf("Filenames = %#v, missing %q", got.Filenames, want)
		}
	}
}

func TestInferStructuredToolResultNormalizesCountSearch(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "README.md:2\nsrc/app.ts:5\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "rg",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "rg",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "needle",
			Command: "rg -c needle README.md src/app.ts",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "grep" || got.Mode != "count" {
		t.Fatalf("result = %+v, want grep count mode", got)
	}
	if got.NumResults != 7 || got.NumFiles != 2 {
		t.Fatalf("counts summary = results %d files %d; result = %+v", got.NumResults, got.NumFiles, got)
	}
	if len(got.Counts) != 2 {
		t.Fatalf("Counts = %#v, want two per-file counts", got.Counts)
	}
	if got.Counts[0].Name != "README.md" || got.Counts[0].Value != "2" {
		t.Fatalf("Counts[0] = %+v, want README.md:2", got.Counts[0])
	}
	if got.Counts[1].Name != "src/app.ts" || got.Counts[1].Value != "5" {
		t.Fatalf("Counts[1] = %+v, want src/app.ts:5", got.Counts[1])
	}
	for _, want := range []string{"README.md", "src/app.ts"} {
		if !stringSliceContains(got.Filenames, want) {
			t.Fatalf("Filenames = %#v, missing %q", got.Filenames, want)
		}
	}
}

func TestInferStructuredToolResultNormalizesSingleFileCountSearch(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "3\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "grep",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "grep",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "needle",
			Command: "grep -c needle README.md",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Mode != "count" || got.NumResults != 3 {
		t.Fatalf("result = %+v, want count mode with 3 results", got)
	}
	if len(got.Counts) != 1 || got.Counts[0].Name != "matches" || got.Counts[0].Value != "3" {
		t.Fatalf("Counts = %#v, want matches:3", got.Counts)
	}
}

func TestInferStructuredToolResultPrefersNeutralReadResultFields(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Output     string `json:"output"`
		Content    string `json:"content"`
		FilePath   string `json:"file_path"`
		StartLine  int    `json:"start_line"`
		TotalLines int    `json:"total_lines"`
		NumLines   int    `json:"num_lines"`
	}{
		Output:     "    12\tline 12\n    13\tline 13\n",
		Content:    "line 12\nline 13\n",
		FilePath:   "src/app.ts",
		StartLine:  12,
		TotalLines: 13,
		NumLines:   2,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:     "file",
			FilePath: "src/app.ts",
			Command:  "cat src/app.ts",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Content != "line 12\nline 13\n" {
		t.Fatalf("Content = %q, want neutral content without line numbers", got.Content)
	}
	if got.StartLine != 12 || got.TotalLines != 13 || got.NumLines != 2 {
		t.Fatalf("line fields = start %d total %d num %d, want 12/13/2; result = %+v", got.StartLine, got.TotalLines, got.NumLines, got)
	}
}

func TestInferStructuredToolResultPrefersNeutralSearchResultFields(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Content      string               `json:"content"`
		Mode         string               `json:"mode"`
		Filenames    []string             `json:"filenames"`
		Counts       []StructuredArgument `json:"counts"`
		NumFiles     int                  `json:"num_files"`
		NumResults   int                  `json:"num_results"`
		NumLines     int                  `json:"num_lines"`
		AppliedLimit int                  `json:"applied_limit"`
	}{
		Content: "summary-only output\n",
		Mode:    "count",
		Filenames: []string{
			"README.md",
		},
		Counts: []StructuredArgument{
			{Name: "README.md", Value: "2"},
		},
		NumFiles:     1,
		NumResults:   2,
		NumLines:     1,
		AppliedLimit: 100,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "needle",
			Command: `rg needle README.md`,
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Mode != "count" || got.NumResults != 2 || got.NumFiles != 1 || got.NumLines != 1 || got.AppliedLimit != 100 {
		t.Fatalf("summary = mode %q results %d files %d lines %d applied_limit %d; result = %+v", got.Mode, got.NumResults, got.NumFiles, got.NumLines, got.AppliedLimit, got)
	}
	if len(got.Counts) != 1 || got.Counts[0].Name != "README.md" || got.Counts[0].Value != "2" {
		t.Fatalf("Counts = %#v, want README.md=2", got.Counts)
	}
	if len(got.Filenames) != 1 || got.Filenames[0] != "README.md" {
		t.Fatalf("Filenames = %#v, want README.md", got.Filenames)
	}
}

func TestInferStructuredToolResultNormalizesNoMatchSearchFromNeutralFields(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		ExitCode   int    `json:"exit_code"`
		Pattern    string `json:"pattern"`
		Mode       string `json:"mode"`
		NumFiles   int    `json:"num_files"`
		NumResults int    `json:"num_results"`
		NumLines   int    `json:"num_lines"`
	}{
		ExitCode:   1,
		Pattern:    "missing",
		Mode:       "files_with_matches",
		NumFiles:   0,
		NumResults: 0,
		NumLines:   0,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "missing",
			Command: `rg "missing" README.md`,
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "grep" || got.Mode != "files_with_matches" {
		t.Fatalf("result = %+v, want grep files_with_matches", got)
	}
	if got.NumFiles != 0 || got.NumResults != 0 || got.NumLines != 0 {
		t.Fatalf("summary = files %d results %d lines %d, want all zero; result = %+v", got.NumFiles, got.NumResults, got.NumLines, got)
	}
	if len(got.Filenames) != 0 || len(got.Counts) != 0 {
		t.Fatalf("result = %+v, want no filenames/counts for no-match search", got)
	}
}

func TestNormalizeStructuredToolInputKeepsGlobDistinctFromFileSearch(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}{
		Pattern: "**/*.go",
		Path:    "internal",
	})

	got := normalizeStructuredToolInput("Glob", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "glob" {
		t.Fatalf("Kind = %q, want glob; input = %+v", got.Kind, got)
	}
	if got.Pattern != "**/*.go" || got.FilePath != "internal" {
		t.Fatalf("glob input = %+v, want pattern and path", got)
	}
}

func TestInferStructuredToolResultNormalizesGlobResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Filenames  []string `json:"filenames"`
		DurationMs int      `json:"durationMs"`
		NumFiles   int      `json:"numFiles"`
		Truncated  bool     `json:"truncated"`
	}{
		Filenames:  []string{"internal/api/session_structured_types.go", "internal/worker/structured_tool.go"},
		DurationMs: 27,
		NumFiles:   2,
		Truncated:  true,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Glob",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "Glob",
		Input: &StructuredToolInput{
			Kind:    "glob",
			Pattern: "**/*.go",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "glob" {
		t.Fatalf("Kind = %q, want glob; result = %+v", got.Kind, got)
	}
	if got.NumFiles != 2 || got.DurationMs != 27 || !got.Truncated {
		t.Fatalf("glob result = %+v, want count/duration/truncated", got)
	}
	for _, want := range []string{"internal/api/session_structured_types.go", "internal/worker/structured_tool.go"} {
		if !stringSliceContains(got.Filenames, want) {
			t.Fatalf("Filenames = %#v, missing %q", got.Filenames, want)
		}
	}
	if got.Content != "internal/api/session_structured_types.go\ninternal/worker/structured_tool.go\n" {
		t.Fatalf("Content = %q, want filename list", got.Content)
	}
}

func TestNormalizeStructuredToolInputRecognizesWebFetch(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		URL    string `json:"url"`
		Prompt string `json:"prompt"`
	}{
		URL:    "https://example.com/spec",
		Prompt: "Extract the structured contract",
	})

	got := normalizeStructuredToolInput("WebFetch", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "fetch" {
		t.Fatalf("Kind = %q, want fetch; input = %+v", got.Kind, got)
	}
	if got.URL != "https://example.com/spec" || got.Prompt != "Extract the structured contract" {
		t.Fatalf("fetch input = %+v, want URL and prompt", got)
	}
}

func TestNormalizeStructuredToolInputRecognizesWrite(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"path":    "notes.txt",
		"content": "hello structured world",
	})

	got := normalizeStructuredToolInput("Write", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "write" {
		t.Fatalf("Kind = %q, want write; input = %+v", got.Kind, got)
	}
	if got.FilePath != "notes.txt" || got.Text != "hello structured world" {
		t.Fatalf("write input = %+v, want file path and content text", got)
	}
	if got.Language != "text" {
		t.Fatalf("Language = %q, want text; input = %+v", got.Language, got)
	}
	if got.Patch != "" {
		t.Fatalf("Patch = %q, want no fabricated input patch", got.Patch)
	}
}

func TestInferStructuredToolResultNormalizesWriteFileResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"type": "text",
			"file": map[string]any{
				"filePath":   "notes.txt",
				"content":    "hello structured world\n",
				"language":   "text",
				"numLines":   1,
				"startLine":  1,
				"totalLines": 1,
			},
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Write",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "Write",
		Input: &StructuredToolInput{
			Kind:     "write",
			FilePath: "notes.txt",
			Text:     "input text must not be copied into result",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "write" {
		t.Fatalf("Kind = %q, want write; result = %+v", got.Kind, got)
	}
	if got.FilePath != "notes.txt" || got.Language != "text" {
		t.Fatalf("write result file metadata = path %q language %q, want notes.txt/text; result = %+v", got.FilePath, got.Language, got)
	}
	if got.Content != "hello structured world\n" {
		t.Fatalf("Content = %q, want provider result file content", got.Content)
	}
	if got.NumLines != 1 || got.StartLine != 1 || got.TotalLines != 1 {
		t.Fatalf("write result range = num %d start %d total %d, want 1/1/1; result = %+v", got.NumLines, got.StartLine, got.TotalLines, got)
	}
	if got.Patch != "" || len(got.PatchHunks) != 0 {
		t.Fatalf("write result unexpectedly has patch data: %+v", got)
	}
	if got.Content == "input text must not be copied into result" {
		t.Fatalf("write result copied input content into result: %+v", got)
	}
}

func TestInferStructuredToolResultDoesNotGeneratePatchFromNeutralWriteContent(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"file_path": "notes.txt",
		"content":   "hello cursor\n",
		"num_lines": 1,
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Write",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "Write",
		Input: &StructuredToolInput{
			Kind:     "write",
			FilePath: "notes.txt",
			Text:     "hello cursor\n",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "write" {
		t.Fatalf("Kind = %q, want write; result = %+v", got.Kind, got)
	}
	if got.Content != "hello cursor\n" || got.FilePath != "notes.txt" || got.NumLines != 1 {
		t.Fatalf("write result fields = content %q path %q lines %d, want neutral write content", got.Content, got.FilePath, got.NumLines)
	}
	if got.Patch != "" || len(got.PatchHunks) != 0 {
		t.Fatalf("write result unexpectedly generated patch data: %+v", got)
	}
}

func TestInferStructuredToolResultNormalizesWebFetchResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		URL        string `json:"url"`
		Code       int    `json:"code"`
		CodeText   string `json:"codeText"`
		Bytes      int    `json:"bytes"`
		DurationMs int    `json:"durationMs"`
		Result     string `json:"result"`
	}{
		URL:        "https://example.com/spec",
		Code:       200,
		CodeText:   "OK",
		Bytes:      4096,
		DurationMs: 83,
		Result:     "Fetched structured spec content.\nSecond line.",
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "WebFetch",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "WebFetch",
		Input: &StructuredToolInput{
			Kind: "fetch",
			URL:  "https://example.com/spec",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "fetch" {
		t.Fatalf("Kind = %q, want fetch; result = %+v", got.Kind, got)
	}
	if got.URL != "https://example.com/spec" || got.StatusCode != 200 || got.StatusText != "OK" {
		t.Fatalf("fetch status = %+v, want URL/200/OK", got)
	}
	if got.Bytes != 4096 || got.DurationMs != 83 {
		t.Fatalf("fetch metrics = bytes %d duration %d, want 4096/83; result = %+v", got.Bytes, got.DurationMs, got)
	}
	if got.Content != "Fetched structured spec content.\nSecond line." || got.NumLines != 2 {
		t.Fatalf("fetch content = %q lines %d, want fetched content", got.Content, got.NumLines)
	}
}

func TestNormalizeStructuredToolInputRecognizesTodoWrite(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
			Priority   string `json:"priority"`
			ID         string `json:"id"`
		} `json:"todos"`
	}{
		Todos: []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
			Priority   string `json:"priority"`
			ID         string `json:"id"`
		}{{
			Content:    "Normalize structured todo data",
			Status:     "in_progress",
			ActiveForm: "Normalizing structured todo data",
			Priority:   "high",
			ID:         "todo-1",
		}},
	})

	got := normalizeStructuredToolInput("TodoWrite", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "todo" {
		t.Fatalf("Kind = %q, want todo; input = %+v", got.Kind, got)
	}
	if len(got.Todos) != 1 {
		t.Fatalf("Todos = %#v, want one typed todo", got.Todos)
	}
	todo := got.Todos[0]
	if todo.ID != "todo-1" || todo.Content != "Normalize structured todo data" || todo.Status != "in_progress" || todo.ActiveForm != "Normalizing structured todo data" || todo.Priority != "high" {
		t.Fatalf("Todos[0] = %+v, want full typed todo", todo)
	}
}

func TestInferStructuredToolResultNormalizesTodoWriteResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		OldTodos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"oldTodos"`
		NewTodos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"newTodos"`
	}{
		OldTodos: []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		}{{
			Content: "Review raw provider data",
			Status:  "pending",
		}},
		NewTodos: []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		}{{
			Content:    "Review raw provider data",
			Status:     "completed",
			ActiveForm: "Reviewing raw provider data",
		}},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "TodoWrite",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "TodoWrite",
		Input: &StructuredToolInput{
			Kind: "todo",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "todo" {
		t.Fatalf("Kind = %q, want todo; result = %+v", got.Kind, got)
	}
	if len(got.OldTodos) != 1 || got.OldTodos[0].Status != "pending" {
		t.Fatalf("OldTodos = %#v, want pending old todo", got.OldTodos)
	}
	if len(got.NewTodos) != 1 || got.NewTodos[0].Status != "completed" || got.NewTodos[0].ActiveForm != "Reviewing raw provider data" {
		t.Fatalf("NewTodos = %#v, want completed new todo with active form", got.NewTodos)
	}
}

func TestNormalizeStructuredToolInputRecognizesExitPlanMode(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Plan string `json:"plan"`
	}{
		Plan: "1. Inspect MC parsing\n2. Add typed GC data",
	})

	got := normalizeStructuredToolInput("ExitPlanMode", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "plan" {
		t.Fatalf("Kind = %q, want plan; input = %+v", got.Kind, got)
	}
	if got.Plan != "1. Inspect MC parsing\n2. Add typed GC data" {
		t.Fatalf("Plan = %q, want plan text", got.Plan)
	}
}

func TestNormalizeStructuredToolInputRecognizesUpdatePlanSteps(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Explanation string `json:"explanation"`
		Plan        []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}{
		Explanation: "Closing the MC gap",
		Plan: []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		}{{
			Step:   "Add typed plan DTOs",
			Status: "in_progress",
		}},
	})

	got := normalizeStructuredToolInput("update_plan", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "plan" {
		t.Fatalf("Kind = %q, want plan; input = %+v", got.Kind, got)
	}
	if got.Explanation != "Closing the MC gap" {
		t.Fatalf("Explanation = %q, want typed explanation", got.Explanation)
	}
	if len(got.Steps) != 1 || got.Steps[0].Step != "Add typed plan DTOs" || got.Steps[0].Status != "in_progress" {
		t.Fatalf("Steps = %#v, want one typed in-progress step", got.Steps)
	}
	if got.Plan != "" {
		t.Fatalf("Plan = %q, want empty text plan for step array", got.Plan)
	}
}

func TestInferStructuredToolResultNormalizesPlanResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"plan": "Ship typed plan data without HTML.",
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "ExitPlanMode",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "ExitPlanMode",
		Input: &StructuredToolInput{
			Kind: "plan",
			Plan: "Ship typed plan data without HTML.",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "plan" {
		t.Fatalf("Kind = %q, want plan; result = %+v", got.Kind, got)
	}
	if got.Plan != "Ship typed plan data without HTML." {
		t.Fatalf("Plan = %q, want result-side plan", got.Plan)
	}
}

func TestNormalizeStructuredToolInputRecognizesAskUserQuestion(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}{
		Question: "Proceed with typed question DTOs?",
		Options:  []string{"Yes", "No"},
	})

	got := normalizeStructuredToolInput("AskUserQuestion", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "question" {
		t.Fatalf("Kind = %q, want question; input = %+v", got.Kind, got)
	}
	if got.Question != "Proceed with typed question DTOs?" {
		t.Fatalf("Question = %q, want typed question", got.Question)
	}
	if len(got.Options) != 2 || got.Options[0] != "Yes" || got.Options[1] != "No" {
		t.Fatalf("Options = %#v, want Yes/No", got.Options)
	}
}

func TestInferStructuredToolResultNormalizesQuestionResult(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"questions": []map[string]any{
				{
					"question": "Select rollout scope",
					"header":   "Scope",
					"options": []map[string]string{
						{
							"label":       "All providers",
							"description": "Validate first-class and graceful providers",
						},
						{
							"label":       "Claude only",
							"description": "Narrow smoke test",
						},
					},
					"multi_select": true,
				},
			},
			"answer": "All providers",
			"answers": map[string]any{
				"Select rollout scope": "All providers",
			},
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "AskUserQuestion",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "AskUserQuestion",
		Input: &StructuredToolInput{
			Kind:     "question",
			Question: "Proceed with typed question DTOs?",
			Options:  []string{"Yes", "No"},
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "question" {
		t.Fatalf("Kind = %q, want question; result = %+v", got.Kind, got)
	}
	if got.Question != "Select rollout scope" || got.Answer != "All providers" {
		t.Fatalf("question result = %+v, want question and answer", got)
	}
	if len(got.Options) != 2 || got.Options[0] != "All providers" || got.Options[1] != "Claude only" {
		t.Fatalf("Options = %#v, want question option labels carried through", got.Options)
	}
	if len(got.Questions) != 1 || got.Questions[0].Question != "Select rollout scope" || got.Questions[0].Header != "Scope" || !got.Questions[0].MultiSelect {
		t.Fatalf("Questions = %#v, want typed multi-select question", got.Questions)
	}
	if len(got.Questions[0].Options) != 2 || got.Questions[0].Options[0].Label != "All providers" || got.Questions[0].Options[0].Description != "Validate first-class and graceful providers" {
		t.Fatalf("Question options = %#v, want label/description options", got.Questions[0].Options)
	}
	if len(got.Answers) != 1 || got.Answers[0].Name != "Select rollout scope" || got.Answers[0].Value != "All providers" {
		t.Fatalf("Answers = %#v, want selected answer", got.Answers)
	}
}

func TestNormalizeStructuredToolInputRecognizesTaskOutput(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		TaskID string `json:"task_id"`
		Block  bool   `json:"block"`
	}{
		TaskID: "task-123",
		Block:  true,
	})

	got := normalizeStructuredToolInput("TaskOutput", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "task" {
		t.Fatalf("Kind = %q, want task; input = %+v", got.Kind, got)
	}
	if got.TaskID != "task-123" {
		t.Fatalf("TaskID = %q, want task-123; input = %+v", got.TaskID, got)
	}
}

func TestInferStructuredToolResultNormalizesTaskOutput(t *testing.T) {
	exitCode := 0
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"taskId":            "task-123",
			"taskType":          "subagent",
			"status":            "completed",
			"description":       "Run delegated check",
			"output":            "delegated check passed",
			"exitCode":          exitCode,
			"totalDurationMs":   1234,
			"totalTokens":       321,
			"totalToolUseCount": 4,
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "TaskOutput",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "TaskOutput",
		Input: &StructuredToolInput{
			Kind:   "task",
			TaskID: "task-123",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "task" {
		t.Fatalf("Kind = %q, want task; result = %+v", got.Kind, got)
	}
	if got.TaskID != "task-123" || got.TaskType != "subagent" || got.TaskStatus != "completed" {
		t.Fatalf("task metadata = id %q type %q status %q, want task-123/subagent/completed; result = %+v", got.TaskID, got.TaskType, got.TaskStatus, got)
	}
	if got.Description != "Run delegated check" || got.Output != "delegated check passed" {
		t.Fatalf("task content = description %q output %q, want typed task text; result = %+v", got.Description, got.Output, got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0; result = %+v", got.ExitCode, got)
	}
	if got.TotalDurationMs != 1234 || got.TotalTokens != 321 || got.TotalToolUseCount != 4 {
		t.Fatalf("task aggregate metrics = duration %d tokens %d tools %d, want 1234/321/4; result = %+v", got.TotalDurationMs, got.TotalTokens, got.TotalToolUseCount, got)
	}
}

func TestInferStructuredToolResultNormalizesTaskNotificationText(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "<task-notification>\n<task-id>b9i7q3ww5</task-id>\n<status>completed</status>\n<summary>Background command \"Watch run\" completed (exit code 0)</summary>\n</task-notification>")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "TaskOutput",
		Content: raw,
	}
	context := structuredToolContext{
		Name:  "TaskOutput",
		Input: &StructuredToolInput{Kind: "task"},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.TaskID != "b9i7q3ww5" || got.TaskStatus != "completed" {
		t.Fatalf("task notification = id %q status %q, want b9i7q3ww5/completed; result = %+v", got.TaskID, got.TaskStatus, got)
	}
	if got.Description != `Background command "Watch run" completed (exit code 0)` {
		t.Fatalf("Description = %q, want notification summary; result = %+v", got.Description, got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0; result = %+v", got.ExitCode, got)
	}
}

func TestInferStructuredToolResultCarriesBackgroundTaskIDOnBash(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"stdout":           "",
			"stderr":           "",
			"backgroundTaskId": "b1ocqb4ca",
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Bash",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "Bash",
		Input: &StructuredToolInput{
			Kind:    "command",
			Command: "npm test",
		},
	}

	got := inferStructuredToolResult(block, context, "Command running in background with ID: b1ocqb4ca")
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "bash" {
		t.Fatalf("Kind = %q, want bash; result = %+v", got.Kind, got)
	}
	if got.TaskID != "b1ocqb4ca" {
		t.Fatalf("TaskID = %q, want b1ocqb4ca; result = %+v", got.TaskID, got)
	}
}

func TestInferStructuredToolResultNormalizesBashOutputMetadata(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"shellId":     "shell-123",
			"command":     "npm test",
			"status":      "completed",
			"exitCode":    0,
			"stdout":      "ok\n",
			"stderr":      "warn\n",
			"stdoutLines": 1,
			"stderrLines": 1,
			"timestamp":   "2026-06-01T00:00:02Z",
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "BashOutput",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "BashOutput",
		Input: &StructuredToolInput{
			Kind:   "task",
			TaskID: "shell-123",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "bash" {
		t.Fatalf("Kind = %q, want bash; result = %+v", got.Kind, got)
	}
	if got.TaskID != "shell-123" || got.Command != "npm test" || got.TaskStatus != "completed" {
		t.Fatalf("bash output metadata = task %q command %q status %q, want shell-123/npm test/completed; result = %+v", got.TaskID, got.Command, got.TaskStatus, got)
	}
	if got.Stdout != "ok\n" || got.Stderr != "warn\n" {
		t.Fatalf("bash output streams = stdout %q stderr %q, want ok/warn; result = %+v", got.Stdout, got.Stderr, got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0; result = %+v", got.ExitCode, got)
	}
	if got.StdoutLines != 1 || got.StderrLines != 1 || got.Timestamp != "2026-06-01T00:00:02Z" {
		t.Fatalf("bash output lines/timestamp = stdout %d stderr %d timestamp %q, want 1/1/2026-06-01T00:00:02Z; result = %+v", got.StdoutLines, got.StderrLines, got.Timestamp, got)
	}
}

func TestInferStructuredToolResultParsesCodexCommandWrapperExitCode(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]string{
		"output": strings.Join([]string{
			"Chunk ID: ddfdd1",
			"Wall time: 0.0000 seconds",
			"Process exited with code 7",
			"Original token count: 7",
			"Output:",
			"bad-err-codex",
			"bad-out-codex",
			"",
		}, "\n"),
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "command",
			Command: `sh -c 'printf "bad-out-codex\n"; printf "bad-err-codex\n" >&2; exit 7'`,
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("ExitCode = %v, want 7; result = %+v", got.ExitCode, got)
	}
	if got.Stdout != "bad-err-codex\nbad-out-codex\n" {
		t.Fatalf("Stdout = %q, want command output payload only; result = %+v", got.Stdout, got)
	}
	if got.Error == nil || got.Error.Category != "command_failure" {
		t.Fatalf("Error = %+v, want command_failure; result = %+v", got.Error, got)
	}
}

func TestAttachStructuredToolDataLinksWriteStdinToBashCommand(t *testing.T) {
	entries := []HistoryEntry{
		{
			Blocks: []HistoryBlock{
				{
					Kind:      BlockKindToolUse,
					ToolUseID: "call-bash",
					Name:      "Bash",
					Input:     mustMarshalStructuredToolTest(t, map[string]any{"command": "claude --resume"}),
				},
				{
					Kind:      BlockKindToolUse,
					ToolUseID: "call-stdin",
					Name:      "write_stdin",
					Input:     mustMarshalStructuredToolTest(t, map[string]any{"sessionId": 42, "content": "hello\n"}),
				},
			},
		},
		{
			Blocks: []HistoryBlock{
				{
					Kind:      BlockKindToolResult,
					ToolUseID: "call-bash",
					Content:   mustMarshalStructuredToolTest(t, "Process running with session ID: 42"),
				},
				{
					Kind:      BlockKindToolResult,
					ToolUseID: "call-stdin",
					Content:   mustMarshalStructuredToolTest(t, "sent"),
				},
			},
		},
	}

	got := attachStructuredToolData(entries)
	stdinInput := got[0].Blocks[1].StructuredInput
	if stdinInput == nil {
		t.Fatal("stdin structured input is nil")
	}
	if stdinInput.Kind != "stdin" || stdinInput.TaskID != "42" || stdinInput.Text != "hello\n" {
		t.Fatalf("stdin input = %+v, want neutral stdin task/text fields", stdinInput)
	}
	if stdinInput.LinkedCommand != "claude --resume" {
		t.Fatalf("stdin linked_command = %q, want claude --resume; input = %+v", stdinInput.LinkedCommand, stdinInput)
	}
	bashResult := got[1].Blocks[0].StructuredResult
	if bashResult == nil || bashResult.Kind != "bash" || bashResult.TaskID != "42" || bashResult.Command != "claude --resume" {
		t.Fatalf("bash result = %+v, want shell id and command for stdin correlation", bashResult)
	}
	stdinResult := got[1].Blocks[1].StructuredResult
	if stdinResult == nil || stdinResult.Kind != "stdin" || stdinResult.TaskID != "42" || stdinResult.Content != "sent" {
		t.Fatalf("stdin result = %+v, want neutral stdin result", stdinResult)
	}
}

func TestInferStructuredToolResultNormalizesKillShellMetadata(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"toolUseResult": map[string]any{
			"shell_id": "shell-123",
			"message":  "Shell shell-123 killed",
		},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "KillShell",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "KillShell",
		Input: &StructuredToolInput{
			Kind:   "task",
			TaskID: "shell-123",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "bash" {
		t.Fatalf("Kind = %q, want bash; result = %+v", got.Kind, got)
	}
	if got.TaskID != "shell-123" {
		t.Fatalf("TaskID = %q, want shell-123; result = %+v", got.TaskID, got)
	}
	if got.Stdout != "Shell shell-123 killed" || got.Content != "Shell shell-123 killed" {
		t.Fatalf("kill shell text = stdout %q content %q, want typed message; result = %+v", got.Stdout, got.Content, got)
	}
}

func TestInferStructuredToolResultCarriesSearchQueryAndCount(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "Output:\nhttps://example.com/provider-format: Provider format notes\nhttps://example.com/typed-wire: Typed wire notes\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "web_search",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "web_search",
		Input: &StructuredToolInput{
			Kind:  "search",
			Query: "structured tool result formats",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "search" {
		t.Fatalf("Kind = %q, want search; result = %+v", got.Kind, got)
	}
	if got.Query != "structured tool result formats" {
		t.Fatalf("Query = %q, want structured tool result formats; result = %+v", got.Query, got)
	}
	if got.NumResults != 2 {
		t.Fatalf("NumResults = %d, want 2; result = %+v", got.NumResults, got)
	}
	if len(got.ResultItems) != 2 {
		t.Fatalf("ResultItems = %#v, want two URL result items", got.ResultItems)
	}
	if got.ResultItems[0].URL != "https://example.com/provider-format" || got.ResultItems[0].Title != "Provider format notes" {
		t.Fatalf("ResultItems[0] = %+v, want provider format title/url", got.ResultItems[0])
	}
	for _, want := range []string{"https://example.com/provider-format", "https://example.com/typed-wire"} {
		if !stringSliceContains(got.Filenames, want) {
			t.Fatalf("Filenames = %#v, missing %q", got.Filenames, want)
		}
	}
}

func TestInferStructuredToolResultCarriesNeutralSearchResultItems(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"query":       "structured stream format",
		"duration_ms": 1250,
		"result_items": []map[string]string{
			{
				"title":   "Structured Stream Format",
				"url":     "https://example.com/structured",
				"snippet": "Provider-neutral typed data.",
			},
		},
		"content": "searched",
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "WebSearch",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "WebSearch",
		Input: &StructuredToolInput{
			Kind:  "search",
			Query: "structured stream format",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "search" || got.Query != "structured stream format" || got.NumResults != 1 {
		t.Fatalf("result = %+v, want search query with one result", got)
	}
	if got.DurationMs != 1250 {
		t.Fatalf("DurationMs = %d, want 1250; result = %+v", got.DurationMs, got)
	}
	if len(got.ResultItems) != 1 || got.ResultItems[0].Title != "Structured Stream Format" || got.ResultItems[0].URL != "https://example.com/structured" || got.ResultItems[0].Snippet != "Provider-neutral typed data." {
		t.Fatalf("ResultItems = %#v, want typed title/url/snippet", got.ResultItems)
	}
}

func TestNormalizeStructuredToolInputOmitsProviderNativeFallbackFields(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"query":       "structured tool result formats",
		"url":         "https://example.com/search",
		"task_id":     42,
		"description": true,
		"action": map[string]any{
			"source": "web",
			"type":   "search",
		},
		"encoded_action": `{"source":"web","type":"search"}`,
		"native_list":    []any{"web", map[string]any{"source": "provider"}},
		"scope":          "web",
	})

	got := normalizeStructuredToolInput("web_search", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "search" {
		t.Fatalf("Kind = %q, want search; input = %+v", got.Kind, got)
	}
	if got.Query != "structured tool result formats" {
		t.Fatalf("Query = %q, want structured tool result formats; input = %+v", got.Query, got)
	}
	if got.URL != "https://example.com/search" {
		t.Fatalf("URL = %q, want typed search URL; input = %+v", got.URL, got)
	}
	if got.TaskID != "42" || got.Description != "true" {
		t.Fatalf("known neutral scalar fields = task_id %q description %q, want 42/true; input = %+v", got.TaskID, got.Description, got)
	}
	if len(got.Arguments) != 0 {
		t.Fatalf("Arguments = %+v, want provider-native fallback fields omitted", got.Arguments)
	}
}

func TestNormalizeStructuredToolInputOmitsUnknownObjectFallback(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"action": map[string]any{
			"source": "web",
			"type":   "search",
		},
		"encoded_action": `{"source":"web","type":"search"}`,
		"native_list":    []any{"web", map[string]any{"source": "provider"}},
		"scope":          "web",
	})

	if got := normalizeStructuredToolInput("provider_native_tool", raw); got != nil {
		t.Fatalf("normalizeStructuredToolInput() = %+v, want unknown provider object omitted", got)
	}
}

func TestNormalizeStructuredToolInputPreservesExplicitJSONText(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"text": `{"user_supplied":true}`,
	})

	got := normalizeStructuredToolInput("display_text", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "text" || got.Text != `{"user_supplied":true}` {
		t.Fatalf("normalizeStructuredToolInput() = %+v, want explicit JSON text preserved", got)
	}
}

func TestStructuredJSONFieldsKeepsOnlyScalarValues(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"bool":   true,
		"list":   []any{"one", map[string]any{"native": "value"}},
		"null":   nil,
		"number": 42,
		"object": map[string]any{"native": "value"},
		"string": "value",
	})

	got := structuredJSONFields(raw)
	want := []StructuredArgument{
		{Name: "bool", Value: "true"},
		{Name: "number", Value: "42"},
		{Name: "string", Value: "value"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structuredJSONFields() = %+v, want scalar-only %+v", got, want)
	}
}

func TestArgumentListFromRawOmitsNestedValues(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"count":        7,
		"label":        "matches",
		"native":       map[string]any{"source": "provider"},
		"items":        []any{"one", "two"},
		"encoded":      `{"source":"provider"}`,
		"encoded_list": `["provider"]`,
	})

	got := argumentListFromRaw(raw)
	want := []StructuredArgument{
		{Name: "count", Value: "7"},
		{Name: "label", Value: "matches"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argumentListFromRaw() = %+v, want scalar-only %+v", got, want)
	}
}

func TestJSONStringSliceFieldOmitsNestedValues(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, map[string]any{
		"values": []any{"one", 2, true, map[string]any{"source": "provider"}, []string{"nested"}},
	})
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := jsonStringSliceField(object, "values")
	want := []string{"one", "2", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("jsonStringSliceField() = %v, want scalar-only %v", got, want)
	}
}

func TestNormalizeStructuredToolInputDerivesCodexShellRead(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: "sed -n '12,14p' src/app.ts",
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "file" {
		t.Fatalf("Kind = %q, want file; input = %+v", got.Kind, got)
	}
	if got.FilePath != "src/app.ts" {
		t.Fatalf("FilePath = %q, want src/app.ts; input = %+v", got.FilePath, got)
	}
	if got.Language != "typescript" {
		t.Fatalf("Language = %q, want typescript; input = %+v", got.Language, got)
	}
	if got.Command != "sed -n '12,14p' src/app.ts" {
		t.Fatalf("Command = %q, want original command; input = %+v", got.Command, got)
	}
}

func TestNormalizeStructuredToolInputDerivesWrappedShellRead(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: `/usr/bin/env bash -lc "sed -n '12,14p' src/app.ts"`,
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "file" {
		t.Fatalf("Kind = %q, want file; input = %+v", got.Kind, got)
	}
	if got.FilePath != "src/app.ts" || got.Language != "typescript" {
		t.Fatalf("file metadata = path %q language %q, want src/app.ts/typescript; input = %+v", got.FilePath, got.Language, got)
	}
	if got.Command != `/usr/bin/env bash -lc "sed -n '12,14p' src/app.ts"` {
		t.Fatalf("Command = %q, want original wrapped command; input = %+v", got.Command, got)
	}
}

func TestNormalizeStructuredToolInputDerivesSimpleCatRead(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: "cat README.md",
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "file" || got.FilePath != "README.md" || got.Language != "markdown" {
		t.Fatalf("input = %+v, want README.md file read", got)
	}
}

func TestNormalizeStructuredToolInputDoesNotDeriveCompoundCatRead(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: "cat README.md | head",
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "command" {
		t.Fatalf("Kind = %q, want command for compound cat; input = %+v", got.Kind, got)
	}
	if got.FilePath != "" || got.Language != "" {
		t.Fatalf("compound cat derived file metadata unexpectedly: %+v", got)
	}
}

func TestNormalizeStructuredToolInputDerivesNestedWrappedShellGrep(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: `/bin/bash -lc "/bin/sh -lc 'rg -n needle README.md src/app.ts'"`,
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "search" {
		t.Fatalf("Kind = %q, want search; input = %+v", got.Kind, got)
	}
	if got.Pattern != "needle" {
		t.Fatalf("Pattern = %q, want needle; input = %+v", got.Pattern, got)
	}
	for _, want := range []string{"README.md", "src/app.ts"} {
		if !structuredArgumentsContain(got.Arguments, "path", want) {
			t.Fatalf("Arguments = %+v, missing path %q", got.Arguments, want)
		}
	}
	if got.Command != `/bin/bash -lc "/bin/sh -lc 'rg -n needle README.md src/app.ts'"` {
		t.Fatalf("Command = %q, want original wrapped command; input = %+v", got.Command, got)
	}
}

func TestNormalizeStructuredToolInputDoesNotDeriveCompoundGrep(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, struct {
		Command string `json:"cmd"`
	}{
		Command: "rg needle README.md | head",
	})

	got := normalizeStructuredToolInput("exec_command", raw)
	if got == nil {
		t.Fatal("normalizeStructuredToolInput returned nil")
	}
	if got.Kind != "command" {
		t.Fatalf("Kind = %q, want command for compound grep; input = %+v", got.Kind, got)
	}
	if got.Pattern != "" || got.FilePath != "" || len(got.Arguments) != 0 {
		t.Fatalf("compound grep derived search metadata unexpectedly: %+v", got)
	}
}

func TestInferStructuredToolResultUsesDerivedReadContent(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "Command: sed -n '12,14p' src/app.ts\nOutput:\nline 12\nline 13\nline 14\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:     "file",
			FilePath: "src/app.ts",
			Command:  "sed -n '12,14p' src/app.ts",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "read" {
		t.Fatalf("Kind = %q, want read; result = %+v", got.Kind, got)
	}
	if got.Content != "line 12\nline 13\nline 14\n" {
		t.Fatalf("Content = %q, want command output only; result = %+v", got.Content, got)
	}
	if got.FilePath != "src/app.ts" {
		t.Fatalf("FilePath = %q, want src/app.ts; result = %+v", got.FilePath, got)
	}
	if got.Language != "typescript" {
		t.Fatalf("Language = %q, want typescript; result = %+v", got.Language, got)
	}
}

func TestInferStructuredToolResultStripsNumberedReadOutput(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "Command: nl -ba src/app.ts | sed -n '12,14p'\nOutput:\n    12\tline 12\n    13\tline 13\n    14\tline 14\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:     "file",
			FilePath: "src/app.ts",
			Command:  "nl -ba src/app.ts | sed -n '12,14p'",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "read" {
		t.Fatalf("Kind = %q, want read; result = %+v", got.Kind, got)
	}
	if got.Content != "line 12\nline 13\nline 14\n" {
		t.Fatalf("Content = %q, want line numbers stripped; result = %+v", got.Content, got)
	}
	if got.StartLine != 12 || got.TotalLines != 14 || got.NumLines != 3 {
		t.Fatalf("range = start %d total %d lines %d, want 12/14/3; result = %+v", got.StartLine, got.TotalLines, got.NumLines, got)
	}
}

func TestInferStructuredToolResultStripsWrappedNumberedReadOutput(t *testing.T) {
	command := `/bin/bash -lc "nl -ba src/app.ts | sed -n '12,14p'"`
	raw := mustMarshalStructuredToolTest(t, "Command: "+command+"\nOutput:\n    12\tline 12\n    13\tline 13\n    14\tline 14\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:     "file",
			FilePath: "src/app.ts",
			Command:  command,
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "read" {
		t.Fatalf("Kind = %q, want read; result = %+v", got.Kind, got)
	}
	if got.Content != "line 12\nline 13\nline 14\n" {
		t.Fatalf("Content = %q, want line numbers stripped after wrapper unwrapping; result = %+v", got.Content, got)
	}
	if got.StartLine != 12 || got.TotalLines != 14 || got.NumLines != 3 {
		t.Fatalf("range = start %d total %d lines %d, want 12/14/3; result = %+v", got.StartLine, got.TotalLines, got.NumLines, got)
	}
}

func TestInferStructuredToolResultRecognizesWrappedGrepCount(t *testing.T) {
	command := `/bin/bash -lc "rg -c needle README.md src/app.ts"`
	raw := mustMarshalStructuredToolTest(t, "README.md:2\nsrc/app.ts:5\n")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "search",
			Pattern: "needle",
			Command: command,
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "grep" || got.Mode != "count" {
		t.Fatalf("result = %+v, want grep count mode after wrapper unwrapping", got)
	}
	if got.NumResults != 7 {
		t.Fatalf("NumResults = %d, want 7; result = %+v", got.NumResults, got)
	}
}

func TestInferStructuredToolResultParsesJSONStringCommandOutput(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, `{"stdout":"ok ./...\n","stderr":"","exit_code":0}`)
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "command",
			Command: "go test ./...",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "bash" {
		t.Fatalf("Kind = %q, want bash; result = %+v", got.Kind, got)
	}
	if got.Stdout != "ok ./...\n" {
		t.Fatalf("Stdout = %q, want parsed stdout; result = %+v", got.Stdout, got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0; result = %+v", got.ExitCode, got)
	}
}

func TestInferStructuredToolResultClassifiesUserRejectionWithReason(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "Error: The user doesn't want to proceed with this tool use. The user provided the following reason for the rejection: Use a smaller patch")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Edit",
		Content: raw,
		IsError: true,
	}
	context := structuredToolContext{
		Name: "Edit",
		Input: &StructuredToolInput{
			Kind:     "patch",
			FilePath: "README.md",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Error == nil {
		t.Fatalf("Error = nil, want classified user rejection; result = %+v", got)
	}
	if got.Error.Category != "user_rejection_with_reason" || got.Error.UserReason != "Use a smaller patch" {
		t.Fatalf("Error = %+v, want user rejection with reason", got.Error)
	}
	if strings.Contains(got.Error.Message, "Error:") {
		t.Fatalf("Error message = %q, want cleaned message", got.Error.Message)
	}
}

func TestInferStructuredToolResultClassifiesValidationError(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, "<tool_use_error>old_string not found in file</tool_use_error>")
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Edit",
		Content: raw,
		IsError: true,
	}
	context := structuredToolContext{
		Name: "Edit",
		Input: &StructuredToolInput{
			Kind:     "patch",
			FilePath: "README.md",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Error == nil {
		t.Fatalf("Error = nil, want classified validation error; result = %+v", got)
	}
	if got.Error.Category != "validation_error" {
		t.Fatalf("Error = %+v, want validation error", got.Error)
	}
	if got.Error.Message != "old_string not found in file" {
		t.Fatalf("Error message = %q, want stripped tool_use_error text", got.Error.Message)
	}
}

func TestInferStructuredToolResultClassifiesCommandExitError(t *testing.T) {
	raw := mustMarshalStructuredToolTest(t, `{"stdout":"","stderr":"npm ERR! test failed","exit_code":1}`)
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "exec_command",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "exec_command",
		Input: &StructuredToolInput{
			Kind:    "command",
			Command: "npm test",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Error == nil {
		t.Fatalf("Error = nil, want classified command failure; result = %+v", got)
	}
	if got.Error.Category != "command_failure" || got.Error.Message != "npm ERR! test failed" {
		t.Fatalf("Error = %+v, want command failure from exit code", got.Error)
	}
}

func TestInferStructuredToolResultExposesTypedPatchHunks(t *testing.T) {
	replaceAll := false
	userModified := false
	raw := mustMarshalStructuredToolTest(t, struct {
		FilePath        string `json:"filePath"`
		OldString       string `json:"oldString"`
		NewString       string `json:"newString"`
		OriginalFile    string `json:"originalFile"`
		ReplaceAll      bool   `json:"replaceAll"`
		UserModified    bool   `json:"userModified"`
		StructuredPatch []struct {
			OldStart int      `json:"oldStart"`
			OldLines int      `json:"oldLines"`
			NewStart int      `json:"newStart"`
			NewLines int      `json:"newLines"`
			Lines    []string `json:"lines"`
		} `json:"structuredPatch"`
	}{
		FilePath:     "README.md",
		OldString:    "old",
		NewString:    "new",
		OriginalFile: "old\n",
		ReplaceAll:   replaceAll,
		UserModified: userModified,
		StructuredPatch: []struct {
			OldStart int      `json:"oldStart"`
			OldLines int      `json:"oldLines"`
			NewStart int      `json:"newStart"`
			NewLines int      `json:"newLines"`
			Lines    []string `json:"lines"`
		}{{
			OldStart: 3,
			OldLines: 1,
			NewStart: 3,
			NewLines: 1,
			Lines:    []string{"-old", "+new"},
		}},
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "Edit",
		Content: raw,
	}

	got := inferStructuredToolResult(block, structuredToolContext{Name: "Edit"}, "The file README.md has been updated successfully.")
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "edit" {
		t.Fatalf("Kind = %q, want edit; result = %+v", got.Kind, got)
	}
	if len(got.PatchHunks) != 1 {
		t.Fatalf("PatchHunks = %#v, want one hunk; result = %+v", got.PatchHunks, got)
	}
	hunk := got.PatchHunks[0]
	if hunk.FilePath != "README.md" || hunk.OldStart != 3 || hunk.NewStart != 3 {
		t.Fatalf("PatchHunks[0] = %+v, want README.md hunk at line 3", hunk)
	}
	if len(hunk.Lines) != 2 || hunk.Lines[0] != "-old" || hunk.Lines[1] != "+new" {
		t.Fatalf("PatchHunks[0].Lines = %#v, want typed diff lines", hunk.Lines)
	}
	if got.OldString != "old" || got.NewString != "new" || got.OriginalFile != "old\n" {
		t.Fatalf("edit metadata = old %q new %q original %q, want result-side edit context", got.OldString, got.NewString, got.OriginalFile)
	}
	if got.ReplaceAll == nil || *got.ReplaceAll != replaceAll {
		t.Fatalf("ReplaceAll = %v, want explicit false", got.ReplaceAll)
	}
	if got.UserModified == nil || *got.UserModified != userModified {
		t.Fatalf("UserModified = %v, want explicit false", got.UserModified)
	}
	if !stringSliceContains(got.FilePaths, "README.md") {
		t.Fatalf("FilePaths = %#v, want README.md", got.FilePaths)
	}
}

func TestInferStructuredToolResultDoesNotFabricateEditPatchFromInput(t *testing.T) {
	// A high-level edit tool (the isEditTool path, e.g. Edit/str_replace) whose
	// RESULT carries no result-side patch evidence. The tool INPUT supplies a
	// patch and file content; none of it may be fabricated into a result-side
	// diff. This is the symmetric counterpart to the apply_patch guard, covering
	// the isEditTool branch rather than name == "apply_patch".
	inputPatch := strings.Join([]string{
		"@@ -1 +1 @@",
		"-old line",
		"+new line",
	}, "\n")
	raw := mustMarshalStructuredToolTest(t, map[string]string{
		"filePath": "/tmp/project/app.go",
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "str_replace",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "str_replace",
		Input: &StructuredToolInput{
			Kind:     "edit",
			FilePath: "/tmp/project/app.go",
			Patch:    inputPatch,
			Code:     "new line\n",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "edit" {
		t.Fatalf("Kind = %q, want edit; result = %+v", got.Kind, got)
	}
	if got.Patch != "" || len(got.PatchHunks) != 0 {
		t.Fatalf("patch data = patch %q hunks %#v, want no input-derived result patch", got.Patch, got.PatchHunks)
	}
	if got.OldString != "" || got.NewString != "" {
		t.Fatalf("old/new = %q/%q, want empty: the result carried none and input must not leak", got.OldString, got.NewString)
	}
	if got.FilePath != "/tmp/project/app.go" {
		t.Fatalf("FilePath = %q, want input file path for edit association", got.FilePath)
	}
}

func TestInferStructuredToolResultDoesNotUseInputPatchForCodexApplyPatch(t *testing.T) {
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: /tmp/project/src/app.ts",
		"*** Add File: /tmp/project/src/app.ts",
		"+before direct codex",
		"+after direct live structured codex",
		"*** End Patch",
	}, "\n")
	raw := mustMarshalStructuredToolTest(t, map[string]string{
		"output": "Success. Updated the following files:\nM /tmp/project/src/app.ts\n",
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "apply_patch",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "apply_patch",
		Input: &StructuredToolInput{
			Kind:     "patch",
			Patch:    patch,
			FilePath: "/tmp/project/src/app.ts",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "edit" {
		t.Fatalf("Kind = %q, want edit; result = %+v", got.Kind, got)
	}
	if got.Patch != "" || len(got.PatchHunks) != 0 {
		t.Fatalf("patch data = patch %q hunks %#v, want no input-derived result patch", got.Patch, got.PatchHunks)
	}
	if got.FilePath != "/tmp/project/src/app.ts" {
		t.Fatalf("FilePath = %q, want input file path for edit association", got.FilePath)
	}
	if !strings.Contains(got.Content, "Success. Updated the following files") {
		t.Fatalf("Content = %q, want provider result output preserved", got.Content)
	}
}

func TestInferStructuredToolResultIgnoresSuccessfulCodexEditWrapper(t *testing.T) {
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: /tmp/project/src/app.ts",
		"@@",
		"-before-codex",
		"+after-codex",
		"*** End Patch",
	}, "\n")
	raw := mustMarshalStructuredToolTest(t, map[string]string{
		"output": strings.Join([]string{
			"Exit code: 0",
			"Wall time: 0 seconds",
			"Output:",
			"Success. Updated the following files:",
			"M /tmp/project/src/app.ts",
			"",
		}, "\n"),
	})
	block := HistoryBlock{
		Kind:    BlockKindToolResult,
		Name:    "apply_patch",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "apply_patch",
		Input: &StructuredToolInput{
			Kind:     "patch",
			Patch:    patch,
			FilePath: "/tmp/project/src/app.ts",
		},
	}

	got := attachStructuredToolError(inferStructuredToolResult(block, context, structuredJSONText(raw)), block, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Error != nil {
		t.Fatalf("Error = %+v, want nil for successful edit wrapper; result = %+v", got.Error, got)
	}
	if got.Content != "Success. Updated the following files:\nM /tmp/project/src/app.ts\n" {
		t.Fatalf("Content = %q, want command output payload only; result = %+v", got.Content, got)
	}
	if got.Patch != "" || len(got.PatchHunks) != 0 {
		t.Fatalf("patch data = patch %q hunks %#v, want no input-derived result patch", got.Patch, got.PatchHunks)
	}
}

func mustMarshalStructuredToolTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal structured fixture: %v", err)
	}
	return out
}

func structuredArgumentsContain(args []StructuredArgument, name string, value string) bool {
	for _, arg := range args {
		if arg.Name == name && arg.Value == value {
			return true
		}
	}
	return false
}
