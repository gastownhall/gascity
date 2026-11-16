package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// referenceRefsAxis answers the frozen router's legs (referenceWorkAxis) by
// ref, once each store is read once (the plan's dedupe), so the frozen router
// can drive a plan over the frozen topology. Each ref must name its leg's own
// store in that topology; a ref that does not would make the comparison
// compare a different plan, so it fails the test.
type referenceRefsAxis struct {
	t    testing.TB
	s    *Server
	topo storeref.Topology
}

func (a referenceRefsAxis) WorkLegsForID(id string) []storeref.StoreRef {
	var refs []storeref.StoreRef
	for _, leg := range dedupeWorkLegs(referenceWorkAxis{a.s}.WorkLegs(id)) {
		have, ok := topologyWorkLeg(a.topo, leg.Ref)
		if !ok || !beads.SameStore(have.Store, leg.Store) {
			a.t.Fatalf("the frozen router's leg %q for %s reads %v, which the frozen topology names %v", leg.Ref, id, leg.Store, have.Store)
		}
		refs = append(refs, leg.Ref)
	}
	return refs
}

// byIDOutcome is what a by-id door hands its handler, rendered for
// comparison: the owner's ref and store, the row, or the error.
type byIDOutcome struct {
	ref   storeref.StoreRef
	store beads.Store
	id    string
	err   string
}

func (o byIDOutcome) String() string {
	if o.err != "" {
		return "error: " + o.err
	}
	return fmt.Sprintf("%q holds %s", o.ref, o.id)
}

// referenceResolveBeadOwner is resolveBeadOwner as it stood before T7c: the
// frozen topology and router, the raw owner store.
func referenceResolveBeadOwner(t testing.TB, s *Server, id string) byIDOutcome {
	topo := referenceResidencyTopology(s)
	plan, err := storeref.Plan(storeref.ByID{ID: id, WorkAxis: referenceRefsAxis{t: t, s: s, topo: topo}}, topo)
	if err != nil {
		return byIDOutcome{err: err.Error()}
	}
	owner, err := storeref.ResolveOwnerRow(plan, id)
	if err != nil {
		return byIDOutcome{err: beadOwnerError(id, err).Error()}
	}
	b := owner.Bead
	if !owner.Read {
		if b, err = owner.Store.Get(id); err != nil {
			return byIDOutcome{err: beadOwnerError(id, err).Error()}
		}
	}
	return byIDOutcome{ref: owner.Ref, store: owner.Store, id: b.ID}
}

func resolveBeadOwnerOutcome(s *Server, id string) byIDOutcome {
	owner, b, err := s.resolveBeadOwner(id)
	if err != nil {
		return byIDOutcome{err: err.Error()}
	}
	return byIDOutcome{ref: storeref.StoreRef(owner.Ref()), store: owner, id: b.ID}
}

