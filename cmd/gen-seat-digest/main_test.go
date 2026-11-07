package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

const fixtureAgents = `# Title

Intro that is not kept.

<!-- seat:begin -->
Kept intro. Agents must read it.
<!-- seat:end -->

## Section A

- First bullet, dropped.
<!-- seat:begin -->
- Second bullet: never push to main,
  continued on a second line.
<!-- seat:end -->

## Section B

Dropped paragraph.

<!-- seat:begin -->
### Sub B1

Kept sub-section text.

## Section C

Do not run go clean -cache.
<!-- seat:end -->

## Section D

Plain text, no rules.
`

func TestGenerateKeepsMarkedRegionsWithTheirHeadings(t *testing.T) {
	got, err := generate(fixtureAgents)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(got, digestHeader)
	if body == got {
		t.Fatalf("digest does not start with the generated header:\n%s", got)
	}
	want := `Kept intro. Agents must read it.

## Section A

- Second bullet: never push to main,
  continued on a second line.

## Section B

### Sub B1

Kept sub-section text.

## Section C

Do not run go clean -cache.
`
	if body != want {
		t.Fatalf("digest body mismatch\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
	for _, dropped := range []string{"Intro that is not kept", "First bullet", "Dropped paragraph", "Section D", markerBegin, markerEnd} {
		if strings.Contains(body, dropped) {
			t.Errorf("digest kept %q", dropped)
		}
	}
}

func TestGenerateEmitsARegionHeadingOnce(t *testing.T) {
	// The first region opens on the section heading. A later region in the
	// same section, after an unmarked paragraph, does not repeat it.
	src := "# T\n\n<!-- seat:begin -->\n## Build\n\nNever run x.\n<!-- seat:end -->\n\nUnmarked paragraph.\n\n<!-- seat:begin -->\n- Never run y.\n<!-- seat:end -->\n"
	got, err := generate(src)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Build\n\nNever run x.\n\n- Never run y.\n"
	if body := strings.TrimPrefix(got, digestHeader); body != want {
		t.Fatalf("digest body mismatch\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestGenerateRejectsMalformedMarkers(t *testing.T) {
	cases := map[string]string{
		"unclosed":    "# T\n<!-- seat:begin -->\ntext\n",
		"stray end":   "# T\ntext\n<!-- seat:end -->\n",
		"nested":      "# T\n<!-- seat:begin -->\n<!-- seat:begin -->\nx\n<!-- seat:end -->\n<!-- seat:end -->\n",
		"empty":       "# T\n<!-- seat:begin -->\n\n<!-- seat:end -->\n",
		"no regions":  "# T\ntext\n",
		"inline mark": "# T\ntext <!-- seat:begin -->\nmore\n<!-- seat:end -->\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := generate(src); err == nil {
				t.Fatalf("generate accepted malformed markers:\n%s", src)
			}
		})
	}
}

// fencedMarkerAgents marks a region that shows the marker syntax in a fence.
// The example states a rule, so the rule guard reads that fence too.
const fencedMarkerAgents = "# T\n\n## Markers\n\n<!-- seat:begin -->\nMark a region like this:\n\n```markdown\n<!-- seat:begin -->\nAgents must keep this text.\n<!-- seat:end -->\n```\n<!-- seat:end -->\n"

func TestGenerateTreatsFencedLinesAsText(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		// A `#` comment in an unmarked fence is no heading, so it does not
		// hide the section heading of the marked rule after the fence.
		"fenced comment": {
			src:  "# T\n\n## Shell\n\n```bash\n# Force overwrite without prompting\ncp -f a b\n```\n\n<!-- seat:begin -->\n- Never use rm -i.\n<!-- seat:end -->\n",
			want: "## Shell\n\n- Never use rm -i.\n",
		},
		// Seat markers in a fence are an example, not region boundaries.
		"fenced markers": {
			src:  fencedMarkerAgents,
			want: "## Markers\n\nMark a region like this:\n\n```markdown\n<!-- seat:begin -->\nAgents must keep this text.\n<!-- seat:end -->\n```\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := generate(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if body := strings.TrimPrefix(got, digestHeader); body != tc.want {
				t.Fatalf("digest body mismatch\n--- got ---\n%s\n--- want ---\n%s", body, tc.want)
			}
		})
	}
}

func TestBlocksKeepsAFenceWholeAcrossBlankLines(t *testing.T) {
	fence := "```bash\ngo build ./...\n\n# never run go clean -cache here\n```"
	got := blocks("# T\n\n" + fence + "\n\nAfter.\n")
	if want := []string{fence, "After."}; !slices.Equal(got, want) {
		t.Fatalf("blocks = %q, want %q", got, want)
	}
}

