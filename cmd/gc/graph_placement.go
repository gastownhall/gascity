package main

// The one placement rule for graph-class beads (S4a, R-S4).
//
// On a city whose routes serve the graph class from a binding:
//
//  1. NEW ROOTS. Every graph bead that starts a tree — a workflow root, a
//     convergence root, a graft or sub-DAG root — is created in the binding,
//     at EVERY scope. The scope is recorded on the bead as
//     gc.root_store_ref (city:<name> or rig:<name>), never by choosing a
//     different database.
//  2. TREE EXTENSIONS. A bead that extends an existing tree is created in the
//     store that holds the tree's root, found by residence: the binding first,
//     then the scope store while that scope may still hold legacy graph rows.
//  3. NO CROSS-STORE EDGE. Because a tree is co-resident by (1) and (2), an
//     edge inside one tree never crosses stores. A writer that would need one
//     refuses instead.
//
// On a city that relocates nothing, both clauses answer the scope store the
// caller handed in, the same instance, which is exactly the behavior before
// this rule existed.
//
// Before S4a the rule was spread across scopeIsCity, scopeGraphStore,
// controlScopeTakesGraphClass and the convergence scope map, and it gave a rig
// scope its own WORK store for graph rows. That was the source of every
// rig-store graph write on a split city: rig convergence, `cook --attach` in a
// rig, and control dispatch over the trees those two created. Typed-stores T2
// moves graphPlacementForScope into storeref.Stores.Graph(scope) unchanged.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// graphScope names the scope a graph tree belongs to. The zero value is the
// city scope; a non-empty rig names a rig scope.
type graphScope struct {
	rig string
}

// cityGraphScope is the city/HQ scope.
func cityGraphScope() graphScope { return graphScope{} }

// rigGraphScope is the scope of the named rig.
func rigGraphScope(rig string) graphScope { return graphScope{rig: strings.TrimSpace(rig)} }

// isRig reports whether the scope is a rig scope.
func (s graphScope) isRig() bool { return s.rig != "" }

// rootStoreRef is the gc.root_store_ref value that records this scope on a
// graph bead: city:<cityName> or rig:<rig>. It is SCOPE, never location.
func (s graphScope) rootStoreRef(cityName string) string {
	if s.isRig() {
		return "rig:" + s.rig
	}
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		cityName = "city"
	}
	return "city:" + cityName
}

// graphPlacementForScope is R-S4.1: the store a NEW graph root for scope is
// created in. When the routes relocate the graph class that is the binding, at
// every scope; otherwise it is scopeStore itself, the same instance, so
// optional-capability assertions callers make against it keep working.
//
// It goes through resolveGraphStore so the worker-site refusal applies exactly
// as it does to every other graph-class write. scope is not consulted for the
// store choice — that is the point of the rule — but it is part of the
// signature so T2 can move this to Stores.Graph(scope) without changing a
// caller.
func graphPlacementForScope(routes *storageRoutes, cfg *config.City, cityPath string, scope graphScope, scopeStore beads.Store) beads.Store {
	_ = scope // deliberately unused: the rule is scope-independent; kept for Stores.Graph(scope) (T2)
	return resolveGraphStore(routes, scopeStore, cfg, cityPath, nil)
}

// graphRelicLegForScope reports whether readers of scope's graph trees must
// consult scopeStore AS WELL AS the placement store, behind it.
//
// Only a rig scope on a split city can have one: before S4a its convergence
// loops, rig `cook --attach` grafts and their control trees were written to
// the rig's own store, and those legacy trees stay there until the rig is
// imported. The leg retires once the rig is proven relic-free
// (storageRoutes.rigGraphRelicFree), which is a file read.
//
// A city scope never has one. Its retained copies in the work ledger are the
// rows `gc storage migrate` already copied into the binding; reading them
// again would re-offer ids the binding has finished.
//
// A refusing binding gives no leg either: the scope store is then the primary,
// see controlGraphBinding.
func graphRelicLegForScope(routes *storageRoutes, scope graphScope) bool {
	if !scope.isRig() {
		return false
	}
	binding, relocated := graphClassBinding(routes) // residency:allow — S4 placement rule: asks only whether the graph class is relocated (and refused); resolves no bead
	if !relocated || binding == nil {
		return false
	}
	if _, refused := binding.(refusedClassStore); refused {
		return false
	}
	return !routes.rigGraphRelicFree(scope.rig)
}

