package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acpUpdateRecord(update string) string {
	return acpRecord("in", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":`+update+`}}`)
}

// acpToolResults maps each tool_result's tool call id to its block.
func acpToolResults(t *testing.T, sess *Session) map[string]ContentBlock {
	t.Helper()
	results := make(map[string]ContentBlock)
	for _, entry := range sess.Messages {
		for _, block := range entry.ContentBlocks() {
			if block.Type == "tool_result" {
				results[block.ToolUseID] = block
			}
		}
	}
	return results
}

func acpResultText(t *testing.T, block ContentBlock) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(block.Content, &text); err != nil {
		t.Fatalf("tool_result %s content %s is not text: %v", block.ToolUseID, block.Content, err)
	}
	return text
}

func acpMessageMeta(t *testing.T, entry *Entry) (string, map[string]int) {
	t.Helper()
	var message struct {
		StopReason string         `json:"stop_reason"`
		Usage      map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(entry.Message, &message); err != nil {
		t.Fatalf("decoding %s message: %v", entry.UUID, err)
	}
	return message.StopReason, message.Usage
}

// The terminal tool_call_update lines in unreal_tools.jsonl were serialized by
// the ACP Go SDK in the shape acp-unreal sends: the model-visible output is in
// content, and rawOutput is {} or shell counters.
func TestReadACPCaptureToolResultPrefersContentOverRawOutput(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "acp_wire", "unreal_tools.jsonl"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	sess, err := ReadProviderFile("unreal-acp", writeACPCapture(t, string(data)), 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	results := acpToolResults(t, sess)
	for callID, want := range map[string]string{
		"t-read":  "package main\nfunc main() {}",
		"t-shell": "ok  ./...  0.2s",
	} {
		block, ok := results[callID]
		if !ok {
			t.Fatalf("no tool_result for %s", callID)
		}
		if got := acpResultText(t, block); got != want {
			t.Errorf("%s result = %q, want %q", callID, got, want)
		}
		if block.IsError {
			t.Errorf("%s result is_error, want success", callID)
		}
	}
}

func TestReadACPCaptureToolResultContentRules(t *testing.T) {
	prompt := acpRecord("out", acpPromptMsg("2", "go"))
	tests := []struct {
		name    string
		updates []string
		check   func(t *testing.T, block ContentBlock)
	}{
		{
			name: "content on the initial tool_call survives a status-only completion",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit x.go","kind":"edit","status":"pending","content":[{"type":"diff","path":"/w/x.go","oldText":"a","newText":"b"}]}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				var diff map[string]string
				if err := json.Unmarshal(block.Content, &diff); err != nil {
					t.Fatalf("content %s: %v", block.Content, err)
				}
				if diff["file_path"] != "/w/x.go" || diff["old_string"] != "a" || diff["new_string"] != "b" || !strings.Contains(diff["patch"], "+b") {
					t.Errorf("diff result = %v", diff)
				}
			},
		},
		{
			name: "a later content collection replaces the earlier one",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run","status":"pending","content":[{"type":"content","content":{"type":"text","text":"starting"}}]}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"line 1"}},{"type":"terminal","terminalId":"term-7"}]}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if got := acpResultText(t, block); got != "line 1\n[terminal term-7]" {
					t.Errorf("result = %q, want the latest collection", got)
				}
			},
		},
		{
			name: "a diff with text keeps the text as output",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"edited"}},{"type":"diff","path":"/w/y.go","newText":"z"}]}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				var diff map[string]string
				if err := json.Unmarshal(block.Content, &diff); err != nil {
					t.Fatalf("content %s: %v", block.Content, err)
				}
				if diff["output"] != "edited" || diff["file_path"] != "/w/y.go" {
					t.Errorf("diff result = %v", diff)
				}
			},
		},
		{
			name: "rawOutput is the result when there is no content",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{"stdout":"hi"}}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if !strings.Contains(string(block.Content), `"hi"`) {
					t.Errorf("result = %s, want the raw output", block.Content)
				}
			},
		},
		{
			name: "an empty rawOutput and no content leaves the status",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{}}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if string(block.Content) != `{"status":"completed"}` {
					t.Errorf("result = %s, want the status", block.Content)
				}
			},
		},
		{
			name: "a nonzero exit code in rawOutput flags content results as errors",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"shell","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"FAIL"}}],"rawOutput":{"exitCode":1}}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if acpResultText(t, block) != "FAIL" || !block.IsError {
					t.Errorf("result = %s is_error=%v, want the text flagged as an error", block.Content, block.IsError)
				}
			},
		},
		{
			name: "an update for an unannounced tool call still reads its content",
			updates: []string{
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"failed","content":[{"type":"content","content":{"type":"text","text":"denied"}}]}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if acpResultText(t, block) != "denied" || !block.IsError {
					t.Errorf("result = %s is_error=%v", block.Content, block.IsError)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := []string{acpHeaderLine("1"), prompt}
			for _, update := range tt.updates {
				lines = append(lines, acpUpdateRecord(update))
			}
			sess, err := ReadProviderFile("unreal-acp", writeACPCapture(t, joinLines(lines...)), 0)
			if err != nil {
				t.Fatalf("ReadProviderFile: %v", err)
			}
			block, ok := acpToolResults(t, sess)["t1"]
			if !ok {
				t.Fatal("no tool_result for t1")
			}
			tt.check(t, block)
		})
	}
}

