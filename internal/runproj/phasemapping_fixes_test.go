package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression tests for the 2026-07-11 run-view misclassification fixes: steps
// that LEAD UP TO review (pre-review CI repair) classified as the review phase,
// terminal runs classified blocked off a text match, the missing pre-review-ci
// stage in the adopt-pr ladder, and .attempt.N step ids missing every exact
// stage/step match.

func TestStepIDPhaseLeadUpToReviewIsNotReview(t *testing.T) {
	cases := []struct {
		stepID string
		want   string
	}{
		// Leading up to review: the pre-review CI gate and its repair step are
		// implementation-side work, not the review itself.
		{"pre-review-ci", "active"},
		{"repair-pre-review-ci-failures", "implementation"},
		{"repair-pre-review-ci-failures.attempt.1", "implementation"},
		// The review loop itself still classifies as review.
		{"review-loop", "review"},
		{"review-pipeline.review-claude", "review"},
		{"review-pipeline.quality-scorecard", "review"},
		// Approval keeps its existing lead-up behavior.
		{"pre-approval-ci", "active"},
		{"repair-pre-approval-ci-failures", "implementation"},
	}
	for _, tc := range cases {
		if got := stepIDPhase(tc.stepID); got != tc.want {
			t.Errorf("stepIDPhase(%q) = %q, want %q", tc.stepID, got, tc.want)
		}
	}
}

func TestMapRunPhaseTerminalWinsOverBlockedText(t *testing.T) {
	issues := []runIssue{
		{id: "root-1", title: "mol-adopt-pr-v2", status: "closed", metadata: map[string]string{
			beadmeta.KindMetadataKey: "run",
		}},
		// A closed member whose text mentions "blocked" must not pin the whole
		// terminal run into the blocked lane.
		{id: "step-1", title: "Preflight", desc: "aborted: blocked by missing worktree", status: "closed", parent: "root-1"},
	}
	got := mapRunPhase("root-1", issues)
	if got.phase != "complete" {
		t.Fatalf("mapRunPhase phase = %q, want %q", got.phase, "complete")
	}
}

func TestMapRunPhaseBlockedStillWinsWhileRunIsOpen(t *testing.T) {
	issues := []runIssue{
		{id: "root-1", title: "mol-adopt-pr-v2", status: "open"},
		{id: "step-1", title: "Preflight", status: "blocked", parent: "root-1"},
	}
	got := mapRunPhase("root-1", issues)
	if got.phase != "blocked" {
		t.Fatalf("mapRunPhase phase = %q, want %q", got.phase, "blocked")
	}
}

func TestMapRunPhaseFailedRootKeepsCompletePhaseWithFailedLabel(t *testing.T) {
	issues := []runIssue{
		{id: "root-1", title: "mol-adopt-pr-v2", status: "closed", metadata: map[string]string{
			beadmeta.OutcomeMetadataKey: "fail",
		}},
		{id: "step-1", title: "Preflight", status: "closed", parent: "root-1", metadata: map[string]string{
			beadmeta.OutcomeMetadataKey: "fail",
		}},
	}
	got := mapRunPhase("root-1", issues)
	if got.phase != "complete" {
		t.Fatalf("mapRunPhase phase = %q, want %q (RunPhase union has no failed member)", got.phase, "complete")
	}
	if got.label != "failed" {
		t.Fatalf("mapRunPhase label = %q, want %q", got.label, "failed")
	}
}

func TestMapRunPhaseRecoveredRunIsNotLabeledFailed(t *testing.T) {
	// A failed attempt that was retried to success leaves outcome=fail on the
	// attempt bead; only the ROOT outcome speaks for the run.
	issues := []runIssue{
		{id: "root-1", title: "mol-adopt-pr-v2", status: "closed"},
		{id: "step-1", title: "Repair CI", status: "closed", parent: "root-1", metadata: map[string]string{
			beadmeta.OutcomeMetadataKey: "fail",
		}},
	}
	got := mapRunPhase("root-1", issues)
	if got.phase != "complete" || got.label == "failed" {
		t.Fatalf("mapRunPhase = %+v, want phase complete without failed label", got)
	}
}