func TestRuleBlocksFindsEveryRuleKeyword(t *testing.T) {
	src := "# T\n\nPlain.\n\n- You MUST do x.\n- Never do y.\n- Do not do z.\n- Don't do w.\n- Hard ban: v.\n- HARD-RULE: u.\n- harmless\n\n| a | never in a table |\n" +
		// A hard-wrapped file splits a phrase across lines; "do nothing" is no rule.
		"\n- Agents do\n  not run t.\n- A hard\n  rule: s.\n- A hard\n  ban: r.\n- Don’t do q.\n- Do nothing to p.\n\nAgents do\nnot run o.\n" +
		// Emphasis does not hide a rule word; "do **nothing**" is still no rule.
		"\n- Agents do **not** run n.\n- Agents do *not* run m.\n- Agents do _not_ run l.\n- Agents _never_ run k.\n- Do **nothing** to j.\n"
	got := ruleBlocks(src)
	want := []string{
		"- You MUST do x.", "- Never do y.", "- Do not do z.", "- Don't do w.", "- Hard ban: v.", "- HARD-RULE: u.", "| a | never in a table |",
		"- Agents do\n  not run t.", "- A hard\n  rule: s.", "- A hard\n  ban: r.", "- Don’t do q.", "Agents do\nnot run o.",
		"- Agents do **not** run n.", "- Agents do *not* run m.", "- Agents do _not_ run l.", "- Agents _never_ run k.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ruleBlocks = %q, want %q", got, want)
	}
}

func TestMissingRuleBlocksReportsDroppedRules(t *testing.T) {
	src := "# T\n\n<!-- seat:begin -->\n- Never do kept.\n<!-- seat:end -->\n- Never do dropped.\n"
	digest, err := generate(src)
	if err != nil {
		t.Fatal(err)
	}
	missing := missingRuleBlocks(src, digest)
	if len(missing) != 1 || missing[0] != "- Never do dropped." {
		t.Fatalf("missingRuleBlocks = %q, want the dropped rule only", missing)
	}
}

func TestRunGeneratorCheckDetectsDriftAndWriteConverges(t *testing.T) {
	root := t.TempDir()
	src := "# T\n\n<!-- seat:begin -->\n- Never do x.\n<!-- seat:end -->\n"
	if err := os.WriteFile(filepath.Join(root, agentsPath), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err == nil {
		t.Fatal("check passed with no committed digest")
	}
	if err := runGenerator(generatorOptions{Root: root}); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err != nil {
		t.Fatalf("fresh check: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, digestPath), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err == nil || !strings.Contains(err.Error(), digestPath) {
		t.Fatalf("stale check error = %v", err)
	}
}

func TestRunGeneratorRefusesADigestThatDropsARule(t *testing.T) {
	cases := map[string]struct{ dropped, rule string }{
		"one line":         {dropped: "- You must do y.\n", rule: "You must do y"},
		"wrapped do not":   {dropped: "- Agents do\n  not push to main.\n", rule: "not push to main"},
		"wrapped hard ban": {dropped: "Treat the remote cache as a hard\nban for local experiments.\n", rule: "ban for local experiments"},
		"emphasized":       {dropped: "- Agents do **not** push to main.\n", rule: "do **not** push to main"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			src := "# T\n\n<!-- seat:begin -->\n- Never do x.\n<!-- seat:end -->\n\n" + tc.dropped
			if err := os.WriteFile(filepath.Join(root, agentsPath), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			err := runGenerator(generatorOptions{Root: root})
			if err == nil || !strings.Contains(err.Error(), tc.rule) {
				t.Fatalf("runGenerator error = %v, want the dropped rule named", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, digestPath)); !os.IsNotExist(statErr) {
				t.Fatalf("runGenerator wrote a digest that drops a rule (stat err %v)", statErr)
			}
			// -check refuses a committed digest of the marked regions alone.
			digest, err := generate(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeFileAtomic(filepath.Join(root, digestPath), []byte(digest)); err != nil {
				t.Fatal(err)
			}
			if err := runGenerator(generatorOptions{Root: root, Check: true}); err == nil || !strings.Contains(err.Error(), tc.rule) {
				t.Fatalf("check error = %v, want the dropped rule named", err)
			}
		})
	}
}

func TestRunGeneratorAcceptsAFencedMarkerExampleWithARule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, agentsPath), []byte(fencedMarkerAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root}); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestCommittedSeatDigestIsFreshAndKeepsEveryRule(t *testing.T) {
	root := repoRoot(t)
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedSeatDigestWithinBudget(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), digestPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > digestBudgetBytes {
		t.Errorf("%s is %d bytes, budget is %d: mark less of AGENTS.md for seats", digestPath, len(data), digestBudgetBytes)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate repository")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}
