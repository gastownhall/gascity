package api

import (
	"context"
	"crypto/md5" //nolint:gosec // Kimi transcript fixtures use the provider's MD5 workdir layout.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/testutil"
)

// isolateProviderDiscovery points provider transcript discovery at an empty,
// per-test HOME so the structured handler tests never wander into the
// developer's real provider session directories (for example a large
// ~/.codex). Real provider dirs make discovery slow and the no-transcript
// downgrade path nondeterministic against the streaming read deadline; this
// keeps these tests hermetic regardless of the host machine.
func isolateProviderDiscovery(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
}

func TestHandleSessionTranscriptStructuredNormalizesFirstClassProviders(t *testing.T) {
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}

	tests := []struct {
		name               string
		provider           string
		writeFixture       func(t *testing.T, root, workDir, sessionKey string)
		toolCallID         string
		toolName           string
		inputKind          string
		inputFilePath      string
		inputURL           string
		inputPrompt        string
		inputQuestion      string
		inputOptions       []string
		inputCommand       string
		inputQuery         string
		inputPattern       string
		inputText          string
		inputPlan          string
		inputStepCount     int
		inputTodoCount     int
		inputArguments     map[string]string
		resultKind         string
		resultFile         string
		resultContent      string
		resultStdout       string
		resultExit         *int
		resultFiles        []string
		resultItemURLs     []string
		resultMode         string
		resultQuery        string
		resultCount        int
		resultURL          string
		resultStatus       int
		resultStatusText   string
		resultBytes        int
		resultDuration     int
		resultAppliedLimit int
		resultTruncated    bool
		resultQuestion     string
		resultQuestions    int
		resultAnswer       string
		resultAnswers      int
		resultPlan         string
		resultStepCount    int
		resultOldTodos     int
		resultNewTodos     int
		resultPatch        []string
		resultOldString    string
		resultNewString    string
		resultOriginalFile string
		resultReplaceAll   *bool
		resultUserModified *bool
		resultAbsent       []string
	}{
		{
			name:          "claude read",
			provider:      "claude",
			writeFixture:  writeStructuredClaudeReadFixture,
			toolCallID:    "call-claude-read",
			toolName:      "Read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Gas City README",
		},
		{
			name:               "claude edit",
			provider:           "claude",
			writeFixture:       writeStructuredClaudeEditFixture,
			toolCallID:         "call-claude-edit",
			toolName:           "Edit",
			inputKind:          "patch",
			inputFilePath:      "README.md",
			resultKind:         "edit",
			resultFile:         "README.md",
			resultContent:      "updated successfully",
			resultPatch:        []string{"--- README.md", "+++ README.md", "-export const message = \"old line\";", "+export const message = \"new line\";"},
			resultOldString:    "old line",
			resultNewString:    "new line",
			resultOriginalFile: "export const message = \"old line\";\n",
			resultReplaceAll:   boolPtr(false),
			resultUserModified: boolPtr(false),
		},
		{
			name:          "codex patch",
			provider:      "codex",
			writeFixture:  writeStructuredCodexPatchFixture,
			toolCallID:    "call-codex-patch",
			toolName:      "apply_patch",
			inputKind:     "patch",
			inputFilePath: "city.toml",
			resultKind:    "edit",
			resultFile:    "city.toml",
			resultContent: "Updated the following files",
			resultPatch:   []string{"--- city.toml", "+++ city.toml", "+[workspace]"},
		},
		{
			name:          "codex shell read",
			provider:      "codex",
			writeFixture:  writeStructuredCodexShellReadFixture,
			toolCallID:    "call-codex-read",
			toolName:      "exec_command",
			inputKind:     "file",
			inputFilePath: "src/app.ts",
			inputCommand:  "sed -n '12,14p' src/app.ts",
			resultKind:    "read",
			resultFile:    "src/app.ts",
			resultContent: "line 13",
			resultAbsent:  []string{"Command:", "Output:"},
		},
		{
			name:          "codex wrapped shell read",
			provider:      "codex",
			writeFixture:  writeStructuredCodexWrappedShellReadFixture,
			toolCallID:    "call-codex-wrapped-read",
			toolName:      "exec_command",
			inputKind:     "file",
			inputFilePath: "src/app.ts",
			inputCommand:  `/usr/bin/env bash -lc "sed -n '12,14p' src/app.ts"`,
			resultKind:    "read",
			resultFile:    "src/app.ts",
			resultContent: "line 13",
			resultAbsent:  []string{"Command:", "Output:"},
		},
		{
			name:         "codex shell grep",
			provider:     "codex",
			writeFixture: writeStructuredCodexShellGrepFixture,
			toolCallID:   "call-codex-grep",
			toolName:     "exec_command",
			inputKind:    "search",
			inputCommand: "rg -n \"needle\" README.md src/app.ts",
			inputPattern: "needle",
			resultKind:   "grep",
			resultMode:   "content",
			resultFiles:  []string{"README.md", "src/app.ts"},
			resultAbsent: []string{"Command:", "Output:"},
		},
		{
			name:         "codex json string command output",
			provider:     "codex",
			writeFixture: writeStructuredCodexJSONStringCommandFixture,
			toolCallID:   "call-codex-json-command",
			toolName:     "exec_command",
			inputKind:    "command",
			inputCommand: "go test ./...",
			resultKind:   "bash",
			resultStdout: "ok ./...\n",
			resultExit:   intPtr(0),
			resultAbsent: []string{"{\"stdout\""},
		},
		{
			name:           "codex web search",
			provider:       "codex",
			writeFixture:   writeStructuredCodexWebSearchFixture,
			toolCallID:     "call-codex-web-search",
			toolName:       "web_search",
			inputKind:      "search",
			inputQuery:     "structured tool result formats",
			resultKind:     "search",
			resultMode:     "query",
			resultQuery:    "structured tool result formats",
			resultCount:    1,
			resultFiles:    []string{"https://example.com/provider-format"},
			resultItemURLs: []string{"https://example.com/provider-format"},
		},
		{
			name:            "claude glob",
			provider:        "claude",
			writeFixture:    writeStructuredClaudeGlobFixture,
			toolCallID:      "call-claude-glob",
			toolName:        "Glob",
			inputKind:       "glob",
			inputFilePath:   "internal",
			inputPattern:    "**/*.go",
			resultKind:      "glob",
			resultFiles:     []string{"internal/api/session_structured_types.go", "internal/worker/structured_tool.go"},
			resultDuration:  27,
			resultTruncated: true,
		},
		{
			name:               "claude grep",
			provider:           "claude",
			writeFixture:       writeStructuredClaudeGrepFixture,
			toolCallID:         "call-claude-grep",
			toolName:           "Grep",
			inputKind:          "search",
			inputFilePath:      "README.md",
			inputPattern:       "needle",
			resultKind:         "grep",
			resultMode:         "content",
			resultFiles:        []string{"README.md"},
			resultContent:      "README.md:1:needle",
			resultAppliedLimit: 100,
		},
		{
			name:           "claude web search",
			provider:       "claude",
			writeFixture:   writeStructuredClaudeWebSearchFixture,
			toolCallID:     "call-claude-search",
			toolName:       "WebSearch",
			inputKind:      "search",
			inputQuery:     "structured stream format",
			resultKind:     "search",
			resultQuery:    "structured stream format",
			resultCount:    1,
			resultDuration: 1250,
			resultItemURLs: []string{"https://example.com/structured"},
		},
		{
			name:             "claude web fetch",
			provider:         "claude",
			writeFixture:     writeStructuredClaudeWebFetchFixture,
			toolCallID:       "call-claude-fetch",
			toolName:         "WebFetch",
			inputKind:        "fetch",
			inputURL:         "https://example.com/spec",
			inputPrompt:      "Extract the structured contract",
			resultKind:       "fetch",
			resultURL:        "https://example.com/spec",
			resultStatus:     200,
			resultStatusText: "OK",
			resultBytes:      4096,
			resultDuration:   83,
			resultContent:    "Fetched structured spec content.",
		},
		{
			name:           "claude todo write",
			provider:       "claude",
			writeFixture:   writeStructuredClaudeTodoWriteFixture,
			toolCallID:     "call-claude-todo",
			toolName:       "TodoWrite",
			inputKind:      "todo",
			inputTodoCount: 1,
			resultKind:     "todo",
			resultOldTodos: 1,
			resultNewTodos: 2,
			resultContent:  "todos updated",
		},
		{
			name:          "claude exit plan mode",
			provider:      "claude",
			writeFixture:  writeStructuredClaudeExitPlanFixture,
			toolCallID:    "call-claude-plan",
			toolName:      "ExitPlanMode",
			inputKind:     "plan",
			inputPlan:     "Inspect MC and expose typed plan data.",
			resultKind:    "plan",
			resultPlan:    "Inspect MC and expose typed plan data.",
			resultContent: "plan captured",
		},
		{
			name:            "claude ask user question",
			provider:        "claude",
			writeFixture:    writeStructuredClaudeAskQuestionFixture,
			toolCallID:      "call-claude-question",
			toolName:        "AskUserQuestion",
			inputKind:       "question",
			inputQuestion:   "Proceed with typed question DTOs?",
			inputOptions:    []string{"Yes", "No"},
			resultKind:      "question",
			resultQuestion:  "Select rollout scope",
			resultQuestions: 1,
			resultAnswer:    "All providers",
			resultAnswers:   1,
			resultContent:   "question answered",
		},
		{
			name:         "gemini grep",
			provider:     "gemini",
			writeFixture: writeStructuredGeminiGrepFixture,
			toolCallID:   "call-gemini-grep",
			toolName:     "grep_search",
			inputKind:    "search",
			inputPattern: "needle",
			resultKind:   "grep",
			resultFiles:  []string{"README.md", "main.go"},
		},
		{
			name:          "gemini write fileDiff",
			provider:      "gemini",
			writeFixture:  writeStructuredGeminiWriteFixture,
			toolCallID:    "call-gemini-write",
			toolName:      "write_file",
			inputKind:     "write",
			inputFilePath: "notes.txt",
			inputText:     "hello gemini",
			resultKind:    "write",
			resultFile:    "notes.txt",
			resultContent: "Successfully created",
			resultPatch:   []string{"Index: notes.txt", "+hello gemini"},
		},
		{
			name:          "gemini write content pair",
			provider:      "gemini",
			writeFixture:  writeStructuredGeminiWriteContentPairFixture,
			toolCallID:    "call-gemini-write",
			toolName:      "write_file",
			inputKind:     "write",
			inputFilePath: "notes.txt",
			inputText:     "hello gemini",
			resultKind:    "write",
			resultFile:    "notes.txt",
			resultContent: "Successfully created",
			resultPatch:   []string{"--- notes.txt", "-old text", "+hello gemini"},
		},
		{
			name:          "kimi read",
			provider:      "kimi",
			writeFixture:  writeStructuredKimiReadFixture,
			toolCallID:    "call-kimi-read",
			toolName:      "Read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Kimi file data",
		},
		{
			name:          "kimi edit result patch",
			provider:      "kimi",
			writeFixture:  writeStructuredKimiEditPatchFixture,
			toolCallID:    "call-kimi-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
			resultPatch:   []string{"--- README.md", "-old", "+new"},
		},
		{
			name:          "opencode edit",
			provider:      "opencode",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:          "opencode edit result patch",
			provider:      "opencode",
			writeFixture:  writeStructuredOpenCodeEditPatchResultFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
			resultPatch:   []string{"--- README.md", "-old", "+new"},
		},
		{
			name:          "groq opencode alias edit",
			provider:      "groq",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:          "cerebras opencode alias edit",
			provider:      "cerebras",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:         "mimocode bash",
			provider:     "mimocode",
			writeFixture: writeStructuredMimoCodeBashFixture,
			toolCallID:   "call-mimocode-bash",
			toolName:     "Bash",
			inputKind:    "command",
			inputCommand: "go test ./...",
			resultKind:   "bash",
			resultStdout: "ok ./...",
			resultExit:   intPtr(0),
		},
		{
			name:         "mimocode bash git diff stays command",
			provider:     "mimocode",
			writeFixture: writeStructuredMimoCodeBashDiffFixture,
			toolCallID:   "call-mimocode-diff",
			toolName:     "Bash",
			inputKind:    "command",
			inputCommand: "git diff -- src/app.ts",
			resultKind:   "bash",
			resultStdout: "diff --git a/src/app.ts b/src/app.ts\n@@\n-old\n+new",
		},
		{
			name:         "claude bash nested toolUseResult",
			provider:     "claude",
			writeFixture: writeStructuredClaudeBashToolUseResultFixture,
			toolCallID:   "call-claude-bash",
			toolName:     "Bash",
			inputKind:    "command",
			inputCommand: "npm test",
			resultKind:   "bash",
			resultStdout: "tests passed\n",
			resultExit:   intPtr(0),
		},
		{
			name:         "claude kill shell",
			provider:     "claude",
			writeFixture: writeStructuredClaudeKillShellFixture,
			toolCallID:   "call-claude-kill",
			toolName:     "KillShell",
			inputKind:    "task",
			resultKind:   "bash",
			resultStdout: "Shell shell-123 killed",
		},
		{
			name:          "pi read",
			provider:      "pi",
			writeFixture:  writeStructuredPiReadFixture,
			toolCallID:    "call-pi-read",
			toolName:      "read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Pi file data",
		},
		{
			name:          "pi edit result patch",
			provider:      "pi",
			writeFixture:  writeStructuredPiEditPatchFixture,
			toolCallID:    "call-pi-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
			resultPatch:   []string{"--- README.md", "-old", "+new"},
		},
		{
			name:          "omp pi alias read",
			provider:      "omp",
			writeFixture:  writeStructuredPiReadFixture,
			toolCallID:    "call-pi-read",
			toolName:      "read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Pi file data",
		},
		{
			name:          "kiro acp write result patch",
			provider:      "kiro",
			writeFixture:  writeStructuredKiroWritePatchFixture,
			toolCallID:    "call-kiro-write",
			toolName:      "write",
			inputKind:     "write",
			inputFilePath: "notes.txt",
			inputText:     "hello kiro\n",
			resultKind:    "write",
			resultFile:    "notes.txt",
			resultPatch:   []string{"*** Update File: notes.txt", "-old", "+hello kiro"},
		},
		{
			name:            "amp stream-json edit result patch",
			provider:        "amp",
			writeFixture:    writeStructuredAmpEditPatchFixture,
			toolCallID:      "call-amp-edit",
			toolName:        "edit_file",
			inputKind:       "patch",
			inputFilePath:   "notes.txt",
			resultKind:      "edit",
			resultFile:      "notes.txt",
			resultPatch:     []string{"*** Update File: notes.txt", "-old", "+new"},
			resultOldString: "old",
			resultNewString: "new",
		},
		{
			name:          "cursor stream-json write",
			provider:      "cursor",
			writeFixture:  writeStructuredCursorWriteFixture,
			toolCallID:    "call-cursor-write",
			toolName:      "Write",
			inputKind:     "write",
			inputFilePath: "notes.txt",
			inputText:     "hello cursor\n",
			resultKind:    "write",
			resultFile:    "notes.txt",
			resultContent: "hello cursor",
			resultAbsent:  []string{"fileText", "linesCreated", "fileSize"},
		},
		{
			name:          "cursor stream-json read",
			provider:      "cursor",
			writeFixture:  writeStructuredCursorReadFixture,
			toolCallID:    "call-cursor-read",
			toolName:      "Read",
			inputKind:     "file",
			inputFilePath: "src/app.ts",
			resultKind:    "read",
			resultFile:    "src/app.ts",
			resultContent: "export const app = true;",
			resultAbsent:  []string{"readToolCall", "toolCallId", "totalLines", "totalChars"},
		},
		{
			name:         "cursor stream-json bash",
			provider:     "cursor",
			writeFixture: writeStructuredCursorBashFixture,
			toolCallID:   "call-cursor-bash",
			toolName:     "Bash",
			inputKind:    "command",
			inputCommand: "npm test",
			resultKind:   "bash",
			resultStdout: "ok\n",
			resultExit:   intPtr(0),
			resultAbsent: []string{"exitCode"},
		},
		{
			name:            "grok acp edit result patch",
			provider:        "grok",
			writeFixture:    writeStructuredGrokACPEditPatchFixture,
			toolCallID:      "call-grok-edit",
			toolName:        "search_replace",
			inputKind:       "patch",
			inputFilePath:   "notes.txt",
			resultKind:      "edit",
			resultFile:      "notes.txt",
			resultPatch:     []string{"*** Update File: notes.txt", "-old", "+new"},
			resultOldString: "old",
			resultNewString: "new",
		},
		{
			name:            "auggie acp edit result patch",
			provider:        "auggie",
			writeFixture:    writeStructuredAuggieACPEditPatchFixture,
			toolCallID:      "call-auggie-edit",
			toolName:        "str-replace-editor",
			inputKind:       "patch",
			inputFilePath:   "notes.txt",
			resultKind:      "edit",
			resultFile:      "notes.txt",
			resultPatch:     []string{"*** Update File: notes.txt", "-old", "+new"},
			resultOldString: "old",
			resultNewString: "new",
		},
		{
			name:          "antigravity write",
			provider:      "antigravity",
			writeFixture:  writeStructuredAntigravityWriteFixture,
			toolCallID:    "call-antigravity-write",
			toolName:      "Write",
			inputKind:     "write",
			inputFilePath: "notes.txt",
			inputText:     "hello structured world",
			resultKind:    "write",
			resultFile:    "notes.txt",
			resultContent: "wrote notes.txt",
		},
		{
			name:          "antigravity write result patch",
			provider:      "antigravity",
			writeFixture:  writeStructuredAntigravityEditPatchFixture,
			toolCallID:    "call-antigravity-edit",
			toolName:      "Edit",
			inputKind:     "patch",
			inputFilePath: "notes.txt",
			resultKind:    "edit",
			resultFile:    "notes.txt",
			resultContent: "Edited notes.txt",
			resultPatch:   []string{"--- notes.txt", "-old", "+new"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			searchBase := t.TempDir()
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)
			srv.sessionLogSearchPaths = []string{searchBase}

			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			workDir := t.TempDir()
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: tt.provider, WorkDir: workDir, Provider: tt.provider, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			tt.writeFixture(t, searchBase, info.WorkDir, info.SessionKey)

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			body := w.Body.Bytes()
			var resp sessionTranscriptGetResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Format != "structured" {
				t.Fatalf("Format = %q, want structured", resp.Format)
			}
			if resp.SchemaVersion != sessionStructuredSchemaVersion {
				t.Fatalf("SchemaVersion = %q, want %q", resp.SchemaVersion, sessionStructuredSchemaVersion)
			}
			if resp.History == nil || resp.History.TranscriptStreamID == "" {
				t.Fatalf("structured response missing history envelope: %+v", resp.History)
			}

			toolUse, toolResult := findStructuredToolPair(structuredTranscriptMessages(resp), tt.toolCallID)
			if toolUse == nil {
				t.Fatalf("missing tool_use %q in structured messages: %+v", tt.toolCallID, structuredTranscriptMessages(resp))
			}
			if toolResult == nil {
				t.Fatalf("missing tool_result %q in structured messages: %+v", tt.toolCallID, structuredTranscriptMessages(resp))
			}
			if toolUse.Name != tt.toolName {
				t.Fatalf("tool name = %q, want %q", toolUse.Name, tt.toolName)
			}
			if toolUse.Input == nil {
				t.Fatalf("tool input is nil")
			}
			assertStructuredInput(t, toolUse.Input, tt.inputKind, tt.inputFilePath, tt.inputURL, tt.inputPrompt, tt.inputQuestion, tt.inputOptions, tt.inputCommand, tt.inputQuery, tt.inputPattern, tt.inputText, tt.inputPlan, tt.inputStepCount, tt.inputTodoCount)
			assertStructuredInputArguments(t, toolUse.Input.Arguments, tt.inputArguments)
			assertStructuredResult(t, toolResult.Structured, tt.resultKind, tt.resultFile, tt.resultContent, tt.resultStdout, tt.resultExit, tt.resultFiles, tt.resultItemURLs, tt.resultMode, tt.resultQuery, tt.resultCount, tt.resultURL, tt.resultStatus, tt.resultStatusText, tt.resultBytes, tt.resultDuration, tt.resultAppliedLimit, tt.resultTruncated, tt.resultQuestion, tt.resultQuestions, tt.resultAnswer, tt.resultAnswers, tt.resultPlan, tt.resultStepCount, tt.resultOldTodos, tt.resultNewTodos, tt.resultPatch, tt.resultOldString, tt.resultNewString, tt.resultOriginalFile, tt.resultReplaceAll, tt.resultUserModified, tt.resultAbsent)

			assertNoStructuredWireLeak(t, body)
		})
	}
}

