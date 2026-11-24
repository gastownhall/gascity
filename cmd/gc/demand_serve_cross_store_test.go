package main

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/runtime"
)

// demandLegWrappings are the shapes a work leg reaches the reader in: the bare
// engine, the bead-policy wrapper every store cmd/gc opens sits behind
// (main.go), and the controller's cache under that wrapper (api_state.go). Each
// forwards the absent-blocker listing, so a test over the bare engine alone
// cannot see a reader that mistakes a forwarded listing for a stranding store
// (#8360 review, Finding 1).
var demandLegWrappings = []struct {
	name string
	wrap func(beads.Store, *config.City) beads.Store
}{
	{"bare engine", func(s beads.Store, _ *config.City) beads.Store { return s }},
	{"policy-wrapped", wrapStoreWithBeadPolicies},
	{"cached and policy-wrapped", func(s beads.Store, cfg *config.City) beads.Store {
		return wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(s, nil), cfg)
	}},
}

// TestControllerDemandAgreesWithServeOnCrossStoreBlockers is the demand half of
// the owner decision: pool demand applies the SAME cross-store rule as
// `gc ready`, so the rows the controller counts as demand and the rows a seat
// is served are the same rows. It runs on a split city with a real bd/Dolt work
// store and rig, in every wrapping production puts them behind, and a real
// SQLite binding, over the three edge shapes that differ by backend: an
// external:<id> ref, a cross-rig blocker and a dangling literal. The gate is
// armed as production arms it: handed the sessions store, which on a split
// city is the binding.
func TestControllerDemandAgreesWithServeOnCrossStoreBlockers(t *testing.T) {
	for _, wrapping := range demandLegWrappings {
		t.Run(wrapping.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city", Prefix: "gc"},
				Rigs:      []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: "rg"}},
				Storage:   splitEnvStorageConfig(),
			}
			workEngine := splittest.NewNativeWorkStore(t, "gc")
			rigEngine := splittest.NewNativeWorkStore(t, "rg")
			work := wrapping.wrap(workEngine, cfg)
			rig := wrapping.wrap(rigEngine, cfg)
			binding := newSplitEnvClassLeaf(t)
			routes := splitEnvRoutes(binding)
			registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
			t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
			rigStores := map[string]beads.Store{"rg": rig}

			last := 4
			step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task", Priority: &last})
			if err != nil {
				t.Fatal(err)
			}
			rigBlocker, err := rigEngine.Create(beads.Bead{Title: "rig blocker", Type: "task", Priority: &last})
			if err != nil {
				t.Fatal(err)
			}
			newRow := func(title, target string) beads.Bead {
				t.Helper()
				row, err := workEngine.Create(beads.Bead{Title: title, Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				if target != "" {
					if err := workEngine.DepAdd(row.ID, target, "blocks"); err != nil {
						t.Fatalf("DepAdd(%s -> %s): %v", row.ID, target, err)
					}
				}
				return row
			}
			extRow := newRow("external id dep", "external:"+step.ID)
			rigRow := newRow("cross-rig dep", rigBlocker.ID)
			danglingRow := newRow("dangling dep", "discovered-from:gc-404")
			capRow := newRow("capability dep", "external:rg:some-capability")
			clean := newRow("clean", "")

			legs, err := readyLegsForTopology(residencyTopologyForCity(cityPath, cfg, work, rigStores))
			if err != nil {
				t.Fatal(err)
			}
			if !readywork.IsSplit(legs) {
				t.Fatal("fixture: the legs do not read as a split city")
			}
			agree := func(phase string) (demand []string) {
				t.Helper()
				cache := newReadyDemandCache().withCrossStoreGate(cityPath, cfg, binding, rigStores, nil)
				for _, leg := range legs {
					rows, err := cache.controllerDemandReady(leg.Store, leg.Label)
					if err != nil {
						t.Fatalf("%s: demand ready on %s: %v", phase, leg.Label, err)
					}
					for _, b := range rows {
						demand = append(demand, b.ID)
					}
				}
				served, err := readywork.ReadContext(context.Background(), legs, readywork.Query{})
				if err != nil {
					t.Fatal(err)
				}
				var serve []string
				for _, r := range served {
					serve = append(serve, r.ID)
				}
				slices.Sort(demand)
				slices.Sort(serve)
				demand = slices.Compact(demand)
				if !slices.Equal(demand, serve) {
					t.Fatalf("%s: demand counts %v, serve answers %v: the controller would spawn for work no seat is served, or starve work a seat would be", phase, demand, serve)
				}
				return demand
			}

			before := agree("blockers open")
			for _, id := range []string{extRow.ID, rigRow.ID, danglingRow.ID} {
				if slices.Contains(before, id) {
					t.Errorf("blockers open: %s is counted and served while its blocker holds it", id)
				}
			}
			for _, id := range []string{capRow.ID, clean.ID} {
				if !slices.Contains(before, id) {
					t.Errorf("blockers open: %s is neither counted nor served; nothing holds it", id)
				}
			}
			if err := binding.Close(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := rigEngine.Close(rigBlocker.ID); err != nil {
				t.Fatal(err)
			}
			after := agree("blockers closed")
			for _, id := range []string{extRow.ID, rigRow.ID} {
				if !slices.Contains(after, id) {
					t.Errorf("blockers closed: %s is still held", id)
				}
			}
			if slices.Contains(after, danglingRow.ID) {
				t.Errorf("blockers closed: %s, whose blocker no store holds, is counted and served", danglingRow.ID)
			}
		})
	}
}

