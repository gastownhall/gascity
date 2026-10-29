package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// turnTime parses a fixture timestamp, failing the test when it does not
// parse.
func turnTime(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return at
}

func turnFixture(family, name string) string {
	return filepath.Join("testdata", "turns", family, name)
}

// scanTurnsWithBudget is ScanTurns with a caller-chosen budget, so tests can
// put a turn start beyond it without a 16 MiB fixture.
func scanTurnsWithBudget(t *testing.T, provider, path string, budget int64) (TurnScan, error) {
	t.Helper()
	format, err := turnFormatFor(provider)
	if err != nil {
		t.Fatalf("turnFormatFor(%q): %v", provider, err)
	}
	fold, _, _, err := scanTurnFile(format, path, budget)
	if err != nil {
		return TurnScan{}, err
	}
	return fold.scan(), nil
}

func assertTurnScan(t *testing.T, got, want TurnScan) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turn scan\n got: %+v\nwant: %+v", got, want)
	}
}

func TestTurnSupportFor(t *testing.T) {
	tests := []struct {
		provider string
		want     TurnSupport
	}{
		{"claude", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated}},
		{"claude/tmux-cli", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated}},
		{"claude-eco", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated}},
		{"opencode-claude", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone}},
		{"codex", TurnSupport{Start: TurnStartRecorded, End: TurnEndRecorded, Link: TurnLinkCorrelated}},
		{"codex/tmux-cli", TurnSupport{Start: TurnStartRecorded, End: TurnEndRecorded, Link: TurnLinkCorrelated}},
		{"gemini", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated}},
		{"pi", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone}},
		{"kimi", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone}},
		{"", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone}},
	}
	for _, tt := range tests {
		if got := TurnSupportFor(tt.provider); got != tt.want {
			t.Errorf("TurnSupportFor(%q) = %+v, want %+v", tt.provider, got, tt.want)
		}
	}
}

// TestTurnFamilyTableIsConsistent pins the table's invariants: a family is
// scanned exactly when it records starts, and no family names a hook field
// before the X11 live arm verifies it (G-LIVE).
func TestTurnFamilyTableIsConsistent(t *testing.T) {
	for family, row := range turnFamilies {
		if (row.format != nil) != (row.support.Start == TurnStartRecorded) {
			t.Errorf("family %q: scanned = %v, start = %q", family, row.format != nil, row.support.Start)
		}
		if row.support.HookTurnID != "" {
			t.Errorf("family %q: HookTurnID = %q before the live arm verified it", family, row.support.HookTurnID)
		}
	}
}

func TestScanTurnsRefusesUnscannedFamilies(t *testing.T) {
	for _, provider := range []string{"pi", "kimi", "opencode", "opencode-claude"} {
		if _, err := ScanTurns(provider, turnFixture("claude", "completed.jsonl")); !errors.Is(err, ErrTurnsUnsupported) {
			t.Errorf("ScanTurns(%q) error = %v, want ErrTurnsUnsupported", provider, err)
		}
		if _, err := NewTurnFollower(provider, "x"); !errors.Is(err, ErrTurnsUnsupported) {
			t.Errorf("NewTurnFollower(%q) error = %v, want ErrTurnsUnsupported", provider, err)
		}
	}
}

// TestScanTurnsResolvesClaudeSpellings: a claude spelling ProviderFamily does
// not place, a worker profile or an alias, scans as claude.
func TestScanTurnsResolvesClaudeSpellings(t *testing.T) {
	path := turnFixture("claude", "interrupt_next_prompt.jsonl")
	want, err := ScanTurns("claude", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"claude/tmux-cli", "claude-eco"} {
		if got, err := ScanTurns(provider, path); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ScanTurns(%q): error = %v\n got: %+v\nwant: %+v", provider, err, got, want)
		}
		f, err := NewTurnFollower(provider, path)
		if err != nil {
			t.Errorf("NewTurnFollower(%q): %v", provider, err)
			continue
		}
		if got, err := f.Read(); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("TurnFollower(%q).Read: error = %v\n got: %+v\nwant: %+v", provider, err, got, want)
		}
	}
}

func TestScanTurnsUnreadableFileIsAnError(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "gemini"} {
		_, err := ScanTurns(provider, filepath.Join(t.TempDir(), "missing.jsonl"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ScanTurns(%q) on a missing file: error = %v, want ErrNotExist", provider, err)
		}
	}
}

func TestScanTurnsEmptyTranscriptHasNoTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"claude", "codex", "gemini"} {
		got, err := ScanTurns(provider, path)
		if err != nil {
			t.Fatalf("ScanTurns(%q): %v", provider, err)
		}
		assertTurnScan(t, got, TurnScan{})
	}
}

