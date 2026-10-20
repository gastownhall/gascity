package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	claudeP1 = "7f3e9a10-1111-4aaa-8000-000000000001"
	claudeP2 = "7f3e9a10-2222-4aaa-8000-000000000002"
)

func TestScanTurnsClaudeFixtures(t *testing.T) {
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	tests := []struct {
		name string
		want func(t *testing.T) TurnScan
	}{
		{"completed.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP2, StartedAt: turnTime(t, ts("01:00.789")), EndedAt: turnTime(t, ts("01:03.001")), Status: TurnStatusCompleted},
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:05.456")), Status: TurnStatusCompleted},
			}}
		}},
		{"stop_hook_continuation.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:03.500")), Status: TurnStatusCompleted},
			}}
		}},
		{"interrupt_next_prompt.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:10.050"))},
				Recent: []ScannedTurn{
					{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:10.000")), Status: TurnStatusCancelled},
				},
			}
		}},
		// Input queued while a tool ran, then an interrupt: Claude stamps the
		// in-flight tool result, the marker and the queued prompt with the
		// queued prompt's promptId. The tool result continues the
		// interrupted turn; the prompt opens the next one.
		{"interrupt_queued_tool_result.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:10.050"))},
				Recent: []ScannedTurn{
					{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:10.000")), Status: TurnStatusCancelled},
				},
			}
		}},
		// A scheduled task fires while a tool runs: the in-flight tool
		// result carries the scheduled prompt's promptId, but the running
		// turn completes and the scheduled prompt opens its own turn.
		{"scheduled_task_mid_tool.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:08.285"))},
				Recent: []ScannedTurn{
					{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:08.233")), Status: TurnStatusCompleted},
				},
			}
		}},
		{"interrupt_same_prompt.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:02.345")), Status: TurnStatusCancelled},
			}}
		}},
		// An API error alone ends nothing: Claude Code retries some errors
		// within the turn. The turn ends failed at its turn_duration or when
		// a new prompt starts; a CLI that dies after the error leaves the end
		// to the idle overlay (TI-2b).
		{"api_error_last.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:02.250"))},
				Recent: []ScannedTurn{
					{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:01.500")), Status: TurnStatusCompleted},
				},
			}
		}},
		{"api_error_mid_response.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:08.010")), Status: TurnStatusCompleted},
			}}
		}},
		{"sidechain.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Open: true, Current: ScannedTurn{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123"))}}
		}},
		{"compaction_mid_turn.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:06.789")), Status: TurnStatusCompleted},
			}}
		}},
		// A session continued into a new file after it ran out of context
		// opens inside the running turn: the compaction summary, then that
		// turn's tool results, none carrying a promptId.
		{"continued_session.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Recent: []ScannedTurn{
					{ID: claudeP2, StartedAt: turnTime(t, "2026-10-13T05:01:00.459Z"), EndedAt: turnTime(t, "2026-10-13T05:01:01.500Z"), Status: TurnStatusCompleted},
					{EndedAt: turnTime(t, "2026-10-13T05:00:05.025Z"), Status: TurnStatusFailed, Failure: TurnFailure{Code: "server_error", Status: 529, Message: "API Error: overloaded"}},
				},
				MissingIDs: true,
			}
		}},
		{"start_beyond_budget.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:02.250"))},
				Recent: []ScannedTurn{
					{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:01.500")), Status: TurnStatusCompleted},
				},
			}
		}},
		{"no_prompt_id.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:       true,
				Current:    ScannedTurn{StartedAt: turnTime(t, ts("00:04.678"))},
				Recent:     []ScannedTurn{{StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:03.500")), Status: TurnStatusCompleted}},
				MissingIDs: true,
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ScanTurns("claude", turnFixture("claude", tt.name))
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			assertTurnScan(t, got, tt.want(t))
		})
	}
}

func TestScanTurnsClaudeTornTailIsAnError(t *testing.T) {
	_, err := ScanTurns("claude", turnFixture("claude", "torn_tail.jsonl"))
	if !errors.Is(err, ErrTornTurnRecord) {
		t.Fatalf("error = %v, want ErrTornTurnRecord", err)
	}
}

