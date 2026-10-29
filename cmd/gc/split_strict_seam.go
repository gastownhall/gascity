package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// splitStrictSeam is the split-strict test gate's hook into the city's store
// accessors. It is nil in every production binary, so the helpers below return
// their argument unchanged: only cmd/gc's test binary installs it, and only
// under GC_TEST_SPLIT_STRICT=1 (split_strict_gate_test.go).
//
// With it installed, a city whose routes relocate nothing still looks split to
// the code under test: work accessors (controllerState.CityBeadStore, BeadStore,
// BeadStores, and the CityRuntime's standalone stores) return a view of the one
// physical store that hides infrastructure rows, and resolveClassStore's
// identity branch returns a view that hides work rows. A read that reaches the
// wrong one is charged to its call site (internal/beads/splittest/projection.go).
var splitStrictSeam interface {
	workView(beads.Store) beads.Store
	classView(beads.Store) beads.Store
}

// splitStrictWorkView returns the gate's work view of store, or store.
func splitStrictWorkView(store beads.Store) beads.Store {
	if splitStrictSeam == nil || store == nil {
		return store
	}
	return splitStrictSeam.workView(store)
}

// splitStrictClassView returns the gate's class view of store, or store.
func splitStrictClassView(store beads.Store) beads.Store {
	if splitStrictSeam == nil || store == nil {
		return store
	}
	return splitStrictSeam.classView(store)
}

// splitStrictWorkViews applies splitStrictWorkView to every store of a map,
// returning the map itself when the gate is off.
//
// residency:allow — projects the caller's own snapshot store for store; it
// opens no store, resolves no bead and builds no leg list of its own.
func splitStrictWorkViews(stores map[string]beads.Store) map[string]beads.Store {
	if splitStrictSeam == nil || stores == nil {
		return stores
	}
	out := make(map[string]beads.Store, len(stores))
	for name, store := range stores {
		out[name] = splitStrictWorkView(store)
	}
	return out
}

// splitStrictEventStores maps the stores a bead event routed to onto the
// gate's views, for the reads that follow it (autoclose, the autoclose sweep,
// completion reconcile): an id in a reserved class namespace is the class
// binding's, as the routes would place it on a split city, and every other id
// the work side's. A relic id (a pre-split infrastructure row under the city
// or a rig prefix) therefore routes to the work view, and its read of a
// binding-resident row is charged to its call site, unless the residence
// probe proved the binding (relic) holds it: that store is a class store and
// gets the class view. The relic returned is the same view, so owner
// comparisons (sameBeadStore) still match. The cache application keeps the
// raw stores; this is identity in production.
//
// residency:allow — a test-gate projection of the caller's own routed stores (one view per store, same order); it plans and opens nothing and is identity in production.
func splitStrictEventStores(id string, stores []beads.Store, relic beads.Store) ([]beads.Store, beads.Store) {
	if splitStrictSeam == nil || len(stores) == 0 {
		return stores, relic
	}
	classID := false
	for _, class := range infraMigrationClasses {
		for _, prefix := range config.ReservedClassPrefixesFor(string(class)) { // residency:allow — classifies an id for the test gate; resolves no bead
			if strings.HasPrefix(id, prefix+"-") {
				classID = true
			}
		}
	}
	var relicView beads.Store
	if relic != nil {
		relicView = splitStrictClassView(relic)
	}
	out := make([]beads.Store, len(stores))
	for i, store := range stores {
		switch {
		case relic != nil && sameBeadStore(store, relic):
			out[i] = relicView
		case classID:
			out[i] = splitStrictClassView(store)
		default:
			out[i] = splitStrictWorkView(store)
		}
	}
	return out, relicView
}
