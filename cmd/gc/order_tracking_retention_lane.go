package main

import (
	"context"
	"fmt"
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
//
// A suspended city gets no pass, by the predicate the order pass applies: the
// sweep reads every scope's store, which would restart the proxies suspension
// retired. Under high FS pressure the pass sheds the sweep by the orders lane's
// policy (orderTrackingRetentionShouldSkipForFSPressure).
func (cr *CityRuntime) runOrderTrackingRetentionLanePass(ctx context.Context) {
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
	if cr.orderTrackingRetentionShouldSkipForFSPressure() {
		return
	}
	cr.runOrderTrackingRetentionWatchdog(cfg, time.Now())
}

// orderTrackingRetentionFSPressureEpisode tracks one run of FS-pressure-skipped
// retention passes: how many passes in a row skipped, and whether the episode
// has logged its skip line.
type orderTrackingRetentionFSPressureEpisode struct {
	skips  int
	logged bool
}

// orderTrackingRetentionShouldSkipForFSPressure reports whether this retention
// pass sheds its sweep for FS pressure, by the orders lane's policy: while
// pressure is high, up to maxConsecutiveFSPressureSkips passes in a row skip,
// with one log line per episode, and the pass after them is forced through so
// retention cannot starve. A pass without high pressure ends the episode.
// Skipped passes leave the sweep's interval stamp alone, so the next pass that
// runs sweeps.
func (cr *CityRuntime) orderTrackingRetentionShouldSkipForFSPressure() bool {
	episode := &cr.orderTrackingRetentionFSPressure
	status, ok := currentFSPressureStatus(cr.stderr)
	if !ok || !status.High {
		*episode = orderTrackingRetentionFSPressureEpisode{}
		return false
	}
	if episode.skips >= maxConsecutiveFSPressureSkips {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), forcing the order-tracking retention sweep after %d skipped passes\n", //nolint:errcheck // best-effort stderr
				status.Avg60, status.Threshold, episode.skips)
		}
		*episode = orderTrackingRetentionFSPressureEpisode{}
		return false
	}
	episode.skips++
	if !episode.logged && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), skipping the order-tracking retention sweep\n", //nolint:errcheck // best-effort stderr
			status.Avg60, status.Threshold)
	}
	episode.logged = true
	return true
}
