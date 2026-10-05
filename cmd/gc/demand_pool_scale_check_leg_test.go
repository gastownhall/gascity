package main

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The POOL-DEMAND half of reader agreement (#6019).
//
// residency_leg_agreement_test.go asserts the census and the claim reader agree
// about which stores exist. The default pool-demand probe is a third reader: it
// is the one that mints capacity, and it used to read one store — the leading
// handle the controller passes, which on a converged split IS the binding. A
// routed bead in the city work ledger was then served to any seat that started
// (`gc ready` federates Plan(RoutedWork)) and counted by nobody, so a pool whose
// queue was work-class never demand-spawned.

// poolDemandForEnv builds the default scale-check targets exactly as the
// controller's agent loop does (leading = the sessions-class store) and counts
// them the way the demand pass does.
func poolDemandForEnv(t *testing.T, e legAgreementEnv) (demandTargets, map[string]int, map[string]scaleCheckDemand, map[string]bool, []error) {
	t.Helper()
	targets := buildDemandTargets(e.cityName, e.cityPath, e.cfg, e.leading(), e.rigs, nil, nil, controllerQueryRuntimeEnv, io.Discard)
	counts, demand, partials, errs := defaultScaleCheckCountsAndDemand(e.cfg, targets.defaultScaleTargets, newReadyDemandCache())
	return targets, counts, demand, partials, errs
}

func seedRoutedWork(t *testing.T, store beads.Store, target, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
	})
	if err != nil {
		t.Fatalf("seed routed work: %v", err)
	}
	return b
}

// claimableRoutedIDs is what a seat for target is served: `gc ready` over the
// claim reader's legs.
func claimableRoutedIDs(t *testing.T, e legAgreementEnv, target string) map[string]bool {
	t.Helper()
	legs, err := readyFederationLegsOverBinding(e.cityName, e.work, e.rigs, e.binding)
	if err != nil {
		t.Fatalf("readyFederationLegsOverBinding: %v", err)
	}
	rows, err := readyBeadsForOpts(legs, readyOpts{
		unassigned:     true,
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=" + target},
	})
	if err != nil {
		t.Fatalf("gc ready over the claim legs: %v", err)
	}
	out := map[string]bool{}
	for _, row := range rows {
		out[row.ID] = true
	}
	return out
}

func assertPoolDemandMatchesClaim(t *testing.T, e legAgreementEnv, template string, seeded []beads.Bead) {
	t.Helper()
	claimable := claimableRoutedIDs(t, e, template)
	for _, b := range seeded {
		if !claimable[b.ID] {
			t.Fatalf("fixture: seeded routed bead %s is not claimable; the claim reader does not see it", b.ID)
		}
	}
	_, counts, demand, partials, errs := poolDemandForEnv(t, e)
	if len(errs) != 0 {
		t.Fatalf("pool demand probe errors: %v", errs)
	}
	if len(partials) != 0 {
		t.Fatalf("pool demand probe partial templates: %v", partials)
	}
	counted := map[string]bool{}
	for _, id := range demand[template].WorkBeadIDs {
		counted[id] = true
	}
	for _, b := range seeded {
		if !counted[b.ID] {
			t.Errorf("routed bead %s (%q) is CLAIMABLE but uncounted: the pool demand probe does not read the leg it lives in, so no seat is ever spawned to take it", b.ID, b.Title)
		}
	}
	if got, want := counts[template], len(claimable); got != want {
		t.Fatalf("pool demand counted %d of %d claimable routed rows for %s", got, want, template)
	}
}

func TestPoolScaleCheckCountsRoutedWorkInTheCityWorkLedger(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	seeded := []beads.Bead{
		seedRoutedWork(t, e.work, "worker", "work-ledger row"),
		seedRoutedWork(t, e.binding, "worker", "binding row"),
	}
	assertPoolDemandMatchesClaim(t, e, "worker", seeded)
}

func TestRigPoolScaleCheckCountsRoutedWorkInTheCityWorkLedger(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	e.cfg.Agents = []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	seeded := []beads.Bead{
		seedRoutedWork(t, e.rigs["alpha"], "alpha/worker", "rig row"),
		seedRoutedWork(t, e.work, "alpha/worker", "work-ledger row"),
		seedRoutedWork(t, e.binding, "alpha/worker", "binding row"),
	}
	assertPoolDemandMatchesClaim(t, e, "alpha/worker", seeded)
}

