package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestFairPoolSessionCreateSharesReservesRecoveryBeforeFresh guards the
// recovery-before-fresh invariant end-to-end. applyNestedCaps sorts recovery
// ahead of fresh within a pool, but the per-tick create budget is what decides
// which pool spends a scarce token. Before the fix the allocator counted only
// Tier=="new" demand, so a pool whose sole request was a wake-known-identity
// recovery reserved nothing and an alphabetically-earlier pool's fresh work
// spent the only token.
func TestFairPoolSessionCreateSharesReservesRecoveryBeforeFresh(t *testing.T) {
	// "alpha" sorts first and has fresh work; "zulu" sorts last and needs a
	// replacement session for an identity whose session exited. With budget=1
	// the recovery must win for every seed.
	states := []PoolDesiredState{
		{Template: "alpha", Requests: []SessionRequest{{Template: "alpha", Tier: "new"}}},
		{Template: "zulu", Requests: []SessionRequest{{Template: "zulu", Tier: "wake-known-identity", WorkBeadID: "w-1"}}},
	}
	for seed := uint64(0); seed < 5; seed++ {
		shares, _ := fairPoolSessionCreateShares(states, 1, beads.PolicyPriorityFIFO, seed)
		if shares["zulu"] != 1 {
			t.Errorf("seed=%d: recovery pool zulu got %d budget, want 1 (recovery reserved before fresh)", seed, shares["zulu"])
		}
		if shares["alpha"] != 0 {
			t.Errorf("seed=%d: fresh alpha got %d budget, want 0 (budget consumed by recovery)", seed, shares["alpha"])
		}
	}
}

// TestFairPoolSessionCreateSharesOrdersFreshByPriorityBand guards cross-template
// priority ordering of the create budget. Before the fix the allocator rotated
// fresh tokens by template with no band awareness, so a P0 queue lost the only
// token to a P2 queue on every seed that rotated the P2 pool first.
func TestFairPoolSessionCreateSharesOrdersFreshByPriorityBand(t *testing.T) {
	p0, p2 := 0, 2
	states := []PoolDesiredState{
		{Template: "alpha", Requests: []SessionRequest{{Template: "alpha", Tier: "new", BeadPriority: &p2, WorkBeadID: "alpha-p2"}}},
		{Template: "zulu", Requests: []SessionRequest{{Template: "zulu", Tier: "new", BeadPriority: &p0, WorkBeadID: "zulu-p0"}}},
	}
	for seed := uint64(0); seed < 5; seed++ {
		shares, _ := fairPoolSessionCreateShares(states, 1, beads.PolicyPriorityFIFO, seed)
		if shares["zulu"] != 1 {
			t.Errorf("seed=%d: P0 pool zulu got %d budget, want 1 (urgent band wins across templates)", seed, shares["zulu"])
		}
		if shares["alpha"] != 0 {
			t.Errorf("seed=%d: P2 pool alpha got %d budget, want 0 (weaker band deferred)", seed, shares["alpha"])
		}
	}

	// A policy without priority bands must keep the pure seed-rotated
	// round-robin: band ordering here would contradict the admission order
	// applyNestedCaps uses for the same requests.
	rotated := map[string]bool{}
	for seed := uint64(0); seed < 4; seed++ {
		shares, _ := fairPoolSessionCreateShares(states, 1, beads.PolicyFIFO, seed)
		for template, n := range shares {
			if n > 0 {
				rotated[template] = true
			}
		}
	}
	if len(rotated) != 2 {
		t.Errorf("fifo policy funded %v, want both templates across seeds (no band ordering without bands)", rotated)
	}
}

// TestFairPoolSessionCreateSharesKeepsFloorAheadOfUrgentFresh guards #2893
// against the priority ordering added for #4322: a cold pool's
// min_active_sessions floor spawn is reserved before elastic demand competes,
// and that reservation must not be reordered by another pool's more urgent
// fresh work.
func TestFairPoolSessionCreateSharesKeepsFloorAheadOfUrgentFresh(t *testing.T) {
	p0 := 0
	// "alpha" carries three P0 elastic requests; "zulu" carries one floor
	// spawn with no work bead at all (P2 by default). The floor still wins.
	states := []PoolDesiredState{
		{Template: "alpha", Requests: []SessionRequest{
			{Template: "alpha", Tier: "new", BeadPriority: &p0, WorkBeadID: "alpha-1"},
			{Template: "alpha", Tier: "new", BeadPriority: &p0, WorkBeadID: "alpha-2"},
			{Template: "alpha", Tier: "new", BeadPriority: &p0, WorkBeadID: "alpha-3"},
		}},
		{Template: "zulu", Requests: []SessionRequest{{Template: "zulu", Tier: "new", FloorGuarantee: true}}},
	}
	for seed := uint64(0); seed < 5; seed++ {
		shares, _ := fairPoolSessionCreateShares(states, 1, beads.PolicyPriorityFIFO, seed)
		if shares["zulu"] != 1 {
			t.Errorf("seed=%d: floor pool zulu got %d budget, want 1 (floor reserved before urgent elastic)", seed, shares["zulu"])
		}
		if shares["alpha"] != 0 {
			t.Errorf("seed=%d: P0 elastic alpha got %d budget, want 0 (budget consumed by floor)", seed, shares["alpha"])
		}
	}
}

