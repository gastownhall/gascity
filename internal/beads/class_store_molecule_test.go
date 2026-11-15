package beads

import "testing"

// TestMoleculeTarget pins the molecule target (typed-stores T5a): built from
// a graph or a work handle, it records which class it is, is the same store
// as the handle it was built from, and is a class layer As and SameStore see
// through.
func TestMoleculeTarget(t *testing.T) {
	mem := NewMemStore()
	g := GraphMolecule(GraphStore{mem})
	w := WorkMolecule(WorkStore{mem})
	if !g.IsGraph() || w.IsGraph() {
		t.Fatalf("IsGraph: graph target %v, work target %v", g.IsGraph(), w.IsGraph())
	}
	if !SameStore(g, mem) || !SameStore(w, GraphStore{mem}) {
		t.Error("a molecule target is not the store its handle wraps")
	}
	if g.Raw() != Store(mem) {
		t.Error("Raw is not the wrapped store")
	}
	if _, ok := As[ConditionalWriter](g); !ok {
		t.Error("As does not see through a molecule target")
	}
	if g.ConditionalWritesResolveTarget() != Store(mem) {
		t.Error("the resolve target is not the wrapped store")
	}
	for name, tc := range map[string]struct {
		target MoleculeTarget
		absent bool
	}{
		"a graph target":    {g, false},
		"a work target":     {w, false},
		"the zero target":   {MoleculeTarget{}, true},
		"over a zero graph": {GraphMolecule(GraphStore{}), true},
		"over a zero work":  {WorkMolecule(WorkStore{}), true},
	} {
		if got := tc.target.Absent(); got != tc.absent {
			t.Errorf("%s: Absent = %v, want %v", name, got, tc.absent)
		}
	}
	if !GraphMolecule(GraphStore{}).IsGraph() {
		t.Error("a graph target over a zero handle forgot its class")
	}
}

// TestMoleculeTargetForwardsGuardedBatchDiscovery: GuardedBatcherFor finds a
// store's guarded batcher through a molecule target, as it does through a
// class handle.
func TestMoleculeTargetForwardsGuardedBatchDiscovery(t *testing.T) {
	mem := NewMemStore()
	want, ok := GuardedBatcherFor(mem)
	if !ok {
		t.Fatal("fixture: MemStore has no guarded batcher")
	}
	for name, target := range map[string]MoleculeTarget{
		"a graph target": GraphMolecule(GraphStore{mem}),
		"a work target":  WorkMolecule(WorkStore{mem}),
	} {
		got, ok := GuardedBatcherFor(target)
		if !ok {
			t.Errorf("%s: GuardedBatcherFor found no guarded batcher", name)
			continue
		}
		if got != want {
			t.Errorf("%s: GuardedBatcherFor = %#v, want the store's %#v", name, got, want)
		}
	}
}

// TestMoleculeTargetHandleProjections pins the class-faithful way back to the
// handle a molecule target was built from: a work target yields only a work
// handle, a graph target only a graph handle, both over the store the target
// wraps, and an absent target yields neither, so a zero handle never becomes
// a present one.
func TestMoleculeTargetHandleProjections(t *testing.T) {
	mem := NewMemStore()
	for name, tc := range map[string]struct {
		target    MoleculeTarget
		wantWork  bool
		wantGraph bool
	}{
		"a work target":     {WorkMolecule(WorkStore{mem}), true, false},
		"a graph target":    {GraphMolecule(GraphStore{mem}), false, true},
		"the zero target":   {MoleculeTarget{}, false, false},
		"over a zero work":  {WorkMolecule(WorkStore{}), false, false},
		"over a zero graph": {GraphMolecule(GraphStore{}), false, false},
	} {
		work, gotWork := tc.target.WorkHandle()
		graph, gotGraph := tc.target.GraphHandle()
		if gotWork != tc.wantWork {
			t.Errorf("%s: WorkHandle present = %v, want %v", name, gotWork, tc.wantWork)
		}
		if gotGraph != tc.wantGraph {
			t.Errorf("%s: GraphHandle present = %v, want %v", name, gotGraph, tc.wantGraph)
		}
		if gotWork && work.Store != Store(mem) {
			t.Errorf("%s: WorkHandle wraps a different store than the target", name)
		}
		if gotGraph && graph.Store != Store(mem) {
			t.Errorf("%s: GraphHandle wraps a different store than the target", name)
		}
		if !gotWork && work != (WorkStore{}) {
			t.Errorf("%s: an absent WorkHandle is not the zero handle", name)
		}
		if !gotGraph && graph != (GraphStore{}) {
			t.Errorf("%s: an absent GraphHandle is not the zero handle", name)
		}
	}
}