func TestHandleSessionTranscriptStructuredGracefullyDowngradesAllBuiltinProviders(t *testing.T) {
	isolateProviderDiscovery(t)
	for _, provider := range config.BuiltinProviderOrder() {
		t.Run(provider, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: provider, WorkDir: t.TempDir(), Provider: provider, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fs.sp.SetPeekOutput(info.SessionName, provider+" pane output")

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0&include_thinking=true", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp sessionTranscriptGetResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Format != "structured" {
				t.Fatalf("Format = %q, want structured; body: %s", resp.Format, w.Body.String())
			}
			if resp.SchemaVersion != sessionStructuredSchemaVersion {
				t.Fatalf("SchemaVersion = %q, want %q", resp.SchemaVersion, sessionStructuredSchemaVersion)
			}
			if resp.History == nil {
				t.Fatal("History is nil, want degraded structured history")
			}
			if resp.History.Continuity.Status != "degraded" {
				t.Fatalf("History continuity = %q, want degraded", resp.History.Continuity.Status)
			}
			if len(resp.History.Diagnostics) == 0 || resp.History.Diagnostics[0].Code != structuredTranscriptUnavailableCode {
				t.Fatalf("Diagnostics = %+v, want transcript_unavailable", resp.History.Diagnostics)
			}
			resume, ok := decodeStructuredResumeToken(resp.History.Cursor.ResumeToken)
			if !ok || !resume.IncludeThinking {
				t.Fatalf("fallback resume token = %+v, valid=%t; want include_thinking=true", resume, ok)
			}
			if len(structuredTranscriptMessages(resp)) != 1 {
				t.Fatalf("StructuredMessages len = %d, want 1: %+v", len(structuredTranscriptMessages(resp)), structuredTranscriptMessages(resp))
			}
			msg := structuredTranscriptMessages(resp)[0]
			if msg.Provider != provider {
				t.Fatalf("message provider = %q, want %q", msg.Provider, provider)
			}
			if msg.Role != "assistant" {
				t.Fatalf("message role = %q, want assistant for pane-output fallback", msg.Role)
			}
			if len(msg.Blocks) != 1 || msg.Blocks[0].Type != "text" || !strings.Contains(msg.Blocks[0].Text, provider+" pane output") {
				t.Fatalf("message blocks = %+v, want provider-neutral text fallback", msg.Blocks)
			}
		})
	}
}

