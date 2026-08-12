package workertest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	worker "github.com/gastownhall/gascity/internal/worker"
)

func TestStructuredCatalogStaysAligned(t *testing.T) {
	catalog := StructuredCatalog()
	want := []RequirementCode{
		RequirementStructuredToolResult,
		RequirementStructuredNoNativeLeak,
		RequirementStructuredEditEvidence,
	}
	if len(catalog) != len(want) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(want))
	}
	seen := map[RequirementCode]bool{}
	for _, requirement := range catalog {
		if requirement.Group != "structured" {
			t.Fatalf("requirement %s group = %q, want structured", requirement.Code, requirement.Group)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = true
	}
	for _, code := range want {
		if !seen[code] {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}
}

// structuredConformanceProfile pairs a worker profile with a loader that
// materializes a normalized history carrying a structured tool result. All
// three canonical profiles run here against SYNTHETIC fixtures whose frame
// shapes mirror the repo's provider fixtures (writeStructuredCodexPatchFixture,
// writeStructuredGeminiWriteFixture, and the Claude transcript shape). Real
// broker-captured transcripts are exercised separately by
// TestStructuredCorpusConformance against testdata/corpus/.
type structuredConformanceProfile struct {
	profile ProfileID
	load    func(t *testing.T) *worker.HistorySnapshot
}

func structuredConformanceProfiles() []structuredConformanceProfile {
	return []structuredConformanceProfile{
		{profile: ProfileClaudeTmuxCLI, load: loadClaudeStructuredHistory},
		{profile: ProfileCodexTmuxCLI, load: loadCodexStructuredHistory},
		{profile: ProfileGeminiTmuxCLI, load: loadGeminiStructuredHistory},
	}
}

// loadGeminiStructuredHistory writes a Gemini session containing a write_file
// tool call whose resultDisplay carries a file diff, then normalizes it through
// the real worker adapter. The shape mirrors the repo's existing gemini fixtures
// (writeStructuredGeminiWriteFixture).
func loadGeminiStructuredHistory(t *testing.T) *worker.HistorySnapshot {
	t.Helper()
	projectDir := filepath.Join(t.TempDir(), "gemini-project")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o750); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte("/work"), 0o600); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	path := filepath.Join(chatsDir, "session-structured.json")
	if err := os.WriteFile(path, []byte(geminiStructuredFixtureJSON()), 0o600); err != nil {
		t.Fatalf("write gemini fixture: %v", err)
	}
	history, err := (worker.SessionLogAdapter{}).LoadHistory(worker.LoadRequest{
		Provider:       "gemini",
		TranscriptPath: path,
		GCSessionID:    "gemini-struct",
	})
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	return history
}

// geminiStructuredFixtureJSON returns a Gemini session whose write_file tool call
// reports a result-side file diff.
func geminiStructuredFixtureJSON() string {
	return `{
  "sessionId": "gemini-structured",
  "messages": [
    {"id":"gemini-1","timestamp":"2026-06-01T00:00:00Z","type":"gemini","content":"writing","toolCalls":[{"id":"call-gemini-write","name":"write_file","args":{"file_path":"notes.txt","content":"hello gemini"},"result":[{"functionResponse":{"id":"call-gemini-write","response":{"output":"Successfully created and wrote to new file: notes.txt"}}}],"resultDisplay":{"fileDiff":"Index: notes.txt\n===================================================================\n--- notes.txt\tOriginal\n+++ notes.txt\tWritten\n@@ -0,0 +1 @@\n+hello gemini","filePath":"notes.txt","originalContent":"","newContent":"hello gemini"}}]}
  ]
}`
}

func TestStructuredConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "structured", map[string]string{"tier": "worker-core"})

	for _, tc := range structuredConformanceProfiles() {
		tc := tc
		t.Run(string(tc.profile), func(t *testing.T) {
			history := tc.load(t)

			t.Run(string(RequirementStructuredToolResult), func(t *testing.T) {
				reporter.Require(t, StructuredToolResultResult(tc.profile, history))
			})
			t.Run(string(RequirementStructuredNoNativeLeak), func(t *testing.T) {
				reporter.Require(t, StructuredNoNativeLeakResult(tc.profile, history))
			})
			t.Run(string(RequirementStructuredEditEvidence), func(t *testing.T) {
				result := StructuredEditEvidenceResult(tc.profile, history)
				if result.Status == ResultUnsupported {
					reporter.Record(result)
					t.Skip(result.Detail)
					return
				}
				reporter.Require(t, result)
			})
		})
	}
}

// loadClaudeStructuredHistory writes a Claude JSONL transcript containing an
// Edit tool call whose result carries provider-side patch evidence, then
// normalizes it through the real worker adapter.
func loadClaudeStructuredHistory(t *testing.T) *worker.HistorySnapshot {
	t.Helper()
	lines := claudeStructuredFixtureLines()
	path := filepath.Join(t.TempDir(), "session-struct.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	history, err := (worker.SessionLogAdapter{}).LoadHistory(worker.LoadRequest{
		Provider:       "claude",
		TranscriptPath: path,
		GCSessionID:    "struct-1",
	})
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	return history
}

// loadCodexStructuredHistory writes a Codex rollout containing an apply_patch
// edit whose patch_apply_end event carries the provider's unified diff, then
// normalizes it through the real worker adapter. The frame shapes mirror the
// repo's existing codex fixtures (writeStructuredCodexPatchFixture).
func loadCodexStructuredHistory(t *testing.T) *worker.HistorySnapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-06-01T00-00-00-codexstruct.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(codexStructuredFixtureLines(), "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
	history, err := (worker.SessionLogAdapter{}).LoadHistory(worker.LoadRequest{
		Provider:       "codex",
		TranscriptPath: path,
		GCSessionID:    "codex-struct",
	})
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	return history
}

// codexStructuredFixtureLines returns a Codex rollout whose apply_patch result
// carries the provider's unified diff via a patch_apply_end event.
func codexStructuredFixtureLines() []string {
	return []string{
		`{"timestamp":"2026-06-01T00:00:00Z","type":"session_meta","payload":{"cwd":"/work"}}`,
		`{"timestamp":"2026-06-01T00:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-codex-edit","name":"apply_patch","input":"*** Begin Patch\n*** Update File: note.txt\n@@\n-// sample file\n+// golden file\n*** End Patch\n"}}`,
		`{"timestamp":"2026-06-01T00:00:02Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call-codex-edit","stdout":"Success. Updated the following files:\nM note.txt\n","stderr":"","success":true,"changes":{"note.txt":{"type":"update","unified_diff":"@@ -1 +1 @@\n-// sample file\n+// golden file\n","move_path":null}},"status":"completed"}}`,
		`{"timestamp":"2026-06-01T00:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-codex-edit","output":"{\"output\":\"Success. Updated the following files:\\nM note.txt\\n\"}"}}`,
	}
}

// claudeStructuredFixtureLines returns a Claude JSONL transcript whose Edit tool
// result carries provider-side structuredPatch evidence. Shared by the synthetic
// conformance test and the corpus-loader test.
func claudeStructuredFixtureLines() []string {
	return []string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"Edit README.md."},"timestamp":"2026-06-01T00:00:00Z","sessionId":"struct-1"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","id":"m1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"call-edit","name":"Edit","input":{"file_path":"README.md","old_string":"old line","new_string":"new line"}}]},"timestamp":"2026-06-01T00:00:01Z","sessionId":"struct-1"}`,
		`{"uuid":"r1","parentUuid":"a1","type":"tool_result","toolUseID":"call-edit","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-edit","content":"The file README.md has been updated successfully."}]},"toolUseResult":{"filePath":"README.md","oldString":"old line","newString":"new line","originalFile":"export const message = \"old line\";\n","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-export const message = \"old line\";","+export const message = \"new line\";"]}],"userModified":false,"replaceAll":false},"timestamp":"2026-06-01T00:00:02Z","sessionId":"struct-1"}`,
	}
}
