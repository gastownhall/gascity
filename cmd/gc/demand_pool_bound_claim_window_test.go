package main

import (
	"fmt"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// seedBoundRoutedWork creates a ready row routed to target carrying complete
// managed-worktree evidence for its own checkout, the shape a provisioner
// stamps onto a step before a seat is placed in its worktree.
func seedBoundRoutedWork(t *testing.T, store beads.Store, target string, n int) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title: fmt.Sprintf("bound %02d", n), Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:           target,
			beadmeta.WorkDirMetadataKey:            fmt.Sprintf("/wt/r1/w%02d", n),
			beadmeta.WorktreeRepoMetadataKey:       "/rigs/r1",
			beadmeta.WorktreeRootMetadataKey:       "/wt/r1",
			beadmeta.WorkBranchMetadataKey:         fmt.Sprintf("wt/w%02d", n),
			beadmeta.WorktreeBaseRefMetadataKey:    "main",
			beadmeta.WorktreeBaseSHAMetadataKey:    "4a43c56500000000000000000000000000000000",
			beadmeta.WorktreeCreatorMetadataKey:    "provisioner",
			beadmeta.WorktreeOwnerMetadataKey:      "provisioner",
			beadmeta.WorktreeGenerationMetadataKey: "g1",
			beadmeta.WorktreeLifecycleMetadataKey:  "active",
		},
	})
	if err != nil {
		t.Fatalf("seed bound routed work: %v", err)
	}
	return b
}

// boundDemandWindowCase seeds rows on an uncapped pool, then checks the demand
// pass against the order the claim window serves: a row bound to a managed
// worktree is counted and named only while its rank is inside the window, and
// a row bound to no worktree always is. It returns the worker's requests.
func boundDemandWindowCase(t *testing.T, seed func(e legAgreementEnv) map[string]bool) []SessionRequest {
	t.Helper()
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true"}}
	bound := seed(e)
	claimOrder := claimOrderedRoutedIDs(t, e, "worker")
	if len(claimOrder) != len(bound) {
		t.Fatalf("claim order = %d rows, want %d", len(claimOrder), len(bound))
	}
	want := map[string]bool{}
	for rank, id := range claimOrder {
		if !bound[id] || rank < config.RoutedReadyClaimWindow {
			want[id] = true
		}
	}
	_, counts, demand, _, errs := poolDemandForEnv(t, e)
	if len(errs) != 0 {
		t.Fatalf("demand errors: %v", errs)
	}
	if counts["worker"] != len(want) {
		t.Fatalf("worker demand = %d, want %d: a bound row past the %d-row claim window is not demand this pass", counts["worker"], len(want), config.RoutedReadyClaimWindow)
	}
	named := demand["worker"].WorkBeadIDs
	if len(named) != len(want) {
		t.Fatalf("named %d rows, want %d", len(named), len(want))
	}
	for _, id := range named {
		if !want[id] {
			t.Fatalf("named %s, a bound row outside the claim window; a seat placed for it would leave every row it is served", id)
		}
	}
	states := ComputePoolDesiredStatesWithDemandTraced(e.cfg, nil, nil, counts, demand, nil)
	var requests []SessionRequest
	for _, state := range states {
		if state.Template == "worker" {
			requests = append(requests, state.Requests...)
		}
	}
	for _, req := range requests {
		if req.WorkBeadID != "" && !want[req.WorkBeadID] {
			t.Fatalf("a seat request names %s, a bound row outside the claim window", req.WorkBeadID)
		}
	}
	return requests
}

// More ready rows bound to managed worktrees than the routed claim window
// holds: only the rows the window serves get a seat. A seat placed for a row
// behind the window would skip every row it is served (each bound to another
// worktree) and drain no_work, and be placed again every tick until claims
// advance the window.
func TestPoolDemandNamesNoBoundRowBeyondTheClaimWindow(t *testing.T) {
	const rows = config.RoutedReadyClaimWindow + 5
	requests := boundDemandWindowCase(t, func(e legAgreementEnv) map[string]bool {
		bound := map[string]bool{}
		for i := 0; i < rows; i++ {
			bound[seedBoundRoutedWork(t, e.work, "worker", i).ID] = true
		}
		return bound
	})
	withSpec := 0
	for _, req := range requests {
		if req.WorktreeSpec != nil {
			withSpec++
		}
	}
	if withSpec != config.RoutedReadyClaimWindow {
		t.Fatalf("bound seat requests = %d, want %d (one per row the claim window serves)", withSpec, config.RoutedReadyClaimWindow)
	}
}

