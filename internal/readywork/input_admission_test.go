package readywork

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestNativeInputAdmissionReadyAndDemand(t *testing.T) {
	work, graph := beads.NewMemStore(), beads.NewMemStore()
	work.HonorExplicitIDs, graph.HonorExplicitIDs = true, true
	input := mustCreate(t, work, beads.Bead{ID: "work-input", Title: "input", Labels: []string{"hold:external"}})
	conv := mustCreate(t, work, beads.Bead{ID: "work-inputs", Type: "convoy", Title: "inputs"})
	if err := work.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	root := mustCreate(t, graph, beads.Bead{ID: "graph-root", Title: "root", Type: "molecule", Metadata: map[string]string{"gc.kind": "workflow", "gc.input_convoy_id": conv.ID}})
	step := mustCreate(t, graph, beads.Bead{ID: "graph-step", Title: "step", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "rig/pool"}})
	legs := []Leg{{Label: "work", Store: work, Work: beadstest.Work(work), Graph: beadstest.Graph(work)}, {Label: "graph", Store: graph, Graph: beadstest.Graph(graph), ClassBinding: true}}
	query := Query{MetadataFields: []string{"gc.routed_to=rig/pool"}, Limit: 1}
	check := func(want int) {
		t.Helper()
		rows, err := Read(legs, query)
		if err != nil || len(rows) != want {
			t.Fatalf("Ready = %v, %v; want %d rows", rows, err, want)
		}
		gate := NewLegGate(legs, beads.ReadyQuery{})
		demand, err := gate.Gate(context.Background(), 1, []beads.Bead{step})
		if err != nil || len(demand) != want {
			t.Fatalf("demand = %v, %v; want %d rows", demand, err, want)
		}
	}
	check(0)
	progress, worker := "in_progress", "worker"
	if err := graph.Update(step.ID, beads.UpdateOpts{Status: &progress, Assignee: &worker}); err != nil {
		t.Fatal(err)
	}
	resumed, err := ReadUntilUnblocked(context.Background(), legs, Query{Status: StatusInProgress, Assignee: worker, Limit: 1})
	if err != nil || len(resumed) != 1 || len(resumed[0].BlockedBy) != 1 || resumed[0].BlockedBy[0].ID != input.ID {
		t.Fatalf("held resume = %v, %v; want actual input blocker", resumed, err)
	}
	if err := work.Update(input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	resumed, err = ReadUntilUnblocked(context.Background(), legs, Query{Status: StatusInProgress, Assignee: worker, Limit: 1})
	if err != nil || len(resumed) != 1 || len(resumed[0].BlockedBy) != 0 {
		t.Fatalf("ready resume = %v, %v", resumed, err)
	}
	open := "open"
	if err := graph.Update(step.ID, beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatal(err)
	}
	check(1)
}

func TestNativeInputAdmissionBeforeReadyLimit(t *testing.T) {
	store := beads.NewMemStore()
	conv := mustCreate(t, store, beads.Bead{ID: "convoy", Type: "convoy", Title: "inputs", Labels: []string{"hold:mayor"}})
	root := mustCreate(t, store, beads.Bead{ID: "root", Type: "molecule", Title: "workflow", Metadata: map[string]string{"gc.input_convoy_id": conv.ID}})
	// More held rows than the ordered-read slack must not starve the later ready row.
	for i := range boundedSlack + 2 {
		p := 0
		mustCreate(t, store, beads.Bead{Title: "held", Priority: &p, Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "pool", "index": string(rune('a' + i))}})
	}
	ready := mustCreate(t, store, beads.Bead{Title: "ready", Metadata: map[string]string{"gc.routed_to": "pool"}})
	rows, err := Read([]Leg{{Label: "unified", Store: store, Work: beadstest.Work(store), Graph: beadstest.Graph(store)}}, Query{MetadataFields: []string{"gc.routed_to=pool"}, Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].ID != ready.ID {
		t.Fatalf("Read = %v, %v; want %s", rows, err, ready.ID)
	}
}
