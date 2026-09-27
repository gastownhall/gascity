package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// Order dispatch runs on its own lane, off the controller tick.
//
// On maintainer-city dispatch_orders cost ~32s p50 of every tick while the
// controller goroutine was 97-100% busy in ticks, so everything queued behind
// the tick — reload replies, pokes, convergence — waited on order gates, and a
// due order waited on every session phase. The lane is the same shape as the
// route-recovery and completions backstop lanes: its own goroutine, its own
// cadence, safeTick recovery per pass, stopped by the city context.
//
// What the lane owns, and why it can:
//
//   - The live dispatcher (cr.od), the retired dispatchers awaiting drain, and
//     the dispatch-side watchdog clocks are held under passMu. A pass holds it
//     for its whole length; nothing else dispatches.
//   - The order-set bookkeeping (cr.orderSet, cr.orderSetSignature,
//     cr.orderRescanLast) and a staged replacement dispatcher are held under
//     setMu, because a config reload on the controller goroutine writes them.
//
// A reload never waits for a pass. It stages the rebuilt dispatcher and tries
// passMu: an idle lane installs it immediately (the pre-lane behavior, and what
// every directly-driven test observes); a busy lane installs it at the start of
// its next pass, draining the outgoing dispatcher first exactly as the reload
// did. Waiting would make reload-reply latency scale with order count again
// (#3206).
type ordersLane struct {
	passMu sync.Mutex

	setMu      sync.Mutex
	pending    orderDispatcher
	hasPending bool

	// wakeCh coalesces "a tick would have dispatched orders now" signals into
	// at most one extra pass. Buffered 1: a wake that lands mid-pass runs one
	// more pass after it, never a queue of them.
	wakeCh chan struct{}

	// Lane-local FS pressure episode, guarded by passMu. The tick's own episode
	// counters stay the tick's: the two paths shed independently.
	fsPressureSkips  int
	fsPressureLogged bool
}

const (
	// ordersLaneSafeTickTrigger names the lane in safeTick panic lines.
	ordersLaneSafeTickTrigger = "orders-lane"
	// ordersLaneTraceTrigger is the trace-cycle trigger for lane passes. The
	// dispatch_orders operation record keeps its site and name; only the cycle
	// it lands in changes, from a controller tick to an orders pass.
	ordersLaneTraceTrigger TraceTickTrigger = "orders"

	ordersLaneReasonCadence = "cadence"
	ordersLaneReasonWake    = "tick_wake"
)

func newOrdersLane() *ordersLane {
	return &ordersLane{wakeCh: make(chan struct{}, 1)}
}

// ordersLaneOf returns this runtime's lane, creating it on first use so a
// directly-constructed CityRuntime needs no wiring.
func (cr *CityRuntime) ordersLaneOf() *ordersLane {
	cr.ordersLaneOnce.Do(func() { cr.ordersLane = newOrdersLane() })
	return cr.ordersLane
}

// wake asks the lane for a pass as soon as it is free. Non-blocking; harmless
// when no lane goroutine is running.
func (l *ordersLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// startOrdersLane starts the lane goroutine and returns a channel closed when
// it exits. The cadence is the patrol interval — the rate the tick used to
// offer orders a pass — read once, as the tick's own ticker is. Ticks also wake
// the lane, so a poke-driven tick still gets a prompt dispatch.
func (cr *CityRuntime) startOrdersLane(ctx context.Context, cityRoot string) <-chan struct{} {
	lane := cr.ordersLaneOf()
	interval := cr.serviceConfigSnapshot().Daemon.PatrolIntervalDuration()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			reason := ordersLaneReasonCadence
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-lane.wakeCh:
				reason = ordersLaneReasonWake
			}
			cr.safeTick(func() {
				cr.runOrdersLanePass(ctx, cityRoot, reason)
			}, ordersLaneSafeTickTrigger)
		}
	}()
	return done
}

// runOrdersLanePass is one lane pass: the FS-pressure gate, then the
// managed-Dolt preflight, then dispatch — the order the tick ran them in, so a
// pressure-skipped or endpoint-repair pass writes no tracking first.
func (cr *CityRuntime) runOrdersLanePass(ctx context.Context, cityRoot, reason string) {
	if ctx.Err() != nil {
		return
	}
	lane := cr.ordersLaneOf()
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	// The lock may have been held by a reload's drain; shutdown can begin
	// meanwhile, and then the pass must not read pressure or preflight.
	if ctx.Err() != nil {
		return
	}

	trace := cr.beginOrdersLaneTrace(reason)
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "orders", "reason": reason})
		}
	}()

	if cr.ordersLaneShouldSkipForFSPressureLocked(lane, trace, reason) {
		completion = TraceCompletionCompleted
		return
	}

	phaseStart := time.Now()
	cr.ensureManagedDoltPublishedForTick()
	if trace != nil {
		trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, TraceOutcomeComplete,
			"managed_dolt_preflight", time.Since(phaseStart), nil)
	}
	if ctx.Err() != nil {
		return
	}

	phaseStart = time.Now()
	cr.dispatchOrdersLocked(ctx, cityRoot)
	if trace != nil {
		trace.RecordControllerOperation(TraceSiteOrderDispatch, TraceReasonRetained, TraceOutcomeComplete,
			"dispatch_orders", time.Since(phaseStart), nil)
	}
	if ctx.Err() != nil {
		return
	}
	completion = TraceCompletionCompleted
}

// beginOrdersLaneTrace opens the pass's trace cycle from the published config.
// The config revision is omitted: it is written unlocked on the controller
// goroutine, and a lane pass is not a config-revision boundary.
func (cr *CityRuntime) beginOrdersLaneTrace(reason string) *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	return cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(ordersLaneTraceTrigger),
		TriggerDetail: reason,
		CityPath:      cr.cityPath,
	}, cr.serviceConfigSnapshot(), nil)
}

