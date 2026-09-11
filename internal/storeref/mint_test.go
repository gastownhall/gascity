package storeref

// Whether a binding mints truthfully.
//
// ClassBinding.MintsReserved is half of the residence-probe retirement
// condition, and its doc has always demanded a VERIFIED check rather than a
// constructor's optimism. The verification available is the store's own
// declaration: a store that implements HasIDPrefix names the namespace it mints
// into, and the binding names the namespaces it claims. When the first is one
// of the second, a bead this binding creates from now on is recognizable from
// its id alone — which is exactly what the retirement premise needs.
//
// It is only ever half. A store that mints truthfully today can still hold
// relics migrated in under a foreign id, which is why HasLegacyResidents is the
// other half and why nothing retires until a census says so.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestMintsInsideNamespaceMatchesADeclaredPrefix(t *testing.T) {
	store := newPrefixedStore("gcg")
	if !MintsInsideNamespace(store, []string{"gcg", "gcm", "gcs", "gco", "gcn", "gcnq"}) {
		t.Error("a store minting gcg- into a binding that claims gcg is not recognized as minting truthfully")
	}
}

func TestMintsInsideNamespaceRejectsAnUnclaimedPrefix(t *testing.T) {
	// The shape the field exists to exclude: the binding claims the graph
	// namespace but its store mints work ids, so a new bead's id says nothing
	// about where it lives.
	store := newPrefixedStore("ga")
	if MintsInsideNamespace(store, []string{"gcg"}) {
		t.Error("a store minting ga- was called truthful for a gcg binding")
	}
}

// The control that keeps the check from defaulting to optimism: a store that
// declares nothing has verified nothing.
func TestMintsInsideNamespaceRejectsAStoreThatDeclaresNothing(t *testing.T) {
	if MintsInsideNamespace(beads.NewMemStore(), []string{"gcg"}) {
		t.Error("a store with no IDPrefix() was called truthful; nothing about it was verified")
	}
	if MintsInsideNamespace(newPrefixedStore(""), []string{"gcg"}) {
		t.Error("a store declaring an empty prefix was called truthful")
	}
	if MintsInsideNamespace(nil, []string{"gcg"}) {
		t.Error("a nil store was called truthful")
	}
}

func TestMintsInsideNamespaceRejectsABindingThatClaimsNothing(t *testing.T) {
	if MintsInsideNamespace(newPrefixedStore("gcg"), nil) {
		t.Error("a binding claiming no namespace cannot have a store minting inside it")
	}
}

// newPrefixedStore is newPrefixed as a beads.Store, so a row can pass the nil
// interface the "declares nothing" control needs.
func newPrefixedStore(prefix string) beads.Store { return newPrefixed(prefix) }
