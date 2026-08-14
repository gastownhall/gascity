package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The LEG-SET half of reader agreement.
//
// demand_serve_agreement_test.go pins the ROW half: one eligibility semantics in
// two representations, asserted row by row. That corpus is blind to the question
// this file asks, because a row can only be counted or served by a reader that
// READS THE STORE IT LIVES IN. Every divergence in the D1–D9 family was
// leg-shaped, not row-shaped: the demand read's city leg was the binding while
// the claim read's was the work store, and the HQ work store was in neither (D6).
//
// After S3 both sides resolve their legs from one topology through the resolver
// — the controller census through Plan(Census), the reader the hook claims
// through (`gc ready`) through Plan(RoutedWork) — so the sets agree by
// construction. This asserts that construction, and fails DIFFERENTLY from the
// row corpus: a lost leg here is a set mismatch and a seeded bead no reader
// serves, while a mis-ordered plan is a golden diff in
// internal/storeref's corpus and nothing here.

// legAgreementEnv is one city's opened stores, in the shape production hands
// them to the two readers.
type legAgreementEnv struct {
	cityPath string
	cityName string
	cfg      *config.City
	work     beads.Store
	binding  beads.Store
	rigs     map[string]beads.Store
}

func newLegAgreementEnv(t *testing.T, split bool, rigNames ...string) legAgreementEnv {
	t.Helper()
	e := legAgreementEnv{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg:      &config.City{Workspace: config.Workspace{Name: "test-city"}},
		work:     beads.NewMemStoreFrom(1, nil, nil),
		rigs:     map[string]beads.Store{},
	}
	for i, name := range rigNames {
		e.cfg.Rigs = append(e.cfg.Rigs, config.Rig{Name: name, Path: t.TempDir()})
		// Distinct id sequences per store: two MemStores both minting "gc-1"
		// would look like one co-resident bead to a first-leg-wins fold.
		e.rigs[name] = beads.NewMemStoreFrom(1000*(i+1), nil, nil)
	}
	if split {
		e.binding = beads.NewMemStoreFrom(500000, nil, nil)
		routes := splitRoutes(e.binding)
		registerResidencyRoutes(e.cityPath, routes, func() beads.Store { return e.work })
		t.Cleanup(func() { unregisterResidencyRoutes(e.cityPath, routes) })
	}
	return e
}

// leading is the store production hands the census arms: the sessions-class
// store, which on a converged split IS the binding.
func (e legAgreementEnv) leading() beads.Store {
	if e.binding != nil {
		return e.binding
	}
	return e.work
}

// allLegs is every store this city can hold routed work in, which is what both
// readers must cover between them.
func (e legAgreementEnv) allLegs() []beads.Store {
	legs := []beads.Store{e.work}
	for _, rig := range e.cfg.Rigs {
		legs = append(legs, e.rigs[rig.Name])
	}
	if e.binding != nil {
		legs = append(legs, e.binding)
	}
	return legs
}

func (e legAgreementEnv) demandLegs(t *testing.T) []beads.Store {
	t.Helper()
	candidates, err := censusStoreCandidates(e.cityPath, e.cfg, e.leading(), e.rigs, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	out := make([]beads.Store, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.store)
	}
	return out
}

func (e legAgreementEnv) claimLegs(t *testing.T) []beads.Store {
	t.Helper()
	legs, err := readyFederationLegsOverBinding(e.cityName, e.work, e.rigs, e.binding)
	if err != nil {
		t.Fatalf("readyFederationLegsOverBinding: %v", err)
	}
	out := make([]beads.Store, 0, len(legs))
	for _, l := range legs {
		out = append(out, l.store)
	}
	return out
}

func sameStoreSequence(a, b []beads.Store) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDemandAndClaimReadTheSameLegsInTheSameOrder is the property. It runs over
// the topologies the resolver's own corpus names: single-store, whole split,
// whole split with rigs.
func TestDemandAndClaimReadTheSameLegsInTheSameOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		split bool
		rigs  []string
	}{
		{name: "T0-single-store", split: false},
		{name: "T0-single-store-with-rigs", split: false, rigs: []string{"alpha", "bravo"}},
		{name: "T1-whole-split", split: true},
		{name: "T2-whole-split-with-rigs", split: true, rigs: []string{"alpha", "bravo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLegAgreementEnv(t, tc.split, tc.rigs...)
			demand, claim := e.demandLegs(t), e.claimLegs(t)
			if !sameStoreSequence(demand, claim) {
				t.Fatalf("demand reads %d legs and the claim reader %d, or they disagree about the order. That is the D1-D9 divergence class: a bead in a leg only one side reads is either spawned-for-and-undrainable or claimable-and-uncounted",
					len(demand), len(claim))
			}
			if !sameStoreSequence(demand, e.allLegs()) {
				t.Fatalf("the agreed leg set is not every store this city holds routed work in (%d legs vs %d). Agreeing on a SMALLER set is agreement, and it is the failure this property exists to catch",
					len(demand), len(e.allLegs()))
			}
		})
	}
}

