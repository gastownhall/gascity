package beads

import (
	"errors"
	"testing"
)

// A row can carry a prefix this store does not mint: a pinned id, or a relic a
// storage migration copied in. The namespace boundary the weak-ParentID
// contract draws is the STORE's, so a parent inside the namespace this store
// mints is one it can see the absence of, and it must be refused before
// anything is written — whatever prefix the child happens to carry.
//
// Keying the question on the child's prefix instead reads a parent in the
// store's own namespace as foreign and lets the reparent land dangling, which
// is what main refused and what beads.Bead.ParentID promises.
func TestNativeDoltStoreResolvesAParentInItsOwnNamespaceForAForeignPrefixedChild(t *testing.T) {
	store := newNativeDoltStoreWithStorageAndPrefix(newNativeDoltMemStorage(), "native-test", "ga")
	relic, err := store.Create(Bead{ID: "gc-1", Title: "a row carrying another ledger's prefix"})
	if err != nil {
		t.Fatalf("Create with a pinned foreign-prefixed id: %v", err)
	}
	if relic.ID != "gc-1" {
		t.Fatalf("Create returned id %q, want the pinned %q", relic.ID, "gc-1")
	}

	dangling := "ga-999999"
	err = store.Update(relic.ID, UpdateOpts{ParentID: &dangling})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(%q, parent %q) = %v, want ErrNotFound: the parent is inside the namespace this store mints, so its absence is a row this store can see", relic.ID, dangling, err)
	}
	got, err := store.Get(relic.ID)
	if err != nil {
		t.Fatalf("Get after the refused reparent: %v", err)
	}
	if got.ParentID != "" {
		t.Errorf("ParentID after the refused reparent is %q, want it unchanged; the refusal has to come before the write", got.ParentID)
	}

	if _, err := store.Create(Bead{ID: "gc-2", Title: "same shape, on the create arm", ParentID: dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Create(child %q, parent %q) = %v, want ErrNotFound; Create and Update have to agree", "gc-2", dangling, err)
	}
	if _, err := store.Get("gc-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the refused create Get(%q) = %v, want ErrNotFound", "gc-2", err)
	}

	// The control that keeps this from being "resolve every parent": an id in a
	// namespace this store does not serve stays weak on both arms, because the
	// row it names lives in another ledger and refusing it would break every
	// cross-store molecule.
	foreign := "gcg-70b1e5f2-a"
	if err := store.Update(relic.ID, UpdateOpts{ParentID: &foreign}); err != nil {
		t.Errorf("Update(%q, parent %q) = %v, want it carried verbatim", relic.ID, foreign, err)
	}
	if _, err := store.Create(Bead{ID: "gc-3", Title: "child of another ledger's molecule", ParentID: foreign}); err != nil {
		t.Errorf("Create(child %q, parent %q) = %v, want it carried verbatim", "gc-3", foreign, err)
	}
}

// The conformance fixture opens this store with no declared prefix. Create is
// the one arm that cannot read the child's namespace off the child — the
// upstream library assigns the id — so a store that answered "foreign" for
// every parent there admitted a dangling parent inside its own namespace that
// Update, which sees the child's real id, refuses. The two arms have to agree:
// that is the rule the create-path skip is written against.
func TestNativeDoltStoreWithoutADeclaredPrefixAgreesBetweenCreateAndUpdate(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	control, err := store.Create(Bead{Title: "control"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	namespace := nativeBeadIDPrefix(control.ID)
	if namespace == "" {
		t.Fatalf("this fixture minted %q, which carries no namespace segment; the rows below would compare nothing", control.ID)
	}
	dangling := namespace + "-999999"

	if _, err := store.Create(Bead{Title: "step", ParentID: dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Create(parent %q) = %v, want ErrNotFound: the parent is in the namespace this store mints under, and Update refuses the same value", dangling, err)
	}
	if err := store.Update(control.ID, UpdateOpts{ParentID: &dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(parent %q) = %v, want ErrNotFound", dangling, err)
	}

	// Both arms stay weak for an id in a namespace this store does not mint.
	foreign := "gcg-70b1e5f2-a"
	child, err := store.Create(Bead{Title: "cross-store step", ParentID: foreign})
	if err != nil {
		t.Fatalf("Create(parent %q) = %v, want it carried verbatim", foreign, err)
	}
	if child.ParentID != foreign {
		t.Errorf("ParentID came back %q, want %q verbatim", child.ParentID, foreign)
	}
	if err := store.Update(control.ID, UpdateOpts{ParentID: &foreign}); err != nil {
		t.Errorf("Update(parent %q) = %v, want it carried verbatim", foreign, err)
	}
}
