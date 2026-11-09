package api

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// apiAgreementIntents are the residency questions the API plane and a
// State's minted topology must answer alike, over a corpus of ids.
func apiAgreementIntents() map[string]storeref.Intent {
	out := map[string]storeref.Intent{
		"RoutedWork":          storeref.RoutedWork{},
		"AssignedWork(sweep)": storeref.AssignedWork{},
		"Session":             storeref.Session{},
		"Census(all)":         storeref.Census{},
		"Class(graph)":        storeref.Class{C: coordclass.ClassGraph},
		"Class(sessions)":     storeref.Class{C: coordclass.ClassSessions},
	}
	for _, id := range []string{"gcg-1", "gcnq-1-q", "tc-1", "mr-1", "zz-1"} {
		out["ByID("+id+")"] = storeref.ByID{ID: id}
	}
	return out
}

// assertAPITopologyAgrees fails when st's Stores().Topology() plans any
// agreement intent other than the API plane's residencyTopology over st does:
// refs, roles, policies and order.
func assertAPITopologyAgrees(t *testing.T, label string, st State) {
	t.Helper()
	minted, plane := st.Stores().Topology(), New(st).residencyTopology()
	render := func(i storeref.Intent, topo storeref.Topology) string {
		p, err := storeref.Plan(i, topo)
		if err != nil {
			return "error: " + err.Error()
		}
		return p.String()
	}
	for name, i := range apiAgreementIntents() {
		if got, want := render(i, minted), render(i, plane); got != want {
			t.Errorf("%s, %s:\n Stores().Topology()  %s\n residencyTopology()  %s", label, name, got, want)
		}
	}
	if got, want := fmt.Sprint(minted.ClaimRefs()), fmt.Sprint(plane.ClaimRefs()); got != want {
		t.Errorf("%s: claim refs %s, the API plane %s", label, got, want)
	}
	if got, want := fmt.Sprint(minted.PrefixFaults()), fmt.Sprint(plane.PrefixFaults()); got != want {
		t.Errorf("%s: prefix faults %s, the API plane %s", label, got, want)
	}
}

// fakeTopologyShapes are the fake States the agreement runs over: the default
// fake, a whole split before and after its census cleared it, a graph-only
// relocation, and a city with configured prefixes.
func fakeTopologyShapes(t *testing.T) map[string]*fakeState {
	withPrefixes := func(st *fakeState) *fakeState {
		st.cfg.Workspace.Prefix = "tc"
		st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "/tmp/myrig", Prefix: "mr"}}
		return st
	}
	wholeSplit := func(clean bool) *fakeState {
		st := withPrefixes(newFakeState(t))
		st.cityBeadStore = beads.NewMemStore()
		binding := mintingGraphStore(t)
		st.graphBeadStore, st.sessionsBeadStore, st.ordersBeadStore, st.nudgesBeadStore = binding, binding, binding, binding
		if clean {
			st.bindingRelics = map[beads.Store]bool{binding: false}
		}
		return st
	}
	graphOnly := withPrefixes(newFakeState(t))
	graphOnly.cityBeadStore = beads.NewMemStore()
	graphOnly.graphBeadStore = beads.NewMemStore()
	return map[string]*fakeState{
		"default":                     newFakeState(t),
		"prefixes":                    withPrefixes(newFakeState(t)),
		"whole split":                 wholeSplit(false),
		"whole split, censused clean": wholeSplit(true),
		"graph only":                  graphOnly,
	}
}

// TestFakeStateTopologyMatchesProduction: the fake State's minted Stores
// carry the same topology the API plane derives from the same State
// (residencyTopology), so a T7 conversion that reads Stores().Topology() sees
// what production would, in both gate modes. Under the gate a single-store
// fake's class handles read a class view, and both sides name it as the whole
// split's binding, class:gmnos.
func TestFakeStateTopologyMatchesProduction(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for name, st := range fakeTopologyShapes(t) {
				if gate && name == "graph only" {
					assertTheAPIBlindSpot(t, st)
					continue
				}
				assertAPITopologyAgrees(t, name, st)
			}
			topo := newFakeState(t).Stores().Topology()
			switch {
			case gate && (len(topo.Bindings) != 1 || topo.Bindings[0].Leg.Ref != "class:gmnos"):
				t.Errorf("a single-store fake under the gate: bindings %+v, want the class view as class:gmnos", topo.Bindings)
			case !gate && !topo.IsSingleStore():
				t.Errorf("a single-store fake off the gate has bindings: %+v", topo.Bindings)
			}
		})
	}
}

// assertTheAPIBlindSpot pins the one shape the two disagree on, and why: a
// graph-only relocation under the gate leaves the other four classes on the
// city store's class view, a binding of their own. The API plane observes no
// messaging store (classStoreAccessors), sees three of its four observable
// classes there and so cannot complete the set: it names the view class:nos,
// where the minted topology, which sees every class handle, names it
// class:mnos as the CLI and controller planes would. No serving city has this
// shape (only the whole split is served), and T7c retires the API's own
// derivation for Stores().Topology().
func assertTheAPIBlindSpot(t *testing.T, st State) {
	t.Helper()
	refs := func(topo storeref.Topology) string { return fmt.Sprint(topo.ClaimRefs()) }
	if got, want := refs(st.Stores().Topology()), "[ class:g class:mnos]"; got != want {
		t.Errorf("graph only under the gate: minted claim refs %s, want %s", got, want)
	}
	if got, want := refs(New(st).residencyTopology()), "[ class:g class:nos]"; got != want {
		t.Errorf("graph only under the gate: API plane claim refs %s, want %s", got, want)
	}
}

// TestStoresHoldsIsTheAPIHoldsRule: Stores.Holds, asked by the owner's ref,
// answers what the API's PATCH door asks of a store today (storeClassHolds),
// for the binding, the city store and a rig store, on a split and an unsplit
// city, in both gate modes. The graph-only shape under the gate is the API's
// messaging blind spot (assertTheAPIBlindSpot): there Holds counts messaging
// where the API rule cannot see it.
func TestStoresHoldsIsTheAPIHoldsRule(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for name, st := range fakeTopologyShapes(t) {
				if gate && name == "graph only" {
					continue
				}
				srv := New(st)
				topo := st.Stores().Topology()
				owners := map[storeref.StoreRef]beads.Store{storeref.WorkRef: topo.Work.Store}
				for _, rig := range topo.Rigs {
					owners[rig.Ref] = rig.Store
				}
				for _, b := range topo.Bindings {
					owners[b.Leg.Ref] = b.Leg.Store
				}
				for ref, store := range owners {
					got, want := st.Stores().Holds(ref), srv.storeClassHolds(store)
					slices.Sort(got)
					slices.Sort(want)
					if fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("%s, %q: Holds %v, the API rule %v", name, ref, got, want)
					}
				}
			}
		})
	}
}