func TestHandleSessionTranscriptStructuredSkipsCodexUnknownEvents(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: workDir, Provider: "codex", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredCodexFixture(t, searchBase, info.WorkDir, "2026-06-01T00-05-00", info.SessionKey, []string{
		`{"timestamp":"2026-06-01T00:05:01Z","type":"event_msg","payload":{"type":"shutdown_complete","data":"provider-native event"}}`,
		`{"timestamp":"2026-06-01T00:05:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"assistant survived unknown event"}]}}`,
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(structuredTranscriptMessages(resp)) != 1 {
		t.Fatalf("StructuredMessages = %+v, want only assistant message", structuredTranscriptMessages(resp))
	}
	got := structuredTranscriptMessages(resp)[0]
	if got.Role != "assistant" || len(got.Blocks) != 1 || got.Blocks[0].Text != "assistant survived unknown event" {
		t.Fatalf("structured messages = %+v, want assistant text only", structuredTranscriptMessages(resp))
	}
	assertNoStructuredWireLeak(t, body, "shutdown_complete", "provider-native event")
}

func TestHandleSessionTranscriptStructuredNormalizesCodexSystemErrors(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: workDir, Provider: "codex", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredCodexFixture(t, searchBase, info.WorkDir, "2026-06-01T00-06-00", info.SessionKey, []string{
		`{"timestamp":"2026-06-01T00:06:01Z","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}`,
		`{"timestamp":"2026-06-01T00:06:02Z","type":"event_msg","payload":{"type":"stream_error","message":"stream interrupted"}}`,
		`{"timestamp":"2026-06-01T00:06:03Z","type":"event_msg","payload":{"type":"turn_aborted","message":"turn was aborted"}}`,
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wants := []SessionStructuredSystemEvent{
		{Kind: "error", Category: "usage_limit", Code: "usage_limit_exceeded", Message: "You've hit your usage limit."},
		{Kind: "error", Category: "stream_error", Message: "stream interrupted"},
		{Kind: "turn_aborted", Category: "turn_aborted", Message: "turn was aborted"},
	}
	if len(structuredTranscriptMessages(resp)) != len(wants) {
		t.Fatalf("StructuredMessages = %+v, want %d system events", structuredTranscriptMessages(resp), len(wants))
	}
	for i, want := range wants {
		msg := structuredTranscriptMessages(resp)[i]
		if msg.Role != "system" {
			t.Fatalf("[%d] role = %q, want system; msg = %+v", i, msg.Role, msg)
		}
		if msg.SystemEvent == nil {
			t.Fatalf("[%d] system_event is nil; msg = %+v", i, msg)
		}
		if *msg.SystemEvent != want {
			t.Fatalf("[%d] system_event = %+v, want %+v", i, *msg.SystemEvent, want)
		}
		if len(msg.Blocks) != 1 || msg.Blocks[0].Type != "text" || msg.Blocks[0].Text != want.Message {
			t.Fatalf("[%d] blocks = %+v, want clean system message text %q", i, msg.Blocks, want.Message)
		}
	}
	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionTranscriptStructuredNormalizesGeminiSystemError(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "gemini", WorkDir: workDir, Provider: "gemini", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredGeminiErrorFixture(t, searchBase, info.WorkDir, info.SessionKey)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(structuredTranscriptMessages(resp)) != 1 {
		t.Fatalf("StructuredMessages = %+v, want one Gemini system event", structuredTranscriptMessages(resp))
	}
	msg := structuredTranscriptMessages(resp)[0]
	if msg.Role != "system" {
		t.Fatalf("role = %q, want system; msg = %+v", msg.Role, msg)
	}
	want := SessionStructuredSystemEvent{Kind: "error", Category: "provider_error", Message: "Gemini stream interrupted"}
	if msg.SystemEvent == nil || *msg.SystemEvent != want {
		t.Fatalf("system_event = %+v, want %+v", msg.SystemEvent, want)
	}
	if len(msg.Blocks) != 1 || msg.Blocks[0].Type != "text" || msg.Blocks[0].Text != want.Message {
		t.Fatalf("blocks = %+v, want clean Gemini error text %q", msg.Blocks, want.Message)
	}
	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionTranscriptStructuredNormalizesClaudeTaskOutput(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Resume: session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredClaudeTaskOutputFixture(t, searchBase, info.WorkDir, info.SessionKey)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	toolUse, toolResult := findStructuredToolPair(structuredTranscriptMessages(resp), "call-claude-task")
	if toolUse == nil || toolResult == nil {
		t.Fatalf("missing task tool pair in structured messages: %+v", structuredTranscriptMessages(resp))
	}
	if toolUse.Input == nil || toolUse.Input.Kind != "task" || toolUse.Input.TaskID != "task-123" {
		t.Fatalf("task input = %+v, want neutral task input with task-123", toolUse.Input)
	}
	if toolResult.Structured == nil {
		t.Fatal("task structured result is nil")
	}
	got := toolResult.Structured
	if got.Kind != "task" || got.TaskID != "task-123" || got.TaskType != "subagent" || got.TaskStatus != "completed" {
		t.Fatalf("task structured result = %+v, want task metadata", got)
	}
	if got.Description != "Run delegated check" || got.Output != "delegated check passed" {
		t.Fatalf("task result text = description %q output %q, want typed task output; result = %+v", got.Description, got.Output, got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("task exit_code = %v, want 0; result = %+v", got.ExitCode, got)
	}
	if got.TotalDurationMs != 1234 || got.TotalTokens != 321 || got.TotalToolUseCount != 4 {
		t.Fatalf("task aggregate metrics = duration %d tokens %d tools %d, want 1234/321/4; result = %+v", got.TotalDurationMs, got.TotalTokens, got.TotalToolUseCount, got)
	}

	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionTranscriptStructuredNormalizesClaudeBashOutput(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Resume: session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredClaudeBashOutputFixture(t, searchBase, info.WorkDir, info.SessionKey)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	toolUse, toolResult := findStructuredToolPair(structuredTranscriptMessages(resp), "call-claude-bash-output")
	if toolUse == nil || toolResult == nil {
		t.Fatalf("missing bash output tool pair in structured messages: %+v", structuredTranscriptMessages(resp))
	}
	if toolUse.Input == nil || toolUse.Input.Kind != "task" || toolUse.Input.TaskID != "shell-123" {
		t.Fatalf("bash output input = %+v, want neutral task input with shell-123", toolUse.Input)
	}
	if toolResult.Structured == nil {
		t.Fatal("bash output structured result is nil")
	}
	got := toolResult.Structured
	if got.Kind != "bash" || got.TaskID != "shell-123" || got.Command != "npm test" || got.TaskStatus != "completed" {
		t.Fatalf("bash output structured result = %+v, want bash shell metadata", got)
	}
	if got.Stdout != "ok\n" || got.Stderr != "warn\n" || got.StdoutLines != 1 || got.StderrLines != 1 || got.Timestamp != "2026-06-01T00:00:02Z" {
		t.Fatalf("bash output streams = %+v, want stdout/stderr line metadata and timestamp", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("bash output exit_code = %v, want 0; result = %+v", got.ExitCode, got)
	}

	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionTranscriptStructuredLinksClaudeWriteStdin(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Resume: session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredClaudeWriteStdinFixture(t, searchBase, info.WorkDir, info.SessionKey)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bashUse, bashResult := findStructuredToolPair(structuredTranscriptMessages(resp), "call-claude-bash")
	if bashUse == nil || bashResult == nil {
		t.Fatalf("missing bash tool pair in structured messages: %+v", structuredTranscriptMessages(resp))
	}
	if bashResult.Structured == nil || bashResult.Structured.Kind != "bash" || bashResult.Structured.TaskID != "42" || bashResult.Structured.Command != "claude --resume" {
		t.Fatalf("bash structured result = %+v, want command and neutral shell id", bashResult.Structured)
	}
	stdinUse, stdinResult := findStructuredToolPair(structuredTranscriptMessages(resp), "call-claude-stdin")
	if stdinUse == nil || stdinResult == nil {
		t.Fatalf("missing stdin tool pair in structured messages: %+v", structuredTranscriptMessages(resp))
	}
	if stdinUse.Input == nil || stdinUse.Input.Kind != "stdin" || stdinUse.Input.TaskID != "42" || stdinUse.Input.Text != "hello\n" {
		t.Fatalf("stdin input = %+v, want typed stdin task/text", stdinUse.Input)
	}
	if stdinUse.Input.LinkedCommand != "claude --resume" {
		t.Fatalf("stdin linked_command = %q, want claude --resume; input = %+v", stdinUse.Input.LinkedCommand, stdinUse.Input)
	}
	if stdinResult.Structured == nil || stdinResult.Structured.Kind != "stdin" || stdinResult.Structured.TaskID != "42" || stdinResult.Structured.Content != "sent" {
		t.Fatalf("stdin structured result = %+v, want typed stdin result", stdinResult.Structured)
	}

	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionStreamStructuredGracefullyDowngradesWithoutTranscript(t *testing.T) {
	isolateProviderDiscovery(t)
	for _, provider := range config.BuiltinProviderOrder() {
		t.Run(provider, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: provider, WorkDir: t.TempDir(), Provider: provider, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fs.sp.SetPeekOutput(info.SessionName, provider+" pane output")

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			rec := newSyncResponseRecorder()
			req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=structured", nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				h.ServeHTTP(rec, req)
				close(done)
			}()

			body := waitForRecorderSubstring(t, rec, `"format":"structured"`, 500*time.Millisecond)
			if !strings.Contains(body, `"format":"structured"`) {
				t.Fatalf("stream body missing structured fallback event: %s", body)
			}
			if !strings.Contains(body, structuredTranscriptUnavailableCode) {
				t.Fatalf("stream body missing degraded diagnostic: %s", body)
			}
			if !strings.Contains(body, provider+" pane output") {
				t.Fatalf("stream body missing text fallback: %s", body)
			}
			if !strings.Contains(body, `"role":"assistant"`) {
				t.Fatalf("stream body fallback role is not assistant: %s", body)
			}
			cancel()
			<-done
		})
	}
}

func TestHandleSessionStreamStructuredUsesRelocatedSessionStoreForPaneFallback(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	relocated := beads.NewMemStore()
	fs.sessionsBeadStore = relocated
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(relocated, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Relocated",
		Command:  "cursor",
		WorkDir:  t.TempDir(),
		Provider: "cursor",
		Resume:   session.ProviderResume{},
		Hints:    runtime.Config{},
		ExtraMeta: map[string]string{
			"session_origin": "manual",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs.sp.SetPeekOutput(info.SessionName, "relocated pane output")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/stream?format=structured", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, "relocated pane output", 10*time.Second)
	if !strings.Contains(body, structuredTranscriptUnavailableCode) {
		t.Fatalf("stream body missing degraded diagnostic: %s", body)
	}
	cancel()
	<-done
}

func TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect(t *testing.T) {
	for _, surface := range []string{"city-huma", "legacy"} {
		t.Run(surface, func(t *testing.T) {
			isolateProviderDiscovery(t)
			fs := newSessionFakeState(t)
			searchBase := t.TempDir()
			srv := New(fs)
			structuredPeekPoll := make(chan time.Time)
			srv.structuredPeekPoll = structuredPeekPoll
			humaHandler := newTestCityHandlerWith(t, fs, srv)
			srv.sessionLogSearchPaths = []string{searchBase}

			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			workDir := t.TempDir()
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
				Template: "myrig/worker",
				Title:    "Promote",
				Command:  "claude",
				WorkDir:  workDir,
				Provider: "claude",
				Hints:    runtime.Config{},
				ExtraMeta: map[string]string{
					"session_origin": "manual",
				},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fs.sp.SetPeekOutput(info.SessionName, "pane fallback before history")

			handler := humaHandler
			path := cityURL(fs, "/session/") + info.ID + "/stream?format=structured"
			if surface == "legacy" {
				handler = srv.legacySessionHandler()
				path = "/v0/session/" + info.ID + "/stream?format=structured"
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			rec := newSyncResponseRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(rec, req)
				close(done)
			}()

			fallbackBody := waitForRecorderSubstring(t, rec, "pane fallback before history", 10*time.Second)
			if !strings.Contains(fallbackBody, structuredTranscriptUnavailableCode) {
				t.Fatalf("fallback body missing degraded diagnostic: %s", fallbackBody)
			}

			writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
				`{"uuid":"m1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":"authoritative history"},"timestamp":"2025-01-01T00:00:00Z"}`,
			)
			select {
			case structuredPeekPoll <- time.Now():
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("structured peek stream did not consume the injected poll tick")
			}

			body := waitForRecorderSubstring(t, rec, `"reset_reason":"stream_changed"`, 10*time.Second)
			cancel()
			<-done

			var promoted *SessionStreamStructuredMessageEvent
			for _, frame := range parseSSETestFrames(body) {
				if frame.Event != "structured" {
					continue
				}
				var update SessionStreamStructuredMessageEvent
				if err := json.Unmarshal([]byte(frame.Data), &update); err != nil {
					t.Fatalf("decode structured frame: %v; data=%s", err, frame.Data)
				}
				if update.Operation == sessionStructuredOperationReset {
					promoted = &update
				}
			}
			if promoted == nil || promoted.ResetReason != sessionStructuredResetStreamChanged {
				t.Fatalf("structured frames did not promote with reset/stream_changed: %s", body)
			}
			if got := structuredMessageIDs(promoted.StructuredMessages); !equalStrings(got, []string{"m1"}) {
				t.Fatalf("promoted message IDs = %v, want [m1]", got)
			}
			if len(promoted.StructuredMessages[0].Blocks) != 1 || promoted.StructuredMessages[0].Blocks[0].Text != "authoritative history" {
				t.Fatalf("promoted message blocks = %+v, want authoritative history", promoted.StructuredMessages[0].Blocks)
			}
		})
	}
}

func TestHandleSessionStreamStructuredClosedWithoutHistoryMatchesTranscriptSnapshot(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Closed",
		Command:  "cursor",
		WorkDir:  t.TempDir(),
		Provider: "cursor",
		Resume:   session.ProviderResume{},
		Hints:    runtime.Config{},
		ExtraMeta: map[string]string{
			"session_origin": "manual",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restRec := httptest.NewRecorder()
	restReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/transcript?format=structured", nil)
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want %d; body: %s", restRec.Code, http.StatusOK, restRec.Body.String())
	}
	var rest sessionTranscriptGetResponse
	if err := json.NewDecoder(restRec.Body).Decode(&rest); err != nil {
		t.Fatalf("decode REST snapshot: %v", err)
	}
	if rest.Operation != sessionStructuredOperationSnapshot {
		t.Fatalf("REST operation = %q, want %q", rest.Operation, sessionStructuredOperationSnapshot)
	}
	if rest.History == nil || rest.History.Cursor.ResumeToken == "" {
		t.Fatalf("REST history cursor = %+v, want resume token", rest.History)
	}

	streamRec := httptest.NewRecorder()
	streamReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/stream?format=structured", nil)
	h.ServeHTTP(streamRec, streamReq)
	if streamRec.Code != http.StatusOK {
		t.Fatalf("SSE status = %d, want %d; body: %s", streamRec.Code, http.StatusOK, streamRec.Body.String())
	}
	frame := firstSSETestFrame(t, streamRec.Body.String(), "structured")
	var streamed SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(frame.Data), &streamed); err != nil {
		t.Fatalf("decode SSE snapshot: %v; data=%s", err, frame.Data)
	}

	if !reflect.DeepEqual(streamed.History, rest.History) {
		t.Fatalf("SSE history = %+v, want REST history %+v", streamed.History, rest.History)
	}
	if !reflect.DeepEqual(streamed.StructuredMessages, structuredTranscriptMessages(rest)) {
		t.Fatalf("SSE messages = %+v, want REST messages %+v", streamed.StructuredMessages, structuredTranscriptMessages(rest))
	}
	if streamed.History == nil || streamed.History.Continuity.Status != "degraded" {
		t.Fatalf("SSE history = %+v, want degraded structured fallback", streamed.History)
	}
}

func TestHandleSessionStreamStructuredAfterCursorSuppressesRESTSnapshotReplay(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Resume",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"m1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	restRec := httptest.NewRecorder()
	restReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/transcript?format=structured", nil)
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want %d; body: %s", restRec.Code, http.StatusOK, restRec.Body.String())
	}
	var snapshot sessionTranscriptGetResponse
	if err := json.NewDecoder(restRec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode REST snapshot: %v", err)
	}
	if snapshot.History == nil || snapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("REST history cursor = %+v, want resume token", snapshot.History)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	path := cityURL(fs, "/session/") + info.ID + "/stream?format=structured&after_cursor=" + url.QueryEscape(snapshot.History.Cursor.ResumeToken)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, "event: activity", 10*time.Second)
	cancel()
	<-done
	if strings.Contains(body, "event: structured") {
		t.Fatalf("stream replayed the exact REST snapshot: %s", body)
	}
}

func TestHandleSessionStreamStructuredResumesFromPaginatedRESTSnapshot(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Paginated resume",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"m1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":"one"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"m2","parentUuid":"m1","type":"assistant","message":{"role":"assistant","content":"two"},"timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"m3","parentUuid":"m2","type":"assistant","message":{"role":"assistant","content":"three"},"timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"m4","parentUuid":"m3","type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":"four"},"timestamp":"2025-01-01T00:00:03Z"}`,
	)

	restRec := httptest.NewRecorder()
	restReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&after=m2", nil)
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want %d; body: %s", restRec.Code, http.StatusOK, restRec.Body.String())
	}
	var snapshot sessionTranscriptGetResponse
	if err := json.NewDecoder(restRec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode REST snapshot: %v", err)
	}
	if got := structuredMessageIDs(structuredTranscriptMessages(snapshot)); !equalStrings(got, []string{"m3", "m4"}) {
		t.Fatalf("paginated REST message IDs = %v, want [m3 m4]", got)
	}
	if snapshot.History == nil || snapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("REST history cursor = %+v, want resume token", snapshot.History)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*testutil.GoroutineRaceTimeout)
	defer cancel()
	rec := newSyncResponseRecorder()
	path := cityURL(fs, "/session/") + info.ID + "/stream?format=structured&after_cursor=" + url.QueryEscape(snapshot.History.Cursor.ResumeToken)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	initialBody := waitForRecorderSubstring(t, rec, "event: activity", testutil.GoroutineRaceTimeout)
	if strings.Contains(initialBody, "event: structured") {
		cancel()
		<-done
		t.Fatalf("stream reset or replayed the paginated REST snapshot: %s", initialBody)
	}

	logPath := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir), info.SessionKey+".jsonl")
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("open transcript for append: %v", err)
	}
	_, writeErr := fmt.Fprintln(file, `{"uuid":"m5","parentUuid":"m4","type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":"five"},"timestamp":"2025-01-01T00:00:04Z"}`)
	closeErr := file.Close()
	if writeErr != nil {
		cancel()
		<-done
		t.Fatalf("append transcript: %v", writeErr)
	}
	if closeErr != nil {
		cancel()
		<-done
		t.Fatalf("close transcript: %v", closeErr)
	}

	body := waitForRecorderSubstring(t, rec, "event: structured", testutil.GoroutineRaceTimeout)
	cancel()
	<-done
	frame := firstSSETestFrame(t, body, "structured")
	var update SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(frame.Data), &update); err != nil {
		t.Fatalf("decode structured upsert: %v; data=%s", err, frame.Data)
	}
	if update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationUpsert)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m4", "m5"}) {
		t.Fatalf("upsert IDs = %v, want inclusive tail [m4 m5]", got)
	}
}

