package main

// The router's cmd/gc side: where the controller, the standalone runtime and
// the one-shot CLI get their storeref.Stores (typed-stores design §3.2).
//
// Every class handle this package hands out is minted by storeref.NewStores
// from a topology built here, from the routes this process opened and the work
// stores it holds. The mapping is resolveClassStore's, moved into storeref:
// the worker-site refusal, else the routes' binding, else the work store the
// scope holds, seen through the split-strict gate's class view. The topology
// is assembled from the opened routes exactly as residency plans are
// (residency_topology.go), so a handle and a plan cannot disagree about where
// a class lives.
//
// # One epoch, re-minted when its inputs change
//
// A controller swaps its city and rig stores on reload, under its lock, and a
// Stores is a value over one set of them. Rather than re-mint at every place a
// store field is assigned, a holder keeps the Stores beside the inputs it was
// minted from and re-mints on the first read after any of them changed. A
// reader therefore always gets the handles of the stores the holder serves at
// that moment, as the per-call wrappers this replaced did, without paying for
// a mint per call.

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// mintCityStores mints the class handles over topo, whose bindings must be
// the ones residencyBindingsFromRoutes derives from routes; every caller here
// builds it with cityStoresTopology.
func mintCityStores(cityPath string, routes *storageRoutes, topo storeref.Topology) storeref.Stores {
	return storeref.NewStores(topo, cityStoresOptions(cityPath, routes))
}

// cityStoresOptions are this plane's own inputs to the mint: the worker-site
// refusal per class, judged against the binding name each class resolves to,
// and the split-strict projection.
func cityStoresOptions(cityPath string, routes *storageRoutes) storeref.StoresOptions {
	opts := storeref.StoresOptions{Projection: splitStrictSeam}
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		// This agrees with the funnel's Classes.BindingFor only because the
		// one served split shape, storageSplitWhole, relocates every class to
		// the one routes.binding; a per-class shape must name the class's own
		// binding here.
		binding := config.StorageWorkBinding
		if _, relocated := routes.storeFor(class); relocated { // residency:allow — asks only whether the class is relocated, to name the binding the worker-site check compares; resolves no bead
			binding = routes.binding
		}
		refusal := workerSiteClassRefusal(cityPath, class.String(), binding)
		if refusal == nil {
			continue
		}
		if opts.ClassRefusals == nil {
			opts.ClassRefusals = make(map[coordclass.Class]storeref.RefusingClassStore, 1)
		}
		opts.ClassRefusals[class] = refusedClassStore{err: standingStorageRefusal{err: refusal}}
	}
	return opts
}

// cityStoresTopology is the topology a Stores is minted over: the opened
// routes' bindings, the city work leg and the rig legs the holder serves.
//
// The rig set is every rig the holder holds open, a suspended one included,
// because that is the set its class accessors hand out. Plans are built over
// the serving frame instead (CityRuntime.residencyTopology), which is why a
// Stores exposes no plan topology.
func cityStoresTopology(routes *storageRoutes, work storeref.Leg, rigs []storeref.Leg) storeref.Topology {
	bindings, refused := residencyBindingsFromRoutes(routes)
	work.Ref = storeref.WorkRef
	return storeref.Topology{Work: work, Rigs: rigs, Bindings: bindings, Refused: refused}
}

// scopeStores mints the handles a caller holding one opened scope store and
// the routes gets: the scope store is the work leg, so a relocated class
// answers from the binding at every scope and an unrelocated one from that
// scope store. It is the frame every per-call class resolver has always
// taken, and it mints per call; a holder with an epoch uses its Stores.
func scopeStores(cityPath string, routes *storageRoutes, scope storeref.Leg) storeref.Stores {
	return mintCityStores(cityPath, routes, cityStoresTopology(routes, scope, nil))
}

// cliStores is scopeStores for a one-shot command: the routes come from the
// one-shot funnel, resolved per call so an unsettled answer is re-read on its
// own schedule, as it was.
func cliStores(cityPath string, scope storeref.Leg) storeref.Stores {
	return scopeStores(cityPath, cliStorageRoutes(cityPath), scope)
}

// storesEpoch is one minted Stores and the inputs it was minted from.
type storesEpoch struct {
	routes *storageRoutes
	work   storeref.Leg
	rigs   []storeref.Leg
	stores storeref.Stores
}

// storesEpochCache holds a holder's current epoch.
type storesEpochCache struct{ current atomic.Pointer[storesEpoch] }

// get returns the Stores for work and rigs over routes, minting it when the
// cached epoch was minted from different stores.
func (c *storesEpochCache) get(cityPath string, routes *storageRoutes, work storeref.Leg, rigs []storeref.Leg) storeref.Stores {
	if e := c.current.Load(); e != nil && e.routes == routes && sameStoresLegs(e.work, e.rigs, work, rigs) {
		return e.stores
	}
	e := &storesEpoch{routes: routes, work: work, rigs: rigs}
	e.stores = mintCityStores(cityPath, routes, cityStoresTopology(routes, work, rigs))
	c.current.Store(e)
	return e.stores
}

// sameStoresLegs reports whether two leg sets name the same stores. The
// bindings are derived from the routes, which the caller compares by pointer.
func sameStoresLegs(aWork storeref.Leg, aRigs []storeref.Leg, bWork storeref.Leg, bRigs []storeref.Leg) bool {
	if !sameBeadStore(aWork.Store, bWork.Store) || len(aRigs) != len(bRigs) {
		return false
	}
	for i := range aRigs {
		if aRigs[i].Ref != bRigs[i].Ref || !sameBeadStore(aRigs[i].Store, bRigs[i].Store) {
			return false
		}
	}
	return true
}
