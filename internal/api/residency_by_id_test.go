package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/storeref/storereftest"
)

// beadStoresForID renders the by-id PLAN as the ordered store list this
// package's retired resolver of the same name returned.
//
// It exists so every list-shaped pin written against that resolver keeps
// asserting the same thing about the plan that replaced it — the before/after
// artifact of the S1 migration. Production code resolves through
// storeref.ResolveOwner and never sees a list, which is why this lives in a
// test file: reaching a plan's legs directly is exactly what the residency
// boundary forbids of a consumer.
func (s *Server) beadStoresForID(id string) []beads.Store {
	plan, err := storeref.Plan(storeref.ByID{ID: id, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil
	}
	stores := make([]beads.Store, 0, len(plan.Legs))
	for _, leg := range plan.Legs {
		stores = append(stores, leg.Leg.Store)
	}
	return stores
}

// The tests below cover the residence probe: the leg that reaches a bead
// RESIDENT in the class binding under a work-shaped id.
//
// Two populations wear that shape. `gc storage migrate` preserves ids, so every
// row it relocated kept its HQ/rig-era prefix; and a class store MINTS from its
// own binding workspace's prefix, so a synthetic created there with no id — an
// input convoy, a patrol root, a wisp — is born work-shaped and class-resident.
// Both are outside the class namespace, so the namespace arm never fires for
// them, and before the probe the prefix resolver answered them from the work
// store alone: a 404 on every read and every write of a bead the city holds.

// classResidentWorkShapedID seeds a bead into the graph store under an id in
// the WORK prefix's namespace.
func classResidentWorkShapedID(t *testing.T, graph beads.Store, id string) string {
	t.Helper()
	return seedWithPinnedID(t, graph, id, "class-resident under a work prefix")
}

// seedWithPinnedID creates a bead under an exact id, which is what makes these
// fixtures able to model an id the class binding holds and a prefix store
// routes elsewhere.
func seedWithPinnedID(t *testing.T, store beads.Store, id, title string) string {
	t.Helper()
	mem, ok := store.(*beads.MemStore)
	if !ok {
		t.Fatalf("fixture store is %T, want *beads.MemStore so the test can pin the seeded id", store)
	}
	mem.HonorExplicitIDs = true
	created, err := mem.Create(beads.Bead{ID: id, Title: title})
	if err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	if created.ID != id {
		t.Fatalf("the fixture store minted %q instead of the pinned %q", created.ID, id)
	}
	return id
}

// TestByIDPlanLeadsWithTheResidenceProbe is the S1 contract, arm by arm, and the
// before/after pin for the whole slice.
//
// Each subtest names the work-axis arm it exercises. The work axis is
// BYTE-IDENTICAL to the retired resolver's on every one of them — this plane
// routes it (longest configured prefix, then routes.jsonl, then the scan) and
// the resolver takes that answer rather than substituting its own — and the
// only delta is the binding leg the plan puts in FRONT.
//
// In front, not behind, and that is the decision this slice makes: the binding
// holds the live copy of a relocated bead while the work store holds at most the
// migration's retained one, frozen when it was copied. cmd/gc's by-id door and
// classRoutedStoreForID have always probed the binding first, so leading with it
// is also what makes the two surfaces answer one id the same way.
func TestByIDPlanLeadsWithTheResidenceProbe(t *testing.T) {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}

	t.Run("configured prefix arm", func(t *testing.T) {
		st, graph, city, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"

		got := New(st).beadStoresForID("mc-123")
		if len(got) != 2 || got[0] != graph || got[1] != city {
			t.Fatalf("beadStoresForID(mc-123) = %v (len %d), want [graph %p, city %p] — the binding probe leads, the routed work store keeps its place behind it", got, len(got), graph, city)
		}
	})

	t.Run("routes.jsonl arm", func(t *testing.T) {
		st, graph, _, rig := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"
		writeRoutesJSONL(t, st.cityPath, `{"prefix":"rt","path":"rigs/myrig"}`)

		got := New(st).beadStoresForID("rt-1")
		if len(got) != 2 || got[0] != graph || got[1] != rig {
			t.Fatalf("beadStoresForID(rt-1) = %v (len %d), want [graph %p, rig %p] — a routes.jsonl-routed id keeps its route", got, len(got), graph, rig)
		}
	})

	t.Run("scan arm", func(t *testing.T) {
		st, graph, city, rig := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"

		got := New(st).beadStoresForID("zz-1")
		if len(got) != 3 || got[0] != graph || got[1] != city || got[2] != rig {
			t.Fatalf("beadStoresForID(zz-1) = %v (len %d), want [graph %p, city %p, rig %p] — an id no prefix claims still scans every work store", got, len(got), graph, city, rig)
		}
	})

	t.Run("namespace arm is untouched", func(t *testing.T) {
		st, graph, city, _ := relocatedGraphRouteState(t)

		got := New(st).beadStoresForID(prefix + "-1")
		if len(got) != 2 || got[0] != graph || got[1] != city {
			t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want the namespace arm's own [graph %p, city %p] with no second graph leg", prefix, got, len(got), graph, city)
		}
	})

	// A rig whose store IS the relocated class store. State hands out shared
	// store INSTANCES in the file provider — sortedRigNames dedupes by store
	// identity for exactly that reason — so an arm can already contain the store
	// the probe would add. Probing the same store twice on the fail-fast by-id
	// path is a duplicated 500 attributed to a store already reported.
	t.Run("class store already among the candidates", func(t *testing.T) {
		st, graph, city, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"
		st.cfg.Rigs[0].Prefix = "rw"
		st.stores["myrig"] = graph

		s := New(st)
		if got := s.beadStoresForID("rw-1"); len(got) != 1 || got[0] != graph {
			t.Fatalf("beadStoresForID(rw-1) = %v (len %d), want only the graph store %p once", got, len(got), graph)
		}
		if got := s.beadStoresForID("zz-1"); len(got) != 2 || got[0] != graph || got[1] != city {
			t.Fatalf("beadStoresForID(zz-1) = %v (len %d), want [graph %p, city %p] with the graph leg listed once", got, len(got), graph, city)
		}
	})

	t.Run("single-store city is identity", func(t *testing.T) {
		st := newFakeState(t)
		city := beads.NewMemStore()
		rig := beads.NewMemStore()
		st.cityBeadStore = city
		st.stores = map[string]beads.Store{"myrig": rig}
		st.cfg.Workspace.Prefix = "mc"
		st.cfg.Rigs = []config.Rig{{Name: "myrig", Path: "/tmp/myrig", Prefix: "rw"}}

		s := New(st)
		if got := s.beadStoresForID("mc-123"); len(got) != 1 || got[0] != city {
			t.Fatalf("beadStoresForID(mc-123) = %v (len %d), want only the city store %p", got, len(got), city)
		}
		if got := s.beadStoresForID("rw-1"); len(got) != 1 || got[0] != rig {
			t.Fatalf("beadStoresForID(rw-1) = %v (len %d), want only the rig store %p", got, len(got), rig)
		}
		if got := s.beadStoresForID("zz-1"); len(got) != 2 || got[0] != city || got[1] != rig {
			t.Fatalf("beadStoresForID(zz-1) = %v (len %d), want the unchanged scan [city %p, rig %p]", got, len(got), city, rig)
		}
	})
}