func TestAdoptPrStageLadderCoversPreReviewCI(t *testing.T) {
	stages := stagesForFormula("mol-adopt-pr-v2", true)
	keys := make([]string, len(stages))
	byKey := map[string]formulaStage{}
	for i, s := range stages {
		keys[i] = s.key
		byKey[s.key] = s
	}

	pre, ok := byKey["pre-review-ci"]
	if !ok {
		t.Fatalf("mol-adopt-pr-v2 ladder %v lacks a pre-review-ci stage", keys)
	}
	if !containsString(pre.steps, "pre-review-ci") || !containsString(pre.steps, "repair-pre-review-ci-failures") {
		t.Fatalf("pre-review-ci stage steps = %v, want pre-review-ci + repair-pre-review-ci-failures", pre.steps)
	}

	// It must sit between rebase and review.
	idx := map[string]int{}
	for i, k := range keys {
		idx[k] = i
	}
	if idx["rebase"] >= idx["pre-review-ci"] || idx["pre-review-ci"] >= idx["review"] {
		t.Fatalf("stage order %v: pre-review-ci must be between rebase and review", keys)
	}

	// The pre-approval CI stage must match the step ids runs actually emit.
	ci := byKey["ci"]
	if !containsString(ci.steps, "repair-pre-approval-ci-failures") {
		t.Fatalf("ci stage steps = %v, want repair-pre-approval-ci-failures included", ci.steps)
	}
}

func TestFormulaActiveStageIndexMatchesAttemptSuffixedStep(t *testing.T) {
	stages := stagesForFormula("mol-adopt-pr-v2", true)
	issues := []runIssue{
		// Closed earlier stages.
		{id: "s1", status: "closed", metadata: map[string]string{beadmeta.StepIDMetadataKey: "preflight"}, updatedAt: "2026-07-11T01:00:00Z"},
		{id: "s2", status: "closed", metadata: map[string]string{beadmeta.StepIDMetadataKey: "rebase-check"}, updatedAt: "2026-07-11T02:00:00Z"},
		// The live iteration-2 repair attempt carries an attempt-suffixed step id.
		{id: "s3", status: "in_progress", metadata: map[string]string{beadmeta.StepIDMetadataKey: "repair-pre-review-ci-failures.attempt.1"}, updatedAt: "2026-07-11T03:00:00Z"},
		// Review steps exist but have not started.
		{id: "s4", status: "open", metadata: map[string]string{beadmeta.StepIDMetadataKey: "review-pipeline.review-claude"}, updatedAt: "2026-07-11T02:30:00Z"},
	}
	got := formulaActiveStageIndex(stages, issues)
	want := -1
	for i, s := range stages {
		if s.key == "pre-review-ci" {
			want = i
		}
	}
	if want == -1 {
		t.Fatal("ladder lacks pre-review-ci stage")
	}
	if got != want {
		t.Fatalf("formulaActiveStageIndex = %d (%s), want %d (pre-review-ci)", got, stageKeyAt(stages, got), want)
	}
}

func stageKeyAt(stages []formulaStage, idx int) string {
	if idx < 0 || idx >= len(stages) {
		return "none"
	}
	return stages[idx].key
}

func TestStripAttemptSuffix(t *testing.T) {
	cases := map[string]string{
		"repair-pre-review-ci-failures.attempt.1": "repair-pre-review-ci-failures",
		"finalize.attempt.12":                     "finalize",
		"review-loop.iteration.1.apply-fixes":     "review-loop.iteration.1.apply-fixes",
		"plain-step":                              "plain-step",
		"attempt.1":                               "attempt.1", // never strip to empty
	}
	for in, want := range cases {
		if got := stripAttemptSuffix(in); got != want {
			t.Errorf("stripAttemptSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
