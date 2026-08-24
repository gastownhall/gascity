package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestNativeDoltStoreConformance(t *testing.T) {
	beadstest.RunStoreTests(t, beads.NewNativeDoltStoreForConformance)
}

// TestNativeDoltStorePinnedIDFenceConformance runs the shared fenced-Create
// suite against the store the beads-workspace provider serves its bindings
// from.
//
// The SQLite provider has fenced since the contract landed; this one is the
// other half of invariant 16, and until this row existed a workspace binding
// held its namespace claim by convention alone. The hazard is constructible
// today: an infrastructure-only assignment (graph+nudges, say) opens a
// workspace whose Create honors any pinned id verbatim, so one foreign bead
// makes the claim false and leaves that bead unreachable by every id-shaped
// lookup of the namespace it actually sits in.
func TestNativeDoltStorePinnedIDFenceConformance(t *testing.T) {
	beadstest.RunPinnedIDFenceConformance(t, func(t *testing.T, mintPrefix string, namespaces ...string) beads.Store {
		t.Helper()
		return beads.NewNativeDoltStoreForPinnedIDFenceConformance(mintPrefix, namespaces...)
	})
}

// TestNativeDoltStoreIsAForeignIDCreator keeps two conformance rows from
// skipping green.
//
// The suite's TheForeignIDCreateStaysOpenForTheMigrationCopy and
// TheNamespaceIsCheckedBeforeExistence both t.Skip on a store that is not a
// ForeignIDCreator, which is right for a suite that cannot demand an optional
// capability — and invisible here. Those two rows are the ones that prove the
// fence leaves the `gc storage migrate` copy path open and that a refusal about
// a disclaimed namespace does not double as a probe for what the store holds.
// Drop CreateWithForeignID and both go quiet while the package still reports ok.
//
// The static `var _ ForeignIDCreator = (*NativeDoltStore)(nil)` next to the
// store catches that same removal at compile time, so on a build that compiles
// this row cannot fail on its own. It is here because the assert is a line
// somebody can delete while removing the method, and because it names the
// consequence — two rows go quiet — which a compile error does not.
func TestNativeDoltStoreIsAForeignIDCreator(t *testing.T) {
	store := beads.NewNativeDoltStoreForPinnedIDFenceConformance("kitn", "kitn")
	if _, ok := store.(beads.ForeignIDCreator); !ok {
		t.Fatalf("%T is not a beads.ForeignIDCreator; the two conformance rows that depend on it are skipping, and a skip reports as a pass", store)
	}
}