// Rows bound to no worktree are demand wherever they rank: any seat in the
// pool's own directory can claim them. Only the bound rows behind the window
// wait.
func TestPoolDemandKeepsUnboundRowsBeyondTheClaimWindow(t *testing.T) {
	boundDemandWindowCase(t, func(e legAgreementEnv) map[string]bool {
		bound := map[string]bool{}
		for i := 0; i < config.RoutedReadyClaimWindow+5; i++ {
			bound[seedBoundRoutedWork(t, e.work, "worker", i).ID] = true
		}
		for i := 0; i < 5; i++ {
			bound[seedRoutedWork(t, e.work, "worker", fmt.Sprintf("unbound %02d", i)).ID] = false
		}
		return bound
	})
}

// warmCustomPoolDemand runs one demand pass for a warm pool (one live seat)
// whose custom scale_check answered customCount, over the stamped rows seeded.
func warmCustomPoolDemand(t *testing.T, customCount int, rows int) (mergedDemand, map[string]bool, *config.City) {
	t.Helper()
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "echo ignored"}}
	stamped := map[string]bool{}
	for i := 0; i < rows; i++ {
		stamped[seedBoundRoutedWork(t, e.work, "worker", i).ID] = true
	}
	live := []session.Info{{ID: "s-1", Template: "worker", PoolManaged: true, PoolSlot: "1"}}
	targets := buildDemandTargets(e.cityName, e.cityPath, e.cfg, e.leading(), e.rigs, nil, live, controllerQueryRuntimeEnv, io.Discard)
	if targets.coldWakeTemplates["worker"] {
		t.Fatal("fixture: the pool has a live seat, so it must not be cold")
	}
	in := collectedDemand{
		CustomCounts:           map[string]int{"worker": customCount},
		ColdWakeTemplates:      targets.coldWakeTemplates,
		NamedOnDemandTemplates: targets.namedOnDemandTemplates,
		NameOnlyTemplates:      targets.nameOnlyTemplates,
	}
	if len(targets.defaultScaleTargets) > 0 {
		var errs []error
		in.DefaultProbed = true
		in.DefaultCounts, in.DefaultDemand, in.DefaultPartials, errs = defaultScaleCheckCountsAndDemand(e.cfg, targets.defaultScaleTargets, newReadyDemandCache())
		if len(errs) != 0 {
			t.Fatalf("demand errors: %v", errs)
		}
	}
	return mergeCollectedDemand(e.cfg, in), stamped, e.cfg
}

// A warm pool with a custom scale_check names its seats' work from the routed
// ready rows in claim order, so a seat for a row bound to a managed worktree
// starts in that worktree. The custom count stays authoritative: three ready
// rows, a count of two, two seats.
func TestWarmCustomScaleCheckPoolPlacesSeatsInTheirRowsWorktrees(t *testing.T) {
	merged, stamped, cfg := warmCustomPoolDemand(t, 2, 3)
	if got := merged.ScaleCheckCounts["worker"]; got != 2 {
		t.Fatalf("worker demand = %d, want the custom count 2", got)
	}
	states := ComputePoolDesiredStatesWithDemandTraced(cfg, nil, nil, merged.ScaleCheckCounts, merged.ScaleCheckDemand, nil)
	var requests []SessionRequest
	for _, state := range states {
		if state.Template == "worker" {
			requests = append(requests, state.Requests...)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("worker requests = %d, want 2", len(requests))
	}
	for _, req := range requests {
		if !stamped[req.WorkBeadID] || req.WorktreeSpec == nil {
			t.Fatalf("request %+v carries no stamped row and worktree; a blind seat leaves every stamped row", req)
		}
	}
}

// Naming never raises a warm custom pool's count: a scale_check answering zero
// places no seat although stamped rows are ready.
func TestWarmCustomScaleCheckPoolNamingKeepsTheCustomCount(t *testing.T) {
	merged, _, _ := warmCustomPoolDemand(t, 0, 3)
	if got := merged.ScaleCheckCounts["worker"]; got != 0 {
		t.Fatalf("worker demand = %d, want the custom count 0", got)
	}
	if named := merged.ScaleCheckDemand["worker"].WorkBeadIDs; len(named) != 0 {
		t.Fatalf("named %v for a pool whose custom count is 0", named)
	}
}
