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
