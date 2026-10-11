package runproj

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// This file is the end-to-end contract gate for the run projection. The rest of
// the package's tests are split into two worlds that never meet: fold_test.go /
// projector_test.go drive events → Fold but assert only the fold map, while the
// golden and terminal-clamp tests load a []beads.Bead straight into
// BuildRunSummary / BuildRunDetail and never touch Fold. Nothing chained a real
// bead.* event stream all the way through Fold/Projector into the two builders —
// so when a store stopped emitting bead.* for graph.v2 workflow roots the runs
// silently vanished and no test failed.
//
// These tests close that gap: they synthesize a full-lifecycle bead.* event
// stream (created → in_progress → closed), fold it through the Projector exactly
// as the per-city tailer does, and assert BOTH BuildRunSummary (the lane lists)
// and BuildRunDetail (the DAG renders) off the folded snapshot. If the fold
// starves, the summary comes back empty and the detail loses its nodes — both of
// which these assertions catch.
//
// The run beads are built with the package's shared graph.v2 run-bead builders
// (clampRootBead / clampStepBead, detail_terminal_clamp_test.go): the exact
// fixture the direct-build tests already trust, now driven through the event
// chain instead of handed straight to the builders.

// runBeadEvent wraps a full bead snapshot in a bead.* lifecycle event, marshaling
// the whole bead (metadata included) as the raw payload — the shape
// CachingStore.notifyChange emits and Fold/Projector decode. The package's
// beadEvent (fold_test.go) marshals only id/status/type, so a run whose grouping
// and rendering depend on metadata needs this fuller builder.
func runBeadEvent(seq uint64, typ string, b beads.Bead) events.Event {
	payload, _ := json.Marshal(b)
	return events.Event{Seq: seq, Type: typ, Subject: b.ID, Payload: payload}
}

// projectRunBeads folds an event stream through a Projector and returns the beads
// in first-seen order — the deterministic slice BuildRunSummary/BuildRunDetail
// expect, and the exact path the live tailer feeds them.
func projectRunBeads(evts []events.Event) []beads.Bead {
	p := NewProjector()
	p.Apply(evts)
	return p.Beads()
}

// TestContractGraphV2RunProjectsThroughEventsToSummaryAndDetail drives a graph.v2
// run's full lifecycle as a bead.* event stream and proves it renders end to end:
// while its steps are in flight the run lists as an ACTIVE lane (the precise shape
// that "went invisible" when the store stopped emitting bead.*), and once the root
// closes it moves to the HISTORICAL lanes and BuildRunDetail renders every node
// terminal with phase "complete".
func TestContractGraphV2RunProjectsThroughEventsToSummaryAndDetail(t *testing.T) {
	const rootID = "gcg-contract-run"

	// Mid-flight: the root and both steps are created and driven to in_progress.
	midFlight := []events.Event{
		runBeadEvent(1, events.BeadCreated, clampRootBead(rootID, "open", nil)),
		runBeadEvent(2, events.BeadUpdated, clampRootBead(rootID, "in_progress", nil)),
		runBeadEvent(3, events.BeadCreated, clampStepBead(rootID, rootID+".1", "preflight", "open", nil)),
		runBeadEvent(4, events.BeadUpdated, clampStepBead(rootID, rootID+".1", "preflight", "in_progress", nil)),
		runBeadEvent(5, events.BeadCreated, clampStepBead(rootID, rootID+".2", "review-loop", "open", nil)),
		runBeadEvent(6, events.BeadUpdated, clampStepBead(rootID, rootID+".2", "review-loop", "in_progress", nil)),
	}
	// Terminal: both steps then the root close.
	terminal := []events.Event{
		runBeadEvent(7, events.BeadClosed, clampStepBead(rootID, rootID+".1", "preflight", "closed", nil)),
		runBeadEvent(8, events.BeadClosed, clampStepBead(rootID, rootID+".2", "review-loop", "closed", nil)),
		runBeadEvent(9, events.BeadClosed, clampRootBead(rootID, "closed", nil)),
	}

	// Fold decodes every bead.* event: a decode starve (the run-view RCA signature)
	// would drop snapshots and shrink this map, which is exactly what left the run
	// view blank.
	if folded := Fold(append(append([]events.Event{}, midFlight...), terminal...)); len(folded) != 3 {
		t.Fatalf("Fold size = %d, want 3 (root + 2 steps); the fold starved", len(folded))
	}

	// Drive the same stream through the Projector in two passes, mirroring the live
	// tailer: a cold pass over the in-flight events, then an incremental Apply of
	// the closing events.
	p := NewProjector()
	p.Apply(midFlight)

	midSummary := BuildRunSummary(FilterRunBeads(p.Beads()))
	activeLane, ok := findLaneInGroup(midSummary.Lanes, rootID)
	if !ok {
		t.Fatalf("in-flight run %q not present as an active lane; the run went invisible.\nlanes=%+v", rootID, midSummary.Lanes)
	}
	if laneInGroup(midSummary.HistoricalLanes, rootID) {
		t.Errorf("in-flight run %q already in historical lanes, want active only", rootID)
	}
	if activeLane.Phase == "complete" {
		t.Errorf("in-flight lane phase = %q, want a non-terminal phase", activeLane.Phase)
	}
	// Formula identity must survive the event chain (it rides gc.formula metadata).
	if activeLane.Formula.Status != "known" || activeLane.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("in-flight lane formula = %+v, want known/mol-adopt-pr-v2", activeLane.Formula)
	}

	// Live-tail the closing events onto the warm projector.
	p.Apply(terminal)
	finalBeads := p.Beads()

	finalSummary := BuildRunSummary(FilterRunBeads(finalBeads))
	if laneInGroup(finalSummary.Lanes, rootID) {
		t.Errorf("closed run %q still in active lanes, want historical", rootID)
	}
	historicalLane, ok := findLaneInGroup(finalSummary.HistoricalLanes, rootID)
	if !ok {
		t.Fatalf("closed run %q not present in historical lanes; the run went invisible.\nhistorical=%+v", rootID, finalSummary.HistoricalLanes)
	}
	if historicalLane.Phase != "complete" {
		t.Errorf("historical lane phase = %q, want complete", historicalLane.Phase)
	}

	// The same folded snapshot must render a terminal detail DAG.
	detail, err := BuildRunDetail(finalBeads, rootID, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	statuses := nodeStatusByID(detail)
	for _, id := range []string{rootID, "preflight", "review-loop"} {
		if statuses[id] != "completed" {
			t.Errorf("node %q status = %q, want completed", id, statuses[id])
		}
	}
	if detail.Phase != "complete" {
		t.Errorf("detail.Phase = %q, want complete", detail.Phase)
	}
	if !detail.Progress.Terminal {
		t.Error("detail.Progress.Terminal = false, want true — every node is terminal")
	}
}

