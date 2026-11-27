package readywork

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// nativeWorkflow is one native workflow fixture: an input tracked by a
// convoy, a graph.v2 root naming that convoy, and a routed ready step of the
// root with no graph-local blocker.
type nativeWorkflow struct {
	input, convoy, root, step beads.Bead
}

// newNativeWorkflow creates the input and its convoy in work, and the root and
// its step in graph (the same store on a unified city).
func newNativeWorkflow(t *testing.T, work, graph beads.Store, input beads.Bead) nativeWorkflow {
	t.Helper()
	if input.Title == "" {
		input.Title = "input"
	}
	if input.Type == "" {
		input.Type = "task"
	}
	status := input.Status
	input.Status = ""
	in := mustCreate(t, work, input)
	if status != "" {
		if err := work.Update(in.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("setting input %s status %s: %v", in.ID, status, err)
		}
	}
	conv := mustCreate(t, work, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := work.DepAdd(conv.ID, in.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	root := mustCreate(t, graph, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.input_convoy_id": conv.ID},
	})
	step := mustCreate(t, graph, beads.Bead{
		Title: "graph-step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.routed_to":    "gascity/pool",
		},
	})
	return nativeWorkflow{input: in, convoy: conv, root: root, step: step}
}

var poolQuery = Query{MetadataFields: []string{"gc.routed_to=gascity/pool"}}

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

// TestNativeInputAdmissionWithholdsEveryInputFact is AC-1: each "do not start
// yet" fact on the input (both canonical holds, raw blocked status, a dated
// and an indefinite deferral) withholds the step, on a unified store and on
// split work/graph stores.
func TestNativeInputAdmissionWithholdsEveryInputFact(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	facts := map[string]beads.Bead{
		"hold:external":       {Labels: []string{"hold:external"}},
		"hold:mayor":          {Labels: []string{"hold:mayor"}},
		"blocked status":      {Status: "blocked"},
		"future defer_until":  {DeferUntil: &future},
		"indefinite deferral": {IndefinitelyDeferred: true},
	}
	for city, open := range openCities {
		for fact, input := range facts {
			t.Run(city+"/"+fact, func(t *testing.T) {
				work, graph, legs := open(t)
				wf := newNativeWorkflow(t, work, graph, input)
				if ids := readyIDs(t, legs, poolQuery); slices.Contains(ids, wf.step.ID) {
					t.Fatalf("Ready = %v contains step %s whose input %s is %s; want it withheld", ids, wf.step.ID, wf.input.ID, fact)
				}
			})
		}
	}
}

// TestNativeInputAdmissionPrerequisiteAddedAfterMaterialization is AC-3: a
// ready-blocking prerequisite added to the input after the workflow was
// materialized withholds the step until the prerequisite closes, whether the
// prerequisite lives in the input's store or, named "external:<id>", in the
// other store of a split city.
func TestNativeInputAdmissionPrerequisiteAddedAfterMaterialization(t *testing.T) {
	cases := map[string]struct {
		split   bool
		inGraph bool
	}{
		"unified/same store": {},
		"split/same store":   {split: true},
		"split/other store":  {split: true, inGraph: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			work := beads.NewMemStore()
			graph, legs := beads.Store(work), []Leg{testLeg("city", work)}
			if tc.split {
				graph = sqliteBinding(t)
				legs = splitLegs(work, graph)
			}
			wf := newNativeWorkflow(t, work, graph, beads.Bead{})
			if ids := readyIDs(t, legs, poolQuery); !slices.Contains(ids, wf.step.ID) {
				t.Fatalf("Ready = %v lacks step %s before any prerequisite; want it admitted", ids, wf.step.ID)
			}

			home := beads.Store(work)
			if tc.inGraph {
				home = graph
			}
			blocker := mustCreate(t, home, beads.Bead{Title: "prerequisite", Type: "task"})
			target := blocker.ID
			if tc.inGraph {
				target = "external:" + blocker.ID
			}
			if err := work.DepAdd(wf.input.ID, target, "blocks"); err != nil {
				t.Fatalf("DepAdd blocks: %v", err)
			}
			if ids := readyIDs(t, legs, poolQuery); slices.Contains(ids, wf.step.ID) {
				t.Fatalf("Ready = %v contains step %s whose input %s waits on open prerequisite %s; want it withheld", ids, wf.step.ID, wf.input.ID, blocker.ID)
			}

			closeBead(t, home, blocker.ID)
			if ids := readyIDs(t, legs, poolQuery); !slices.Contains(ids, wf.step.ID) {
				t.Fatalf("Ready = %v lacks step %s after prerequisite %s closed; want it readmitted", ids, wf.step.ID, blocker.ID)
			}
		})
	}
}

// openCities are the two city shapes the admission tests run on: one store,
// and split work/graph stores.
var openCities = map[string]func(t *testing.T) (work, graph beads.Store, legs []Leg){
	"unified": func(*testing.T) (beads.Store, beads.Store, []Leg) {
		store := beads.NewMemStore()
		return store, store, []Leg{testLeg("city", store)}
	},
	"split": func(t *testing.T) (beads.Store, beads.Store, []Leg) {
		work := beads.NewMemStore()
		binding := sqliteBinding(t)
		return work, binding, splitLegs(work, binding)
	},
}

