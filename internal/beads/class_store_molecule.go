package beads

// MoleculeTarget is the store a molecule is materialized in, read from or torn
// down in (typed-stores design T5a): the graph class for a graph.v2 workflow or
// a root-only wisp, the work class for a v1 poured molecule. Which one a recipe
// belongs in is molecule.RecipeClass's answer; the target records the caller's
// choice, so a molecule operation can take either class without a raw store.
//
// It is built only from a class handle (GraphMolecule, WorkMolecule), so it
// carries no class the caller did not already hold. Like the class handles it
// promotes every Store method over the wrapped store, is a class layer that As
// and SameStore see through, and declares the wrapped store as its
// conditional-writes resolution target.
type MoleculeTarget struct {
	Store
	graph bool
}

// GraphMolecule is the graph-class molecule target over s.
func GraphMolecule(s GraphStore) MoleculeTarget { return MoleculeTarget{Store: s.Store, graph: true} }

// WorkMolecule is the work-class molecule target over s.
func WorkMolecule(s WorkStore) MoleculeTarget { return MoleculeTarget{Store: s.Store} }

// IsGraph reports whether t is a graph-class target.
func (t MoleculeTarget) IsGraph() bool { return t.graph }

// Absent reports whether t wraps no store: the zero target, or a target built
// from a zero handle.
func (t MoleculeTarget) Absent() bool { return t.Store == nil }

// Raw returns the store the target wraps; see WorkStore.Raw.
func (t MoleculeTarget) Raw() Store { return t.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (t MoleculeTarget) ConditionalWritesResolveTarget() Store { return t.Store }

func (t MoleculeTarget) unwrapClassLayer() Store { return t.Store }

var _ ConditionalWritesResolveTargeter = MoleculeTarget{}

var _ GuardedBatcherHandleProvider = MoleculeTarget{}

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store,
// as the class handles do (class_store_guarded_batch.go).
func (t MoleculeTarget) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(t.Store)
}

// WorkHandle is the work handle this target was built from: present only for a
// present work-class target. A graph-class or absent target yields none, so a
// zero handle never becomes a present one.
func (t MoleculeTarget) WorkHandle() (WorkStore, bool) {
	if t.Absent() || t.graph {
		return WorkStore{}, false
	}
	return WorkStore{Store: t.Store}, true
}

// GraphHandle is the graph handle this target was built from: present only for
// a present graph-class target. A work-class or absent target yields none, so
// a zero handle never becomes a present one.
func (t MoleculeTarget) GraphHandle() (GraphStore, bool) {
	if t.Absent() || !t.graph {
		return GraphStore{}, false
	}
	return GraphStore{Store: t.Store}, true
}
