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
		// The naming arm of the same restart-mid-tool-call shape: no
		// announcing tool_call reached this process, so the result block's
		// name comes from the terminal update through toolCall's own
		// title/kind/"tool" chain. The three rows below pin each link;
		// reverting the fallback to a bare &acpToolState{} turns all three
		// red and nothing else in the package.
		{
			name: "an unannounced tool call is named from the update title",
			updates: []string{
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","title":"Edit x.go","kind":"edit","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if block.Name != "Edit x.go" {
					t.Errorf("tool name = %q, want the update's title", block.Name)
				}
			},
		},
		{
			name: "an unannounced tool call with no title falls back to its kind",
			updates: []string{
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","kind":"edit","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if block.Name != "edit" {
					t.Errorf("tool name = %q, want the update's kind", block.Name)
				}
			},
		},
		{
			name: "an unannounced tool call with neither title nor kind is named tool",
			updates: []string{
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}`,
			},
			check: func(t *testing.T, block ContentBlock) {
				if block.Name != "tool" {
					t.Errorf("tool name = %q, want the %q default", block.Name, "tool")
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

// A run open at EOF is partial whether or not a gc prompt is outstanding.
// Both shapes here open a run with none in flight, so gating on the prompt
// streamed them truncated under an id that never settles.
func TestReadACPCaptureRunOpenWithNoPromptInFlightIsPartial(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		text  string
	}{
		{
			// update() admits a user_message_chunk run only when no prompt is
			// in flight, so this run can never have one.
			name: "user message chunk",
			lines: []string{
				acpHeaderLine("1"),
				acpRecord("in", acpChunkMsg("user_message_chunk", "PART-ONE ")),
			},
			text: "PART-ONE ",
		},
		{
			// The prompt's response cleared currentPrompt, then the agent kept
			// streaming; this opens a fresh run with none outstanding.
			name: "agent message chunk after end_turn",
			lines: []string{
				acpHeaderLine("1"),
				acpRecord("out", acpPromptMsg("2", "go")),
				acpRecord("in", acpChunkMsg("agent_message_chunk", "DONE")),
				acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`)),
				acpRecord("in", acpChunkMsg("agent_message_chunk", "AFTER-")),
			},
			text: "AFTER-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := ReadProviderFile("unreal-acp", writeACPCapture(t, joinLines(tc.lines...)), 0)
			if err != nil {
				t.Fatalf("ReadProviderFile: %v", err)
			}
			last := sess.Messages[len(sess.Messages)-1]
			if acpEntryText(last) != tc.text {
				t.Fatalf("last entry %q, want the open run %q", acpEntryText(last), tc.text)
			}
			if !last.Partial {
				t.Errorf("open run %q partial=false, want partial", acpEntryText(last))
			}
			// Only the open run is partial; earlier entries already settled.
			for _, entry := range sess.Messages[:len(sess.Messages)-1] {
				if entry.Partial {
					t.Errorf("settled entry %s is partial", entry.UUID)
				}
			}
		})
	}
}

// The withheld population that settledHistorySnapshot's doc block records: a
// capture that stops growing with a run open never settles it, so repeated
// reads of the same bytes keep reporting the trailing run partial.
// TestReadACPCapturePartialRun is the contrast -- there the file grows and the
// run settles on the next read.
func TestReadACPCaptureQuiescedRunStaysPartialAcrossReads(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "DONE")),
		acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`)),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "TAIL")),
	))
	for _, read := range []string{"first", "second"} {
		sess, err := ReadProviderFile("unreal-acp", path, 0)
		if err != nil {
			t.Fatalf("%s read ReadProviderFile: %v", read, err)
		}
		last := sess.Messages[len(sess.Messages)-1]
		if acpEntryText(last) != "TAIL" || !last.Partial {
			t.Fatalf("%s read: trailing run %q partial=%v, want the post-end_turn run still held back",
				read, acpEntryText(last), last.Partial)
		}
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

// The mirror of TestReadACPCaptureDropMarkers: the drop sits after the
// response, so the gap can hide a newer prompt and the answered prompt the
// backward scan reaches no longer proves the agent is idle. Both directions
// need a pin, since fixing one while leaving the other unpinned is how the
// asymmetry arose.
func TestReadACPCaptureDropAfterResponseIsUnknown(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "x")),
		acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`)),
		`{"ts":"2026-09-25T10:00:04Z","dir":"meta","dropped":3}`,
		acpUpdateRecord(`{"sessionUpdate":"available_commands_update","availableCommands":[]}`),
	))
	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatalf("ExtractTailMeta: %v", err)
	}
	if meta.Activity != "" {
		t.Errorf("activity = %q, want unknown: the drop can hide a newer prompt", meta.Activity)
	}
}

// The third decided exit of the backward scan. The two tests above both
// reach the prompt arm; here the drop hides every prompt since the header,
// so the scan runs out of records and lands on the header itself. Drops are
// direction-blind, so this gap can hide the only prompt of the epoch just as
// easily as a response, and the header cannot prove idleness across it.
func TestReadACPCaptureDropBeforeAnyPromptIsUnknown(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		`{"ts":"2026-09-25T10:00:01Z","dir":"meta","dropped":2}`,
		acpUpdateRecord(`{"sessionUpdate":"available_commands_update","availableCommands":[]}`),
	))
	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatalf("ExtractTailMeta: %v", err)
	}
	if meta.Activity != "" {
		t.Errorf("activity = %q, want unknown: the drop can hide the only prompt", meta.Activity)
	}
}
