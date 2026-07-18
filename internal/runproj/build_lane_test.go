package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func runRoot(id, formula string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "Run " + id,
		Status: "closed",
		Type:   "molecule",
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
		},
	}
}

// TestBuildRunLaneResolvesBeyondHistoricalCap proves a completed run that would
// be truncated out of BuildRunSummary's 50-lane historical cap is still
// resolvable via BuildRunLane — the guard for the false-404 defect.
func TestBuildRunLaneResolvesBeyondHistoricalCap(t *testing.T) {
	var beadList []beads.Bead
	for i := 0; i < 60; i++ {
		beadList = append(beadList, runRoot(runIDf(i), "mol-adopt-pr-v2"))
	}

	summary := BuildRunSummary(beadList)
	if len(summary.HistoricalLanes) > maxHistoricalLanes {
		t.Fatalf("historical lanes = %d, expected cap at %d", len(summary.HistoricalLanes), maxHistoricalLanes)
	}

	// A run that is NOT present in the capped summary output must still resolve.
	inSummary := map[string]bool{}
	for _, l := range summary.HistoricalLanes {
		inSummary[l.ID] = true
	}
	var truncated string
	for i := 0; i < 60; i++ {
		if !inSummary[runIDf(i)] {
			truncated = runIDf(i)
			break
		}
	}
	if truncated == "" {
		t.Fatal("expected at least one run truncated out of the summary")
	}

	lane, ok := BuildRunLane(beadList, truncated)
	if !ok {
		t.Fatalf("BuildRunLane(%s) ok=false, want a resolvable lane despite the cap", truncated)
	}
	if lane.ID != truncated {
		t.Fatalf("lane.ID = %q, want %q", lane.ID, truncated)
	}
}

func TestBuildRunLaneRejectsNonRun(t *testing.T) {
	beadList := []beads.Bead{{ID: "plain", Type: "task"}}
	if _, ok := BuildRunLane(beadList, "plain"); ok {
		t.Error("BuildRunLane(plain task) ok=true, want false")
	}
	if _, ok := BuildRunLane(beadList, "missing"); ok {
		t.Error("BuildRunLane(missing) ok=true, want false")
	}
	if _, ok := BuildRunLane(nil, ""); ok {
		t.Error("BuildRunLane(nil, \"\") ok=true, want false")
	}
}

func runIDf(i int) string {
	return "run-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

// runStep builds a primary step bead grouped under rootID via gc.root_bead_id.
func runStep(id, rootID, stepID, status string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  stepID,
		Status: status,
		Type:   "task",
		Metadata: map[string]string{
			"gc.root_bead_id": rootID,
			"gc.step_id":      stepID,
		},
	}
}

// TestBuildRunLaneResolvesAttemptSuffixedActiveStep is the summary-level
// regression for the attempt-suffixed active step: a live adopt-pr retry exposes
// repair-pre-review-ci-failures.attempt.1 as its active step id. The lane must
// resolve the formula stage position, mark FormulaStageResolved, AND surface the
// step attempt — none of which held while summary compared/looked up the raw
// suffixed id against the authored base ids in the stage tables. The honest raw
// step id is still carried on progress.stepID.
func TestBuildRunLaneResolvesAttemptSuffixedActiveStep(t *testing.T) {
	root := runRoot("run-retry", "mol-adopt-pr-v2")
	root.Status = "open"
	beadList := []beads.Bead{
		root,
		runStep("s-preflight", "run-retry", "preflight", "closed"),
		runStep("s-rebase", "run-retry", "rebase-check", "closed"),
		runStep("s-repair", "run-retry", "repair-pre-review-ci-failures.attempt.1", "in_progress"),
	}

	lane, ok := BuildRunLane(beadList, "run-retry")
	if !ok {
		t.Fatal("BuildRunLane(run-retry) ok=false, want a resolvable lane")
	}
	if lane.Progress.Status != "active_step" {
		t.Fatalf("progress.status = %q, want active_step", lane.Progress.Status)
	}
	if lane.Progress.StepID != "repair-pre-review-ci-failures.attempt.1" {
		t.Fatalf("progress.stepID = %q, want the honest attempt-suffixed id", lane.Progress.StepID)
	}
	if lane.Progress.Stage.Status != "available" || lane.Progress.Stage.Key != "pre-review-ci" {
		t.Fatalf("progress.stage = %+v, want available pre-review-ci", lane.Progress.Stage)
	}
	if !lane.FormulaStageResolved {
		t.Fatal("formulaStageResolved = false, want true for the attempt-suffixed active step")
	}
	if lane.Progress.Attempt.Status != "available" || lane.Progress.Attempt.Value != 1 {
		t.Fatalf("progress.attempt = %+v, want available value 1", lane.Progress.Attempt)
	}
}
