package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// controllerClassAccessor names a controllerState per-class accessor for the
// identity conformance table.
type controllerClassAccessor struct {
	name string
	got  func(cs *controllerState) beads.Store
}

var controllerCityClassAccessors = []controllerClassAccessor{
	{"graphBeadStore", func(cs *controllerState) beads.Store { return cs.graphBeadStore() }},
	{"sessionsBeadStore", func(cs *controllerState) beads.Store { return cs.sessionsBeadStore() }},
	{"mailBeadStore", func(cs *controllerState) beads.Store { return cs.mailBeadStore() }},
	{"nudgesBeadStore", func(cs *controllerState) beads.Store { return cs.nudgesBeadStore() }},
	{"ordersBeadStore", func(cs *controllerState) beads.Store { return cs.ordersBeadStore("") }},
	{"cityWorkStore", func(cs *controllerState) beads.Store { return cs.cityWorkStore() }},
}

// TestControllerStateClassAccessorsAreIdentity pins that every controllerState
// per-class accessor returns the exact same pointer the call site uses today:
// CityBeadStore() for the city-resident classes and BeadStores() for work.
func TestControllerStateClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	cs := &controllerState{
		cityName:      "test-city",
		cityBeadStore: city,
		beadStores:    map[string]beads.Store{"myrig": rig},
	}

	for _, acc := range controllerCityClassAccessors {
		if got := acc.got(cs); !sameStorePtr(got, city) {
			t.Errorf("controllerState.%s() = %p, want CityBeadStore %p", acc.name, got, city)
		}
	}

	work := cs.workBeadStores()
	want := cs.BeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		if !sameStorePtr(work[name], store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name], store)
		}
	}
}

// TestCityRuntimeClassAccessorsAreIdentity pins that every CityRuntime per-class
// accessor returns the same pointer the runtime call site uses today.
func TestCityRuntimeClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	cr := &CityRuntime{
		cityName:            "test-city",
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"myrig": beads.NewMemStore()},
	}

	accessors := []struct {
		name string
		got  func() beads.Store
	}{
		{"graphBeadStore", cr.graphBeadStore},
		{"sessionsBeadStore", cr.sessionsBeadStore},
		{"mailBeadStore", cr.mailBeadStore},
		{"nudgesBeadStore", cr.nudgesBeadStore},
		{"cityWorkStore", cr.cityWorkStore},
	}
	for _, acc := range accessors {
		if got := acc.got(); !sameStorePtr(got, city) {
			t.Errorf("CityRuntime.%s() = %p, want cityBeadStore %p", acc.name, got, city)
		}
	}
	if got := cr.ordersBeadStore("myrig"); !sameStorePtr(got, city) {
		t.Errorf("CityRuntime.ordersBeadStore() = %p, want cityBeadStore %p", got, city)
	}

	work := cr.workBeadStores()
	want := cr.rigBeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		if !sameStorePtr(work[name], store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name], store)
		}
	}
}

// TestResolveClassStoreIsClassAgnostic pins that resolveClassStore opens the
// store funnel with the same (storePath, cityPath) scope for every coordination
// class and returns the identical store the funnel produced — the class never
// changes the routing today. It swaps the open seam so no real (Dolt) store is
// opened.
func TestResolveClassStoreIsClassAgnostic(t *testing.T) {
	sentinel := beads.NewMemStore()
	var sawStorePath, sawCityPath string
	calls := 0
	orig := resolveClassStoreOpen
	t.Cleanup(func() { resolveClassStoreOpen = orig })
	resolveClassStoreOpen = func(storePath, cityPath string) (beads.Store, error) {
		calls++
		sawStorePath, sawCityPath = storePath, cityPath
		return sentinel, nil
	}

	classes := coordclass.Classes()
	for _, class := range classes {
		got, err := resolveClassStore(class, "/store", "/city")
		if err != nil {
			t.Fatalf("resolveClassStore(%s): %v", class, err)
		}
		if !sameStorePtr(got, sentinel) {
			t.Errorf("resolveClassStore(%s) = %p, want funnel store %p", class, got, sentinel)
		}
		if sawStorePath != "/store" || sawCityPath != "/city" {
			t.Errorf("resolveClassStore(%s) opened scope (%q, %q), want (/store, /city)", class, sawStorePath, sawCityPath)
		}
	}
	if calls != len(classes) {
		t.Errorf("open funnel called %d times, want %d (one per class)", calls, len(classes))
	}
}

// sameStorePtr reports pointer identity between two stores.
func sameStorePtr(a, b beads.Store) bool {
	ka, oka := storePointerKey(a)
	kb, okb := storePointerKey(b)
	return oka && okb && ka == kb
}
