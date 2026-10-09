package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The beads topology acceptance rows are gated three times: by a build tag,
// by GC_ACCEPTANCE_TOPOLOGY_MATRIX (the slow rows skip without it), and by
// which Bazel target runs them. Each gate is easy to get wrong silently.
//
// It happened. TestProxiedNativeLifecycle (695 lines) and
// TestProxiedNativeSafety (550 lines) carried //go:build acceptance_a and
// were named by no -run expression in any CI job — so the proxied-native
// lane's entire evidence base ran exactly once, on the author's box: the
// per-crash-shape ping and recover budgets, foreign-root's "0 pings, 0 dolt
// stop", the no-spawn positive control, both no-migrate rows (the
// BD_ALLOW_REMOTE_MIGRATE consent fence) and the author-at-commit pin. A
// regression in any of them would have landed green (council pr2 C-F1).
//
// Bazel's acceptance lane now runs every acceptance_a test:
// //test/acceptance:acceptance_test runs all but the SOLO_TESTS (it skips
// those by name), and :acceptance_solo_tests runs each SOLO_TESTS entry in a
// target of its own. This guard pins the seams that would turn that into a
// green no-op:
//
//   - :acceptance_test's env is ACCEPTANCE_ENV, which sets the topology
//     matrix switch and GC_REQUIRE_ACCEPTANCE_TOOLING (a missing bd, dolt or
//     row precondition fails instead of skipping);
//   - every SOLO_TESTS entry names a top-level test an acceptance_a file in
//     test/acceptance declares (a stale name would run nothing, and skip
//     nothing in :acceptance_test either);
//   - bazel.yml's acceptance lane names both targets.
func TestBeadsTopologyRowsRunInTheBazelAcceptanceLane(t *testing.T) {
	root := repoRoot(t)
	build := readRepoFile(t, root, "test/acceptance/BUILD.bazel")

	env := regexp.MustCompile(`(?ms)^ACCEPTANCE_ENV = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if env == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no ACCEPTANCE_ENV dict")
	}
	for _, want := range []string{
		`"GC_ACCEPTANCE_TOPOLOGY_MATRIX": "1",`,
		`"GC_REQUIRE_ACCEPTANCE_TOOLING": "1",`,
	} {
		if !strings.Contains(env[1], want) {
			t.Errorf("ACCEPTANCE_ENV lacks %s; the beads topology rows would skip into a cached pass:\n%s", want, env[1])
		}
	}
	rule := regexp.MustCompile(`(?ms)^go_test\(\n    name = "acceptance_test",\n.*?^\)`).FindString(build)
	if !strings.Contains(rule, "    env = ACCEPTANCE_ENV,\n") {
		t.Errorf("acceptance_test does not set env = ACCEPTANCE_ENV:\n%s", rule)
	}
	if !strings.Contains(rule, `args = ["-test.skip=^(%s)$$" % "|".join(SOLO_TESTS.values())],`) {
		t.Errorf("acceptance_test must skip exactly the SOLO_TESTS, which run in targets of their own:\n%s", rule)
	}

	solo := regexp.MustCompile(`(?ms)^SOLO_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if solo == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no SOLO_TESTS dict")
	}
	entries := regexp.MustCompile(`(?m)^    "([a-z0-9_]+_test)": "(Test[A-Za-z0-9_]+)",$`).FindAllStringSubmatch(solo[1], -1)
	if len(entries) == 0 {
		t.Fatal("SOLO_TESTS has no entries; the scan is broken")
	}

	declared := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(root, "test", "acceptance", "*_test.go"))
	if err != nil {
		t.Fatalf("glob test/acceptance: %v", err)
	}
	for _, path := range files {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.HasPrefix(string(body), "//go:build acceptance_a\n") {
			continue
		}
		for _, name := range topLevelTestFunctions(string(body)) {
			declared[name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no acceptance_a test functions found in test/acceptance; the scan is broken")
	}
	for _, entry := range entries {
		if !declared[entry[2]] {
			t.Errorf("SOLO_TESTS %s names %s, which no acceptance_a file in test/acceptance declares: "+
				"the target runs nothing", entry[1], entry[2])
		}
	}

	workflow := readRepoFile(t, root, ".github/workflows/bazel.yml")
	lane := regexp.MustCompile(`(?m)^\s*acceptance='(.*)'$`).FindStringSubmatch(workflow)
	if lane == nil {
		t.Fatal("bazel.yml defines no acceptance lane")
	}
	for _, target := range []string{"//test/acceptance:acceptance_test", "//test/acceptance:acceptance_solo_tests"} {
		if !strings.Contains(lane[1], " "+target) {
			t.Errorf("bazel.yml's acceptance lane does not run %s:\n%s", target, lane[1])
		}
	}
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // a fixed path inside the repo
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// runPattern matches the body of a `-run '<expr>'` argument. CI writes every
// one of them single-quoted, which is what keeps this a text scan rather than
// a shell parser.
var runPattern = regexp.MustCompile(`-run\s+'([^']*)'`)

func collectRunExpressions(workflow string) []string {
	var out []string
	for _, match := range runPattern.FindAllStringSubmatch(workflow, -1) {
		out = append(out, match[1])
	}
	return out
}

// testFuncPattern matches a top-level Go test function declaration. The anchor
// is the line start, so a method or a nested closure cannot be mistaken for
// one.
var testFuncPattern = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(t \*testing\.T\)`)

func topLevelTestFunctions(body string) []string {
	var out []string
	for _, match := range testFuncPattern.FindAllStringSubmatch(body, -1) {
		out = append(out, match[1])
	}
	return out
}

// acceptanceWorkflowDoc is the part of a workflow the env-gate guard reads.
type acceptanceWorkflowDoc struct {
	Env  map[string]any `yaml:"env"`
	Jobs map[string]struct {
		Env   map[string]any `yaml:"env"`
		Steps []struct {
			Env map[string]any `yaml:"env"`
			Run string         `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestAcceptancePerfGateHasALane is round3 review (completeness).
//
// TestBeadsProxiedDefaultNativeLane gates the proxied-native lane's
// `gc status --json` wall clock only when GC_ACCEPTANCE_PERF is set, because
// wall clock on a shared PR runner is a statement about the runner. The plan leaves the number
// to "the GC_ACCEPTANCE_PERF nightly lane" — and nothing anywhere set the
// variable: no workflow, no Makefile target, and `make test-acceptance` runs
// under `env -i`, which dropped it even when a developer exported it. So a flag-on
// `gc status` ten times slower passed every job, nightly included.
//
// This is the same shape of guard as TestProxiedAcceptanceFunctionsAreSelectedByCI,
// one level finer: an env gate is a gate only if some job both sets it and
// selects the test that reads it.
func TestAcceptancePerfGateHasALane(t *testing.T) {
	const (
		gate     = "GC_ACCEPTANCE_PERF"
		testName = "TestBeadsProxiedDefaultNativeLane"
	)
	root := repoRoot(t)
	set := func(values ...map[string]any) bool {
		for _, env := range values {
			if value, ok := env[gate]; ok && strings.TrimSpace(fmt.Sprint(value)) != "" {
				return true
			}
		}
		return false
	}

	var lanes []string
	for _, workflow := range []string{"ci.yml", "nightly.yml"} {
		body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", workflow)) //nolint:gosec // a fixed path inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		var doc acceptanceWorkflowDoc
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parse %s: %v", workflow, err)
		}
		if len(doc.Jobs) == 0 {
			t.Fatalf("%s declares no jobs; the scan is broken", workflow)
		}
		for name, job := range doc.Jobs {
			for _, step := range job.Steps {
				if !set(doc.Env, job.Env, step.Env) {
					continue
				}
				for _, expr := range collectRunExpressions(step.Run) {
					if strings.Contains(expr, testName) {
						lanes = append(lanes, workflow+":"+name)
					}
				}
			}
		}
	}
	if len(lanes) == 0 {
		t.Errorf("no workflow job sets %s and runs %s, so the proxied-native `gc status` wall-clock gate "+
			"(assertProxiedNativePerfHeadroom) is enforced nowhere: add the variable to the job that is "+
			"meant to be its lane", gate, testName)
	}

	// And the local seam: TEST_ENV is `env -i`, so a variable the recipe does
	// not name never reaches `go test`.
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := ""
	lines := strings.Split(string(makefile), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "test-acceptance-go:") && i+1 < len(lines) {
			recipe = lines[i+1]
			break
		}
	}
	if recipe == "" {
		t.Fatal("the Makefile has no test-acceptance-go recipe; the scan is broken")
	}
	if !strings.Contains(recipe, gate+"=") {
		t.Errorf("`make test-acceptance-go` does not pass %s through its env -i allowlist, so exporting it "+
			"runs the suite with the perf gate silently off:\n%s", gate, recipe)
	}
}

