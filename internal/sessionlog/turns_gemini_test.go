package sessionlog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	geminiG1 = "9d1e0000-aaaa-4000-8000-000000000001"
	geminiG2 = "9d1e0000-bbbb-4000-8000-000000000002"
)

// TestScanTurnsGeminiFixtures: Gemini records starts only, so each turn runs
// until the next user message.
func TestScanTurnsGeminiFixtures(t *testing.T) {
	for _, name := range []string{"chat.json", "chat.jsonl"} {
		t.Run(name, func(t *testing.T) {
			got, err := ScanTurns("gemini", turnFixture("gemini", name))
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			assertTurnScan(t, got, TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: geminiG2, StartedAt: turnTime(t, "2026-06-21T17:09:00.123Z")},
				Recent: []ScannedTurn{
					{ID: geminiG1, StartedAt: turnTime(t, "2026-06-21T17:08:00.694Z"), EndedAt: turnTime(t, "2026-06-21T17:09:00.123Z")},
				},
			})
		})
	}
}

func TestScanTurnsGeminiTornIsAnError(t *testing.T) {
	if _, err := ScanTurns("gemini", turnFixture("gemini", "torn_tail.jsonl")); !errors.Is(err, ErrTornTurnRecord) {
		t.Fatalf("JSONL error = %v, want ErrTornTurnRecord", err)
	}
	data, err := os.ReadFile(turnFixture("gemini", "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanTurns("gemini", path); !errors.Is(err, ErrTornTurnRecord) {
		t.Fatalf("JSON error = %v, want ErrTornTurnRecord", err)
	}
}

// TestScanTurnsGeminiJSONLStopsAtSnapshot: a snapshot replaces every message
// before it, and messages after a snapshot within the budget are enough.
func TestScanTurnsGeminiJSONLStopsAtSnapshot(t *testing.T) {
	path := writeTurnLines(t, "session.jsonl",
		`{"sessionId":"s","kind":"main"}`,
		`{"id":"old","timestamp":"2026-06-21T17:00:00.000Z","type":"user","content":"prompt"}`,
		`{"$set":{"messages":[{"id":"u1","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"prompt"}]}}`,
		`{"id":"u2","timestamp":"2026-06-21T17:09:00.123Z","type":"user","content":"prompt"}`,
	)
	got, err := ScanTurns("gemini", path)
	if err != nil {
		t.Fatalf("ScanTurns: %v", err)
	}
	assertTurnScan(t, got, TurnScan{
		Open:    true,
		Current: ScannedTurn{ID: "u2", StartedAt: turnTime(t, "2026-06-21T17:09:00.123Z")},
		Recent:  []ScannedTurn{{ID: "u1", StartedAt: turnTime(t, "2026-06-21T17:08:00.694Z"), EndedAt: turnTime(t, "2026-06-21T17:09:00.123Z")}},
	})
}
