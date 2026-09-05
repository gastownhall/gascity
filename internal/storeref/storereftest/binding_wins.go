// Package storereftest holds the cross-plane residency properties — the ones
// that are only worth anything if EVERY by-id surface satisfies them.
//
// The residency resolver exists because planes disagreed about which store
// holds a bead. A per-plane test cannot detect that: two surfaces can each pass
// their own well-written pin and still answer differently, which is the bug. So
// the property lives here once, as an executable definition, and each plane
// supplies an adapter. A plane that drifts fails the shared clause with the
// shared sentence, and a plane that is never wired in is visibly absent rather
// than silently uncovered.
//
// This package is a TEST KIT that does not import the resolver: a kit built on
// the thing under test would pass by construction. It takes two stores and two
// closures and checks observable behavior.
package storereftest

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// BindingWinsStores is the dual-residency fixture: the two stores, the id both
// of them hold, and the control id only work holds.
//
// # Why dual residency is the shape worth pinning
//
// `gc storage migrate` copies every non-work bead with its id PRESERVED and
// never deletes the source, so after a migration a relocated bead exists twice:
// the binding holds the row the controller and the class doors read and write,
// and the work store holds a copy frozen at migration time. Both answer a Get
// for the same id. A binding-blind surface therefore does not 404 and does not
// error — it succeeds, against the stale copy, and a write that follows it
// lands where nothing reads. Exit 0, no diagnostic. That silence is why this
// has to be asserted rather than reasoned about.
type BindingWinsStores struct {
	// Binding is the relocated class store: the authoritative copy.
	Binding beads.Store

	// Work is the city work store holding the migration's retained copy.
	Work beads.Store

	// DualID is resident in BOTH stores under the same id, with a DIFFERENT
	// title in each so a read can be attributed to one of them.
	DualID string

	// BindingTitle is the title of the binding's copy of DualID.
	BindingTitle string

	// WorkOnlyID is held by Work alone and by no binding — the control. Without
	// it "the binding wins" and "residence decides" are the same test, and a
	// surface that routed unconditionally on the prefix would pass.
	WorkOnlyID string

	// WorkOnlyTitle is the title of WorkOnlyID's only copy.
	WorkOnlyTitle string
}

// BindingWinsSurface is one plane's by-id surface, reduced to the two verbs the
// property is about.
//
// Get and Close take a *testing.T so an adapter can fail with its own plane's
// diagnostic — an HTTP status, a CLI exit code and stderr — instead of
// flattening every transport failure into one opaque error this package would
// have to describe badly.
type BindingWinsSurface struct {
	// Name is the plane, for failure messages: "gc bd by-id door", "GET /beads".
	Name string

	// Get reads the bead the surface serves for id.
	Get func(t *testing.T, id string) beads.Bead

	// Close performs the surface's close of id.
	Close func(t *testing.T, id string)
}