// TestScanTurnsClaudeContinuedSessionOpensMidTurn: a session continued into
// a new file after it ran out of context opens with the compaction summary
// and the running turn's tool results, with no prompt. Read whole, the file
// still begins inside that turn, so its first tool result opens it with an
// unknown start, and the turn's end is kept. The compaction summary opens
// nothing.
func TestScanTurnsClaudeContinuedSessionOpensMidTurn(t *testing.T) {
	data, err := os.ReadFile(turnFixture("claude", "continued_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	running := TurnScan{Open: true, MissingIDs: true}
	failed := ScannedTurn{EndedAt: turnTime(t, ts("00:05.025")), Status: TurnStatusFailed, Failure: TurnFailure{Code: "server_error", Status: 529, Message: "API Error: overloaded"}}
	tests := []struct {
		lines int
		want  TurnScan
	}{
		{3, TurnScan{}}, // through the compaction summary
		{4, TurnScan{}}, // the running turn's tool use
		{5, running},    // its first tool result
		{8, running},    // the API error leaves it open
		{9, TurnScan{Recent: []ScannedTurn{failed}, MissingIDs: true}},
		{10, TurnScan{
			Open:       true,
			Current:    ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("01:00.459"))},
			Recent:     []ScannedTurn{failed},
			MissingIDs: true,
		}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("lines=%d", tt.lines), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "claude.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(lines[:tt.lines], "")), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ScanTurns("claude", path)
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			assertTurnScan(t, got, tt.want)
		})
	}
}

// TestScanTurnsClaudeStartBeyondBudget: the open turn's id is still known
// from its tool results when its prompt lies beyond the budget, but its start
// time is not; a window holding no turn record is an error, never idle.
func TestScanTurnsClaudeStartBeyondBudget(t *testing.T) {
	path := turnFixture("claude", "start_beyond_budget.jsonl")
	got, err := scanTurnsWithBudget(t, "claude", path, 4096)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertTurnScan(t, got, TurnScan{Open: true, Current: ScannedTurn{ID: claudeP2}})

	if _, err := scanTurnsWithBudget(t, "claude", path, 1024); !errors.Is(err, ErrTurnStartBeyondBudget) {
		t.Fatalf("scan with no record in budget: error = %v, want ErrTurnStartBeyondBudget", err)
	}
}

// TestScanTurnsClaudeWindowStartsAtMisstampedToolResult: wherever the scan
// window begins in a turn interrupted with input queued, up to the interrupt
// marker, the queued prompt opens the next turn. A window that begins at the
// tool result stamped with the queued prompt's id first opens the interrupted
// turn under that id; the queued prompt then shows the id was not that
// turn's, so the interrupted turn keeps no id and the prompt opens its own
// turn. A window that begins at the marker reads only the interrupted turn's
// end, which records no id of it, so that turn has none and MissingIDs is
// set.
func TestScanTurnsClaudeWindowStartsAtMisstampedToolResult(t *testing.T) {
	path := turnFixture("claude", "interrupt_queued_tool_result.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	interrupted := func(id string) []ScannedTurn {
		return []ScannedTurn{{ID: id, EndedAt: turnTime(t, ts("00:10.000")), Status: TurnStatusCancelled}}
	}
	for first, tt := range map[int]struct {
		recent     []ScannedTurn
		missingIDs bool
	}{
		2: {interrupted(claudeP1), false},
		3: {interrupted(claudeP1), false},
		4: {interrupted(""), false},
		5: {interrupted(""), false},
		6: {interrupted(""), false},
		7: {interrupted(""), true},
	} {
		// The window holds line first onward; the budget also covers the
		// newline before it, so the line reads whole.
		budget := int64(len(strings.Join(lines[first-1:], ""))) + 1
		got, err := scanTurnsWithBudget(t, "claude", path, budget)
		if err != nil {
			t.Fatalf("window from line %d: %v", first, err)
		}
		want := TurnScan{
			Open:       true,
			Current:    ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:10.050"))},
			Recent:     tt.recent,
			MissingIDs: tt.missingIDs,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("window from line %d:\n got: %+v\nwant: %+v", first, got, want)
		}
	}
}

