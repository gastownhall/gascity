package main

import (
	"context"
	"time"
)

// orderTrackingRetentionLaneTrigger names the retention lane's passes in
// safeTick's panic report.
const orderTrackingRetentionLaneTrigger = "order-tracking-retention"

// startOrderTrackingRetentionLane starts the closed order-tracking retention
// sweep on its own paced lane and returns the function that stops the lane and
// waits for its goroutine to exit.
//
// The sweep lists every closed tracking bead and deletes up to
// orderTrackingRetentionWatchdogDeleteBudget graphs, and on a bd store each of
// those store calls is a subprocess, so a sweep takes minutes on a large
// backlog. Its own lane keeps those minutes off order dispatch and off
// startup readiness (#6429). One goroutine runs every pass, so a sweep never
// overlaps another.
//
// The first pass runs at once rather than one interval after the lane starts,
// so a restart prunes as soon as the city is ready; later passes run one
// interval after the previous pass ended.
func (cr *CityRuntime) startOrderTrackingRetentionLane(ctx context.Context) func() {
	laneCtx, cancel := context.WithCancel(ctx)
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

// runOrderTrackingRetentionLanePass is one retention lane pass: the watchdog
// with the current config snapshot. The lane goroutine is its only caller
// outside tests.
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
