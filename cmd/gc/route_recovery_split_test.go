package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// routedOpenBead is an open, unassigned bead that already carries its pool
// route: nothing to repair, but a seat must be spawned for it.
func routedOpenBead(id string, metadata map[string]string) beads.Bead {
	md := map[string]string{beadmeta.RoutedToMetadataKey: routeRecoveryTestPool}
	for k, v := range metadata {
		md[k] = v
	}
	return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Metadata: md}
}

// splitRouteRecoveryPlan is Plan(RoutedWork) over one work ledger and one class
// binding: the converged split.
func splitRouteRecoveryPlan(t *testing.T, work, binding beads.Store) storeref.ResolvedPlan {
	t.Helper()
	topo := assembleResidencyTopology(&config.City{}, work, nil,
		[]storeref.ClassBinding{{
			Classes: []coordclass.Class{coordclass.ClassGraph},
			Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
		}}, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}
	if !plan.TouchesBinding() {
		t.Fatal("fixture: the plan has no binding")
	}
	return plan
}

// TestRouteRecoveryBackstopRaisesNoOffPlaneAlarmOnASingleStoreCity is the
// production false alarm (prod-evidence item 5, #7410): every single-store city
// was told, once an hour, that its routed work sat "on a work leg the runtime
// plane does not read" and to run `gc storage migrate` — 1,931 such lines in two
// weeks across five cities that have no binding to migrate to. A city with no
// binding has no off-plane leg: its work store IS the runtime plane's store.
func TestRouteRecoveryBackstopRaisesNoOffPlaneAlarmOnASingleStoreCity(t *testing.T) {
	cr, _ := routeRecoveryRuntime(t,
		routedOpenBead("CW-1", nil),
		routedOpenBead("CW-2", map[string]string{beadmeta.RootBeadIDMetadataKey: "CW-root"}),
	)
	var stderr bytes.Buffer
	cr.stderr = &stderr
	cr.logPrefix = "gc"

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if report.offPlaneRouted != 0 || report.offPlaneInfra != 0 {
		t.Errorf("a single-store city counted off_plane_routed=%d off_plane_infra=%d, want 0: there is no binding for any leg to be off", report.offPlaneRouted, report.offPlaneInfra)
	}
	for _, alarm := range []string{"storage migrate", "does not read", "outside the class binding"} {
		if strings.Contains(stderr.String(), alarm) {
			t.Errorf("a single-store city logged %q: %s", alarm, stderr.String())
		}
	}
	// Control: the pass did run and say so.
	if !strings.Contains(stderr.String(), "route recovery (backstop): pass") {
		t.Fatalf("no backstop pass line: %s", stderr.String())
	}
}

