package readywork

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// Native input admission (issue #8832): a graph.v2 step whose workflow root
// records gc.input_convoy_id is admitted to new execution only when the input
// that convoy tracks is currently admissible. The input's authored facts —
// canonical dispatch holds, raw blocked status, deferrals, unsatisfied
// ready-blocking prerequisites — are read live on every read, over a unified
// store and over split work/graph stores alike.

// inputFixture is one native workflow over an input: a root molecule carrying
// gc.input_convoy_id, an input convoy tracking input, and a routed graph step
// naming the root. The step has no graph-local blocker of its own.
type inputFixture struct {
	input   beads.Bead
	convoy  beads.Bead
	root    beads.Bead
	step    beads.Bead
	work    beads.Store
	binding beads.Store
}

// materializeInputWorkflow stamps the fixture into work (the input and its
// convoy) and, when binding is non-nil, the graph half (root and step) there;
// otherwise everything lands in work, a unified city.
func materializeInputWorkflow(t *testing.T, work, binding beads.Store) inputFixture {
	return materializeInputWorkflowWithInput(t, work, binding, beads.Bead{Title: "input", Type: "task"})
}

// materializeInputWorkflowWithInput is materializeInputWorkflow with the input
// bead's own fields supplied by the caller (a deferral, a status, labels).
func materializeInputWorkflowWithInput(t *testing.T, work, binding beads.Store, input beads.Bead) inputFixture {
	t.Helper()
	graph := work
	if binding != nil {
		graph = binding
	}
	fx := inputFixture{work: work, binding: binding}
	fx.input = mustCreate(t, work, input)
	fx.convoy = mustCreate(t, work, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := work.DepAdd(fx.convoy.ID, fx.input.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	fx.root = mustCreate(t, graph, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.input_convoy_id": fx.convoy.ID},
	})
	fx.step = mustCreate(t, graph, beads.Bead{
		Title: "graph-step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": fx.root.ID,
			"gc.routed_to":    "gascity/pool",
		},
	})
	return fx
}