// TestSingleStoreCityByIDPerformsNoProbe is the identity fast-path, measured.
//
// A city that relocates nothing must pay nothing for a seam it cannot use: the
// plan's only leg is the work residual, ResolveOwner hands it back unprobed, and
// the handler's own read is the only read. The control is the relocated city,
// which must probe — without it a broken counter would report a pass.
func TestSingleStoreCityByIDPerformsNoProbe(t *testing.T) {
	seed := func(store *beads.MemStore, id string) {
		store.HonorExplicitIDs = true
		if _, err := store.Create(beads.Bead{ID: id, Title: "subject"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}

	t.Run("single-store city reads once", func(t *testing.T) {
		st := newFakeState(t)
		city := beads.NewMemStore()
		seed(city, "mc-1")
		counted := &countingGetStore{Store: city}
		st.cityBeadStore = counted
		st.stores = nil
		st.cfg.Rigs = nil
		st.cfg.Workspace.Prefix = "mc"

		if _, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: "mc-1"}); err != nil {
			t.Fatalf("GET mc-1: %v", err)
		}
		if counted.gets != 1 {
			t.Fatalf("the work store was read %d times, want 1 — a city that relocates nothing must not pay for the seam", counted.gets)
		}
	})

	t.Run("control: a relocated city probes", func(t *testing.T) {
		st, graph, city, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"
		memCity, ok := city.(*beads.MemStore)
		if !ok {
			t.Fatalf("fixture city store is %T", city)
		}
		seed(memCity, "mc-1")
		counted := &countingGetStore{Store: graph}
		st.graphBeadStore = counted

		if _, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: "mc-1"}); err != nil {
			t.Fatalf("GET mc-1: %v", err)
		}
		if counted.gets != 1 {
			t.Fatalf("the binding was probed %d times, want 1 — the counter proves the fast-path assertion above can fail", counted.gets)
		}
	})

	// A bead the PROBE finds must not then be read again. The probe's own read
	// is the read the handler needed, and discarding it would double the cost of
	// every by-id operation against a relocated city's binding — the hot path.
	t.Run("a probed hit is read exactly once", func(t *testing.T) {
		st, graph, _, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"
		memGraph, ok := graph.(*beads.MemStore)
		if !ok {
			t.Fatalf("fixture graph store is %T", graph)
		}
		seed(memGraph, "mc-relic1")
		counted := &countingGetStore{Store: graph}
		st.graphBeadStore = counted

		out, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: "mc-relic1"})
		if err != nil {
			t.Fatalf("GET mc-relic1: %v", err)
		}
		if out.Body.ID != "mc-relic1" {
			t.Fatalf("resolved bead id = %q, want mc-relic1", out.Body.ID)
		}
		if counted.gets != 1 {
			t.Fatalf("the binding was read %d times, want 1 — the probe's read is the handler's read", counted.gets)
		}
	})
}

