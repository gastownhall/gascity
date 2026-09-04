package processenv

import (
	"strings"
	"testing"
)

func TestParseEnvFileParsesCoreSyntax(t *testing.T) {
	content := `# leading comment
ANTHROPIC_AUTH_TOKEN=sk-live-123

export OPENAI_API_KEY=sk-openai-456
GC_DOLT_PASSWORD = secret with spaces
QUOTED_DOUBLE="value with = and # inside"
QUOTED_SINGLE='single value'
   # indented comment
EMPTY_VALUE=
TRAILING_INLINE=keep#notacomment
`
	got, errs := ParseEnvFile(content)
	if len(errs) != 0 {
		t.Fatalf("ParseEnvFile returned errors: %v", errs)
	}
	want := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-live-123",
		"OPENAI_API_KEY":       "sk-openai-456",
		"GC_DOLT_PASSWORD":     "secret with spaces",
		"QUOTED_DOUBLE":        "value with = and # inside",
		"QUOTED_SINGLE":        "single value",
		"EMPTY_VALUE":          "",
		"TRAILING_INLINE":      "keep#notacomment",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
}

func TestParseEnvFileEmptyContentReturnsEmptyMap(t *testing.T) {
	got, errs := ParseEnvFile("")
	if len(errs) != 0 {
		t.Fatalf("ParseEnvFile returned errors: %v", errs)
	}
	if len(got) != 0 {
		t.Fatalf("ParseEnvFile(\"\") = %v, want empty map", got)
	}
}

func TestParseEnvFileRejectsMalformedLines(t *testing.T) {
	for name, content := range map[string]string{
		"missing equals":       "ANTHROPIC_AUTH_TOKEN sk-live-123",
		"empty key":            "=value",
		"empty key after trim": "   =value",
	} {
		if _, errs := ParseEnvFile(content); len(errs) == 0 {
			t.Errorf("ParseEnvFile(%s) = no errors, want an error", name)
		}
	}
}

func TestParseEnvFileLastDuplicateWins(t *testing.T) {
	got, errs := ParseEnvFile("KEY=first\nKEY=second\n")
	if len(errs) != 0 {
		t.Fatalf("ParseEnvFile returned errors: %v", errs)
	}
	if got["KEY"] != "second" {
		t.Errorf("ParseEnvFile duplicate KEY = %q, want %q", got["KEY"], "second")
	}
}

// TestParseEnvFileMalformedLineKeepsValidEntries asserts the fix for #5982: a
// single malformed line no longer discards every already-parsed entry. All
// valid lines survive, and the one bad line surfaces as a single error.
func TestParseEnvFileMalformedLineKeepsValidEntries(t *testing.T) {
	content := "GOOD_ONE=first\nMALFORMED LINE WITHOUT EQUALS\nGOOD_TWO=second\n"
	got, errs := ParseEnvFile(content)
	if len(errs) != 1 {
		t.Fatalf("ParseEnvFile returned %d errors, want 1: %v", len(errs), errs)
	}
	want := map[string]string{
		"GOOD_ONE": "first",
		"GOOD_TWO": "second",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
}

// TestParseEnvFileMultipleMalformedLinesEachReported asserts that every
// malformed line is reported individually, with the correct 1-based line
// number, while all valid entries in the same file still survive.
func TestParseEnvFileMultipleMalformedLinesEachReported(t *testing.T) {
	content := "GOOD_ONE=first\nMISSING EQUALS HERE\n=empty-key\nGOOD_TWO=second\n"
	got, errs := ParseEnvFile(content)
	if len(errs) != 2 {
		t.Fatalf("ParseEnvFile returned %d errors, want 2: %v", len(errs), errs)
	}
	if !strings.HasPrefix(errs[0].Error(), "line 2:") {
		t.Errorf("errs[0] = %v, want to reference line 2", errs[0])
	}
	if !strings.HasPrefix(errs[1].Error(), "line 3:") {
		t.Errorf("errs[1] = %v, want to reference line 3", errs[1])
	}
	want := map[string]string{
		"GOOD_ONE": "first",
		"GOOD_TWO": "second",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
}
