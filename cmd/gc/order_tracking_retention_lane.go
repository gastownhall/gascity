package main

import (
	"context"
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
	// Pre-filled so the first pass runs at once, not one interval after the
	// lane starts. The paced lane joins every wake into the one pass that
	// follows it, so the slot is never a queue of passes.
	first := make(chan struct{}, 1)
	first <- struct{}{}
	done := startPacedLane(laneCtx, orderTrackingRetentionWatchdogInterval, 0, first, func(bool) {
		cr.safeTick(func() { cr.runOrderTrackingRetentionLanePass(laneCtx) }, orderTrackingRetentionLaneTrigger)
	})
	return func() {
		cancel()
		<-done
	}
}

// runOrderTrackingRetentionLanePass is one retention lane pass. It returns
// without any store call when the city context is canceled or its config is
// not published yet.
func (cr *CityRuntime) runOrderTrackingRetentionLanePass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	if cfg == nil {
		return
	}
	cr.runOrderTrackingRetentionWatchdog(cfg, time.Now())
}