type countingGetStore struct {
	beads.Store
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// TestBeadGetServesClassResident is the read half through the real handler:
// GET /v0/bead/{id} answered 404 for a bead the city holds, because the
// prefix-routed work store was the only candidate.
func TestBeadGetServesClassResident(t *testing.T) {
	st, graph, city, _ := relocatedGraphRouteState(t)
	st.cfg.Workspace.Prefix = "mc"
	id := classResidentWorkShapedID(t, graph, "mc-relic1")
	if _, err := city.Get(id); err == nil {
		t.Fatalf("the work store also holds %s; the fixture proves nothing", id)
	}

	out, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: id})
	if err != nil {
		t.Fatalf("GET bead %s: %v — a class-resident work-shaped id is unreachable by id", id, err)
	}
	if out.Body.ID != id {
		t.Fatalf("resolved bead id = %q, want %q", out.Body.ID, id)
	}
}

// TestBeadWriteLandsInClassStoreForWorkPrefixedResident is the write half,
// across every mutating by-id handler.
//
// Each one resolves the owner once and then binds its write to THAT store —
// "once Get succeeded in the resolved store, treat Update-ErrNotFound as a
// concurrent-delete race rather than resolving again". Read/write coherence on
// this surface is therefore structural, and adding a read leg adds the write leg
// with it.
func TestBeadWriteLandsInClassStoreForWorkPrefixedResident(t *testing.T) {
	const renamed = "renamed by the api"
	for name, tc := range map[string]struct {
		setup  func(*testing.T, beads.Store, string)
		mutate func(*Server, string) error
		verify func(*testing.T, beads.Bead)
	}{
		"close": {
			mutate: func(s *Server, id string) error {
				_, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: id})
				return err
			},
			verify: func(t *testing.T, b beads.Bead) {
				if b.Status != "closed" {
					t.Errorf("status = %q, want closed", b.Status)
				}
			},
		},
		"delete": {
			mutate: func(s *Server, id string) error {
				_, err := s.humaHandleBeadDelete(context.Background(), &BeadDeleteInput{ID: id})
				return err
			},
			verify: func(t *testing.T, b beads.Bead) {
				if b.Status != "closed" {
					t.Errorf("status = %q, want closed (DELETE is a soft close on this surface)", b.Status)
				}
			},
		},
		"update": {
			mutate: func(s *Server, id string) error {
				title := renamed
				_, err := s.humaHandleBeadUpdate(context.Background(), &BeadUpdateInput{ID: id, Body: beadUpdateBody{Title: &title}})
				return err
			},
			verify: func(t *testing.T, b beads.Bead) {
				if b.Title != renamed {
					t.Errorf("title = %q, want %q", b.Title, renamed)
				}
			},
		},
		"assign": {
			setup: func(t *testing.T, graph beads.Store, id string) {
				held := "previous-holder"
				if err := graph.Update(id, beads.UpdateOpts{Assignee: &held}); err != nil {
					t.Fatalf("seeding an assignee on %s: %v", id, err)
				}
			},
			mutate: func(s *Server, id string) error {
				in := &BeadAssignInput{ID: id}
				in.Body.Assignee = ""
				_, err := s.humaHandleBeadAssign(context.Background(), in)
				return err
			},
			verify: func(t *testing.T, b beads.Bead) {
				if b.Assignee != "" {
					t.Errorf("assignee = %q, want the routed assign to have cleared it", b.Assignee)
				}
			},
		},
		"reopen": {
			setup: func(t *testing.T, graph beads.Store, id string) {
				if err := graph.Close(id); err != nil {
					t.Fatalf("pre-closing %s: %v", id, err)
				}
			},
			mutate: func(s *Server, id string) error {
				_, err := s.humaHandleBeadReopen(context.Background(), &BeadReopenInput{ID: id})
				return err
			},
			verify: func(t *testing.T, b beads.Bead) {
				if b.Status == "closed" {
					t.Errorf("status = %q, want reopened", b.Status)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, graph, city, _ := relocatedGraphRouteState(t)
			st.cfg.Workspace.Prefix = "mc"
			id := classResidentWorkShapedID(t, graph, "mc-relic1")
			if tc.setup != nil {
				tc.setup(t, graph, id)
			}

			if err := tc.mutate(New(st), id); err != nil {
				t.Fatalf("%s %s: %v — the write 404'd on a bead resident in the class binding", name, id, err)
			}
			if _, err := city.Get(id); err == nil {
				t.Errorf("the work store holds %s after the routed %s; the write must land in the store whose Get answered", id, name)
			}
			after, err := graph.Get(id)
			if err != nil {
				t.Fatalf("re-reading %s from the class binding: %v", id, err)
			}
			tc.verify(t, after)
		})
	}
}

