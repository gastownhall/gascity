package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

// warmCustomPass is one demand pass over a city whose one agent is a pool
// with a custom scale_check, kept warm by one live seat: the agent loop's
// targets, what the collectors read, the probe's errors, and the merge.
type warmCustomPass struct {
	targets   demandTargets
	collected collectedDemand
	errs      []error
	merged    mergedDemand
}

// runWarmCustomPass runs one demand pass over e, whose custom scale_check
// answered customCount.
func runWarmCustomPass(t *testing.T, e legAgreementEnv, customCount int) warmCustomPass {
	t.Helper()
	template := e.cfg.Agents[0].QualifiedName()
	live := []session.Info{{ID: "s-1", Template: template, PoolManaged: true, PoolSlot: "1"}}
	pass := warmCustomPass{targets: buildDemandTargets(e.cityName, e.cityPath, e.cfg, e.leading(), e.rigs, nil, live, controllerQueryRuntimeEnv, io.Discard)}
	if pass.targets.coldWakeTemplates[template] {
		t.Fatal("fixture: the pool has a live seat, so it must not be cold")
	}
	c := &pass.collected
	c.CustomCounts = map[string]int{template: customCount}
	c.ColdWakeTemplates, c.NamedOnDemandTemplates, c.NameOnlyTemplates = pass.targets.coldWakeTemplates, pass.targets.namedOnDemandTemplates, pass.targets.nameOnlyTemplates
	if len(pass.targets.defaultScaleTargets) > 0 {
		c.DefaultProbed = true
		c.DefaultCounts, c.DefaultDemand, c.DefaultPartials, pass.errs = defaultScaleCheckCountsAndDemand(e.cfg, pass.targets.defaultScaleTargets, newReadyDemandCache())
	}
	pass.merged = mergeCollectedDemand(e.cfg, pass.collected)
	return pass
}

// requests is the pool's requests for the pass's demand over sessions.
func (p warmCustomPass) requests(cfg *config.City, sessions []session.Info) []SessionRequest {
	var requests []SessionRequest
	for _, state := range ComputePoolDesiredStatesWithDemandTraced(cfg, nil, sessions, p.merged.ScaleCheckCounts, p.merged.ScaleCheckDemand, nil) {
		if state.Template == cfg.Agents[0].QualifiedName() {
			requests = append(requests, state.Requests...)
		}
	}
	return requests
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
	pass := runWarmCustomPass(t, e, customCount)
	if len(pass.errs) != 0 {
		t.Fatalf("demand errors: %v", pass.errs)
	}
	return pass.merged, stamped, e.cfg
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

// seatFor is where realization starts the seat of request
// (verifiedPoolTriggerWorkDir) in a pool whose own directory is poolDir: the
// verified worktree of the work it names, which the seat's claim reads as a
// managed worktree (hookSeatManaged), or the pool's own directory.
func seatFor(request SessionRequest, poolDir string) (seat hookPoolSeat, managed bool) {
	if request.WorktreeSpec != nil {
		return hookPoolSeat{WorkDir: request.WorktreeSpec.Path, PoolDir: poolDir}, true
	}
	return hookPoolSeat{WorkDir: poolWorkDirUnder(poolDir, request), PoolDir: poolDir}, false
}

// A warm custom pool whose agent has its own work_query names no seat from the
// default probe, which reads the generated routed tier, not that query. Older
// row A carries complete managed-worktree evidence but is not what the custom
// query selects; unbound row B is; the custom count is one. The seat starts
// unnamed in the pool's own directory, where its claim takes B. The control,
// the same rows on a pool with the generated query, names A and starts the
// seat in A's worktree, which leaves B: the placement the custom query's pool
// must not get.
func TestWarmCustomScaleCheckPoolWithCustomWorkQueryNamesNoSeat(t *testing.T) {
	const poolDir = "/city/pools/worker"
	for _, tc := range []struct {
		name      string
		workQuery string
		named     bool
	}{
		{name: "generated work query (control)", named: true},
		{name: "custom work query", workQuery: "bd ready --label=selected --json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLegAgreementEnv(t, true)
			e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "echo 1", WorkQuery: tc.workQuery}}
			a := seedBoundRoutedWork(t, e.work, e.cfg.Agents[0].Name, 0)
			b := seedRoutedWork(t, e.work, "worker", "selected by the custom query")
			pass := runWarmCustomPass(t, e, 1)
			if len(pass.errs) != 0 {
				t.Fatalf("demand errors: %v", pass.errs)
			}
			if got := pass.targets.nameOnlyTemplates["worker"]; got != tc.named {
				t.Fatalf("nameOnlyTemplates[worker] = %v, want %v", got, tc.named)
			}
			requests := pass.requests(e.cfg, nil)
			if len(requests) != 1 {
				t.Fatalf("requests = %+v, want the custom count's one seat", requests)
			}
			req := requests[0]
			seat, managed := seatFor(req, poolDir)
			leavesB := hookClaimSeatLeaves(b, seat, func() bool { return managed })
			if tc.named {
				if req.WorkBeadID != a.ID || req.WorktreeSpec == nil || !leavesB {
					t.Fatalf("control: request %+v (leaves %s: %v), want the seat named for %s in its worktree, leaving %s", req, b.ID, leavesB, a.ID, b.ID)
				}
				return
			}
			if req.WorkBeadID != "" || req.WorktreeSpec != nil {
				t.Fatalf("request %+v names %s; a pool with a custom work_query starts its seat unnamed", req, req.WorkBeadID)
			}
			if leavesB {
				t.Fatalf("the seat in %s leaves %s, the work its custom query selects", seat.WorkDir, b.ID)
			}
		})
	}
}

