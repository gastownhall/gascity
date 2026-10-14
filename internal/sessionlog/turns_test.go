package sessionlog

import (
	"errors"
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
		{"claude", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated, Replace: TurnReplaceInterrupt}},
		{"codex", TurnSupport{Start: TurnStartRecorded, End: TurnEndRecorded, Link: TurnLinkCorrelated, Replace: TurnReplaceInterrupt}},
		{"codex/tmux-cli", TurnSupport{Start: TurnStartRecorded, End: TurnEndRecorded, Link: TurnLinkCorrelated, Replace: TurnReplaceInterrupt}},
		{"gemini", TurnSupport{Start: TurnStartRecorded, End: TurnEndInferred, Link: TurnLinkCorrelated, Replace: TurnReplaceInterruptReset}},
		{"pi", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone, Replace: TurnReplaceRestart}},
		{"kimi", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone, Replace: TurnReplaceInterrupt}},
		{"", TurnSupport{Start: TurnStartNone, End: TurnEndNone, Link: TurnLinkNone, Replace: TurnReplaceInterrupt}},
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
	for _, provider := range []string{"pi", "kimi", "opencode"} {
		if _, err := ScanTurns(provider, turnFixture("claude", "completed.jsonl")); !errors.Is(err, ErrTurnsUnsupported) {
			t.Errorf("ScanTurns(%q) error = %v, want ErrTurnsUnsupported", provider, err)
		}
		if _, err := NewTurnFollower(provider, "x"); !errors.Is(err, ErrTurnsUnsupported) {
			t.Errorf("NewTurnFollower(%q) error = %v, want ErrTurnsUnsupported", provider, err)
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