// A bead co-resident in the work ledger and the binding (a migrated city:
// `gc storage migrate` preserves ids) is ONE unit of demand, and the copy that
// answers is the work ledger's — first leg wins, as in the claim reader.
func TestPoolScaleCheckCoResidentRowCountsOnceFirstLegWins(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	// Both stores honor a pinned id so the same row can live in each.
	e.work.(*beads.MemStore).HonorExplicitIDs = true
	e.binding.(*beads.MemStore).HonorExplicitIDs = true
	for store, title := range map[beads.Store]string{e.work: "work copy", e.binding: "binding copy"} {
		if _, err := store.Create(beads.Bead{
			ID:       "gc-co-1",
			Title:    title,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		}); err != nil {
			t.Fatalf("seed co-resident copy: %v", err)
		}
	}

	for i := 0; i < 20; i++ { // group iteration must not be map-order dependent
		_, counts, demand, _, errs := poolDemandForEnv(t, e)
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if counts["worker"] != 1 {
			t.Fatalf("co-resident row counted %d times, want 1", counts["worker"])
		}
		if got := demand["worker"].Titles["gc-co-1"]; got != "work copy" {
			t.Fatalf("co-resident row answered from %q, want the work ledger's copy (first leg wins, as the claim reader)", got)
		}
	}
}

// Single-store cities take none of this: the probe target is exactly the one
// store it always was, so no extra read lands on the tick.
func TestPoolScaleCheckSingleStoreCityKeepsOneTarget(t *testing.T) {
	e := newLegAgreementEnv(t, false)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	seeded := []beads.Bead{seedRoutedWork(t, e.work, "worker", "work row")}
	targets, _, _, _, _ := poolDemandForEnv(t, e)
	if len(targets.defaultScaleTargets) != 1 {
		t.Fatalf("single-store city got %d default targets, want 1: %+v", len(targets.defaultScaleTargets), targets.defaultScaleTargets)
	}
	if tg := targets.defaultScaleTargets[0]; tg.store != e.work || tg.storeKey != "city" {
		t.Fatalf("single-store target = %+v, want the city store under key city", tg)
	}
	assertPoolDemandMatchesClaim(t, e, "worker", seeded)
}

// darkLedgerReadyStore is a work ledger whose ready read fails outright.
type darkLedgerReadyStore struct{ beads.Store }

func (darkLedgerReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, errDarkLeg{}
}

// A dark city leg — the work ledger or the binding — is a leg the probe could
// not read, so the count is not authoritative: the pool and its named-backing
// template go partial (retain, don't drain) instead of reporting the readable
// leg's rows as the whole answer. Both legs count under "city", so this is
// also the guard that they are two reads and not one merged group.
func TestPoolScaleCheckDarkCityLegMarksPartial(t *testing.T) {
	for _, darkBinding := range []bool{false, true} {
		name := "dark-work-ledger"
		if darkBinding {
			name = "dark-binding"
		}
		t.Run(name, func(t *testing.T) {
			e := newLegAgreementEnv(t, false)
			work := beads.NewMemStoreFrom(1, nil, nil)
			binding := beads.NewMemStoreFrom(500000, nil, nil)
			readable := beads.Store(binding)
			e.work, e.binding = darkLedgerReadyStore{Store: work}, binding
			if darkBinding {
				e.work, e.binding, readable = work, darkLedgerReadyStore{Store: binding}, work
			}
			routes := splitRoutes(e.binding)
			registerResidencyRoutes(e.cityPath, routes, func() beads.Store { return e.work })
			t.Cleanup(func() { unregisterResidencyRoutes(e.cityPath, routes) })
			e.cfg.Agents = []config.Agent{
				{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)},
				{Name: "helper", StartCommand: "true", MaxActiveSessions: intPtr(1)},
			}
			e.cfg.NamedSessions = []config.NamedSession{{Template: "helper", Mode: "on_demand"}}
			seedRoutedWork(t, readable, "worker", "readable row")

			targets, counts, _, partials, errs := poolDemandForEnv(t, e)
			if len(errs) == 0 || !partials["worker"] {
				t.Fatalf("errs=%v partials=%v, want an error and worker partial", errs, partials)
			}
			if counts["worker"] != 1 {
				t.Fatalf("readable row counted %d, want 1 (the readable leg still counts)", counts["worker"])
			}
			_, namedPartials, namedErrs := defaultNamedSessionDemand(targets.defaultNamedScaleTargets, e.cfg, e.cityPath, newReadyDemandCache())
			if len(namedErrs) == 0 || !namedPartials["helper"] {
				t.Fatalf("named errs=%v partials=%v, want helper partial", namedErrs, namedPartials)
			}
		})
	}
}

