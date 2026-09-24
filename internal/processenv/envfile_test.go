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

// TestParseEnvFileMultiLineQuotedValueSkipsWholeBlock asserts the fix for
// #6022: a quoted value continued onto later lines is skipped as one block
// through the line holding the matching closing quote. No continuation line
// becomes a key of its own (the stray CODEX_HOME), the opening key is not
// kept with a truncated value, and valid entries on either side survive.
func TestParseEnvFileMultiLineQuotedValueSkipsWholeBlock(t *testing.T) {
	content := "BEFORE=kept-before\n" +
		"GC_NOMAD_AGENT_LAUNCH_SCRIPT=\"export PATH=/mnt/nomad/gc/bin/current:$PATH\n" +
		"export CODEX_HOME=$NOMAD_SECRETS_DIR/codex-home\"\n" +
		"AFTER=kept-after\n"
	got, errs := ParseEnvFile(content)
	want := map[string]string{"BEFORE": "kept-before", "AFTER": "kept-after"}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %v, want exactly %v", got, want)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
	if _, ok := got["CODEX_HOME"]; ok {
		t.Errorf("continuation line leaked as key CODEX_HOME: %v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("ParseEnvFile returned %d errors, want 1: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	if !strings.HasPrefix(msg, "lines 2-3:") || !strings.Contains(msg, "GC_NOMAD_AGENT_LAUNCH_SCRIPT") {
		t.Errorf("error = %q, want it to name lines 2-3 and key GC_NOMAD_AGENT_LAUNCH_SCRIPT", msg)
	}
}

// TestParseEnvFileMultiLinePasteKeepsLaterEntries asserts that a multi-line
// PEM or JSON paste is skipped as one block (including continuation lines
// that contain '=', such as base64 padding) and every valid entry after the
// block survives.
func TestParseEnvFileMultiLinePasteKeepsLaterEntries(t *testing.T) {
	content := "PEM_KEY=\"-----BEGIN PRIVATE KEY-----\n" +
		"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC==\n" +
		"-----END PRIVATE KEY-----\"\n" +
		"CODEX_AUTH_JSON='{\n" +
		"  \"token\": \"eyJhbGciOi\"\n" +
		"}'\n" +
		"ANTHROPIC_AUTH_TOKEN=sk-after-block\n"
	got, errs := ParseEnvFile(content)
	if len(got) != 1 || got["ANTHROPIC_AUTH_TOKEN"] != "sk-after-block" {
		t.Fatalf("ParseEnvFile returned %v, want only ANTHROPIC_AUTH_TOKEN=sk-after-block", got)
	}
	if len(errs) != 2 {
		t.Fatalf("ParseEnvFile returned %d errors, want 2: %v", len(errs), errs)
	}
	if !strings.HasPrefix(errs[0].Error(), "lines 1-3:") || !strings.HasPrefix(errs[1].Error(), "lines 4-6:") {
		t.Errorf("errors = %v, want blocks lines 1-3 and lines 4-6", errs)
	}
}

// TestParseEnvFileQuoteOpenToEOFSkipsOnlyOpeningLine asserts that when no
// later line closes an opening quote, only the opening line is skipped and
// the following lines are parsed normally.
func TestParseEnvFileQuoteOpenToEOFSkipsOnlyOpeningLine(t *testing.T) {
	got, errs := ParseEnvFile("GOOD_ONE=first\nBROKEN=\"never closed\nGOOD_TWO=second\n")
	if len(got) != 2 || got["GOOD_ONE"] != "first" || got["GOOD_TWO"] != "second" {
		t.Fatalf("ParseEnvFile returned %v, want GOOD_ONE and GOOD_TWO only", got)
	}
	if len(errs) != 1 {
		t.Fatalf("ParseEnvFile returned %d errors, want 1: %v", len(errs), errs)
	}
	if msg := errs[0].Error(); !strings.HasPrefix(msg, "line 2:") || !strings.Contains(msg, "BROKEN") {
		t.Errorf("error = %q, want it to name line 2 and key BROKEN", msg)
	}
}

// TestParseEnvFileQuotedValueWithTrailingTextStaysLiteral pins the existing
// behavior for a quote closed on the same line but followed by more text: the
// whole value is kept literally and is not an error, so existing files with an
// inline comment or shell-style concatenation do not lose the key.
func TestParseEnvFileQuotedValueWithTrailingTextStaysLiteral(t *testing.T) {
	got, errs := ParseEnvFile("OPENAI_API_KEY=\"sk-o\" # work key\nCONCAT=\"a\"'b'c\n")
	if len(errs) != 0 {
		t.Fatalf("ParseEnvFile returned errors: %v", errs)
	}
	want := map[string]string{
		"OPENAI_API_KEY": `"sk-o" # work key`,
		"CONCAT":         `"a"'b'c`,
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
}

// TestParseEnvFileCRLFQuotedValues asserts CRLF line endings work for quoted
// single-line values and for a skipped multi-line block.
func TestParseEnvFileCRLFQuotedValues(t *testing.T) {
	content := "DOUBLE=\"double value\"\r\n" +
		"SINGLE='single value'\r\n" +
		"BLOCK=\"first\r\n" +
		"export STRAY=second\"\r\n" +
		"AFTER=after\r\n"
	got, errs := ParseEnvFile(content)
	want := map[string]string{"DOUBLE": "double value", "SINGLE": "single value", "AFTER": "after"}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %v, want exactly %v", got, want)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
	if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "lines 3-4:") {
		t.Fatalf("ParseEnvFile errors = %v, want one error for lines 3-4", errs)
	}
}

// TestParseEnvFileErrorsContainNoSecretMaterial asserts that parse errors name
// only line numbers and keys: no value or raw line content from a malformed
// line, a skipped block, or an unterminated quote reaches the error text.
func TestParseEnvFileErrorsContainNoSecretMaterial(t *testing.T) {
	secrets := []string{"s3cr3t-no-equals", "s3cr3t-empty-key", "s3cr3t-block-open", "s3cr3t-block-mid", "s3cr3t-block-close", "s3cr3t-eof"}
	content := "  \"token\": \"s3cr3t-no-equals\"\n" +
		"=s3cr3t-empty-key\n" +
		"BLOCK_KEY=\"s3cr3t-block-open\n" +
		"s3cr3t-block-mid\n" +
		"s3cr3t-block-close\"\n" +
		"EOF_KEY='s3cr3t-eof\n"
	_, errs := ParseEnvFile(content)
	if len(errs) != 4 {
		t.Fatalf("ParseEnvFile returned %d errors, want 4: %v", len(errs), errs)
	}
	for _, err := range errs {
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q leaks secret material %q", err, secret)
			}
		}
	}
}