// seedPinned creates id in mem, a MemStore that honors explicit ids.
func seedPinned(t testing.TB, mem *beads.MemStore, ids ...string) {
	t.Helper()
	mem.HonorExplicitIDs = true
	for _, id := range ids {
		if _, err := mem.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
}

// byIDExecIDs is every id the executed comparison resolves on every shape:
// one per arm, present and absent.
var byIDExecIDs = []string{"tc-1", "tc-co", "tc-relic", "tc-404", "mr-1", "mr-404", "ot-1", "tx-1", "tx-9", "tx-404", "gcg-1", "gcg-404", "zz-1", "zz-404"}

// byIDExecBase is the unsplit city every executed shape starts from: the
// city store (tc-1, tc-co) and two rigs, myrig (mr-1, zz-1) and other (ot-1).
func byIDExecBase(t *testing.T) (*fakeState, *beads.MemStore, *beads.MemStore, *beads.MemStore) {
	st := newFakeState(t)
	city, mine, other := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	seedPinned(t, city, "tc-1", "tc-co")
	seedPinned(t, mine, "mr-1", "zz-1")
	seedPinned(t, other, "ot-1")
	st.cityBeadStore = city
	st.cfg.Workspace.Prefix = "tc"
	st.cfg.Rigs = []config.Rig{{Name: "myrig", Prefix: "mr"}, {Name: "other", Prefix: "ot"}}
	st.stores = map[string]beads.Store{"myrig": mine, "other": other}
	return st, city, mine, other
}

// byIDExecShapes are the cities the executed comparison runs over. Each
// seeds the ids it holds: tc- in the city, mr-/ot-/tx- in their rigs, zz-1
// (an id no prefix routes) in a rig, and on a split gcg-1, tc-co (also in the
// city: a co-resident id) and tc-relic (a migrate-preserved relic) in the
// binding.
func byIDExecShapes(t *testing.T) map[string]State {
	base := func() (*fakeState, *beads.MemStore, *beads.MemStore, *beads.MemStore) { return byIDExecBase(t) }
	split := func(st *fakeState, binding beads.Store) {
		st.graphBeadStore, st.sessionsBeadStore, st.ordersBeadStore, st.nudgesBeadStore = binding, binding, binding, binding
	}
	bindingMem := func() *beads.MemStore {
		mem := beads.NewMemStore()
		seedPinned(t, mem, "gcg-1", "tc-co", "tc-relic")
		return mem
	}
	refusal := standingRefusingStore{Store: beads.NewMemStore()}
	out := map[string]State{}

	unsplit, _, _, _ := base()
	out["unsplit"] = unsplit

	whole, _, _, _ := base()
	split(whole, bindingMem())
	out["whole split"] = whole

	clean, _, _, _ := base()
	cleanBinding := bindingMem()
	split(clean, cleanBinding)
	clean.bindingRelics = map[beads.Store]bool{cleanBinding: false}
	out["whole split, censused clean"] = clean

	graphOnly, _, _, _ := base()
	graphOnly.graphBeadStore = bindingMem()
	out["graph only"] = graphOnly

	named, _, _, _ := base()
	namedRig := beads.NewMemStore()
	seedPinned(t, namedRig, "tx-1")
	named.cfg.Rigs = append(named.cfg.Rigs, config.Rig{Name: named.cityName, Prefix: "tx"})
	named.stores[named.cityName] = namedRig
	out["a rig named like the city"] = named

	unopened, _, _, unopenedOther := base()
	seedPinned(t, unopenedOther, "tx-9")
	unopened.cfg.Rigs = append(unopened.cfg.Rigs, config.Rig{Name: unopened.cityName, Prefix: "tx"})
	out["an unopened rig named like the city"] = unopened

	worker, _, _, _ := base()
	split(worker, bindingMem())
	worker.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{}
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			worker.classRefusals[c] = refusal
		}
	}
	out["a worker site refusing every class"] = worker

	cached, city, mine, _ := base()
	bmem := bindingMem()
	caches := map[string]*beads.CachingStore{}
	for name, mem := range map[string]*beads.MemStore{"city": city, "myrig": mine, "binding": bmem} {
		c := beads.NewCachingStoreForTest(mem, nil)
		if err := c.Prime(t.Context()); err != nil {
			t.Fatal(err)
		}
		caches[name] = c
	}
	cached.cityBeadStore, cached.stores["myrig"] = caches["city"], caches["myrig"]
	split(cached, caches["binding"])
	out["cache-backed whole split"] = cached

	suspended, _, _, _ := base()
	split(suspended, bindingMem())
	out["a suspended rig"] = suspendedRigState{fakeState: suspended, rig: "other"}

	noCity, _, _, _ := base()
	noCity.cityBeadStore = nil
	out["no city store"] = noCity
	return out
}

// byIDDeclaredDivergences are the outcomes T7c changes, keyed "shape id",
// as [the frozen door's, the door's] (the PR body declares each). A
// configured rig named like the city whose store did not open: the frozen
// door looked its prefix up in BeadStores(), which lists the CITY under that
// name, so it routed a tx- id to the city alone and answered 404 for a bead
// another rig holds. The door finds no loaded leg for the prefix, scans, and
// serves it.
var byIDDeclaredDivergences = map[string][2]string{
	"an unopened rig named like the city tx-9": {"error: bead tx-9 not found", `"rig:other" holds tx-9`},
}