// TestEveryLegsRoutedWorkIsBothCountedAndClaimable is the same property stated
// over rows: seed one open routed bead into EVERY leg, and require the demand
// side and the claim reader to see the same set.
func TestEveryLegsRoutedWorkIsBothCountedAndClaimable(t *testing.T) {
	const target = "worker"
	e := newLegAgreementEnv(t, true, "alpha", "bravo")

	seeded := map[string]bool{}
	for _, store := range e.allLegs() {
		bead, err := store.Create(beads.Bead{
			Title:    "routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
		})
		if err != nil {
			t.Fatalf("seed routed work: %v", err)
		}
		seeded[bead.ID] = true
	}

	counted := map[string]bool{}
	for _, store := range e.demandLegs(t) {
		rows, err := store.List(beads.ListQuery{Status: "open", TierMode: beads.FederatedReadTier})
		if err != nil {
			t.Fatalf("demand leg List: %v", err)
		}
		for _, row := range rows {
			if seeded[row.ID] {
				counted[row.ID] = true
			}
		}
	}

	claimable := map[string]bool{}
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
	for _, row := range rows {
		if seeded[row.ID] {
			claimable[row.ID] = true
		}
	}

	for id := range seeded {
		if !counted[id] {
			t.Errorf("routed bead %s is CLAIMABLE but uncounted: the controller never mints demand for it, so no seat is ever spawned to take it", id)
		}
		if !claimable[id] {
			t.Errorf("routed bead %s is COUNTED but unclaimable: a seat spawns for it, finds nothing, and drains — the spawn/idle treadmill", id)
		}
	}
	if len(counted) != len(seeded) || len(claimable) != len(seeded) {
		t.Fatalf("counted %d, claimable %d, seeded %d", len(counted), len(claimable), len(seeded))
	}

	// CONTROL, fixture rot: a bead in NO leg is in neither set, so the counts
	// above are not both satisfied by a reader that returns everything.
	orphan := beads.NewMemStoreFrom(900000, nil, nil)
	stray, err := orphan.Create(beads.Bead{
		Title: "unreachable", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
	})
	if err != nil {
		t.Fatalf("seed stray: %v", err)
	}
	if counted[stray.ID] || claimable[stray.ID] {
		t.Fatal("a bead in no leg was reported by a reader; the fixture is not exercising the legs")
	}
}

// TestDroppingALegBreaksAgreementAndNotTheRowCorpus is the differently-failing
// control. A leg removed from ONE side leaves every row's eligibility semantics
// untouched — the row corpus stays green — and shows up here as a set mismatch
// plus a bead one reader cannot see.
func TestDroppingALegBreaksAgreementAndNotTheRowCorpus(t *testing.T) {
	e := newLegAgreementEnv(t, true, "alpha")
	demand := e.demandLegs(t)
	if len(demand) < 2 {
		t.Fatalf("the fixture resolved %d legs; the control needs one to drop", len(demand))
	}
	mutated := demand[:len(demand)-1] // drop the binding, the D5/ga-whzrt shape

	if sameStoreSequence(mutated, e.claimLegs(t)) {
		t.Fatal("dropping a leg left the two readers agreeing; this control proves nothing")
	}
	// And the dropped leg really was carrying reachable work, so the mismatch is
	// a lost answer rather than a lost empty.
	if _, err := e.binding.Create(beads.Bead{Title: "binding-only", Type: "task", Status: "open"}); err != nil {
		t.Fatalf("seed binding work: %v", err)
	}
	for _, store := range mutated {
		rows, err := store.List(beads.ListQuery{Status: "open", TierMode: beads.FederatedReadTier})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, row := range rows {
			if row.Title == "binding-only" {
				t.Fatal("the dropped leg's work is still reachable through the remaining legs; the fixture's stores are not distinct")
			}
		}
	}
}
