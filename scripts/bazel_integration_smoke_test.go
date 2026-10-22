package scripts_test

import (
	"regexp"
	"slices"
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

// goVariantTestBlock returns the go_variant_test(name = name, ...) text in
// build (read from path), or "" if there is none.
func goVariantTestBlock(build, name string) string {
	m := regexp.MustCompile(`(?s)go_variant_test\(\n\s*name = "` + regexp.QuoteMeta(name) + `",.*?\n\)\n`).FindString(build)
	return m
}

// integrationSmokeTestArgs returns the "-test.run=..." value of
// test/integration:integration_smoke_test's args attribute.
func integrationSmokeTestArgs(t *testing.T, root string) string {
	t.Helper()
	build := readFile(t, root, "test/integration/BUILD.bazel")
	m := regexp.MustCompile(`(?s)name = "integration_smoke_test",\n\s*args = \["-test\.run=(.*?)"\],`).FindStringSubmatch(build)
	if m == nil {
		t.Fatalf("test/integration/BUILD.bazel integration_smoke_test has no args = [\"-test.run=...\"]")
	}
	return m[1]
}

// validateIntegrationSmokeVariant checks block (a go_variant_test(name =
// "integration_smoke_test", ...) text, as goVariantTestBlock(build,
// "integration_smoke_test") returns) against the invariants GA-1 pins:
//   - test must point at :integration_test, the wrapped go_test. Pointing it
//     at :integration_unsharded_test, say, would give 14 green shards that
//     run none of the smoke tests (the unsharded target carries none of
//     them), and the gate would still pass.
//   - tags must include "manual". Without it, bazel test //... (the unit
//     lane) and coverage would each pick up 14 more no-op shards (the
//     direct :integration_test target already covers the compile; running
//     the variant there adds nothing but cost).
//   - shard_count must be 14, the same sharding integration_test's own
//     TestRunner actions use (scripts/bazel_integration_packages_budget_test.go
//     is this repo's precedent for pinning a shard_count the same way).
//   - args must carry the -test.run filter wantFilter builds
//     (TestIntegrationSmokeLaneMatchesShardScript derives it from
//     scripts/test-integration-shard).
//
// It is a pure function of its input text (no *testing.T), so mutation
// proofs can feed it a hand-built block and check the errors it returns,
// without touching the repository's own BUILD.bazel.
func validateIntegrationSmokeVariant(block, wantFilter string) []string {
	var errs []string
	if block == "" {
		return []string{"no go_variant_test(name = \"integration_smoke_test\", ...) block"}
	}
	if m := regexp.MustCompile(`\n\s*test = "([^"]*)",`).FindStringSubmatch(block); m == nil {
		errs = append(errs, "no test = \"...\" attribute")
	} else if m[1] != ":integration_test" {
		errs = append(errs, `test = "`+m[1]+`", want ":integration_test"`)
	}
	if m := regexp.MustCompile(`\n\s*tags = \[([^]]*)\],`).FindStringSubmatch(block); m == nil {
		errs = append(errs, "no tags = [...] attribute")
	} else if !slices.Contains(splitQuotedList(m[1]), "manual") {
		errs = append(errs, `tags = [`+m[1]+`], want "manual" among them`)
	}
	if m := regexp.MustCompile(`\n\s*shard_count = (\d+),`).FindStringSubmatch(block); m == nil {
		errs = append(errs, "no shard_count = <N> attribute")
	} else if m[1] != "14" {
		errs = append(errs, "shard_count = "+m[1]+", want 14")
	}
	if m := regexp.MustCompile(`\n\s*args = \["-test\.run=(.*?)"\],`).FindStringSubmatch(block); m == nil {
		errs = append(errs, "no args = [\"-test.run=...\"] attribute")
	} else if m[1] != wantFilter {
		errs = append(errs, "args -test.run="+m[1]+", want "+wantFilter)
	}
	return errs
}

// splitQuotedList splits a Starlark list body (the inside of tags = [...])
// into its quoted string elements.
func splitQuotedList(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestIntegrationSmokeLaneMatchesShardScript: bazel.yml's gating
// integration-smoke lane (test/integration/BUILD.bazel
// integration_smoke_test, a go_variant_test over integration_test whose
// args filters) runs exactly the tests scripts/test-integration-shard names
// as its bdstore and rest-smoke shards (bdstore_tests, rest_smoke_tests), so
// a test added to or dropped from either list cannot silently leave the
// gating set.
func TestIntegrationSmokeLaneMatchesShardScript(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, "scripts/test-integration-shard")
	tests := append(shardScriptTests(t, script, "bdstore_tests"), shardScriptTests(t, script, "rest_smoke_tests")...)
	// $$-escaped in the BUILD file (go_variant_test's args is a plain
	// Starlark string, not a %-format, but this repo writes its go_variant_test
	// end anchors as $$ for consistency with the generated ones elsewhere).
	want := "^(" + strings.Join(tests, "|") + ")$$"
	if got := integrationSmokeTestArgs(t, root); got != want {
		t.Errorf("test/integration/BUILD.bazel integration_smoke_test args\n  %s\nwant (from scripts/test-integration-shard)\n  %s", got, want)
	}
	if got, want := multiLaneCommands["integration-smoke"], "test --config=ci --config=integration --keep_going //test/integration:integration_smoke_test"; got != want {
		t.Errorf("integration-smoke lane command %q, want %q", got, want)
	}
	for _, name := range tests {
		if !regexp.MustCompile(`^Test\w+$`).MatchString(name) {
			t.Errorf("shard test name %q is not a plain top-level test name (it goes into a -test.run regexp)", name)
		}
	}
}

// TestIntegrationSmokeVariantIsFullyPinned: the go_variant_test block itself
// (test, tags, shard_count), not just its args filter, has to be pinned: see
// validateIntegrationSmokeVariant's doc comment for what each attribute
// guards against.
func TestIntegrationSmokeVariantIsFullyPinned(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, "scripts/test-integration-shard")
	tests := append(shardScriptTests(t, script, "bdstore_tests"), shardScriptTests(t, script, "rest_smoke_tests")...)
	wantFilter := "^(" + strings.Join(tests, "|") + ")$$"

	build := readFile(t, root, "test/integration/BUILD.bazel")
	block := goVariantTestBlock(build, "integration_smoke_test")
	if errs := validateIntegrationSmokeVariant(block, wantFilter); len(errs) > 0 {
		t.Errorf("test/integration/BUILD.bazel integration_smoke_test:\n  %s", strings.Join(errs, "\n  "))
	}
}

