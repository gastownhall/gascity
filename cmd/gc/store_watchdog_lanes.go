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
// A pass that is not due does nothing else, a suspended city's passes reach
// no store, and under FS pressure each lane sheds due passes on its own
// episode (fsPressureEpisode). Passes on one lane never overlap,
// and lanes never wait on each other or on the orders lane.
type storeWatchdogLane struct {
	spec storeWatchdogSpec

	// passMu serializes passes, and guards the watchdog's clocks and
	// fsPressure.
	passMu sync.Mutex

	// fsPressure is the lane's own FS pressure episode: the lane sheds and
	// forces its passes independently of the tick, the orders lane and the
	// other watchdog lanes.
	fsPressure fsPressureEpisode

	// The last attempt's status and the last completed pass, for the tick
	// trace (statusMu). Only work that finished uncanceled is a completed
	// pass, so a lane shed by pressure, suspended, canceled or blocked in a
	// store call shows a growing age.
	statusMu    sync.Mutex
	lastStatus  storeWatchdogPassStatus
	lastAt      time.Time
	completedAt time.Time
	completed   bool
}

// storeWatchdogPassStatus is how a lane's pass attempt ended.
type storeWatchdogPassStatus string

const (
	storeWatchdogPassNotDue          storeWatchdogPassStatus = "not_due"
	storeWatchdogPassSuspended       storeWatchdogPassStatus = "suspended"
	storeWatchdogPassPressureSkipped storeWatchdogPassStatus = "pressure_skipped"
	storeWatchdogPassCanceled        storeWatchdogPassStatus = "canceled"
	// storeWatchdogPassAborted is a pass whose work never returned: it
	// panicked, and safeTick recovered it.
	storeWatchdogPassAborted   storeWatchdogPassStatus = "aborted"
	storeWatchdogPassCompleted storeWatchdogPassStatus = "completed"
)

// storeWatchdogLaneStatus is a snapshot of a lane's status.
type storeWatchdogLaneStatus struct {
	last        storeWatchdogPassStatus
	lastAt      time.Time
	completedAt time.Time
	completed   bool
}

// noteStatus records how an attempt ended at at; a completed one is also the
// lane's last completed pass.
func (l *storeWatchdogLane) noteStatus(status storeWatchdogPassStatus, at time.Time) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	l.lastStatus, l.lastAt = status, at
	if status == storeWatchdogPassCompleted {
		l.completedAt, l.completed = at, true
	}
}

// status returns the lane's status snapshot.
func (l *storeWatchdogLane) status() storeWatchdogLaneStatus {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return storeWatchdogLaneStatus{last: l.lastStatus, lastAt: l.lastAt, completedAt: l.completedAt, completed: l.completed}
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
		run:  (*CityRuntime).runNudgeMailSweepWatchdog,
	},
}

const (
	// storeWatchdogLaneSafeTickTrigger names a lane in safeTick panic lines.
	storeWatchdogLaneSafeTickTrigger = "store-watchdog-lane"
	// storeWatchdogLaneTraceTrigger is the trace-cycle trigger for a due
	// pass; the cycle's trigger detail is the watchdog key.
	storeWatchdogLaneTraceTrigger TraceTickTrigger = "store_watchdog"
)

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

// storeWatchdogLaneSet is a started set of store watchdog lanes.
type storeWatchdogLaneSet struct {
	cancel context.CancelFunc
	done   []<-chan struct{}
}

// stop cancels every lane, then waits for each to exit. Canceling all of
// them first means a lane blocked in a store call does not hold the others'
// shutdown: they stop while it finishes the call it is in.
func (s storeWatchdogLaneSet) stop() {
	s.cancel()
	for _, done := range s.done {
		<-done
	}
}

