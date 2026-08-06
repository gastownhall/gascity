package convoy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// MemberClasses names the class handles a single convoy operation spans.
//
// Convoy membership is the one deliberately mixed-class relation in the bead
// substrate: the convoy bead and its `tracks` edges are owned by one class
// (Work for a user convoy, Graph for a synthetic formula/drain convoy) while
// the members themselves can be owned by the other. Per the class-ownership
// table only Work and Graph ever own
// convoy members, so those are the only classes an operation can name here;
// Sessions, Messaging, Orders and Nudges are non-participating by construction
// rather than by a runtime skip.
//
// Naming is the whole contract, in both directions:
//
//   - A class the caller does not name does not participate. It is never read,
//     contributes an EMPTY result, and can never turn a lookup into an error.
//   - A class the caller does name participates. Its read failures are returned
//     with the class as provenance and are never flattened into "member not
//     found", because an unreachable owner and a deleted bead must not look
//     alike.
//
// This is explicit composition, not federation: there is no registry, no
// probing for a provider, and no first-match ownership. Candidate handles come
// from the caller, ownership is proven before any mutation, and a bead owned by
// two named handles is a typed error rather than a silent winner.
type MemberClasses struct {
	// Convoy is the handle for the class that owns the convoy bead itself and
	// its membership edges. It is required: every convoy operation reads or
	// writes the convoy's own edges through it, and it is also the first
	// candidate probed for members (a same-class convoy owns its members).
	Convoy beads.Store

	// Work names the Work-class scopes whose beads can be members. Work is a
	// topology, not one store — the HQ workspace plus each rig workspace — so
	// it is a slice; a caller that spans only the HQ scope names one handle.
	// Nil or empty means this operation does not span the Work class.
	Work []beads.Store

	// Graph names the Graph-class handle when graph-class beads (formula steps,
	// control beads, synthetic convoys) can be members. Nil means this
	// operation does not span the Graph class.
	Graph beads.Store
}

// ErrDuplicateResidence reports that one bead id was found in more than one
// named class handle. A bead has exactly one owner; two residences mean the
// candidate set is wrong or a migration left a copy behind, and neither a read
// nor a mutation may pick a winner. Match it with errors.Is.
var ErrDuplicateResidence = errors.New("duplicate residence")

// DuplicateResidenceError is the typed form of ErrDuplicateResidence, carrying
// the id and the names of every class that claimed it.
type DuplicateResidenceError struct {
	ID      string
	Classes []string
}

// Error implements error.
func (e *DuplicateResidenceError) Error() string {
	return fmt.Sprintf("bead %s is resident in more than one named class store (%s)", e.ID, strings.Join(e.Classes, ", "))
}

// Unwrap makes errors.Is(err, ErrDuplicateResidence) true.
func (e *DuplicateResidenceError) Unwrap() error { return ErrDuplicateResidence }

// ErrMemberNotCoResident reports that a membership edge cannot be written
// because the convoy and the member are owned by different class handles. A
// `tracks` dependency row lives in one store's dep table and can only reference
// an id that store can resolve, so a cross-class edge is refused before any
// write rather than left to fail inside the backend with "no issue found" —
// an error that names neither the convoy nor the class that could not see it,
// and that arrives after the caller has already decided the edge exists.
// Match it with errors.Is.
var ErrMemberNotCoResident = errors.New("convoy member is not co-resident with the convoy")

// ErrNoConvoyClass reports that an operation was given no handle for the class
// that owns the convoy. Every convoy operation reads or writes that class, so
// this is a caller error rather than an empty result. It also catches a typed
// nil handle, which is a NON-nil interface value and would otherwise panic on
// first use rather than fail with a diagnosis.
var ErrNoConvoyClass = errors.New("no handle named for the convoy's own class")

// requireConvoyHandle reports whether the operation can proceed at all.
func (m MemberClasses) requireConvoyHandle() error {
	if isNilStore(m.Convoy) {
		return ErrNoConvoyClass
	}
	return nil
}

// classHandle is one named candidate: a class name for provenance and the
// handle the caller supplied for it.
type classHandle struct {
	class string
	store beads.Store
}

