package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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
// (#3206). The cost is bounded by one pass: a pass already in flight when the
// reload lands finishes on the outgoing dispatcher, as if the reload had
// arrived just after it.
//
// A reload publishes its config and then, when it stages a dispatcher, bumps
// the order-set generation under setMu. Each pass reads the generation and the
// one config snapshot everything in the pass reads — the trace, the periodic
// rescan and the watchdogs — as a pair under setMu (orderPassConfig), so no
// stage lands between them, and the rescan stages its result only if the
// generation is unchanged. A rescan whose pair predates a stage is therefore
// discarded, and one whose pair follows it reads the reloaded config. Without
// this, a rescan that read the pre-reload config could finish after the
// reload, overwrite the newer dispatcher and revert the order set.
type ordersLane struct {
	passMu sync.Mutex

	setMu      sync.Mutex
	pending    orderDispatcher
	hasPending bool
	// generation counts reload-side order-set publications (setMu).
	generation uint64

	// wakeCh carries "a tick would have dispatched orders now" signals.
	// Buffered 1, and wakes that must wait out the duty cycle join the one
	// pass that follows it, so a burst of wakes is never a queue of passes.
	wakeCh chan struct{}

	// Lane-local FS pressure episode, guarded by passMu. The tick's own
	// episode counters stay the tick's: the two paths shed independently, and
	// the lane ends its episode only when it sees pressure drop or forces a
	// pass. A tick forced every few seconds under pressure must not reset the
	// lane's count, or the lane would never reach its own forced pass.
	fsPressureSkips  int
	fsPressureLogged bool

	// Last pass that reached dispatch, for the tick trace (statusMu). A pass
	// the FS gate skipped does not count, so a lane starved by pressure shows
	// a growing age.
	statusMu       sync.Mutex
	lastPassAt     time.Time
	lastPassReason string
	passRan        bool

	// Pass counts by trigger, for observability and tests.
	cadencePasses atomic.Int64
	wakePasses    atomic.Int64
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

// wake asks the lane for a pass. Non-blocking; harmless when no lane goroutine
// is running.
func (l *ordersLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// notePass records a pass that reached dispatch, for the tick trace.
func (l *ordersLane) notePass(at time.Time, reason string) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	l.lastPassAt, l.lastPassReason, l.passRan = at, reason, true
}

// lastPass reports when the last pass that reached dispatch finished, what
// triggered it, and whether one has at all.
func (l *ordersLane) lastPass() (at time.Time, reason string, ran bool) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return l.lastPassAt, l.lastPassReason, l.passRan
}

// startOrdersLane starts the lane goroutine and returns a channel closed when
// it exits. It is paced by startPacedLane: a tick wake runs a pass once the
// lane has idled as long as its previous pass ran, and a backstop timer runs
// one patrol interval (read once, as the tick's ticker is) after the previous
// pass ended if nothing else has, the backstop for a wedged or slow tick.
//
// On maintainer-city, where a pass (~32s) outlasts the patrol interval (15s),
// the backstop comes first and the lane runs one pass per interval plus pass
// time, whatever the tick does.
func (cr *CityRuntime) startOrdersLane(ctx context.Context, cityRoot string) <-chan struct{} {
	lane := cr.ordersLaneOf()
	interval := cr.serviceConfigSnapshot().Daemon.PatrolIntervalDuration()
	return startPacedLane(ctx, interval, 0, lane.wakeCh, func(wake bool) {
		reason := ordersLaneReasonCadence
		if wake {
			reason = ordersLaneReasonWake
			lane.wakePasses.Add(1)
		} else {
			lane.cadencePasses.Add(1)
		}
		cr.safeTick(func() {
			cr.runOrdersLanePass(ctx, cityRoot, reason)
		}, ordersLaneSafeTickTrigger)
	})
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

	generation, cfg := cr.orderPassConfig()
	trace := cr.beginOrdersLaneTrace(cfg, reason)
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
	cr.dispatchOrdersLocked(ctx, cityRoot, generation, cfg)
	lane.notePass(time.Now(), reason)
	if trace != nil {
		trace.RecordControllerOperation(TraceSiteOrderDispatch, TraceReasonRetained, TraceOutcomeComplete,
			"dispatch_orders", time.Since(phaseStart), nil)
	}
	if ctx.Err() != nil {
		return
	}
	completion = TraceCompletionCompleted
}

