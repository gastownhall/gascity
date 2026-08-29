package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

func splitRoutes(store beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{}}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			routes.stores[class] = store
		}
	}
	return routes
}

func residencyTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/tmp/alpha", Prefix: "ra"},
			{Name: "bravo", Path: "/tmp/bravo", Prefix: "rb"},
		},
	}
}

// A city that relocates nothing gets a one-leg topology, and by-id resolution
// hands back the caller's own store with no probe at all.
func TestControllerResidencyTopologyIsSingleStoreWithoutRoutes(t *testing.T) {
	work := beads.NewMemStore()
	cr := &CityRuntime{cfg: residencyTestConfig(), standaloneCityStore: work}

	topo := cr.residencyTopology(cr.rigBeadStores())
	if !topo.IsSingleStore() {
		t.Fatalf("nil routes produced %d bindings, want none", len(topo.Bindings))
	}
	plan, err := storeref.Plan(storeref.ByID{ID: "ga-1"}, topo)
	if err != nil {
		t.Fatalf("Plan(ByID): %v", err)
	}
	if got, want := plan.String(), `FirstOwner: ""[WorkFallback,Fatal]`; got != want {
		t.Fatalf("ByID plan = %q, want %q", got, want)
	}
}

// The controller's whole-split topology: one binding for all five classes, the
// city work store as a DISTINCT leg beside it, and the rigs in name order with
// their configured prefixes.
func TestControllerResidencyTopologyBuildsFromOpenedRoutes(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	alpha := beads.NewMemStore()
	bravo := beads.NewMemStore()
	cr := &CityRuntime{
		cfg:                 residencyTestConfig(),
		standaloneCityStore: work,
		standaloneRigStores: map[string]beads.Store{"bravo": bravo, "alpha": alpha},
		storageRoutes:       splitRoutes(binding),
	}

	topo := cr.residencyTopology(cr.rigBeadStores())
	if len(topo.Bindings) != 1 {
		t.Fatalf("got %d bindings, want 1 — the whole split is ONE store", len(topo.Bindings))
	}
	if got, want := topo.Bindings[0].Leg.Ref, storeref.ClassRef(coordclass.Classes()[1:]); got != want {
		t.Fatalf("binding ref = %q, want %q", got, want)
	}
	if got := []storeref.StoreRef{topo.Rigs[0].Ref, topo.Rigs[1].Ref}; got[0] != storeref.RigRef("alpha") || got[1] != storeref.RigRef("bravo") {
		t.Fatalf("rig legs = %v, want alpha then bravo", got)
	}
	if topo.Rigs[0].Prefix != "ra" {
		t.Fatalf("rig alpha prefix = %q, want %q", topo.Rigs[0].Prefix, "ra")
	}

	// The census leg set is the D6 shape: the binding is beside the city work
	// store, never instead of it.
	census, err := storeref.Plan(storeref.Census{}, topo)
	if err != nil {
		t.Fatalf("Plan(Census): %v", err)
	}
	want := `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`
	if got := census.String(); got != want {
		t.Fatalf("Census plan =\n %s\nwant\n %s", got, want)
	}
}

// THE S0 CARRY-FORWARD. A suspended rig is routinely DARK, and a dark
// FederationTail leg makes every census result Partial — which means
// retain-don't-reap fleet-wide. The constructor is TOLD which rigs are serving,
// so the serving set never contains one.
//
// The control is the same topology built with the suspended rig INCLUDED: it
// must produce the leg, or the assertion below would pass on a constructor that
// simply lost all rig legs.
func TestControllerResidencyTopologyIsToldWhichRigsAreServing(t *testing.T) {
	cfg := residencyTestConfig()
	cr := &CityRuntime{
		cfg:                 cfg,
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"alpha": beads.NewMemStore(), "bravo": beads.NewMemStore()},
		storageRoutes:       splitRoutes(beads.NewMemStore()),
	}
	suspended := map[string]bool{filepath.Clean("/tmp/bravo"): true}

	serving := cr.residencyTopology(servingRigStores(cfg, cr.rigBeadStores(), suspended))
	census, err := storeref.Plan(storeref.Census{}, serving)
	if err != nil {
		t.Fatalf("Plan(Census): %v", err)
	}
	want := `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`
	if got := census.String(); got != want {
		t.Fatalf("census over the SERVING rigs =\n %s\nwant\n %s", got, want)
	}

	// Control: with no suspension frame the bravo leg is present, so the row
	// above is asserting exclusion rather than an empty rig set.
	all := cr.residencyTopology(servingRigStores(cfg, cr.rigBeadStores(), nil))
	control, err := storeref.Plan(storeref.Census{}, all)
	if err != nil {
		t.Fatalf("Plan(Census) over every rig: %v", err)
	}
	if !strings.Contains(control.String(), "rig:bravo") {
		t.Fatalf("the control lost the bravo leg too (%s); the exclusion assertion above proves nothing", control.String())
	}
}

