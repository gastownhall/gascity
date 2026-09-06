package splittest

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// defaultWorkPrefix is the id-prefix segment beads.MemStore mints under by
// default ("gc-<n>", memstore.go Create) — the kit's stand-in for a rig/HQ work
// scope's EffectivePrefix. It is deliberately NOT a reserved class prefix
// (config.IsReservedClassPrefix("gc") is false), so a work store and a
// coordination-class store are prefix-disjoint the same way a real split city
// is: every relocated-class bead carries a reserved class prefix, no work bead
// does.
const defaultWorkPrefix = "gc"

// NewSplitStores returns a prefix-disjoint strict (work, graph) store pair for
// split-store tests in beads-level consumers (internal/convoy,
// internal/molecule, internal/dispatch, ...). It is the LEAF construction —
// no policy or class wrapping — so the kit stays importable from internal
// packages; callers that need the compile-time class seam wrap the leaves in
// beads.WorkStore / beads.GraphStore themselves.
//
// Store shapes:
//
//   - work mints "gc-<n>" on BdSemantics: a graph-prefixed create and a dep on
//     a graph bead are hard errors, the way bd fails them.
//   - graph mints under config.ReservedClassPrefix(config.BeadClassGraph)
//     ("gcg-<n>") on SQLiteSemantics: the same two writes are ACCEPTED, the way
//     the SQLite class database accepts them, and recorded as residence
//     violations that fail the test at cleanup unless the fixture claims them.
//     It honors explicit in-prefix ids, so production-shaped wisp ids
//     (gcg-wisp-*) round-trip, and exposes IDPrefix() == "gcg" for storeref
//     prefix routing.
//
// The asymmetry is the point: these two stores really do run on different
// backends, and a pair that answered the same way would misrepresent one of
// them. See the package doc's rule table.
//
// Graph is the pair's coordination class because it is the one the live
// incidents happened in; NewClassStore builds the same strict leaf for any
// other relocated class.
func NewSplitStores(t *testing.T) (work, graph beads.Store) {
	t.Helper()
	return NewWorkStore(t, defaultWorkPrefix), NewClassStore(t, config.BeadClassGraph)
}

// NewClassStore returns a strict store leaf for a relocated coordination class,
// keyed by the class itself (config.BeadClassGraph, BeadClassMessaging,
// BeadClassSessions, BeadClassOrders, BeadClassNudges) rather than by a
// hardcoded prefix. config.ReservedClassPrefix is the single source of truth for
// which prefix a class mints under, so a class that gains, loses, or changes its
// reserved prefix moves this kit with it.
//
// A class with no reserved prefix — config.BeadClassWork, whose beads stay on
// bd/Dolt under a rig/HQ EffectivePrefix — fails the test: its ids are
// indistinguishable from work ids, so there is no namespace for the residence
// invariant to be about. Use NewWorkStore for those.
//
// The leaf runs on SQLiteSemantics, because that is what serves a relocated
// class in production: internal/storebinding/sqlite/beads_engine.go OpenEngine
// opens beads.OpenSQLiteStore with the class's reserved prefix. A residence
// violation here is therefore ACCEPTED and recorded, not rejected — see the
// package doc's rule table and TakeResidenceViolations.
func NewClassStore(t *testing.T, class string) beads.Store {
	t.Helper()
	prefix, err := classPrefix(class)
	if err != nil {
		t.Fatalf("splittest.NewClassStore: %v", err)
	}
	return newStrictMemLeaf(t, prefix, SQLiteSemantics)
}

// classPrefix resolves a relocated coordination class to the id prefix its
// store mints under. Split out of NewClassStore so the rejection rule is
// testable without a failing *testing.T.
func classPrefix(class string) (string, error) {
	prefix, ok := config.ReservedClassPrefix(class)
	if !ok {
		return "", fmt.Errorf("bead class %q has no reserved id prefix; only relocated coordination classes have one — use NewWorkStore for work-class beads", class)
	}
	return prefix, nil
}

// NewWorkStore returns a strict work-store leaf minting under the given work
// prefix (a rig's or HQ's config.Rig.EffectivePrefix, e.g. "ra" for "rig-A"),
// for split-store tests that need the third store of a real city: an HQ work
// store, a coordination-class store, and per-rig work stores. It honors explicit
// in-prefix ids (bd accepts an in-prefix --id) and, on BdSemantics — the backend
// a work store really runs on — hard-fails foreign-prefix creates and cross-store
// deps.
//
// The prefix must be a genuine WORK prefix: not empty and not a reserved class
// prefix (a work store minting class-shaped ids would break the residence
// invariant the kit models). It may be defaultWorkPrefix only for the city's
// primary work store — passing it for a second store would alias the
// NewSplitStores work leaf's id space and defeat by-id prefix routing across
// the trio, so callers building a trio must pass distinct prefixes.
func NewWorkStore(t *testing.T, prefix string) beads.Store {
	t.Helper()
	p, err := workPrefix(prefix)
	if err != nil {
		t.Fatalf("splittest.NewWorkStore: %v", err)
	}
	return newStrictMemLeaf(t, p, BdSemantics)
}

// workPrefix normalizes and validates a work-store id prefix. Split out of
// NewWorkStore so the rejection rules are testable without a failing
// *testing.T.
func workPrefix(prefix string) (string, error) {
	p := normalizePrefix(prefix)
	if p == "" {
		return "", fmt.Errorf("empty work prefix %q; pass the rig's or HQ's EffectivePrefix", prefix)
	}
	if config.IsReservedClassPrefix(p) {
		return "", fmt.Errorf("prefix %q is a reserved class prefix; work stores hold work beads and must mint outside the reserved id space (use NewClassStore for a relocated class)", p)
	}
	return p, nil
}

// newStrictMemLeaf builds the kit's standard leaf: an in-memory store that mints
// under prefix and round-trips a pinned in-prefix id, wrapped in the strict
// checks for the given backend. beads.MemStore.IDPrefix supplies the minting
// half and HonorExplicitIDs the accepting half — together they are what makes a
// MemStore able to stand in for a real per-class database instead of one global
// id space.
func newStrictMemLeaf(t *testing.T, prefix string, semantics Semantics) beads.Store {
	t.Helper()
	leaf := beads.NewMemStore()
	leaf.IDPrefix = prefix
	leaf.HonorExplicitIDs = true
	return StrictWithPrefix(t, leaf, prefix, semantics)
}
