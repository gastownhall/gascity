package runproj

import "testing"

// Regression tests for the blocked lane: mapRunPhase keys "blocked" on the
// authoritative bd status, never on a substring scan of step text. "blocked" is
// reserved for runs the store itself marks blocked — the same invariant
// detail_displaystate_fixes_test.go asserts at the node level.

// mol-review-quorum's review step lists the verdict enum in its description, so
// its text contains the word "blocked" on every run. An OPEN such run must not
// be bucketed into the blocked lane, where the operator is offered a
// claim-a-worker remedy that cannot apply.
func TestMapRunPhaseOpenRunMentioningBlockedIsNotBlocked(t *testing.T) {
	issues := []runIssue{
		{id: "root", status: "open", title: "mol-review-quorum"},
		{
			id:     "root.1",
			parent: "root",
			status: "in_progress",
			title:  "Review the change",
			desc:   "Return a verdict: pass, pass_with_findings, fail, or blocked.",
		},
	}
	got := mapRunPhase("root", issues)
	if got.phase == "blocked" {
		t.Errorf("phase = %q for an open run that only MENTIONS \"blocked\" in step text; want any non-blocked phase", got.phase)
	}
}

// The same text on a fully-closed run must land in history, not the blocked lane.
func TestMapRunPhaseClosedRunMentioningBlockedIsComplete(t *testing.T) {
	issues := []runIssue{
		{id: "root", status: "closed", title: "mol-review-quorum"},
		{
			id:     "root.1",
			parent: "root",
			status: "closed",
			title:  "Review the change",
			desc:   "Return a verdict: pass, pass_with_findings, fail, or blocked.",
		},
	}
	got := mapRunPhase("root", issues)
	if got.phase != "complete" {
		t.Errorf("phase = %q for a fully-closed run, want %q", got.phase, "complete")
	}
}

// Guard against over-correcting: a real store-marked blocked member on an open
// run still drives the blocked lane.
func TestMapRunPhaseStoreBlockedStatusStillBlocks(t *testing.T) {
	issues := []runIssue{
		{id: "root", status: "open", title: "some-run"},
		{id: "root.1", parent: "root", status: "blocked", title: "Waiting on an operator"},
	}
	got := mapRunPhase("root", issues)
	if got.phase != "blocked" {
		t.Errorf("phase = %q for a run with a status==blocked member, want %q", got.phase, "blocked")
	}
	if got.label != "blocked" {
		t.Errorf("label = %q, want %q", got.label, "blocked")
	}
}
