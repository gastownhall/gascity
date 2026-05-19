package workertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	worker "github.com/gastownhall/gascity/internal/worker"
)

func writeMalformedHistoryTranscript(t *testing.T, profile Profile) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		path := filepath.Join(t.TempDir(), "session.jsonl")
		body := strings.Join([]string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"malformed-claude"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":"done"},"timestamp":"2026-04-04T09:00:01Z","sessionId":"malformed-claude"}`,
		}, "\n") + "\n" + `{"uuid":"torn","type":"assistant","message":`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write malformed claude transcript: %v", err)
		}
		return path
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}`,
			`not json`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
		})
	case ProfileGeminiTmuxCLI:
		path := filepath.Join(t.TempDir(), "session.json")
		if err := os.WriteFile(path, []byte(`{"sessionId":"malformed-gemini","messages":[`), 0o644); err != nil {
			t.Fatalf("write malformed gemini transcript: %v", err)
		}
		return path
	case ProfileOpenCodeTmuxCLI:
		path := filepath.Join(t.TempDir(), "session.json")
		if err := os.WriteFile(path, []byte(`{"info":{"id":"malformed-opencode","directory":"/tmp/gascity/phase2/opencode"},"messages":[`), 0o644); err != nil {
			t.Fatalf("write malformed opencode transcript: %v", err)
		}
		return path
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeInteractionHistoryTranscript(t *testing.T, profile Profile) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		return writeLinesFile(t, "session.jsonl", []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"interaction-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"interaction-phase2"}`,
		})
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"run a tool"}]}}`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}}`,
		})
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		path := filepath.Join(dir, "session.json")
		body := `{
  "sessionId": "gemini-interaction-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"user","content":"run a tool"},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"gemini","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}]}
  ]
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini interaction transcript: %v", err)
		}
		return path
	case ProfileOpenCodeTmuxCLI:
		return writeOpenCodeExportTranscript(t, "opencode-interaction-phase2", "/tmp/gascity/phase2/opencode", []string{
			`{"info":{"id":"msg_user_1","sessionID":"opencode-interaction-phase2","role":"user","time":{"created":1770000000000}},"parts":[{"id":"part_user_1","type":"text","text":"run a tool"}]}`,
			`{"info":{"id":"msg_assistant_1","sessionID":"opencode-interaction-phase2","role":"assistant","parentID":"msg_user_1","time":{"created":1770000001000}},"parts":[{"id":"part_interaction_1","type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}]}`,
		})
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeInteractionLifecycleTranscript(t *testing.T, profile Profile, finalState worker.InteractionState) string {
	t.Helper()

	finalStateText := string(finalState)
	finalAction := "acknowledge"
	if finalState == worker.InteractionStateDismissed {
		finalAction = "dismiss"
	}

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		return writeLinesFile(t, "session.jsonl", []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"interaction-lifecycle-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"interaction-lifecycle-phase2"}`,
			fmt.Sprintf(`{"uuid":"u2","parentUuid":"a1","type":"user","message":{"role":"user","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":%q,"action":%q}]},"timestamp":"2026-04-04T09:00:02Z","sessionId":"interaction-lifecycle-phase2"}`, finalStateText, finalAction),
		})
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}}`,
			fmt.Sprintf(`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":%q,"action":%q}}`, finalStateText, finalAction),
		})
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		path := filepath.Join(dir, "session.json")
		body := fmt.Sprintf(`{
  "sessionId": "gemini-interaction-lifecycle-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"gemini","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"user","content":"interaction updated","interactions":[{"request_id":"approval-1","kind":"approval","state":%q,"action":%q}]}
  ]
}`, finalStateText, finalAction)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini interaction lifecycle transcript: %v", err)
		}
		return path
	case ProfileOpenCodeTmuxCLI:
		return writeOpenCodeExportTranscript(t, "opencode-interaction-lifecycle-phase2", "/tmp/gascity/phase2/opencode", []string{
			`{"info":{"id":"msg_assistant_1","sessionID":"opencode-interaction-lifecycle-phase2","role":"assistant","time":{"created":1770000000000}},"parts":[{"id":"part_interaction_1","type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]}`,
			fmt.Sprintf(`{"info":{"id":"msg_user_1","sessionID":"opencode-interaction-lifecycle-phase2","role":"user","parentID":"msg_assistant_1","time":{"created":1770000001000}},"parts":[{"id":"part_interaction_2","type":"interaction","request_id":"approval-1","kind":"approval","state":%q,"action":%q}]}`, finalStateText, finalAction),
		})
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeToolTranscript(t *testing.T, profile Profile, openTail bool) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		lines := []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"read the file"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"tool-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"}}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"tool-phase2"}`,
		}
		if !openTail {
			lines = append(lines,
				`{"uuid":"r1","parentUuid":"a1","type":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file data"}],"is_error":false},"timestamp":"2026-04-04T09:00:02Z","sessionId":"tool-phase2"}`,
				`{"uuid":"a2","parentUuid":"r1","type":"assistant","message":{"role":"assistant","content":"done"},"timestamp":"2026-04-04T09:00:03Z","sessionId":"tool-phase2"}`,
			)
		}
		return writeLinesFile(t, "session.jsonl", lines)
	case ProfileCodexTmuxCLI:
		lines := []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"session_meta","payload":{"cwd":"/tmp/gascity/phase2/codex"}}`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"Read"}}`,
		}
		if !openTail {
			lines = append(lines,
				`{"timestamp":"2026-04-04T09:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"file data"}}`,
				`{"timestamp":"2026-04-04T09:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
			)
		}
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), lines)
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		projectDir := filepath.Join(dir, "project-a", "chats")
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatalf("mkdir gemini tree: %v", err)
		}
		path := filepath.Join(projectDir, "session.json")
		body := `{
  "sessionId": "gemini-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"user","content":"hello"},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"gemini","content":"checking","toolCalls":[{"id":"tool-2","name":"Read","args":{"path":"README.md"}}]}` + trailingGeminiToolResult(openTail) + `
  ]
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini transcript: %v", err)
		}
		return path
	case ProfileOpenCodeTmuxCLI:
		state := `{"status":"running","input":{"path":"README.md"}}`
		tail := ""
		if !openTail {
			state = `{"status":"completed","input":{"path":"README.md"},"output":"file data"}`
			tail = `,{"info":{"id":"msg_assistant_2","sessionID":"opencode-tool-phase2","role":"assistant","parentID":"msg_assistant_1","time":{"created":1770000002000}},"parts":[{"id":"part_assistant_2","type":"text","text":"done"}]}`
		}
		return writeOpenCodeExportTranscript(t, "opencode-tool-phase2", "/tmp/gascity/phase2/opencode", []string{
			`{"info":{"id":"msg_user_1","sessionID":"opencode-tool-phase2","role":"user","time":{"created":1770000000000}},"parts":[{"id":"part_user_1","type":"text","text":"read the file"}]}`,
			`{"info":{"id":"msg_assistant_1","sessionID":"opencode-tool-phase2","role":"assistant","parentID":"msg_user_1","time":{"created":1770000001000}},"parts":[{"id":"part_tool_1","type":"tool","callID":"call-1","tool":"Read","state":` + state + `}]}` + tail,
		})
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeOpenCodeExportTranscript(t *testing.T, sessionID, workDir string, messages []string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "session.json")
	body := `{"info":{"id":` + fmt.Sprintf("%q", sessionID) + `,"directory":` + fmt.Sprintf("%q", workDir) + `},"messages":[` + strings.Join(messages, ",") + `]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write opencode transcript: %v", err)
	}
	return path
}

func writeLinesFile(t *testing.T, rel string, lines []string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func trailingGeminiToolResult(openTail bool) string {
	if openTail {
		return ""
	}
	return `,
    {"id":"m3","timestamp":"2026-04-04T09:00:02Z","type":"gemini","content":"done","toolCalls":[{"id":"tool-2","name":"Read","args":{"path":"README.md"},"result":[{"functionResponse":{"id":"tool-2","response":{"output":"file data"}}}]}]}`
}

func historyHasBlockKind(history *worker.HistorySnapshot, kind worker.BlockKind) bool {
	for _, entry := range history.Entries {
		for _, block := range entry.Blocks {
			if block.Kind == kind {
				return true
			}
		}
	}
	return false
}

func historyHasOpenToolUseEvidence(history *worker.HistorySnapshot) bool {
	if len(history.TailState.OpenToolUseIDs) > 0 {
		for _, toolUseID := range history.TailState.OpenToolUseIDs {
			if strings.TrimSpace(toolUseID) != "" {
				return true
			}
		}
	}
	if len(history.Entries) == 0 {
		return false
	}
	last := history.Entries[len(history.Entries)-1]
	for _, block := range last.Blocks {
		if block.Kind == worker.BlockKindToolUse {
			return true
		}
	}
	return false
}

func loadHistory(t *testing.T, provider, path string) *worker.HistorySnapshot {
	t.Helper()

	history, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
		Provider:       provider,
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory(%s): %v", provider, err)
	}
	return history
}
