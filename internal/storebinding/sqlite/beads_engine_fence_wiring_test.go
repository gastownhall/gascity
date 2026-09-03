package sqlite

// That the fence engineReservedPrefixes computes is the fence the opened store
// actually enforces.
//
// The tests next door pin what the function returns. They cannot see whether
// the return value ever reaches the store: deleting the
// WithSQLiteStoreReservedIDPrefixes option from OpenEngine leaves every one of
// them green and ships a binding that accepts any pinned id at all — the exact
// state the fence was added to end. The rows here go through OpenEngine and ask
// the store, which is the only place that question has an answer.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// openBeadsEngine drives OpenEngine for the given classes and returns the store
// it hands back, closed on cleanup.
func openBeadsEngine(t *testing.T, classes ...coordclass.Class) beads.Store {
	t.Helper()
	spec := beadsTestSpec(t.TempDir())
	generic, err := BeadsProviderFactory{}.New(spec)
	if err != nil {
		t.Fatalf("binding the Beads provider: %v", err)
	}
	provider, ok := generic.(*beadsProvider)
	if !ok {
		t.Fatalf("New returned %T, want *beadsProvider", generic)
	}
	store, closer, err := provider.OpenEngine(provider.spec, classSet(t, classes...))
	if err != nil {
		t.Fatalf("OpenEngine(%v): %v", classes, err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return store
}

func TestOpenEngineFencesAnInfrastructureBindingToEveryNamespaceItHolds(t *testing.T) {
	store := openBeadsEngine(t, coordclass.ClassGraph, coordclass.ClassNudges)

	if _, err := store.Create(beads.Bead{ID: "ga-1", Title: "a work id pinned into an infrastructure binding"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
		t.Errorf("Create(\"ga-1\") = %v, want ErrPinnedIDOutsideNamespace; the computed namespace set never reached the store, and this binding's id claim is decorative", err)
	}

	// The second half of the wiring, and the reason this is not one assertion:
	// a binding holds more than the class it mints under. The nudge queue's
	// records live in the nudges store under their own namespace, so passing
	// only the first computed prefix — or only the graph one this store mints
	// with — would refuse them. Both must be reachable.
	for _, id := range []string{"gcg-pinned", "gcn-pinned", "gcnq-pinned"} {
		if _, err := store.Create(beads.Bead{ID: id, Title: "held by this binding"}); err != nil {
			t.Errorf("Create(%q) was refused: %v; the store was fenced to less than the binding holds", id, err)
		}
	}
}

// A binding that serves work is unfenceable — work beads carry the operator's
// configured rig or HQ prefix — and OpenEngine must pass that through as an
// unfenced store rather than as an empty-but-present namespace set, which would
// refuse everything.
func TestOpenEngineLeavesAWorkServingBindingUnfenced(t *testing.T) {
	store := openBeadsEngine(t, coordclass.ClassWork, coordclass.ClassGraph)
	if _, err := store.Create(beads.Bead{ID: "someRig-1", Title: "a work bead under an operator-configured prefix"}); err != nil {
		t.Errorf("Create(\"someRig-1\") was refused: %v; a binding serving work claims no namespace and must hold whatever the operator configured", err)
	}
}
