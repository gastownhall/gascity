package scripts_test

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is a static-analysis test over the workflow YAML, in the style
// of ci_merge_queue_test.go (which covers merge_group alone). It extends the
// same question -- does every required context of ciUnionRequiredChecks
// report, or report skipped, rather than never posting at all -- to
// pull_request, both same-repo and from a fork. #9114 (ci-speed-review-
// 2026-11-20.md §5, §11 item 3) was exactly this failure on merge_group: a
// required context ("bazel / split-strict", no longer required) whose
// producing job never started there, so the context stayed pending and the
// queue timed out every entry for 33.4 h. The same class of break can hit a
// fork pull request if a job is gated on a fork-only or same-repo-only
// condition, or hit merge_group again if a future change narrows one of
// today's always-on jobs.
//
// No live endpoint is read: the required-check list is the checked-in
// ciUnionRequiredChecks (ci_required_checks_union_test.go), and the event
// contexts below are literal fixtures, not calls to the GitHub API.

// ciUnionStoreclassYML: the fourth workflow with a required check. The
// other three (bazel.yml, ci.yml, codeql.yml) are mergeQueueWorkflows,
// defined in ci_merge_queue_test.go.
const ciUnionStoreclassYML = ".github/workflows/storeclass-baseline.yml"

// ciUnionRequiredWorkflows: every workflow that produces a context in
// ciUnionRequiredChecks.
var ciUnionRequiredWorkflows = append(slices.Clone(mergeQueueWorkflows), ciUnionStoreclassYML)

// ciUnionRequiredProducers: the job id that reports each required context,
// by its final (matrix-expanded) name. It is a cross-check against what
// the YAML actually names the producer, not the source of truth: the test
// below finds producers by scanning the workflows, then compares.
var ciUnionRequiredProducers = map[string]string{
	// ci.yml
	"Check":         "check",
	"CI / required": "ci-required",
	// codeql.yml (one job, four matrix legs)
	"Analyze (actions)":               "analyze",
	"Analyze (go)":                    "analyze",
	"Analyze (javascript-typescript)": "analyze",
	"Analyze (python)":                "analyze",
	// bazel.yml
	"bazel test (side-by-side)": "gate",
	"BUILD files in sync":       "sync-check",
	// storeclass-baseline.yml
	"storeclass baseline and growth gate": "baseline",
}

// prCtx: an evalGHExpr context for a pull_request run against main, fork
// true for a contributor's fork, false for a same-repo branch. It is
// typed, not the plain map[string]string most of these tests use, because
// github.event.pull_request.head.repo.fork is a real JSON boolean on
// GitHub: compared against the literal `true`, a string "true" never
// matches it (evalGHExprTyped's doc comment, gh_expr_test.go), so a
// string-valued fixture could never fail the "mode a fork never gets"
// check below.
func prCtx(fork bool) map[string]any {
	actor := "alice"
	if fork {
		actor = "alice-fork"
	}
	return map[string]any{
		"github.event_name":                          "pull_request",
		"github.repository":                          mergeQueueRepo,
		"github.repository_owner":                    "gastownhall",
		"github.repository_owner_id":                 gastownhallOwnerID,
		"github.actor":                               actor,
		"github.base_ref":                            "main",
		"github.head_ref":                            "feature/x",
		"github.run_id":                              "515151",
		"github.run_attempt":                         "1",
		"github.event.repository.default_branch":     "main",
		"github.event.pull_request.number":           float64(7001),
		"github.event.pull_request.user.login":       actor,
		"github.event.pull_request.head.repo.fork":   fork,
		"github.event.pull_request.base.ref":         "main",
		"vars.RBE_WEST_WORKERS":                      "true",
		"secrets.RBE_WEST_EXECUTOR":                  "grpcs://executor.invalid",
		"matrix.lane":                                "unit",
		"needs.rbe.outputs.mode":                     "remote",
		"steps.decide.outputs.mode":                  "remote",
		"steps.verify.outcome":                       "success",
		"steps.bazel.outcome":                        "success",
		"steps.llvm.outcome":                         "success",
		"needs.runner-policy.outputs.use_blacksmith": "true",
		"needs.runner-policy.outputs.runner_2vcpu":   "blacksmith-2vcpu-ubuntu-2404",
		"needs.runner-policy.outputs.runner_16vcpu":  "blacksmith-16vcpu-ubuntu-2404",
	}
}

// typedCtx widens a plain string context (mergeGroupCtx, ci_merge_queue_
// test.go) to evalGHExprTyped's map[string]any, so workflowTriggersOn and
// requiredJobReports take one context type across all three events.
func typedCtx(ctx map[string]string) map[string]any {
	typed := make(map[string]any, len(ctx))
	for k, v := range ctx {
		typed[k] = v
	}
	return typed
}

// ciUnionEvents: the three events a pull request's head SHA actually runs
// under, and the context each required check must report (or report
// skipped) on.
func ciUnionEvents() map[string]map[string]any {
	return map[string]map[string]any{
		"pull_request (same-repo)": prCtx(false),
		"pull_request (fork)":      prCtx(true),
		"merge_group":              typedCtx(mergeGroupCtx(nil)),
	}
}

