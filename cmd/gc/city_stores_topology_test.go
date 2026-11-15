package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// with the same work prefix, on a single-store city, a whole split before and
// after its census cleared it, a refused city, a worker site whose plane
// refuses every relocated class (the topology still names class:gmnos, and
// fails closed: it agrees with the planes carrying that refusal), a worker
// site that attaches only the graph class (refused per class, not city-wide),
// and a whole split with a suspended rig (kept, marked, read by no plan). Each rig's relic-free bit is the one the API reads
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

	shapes := []string{"single store", "whole split", "whole split, censused clean", "refused", "worker site, whole split", "worker site, graph attached", "whole split, a rig suspended"}
	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			resetControlPlaneForTest(t)
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
			case "worker site, whole split":
				// A worker site whose descriptor attaches no binding: the plane
				// refuses every relocated class per class (classRefusal).
				writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
			case "worker site, graph attached":
				// The descriptor attaches the graph class to the binding: the
				// plane refuses the other relocated classes only, and the
				// topology is not refused.
				writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{splitEnvBinding: {"graph"}}))
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
			serving := rigs
			minted := map[string]storeref.Stores{"controllerState": cs.Stores()}
			if shape == "whole split, a rig suspended" {
				// The controller plane is told the serving rigs; the epoch keeps
				// the suspended rig, marked, and no plan reads it. A standalone
				// runtime publishes no suspension set.
				cs.setSuspendedRigs(map[string]bool{"busy": true})
				serving = map[string]beads.Store{"packs": rigs["packs"]}
				minted["controllerState"] = cs.Stores()
			} else {
				minted["standalone runtime"] = cr.cityStores()
			}
			if shape == "worker site, graph attached" {
				_, graphRefused := beads.As[storeref.RefusingStore](cs.Stores().Graph())
				_, sessionsRefused := beads.As[storeref.RefusingStore](cs.Stores().Sessions())
				if graphRefused || !sessionsRefused {
					t.Fatalf("fixture: graph refused %v, sessions refused %v; want only sessions", graphRefused, sessionsRefused)
				}
			}
			if shape == "worker site, whole split" {
				for name, handle := range map[string]beads.Store{"graph": cs.Stores().Graph(), "sessions": cs.Stores().Sessions(), "mail": cs.Stores().Mail(), "orders": cs.Stores().Orders(), "nudges": cs.Stores().Nudges()} {
					if _, refused := beads.As[storeref.RefusingStore](handle); !refused {
						t.Fatalf("fixture: the worker site's %s handle does not refuse", name)
					}
				}
			}

			registerResidencyRoutes(cityPath, routes, nil)
			t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
			planes := map[string]storeref.Topology{
				"controller":       cr.residencyTopology(serving),
				"registered spine": residencyTopologyForCity(cityPath, cfg, work, serving),
			}
			for holder, stores := range minted {
				topo := stores.Topology()
				if shape == "worker site, whole split" {
					// Every relocated class's handle refuses, so the topology
					// fails closed, as the one-shot funnel's routes do on a
					// worker site; its refs stay the routes' (class:gmnos).
					if topo.Refused == nil || fmt.Sprint(topo.ClaimRefs()) != "[ class:gmnos]" {
						t.Errorf("%s: refused %v, claim refs %v; want refused, [ class:gmnos]", holder, topo.Refused, topo.ClaimRefs())
					}
					for plane, want := range planes {
						want.Refused = topo.Refused
						planes[plane] = want
					}
				}
				for plane, want := range planes {
					assertTopologiesPlanAlike(t, holder+" vs "+plane, topo, want)
					if topo.Work.Prefix != want.Work.Prefix {
						t.Errorf("%s vs %s: work prefix %q, plane %q", holder, plane, topo.Work.Prefix, want.Work.Prefix)
					}
				}
				if (topo.Refused != nil) != (shape == "refused" || shape == "worker site, whole split") {
					t.Errorf("%s: refused = %v; only the refused city and the worker site are refused", holder, topo.Refused)
				}
				for _, leg := range topo.Rigs {
					name, _ := storeref.ScopeRigContext(string(leg.Ref))
					if leg.RelicFree != cs.RigGraphRelicFree(name) {
						t.Errorf("%s: rig %s relic-free %v, the API's RigGraphRelicFree says %v", holder, name, leg.RelicFree, cs.RigGraphRelicFree(name))
					}
				}
			}
			wantPacksFree := shape != "single store" && shape != "refused"
			got := minted["controllerState"].Topology()
			if len(got.Rigs) != 2 || got.Rigs[1].Ref != storeref.RigRef("packs") || got.Rigs[1].RelicFree != wantPacksFree {
				t.Errorf("rig legs = %+v, want packs relic-free = %v", got.Rigs, wantPacksFree)
			}
			if busy := got.Rigs[0]; busy.Suspended != (shape == "whole split, a rig suspended") {
				t.Errorf("rig busy suspended = %v in shape %q", busy.Suspended, shape)
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

// workflowPlan is the epoch's Workflow plan, as a corpus row.
func workflowPlan(t *testing.T, cs *controllerState) string {
	t.Helper()
	p, err := cs.Stores().Plan(storeref.Workflow{SuspendedRigs: storeref.WorkflowReadsSuspendedRigs})
	if err != nil {
		t.Fatalf("Plan(Workflow): %v", err)
	}
	return p.String()
}

// TestStoresRelicLegsFollowTheCensusContract (R4, S5-G): the relic legs are
// per epoch. A rig proven relic-free has none; once its record no longer
// speaks for it (S5-G --to-rigs renames the records) the next epoch reads it
// as a relic leg again; once the routes serve no binding (--to-work) no relic
// role remains. A record that changes within an epoch is not seen until the
// epoch re-mints, and a rig-generation bump (gc rig add, a reload) re-mints.
func TestStoresRelicLegsFollowTheCensusContract(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	routes, cityPath, rigDirs := relicLegRoutes(t, true)
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}, Rigs: []config.Rig{{Name: "packs", Path: rigDirs["packs"]}, {Name: "busy", Path: rigDirs["busy"]}}}
	cs := &controllerState{
		cityName: "c", cityPath: cityPath, cfg: cfg, cityBeadStore: beads.NewMemStore(), storageRoutes: routes,
		beadStores: map[string]beads.Store{"packs": beads.NewMemStore(), "busy": beads.NewMemStore()},
	}
	cs.rigStoresGen++
	bump := func() {
		cs.mu.Lock()
		cs.beadStores = map[string]beads.Store{"packs": cs.beadStores["packs"], "busy": cs.beadStores["busy"]}
		cs.rigStoresGen++
		cs.mu.Unlock()
	}
	clean := filepath.Join(routes.rigMarkers.bindingRoot, rigGraphCleanMarkerName("packs"))

	const proven = `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:busy[Relic,Fatal,relic]`
	const relic = `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:busy[Relic,Fatal,relic] > rig:packs[Relic,Fatal,relic]`
	if got := workflowPlan(t, cs); got != proven {
		t.Fatalf("packs proven relic-free:\n got  %s\n want %s", got, proven)
	}

	// --to-rigs: the records are renamed, so packs is no longer proven.
	if err := os.Rename(clean, clean+".rolled-back"); err != nil {
		t.Fatal(err)
	}
	if got := workflowPlan(t, cs); got != proven {
		t.Errorf("within the epoch the census is not re-read:\n got  %s\n want %s", got, proven)
	}
	bump()
	if got := workflowPlan(t, cs); got != relic {
		t.Errorf("after the re-mint the relic leg is back:\n got  %s\n want %s", got, relic)
	}

	// A record written again is picked up by the next re-mint.
	if err := os.Rename(clean+".rolled-back", clean); err != nil {
		t.Fatal(err)
	}
	bump()
	if got := workflowPlan(t, cs); got != proven {
		t.Errorf("a rig-generation re-mint picks up the record again:\n got  %s\n want %s", got, proven)
	}

	// --to-work: the routes serve no binding, so no relic role remains.
	unsplit := &storageRoutes{rigMarkers: routes.rigMarkers}
	cs.mu.Lock()
	cs.storageRoutes = unsplit
	cs.mu.Unlock()
	const none = `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:busy[FederationTail,Fatal,federated] > rig:packs[FederationTail,Fatal,federated]`
	if got := workflowPlan(t, cs); got != none {
		t.Errorf("after --to-work:\n got  %s\n want %s", got, none)
	}
}

