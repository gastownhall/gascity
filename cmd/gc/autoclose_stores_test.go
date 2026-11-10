package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// testStores is the epoch an autoclose run is handed in a test: work is the
// city work store, and a graph store other than work is a binding serving
// every infrastructure class, so Stores().Graph() is graph (nil or work:
// nothing relocated). It mints with no projection: a test that wants the
// gate's views passes them.
func testStores(work, graph beads.Store) storeref.Stores {
	return testStoresWithRigs(work, graph, nil)
}

// testStoresWithRigs is testStores with rig work stores, keyed by rig name,
// each with its configured prefix ("name" -> {store, prefix}).
func testStoresWithRigs(work, graph beads.Store, rigs map[string]testRig) storeref.Stores {
	t := storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: work}}
	stores := map[string]beads.Store{}
	for name, r := range rigs {
		stores[name] = r.store
	}
	for _, leg := range storeref.RigLegs(stores) {
		name, _ := storeref.ScopeRigContext(string(leg.Ref))
		leg.Prefix = rigs[name].prefix
		t.Rigs = append(t.Rigs, leg)
	}
	if graph != nil && !beads.SameStore(graph, work) {
		var classes []coordclass.Class
		for _, c := range coordclass.Classes() {
			if c.IsInfrastructure() {
				classes = append(classes, c)
			}
		}
		t.Bindings, t.Refused = storeref.BuildBinding(graph, classes, storeref.BindingOptions{})
	}
	return storeref.NewStores(t, storeref.StoresOptions{})
}

// testRig is one rig of testStoresWithRigs.
type testRig struct {
	store  beads.Store
	prefix string
}

// TestTestStoresHandsOutTheGraphStoreItWasGiven pins the helper's premise:
// the graph handle is the graph store, raw, on both shapes.
func TestTestStoresHandsOutTheGraphStoreItWasGiven(t *testing.T) {
	work, graph := beads.NewMemStore(), beads.NewMemStore()
	if got := testStores(work, graph).Graph().Store; got != beads.Store(graph) {
		t.Fatalf("split: Graph() = %T, want the graph store", got)
	}
	if got := testStores(work, work).Graph().Store; got != beads.Store(work) {
		t.Fatalf("unsplit: Graph() = %T, want the work store", got)
	}
}

// testWispGCRoots is a wisp GC pass's readers over graph and the census legs
// of legs (every leg with a store that is not graph, by ref), minted as the
// test mint mints legs (federated), for a test that drives the GC over stores
// it built: each census leg reads its own store. WorkLegs.wispGCRoots, the
// production builder, plans them over an epoch (t7g_wisp_gc_reference_test.go).
func testWispGCRoots(graph beads.Store, legs wispGCLegs) wispGCRoots {
	out := wispGCRoots{graph: beadstest.FederatedLeg(graph), work: map[string]beads.FederatedReader{}, workErr: map[string]error{}}
	if legs == nil || legs.unusable() != nil {
		return out
	}
	legs.eachLeg(func(ref string, store beads.Store) {
		if store != nil && !sameBeadStore(store, graph) {
			out.work[ref] = beadstest.FederatedLeg(store)
		}
	})
	return out
}
