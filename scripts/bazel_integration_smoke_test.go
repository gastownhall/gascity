package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// shardScriptTests returns the test names in the bash array name of
// scripts/test-integration-shard.
func shardScriptTests(t *testing.T, script, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\n` + regexp.QuoteMeta(name) + `=\(\n(.*?)\n\)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("scripts/test-integration-shard has no %s array", name)
	}
	var tests []string
	for _, line := range strings.Split(m[1], "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			tests = append(tests, line)
		}
	}
	if len(tests) == 0 {
		t.Fatalf("scripts/test-integration-shard %s is empty", name)
	}
	return tests
}

// TestIntegrationSmokeLaneMatchesShardScript: bazel.yml's gating
// integration-smoke lane (.bazelrc test:integration-smoke over
// //test/integration:integration_test) runs exactly the tests
// scripts/test-integration-shard names as its bdstore and rest-smoke shards
// (bdstore_tests, rest_smoke_tests), so a test added to or dropped from
// either list cannot silently leave the gating set.
func TestIntegrationSmokeLaneMatchesShardScript(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, "scripts/test-integration-shard")
	tests := append(shardScriptTests(t, script, "bdstore_tests"), shardScriptTests(t, script, "rest_smoke_tests")...)
	want := "^(" + strings.Join(tests, "|") + ")$"
	if got := bazelRCFlagValue(readFile(t, root, ".bazelrc"), "test:integration-smoke --test_filter"); got != want {
		t.Errorf(".bazelrc test:integration-smoke --test_filter\n  %s\nwant (from scripts/test-integration-shard)\n  %s", got, want)
	}
	if got, want := multiLaneCommands["integration-smoke"], "test --config=ci --config=integration-smoke --keep_going //test/integration:integration_test"; got != want {
		t.Errorf("integration-smoke lane command %q, want %q", got, want)
	}
	for _, name := range tests {
		if !regexp.MustCompile(`^Test\w+$`).MatchString(name) {
			t.Errorf("shard test name %q is not a plain top-level test name (it goes into a --test_filter regexp)", name)
		}
	}
}

// TestReviewFormulasRunTheShardScriptsFormulaTestsUnderBazel:
// review-formulas.yml runs scripts/test-integration-shard's formula_tests
// (the review-formulas-* shards) as one Bazel run of
// //test/integration:integration_test under .bazelrc
// test:integration-review-formulas, never as go test on the runner.
func TestReviewFormulasRunTheShardScriptsFormulaTestsUnderBazel(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, "scripts/test-integration-shard")
	want := "^(" + strings.Join(shardScriptTests(t, script, "formula_tests"), "|") + ")$"
	if got := bazelRCFlagValue(readFile(t, root, ".bazelrc"), "test:integration-review-formulas --test_filter"); got != want {
		t.Errorf(".bazelrc test:integration-review-formulas --test_filter\n  %s\nwant (from scripts/test-integration-shard formula_tests)\n  %s", got, want)
	}

	wf := readCriticalPathWorkflow(t, "review-formulas.yml")
	var runsBazel bool
	for jobName, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "test-integration-shard") || strings.Contains(step.Run, "make test-integration") || strings.Contains(step.Run, "go test") {
				t.Errorf("review-formulas.yml %s step %q runs %q on the runner; the formula tests run under Bazel", jobName, step.Name, strings.TrimSpace(step.Run))
			}
			if strings.Contains(step.Run, "bazel test --config=ci --config=integration-review-formulas") && strings.Contains(step.Run, "//test/integration:integration_test") {
				runsBazel = true
			}
		}
	}
	if !runsBazel {
		t.Error("review-formulas.yml runs no `bazel test --config=ci --config=integration-review-formulas //test/integration:integration_test`")
	}
	if got := wf.Jobs["review-formulas"].Name; got != "Integration / review-formulas" {
		t.Errorf("review-formulas.yml fan-in name = %q, want %q (scripts/prwatchdog watches it)", got, "Integration / review-formulas")
	}
}