func TestHandleSessionStreamStructuredResumesFromEmptyPaginatedRESTSnapshot(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Empty paginated resume",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"m1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":"one"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"m2","parentUuid":"m1","type":"assistant","message":{"role":"assistant","content":"two"},"timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"m3","parentUuid":"m2","type":"assistant","message":{"role":"assistant","content":"three"},"timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"m4","parentUuid":"m3","type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":"four"},"timestamp":"2025-01-01T00:00:03Z"}`,
	)

	restRec := httptest.NewRecorder()
	restReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&after=m4", nil)
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want %d; body: %s", restRec.Code, http.StatusOK, restRec.Body.String())
	}
	var snapshot sessionTranscriptGetResponse
	if err := json.NewDecoder(restRec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode REST snapshot: %v", err)
	}
	if got := structuredMessageIDs(structuredTranscriptMessages(snapshot)); len(got) != 0 {
		t.Fatalf("empty paginated REST message IDs = %v, want none", got)
	}
	if snapshot.History == nil || snapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("REST history cursor = %+v, want resume token", snapshot.History)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*testutil.GoroutineRaceTimeout)
	defer cancel()
	rec := newSyncResponseRecorder()
	path := cityURL(fs, "/session/") + info.ID + "/stream?format=structured&after_cursor=" + url.QueryEscape(snapshot.History.Cursor.ResumeToken)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	initialBody := waitForRecorderSubstring(t, rec, "event: structured", testutil.GoroutineRaceTimeout)
	initialFrame := firstSSETestFrame(t, initialBody, "structured")
	var initialUpdate SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(initialFrame.Data), &initialUpdate); err != nil {
		cancel()
		<-done
		t.Fatalf("decode initial structured upsert: %v; data=%s", err, initialFrame.Data)
	}
	if initialUpdate.Operation != sessionStructuredOperationUpsert {
		cancel()
		<-done
		t.Fatalf("initial operation = %q, want %q", initialUpdate.Operation, sessionStructuredOperationUpsert)
	}
	if got := structuredMessageIDs(initialUpdate.StructuredMessages); !equalStrings(got, []string{"m4"}) {
		cancel()
		<-done
		t.Fatalf("initial upsert IDs = %v, want bounded anchor [m4]", got)
	}

	logPath := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir), info.SessionKey+".jsonl")
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("open transcript for append: %v", err)
	}
	_, writeErr := fmt.Fprintln(file, `{"uuid":"m5","parentUuid":"m4","type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":"five"},"timestamp":"2025-01-01T00:00:04Z"}`)
	closeErr := file.Close()
	if writeErr != nil {
		cancel()
		<-done
		t.Fatalf("append transcript: %v", writeErr)
	}
	if closeErr != nil {
		cancel()
		<-done
		t.Fatalf("close transcript: %v", closeErr)
	}

	body := waitForRecorderSubstring(t, rec, `"id":"m5"`, testutil.GoroutineRaceTimeout)
	cancel()
	<-done
	var frame sseTestFrame
	for _, candidate := range parseSSETestFrames(body) {
		if candidate.Event == "structured" {
			frame = candidate
		}
	}
	if frame.Event == "" {
		t.Fatalf("appended structured event not found in body: %s", body)
	}
	var update SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(frame.Data), &update); err != nil {
		t.Fatalf("decode structured upsert: %v; data=%s", err, frame.Data)
	}
	if update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationUpsert)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m4", "m5"}) {
		t.Fatalf("upsert IDs = %v, want inclusive tail [m4 m5]", got)
	}
}

