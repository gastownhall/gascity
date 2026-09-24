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
// only line numbers and identifier keys: no value, raw line content, or text
// before the '=' of a non-identifier "key" (which on a JSON paste line is the
// secret itself) reaches the error text.
func TestParseEnvFileErrorsContainNoSecretMaterial(t *testing.T) {
	for name, tc := range map[string]struct {
		content  string
		secrets  []string
		wantErrs int
	}{
		"mixed malformed lines": {
			content: "  \"token\": \"s3cr3t-no-equals\"\n" +
				"=s3cr3t-empty-key\n" +
				"BLOCK_KEY=\"s3cr3t-block-open\n" +
				"s3cr3t-block-mid\n" +
				"s3cr3t-block-close\"\n" +
				"EOF_KEY='s3cr3t-eof\n",
			secrets:  []string{"s3cr3t-no-equals", "s3cr3t-empty-key", "s3cr3t-block-open", "s3cr3t-block-mid", "s3cr3t-block-close", "s3cr3t-eof"},
			wantErrs: 4,
		},
		"unquoted JSON paste with base64 padding": {
			content: "CODEX_AUTH_JSON={\n" +
				"  \"refresh_token\": \"rt-SECRET=\"\n" +
				"}\n",
			secrets:  []string{"rt-SECRET", "refresh_token"},
			wantErrs: 2,
		},
		"double-quoted JSON paste": {
			content: "CODEX_AUTH_JSON=\"{\n" +
				"  \"access\": \"at-SECRET\"\n",
			secrets:  []string{"at-SECRET", "access"},
			wantErrs: 2,
		},
		"non-identifier key": {
			content:  "sk-live-SECRET=value\n",
			secrets:  []string{"sk-live-SECRET"},
			wantErrs: 1,
		},
	} {
		got, errs := ParseEnvFile(tc.content)
		if len(errs) != tc.wantErrs {
			t.Errorf("%s: ParseEnvFile returned %d errors, want %d: %v", name, len(errs), tc.wantErrs, errs)
		}
		for _, err := range errs {
			for _, secret := range tc.secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s: error %q leaks secret material %q", name, err, secret)
				}
			}
		}
		for key := range got {
			if !isEnvIdentifier(key) {
				t.Errorf("%s: map holds non-identifier key %q", name, key)
			}
		}
	}
}

// TestParseEnvFileRejectsNonIdentifierKeys asserts that a key outside
// [A-Za-z_][A-Za-z0-9_]* is a malformed line reported without its text and
// never added to the map, while valid entries around it survive.
func TestParseEnvFileRejectsNonIdentifierKeys(t *testing.T) {
	content := "GOOD=1\n" +
		"\"quoted\": \"x=\"\n" +
		"1LEADING_DIGIT=x\n" +
		"HAS-DASH=x\n" +
		"export HAS SPACE=x\n" +
		"_ok_2=y\n"
	got, errs := ParseEnvFile(content)
	want := map[string]string{"GOOD": "1", "_ok_2": "y"}
	if len(got) != len(want) || got["GOOD"] != "1" || got["_ok_2"] != "y" {
		t.Fatalf("ParseEnvFile returned %v, want exactly %v", got, want)
	}
	wantErrs := []string{"line 2: invalid key", "line 3: invalid key", "line 4: invalid key", "line 5: invalid key"}
	if len(errs) != len(wantErrs) {
		t.Fatalf("ParseEnvFile returned %d errors, want %d: %v", len(errs), len(wantErrs), errs)
	}
	for i, want := range wantErrs {
		if errs[i].Error() != want {
			t.Errorf("errs[%d] = %q, want %q", i, errs[i], want)
		}
	}
}

// TestParseEnvFileUnclosedQuoteDoesNotSwallowValidAssignments asserts the
// closing-line rule: a candidate closing line that is itself a complete
// one-line assignment does not close the block, so a typo'd unclosed quote
// costs only its own line and every valid entry after it survives.
func TestParseEnvFileUnclosedQuoteDoesNotSwallowValidAssignments(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    map[string]string
	}{
		"typo before plain and quoted keys": {
			content: "ANTHROPIC_API_KEY=\"sk-ant-1\nGITHUB_TOKEN=ghp_plain\nOPENAI_API_KEY=\"sk-o\"\nLAST=z\n",
			want:    map[string]string{"GITHUB_TOKEN": "ghp_plain", "OPENAI_API_KEY": "sk-o", "LAST": "z"},
		},
		"lone opening quote": {
			content: "A=\"\nB=\"sk-b\"\n",
			want:    map[string]string{"B": "sk-b"},
		},
		"apostrophe-opened value": {
			content: "NOTE='it\nSAY=it's\nGREETING='hello'\nLAST=z\n",
			want:    map[string]string{"SAY": "it's", "GREETING": "hello", "LAST": "z"},
		},
	} {
		got, errs := ParseEnvFile(tc.content)
		if len(got) != len(tc.want) {
			t.Errorf("%s: ParseEnvFile returned %v, want exactly %v", name, got, tc.want)
		}
		for key, wantVal := range tc.want {
			if got[key] != wantVal {
				t.Errorf("%s: ParseEnvFile()[%q] = %q, want %q", name, key, got[key], wantVal)
			}
		}
		if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "line 1: unterminated quote") {
			t.Errorf("%s: errors = %v, want one unterminated-quote error for line 1", name, errs)
		}
	}
}

// TestParseEnvFileDoubleQuotedJSONPasteIsOneBlock asserts that a double-quoted
// multi-line JSON paste is skipped as a single block: JSON field lines that
// end with a quote but hold an even number of quotes do not close it, so no
// field line is reported (or parsed) on its own.
func TestParseEnvFileDoubleQuotedJSONPasteIsOneBlock(t *testing.T) {
	content := "CODEX_AUTH_JSON=\"{\n" +
		"  \"tokens\": {\n" +
		"    \"access\": \"at-SECRET\",\n" +
		"    \"refresh_token\": \"rt-SECRET=\"\n" +
		"  },\n" +
		"  \"last\": \"x\"\n" +
		"}\"\n" +
		"AFTER=kept\n"
	got, errs := ParseEnvFile(content)
	if len(got) != 1 || got["AFTER"] != "kept" {
		t.Fatalf("ParseEnvFile returned %v, want only AFTER=kept", got)
	}
	if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "lines 1-7:") {
		t.Fatalf("ParseEnvFile errors = %v, want one error for lines 1-7", errs)
	}
}
