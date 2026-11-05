package beads_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

// lifecycleRootID is the single graph.v2 run this test drives end to end.
const lifecycleRootID = "gcg-lifecycle-root"

// lifecycleCreatedAt timestamps every bead in the run so the projected updated_at
// is populated (a run that happened at a real time), matching the sibling
// round-trip test's fixture.
var lifecycleCreatedAt = time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)

// TestRunViewLifecycleRoundTripFromNotifyChange is the producer-fidelity gate for
// the run projection. TestRunViewRoundTripFromNotifyChange (this package) proves a
// two-bead run survives the producer→fold→summary seam; this widens that to a full
// graph.v2 lifecycle (created → in_progress → closed for every bead) emitted
// through the REAL producer (CachingStore.notifyChange), and drives it all the way
// into BOTH runproj.BuildRunSummary AND runproj.BuildRunDetail.
//
// It is the exact regression the run-view RCA describes: a store that stops
// emitting bead.* for graph.v2 roots. If the producer drops those events the fold
// starves, the summary loses the lane, and the detail loses its nodes — each of
// which fails an assertion here. Because the events come from the production
// marshal + run/session id-resolution path (not a hand-built payload), a wire-shape
// or metadata drift between producer and consumer also trips this gate.
func TestRunViewLifecycleRoundTripFromNotifyChange(t *testing.T) {
	step1 := lifecycleRootID + ".1"
	step2 := lifecycleRootID + ".2"

	// Full lifecycle emitted through the real producer, ending terminal-success:
	// root goes in_progress; each step is created, runs, then closes; the root
	// closes last.
	evts := recordThroughNotifyChange(t,
		beadSeed{events.BeadCreated, lifecycleRoot("in_progress")},
		beadSeed{events.BeadCreated, lifecycleStep(step1, "preflight", "open")},
		beadSeed{events.BeadUpdated, lifecycleStep(step1, "preflight", "in_progress")},
		beadSeed{events.BeadClosed, lifecycleStep(step1, "preflight", "closed")},
		beadSeed{events.BeadCreated, lifecycleStep(step2, "review-loop", "open")},
		beadSeed{events.BeadUpdated, lifecycleStep(step2, "review-loop", "in_progress")},
		beadSeed{events.BeadClosed, lifecycleStep(step2, "review-loop", "closed")},
		beadSeed{events.BeadClosed, lifecycleRoot("closed")},
	)

	folded := runproj.Fold(evts)
	if len(folded) != 3 {
		t.Fatalf("fold size = %d, want 3 (root + 2 steps); the fold starved", len(folded))
	}
	ordered := beadsInSeenOrder(evts, folded)

	// Summary: the closed run must list in the historical bucket, with its formula.
	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(ordered))
	var lane runproj.RunLane
	found := false
	for _, l := range summary.HistoricalLanes {
		if l.ID == lifecycleRootID {
			lane, found = l, true
		}
	}
	if !found {
		t.Fatalf("closed run %q not present in historical lanes — the projection starved.\nsummary=%+v", lifecycleRootID, summary)
	}
	if lane.Phase != "complete" {
		t.Errorf("lane phase = %q, want complete (root closed)", lane.Phase)
	}
	if lane.Formula.Status != "known" || lane.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("lane formula = %+v, want known/mol-adopt-pr-v2", lane.Formula)
	}

	// Detail: the same folded snapshot must render a terminal DAG.
	detail, err := runproj.BuildRunDetail(ordered, lifecycleRootID, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	statusByNode := map[string]string{}
	for _, node := range detail.Nodes {
		statusByNode[node.ID] = node.Status
	}
	for _, id := range []string{lifecycleRootID, "preflight", "review-loop"} {
		if statusByNode[id] != "completed" {
			t.Errorf("node %q status = %q, want completed", id, statusByNode[id])
		}
	}
	if detail.Phase != "complete" {
		t.Errorf("detail.Phase = %q, want complete", detail.Phase)
	}
	if !detail.Progress.Terminal {
		t.Error("detail.Progress.Terminal = false, want true — every node is terminal")
	}
}

// lifecycleRoot builds the graph.v2 run-root bead at a given lifecycle status — the
// marker set that makes the run project: gc.formula_contract=graph.v2 and
// gc.kind=run drive lane grouping and detail eligibility; gc.formula names the
// formula; gc.root_store_ref + gc.scope_kind/gc.scope_ref supply the run
// identity/scope BuildRunDetail requires.
func lifecycleRoot(status string) beads.Bead {
	return beads.Bead{
		ID:        lifecycleRootID,
		Title:     "adopt PR #42",
		Status:    status,
		Type:      "molecule",
		CreatedAt: lifecycleCreatedAt,
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.KindMetadataKey:            "run",
			beadmeta.FormulaMetadataKey:         "mol-adopt-pr-v2",
			beadmeta.RunTargetMetadataKey:       "rig:demo",
			beadmeta.RootStoreRefMetadataKey:    "rig:demo",
			beadmeta.ScopeKindMetadataKey:       "rig",
			beadmeta.ScopeRefMetadataKey:        "demo",
		},
	}
}

// lifecycleStep builds a graph.v2 run step rooted at lifecycleRootID:
// gc.root_bead_id groups it under the root and gc.step_id is its semantic node id
// in the detail DAG. id is the bead id, stepID the semantic step id.
func lifecycleStep(id, stepID, status string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     stepID,
		Status:    status,
		Type:      "task",
		CreatedAt: lifecycleCreatedAt,
		ParentID:  lifecycleRootID,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "step",
			beadmeta.RootBeadIDMetadataKey: lifecycleRootID,
			beadmeta.StepIDMetadataKey:     stepID,
			beadmeta.StepRefMetadataKey:    "mol-adopt-pr-v2." + stepID,
		},
	}
}