// ordersLaneShouldSkipForFSPressureLocked is the tick's pressure gate applied
// to the lane: skip while pressure is high, but force a pass after
// maxConsecutiveFSPressureSkips so orders cannot starve. It logs once per
// episode and records the decision on the lane's trace; the supervisor
// skipped-tick event stays the tick's, so its counts keep meaning ticks.
func (cr *CityRuntime) ordersLaneShouldSkipForFSPressureLocked(lane *ordersLane, trace *sessionReconcilerTraceCycle, reason string) bool {
	status, ok := currentFSPressureStatus(cr.stderr)
	if !ok || !status.High {
		lane.fsPressureSkips = 0
		lane.fsPressureLogged = false
		return false
	}
	trigger := ordersLaneSafeTickTrigger + ":" + reason
	if lane.fsPressureSkips >= maxConsecutiveFSPressureSkips {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), forcing order dispatch after %d skipped passes\n", //nolint:errcheck // best-effort stderr
				status.Avg60, status.Threshold, lane.fsPressureSkips)
		}
		recordFSPressureForcedTickTrace(trace, trigger, status, lane.fsPressureSkips)
		lane.fsPressureSkips = 0
		lane.fsPressureLogged = false
		return false
	}
	lane.fsPressureSkips++
	if !lane.fsPressureLogged && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), skipping order dispatch\n", //nolint:errcheck // best-effort stderr
			status.Avg60, status.Threshold)
	}
	lane.fsPressureLogged = true
	recordFSPressureSkippedTickTrace(trace, trigger, status, lane.fsPressureSkips)
	return true
}

// dispatchOrders runs one dispatch outside the lane goroutine — the startup
// pass, which must precede the cold-start session reconcile (MAINT-008). It
// takes the same lock a lane pass does.
func (cr *CityRuntime) dispatchOrders(ctx context.Context, cityRoot string) {
	lane := cr.ordersLaneOf()
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	cr.dispatchOrdersLocked(ctx, cityRoot)
}

// dispatchOrdersLocked is the dispatch body. passMu must be held.
func (cr *CityRuntime) dispatchOrdersLocked(ctx context.Context, cityRoot string) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	// A reload staged while the previous pass held the lock is installed
	// before anything dispatches against the outgoing dispatcher.
	cr.installPendingOrderDispatcherLocked(ctx)
	if !cr.wispIndexMigrationApplied {
		cr.wispIndexMigrationApplied = true
		cr.applyWispQueryIndexes(ctx)
	}
	cr.rescanOrderDispatcherIfDue(ctx, cityRoot, now)
	cr.installPendingOrderDispatcherLocked(ctx)
	cr.runOrderTrackingSweepWatchdog(now)
	cr.runOrderTrackingRetentionWatchdog(now)
	cr.runNudgeMailSweepWatchdog(now)
	if cr.od != nil {
		cr.od.dispatch(ctx, cityRoot, now)
	}
}

// stageOrderDispatcher records next as the dispatcher the lane should run and
// the order set it was built from, returning the change summary against the
// previously staged-or-live set. It never blocks on a pass. A staged dispatcher
// superseded before install never dispatched, so it has nothing to drain.
func (cr *CityRuntime) stageOrderDispatcher(next orderDispatcher, set []orders.Order, signature string, now time.Time) string {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	summary := orderSetChangeSummary(cr.orderSet, set)
	lane.pending = next
	lane.hasPending = true
	cr.orderSet = set
	cr.orderSetSignature = signature
	cr.orderRescanLast = now
	return summary
}

// tryInstallPendingOrderDispatcher installs a staged dispatcher now if no pass
// holds the lane; otherwise the running pass's successor installs it.
func (cr *CityRuntime) tryInstallPendingOrderDispatcher(ctx context.Context) {
	lane := cr.ordersLaneOf()
	if !lane.passMu.TryLock() {
		return
	}
	defer lane.passMu.Unlock()
	cr.installPendingOrderDispatcherLocked(ctx)
}

// installPendingOrderDispatcherLocked drains the live dispatcher and swaps in
// the staged one, carrying its warm state (#3201). passMu must be held, which
// is what guarantees no dispatch creates new in-flight work on the outgoing
// dispatcher while drain observes it. The drain is capped at
// reloadOrderDrainTimeout and derives from ctx, so shutdown short-circuits it;
// a timed-out dispatcher is retained for the shutdown drain.
func (cr *CityRuntime) installPendingOrderDispatcherLocked(ctx context.Context) {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	next, ok := lane.pending, lane.hasPending
	lane.pending, lane.hasPending = nil, false
	lane.setMu.Unlock()
	if !ok {
		return
	}
	if cr.od != nil {
		drainCtx, drainCancel := context.WithTimeout(ctx, reloadOrderDrainTimeout)
		cr.drainOutgoingOrderDispatcher(drainCtx, cr.od)
		drainCancel()
	}
	cr.replaceOrderDispatcher(next)
}

// orderRescanDue reports whether the periodic order rescan should run.
func (cr *CityRuntime) orderRescanDue(now time.Time) bool {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	return cr.orderRescanLast.IsZero() || now.Sub(cr.orderRescanLast) >= orderRescanInterval
}

// markOrderRescan records a rescan attempt so a failing scan is retried on the
// rescan interval, not every pass.
func (cr *CityRuntime) markOrderRescan(now time.Time) {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	cr.orderRescanLast = now
}

// orderSetUnchanged records the scan time and reports whether signature is the
// order set already staged or live.
func (cr *CityRuntime) orderSetUnchanged(signature string, now time.Time) bool {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	cr.orderRescanLast = now
	return signature == cr.orderSetSignature
}
