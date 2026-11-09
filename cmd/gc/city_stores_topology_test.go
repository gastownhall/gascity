package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// topologyAgreementIntents are the residency questions the planes must answer
// alike, over a corpus of ids: inside the binding's namespace, inside the nudge
// queue's, work-shaped, and inside each rig's configured prefix.
func topologyAgreementIntents() map[string]storeref.Intent {
	out := map[string]storeref.Intent{
		"RoutedWork":                     storeref.RoutedWork{},
		"AssignedWork(sweep)":            storeref.AssignedWork{},
		"AssignedWork(claim-escalation)": storeref.AssignedWork{Purpose: storeref.AssignedWorkClaimEscalation},
		"Session":                        storeref.Session{},
		"Census(all)":                    storeref.Census{},
		"Census(graph)":                  storeref.Census{Classes: []coordclass.Class{coordclass.ClassGraph}},
		"Class(graph)":                   storeref.Class{C: coordclass.ClassGraph},
		"Class(sessions)":                storeref.Class{C: coordclass.ClassSessions},
	}
	for _, id := range []string{"gcg-1", "gcnq-1-q", "tc-1", "pk-1", "bz-1", "zz-1"} {
		out["ByID("+id+")"] = storeref.ByID{ID: id}
	}
	return out
}

// assertTopologiesPlanAlike fails when any agreement intent plans differently
// (refs, roles, policies and order) over got than over want, or when the two
// disagree about prefix faults or claim refs.
func assertTopologiesPlanAlike(t *testing.T, label string, got, want storeref.Topology) {
	t.Helper()
	render := func(i storeref.Intent, topo storeref.Topology) string {
		p, err := storeref.Plan(i, topo)
		if err != nil {
			return "error: " + err.Error()
		}
		return p.String()
	}
	for name, i := range topologyAgreementIntents() {
		if g, w := render(i, got), render(i, want); g != w {
			t.Errorf("%s, %s:\n Stores.Topology() %s\n plane             %s", label, name, g, w)
		}
	}
	if g, w := fmt.Sprint(got.PrefixFaults()), fmt.Sprint(want.PrefixFaults()); g != w {
		t.Errorf("%s: prefix faults %s, plane %s", label, g, w)
	}
	if g, w := fmt.Sprint(got.ClaimRefs()), fmt.Sprint(want.ClaimRefs()); g != w {
		t.Errorf("%s: claim refs %s, plane %s", label, g, w)
	}
}