// goodIntegrationSmokeVariantBlock is a minimal go_variant_test block that
// satisfies validateIntegrationSmokeVariant, for the mutation proofs below to
// mutate one attribute at a time from.
const goodIntegrationSmokeVariantBlock = `go_variant_test(
    name = "integration_smoke_test",
    args = ["-test.run=^(TestA|TestB)$$"],
    shard_count = 14,
    tags = ["manual"],
    test = ":integration_test",
)
`

// TestIntegrationSmokeVariantGuardCatchesMutations: validateIntegrationSmokeVariant's
// own bite. Each case starts from goodIntegrationSmokeVariantBlock (which the
// guard accepts) and mutates exactly the thing #7798's should-fix list named,
// then checks the guard catches it; without this, an edit to
// validateIntegrationSmokeVariant's regexes could defang the whole check
// silently, the same risk check-split-topology-rows_test.go names for its own
// guard.
func TestIntegrationSmokeVariantGuardCatchesMutations(t *testing.T) {
	wantFilter := "^(TestA|TestB)$$"
	if errs := validateIntegrationSmokeVariant(goodIntegrationSmokeVariantBlock, wantFilter); len(errs) > 0 {
		t.Fatalf("the fixture itself must pass the guard: %v", errs)
	}

	for _, tt := range []struct {
		name, mutate string
	}{
		{
			// Today: 14 green shards that run none of the smoke tests,
			// because :integration_unsharded_test carries no gotags-select
			// and none of the filtered tests.
			name:   "test points at the unsharded target instead of integration_test",
			mutate: strings.Replace(goodIntegrationSmokeVariantBlock, `test = ":integration_test",`, `test = ":integration_unsharded_test",`, 1),
		},
		{
			// Without "manual", bazel test //... and coverage would each
			// pick up 14 more no-op shards of the same tests
			// :integration_test's own shards already run.
			name:   "tags drops manual",
			mutate: strings.Replace(goodIntegrationSmokeVariantBlock, `tags = ["manual"],`, `tags = [],`, 1),
		},
		{
			name:   "shard_count changes",
			mutate: strings.Replace(goodIntegrationSmokeVariantBlock, `shard_count = 14,`, `shard_count = 7,`, 1),
		},
		{
			name:   "args filter changes",
			mutate: strings.Replace(goodIntegrationSmokeVariantBlock, `args = ["-test.run=^(TestA|TestB)$$"],`, `args = ["-test.run=^(TestA)$$"],`, 1),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mutate == goodIntegrationSmokeVariantBlock {
				t.Fatalf("mutation %q did not change the fixture; fix the strings.Replace old string", tt.name)
			}
			if errs := validateIntegrationSmokeVariant(tt.mutate, wantFilter); len(errs) == 0 {
				t.Errorf("validateIntegrationSmokeVariant did not catch: %s", tt.name)
			}
		})
	}
}