// TestByIDResolutionMatchesTheFrozenDoor runs the whole by-id door, old and
// new, through ResolveOwnerRow and the residual's own read, on every shape,
// gate on and off, for an id on every arm: the frozen topology and router
// beside Stores().Topology() and the ref-routed axis. Each must hand its
// handler the same owner (ref and physical store) and row, or the same error,
// but for the declared divergences.
// A worker site that refuses every class fails closed for by-id reads on both
// sides (the binding leg is minted over the refusing store).
func TestByIDResolutionMatchesTheFrozenDoor(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for name, st := range byIDExecShapes(t) {
				s := New(st)
				for _, id := range byIDExecIDs {
					want, got := referenceResolveBeadOwner(t, s, id), resolveBeadOwnerOutcome(s, id)
					if diverged, ok := byIDDeclaredDivergences[name+" "+id]; ok {
						if got.String() != diverged[1] || want.String() != diverged[0] {
							t.Errorf("%s, %s: the door resolves %s and the frozen door %s, want the declared %s and %s", name, id, got, want, diverged[1], diverged[0])
						}
						continue
					}
					if got.String() != want.String() {
						t.Errorf("%s, %s: the door resolves %s, the frozen door %s", name, id, got, want)
						continue
					}
					if want.err == "" && !beads.SameStore(got.store, want.store) {
						t.Errorf("%s, %s: the owner reads %v, the frozen door's %v", name, id, got.store, want.store)
					}
				}
			}
		})
	}
}

// TestWorkerSiteRefusesByIDOnBothSides is the fully refused worker site's
// by-id half, executed (SHOULD-1 of the T7c review). The binding leg is
// minted over the refusing store, so an id inside the binding's namespace is
// refused, and a relic the binding holds under a work-shaped id is not served
// from it (its residence probe meets the standing refusal and is tolerated):
// as the frozen door did, whose binding leg was the refusing class handle.
// A work bead is still served: a refused city still serves work.
func TestWorkerSiteRefusesByIDOnBothSides(t *testing.T) {
	s := New(byIDExecShapes(t)["a worker site refusing every class"])
	for id, want := range map[string]string{"gcg-1": standingTestRefusal{}.Error(), "tc-relic": "bead tc-relic not found"} {
		before, got := referenceResolveBeadOwner(t, s, id), resolveBeadOwnerOutcome(s, id)
		if !strings.Contains(got.err, want) || got.err != before.err {
			t.Errorf("%s: the door resolves %s, the frozen door %s; want both %q", id, got, before, want)
		}
	}
	if got := resolveBeadOwnerOutcome(s, "tc-1"); got.err != "" || got.ref != storeref.WorkRef {
		t.Errorf("a work bead on a refused worker site resolves %s, want the city work store", got)
	}
}

// TestByIDWorkAxisRoutesOverThePlannedEpoch pins that the work axis routes
// over the epoch the plan is planned over, not a later Stores(): a holder that
// re-mints between the plan and the route would otherwise hand the plan refs
// it did not open, or routes it cannot read.
func TestByIDWorkAxisRoutesOverThePlannedEpoch(t *testing.T) {
	st, _, _, _ := byIDExecBase(t)
	// A later epoch with another city store and no rig at all: a route read
	// from it would not route mr-1 to myrig.
	later := newFakeState(t)
	later.cityBeadStore = beads.NewMemStore()
	later.stores = map[string]beads.Store{}
	s := New(&epochFlipState{fakeState: st, first: st.Stores(), later: later.Stores()})
	if got := resolveBeadOwnerOutcome(s, "mr-1"); got.err != "" || got.ref != storeref.RigRef("myrig") {
		t.Fatalf("mr-1 resolves %s, want rig:myrig of the epoch the plan was planned over", got)
	}
}

// epochFlipState answers its first Stores() with first and every later one
// with later, as a holder that re-minted in between would.
type epochFlipState struct {
	*fakeState
	first, later storeref.Stores
	calls        int
}

func (s *epochFlipState) Stores() storeref.Stores {
	s.calls++
	if s.calls == 1 {
		return s.first
	}
	return s.later
}

// standingRefusingStore refuses every operation the by-id and create doors
// make with a standing refusal, as cmd/gc's refusedClassStore does.
type standingRefusingStore struct{ beads.Store }

func (standingRefusingStore) StorageRefusal() error { return standingTestRefusal{} }

func (standingRefusingStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, standingTestRefusal{}
}

func (standingRefusingStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, standingTestRefusal{}
}