func TestHandleSessionStreamStructuredInvalidCursorEmitsResetSnapshot(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Reset",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"m1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/stream?format=structured&after_cursor=not-a-token", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	frame := firstSSETestFrame(t, rec.Body.String(), "structured")
	var update SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(frame.Data), &update); err != nil {
		t.Fatalf("decode structured reset: %v; data=%s", err, frame.Data)
	}
	if update.Operation != sessionStructuredOperationReset || update.ResetReason != sessionStructuredResetResumeInvalid {
		t.Fatalf("reset operation = %q reason = %q, want reset/%s", update.Operation, update.ResetReason, sessionStructuredResetResumeInvalid)
	}
	if len(update.StructuredMessages) != 1 || update.StructuredMessages[0].ID != "m1" {
		t.Fatalf("reset messages = %+v, want full m1 snapshot", update.StructuredMessages)
	}
	if frame.ID == "" || update.History == nil || frame.ID != update.History.Cursor.ResumeToken {
		t.Fatalf("SSE id = %q history = %+v, want matching resume token", frame.ID, update.History)
	}
}

func TestHandleSessionStreamStructuredResumeEmitsInclusiveTailUpsert(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Append",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"m1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	restRec := httptest.NewRecorder()
	restReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/transcript?format=structured", nil)
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want %d; body: %s", restRec.Code, http.StatusOK, restRec.Body.String())
	}
	var snapshot sessionTranscriptGetResponse
	if err := json.NewDecoder(restRec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode REST snapshot: %v", err)
	}
	if snapshot.History == nil || snapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("REST history cursor = %+v, want resume token", snapshot.History)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	path := cityURL(fs, "/session/") + info.ID + "/stream?format=structured&after_cursor=" + url.QueryEscape(snapshot.History.Cursor.ResumeToken)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	initialBody := waitForRecorderSubstring(t, rec, "event: activity", 10*time.Second)
	if !strings.Contains(initialBody, "event: activity") {
		t.Fatalf("stream readiness activity did not arrive: %s", initialBody)
	}

	logPath := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir), info.SessionKey+".jsonl")
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	_, writeErr := fmt.Fprintln(file, `{"uuid":"m2","parentUuid":"m1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:01Z"}`)
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatalf("append transcript: %v", writeErr)
	}
	if closeErr != nil {
		t.Fatalf("close transcript: %v", closeErr)
	}

	body := waitForRecorderSubstring(t, rec, "event: structured", 10*time.Second)
	cancel()
	<-done
	frame := firstSSETestFrame(t, body, "structured")
	var update SessionStreamStructuredMessageEvent
	if err := json.Unmarshal([]byte(frame.Data), &update); err != nil {
		t.Fatalf("decode structured upsert: %v; data=%s", err, frame.Data)
	}
	if update.Operation != sessionStructuredOperationUpsert {
		t.Fatalf("operation = %q, want %q", update.Operation, sessionStructuredOperationUpsert)
	}
	if got := structuredMessageIDs(update.StructuredMessages); !equalStrings(got, []string{"m1", "m2"}) {
		t.Fatalf("upsert IDs = %v, want inclusive tail [m1 m2]", got)
	}
}

func TestLegacySessionTranscriptStructuredGracefullyDowngrades(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "cursor", WorkDir: t.TempDir(), Provider: "cursor", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs.sp.SetPeekOutput(info.SessionName, "cursor pane output")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?format=structured&tail=0&include_thinking=true", nil)
	srv.legacySessionHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptGetResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "structured" {
		t.Fatalf("Format = %q, want structured; body: %s", resp.Format, w.Body.String())
	}
	if resp.History == nil || resp.History.Continuity.Status != "degraded" {
		t.Fatalf("History = %+v, want degraded structured fallback", resp.History)
	}
	resume, ok := decodeStructuredResumeToken(resp.History.Cursor.ResumeToken)
	if !ok || !resume.IncludeThinking {
		t.Fatalf("legacy fallback resume token = %+v, valid=%t; want include_thinking=true", resume, ok)
	}
	if len(structuredTranscriptMessages(resp)) != 1 || structuredTranscriptMessages(resp)[0].Role != "assistant" || !strings.Contains(structuredTranscriptMessages(resp)[0].Blocks[0].Text, "cursor pane output") {
		t.Fatalf("StructuredMessages = %+v, want cursor pane output text fallback", structuredTranscriptMessages(resp))
	}
}