// A row whose worktree evidence is unusable (here: gc.work_dir with an
// incomplete gc.worktree_* set) names no seat of a warm custom pool: no seat
// could be placed for it. The custom count's one seat is named for the next
// row instead, and the merge reports the refused row as realization reports a
// named request whose evidence it refuses. Named for the unusable P0 row, the
// pool's one seat would be refused and the unbound P2 row left unserved.
func TestWarmCustomScaleCheckPoolNamesNoRowWithUnusableEvidence(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "echo 1"}}
	p0, p2 := 0, 2
	unusable, err := e.work.Create(beads.Bead{Title: "unusable evidence", Type: "task", Status: "open", Priority: &p0, Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:     "worker",
		beadmeta.WorkDirMetadataKey:      "/wt/r1/unusable",
		beadmeta.WorktreeRepoMetadataKey: "/rigs/r1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := e.work.Create(beads.Bead{Title: "unbound", Type: "task", Status: "open", Priority: &p2, Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	pass := runWarmCustomPass(t, e, 1)
	if len(pass.errs) != 0 {
		t.Fatalf("demand errors: %v", pass.errs)
	}
	if probed := pass.collected.DefaultDemand["worker"]; len(probed.WorkBeadIDs) != 2 || probed.WorkBeadIDs[0] != unusable.ID || probed.WorktreeErrors[unusable.ID] == "" {
		t.Fatalf("fixture: probe read %v with worktree errors %v, want %s first and unusable", probed.WorkBeadIDs, probed.WorktreeErrors, unusable.ID)
	}
	requests := pass.requests(e.cfg, nil)
	if len(requests) != 1 || requests[0].WorkBeadID != unbound.ID || requests[0].WorktreeError != "" || !requests[0].NameOnly {
		t.Fatalf("requests = %+v, want the custom count's one seat named for %s", requests, unbound.ID)
	}
	refusals := pass.merged.NameRefusals
	if len(refusals) != 1 || refusals[0].Template != "worker" || refusals[0].WorkBeadID != unusable.ID ||
		!errors.Is(refusals[0].err(), errPoolTriggerWorktreeEvidence) || !strings.Contains(refusals[0].err().Error(), "missing gc.worktree_root") {
		t.Fatalf("name refusals = %+v, want %s refused for its missing gc.worktree_root", refusals, unusable.ID)
	}
}

// A warm custom pool's probe only names work, so a leg it cannot read changes
// neither the pool's count nor its drain retention: the custom count is its
// whole answer. The probe reads the dark leg (its partial is collected); the
// merge leaves the pool out of every partial set.
func TestWarmCustomScaleCheckPoolProbePartialChangesNoRetention(t *testing.T) {
	e := newLegAgreementEnv(t, false)
	work := beads.NewMemStoreFrom(1, nil, nil)
	binding := beads.NewMemStoreFrom(500000, nil, nil)
	e.work, e.binding = darkLedgerReadyStore{Store: work}, binding
	routes := splitRoutes(e.binding)
	registerResidencyRoutes(e.cityPath, routes, func() beads.Store { return e.work })
	t.Cleanup(func() { unregisterResidencyRoutes(e.cityPath, routes) })
	e.cfg.Agents = []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "echo 2"}}
	readable := seedBoundRoutedWork(t, binding, e.cfg.Agents[0].Name, 0)
	pass := runWarmCustomPass(t, e, 2)
	if !pass.targets.nameOnlyTemplates["worker"] || !pass.collected.DefaultPartials["worker"] || len(pass.errs) == 0 {
		t.Fatalf("fixture: name-only %v, probe partials %v, errors %v; want a name-only probe that read the dark leg",
			pass.targets.nameOnlyTemplates, pass.collected.DefaultPartials, pass.errs)
	}
	m := pass.merged
	if m.PoolScaleCheckPartial["worker"] || m.PoolPartialRetention["worker"] || m.ScaleCheckPartial["worker"] {
		t.Fatalf("partials: pool %v, retention %v, all %v; a name-only probe's partial must change no retention",
			m.PoolScaleCheckPartial, m.PoolPartialRetention, m.ScaleCheckPartial)
	}
	if got := m.ScaleCheckCounts["worker"]; got != 2 {
		t.Fatalf("worker demand = %d, want the custom count 2", got)
	}
	if named := m.ScaleCheckDemand["worker"].WorkBeadIDs; len(named) != 1 || named[0] != readable.ID {
		t.Fatalf("named %v, want the readable leg's %s", named, readable.ID)
	}
}