// TestScanTurnsClaudeEndBeforeFirstStart: a fold that begins inside a turn
// and reads that turn's end, a turn_duration or an interrupt, before any turn
// start records the end in Recent as a turn with neither start nor id, and
// sets MissingIDs. An API error pending at the turn_duration makes the turn
// failed; a tool result after the error opens the turn and clears it. The
// fold begins inside the turn in a scan window cut after the turn's prompt,
// and in a file that begins mid-turn, as a continued session's does. Read
// whole from the prompt, the file reports the same turns, the oldest with
// the prompt's id and start.
func TestScanTurnsClaudeEndBeforeFirstStart(t *testing.T) {
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	prompt := func(clock, id string) string {
		return `{"type":"user","timestamp":"` + ts(clock) + `","promptId":"` + id + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"prompt"}}`
	}
	reply := func(clock string) string {
		return `{"type":"assistant","timestamp":"` + ts(clock) + `","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`
	}
	apiErr := func(clock string) string {
		return `{"type":"assistant","timestamp":"` + ts(clock) + `","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`
	}
	toolResult := func(clock, id string) string {
		return `{"type":"user","timestamp":"` + ts(clock) + `","promptId":"` + id + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]}}`
	}
	interrupt := func(clock, id string) string {
		return `{"type":"user","timestamp":"` + ts(clock) + `","promptId":"` + id + `","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	}
	end := func(clock string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"` + ts(clock) + `"}`
	}
	ended := func(id, clock string, status TurnStatus) []ScannedTurn {
		var failure TurnFailure
		if status == TurnStatusFailed {
			failure.Message = "API Error"
		}
		return []ScannedTurn{{ID: id, EndedAt: turnTime(t, ts(clock)), Status: status, Failure: failure}}
	}
	tests := []struct {
		name string
		// rest is what follows the turn's prompt, p1 at 00:00.100.
		rest []string
		want TurnScan
	}{
		{
			"completed",
			[]string{reply("00:01.000"), reply("00:02.000"), end("00:03.000")},
			TurnScan{Recent: ended("", "00:03.000", TurnStatusCompleted), MissingIDs: true},
		},
		{
			"failed",
			[]string{reply("00:01.000"), apiErr("00:02.000"), end("00:03.000")},
			TurnScan{Recent: ended("", "00:03.000", TurnStatusFailed), MissingIDs: true},
		},
		{
			"canceled",
			[]string{reply("00:01.000"), interrupt("00:02.000", claudeP2)},
			TurnScan{Recent: ended("", "00:02.000", TurnStatusCancelled), MissingIDs: true},
		},
		{
			"next prompt",
			[]string{reply("00:01.000"), end("00:02.000"), prompt("00:03.000", claudeP2)},
			TurnScan{
				Open:       true,
				Current:    ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:03.000"))},
				Recent:     ended("", "00:02.000", TurnStatusCompleted),
				MissingIDs: true,
			},
		},
		{
			"tool result clears the error",
			[]string{apiErr("00:01.000"), toolResult("00:02.000", claudeP1), end("00:03.000")},
			TurnScan{Recent: ended(claudeP1, "00:03.000", TurnStatusCompleted)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTurnLines(t, "claude.jsonl", append([]string{prompt("00:00.100", claudeP1)}, tt.rest...)...)
			// The window holds the lines after the prompt, and the newline
			// before them.
			got, err := scanTurnsWithBudget(t, "claude", path, int64(len(strings.Join(tt.rest, "\n")))+2)
			if err != nil {
				t.Fatalf("window after the prompt: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("window after the prompt:\n got: %+v\nwant: %+v", got, tt.want)
			}

			whole := tt.want
			whole.Recent = slices.Clone(tt.want.Recent)
			oldest := &whole.Recent[len(whole.Recent)-1]
			oldest.ID, oldest.StartedAt = claudeP1, turnTime(t, ts("00:00.100"))
			whole.MissingIDs = false
			if got, err := ScanTurns("claude", path); err != nil || !reflect.DeepEqual(got, whole) {
				t.Errorf("whole file: error = %v\n got: %+v\nwant: %+v", err, got, whole)
			}

			// A continued session's file opens with its compaction summary,
			// which opens no turn. A follower folds the rest line by line.
			path = writeTurnLines(t, "continued.jsonl", `{"type":"user","isCompactSummary":true,"timestamp":"`+ts("00:00.050")+`","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context."}}`)
			f, err := NewTurnFollower("claude", path)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range tt.rest {
				appendTurnBytes(t, path, line+"\n")
				assertFollowerMatchesScan(t, fmt.Sprintf("continued session, line %d", i+2), f, "claude", path)
			}
			if got, err := ScanTurns("claude", path); err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("continued session: error = %v\n got: %+v\nwant: %+v", err, got, tt.want)
			}
		})
	}
}

func TestClaudeInterruptEntry(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{`{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}`, true},
		{`{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}`, true},
		{`{"role":"user","content":"[Request interrupted by user]"}`, true},
		{`"{\"role\":\"user\",\"content\":\"[Request interrupted by user]\"}"`, true},
		{`{"role":"user","content":"please see [Request interrupted by user] in the log"}`, false},
		{`{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"},{"type":"text","text":"and more"}]}`, false},
		{`{"role":"user","content":[{"type":"tool_result","content":"[Request interrupted by user for tool use]"}]}`, false},
		{`{"role":"user","content":"prompt one"}`, false},
	}
	for _, tt := range tests {
		if got := claudeInterruptEntry([]byte(tt.message)); got != tt.want {
			t.Errorf("claudeInterruptEntry(%s) = %v, want %v", tt.message, got, tt.want)
		}
	}
}