// TestBeadDualResidentAnswersFromTheBinding is the ambiguity pin, and it is the
// one place S1 deliberately CHANGES an answer this surface already served.
//
// `gc storage migrate` copies with ids preserved and never deletes the source,
// so a relocated bead can be resident in both stores. The API used to answer
// such an id from the work store; #5245 proposed keeping that by appending the
// class leg behind the prefix-routed ones. S1 leads with it instead, for two
// reasons that outrank byte-identity here:
//
//   - The work copy is the migration's RETAINED one, frozen when it was copied.
//     The binding holds the row the controller and the class doors read and
//     write. Serving the work copy is not "the old answer"; it is a stale answer,
//     and a write that follows it lands where nothing reads.
//   - cmd/gc already answers this id from the binding — the by-id class door and
//     classRoutedStoreForID both probe residence first. Appending behind would
//     have left the CLI and the API disagreeing about which copy of one id is
//     real, which is the divergence the one-lookup-contract exists to end.
//
// The control is the id the binding does NOT hold: it must still answer from the
// work store, which is what proves this is about residence and not about the
// binding winning everything.
//
// The clauses themselves live in storereftest because they are only worth
// anything if BOTH by-id surfaces satisfy them — this one and cmd/gc's class
// door (TestBdCloseDualResidentWritesServingCopy). Two planes each passing their
// own well-written pin while answering one id differently is precisely the bug,
// so the property is written once and this test supplies the HTTP adapter.
func TestBeadDualResidentAnswersFromTheBinding(t *testing.T) {
	st, graph, city, _ := relocatedGraphRouteState(t)
	st.cfg.Workspace.Prefix = "mc"
	id := classResidentWorkShapedID(t, graph, "mc-dual1")
	seedWithPinnedID(t, city, id, "the retained work copy")
	seedWithPinnedID(t, city, "mc-workonly1", "a work bead the binding never held")

	s := New(st)
	storereftest.RunBindingWins(t,
		storereftest.BindingWinsStores{
			Binding:       graph,
			Work:          city,
			DualID:        id,
			BindingTitle:  "class-resident under a work prefix",
			WorkOnlyID:    "mc-workonly1",
			WorkOnlyTitle: "a work bead the binding never held",
		},
		storereftest.BindingWinsSurface{
			Name: "the beads HTTP by-id surface",
			Get: func(t *testing.T, id string) beads.Bead {
				t.Helper()
				out, err := s.humaHandleBeadGet(context.Background(), &BeadGetInput{ID: id})
				if err != nil {
					t.Fatalf("GET bead %s: %v", id, err)
				}
				return out.Body
			},
			Close: func(t *testing.T, id string) {
				t.Helper()
				if _, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: id}); err != nil {
					t.Fatalf("POST bead %s close: %v", id, err)
				}
			},
		})
}

