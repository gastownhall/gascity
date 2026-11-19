package api

import (
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// byIDHandleWraps are apiHandleWraps plus the resident handle the by-id door
// itself hands its handlers since T7c (R2, R9).
func byIDHandleWraps() map[string]func(beads.Store) beads.Store {
	out := map[string]func(beads.Store) beads.Store{
		"a resident handle": func(s beads.Store) beads.Store { return beadstest.Resident(s) },
	}
	for name, wrap := range apiHandleWraps {
		out[name] = wrap
	}
	return out
}

// TestByIDHandlersServeThroughAHandle runs the by-id doors (GET, PATCH,
// close, reopen) over a rig store reached as the raw store and as each handle
// shape, and the split doors over a binding reached the same ways: each must
// answer, and write, exactly as through the raw store. resolveBeadOwner hands
// its handlers a ResidentStore over whatever the epoch minted, so a handler
// that compared it with a raw store, or asserted a concrete type on it, would
// lose the write or answer 404.
func TestByIDHandlersServeThroughAHandle(t *testing.T) {
	for name, wrap := range byIDHandleWraps() {
		t.Run(name+"/rig", func(t *testing.T) {
			state := newFakeState(t)
			rig := beads.NewMemStore()
			b, err := rig.Create(beads.Bead{Title: "work", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			state.stores = map[string]beads.Store{"myrig": wrap(rig)}
			routed := asHandedState{state}
			h := newTestCityHandler(t, routed)

			if rec := beadRequest(t, h, http.MethodGet, cityURL(routed, "/bead/")+b.ID, ""); rec.Code != http.StatusOK {
				t.Fatalf("GET = %d; body=%s", rec.Code, rec.Body.String())
			}
			if rec := beadRequest(t, h, http.MethodPatch, cityURL(routed, "/bead/")+b.ID, `{"title":"renamed"}`); rec.Code != http.StatusOK {
				t.Fatalf("PATCH = %d; body=%s", rec.Code, rec.Body.String())
			}
			if rec := beadRequest(t, h, http.MethodPost, cityURL(routed, "/bead/")+b.ID+"/close", `{}`); rec.Code != http.StatusOK {
				t.Fatalf("close = %d; body=%s", rec.Code, rec.Body.String())
			}
			got, err := rig.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != "renamed" || got.Status != "closed" {
				t.Fatalf("the rig row is %q/%s, want renamed/closed: a write missed the store the door resolved", got.Title, got.Status)
			}
			if rec := beadRequest(t, h, http.MethodPost, cityURL(routed, "/bead/")+b.ID+"/reopen", ""); rec.Code != http.StatusOK {
				t.Fatalf("reopen = %d; body=%s", rec.Code, rec.Body.String())
			}
			if got, _ := rig.Get(b.ID); got.Status != "open" {
				t.Fatalf("reopen left the rig row %s", got.Status)
			}
		})
		t.Run(name+"/binding", func(t *testing.T) {
			state, binding := bindingResidentState(t, wrap)
			h := newTestCityHandler(t, state)
			if rec := beadRequest(t, h, http.MethodGet, cityURL(state, "/bead/")+"gcg-1", ""); rec.Code != http.StatusOK {
				t.Fatalf("GET of a binding resident = %d; body=%s", rec.Code, rec.Body.String())
			}
			assertBindingRepairIsServed(t, h, state, binding)
			if rec := beadRequest(t, h, http.MethodPatch, cityURL(state, "/bead/")+"gcg-1", `{"metadata":{"gc.root_bead_id":""}}`); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH that turns a binding resident into a work bead = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// bindingResidentState is a whole-split city whose binding, reached as
// wrap(binding), holds gcg-1, a graph step, and gcg-3, a binding resident
// that reads as work class (a pre-split copy) under the binding's namespace.
func bindingResidentState(t *testing.T, wrap func(beads.Store) beads.Store) (asHandedState, beads.Store) {
	t.Helper()
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	binding := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "gcg-1", Title: "step", Type: "task", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "gcg-0"}},
		{ID: "gcg-3", Title: "stranded", Type: "task", Status: "open"},
	}, nil)
	handle := wrap(binding)
	state.graphBeadStore, state.sessionsBeadStore, state.ordersBeadStore, state.nudgesBeadStore = handle, handle, handle, handle
	return asHandedState{state}, binding
}

// assertBindingRepairIsServed is the T7c holds pin: a PATCH that moves a
// binding resident INTO a class the binding serves (a work-class row repaired
// back into the graph class) is legitimate, and is served. The PATCH door asks
// what the owner holds by its ref (Stores.Holds); asked by comparing the
// owner's handle with the binding's store it never matched, judged the
// binding a work store that does not hold graph, and refused with 422.
func assertBindingRepairIsServed(t *testing.T, h http.Handler, state State, binding beads.Store) {
	t.Helper()
	rec := beadRequest(t, h, http.MethodPatch, cityURL(state, "/bead/")+"gcg-3", `{"metadata":{"gc.root_bead_id":"gcg-0"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repairing a binding resident into the graph class = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := binding.Get("gcg-3")
	if err != nil {
		t.Fatal(err)
	}
	if class := coordclass.Classify(got); class != coordclass.ClassGraph {
		t.Fatalf("the repaired row is %s class in the binding, want graph", class)
	}
}

// TestBindingRepairPatchIsServedOnTheProductionShape is the holds pin on the
// fake State as production builds it (gate views included under the gate).
func TestBindingRepairPatchIsServedOnTheProductionShape(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	binding := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gcg-3", Title: "stranded", Type: "task", Status: "open"}}, nil)
	state.graphBeadStore, state.sessionsBeadStore, state.ordersBeadStore, state.nudgesBeadStore = binding, binding, binding, binding
	assertBindingRepairIsServed(t, newTestCityHandler(t, state), state, binding)
}

// TestByIDOwnerOverACacheReadsLiveAndWritesThroughIt pins the resident handle
// over a CachingStore (R1): the door's owner is the handle over the cache, so
// a handler's write goes through the cache (which sees it, as a GET after it
// shows), and a live read through the handle reaches past the cache to the
// backing, as through the raw cache.
func TestByIDOwnerOverACacheReadsLiveAndWritesThroughIt(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatal(err)
	}
	state.stores = map[string]beads.Store{"myrig": cache}
	// As handed: the gate's work view would sit over the cache, where a
	// production epoch puts it inside the handle.
	routed := asHandedState{state}
	s := New(routed)

	owner, _, err := s.resolveBeadOwner(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beads.SameStore(owner, cache) {
		t.Fatalf("the owner is %v, want the rig's cache", owner)
	}
	if _, ok := beads.As[*beads.CachingStore](owner); !ok {
		t.Fatal("the owner handle hides the cache from beads.As")
	}

	// Behind the cache's back: the cached read is stale, the live one is not.
	behind := "behind the cache"
	if err := mem.Update(b.ID, beads.UpdateOpts{Title: &behind}); err != nil {
		t.Fatal(err)
	}
	live, err := beads.LiveFor(owner).Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Title != "behind the cache" {
		t.Fatalf("a live read through the owner handle = %q, want the backing's row", live.Title)
	}

	h := newTestCityHandler(t, routed)
	if rec := beadRequest(t, h, http.MethodPost, cityURL(routed, "/bead/")+b.ID+"/close", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("close = %d; body=%s", rec.Code, rec.Body.String())
	}
	cached, err := cache.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Status != "closed" {
		t.Fatalf("the cache reads %s after a close through the door, want closed: the write bypassed it", cached.Status)
	}
}

// TestByIDOwnerErrorIsAZeroHandle pins R9 on the door: a miss and a malformed
// id answer the zero resident handle, which no caller reads (each returns the
// error), and which IsNilStore names as no store.
func TestByIDOwnerErrorIsAZeroHandle(t *testing.T) {
	s := New(newFakeState(t))
	for _, id := range []string{"", "  ", "zz-404"} {
		owner, _, err := s.resolveBeadOwner(id)
		if err == nil {
			t.Fatalf("resolveBeadOwner(%q) succeeded", id)
		}
		if owner != (beads.ResidentStore{}) || !beads.IsNilStore(owner) {
			t.Errorf("resolveBeadOwner(%q) = %v alongside %v, want the zero handle", id, owner, err)
		}
	}
}

// TestByIDOwnerIsNamedByItsLeg pins the owner's ref, which the PATCH door asks
// Holds by: the binding's for a binding resident, the rig's for a rig bead.
func TestByIDOwnerIsNamedByItsLeg(t *testing.T) {
	state, _ := bindingResidentState(t, func(s beads.Store) beads.Store { return s })
	rig := beads.NewMemStore()
	b, err := rig.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	state.stores = map[string]beads.Store{"myrig": rig}
	s := New(state)
	binding, _ := state.Stores().Topology().BindingFor(coordclass.ClassGraph)
	for id, want := range map[string]storeref.StoreRef{"gcg-1": binding.Leg.Ref, b.ID: storeref.RigRef("myrig")} {
		owner, _, err := s.resolveBeadOwner(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := storeref.StoreRef(owner.Ref()); got != want {
			t.Errorf("%s: owner ref %q, want %q", id, got, want)
		}
	}
}
