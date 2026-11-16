package api

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// workAxisRefs is the by-id work axis's answer for id, as refs.
func workAxisRefs(s *Server, id string) []storeref.StoreRef {
	return apiWorkAxis{s: s, st: s.state.Stores()}.WorkLegsForID(id)
}

// TestUnroutedWorkAxisListsTheCityOnce is E4 (typed-stores T7 plan §4.6,
// R12): an id no configured prefix and no routes.jsonl entry claims is
// scanned over the city work store, then every rig in name order, each once.
// State.BeadStores() also lists the city store under the city's name, so a
// scan built from it named the city twice, the second time as a rig.
func TestUnroutedWorkAxisListsTheCityOnce(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore(), "zulu": beads.NewMemStore()}
	state.cfg.Rigs = []config.Rig{{Name: "zulu", Prefix: "zu"}, {Name: "alpha", Prefix: "al"}}
	s := New(state)

	got := fmt.Sprint(workAxisRefs(s, "unrouted-1"))
	want := fmt.Sprint([]storeref.StoreRef{storeref.WorkRef, storeref.RigRef("alpha"), storeref.RigRef("zulu")})
	if got != want {
		t.Fatalf("the unrouted work axis is %s, want %s: the city store once, then the rigs by name", got, want)
	}
}

// suspendedRigState is a fake whose epoch marks one rig suspended, as a
// production holder marks a rig it opened suspended (Leg.Suspended).
type suspendedRigState struct {
	*fakeState
	rig string
}

func (s suspendedRigState) Stores() storeref.Stores {
	topo := s.fakeState.Stores().Topology()
	topo.Rigs = append([]storeref.Leg(nil), topo.Rigs...)
	for i := range topo.Rigs {
		if topo.Rigs[i].Ref == storeref.RigRef(s.rig) {
			topo.Rigs[i].Suspended = true
		}
	}
	return storeref.NewStores(topo, storeref.StoresOptions{})
}

// workAxisShapes are the States the work-axis equivalence runs over.
func workAxisShapes(t *testing.T) map[string]State {
	withPrefixes := func(st *fakeState) *fakeState {
		st.cityBeadStore = beads.NewMemStore()
		st.cfg.Workspace.Prefix = "tc"
		st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "/tmp/myrig", Prefix: "mr"}, {Name: "other", Prefix: "ot"}}
		st.stores = map[string]beads.Store{"myrig": beads.NewMemStore(), "other": beads.NewMemStore()}
		return st
	}
	out := map[string]State{}
	for name, st := range fakeTopologyShapes(t) {
		out[name] = st
	}
	out["two rigs"] = withPrefixes(newFakeState(t))
	shared := withPrefixes(newFakeState(t))
	shared.stores["other"] = shared.stores["myrig"]
	out["rigs sharing a store"] = shared
	fileMode := withPrefixes(newFakeState(t))
	fileMode.stores["myrig"] = fileMode.cityBeadStore
	out["a rig over the city store"] = fileMode
	noCity := withPrefixes(newFakeState(t))
	noCity.cityBeadStore = nil
	out["no city store"] = noCity
	unloaded := withPrefixes(newFakeState(t))
	delete(unloaded.stores, "myrig")
	out["a configured rig not loaded"] = unloaded
	routed, _, _ := configureBeadRouteState(t)
	out["routes.jsonl"] = routed
	out["a suspended rig"] = suspendedRigState{fakeState: withPrefixes(newFakeState(t)), rig: "other"}
	nested := withPrefixes(newFakeState(t))
	// The longer prefix first in config order, so "the last covering prefix
	// wins" and "the longest wins" disagree.
	nested.cfg.Rigs = append([]config.Rig{{Name: "deep", Prefix: "mr-dp"}}, nested.cfg.Rigs...)
	nested.stores["deep"] = beads.NewMemStore()
	out["nested prefixes"] = nested
	return out
}

