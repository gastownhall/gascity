package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
// marker, the queued prompt opens the next turn. A window that begins at the tool result stamped with
// the queued prompt's id first opens the interrupted turn under that id; the
// queued prompt then shows the id was not that turn's, so the interrupted
// turn keeps no id and the prompt opens its own turn.
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
	for first, recent := range map[int][]ScannedTurn{
		2: interrupted(claudeP1),
		3: interrupted(claudeP1),
		4: interrupted(""),
		5: interrupted(""),
		6: interrupted(""),
		7: nil,
	} {
		// The window holds line first onward; the budget also covers the
		// newline before it, so the line reads whole.
		budget := int64(len(strings.Join(lines[first-1:], ""))) + 1
		got, err := scanTurnsWithBudget(t, "claude", path, budget)
		if err != nil {
			t.Fatalf("window from line %d: %v", first, err)
		}
		want := TurnScan{
			Open:    true,
			Current: ScannedTurn{ID: claudeP2, StartedAt: turnTime(t, ts("00:10.050"))},
			Recent:  recent,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("window from line %d:\n got: %+v\nwant: %+v", first, got, want)
		}
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
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","message":{"role":"user","content":"prompt"}}`
	}
	apiErr := func(ts string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`
	}
	reply := func(ts string, sidechain bool) string {
		return fmt.Sprintf(`{"type":"assistant","isSidechain":%v,"timestamp":"2026-10-13T05:%sZ","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`, sidechain, ts)
	}
	end := func(ts string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:` + ts + `Z"}`
	}
	path := writeTurnLines(t, "claude.jsonl",
		user("00:00.100", "p1"), apiErr("00:01.000"), end("00:01.010"),
		user("00:02.100", "p2"), apiErr("00:03.000"), user("00:04.000", "p2"), end("00:05.000"),
		user("00:06.100", "p3"), apiErr("00:07.000"), reply("00:08.000", false), end("00:09.000"),
		user("00:10.100", "p4"), apiErr("00:11.000"), reply("00:11.500", true), end("00:12.000"),
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
			{ID: "p5", StartedAt: turnTime(t, ts("00:13.100")), EndedAt: turnTime(t, ts("00:14.000")), Status: TurnStatusFailed},
			{ID: "p4", StartedAt: turnTime(t, ts("00:10.100")), EndedAt: turnTime(t, ts("00:12.000")), Status: TurnStatusFailed},
			{ID: "p3", StartedAt: turnTime(t, ts("00:06.100")), EndedAt: turnTime(t, ts("00:09.000")), Status: TurnStatusCompleted},
			{ID: "p2", StartedAt: turnTime(t, ts("00:02.100")), EndedAt: turnTime(t, ts("00:05.000")), Status: TurnStatusCompleted},
			{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:01.010")), Status: TurnStatusFailed},
		},
	})
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
