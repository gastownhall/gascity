package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The retention prune's delete run held the orders pass for 7-15
// minutes every 15 minutes, so every cooldown order fired late together. An
// orders pass must now run none of the store watchdogs: it dispatches, and the
// maintenance lanes own the sweeps.
func TestOrdersLanePassRunsNoStoreWatchdogs(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)

	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)

	if !od.called.Load() {
		t.Fatal("orders pass did not dispatch")
	}
	if !cr.orderSweepWatchdogLast.IsZero() {
		t.Errorf("orders pass ran the order-tracking sweep watchdog (last = %s)", cr.orderSweepWatchdogLast)
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Errorf("orders pass ran the order-tracking retention watchdog (last = %s)", cr.orderTrackingRetentionWatchdogLast)
	}
	if !cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Errorf("orders pass ran the nudge/mail sweep watchdog (last = %s)", cr.nudgeMailSweepWatchdogLast)
	}
}

// A maintenance job stuck in a slow store
// call must not hold order dispatch. With the bead-retention lane parked in a
// job, orders passes keep dispatching.
func TestOrdersDispatchWhileMaintenanceJobIsBlocked(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "10ms", nil)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseJob)
	lane := &orderMaintenanceLane{
		name:    beadRetentionLaneName,
		trigger: beadRetentionLaneTraceTrigger,
		jobs: []orderMaintenanceJob{{name: "stuck_prune", run: func(context.Context, *config.City, time.Time) {
			enterOnce.Do(func() { close(entered) })
			<-release
		}}},
	}
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		cr.runOrderMaintenancePass(context.Background(), lane)
	}()
	awaitClose(t, entered, "the maintenance job starting")

	startOrdersLaneForTest(t, cr)
	waitForCalls(t, od.calls.Load, 3)

	releaseJob()
	awaitClose(t, maintenanceDone, "the maintenance pass finishing after release")
}

// Each maintenance lane runs its watchdogs: between them the two lanes cover
// all three, so moving them off the orders lane drops none.
func TestOrderMaintenanceLanesRunEveryWatchdog(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	cr.cityPath = "" // skips the backup-age guard, as the retention watchdog's own tests do

	for _, lane := range cr.orderMaintenanceLanes() {
		cr.runOrderMaintenancePass(context.Background(), lane)
	}

	if cr.orderSweepWatchdogLast.IsZero() {
		t.Error("order-tracking sweep watchdog did not run")
	}
	if cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Error("order-tracking retention watchdog did not run")
	}
	if cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Error("nudge/mail sweep watchdog did not run")
	}
}

// The order-tracking sweep, whose stale beads jam dispatch gates, must not
// share a lane with the slow garbage collectors.
func TestOrderTrackingSweepHasALaneOfItsOwn(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	for _, lane := range cr.orderMaintenanceLanes() {
		names := make([]string, 0, len(lane.jobs))
		for _, job := range lane.jobs {
			names = append(names, job.name)
		}
		hasSweep := false
		for _, name := range names {
			if name == "order_tracking_sweep_watchdog" {
				hasSweep = true
			}
		}
		if hasSweep && len(names) != 1 {
			t.Fatalf("lane %s runs %v; the order-tracking sweep must run alone", lane.name, names)
		}
		if lane.interval <= 0 {
			t.Fatalf("lane %s interval = %s, want > 0", lane.name, lane.interval)
		}
	}
}

// A suspended city gets no maintenance pass: the sweeps would open every
// scope's store and restart the proxies suspension retired.
func TestOrderMaintenancePassSkipsSuspendedCity(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	cr.cfg.Workspace.SuspendedOnStart = true
	ran := false
	lane := &orderMaintenanceLane{
		name: beadRetentionLaneName,
		jobs: []orderMaintenanceJob{{name: "job", run: func(context.Context, *config.City, time.Time) { ran = true }}},
	}
	cr.runOrderMaintenancePass(context.Background(), lane)
	if ran {
		t.Fatal("maintenance job ran for a suspended city")
	}
}

