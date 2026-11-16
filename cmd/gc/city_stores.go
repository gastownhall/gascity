package main

// The router's cmd/gc side: where the controller, the standalone runtime and
// the one-shot CLI get their storeref.Stores (typed-stores design §3.2).
//
// Every class handle this package hands out is minted by storeref.NewStores
// from a topology built here, from the routes this process opened and the work
// store it holds, and every raw class store resolveClassStore hands out is
// storeref.Place over the same topology. The mapping is storeref's: the
// worker-site refusal, else the routes' binding, else the work store, seen
// through the split-strict gate's class view. This is the one cmd/gc file the
// storeclass lint lets call NewStores and Place.
//
// # One epoch, re-minted when its inputs change
//
// A holder keeps one Stores beside the inputs it was minted from and re-mints
// on the first read after any of them changed: the city work store, the rig
// set (by its generation), and the routes — which routes value, and the store
// it hands each class. The last is what makes an IN-PLACE rewrite of the
// routes visible, and the controller makes one: withControllerCache replaces
// each binding engine with its CachingStore under the same pointer. Keying on
// the pointer alone would keep handles over the uncached engine, whose reads
// bypass the cache and whose writes skip its event emission.
//
// A holder reads the city work store and the rig set under one lock, the lock
// it swaps them under, so one Stores never holds a city store from one reload
// and rig stores from another.

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// mintClasses is the class order the bindings memo records the routes in.
var mintClasses = infrastructureClasses()

// routesBindings is the class bindings derived from one state of a routes
// value: the store it handed each class, in mintClasses order, and the
// bindings and refusal grouped from them. The router reads a binding's store
// and classes; the bindings also carry the boot census's verdicts
// (storageRoutes.hasLegacyResidents, read below the controller's cache), so
// the epoch's Topology answers residency as the CLI and controller planes do.
// The census is taken once, at boot, before the routes are published, so a
// memo derived from them never sees it change.
type routesBindings struct {
	stores   []beads.Store
	bindings []storeref.ClassBinding
	refused  error
}

// noRoutesBindings is the answer for nil routes: nothing relocated.
var noRoutesBindings = &routesBindings{stores: make([]beads.Store, len(mintClasses))}

// current reports whether r still hands every class the store m was derived
// from. It reads the routes and allocates nothing.
func (m *routesBindings) current(r *storageRoutes) bool {
	for i, class := range mintClasses {
		store, _ := r.storeFor(class) // residency:allow — compares the store each class routes to with the memo's, to notice an in-place rewrite; resolves no bead
		if !sameBeadStore(store, m.stores[i]) {
			return false
		}
	}
	return true
}

// mintBindings returns the bindings for r's current class stores, derived once
// per state of the routes.
func (r *storageRoutes) mintBindings() *routesBindings {
	if r == nil {
		return noRoutesBindings
	}
	if m := r.mint.Load(); m != nil && m.current(r) {
		return m
	}
	m := &routesBindings{stores: make([]beads.Store, len(mintClasses))}
	order := make([]beads.Store, 0, 1)
	byStore := map[beads.Store][]coordclass.Class{}
	for i, class := range mintClasses {
		store, relocated := r.storeFor(class) // residency:allow — groups the routes' class stores into the bindings the router mints over; resolves no bead
		m.stores[i] = store
		if !relocated || store == nil {
			continue
		}
		if _, seen := byStore[store]; !seen {
			order = append(order, store)
		}
		byStore[store] = append(byStore[store], class)
	}
	m.bindings, m.refused = storeref.BuildBindings(order, byStore, storeref.BindingOptions{Relics: r.hasLegacyResidents})
	r.mint.Store(m)
	return m
}

// classRefusal is the worker-site refusal for class, judged against the name of
// the binding the routes resolve it to, as a refusing store; nil when the class
// is not refused.
func classRefusal(cityPath string, routes *storageRoutes, class coordclass.Class) storeref.RefusingClassStore {
	// This agrees with the funnel's Classes.BindingFor only because the one
	// served split shape, storageSplitWhole, relocates every class to the one
	// routes.binding; a per-class shape must name the class's own binding here.
	binding := config.StorageWorkBinding
	if _, relocated := routes.storeFor(class); relocated { // residency:allow — asks only whether the class is relocated, to name the binding the worker-site check compares; resolves no bead
		binding = routes.binding
	}
	refusal := workerSiteClassRefusal(cityPath, class.String(), binding)
	if refusal == nil {
		return nil
	}
	return refusedClassStore{err: standingStorageRefusal{err: refusal}}
}

// cityTopology is the topology the router reads over routes: the work store
// and the bindings.
func cityTopology(routes *storageRoutes, work beads.Store) storeref.Topology {
	b := routes.mintBindings()
	return storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: work}, Bindings: b.bindings, Refused: b.refused}
}