// wholeSplitState builds the shape a relocated city actually serves: ONE
// binding carrying every infrastructure class, which is what makes the
// topology's binding claim all five reserved prefixes rather than only the
// graph one.
func wholeSplitState(t *testing.T) (*fakeState, beads.Store, beads.Store) {
	t.Helper()
	st := newFakeState(t)
	city := beads.NewMemStore()
	binding := beads.NewMemStore()
	st.cityBeadStore = city
	st.graphBeadStore = binding
	st.sessionsBeadStore = binding
	st.ordersBeadStore = binding
	st.nudgesBeadStore = binding
	st.stores = nil
	st.cfg.Rigs = nil
	st.cfg.Workspace.Prefix = "mc"
	return st, binding, city
}

// TestWholeSplitBindingIsAuthorityForEveryReservedPrefix pins a production
// routing change this slice makes deliberately.
//
// The retired resolver's class arm was GRAPH ONLY — its own doc said adding the
// other classes "would change which stores answer for gcs-/gco-/gcn- ids". The
// resolver takes the topology as it is, so on a city serving the whole split the
// binding is the AUTHORITY for every prefix it mints, which is what cmd/gc has
// always done (bdIDIsClassReserved reads the whole reserved set) and therefore
// what makes the two surfaces answer one id the same way.
//
// The store ORDER was already going to be [binding, work] for these ids — the
// residence probe leads regardless — so what the namespace arm changes is the
// leg's POLICY, and the control below is where that becomes visible.
func TestWholeSplitBindingIsAuthorityForEveryReservedPrefix(t *testing.T) {
	st, binding, city := wholeSplitState(t)
	s := New(st)

	for _, class := range []string{config.BeadClassGraph, config.BeadClassSessions, config.BeadClassOrders, config.BeadClassNudges} {
		prefix, ok := config.ReservedClassPrefix(class)
		if !ok {
			t.Fatalf("ReservedClassPrefix(%s) returned ok=false", class)
		}
		got := s.beadStoresForID(prefix + "-1")
		if len(got) != 2 || got[0] != binding || got[1] != city {
			t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [binding %p, city %p]", prefix, got, len(got), binding, city)
		}
	}
}