// TestContractFailedStepPresentsFailedNodeThroughEvents proves a step that closes
// with gc.outcome=fail surfaces as a "failed" node once its lifecycle is projected
// through the event chain. The root is left in_progress so the terminal-root clamp
// stays inert: the "failed" status is unambiguously the work of presentationStatus
// (detail_nodeshape.go), not the clamp.
func TestContractFailedStepPresentsFailedNodeThroughEvents(t *testing.T) {
	const rootID = "gcg-contract-fail"

	evts := []events.Event{
		runBeadEvent(1, events.BeadCreated, clampRootBead(rootID, "open", nil)),
		runBeadEvent(2, events.BeadUpdated, clampRootBead(rootID, "in_progress", nil)),
		// A clean preflight step: created → closed with no outcome → completed.
		runBeadEvent(3, events.BeadCreated, clampStepBead(rootID, rootID+".1", "preflight", "open", nil)),
		runBeadEvent(4, events.BeadClosed, clampStepBead(rootID, rootID+".1", "preflight", "closed", nil)),
		// A review step that runs then closes failed: created → in_progress → closed
		// carrying gc.outcome=fail.
		runBeadEvent(5, events.BeadCreated, clampStepBead(rootID, rootID+".2", "review-loop", "open", nil)),
		runBeadEvent(6, events.BeadUpdated, clampStepBead(rootID, rootID+".2", "review-loop", "in_progress", nil)),
		runBeadEvent(7, events.BeadClosed, clampStepBead(rootID, rootID+".2", "review-loop", "closed",
			map[string]string{beadmeta.OutcomeMetadataKey: "fail"})),
	}

	beadList := projectRunBeads(evts)

	// The run with its failed step still lists (as an active lane — the root is
	// in_progress, a failed review round pending retry).
	summary := BuildRunSummary(FilterRunBeads(beadList))
	if !laneInGroup(summary.Lanes, rootID) {
		t.Fatalf("run %q with a failed step not present as an active lane.\nlanes=%+v", rootID, summary.Lanes)
	}

	detail, err := BuildRunDetail(beadList, rootID, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	node, ok := nodeByID(detail, "review-loop")
	if !ok {
		t.Fatalf("failed step node %q not found; nodes=%+v", "review-loop", detail.Nodes)
	}
	if node.Status != "failed" {
		t.Errorf("failed step node status = %q, want failed (closed + gc.outcome=fail)", node.Status)
	}
	if statuses := nodeStatusByID(detail); statuses["preflight"] != "completed" {
		t.Errorf("clean step node status = %q, want completed", statuses["preflight"])
	}
}