// TestSplitCityDemandIsGatedWhenHandedTheSessionsBinding drives both demand
// passes the way the controller calls them (#8360 review, Finding 2): every
// caller hands them the sessions store, which on a split city is the binding.
// A gate planned over that store found no binding leg and never armed, so
// demand counted a routed row that a cross-store blocker holds, which no seat is
// served, and missed a binding row whose work blocker had closed, which a seat
// is served.
func TestSplitCityDemandIsGatedWhenHandedTheSessionsBinding(t *testing.T) {
	cityPath := t.TempDir()
	maxSessions, minSessions := 5, 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc", Prefix: "gc"},
		Storage:   splitEnvStorageConfig(),
		Agents:    []config.Agent{{Name: "dog", StartCommand: "true", MinActiveSessions: &minSessions, MaxActiveSessions: &maxSessions}},
	}
	workEngine := splittest.NewNativeWorkStore(t, "gc")
	work := wrapStoreWithBeadPolicies(workEngine, cfg)
	binding := newSplitEnvClassLeaf(t)
	routes := splitEnvRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	routed := map[string]string{"gc.routed_to": "dog"}
	step, err := binding.Create(beads.Bead{Title: "graph blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	held, err := workEngine.Create(beads.Bead{Title: "held by an open graph step", Type: "task", Metadata: routed})
	if err != nil {
		t.Fatal(err)
	}
	if err := workEngine.DepAdd(held.ID, "external:"+step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	clean, err := workEngine.Create(beads.Bead{Title: "clean", Type: "task", Metadata: routed})
	if err != nil {
		t.Fatal(err)
	}
	workBlocker, err := workEngine.Create(beads.Bead{Title: "work blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := binding.Create(beads.Bead{Title: "graph row behind a work blocker", Type: "task", Metadata: routed})
	if err != nil {
		t.Fatal(err)
	}
	// The cross-store edge a split city's binding strands a row behind is the
	// shape under test, so the strict leaf's record of it is claimed here.
	if err := binding.DepAdd(stranded.ID, workBlocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if got := splittest.TakeResidenceViolations(binding); len(got) != 1 {
		t.Fatalf("the binding recorded %d residence violation(s) for the one cross-store edge, want 1: %+v", len(got), got)
	}

	// demand returns the rows the v2 gather counts for dog, after checking the
	// legacy pass counts as many.
	demand := func(phase string) []string {
		t.Helper()
		view, err := gatherDemand(demandGatherEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: binding}, legacyDemandReads{})
		if err != nil {
			t.Fatalf("%s: gatherDemand: %v", phase, err)
		}
		ids := slices.Clone(view.Collected.DefaultDemand["dog"].WorkBeadIDs)
		slices.Sort(ids)
		if got := view.Collected.DefaultCounts["dog"]; got != len(ids) {
			t.Fatalf("%s: v2 DefaultCounts[dog] = %d beside demand rows %v", phase, got, ids)
		}
		legacy := buildDesiredStateWithSessionBeads("gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), binding, nil, nil, nil, io.Discard)
		if got := legacy.ScaleCheckCounts["dog"]; got != len(ids) {
			t.Fatalf("%s: legacy ScaleCheckCounts[dog] = %d, v2 counts %v: the passes disagree", phase, got, ids)
		}
		return ids
	}
	want := func(ids ...string) []string {
		slices.Sort(ids)
		return ids
	}

	if got := demand("blockers open"); !slices.Equal(got, want(clean.ID)) {
		t.Fatalf("blockers open: demand counts %v, want only %s: %s waits on open graph step %s and %s on open work bead %s", got, clean.ID, held.ID, step.ID, stranded.ID, workBlocker.ID)
	}
	if err := workEngine.Close(workBlocker.ID); err != nil {
		t.Fatal(err)
	}
	if got := demand("work blocker closed"); !slices.Equal(got, want(clean.ID, stranded.ID)) {
		t.Fatalf("work blocker closed: demand counts %v, want %s and the released binding row %s", got, clean.ID, stranded.ID)
	}
	if err := binding.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if got := demand("graph blocker closed"); !slices.Equal(got, want(clean.ID, stranded.ID, held.ID)) {
		t.Fatalf("graph blocker closed: demand counts %v, want %s, %s and %s", got, clean.ID, stranded.ID, held.ID)
	}
}
