package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func closedStepEvent(t *testing.T, stepID, rootID string) events.Event {
	t.Helper()
	payload, err := json.Marshal(beads.Bead{
		ID: stepID, Status: "closed",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
	})
	if err != nil {
		t.Fatalf("marshal step payload: %v", err)
	}
	return events.Event{Type: events.BeadClosed, Subject: stepID, Payload: payload}
}

// TestCompletionsLaneNamesRootsFromBothEventShapes pins the delta feed's two
// inputs. A step's closure reaches this process either as an execution.step_*
// fact (which states its RunID) or as a bead.closed notification carrying the
// physical step snapshot (whose gc.root_bead_id is the root). Missing either
// shape would leave a whole class of closes to wait for the hourly sweep.
func TestCompletionsLaneNamesRootsFromBothEventShapes(t *testing.T) {
	lane := newCompletionsLane()
	lane.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: "gcg-root-a", Subject: "gcg-step-a"})
	lane.observe(closedStepEvent(t, "gcg-step-b", "gcg-root-b"))
	// Neither shape: ordinary traffic must cost the tick nothing.
	lane.observe(events.Event{Type: events.BeadUpdated, Subject: "ga-1"})
	lane.observe(closedStepEvent(t, "ga-2", ""))

	got := map[string]bool{}
	for _, id := range lane.takePending() {
		got[id] = true
	}
	if len(got) != 2 || !got["gcg-root-a"] || !got["gcg-root-b"] {
		t.Fatalf("pending roots = %v, want exactly gcg-root-a and gcg-root-b", got)
	}
	// Control: draining really drained, so the set above is per-pass and not
	// an ever-growing list the tick would re-walk.
	if rest := lane.takePending(); len(rest) != 0 {
		t.Fatalf("pending after drain = %v, want empty", rest)
	}
}

// TestCompletionsLaneSweepCadenceReplacesTriggerNameGating pins the schedule.
// The pre-slice gate was `trigger == "patrol"`, which under overload means
// "every tick" — explicit cadence state is what makes "rare" actually rare.
func TestCompletionsLaneSweepCadenceReplacesTriggerNameGating(t *testing.T) {
	now := time.Now()
	lane := newCompletionsLane()
	if !lane.sweepDue(now) {
		t.Fatal("a lane that has never swept is not due; nothing has converged yet")
	}
	lane.noteSweepRan(now)
	if lane.sweepDue(now.Add(time.Minute)) {
		t.Fatal("the sweep is due a minute after a full one; the cadence gate is not gating")
	}
	if !lane.sweepDue(now.Add(completionsBackstopInterval)) {
		t.Fatal("the sweep is not due at its cadence")
	}
	// A gap in the feed makes it due immediately: the delta lane can no longer
	// claim to name every changed root.
	lane.force()
	if !lane.sweepDue(now.Add(time.Second)) {
		t.Fatal("a feed gap did not force the sweep")
	}
}

// TestCompletionsLaneOverflowForcesTheSweep pins the other gap shape: more named
// roots than the lane will hold means candidates would have to be dropped, and a
// dropped root is a lifecycle gap nothing else is looking for.
func TestCompletionsLaneOverflowForcesTheSweep(t *testing.T) {
	lane := newCompletionsLane()
	lane.noteSweepRan(time.Now())
	for i := range completionsCandidateCap + 1 {
		lane.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: overflowBeadID(i)})
	}
	if !lane.sweepDue(time.Now()) {
		t.Fatal("candidate overflow did not force the sweep")
	}
	// Control: below the cap the lane keeps its candidates and stays un-forced.
	small := newCompletionsLane()
	small.noteSweepRan(time.Now())
	small.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: "gcg-root-a"})
	if small.sweepDue(time.Now()) {
		t.Fatal("a single named root forced the sweep; overflow is not what the assertion above measured")
	}
	if got := small.takePending(); len(got) != 1 || got[0] != "gcg-root-a" {
		t.Fatalf("pending = %v, want [gcg-root-a]", got)
	}
}

// TestCompletionReconcileInputsNarrowToTheInfraStoreOnTheRuntimePlane is the
// operator invariant on the completions lane (ga-l7jdg, bd memory
// gascity-runtime-infra-store-invariant): city operations read the infra/class
// store only, so the tick's delta pass gets ONE leg — the graph class store —
// while the off-tick convergence lane keeps the whole fan it must converge.
//
// resolveGraphStore answers "the binding when the graph class is relocated, the
// city store otherwise", so the narrowing needs no special case for a
// single-store city: there the work store IS the infra store, and the runtime
// plane's one leg is the right one.
func TestCompletionReconcileInputsNarrowToTheInfraStoreOnTheRuntimePlane(t *testing.T) {
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityBeadStore: beads.NewMemStore(),
		beadStores:    map[string]beads.Store{"alpha": beads.NewMemStore(), "beta": beads.NewMemStore()},
		eventProv:     events.NewFake(),
	}

	ep, runtimeFan := cs.completionReconcileInputs(runtimePlane)
	if ep == nil {
		t.Fatal("no event provider; the fixture cannot express the invariant")
	}
	if len(runtimeFan) != 1 {
		t.Fatalf("the runtime plane fans out to %d store(s), want 1 (the infra/class store)", len(runtimeFan))
	}
	// Control: the convergence lane keeps every store, so "one leg" above is a
	// narrowing and not a fan that collapsed for some other reason.
	_, reconcileFan := cs.completionReconcileInputs(reconcilePlane)
	if len(reconcileFan) <= len(runtimeFan) {
		t.Fatalf("the convergence lane fans out to %d store(s) and the runtime plane to %d; the planes are not distinguishable",
			len(reconcileFan), len(runtimeFan))
	}
	if len(reconcileFan) != 3 {
		t.Fatalf("the convergence lane fans out to %d store(s), want 3 (city work + two rigs)", len(reconcileFan))
	}
}