// legs returns the fixture's read topology: one unified leg, or the split
// city's work leg plus graph binding leg.
func (fx inputFixture) legs() []Leg {
	if fx.binding == nil {
		return []Leg{testLeg("city", fx.work)}
	}
	return []Leg{
		testLeg("work", fx.work),
		{Label: "graph", Store: fx.binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
	}
}

func (fx inputFixture) routedQuery() Query {
	return Query{MetadataFields: []string{"gc.routed_to=gascity/pool"}}
}

// containsID reports whether rows contain id.
func containsID(rows []ReadyBead, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// idsOf lists rows' ids for failure messages.
func idsOf(rows []beads.Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// ptrTime returns a pointer to t.
func ptrTime(t time.Time) *time.Time { return &t }

// TestNativeInputAdmissionWithholdsStepOfHeldInput pins issue #8832: a native
// workflow's ready step must not be admitted while the input its root's
// gc.input_convoy_id convoy tracks is parked on a canonical dispatch hold.
func TestNativeInputAdmissionWithholdsStepOfHeldInput(t *testing.T) {
	store := beads.NewMemStore()
	input := mustCreate(t, store, beads.Bead{Title: "input", Type: "task", Labels: []string{"hold:external"}})
	conv := mustCreate(t, store, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := store.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.input_convoy_id": conv.ID},
	})
	step := mustCreate(t, store, beads.Bead{
		Title: "graph-step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.routed_to":    "gascity/pool",
		},
	})

	rows, err := Read([]Leg{testLeg("city", store)}, Query{MetadataFields: []string{"gc.routed_to=gascity/pool"}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, r := range rows {
		if r.ID == step.ID {
			t.Fatalf("Ready contains graph-step %s whose input %s carries hold:external; want it withheld", step.ID, input.ID)
		}
	}
}

// TestNativeInputAdmissionWithholdsAcrossReasonsAndStores covers AC-1: each
// withholding reason on the input (both canonical holds, raw blocked status, a
// future deferral) withholds the step over a unified store.
func TestNativeInputAdmissionWithholdsAcrossReasonsAndStores(t *testing.T) {
	cases := []struct {
		name   string
		input  beads.Bead
		mutate func(t *testing.T, store beads.Store, input beads.Bead)
	}{
		{
			name: "hold external unified",
			mutate: func(t *testing.T, store beads.Store, input beads.Bead) {
				if err := store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatalf("holding input: %v", err)
				}
			},
		},
		{
			name: "hold mayor unified",
			mutate: func(t *testing.T, store beads.Store, input beads.Bead) {
				if err := store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:mayor"}}); err != nil {
					t.Fatalf("holding input: %v", err)
				}
			},
		},
		{
			name: "blocked status unified",
			mutate: func(t *testing.T, store beads.Store, input beads.Bead) {
				status := "blocked"
				if err := store.Update(input.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("blocking input: %v", err)
				}
			},
		},
		{
			name: "future deferral unified",
			input: beads.Bead{
				Title:      "input",
				Type:       "task",
				DeferUntil: ptrTime(time.Now().Add(time.Hour)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			fx := materializeInputWorkflowWithInput(t, store, nil, tc.input)
			if tc.mutate != nil {
				tc.mutate(t, store, fx.input)
			}
			rows, err := Read(fx.legs(), fx.routedQuery())
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if containsID(rows, fx.step.ID) {
				t.Fatalf("Ready contains graph-step %s; want it withheld", fx.step.ID)
			}
		})
	}
}

// TestNativeInputAdmissionWithholdsOnSplitStores covers AC-1's split-city arm:
// the root and its step in the graph binding, the input and its convoy in the
// work store, the input on hold:external.
func TestNativeInputAdmissionWithholdsOnSplitStores(t *testing.T) {
	work, binding := splittest.NewSplitStores(t)
	fx := materializeInputWorkflow(t, work, binding)
	if err := work.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
		t.Fatalf("holding input: %v", err)
	}
	rows, err := Read(fx.legs(), fx.routedQuery())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if containsID(rows, fx.step.ID) {
		t.Fatalf("Ready contains graph-step %s; want it withheld", fx.step.ID)
	}
}

// TestNativeInputAdmissionPrerequisiteWithholdsAndReleases covers AC-3: a
// ready-blocking prerequisite authored AFTER the workflow was materialized — a
// blocks edge from the input to an open target, no workflow bead edited —
// withholds the step, and closing the target re-admits it: the edge and the
// target's copies are judged live on every read. On the split city the target
// sits beside the input in the work store (a dep row cannot reference another
// store's id namespace), so the step in the graph store is judged from input
// facts the graph never holds.
func TestNativeInputAdmissionPrerequisiteWithholdsAndReleases(t *testing.T) {
	withholds := func(t *testing.T, fx inputFixture) {
		t.Helper()
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if containsID(rows, fx.step.ID) {
			t.Fatalf("Ready contains graph-step %s whose input waits on an open prerequisite; want it withheld", fx.step.ID)
		}
	}
	admits := func(t *testing.T, fx inputFixture) {
		t.Helper()
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits graph-step %s whose input's prerequisite target is closed; want it admitted", fx.step.ID)
		}
	}
	t.Run("target in the work store unified", func(t *testing.T) {
		store := beads.NewMemStore()
		fx := materializeInputWorkflow(t, store, nil)
		target := mustCreate(t, store, beads.Bead{Title: "prerequisite target", Type: "task"})
		if err := store.DepAdd(fx.input.ID, target.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd blocks: %v", err)
		}
		withholds(t, fx)
		if err := store.Close(target.ID); err != nil {
			t.Fatalf("closing prerequisite target: %v", err)
		}
		admits(t, fx)
	})
	t.Run("target beside the input split", func(t *testing.T) {
		work, binding := splittest.NewSplitStores(t)
		fx := materializeInputWorkflow(t, work, binding)
		target := mustCreate(t, work, beads.Bead{Title: "prerequisite target", Type: "task"})
		if err := work.DepAdd(fx.input.ID, target.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd blocks: %v", err)
		}
		withholds(t, fx)
		if err := work.Close(target.ID); err != nil {
			t.Fatalf("closing prerequisite target: %v", err)
		}
		admits(t, fx)
	})
}