// TestStoresEpochSurvivesAnUnchangedSuspensionSet: the city runtime
// republishes the suspension set every tick; an unchanged set keeps the
// epoch, and a changed one re-mints it with the rig marked.
func TestStoresEpochSurvivesAnUnchangedSuspensionSet(t *testing.T) {
	cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cityBeadStore: beads.NewMemStore(), beadStores: map[string]beads.Store{"r": beads.NewMemStore()}}
	cs.rigStoresGen++
	cs.setSuspendedRigs(map[string]bool{"r": false})
	_ = cs.Stores()
	epoch := cs.stores.current.Load()
	cs.setSuspendedRigs(map[string]bool{})
	if _ = cs.Stores(); cs.stores.current.Load() != epoch {
		t.Error("republishing an unchanged suspension set re-minted the epoch")
	}
	cs.setSuspendedRigs(map[string]bool{"r": true})
	if topo := cs.Stores().Topology(); cs.stores.current.Load() == epoch || len(topo.Rigs) != 1 || !topo.Rigs[0].Suspended {
		t.Errorf("a changed suspension set: rigs %+v; want a re-minted epoch with r suspended", topo.Rigs)
	}
}

// TestCityRuntimeStoresCarryTheStateSuspension: a runtime with a controller
// state mints through the state's epoch (storesOver), suspension included:
// cr.cityStores() marks the suspended rig, a plan that skips suspended rigs
// leaves it out, and
// alternating cs.Stores() and cr.cityStores() share one epoch.
func TestCityRuntimeStoresCarryTheStateSuspension(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = nil
	t.Cleanup(func() { splitStrictSeam = saved })

	routes := splitRoutes(beads.NewMemStore())
	cityPath := t.TempDir()
	cs := &controllerState{
		cityName: "c", cityPath: cityPath, cityBeadStore: beads.NewMemStore(), storageRoutes: routes,
		beadStores: map[string]beads.Store{"live": beads.NewMemStore(), "parked": beads.NewMemStore()},
	}
	cs.rigStoresGen++
	cs.setSuspendedRigs(map[string]bool{"parked": true})
	cr := &CityRuntime{cityName: "c", cityPath: cityPath, cs: cs, storageRoutes: routes}

	topo := cr.cityStores().Topology()
	if len(topo.Rigs) != 2 || topo.Rigs[0].Ref != storeref.RigRef("live") || topo.Rigs[0].Suspended || !topo.Rigs[1].Suspended {
		t.Fatalf("runtime topology rigs = %+v; want live serving, parked suspended", topo.Rigs)
	}
	// A plan that skips suspended rigs leaves parked out. (Molecule{Roots}
	// takes its work legs from the census by ref since T7b.1, so a Workflow
	// reader that skips suspended rigs shows the epoch's suspension here.)
	plan, err := cr.cityStores().Plan(storeref.Workflow{SuspendedRigs: storeref.WorkflowSkipsSuspendedRigs})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.String(); !strings.Contains(got, "rig:live[") || strings.Contains(got, "rig:parked") {
		t.Errorf("a suspension-skipping plan through the runtime = %s; want rig:live read and rig:parked left out", got)
	}
	epoch := cs.stores.current.Load()
	for range 3 {
		_ = cs.Stores()
		_ = cr.cityStores()
	}
	if cs.stores.current.Load() != epoch {
		t.Error("alternating cs.Stores() and cr.cityStores() re-minted the epoch")
	}
}
