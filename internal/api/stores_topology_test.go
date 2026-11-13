package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
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
// agreement intent other than the API plane's retired residencyTopology
// (frozen in residency_reference_test.go) over st did: refs, roles, policies
// and order.
func assertAPITopologyAgrees(t *testing.T, label string, st State) {
	t.Helper()
	minted, plane := st.Stores().Topology(), referenceResidencyTopology(New(st))
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
	if minted.Work.Prefix != plane.Work.Prefix {
		t.Errorf("%s: work prefix %q, the API plane %q", label, minted.Work.Prefix, plane.Work.Prefix)
	}
	if (minted.Refused != nil) != (plane.Refused != nil) {
		t.Errorf("%s: refused %v, the API plane %v", label, minted.Refused, plane.Refused)
	}
	planeRigs := map[storeref.StoreRef]string{}
	for _, leg := range plane.Rigs {
		planeRigs[leg.Ref] = leg.Prefix
	}
	for _, leg := range minted.Rigs {
		if want, ok := planeRigs[leg.Ref]; ok && leg.Prefix != want {
			t.Errorf("%s: rig %s prefix %q, the API plane %q", label, leg.Ref, leg.Prefix, want)
		}
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
// carry the same topology the API plane derived from the same State before
// T7c (the frozen residencyTopology), so the API, which reads
// Stores().Topology() since, sees what it saw, in both gate modes. Under the gate a single-store
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
// shape (only the whole split is served). T7c retired the API's own
// derivation for Stores().Topology(), so the API now names it class:mnos too:
// the one behavior change the switch makes outside a per-class refusal.
func assertTheAPIBlindSpot(t *testing.T, st State) {
	t.Helper()
	refs := func(topo storeref.Topology) string { return fmt.Sprint(topo.ClaimRefs()) }
	if got, want := refs(st.Stores().Topology()), "[ class:g class:mnos]"; got != want {
		t.Errorf("graph only under the gate: minted claim refs %s, want %s", got, want)
	}
	if got, want := refs(referenceResidencyTopology(New(st))), "[ class:g class:nos]"; got != want {
		t.Errorf("graph only under the gate: API plane claim refs %s, want %s", got, want)
	}
}

// TestStoresHoldsIsTheAPIHoldsRule: Stores.Holds, asked by the owner's ref,
// answers what the API's PATCH door asked of a store before T7c (the frozen
// storeClassHolds),
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
					got, want := st.Stores().Holds(ref), referenceStoreClassHolds(srv, store)
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

// refusingFakeStore stands in for cmd/gc's refusedClassStore: a store that
// answers every operation with a standing refusal.
type refusingFakeStore struct{ beads.Store }

func (refusingFakeStore) StorageRefusal() error {
	return errors.New("refused: this worker site serves no binding")
}

// TestWorkerSiteTopologyFailsClosedOnBothSides: a worker site whose every
// relocated class's handle refuses. The minted topology fails closed and keeps
// the binding's ref, and the API plane, which read the one refusing store
// behind every class handle as the whole split's binding, was refused too: the
// two agree on every plan, and, executed, on every by-id read. A render
// compares refs only, so the reads are run: an id inside the binding's
// namespace and a co-resident id (in the binding and the city) reach the
// minted binding leg, which is the refusing store (storeref's
// bindingRefusal), exactly as they reached the plane's.
func TestWorkerSiteTopologyFailsClosedOnBothSides(t *testing.T) {
	st := fakeTopologyShapes(t)["whole split"]
	city := beads.NewMemStore()
	seedPinned(t, city, "tc-co")
	st.cityBeadStore = city
	binding, ok := st.graphBeadStore.(prefixDeclaringStore)
	if !ok {
		t.Fatalf("fixture: the binding is %T", st.graphBeadStore)
	}
	const inside = "gcg-1"
	seedPinned(t, binding.Store.(*beads.MemStore), inside, "tc-co")

	refusal := standingRefusingStore{Store: beads.NewMemStore()}
	st.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{}
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			st.classRefusals[c] = refusal
		}
	}
	minted := st.Stores().Topology()
	if minted.Refused == nil || fmt.Sprint(minted.ClaimRefs()) != "[ class:gmnos]" {
		t.Errorf("minted topology: refused %v, claim refs %v; want refused, [ class:gmnos]", minted.Refused, minted.ClaimRefs())
	}
	assertAPITopologyAgrees(t, "worker site", st)

	plane := referenceResidencyTopology(New(st))
	resolve := func(topo storeref.Topology, id string) string {
		plan, err := storeref.Plan(storeref.ByID{ID: id}, topo)
		if err != nil {
			return "plan error: " + err.Error()
		}
		owner, err := storeref.ResolveOwnerRow(plan, id)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("%q read=%t", owner.Ref, owner.Read)
	}
	for _, id := range []string{inside, "tc-co"} {
		got, want := resolve(minted, id), resolve(plane, id)
		if got != want {
			t.Errorf("%s: minted resolves %s, the API plane %s", id, got, want)
		}
		if id == inside && !strings.Contains(got, standingTestRefusal{}.Error()) {
			t.Errorf("%s inside the refused binding's namespace resolves %s, want the refusal", id, got)
		}
	}
}