// TestNativeInputAdmissionBoundedReadRefillsPastWithheldRows covers AC-4: with
// a limit pushed to the legs, more than limit+slack withheld rows ahead of an
// admissible row must not end the read early — a withheld row is not a row in
// hand, so the leg is asked again, widening its own cut, until one of its
// rows can be served.
func TestNativeInputAdmissionBoundedReadRefillsPastWithheldRows(t *testing.T) {
	store := beads.NewMemStore()
	withheld := boundedSlack + Query{Limit: 1}.Limit + 4
	for i := 0; i < withheld; i++ {
		fx := materializeInputWorkflow(t, store, nil)
		if err := store.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			t.Fatalf("holding input %d: %v", i, err)
		}
	}
	admissible := materializeInputWorkflow(t, store, nil)
	rows, err := Read([]Leg{testLeg("city", store)}, Query{Limit: 1, MetadataFields: []string{"gc.routed_to=gascity/pool"}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != admissible.step.ID {
		t.Fatalf("Read with limit 1 behind %d withheld rows = %d rows; want exactly the admissible step %s", withheld, len(rows), admissible.step.ID)
	}
}

// TestNativeInputAdmissionLegGateWithholdsOnUnifiedCity covers AC-4's gate arm:
// the controller's LegGate — the same gate demand and serve share — withholds
// a held-input row on a unified city too, not only a split one, and keeps the
// row once the hold is released.
func TestNativeInputAdmissionLegGateWithholdsOnUnifiedCity(t *testing.T) {
	store := beads.NewMemStore()
	fx := materializeInputWorkflow(t, store, nil)
	if err := store.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
		t.Fatalf("holding input: %v", err)
	}
	gate := NewLegGate([]Leg{testLeg("city", store)}, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	gated, err := gate.Gate(context.Background(), 0, []beads.Bead{fx.step})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if len(gated) != 0 {
		t.Fatalf("Gate kept held-input step %s on a unified city; want it withheld", fx.step.ID)
	}
	if err := store.Update(fx.input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatalf("releasing hold: %v", err)
	}
	gate = NewLegGate([]Leg{testLeg("city", store)}, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	gated, err = gate.Gate(context.Background(), 0, []beads.Bead{fx.step})
	if err != nil {
		t.Fatalf("Gate after release: %v", err)
	}
	if len(gated) != 1 || gated[0].ID != fx.step.ID {
		t.Fatalf("Gate after release = %v; want the step kept", idsOf(gated))
	}
}

// TestNativeInputAdmissionOpenInputStaysEligible covers AC-2: an input that is
// open with no unsatisfied ready-blocking dependency does not withhold its
// workflow's step, and neither does an in_progress input — existing ownership
// is never an admission constraint.
func TestNativeInputAdmissionOpenInputStaysEligible(t *testing.T) {
	t.Run("open planning input unified", func(t *testing.T) {
		store := beads.NewMemStore()
		fx := materializeInputWorkflow(t, store, nil)
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits graph-step %s whose open input carries no hold, block, deferral or prerequisite; want it admitted", fx.step.ID)
		}
	})
	t.Run("open planning input split", func(t *testing.T) {
		work, binding := splittest.NewSplitStores(t)
		fx := materializeInputWorkflow(t, work, binding)
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits graph-step %s; want it admitted", fx.step.ID)
		}
	})
	t.Run("in_progress input unified", func(t *testing.T) {
		store := beads.NewMemStore()
		fx := materializeInputWorkflow(t, store, nil)
		setInProgress(t, store, fx.input.ID, "seat-1")
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits graph-step %s whose input is in_progress (existing ownership); want it admitted", fx.step.ID)
		}
	})
}

// TestNativeInputAdmissionServesControlKindRowsWithHeldInput covers AC-7's
// control arm: a control-kind row is dispatcher bookkeeping, never a subject of
// input admission, so a held input on its workflow's convoy withholds nothing —
// check, drain and finalization keep running through a park exactly as before
// the gate existed.
func TestNativeInputAdmissionServesControlKindRowsWithHeldInput(t *testing.T) {
	for _, kind := range []string{beadmeta.KindCheck, beadmeta.KindDrain, beadmeta.KindWorkflowFinalize} {
		t.Run(kind, func(t *testing.T) {
			store := beads.NewMemStore()
			fx := materializeInputWorkflow(t, store, nil)
			if err := store.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
				t.Fatalf("holding input: %v", err)
			}
			control := mustCreate(t, store, beads.Bead{
				Title: "control row",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       kind,
					beadmeta.RootBeadIDMetadataKey: fx.root.ID,
					beadmeta.RoutedToMetadataKey:   "gascity/pool",
				},
			})
			rows, err := Read(fx.legs(), fx.routedQuery())
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !containsID(rows, control.ID) {
				t.Fatalf("Ready omits %s control row %s whose workflow input is held; control kinds are never judged", kind, control.ID)
			}
			if containsID(rows, fx.step.ID) {
				t.Fatalf("Ready contains graph-step %s whose input is held; want only the control row served", fx.step.ID)
			}
		})
	}
}

// TestNativeInputAdmissionServesStoppedWorkflowRows covers AC-7's stopped arm:
// once a workflow has settled or been canceled, its remaining rows belong to
// the stop gates and the teardown tail, so a held input withholds nothing —
// the rows stay visible to whatever closes them, exactly as before the gate.
func TestNativeInputAdmissionServesStoppedWorkflowRows(t *testing.T) {
	t.Run("closed root", func(t *testing.T) {
		store := beads.NewMemStore()
		fx := materializeInputWorkflow(t, store, nil)
		if err := store.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			t.Fatalf("holding input: %v", err)
		}
		if err := store.Close(fx.root.ID); err != nil {
			t.Fatalf("closing root: %v", err)
		}
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits teardown-tail step %s of a settled workflow whose input is held; the tail must keep running", fx.step.ID)
		}
	})
	t.Run("cancel requested", func(t *testing.T) {
		store := beads.NewMemStore()
		fx := materializeInputWorkflow(t, store, nil)
		if err := store.Update(fx.input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			t.Fatalf("holding input: %v", err)
		}
		if err := store.Update(fx.root.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.CancelRequestedMetadataKey: "1"}}); err != nil {
			t.Fatalf("stamping cancel intent: %v", err)
		}
		rows, err := Read(fx.legs(), fx.routedQuery())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !containsID(rows, fx.step.ID) {
			t.Fatalf("Ready omits step %s of a cancel-requested workflow whose input is held; the stop gate must keep seeing it", fx.step.ID)
		}
	})
}
