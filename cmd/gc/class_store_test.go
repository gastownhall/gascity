package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// controllerClassAccessor names a controllerState per-class accessor for the
// identity conformance table.
type controllerClassAccessor struct {
	name string
	got  func(cs *controllerState) beads.Store
}

var controllerCityClassAccessors = []controllerClassAccessor{
	// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"graphBeadStore", func(cs *controllerState) beads.Store { return cs.graphBeadStore().Store }},
	// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"sessionsBeadStore", func(cs *controllerState) beads.Store { return cs.sessionsBeadStore().Store }},
	// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"mailBeadStore", func(cs *controllerState) beads.Store { return cs.mailBeadStore().Store }},
	// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"nudgesBeadStore", func(cs *controllerState) beads.Store { return cs.nudgesBeadStore().Store }},
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
		// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"graphBeadStore", func() beads.Store { return cr.graphBeadStore().Store }},
		// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"sessionsBeadStore", func() beads.Store { return cr.sessionsBeadStore().Store }},
		// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"mailBeadStore", func() beads.Store { return cr.mailBeadStore().Store }},
		// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"nudgesBeadStore", func() beads.Store { return cr.nudgesBeadStore().Store }},
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

// sameStorePtr reports pointer identity between two stores.
func sameStorePtr(a, b beads.Store) bool {
	ka, oka := storePointerKey(a)
	kb, okb := storePointerKey(b)
	return oka && okb && ka == kb
}