// TestAPIPlaneTurnsAPerClassRefusalCityWide pins the one known divergence a
// refusal of SOME relocated classes opens (a refusal of every one fails closed
// on both sides: TestWorkerSiteTopologyFailsClosedOnBothSides). The minted topology follows
// the routes: the binding is class:gmnos, the topology is not refused, and
// the refusal stays with the refused class's handle. The API plane derives its
// bindings from the handles, so it reads the refusing graph handle as a
// binding of its own and the city as refused: every plan that touches a
// binding failed there. T7c's switch to Stores().Topology() changed that:
// the topology is not refused, and only the refused class's handle refuses
// (TestPerClassRefusalRefusesOnlyThatClassAfterT7c). No serving city has this
// shape: the worker site refuses every relocated class or none.
func TestAPIPlaneTurnsAPerClassRefusalCityWide(t *testing.T) {
	st := fakeTopologyShapes(t)["whole split"]
	st.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{coordclass.ClassGraph: refusingFakeStore{Store: beads.NewMemStore()}}
	if _, refused := beads.As[storeref.RefusingStore](st.Stores().Graph()); !refused {
		t.Fatal("fixture: the graph handle does not refuse")
	}
	minted, plane := st.Stores().Topology(), referenceResidencyTopology(New(st))
	if got := fmt.Sprint(minted.ClaimRefs()); got != "[ class:gmnos]" || minted.Refused != nil {
		t.Errorf("minted topology: claim refs %s, refused %v; want [ class:gmnos], not refused", got, minted.Refused)
	}
	if got := fmt.Sprint(plane.ClaimRefs()); got != "[ class:g class:nos]" || plane.Refused == nil {
		t.Errorf("API plane: claim refs %s, refused %v; want [ class:g class:nos], refused", got, plane.Refused)
	}
}

// refusingOpsStore is refusingFakeStore whose create and by-id read refuse,
// as cmd/gc's refusedClassStore refuses every operation.
type refusingOpsStore struct{ refusingFakeStore }

func (s refusingOpsStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, s.StorageRefusal()
}

func (s refusingOpsStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.StorageRefusal() }

