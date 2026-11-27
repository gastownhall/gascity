package readywork

import (
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
	cities := map[string]func(t *testing.T) (work, graph beads.Store, legs []Leg){
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
	for city, open := range cities {
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
