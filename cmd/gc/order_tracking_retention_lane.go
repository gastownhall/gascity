package main

import (
	"context"
	"fmt"
	"time"
)

// orderTrackingRetentionLaneTrigger names the retention lane's safeTick site
// in the reconciler-tick panic log, so an operator can tell which lane fired.
const orderTrackingRetentionLaneTrigger = "order-tracking-retention"

// startOrderTrackingRetentionLane starts the closed order-tracking retention
// sweep on its own paced lane and returns the function that stops it and waits
// for it.
//
// The sweep used to run synchronously inside the order pass under the orders
// lane's passMu, and its zero last-run stamp made the startup order pass —
// which runs before the orders lane, the cold-start reconcile and readiness —
// always sweep. On a store with ~73k closed tracking beads a restart stayed
// not-ready for about six minutes (#6429), and every due lane pass stopped
// order dispatch until up to orderTrackingRetentionWatchdogDeleteBudget graph
// deletes finished. On its own lane the sweep blocks nothing: one goroutine
// runs every pass, so the lane is single-flight by construction and needs no
// flag, lock or join.
//
// The lane is the same shape as the orders lane: its own goroutine, its own
// cadence (orderTrackingRetentionWatchdogInterval), safeTick recovery per
// pass, stopped by the city context. The pre-filled wake channel makes the
// first pass run at once rather than one interval after the city becomes
// ready. run() registers the stop as a deferred call, so the lane is canceled
// and joined before the deferred shutdown() closes the storage binding: the
// sweep is never mid-store-call when its handles close.
func (cr *CityRuntime) startOrderTrackingRetentionLane(ctx context.Context) func() {
	laneCtx, cancel := context.WithCancel(ctx)
	// The lane's FS-pressure episode. Only the lane goroutine touches it, so
	// it needs no lock — the same arrangement as the orders lane's counters,
	// kept here instead of on CityRuntime so a directly-driven pass (tests)
	// supplies its own.
	episode := &retentionLaneFSEpisode{}
	// Pre-filled so the first pass runs at once, not one interval after the
	// lane starts. The paced lane joins every wake into the one pass that
	// follows it, so the slot is never a queue of passes.
	first := make(chan struct{}, 1)
	first <- struct{}{}
	done := startPacedLane(laneCtx, orderTrackingRetentionWatchdogInterval, 0, first, func(bool) {
		cr.safeTick(func() { cr.runOrderTrackingRetentionLanePass(laneCtx, episode) }, orderTrackingRetentionLaneTrigger)
	})
	return func() {
		cancel()
		<-done
	}
}

// retentionLaneFSEpisode is one retention lane's FS-pressure episode state.
type retentionLaneFSEpisode struct {
	skips  int
	logged bool
}

// retentionLaneShouldSkipForFSPressure is the orders lane's pressure gate
// applied to the retention lane: skip while pressure is high, but force a pass
// after maxConsecutiveFSPressureSkips so retention cannot starve. It logs once
// per episode. The supervisor skipped-tick event stays the tick's, and the
// retention sweep records no trace, so there is nothing else to record.
func (cr *CityRuntime) retentionLaneShouldSkipForFSPressure(episode *retentionLaneFSEpisode) bool {
	status, ok := currentFSPressureStatus(cr.stderr)
	if !ok || !status.High {
		episode.skips = 0
		episode.logged = false
		return false
	}
	if episode.skips >= maxConsecutiveFSPressureSkips {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), forcing order-tracking retention sweep after %d skipped pass(es)\n", //nolint:errcheck // best-effort stderr
				status.Avg60, status.Threshold, episode.skips)
		}
		episode.skips = 0
		episode.logged = false
		return false
	}
	episode.skips++
	if !episode.logged && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), skipping order-tracking retention sweep\n", //nolint:errcheck // best-effort stderr
			status.Avg60, status.Threshold)
	}
	episode.logged = true
	return true
}

// runOrderTrackingRetentionLanePass is one retention lane pass. It returns
// without any store call when the city context is canceled, its config is not
// published yet, or the city is suspended — the predicate the order pass
// applies, because a read on a suspended city restarts the proxies suspension
// retired.
func (cr *CityRuntime) runOrderTrackingRetentionLanePass(ctx context.Context, episode *retentionLaneFSEpisode) {
	if ctx.Err() != nil {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	if cfg == nil {
		return
	}
	if effectiveCitySuspended(cfg, loadSuspensionStateBestEffort(cr.cityPath)) {
		return
	}
	if cr.retentionLaneShouldSkipForFSPressure(episode) {
		return
	}
	cr.runOrderTrackingRetentionWatchdog(ctx, cfg, time.Now())
}