// splitRigGraphRelicFree reports whether a reader may skip rig's own store as
// a graph leg: only on a city serving the graph class from a live (not
// refusing) binding, and only once the rig is proven relic-free. Every other
// case — a single-store city, a refused city, a rig awaiting import — answers
// false and keeps the rig leg.
func splitRigGraphRelicFree(routes *storageRoutes, rig string) bool {
	binding, relocated := graphClassBinding(routes) // residency:allow — S4 placement rule: asks only whether the graph class is relocated (and refused); resolves no bead
	if !relocated || binding == nil {
		return false
	}
	if _, refused := binding.(refusedClassStore); refused {
		return false
	}
	return routes.rigGraphRelicFree(rig)
}

// graphResidenceForTree is R-S4.2: it reads id from the store that holds it,
// for a reader of id or a write that extends id's tree. placement is asked
// first; relic, when non-nil (graphRelicLegForScope), answers only a clean
// miss there. It returns the bead and the store that holds it.
//
// accept, when non-nil, is the caller's scope check on a row the placement
// store returned: the binding is shared by every scope, so a row of another
// scope is NOT this scope's and reads as not found (errGraphOutOfScope, which
// also matches beads.ErrNotFound). That row is still the one the binding holds
// for id, so the relic leg is not asked for a twin of it: an id the binding
// holds resolves to the binding's row, as it does for control dispatch. A
// relic store is the rig's own, so its rows need no check.
//
// An error is a read that failed, never absence, on either leg: a placement
// read that failed for any other reason is returned as is, and the relic leg
// is not asked; a relic read that failed is returned without the placement's
// not-found. Only a miss on both legs joins both errors, so errors.Is(err,
// beads.ErrNotFound) still holds for it.
func graphResidenceForTree(placement, relic beads.Store, id string, accept func(beads.Bead) bool) (beads.Bead, beads.Store, error) {
	b, err := placement.Get(id)
	if err == nil && accept != nil && !accept(b) {
		return beads.Bead{}, placement, fmt.Errorf("%w: %s %w", beads.ErrNotFound, id, errGraphOutOfScope)
	}
	if err == nil || relic == nil || relic == placement || !errors.Is(err, beads.ErrNotFound) {
		return b, placement, err
	}
	// Relic leg (S4 debt): a rig ledger holds graph rows only until the rig
	// is imported (gc storage migrate --from-rigs); the leg retires with it.
	rb, rerr := beads.RelicGet(relic, id)
	switch {
	case rerr == nil:
		return rb, relic, nil
	case errors.Is(rerr, beads.ErrNotFound):
		return beads.Bead{}, nil, errors.Join(err, rerr)
	default:
		return beads.Bead{}, nil, fmt.Errorf("reading %s from the scope's own store (its relic leg): %w", id, rerr)
	}
}

// errGraphOutOfScope marks a row the shared binding holds for ANOTHER scope.
var errGraphOutOfScope = errors.New("belongs to another scope")

// rigNameForScopeDir returns the name of the rig whose root is storePath.
func rigNameForScopeDir(cityPath, storePath string, rigs []config.Rig) (string, bool) {
	scopeRoot := resolveStoreScopeRoot(cityPath, storePath)
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if samePath(resolveStoreScopeRoot(cityPath, rig.Path), scopeRoot) {
			return rig.Name, true
		}
	}
	return "", false
}

// rigGraphMarkers is what storageRoutes knows about the per-rig import
// records: where they live and which directory is which rig.
type rigGraphMarkers struct {
	// bindingRoot is the canonical binding root, the directory holding the
	// city's infra.migrated marker and, beside it, the per-rig records.
	bindingRoot string
	// cityPath is the city root, whose .gc holds the per-rig cleared notes.
	cityPath string
	// rigs are the city's configured rigs, for mapping a scope directory to
	// its rig name.
	rigs []config.Rig
}