// A refused city's routes serve every infrastructure class from a store that
// refuses, and the topology must carry that verdict rather than degrade.
func TestControllerResidencyTopologyCarriesTheStandingRefusal(t *testing.T) {
	refusal := standingStorageRefusal{err: errStorageRefusedForTest{}}
	cr := &CityRuntime{
		cfg:                 residencyTestConfig(),
		standaloneCityStore: beads.NewMemStore(),
		storageRoutes:       splitRoutes(refusedClassStore{err: refusal}),
	}

	topo := cr.residencyTopology(cr.rigBeadStores())
	if topo.Refused == nil {
		t.Fatal("a refusing binding produced a topology with no refusal")
	}
	if !storeref.IsStandingRefusal(topo.Refused) {
		t.Fatalf("the refusal is not recognized as a standing storage refusal: %v", topo.Refused)
	}
	if _, err := storeref.Plan(storeref.RoutedWork{}, topo); err == nil {
		t.Fatal("RoutedWork planned successfully over a refused topology — \"no work forever\"")
	}
	// The by-id carve-out survives: a refused city still serves WORK.
	plan, err := storeref.Plan(storeref.ByID{ID: "ga-1"}, topo)
	if err != nil {
		t.Fatalf("Plan(ByID) over a refused topology: %v", err)
	}
	if _, ref, err := storeref.ResolveOwner(plan, "ga-1"); err != nil || ref != storeref.WorkRef {
		t.Fatalf("ResolveOwner = (%q, %v), want the work leg with no error", ref, err)
	}
}

type errStorageRefusedForTest struct{}

func (errStorageRefusedForTest) Error() string {
	return "storage refused: run `gc storage migrate`"
}

// The one-shot CLI resolves its bindings once per city. The work and rig legs
// are the caller's own opened stores, so they are rebuilt; the binding
// derivation is not.
func TestCLIResidencyBindingsAreMemoizedPerCity(t *testing.T) {
	t.Cleanup(resetCLIResidencyBindings)
	resetCLIResidencyBindings()

	cityPath := t.TempDir()
	first, err := cliResidencyBindings(cityPath)
	if err != nil {
		t.Fatalf("cliResidencyBindings: %v", err)
	}
	second, err := cliResidencyBindings(filepath.Clean(cityPath))
	if err != nil {
		t.Fatalf("cliResidencyBindings (second): %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("memo returned %d bindings then %d", len(first), len(second))
	}
	// A city with no city.toml relocates nothing, so both answers are nil —
	// which is also the identity answer every default city gets.
	if len(first) != 0 {
		t.Fatalf("a city with no config produced %d bindings", len(first))
	}

	topo := cliResidencyTopology(cityPath, residencyTestConfig(), beads.NewMemStore(), nil)
	if !topo.IsSingleStore() {
		t.Fatalf("a city with no config produced %d bindings", len(topo.Bindings))
	}
}

// The tripwire, in ONE place: this build's constructors serve the whole split
// or nothing. Topology can EXPRESS a per-class fan-out and the corpus asserts
// the resolver's answers for it, but no constructor here may produce one until
// the runtime can serve it.
func TestTopologyConstructorsServeOnlyTheWholeSplit(t *testing.T) {
	binding := beads.NewMemStore()
	bindings, refused := residencyBindingsFromRoutes(splitRoutes(binding))
	if refused != nil {
		t.Fatalf("a healthy binding reported a refusal: %v", refused)
	}
	if len(bindings) != 1 {
		t.Fatalf("this build produced %d bindings; storage_boot admits a split only when all five infrastructure classes name ONE binding", len(bindings))
	}
	if len(bindings[0].Classes) != 5 {
		t.Fatalf("the binding carries %d classes, want all five", len(bindings[0].Classes))
	}
	if bindings[0].MintsReserved {
		t.Fatal("MintsReserved is set for a store that declares no mint prefix; the bit is an OBSERVATION and a store that names nothing has verified nothing")
	}
	if !bindings[0].HasLegacyResidents {
		t.Fatal("HasLegacyResidents is clear on routes the boot never censused; an unasked question is not a clean answer, and the probe would retire over every id the migration preserved")
	}
}

// The three planes must name one physical binding with ONE ref: the vocabulary
// is what a persisted census row and the wake filter that reads it back share.
func TestAllPlanesNameTheWholeSplitBindingIdentically(t *testing.T) {
	want := storeref.ClassRef([]coordclass.Class{
		coordclass.ClassGraph, coordclass.ClassMessaging, coordclass.ClassSessions,
		coordclass.ClassOrders, coordclass.ClassNudges,
	})
	bindings, _ := residencyBindingsFromRoutes(splitRoutes(beads.NewMemStore()))
	if len(bindings) != 1 || bindings[0].Leg.Ref != want {
		t.Fatalf("the cmd/gc planes name the whole-split binding %q, want %q", bindings[0].Leg.Ref, want)
	}
	// internal/api's half of the same assertion is
	// TestAPIResidencyTopologyGroupsTheWholeSplitIntoOneBinding, which pins the
	// same constant from the other plane.
}