// RunBindingWins asserts the four clauses every by-id surface must satisfy on a
// dual-resident city. Each is numbered in its failure message, so a plane that
// breaks one is diagnosed by clause rather than by which adapter noticed.
//
//	(1) The READ serves the binding's copy, not the retained work copy.
//	(2) The CLOSE writes the binding's copy, and the surface's own next read
//	    sees it — the write follows the read, on the same surface.
//	(3) The work copy is UNTOUCHED: one id, one owner, one write.
//	(4) The control id the binding never held still answers from work, which is
//	    what makes this residence rather than the binding winning everything.
//
// The fixture's own premise is checked first. A dual resident whose two copies
// carry the same title, or which one of the stores does not actually hold,
// makes every clause below vacuous — and a vacuously green cross-plane property
// is worse than no property, because it is cited as proof of agreement.
func RunBindingWins(t *testing.T, stores BindingWinsStores, surface BindingWinsSurface) {
	t.Helper()
	assertBindingWinsPremise(t, stores, surface)

	got := surface.Get(t, stores.DualID)
	if got.Title != stores.BindingTitle {
		t.Errorf("%s clause 1: reading the dual-resident %s served %q, want the binding's copy %q — the work copy is the migration's retained one, frozen when it was copied, so serving it is not the old answer but a stale one",
			surface.Name, stores.DualID, got.Title, stores.BindingTitle)
	}

	surface.Close(t, stores.DualID)

	bindingCopy, err := stores.Binding.Get(stores.DualID)
	if err != nil {
		t.Fatalf("%s clause 2: re-reading the binding's copy of %s: %v", surface.Name, stores.DualID, err)
	}
	if bindingCopy.Status != "closed" {
		t.Errorf("%s clause 2: the binding's copy of %s is %q after the close, want closed — a write that does not follow the read lands where nothing reads",
			surface.Name, stores.DualID, bindingCopy.Status)
	}

	if served := surface.Get(t, stores.DualID); served.Status != "closed" {
		t.Errorf("%s clause 2: after the close, the surface's own read of %s reports %q — the surface's read and its write disagree about which copy is authoritative, which is the divergence even a correct write cannot fix",
			surface.Name, stores.DualID, served.Status)
	}

	workCopy, err := stores.Work.Get(stores.DualID)
	if err != nil {
		t.Fatalf("%s clause 3: re-reading the work copy of %s: %v", surface.Name, stores.DualID, err)
	}
	if workCopy.Status == "closed" {
		t.Errorf("%s clause 3: the close reached the retained work copy of %s as well; one id, one owner, one write",
			surface.Name, stores.DualID)
	}

	control := surface.Get(t, stores.WorkOnlyID)
	if control.Title != stores.WorkOnlyTitle {
		t.Errorf("%s clause 4: the control %s served %q, want the work copy %q — an id no binding holds must still answer from work, or this surface routes on the prefix rather than on residence",
			surface.Name, stores.WorkOnlyID, control.Title, stores.WorkOnlyTitle)
	}
}

// assertBindingWinsPremise fails the test when the fixture does not model dual
// residency, so a clause can never pass for the wrong reason.
func assertBindingWinsPremise(t *testing.T, stores BindingWinsStores, surface BindingWinsSurface) {
	t.Helper()
	switch {
	case stores.Binding == nil || stores.Work == nil:
		t.Fatalf("%s: the binding-wins fixture needs both stores", surface.Name)
	case stores.DualID == "" || stores.WorkOnlyID == "":
		t.Fatalf("%s: the binding-wins fixture needs both a dual-resident id and a work-only control", surface.Name)
	case surface.Get == nil || surface.Close == nil:
		t.Fatalf("%s: the binding-wins surface needs both a Get and a Close", surface.Name)
	case stores.BindingTitle == stores.WorkOnlyTitle:
		t.Fatalf("%s: the binding copy and the control carry the same title, so clauses 1 and 4 cannot be told apart", surface.Name)
	}

	bindingCopy, err := stores.Binding.Get(stores.DualID)
	if err != nil {
		t.Fatalf("%s: the binding does not hold %s (%v); without two copies there is no dual residency to resolve", surface.Name, stores.DualID, err)
	}
	if bindingCopy.Title != stores.BindingTitle {
		t.Fatalf("%s: the binding's copy of %s is titled %q, not the declared %q", surface.Name, stores.DualID, bindingCopy.Title, stores.BindingTitle)
	}
	if bindingCopy.Status == "closed" {
		t.Fatalf("%s: the binding's copy of %s is already closed, so clause 2 cannot observe the close", surface.Name, stores.DualID)
	}

	workCopy, err := stores.Work.Get(stores.DualID)
	if err != nil {
		t.Fatalf("%s: the work store does not hold %s (%v); a surface cannot answer from the wrong copy if there is only one", surface.Name, stores.DualID, err)
	}
	if workCopy.Title == stores.BindingTitle {
		t.Fatalf("%s: both copies of %s are titled %q, so clause 1 cannot attribute a read to either store", surface.Name, stores.DualID, workCopy.Title)
	}
	if workCopy.Status == "closed" {
		t.Fatalf("%s: the retained work copy of %s is already closed, so clause 3 cannot observe a stray write", surface.Name, stores.DualID)
	}

	if _, err := stores.Binding.Get(stores.WorkOnlyID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("%s: the binding answers for the control id %s (%v); the control must be an id no binding holds", surface.Name, stores.WorkOnlyID, err)
	}
}