// TestBuildDesiredStateCreateBudgetAdmitsRecoveryOverFreshPool is the
// end-to-end form of the recovery reservation: pools are realized in
// alphabetical order, so without a tier-aware budget "alpha" spends the single
// per-tick create token on fresh scale-check demand and "zulu" never gets the
// replacement session its committed identity needs.
func TestBuildDesiredStateCreateBudgetAdmitsRecoveryOverFreshPool(t *testing.T) {
	maxWakes := 1
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "committed zulu work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "zulu",
		Metadata: map[string]string{"gc.routed_to": "zulu"},
	}); err != nil {
		t.Fatalf("create assigned work: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{
				Name:              "alpha",
				StartCommand:      "true",
				ScaleCheck:        "printf 5",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			},
			{
				Name:              "zulu",
				StartCommand:      "true",
				ScaleCheck:        "printf 0",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			},
		},
	}
	var stderr bytes.Buffer

	result := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)

	counts := map[string]int{}
	for _, tp := range result.State {
		counts[tp.TemplateName]++
	}
	if got := counts["zulu"]; got != 1 {
		t.Fatalf("zulu recovery sessions = %d, want 1 (recovery outranks alpha's fresh demand); counts=%v stderr=%q", got, counts, stderr.String())
	}
	if got := counts["alpha"]; got != 0 {
		t.Fatalf("alpha fresh creates = %d, want 0 (single token spent on recovery); counts=%v stderr=%q", got, counts, stderr.String())
	}
}

// TestDefaultScaleCheckDemandKeepsSameIDStoresApart covers a cold rig pool
// probing both its own rig store and the city store (the cross-store cold-wake
// probe): independent stores mint IDs independently, so both can hold a "dup".
// A demand entry keys its metadata by bare bead ID, so before the fix map
// iteration order decided which store's priority, created_at, title and store
// ref represented BOTH rows — a real P0 could be dispatched as the P2 it
// collided with, and its session could bind to the other store's work.
func TestDefaultScaleCheckDemandKeepsSameIDStoresApart(t *testing.T) {
	const (
		template = "svc/executor"
		sharedID = "dup"
	)
	p0, p2 := 0, 2
	cityCreated := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	rigCreated := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:        sharedID,
		Title:     "city urgent work",
		Type:      "task",
		Status:    "open",
		Priority:  &p0,
		CreatedAt: cityCreated,
		Metadata:  map[string]string{"gc.routed_to": template},
	}}, nil)
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:        sharedID,
		Title:     "rig routine work",
		Type:      "task",
		Status:    "open",
		Priority:  &p2,
		CreatedAt: rigCreated,
		Metadata:  map[string]string{"gc.routed_to": template},
	}}, nil)
	targets := []defaultScaleCheckTarget{
		{template: template, storeKey: "rig:svc", store: rigStore},
		{template: template, storeKey: "city", store: cityStore},
	}

	// Repeat: the pre-fix clobber is a map-iteration-order lottery, so a single
	// pass can pass by luck.
	for attempt := 0; attempt < 32; attempt++ {
		counts, demand, _, errs := defaultScaleCheckCountsAndDemand(targets, nil)
		if len(errs) != 0 {
			t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
		}
		if got := counts[template]; got != 2 {
			t.Fatalf("attempt %d: counts[%q] = %d, want 2 (both stores' rows are real demand)", attempt, template, got)
		}
		entry := demand[template]
		if got := len(entry.WorkBeadIDs); got != 1 || entry.WorkBeadIDs[0] != sharedID {
			t.Fatalf("attempt %d: WorkBeadIDs = %v, want exactly one %q row (an ID-keyed entry cannot represent two)", attempt, entry.WorkBeadIDs, sharedID)
		}
		// The surviving row is the most urgent one, and every piece of its
		// metadata describes that same bead in that same store.
		if got := entry.Priorities[sharedID]; got != p0 {
			t.Fatalf("attempt %d: Priorities[%q] = %d, want %d (the city P0 must not be ranked as the rig P2)", attempt, sharedID, got, p0)
		}
		if got := entry.StoreRefs[sharedID]; got != "city" {
			t.Fatalf("attempt %d: StoreRefs[%q] = %q, want city (session must bind to the store the priority came from)", attempt, sharedID, got)
		}
		if got := entry.Titles[sharedID]; got != "city urgent work" {
			t.Fatalf("attempt %d: Titles[%q] = %q, want the city row's title", attempt, sharedID, got)
		}
		if got := entry.CreatedAt[sharedID]; !got.Equal(cityCreated) {
			t.Fatalf("attempt %d: CreatedAt[%q] = %s, want the city row's time %s", attempt, sharedID, got, cityCreated)
		}
	}
}
