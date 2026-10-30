package scripts_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The "Mac regression summary" check is meant to become a required check on
// main. These tests pin what that needs from mac-regression.yml: the summary
// fails closed on every tier the gate selected, no tier hides a failure, the
// integration tiers run whether or not the smoke tier passed, every non-docs
// pull request runs the smoke tier, and the check reports on merge-queue
// entries.

// ghNotCancelled is the step condition that runs unless the run was
// canceled, in GitHub Actions' spelling.
const ghNotCancelled = "${{ !cancelled() }}" //nolint:misspell // GitHub Actions spells it cancelled()

func macRegressionStep(t *testing.T, job, id string) ciCriticalPathStep {
	t.Helper()
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("mac-regression.yml has no %s job", job)
	}
	for _, step := range j.Steps {
		if step.ID == id || step.Name == id {
			return step
		}
	}
	t.Fatalf("mac-regression.yml %s has no step %q", job, id)
	return ciCriticalPathStep{}
}

// runMacGateStep runs the gate's decision script with env and returns its
// GITHUB_OUTPUT as a map.
func runMacGateStep(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	step := macRegressionStep(t, "gate", "gate")
	script := strings.ReplaceAll(step.Run, "${{ github.repository }}", "gastownhall/gascity")
	if strings.Contains(script, "${{") {
		t.Fatalf("gate script has an expression this test does not substitute:\n%s", script)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "github_output")
	full := map[string]string{"GITHUB_OUTPUT": out}
	for k, v := range env {
		full[k] = v
	}
	if log, err := runWorkflowStepScript(t, dir, script, full); err != nil {
		t.Fatalf("gate script: %v\n%s", err, log)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	return got
}

func TestMacRegressionGateRunsSmokeOnEveryNonDocsPullRequest(t *testing.T) {
	sameRepoPR := map[string]string{
		"EVENT_NAME":     "pull_request",
		"PR_HEAD_REPO":   "gastownhall/gascity",
		"PR_DRAFT":       "false",
		"NEEDS_LABEL":    "false",
		"FILTER_OUTCOME": "success",
		"CODE_CHANGED":   "true",
	}
	with := func(over map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range sameRepoPR {
			env[k] = v
		}
		for k, v := range over {
			env[k] = v
		}
		return env
	}
	cases := []struct {
		name                  string
		env                   map[string]string
		smoke, full, formulas string
		reason                string
	}{
		{name: "code change", env: sameRepoPR, smoke: "true", full: "false", formulas: "false"},
		{name: "docs-only change", env: with(map[string]string{"CODE_CHANGED": "false"}), smoke: "false", full: "false", formulas: "false", reason: "docs-only"},
		{name: "diff not classified", env: with(map[string]string{"FILTER_OUTCOME": "failure", "CODE_CHANGED": ""}), smoke: "true", full: "false", formulas: "false"},
		{name: "needs-mac label", env: with(map[string]string{"NEEDS_LABEL": "true", "CODE_CHANGED": "false"}), smoke: "true", full: "true", formulas: "false"},
		{name: "draft", env: with(map[string]string{"PR_DRAFT": "true"}), smoke: "false", full: "false", formulas: "false", reason: "draft"},
		{name: "fork", env: with(map[string]string{"PR_HEAD_REPO": "someone/gascity"}), smoke: "false", full: "false", formulas: "false", reason: "fork"},
		{name: "merge group", env: map[string]string{"EVENT_NAME": "merge_group"}, smoke: "false", full: "false", formulas: "false", reason: "merge_group"},
		{name: "nightly", env: map[string]string{"EVENT_NAME": "schedule"}, smoke: "true", full: "true", formulas: "true"},
		{name: "dispatch full", env: map[string]string{"EVENT_NAME": "workflow_dispatch", "SUITE_INPUT": "full"}, smoke: "true", full: "true", formulas: "true"},
		{name: "dispatch needs-mac", env: map[string]string{"EVENT_NAME": "workflow_dispatch", "SUITE_INPUT": "needs-mac"}, smoke: "true", full: "true", formulas: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runMacGateStep(t, tc.env)
			if got["run_smoke"] != tc.smoke || got["run_full"] != tc.full || got["run_review_formulas"] != tc.formulas {
				t.Errorf("gate = smoke %q full %q review-formulas %q (%s), want %q %q %q", got["run_smoke"], got["run_full"], got["run_review_formulas"], got["reason"], tc.smoke, tc.full, tc.formulas)
			}
			if !strings.Contains(got["reason"], tc.reason) {
				t.Errorf("gate reason = %q, want it to mention %q", got["reason"], tc.reason)
			}
		})
	}
}

