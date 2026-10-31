package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestScanTurnsGeminiJSONBeyondBudgetIsAnError: a whole-document chat file is
// read whole, so one larger than the budget is an error, never idle.
func TestScanTurnsGeminiJSONBeyondBudgetIsAnError(t *testing.T) {
	path := turnFixture("gemini", "chat.json")
	if _, err := scanTurnsWithBudget(t, "gemini", path, 64); !errors.Is(err, ErrTurnTranscriptBeyondBudget) {
		t.Fatalf("error = %v, want ErrTurnTranscriptBeyondBudget", err)
	}
}

// TestScanTurnsGeminiJSONLStartBeyondBudget: a JSONL window that holds only
// replies is an error, never idle.
func TestScanTurnsGeminiJSONLStartBeyondBudget(t *testing.T) {
	lines := []string{
		`{"sessionId":"s","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"prompt"}`,
	}
	for i := range 10 {
		lines = append(lines, fmt.Sprintf(`{"id":"g%d","timestamp":"2026-06-21T17:08:01.000Z","type":"gemini","content":%q}`, i, strings.Repeat("y", 200)))
	}
	path := writeTurnLines(t, "session.jsonl", lines...)
	if _, err := scanTurnsWithBudget(t, "gemini", path, 1000); !errors.Is(err, ErrTurnStartBeyondBudget) {
		t.Fatalf("error = %v, want ErrTurnStartBeyondBudget", err)
	}
}

// TestScanTurnsGeminiJSONLWindowWithoutSnapshotIsAnError: a JSONL line for a
// user id adds a message or edits one in place, and only the lines since the
// last snapshot tell which. A window cut off before that snapshot, or inside
// it, is an error, never a turn read off an edit.
func TestScanTurnsGeminiJSONLWindowWithoutSnapshotIsAnError(t *testing.T) {
	snapshot := func(content string) string {
		return `{"$set":{"messages":[{"id":"u1","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"` + content + `"}]}}`
	}
	reply := `{"id":"g1","timestamp":"2026-06-21T17:08:01.000Z","type":"gemini","content":"` + strings.Repeat("y", 1000) + `"}`
	newPrompt := `{"id":"u2","timestamp":"2026-06-21T17:09:00.123Z","type":"user","content":"prompt"}`
	editOfU1 := `{"id":"u1","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"edited prompt"}`
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"snapshot before the window", []string{snapshot("prompt"), reply, newPrompt, editOfU1}},
		{"snapshot across the window start", []string{snapshot(strings.Repeat("x", 1000)), newPrompt, editOfU1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTurnLines(t, "session.jsonl", append([]string{`{"sessionId":"s","kind":"main"}`}, tc.lines...)...)
			// Read from the snapshot on, the edit replaces u1 in place.
			got, err := ScanTurns("gemini", path)
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			assertTurnScan(t, got, TurnScan{
				Open:    true,
				Current: ScannedTurn{ID: "u2", StartedAt: turnTime(t, "2026-06-21T17:09:00.123Z")},
				Recent:  []ScannedTurn{{ID: "u1", StartedAt: turnTime(t, "2026-06-21T17:08:00.694Z"), EndedAt: turnTime(t, "2026-06-21T17:09:00.123Z")}},
			})
			// The window holds both user lines but not the whole snapshot.
			if _, err := scanTurnsWithBudget(t, "gemini", path, 600); !errors.Is(err, ErrTurnStartBeyondBudget) {
				t.Fatalf("error = %v, want ErrTurnStartBeyondBudget", err)
			}
		})
	}
}

// TestTurnFollowerKeepsUnchangedGeminiJSON: a whole-document file whose
// identity, size and modification time are unchanged is not read again,
// unless it was modified so close to the last read that a rewrite in the
// same clock tick would leave its modification time unchanged.
func TestTurnFollowerKeepsUnchangedGeminiJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"modified long before the read", time.Hour, "ua"},
		{"modified just before the read", 0, "ub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			doc := func(id string) []byte {
				return []byte(`{"messages":[{"id":"` + id + `","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"prompt"}]}`)
			}
			if err := os.WriteFile(path, doc("ua"), 0o600); err != nil {
				t.Fatal(err)
			}
			modTime := time.Now().Add(-tc.age)
			if err := os.Chtimes(path, modTime, modTime); err != nil {
				t.Fatal(err)
			}
			f, err := NewTurnFollower("gemini", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Read(); err != nil {
				t.Fatal(err)
			}
			// Rewrite in place, same size, and restore the modification time.
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt(doc("ub"), 0); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, modTime, modTime); err != nil {
				t.Fatal(err)
			}
			got, err := f.Read()
			if err != nil {
				t.Fatal(err)
			}
			if !got.Open || got.Current.ID != tc.want {
				t.Fatalf("follower = %+v, want %s open", got, tc.want)
			}
		})
	}
}
