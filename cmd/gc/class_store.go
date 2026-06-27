package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
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

// createTarget returns the inner store that owns creates of the given
// coordination class for this policy-wrapped store. It is the create-side seam:
// the create chokepoint (Create / ApplyGraphPlan / the wisp-root lookup in
// policyForCreate) routes through it instead of reaching for the embedded store
// directly, so a future per-class split changes only this method. A
// beadPolicyStore wraps exactly one underlying store today, so every class
// collapses to that same embedded store and createTarget is identity — it
// returns the exact store the create chokepoint already used, preserving the
// StorageCreateStore / GraphApplyStore optional-capability assertions that the
// create path relies on.
func (s *beadPolicyStore) createTarget(_ coordclass.Class) beads.Store {
	return s.Store
}

// graphApplierFor returns the graph-apply capability that owns graph creates of
// the given coordination class for this graph-policy-wrapped store. It is the
// graph-create arm of the create-side seam: ApplyGraphPlan routes through it
// instead of reaching for the cached applier directly. A beadPolicyGraphStore
// wraps exactly one underlying applier today, so every class collapses to that
// cached instance — graphApplierFor returns the exact GraphApplyStore the apply
// path already used, preserving the StorageGraphApplyStore optional-capability
// assertion. A future per-class split derives the applier from
// createTarget(class) here.
func (s *beadPolicyGraphStore) graphApplierFor(_ coordclass.Class) beads.GraphApplyStore {
	return s.applier
}

// resolveClassStore returns the beads.Store backing a coordination class. It is
// the single dispatch point for per-class backend selection. Upstream Gas City
// is single-store: every coordination class collapses to the same Provider/Dolt
// work store, so this is the identity resolver today — it returns workStore
// unchanged for every class.
//
// The signature carries cfg, cityPath, class, and rec so the per-class /
// relocated backend dispatch (open the class's own embedded store when
// [beads.classes.<class>].backend selects one, emitting bead.* events via rec,
// falling back to the work store on miss) plugs in HERE as the documented
// fast-follow without a call-site change. Until then the parameters are accepted
// for forward-compatibility and ignored.
func resolveClassStore(workStore beads.Store, cfg *config.City, cityPath, class string, rec events.Recorder) beads.Store {
	_ = cfg
	_ = cityPath
	_ = class
	_ = rec
	return workStore
}

// resolveMailMessagesStore returns the message-persistence store for mail
// (messaging-class) beads. Identity today: the work store. When messaging
// relocates, this is the seam that diverges from session reads (which stay on
// the work store until sessions relocate); the divergence plugs in at
// resolveClassStore.
func resolveMailMessagesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassMessaging, rec)
}

// resolveOrderStore returns the order-tracking store. Identity today: the work
// store. When orders relocate, the embedded order store plugs in at
// resolveClassStore; returned as a beads.Store so the dispatch path can use it
// both as the order-tracking seam and, when distinct from the work store, as an
// extra gate-read store.
func resolveOrderStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassOrders, rec)
}

// resolveNudgesStore returns the nudge-shadow store. Identity today: the work
// store. When nudges relocate, the class store plugs in at resolveClassStore;
// returned as a beads.Store, which satisfies the nudge-store seam for free, so
// only the leaf nudge-bead operations route here.
func resolveNudgesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassNudges, rec)
}

// resolveSessionStore returns the session-lifecycle store. Identity today: the
// work store. Session-class beads are session lifecycle beads and durable
// session waits; only those bead ops route here. When sessions relocate, the
// class store plugs in at resolveClassStore.
func resolveSessionStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassSessions, rec)
}

// resolveGraphStore returns the beads.Store backing the GRAPH coordination
// class. Identity today: the work store. When graph relocates, the dedicated
// graph-store dispatch plugs in at resolveClassStore (graph uses its own legacy
// .gc/ location and is event-silent by design, so rec is accepted for signature
// parity with the other resolve*Store helpers and ignored here).
func resolveGraphStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassGraph, rec)
}

// newCityMailProvider builds the controller's mail provider over the work store.
// Identity today: it is byte-identical to newMailProvider — message persistence
// and session reads are both the work store, with no relocated class store and
// no recorder. When messaging relocates, resolveMailMessagesStore diverges and
// this is where the two-store mail provider plugs in.
func newCityMailProvider(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) mail.Provider {
	_ = resolveMailMessagesStore(workStore, cfg, cityPath, rec)
	return newMailProvider(workStore)
}
