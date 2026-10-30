package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// legacyResolveClassStore is resolveClassStore as it stood before the router
// (origin/main adf99aa6c8), kept verbatim as the oracle the router must agree
// with: the worker-site refusal, else the routes' store, else the scope store
// through the split-strict class view.
func legacyResolveClassStore(routes *storageRoutes, workStore beads.Store, cityPath, class string) beads.Store {
	store, relocated := routes.storeFor(coordclassFor(class))
	binding := config.StorageWorkBinding
	if relocated {
		binding = routes.binding
	}
	if refusal := workerSiteClassRefusal(cityPath, class, binding); refusal != nil {
		return refusedClassStore{err: standingStorageRefusal{err: refusal}}
	}
	if relocated {
		return store
	}
	return splitStrictClassView(workStore)
}

// resolvedKey renders a resolved store for comparison: a refusal by its
// sentence (each resolution builds its own value), a tagged view by side and
// the store under it (the tagged seam builds a fresh view per call), and any
// other store by identity.
func resolvedKey(s beads.Store) any {
	switch v := s.(type) {
	case refusedClassStore:
		return "refused: " + v.err.Error()
	case *taggedView:
		return fmt.Sprintf("view(class=%v) over %p", v.class, v.Store)
	case nil:
		return "<nil>"
	}
	return s
}

// handleOf reads a class handle back as the store it wraps, for comparison.
func cityStoresClassHandle(s storeref.Stores, class string, scope storeref.StoreRef) beads.Store {
	switch coordclassFor(class) {
	case coordclass.ClassGraph:
		return s.Graph(scope).Store
	case coordclass.ClassOrders:
		return s.Orders(scope).Store
	case coordclass.ClassSessions:
		return s.Sessions().Store
	case coordclass.ClassMessaging:
		return s.Mail().Store
	case coordclass.ClassNudges:
		return s.Nudges().Store
	}
	return s.Work().Store
}

var routedClassNames = []string{
	config.BeadClassGraph, config.BeadClassMessaging, config.BeadClassSessions,
	config.BeadClassOrders, config.BeadClassNudges,
}

// TestCityStoresAgreeWithTheLegacyResolver is T2's equivalence table: for
// every routes shape (none, whole split, graph-only split, refused), plane
// (local, worker site attached to the work ledger only, unusable descriptor),
// projection (off, on), class, and scope store (city, rig, none), the router
// hands out exactly the store the pre-router resolver did, both as a handle
// and through resolveClassStore.
func TestCityStoresAgreeWithTheLegacyResolver(t *testing.T) {
	binding := beads.NewMemStore()
	routeShapes := map[string]func() *storageRoutes{
		"none": func() *storageRoutes { return nil },
		"whole-split": func() *storageRoutes {
			r := &storageRoutes{stores: map[coordclass.Class]beads.Store{}, binding: "infra"}
			for _, c := range infrastructureClasses() {
				r.stores[c] = binding
			}
			return r
		},
		"graph-only": func() *storageRoutes {
			return &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: binding}, binding: "infra"}
		},
		"refused": func() *storageRoutes {
			return refusingStorageRoutes("infra", errors.New("refused: run gc storage migrate"))
		},
	}
	planes := map[string]func(t *testing.T, cityDir string){
		"local":  func(*testing.T, string) {},
		"worker": func(t *testing.T, cityDir string) { writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil)) },
		"worker-unusable": func(t *testing.T, cityDir string) {
			writeMalformedDescriptor(t, cityDir)
		},
	}
	for planeName, plane := range planes {
		for _, projected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/projected=%v", planeName, projected), func(t *testing.T) {
				resetControlPlaneForTest(t)
				cityDir := newWorkerSiteCity(t)
				plane(t, cityDir)
				if projected {
					saved := splitStrictSeam
					splitStrictSeam = taggedSeam{}
					t.Cleanup(func() { splitStrictSeam = saved })
				}
				city, rig := beads.NewMemStore(), beads.NewMemStore()
				for shapeName, shape := range routeShapes {
					routes := shape()
					stores := mintCityStores(cityDir, routes, cityStoresTopology(routes,
						storeref.Leg{Store: city}, storeref.RigLegs(map[string]beads.Store{"r1": rig})))
					unscoped := mintCityStores(cityDir, routes, cityStoresTopology(routes, storeref.Leg{}, nil))
					for _, class := range routedClassNames {
						scopes := []struct {
							name  string
							store beads.Store
							got   beads.Store
						}{
							{"city", city, cityStoresClassHandle(stores, class, storeref.WorkRef)},
							{"none", nil, cityStoresClassHandle(unscoped, class, storeref.WorkRef)},
						}
						if c := coordclassFor(class); c == coordclass.ClassGraph || c == coordclass.ClassOrders {
							scopes = append(scopes, struct {
								name  string
								store beads.Store
								got   beads.Store
							}{"rig", rig, cityStoresClassHandle(stores, class, storeref.RigRef("r1"))})
						}
						for _, sc := range scopes {
							want := resolvedKey(legacyResolveClassStore(routes, sc.store, cityDir, class))
							if got := resolvedKey(sc.got); got != want {
								t.Errorf("%s %s at %s: handle wraps %v, legacy resolver %v", shapeName, class, sc.name, got, want)
							}
							if got := resolvedKey(resolveClassStore(routes, sc.store, nil, cityDir, class, nil)); got != want {
								t.Errorf("%s %s at %s: resolveClassStore %v, legacy resolver %v", shapeName, class, sc.name, got, want)
							}
						}
					}
					if got, want := resolvedKey(stores.Work().Store), resolvedKey(splitStrictWorkView(city)); got != want {
						t.Errorf("%s: work handle %v, want the city store's work view %v", shapeName, got, want)
					}
					if h, ok := stores.Rig("r1"); !ok || resolvedKey(h.Store) != resolvedKey(splitStrictWorkView(rig)) {
						t.Errorf("%s: rig handle %v (held %v), want the rig store's work view", shapeName, h.Store, ok)
					}
				}
			})
		}
	}
}

