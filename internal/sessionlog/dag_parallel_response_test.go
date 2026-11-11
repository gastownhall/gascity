package sessionlog

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestBuildDagKeepsParallelCallsFromOneResponse pins the layout Claude Code
// 2.1.296 writes for parallel tool calls (observed live): one API response is
// split into rows chained by parentUuid and sharing message.id (thinking ->
// call-a -> call-b). Each result is parented to its own call, and the reply
// continues from the EARLIER call's result. The selected branch then runs
// through call-a and result-a, leaving call-b and result-b on a sibling
// branch. Both calls and both results belong to the conversation.
func TestBuildDagKeepsParallelCallsFromOneResponse(t *testing.T) {
	msg := func(role, id, content string) json.RawMessage {
		return json.RawMessage(`{"role":"` + role + `","id":"` + id + `","content":` + content + `}`)
	}
	entries := []*Entry{
		{UUID: "root", Type: "user", Message: msg("user", "", `"run both"`)},
		{UUID: "think", ParentUUID: "root", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"thinking","thinking":""}]`)},
		{UUID: "call-a", ParentUUID: "think", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"echo PAR1"}}]`)},
		{UUID: "call-b", ParentUUID: "call-a", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"echo PAR2"}}]`)},
		{UUID: "result-b", ParentUUID: "call-b", Type: "user", Message: msg("user", "", `[{"type":"tool_result","tool_use_id":"b","content":"PAR2"}]`)},
		{UUID: "result-a", ParentUUID: "call-a", Type: "user", Message: msg("user", "", `[{"type":"tool_result","tool_use_id":"a","content":"PAR1"}]`)},
		{UUID: "answer", ParentUUID: "result-a", Type: "assistant", Message: msg("assistant", "msg_2", `[{"type":"text","text":"done"}]`)},
	}
	for i, e := range entries {
		e.Timestamp = time.Unix(int64(i), 0)
	}
	dag := BuildDag(entries)
	var got []string
	for _, e := range dag.ActiveBranch {
		got = append(got, e.UUID)
	}
	want := []string{"root", "think", "call-a", "call-b", "result-b", "result-a", "answer"}
	if len(got) != len(want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active branch = %v, want %v", got, want)
		}
	}
	if len(dag.OrphanedToolUseIDs) != 0 {
		t.Fatalf("answered parallel calls reported open: %v", dag.OrphanedToolUseIDs)
	}

	// A sibling assistant row from a different response is another branch,
	// not part of the selected conversation.
	other := &Entry{
		UUID: "other", ParentUUID: "call-a", Type: "assistant", Timestamp: time.Unix(1, 0),
		Message: msg("assistant", "msg_other", `[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"echo nope"}}]`),
	}
	dag = BuildDag(append(append([]*Entry{}, entries...), other))
	for _, e := range dag.ActiveBranch {
		if e.UUID == "other" {
			t.Fatal("a sibling row from another response entered the selected conversation")
		}
	}
}

// TestBuildDagResultTipSiblings pins which assistant sibling of a tool call
// demotes that call's newer result from the active tip: only another call of
// the same response, or of a response whose identity is unknown. A call from
// another response forked the conversation, and a text-only row such as an
// API error is not a call.
func TestBuildDagResultTipSiblings(t *testing.T) {
	tests := []struct {
		name        string
		response    string // message.id of call-a
		sibling     *Entry
		branch      []string
		open        []string
		hasBranches bool
	}{{
		name:     "call of the same response stays parallel",
		response: "msg_1",
		sibling:  claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
		branch:   []string{"root", "call-a", "call-b", "result-a"},
		open:     []string{"b"},
	}, {
		name:        "call from another response forks",
		response:    "msg_1",
		sibling:     claudeAssistant("other", "call-a", "msg_other", toolUseContent("x")),
		branch:      []string{"root", "call-a", "result-a"},
		hasBranches: true,
	}, {
		name:     "call without a response id stays parallel",
		response: "msg_1",
		sibling:  claudeAssistant("call-b", "call-a", "", toolUseContent("b")),
		branch:   []string{"root", "call-a", "call-b", "result-a"},
		open:     []string{"b"},
	}, {
		name:     "call under a parent without a response id stays parallel",
		response: "",
		sibling:  claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
		branch:   []string{"root", "call-a", "call-b", "result-a"},
		open:     []string{"b"},
	}, {
		name:        "text-only row is not a call",
		response:    "msg_1",
		sibling:     claudeAssistant("error", "call-a", "", textContent("API Error")),
		branch:      []string{"root", "call-a", "result-a"},
		hasBranches: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dag := BuildDag(claudeRows(
				claudeUser("root", "", `"go"`),
				claudeAssistant("call-a", "root", tt.response, toolUseContent("a")),
				tt.sibling,
				claudeUser("result-a", "call-a", toolResultContent("a")),
			))
			assertActiveBranch(t, dag, tt.branch)
			assertOpenToolUses(t, dag, tt.open...)
			if dag.HasBranches != tt.hasBranches {
				t.Fatalf("HasBranches = %v, want %v", dag.HasBranches, tt.hasBranches)
			}
		})
	}
}

// TestBuildDagAbandonedResponseCalls pins how long a call of a selected
// response that has no result anywhere stays in the conversation. While the
// response is current the call may still be running, so it stays open even
// after its sibling's result gains a child or carries queued user text. Once
// the selected branch has a later user prompt or a later row of another
// response, Claude Code abandoned the call and it leaves the conversation. A
// row without a call is never abandoned.
func TestBuildDagAbandonedResponseCalls(t *testing.T) {
	tests := []struct {
		name   string
		rows   []*Entry
		branch []string
		open   []string
	}{{
		name: "sole call forked away by the next prompt",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("text", "prompt", "msg_1", textContent("checking")),
			claudeAssistant("call-b", "text", "msg_1", toolUseContent("b")),
			claudeUser("prompt2", "text", `"again"`),
			claudeAssistant("answer", "prompt2", "msg_2", textContent("done")),
		},
		branch: []string{"prompt", "text", "prompt2", "answer"},
	}, {
		name: "parallel call left behind by the next prompt",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeUser("result-a", "call-a", toolResultContent("a")),
			claudeUser("prompt2", "result-a", `"again"`),
		},
		branch: []string{"prompt", "call-a", "result-a", "prompt2"},
	}, {
		name: "parallel call left behind by the next response",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeUser("result-a", "call-a", toolResultContent("a")),
			claudeAssistant("answer", "result-a", "msg_2", textContent("done")),
		},
		branch: []string{"prompt", "call-a", "result-a", "answer"},
	}, {
		name: "running call beside a result with an attachment",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeUser("result-a", "call-a", toolResultContent("a")),
			{UUID: "attach-a", ParentUUID: "result-a", Type: "attachment"},
		},
		branch: []string{"prompt", "call-a", "call-b", "result-a", "attach-a"},
		open:   []string{"b"},
	}, {
		name: "running call beside a result carrying queued text",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeUser("result-a", "call-a", `[{"type":"tool_result","tool_use_id":"a","content":"ok"},{"type":"text","text":"also check y"}]`),
		},
		branch: []string{"prompt", "call-a", "call-b", "result-a"},
		open:   []string{"b"},
	}, {
		name: "running call beside a later row of its response",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeAssistant("call-c", "call-a", "msg_1", toolUseContent("c")),
		},
		branch: []string{"prompt", "call-a", "call-b", "call-c"},
		open:   []string{"a", "b", "c"},
	}, {
		name: "row of the response without a call stays after the next prompt",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("note", "call-a", "msg_1", textContent("running")),
			claudeUser("result-a", "call-a", toolResultContent("a")),
			claudeUser("prompt2", "result-a", `"again"`),
		},
		branch: []string{"prompt", "call-a", "note", "result-a", "prompt2"},
	}, {
		// Dropping the row would also drop the result of its answered call.
		name: "row with an answered call keeps its result",
		rows: []*Entry{
			claudeUser("prompt", "", `"go"`),
			claudeAssistant("call-a", "prompt", "msg_1", toolUseContent("a")),
			claudeAssistant("call-bc", "call-a", "msg_1", toolUseContent("b", "c")),
			claudeUser("result-b", "call-bc", toolResultContent("b")),
			claudeUser("result-a", "call-a", toolResultContent("a")),
			claudeUser("prompt2", "result-a", `"again"`),
		},
		branch: []string{"prompt", "call-a", "call-bc", "result-b", "result-a", "prompt2"},
		open:   []string{"c"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dag := BuildDag(claudeRows(tt.rows...))
			assertActiveBranch(t, dag, tt.branch)
			assertOpenToolUses(t, dag, tt.open...)
		})
	}
}

// TestBuildDagRetainsOnlyAttachedResults pins which sibling rows of a selected
// tool call count as its result attachment. Each row sits where the earlier
// call's result would, between the later call and that call's result.
func TestBuildDagRetainsOnlyAttachedResults(t *testing.T) {
	tests := []struct {
		name   string
		row    *Entry
		branch []string
		open   []string
	}{{
		name:   "result carrying user text stays out",
		row:    claudeUser("mixed-a", "call-a", `[{"type":"tool_result","tool_use_id":"a","content":"ok"},{"type":"text","text":"queued"}]`),
		branch: []string{"root", "call-a", "call-b", "result-b", "answer"},
	}, {
		name:   "result for a call the parent never made stays out",
		row:    claudeUser("foreign", "call-a", toolResultContent("z")),
		branch: []string{"root", "call-a", "call-b", "result-b", "answer"},
		open:   []string{"a"},
	}, {
		name:   "block-less user row keyed by tool id stays out",
		row:    &Entry{UUID: "user-a", ParentUUID: "call-a", Type: "user", ToolUseID: "a"},
		branch: []string{"root", "call-a", "call-b", "result-b", "answer"},
		open:   []string{"a"},
	}, {
		name:   "block-less tool_result row keyed by tool id is retained",
		row:    &Entry{UUID: "result-a", ParentUUID: "call-a", Type: "tool_result", ToolUseID: "a"},
		branch: []string{"root", "call-a", "call-b", "result-a", "result-b", "answer"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dag := BuildDag(claudeRows(
				claudeUser("root", "", `"go"`),
				claudeAssistant("call-a", "root", "msg_1", toolUseContent("a")),
				claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
				tt.row,
				claudeUser("result-b", "call-b", toolResultContent("b")),
				claudeAssistant("answer", "result-b", "msg_2", textContent("done")),
			))
			assertActiveBranch(t, dag, tt.branch)
			assertOpenToolUses(t, dag, tt.open...)
		})
	}
}

// TestBuildDagRetainsRecordedResultRows pins that a result is retained only
// from the row recorded for its UUID, and only when recorded after its parent.
func TestBuildDagRetainsRecordedResultRows(t *testing.T) {
	t.Run("row recorded before its parent stays out", func(t *testing.T) {
		dag := BuildDag(claudeRows(
			claudeUser("root", "", `"go"`),
			claudeUser("early-a", "call-a", toolResultContent("a")),
			claudeAssistant("call-a", "root", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			claudeUser("result-b", "call-b", toolResultContent("b")),
			claudeAssistant("answer", "result-b", "msg_2", textContent("done")),
		))
		assertActiveBranch(t, dag, []string{"root", "call-a", "call-b", "result-b", "answer"})
	})
	t.Run("superseded copy of a row stays out", func(t *testing.T) {
		partial := claudeUser("result-a", "call-a", `[{"type":"tool_result","tool_use_id":"a","content":"par"}]`)
		recorded := claudeUser("result-a", "call-a", toolResultContent("a"))
		dag := BuildDag(claudeRows(
			claudeUser("root", "", `"go"`),
			claudeAssistant("call-a", "root", "msg_1", toolUseContent("a")),
			claudeAssistant("call-b", "call-a", "msg_1", toolUseContent("b")),
			partial,
			recorded,
			claudeUser("result-b", "call-b", toolResultContent("b")),
			claudeAssistant("answer", "result-b", "msg_2", textContent("done")),
		))
		assertActiveBranch(t, dag, []string{"root", "call-a", "call-b", "result-a", "result-b", "answer"})
		if slices.Contains(dag.ActiveBranch, partial) || !slices.Contains(dag.ActiveBranch, recorded) {
			t.Fatal("active branch retained the superseded copy of result-a")
		}
	})
}

// claudeRows stamps rows one second apart in file order, as Claude Code
// appends them.
func claudeRows(rows ...*Entry) []*Entry {
	for i, row := range rows {
		row.Timestamp = time.Unix(int64(i), 0)
	}
	return rows
}

func claudeUser(uuid, parent, content string) *Entry {
	return &Entry{UUID: uuid, ParentUUID: parent, Type: "user", Message: json.RawMessage(`{"role":"user","content":` + content + `}`)}
}

func claudeAssistant(uuid, parent, response, content string) *Entry {
	return &Entry{UUID: uuid, ParentUUID: parent, Type: "assistant", Message: json.RawMessage(`{"role":"assistant","id":"` + response + `","content":` + content + `}`)}
}

func toolUseContent(ids ...string) string {
	blocks := make([]string, len(ids))
	for i, id := range ids {
		blocks[i] = `{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}`
	}
	return "[" + strings.Join(blocks, ",") + "]"
}

func toolResultContent(id string) string {
	return `[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]`
}

func textContent(text string) string {
	return `[{"type":"text","text":"` + text + `"}]`
}

func assertActiveBranch(t *testing.T, dag *DagResult, want []string) {
	t.Helper()
	var got []string
	for _, e := range dag.ActiveBranch {
		got = append(got, e.UUID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
}

func assertOpenToolUses(t *testing.T, dag *DagResult, want ...string) {
	t.Helper()
	if got := slices.Sorted(maps.Keys(dag.OrphanedToolUseIDs)); !slices.Equal(got, want) {
		t.Fatalf("open tool calls = %v, want %v", got, want)
	}
}
