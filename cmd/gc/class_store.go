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

// graphBeadStore returns the store that owns graph (workflow/v2) beads. It
// delegates to the exported GraphBeadStore() accessor so the api.State surface
// and the controller's own callers share one resolver. Identity to the work
// store at the default bd backend; returned as the strongly-typed
// beads.GraphStore so the graph class stays statically visible.
func (cs *controllerState) graphBeadStore() beads.GraphStore {
	return cs.GraphBeadStore()
}

// sessionsBeadStore returns the store that owns session and session-wait beads.
// It delegates to the exported SessionsBeadStore() accessor so the api.State
// surface and the controller's own callers share one resolver. Identity to the
// work store at the default bd backend; returned as the strongly-typed
// beads.SessionStore so the session class stays statically visible.
func (cs *controllerState) sessionsBeadStore() beads.SessionStore {
	return cs.SessionsBeadStore()
}

// mailBeadStore returns the store that owns mail (message) beads: the configured
// messaging class store when [beads.classes.messaging] relocates messaging, else
// the work store. Identity to the work store at the default bd backend; returned
// as the strongly-typed beads.MailStore so the messaging class stays statically
// visible.
func (cs *controllerState) mailBeadStore() beads.MailStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.MailStore{Store: resolveMailMessagesStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// nudgesBeadStore returns the store that owns nudge beads. It delegates to the
// exported NudgesBeadStore() accessor so the api.State surface and the
// controller's own callers share one resolver. Identity to the work store at the
// default bd backend; returned as the strongly-typed beads.NudgesStore so the
// nudges class stays statically visible.
func (cs *controllerState) nudgesBeadStore() beads.NudgesStore {
	return cs.NudgesBeadStore()
}

// ordersBeadStore returns the store that owns order-tracking bookkeeping beads
// for the given scope (rig name, or "" for the city): the configured orders class
// store when [beads.classes.orders] relocates orders, else the work store. The
// scope is accepted so a future per-scope orders backend can route without a
// call-site change. Identity to the work store at the default bd backend;
// returned as the strongly-typed beads.OrdersStore so the orders class stays
// statically visible. This is the city-scope simple case; per-order scope
// (rig/pool-routed orders) resolves PER ORDER through resolveOrderStoreTarget
// (the federated dispatch/sweep paths in order_store.go / order_dispatch.go).
func (cs *controllerState) ordersBeadStore(_ string) beads.OrdersStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.OrdersStore{Store: resolveOrderStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// cityWorkStore returns the city-level store for ordinary WORK-class beads that
// are not scoped to a named rig. Work is the default/residual coordination class
// (everything Classify does not route elsewhere), so this is the typed accessor
// for the work class — distinct from CityBeadStore(), which stays beads.Store as
// the federation/by-id/default root. Returned as the strongly-typed
// beads.WorkStore so the work class stays statically visible; the wrapper carries
// the exact same underlying store value CityBeadStore() returns today, so it is
// byte-identical. Pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cs *controllerState) cityWorkStore() beads.WorkStore {
	return beads.WorkStore{Store: cs.CityBeadStore()}
}

// workBeadStores returns all rig WORK-class stores keyed by rig name, including
// the HQ city store, as strongly-typed beads.WorkStore values. Each wrapper
// carries the exact same underlying store value BeadStores() returns today, so it
// is byte-identical; pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cs *controllerState) workBeadStores() map[string]beads.WorkStore {
	return toWorkStores(cs.BeadStores())
}

// graphBeadStore returns the runtime's graph (workflow/v2) bead store: the
// dedicated graph store when [beads.classes.graph] relocates graph, else the
// work store. Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.GraphStore so the graph class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) graphBeadStore() beads.GraphStore {
	return beads.GraphStore{Store: resolveGraphStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// sessionsBeadStore returns the runtime's session/session-wait bead store: the
// configured session class store (with the controller recorder so relocated
// session writes emit bead.*) when [beads.classes.sessions] relocates sessions,
// else the work store. Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.SessionStore so the session class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) sessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{Store: resolveSessionStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// mailBeadStore returns the runtime's mail (message) bead store: the configured
// messaging class store when [beads.classes.messaging] relocates messaging, else
// the work store. Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.MailStore so the messaging class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) mailBeadStore() beads.MailStore {
	return beads.MailStore{Store: resolveMailMessagesStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// nudgesBeadStore returns the runtime's nudge bead store: the configured nudges
// class store when [beads.classes.nudges] relocates nudges, else the work store.
// Byte-identical to cityBeadStore() at the default bd backend. Returned as the
// strongly-typed beads.NudgesStore so the nudges class stays statically visible;
// the wrapper carries the same underlying store value.
func (cr *CityRuntime) nudgesBeadStore() beads.NudgesStore {
	return beads.NudgesStore{Store: resolveNudgesStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// ordersBeadStore returns the runtime's order-tracking bead store for the given
// scope: the configured orders class store when [beads.classes.orders] relocates
// orders, else the work store. The scope is accepted for forward compatibility.
// Byte-identical to cityBeadStore() at the default bd backend; returned as the
// strongly-typed beads.OrdersStore so the orders class stays statically visible;
// the wrapper carries the same underlying store value. This is the city-scope
// simple case; per-order scope resolution flows through resolveOrderStoreTarget
// in the federated dispatch/sweep paths.
func (cr *CityRuntime) ordersBeadStore(_ string) beads.OrdersStore {
	return beads.OrdersStore{Store: resolveOrderStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// cityWorkStore returns the runtime's city-level WORK-class bead store. Work is
// the default/residual coordination class; this is its typed accessor, distinct
// from the federation/by-id/default cityBeadStore() root. Returned as the
// strongly-typed beads.WorkStore carrying the same underlying store value
// cityBeadStore() returns today, so it is byte-identical; pass the embedded
// .Store field to any generic beads.Store helper shared across classes.
func (cr *CityRuntime) cityWorkStore() beads.WorkStore {
	return beads.WorkStore{Store: cr.cityBeadStore()}
}

// workBeadStores returns the runtime's per-rig WORK-class stores keyed by rig
// name as strongly-typed beads.WorkStore values. Each wrapper carries the exact
// same underlying store value rigBeadStores() returns today, so it is
// byte-identical; pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cr *CityRuntime) workBeadStores() map[string]beads.WorkStore {
	return toWorkStores(cr.rigBeadStores())
}

// toWorkStores wraps each store in a rig→store map as a strongly-typed
// beads.WorkStore, carrying the same underlying store value so the result is
// byte-identical to the input map.
func toWorkStores(stores map[string]beads.Store) map[string]beads.WorkStore {
	if stores == nil {
		return nil
	}
	out := make(map[string]beads.WorkStore, len(stores))
	for name, store := range stores {
		out[name] = beads.WorkStore{Store: store}
	}
	return out
}

// unwrapWorkStores unwraps a rig→work-store map back to a generic
// rig→beads.Store map for passing into helpers shared across coordination
// classes. Each value carries the same underlying store, so the result is
// byte-identical.
func unwrapWorkStores(stores map[string]beads.WorkStore) map[string]beads.Store {
	if stores == nil {
		return nil
	}
	out := make(map[string]beads.Store, len(stores))
	for name, store := range stores {
		out[name] = store.Store
	}
	return out
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
