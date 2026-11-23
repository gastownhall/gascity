package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeTurnLines returns n Claude turns ending in turn: interrupted (the
// tool result and the marker stamped with the next promptId), completed
// (after Stop hook feedback stamped with the next promptId), failed, or left
// open until the next turn starts (after an entry Claude wrote itself,
// stamped with an id no prompt carries), by turn number modulo 4.
func claudeTurnLines(n int) []string {
	var lines []string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("p%02d", i)
		resultID := id
		if i%4 == 0 {
			resultID = fmt.Sprintf("p%02d", i+1)
		}
		ts := func(sec int) string { return fmt.Sprintf("2026-10-13T05:%02d:%02d.%03dZ", i, sec, i) }
		lines = append(lines,
			`{"type":"user","timestamp":"`+ts(0)+`","promptId":"`+id+`","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"prompt"}}`,
			`{"type":"assistant","timestamp":"`+ts(1)+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"t"}],"stop_reason":"tool_use"}}`,
			`{"type":"user","timestamp":"`+ts(2)+`","promptId":"`+resultID+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]}}`,
		)
		switch i % 4 {
		case 0:
			lines = append(lines, `{"type":"user","timestamp":"`+ts(3)+`","promptId":"`+fmt.Sprintf("p%02d", i+1)+`","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
		case 1:
			lines = append(lines,
				`{"type":"user","timestamp":"`+ts(3)+`","promptId":"`+fmt.Sprintf("p%02d", i+1)+`","isMeta":true,"message":{"role":"user","content":"Stop hook feedback: hook text"}}`,
				`{"type":"system","subtype":"turn_duration","timestamp":"`+ts(4)+`"}`)
		case 2:
			lines = append(lines, `{"type":"assistant","timestamp":"`+ts(4)+`","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`)
		case 3:
			lines = append(lines, `{"type":"user","timestamp":"`+ts(3)+`","promptId":"`+fmt.Sprintf("q%02d", i)+`","isMeta":true,"message":{"role":"user","content":"local command output"}}`)
		}
	}
	return lines
}

// codexTurnLines returns n Codex turns ending aborted, completed, failed, or
// left open until the next turn starts, by turn number modulo 4.
func codexTurnLines(n int) []string {
	var lines []string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("t%02d", i)
		ts := func(sec int) string { return fmt.Sprintf("2026-10-08T02:%02d:%02d.%03dZ", i, sec, i) }
		lines = append(lines,
			`{"timestamp":"`+ts(0)+`","type":"event_msg","payload":{"type":"task_started","turn_id":"`+id+`","started_at":1791426142}}`,
			`{"timestamp":"`+ts(1)+`","type":"turn_context","payload":{"turn_id":"`+id+`","model":"m"}}`,
			`{"timestamp":"`+ts(2)+`","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}}`,
		)
		switch i % 4 {
		case 0:
			lines = append(lines, `{"timestamp":"`+ts(3)+`","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"`+id+`"}}`)
		case 1:
			lines = append(lines, `{"timestamp":"`+ts(3)+`","type":"event_msg","payload":{"type":"task_complete","turn_id":"`+id+`","error":null}}`)
		case 2:
			lines = append(lines, `{"timestamp":"`+ts(3)+`","type":"event_msg","payload":{"type":"task_complete","turn_id":"`+id+`","error":{"message":"boom"}}}`)
		}
	}
	return lines
}

// geminiTurnLines returns a Gemini JSONL stream of n turns with a snapshot
// halfway.
func geminiTurnLines(n int) []string {
	lines := []string{`{"sessionId":"s","kind":"main"}`}
	var all []string
	for i := 1; i <= n; i++ {
		msg := fmt.Sprintf(`{"id":"u%02d","timestamp":"2026-06-21T17:%02d:00.%03dZ","type":"user","content":"prompt"}`, i, i, i)
		reply := fmt.Sprintf(`{"id":"a%02d","timestamp":"2026-06-21T17:%02d:05Z","type":"gemini","content":"reply"}`, i, i)
		all = append(all, msg, reply)
		if i == n/2 {
			// A snapshot replaces the whole list with every message so far.
			lines = append(lines, `{"$set":{"messages":[`+strings.Join(all, ",")+`]}}`)
			continue
		}
		lines = append(lines, msg, reply)
	}
	return lines
}

// TestScanTurnsKeepsEightRecentNewestFirst covers the stop-by-count path:
// the scan reads back only as far as the oldest turn it reports.
func TestScanTurnsKeepsEightRecentNewestFirst(t *testing.T) {
	cases := []struct {
		provider string
		lines    []string
		first    string
	}{
		{"claude", claudeTurnLines(11), "p"},
		{"codex", codexTurnLines(11), "t"},
		{"gemini", geminiTurnLines(11), "u"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			got, err := ScanTurns(tc.provider, writeTurnLines(t, "transcript.jsonl", tc.lines...))
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			if len(got.Recent) != MaxRecentTurns {
				t.Fatalf("Recent = %d turns, want %d: %+v", len(got.Recent), MaxRecentTurns, got.Recent)
			}
			for i, turn := range got.Recent {
				if want := fmt.Sprintf("%s%02d", tc.first, 10-i); turn.ID != want {
					t.Fatalf("Recent[%d].ID = %q, want %q", i, turn.ID, want)
				}
				if turn.StartedAt.IsZero() {
					t.Fatalf("Recent[%d] has no start: the scan stopped inside it", i)
				}
			}
			if !got.Open || got.Current.ID != tc.first+"11" {
				t.Fatalf("Current = %+v (open %v), want turn 11 open", got.Current, got.Open)
			}
		})
	}
}

