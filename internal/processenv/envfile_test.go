package processenv

import "testing"

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
	got, err := ParseEnvFile(content)
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
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
	got, err := ParseEnvFile("")
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
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
		if _, err := ParseEnvFile(content); err == nil {
			t.Errorf("ParseEnvFile(%s) = nil error, want error", name)
		}
	}
}

func TestParseEnvFileLastDuplicateWins(t *testing.T) {
	got, err := ParseEnvFile("KEY=first\nKEY=second\n")
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if got["KEY"] != "second" {
		t.Errorf("ParseEnvFile duplicate KEY = %q, want %q", got["KEY"], "second")
	}
}
