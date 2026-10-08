package scripts_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Merge-queue readiness (TESTING.md "Merge queue"). The queue is dormant
// until the main ruleset enables it, but every required check must already
// report on a queue entry's merge-group commit, from a merge_group run that
// has no github.event.pull_request at all: Check and CI / required
// (ci.yml), the four Analyze checks (codeql.yml), "bazel test
// (side-by-side)" and "BUILD files in sync" (bazel.yml). These tests
// simulate that event with evalGHExpr (gh_expr_test.go) and the workflows'
// own step scripts, following gastownhall/beads scripts/ci_merge_queue_test.go.

const (
	mergeQueueRepo      = "gastownhall/gascity"
	mergeQueueBaseSHA   = "1111111111111111111111111111111111111111"
	mergeQueueHeadSHA   = "2222222222222222222222222222222222222222"
	mergeQueueRef       = "refs/heads/gh-readonly-queue/main/pr-7001-" + mergeQueueBaseSHA
	mergeQueueRunID     = "424242"
	mergeQueueBotActor  = "github-merge-queue[bot]"
	mergeQueueCodeQLYML = ".github/workflows/codeql.yml"
	mergeQueueCIYML     = ".github/workflows/ci.yml"
)

// mergeQueueWorkflows: the workflows that carry a required check.
var mergeQueueWorkflows = []string{bazelMultiLaneWorkflow, mergeQueueCIYML, mergeQueueCodeQLYML}

// mergeGroupCtx: an evalGHExpr context for a merge_group run (no
// github.event.pull_request.* key: null on this event), plus extra.
func mergeGroupCtx(extra map[string]string) map[string]string {
	ctx := map[string]string{
		"github.event_name":                          "merge_group",
		"github.repository":                          mergeQueueRepo,
		"github.actor":                               mergeQueueBotActor,
		"github.ref":                                 mergeQueueRef,
		"github.run_id":                              mergeQueueRunID,
		"github.run_attempt":                         "1",
		"github.event.repository.default_branch":     "main",
		"github.event.merge_group.base_sha":          mergeQueueBaseSHA,
		"github.event.merge_group.head_sha":          mergeQueueHeadSHA,
		"github.event.merge_group.base_ref":          "refs/heads/main",
		"github.event.merge_group.head_ref":          mergeQueueRef,
		"vars.RBE_WEST_WORKERS":                      "true",
		"secrets.RBE_WEST_EXECUTOR":                  "grpcs://executor.invalid",
		"matrix.lane":                                "unit",
		"needs.rbe.outputs.mode":                     "remote",
		"steps.decide.outputs.mode":                  "remote",
		"steps.verify.outcome":                       "success",
		"steps.bazel.outcome":                        "success",
		"steps.llvm.outcome":                         "success",
		"needs.runner-policy.outputs.use_blacksmith": "true",
	}
	maps.Copy(ctx, extra)
	return ctx
}

type mqStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type mqJob struct {
	Name     string            `yaml:"name"`
	If       string            `yaml:"if"`
	Needs    yaml.Node         `yaml:"needs"`
	Env      map[string]string `yaml:"env"`
	Steps    []mqStep          `yaml:"steps"`
	Strategy struct {
		Matrix struct {
			// A literal list (codeql.yml) or an expression (bazel.yml's
			// fromJSON of the rbe job's lanes).
			Include yaml.Node `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
}

type mqWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Concurrency map[string]string    `yaml:"concurrency"`
	Jobs        map[string]mqJob     `yaml:"jobs"`
}

func readMergeQueueWorkflow(t *testing.T, rel string) mqWorkflow {
	t.Helper()
	var wf mqWorkflow
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), rel)), &wf); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return wf
}

func (j mqJob) needs(t *testing.T) []string {
	t.Helper()
	var one string
	if j.Needs.Kind == 0 {
		return nil
	}
	if err := j.Needs.Decode(&one); err == nil {
		return []string{one}
	}
	var many []string
	if err := j.Needs.Decode(&many); err != nil {
		t.Fatalf("needs: %v", err)
	}
	return many
}

func mqStepKey(s mqStep) string {
	if s.ID != "" {
		return s.ID
	}
	return s.Name
}

// TestMergeQueueRequiredChecksRunOnMergeGroup: the three workflows run on
// merge_group (exactly checks_requested), every required check is produced
// by a job of theirs, and no producing job, nor any job it needs, has an
// `if` that skips it on a merge group (a skipped required check never
// reports, and the entry waits until the queue's timeout ejects it).
func TestMergeQueueRequiredChecksRunOnMergeGroup(t *testing.T) {
	producers := map[string]string{} // required check -> workflow:job
	workflows := map[string]mqWorkflow{}
	for _, rel := range mergeQueueWorkflows {
		wf := readMergeQueueWorkflow(t, rel)
		workflows[rel] = wf
		mg, ok := wf.On["merge_group"]
		if !ok {
			t.Errorf("%s has no merge_group trigger; a queued PR would wait forever for its required checks", rel)
			continue
		}
		var trig struct {
			Types []string `yaml:"types"`
		}
		if err := mg.Decode(&trig); err != nil || !slices.Equal(trig.Types, []string{"checks_requested"}) {
			t.Errorf("%s merge_group types = %v (%v), want exactly [checks_requested]", rel, trig.Types, err)
		}
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
			}
			for _, name := range names {
				if slices.Contains(gascityRequiredChecks, name) {
					if prev, dup := producers[name]; dup {
						t.Errorf("required check %q is produced by %s and %s:%s", name, prev, rel, id)
					}
					producers[name] = rel + ":" + id
				}
			}
		}
	}
	for _, name := range gascityRequiredChecks {
		p, ok := producers[name]
		if !ok {
			t.Errorf("required check %q is produced by no job of %v", name, mergeQueueWorkflows)
			continue
		}
		rel, id, _ := strings.Cut(p, ":")
		wf := workflows[rel]
		// The producing job and its needs closure run on a merge group.
		seen := map[string]bool{}
		var walk func(id string)
		walk = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			job, ok := wf.Jobs[id]
			if !ok {
				t.Fatalf("%s: job %s (needed by %q) does not exist", rel, id, name)
			}
			if job.If != "" {
				runs := evalGHIfNeeds(t, job.If, mergeGroupCtx(nil))
				// Path-gated jobs (needs.changes.outputs.*) may skip; the
				// fan-in job's allow_skipped list decides, as on a PR.
				if !runs && !strings.Contains(job.If, "needs.") {
					t.Errorf("%s job %s (behind required check %q) has if %q, false on merge_group", rel, id, name, job.If)
				}
			}
			for _, n := range job.needs(t) {
				walk(n)
			}
		}
		walk(id)
		if job := wf.Jobs[id]; job.If != "" && job.If != "always()" && job.If != "${{ always() }}" {
			t.Errorf("%s job %s produces required check %q with if %q; it must always run", rel, id, name, job.If)
		}
	}
}

// evalGHIfNeeds evaluates a job `if`, treating any needs.* output the
// context lacks as the value that runs the job (the decision jobs' outputs
// are tested on their own).
func evalGHIfNeeds(t *testing.T, cond string, ctx map[string]string) bool {
	t.Helper()
	if strings.Contains(cond, "needs.") {
		return true
	}
	return evalGHIf(t, cond, ctx)
}

var mqEventRef = regexp.MustCompile(`github\.event_name|github\.event\.|github\.base_ref|github\.head_ref`)

// mergeQueueEventExprs: every expression in the three workflows that reads
// the event (its name, its payload, base_ref/head_ref), keyed
// "<file> <location>", and the value it yields on a merge_group run. A new
// one fails TestMergeQueueEventExpressionsAreAccountedFor until it is listed
// here with its merge_group value: no github.event.pull_request.* read
// without a fallback that keeps the merge group on the same-repo path.
var mergeQueueEventExprs = map[string]string{
	// bazel.yml
	"bazel.yml concurrency.group":                                                          "bazel-yml-" + mergeQueueRef, // the queue ref: unique per entry and base
	"bazel.yml concurrency.cancel-in-progress":                                             "false",                      // never cancel a queue run
	"bazel.yml jobs.rbe.steps[decide].env.PULL_REQUEST":                                    "false",                      // same-repo path...
	"bazel.yml jobs.rbe.steps[decide].env.FORK":                                            "false",                      // ...never the mint
	"bazel.yml jobs.rbe.steps[decide].env.PR_NUMBER":                                       "",                           // read in the fork arm only
	"bazel.yml jobs.rbe.steps[lanes].env.EVENT":                                            "merge_group",                // the PR lane set
	"bazel.yml jobs.rbe.steps[base].if":                                                    "false",                      // the queue ref is already the merge
	"bazel.yml jobs.rbe.steps[base].env.BASE_REF":                                          "",                           // (step skipped)
	"bazel.yml jobs.rbe.steps[Pre-warm the OSS worker pool (rbe-west)].env.DEFAULT_BRANCH": "main",
	"bazel.yml jobs.lane.steps[worker-env].env.DEFAULT_BRANCH":                             "main",
	"bazel.yml jobs.lane.steps[bazel].env.RBE_FORK_PR":                                     "", // fork modes only
	"bazel.yml jobs.lane.steps[OpenAPI breaking-change base spec].if":                      "true",
	"bazel.yml jobs.lane.steps[OpenAPI breaking-change base spec].env.BASE_SHA":            mergeQueueBaseSHA,
	"bazel.yml jobs.lane.steps[ci-analytics].env.PR_HINT":                                  "0",
	"bazel.yml jobs.lane.steps[Save LLVM archive cache].if":                                "false",       // push to main saves
	"bazel.yml jobs.lane.steps[Save Bazel runner cache].if":                                "false",       // push to main saves
	"bazel.yml jobs.gate.steps[Evaluate].env.EVENT":                                        "merge_group", // logged only
	"bazel.yml jobs.coverage.if":                                                           "false",       // nightly/dispatch only
	"bazel.yml jobs.coverage.steps[worker-env].env.DEFAULT_BRANCH":                         "main",
	"bazel.yml jobs.coverage.steps[ci-analytics].env.PR_HINT":                              "0",
	// ci.yml
	"ci.yml concurrency.group":                               "ci-merge_group-" + mergeQueueRef,
	"ci.yml concurrency.cancel-in-progress":                  "false",
	"ci.yml jobs.runner-policy.steps[policy].env.EVENT_NAME": "merge_group",
	"ci.yml jobs.runner-policy.steps[policy].env.PR_AUTHOR":  "", // runner_policy.py ignores it
	"ci.yml jobs.changes.steps[filter].with.base":            mergeQueueBaseSHA,
	"ci.yml jobs.changes.steps[filter].with.ref":             mergeQueueHeadSHA,
	// codeql.yml
	"codeql.yml jobs.runner-policy.steps[policy].env.EVENT_NAME": "merge_group",
	"codeql.yml jobs.runner-policy.steps[policy].env.PR_AUTHOR":  "",
	"codeql.yml jobs.analyze.steps[Save Go build cache].if":      "false", // push to main saves
}

// TestMergeQueueEventExpressionsAreAccountedFor evaluates every
// event-reading expression of the three workflows on a merge_group run and
// checks it against mergeQueueEventExprs, in both directions.
func TestMergeQueueEventExpressionsAreAccountedFor(t *testing.T) {
	ctx := mergeGroupCtx(nil)
	got := map[string]string{}
	add := func(key, expr string, isIf bool) {
		if !mqEventRef.MatchString(expr) {
			return
		}
		if _, dup := got[key]; dup {
			t.Errorf("two event expressions at %s; give the steps ids", key)
		}
		if isIf {
			got[key] = fmt.Sprint(evalGHIf(t, expr, ctx))
			return
		}
		got[key] = interpolateGH(t, expr, ctx)
	}
	for _, rel := range mergeQueueWorkflows {
		wf := readMergeQueueWorkflow(t, rel)
		file := strings.TrimPrefix(rel, ".github/workflows/")
		for k, v := range wf.Concurrency {
			add(file+" concurrency."+k, v, false)
		}
		for id, job := range wf.Jobs {
			prefix := file + " jobs." + id
			add(prefix+".if", job.If, true)
			for k, v := range job.Env {
				add(prefix+".env."+k, v, false)
			}
			for _, step := range job.Steps {
				sp := prefix + ".steps[" + mqStepKey(step) + "]"
				add(sp+".if", step.If, true)
				for k, v := range step.Env {
					add(sp+".env."+k, v, false)
				}
				for k, v := range step.With {
					add(sp+".with."+k, v, false)
				}
				if strings.Contains(step.Run, "github.event.pull_request") || strings.Contains(step.Run, "github.base_ref") || strings.Contains(step.Run, "github.head_ref") {
					t.Errorf("%s run interpolates an event field a merge group lacks; pass it through env", sp)
				}
			}
		}
	}
	keys := slices.Sorted(maps.Keys(got))
	for _, k := range keys {
		want, ok := mergeQueueEventExprs[k]
		if !ok {
			t.Errorf("unlisted event expression %s = %q on merge_group; add it to mergeQueueEventExprs once its merge_group value is right", k, got[k])
			continue
		}
		if got[k] != want {
			t.Errorf("%s = %q on merge_group, want %q", k, got[k], want)
		}
	}
	var stale []string
	for k := range mergeQueueEventExprs {
		if _, ok := got[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("mergeQueueEventExprs lists %s, which no workflow has any more", k)
	}
}

// TestMergeQueueRunsOnTheSameRepoPath: a merge group is same-repo trust
// (only people who can merge can queue). bazel.yml's decide step, fed the
// env its expressions yield on merge_group, takes mode remote without asking
// the mint, also when the queueing actor is Dependabot; fresh-merge (a
// pull_request-only merge) is a no-op, and the gate passes a merge group's
// green run.
func TestMergeQueueRunsOnTheSameRepoPath(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := multiLaneRBEStep(t, wf, "decide")
	for _, actor := range []string{mergeQueueBotActor, "dependabot[bot]", "alice"} {
		env := map[string]string{}
		for k, v := range step.Env {
			env[k] = interpolateGH(t, v, mergeGroupCtx(map[string]string{"github.actor": actor}))
		}
		env["HAS_EXECUTOR"] = "true"
		mode, tier, err := multiLaneDecide(t, step.Run, env)
		if err != nil {
			t.Errorf("actor %s: decide failed on merge_group: %v", actor, err)
			continue
		}
		if mode != "remote" || tier != "" {
			t.Errorf("actor %s: merge_group mode %q tier %q, want remote (same-repo trust)", actor, mode, tier)
		}
	}

	var action struct {
		Runs struct {
			Steps []mqStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), ".github/actions/fresh-merge/action.yml")), &action); err != nil {
		t.Fatal(err)
	}
	for _, s := range action.Runs.Steps {
		if evalGHIf(t, s.If, mergeGroupCtx(nil)) {
			t.Errorf("fresh-merge step %q runs on merge_group (if %q); the queue ref is already the merge", s.Name, s.If)
		}
	}
}