// TestStoresTopologyAgreesWithTheControllerPlanes: the topology an epoch was
// minted as (Stores.Topology, from the controller's state and the standalone
// runtime) plans every residency question as the controller's own constructor
// (residencyTopology) and the registered spine (residencyTopologyForCity) do,
// on a single-store city, a whole split before and after its census cleared
// it, and a refused city. Each rig's relic-free bit is the one the API reads
// (RigGraphRelicFree).
//
// The split-strict gate's projection is off here: the planes build from the
// routes and see no class view, while the epoch's handles do, so under the
// gate the epoch keeps a split shape the planes cannot (the point of planning
// T7 federations over handles). TestStoresTopologyUnderTheSeamKeepsTheSplit
// pins that side.
func TestStoresTopologyAgreesWithTheControllerPlanes(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	for _, shape := range []string{"single store", "whole split", "whole split, censused clean", "refused"} {
		t.Run(shape, func(t *testing.T) {
			routes, cityPath, rigDirs := relicLegRoutes(t, shape != "single store" && shape != "refused")
			switch shape {
			case "single store":
				routes = nil
			case "refused":
				refused := refusingStorageRoutes("infra", errors.New("refused: run gc storage migrate"))
				refused.rigMarkers = routes.rigMarkers
				routes = refused
			case "whole split, censused clean":
				binding, _ := routes.storeFor(coordclass.ClassGraph)
				routes.relics = map[beads.Store]bool{binding: false}
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city", Prefix: "tc"},
				Rigs: []config.Rig{
					{Name: "packs", Path: rigDirs["packs"], Prefix: "pk"},
					{Name: "busy", Path: rigDirs["busy"], Prefix: "bz"},
				},
			}
			work := beads.NewMemStore()
			rigs := map[string]beads.Store{"packs": beads.NewMemStore(), "busy": beads.NewMemStore()}
			cs := &controllerState{cityName: "test-city", cityPath: cityPath, cfg: cfg, cityBeadStore: work, storageRoutes: routes, beadStores: rigs}
			cs.rigStoresGen++
			cr := &CityRuntime{cityName: "test-city", cityPath: cityPath, cfg: cfg, storageRoutes: routes}
			cr.setStandaloneStores(work, rigs)

			registerResidencyRoutes(cityPath, routes, nil)
			t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
			planes := map[string]storeref.Topology{
				"controller":       cr.residencyTopology(rigs),
				"registered spine": residencyTopologyForCity(cityPath, cfg, work, rigs),
			}
			minted := map[string]storeref.Stores{"controllerState": cs.Stores(), "standalone runtime": cr.cityStores()}
			for holder, stores := range minted {
				topo := stores.Topology()
				for plane, want := range planes {
					assertTopologiesPlanAlike(t, holder+" vs "+plane, topo, want)
				}
				for _, leg := range topo.Rigs {
					name, _ := storeref.ScopeRigContext(string(leg.Ref))
					if leg.RelicFree != cs.RigGraphRelicFree(name) {
						t.Errorf("%s: rig %s relic-free %v, the API's RigGraphRelicFree says %v", holder, name, leg.RelicFree, cs.RigGraphRelicFree(name))
					}
				}
			}
			wantPacksFree := shape == "whole split" || shape == "whole split, censused clean"
			if got := minted["controllerState"].Topology(); len(got.Rigs) != 2 || got.Rigs[1].Ref != storeref.RigRef("packs") || got.Rigs[1].RelicFree != wantPacksFree {
				t.Errorf("rig legs = %+v, want packs relic-free = %v", got.Rigs, wantPacksFree)
			}
		})
	}
}

// TestStoresTopologyAgreesWithTheCLIPlane: on a real migrated one-shot city,
// the CLI's handles (cliStores) were minted as the topology the CLI's own
// constructor (cliResidencyTopology) builds from the same funnel.
func TestStoresTopologyAgreesWithTheCLIPlane(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	work := beads.NewMemStore()
	topo := cliStores(cityPath, work).Topology()
	if topo.IsSingleStore() {
		t.Fatal("the migrated city's CLI handles were minted single-store")
	}
	assertTopologiesPlanAlike(t, "cliStores vs cliResidencyTopology", topo, cliResidencyTopology(cityPath, cfg, work, nil))
}

// TestStoresTopologyUnderTheSeamKeepsTheSplit: under the split-strict seam a
// single-store city's handles read a class view, so the epoch's topology names
// that view as the whole split's binding while the controller's plane, built
// from the routes, sees none. This is the difference T7's handle-planned
// federations rely on: a gate fixture keeps the split branch.
func TestStoresTopologyUnderTheSeamKeepsTheSplit(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = projectionSeam{}
	t.Cleanup(func() { splitStrictSeam = saved })

	cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cityBeadStore: beads.NewMemStore()}
	topo := cs.Stores().Topology()
	if len(topo.Bindings) != 1 || topo.Bindings[0].Leg.Ref != "class:gmnos" {
		t.Fatalf("bindings under the seam = %+v, want the class view as class:gmnos", topo.Bindings)
	}
	if !beads.SameStore(topo.Bindings[0].Leg.Store, cs.Stores().Graph()) || !beads.SameStore(topo.Work.Store, cs.Stores().Work()) {
		t.Error("the seam topology's legs are not the handles' stores")
	}
	cr := &CityRuntime{cityName: "c", standaloneCityStore: cs.cityBeadStore}
	if plane := cr.residencyTopology(nil); !plane.IsSingleStore() {
		t.Errorf("the controller plane saw a binding under the seam: %+v", plane.Bindings)
	}
}

// projectionSeam is the split-strict gate's own projection as the seam, on
// whether or not the test binary runs with the gate: its views are memoized
// per store and side, as the gate's are.
type projectionSeam struct{}

func (projectionSeam) WorkView(s beads.Store) beads.Store {
	return splittest.ProjectionOf(s).WorkViewOf(s)
}

