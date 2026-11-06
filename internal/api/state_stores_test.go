package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// checkStoresWrapTheAccessors fails for every class or rig whose Stores()
// handle does not wrap exactly the store the old accessor returns.
func checkStoresWrapTheAccessors(t *testing.T, name string, st accessorState) {
	t.Helper()
	s := st.Stores()
	for class, pair := range map[string][2]beads.Store{
		"work":     {s.Work().Store, st.CityBeadStore()},
		"graph":    {s.Graph().Store, st.GraphBeadStore().Store},
		"sessions": {s.Sessions().Store, st.SessionsBeadStore().Store},
		"orders":   {s.Orders().Store, st.OrdersBeadStore().Store},
		"nudges":   {s.Nudges().Store, st.NudgesBeadStore().Store},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: Stores().%s wraps %v, the accessor returns %v", name, class, pair[0], pair[1])
		}
	}
	if g, ok := s.GraphFor(storeref.WorkRef); !ok || g != s.Graph() {
		t.Errorf("%s: GraphFor(city) is not the city graph handle", name)
	}
	rigs := map[string]bool{}
	for _, r := range s.Rigs() {
		rigs[r.Name] = true
		if r.Work.Store != st.BeadStore(r.Name) {
			t.Errorf("%s: rig %s wraps %v, BeadStore returns %v", name, r.Name, r.Work.Store, st.BeadStore(r.Name))
		}
	}
	for rig, store := range st.BeadStores() {
		if rig == st.CityName() || store == nil {
			continue
		}
		if !rigs[rig] {
			t.Errorf("%s: BeadStores holds rig %s, Stores().Rigs() does not", name, rig)
		}
	}
}

// TestFakeStateStoresWrapTheAccessorsStores is State.Stores's equivalence
// table over the test fakes (typed-stores T6a): unsplit, with a city store,
// with each class relocated, with a class "relocated" to the city store itself,
// with rigs, and the two fakes that override a store accessor. It runs in both
// gate modes, so under GC_TEST_SPLIT_STRICT=1 every handle carries the gate's
// view exactly as its accessor does.
func TestFakeStateStoresWrapTheAccessorsStores(t *testing.T) {
	cases := map[string]func(*fakeState){
		"default":      func(*fakeState) {},
		"a city store": func(f *fakeState) { f.cityBeadStore = beads.NewMemStore() },
		"two rigs":     func(f *fakeState) { f.stores["other"] = beads.NewMemStore() },
		"a nil rig":    func(f *fakeState) { f.stores["unopened"] = nil },
		"graph split":  func(f *fakeState) { f.cityBeadStore, f.graphBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"orders split": func(f *fakeState) { f.cityBeadStore, f.ordersBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"nudges split": func(f *fakeState) { f.cityBeadStore, f.nudgesBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"sessions split": func(f *fakeState) {
			f.cityBeadStore, f.sessionsBeadStore = beads.NewMemStore(), beads.NewMemStore()
		},
		"whole split": func(f *fakeState) {
			binding := beads.NewMemStore()
			f.cityBeadStore = beads.NewMemStore()
			f.graphBeadStore, f.sessionsBeadStore, f.ordersBeadStore, f.nudgesBeadStore = binding, binding, binding, binding
		},
		"relocated to the city store": func(f *fakeState) {
			f.cityBeadStore = beads.NewMemStore()
			f.graphBeadStore = f.cityBeadStore
		},
	}
	for name, setup := range cases {
		f := newFakeState(t)
		setup(f)
		checkStoresWrapTheAccessors(t, name, f)
	}
	st, _, _ := splitClassState(t)
	checkStoresWrapTheAccessors(t, "sessions class unavailable", sessionClassUnavailableState{st})
	f := newFakeState(t)
	f.cityBeadStore = beads.NewMemStore()
	f.graphBeadStore = beads.NewMemStore()
	checkStoresWrapTheAccessors(t, "rebuilt over a new store", &rebuiltOverState{fakeState: f, store: beads.NewMemStore()})
}
