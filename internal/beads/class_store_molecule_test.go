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