// TestScanTurnsClaudeOverlongPromptIDIsNotAnIdentity pins X11 D2: an id over
// MaxTurnIDLen downgrades the transcript rather than being cut.
func TestScanTurnsClaudeOverlongPromptIDIsNotAnIdentity(t *testing.T) {
	long := make([]byte, MaxTurnIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	path := writeTurnLines(t, "claude.jsonl",
		`{"type":"user","timestamp":"2026-10-13T05:00:00.123Z","promptId":"`+string(long)+`","message":{"role":"user","content":"prompt one"}}`,
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	assertTurnScan(t, got, TurnScan{Open: true, Current: ScannedTurn{StartedAt: turnTime(t, "2026-10-13T05:00:00.123Z")}, MissingIDs: true})
}

// TestScanTurnsClaudeMidResponseRetryStaysOpen: Claude Code writes "API
// Error: Connection lost mid-response" and then continues the same turn, so
// every prefix of the retried turn reads it open until its turn_duration.
func TestScanTurnsClaudeMidResponseRetryStaysOpen(t *testing.T) {
	data, err := os.ReadFile(turnFixture("claude", "api_error_mid_response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
	want := TurnScan{Open: true, Current: ScannedTurn{ID: claudeP1, StartedAt: turnTime(t, "2026-10-13T05:00:00.123Z")}}
	for n := 1; n < len(lines); n++ {
		path := filepath.Join(t.TempDir(), "claude.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "")), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ScanTurns("claude", path)
		if err != nil {
			t.Fatalf("after line %d: ScanTurns: %v", n, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after line %d:\n got: %+v\nwant: %+v", n, got, want)
		}
	}
}

// TestScanTurnsClaudeAPIErrorThenTurnEnd: an API error the turn does not
// recover from ends the turn failed, at its turn_duration or, when a new
// prompt comes first, at the error. A later main-chain reply or tool result
// means the turn recovered, and it ends as its turn_duration says.
func TestScanTurnsClaudeAPIErrorThenTurnEnd(t *testing.T) {
	user := func(ts, id string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","promptSource":"typed","message":{"role":"user","content":"prompt"}}`
	}
	apiErr := func(ts string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`
	}
	reply := func(ts string, sidechain bool) string {
		return fmt.Sprintf(`{"type":"assistant","isSidechain":%v,"timestamp":"2026-10-13T05:%sZ","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`, sidechain, ts)
	}
	toolResult := func(ts, id string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]}}`
	}
	end := func(ts string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:` + ts + `Z"}`
	}
	path := writeTurnLines(t, "claude.jsonl",
		user("00:00.100", "p1"), apiErr("00:01.000"), end("00:01.010"),
		user("00:02.100", "p2"), apiErr("00:03.000"), user("00:04.000", "p2"), end("00:05.000"),
		user("00:06.100", "p3"), apiErr("00:07.000"), reply("00:08.000", false), end("00:09.000"),
		user("00:10.100", "p4"), apiErr("00:11.000"), reply("00:11.500", true), end("00:12.000"),
		user("00:12.100", "p7"), apiErr("00:12.300"), toolResult("00:12.500", "p7"), end("00:12.900"),
		user("00:13.100", "p5"), apiErr("00:14.000"), user("00:15.100", "p6"),
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	assertTurnScan(t, got, TurnScan{
		Open:    true,
		Current: ScannedTurn{ID: "p6", StartedAt: turnTime(t, ts("00:15.100"))},
		Recent: []ScannedTurn{
			{ID: "p5", StartedAt: turnTime(t, ts("00:13.100")), EndedAt: turnTime(t, ts("00:14.000")), Status: TurnStatusFailed, Failure: TurnFailure{Message: "API Error"}},
			{ID: "p7", StartedAt: turnTime(t, ts("00:12.100")), EndedAt: turnTime(t, ts("00:12.900")), Status: TurnStatusCompleted},
			{ID: "p4", StartedAt: turnTime(t, ts("00:10.100")), EndedAt: turnTime(t, ts("00:12.000")), Status: TurnStatusFailed, Failure: TurnFailure{Message: "API Error"}},
			{ID: "p3", StartedAt: turnTime(t, ts("00:06.100")), EndedAt: turnTime(t, ts("00:09.000")), Status: TurnStatusCompleted},
			{ID: "p2", StartedAt: turnTime(t, ts("00:02.100")), EndedAt: turnTime(t, ts("00:05.000")), Status: TurnStatusCompleted},
			{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:01.010")), Status: TurnStatusFailed, Failure: TurnFailure{Message: "API Error"}},
		},
	})
}

// TestScanTurnsClaudeUnpromptedEntryContinuesOpenTurn: a user entry Claude
// writes itself, with neither origin nor promptSource, may carry a queued
// prompt's promptId before that prompt is written: Stop hook feedback, or the
// entry a resumed session opens with. While a turn is open such an entry
// continues it, so the queued prompt still opens its own turn. With no turn
// open, it opens one. A local slash command or bash-mode input opens one too,
// though the model never runs for it: the open p4 below is that known limit,
// left to the caller (TurnScan.Open), not a turn the agent runs.
func TestScanTurnsClaudeUnpromptedEntryContinuesOpenTurn(t *testing.T) {
	prompt := func(ts, id string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","origin":{"kind":"human"},"promptSource":"queued","message":{"role":"user","content":"prompt"}}`
	}
	meta := func(ts, id, text string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","isMeta":true,"message":{"role":"user","content":"` + text + `"}}`
	}
	apiErr := func(ts string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`
	}
	end := func(ts string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:` + ts + `Z"}`
	}
	path := writeTurnLines(t, "claude.jsonl",
		prompt("00:00.100", "p1"), meta("00:01.000", "p2", "Stop hook feedback: hook text"), end("00:01.010"),
		prompt("00:01.100", "p2"), apiErr("00:02.000"),
		meta("00:03.000", "p3", "Continue from where you left off."), prompt("00:03.050", "p3"), end("00:04.000"),
		meta("00:05.000", "p4", "local command caveat"), meta("00:05.000", "p4", "local command output"),
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	assertTurnScan(t, got, TurnScan{
		Open:    true,
		Current: ScannedTurn{ID: "p4", StartedAt: turnTime(t, ts("00:05.000"))},
		Recent: []ScannedTurn{
			{ID: "p3", StartedAt: turnTime(t, ts("00:03.050")), EndedAt: turnTime(t, ts("00:04.000")), Status: TurnStatusCompleted},
			{ID: "p2", StartedAt: turnTime(t, ts("00:01.100")), EndedAt: turnTime(t, ts("00:02.000")), Status: TurnStatusFailed, Failure: TurnFailure{Message: "API Error"}},
			{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:01.010")), Status: TurnStatusCompleted},
		},
	})
}

// TestScanTurnsClaudeWindowStartsAtUnpromptedEntry: a scan window that
// begins at Stop hook feedback stamped with the queued prompt's id opens the
// ending turn under that id; the queued prompt then shows the id was its own,
// so the ended turn keeps no id and the prompt opens its own turn.
func TestScanTurnsClaudeWindowStartsAtUnpromptedEntry(t *testing.T) {
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	lines := []string{
		`{"type":"user","timestamp":"` + ts("00:00.100") + `","promptId":"p1","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"prompt"}}`,
		`{"type":"user","timestamp":"` + ts("00:01.000") + `","promptId":"p1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]}}`,
		`{"type":"user","timestamp":"` + ts("00:02.000") + `","promptId":"p2","isMeta":true,"message":{"role":"user","content":"Stop hook feedback: hook text"}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"` + ts("00:02.010") + `"}`,
		`{"type":"user","timestamp":"` + ts("00:02.100") + `","promptId":"p2","origin":{"kind":"human"},"promptSource":"queued","message":{"role":"user","content":"prompt"}}`,
	}
	path := writeTurnLines(t, "claude.jsonl", lines...)
	ended := func(id string) []ScannedTurn {
		return []ScannedTurn{{ID: id, EndedAt: turnTime(t, ts("00:02.010")), Status: TurnStatusCompleted}}
	}
	for first, recent := range map[int][]ScannedTurn{
		2: ended("p1"),
		3: ended(""),
	} {
		// The window holds line first onward, and the newline before it.
		budget := int64(len(strings.Join(lines[first-1:], "\n"))) + 2
		got, err := scanTurnsWithBudget(t, "claude", path, budget)
		if err != nil {
			t.Fatalf("window from line %d: %v", first, err)
		}
		want := TurnScan{
			Open:    true,
			Current: ScannedTurn{ID: "p2", StartedAt: turnTime(t, ts("00:02.100"))},
			Recent:  recent,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("window from line %d:\n got: %+v\nwant: %+v", first, got, want)
		}
	}
}

// TestScanTurnsClaudePromptOfMarkerTextIsAPrompt: Claude's interrupt marker
// carries neither origin nor promptSource, and every prompt carries them, so
// a delivered prompt whose whole text is the marker opens its own turn.
func TestScanTurnsClaudePromptOfMarkerTextIsAPrompt(t *testing.T) {
	markerText := `"content":[{"type":"text","text":"[Request interrupted by user]"}]`
	path := writeTurnLines(t, "claude.jsonl",
		`{"type":"user","timestamp":"2026-10-13T05:00:00.100Z","promptId":"p1","origin":{"kind":"human"},"promptSource":"prompt","message":{"role":"user","content":"prompt"}}`,
		`{"type":"user","timestamp":"2026-10-13T05:00:02.100Z","promptId":"p2","origin":{"kind":"human"},"promptSource":"prompt","message":{"role":"user",`+markerText+`}}`,
		`{"type":"user","timestamp":"2026-10-13T05:00:03.100Z","promptId":"p3","promptSource":"system","isMeta":true,"message":{"role":"user","content":"[Request interrupted by user]"}}`,
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	assertTurnScan(t, got, TurnScan{
		Open:    true,
		Current: ScannedTurn{ID: "p3", StartedAt: turnTime(t, ts("00:03.100"))},
		Recent: []ScannedTurn{
			{ID: "p2", StartedAt: turnTime(t, ts("00:02.100")), EndedAt: turnTime(t, ts("00:03.100"))},
			{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:02.100"))},
		},
	})
}

// TestScanTurnsClaudeFilterIgnoresSpacing: the type-name filter matches the
// JSON string alone, so a transcript written with spaces reads the same.
func TestScanTurnsClaudeFilterIgnoresSpacing(t *testing.T) {
	for _, name := range []string{"completed.jsonl", "interrupt_queued_tool_result.jsonl", "api_error_last.jsonl"} {
		src := turnFixture("claude", name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		spaced := strings.NewReplacer(`":`, `": `, `,"`, `, "`).Replace(string(data))
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(spaced), 0o600); err != nil {
			t.Fatal(err)
		}
		want, err := ScanTurns("claude", src)
		if err != nil {
			t.Fatalf("ScanTurns(%s): %v", name, err)
		}
		got, err := ScanTurns("claude", path)
		if err != nil {
			t.Fatalf("ScanTurns(spaced %s): %v", name, err)
		}
		assertTurnScan(t, got, want)
	}
}

// BenchmarkScanTurnsClaudeLongTurn scans a full 16 MiB window of one long
// turn: mostly attachments, with tool calls and results between them.
func BenchmarkScanTurnsClaudeLongTurn(b *testing.B) {
	attachment := `{"type":"attachment","timestamp":"2026-10-13T05:00:01.000Z","attachment":{"type":"file","content":"` + strings.Repeat("a", 16<<10) + `"}}`
	call := `{"type":"assistant","timestamp":"2026-10-13T05:00:01.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","input":{"command":"` + strings.Repeat("c", 2<<10) + `"}}]}}`
	result := `{"type":"user","timestamp":"2026-10-13T05:00:01.000Z","promptId":"p1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"` + strings.Repeat("r", 4<<10) + `"}]}}`
	lines := []string{`{"type":"user","timestamp":"2026-10-13T05:00:00.000Z","promptId":"p1","origin":{"kind":"human"},"promptSource":"prompt","message":{"role":"user","content":"prompt"}}`}
	for size := 0; size < turnScanBudget+(1<<20); size += len(attachment) + len(call) + len(result) {
		lines = append(lines, attachment, call, result)
	}
	path := filepath.Join(b.TempDir(), "claude.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := ScanTurns("claude", path); err != nil {
			b.Fatal(err)
		}
	}
}

// TestScanTurnsClaudeFailedTurnKeepsItsError: a turn that ends failed keeps
// the API error it ended on, as Claude Code classified it: the entry's error
// class, its HTTP status and its text. The last error pending when the turn
// ended is the one kept; a turn that recovered keeps none. RateLimited reads
// the class or the status, never the text (the 2026-10-19 GLM pool cooldown:
// "Request rejected (429) · pool cooling for another 3d 16h").
func TestScanTurnsClaudeFailedTurnKeepsItsError(t *testing.T) {
	user := func(ts, id string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","promptSource":"typed","message":{"role":"user","content":"prompt"}}`
	}
	apiErr := func(ts, extra, text string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","isApiErrorMessage":true` + extra + `,"message":{"role":"assistant","content":[{"type":"text","text":` + strconv.Quote(text) + `}]}}`
	}
	reply := func(ts string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`
	}
	end := func(ts string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:` + ts + `Z"}`
	}
	const cooling = "API Error: Request rejected (429) · pool cooling for another 3d 16h; every account is in a rate-limit cool-down, so retrying before then cannot succeed"
	long := strings.Repeat("é", MaxTurnFailureMessage) // two bytes a rune: cut on a rune boundary
	path := writeTurnLines(t, "claude.jsonl",
		user("00:00.100", "p1"), apiErr("00:01.000", `,"error":"server_error"`, "API Error: Connection lost mid-response."), apiErr("00:01.500", `,"error":"rate_limit","apiErrorStatus":429`, cooling), end("00:01.510"),
		user("00:02.100", "p2"), apiErr("00:03.000", `,"error":"server_error","apiErrorStatus":503`, "API Error: 503 status code (no body)"), end("00:03.010"),
		user("00:04.100", "p3"), apiErr("00:05.000", `,"error":"rate_limit","apiErrorStatus":429`, cooling), reply("00:06.000"), end("00:07.000"),
		user("00:08.100", "p4"), apiErr("00:09.000", `,"apiErrorStatus":429`, "API Error: 429"), user("00:10.100", "p5"),
		apiErr("00:11.000", `,"error":{"type":"rate_limit_error"},"apiErrorStatus":"429"`, long), end("00:11.010"),
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	cut := long[:MaxTurnFailureMessage]
	assertTurnScan(t, got, TurnScan{
		Recent: []ScannedTurn{
			{ID: "p5", StartedAt: turnTime(t, ts("00:10.100")), EndedAt: turnTime(t, ts("00:11.010")), Status: TurnStatusFailed, Failure: TurnFailure{Message: cut}},
			{ID: "p4", StartedAt: turnTime(t, ts("00:08.100")), EndedAt: turnTime(t, ts("00:09.000")), Status: TurnStatusFailed, Failure: TurnFailure{Status: 429, Message: "API Error: 429"}},
			{ID: "p3", StartedAt: turnTime(t, ts("00:04.100")), EndedAt: turnTime(t, ts("00:07.000")), Status: TurnStatusCompleted},
			{ID: "p2", StartedAt: turnTime(t, ts("00:02.100")), EndedAt: turnTime(t, ts("00:03.010")), Status: TurnStatusFailed, Failure: TurnFailure{Code: "server_error", Status: 503, Message: "API Error: 503 status code (no body)"}},
			{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:01.510")), Status: TurnStatusFailed, Failure: TurnFailure{Code: "rate_limit", Status: 429, Message: cooling}},
		},
	})
	for _, tc := range []struct {
		f    TurnFailure
		want bool
	}{
		{TurnFailure{Code: "rate_limit", Status: 429}, true},
		{TurnFailure{Code: "rate_limit"}, true},
		{TurnFailure{Status: 429}, true},
		{TurnFailure{Code: "server_error", Status: 503}, false},
		{TurnFailure{Message: "API Error: Request rejected (429)"}, false}, // the text is never parsed
		{TurnFailure{}, false},
	} {
		if got := tc.f.RateLimited(); got != tc.want {
			t.Errorf("%+v.RateLimited() = %v, want %v", tc.f, got, tc.want)
		}
	}
	if !utf8.ValidString(got.Recent[0].Failure.Message) {
		t.Errorf("message cut mid-rune: %q", got.Recent[0].Failure.Message)
	}
}