func TestLegacySessionStreamStructuredGracefullyDowngrades(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "cursor", WorkDir: t.TempDir(), Provider: "cursor", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs.sp.SetPeekOutput(info.SessionName, "cursor pane output")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=structured", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		srv.legacySessionHandler().ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, `"format":"structured"`, 500*time.Millisecond)
	if !strings.Contains(body, "event: structured") {
		t.Fatalf("stream body missing structured event name: %s", body)
	}
	if !strings.Contains(body, structuredTranscriptUnavailableCode) {
		t.Fatalf("stream body missing degraded diagnostic: %s", body)
	}
	if !strings.Contains(body, "cursor pane output") {
		t.Fatalf("stream body missing text fallback: %s", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("stream body fallback role is not assistant: %s", body)
	}
	cancel()
	<-done
}

func findStructuredToolPair(messages []SessionStructuredMessage, toolCallID string) (*SessionStructuredBlock, *SessionStructuredBlock) {
	var toolUse *SessionStructuredBlock
	var toolResult *SessionStructuredBlock
	for i := range messages {
		for j := range messages[i].Blocks {
			block := &messages[i].Blocks[j]
			switch block.Type {
			case "tool_use":
				if block.ID == toolCallID || block.ToolCallID == toolCallID {
					toolUse = block
				}
			case "tool_result":
				if block.ToolCallID == toolCallID {
					toolResult = block
				}
			}
		}
	}
	return toolUse, toolResult
}

func assertStructuredInput(t *testing.T, input *SessionStructuredToolInput, kind, filePath, url, prompt, question string, options []string, command, query, pattern, text, plan string, stepCount int, todoCount int) {
	t.Helper()
	if input.Kind != kind {
		t.Fatalf("input kind = %q, want %q; input = %+v", input.Kind, kind, input)
	}
	if filePath != "" && input.FilePath != filePath {
		t.Fatalf("input file_path = %q, want %q; input = %+v", input.FilePath, filePath, input)
	}
	if url != "" && input.URL != url {
		t.Fatalf("input url = %q, want %q; input = %+v", input.URL, url, input)
	}
	if prompt != "" && input.Prompt != prompt {
		t.Fatalf("input prompt = %q, want %q; input = %+v", input.Prompt, prompt, input)
	}
	if question != "" && input.Question != question {
		t.Fatalf("input question = %q, want %q; input = %+v", input.Question, question, input)
	}
	for _, want := range options {
		if !stringSliceContains(input.Options, want) {
			t.Fatalf("input options = %#v, missing %q; input = %+v", input.Options, want, input)
		}
	}
	if command != "" && input.Command != command {
		t.Fatalf("input command = %q, want %q; input = %+v", input.Command, command, input)
	}
	if query != "" && input.Query != query {
		t.Fatalf("input query = %q, want %q; input = %+v", input.Query, query, input)
	}
	if pattern != "" && input.Pattern != pattern {
		t.Fatalf("input pattern = %q, want %q; input = %+v", input.Pattern, pattern, input)
	}
	if text != "" && input.Text != text {
		t.Fatalf("input text = %q, want %q; input = %+v", input.Text, text, input)
	}
	if plan != "" && input.Plan != plan {
		t.Fatalf("input plan = %q, want %q; input = %+v", input.Plan, plan, input)
	}
	if stepCount != 0 && len(input.Steps) != stepCount {
		t.Fatalf("input steps = %#v, want %d steps; input = %+v", input.Steps, stepCount, input)
	}
	if todoCount != 0 && len(input.Todos) != todoCount {
		t.Fatalf("input todos = %#v, want %d todo items; input = %+v", input.Todos, todoCount, input)
	}
}

func assertStructuredInputArguments(t *testing.T, args []SessionStructuredArgument, wants map[string]string) {
	t.Helper()
	for name, wantSubstring := range wants {
		found := false
		for _, arg := range args {
			if arg.Name == name && strings.Contains(arg.Value, wantSubstring) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("input arguments = %+v, missing %s containing %q", args, name, wantSubstring)
		}
	}
}

func assertStructuredResult(t *testing.T, result *SessionStructuredToolResult, kind, filePath, content, stdout string, exitCode *int, filenames []string, resultItemURLs []string, mode, query string, numResults int, url string, statusCode int, statusText string, bytesValue int, durationMs int, appliedLimit int, truncated bool, question string, questionCount int, answer string, answerCount int, plan string, stepCount int, oldTodoCount int, newTodoCount int, patchSubstrings []string, oldString string, newString string, originalFile string, replaceAll *bool, userModified *bool, absentSubstrings []string) {
	t.Helper()
	if result == nil {
		t.Fatal("structured result is nil")
	}
	if result.Kind != kind {
		t.Fatalf("result kind = %q, want %q; result = %+v", result.Kind, kind, result)
	}
	if filePath != "" && result.FilePath != filePath {
		t.Fatalf("result file_path = %q, want %q; result = %+v", result.FilePath, filePath, result)
	}
	if content != "" && !strings.Contains(result.Content, content) {
		t.Fatalf("result content = %q, want substring %q; result = %+v", result.Content, content, result)
	}
	for _, absent := range absentSubstrings {
		if strings.Contains(result.Content, absent) || strings.Contains(result.Stdout, absent) || strings.Contains(result.Text, absent) {
			t.Fatalf("result contains unwanted substring %q; result = %+v", absent, result)
		}
	}
	if stdout != "" && result.Stdout != stdout {
		t.Fatalf("result stdout = %q, want %q; result = %+v", result.Stdout, stdout, result)
	}
	if exitCode != nil {
		if result.ExitCode == nil || *result.ExitCode != *exitCode {
			t.Fatalf("result exit_code = %v, want %d; result = %+v", result.ExitCode, *exitCode, result)
		}
	}
	if mode != "" && result.Mode != mode {
		t.Fatalf("result mode = %q, want %q; result = %+v", result.Mode, mode, result)
	}
	if query != "" && result.Query != query {
		t.Fatalf("result query = %q, want %q; result = %+v", result.Query, query, result)
	}
	if numResults != 0 && result.NumResults != numResults {
		t.Fatalf("result num_results = %d, want %d; result = %+v", result.NumResults, numResults, result)
	}
	if url != "" && result.URL != url {
		t.Fatalf("result url = %q, want %q; result = %+v", result.URL, url, result)
	}
	if statusCode != 0 && result.StatusCode != statusCode {
		t.Fatalf("result status_code = %d, want %d; result = %+v", result.StatusCode, statusCode, result)
	}
	if statusText != "" && result.StatusText != statusText {
		t.Fatalf("result status_text = %q, want %q; result = %+v", result.StatusText, statusText, result)
	}
	if bytesValue != 0 && result.Bytes != bytesValue {
		t.Fatalf("result bytes = %d, want %d; result = %+v", result.Bytes, bytesValue, result)
	}
	if durationMs != 0 && result.DurationMs != durationMs {
		t.Fatalf("result duration_ms = %d, want %d; result = %+v", result.DurationMs, durationMs, result)
	}
	if appliedLimit != 0 && result.AppliedLimit != appliedLimit {
		t.Fatalf("result applied_limit = %d, want %d; result = %+v", result.AppliedLimit, appliedLimit, result)
	}
	if truncated && !result.Truncated {
		t.Fatalf("result truncated = false, want true; result = %+v", result)
	}
	if question != "" && result.Question != question {
		t.Fatalf("result question = %q, want %q; result = %+v", result.Question, question, result)
	}
	if questionCount != 0 {
		if len(result.Questions) != questionCount {
			t.Fatalf("result questions = %#v, want %d questions; result = %+v", result.Questions, questionCount, result)
		}
		if result.Questions[0].Question == "" || result.Questions[0].Header == "" || !result.Questions[0].MultiSelect || len(result.Questions[0].Options) == 0 || result.Questions[0].Options[0].Description == "" {
			t.Fatalf("result questions = %#v, want question text, header, multi-select, and option descriptions", result.Questions)
		}
	}
	if answer != "" && result.Answer != answer {
		t.Fatalf("result answer = %q, want %q; result = %+v", result.Answer, answer, result)
	}
	if answerCount != 0 && len(result.Answers) != answerCount {
		t.Fatalf("result answers = %#v, want %d answers; result = %+v", result.Answers, answerCount, result)
	}
	if plan != "" && result.Plan != plan {
		t.Fatalf("result plan = %q, want %q; result = %+v", result.Plan, plan, result)
	}
	if stepCount != 0 && len(result.Steps) != stepCount {
		t.Fatalf("result steps = %#v, want %d steps; result = %+v", result.Steps, stepCount, result)
	}
	if len(filenames) > 0 && result.NumFiles != len(filenames) {
		t.Fatalf("result num_files = %d, want %d; result = %+v", result.NumFiles, len(filenames), result)
	}
	if oldTodoCount != 0 && len(result.OldTodos) != oldTodoCount {
		t.Fatalf("result old_todos = %#v, want %d items; result = %+v", result.OldTodos, oldTodoCount, result)
	}
	if newTodoCount != 0 && len(result.NewTodos) != newTodoCount {
		t.Fatalf("result new_todos = %#v, want %d items; result = %+v", result.NewTodos, newTodoCount, result)
	}
	for _, want := range filenames {
		if !stringSliceContains(result.Filenames, want) {
			t.Fatalf("result filenames = %#v, missing %q; result = %+v", result.Filenames, want, result)
		}
	}
	for _, want := range resultItemURLs {
		if !structuredResultItemsContainURL(result.ResultItems, want) {
			t.Fatalf("result_items = %#v, missing URL %q; result = %+v", result.ResultItems, want, result)
		}
	}
	for _, want := range patchSubstrings {
		if !strings.Contains(result.Patch, want) {
			t.Fatalf("result patch = %q, missing %q; result = %+v", result.Patch, want, result)
		}
	}
	isPatchResultKind := kind == "edit" || kind == "write"
	if isPatchResultKind && len(patchSubstrings) > 0 && len(result.PatchHunks) == 0 {
		t.Fatalf("%s result has patch %q but no typed patch_hunks; result = %+v", kind, result.Patch, result)
	}
	if isPatchResultKind && len(patchSubstrings) == 0 && result.Patch != "" {
		t.Fatalf("%s result unexpectedly has generated patch %q; result = %+v", kind, result.Patch, result)
	}
	if oldString != "" && result.OldString != oldString {
		t.Fatalf("result old_string = %q, want %q; result = %+v", result.OldString, oldString, result)
	}
	if newString != "" && result.NewString != newString {
		t.Fatalf("result new_string = %q, want %q; result = %+v", result.NewString, newString, result)
	}
	if originalFile != "" && result.OriginalFile != originalFile {
		t.Fatalf("result original_file = %q, want %q; result = %+v", result.OriginalFile, originalFile, result)
	}
	if replaceAll != nil {
		if result.ReplaceAll == nil || *result.ReplaceAll != *replaceAll {
			t.Fatalf("result replace_all = %v, want %v; result = %+v", result.ReplaceAll, *replaceAll, result)
		}
	}
	if userModified != nil {
		if result.UserModified == nil || *result.UserModified != *userModified {
			t.Fatalf("result user_modified = %v, want %v; result = %+v", result.UserModified, *userModified, result)
		}
	}
	if !isPatchResultKind && result.Patch != "" {
		t.Fatalf("non-edit result unexpectedly has patch %q; result = %+v", result.Patch, result)
	}
}

func writeStructuredClaudeReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-read","name":"Read","input":{"file_path":"README.md"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-2","parentUuid":"claude-1","type":"tool_result","toolUseID":"call-claude-read","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-read","content":"read complete"}]},"toolUseResult":{"type":"text","file":{"filePath":"README.md","content":"Gas City README\n","numLines":1,"startLine":1,"totalLines":1,"language":"markdown"}},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeEditFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-edit-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-edit","name":"Edit","input":{"file_path":"README.md","old_string":"old line","new_string":"new line"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-edit-2","parentUuid":"claude-edit-1","type":"tool_result","toolUseID":"call-claude-edit","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-edit","content":"The file README.md has been updated successfully."}]},"toolUseResult":{"filePath":"README.md","oldString":"old line","newString":"new line","originalFile":"export const message = \"old line\";\n","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-export const message = \"old line\";","+export const message = \"new line\";"]}],"userModified":false,"replaceAll":false},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeBashToolUseResultFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-bash-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-bash","name":"Bash","input":{"command":"npm test"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-bash-2","parentUuid":"claude-bash-1","type":"tool_result","toolUseID":"call-claude-bash","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-bash","content":"command completed"}]},"toolUseResult":{"stdout":"tests passed\n","stderr":"","exitCode":0},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeGlobFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-glob-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-glob","name":"Glob","input":{"pattern":"**/*.go","path":"internal"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-glob-2","parentUuid":"claude-glob-1","type":"tool_result","toolUseID":"call-claude-glob","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-glob","content":"found files"}]},"toolUseResult":{"filenames":["internal/api/session_structured_types.go","internal/worker/structured_tool.go"],"durationMs":27,"numFiles":2,"truncated":true},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeGrepFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-grep-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-grep","name":"Grep","input":{"pattern":"needle","path":"README.md"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-grep-2","parentUuid":"claude-grep-1","type":"tool_result","toolUseID":"call-claude-grep","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-grep","content":"grep complete"}]},"toolUseResult":{"mode":"content","filenames":["README.md"],"content":"README.md:1:needle\n","numLines":1,"appliedLimit":100},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeWebSearchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-search-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-search","name":"WebSearch","input":{"query":"structured stream format"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-search-2","parentUuid":"claude-search-1","type":"tool_result","toolUseID":"call-claude-search","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-search","content":"search complete"}]},"toolUseResult":{"query":"structured stream format","durationSeconds":1.25,"results":[{"tool_use_id":"native-call","content":[{"title":"Structured Stream Format","url":"https://example.com/structured","snippet":"Provider-neutral typed data."}]}]},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeWebFetchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-fetch-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-fetch","name":"WebFetch","input":{"url":"https://example.com/spec","prompt":"Extract the structured contract"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-fetch-2","parentUuid":"claude-fetch-1","type":"tool_result","toolUseID":"call-claude-fetch","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-fetch","content":"fetched"}]},"toolUseResult":{"url":"https://example.com/spec","code":200,"codeText":"OK","bytes":4096,"durationMs":83,"result":"Fetched structured spec content.\nSecond line."},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeTodoWriteFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-todo-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-todo","name":"TodoWrite","input":{"todos":[{"content":"Review raw provider data","status":"in_progress","activeForm":"Reviewing raw provider data","priority":"high","id":"todo-1"}]}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-todo-2","parentUuid":"claude-todo-1","type":"tool_result","toolUseID":"call-claude-todo","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-todo","content":"todos updated"}]},"toolUseResult":{"oldTodos":[{"content":"Review raw provider data","status":"in_progress","activeForm":"Reviewing raw provider data"}],"newTodos":[{"content":"Review raw provider data","status":"completed","activeForm":"Reviewing raw provider data"},{"content":"Normalize typed todos","status":"pending","activeForm":"Normalizing typed todos"}]},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeExitPlanFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-plan-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-plan","name":"ExitPlanMode","input":{"plan":"Inspect MC and expose typed plan data."}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-plan-2","parentUuid":"claude-plan-1","type":"tool_result","toolUseID":"call-claude-plan","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-plan","content":"plan captured"}]},"toolUseResult":{"plan":"Inspect MC and expose typed plan data."},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeAskQuestionFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-question-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-question","name":"AskUserQuestion","input":{"question":"Proceed with typed question DTOs?","options":["Yes","No"]}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-question-2","parentUuid":"claude-question-1","type":"tool_result","toolUseID":"call-claude-question","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-question","content":"question answered"}]},"toolUseResult":{"questions":[{"question":"Select rollout scope","header":"Scope","options":[{"label":"All providers","description":"Validate first-class and graceful providers"},{"label":"Claude only","description":"Narrow smoke test"}],"multiSelect":true}],"answer":"All providers","answers":{"Select rollout scope":"All providers"}},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeTaskOutputFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-task-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-task","name":"TaskOutput","input":{"task_id":"task-123","block":true}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-task-2","parentUuid":"claude-task-1","type":"tool_result","toolUseID":"call-claude-task","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-task","content":"task completed"}]},"toolUseResult":{"taskId":"task-123","taskType":"subagent","status":"completed","description":"Run delegated check","output":"delegated check passed","exitCode":0,"totalDurationMs":1234,"totalTokens":321,"totalToolUseCount":4},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredClaudeBashOutputFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-bash-output-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-bash-output","name":"BashOutput","input":{"shellId":"shell-123","block":true}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-bash-output-2","parentUuid":"claude-bash-output-1","type":"tool_result","toolUseID":"call-claude-bash-output","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-bash-output","content":"bash output complete"}]},"toolUseResult":{"shellId":"shell-123","command":"npm test","status":"completed","exitCode":0,"stdout":"ok\n","stderr":"warn\n","stdoutLines":1,"stderrLines":1,"timestamp":"2026-06-01T00:00:02Z"},"timestamp":"2026-06-01T00:00:02Z"}`,
	)
}

func writeStructuredClaudeWriteStdinFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-stdin-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-bash","name":"Bash","input":{"command":"claude --resume"}},{"type":"tool_use","id":"call-claude-stdin","name":"write_stdin","input":{"sessionId":42,"content":"hello\n"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-stdin-2","parentUuid":"claude-stdin-1","type":"tool_result","toolUseID":"call-claude-bash","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-bash","content":"Process running with session ID: 42"}]},"timestamp":"2026-06-01T00:00:01Z"}`,
		`{"uuid":"claude-stdin-3","parentUuid":"claude-stdin-2","type":"tool_result","toolUseID":"call-claude-stdin","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-stdin","content":"sent"}]},"timestamp":"2026-06-01T00:00:02Z"}`,
	)
}

func writeStructuredClaudeKillShellFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-kill-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-kill","name":"KillShell","input":{"shell_id":"shell-123"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-kill-2","parentUuid":"claude-kill-1","type":"tool_result","toolUseID":"call-claude-kill","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-kill","content":"kill complete"}]},"toolUseResult":{"shell_id":"shell-123","message":"Shell shell-123 killed"},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredCodexPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	payload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-06-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2026-06-01T00:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-codex-patch","name":"apply_patch","input":"*** Begin Patch\n*** Update File: city.toml\n@@\n+[workspace]\n*** End Patch\n"}}`,
		`{"timestamp":"2026-06-01T00:00:02Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call-codex-patch","stdout":"Success. Updated the following files:\nM city.toml\n","stderr":"","success":true,"changes":{"city.toml":{"type":"update","unified_diff":"@@\n+[workspace]\n","move_path":null}},"status":"completed"}}`,
		`{"timestamp":"2026-06-01T00:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-codex-patch","output":"{\"output\":\"Success. Updated the following files:\\nM city.toml\\n\"}"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, structuredCodexFixtureFilename("2026-06-01T00-00-00", sessionKey)), []byte(payload), 0o644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
}

func writeStructuredCodexShellReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeStructuredCodexFixture(t, root, workDir, "2026-06-01T00-01-00", sessionKey, []string{
		`{"timestamp":"2026-06-01T00:01:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-codex-read","name":"exec_command","arguments":"{\"cmd\":\"sed -n '12,14p' src/app.ts\"}"}}`,
		`{"timestamp":"2026-06-01T00:01:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-codex-read","output":"Command: sed -n '12,14p' src/app.ts\nOutput:\nline 12\nline 13\nline 14\n"}}`,
	})
}

func writeStructuredCodexWrappedShellReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeStructuredCodexFixture(t, root, workDir, "2026-06-01T00-01-30", sessionKey, []string{
		`{"timestamp":"2026-06-01T00:01:31Z","type":"response_item","payload":{"type":"function_call","call_id":"call-codex-wrapped-read","name":"exec_command","arguments":"{\"cmd\":\"/usr/bin/env bash -lc \\\"sed -n '12,14p' src/app.ts\\\"\"}"}}`,
		`{"timestamp":"2026-06-01T00:01:32Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-codex-wrapped-read","output":"Command: /usr/bin/env bash -lc \"sed -n '12,14p' src/app.ts\"\nOutput:\nline 12\nline 13\nline 14\n"}}`,
	})
}

func writeStructuredCodexShellGrepFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeStructuredCodexFixture(t, root, workDir, "2026-06-01T00-02-00", sessionKey, []string{
		`{"timestamp":"2026-06-01T00:02:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-codex-grep","name":"exec_command","arguments":"{\"cmd\":\"rg -n \\\"needle\\\" README.md src/app.ts\"}"}}`,
		`{"timestamp":"2026-06-01T00:02:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-codex-grep","output":"Command: rg -n \"needle\" README.md src/app.ts\nOutput:\nREADME.md:1:needle\nsrc/app.ts:7:needle\n"}}`,
	})
}

func writeStructuredCodexJSONStringCommandFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeStructuredCodexFixture(t, root, workDir, "2026-06-01T00-03-00", sessionKey, []string{
		`{"timestamp":"2026-06-01T00:03:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-codex-json-command","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}`,
		`{"timestamp":"2026-06-01T00:03:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-codex-json-command","output":"{\"stdout\":\"ok ./...\\n\",\"stderr\":\"\",\"exit_code\":0}"}}`,
	})
}

func writeStructuredCodexWebSearchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeStructuredCodexFixture(t, root, workDir, "2026-06-01T00-04-00", sessionKey, []string{
		`{"timestamp":"2026-06-01T00:04:01Z","type":"response_item","payload":{"type":"web_search_call","id":"call-codex-web-search","query":"structured tool result formats","input":{"query":"ignored fallback","scope":"web"},"action":{"type":"search","source":"web"}}}`,
		`{"timestamp":"2026-06-01T00:04:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-codex-web-search","output":"Output:\nhttps://example.com/provider-format: Provider format notes\n"}}`,
	})
}

func writeStructuredCodexFixture(t *testing.T, root, workDir, localTimestamp, sessionKey string, entries []string) {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	lines := []string{fmt.Sprintf(`{"timestamp":"2026-06-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir)}
	lines = append(lines, entries...)
	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, structuredCodexFixtureFilename(localTimestamp, sessionKey)), []byte(payload), 0o644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
}

func structuredCodexFixtureFilename(localTimestamp, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "rollout-" + localTimestamp + "-structured.jsonl"
	}
	return "rollout-" + localTimestamp + "-" + sessionKey + ".jsonl"
}

func writeStructuredGeminiGrepFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	projectDir := filepath.Join(root, "gemini-project")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	body := `{
  "sessionId": "gemini-structured",
  "messages": [
    {"id":"gemini-1","timestamp":"2026-06-01T00:00:00Z","type":"gemini","content":"searching","toolCalls":[{"id":"call-gemini-grep","name":"grep_search","args":{"pattern":"needle"},"result":[{"functionResponse":{"id":"call-gemini-grep","response":{"output":"main.go:7:needle\nREADME.md:1:needle\n"}}}]}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, "session-structured.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini fixture: %v", err)
	}
}

func writeStructuredGeminiErrorFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	projectDir := filepath.Join(root, "gemini-project-error")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	body := strings.Join([]string{
		`{"sessionId":"gemini-error-message","kind":"main"}`,
		`{"id":"err-1","timestamp":"2026-06-21T17:08:12Z","type":"error","content":[{"text":"Gemini stream interrupted"}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(chatsDir, "session-error.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini error fixture: %v", err)
	}
}

func writeStructuredGeminiWriteFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	projectDir := filepath.Join(root, "gemini-project")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	body := `{
  "sessionId": "gemini-structured",
  "messages": [
    {"id":"gemini-1","timestamp":"2026-06-01T00:00:00Z","type":"gemini","content":"writing","toolCalls":[{"id":"call-gemini-write","name":"write_file","args":{"file_path":"notes.txt","content":"hello gemini"},"result":[{"functionResponse":{"id":"call-gemini-write","response":{"output":"Successfully created and wrote to new file: notes.txt"}}}],"resultDisplay":{"fileDiff":"Index: notes.txt\n===================================================================\n--- notes.txt\tOriginal\n+++ notes.txt\tWritten\n@@ -0,0 +1 @@\n+hello gemini","filePath":"notes.txt","originalContent":"","newContent":"hello gemini"}}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, "session-structured.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini fixture: %v", err)
	}
}

func writeStructuredGeminiWriteContentPairFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	projectDir := filepath.Join(root, "gemini-project")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	body := `{
  "sessionId": "gemini-structured",
  "messages": [
    {"id":"gemini-1","timestamp":"2026-06-01T00:00:00Z","type":"gemini","content":"writing","toolCalls":[{"id":"call-gemini-write","name":"write_file","args":{"file_path":"notes.txt","content":"hello gemini"},"result":[{"functionResponse":{"id":"call-gemini-write","response":{"output":"Successfully created and wrote to new file: notes.txt"}}}],"resultDisplay":{"filePath":"notes.txt","originalContent":"old text","newContent":"hello gemini"}}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, "session-structured.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini fixture: %v", err)
	}
}

func writeStructuredKimiReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	sum := md5.Sum([]byte(filepath.Clean(workDir)))
	workHash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, workHash, sessionKey, "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir kimi context dir: %v", err)
	}
	payload := strings.Join([]string{
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-kimi-read","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
		`{"role":"tool","content":[{"type":"text","text":"Kimi file data"}],"tool_call_id":"call-kimi-read"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write kimi fixture: %v", err)
	}
}

func writeStructuredKimiEditPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	sum := md5.Sum([]byte(filepath.Clean(workDir)))
	workHash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, workHash, sessionKey, "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir kimi context dir: %v", err)
	}
	payload := strings.Join([]string{
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-kimi-edit","function":{"name":"Edit","arguments":"{\"filePath\":\"README.md\",\"oldString\":\"old\",\"newString\":\"new\"}"}}]}`,
		`{"role":"tool","content":{"output":"Edited README.md","filePath":"README.md","patch":"--- README.md\n+++ README.md\n@@\n-old\n+new"},"tool_call_id":"call-kimi-edit"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write kimi fixture: %v", err)
	}
}

func writeStructuredOpenCodeEditFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"opencode-structured","directory":%q},
  "messages": [
    {"info":{"id":"opencode-1","sessionID":"opencode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-opencode-edit","tool":"Edit","state":{"status":"completed","input":{"filePath":"README.md","oldString":"old","newString":"new"},"output":"Edited README.md"}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "opencode", "session-structured.json"), body)
}

func writeStructuredOpenCodeEditPatchResultFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"opencode-structured","directory":%q},
  "messages": [
    {"info":{"id":"opencode-1","sessionID":"opencode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-opencode-edit","tool":"Edit","state":{"status":"completed","input":{"filePath":"README.md","oldString":"old","newString":"new"},"output":{"output":"Edited README.md","filePath":"README.md","patch":"--- README.md\n+++ README.md\n@@\n-old\n+new"}}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "opencode", "session-structured.json"), body)
}

func writeStructuredMimoCodeBashFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"mimocode-structured","directory":%q},
  "messages": [
    {"info":{"id":"mimocode-1","sessionID":"mimocode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-mimocode-bash","tool":"Bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":{"stdout":"ok ./...","exitCode":0}}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "mimocode", "session-structured.json"), body)
}

func writeStructuredMimoCodeBashDiffFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"mimocode-structured","directory":%q},
  "messages": [
    {"info":{"id":"mimocode-1","sessionID":"mimocode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-mimocode-diff","tool":"Bash","state":{"status":"completed","input":{"command":"git diff -- src/app.ts"},"output":{"stdout":"diff --git a/src/app.ts b/src/app.ts\n@@\n-old\n+new","exitCode":0}}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "mimocode", "session-structured.json"), body)
}

func writeStructuredPiReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-06-01T00:00:00.000Z","cwd":%q}
{"type":"message","id":"pi-user-1","parentId":null,"timestamp":"2026-06-01T00:00:00.000Z","message":{"role":"user","content":"read the file","timestamp":1780272000000}}
{"type":"message","id":"pi-assistant-1","parentId":"pi-user-1","timestamp":"2026-06-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-pi-read","name":"read","arguments":{"path":"README.md"}}],"timestamp":1780272001000}}
{"type":"message","id":"pi-tool-1","parentId":"pi-assistant-1","timestamp":"2026-06-01T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-pi-read","toolName":"read","content":[{"type":"text","text":"Pi file data"}],"isError":false,"timestamp":1780272002000}}
`, sessionKey, workDir)
	path := filepath.Join(root, "pi", sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir pi fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}
}

func writeStructuredPiEditPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-06-01T00:00:00.000Z","cwd":%q}
{"type":"message","id":"pi-assistant-1","parentId":null,"timestamp":"2026-06-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-pi-edit","name":"Edit","arguments":{"filePath":"README.md","oldString":"old","newString":"new"}}],"timestamp":1780272001000}}
{"type":"message","id":"pi-tool-1","parentId":"pi-assistant-1","timestamp":"2026-06-01T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-pi-edit","toolName":"Edit","content":{"output":"Edited README.md","filePath":"README.md","patch":"--- README.md\n+++ README.md\n@@\n-old\n+new"},"isError":false,"timestamp":1780272002000}}
`, sessionKey, workDir)
	path := filepath.Join(root, "pi", sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir pi fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}
}

func writeStructuredKiroWritePatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	sidecar := strings.TrimSuffix(path, filepath.Ext(path)) + ".json"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir kiro fixture dir: %v", err)
	}
	if err := os.WriteFile(sidecar, []byte(fmt.Sprintf(`{"id":%q,"cwd":%q}`, sessionKey, workDir)), 0o644); err != nil {
		t.Fatalf("write kiro sidecar: %v", err)
	}
	body := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call","toolCallId":"call-kiro-write","title":"write","kind":"edit","status":"pending","rawInput":{"path":"notes.txt","content":"hello kiro\n"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-kiro-write","status":"completed","content":[{"type":"diff","path":"notes.txt","oldText":"old\n","newText":"hello kiro\n"}]}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write kiro fixture: %v", err)
	}
}

func writeStructuredAmpEditPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir amp fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"system","subtype":"init","cwd":%q,"session_id":%q,"tools":["edit_file"],"mcp_servers":[]}`, workDir, sessionKey),
		`{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"call-amp-edit","name":"edit_file","input":{"filePath":"notes.txt","oldString":"old","newString":"new"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,"max_tokens":968000}},"parent_tool_use_id":null,"session_id":"` + sessionKey + `"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-amp-edit","content":"{\"filePath\":\"notes.txt\",\"patch\":\"*** Begin Patch\\n*** Update File: notes.txt\\n@@\\n-old\\n+new\\n*** End Patch\",\"oldString\":\"old\",\"newString\":\"new\"}","is_error":false}]},"parent_tool_use_id":null,"session_id":"` + sessionKey + `"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write amp fixture: %v", err)
	}
}

func writeStructuredCursorWriteFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cursor fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"system","subtype":"init","cwd":%q,"session_id":%q}`, workDir, sessionKey),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"writing"}]},"session_id":"` + sessionKey + `"}`,
		`{"type":"tool_call","subtype":"started","call_id":"call-cursor-write","tool_call":{"writeToolCall":{"toolCallId":"call-cursor-write","args":{"path":"notes.txt","fileText":"hello cursor\n"}}},"session_id":"` + sessionKey + `"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-cursor-write","tool_call":{"writeToolCall":{"toolCallId":"call-cursor-write","args":{"path":"notes.txt","fileText":"hello cursor\n"},"result":{"success":{"path":"notes.txt","linesCreated":1,"fileSize":13}}}},"session_id":"` + sessionKey + `"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write cursor fixture: %v", err)
	}
}

func writeStructuredCursorReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cursor fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"system","subtype":"init","cwd":%q,"session_id":%q}`, workDir, sessionKey),
		`{"type":"tool_call","subtype":"started","call_id":"call-cursor-read","tool_call":{"readToolCall":{"toolCallId":"call-cursor-read","args":{"path":"src/app.ts"}}},"session_id":"` + sessionKey + `"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-cursor-read","tool_call":{"readToolCall":{"toolCallId":"call-cursor-read","args":{"path":"src/app.ts"},"result":{"success":{"content":"export const app = true;\n","isEmpty":false,"exceededLimit":false,"totalLines":1,"totalChars":25}}}},"session_id":"` + sessionKey + `"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write cursor fixture: %v", err)
	}
}

func writeStructuredCursorBashFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cursor fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"type":"system","subtype":"init","cwd":%q,"session_id":%q}`, workDir, sessionKey),
		`{"type":"tool_call","subtype":"started","call_id":"call-cursor-bash","tool_call":{"function":{"name":"Bash","arguments":{"command":"npm test"}}},"session_id":"` + sessionKey + `"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-cursor-bash","tool_call":{"function":{"name":"Bash","arguments":{"command":"npm test"},"result":{"success":{"stdout":"ok\n","stderr":"","exitCode":0}}}},"session_id":"` + sessionKey + `"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write cursor fixture: %v", err)
	}
}

func writeStructuredGrokACPEditPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir grok fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"sessionId":%q,"cwd":%q}}`, sessionKey, workDir),
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call","toolCallId":"call-grok-edit","title":"search_replace","kind":"edit","status":"pending","rawInput":{"path":"notes.txt","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-grok-edit","status":"completed","content":[{"type":"diff","path":"notes.txt","oldText":"old\n","newText":"new\n"}]}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write grok fixture: %v", err)
	}
}

func writeStructuredAuggieACPEditPatchFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir auggie fixture dir: %v", err)
	}
	body := strings.Join([]string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"sessionId":%q,"cwd":%q}}`, sessionKey, workDir),
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call","toolCallId":"call-auggie-edit","title":"str-replace-editor","kind":"edit","status":"pending","rawInput":{"path":"notes.txt","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionKey + `","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-auggie-edit","status":"completed","content":[{"type":"diff","path":"notes.txt","oldText":"old\n","newText":"new\n"}]}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write auggie fixture: %v", err)
	}
}

func writeStructuredOpenCodeExport(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir opencode export: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write opencode export: %v", err)
	}
}

func writeStructuredAntigravityWriteFixture(t *testing.T, root, _ string, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir antigravity logs: %v", err)
	}
	body := strings.Join([]string{
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-06-01T00:00:00Z","content":"writing","tool_calls":[{"id":"call-antigravity-write","name":"Write","args":{"path":"notes.txt","content":"hello structured world"}}]}`,
		`{"step_index":2,"type":"WRITE_FILE","created_at":"2026-06-01T00:00:01Z","tool_call_id":"call-antigravity-write","content":"wrote notes.txt"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write antigravity fixture: %v", err)
	}
}

func writeStructuredAntigravityEditPatchFixture(t *testing.T, root, _ string, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir antigravity logs: %v", err)
	}
	resultContent := `{"output":"Edited notes.txt","filePath":"notes.txt","diff":"--- notes.txt\n+++ notes.txt\n@@\n-old\n+new","exitCode":0}`
	resultLine, err := json.Marshal(map[string]any{
		"step_index":   2,
		"type":         "WRITE_FILE",
		"created_at":   "2026-06-01T00:00:01Z",
		"tool_call_id": "call-antigravity-edit",
		"content":      resultContent,
	})
	if err != nil {
		t.Fatalf("marshal antigravity result line: %v", err)
	}
	body := strings.Join([]string{
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-06-01T00:00:00Z","content":"editing","tool_calls":[{"id":"call-antigravity-edit","name":"Edit","args":{"filePath":"notes.txt","oldString":"old","newString":"new"}}]}`,
		string(resultLine),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write antigravity fixture: %v", err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func structuredResultItemsContainURL(items []SessionStructuredSearchResultItem, want string) bool {
	for _, item := range items {
		if item.URL == want {
			return true
		}
	}
	return false
}

func boolPtr(value bool) *bool {
	return &value
}