// TestRouteRecoveryBackstopNamesInfrastructureResidueOnASplitCity: on a split
// city a work leg can hold two kinds of routed bead. A work bead `gc sling`
// routed there is pool-demanded and served by `gc ready` (#6019, #7054), so it
// is counted but is not an alarm. An infrastructure-class row there (a graph
// step, a control bead) is residue no runtime reader looks for: it is reported
// by id, and not with `gc storage migrate`, which never reads a rig store and
// never moves a work bead.
func TestRouteRecoveryBackstopNamesInfrastructureResidueOnASplitCity(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		routedOpenBead("gc-work", nil),
		routedOpenBead("gc-step", map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-root"}),
	}, nil)}
	binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{routedOpenBead("gcg-1", map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"})}, nil)}
	plan := splitRouteRecoveryPlan(t, work, binding)

	report := newRouteRecoveryLane().backstopPass(plan, backstopReasonCadence)
	if report.offPlaneRouted != 2 || report.offPlaneInfra != 1 {
		t.Fatalf("off_plane_routed=%d off_plane_infra=%d, want 2 and 1 (the binding's own row counts for neither)", report.offPlaneRouted, report.offPlaneInfra)
	}
	if len(report.offPlaneSample) != 1 || report.offPlaneSample[0] != "gc-step" {
		t.Fatalf("off-plane sample = %v, want [gc-step]", report.offPlaneSample)
	}
	var stderr bytes.Buffer
	cr := &CityRuntime{logPrefix: "gc", stderr: &stderr}
	cr.logRouteRecovery(report)
	line := stderr.String()
	if !strings.Contains(line, "gc-step") || !strings.Contains(line, "1 open routed infrastructure-class bead") {
		t.Errorf("the alarm %q does not name the residue gc-step", line)
	}
	if strings.Contains(line, "gc-work") || strings.Contains(line, "storage migrate") {
		t.Errorf("the alarm %q names the pool-demanded work bead or the migrate remedy", line)
	}
}

// TestRouteRecoveryRepairsALostRouteOnAWorkBeadOffTheTick is read-audit R11: a
// work bead `gc sling` routed into the ledger loses its route, and the journal
// names it. Pool demand and `gc ready` federate the ledger, so the repair must
// not wait for the hourly scan, and the tick must still never read the ledger
// (TestSteadyTickLedgerRoundTripBudgetIsZero). The tick defers the candidate;
// the off-tick poller repairs it with one batched read of exactly that id.
func TestRouteRecoveryRepairsALostRouteOnAWorkBeadOffTheTick(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
	binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-1")}, nil)}
	plan := splitRouteRecoveryPlan(t, work, binding)
	lane := newRouteRecoveryLane()

	tick := lane.deltaPass(plan, []string{"CW-1", "GB-1", "XX-gone"})
	if work.reads() != 0 {
		t.Fatalf("the tick issued %d ledger round trip(s), want 0", work.reads())
	}
	if tick.restored != 1 || tick.deferred != 2 {
		t.Fatalf("tick restored=%d deferred=%d, want 1 (GB-1) and 2 (CW-1, XX-gone)", tick.restored, tick.deferred)
	}

	off := lane.ledgerCandidatePass(plan, lane.takeLedgerPending())
	if off.restored != 1 {
		t.Fatalf("off-tick pass restored %d, want 1", off.restored)
	}
	if got := mustRoutedTo(t, work, "CW-1"); got != routeRecoveryTestPool {
		t.Fatalf("CW-1 gc.routed_to = %q, want %q: a work bead's lost route waited for the hourly scan", got, routeRecoveryTestPool)
	}
	if work.scanned != 0 {
		t.Fatalf("the off-tick pass scanned the ledger's open corpus %d time(s); it may only re-read the named candidates", work.scanned)
	}
	if off.dropped != 1 {
		t.Fatalf("off-tick dropped = %d, want 1: XX-gone resolves on no leg", off.dropped)
	}
	if again := lane.takeLedgerPending(); len(again) != 0 {
		t.Fatalf("the ledger queue still holds %v after the pass", again)
	}
}

// TestRouteRecoveryLedgerPassWaitsOutQuiescence: the off-tick R11 pass reads
// the work ledger, so while the city is quiescent (suspended, nothing running)
// it reads nothing, as every sibling off-tick reader — and keeps its queue, so
// the candidates the tick deferred are repaired once the city resumes.
func TestRouteRecoveryLedgerPassWaitsOutQuiescence(t *testing.T) {
	cr, store := routeRecoveryRuntime(t, unroutedWorkBead("CW-1"))
	lane := cr.routeRecoveryLaneOf()
	if queued := lane.deferToLedger([]string{"CW-1"}); queued != 1 {
		t.Fatalf("fixture: queued %d candidate(s), want 1", queued)
	}
	cr.beadsQuiescent.Store(true)
	if report := cr.runRouteRecoveryLedgerDeferred(); report.candidates != 0 || report.err != nil {
		t.Fatalf("quiescent pass = %+v; want no candidate taken and no error", report)
	}
	if store.reads() != 0 {
		t.Fatalf("the quiescent pass issued %d ledger read(s), want 0", store.reads())
	}
	if pending := lane.takeLedgerPending(); len(pending) != 1 || pending[0] != "CW-1" {
		t.Fatalf("ledger queue after the quiescent pass = %v, want [CW-1] kept for the resume", pending)
	}
}
