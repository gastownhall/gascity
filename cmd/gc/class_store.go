package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// This file is the controller/CLI-side seam of the per-class store refactor.
// It gives each coordination class a named accessor so a future per-class
// backend becomes a change here rather than at every call site. On a
// single-store city every class collapses to the same concrete store, so these
// are identity helpers today: each returns the exact wrapped+cached store the
// call site already uses, never a re-wrapped instance, so optional-capability
// type assertions (GraphApplyFor, HandlesFor, StorageCreateStore, Counter, ...)
// keep working.

// graphBeadStore returns the store that owns graph (workflow/v2) beads.
// Identity: the city-level bead store today.
func (cs *controllerState) graphBeadStore() beads.Store {
	return cs.CityBeadStore()
}

// sessionsBeadStore returns the store that owns session and session-wait beads.
// Identity: the city-level bead store today.
func (cs *controllerState) sessionsBeadStore() beads.Store {
	return cs.CityBeadStore()
}

// mailBeadStore returns the store that owns mail (message) beads.
// Identity: the city-level bead store today.
func (cs *controllerState) mailBeadStore() beads.Store {
	return cs.CityBeadStore()
}

// nudgesBeadStore returns the store that owns nudge beads.
// Identity: the city-level bead store today.
func (cs *controllerState) nudgesBeadStore() beads.Store {
	return cs.CityBeadStore()
}

// ordersBeadStore returns the store that owns order-tracking bookkeeping beads
// for the given scope (rig name, or "" for the city). Identity: the city-level
// bead store today; the scope is accepted so a future per-scope orders backend
// can route without a call-site change.
func (cs *controllerState) ordersBeadStore(_ string) beads.Store {
	return cs.CityBeadStore()
}

// cityWorkStore returns the city-level store for ordinary work beads that are
// not scoped to a named rig. Identity: the city-level bead store today.
func (cs *controllerState) cityWorkStore() beads.Store {
	return cs.CityBeadStore()
}

// workBeadStores returns all rig work stores keyed by rig name, including the
// HQ city store. Identity: the same map BeadStores() returns today.
func (cs *controllerState) workBeadStores() map[string]beads.Store {
	return cs.BeadStores()
}

// graphBeadStore returns the runtime's graph (workflow/v2) bead store.
// Identity: the city-level bead store today.
func (cr *CityRuntime) graphBeadStore() beads.Store {
	return cr.cityBeadStore()
}

// sessionsBeadStore returns the runtime's session/session-wait bead store.
// Identity: the city-level bead store today.
func (cr *CityRuntime) sessionsBeadStore() beads.Store {
	return cr.cityBeadStore()
}

// mailBeadStore returns the runtime's mail (message) bead store.
// Identity: the city-level bead store today.
func (cr *CityRuntime) mailBeadStore() beads.Store {
	return cr.cityBeadStore()
}

// nudgesBeadStore returns the runtime's nudge bead store.
// Identity: the city-level bead store today.
func (cr *CityRuntime) nudgesBeadStore() beads.Store {
	return cr.cityBeadStore()
}

// ordersBeadStore returns the runtime's order-tracking bead store for the given
// scope. Identity: the city-level bead store today; the scope is accepted for
// forward compatibility.
func (cr *CityRuntime) ordersBeadStore(_ string) beads.Store {
	return cr.cityBeadStore()
}

// cityWorkStore returns the runtime's city-level work bead store.
// Identity: the city-level bead store today.
func (cr *CityRuntime) cityWorkStore() beads.Store {
	return cr.cityBeadStore()
}

// workBeadStores returns the runtime's per-rig work stores keyed by rig name.
// Identity: the same map rigBeadStores() returns today.
func (cr *CityRuntime) workBeadStores() map[string]beads.Store {
	return cr.rigBeadStores()
}

// resolveClassStoreOpen is the seam resolveClassStore opens through. Tests swap
// it to avoid opening a real (native Dolt) store. It defaults to the CLI store
// funnel, so production behavior is unchanged.
var resolveClassStoreOpen = openStoreAtForCity

// resolveClassStore opens the store that owns beads of the given coordination
// class at the given scope, for the CLI funnel that opens a store on demand.
// Identity today: it delegates to the store funnel for every class and ignores
// the class entirely, so the returned store is the exact wrapped+cached value
// the CLI already opens. The class is accepted so a future per-class backend
// can route the open without a call-site change.
func resolveClassStore(_ coordclass.Class, storePath, cityPath string) (beads.Store, error) {
	return resolveClassStoreOpen(storePath, cityPath)
}