// assertFollowerMatchesScan reads f and a fresh ScanTurns of the same file
// and requires the same result, or errors of the same kind.
func assertFollowerMatchesScan(t *testing.T, step string, f *TurnFollower, provider, path string) {
	t.Helper()
	got, gotErr := f.Read()
	want, wantErr := ScanTurns(provider, path)
	if (gotErr == nil) != (wantErr == nil) || errors.Is(gotErr, ErrTornTurnRecord) != errors.Is(wantErr, ErrTornTurnRecord) {
		t.Fatalf("%s: follower error = %v, scan error = %v", step, gotErr, wantErr)
	}
	if wantErr == nil && !equalTurnScans(got, want) {
		t.Fatalf("%s: follower differs from a full scan\nfollower: %+v\n    scan: %+v", step, got, want)
	}
}

func equalTurnScans(a, b TurnScan) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

func appendTurnBytes(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTurnFollowerEqualsRescan: after every append (including a torn one),
// a truncation and a rotation, the follower reports what a full rescan does.
// Thirteen turns put more than MaxRecentTurns turns before turn 12's tool
// result, which carries turn 13's promptId before turn 13's prompt, so a
// rescan has to stop counting turns at the right record.
func TestTurnFollowerEqualsRescan(t *testing.T) {
	cases := []struct {
		provider string
		lines    []string
	}{
		{"claude", claudeTurnLines(13)},
		{"codex", codexTurnLines(13)},
		{"gemini", geminiTurnLines(13)},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := NewTurnFollower(tc.provider, path)
			if err != nil {
				t.Fatalf("NewTurnFollower: %v", err)
			}
			assertFollowerMatchesScan(t, "empty", f, tc.provider, path)
			for i, line := range tc.lines {
				half := len(line) / 2
				appendTurnBytes(t, path, line[:half])
				if _, err := f.Read(); !errors.Is(err, ErrTornTurnRecord) {
					t.Fatalf("line %d torn: follower error = %v, want ErrTornTurnRecord", i, err)
				}
				appendTurnBytes(t, path, line[half:]+"\n")
				assertFollowerMatchesScan(t, fmt.Sprintf("line %d", i), f, tc.provider, path)
			}

			truncated := strings.Join(tc.lines[:len(tc.lines)/3], "\n") + "\n"
			if err := os.WriteFile(path, []byte(truncated), 0o600); err != nil {
				t.Fatal(err)
			}
			assertFollowerMatchesScan(t, "truncated", f, tc.provider, path)

			rotated := filepath.Join(filepath.Dir(path), "next.jsonl")
			if err := os.WriteFile(rotated, []byte(strings.Join(tc.lines[:len(tc.lines)/2], "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(rotated, path); err != nil {
				t.Fatal(err)
			}
			assertFollowerMatchesScan(t, "rotated", f, tc.provider, path)

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Read(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed: follower error = %v, want ErrNotExist", err)
			}
		})
	}
}

// TestTurnFollowerInvalidNewestLine: a newline-terminated invalid record is
// an error while it is the newest, as it is for a rescan, including when a
// torn record is completed with other bytes (a writer died mid-record and a
// restarted one appended a whole record to the same line).
func TestTurnFollowerInvalidNewestLine(t *testing.T) {
	u := func(sec int, id string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":"2026-10-13T05:00:%02d.100Z","promptId":"%s","message":{"role":"user","content":"prompt"}}`, sec, id)
	}
	end := `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:00:01.000Z"}`
	path := writeTurnLines(t, "claude.jsonl", u(0, "p1"), end)
	f, err := NewTurnFollower("claude", path)
	if err != nil {
		t.Fatal(err)
	}
	assertFollowerMatchesScan(t, "start", f, "claude", path)

	appendTurnBytes(t, path, `{"type":"user","timest`)
	if _, err := f.Read(); !errors.Is(err, ErrTornTurnRecord) {
		t.Fatalf("torn: follower error = %v, want ErrTornTurnRecord", err)
	}
	appendTurnBytes(t, path, u(2, "p2")+"\n")
	for _, step := range []string{"completed with other bytes", "unchanged"} {
		if _, err := f.Read(); !errors.Is(err, ErrTornTurnRecord) {
			t.Fatalf("%s: follower error = %v, want ErrTornTurnRecord", step, err)
		}
		assertFollowerMatchesScan(t, step, f, "claude", path)
	}
	appendTurnBytes(t, path, "\n")
	assertFollowerMatchesScan(t, "blank line after it", f, "claude", path)

	appendTurnBytes(t, path, u(3, "p3")+"\n")
	assertFollowerMatchesScan(t, "valid record after it", f, "claude", path)

	appendTurnBytes(t, path, "not json\n")
	assertFollowerMatchesScan(t, "invalid complete line", f, "claude", path)
	appendTurnBytes(t, path, u(4, "p4")+"\n")
	assertFollowerMatchesScan(t, "valid record after the invalid line", f, "claude", path)
}

// TestTurnFollowerSameLineAppend: a complete record written without its
// newline, then another record appended to the same line, is one invalid line
// to a rescan, and so to the follower, whether the follower folded the first
// record by appending or by rescanning.
func TestTurnFollowerSameLineAppend(t *testing.T) {
	u := func(sec int, id string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":"2026-10-13T05:00:%02d.100Z","promptId":"%s","promptSource":"typed","message":{"role":"user","content":"prompt"}}`, sec, id)
	}
	for _, firstRead := range []string{"append", "rescan"} {
		t.Run(firstRead, func(t *testing.T) {
			path := writeTurnLines(t, "claude.jsonl", u(0, "p1"))
			f, err := NewTurnFollower("claude", path)
			if err != nil {
				t.Fatal(err)
			}
			if firstRead == "append" {
				assertFollowerMatchesScan(t, "start", f, "claude", path)
			}
			appendTurnBytes(t, path, u(1, "p2"))
			assertFollowerMatchesScan(t, "record without its newline", f, "claude", path)
			appendTurnBytes(t, path, "  ")
			assertFollowerMatchesScan(t, "blanks on its line", f, "claude", path)
			appendTurnBytes(t, path, u(2, "p3")+"\n")
			if _, err := f.Read(); !errors.Is(err, ErrTornTurnRecord) {
				t.Fatalf("record appended to its line: follower error = %v, want ErrTornTurnRecord", err)
			}
			assertFollowerMatchesScan(t, "record appended to its line", f, "claude", path)
			appendTurnBytes(t, path, u(3, "p4")+"\n")
			assertFollowerMatchesScan(t, "valid record after the line", f, "claude", path)
		})
	}
}

// TestTurnFollowerEqualsRescanOnFixtures: appending each fixture line by
// line, the follower reports what a full rescan does after every line.
func TestTurnFollowerEqualsRescanOnFixtures(t *testing.T) {
	for _, family := range []string{"claude", "codex", "gemini"} {
		fixtures, err := filepath.Glob(filepath.Join("testdata", "turns", family, "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, fixture := range fixtures {
			t.Run(family+"/"+filepath.Base(fixture), func(t *testing.T) {
				data, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "transcript.jsonl")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				f, err := NewTurnFollower(family, path)
				if err != nil {
					t.Fatal(err)
				}
				for i, line := range strings.SplitAfter(string(data), "\n") {
					if line == "" {
						continue
					}
					appendTurnBytes(t, path, line)
					assertFollowerMatchesScan(t, fmt.Sprintf("line %d", i), f, family, path)
				}
			})
		}
	}
}

// TestTurnFollowerFoldsOnlyAppendedBytes: bytes already folded are not read
// again while the file only grows.
func TestTurnFollowerFoldsOnlyAppendedBytes(t *testing.T) {
	first := `{"type":"user","timestamp":"2026-10-13T05:00:00.100Z","promptId":"pa","message":{"role":"user","content":"prompt"}}`
	path := writeTurnLines(t, "claude.jsonl", first)
	f, err := NewTurnFollower("claude", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the folded record in place, same length, then append.
	if err := os.WriteFile(path, []byte(strings.Replace(first, `"pa"`, `"pb"`, 1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendTurnBytes(t, path, `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-13T05:00:01.000Z"}`+"\n")
	got, err := f.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) != 1 || got.Recent[0].ID != "pa" {
		t.Fatalf("follower = %+v, want the folded turn pa ended", got)
	}
}

// TestTurnFollowerFollowsGeminiJSONByRescan: a whole-document chat file is
// rewritten, so every read rescans it.
func TestTurnFollowerFollowsGeminiJSONByRescan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	write := func(ids ...string) {
		t.Helper()
		var msgs []string
		for _, id := range ids {
			msgs = append(msgs, `{"id":"`+id+`","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"prompt"}`)
		}
		if err := os.WriteFile(path, []byte(`{"messages":[`+strings.Join(msgs, ",")+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ua")
	f, err := NewTurnFollower("gemini", path)
	if err != nil {
		t.Fatal(err)
	}
	assertFollowerMatchesScan(t, "first", f, "gemini", path)
	write("ub")
	assertFollowerMatchesScan(t, "same size rewrite", f, "gemini", path)
	write("ub", "uc")
	assertFollowerMatchesScan(t, "grown", f, "gemini", path)
}
