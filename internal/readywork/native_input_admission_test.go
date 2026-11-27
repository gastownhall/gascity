package readywork

import (
	"testing"
	"time"

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