// mintCityStores mints the class handles over work, the rig legs and routes,
// and completes the topology they are minted from with what Stores.Topology
// answers: the configured prefixes from cfg (nil for a frame with none), the
// routes' census verdicts (mintBindings) and each rig's S4 relic-free bit
// (splitRigGraphRelicFree, the bit the API reads through RigGraphRelicFree:
// file reads, so taken once per epoch, and false unless the graph class is
// served from a live binding). Its rigs are every rig the holder opened,
// suspended ones included, as the API serves them; the residency topologies
// the controller's census arms read exclude suspended rigs themselves
// (residency_topology.go).
//
// suspended names the rigs the holder last saw suspended (nil: none known).
// Their legs are kept, marked Leg.Suspended, so the epoch still hands out
// their handles while no plan reads them.
func mintCityStores(cityPath string, routes *storageRoutes, cfg *config.City, work beads.Store, rigs []storeref.Leg, suspended map[string]bool) storeref.Stores {
	opts := storeref.StoresOptions{Projection: splitStrictSeam}
	for _, class := range mintClasses {
		if refusal := classRefusal(cityPath, routes, class); refusal != nil {
			if opts.ClassRefusals == nil {
				opts.ClassRefusals = make(map[coordclass.Class]storeref.RefusingClassStore, 1)
			}
			opts.ClassRefusals[class] = refusal
		}
	}
	t := cityTopology(routes, work)
	t.Work.Prefix = hqPrefixOf(cfg)
	prefixes := rigPrefixesOf(cfg)
	t.Rigs = make([]storeref.Leg, 0, len(rigs))
	for _, leg := range rigs {
		if name, ok := storeref.ScopeRigContext(string(leg.Ref)); ok && name != "" {
			leg.Prefix = prefixes[name]
			leg.RelicFree = splitRigGraphRelicFree(routes, name)
			leg.Suspended = suspended[name]
		}
		t.Rigs = append(t.Rigs, leg)
	}
	return storeref.NewStores(t, opts)
}

// placeClass is the store class lives in over work and routes, for the
// callers that still take a raw store (resolveClassStore). It judges the
// worker-site refusal for class alone.
func placeClass(cityPath string, routes *storageRoutes, work beads.Store, class coordclass.Class) beads.Store {
	opts := storeref.StoresOptions{Projection: splitStrictSeam}
	if class.IsInfrastructure() {
		if refusal := classRefusal(cityPath, routes, class); refusal != nil {
			opts.ClassRefusals = map[coordclass.Class]storeref.RefusingClassStore{class: refusal}
		}
	}
	return storeref.Place(cityTopology(routes, work), opts, class)
}

// scopeStores mints the handles for a caller holding one opened scope store and
// the routes: the scope store is the work store, so a relocated class answers
// from the binding and an unrelocated one from that scope store. It is the
// frame every per-call class resolver has always taken, with no rig scopes.
// It mints per call; a holder with an epoch uses its Stores.
//
// With nil routes it hands out handles of every class over scope, whatever
// scope holds, and the storeclass lint counts none of them: this file is the
// router's allowlisted mint. That is right for a scope store with no routes
// (an unsplit city) and for refusedExtMsgServices, whose store refuses every
// operation. It is not a relabel: a caller holding a store of one class that
// needs a handle of another takes it from the city's Stores, or relabels it
// with a counted literal at its own site.
func scopeStores(cityPath string, routes *storageRoutes, scope beads.Store) storeref.Stores {
	return mintCityStores(cityPath, routes, nil, scope, nil, nil)
}

// cliStores is scopeStores for a one-shot command, over the one-shot funnel's
// routes. The funnel memoizes the routes per city, and the routes memoize their
// bindings, so a command that reaches several class resolvers groups its
// bindings once; an unsettled funnel answer is re-read on its own schedule,
// and brings its own bindings with it.
func cliStores(cityPath string, scope beads.Store) storeref.Stores {
	return scopeStores(cityPath, cliStorageRoutes(cityPath), scope)
}

// rigSet is the rig work stores a holder opened, keyed by rig name, with the
// generation that names this map. The holder bumps the generation wherever it
// replaces the map and never writes into a published map in place, so equal
// generations are one rig set and the epoch can key on the number.
type rigSet struct {
	gen    uint64
	stores map[string]beads.Store
}

// storesEpoch is one minted Stores and the inputs it was minted from.
type storesEpoch struct {
	cityPath string
	bindings *routesBindings
	cfg      *config.City
	// suspended is the holder's published suspension set, by pointer: the
	// controller republishes it only when it changes (setSuspendedRigs).
	suspended *map[string]bool
	work      beads.Store
	rigGen    uint64
	stores    storeref.Stores
}

// storesEpochCache holds a holder's current epoch.
type storesEpochCache struct{ current atomic.Pointer[storesEpoch] }

// get returns the Stores over work, rigs, cfg and routes, minting it when the
// cached epoch was minted from a different city path, config, work store, rig
// set generation, or routes state. The bindings memo stands for the routes: it
// lives on one routes value and is replaced whenever that value hands a class
// a different store, so comparing it covers both the pointer and an in-place
// rewrite. The config is compared by pointer, since a reload publishes a new
// one: it carries the prefixes the topology is completed with; so is the
// suspension set, which marks the rig legs no plan reads. A cache hit
// reads the routes and allocates nothing. The caller reads every input under
// the one lock it swaps them under (R10), so the topology, its census bits and
// its rigs are one epoch's.
func (c *storesEpochCache) get(cityPath string, routes *storageRoutes, cfg *config.City, work beads.Store, rigs rigSet, suspended *map[string]bool) storeref.Stores {
	bindings := routes.mintBindings()
	if e := c.current.Load(); e != nil && e.cityPath == cityPath && e.bindings == bindings && e.cfg == cfg && e.suspended == suspended && sameBeadStore(e.work, work) && e.rigGen == rigs.gen {
		return e.stores
	}
	var names map[string]bool
	if suspended != nil {
		names = *suspended
	}
	e := &storesEpoch{cityPath: cityPath, bindings: bindings, cfg: cfg, suspended: suspended, work: work, rigGen: rigs.gen, stores: mintCityStores(cityPath, routes, cfg, work, storeref.RigLegs(rigs.stores), names)}
	c.current.Store(e)
	return e.stores
}