// evalGHIfNeedsTyped is evalGHIfNeeds (ci_merge_queue_test.go) over a typed
// context: a needs.*.outputs.* condition is assumed true, since the
// decision job it reads is tested on its own.
func evalGHIfNeedsTyped(t *testing.T, cond string, ctx map[string]any) bool {
	t.Helper()
	if strings.Contains(cond, "needs.") {
		return true
	}
	if strings.TrimSpace(cond) == "" {
		return true
	}
	v, err := evalGHExprTyped(cond, ctx)
	if err != nil {
		t.Fatalf("evalGHExprTyped(%q): %v", cond, err)
	}
	return ghTruthy(v)
}

// workflowTriggersOn reports whether wf's `on:` block has a trigger key
// matching the event name in ctx. A workflow missing the trigger entirely
// never runs, so every job in it -- including a required-check producer --
// never reports: the context stays pending forever (exactly #9114, one
// level up: there the job existed but never ran; a missing trigger is the
// same failure for the whole workflow at once).
func workflowTriggersOn(wf mqWorkflow, ctx map[string]any) bool {
	_, ok := wf.On[ctx["github.event_name"].(string)]
	return ok
}

// requiredJobReports walks the needs: closure from id and reports whether
// the job that carries id ultimately reports a result (success, failure or
// skipped) on ctx, as opposed to never running at all because its own `if`
// is false.
func requiredJobReports(t *testing.T, wf mqWorkflow, id string, ctx map[string]any) bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(id string) bool {
		if seen[id] {
			return true
		}
		seen[id] = true
		job, ok := wf.Jobs[id]
		if !ok {
			t.Fatalf("job %s does not exist", id)
		}
		if job.If != "" && !evalGHIfNeedsTyped(t, job.If, ctx) {
			return false
		}
		// A needed job that does not run leaves this job skipped by
		// GitHub's default run condition (success()), unless this job's
		// own `if` (checked above) overrides with always() or !canceled().
		// Either way the job still posts a result -- skipped counts as
		// reporting -- so walking needs here is only for job-id validation
		// (a typo'd needs: entry fails loudly), not for its return value.
		for _, n := range job.needs(t) {
			walk(n)
		}
		return true
	}
	return walk(id)
}

// TestPRRequiredChecksAllReportOnEveryRelevantEvent: every context in
// ciUnionRequiredChecks is produced by exactly one job (matrix-expanded for
// codeql.yml), that job's workflow triggers on pull_request and merge_group,
// and the job's own `if` (and the chain of needs: it forces to always()) does
// not evaluate false on a same-repo pull request, a fork pull request, or a
// merge group. A context with no producer, or whose producer can go quiet
// on one of these events, fails here instead of stalling a real pull
// request's merge gate.
func TestPRRequiredChecksAllReportOnEveryRelevantEvent(t *testing.T) {
	workflows := map[string]mqWorkflow{}
	for _, rel := range ciUnionRequiredWorkflows {
		workflows[rel] = readMergeQueueWorkflow(t, rel)
	}

	// producer: required check name -> (workflow, job id).
	type producer struct {
		workflow string
		job      string
	}
	producers := map[string]producer{}
	for rel, wf := range workflows {
		for id, job := range wf.Jobs {
			names := []string{job.Name}
			if strings.Contains(job.Name, "${{ matrix.language }}") {
				names = nil
				var include []map[string]string
				if err := job.Strategy.Matrix.Include.Decode(&include); err != nil {
					t.Fatalf("%s job %s matrix include: %v", rel, id, err)
				}
				for _, entry := range include {
					names = append(names, strings.ReplaceAll(job.Name, "${{ matrix.language }}", entry["language"]))
				}
				// codeql.yml's matrix is a literal list parsed above, not a
				// runtime fromJSON(needs....) expression: every leg exists
				// on every run. bazel.yml's lane job (not a required-check
				// producer) is the one dynamic matrix in these workflows: its
				// set of lanes is a runtime output
				// (needs.rbe.outputs.lanes), so a lane can vanish from a
				// given run's matrix entirely rather than merely being
				// skipped. If a required check ever moves onto it, this
				// test must gain the same check TestRequiredCheckDetector-
				// BitesOnTheKnownBreakClasses proves for an `if`-skipped
				// job: a matrix-absent entry never posts, not even skipped.
			}
			for _, name := range names {
				if !slices.Contains(ciUnionRequiredChecks, name) {
					continue
				}
				if prev, dup := producers[name]; dup {
					t.Errorf("required check %q is produced by both %s:%s and %s:%s", name, prev.workflow, prev.job, rel, id)
					continue
				}
				producers[name] = producer{workflow: rel, job: id}
			}
		}
	}

	for _, name := range ciUnionRequiredChecks {
		p, ok := producers[name]
		if !ok {
			t.Errorf("required check %q is produced by no job of %v; update ciUnionRequiredProducers and the workflow", name, ciUnionRequiredWorkflows)
			continue
		}
		if want, ok := ciUnionRequiredProducers[name]; ok && want != p.job {
			t.Errorf("required check %q is produced by job %s, but ciUnionRequiredProducers says %s; update the list", name, p.job, want)
		}
		wf := workflows[p.workflow]
		for eventLabel, ctx := range ciUnionEvents() {
			if !workflowTriggersOn(wf, ctx) {
				t.Errorf("required check %q: %s has no %s trigger, so it never runs and the context never reports", name, p.workflow, ctx["github.event_name"])
				continue
			}
			if !requiredJobReports(t, wf, p.job, ctx) {
				t.Errorf("required check %q: %s job %s (via its own if, or a needs job it does not force to always()) does not report on %s; a pull request there would wait forever, as #9114 did on merge_group", name, p.workflow, p.job, eventLabel)
			}
		}
	}
}

