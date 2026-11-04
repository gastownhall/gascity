package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TestControllerStoresRigScopesAreTheRawAccessorsStores is the router's rig
// identity table (typed-stores T6a): on a single-store city and on a whole
// split, and under the split-strict gate when the test binary runs with it,
// every opened rig's work handle wraps exactly the store BeadStore(rig)
// returns, and its graph handle wraps exactly the store the legacy per-scope
// resolver (resolveGraphStore over that rig store) answers. Rigs lists every
// opened rig by name, and a rig that did not open has no scope. The
// runtime's standalone epoch answers the same.
func TestControllerStoresRigScopesAreTheRawAccessorsStores(t *testing.T) {
	binding := beads.NewMemStore()
	for name, routes := range map[string]*storageRoutes{
		"single store": nil,
		"whole split":  messagingSplitRoutes(binding),
		"graph only":   {stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: binding}, binding: "infra"},
	} {
		cityPath := t.TempDir()
		alpha, bravo := beads.NewMemStore(), beads.NewMemStore()
		rigs := map[string]beads.Store{"bravo": bravo, "alpha": alpha, "unopened": nil}
		cs := &controllerState{cityName: "c", cityPath: cityPath, cityBeadStore: beads.NewMemStore(), storageRoutes: routes}
		cs.beadStores = rigs
		cs.rigStoresGen++
		cr := &CityRuntime{cityName: "c", cityPath: cityPath, storageRoutes: routes}
		cr.setStandaloneStores(beads.NewMemStore(), rigs)

		for holder, st := range map[string]storeref.Stores{"controller": cs.Stores(), "standalone runtime": cr.cityStores()} {
			var listed []string
			for _, r := range st.Rigs() {
				listed = append(listed, r.Name)
			}
			if got := fmt.Sprint(listed); got != "[alpha bravo]" {
				t.Errorf("%s, %s: Rigs() = %v, want [alpha bravo]", name, holder, listed)
			}
			for rig, raw := range map[string]beads.Store{"alpha": alpha, "bravo": bravo} {
				work, ok := st.Rig(rig)
				if !ok || work.Store != splitStrictWorkView(raw) {
					t.Errorf("%s, %s: Rig(%s) = %v, %v; want BeadStore's %v", name, holder, rig, work.Store, ok, splitStrictWorkView(raw))
				}
				if holder == "controller" && work.Store != cs.BeadStore(rig) {
					t.Errorf("%s: Rig(%s) wraps %v, BeadStore answers %v", name, rig, work.Store, cs.BeadStore(rig))
				}
				graph, ok := st.GraphFor(storeref.RigRef(rig))
				if want := resolveGraphStore(routes, raw, nil, cityPath, nil); !ok || graph.Store != want {
					t.Errorf("%s, %s: GraphFor(rig:%s) = %v, %v; want the legacy resolver's %v", name, holder, rig, graph.Store, ok, want)
				}
			}
			if _, ok := st.Rig("unopened"); ok {
				t.Errorf("%s, %s: a rig whose store did not open has a scope", name, holder)
			}
		}
	}
}

// TestControllerStoresNeverTearAcrossReloads pins R6 (typed-stores T6a): a
// Stores holds the city store and the rig stores of one reload. Each reload
// swaps both under cs.mu, the rig store tagged with its reload; readers check
// that the rig handle they got belongs to the city handle's reload. Reading
// the city store and the rig set under two locks (or none) lets a reader mint
// across a swap; under -race an unlocked read is also a reported race.
func TestControllerStoresNeverTearAcrossReloads(t *testing.T) {
	cs := &controllerState{cityName: "c", cityBeadStore: beads.NewMemStore()}
	cs.beadStores = map[string]beads.Store{"r": beads.NewMemStore()}
	cs.rigStoresGen++
	var pairs sync.Map // city work handle's store -> its reload's rig handle's store
	pair := func(city, rig beads.Store) { pairs.Store(splitStrictWorkView(city), splitStrictWorkView(rig)) }
	pair(cs.cityBeadStore, cs.beadStores["r"])
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var torn sync.Once
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st := cs.Stores()
				rig, _ := st.Rig("r")
				want, _ := pairs.Load(st.Work().Store)
				if rig.Store != want {
					torn.Do(func() { t.Errorf("a Stores minted a rig store from another reload than its city store") })
				}
			}
		}()
	}
	for range 2000 {
		city, rig := beads.NewMemStore(), beads.NewMemStore()
		pair(city, rig)
		cs.mu.Lock()
		cs.cityBeadStore = city
		cs.beadStores = map[string]beads.Store{"r": rig}
		cs.rigStoresGen++
		cs.mu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// TestCityRuntimeStandaloneStoresFollowARigSwap pins the standalone epoch's
// rig generation: a reload that swaps the runtime's rig stores and keeps its
// city store (setStandaloneStores, city_runtime.go reload) is visible to the
// next read.
func TestCityRuntimeStandaloneStoresFollowARigSwap(t *testing.T) {
	city, first, second := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	cr := &CityRuntime{cityName: "c", cityPath: t.TempDir()}
	cr.setStandaloneStores(city, map[string]beads.Store{"r": first})
	if got, ok := cr.cityStores().Rig("r"); !ok || got.Store != splitStrictWorkView(first) {
		t.Fatalf("rig r = %v, %v; want the first rig store", got.Store, ok)
	}
	cr.setStandaloneStores(city, map[string]beads.Store{"r": second})
	if got, ok := cr.cityStores().Rig("r"); !ok || got.Store != splitStrictWorkView(second) {
		t.Errorf("rig r after the rig swap = %v, %v; want the second rig store %p", got.Store, ok, second)
	}
}
