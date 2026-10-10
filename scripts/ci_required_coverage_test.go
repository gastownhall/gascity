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

// ciAdvisoryAllowlist holds real jobs deliberately left out of
// ci-required's needs closure, each documented in ci.yml itself at the
// referenced line. Any job in this set must stay unreachable from
// ci-required -- TestCIRequiredCoversEveryRealJob fails if one becomes
// reachable, so the allowlist doesn't silently outlive its reason.
var ciAdvisoryAllowlist = map[string]bool{
	"contract-radar-bd-head": true, // ci.yml:422 -- a bd-main break must never block a gascity merge
	"mcp-mail":               true, // ci.yml:1348 -- continue-on-error isolates gascity from upstream mcp_agent_mail API drift
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

// TestCIRequiredCoversEveryRealJob guards against a job silently falling
// out of the merge gate: a job added to ci.yml that neither feeds
// ci-required (directly or via an intermediate rollup) nor is documented
// as advisory can stay green in isolation while never actually blocking
// a broken merge.
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
		if ciAggregateJobs[name] || ciAdvisoryAllowlist[name] {
			continue
		}
		if !reachable[name] {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Fatalf("jobs not reachable from %q's transitive needs: closure and not in ciAdvisoryAllowlist "+
			"(wire into ci-required's needs, an existing rollup's needs, or document as advisory):\n  %v",
			ciRequiredRoot, uncovered)
	}

	var stale []string
	for name := range ciAdvisoryAllowlist {
		if _, ok := wf.Jobs[name]; !ok {
			stale = append(stale, name+" (job no longer exists in ci.yml)")
			continue
		}
		if reachable[name] {
			stale = append(stale, name+" (now reachable from ci-required; remove from ciAdvisoryAllowlist)")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("ciAdvisoryAllowlist is stale:\n  %v", stale)
	}
}