// TestWorkAxisMatchesTheFrozenRouter is the equivalence table for T7c's
// work axis (plan §2.3): over each shape, gate on and off, and ids on every
// arm (HQ prefix, rig prefix, routes.jsonl, unrouted), the ref-routed axis
// reads the stores the frozen raw-store router read, in its order and under
// its refs, once each store is read once (the plan's SameStore dedupe, which
// is where the frozen router's E4 duplicate went).
func TestWorkAxisMatchesTheFrozenRouter(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for name, st := range workAxisShapes(t) {
				s := New(st)
				stores := st.Stores()
				topo := stores.Topology()
				for _, id := range []string{"tc-1", "mr-1", "mr-dp-1", "ot-1", "ga-1", "gb-1", "gz-1", "zz-1", "unrouted"} {
					want := dedupeWorkLegs(referenceWorkAxis{s}.WorkLegs(id))
					var got []storeref.Leg
					for _, ref := range (apiWorkAxis{s: s, st: stores}).WorkLegsForID(id) {
						leg, ok := topologyWorkLeg(topo, ref)
						if !ok {
							t.Fatalf("%s, %s: the axis routed to %q, which the epoch holds no store for", name, id, ref)
						}
						got = append(got, leg)
					}
					got = dedupeWorkLegs(got)
					if renderWorkLegs(got) != renderWorkLegs(want) {
						t.Errorf("%s, %s: work axis %s, the frozen router %s", name, id, renderWorkLegs(got), renderWorkLegs(want))
						continue
					}
					for i := range want {
						if !beads.SameStore(got[i].Store, want[i].Store) {
							t.Errorf("%s, %s: leg %d (%q) reads %v, the frozen router %v", name, id, i, want[i].Ref, got[i].Store, want[i].Store)
						}
					}
				}
			}
		})
	}
}

// topologyWorkLeg is storeref's ref-to-leg mapping, restated: the work leg
// for WorkRef, else the rig leg of that ref, suspended or not.
func topologyWorkLeg(topo storeref.Topology, ref storeref.StoreRef) (storeref.Leg, bool) {
	if ref == storeref.WorkRef {
		return topo.Work, topo.Work.Store != nil
	}
	for _, leg := range topo.Rigs {
		if leg.Ref == ref && leg.Store != nil {
			return leg, true
		}
	}
	return storeref.Leg{}, false
}

// dedupeWorkLegs keeps the first leg over each store, as a plan does.
func dedupeWorkLegs(legs []storeref.Leg) []storeref.Leg {
	var out []storeref.Leg
	for _, leg := range legs {
		if leg.Store == nil || slices.ContainsFunc(out, func(have storeref.Leg) bool { return beads.SameStore(have.Store, leg.Store) }) {
			continue
		}
		out = append(out, leg)
	}
	return out
}

func renderWorkLegs(legs []storeref.Leg) string {
	refs := make([]string, len(legs))
	for i, leg := range legs {
		refs[i] = fmt.Sprintf("%q", leg.Ref)
	}
	return "[" + strings.Join(refs, " ") + "]"
}

// TestByIDReadsASuspendedRigItRoutesTo pins that the by-id door still reads a
// rig the holder reports suspended when the id routes there, as it did when
// it routed over BeadStores(): the route is the plane's, so the plan reads
// the rig's leg by its ref (storeref.WorkAxisRouter).
func TestByIDReadsASuspendedRigItRoutesTo(t *testing.T) {
	st := newFakeState(t)
	st.cityBeadStore = beads.NewMemStore()
	st.cfg.Rigs = []config.Rig{{Name: "other", Prefix: "ot"}}
	other := beads.NewMemStore()
	other.HonorExplicitIDs = true
	if _, err := other.Create(beads.Bead{ID: "ot-1", Title: "work"}); err != nil {
		t.Fatal(err)
	}
	st.stores = map[string]beads.Store{"other": other}
	suspended := suspendedRigState{fakeState: st, rig: "other"}
	if rigs := suspended.Stores().Topology().Rigs; len(rigs) != 1 || !rigs[0].Suspended {
		t.Fatalf("fixture: rigs %+v, want other marked suspended", rigs)
	}
	owner, b, err := New(suspended).resolveBeadOwner("ot-1")
	if err != nil || b.ID != "ot-1" || storeref.StoreRef(owner.Ref()) != storeref.RigRef("other") {
		t.Fatalf("by-id read of a suspended rig's bead = %q at %q, %v; want ot-1 at rig:other", b.ID, owner.Ref(), err)
	}
}

// TestGraphBindingPrefixesMatchTheFrozenRule: the ready leg's reserved
// namespaces, read from the graph binding of the minted topology, are the set
// the frozen rule read off the bindings over the graph store, on every
// agreement shape and in both gate modes.
func TestGraphBindingPrefixesMatchTheFrozenRule(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for name, st := range fakeTopologyShapes(t) {
				if gate && name == "graph only" {
					continue // the API's messaging blind spot (assertTheAPIBlindSpot)
				}
				var want []string
				if graph, ok := relocatedGraphStore(st); ok {
					want = referenceBindingReservedPrefixes(New(st), graph.Store)
				}
				// A set: readywork asks only whether an id is in any of them
				// (inAnyNamespace), and the frozen rule listed messaging's
				// namespace last because it rounded the class set up.
				got := graphBindingPrefixes(st.Stores())
				slices.Sort(got)
				slices.Sort(want)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: graph binding prefixes %v, the frozen rule %v", name, got, want)
				}
			}
		})
	}
}