// TestNativeInputAdmissionLeavesExemptRowsServed is AC-7: under a held input,
// a control-kind row of the workflow, every row of a stopped workflow (a
// closed, canceled or cancel-requested root, the teardown tail included) and an
// in_progress step a session owns are served exactly as without input
// admission.
func TestNativeInputAdmissionLeavesExemptRowsServed(t *testing.T) {
	setRoot := func(t *testing.T, graph beads.Store, rootID string, opts beads.UpdateOpts) {
		t.Helper()
		if err := graph.Update(rootID, opts); err != nil {
			t.Fatalf("updating root %s: %v", rootID, err)
		}
	}
	cases := map[string]func(t *testing.T, graph beads.Store, wf nativeWorkflow) (served string, q Query){
		"control kind": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			finalize := mustCreate(t, graph, beads.Bead{Title: "finalize", Type: "task", Metadata: map[string]string{
				"gc.kind": "workflow-finalize", "gc.root_bead_id": wf.root.ID, "gc.routed_to": "gascity/pool",
			}})
			return finalize.ID, poolQuery
		},
		"canceled root": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			setRoot(t, graph, wf.root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.outcome": "canceled"}})
			return wf.step.ID, poolQuery
		},
		"cancel requested root": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			setRoot(t, graph, wf.root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.cancel_requested": "true"}})
			return wf.step.ID, poolQuery
		},
		"closed root": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			closeBead(t, graph, wf.root.ID)
			return wf.step.ID, poolQuery
		},
		"teardown tail of canceled root": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			teardown := mustCreate(t, graph, beads.Bead{Title: "teardown", Type: "task", Metadata: map[string]string{
				"gc.root_bead_id": wf.root.ID, "gc.scope_role": "teardown", "gc.routed_to": "gascity/pool",
			}})
			setRoot(t, graph, wf.root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.outcome": "canceled"}})
			return teardown.ID, poolQuery
		},
		"in_progress step owned by a session": func(t *testing.T, graph beads.Store, wf nativeWorkflow) (string, Query) {
			setInProgress(t, graph, wf.step.ID, "worker-1")
			return wf.step.ID, Query{Assignee: "worker-1", Status: "in_progress"}
		},
	}
	for city, open := range openCities {
		for name, arrange := range cases {
			t.Run(city+"/"+name, func(t *testing.T) {
				work, graph, legs := open(t)
				wf := newNativeWorkflow(t, work, graph, beads.Bead{Labels: []string{"hold:external"}})
				served, q := arrange(t, graph, wf)
				if ids := readyIDs(t, legs, q); !slices.Contains(ids, served) {
					t.Fatalf("Ready(%+v) = %v lacks %s under held input %s; want it served as without input admission", q, ids, served, wf.input.ID)
				}
			})
		}
	}
}

// TestNativeInputAdmissionBoundedReadRefillsPastWithheldRows is AC-4: on a
// unified city, more than limit+20 withheld steps ordered ahead of an
// admissible one do not end a bounded read short; the admissible step is the
// answer and no withheld one is.
func TestNativeInputAdmissionBoundedReadRefillsPastWithheldRows(t *testing.T) {
	store := beads.NewMemStore()
	held := newNativeWorkflow(t, store, store, beads.Bead{Labels: []string{"hold:external"}})
	first, later := 0, 3
	withheld := map[string]bool{held.step.ID: true}
	for i := 0; i < boundedSlack+5; i++ {
		b := mustCreate(t, store, beads.Bead{Title: "withheld step", Type: "task", Priority: &first, Metadata: map[string]string{
			"gc.root_bead_id": held.root.ID, "gc.routed_to": "gascity/pool",
		}})
		withheld[b.ID] = true
	}
	free := newNativeWorkflow(t, store, store, beads.Bead{})
	if err := store.Update(free.step.ID, beads.UpdateOpts{Priority: &later}); err != nil {
		t.Fatalf("lowering step priority: %v", err)
	}
	legs := []Leg{testLeg("city", externalBlockerStore{Store: store})}
	q := poolQuery
	q.Limit = 1
	if got := readyIDs(t, legs, q); !slices.Equal(got, []string{free.step.ID}) {
		t.Fatalf("Ready(limit=1) = %v, want [%s]: every step ahead of it is withheld by held input %s", got, free.step.ID, held.input.ID)
	}
}

// TestNativeInputAdmissionLegGateOnUnifiedCity is AC-4's demand half: the leg
// gate the controller and the API apply judges input admission on a city with
// no class binding too.
func TestNativeInputAdmissionLegGateOnUnifiedCity(t *testing.T) {
	store := beads.NewMemStore()
	held := newNativeWorkflow(t, store, store, beads.Bead{Labels: []string{"hold:external"}})
	free := newNativeWorkflow(t, store, store, beads.Bead{})
	rows, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	gated, err := NewLegGate([]Leg{testLeg("city", store)}, beads.ReadyQuery{}).Gate(context.Background(), 0, rows)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	ids := make([]string, 0, len(gated))
	for _, b := range gated {
		ids = append(ids, b.ID)
	}
	if slices.Contains(ids, held.step.ID) || !slices.Contains(ids, free.step.ID) {
		t.Fatalf("Gate = %v, want %s (free input) and not %s (input %s held)", ids, free.step.ID, held.step.ID, held.input.ID)
	}
}
