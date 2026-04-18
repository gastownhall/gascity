package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMatchedDeprecatedKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		line    string
		wantKey string
		wantOK  bool
	}{
		{"plain skills", `skills = ["a"]`, "skills", true},
		{"plain mcp", `mcp = []`, "mcp", true},
		{"skills_append", `skills_append = ["x"]`, "skills_append", true},
		{"mcp_append", `mcp_append = []`, "mcp_append", true},
		{"leading whitespace", "   skills = []", "skills", true},
		{"tab indent", "\tskills = [\"x\"]", "skills", true},
		{"no spaces around equals", `skills=["a"]`, "skills", true},
		{"comment line", `# skills = ["a"]`, "", false},
		{"unrelated key", `skills_dir = "x"`, "", false},
		{"key prefix mismatch", `skills_other = []`, "", false},
		{"empty line", "", "", false},
		{"no equals", `skills`, "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := matchedDeprecatedKey(c.line)
			if got != c.wantKey || ok != c.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, c.wantKey, c.wantOK)
			}
		})
	}
}

func TestArrayLineSpanSingleAndMultiLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		span  int
	}{
		{"empty inline", `skills = []`, 1},
		{"populated inline", `skills = ["a", "b"]`, 1},
		{"multi-line array", "skills = [\n  \"a\",\n  \"b\",\n]", 4},
		{"multi-line inline open", "skills = [ \"a\",\n  \"b\"\n]", 3},
		{"string contains bracket", `skills = ["weird]name", "ok"]`, 1},
		{"string contains escape", `skills = ["a\"b]"]`, 1},
		{"unclosed array fallback", "skills = [\n  \"a\",", 1},
		{"comment after open bracket", "skills = [ # comment\n  \"a\"\n]", 3},
		// Regression for Phase 2 review pass 2: array containing a TOML
		// literal multi-line string whose body holds a `]` character.
		// Without literal-multiline tracking, scanBrackets would close
		// the array at the body bracket and miscount the span.
		{"literal multiline in array", "skills = [\n  '''line\n  with ] bracket''',\n  \"ok\"\n]", 5},
		// Single-quote literal strings inside arrays.
		{"literal single in array", `skills = ['weird]name', "ok"]`, 1},
		// Multi-line basic string assigned directly (invalid schema for
		// our tombstones but the scanner must still measure it correctly).
		{"basic multiline as value", "skills = \"\"\"\nbody\n\"\"\"", 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lines := strings.Split(c.input, "\n")
			got := arrayLineSpan(lines, 0)
			if got != c.span {
				t.Fatalf("span = %d, want %d (input %q)", got, c.span, c.input)
			}
		})
	}
}

