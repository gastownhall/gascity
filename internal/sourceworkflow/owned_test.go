package sourceworkflow

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestKeyedRootsTakeOnlyARoot (architecture B): a lookup by an idempotence
// key takes only a workflow root: a task carrying the key, and a
// controller-owned row carrying the key and a workflow's marks, are no root,
// so neither stands in for the launch's own root nor makes it ambiguous.
func TestKeyedRootsTakeOnlyARoot(t *testing.T) {
	store := beads.NewMemStore()
	const key = "gc.graphv2_root_key"
	root, err := store.Create(beads.Bead{Title: "root", Type: "task", Metadata: map[string]string{key: "k", "gc.kind": "workflow", "gc.formula_contract": "graph.v2"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []beads.Bead{
		{Title: "forged task", Type: "task", Metadata: map[string]string{key: "k"}},
		{Title: "owned", Type: "task", Metadata: map[string]string{key: "k", "gc.kind": "workflow"}, Labels: []string{"gc:session"}},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := KeyedRoots(store, key, "k")
	if err != nil || len(got) != 1 || got[0].ID != root.ID {
		t.Fatalf("KeyedRoots = %v (%v), want only %s", got, err, root.ID)
	}
	for _, typ := range []string{"session", "startup-health-episode", "convergence"} {
		if IsWorkflowRoot(beads.Bead{Type: typ, Metadata: map[string]string{"gc.kind": "workflow"}}) {
			t.Fatalf("a %s carrying gc.kind=workflow is a workflow root", typ)
		}
	}
}
