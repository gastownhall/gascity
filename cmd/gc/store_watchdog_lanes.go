package main

import (
	"context"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Store watchdogs run on lanes of their own, one per watchdog.
//
// The stale order-tracking sweep, the closed order-tracking retention prune
// and the nudge/mail sweep read and mutate every scope's store and need no
// order dispatcher. Run inline in the orders pass they held passMu, so one
// slow prune delayed order dispatch, dispatcher installation and the other
// two watchdogs. Each now runs on an instance of one lane type: its own
// goroutine, paced by startPacedLane at the patrol interval with no wake
// source, safeTick recovery per pass, stopped and joined by run().
//
// What a lane owns, and why it can:
//
//   - Its watchdog's clocks on CityRuntime (orderSweepWatchdog*,
//     orderTrackingRetentionWatchdogLast, nudgeMailSweepWatchdogLast). Only
//     that lane's pass reads or writes them; passMu serializes a pass run
//     directly (tests) with the lane goroutine's.
//
// The patrol interval paces attempts; the watchdog's own clock decides
// whether an attempt is due, so the watchdog intervals (30 s for the tracking
// sweep, 15 minutes for retention, five minutes for nudge/mail) are unchanged.
// A pass that is not due does nothing else. Passes on one lane never overlap,
// and lanes never wait on each other or on the orders lane.
type storeWatchdogLane struct {
	spec storeWatchdogSpec

	// passMu serializes passes, and guards the watchdog's clocks.
	passMu sync.Mutex
}

// storeWatchdogKey names one store watchdog.
type storeWatchdogKey string

const (
	storeWatchdogOrderTrackingSweep     storeWatchdogKey = "order_tracking_sweep"
	storeWatchdogOrderTrackingRetention storeWatchdogKey = "order_tracking_retention"
	storeWatchdogNudgeMailSweep         storeWatchdogKey = "nudge_mail_sweep"
)

// storeWatchdogSpec is one watchdog: its key, the work its log lines name,
// whether it is due, and its body.
type storeWatchdogSpec struct {
	key  storeWatchdogKey
	work string
	due  func(cr *CityRuntime, now time.Time) bool
	run  func(cr *CityRuntime, ctx context.Context, cfg *config.City, now time.Time)
}

// storeWatchdogSpecs is every store watchdog, one lane each, in start order.
var storeWatchdogSpecs = []storeWatchdogSpec{
	{
		key:  storeWatchdogOrderTrackingSweep,
		work: "order tracking sweep",
		due:  (*CityRuntime).orderTrackingSweepWatchdogDue,
		run: func(cr *CityRuntime, _ context.Context, cfg *config.City, now time.Time) {
			cr.runOrderTrackingSweepWatchdog(cfg, now)
		},
	},
	{
		key:  storeWatchdogOrderTrackingRetention,
		work: "order tracking retention",
		due:  (*CityRuntime).orderTrackingRetentionWatchdogDue,
		run: func(cr *CityRuntime, _ context.Context, cfg *config.City, now time.Time) {
			cr.runOrderTrackingRetentionWatchdog(cfg, now)
		},
	},
	{
		key:  storeWatchdogNudgeMailSweep,
		work: "nudge/mail sweep",
		due:  (*CityRuntime).nudgeMailSweepWatchdogDue,
		run: func(cr *CityRuntime, _ context.Context, cfg *config.City, now time.Time) {
			cr.runNudgeMailSweepWatchdog(cfg, now)
		},
	},
}

// storeWatchdogLanesOf returns this runtime's store watchdog lanes, creating
// them on first use so a directly-constructed CityRuntime needs no wiring.
func (cr *CityRuntime) storeWatchdogLanesOf() []*storeWatchdogLane {
	cr.storeWatchdogLanesOnce.Do(func() {
		lanes := make([]*storeWatchdogLane, 0, len(storeWatchdogSpecs))
		for _, spec := range storeWatchdogSpecs {
			lanes = append(lanes, &storeWatchdogLane{spec: spec})
		}
		cr.storeWatchdogLanes = lanes
	})
	return cr.storeWatchdogLanes
}

// storeWatchdogLaneOf returns the lane running the watchdog key, or nil.
func (cr *CityRuntime) storeWatchdogLaneOf(key storeWatchdogKey) *storeWatchdogLane {
	for _, lane := range cr.storeWatchdogLanesOf() {
		if lane.spec.key == key {
			return lane
		}
	}
	return nil
}

// runStoreWatchdogLanePass is one pass of lane: nothing unless the watchdog
// is due, then the watchdog body against the published config.
func (cr *CityRuntime) runStoreWatchdogLanePass(ctx context.Context, lane *storeWatchdogLane) {
	if ctx.Err() != nil {
		return
	}
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	if !lane.spec.due(cr, now) {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	lane.spec.run(cr, ctx, cfg, now)
}
