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

func TestParseEnvFileErrorsOnUnterminatedMultiLineQuote(t *testing.T) {
	content := `GC_NOMAD_AGENT_LAUNCH_SCRIPT="export PATH=/mnt/nomad/gc/bin/current:$PATH
export CODEX_HOME=$NOMAD_SECRETS_DIR/codex-home"
`
	_, err := ParseEnvFile(content)
	if err == nil {
		t.Fatal("expected a parse error for an unterminated quote spanning lines, got nil")
	}
	if !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "GC_NOMAD_AGENT_LAUNCH_SCRIPT") {
		t.Errorf("ParseEnvFile error = %q, want it to name line 1 and key GC_NOMAD_AGENT_LAUNCH_SCRIPT", err)
	}
}

func TestParseEnvFileRejectsUnterminatedQuoteVariants(t *testing.T) {
	for name, content := range map[string]string{
		"unterminated double quote": `FOO="bar`,
		"unterminated single quote": `FOO='bar`,
		"opens quote, closes other": `FOO="bar'`,
	} {
		if _, err := ParseEnvFile(content); err == nil {
			t.Errorf("ParseEnvFile(%s) = nil error, want error", name)
		}
	}
}

func TestParseEnvFileAcceptsLegitimateQuotedAndEdgeCaseValues(t *testing.T) {
	content := `FOO="bar"
BAZ=
LONE_DOUBLE_QUOTE="
LONE_SINGLE_QUOTE='
EMPTY_QUOTES=""
`
	got, err := ParseEnvFile(content)
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	want := map[string]string{
		"FOO":               "bar",
		"BAZ":               "",
		"LONE_DOUBLE_QUOTE": `"`,
		"LONE_SINGLE_QUOTE": `'`,
		"EMPTY_QUOTES":      "",
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