// testFilterLanes returns the .bazelrc test:<lane> configs (sorted, deduped)
// that set --test_filter.
func testFilterLanes(rc string) []string {
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^test:(\S+) --test_filter=`).FindAllStringSubmatch(rc, -1) {
		seen[m[1]] = true
	}
	lanes := make([]string, 0, len(seen))
	for lane := range seen {
		lanes = append(lanes, lane)
	}
	slices.Sort(lanes)
	return lanes
}

// testFilterAllowedLanes is the only .bazelrc config allowed to carry a
// global --test_filter: review-formulas.yml's own lane
// (TestReviewFormulasRunTheShardScriptsFormulaTestsUnderBazel), which runs no
// other lane's tests and so cannot filter them away. GA-1 moved
// integration-smoke's filter off --test_filter and onto
// integration_smoke_test's args for exactly the opposite reason: a lane that
// may one day share an invocation with another lane (the GA-2
// integration-gate union) must not carry a global filter.
var testFilterAllowedLanes = []string{"integration-review-formulas"}

// TestOnlyIntegrationReviewFormulasSetsTestFilter: no lane but
// integration-review-formulas may set a global --test_filter in .bazelrc.
func TestOnlyIntegrationReviewFormulasSetsTestFilter(t *testing.T) {
	rc := readFile(t, repoRoot(t), ".bazelrc")
	if got := testFilterLanes(rc); !slices.Equal(got, testFilterAllowedLanes) {
		t.Errorf(".bazelrc --test_filter lanes = %v, want %v", got, testFilterAllowedLanes)
	}
}

// TestOnlyIntegrationReviewFormulasSetsTestFilterGuardCatchesAnotherLane:
// testFilterLanes' own mutation proof.
func TestOnlyIntegrationReviewFormulasSetsTestFilterGuardCatchesAnotherLane(t *testing.T) {
	rc := "test:integration-review-formulas --test_filter=^(TestA)$\n" +
		"test:integration-smoke --test_filter=^(TestB)$\n"
	got := testFilterLanes(rc)
	want := []string{"integration-review-formulas", "integration-smoke"}
	if !slices.Equal(got, want) {
		t.Fatalf("testFilterLanes(%q) = %v, want %v", rc, got, want)
	}
	if slices.Equal(got, testFilterAllowedLanes) {
		t.Errorf("testFilterLanes did not catch the extra lane")
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