// A refused city has no routed-work plan. The probe still reads the leading
// handle, but the plan error is not dropped: the city-scope pool is reported
// partial (retain, don't drain) and the error reaches the demand pass.
func TestPoolScaleCheckRefusedCityIsPartialNotSilent(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	resetCLIResidencyBindings()
	t.Cleanup(resetCLIResidencyBindings)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() {
		entry.routes = refusingStorageRoutes("infra", errStorageRefusedForTest{})
	})
	cfg := residencyTestConfig()
	cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	store := beads.NewMemStore()
	seedRoutedWork(t, store, "worker", "leading row")

	targets := buildDemandTargets(cfg.Workspace.Name, cityPath, cfg, store, nil, nil, nil, controllerQueryRuntimeEnv, io.Discard)
	counts, _, partials, errs := defaultScaleCheckCountsAndDemand(cfg, targets.defaultScaleTargets, newReadyDemandCache())
	if len(errs) == 0 || !partials["worker"] {
		t.Fatalf("refused city: errs=%v partials=%v, want the plan error and worker partial", errs, partials)
	}
	if counts["worker"] != 1 {
		t.Fatalf("leading row counted %d, want 1 (the leading handle is still read)", counts["worker"])
	}
}

// The RELIC case, pinned honestly: a migrated city that still holds a frozen
// work-store copy of a relocated bead — ledger copy open, binding copy closed
// under the same id (gc 1.5.0's "source retained" migration, #5987).
//
// The ledger leg is first, so the open frozen copy wins and the probe counts 1:
// phantom demand for work the binding already closed. This is NOT a divergence
// between the readers — `gc ready` serves the same frozen copy, and that is
// asserted below — it is the retained-copy data defect itself. The probe does
// not paper over it (a binding-first count would disagree with the claim reader,
// the D6 shape). What keeps it out of production is #7074: `gc storage migrate`
// clears the retained copies after proof, and boot refuses a city that still
// holds them (storage.binding.unconverged, outcome retained-copies). This change
// must therefore merge strictly after #7074. If this test starts counting 0,
// the leg order or the dedupe changed — re-check agreement with `gc ready`
// before updating it.
func TestPoolScaleCheckRetainedFrozenCopyCountsFirstLegWins(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}}
	e.work.(*beads.MemStore).HonorExplicitIDs = true
	e.binding.(*beads.MemStore).HonorExplicitIDs = true
	for _, store := range []*beads.MemStore{e.work.(*beads.MemStore), e.binding.(*beads.MemStore)} {
		if _, err := store.Create(beads.Bead{
			ID:       "gc-relic-1",
			Title:    "relocated step",
			Type:     "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		}); err != nil {
			t.Fatalf("seed relic copy: %v", err)
		}
	}
	closed := "closed"
	if err := e.binding.Update("gc-relic-1", beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close binding copy: %v", err)
	}

	_, counts, demand, partials, errs := poolDemandForEnv(t, e)
	if len(errs) != 0 || len(partials) != 0 {
		t.Fatalf("errs=%v partials=%v", errs, partials)
	}
	if counts["worker"] != 1 || demand["worker"].WorkBeadIDs[0] != "gc-relic-1" {
		t.Fatalf("relic: counted %d (%v), want 1 — the ledger's open frozen copy, first leg wins", counts["worker"], demand["worker"].WorkBeadIDs)
	}
	if claimable := claimableRoutedIDs(t, e, "worker"); !claimable["gc-relic-1"] || len(claimable) != 1 {
		t.Fatalf("claim reader served %v; the probe must agree with it on the relic (both serve the frozen copy until #7074 clears it)", claimable)
	}
}