// The gate classifies a pull request's diff as docs-only or not; anything
// outside the docs trees (and the root's markdown files) runs the smoke tier.
func TestMacRegressionGateClassifiesDocsOnlyDiffs(t *testing.T) {
	filter := macRegressionStep(t, "gate", "filter")
	if got := filter.With["predicate-quantifier"]; got != "every" {
		t.Errorf("gate filter predicate-quantifier = %q, want every (a file is code only when it matches every pattern)", got)
	}
	var filters map[string][]string
	if err := yaml.Unmarshal([]byte(filter.With["filters"]), &filters); err != nil {
		t.Fatalf("gate filters: %v", err)
	}
	want := []string{"**", "!docs/**", "!engdocs/**", "!*.md"}
	if got := filters["code"]; !slices.Equal(got, want) {
		t.Errorf("gate code filter = %q, want %q", got, want)
	}
	decide := macRegressionStep(t, "gate", "gate")
	for name, want := range map[string]string{
		"FILTER_OUTCOME": "${{ steps.filter.outcome }}",
		"CODE_CHANGED":   "${{ steps.filter.outputs.code }}",
	} {
		if got := decide.Env[name]; got != want {
			t.Errorf("gate decision env %s = %q, want %q", name, got, want)
		}
	}
}

// runMacSummary runs the summary script with the gate's selection and each
// tier's result, and reports whether it passed.
func runMacSummary(t *testing.T, env map[string]string) (bool, string) {
	t.Helper()
	step := macRegressionStep(t, "mac-regression-summary", "Summarize")
	if strings.Contains(step.Run, "${{") {
		t.Fatalf("summary script has an inline expression; pass it through env:\n%s", step.Run)
	}
	dir := t.TempDir()
	full := map[string]string{"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary")}
	for k, v := range env {
		full[k] = v
	}
	out, err := runWorkflowStepScript(t, dir, step.Run, full)
	return err == nil, out
}