// TestControllerStoresFollowTheSwappedStores pins the epoch: a reload that
// swaps the city or a rig store is visible to the next accessor read, and an
// unchanged state hands back the same minted Stores. The expected stores go
// through the split-strict views, which are identity outside the gate.
func TestControllerStoresFollowTheSwappedStores(t *testing.T) {
	first, rig := beads.NewMemStore(), beads.NewMemStore()
	cs := &controllerState{cityName: "c", cityBeadStore: first, beadStores: map[string]beads.Store{"r1": rig}}
	if got := cs.sessionsBeadStore().Store; got != splitStrictClassView(first) {
		t.Fatalf("sessions = %p, want the city store %p", got, first)
	}
	epoch := cs.stores.current.Load()
	_ = cs.Stores()
	if cs.stores.current.Load() != epoch {
		t.Error("an unchanged state re-minted its Stores")
	}

	second := beads.NewMemStore()
	cs.mu.Lock()
	cs.cityBeadStore = second
	cs.mu.Unlock()
	if got := cs.GraphBeadStore().Store; got != splitStrictClassView(second) {
		t.Errorf("graph after the swap = %p, want the new city store %p", got, second)
	}

	rig2 := beads.NewMemStore()
	cs.mu.Lock()
	cs.beadStores["r1"] = rig2
	cs.mu.Unlock()
	if h, ok := cs.Stores().Rig("r1"); !ok || h.Store != splitStrictWorkView(rig2) {
		t.Errorf("rig after an in-place swap = %v, want the new rig store", h.Store)
	}
}

// TestCityRuntimeStoresUseTheStateWhenItHasOne pins that a runtime with a
// controllerState mints over the state's stores, not its standalone ones.
func TestCityRuntimeStoresUseTheStateWhenItHasOne(t *testing.T) {
	stateCity, standalone := beads.NewMemStore(), beads.NewMemStore()
	cr := &CityRuntime{
		cityName:            "c",
		cs:                  &controllerState{cityName: "c", cityBeadStore: stateCity},
		standaloneCityStore: standalone,
	}
	if got := cr.nudgesBeadStore().Store; got != splitStrictClassView(stateCity) {
		t.Errorf("nudges = %p, want the state's city store %p", got, stateCity)
	}
	cr.cs = nil
	if got := cr.nudgesBeadStore().Store; got != splitStrictClassView(standalone) {
		t.Errorf("nudges without a state = %p, want the standalone store %p", got, standalone)
	}
}
