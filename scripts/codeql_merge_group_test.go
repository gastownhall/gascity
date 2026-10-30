package scripts_test

import (
	"maps"
	"testing"
)

// CodeQL off the merge-queue path (TESTING.md "Merge queue"). The four
// Analyze checks stay required (main ruleset and classic branch protection),
// so codeql.yml still runs on merge_group and every leg still reports its
// check, but a merge group analyzes nothing: each leg runs one step that says
// why, on the 2 vCPU runner. Each queued pull request's own run analyzed it,
// and the ruleset's code_scanning rule gated it there (GitHub applies that
// rule to pull requests, never to merge queue groups); the push to main
// analyzes the merged commit.

// codeqlMergeGroupStep is the id of the one step a merge group runs.
const codeqlMergeGroupStep = "merge-group"

// codeqlAnalysisSteps are the steps every leg of a pull request, push,
// schedule or dispatch run executes, whatever its language.
var codeqlAnalysisSteps = []string{"Checkout", "Initialize CodeQL", "Perform CodeQL Analysis"}

func TestCodeQLMergeGroupReportsWithoutAnalysis(t *testing.T) {
	wf := readMergeQueueWorkflow(t, mergeQueueCodeQLYML)
	job, ok := wf.Jobs["analyze"]
	if !ok {
		t.Fatalf("%s has no analyze job", mergeQueueCodeQLYML)
	}
	var include []map[string]string
	if err := job.Strategy.Matrix.Include.Decode(&include); err != nil {
		t.Fatalf("analyze matrix include: %v", err)
	}
	if len(include) != 4 {
		t.Fatalf("analyze has %d legs, want the four required Analyze checks", len(include))
	}
	stub := -1
	for i, s := range job.Steps {
		if s.ID == codeqlMergeGroupStep {
			stub = i
		}
	}
	if stub < 0 {
		t.Fatalf("analyze has no step with id %q (the merge group's only step)", codeqlMergeGroupStep)
	}
	if s := job.Steps[stub]; s.Uses != "" || s.Run == "" {
		t.Errorf("the merge group step uses %q and runs %q; want a run step that needs no checkout", s.Uses, s.Run)
	}

	runners := map[string]string{
		"needs.runner-policy.outputs.runner_2vcpu":  "blacksmith-2vcpu-ubuntu-2404",
		"needs.runner-policy.outputs.runner_16vcpu": "blacksmith-16vcpu-ubuntu-2404",
	}
	events := []struct {
		name   string
		ctx    map[string]string
		runner string
	}{
		{"merge_group", mergeGroupCtx(nil), "blacksmith-2vcpu-ubuntu-2404"},
		{"pull_request", map[string]string{"github.event_name": "pull_request", "github.ref": "refs/pull/7001/merge"}, "blacksmith-16vcpu-ubuntu-2404"},
		{"push", map[string]string{"github.event_name": "push", "github.ref": "refs/heads/main"}, "blacksmith-16vcpu-ubuntu-2404"},
		{"schedule", map[string]string{"github.event_name": "schedule", "github.ref": "refs/heads/main"}, "blacksmith-16vcpu-ubuntu-2404"},
		{"workflow_dispatch", map[string]string{"github.event_name": "workflow_dispatch", "github.ref": "refs/heads/main"}, "blacksmith-16vcpu-ubuntu-2404"},
	}
	for _, ev := range events {
		ctx := maps.Clone(ev.ctx)
		maps.Copy(ctx, runners)
		if got := interpolateGH(t, job.RunsOn, ctx); got != ev.runner {
			t.Errorf("%s: analyze runs on %q, want %q", ev.name, got, ev.runner)
		}
		for _, leg := range include {
			ctx := maps.Clone(ctx)
			ctx["matrix.language"] = leg["language"]
			ctx["matrix.build-mode"] = leg["build-mode"]
			if ev.name != "merge_group" {
				// A skipped step has no outputs: only a run that took the
				// gocache step sees its answer.
				ctx["steps.gocache.outputs.managed"] = "false"
			}
			ran := map[string]bool{}
			for _, s := range job.Steps {
				ran[mqStepKey(s)] = evalGHIf(t, s.If, ctx)
			}
			if ev.name == "merge_group" {
				for _, s := range job.Steps {
					key := mqStepKey(s)
					if want := key == codeqlMergeGroupStep; ran[key] != want {
						t.Errorf("merge_group, %s leg: step %q runs %v, want %v (a merge group analyzes nothing)", leg["language"], key, ran[key], want)
					}
				}
				continue
			}
			if ran[codeqlMergeGroupStep] {
				t.Errorf("%s, %s leg: the merge group step runs", ev.name, leg["language"])
			}
			for _, key := range codeqlAnalysisSteps {
				if !ran[key] {
					t.Errorf("%s, %s leg: step %q is skipped; every leg outside a merge group analyzes", ev.name, leg["language"], key)
				}
			}
			if want := leg["build-mode"] == "autobuild"; ran["Autobuild"] != want {
				t.Errorf("%s, %s leg: Autobuild runs %v, want %v", ev.name, leg["language"], ran["Autobuild"], want)
			}
			for _, key := range []string{"Set up Go", "Restore Go modules", "gocache", "gocache-restore"} {
				if want := leg["language"] == "go"; ran[key] != want {
					t.Errorf("%s, %s leg: step %q runs %v, want %v", ev.name, leg["language"], key, ran[key], want)
				}
			}
		}
	}
}