// rigGraphImportMarkerName is the per-rig import marker S4b writes beside the
// city's infra.migrated once a rig's graph rows are proven copied.
func rigGraphImportMarkerName(rig string) string { return "infra.rig." + rig + ".migrated" }

// rigGraphInFlightName is the record an import writes before it touches the
// binding and removes after its marker (S4b): while it exists the rig is
// never relic-free, and boot refuses.
func rigGraphInFlightName(rig string) string { return "infra.rig." + rig + ".importing" }

// rigGraphCleanMarkerName is the per-rig record S4b writes when a census found
// no infrastructure row in the rig's store at all (and `gc rig add` writes for
// a new, empty rig store on a split city).
func rigGraphCleanMarkerName(rig string) string { return "infra.rig." + rig + ".clean" }

// rigGraphClearedNotePath is the per-rig cleared note: Complete once the rig's
// imported copies have been removed from its store.
func rigGraphClearedNotePath(cityPath, rig string) string {
	return filepath.Join(cityPath, ".gc", "storage-infra-cleared.rig."+rig+".json")
}

// rigGraphRelicFree reports whether rig's own store is PROVEN to hold no
// graph row any more, so readers may stop consulting it as a relic leg:
//
//   - its clean record exists (a census found no infrastructure row); or
//   - its import marker exists AND its cleared note says the clear completed;
//
// and the record names the store the rig resolves to now, and no import of
// it is in flight.
//
// Everything else — no records, a marker with a pending or unreadable note, a
// routes value that does not know where the records live, a rig name that
// cannot be a file name — answers false, which keeps the relic leg. That is
// the pessimistic default ClassBinding.HasLegacyResidents uses: retiring a leg
// is the claim that needs evidence.
func (r *storageRoutes) rigGraphRelicFree(rig string) bool {
	if r == nil || r.rigMarkers == nil {
		return false
	}
	m := r.rigMarkers
	if m.bindingRoot == "" || !rigNameIsFileSafe(rig) {
		return false
	}
	if m.cityPath == "" {
		return false
	}
	// A record speaks only for the store it names (S4b): a new store under a
	// reused rig name keeps its relic legs until it is censused itself.
	storeRoot := ""
	for _, r := range m.rigs {
		if r.Name == rig && strings.TrimSpace(r.Path) != "" {
			storeRoot = resolveStoreScopeRoot(m.cityPath, r.Path)
		}
	}
	if storeRoot == "" {
		return false
	}
	if fileExists(filepath.Join(m.bindingRoot, rigGraphInFlightName(rig))) {
		return false
	}
	if rigRecordSpeaksFor(filepath.Join(m.bindingRoot, rigGraphCleanMarkerName(rig)), storeRoot) {
		return true
	}
	if !rigRecordSpeaksFor(filepath.Join(m.bindingRoot, rigGraphImportMarkerName(rig)), storeRoot) {
		return false
	}
	note, found, err := readInfraClearedNoteAt(rigGraphClearedNotePath(m.cityPath, rig))
	return err == nil && found && note.Complete
}

// rigGraphScopeForDir maps a scope directory to its graph scope using the rigs
// the routes were opened with. A directory the routes cannot name is treated
// as an unknown rig scope, which keeps every relic leg (never relic-free).
func (r *storageRoutes) rigGraphScopeForDir(cityPath, storePath string) graphScope {
	if scopeIsCity(cityPath, storePath) {
		return cityGraphScope()
	}
	if r != nil && r.rigMarkers != nil {
		if rig, ok := rigNameForScopeDir(cityPath, storePath, r.rigMarkers.rigs); ok {
			return rigGraphScope(rig)
		}
	}
	// An unnamed rig directory: a rig scope whose relic state is unknown.
	return graphScope{rig: unknownRigScope}
}

// unknownRigScope names a rig scope whose directory no configured rig owns.
// It cannot be a real rig name (rigNameIsFileSafe rejects it), so it is never
// relic-free and never matches a stamped gc.root_store_ref.
const unknownRigScope = "\x00unknown"

// rigNameIsFileSafe reports whether rig can be embedded in a record file name.
func rigNameIsFileSafe(rig string) bool {
	if rig == "" || rig == "." || rig == ".." {
		return false
	}
	return !strings.ContainsAny(rig, "/\\\x00")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
