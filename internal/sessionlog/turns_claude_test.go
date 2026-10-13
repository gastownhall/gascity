package sessionlog

import (
	"errors"
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
		{"interrupt_same_prompt.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:02.345")), Status: TurnStatusCancelled},
			}}
		}},
		{"api_error_last.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: claudeP2, StartedAt: turnTime(t, ts("00:02.250")), EndedAt: turnTime(t, ts("00:14.562")), Status: TurnStatusFailed},
				{ID: claudeP1, StartedAt: turnTime(t, ts("00:00.123")), EndedAt: turnTime(t, ts("00:01.500")), Status: TurnStatusCompleted},
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

// TestScanTurnsClaudeAPIErrorThenTurnEnd: an API error followed by the
// turn's turn_duration ends the turn failed at the turn_duration; an API
// error the turn recovers from ends nothing.
func TestScanTurnsClaudeAPIErrorThenTurnEnd(t *testing.T) {
	user := func(ts, id string) string {
		return `{"type":"user","timestamp":"2026-10-13T05:` + ts + `Z","promptId":"` + id + `","message":{"role":"user","content":"prompt"}}`
	}
	apiErr := func(ts string) string {
		return `{"type":"assistant","timestamp":"2026-10-13T05:` + ts + `Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`
	}
	end := func(ts string) string {
		return `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:` + ts + `Z"}`
	}
	path := writeTurnLines(t, "claude.jsonl",
		user("00:00.100", "p1"), apiErr("00:01.000"), end("00:01.010"),
		user("00:02.100", "p2"), apiErr("00:03.000"), user("00:04.000", "p2"), end("00:05.000"),
	)
	got, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	ts := func(clock string) string { return "2026-10-13T05:" + clock + "Z" }
	assertTurnScan(t, got, TurnScan{Recent: []ScannedTurn{
		{ID: "p2", StartedAt: turnTime(t, ts("00:02.100")), EndedAt: turnTime(t, ts("00:05.000")), Status: TurnStatusCompleted},
		{ID: "p1", StartedAt: turnTime(t, ts("00:00.100")), EndedAt: turnTime(t, ts("00:01.010")), Status: TurnStatusFailed},
	}})
}