// A canceled pass runs no further job, and the job that is running gets the
// lane's context so it can stop.
func TestOrderMaintenancePassStopsOnCancel(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	ctx, cancel := context.WithCancel(context.Background())
	var ran []string
	lane := &orderMaintenanceLane{
		name: beadRetentionLaneName,
		jobs: []orderMaintenanceJob{
			{name: "first", run: func(jobCtx context.Context, _ *config.City, _ time.Time) {
				ran = append(ran, "first")
				cancel()
				if jobCtx.Err() == nil {
					t.Error("job context not canceled with the lane's")
				}
			}},
			{name: "second", run: func(context.Context, *config.City, time.Time) { ran = append(ran, "second") }},
		},
	}
	cr.runOrderMaintenancePass(ctx, lane)
	if strings.Join(ran, ",") != "first" {
		t.Fatalf("jobs run = %v, want only the first", ran)
	}
}

// A pass past lanePassSlowThreshold writes one line naming every step's time;
// a fast pass writes nothing.
func TestLogSlowLanePass(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{logPrefix: "gc test", stderr: &stderr}
	phases := []lanePhase{
		{name: "nudge_mail_sweep_watchdog", took: 2 * time.Second},
		{name: "order_tracking_retention_watchdog", took: 7*time.Minute + 30*time.Second},
	}

	cr.logSlowLanePass(beadRetentionLaneName, orderMaintenanceLaneReason, lanePassSlowThreshold-time.Millisecond, phases)
	if stderr.Len() != 0 {
		t.Fatalf("fast pass logged %q, want nothing", stderr.String())
	}

	cr.logSlowLanePass(beadRetentionLaneName, orderMaintenanceLaneReason, 7*time.Minute+32*time.Second, phases)
	out := stderr.String()
	for _, want := range []string{
		"gc test: lane bead-retention: slow pass (cadence) took 7m32s",
		"nudge_mail_sweep_watchdog=2s",
		"order_tracking_retention_watchdog=7m30s",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("slow-pass line = %q, want it to contain %q", out, want)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("slow-pass lines = %d, want 1", n)
	}
}

// The orders pass times its own steps, so a slow pass names the step.
func TestOrdersDispatchStepsAreTimed(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	phases := cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
	var names []string
	for _, p := range phases {
		names = append(names, p.name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "order_rescan") || !strings.HasSuffix(got, "order_dispatch") {
		t.Fatalf("dispatch steps = %q, want the rescan and ending with the dispatch", got)
	}
	if !od.called.Load() {
		t.Fatal("dispatch step did not dispatch")
	}
}

// The retention prune stops before its next delete once its context is done,
// so shutdown does not wait out a full delete budget.
func TestRetentionPruneStopsWhenContextDone(t *testing.T) {
	now := time.Now()
	policy := orderTrackingRetentionPolicy{deleteAfterClose: time.Hour, retainLast: minClosedOrderTrackingRetained}
	store := beads.NewMemStore()
	for i := 0; i < minClosedOrderTrackingRetained+5; i++ {
		b, err := store.Create(beads.Bead{
			Title:  "order run",
			Type:   "task",
			Labels: []string{labelOrderTracking, "order-run:prune-me"},
		})
		if err != nil {
			t.Fatalf("create tracking bead: %v", err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close tracking bead: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(ctx, []beads.Store{store}, now.Add(2*time.Hour), policy, nil, 100)
	if deleted != 0 {
		t.Fatalf("deleted = %d after cancellation, want 0", deleted)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The same store with a live context prunes the expired runs, so the
	// zero above is the cancellation, not an empty candidate set.
	deleted, err = sweepClosedOrderTrackingRetentionAcrossStoresBounded(context.Background(), []beads.Store{store}, now.Add(2*time.Hour), policy, nil, 100)
	if err != nil {
		t.Fatalf("prune with a live context: %v", err)
	}
	if deleted != 5 {
		t.Fatalf("deleted = %d with a live context, want 5", deleted)
	}
}
