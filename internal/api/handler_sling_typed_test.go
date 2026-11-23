package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
				graph, _ := relocatedGraphStore(state)
				wantGraph = graph.Store
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

// TestSlingReslingOfPoolClaimedWorkIsIdempotent pins POST /sling's sessions
// store (OD-3c): the deps carry Stores().Sessions(), so a re-sling of work
// one of the pool's own sessions claimed under its session bead id, which
// lives in the sessions store, is an idempotent no-op on a split city and
// routes nothing again.
func TestSlingReslingOfPoolClaimedWorkIsIdempotent(t *testing.T) {
	h, state := newSlingTestServer(t)
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "smiths", Dir: "myrig", MaxActiveSessions: intPtr(4)})
	sessions := beads.NewMemStoreFrom(500, nil, nil)
	state.sessionsBeadStore = sessions
	own, err := sessions.Create(beads.Bead{
		Title:    "pool session",
		Type:     "session",
		Labels:   []string{"gc:session"},
		Metadata: map[string]string{"template": "myrig/smiths", "pool_managed": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	work := state.stores["myrig"]
	held, err := work.Create(beads.Bead{Title: "held", Type: "task", Status: "in_progress", Assignee: own.ID, Metadata: map[string]string{"gc.routed_to": "myrig/smiths"}})
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := work.Create(beads.Bead{Title: sling.AutoConvoyRootTitle(held.ID), Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(convoy.ID, held.ID, "tracks"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(`{"target":"myrig/smiths","bead":"`+held.ID+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["convoy_id"]; got != nil && got != "" {
		t.Errorf("re-sling of work pool session %s holds finalized again (convoy_id %v), want an idempotent no-op", own.ID, got)
	}
}

// TestGraphBindingIsTheRelocatedGraphStore pins graphBinding, the binding
// apiBeadRouter honors a request's store for, read from the request's own
// Stores: it is the store relocatedGraphStore names, the binding on a city
// that relocates graph and none on one that does not.
func TestGraphBindingIsTheRelocatedGraphStore(t *testing.T) {
	for _, split := range []bool{false, true} {
		state := newFakeState(t)
		state.cityBeadStore = beads.NewMemStore()
		if split {
			state.graphBeadStore = beads.NewMemStore()
		}
		graph, relocated := relocatedGraphStore(state)
		got, want := graphBinding(state.Stores()).Store, graph.Store
		if got != want {
			t.Errorf("split=%t: graphBinding = %v, want relocatedGraphStore's %v", split, got, want)
		}
		if split && !relocated {
			t.Errorf("split=%t: no binding; the fixture does not relocate graph", split)
		}
	}
}