func TestReadACPCaptureTurnEndStamping(t *testing.T) {
	endTurn := `{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":2}}`
	t.Run("a turn ending on text stamps the text", func(t *testing.T) {
		sess, err := ReadProviderFile("unreal-acp", writeACPCapture(t, joinLines(
			acpHeaderLine("1"),
			acpRecord("out", acpPromptMsg("2", "go")),
			acpRecord("in", acpChunkMsg("agent_message_chunk", "done")),
			acpRecord("in", acpResultMsg("2", endTurn)),
		)), 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		last := sess.Messages[len(sess.Messages)-1]
		stop, usage := acpMessageMeta(t, last)
		if acpEntryText(last) != "done" || stop != "end_turn" || usage["input_tokens"] != 10 {
			t.Errorf("last entry %q stop=%q usage=%v, want the stamped text", acpEntryText(last), stop, usage)
		}
	})
	t.Run("a turn ending on a tool result gets its own entry", func(t *testing.T) {
		before := joinLines(
			acpHeaderLine("1"),
			acpRecord("out", acpPromptMsg("2", "go")),
			acpRecord("in", acpChunkMsg("agent_message_chunk", "Running.")),
			acpUpdateRecord(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run","status":"pending"}`),
			acpUpdateRecord(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"ok"}}]}`),
		)
		path := writeACPCapture(t, before)
		open, err := ReadProviderFile("unreal-acp", path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		if err := os.WriteFile(path, []byte(before+acpRecord("in", acpResultMsg("2", endTurn))+"\n"), 0o600); err != nil {
			t.Fatalf("append: %v", err)
		}
		done, err := ReadProviderFile("unreal-acp", path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		if len(done.Messages) != len(open.Messages)+1 {
			t.Fatalf("entries %d -> %d, want one turn-end entry added", len(open.Messages), len(done.Messages))
		}
		// Entries already read are unchanged: nothing earlier is rewritten.
		for i, entry := range open.Messages {
			if string(done.Messages[i].Message) != string(entry.Message) {
				t.Errorf("entry %s rewritten: %s -> %s", entry.UUID, entry.Message, done.Messages[i].Message)
			}
		}
		last := done.Messages[len(done.Messages)-1]
		stop, usage := acpMessageMeta(t, last)
		if last.Type != "assistant" || stop != "end_turn" || usage["output_tokens"] != 2 || len(last.ContentBlocks()) != 0 {
			t.Errorf("turn-end entry %s type=%s stop=%q usage=%v blocks=%v", last.UUID, last.Type, stop, usage, last.ContentBlocks())
		}
	})
	t.Run("a late response to an older prompt does not end the newer turn", func(t *testing.T) {
		path := writeACPCapture(t, joinLines(
			acpHeaderLine("1"),
			acpRecord("out", acpPromptMsg("3", "first")),
			acpRecord("out", acpPromptMsg("4", "second")),
			acpRecord("in", acpChunkMsg("agent_message_chunk", "answer")),
			acpRecord("in", acpResultMsg("3", `{"stopReason":"max_tokens"}`)),
		))
		sess, err := ReadProviderFile("unreal-acp", path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		last := sess.Messages[len(sess.Messages)-1]
		if stop, _ := acpMessageMeta(t, last); acpEntryText(last) != "answer" || stop != "" {
			t.Errorf("last entry %q stop=%q, want the newer turn's text unstamped", acpEntryText(last), stop)
		}
		if !last.Partial {
			t.Error("the newer turn's reply is not partial, want it still open")
		}
	})
}

func TestReadACPCapturePartialRun(t *testing.T) {
	lines := []string{
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "ALPHA-")),
	}
	sess, err := ReadProviderFile("unreal-acp", writeACPCapture(t, joinLines(lines...)), 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	last := sess.Messages[len(sess.Messages)-1]
	if !last.Partial || acpEntryText(last) != "ALPHA-" {
		t.Fatalf("streaming reply %q partial=%v, want partial", acpEntryText(last), last.Partial)
	}
	if sess.Messages[0].Partial {
		t.Error("the prompt is partial, want only the open run")
	}

	lines = append(lines,
		acpRecord("in", acpChunkMsg("agent_message_chunk", "BRAVO")),
		acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`)),
	)
	sess, err = ReadProviderFile("unreal-acp", writeACPCapture(t, joinLines(lines...)), 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	last = sess.Messages[len(sess.Messages)-1]
	if last.Partial || acpEntryText(last) != "ALPHA-BRAVO" {
		t.Fatalf("finished reply %q partial=%v, want settled", acpEntryText(last), last.Partial)
	}
}

func TestReadACPCaptureDropMarkers(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "x")),
		`{"ts":"2026-09-25T10:00:03Z","dir":"meta","dropped":2}`,
		acpUpdateRecord(`{"sessionUpdate":"available_commands_update","availableCommands":[]}`),
	))
	sess, err := ReadProviderFile("unreal-acp", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	if sess.Diagnostics.DroppedRecordCount != 2 {
		t.Errorf("DroppedRecordCount = %d, want 2", sess.Diagnostics.DroppedRecordCount)
	}
	// The dropped records may include the prompt's response, so the turn
	// state is unknown rather than in-turn forever.
	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatalf("ExtractTailMeta: %v", err)
	}
	if meta.Activity != "" {
		t.Errorf("activity = %q, want unknown after a drop marker", meta.Activity)
	}
}