// inRowSkipPattern matches a call that skips a test from inside it: t.Skip,
// t.Skipf, t.SkipNow on any receiver. A line whose code is commented out does
// not count.
var inRowSkipPattern = regexp.MustCompile(`\.Skip(f|Now)?\(`)

// TestProxiedAcceptanceRowsNeverSkipInRow is round4's missed completeness low.
//
// Every function in these files runs in Bazel's required acceptance lane,
// which sets GC_REQUIRE_ACCEPTANCE_TOOLING (test/acceptance/BUILD.bazel's
// ACCEPTANCE_ENV), and that switch is what turns a missing precondition into
// a failure. An in-row t.Skip bypasses it: no-migrate-behind-ignored (the only
// acceptance proof that an exported BD_ALLOW_REMOTE_MIGRATE never reaches the
// library on the ignored lane) and dead-record-one-ping each carried one, and
// on a bd that produced their skip condition the job reported success with the
// row unrun, visible only in a step summary nothing gates on. A row whose
// precondition is missing calls helpers.MissingPrecondition (or
// MissingTooling), which skips locally and fails under the switch.
func TestProxiedAcceptanceRowsNeverSkipInRow(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "test", "acceptance", "beads_proxied_*_test.go"))
	if err != nil {
		t.Fatalf("glob the proxied acceptance files: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no test/acceptance/beads_proxied_*_test.go files found; the guard has nothing to guard")
	}
	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if inRowSkipPattern.MatchString(code) {
				t.Errorf("%s:%d skips from inside a row that runs in a required job under GC_REQUIRE_ACCEPTANCE_TOOLING:\n\t%s\n"+
					"call helpers.MissingPrecondition instead, so the job fails rather than passing with the row unrun",
					filepath.Base(path), n+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestTopologyMatrixPhasesCoverEverySubtestOnce guards the split of
// TestBeadsInitTopologyMatrix across several solo targets
// (test/acceptance/BUILD.bazel TOPOLOGY_PHASES): each target narrows the
// matrix to TOPOLOGY_SETUP_PHASES plus its own phases with a subtest
// pattern, and Go's -test.run skips a subtest it does not match without a
// word. So a subtest added to the matrix and to no target's phase list, or a
// phase renamed in the test but not here, would quietly stop running. This
// pins that every t.Run name in the matrix is a setup phase or belongs to
// exactly one target, that every name listed is one the matrix declares, and
// that every TOPOLOGY_PHASES target is a SOLO_TESTS entry for the matrix.
func TestTopologyMatrixPhasesCoverEverySubtestOnce(t *testing.T) {
	root := repoRoot(t)
	build := readRepoFile(t, root, "test/acceptance/BUILD.bazel")
	quoted := regexp.MustCompile(`"([a-z0-9-]+)"`)

	setupBlock := regexp.MustCompile(`(?ms)^TOPOLOGY_SETUP_PHASES = \[\n(.*?)^\]`).FindStringSubmatch(build)
	if setupBlock == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no TOPOLOGY_SETUP_PHASES list")
	}
	setup := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(setupBlock[1], -1) {
		setup[m[1]] = true
	}

	phasesBlock := regexp.MustCompile(`(?ms)^TOPOLOGY_PHASES = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if phasesBlock == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no TOPOLOGY_PHASES dict")
	}
	owner := map[string]string{}
	var targets []string
	for _, entry := range regexp.MustCompile(`(?ms)^    "([a-z0-9_]+_test)": \[(.*?)\],$`).FindAllStringSubmatch(phasesBlock[1], -1) {
		targets = append(targets, entry[1])
		for _, m := range quoted.FindAllStringSubmatch(entry[2], -1) {
			if prev, ok := owner[m[1]]; ok {
				t.Errorf("subtest %s is in both %s and %s; each runs in exactly one target", m[1], prev, entry[1])
			}
			if setup[m[1]] {
				t.Errorf("subtest %s is a TOPOLOGY_SETUP_PHASES step (every target runs it) and also %s's phase", m[1], entry[1])
			}
			owner[m[1]] = entry[1]
		}
	}
	if len(targets) < 2 {
		t.Fatalf("TOPOLOGY_PHASES has %d targets; the scan is broken", len(targets))
	}
	solo := regexp.MustCompile(`(?ms)^SOLO_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if solo == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no SOLO_TESTS dict")
	}
	for _, target := range targets {
		if !strings.Contains(solo[1], `    "`+target+`": "TestBeadsInitTopologyMatrix",`) {
			t.Errorf("TOPOLOGY_PHASES target %s is not a SOLO_TESTS entry for TestBeadsInitTopologyMatrix", target)
		}
	}

	body := readRepoFile(t, root, "test/acceptance/beads_topology_matrix_test.go")
	start := strings.Index(body, "func TestBeadsInitTopologyMatrix(t *testing.T) {")
	if start < 0 {
		t.Fatal("beads_topology_matrix_test.go declares no TestBeadsInitTopologyMatrix")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("TestBeadsInitTopologyMatrix is not closed; the scan is broken")
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`t\.Run\("([a-z0-9-]+)"`).FindAllStringSubmatch(body[start:start+end], -1) {
		declared[m[1]] = true
		if !setup[m[1]] && owner[m[1]] == "" {
			t.Errorf("TestBeadsInitTopologyMatrix subtest %s is in no TOPOLOGY_PHASES target and no setup phase: "+
				"no Bazel target runs it", m[1])
		}
	}
	if len(declared) < 5 {
		t.Fatalf("found %d t.Run names in TestBeadsInitTopologyMatrix; the scan is broken", len(declared))
	}
	for name := range setup {
		if !declared[name] {
			t.Errorf("TOPOLOGY_SETUP_PHASES names %s, which TestBeadsInitTopologyMatrix does not declare", name)
		}
	}
	for name, target := range owner {
		if !declared[name] {
			t.Errorf("TOPOLOGY_PHASES %s names %s, which TestBeadsInitTopologyMatrix does not declare: the target runs less than it says", target, name)
		}
	}
}
