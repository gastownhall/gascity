package main

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestRuleBlocksFindsEveryRuleKeyword(t *testing.T) {
	src := "# T\n\nPlain.\n\n- You MUST do x.\n- Never do y.\n- Do not do z.\n- Don't do w.\n- Hard ban: v.\n- HARD-RULE: u.\n- harmless\n\n| a | never in a table |\n"
	got := ruleBlocks(src)
	want := []string{"- You MUST do x.", "- Never do y.", "- Do not do z.", "- Don't do w.", "- Hard ban: v.", "- HARD-RULE: u.", "| a | never in a table |"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
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
	root := t.TempDir()
	src := "# T\n\n<!-- seat:begin -->\n- Never do x.\n<!-- seat:end -->\n\n- You must do y.\n"
	if err := os.WriteFile(filepath.Join(root, agentsPath), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runGenerator(generatorOptions{Root: root})
	if err == nil || !strings.Contains(err.Error(), "You must do y") {
		t.Fatalf("runGenerator error = %v, want the dropped rule named", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, digestPath)); !os.IsNotExist(statErr) {
		t.Fatalf("runGenerator wrote a digest that drops a rule (stat err %v)", statErr)
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