// A warm custom pool's in-flight seat that claimed nothing yet is concrete
// capacity: the names rebind it to the first row in claim order, with that
// row's worktree, and a fresh request takes the next. Both only name their
// seats (NameOnly), so realization keeps either seat when its row's evidence
// is refused (nameOnlyFallback).
func TestWarmCustomScaleCheckPoolRebindsAnInFlightSeatToItsFirstName(t *testing.T) {
	e := newLegAgreementEnv(t, true)
	e.cfg.Agents = []config.Agent{{Name: "claude", StartCommand: "true", ScaleCheck: "echo 2"}}
	first := seedBoundRoutedWork(t, e.work, e.cfg.Agents[0].Name, 0)
	second := seedBoundRoutedWork(t, e.work, e.cfg.Agents[0].Name, 1)
	pass := runWarmCustomPass(t, e, 2)
	if len(pass.errs) != 0 {
		t.Fatalf("demand errors: %v", pass.errs)
	}
	requests := pass.requests(e.cfg, sessionInfosFromBeads([]beads.Bead{pendingPoolSessionBead("sess-1")}))
	if len(requests) != 2 {
		t.Fatalf("requests = %+v, want the in-flight seat and one fresh seat", requests)
	}
	bySession := map[string]SessionRequest{}
	for _, req := range requests {
		bySession[req.SessionBeadID] = req
	}
	for session, work := range map[string]string{"sess-1": first.ID, "": second.ID} {
		req, ok := bySession[session]
		if !ok || req.WorkBeadID != work || req.WorktreeSpec == nil || req.WorktreeSpec.BeadID != work || !req.NameOnly {
			t.Fatalf("requests = %+v, want session %q named for %s in its worktree, name-only", requests, session, work)
		}
	}
}

// The legacy demand pass reports each row the merge names no seat for, as its
// realization reports a named request whose evidence it refuses, and the
// custom count's seat still starts: a pool kept warm by its floor, whose one
// routed row has unusable worktree evidence.
func TestBuildDesiredStateReportsRowsAWarmCustomPoolNamesNoSeatFor(t *testing.T) {
	store := beads.NewMemStore()
	row, err := store.Create(beads.Bead{Title: "unusable evidence", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:     "worker",
		beadmeta.WorkDirMetadataKey:      "/wt/r1/unusable",
		beadmeta.WorktreeRepoMetadataKey: "/rigs/r1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "worker", StartCommand: "true", ScaleCheck: "echo 1",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
		}},
	}
	var stderr bytes.Buffer
	ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	want := fmt.Sprintf("pool %q trigger bead %s: %v invalid: work bead %s", "worker", row.ID, errPoolTriggerWorktreeEvidence, row.ID)
	if out := stderr.String(); !strings.Contains(out, want) || !strings.Contains(out, "(naming no seat for it)") {
		t.Fatalf("stderr = %q; want %q reported, naming no seat for it", out, want)
	}
	if got := ds.ScaleCheckCounts["worker"]; got != 1 {
		t.Fatalf("worker demand = %d, want the custom count 1", got)
	}
	seats := 0
	for _, tp := range ds.State {
		if tp.TemplateName == "worker" {
			seats++
		}
	}
	if seats != 1 {
		t.Fatalf("worker seats desired = %d, want the custom count's one seat", seats)
	}
}