func (projectionSeam) ClassView(s beads.Store) beads.Store {
	return splittest.ProjectionOf(s).ClassViewOf(s)
}

// TestStoresEpochTopologyNeverTears (R10): readers race reloads that swap the
// city store, the rig set and the config together under cs.mu; every Stores a
// reader gets has a topology whose work leg, rig legs and prefixes come from
// one reload, and whose handles are that topology's stores.
func TestStoresEpochTopologyNeverTears(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	type reload struct {
		city beads.Store
		rig  beads.Store
		cfg  *config.City
	}
	gens := map[beads.Store]int{}
	var gensMu sync.Mutex
	next := func(i int) reload {
		r := reload{
			city: beads.NewMemStore(),
			rig:  beads.NewMemStore(),
			cfg:  &config.City{Workspace: config.Workspace{Name: "c", Prefix: fmt.Sprintf("p%d", i)}, Rigs: []config.Rig{{Name: "r", Path: "/tmp/r", Prefix: fmt.Sprintf("r%d", i)}}},
		}
		gensMu.Lock()
		gens[r.city], gens[r.rig] = i, i
		gensMu.Unlock()
		return r
	}
	first := next(0)
	cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cityBeadStore: first.city, cfg: first.cfg, beadStores: map[string]beads.Store{"r": first.rig}, storageRoutes: splitRoutes(beads.NewMemStore())}
	cs.rigStoresGen++

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var torn sync.Once
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := cs.Stores()
				topo := s.Topology()
				if len(topo.Rigs) != 1 {
					torn.Do(func() { t.Errorf("a topology with %d rig legs", len(topo.Rigs)) })
					continue
				}
				gensMu.Lock()
				cityGen, rigGen := gens[topo.Work.Store], gens[topo.Rigs[0].Store]
				gensMu.Unlock()
				wantWork, wantRig := fmt.Sprintf("p%d", cityGen), fmt.Sprintf("r%d", cityGen)
				if cityGen != rigGen || topo.Work.Prefix != wantWork || topo.Rigs[0].Prefix != wantRig {
					torn.Do(func() {
						t.Errorf("a torn epoch: city from reload %d (prefix %q), rig from reload %d (prefix %q)", cityGen, topo.Work.Prefix, rigGen, topo.Rigs[0].Prefix)
					})
				}
				if !beads.SameStore(topo.Work.Store, s.Work()) || len(topo.Bindings) != 1 || !beads.SameStore(topo.Bindings[0].Leg.Store, s.Graph()) {
					torn.Do(func() { t.Error("a topology whose legs are not its handles' stores") })
				}
			}
		}()
	}
	for i := 1; i <= 2000; i++ {
		r := next(i)
		cs.mu.Lock()
		cs.cityBeadStore, cs.cfg = r.city, r.cfg
		cs.beadStores = map[string]beads.Store{"r": r.rig}
		cs.rigStoresGen++
		cs.mu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// TestStoresEpochFollowsAConfigOnlyReload: a reload that swaps only the config
// (the store-reuse path) re-mints the epoch, so the topology's prefixes are
// the new config's.
func TestStoresEpochFollowsAConfigOnlyReload(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	cfgWith := func(prefix string) *config.City {
		return &config.City{Workspace: config.Workspace{Name: "c", Prefix: prefix}, Rigs: []config.Rig{{Name: "r", Path: "/tmp/r", Prefix: prefix + "r"}}}
	}
	cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cfg: cfgWith("aa"), cityBeadStore: beads.NewMemStore(), beadStores: map[string]beads.Store{"r": beads.NewMemStore()}}
	cs.rigStoresGen++
	if got := cs.Stores().Topology(); got.Work.Prefix != "aa" || got.Rigs[0].Prefix != "aar" {
		t.Fatalf("first epoch prefixes = %q, %q", got.Work.Prefix, got.Rigs[0].Prefix)
	}
	cs.mu.Lock()
	cs.cfg = cfgWith("bb")
	cs.mu.Unlock()
	if got := cs.Stores().Topology(); got.Work.Prefix != "bb" || got.Rigs[0].Prefix != "bbr" {
		t.Errorf("after a config-only reload the prefixes are %q, %q; want the new config's", got.Work.Prefix, got.Rigs[0].Prefix)
	}
}