// candidates returns the named class handles in deterministic probe order: the
// convoy's own class first (a same-class member is the common case), then each
// Work scope, then Graph. Unnamed classes and repeats of a handle already in
// the set are omitted, so naming the same physical store twice is one candidate
// and can never look like duplicate residence.
func (m MemberClasses) candidates() []classHandle {
	out := make([]classHandle, 0, 2+len(m.Work))
	add := func(class string, store beads.Store) {
		if isNilStore(store) {
			return
		}
		for _, existing := range out {
			if sameHandle(existing.store, store) {
				return
			}
		}
		out = append(out, classHandle{class: class, store: store})
	}
	add("convoy", m.Convoy)
	for i, w := range m.Work {
		class := coordclass.ClassWork.String()
		if len(m.Work) > 1 {
			class = fmt.Sprintf("%s[%d]", class, i)
		}
		add(class, w)
	}
	add(coordclass.ClassGraph.String(), m.Graph)
	return out
}

// resolveMember returns the bead and the class handle that owns it, applying
// the by-id residence rule: an exact prefix owner is authoritative on its own;
// otherwise every named class is probed and exactly one owner must answer.
// Zero owners return beads.ErrNotFound; more than one returns a
// *DuplicateResidenceError; a read failure from a named (participating) class
// is returned with that class as provenance even when another class holds the
// bead, because uniqueness cannot be proven while a participant is unreadable.
func (m MemberClasses) resolveMember(id string) (beads.Bead, classHandle, error) {
	cands := m.candidates()
	if len(cands) == 0 {
		return beads.Bead{}, classHandle{}, fmt.Errorf("resolving %s: no class handle named: %w", id, beads.ErrNotFound)
	}

	// Recorded ownership first: a store that mints id's prefix owns it, so no
	// probe of the other classes is needed and no second residence is possible.
	stores := make([]beads.Store, 0, len(cands))
	for _, c := range cands {
		stores = append(stores, c.store)
	}
	if owner := storeref.PrefixOwner(id, stores); owner != nil {
		for _, c := range cands {
			if !sameHandle(c.store, owner) {
				continue
			}
			got, err := owner.Get(id)
			if err == nil {
				return got, c, nil
			}
			if !errors.Is(err, beads.ErrNotFound) {
				return beads.Bead{}, classHandle{}, classReadError(c, id, err)
			}
			// A prefix owner that reports the bead absent means a partial
			// migration or a foreign id shape; fall through to the full
			// named-class probe rather than reducing correctness.
			break
		}
	}

	var (
		found      beads.Bead
		owner      classHandle
		haveOwner  bool
		duplicates []string
	)
	for _, c := range cands {
		got, err := c.store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return beads.Bead{}, classHandle{}, classReadError(c, id, err)
		}
		if haveOwner {
			if len(duplicates) == 0 {
				duplicates = append(duplicates, owner.class)
			}
			duplicates = append(duplicates, c.class)
			continue
		}
		found, owner, haveOwner = got, c, true
	}
	if len(duplicates) > 0 {
		return beads.Bead{}, classHandle{}, &DuplicateResidenceError{ID: id, Classes: duplicates}
	}
	if !haveOwner {
		return beads.Bead{}, classHandle{}, fmt.Errorf("resolving %s across %s: %w", id, strings.Join(classNames(cands), ", "), beads.ErrNotFound)
	}
	return found, owner, nil
}

// classReadError wraps a participating class's read failure with the class as
// provenance. beads.ErrMetadataParse keeps its identity so membership readers
// can still keep an unprojectable edge visible.
func classReadError(c classHandle, id string, err error) error {
	return fmt.Errorf("reading %s from the %s class store: %w", id, c.class, err)
}

func classNames(cands []classHandle) []string {
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.class)
	}
	return names
}

// isNilStore reports whether a handle is unnamed. A typed nil pointer in a
// beads.Store interface is a NON-nil interface value that panics on first use,
// so the nil check has to look through the interface.
func isNilStore(s beads.Store) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// sameHandle reports whether two named handles are the same physical store.
// Interface comparison panics when both hold the same non-comparable dynamic
// type, so comparability is checked first; two handles that cannot be compared
// are treated as distinct, which is the conservative answer (they are probed
// separately and a genuine double-residence is reported rather than hidden).
func sameHandle(a, b beads.Store) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() || !av.Comparable() || !bv.Comparable() {
		return false
	}
	return a == b
}