// startStoreWatchdogLanes starts one goroutine per store watchdog, under a
// context derived from ctx. Each runs a pass one patrol interval (read once
// from the published config, as the orders lane reads it) after the previous
// pass ended. The caller must stop the returned set before the stores the
// watchdogs read are closed.
func (cr *CityRuntime) startStoreWatchdogLanes(ctx context.Context) storeWatchdogLaneSet {
	laneCtx, cancel := context.WithCancel(ctx)
	interval := cr.serviceConfigSnapshot().Daemon.PatrolIntervalDuration()
	lanes := cr.storeWatchdogLanesOf()
	set := storeWatchdogLaneSet{cancel: cancel, done: make([]<-chan struct{}, 0, len(lanes))}
	for _, lane := range lanes {
		set.done = append(set.done, startPacedLane(laneCtx, interval, 0, nil, func(bool) {
			cr.safeTick(func() {
				cr.runStoreWatchdogLanePass(laneCtx, lane)
			}, storeWatchdogLaneSafeTickTrigger+":"+string(lane.spec.key))
		}))
	}
	return set
}

// runStoreWatchdogLanePass is one pass of lane: nothing unless the watchdog
// is due, nothing in a suspended city, nothing while the lane sheds FS
// pressure, then the watchdog body against the published config. A skipped
// pass advances no watchdog clock, so the watchdog stays due and the next
// pass after the city resumes or the pressure episode forces it runs.
//
// Every attempt records its status for the tick. A not-due or suspended
// attempt does no I/O and writes no trace; a due attempt of a live city is a
// trace cycle of its own, triggered store_watchdog with the watchdog as its
// detail, whether the pressure gate sheds it or its work runs.
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
		lane.noteStatus(storeWatchdogPassNotDue, now)
		return
	}
	cfg := cr.serviceConfigSnapshot()
	// A suspended city's watchdogs read no store: every one reads every
	// scope's store, restarting the proxies suspension retired.
	if effectiveCitySuspended(cfg, loadSuspensionStateBestEffort(cr.cityPath)) {
		lane.noteStatus(storeWatchdogPassSuspended, now)
		return
	}

	key := string(lane.spec.key)
	trace := cr.beginStoreWatchdogTrace(cfg, lane.spec.key)
	status := storeWatchdogPassAborted
	defer func() {
		lane.noteStatus(status, time.Now())
		completion := TraceCompletionAborted
		if status == storeWatchdogPassCompleted || status == storeWatchdogPassPressureSkipped {
			completion = TraceCompletionCompleted
		}
		trace.end(completion, traceRecordPayload{"phase": "store_watchdog", "watchdog": key, "status": string(status)})
	}()

	// Only a due pass of a live city counts toward the lane's pressure
	// episode, so not-due attempts neither shed nor force anything.
	if lane.fsPressure.shouldSkip(cr.stderr, trace, storeWatchdogLaneSafeTickTrigger+":"+key, lane.spec.work) {
		status = storeWatchdogPassPressureSkipped
		return
	}
	if ctx.Err() != nil {
		status = storeWatchdogPassCanceled
		return
	}
	start := time.Now()
	lane.spec.run(cr, ctx, cfg, now)
	status, outcome := storeWatchdogPassCompleted, TraceOutcomeComplete
	if ctx.Err() != nil {
		status, outcome = storeWatchdogPassCanceled, TraceOutcomeCanceled
	}
	trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, outcome, key, time.Since(start),
		map[string]any{"watchdog": key, "status": string(status)})
}

// beginStoreWatchdogTrace opens the trace cycle of one due pass of the
// watchdog key, or returns nil when tracing is off.
func (cr *CityRuntime) beginStoreWatchdogTrace(cfg *config.City, key storeWatchdogKey) *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	return cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(storeWatchdogLaneTraceTrigger),
		TriggerDetail: string(key),
		CityPath:      cr.cityPath,
	}, cfg, nil)
}

// storeWatchdogLaneTickFields are the tick record's fields for the store
// watchdog lanes, each lane's under its key: the age of its last completed
// pass (backstop_ran false when it has none), and its last attempt's status
// and that attempt's age.
func (cr *CityRuntime) storeWatchdogLaneTickFields() map[string]any {
	fields := map[string]any{}
	for _, lane := range cr.storeWatchdogLanesOf() {
		st := lane.status()
		laneFields := map[string]any{}
		addBackstopAgeFields(laneFields, st.completedAt, "", st.completed)
		if st.last != "" {
			laneFields["last_status"] = string(st.last)
			laneFields["last_status_age_seconds"] = int(time.Since(st.lastAt).Round(time.Second) / time.Second)
		}
		for k, v := range laneFields {
			fields[string(lane.spec.key)+"_"+k] = v
		}
	}
	return fields
}
