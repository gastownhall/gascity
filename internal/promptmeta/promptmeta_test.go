package promptmeta

import (
	"testing"
)

func TestParse_NoFrontMatter(t *testing.T) {
	body := "Just a prompt body\nwith two lines.\n"
	fm, got := Parse(body)
	if !fm.IsZero() {
		t.Errorf("expected zero FrontMatter, got %+v", fm)
	}
	if got != body {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestParse_OnlyOpeningDelimiter(t *testing.T) {
	in := "---\nversion: v1\nbody without close"
	fm, got := Parse(in)
	if !fm.IsZero() {
		t.Errorf("missing close delimiter must yield zero FrontMatter, got %+v", fm)
	}
	if got != in {
		t.Errorf("body must be unchanged: got %q", got)
	}
}

func TestParse_BasicVersion(t *testing.T) {
	in := "---\nversion: v3\n---\nbody line one\nbody line two\n"
	fm, body := Parse(in)
	if fm.Version != "v3" {
		t.Errorf("Version = %q, want v3", fm.Version)
	}
	if body != "body line one\nbody line two\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParse_DelimiterOnlyMarkdownIsNotFrontMatter(t *testing.T) {
	in := "---\nThis is a markdown divider section.\n---\nBody line that should reach the agent.\n"
	fm, body := Parse(in)
	if !fm.IsZero() {
		t.Fatalf("delimiter-only markdown should not parse as frontmatter: %+v", fm)
	}
	if body != in {
		t.Fatalf("body = %q, want original input", body)
	}
}

func TestParse_QuotedValues(t *testing.T) {
	cases := []string{
		`version: "v3"`,
		`version: 'v3'`,
		`version: v3`,
		`version:    v3   `,
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			in := "---\n" + line + "\n---\nbody\n"
			fm, _ := Parse(in)
			if fm.Version != "v3" {
				t.Errorf("Version = %q, want v3", fm.Version)
			}
		})
	}
}

func TestParse_RawCarriesAllPairs(t *testing.T) {
	in := "---\nversion: v2\nauthor: ari\nnotes: handles edge cases\n---\nbody\n"
	fm, _ := Parse(in)
	if fm.Raw["version"] != "v2" {
		t.Errorf("Raw[version] = %q", fm.Raw["version"])
	}
	if fm.Raw["author"] != "ari" {
		t.Errorf("Raw[author] = %q", fm.Raw["author"])
	}
	if fm.Raw["notes"] != "handles edge cases" {
		t.Errorf("Raw[notes] = %q", fm.Raw["notes"])
	}
}

func TestParse_BlankLinesAndComments(t *testing.T) {
	in := "---\n\n# this is a comment\nversion: v4\n# trailing comment\n---\nbody\n"
	fm, _ := Parse(in)
	if fm.Version != "v4" {
		t.Errorf("Version = %q, want v4", fm.Version)
	}
	if _, hasComment := fm.Raw["# this is a comment"]; hasComment {
		t.Error("comments must not become Raw keys")
	}
}

func TestParse_LaterKeyWins(t *testing.T) {
	in := "---\nversion: v1\nversion: v2\n---\nbody"
	fm, _ := Parse(in)
	if fm.Version != "v2" {
		t.Errorf("later key should win: got %q", fm.Version)
	}
}

func TestParse_CRLFLineEndings(t *testing.T) {
	in := "---\r\nversion: v3\r\n---\r\nbody line\r\n"
	fm, body := Parse(in)
	if fm.Version != "v3" {
		t.Errorf("CRLF Version = %q, want v3", fm.Version)
	}
	if body != "body line\r\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParse_MalformedNotPanic(t *testing.T) {
	cases := []string{
		"",
		"---",
		"---\n",
		"---\n---",
		"---\n---\n",
		"---\nno colon\n---\nbody",
		"---\n: empty key\nversion: v1\n---\nbody",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse panicked: %v", r)
				}
			}()
			Parse(in)
		})
	}
}

func TestParse_EmptyKeyDropped(t *testing.T) {
	in := "---\n: empty\nversion: v1\n---\nbody"
	fm, _ := Parse(in)
	if _, has := fm.Raw[""]; has {
		t.Error("empty key must not be in Raw")
	}
	if fm.Version != "v1" {
		t.Errorf("Version = %q", fm.Version)
	}
}

func TestParse_BodyEmptyOK(t *testing.T) {
	in := "---\nversion: v1\n---\n"
	fm, body := Parse(in)
	if fm.Version != "v1" {
		t.Errorf("Version = %q", fm.Version)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestSHA(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"hello", "hello", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SHA(tc.in)
			if got != tc.want {
				t.Errorf("SHA(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSHA_DifferentInputsDifferentOutputs(t *testing.T) {
	a := SHA("rendered A")
	b := SHA("rendered B")
	if a == b {
		t.Fatal("different inputs must produce different SHA outputs")
	}
}

func TestSHA_DeterministicOnSameInput(t *testing.T) {
	in := "stable rendered prompt"
	a := SHA(in)
	b := SHA(in)
	if a != b {
		t.Errorf("SHA must be deterministic, got %q vs %q", a, b)
	}
}

// TestSHAResolvesUnbumpedTemplateEditScenario simulates the failure mode
// 1e is designed to detect: an operator edits a template body but does
// not bump the `version`. Two renders share Version="v3" but the SHA
// values diverge, surfacing the silent change.
func TestSHAResolvesUnbumpedTemplateEditScenario(t *testing.T) {
	v1Body := "Step 1: do the thing.\nStep 2: report it.\n"
	v2Body := "Step 1: do the thing.\nStep 2: report it AND verify it.\n"
	if SHA(v1Body) == SHA(v2Body) {
		t.Fatal("rendered bodies should hash to different SHAs")
	}
	// The Version field is unchanged in both — that's the failure mode.
	// SHA carries the forensic answer.
}