// TestByIDFailsLoudWhenTheBindingCannotBeRead is the cost of the residence
// probe, stated as a contract rather than discovered in production.
//
// On a split city the binding can hold a bead under ANY id, so a binding that
// cannot be read has told the resolution nothing — and answering from the work
// store anyway would serve `gc storage migrate`'s retained copy, frozen at
// migration time, as though it were the live row. That is the failure this lane
// exists to prevent, so the read fails loud with the store named instead.
//
// Two controls keep the claim narrow. A city that relocates nothing has no
// binding to probe and is untouched. And a STANDING STORAGE REFUSAL — the
// build's verdict about the city's configuration rather than a fault of this
// read — is tolerated for an id no relocated class could own, because a refused
// city still serves work from its work ledger.
func TestByIDFailsLoudWhenTheBindingCannotBeRead(t *testing.T) {
	seedWork := func(t *testing.T, city beads.Store) string {
		t.Helper()
		return seedWithPinnedID(t, city, "mc-work1", "a plain work bead")
	}

	t.Run("unreachable binding fails a work-id read", func(t *testing.T) {
		st, binding, city := wholeSplitState(t)
		id := seedWork(t, city)
		st.graphBeadStore = getFailStore{Store: binding, err: errors.New("infra binding unreachable")}

		_, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: id})
		var statusErr huma.StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("GET returned %T %v, want a Huma status error", err, err)
		}
		if statusErr.GetStatus() != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 — the binding could hold this id and could not be asked", statusErr.GetStatus())
		}
	})

	t.Run("control: a city that relocates nothing is untouched", func(t *testing.T) {
		st := newFakeState(t)
		city := beads.NewMemStore()
		st.cityBeadStore = city
		st.stores = nil
		st.cfg.Rigs = nil
		st.cfg.Workspace.Prefix = "mc"
		id := seedWork(t, city)

		if _, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: id}); err != nil {
			t.Fatalf("GET %s: %v — a single-store city has no binding to probe", id, err)
		}
	})

	t.Run("control: a standing refusal still serves the work id", func(t *testing.T) {
		st, binding, city := wholeSplitState(t)
		id := seedWork(t, city)
		st.graphBeadStore = getFailStore{Store: binding, err: standingRefusalErr{}}

		out, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: id})
		if err != nil {
			t.Fatalf("GET %s: %v — a refused city still serves work from its work ledger", id, err)
		}
		if out.Body.ID != id {
			t.Fatalf("resolved bead id = %q, want %q", out.Body.ID, id)
		}
	})
}

// standingRefusalErr is the build's verdict about the CITY's storage
// configuration, as distinct from a fault the read ran into.
type standingRefusalErr struct{}

func (standingRefusalErr) Error() string           { return "storage refused: run `gc storage migrate`" }
func (standingRefusalErr) StandingStorageRefusal() {}

// TestBeadMissThenUnreachableClassStoreIs500Not404 keeps the fail-loud doctrine
// on the added leg: an unreachable store must not be reported as a missing
// bead. The control proves the change is about reachability, not about turning
// every miss into an error.
func TestBeadMissThenUnreachableClassStoreIs500Not404(t *testing.T) {
	t.Run("unreachable class store", func(t *testing.T) {
		st, graph, _, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"
		st.graphBeadStore = getFailStore{Store: graph, err: errors.New("infra binding unreachable")}

		_, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: "mc-relic1"})
		var statusErr huma.StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("GET returned %T %v, want a Huma status error", err, err)
		}
		if statusErr.GetStatus() != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 — an unreachable store reported as a missing bead is the root-loss shape", statusErr.GetStatus())
		}
	})

	t.Run("clean miss stays 404", func(t *testing.T) {
		st, _, _, _ := relocatedGraphRouteState(t)
		st.cfg.Workspace.Prefix = "mc"

		_, err := New(st).humaHandleBeadGet(context.Background(), &BeadGetInput{ID: "mc-relic1"})
		var statusErr huma.StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("GET returned %T %v, want a Huma status error", err, err)
		}
		if statusErr.GetStatus() != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 — a bead no store holds is absent, not an outage", statusErr.GetStatus())
		}
	})
}
