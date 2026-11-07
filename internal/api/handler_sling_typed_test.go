package api

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TestSlingStoresAreTheStoresTheAccessorsServed pins POST /sling's stores
// (typed-stores T6d, R8) on four store shapes: a city that relocates nothing
// and one that relocates graph, each at city and at rig scope. The work
// handle findSlingStore returns wraps the store the raw accessor
// (CityBeadStore, BeadStore) served there, so its reads take the same cached
// or live path; the graph handle is the relocated graph binding on the split
// city, and the selected work store itself on the other, where the sling
// cooks the workflow next to its source bead (the old nil GraphStore's
// collapse, through the gate's class view when the split-strict gate is on).
// An unopened rig scope has neither.
func TestSlingStoresAreTheStoresTheAccessorsServed(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, rigScope := range []bool{false, true} {
			state := newFakeState(t)
			city := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
			rig := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
			state.cityBeadStore = city
			state.stores = map[string]beads.Store{"myrig": rig}
			if split {
				state.graphBeadStore = beads.NewMemStore()
			}
			srv := New(state)
			st := state.Stores()
			name, rigName, agent := "city", "", config.Agent{Name: "mayor"}
			old, wantScope := state.CityBeadStore(), storeref.WorkRef
			if rigScope {
				name, rigName, agent = "rig", "myrig", config.Agent{Name: "worker", Dir: "myrig"}
				old, wantScope = state.BeadStore("myrig"), storeref.RigRef("myrig")
			}
			if split {
				name = "split " + name
			}

			work, scope := srv.findSlingStore(st, rigName, agent, "")
			if work.Store != old || scope != wantScope {
				t.Errorf("%s: findSlingStore = %v at %s, want the accessor's %v at %s", name, work.Store, scope, old, wantScope)
			}
			_, oldLive := beads.As[beads.CacheLivenessReporter](old)
			if _, live := beads.As[beads.CacheLivenessReporter](work); live != oldLive {
				t.Errorf("%s: the work handle reports cache liveness %t, the accessor's store %t", name, live, oldLive)
			}

			wantGraph := splittest.GateClassView(old)
			if split {
				wantGraph = relocatedGraphStore(state)
			}
			if got := graphAt(st, scope).Store; got != wantGraph {
				t.Errorf("%s: graph store = %v, want %v", name, got, wantGraph)
			}
		}
	}

	state := newFakeState(t)
	if got := graphAt(state.Stores(), storeref.RigRef("unopened")); got != (beads.GraphStore{}) {
		t.Errorf("graph store at an unopened rig = %v, want the zero handle", got)
	}
}

// TestAPIBeadRouterWithNoStoreRefuses pins apiBeadRouter's absent store: with
// a zero work store and no graph-binding target named by the request it
// refuses instead of writing through a zero handle.
func TestAPIBeadRouterWithNoStoreRefuses(t *testing.T) {
	r := apiBeadRouter{server: New(newFakeState(t))}
	err := r.Route(t.Context(), sling.RouteRequest{BeadID: "gc-1", Target: "myrig/worker"})
	if err == nil || !strings.Contains(err.Error(), "requires a store") {
		t.Fatalf("Route with no store = %v, want the requires-a-store refusal", err)
	}
}