func TestMacRegressionSummaryFailsClosed(t *testing.T) {
	smokeTiers := []string{"QUALITY", "UNIT", "CMD_GC", "ACCEPTANCE"}
	fullTiers := []string{"COVER", "INT_PACKAGES", "INT_BDSTORE", "INT_REST"}
	run := func(smoke, full, formulas bool, results map[string]string) map[string]string {
		env := map[string]string{
			"GATE_RESULT":         "success",
			"RUN_SMOKE":           map[bool]string{true: "true", false: "false"}[smoke],
			"RUN_FULL":            map[bool]string{true: "true", false: "false"}[full],
			"RUN_REVIEW_FORMULAS": map[bool]string{true: "true", false: "false"}[formulas],
			"REASON":              "test",
		}
		for _, tier := range append(append(slices.Clone(smokeTiers), fullTiers...), "REVIEW_FORMULAS") {
			env[tier] = "skipped"
		}
		if smoke {
			for _, tier := range smokeTiers {
				env[tier] = "success"
			}
		}
		if full {
			for _, tier := range fullTiers {
				env[tier] = "success"
			}
		}
		if formulas {
			env["REVIEW_FORMULAS"] = "success"
		}
		for k, v := range results {
			env[k] = v
		}
		return env
	}
	cases := []struct {
		name string
		env  map[string]string
		pass bool
	}{
		{name: "gate failed", env: map[string]string{"GATE_RESULT": "failure"}},
		{name: "nothing requested", env: run(false, false, false, nil), pass: true},
		{name: "smoke all green", env: run(true, false, false, nil), pass: true},
		{name: "full all green", env: run(true, true, false, nil), pass: true},
		{name: "everything green", env: run(true, true, true, nil), pass: true},
		{name: "required smoke tier skipped", env: run(true, false, false, map[string]string{"QUALITY": "skipped"})},
		{name: "required smoke tier canceled", env: run(true, false, false, map[string]string{"CMD_GC": "cancelled"})}, //nolint:misspell // GitHub Actions job result
		{name: "required smoke tier missing", env: run(true, false, false, map[string]string{"ACCEPTANCE": ""})},
		{name: "smoke tier failed", env: run(true, true, false, map[string]string{"UNIT": "failure"})},
		{name: "test-cover failed", env: run(true, true, false, map[string]string{"COVER": "failure"})},
		{name: "required full tier skipped", env: run(true, true, false, map[string]string{"INT_PACKAGES": "skipped"})},
		{name: "bdstore failed", env: run(true, true, false, map[string]string{"INT_BDSTORE": "failure"})},
		{name: "review-formulas requested and skipped", env: run(true, true, true, map[string]string{"REVIEW_FORMULAS": "skipped"})},
		{name: "review-formulas failed", env: run(true, true, true, map[string]string{"REVIEW_FORMULAS": "failure"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if pass, out := runMacSummary(t, tc.env); pass != tc.pass {
				t.Fatalf("summary passed = %v, want %v\n%s", pass, tc.pass, out)
			}
		})
	}
}

// No tier reports success over a failed step, and none hides behind
// continue-on-error: the summary reads needs.<job>.result for every tier.
func TestMacRegressionTiersDoNotMaskFailures(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	for name, job := range wf.Jobs {
		if job.ContinueOnError {
			t.Errorf("%s is continue-on-error", name)
		}
		for _, step := range job.Steps {
			if step.ContinueOnError && (name != "gate" || step.ID != "filter") {
				t.Errorf("%s step %q is continue-on-error", name, step.Name)
			}
		}
	}
	summarize := macRegressionStep(t, "mac-regression-summary", "Summarize")
	for name, value := range summarize.Env {
		if strings.Contains(value, "outputs.outcome") {
			t.Errorf("summary env %s = %q reads a step outcome; read the job result", name, value)
		}
	}
	for name, want := range map[string]string{
		"RUN_FULL":            "${{ needs.gate.outputs.run_full }}",
		"RUN_REVIEW_FORMULAS": "${{ needs.gate.outputs.run_review_formulas }}",
		"COVER":               "${{ needs.mac-cover.result }}",
		"INT_BDSTORE":         "${{ needs.mac-integration-bdstore.result }}",
		"REVIEW_FORMULAS":     "${{ needs.mac-integration-review-formulas.result }}",
	} {
		if got := summarize.Env[name]; got != want {
			t.Errorf("summary env %s = %q, want %q", name, got, want)
		}
	}
}

// The integration tiers run whether or not the smoke tier passed: a red smoke
// job must not skip (and so hide) the integration results, and the acceptance
// job's bd contract steps run after a Tier A failure.
func TestMacRegressionIntegrationTiersDoNotCascade(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	for _, name := range []string{"mac-integration-packages", "mac-integration-bdstore", "mac-integration-rest", "mac-integration-review-formulas", "mac-cover"} {
		job, ok := wf.Jobs[name]
		if !ok {
			t.Fatalf("mac-regression.yml has no %s job", name)
		}
		if want := []string{"runner-policy", "gate"}; !slices.Equal(job.Needs, want) {
			t.Errorf("%s needs = %v, want %v", name, job.Needs, want)
		}
	}
	for _, step := range wf.Jobs["mac-acceptance"].Steps {
		if strings.Contains(step.Run, "test-bd-cli-contract") && strings.TrimSpace(step.If) != ghNotCancelled {
			t.Errorf("acceptance step %q if = %q, want %s", step.Name, step.If, ghNotCancelled)
		}
	}
}

// A required check must report on merge-queue entries, or every entry waits
// out the queue's response timeout and fails.
func TestMacRegressionReportsOnMergeGroup(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On          map[string]yaml.Node `yaml:"on"`
		Concurrency struct {
			Group string `yaml:"group"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	var mg struct {
		Types []string `yaml:"types"`
	}
	node, ok := wf.On["merge_group"]
	if ok {
		if err := node.Decode(&mg); err != nil {
			t.Fatalf("on.merge_group: %v", err)
		}
	}
	if !ok || !slices.Equal(mg.Types, []string{"checks_requested"}) {
		t.Errorf("on.merge_group = %+v (present %v), want types [checks_requested]", mg, ok)
	}
	// The needs-mac label dispatches its own run for the same pull request.
	// Sharing a group with the pull_request run would cancel that run, whose
	// summary then reports failure on the PR head.
	if !strings.Contains(wf.Concurrency.Group, "github.event_name") {
		t.Errorf("concurrency group %q does not separate events", wf.Concurrency.Group)
	}
}

// make test-mac refuses to pass over an empty package list (a failed go list
// expands MAC_UNIT_PKGS to nothing, and go test then tests nothing).
func TestMakeTestMacRefusesAnEmptyPackageList(t *testing.T) {
	root := repoRoot(t)
	if out, err := runWorkflowStepScript(t, t.TempDir(), "make -s -C '"+root+"' test-mac-pkgs-guard MAC_UNIT_PKGS=\n", nil); err == nil {
		t.Fatalf("test-mac-pkgs-guard passed with no packages\n%s", out)
	}
	if out, err := runWorkflowStepScript(t, t.TempDir(), "make -s -C '"+root+"' test-mac-pkgs-guard MAC_UNIT_PKGS=./internal/fsys\n", nil); err != nil {
		t.Fatalf("test-mac-pkgs-guard failed with a package: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\ntest-mac: test-mac-pkgs-guard ") {
		t.Error("test-mac does not depend on test-mac-pkgs-guard")
	}
}
