package sessionlog

import (
	"errors"
	"testing"
)

const (
	codexT1 = "019a1111-0000-7000-8000-000000000001"
	codexT2 = "019a2222-0000-7000-8000-000000000002"
)

func TestScanTurnsCodexFixtures(t *testing.T) {
	ts := func(clock string) string { return "2026-10-08T02:" + clock + "Z" }
	tests := []struct {
		name string
		want func(t *testing.T) TurnScan
	}{
		{"started_complete.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: codexT2, StartedAt: turnTime(t, ts("23:01.007"))},
				Recent: []ScannedTurn{
					{ID: codexT1, StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:30.500")), Status: TurnStatusCompleted},
				},
			}
		}},
		{"aborted.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: codexT1, StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:27.401")), Status: TurnStatusCancelled},
			}}
		}},
		{"error.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{Recent: []ScannedTurn{
				{ID: codexT1, StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:24.999")), Status: TurnStatusFailed},
			}}
		}},
		{"missing_ids.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:       true,
				Current:    ScannedTurn{StartedAt: turnTime(t, ts("23:01.007"))},
				Recent:     []ScannedTurn{{StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:30.500")), Status: TurnStatusCompleted}},
				MissingIDs: true,
			}
		}},
		{"start_beyond_budget.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: codexT2, StartedAt: turnTime(t, ts("23:01.007"))},
				Recent: []ScannedTurn{
					{ID: codexT1, StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:30.500")), Status: TurnStatusCompleted},
				},
			}
		}},
		{"root_turn_id.jsonl", func(t *testing.T) TurnScan {
			return TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: codexT2, StartedAt: turnTime(t, ts("23:01.007"))},
				Recent: []ScannedTurn{
					{ID: codexT1, StartedAt: turnTime(t, ts("22:22.191")), EndedAt: turnTime(t, ts("22:30.500")), Status: TurnStatusCompleted},
				},
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ScanTurns("codex", turnFixture("codex", tt.name))
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			assertTurnScan(t, got, tt.want(t))
		})
	}
}

// TestScanTurnsCodexStartIsEnvelopeTime pins X11 D13: a Codex start is the
// task_started line's millisecond envelope timestamp, never started_at.
func TestScanTurnsCodexStartIsEnvelopeTime(t *testing.T) {
	got, err := ScanTurns("codex", turnFixture("codex", "started_complete.jsonl"))
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	if got.Current.StartedAt.Nanosecond() != 7_000_000 {
		t.Fatalf("StartedAt = %v, want the envelope's .007 s", got.Current.StartedAt)
	}
}

func TestScanTurnsCodexTornTailIsAnError(t *testing.T) {
	_, err := ScanTurns("codex", turnFixture("codex", "torn_tail.jsonl"))
	if !errors.Is(err, ErrTornTurnRecord) {
		t.Fatalf("error = %v, want ErrTornTurnRecord", err)
	}
}

// TestScanTurnsCodexStartBeyondBudget: a turn whose task_started lies beyond
// the budget is known from its turn_context or its end, with no start time;
// a window holding no turn record is an error, never idle.
func TestScanTurnsCodexStartBeyondBudget(t *testing.T) {
	path := turnFixture("codex", "start_beyond_budget.jsonl")
	got, err := scanTurnsWithBudget(t, "codex", path, 3500)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertTurnScan(t, got, TurnScan{Open: true, Current: ScannedTurn{ID: codexT2}})

	if _, err := scanTurnsWithBudget(t, "codex", path, 200); !errors.Is(err, ErrTurnStartBeyondBudget) {
		t.Fatalf("scan with no record in budget: error = %v, want ErrTurnStartBeyondBudget", err)
	}

	ended := writeTurnLines(t, "rollout.jsonl",
		`{"timestamp":"2026-10-08T02:22:22.191Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1","started_at":1791426142}}`,
		`{"timestamp":"2026-10-08T02:22:23.000Z","type":"response_item","payload":{"type":"function_call_output","output":"padding padding padding padding padding padding padding padding padding padding"}}`,
		`{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","error":null}}`,
	)
	got, err = scanTurnsWithBudget(t, "codex", ended, 300)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertTurnScan(t, got, TurnScan{Recent: []ScannedTurn{
		{ID: "t1", EndedAt: turnTime(t, "2026-10-08T02:22:30.500Z"), Status: TurnStatusCompleted},
	}})
}

// TestScanTurnsCodexLateEnds: a delayed end for a turn a newer start closed
// records how it ended and leaves the current turn running.
func TestScanTurnsCodexLateEnds(t *testing.T) {
	path := writeTurnLines(t, "rollout.jsonl",
		`{"timestamp":"2026-10-08T02:22:22.191Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-10-08T02:22:25.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}`,
		`{"timestamp":"2026-10-08T02:22:26.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t1"}}`,
		`{"timestamp":"2026-10-08T02:22:27.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"older"}}`,
	)
	got, err := ScanTurns("codex", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	assertTurnScan(t, got, TurnScan{
		Open:    true,
		Current: ScannedTurn{ID: "t2", StartedAt: turnTime(t, "2026-10-08T02:22:25.000Z")},
		Recent: []ScannedTurn{
			{ID: "t1", StartedAt: turnTime(t, "2026-10-08T02:22:22.191Z"), EndedAt: turnTime(t, "2026-10-08T02:22:26.000Z"), Status: TurnStatusCancelled},
		},
	})
}