func TestFindDeprecatedAttachmentFieldLinesMultipleHits(t *testing.T) {
	t.Parallel()
	src := `[[agent]]
name = "mayor"
provider = "claude"
skills = ["one", "two"]
mcp = []

[[agent]]
name = "polecat"
skills_append = [
  "extra"
]
nudge = "hello"
`
	got := findDeprecatedAttachmentFieldLines(src)
	want := []deprecatedAttachmentLine{
		{Key: "skills", Line: 4},
		{Key: "mcp", Line: 5},
		{Key: "skills_append", Line: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRewriteWithoutDeprecatedAttachmentFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	src := `# preserved comment
[[agent]]
name = "mayor"
provider = "codex"
skills = ["a", "b"]
mcp = ["m"]
nudge = "hi"

[[agent]]
name = "polecat"
skills_append = [
  "alpha",
  "beta",
]
provider = "claude"

[agent_defaults]
mcp_append = []
allow_overlay = ["foo"]
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `# preserved comment
[[agent]]
name = "mayor"
provider = "codex"
nudge = "hi"

[[agent]]
name = "polecat"
provider = "claude"

[agent_defaults]
allow_overlay = ["foo"]
`
	if string(got) != want {
		t.Fatalf("rewrite mismatch.\nGot:\n%s\nWant:\n%s", string(got), want)
	}
}

func TestRewriteIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	src := "[[agent]]\nname = \"x\"\nprovider = \"claude\"\nskills = []\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("non-idempotent rewrite:\nfirst:\n%s\nsecond:\n%s", string(first), string(second))
	}
}

func TestRewritePreservesNoTrailingNewline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	src := "[[agent]]\nname = \"x\"\nskills = []"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "[[agent]]\nname = \"x\""
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

func TestScanCityForDeprecatedAttachmentFieldsScopesProperly(t *testing.T) {
	t.Parallel()
	city := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(city, "city.toml"),
		[]byte("[[agent]]\nname = \"a\"\nskills = [\"x\"]\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(city, "pack.toml"),
		[]byte("[pack]\nname = \"p\"\nschema = 2\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// Out of scope: a fragment under the city dir.
	if err := os.WriteFile(
		filepath.Join(city, "extra.toml"),
		[]byte("[[agent]]\nname = \"b\"\nmcp = [\"y\"]\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	hits, err := scanCityForDeprecatedAttachmentFields(city)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (only city.toml)", len(hits))
	}
	if filepath.Base(hits[0].Path) != "city.toml" {
		t.Fatalf("hit path = %q, want city.toml", hits[0].Path)
	}
	if len(hits[0].Lines) != 1 || hits[0].Lines[0].Key != "skills" {
		t.Fatalf("lines = %+v", hits[0].Lines)
	}
}

func TestDeprecatedAttachmentFieldsCheckEndToEnd(t *testing.T) {
	t.Parallel()
	city := t.TempDir()
	src := "[[agent]]\nname = \"mayor\"\nprovider = \"claude\"\nskills = [\"foo\"]\nmcp_append = [\n  \"bar\",\n]\n"
	cityFile := filepath.Join(city, "city.toml")
	if err := os.WriteFile(cityFile, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	check := &DeprecatedAttachmentFieldsCheck{}
	ctx := &CheckContext{CityPath: city}

	r := check.Run(ctx)
	if r.Status != StatusWarning {
		t.Fatalf("Run status = %v, want Warning; result=%+v", r.Status, r)
	}
	if !strings.Contains(r.Message, "1 file") {
		t.Errorf("message: %s", r.Message)
	}
	if r.FixHint == "" {
		t.Errorf("FixHint missing on warning result")
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatal(err)
	}

	r2 := check.Run(ctx)
	if r2.Status != StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK; result=%+v", r2.Status, r2)
	}

	got, _ := os.ReadFile(cityFile)
	want := "[[agent]]\nname = \"mayor\"\nprovider = \"claude\"\n"
	if string(got) != want {
		t.Fatalf("post-fix file:\n%s\nwant:\n%s", string(got), want)
	}
}

func TestDeprecatedAttachmentFieldsCheckCleanFile(t *testing.T) {
	t.Parallel()
	city := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(city, "city.toml"),
		[]byte("[[agent]]\nname = \"x\"\nprovider = \"claude\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	check := &DeprecatedAttachmentFieldsCheck{}
	r := check.Run(&CheckContext{CityPath: city})
	if r.Status != StatusOK {
		t.Fatalf("clean-file status = %v, want OK", r.Status)
	}
}

func TestDeprecatedAttachmentFieldsCheckNoCityPath(t *testing.T) {
	t.Parallel()
	check := &DeprecatedAttachmentFieldsCheck{}
	r := check.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("empty-path status = %v, want OK", r.Status)
	}
}

// TestRewritePreservesMultilineStringContent is the regression for the
// Phase 2 review: the scanner must not strip lines whose content
// happens to look like a deprecated assignment when they live inside
// a TOML multi-line string. Without triple-quote tracking,
// `gc doctor --fix` would corrupt an illustrative example embedded
// in a description or prompt field.
// TestRewriteWithLiteralMultilineInArray is the regression for the
// pass-2 Codex finding: a deprecated array can validly contain a
// `”'..”'` body whose text includes `]`. Without literal-multiline
// tracking the rewrite would close the array early, leave the
// remaining body content as orphan lines, and corrupt the file.
func TestRewriteWithLiteralMultilineInArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	src := `[[agent]]
name = "x"
skills = [
  '''multi-line skill
  with ] bracket''',
  "other"
]
nudge = "hi"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `[[agent]]
name = "x"
nudge = "hi"
`
	if string(got) != want {
		t.Fatalf("rewrite over-consumed or under-consumed.\nGot:\n%s\nWant:\n%s", string(got), want)
	}
}

func TestRewritePreservesMultilineStringContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	src := `[[agent]]
name = "mayor"
provider = "claude"
description = """
The v0.15.0 config used:
skills = ["foo"]
mcp = ["bar"]
skills_append = ["baz"]
"""
skills = ["real"]
nudge = "hi"

[[agent]]
name = "polecat"
literal_example = '''
Copy this to your city.toml:
  skills = ["legacy"]
  mcp = []
'''
mcp = ["real"]
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWithoutDeprecatedAttachmentFields(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `[[agent]]
name = "mayor"
provider = "claude"
description = """
The v0.15.0 config used:
skills = ["foo"]
mcp = ["bar"]
skills_append = ["baz"]
"""
nudge = "hi"

[[agent]]
name = "polecat"
literal_example = '''
Copy this to your city.toml:
  skills = ["legacy"]
  mcp = []
'''
`
	if string(got) != want {
		t.Fatalf("multi-line string content corrupted.\nGot:\n%s\nWant:\n%s", string(got), want)
	}
}

func TestFindDeprecatedInMultilineStringSkipped(t *testing.T) {
	t.Parallel()
	src := `description = """
skills = ["embedded"]
"""
skills = ["real"]
`
	got := findDeprecatedAttachmentFieldLines(src)
	want := []deprecatedAttachmentLine{{Key: "skills", Line: 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v (the line inside the multi-line string must NOT be reported)", got, want)
	}
}

func TestTomlStringStateTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		in   tomlStringState
		want tomlStringState
	}{
		{"open basic", `x = """`, tomlStringState{}, tomlStringState{inBasic: true}},
		{"close basic", `"""`, tomlStringState{inBasic: true}, tomlStringState{}},
		{"basic one-liner", `x = """one"""`, tomlStringState{}, tomlStringState{}},
		{"open literal", `x = '''`, tomlStringState{}, tomlStringState{inLiteral: true}},
		{"close literal", `'''`, tomlStringState{inLiteral: true}, tomlStringState{}},
		{"literal one-liner", `x = '''raw'''`, tomlStringState{}, tomlStringState{}},
		{"basic-while-in-literal ignored", `blah """ blah`, tomlStringState{inLiteral: true}, tomlStringState{inLiteral: true}},
		{"literal-while-in-basic ignored", `blah ''' blah`, tomlStringState{inBasic: true}, tomlStringState{inBasic: true}},
		{"plain line", `x = "single"`, tomlStringState{}, tomlStringState{}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.in.update(c.line)
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDeprecatedAttachmentFieldsCheckCanFix(t *testing.T) {
	t.Parallel()
	if !(&DeprecatedAttachmentFieldsCheck{}).CanFix() {
		t.Fatal("CanFix should be true")
	}
}
