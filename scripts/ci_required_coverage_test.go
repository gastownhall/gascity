package scripts_test

import (
	"sort"
	"testing"
)

// ciRequiredRoot is the branch-protection status every real CI job must
// ultimately feed, directly or through an intermediate rollup.
const ciRequiredRoot = "ci-required"

// ciAggregateJobs summarize a subset of the job graph by reading
// needs.*.result -- they never run tests themselves, so they are exempt
// from the coverage requirement below. check is the main ruleset's
// required rollup, kept under its historical name; it fans in the same
// gating jobs as ci-required (TestCheckAndCIRequiredFanInTheGatingJobs).
var ciAggregateJobs = map[string]bool{
	ciRequiredRoot: true,
	"check":        true,
}

// ciNeedsClosure walks needs: edges from root and returns every job root
// depends on, directly or transitively, including root itself.
func ciNeedsClosure(jobs map[string]ciCriticalPathJob, root string) map[string]bool {
	seen := make(map[string]bool)
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range jobs[name].Needs {
			visit(dep)
		}
	}
	visit(root)
	return seen
}

// TestCIRequiredCoversEveryRealJob: every real job in ci.yml feeds
// ci-required, directly or via an intermediate rollup. That guards against a
// job silently falling out of the merge gate (green in isolation, blocking
// nothing), and it keeps advisory jobs out of the workflow of the required
// checks: GitHub reports a red Check or CI / required without naming the
// need that failed, so every failed job of a CI run must be one they wait
// on. An advisory job (a live probe that must never block a merge) belongs
// in advisory-probes.yml (TestAdvisoryProbesRunOutsideTheRequiredCIWorkflow).
func TestCIRequiredCoversEveryRealJob(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	if len(wf.Jobs) == 0 {
		t.Fatal("parsed zero jobs from ci.yml -- parser or path is broken")
	}
	if _, ok := wf.Jobs[ciRequiredRoot]; !ok {
		t.Fatalf("ci.yml has no %q job", ciRequiredRoot)
	}

	reachable := ciNeedsClosure(wf.Jobs, ciRequiredRoot)

	var uncovered []string
	for name := range wf.Jobs {
		if ciAggregateJobs[name] {
			continue
		}
		if !reachable[name] {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Fatalf("jobs not reachable from %q's transitive needs: closure:\n  %v\n"+
			"wire a gating job into ci-required's needs (and Check's, or an existing rollup's); "+
			"move an advisory job to advisory-probes.yml",
			ciRequiredRoot, uncovered)
	}
}
