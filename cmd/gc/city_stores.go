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
// on the first read after any of them changed: the city work store, and the
// routes — which routes value, and the store it hands each class. The last is what makes an
// IN-PLACE rewrite of the routes visible, and the controller makes one:
// withControllerCache replaces each binding engine with its CachingStore under
// the same pointer. Keying on the pointer alone would keep handles over the
// uncached engine, whose reads bypass the cache and whose writes skip its
// event emission.

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
// and classes only, so no census is asked for.
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
	m.bindings, m.refused = storeref.BuildBindings(order, byStore, storeref.BindingOptions{})
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

// mintCityStores mints the class handles over work and routes.
func mintCityStores(cityPath string, routes *storageRoutes, work beads.Store) storeref.Stores {
	opts := storeref.StoresOptions{Projection: splitStrictSeam}
	for _, class := range mintClasses {
		if refusal := classRefusal(cityPath, routes, class); refusal != nil {
			if opts.ClassRefusals == nil {
				opts.ClassRefusals = make(map[coordclass.Class]storeref.RefusingClassStore, 1)
			}
			opts.ClassRefusals[class] = refusal
		}
	}
	return storeref.NewStores(cityTopology(routes, work), opts)
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
// frame every per-call class resolver has always taken. It mints per call; a
// holder with an epoch uses its Stores.
func scopeStores(cityPath string, routes *storageRoutes, scope beads.Store) storeref.Stores {
	return mintCityStores(cityPath, routes, scope)
}

// cliStores is scopeStores for a one-shot command, over the one-shot funnel's
// routes. The funnel memoizes the routes per city, and the routes memoize their
// bindings, so a command that reaches several class resolvers groups its
// bindings once; an unsettled funnel answer is re-read on its own schedule,
// and brings its own bindings with it.
func cliStores(cityPath string, scope beads.Store) storeref.Stores {
	return scopeStores(cityPath, cliStorageRoutes(cityPath), scope)
}

// storesEpoch is one minted Stores and the inputs it was minted from.
type storesEpoch struct {
	cityPath string
	bindings *routesBindings
	work     beads.Store
	stores   storeref.Stores
}

// storesEpochCache holds a holder's current epoch.
type storesEpochCache struct{ current atomic.Pointer[storesEpoch] }

// get returns the Stores over work and routes, minting it when the cached
// epoch was minted from a different city path, work store, or routes state.
// The bindings memo stands for the routes: it lives on one routes value and is
// replaced whenever that value hands a class a different store, so comparing
// it covers both the pointer and an in-place rewrite. A cache hit reads the
// routes and allocates nothing.
func (c *storesEpochCache) get(cityPath string, routes *storageRoutes, work beads.Store) storeref.Stores {
	bindings := routes.mintBindings()
	if e := c.current.Load(); e != nil && e.cityPath == cityPath && e.bindings == bindings && sameBeadStore(e.work, work) {
		return e.stores
	}
	e := &storesEpoch{cityPath: cityPath, bindings: bindings, work: work, stores: mintCityStores(cityPath, routes, work)}
	c.current.Store(e)
	return e.stores
}