// TestPerClassRefusalRefusesOnlyThatClassAfterT7c pins the behavior change
// T7c's switch to Stores().Topology() makes on a per-class refusal (a worker
// site refusing SOME relocated classes; no serving city has this shape, since
// a worker site refuses every relocated class or none, and that fails closed
// on both sides). Before, the API read the refusing graph handle as a binding
// of its own and refused the whole city: every by-id read and every
// infrastructure create that touched a binding failed. Now the topology
// follows the routes and is not refused: a by-id read of a binding resident
// resolves through the binding's leg, a create of the refused class is refused
// by that class's handle (the same 400, with or without a rig), and a create
// of a class the site still serves lands in the binding.
func TestPerClassRefusalRefusesOnlyThatClassAfterT7c(t *testing.T) {
	st := fakeTopologyShapes(t)["whole split"]
	binding := st.graphBeadStore
	resident, err := binding.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": "gcg-0"}})
	if err != nil {
		t.Fatal(err)
	}
	st.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{coordclass.ClassGraph: refusingOpsStore{refusingFakeStore{Store: beads.NewMemStore()}}}
	srv := New(st)

	// Before: the frozen API plane read the id's binding leg through the
	// refusing graph handle, and refused the read.
	before, err := storeref.Plan(storeref.ByID{ID: resident.ID}, referenceResidencyTopology(srv))
	if err == nil {
		_, err = storeref.ResolveOwnerRow(before, resident.ID)
	}
	if err == nil {
		t.Fatal("fixture: the frozen API plane served a by-id read of the binding")
	}

	owner, got, err := srv.resolveBeadOwner(resident.ID)
	if err != nil || got.ID != resident.ID || !beads.SameStore(owner, binding) {
		t.Fatalf("by-id read of a binding resident = %v, %q, %v; want the binding's row", owner, got.ID, err)
	}

	// Refused at placement, as the 400 the frozen plane's refused plan gave,
	// rig or no rig.
	for _, rig := range []string{"", "myrig"} {
		_, err := srv.createStoreForBead(beads.Bead{Title: "step", Metadata: map[string]string{"gc.root_bead_id": "gcg-0"}}, rig)
		if status := apiStatus(err); status != http.StatusBadRequest || !strings.Contains(err.Error(), "refused: this worker site serves no binding") {
			t.Fatalf("rig %q: a graph create on a site that refuses graph = %v (status %d), want its refusal as a 400", rig, err, status)
		}
	}

	session := beads.Bead{Title: "s", Type: "session"}
	if coordclass.Classify(session) != coordclass.ClassSessions {
		t.Fatalf("fixture: a session bead classifies as %s", coordclass.Classify(session))
	}
	sessions, err := srv.createStoreForBead(session, "")
	if err != nil {
		t.Fatalf("placing a sessions create: %v", err)
	}
	if bs, ok := sessions.(beads.Store); !ok || !beads.SameStore(bs, binding) {
		t.Fatalf("a sessions create is placed at %v, want the binding", sessions)
	}
}

// TestCreatePlacesEachClassThroughItsOwnHandle pins createStoreForBead's
// class-to-handle map: on a whole split whose site refuses one class, a
// create of that class is refused by its handle and a create of every other
// infrastructure class is served from the binding.
func TestCreatePlacesEachClassThroughItsOwnHandle(t *testing.T) {
	bodies := map[coordclass.Class]beads.Bead{
		coordclass.ClassGraph:     {Title: "g", Labels: []string{"gc:wisp"}},
		coordclass.ClassMessaging: {Title: "m", Type: "message"},
		coordclass.ClassSessions:  {Title: "s", Type: "session"},
		coordclass.ClassOrders:    {Title: "o", Labels: []string{"order-tracking"}},
		coordclass.ClassNudges:    {Title: "n", Labels: []string{"gc:nudge"}},
	}
	for class, body := range bodies {
		if got := coordclass.Classify(body); got != class {
			t.Fatalf("fixture: the %s body classifies as %s", class, got)
		}
	}
	for refused := range bodies {
		st := fakeTopologyShapes(t)["whole split"]
		st.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{refused: refusingOpsStore{refusingFakeStore{Store: beads.NewMemStore()}}}
		srv := New(st)
		for class, body := range bodies {
			store, err := srv.createStoreForBead(body, "")
			if class == refused {
				if apiStatus(err) != http.StatusBadRequest {
					t.Errorf("refusing %s: placing a %s create = %v, want the refusal as a 400", refused, class, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("refusing %s: placing a %s create: %v", refused, class, err)
			}
			if _, err := store.Create(body); err != nil {
				t.Errorf("refusing %s: a %s create returned %v", refused, class, err)
			}
		}
	}
}

// apiStatus is err's HTTP status, or 0 for none.
func apiStatus(err error) int {
	var status huma.StatusError
	if errors.As(err, &status) {
		return status.GetStatus()
	}
	return 0
}
