package main

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// Order-tracking and mail maintenance runs on lanes of its own, off the orders
// lane.
//
// The three store watchdogs used to run at the top of every orders-lane pass,
// ahead of dispatch, under the lane's passMu. Each is a sweep over every
// scope's store, and the retention prune deletes up to
// orderTrackingRetentionWatchdogDeleteBudget beads, each a graph-aware delete
// of several store round-trips. On a loaded host that delete run took 7-15
// minutes, once every orderTrackingRetentionWatchdogInterval, and every order
// waited behind it: cooldown orders all fired ~9 minutes late together and
// health probes went dark for the same span. Dispatch reads none of
// what they write in the same pass, so nothing ties them to it.
//
// There are two lanes, so the slow one cannot hold the one dispatch depends on:
//
//   - order-tracking-sweep closes stale OPEN tracking beads. A stale open bead
//     shuts its order's open-tracking gate (#2168), so this sweep runs on its
//     own short cadence with its small close budget.
//   - bead-retention runs the closed order-tracking retention prune and the
//     stale nudge/mail sweep. Both are garbage collection; a long run delays
//     only the next collection.
//
// Each lane is paced by startPacedLane with no wake source: one pass per its
// interval after the previous pass ended. A pass skips a suspended city (its
// sweeps would reopen every scope's store, restarting the proxies suspension
// retired) and sheds FS pressure with its own episode, as the orders lane does.
// Each watchdog keeps its own interval gate, so a pass whose watchdogs are not
// due costs only those checks.

// orderMaintenanceLane is one maintenance lane: its name, trace trigger,
// pacing interval and the jobs a pass runs in order. Only its own goroutine
// runs its passes, so its FS-pressure episode needs no lock.
type orderMaintenanceLane struct {
	name     string
	trigger  TraceTickTrigger
	interval time.Duration
	jobs     []orderMaintenanceJob

	fsPressure fsPressureEpisode
}

// orderMaintenanceJob is one watchdog a maintenance pass runs.
type orderMaintenanceJob struct {
	name string
	run  func(ctx context.Context, cfg *config.City, now time.Time)
}

const (
	orderTrackingSweepLaneName = "order-tracking-sweep"
	beadRetentionLaneName      = "bead-retention"

	orderTrackingSweepLaneTraceTrigger TraceTickTrigger = "order_tracking_sweep"
	beadRetentionLaneTraceTrigger      TraceTickTrigger = "bead_retention"

	orderMaintenanceLaneReason = "cadence"
)

// orderMaintenanceLanes returns the two maintenance lanes, wired to this
// runtime's watchdogs. The bead-retention lane paces at the shorter of its two
// watchdogs' intervals; the retention prune gates itself to its own.
func (cr *CityRuntime) orderMaintenanceLanes() []*orderMaintenanceLane {
	return []*orderMaintenanceLane{
		{
			name:     orderTrackingSweepLaneName,
			trigger:  orderTrackingSweepLaneTraceTrigger,
			interval: clock.Backstop(orderTrackingSweepWatchdogInterval),
			jobs: []orderMaintenanceJob{
				{name: "order_tracking_sweep_watchdog", run: func(_ context.Context, cfg *config.City, now time.Time) {
					cr.runOrderTrackingSweepWatchdog(cfg, now)
				}},
			},
		},
		{
			name:     beadRetentionLaneName,
			trigger:  beadRetentionLaneTraceTrigger,
			interval: min(nudgeMailSweepWatchdogInterval, orderTrackingRetentionWatchdogInterval),
			jobs: []orderMaintenanceJob{
				{name: "nudge_mail_sweep_watchdog", run: func(_ context.Context, cfg *config.City, now time.Time) {
					cr.runNudgeMailSweepWatchdog(cfg, now)
				}},
				{name: "order_tracking_retention_watchdog", run: cr.runOrderTrackingRetentionWatchdog},
			},
		},
	}
}

// startOrderMaintenanceLanes starts every maintenance lane and returns a
// channel closed once all of them have exited (ctx done).
func (cr *CityRuntime) startOrderMaintenanceLanes(ctx context.Context) <-chan struct{} {
	lanes := cr.orderMaintenanceLanes()
	dones := make([]<-chan struct{}, 0, len(lanes))
	for _, lane := range lanes {
		dones = append(dones, startPacedLane(ctx, lane.interval, 0, nil, func(bool) {
			cr.safeTick(func() {
				cr.runOrderMaintenancePass(ctx, lane)
			}, lane.name)
		}))
	}
	all := make(chan struct{})
	go func() {
		defer close(all)
		for _, done := range dones {
			<-done
		}
	}()
	return all
}

// runOrderMaintenancePass is one pass of lane: the suspension and FS-pressure
// gates, then each job in order, each timed onto the pass's trace and, when
// the pass runs long, the slow-pass line.
func (cr *CityRuntime) runOrderMaintenancePass(ctx context.Context, lane *orderMaintenanceLane) {
	if ctx.Err() != nil {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	if effectiveCitySuspended(cfg, loadSuspensionStateBestEffort(cr.cityPath)) {
		return
	}

	var trace *sessionReconcilerTraceCycle
	if cr.trace != nil {
		trace = cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
			TickTrigger:   string(lane.trigger),
			TriggerDetail: orderMaintenanceLaneReason,
			CityPath:      cr.cityPath,
		}, cfg, nil)
	}
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": lane.name, "reason": orderMaintenanceLaneReason})
		}
	}()

	if cr.laneShouldSkipForFSPressure(&lane.fsPressure, trace, lane.name+":"+orderMaintenanceLaneReason, lane.name) {
		completion = TraceCompletionCompleted
		return
	}

	start := time.Now()
	phases := make([]lanePhase, 0, len(lane.jobs))
	for _, job := range lane.jobs {
		if ctx.Err() != nil {
			break
		}
		jobStart := time.Now()
		job.run(ctx, cfg, jobStart)
		phases = append(phases, lanePhase{name: job.name, took: time.Since(jobStart)})
	}
	recordLanePhases(trace, phases)
	cr.logSlowLanePass(lane.name, orderMaintenanceLaneReason, time.Since(start), phases)
	if ctx.Err() != nil {
		return
	}
	completion = TraceCompletionCompleted
}