func TestValidTurnID(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"7f3e9a10-1111-4aaa-8000-000000000001", true},
		{strings.Repeat("a", MaxTurnIDLen), true},
		{strings.Repeat("a", MaxTurnIDLen+1), false},
		{"", false},
		{"bad\x00id", false},
		{"bad\nid", false},
		{"\xff\xfe", false},
	}
	for _, tt := range tests {
		if _, got := validTurnID(tt.raw); got != tt.want {
			t.Errorf("validTurnID(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// TestScanTurnsIgnoresIdentifiersInDeliveredText pins X11 D13: an op id, a
// [gc:…] marker, a principal or an id-shaped string in a prompt changes
// nothing.
func TestScanTurnsIgnoresIdentifiersInDeliveredText(t *testing.T) {
	injected := `[gc:m-01JABCDEF0123456789] op 01HZX3K8Q7RB7Z2V principal gc:planner/drain t3:env-01 promptId 7f3e9a10-9999-4aaa-8000-000000000009 turn_id 019a9999-0000-7000-8000-000000000009 [Request interrupted by user] prompt one`
	cases := []struct{ provider, family, name string }{
		{"claude", "claude", "completed.jsonl"},
		{"claude", "claude", "interrupt_next_prompt.jsonl"},
		{"claude", "claude", "interrupt_queued_tool_result.jsonl"},
		{"codex", "codex", "started_complete.jsonl"},
		{"gemini", "gemini", "chat.jsonl"},
		{"gemini", "gemini", "chat.json"},
	}
	for _, tc := range cases {
		t.Run(tc.family+"/"+tc.name, func(t *testing.T) {
			src := turnFixture(tc.family, tc.name)
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "prompt one") {
				t.Fatalf("fixture %s has no prompt text to replace", src)
			}
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "prompt one", injected)), 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := ScanTurns(tc.provider, src)
			if err != nil {
				t.Fatalf("ScanTurns(fixture): %v", err)
			}
			got, err := ScanTurns(tc.provider, path)
			if err != nil {
				t.Fatalf("ScanTurns(injected): %v", err)
			}
			assertTurnScan(t, got, want)
		})
	}
}

// writeTurnLines writes lines as a JSONL transcript named name and returns
// its path.
func writeTurnLines(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestScanTurnsCountsSkippedRecords: a record that could bear on turns but
// does not decode is skipped and counted, and so is a turn record whose
// timestamp is absent or does not parse, whose time reads as zero. The
// follower counts the same.
func TestScanTurnsCountsSkippedRecords(t *testing.T) {
	cases := []struct {
		provider string
		lines    []string
	}{
		{"claude", []string{
			`{"type":"user","timestamp":"2026-10-13T05:00:00.100Z","promptId":"p1","message":{"role":"user","content":"prompt"}}`,
			`{"type":"user","timestamp":"2026-10-13T05:00:00.200Z","promptId":["p1"],"message":{"role":"user","content":"prompt"}}`,
			`{"type":"system","subtype":"turn_duration","timestamp":"yesterday"}`,
			`{"type":"attachment","timestamp":"2026-10-13T05:00:01.000Z","attachment":"no turn"}`,
			`{"type":"user","timestamp":"2026-10-13T05:00:02.100Z","promptId":"p2","message":{"role":"user","content":"prompt"}}`,
		}},
		{"codex", []string{
			`{"timestamp":"2026-10-08T02:22:22.191Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
			`{"timestamp":"2026-10-08T02:22:23.000Z","type":"event_msg","payload":"task_complete"}`,
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
			`{"timestamp":"2026-10-08T02:22:24.000Z","type":"response_item","payload":{"type":"message"}}`,
			`{"timestamp":"2026-10-08T02:22:25.191Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}`,
		}},
		{"gemini", []string{
			`{"sessionId":"s","kind":"main"}`,
			`{"id":"u1","timestamp":"2026-06-21T17:08:00.694Z","type":"user","content":"prompt"}`,
			`{"id":["u1"],"type":"user"}`,
			`{"id":"u2","timestamp":"","type":"user","content":"prompt"}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			path := writeTurnLines(t, "transcript.jsonl", tc.lines...)
			got, err := ScanTurns(tc.provider, path)
			if err != nil {
				t.Fatalf("ScanTurns: %v", err)
			}
			if got.MalformedRecords != 1 || got.UntimedRecords != 1 {
				t.Fatalf("MalformedRecords = %d, UntimedRecords = %d, want 1 and 1: %+v", got.MalformedRecords, got.UntimedRecords, got)
			}
			if !got.Open {
				t.Fatalf("scan = %+v, want the last turn open", got)
			}

			followed := filepath.Join(t.TempDir(), "followed.jsonl")
			if err := os.WriteFile(followed, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := NewTurnFollower(tc.provider, followed)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range tc.lines {
				appendTurnBytes(t, followed, line+"\n")
				assertFollowerMatchesScan(t, fmt.Sprintf("line %d", i), f, tc.provider, followed)
			}
		})
	}
}