// --- proving the test bites (no live workflow is mutated; a synthetic
// fixture stands in, so this never depends on, or edits, the checked-in
// YAML) ---

// mqFixtureWorkflow builds a minimal mqWorkflow (one job, "gate") whose
// trigger set and `if` are given, for exercising workflowTriggersOn and
// requiredJobReports the way TestPRRequiredChecksAllReportOnEveryRelevantEvent
// does against the real workflows.
func mqFixtureWorkflow(t *testing.T, onYAML, jobIf string) mqWorkflow {
	t.Helper()
	src := "on:\n" + onYAML + "\njobs:\n  gate:\n    name: gate\n"
	if jobIf != "" {
		src += "    if: " + jobIf + "\n"
	}
	src += "    runs-on: blacksmith-2vcpu-ubuntu-2404\n"
	var wf mqWorkflow
	if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
		t.Fatalf("parse fixture: %v\n%s", err, src)
	}
	return wf
}

// TestRequiredCheckDetectorBitesOnTheKnownBreakClasses proves
// requiredJobReports and workflowTriggersOn actually catch the two breaks
// the review calls out (§11 item 3): a required job whose `if` quietly
// drops it on merge_group, and a job gated on a mode that excludes fork
// pull requests. Both must fail the detector (want false); a fixture the
// detector passes for the wrong reason would let the real test above pass
// silently on a real regression.
func TestRequiredCheckDetectorBitesOnTheKnownBreakClasses(t *testing.T) {
	cases := []struct {
		name   string
		onYAML string
		jobIf  string
		ctx    map[string]any
	}{
		{
			// The #9114 class: a job whose `if` reads only
			// github.event_name and excludes merge_group outright (as
			// opposed to split-strict's real bug, which was never
			// entering the matrix at all -- this is the simpler, and
			// today still possible, version of the same mistake: someone
			// adds "&& github.event_name != 'merge_group'" to a required
			// job's `if` meaning to skip some unrelated setup step, and
			// gets the whole job).
			name:   "if excludes merge_group",
			onYAML: "  pull_request: {}\n  merge_group: {}\n",
			jobIf:  "${{ github.event_name != 'merge_group' }}",
			ctx:    typedCtx(mergeGroupCtx(nil)),
		},
		{
			// A mode forks never get: the job requires same-repo trust
			// (head.repo.fork == false) directly in its own `if`, instead
			// of behind a needs.*.outputs.* decision job (which
			// evalGHIfNeeds treats as still-reporting, since the decision
			// job's own behavior is tested separately).
			name:   "if requires same-repo trust directly",
			onYAML: "  pull_request: {}\n  merge_group: {}\n",
			jobIf:  "${{ github.event.pull_request.head.repo.fork != true }}",
			ctx:    prCtx(true),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := mqFixtureWorkflow(t, tc.onYAML, tc.jobIf)
			if !workflowTriggersOn(wf, tc.ctx) {
				t.Fatalf("fixture trigger setup is wrong: workflowTriggersOn false for %v", tc.ctx["github.event_name"])
			}
			if requiredJobReports(t, wf, "gate", tc.ctx) {
				t.Errorf("requiredJobReports(gate) = true, want false: the detector would miss this break")
			}
		})
	}
}

// TestRequiredCheckDetectorAllowsSkippedToReport proves the other
// direction: a required job that is skipped by an ordinary path gate (as
// credential-provider-windows and pack-gate are, ci.yml's `check` and
// `ci-required` jobs' allow_skipped list) still counts as reporting. A
// detector that flagged every skip, not just a silent non-report, would
// make this test fail on the real workflows today.
func TestRequiredCheckDetectorAllowsSkippedToReport(t *testing.T) {
	wf := mqFixtureWorkflow(t, "  pull_request: {}\n  merge_group: {}\n", "${{ needs.changes.outputs.packs == 'true' }}")
	if !requiredJobReports(t, wf, "gate", prCtx(true)) {
		t.Errorf("requiredJobReports(gate) = false, want true: a needs.*.outputs.* path gate reports skipped, which satisfies a required check")
	}
}