// orderPassConfig returns the order-set generation and the one config snapshot
// a pass reads, as a pair read under setMu. A reload publishes its config and
// then bumps the generation under setMu when it stages, so no stage can land
// between the two reads: either the pair predates the stage (and the stage's
// bump discards a rescan of it) or it includes the stage's config. Nothing a
// pass runs reads cr.cfg itself.
func (cr *CityRuntime) orderPassConfig() (uint64, *config.City) {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	if cr.inOrderPassConfig != nil {
		cr.inOrderPassConfig(lane)
	}
	return lane.generation, cr.serviceConfigSnapshot()
}

// beginOrdersLaneTrace opens the pass's trace cycle from the pass's config
// snapshot. The config revision is omitted: it is written unlocked on the
// controller goroutine, and a lane pass is not a config-revision boundary.
func (cr *CityRuntime) beginOrdersLaneTrace(cfg *config.City, reason string) *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	return cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(ordersLaneTraceTrigger),
		TriggerDetail: reason,
		CityPath:      cr.cityPath,
	}, cfg, nil)
}

// ordersLaneShouldSkipForFSPressureLocked is the tick's pressure gate applied to the
// lane (passMu must be held): skip while pressure is high, but force a pass after
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
	generation, cfg := cr.orderPassConfig()
	cr.dispatchOrdersLocked(ctx, cityRoot, generation, cfg)
}

// dispatchOrdersLocked is the dispatch body. passMu must be held; generation
// and cfg come from orderPassConfig.
func (cr *CityRuntime) dispatchOrdersLocked(ctx context.Context, cityRoot string, generation uint64, cfg *config.City) {
	if ctx.Err() != nil {
		return
	}
	// A suspended city gets no order pass at all: dispatch already skips it,
	// and the tracking and mail watchdogs below would read every scope's
	// store, restarting the proxies suspension retired.
	if effectiveCitySuspended(cfg, loadSuspensionStateBestEffort(cr.cityPath)) {
		return
	}
	now := time.Now()
	if !cr.wispIndexMigrationApplied {
		cr.wispIndexMigrationApplied = true
		cr.applyWispQueryIndexes(ctx)
	}
	cr.rescanOrderDispatcherIfDue(cityRoot, cfg, generation, now)
	// Installs whatever is staged — this rescan's result, or a reload staged
	// while the previous pass held the lock — before anything dispatches
	// against the outgoing dispatcher.
	cr.installPendingOrderDispatcherLocked(ctx)
	cr.runOrderTrackingSweepWatchdog(cfg, now)
	cr.runOrderTrackingRetentionWatchdog(cfg, now)
	cr.runNudgeMailSweepWatchdog(cfg, now)
	if cr.od != nil {
		cr.od.dispatch(ctx, cityRoot, now)
	}
}

// stageOrderDispatcher is the reload-side stage: it records next as the
// dispatcher the lane should run and the order set it was built from, bumps
// the order-set generation so an in-flight lane rescan cannot overwrite it,
// and returns the change summary against the previously staged-or-live set.
// It never blocks on a pass. A staged dispatcher superseded before install
// never dispatched, so it has nothing to drain.
func (cr *CityRuntime) stageOrderDispatcher(next orderDispatcher, set []orders.Order, signature string, now time.Time) string {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	lane.generation++
	return cr.stageOrderDispatcherLocked(lane, next, set, signature, now)
}

// stageRescannedOrderDispatcher is the lane-rescan stage: it stages only if no
// reload published since generation was captured, and reports whether it did.
func (cr *CityRuntime) stageRescannedOrderDispatcher(generation uint64, next orderDispatcher, set []orders.Order, signature string, now time.Time) (string, bool) {
	lane := cr.ordersLaneOf()
	lane.setMu.Lock()
	defer lane.setMu.Unlock()
	if lane.generation != generation {
		return "", false
	}
	return cr.stageOrderDispatcherLocked(lane, next, set, signature, now), true
}

func (cr *CityRuntime) stageOrderDispatcherLocked(lane *ordersLane, next orderDispatcher, set []orders.Order, signature string, now time.Time) string {
	summary := orderSetChangeSummary(cr.orderSet, set)
	lane.pending = next
	lane.hasPending = true
	cr.orderSet = set
	cr.orderSetSignature = signature
	cr.orderRescanLast = now
	return summary
}

// scanOrderSet scans the order set, through the orderSetScan seam when set.
func (cr *CityRuntime) scanOrderSet(cityRoot string, cfg *config.City, cmdName string) (orderSetSnapshot, error) {
	if cr.orderSetScan != nil {
		return cr.orderSetScan(cityRoot, cfg, cmdName)
	}
	return scanOrderSetSnapshotFS(fsys.OSFS{}, cityRoot, cfg, cr.stderr, cmdName)
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
	return cr.orderRescanLast.IsZero() || now.Sub(cr.orderRescanLast) >= clock.Backstop(orderRescanInterval)
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
