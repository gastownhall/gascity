package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	labelOrderTracking = "order-tracking"

	orderTrackingSweepOrder                = "order-tracking-sweep"
	defaultOrderTrackingSweepStaleAfter    = 10 * time.Minute
	orderTrackingSweepWatchdogInterval     = 30 * time.Second
	orderTrackingSweepWatchdogStaleAfter   = 2 * time.Minute
	orderTrackingSweepMetadataReason       = "stale-order-tracking"
	orderTrackingSweepMetadataInitiator    = "order-tracking-sweep"
	orderTrackingWatchdogMetadataInitiator = "controller-watchdog"

	// orphanedOrderTrackingCloseReason is the canonical close_reason
	// stamped on orphan-sweep closes. It satisfies bd's
	// validation.on-close=error validator (which rejects closes without
	// an explicit --reason of >=20 characters) and provides a meaningful
	// audit trail in the closed bead's metadata. Without this, the close
	// is rejected, the bead stays open, and the next sweep tick re-stamps
	// identical metadata — generating one bead.updated event per tick per
	// bead.
	orphanedOrderTrackingCloseReason = "order-tracking sweep: orphaned by prior controller"

	// staleOrderTrackingCloseReason is the canonical close_reason stamped
	// on stale-sweep closes (both the periodic order-tracking-sweep order
	// and the controller's runtime watchdog). Same rationale as
	// orphanedOrderTrackingCloseReason — without an explicit reason of
	// >=20 chars, validation.on-close=error rejects every close, the
	// watchdog retries every 30s, and the order-firing pipeline silently
	// wedges (no bead.created/closed events, only metadata churn).
	staleOrderTrackingCloseReason = "order-tracking sweep: stale tracking bead exceeded retention window"

	completedOrderTrackingCloseReason = "order dispatch completed: tracking bead lifecycle finished"
)

// orderDispatcher evaluates order trigger conditions and dispatches due
// orders as wisps or exec scripts. Follows the nil-guard tracker pattern:
// nil means no auto-dispatchable orders exist.
//
// dispatch runs trigger evaluation synchronously, then spawns a goroutine
// per due order's dispatch action. The tracking bead is created before the
// goroutine launches to prevent re-fire on the next tick.
//
// drain waits for all in-flight dispatch goroutines spawned by prior
// dispatch calls to complete, bounded by ctx. It returns true when all
// tracked dispatches completed. Callers use this on controller exit and
// config reload to ensure tracking bead outcome metadata is persisted
// before the dispatcher is replaced or discarded.
type orderDispatcher interface {
	dispatch(ctx context.Context, cityPath string, now time.Time)
	drain(ctx context.Context) bool
}

// ExecRunner runs a shell command with context, working directory, and
// environment variables. Returns combined stdout or an error.
type ExecRunner func(ctx context.Context, command, dir string, env []string) ([]byte, error)

// shellExecRunner is the production ExecRunner using os/exec.
func shellExecRunner(ctx context.Context, command, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = mergeOrderExecEnv(cmd.Environ(), env)
	return cmd.CombinedOutput()
}

func mergeOrderExecEnv(environ, env []string) []string {
	out := mergeRuntimeEnv(environ, nil)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			out = removeEnvKey(out, key)
		}
	}
	return append(out, env...)
}

func logDispatchError(stderr io.Writer, format string, args ...any) {
	msg := execenv.RedactText(fmt.Sprintf(format, args...), os.Environ())
	log.Print(msg)
	if stderr != nil {
		fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
	}
}

// lockedWriter serializes Write calls so concurrent dispatchOne goroutines
// logging via logDispatchError(m.stderr, ...) do not interleave bytes.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// lockedStderr wraps w for storage on memoryOrderDispatcher.stderr. Returns
// nil unchanged so logDispatchError's nil-guard keeps its original semantics.
func lockedStderr(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &lockedWriter{w: w}
}

type orderStoreFunc func(execStoreTarget) (beads.Store, error)

// memoryOrderDispatcher is the production implementation.
//
// inflightN + inflightDone together track dispatchOne goroutines so
// drain can select on either completion or ctx.Done without spawning an
// orphaned waiter goroutine. dispatch is only ever called from the tick
// goroutine, so addInflight's check-and-create happens-before any
// concurrent drain call on the same instance.
//
// dispatchCtx is the parent context for every dispatchOne goroutine. The
// per-goroutine ctx is derived to cancel when EITHER the caller's tick
// ctx OR dispatchCtx is done (see launchDispatchOne). cancel() cancels
// dispatchCtx.
type memoryOrderDispatcher struct {
	aa           []orders.Order
	storeFn      orderStoreFunc
	ep           events.Provider
	execRun      ExecRunner
	rec          events.Recorder
	stderr       io.Writer
	maxTimeout   time.Duration
	cfg          *config.City
	cityName     string
	cacheMu      sync.Mutex
	lastRunCache map[string]time.Time

	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc

	inflightMu   sync.Mutex
	inflightN    int
	inflightDone chan struct{} // closed when inflightN returns to 0; nil when idle
}

// buildOrderDispatcher scans formula layers for orders and returns a
// dispatcher. Returns nil if no auto-dispatchable orders are found.
// Scans both city-level and per-rig orders. Rig orders get their Rig
// field stamped so they use independent scoped labels.
func buildOrderDispatcher(cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer) orderDispatcher {
	allAA, err := scanAllOrders(cityPath, cfg, stderr, "gc start: order scan")
	if err != nil {
		logDispatchError(stderr, "gc start: order scan: %v", err)
		return nil
	}
	if len(cfg.Orders.Overrides) > 0 {
		if err := orders.ApplyOverrides(allAA, convertOverrides(cfg.Orders.Overrides)); err != nil {
			logDispatchError(stderr, "gc start: order overrides: %v", err)
		}
	}

	// Filter out manual-trigger orders — they are never auto-dispatched.
	var auto []orders.Order
	for _, a := range allAA {
		if a.Trigger != "manual" {
			auto = append(auto, a)
		}
	}
	if len(auto) == 0 {
		return nil
	}

	// Extract events.Provider from recorder if available.
	// FileRecorder implements Provider; Discard does not.
	var ep events.Provider
	if p, ok := rec.(events.Provider); ok {
		ep = p
	}

	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	return &memoryOrderDispatcher{
		aa: auto,
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			return openStoreAtForCity(target.ScopeRoot, cityPath)
		},
		ep:             ep,
		execRun:        shellExecRunner,
		rec:            rec,
		stderr:         lockedStderr(stderr),
		maxTimeout:     cfg.Orders.MaxTimeoutDuration(),
		cfg:            cfg,
		cityName:       loadedCityName(cfg, cityPath),
		dispatchCtx:    dispatchCtx,
		dispatchCancel: dispatchCancel,
	}
}

func (m *memoryOrderDispatcher) dispatch(ctx context.Context, cityPath string, now time.Time) {
	// Skip all order dispatch when the city is suspended.
	if m.cfg != nil && citySuspended(m.cfg) {
		return
	}

	stores := make(map[string]beads.Store)

	for _, a := range m.aa {
		// Skip orders targeting suspended rigs.
		if m.orderRigSuspended(a) {
			continue
		}

		target, err := resolveOrderStoreTarget(cityPath, m.cfg, a)
		if err != nil {
			logDispatchError(m.stderr, "gc: order dispatch: resolving target for %s: %v", a.ScopedName(), err)
			continue
		}

		storeKey := orderStoreTargetKey(target)
		store, ok := stores[storeKey]
		if !ok {
			store, err = m.storeFn(target)
			if err != nil {
				logDispatchError(m.stderr, "gc: order dispatch: opening %s store for %s: %v", target.ScopeKind, a.ScopedName(), err)
				continue
			}
			stores[storeKey] = store
		}

		storesForGate := []beads.Store{store}
		legacyStore, legacyOK := m.legacyCityStoreForTarget(cityPath, target, stores)
		if !legacyOK {
			continue
		}
		if legacyStore != nil {
			storesForGate = append(storesForGate, legacyStore)
		}
		storeKeysForGate := []string{storeKey}
		if legacyStore != nil {
			storeKeysForGate = append(storeKeysForGate, orderStoreTargetKey(legacyOrderCityTarget(cityPath, m.cfg)))
		}
		baseLastRunFn := orders.LastRunAcrossStores(storesForGate...)
		var lastRunErr error
		var lastRunFromCache bool
		lastRunFn := func(orderName string) (time.Time, error) {
			last, fromCache, err := m.cachedLastRun(orderName, storeKeysForGate, baseLastRunFn)
			if err != nil {
				lastRunErr = err
			}
			if fromCache {
				lastRunFromCache = true
			}
			return last, err
		}
		cursorFn := orders.CursorAcrossStores(storesForGate...)
		if a.Trigger == "event" {
			cursor, err := bdCursorAcrossStores(a.ScopedName(), storesForGate...)
			if err != nil {
				logDispatchError(m.stderr, "gc: order dispatch: reading event cursor for %s: %v", a.ScopedName(), err)
				continue
			}
			cursorFn = func(string) uint64 {
				return cursor
			}
		}
		triggerOpts := orderTriggerOptionsForTarget(cityPath, m.cfg, target, a)
		result := orders.CheckTriggerWithOptions(a, now, lastRunFn, m.ep, cursorFn, triggerOpts)
		if lastRunErr != nil {
			logDispatchError(m.stderr, "gc: order dispatch: reading last run for %s: %v", a.ScopedName(), lastRunErr)
			continue
		}
		if !result.Due {
			continue
		}
		if lastRunFromCache && orderTriggerUsesLastRun(a) {
			refreshedLastRun, err := baseLastRunFn(a.ScopedName())
			if err != nil {
				logDispatchError(m.stderr, "gc: order dispatch: refreshing last run for %s: %v", a.ScopedName(), err)
				continue
			}
			if refreshedLastRun.After(result.LastRun) {
				m.rememberLastRun(a.ScopedName(), storeKeysForGate, refreshedLastRun)
				refreshedLastRunFn := func(string) (time.Time, error) {
					return refreshedLastRun, nil
				}
				result = orders.CheckTriggerWithOptions(a, now, refreshedLastRunFn, m.ep, cursorFn, triggerOpts)
				if !result.Due {
					continue
				}
			}
		}

		// Skip dispatch if previous work hasn't been processed yet.
		scoped := a.ScopedName()
		hasOpenWork, err := m.hasOpenWorkInStoresStrict(storesForGate, scoped)
		if err != nil {
			logDispatchError(m.stderr, "gc: order dispatch: checking open work for %s: %v", scoped, err)
			continue
		}
		if hasOpenWork {
			continue
		}

		// Create tracking bead synchronously BEFORE dispatch goroutine.
		// This prevents the cooldown trigger from re-firing on the next tick.
		trackingBead, err := store.Create(beads.Bead{
			Title:  "order:" + scoped,
			Labels: []string{"order-run:" + scoped, labelOrderTracking},
		})
		if err != nil {
			logDispatchError(m.stderr, "gc: order dispatch: creating tracking bead for %s: %v", scoped, err)
			continue
		}
		m.rememberLastRun(scoped, storeKeysForGate, trackingBead.CreatedAt)

		// Fire with timeout; inflight tracks the spawned goroutine so
		// drain can wait for tracking-bead outcome persistence before
		// controller exit or config reload.
		a := a // capture loop variable
		m.addInflight()
		m.launchDispatchOne(ctx, store, target, a, cityPath, trackingBead.ID)
	}
}

// launchDispatchOne spawns dispatchOne with a context that cancels when
// EITHER the caller's tick ctx OR m.dispatchCtx is done — required so
// cancel() reaches goroutines whose tick ctx was context.Background().
// Falls back to the bare caller ctx when m.dispatchCtx is nil (test
// sites that don't initialize the cancel fields).
func (m *memoryOrderDispatcher) launchDispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string) {
	if m.dispatchCtx == nil {
		go m.dispatchOne(ctx, store, target, a, cityPath, trackingID)
		return
	}
	mergedCtx, cancelMerged := context.WithCancel(ctx)
	stopAfter := context.AfterFunc(m.dispatchCtx, cancelMerged)
	go func() {
		defer stopAfter()
		defer cancelMerged()
		m.dispatchOne(mergedCtx, store, target, a, cityPath, trackingID)
	}()
}

// cancel signals all in-flight dispatchOne goroutines to terminate. Safe
// to call multiple times. Caller should follow with drain to wait for
// goroutine completion (exec.CommandContext propagates the cancel as
// SIGKILL; dispatchOne's deferred cleanup writes the tracking-bead
// outcome before doneInflight signals drain).
func (m *memoryOrderDispatcher) cancel() {
	if m.dispatchCancel != nil {
		m.dispatchCancel()
	}
}

// addInflight increments the in-flight count and lazily creates the done
// signal. Called synchronously from dispatch on the tick goroutine.
func (m *memoryOrderDispatcher) addInflight() {
	m.inflightMu.Lock()
	m.inflightN++
	if m.inflightN == 1 {
		m.inflightDone = make(chan struct{})
	}
	m.inflightMu.Unlock()
}

// doneInflight decrements the count and signals completion when the last
// goroutine finishes. Called from dispatchOne's deferred cleanup.
func (m *memoryOrderDispatcher) doneInflight() {
	m.inflightMu.Lock()
	m.inflightN--
	if m.inflightN == 0 && m.inflightDone != nil {
		close(m.inflightDone)
		m.inflightDone = nil
	}
	m.inflightMu.Unlock()
}

// drain blocks until all in-flight dispatchOne goroutines complete or ctx
// expires. It returns true when no work remains and returns immediately if
// nothing is in flight. When ctx expires, any still-running dispatches keep
// running (they will still write tracking-bead outcomes via ctx-unaware store
// calls); the startup sweep closes orphaned tracking beads on the next boot if
// drain did not have enough time to let them finish. The channel-signal design
// spawns no waiter goroutine and cannot leak state past return.
func (m *memoryOrderDispatcher) drain(ctx context.Context) bool {
	m.inflightMu.Lock()
	done := m.inflightDone
	m.inflightMu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *memoryOrderDispatcher) legacyCityStoreForTarget(cityPath string, target execStoreTarget, stores map[string]beads.Store) (beads.Store, bool) {
	if !legacyOrderCityFallbackNeeded(cityPath, target) {
		return nil, true
	}
	legacyTarget := legacyOrderCityTarget(cityPath, m.cfg)
	key := orderStoreTargetKey(legacyTarget)
	if store, ok := stores[key]; ok {
		return store, true
	}
	store, err := m.storeFn(legacyTarget)
	if err != nil {
		logDispatchError(m.stderr, "gc: order dispatch: opening legacy city store for rig order fallback: %v", err)
		return nil, false
	}
	stores[key] = store
	return store, true
}

func (m *memoryOrderDispatcher) cachedLastRun(orderName string, storeKeys []string, read orders.LastRunFunc) (time.Time, bool, error) {
	key := orderHistoryCacheKey(orderName, storeKeys)
	m.cacheMu.Lock()
	if m.lastRunCache != nil {
		if last, ok := m.lastRunCache[key]; ok {
			m.cacheMu.Unlock()
			return last, true, nil
		}
	}
	m.cacheMu.Unlock()

	last, err := read(orderName)
	if err != nil {
		return time.Time{}, false, err
	}
	m.rememberLastRun(orderName, storeKeys, last)
	return last, false, nil
}

func (m *memoryOrderDispatcher) rememberLastRun(orderName string, storeKeys []string, last time.Time) {
	key := orderHistoryCacheKey(orderName, storeKeys)
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.lastRunCache == nil {
		m.lastRunCache = make(map[string]time.Time)
	}
	if existing, ok := m.lastRunCache[key]; !ok || existing.IsZero() || last.After(existing) {
		m.lastRunCache[key] = last
	}
}

func orderHistoryCacheKey(orderName string, storeKeys []string) string {
	return orderName + "\x00" + strings.Join(storeKeys, "\x00")
}

func orderTriggerUsesLastRun(a orders.Order) bool {
	return a.Trigger == "cooldown" || a.Trigger == "cron"
}

func eventCursorLabels(scoped string, headSeq uint64) []string {
	return []string{
		fmt.Sprintf("order:%s", scoped),
		fmt.Sprintf("seq:%d", headSeq),
	}
}

// dispatchOne runs a single order dispatch in its own goroutine.
// For exec orders, runs the script directly. For formula orders,
// instantiates a wisp. Emits events and updates the tracking bead.
func (m *memoryOrderDispatcher) dispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string) {
	// Defer order matters: doneInflight runs last, after Close makes the
	// tracking bead outcome observable to a waiting drain.
	defer m.doneInflight()
	defer closeOrderTrackingBead(store, trackingID) //nolint:errcheck // best-effort close

	timeout := effectiveTimeout(a, m.maxTimeout)
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scoped := a.ScopedName()
	m.rec.Record(events.Event{
		Type:    events.OrderFired,
		Actor:   "controller",
		Subject: scoped,
	})

	if a.IsExec() {
		m.dispatchExec(childCtx, store, target, a, cityPath, trackingID)
	} else {
		m.dispatchWisp(childCtx, store, a, cityPath, trackingID)
	}
}

func closeOrderTrackingBead(store beads.Store, trackingID string) error {
	_, err := store.CloseAll([]string{trackingID}, map[string]string{
		"close_reason": completedOrderTrackingCloseReason,
	})
	return err
}

// dispatchExec runs an exec order's shell command.
func (m *memoryOrderDispatcher) dispatchExec(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string) {
	scoped := a.ScopedName()
	labels := []string{"exec"}
	var headSeq uint64
	var hasEventCursor bool
	if a.Trigger == "event" && m.ep != nil {
		var err error
		headSeq, err = m.ep.LatestSeq()
		if err != nil {
			errMsg := fmt.Sprintf("reading event cursor: %v", err)
			labels = []string{"exec-failed"}
			logDispatchError(m.stderr, "gc: order dispatch: reading event cursor for %s: %v", scoped, err)
			if updateErr := store.Update(trackingID, beads.UpdateOpts{Labels: labels}); updateErr != nil {
				logDispatchError(m.stderr, "gc: order %s: failed to label exec tracking bead %s: %v", scoped, trackingID, updateErr)
			}
			m.rec.Record(events.Event{
				Type:    events.OrderFailed,
				Actor:   "controller",
				Subject: scoped,
				Message: errMsg,
			})
			return
		}
		hasEventCursor = true
		// Event-triggered exec orders persist the cursor before the command
		// runs; otherwise a crash after the side effect can replay the event.
		if err := store.Update(trackingID, beads.UpdateOpts{Labels: eventCursorLabels(scoped, headSeq)}); err != nil {
			logDispatchError(m.stderr, "gc: order %s: failed to label exec event cursor on tracking bead %s: %v", scoped, trackingID, err)
			labels = []string{"exec-failed"}
			if updateErr := store.Update(trackingID, beads.UpdateOpts{Labels: labels}); updateErr != nil {
				logDispatchError(m.stderr, "gc: order %s: failed to label exec tracking bead %s: %v", scoped, trackingID, updateErr)
			}
			m.rec.Record(events.Event{
				Type:    events.OrderFailed,
				Actor:   "controller",
				Subject: scoped,
				Message: fmt.Sprintf("exec tracking bead %s event cursor label failed for seq=%d: %v", trackingID, headSeq, err),
			})
			return
		}
	}

	env := orderExecEnv(cityPath, m.cfg, target, a)
	output, err := m.execRun(ctx, a.Exec, target.ScopeRoot, env)
	var execErrMsg string
	if err != nil {
		redactionEnv := append(os.Environ(), env...)
		execErrMsg = execenv.RedactText(err.Error(), redactionEnv)
		labels = []string{"exec-failed"}
		logDispatchError(m.stderr, "gc: order exec %s failed: %s", scoped, execErrMsg)
		if len(output) > 0 {
			logDispatchError(m.stderr, "gc: order exec %s output: %s", scoped, execenv.RedactText(string(output), redactionEnv))
		}
	}

	// Label tracking bead with outcome via store (not CLI). For event execs,
	// cursor labels were already persisted before the command ran.
	if err := store.Update(trackingID, beads.UpdateOpts{Labels: labels}); err != nil {
		logDispatchError(m.stderr, "gc: order %s: failed to label exec tracking bead %s: %v", scoped, trackingID, err)
		msg := fmt.Sprintf("exec tracking bead %s label failed: %v", trackingID, err)
		if hasEventCursor {
			msg = fmt.Sprintf("seq=%d: %s", headSeq, msg)
		}
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: msg,
		})
		return
	}
	if execErrMsg != "" {
		if hasEventCursor {
			execErrMsg = fmt.Sprintf("seq=%d: %s", headSeq, execErrMsg)
		}
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: execErrMsg,
		})
		return
	}
	m.rec.Record(events.Event{
		Type:    events.OrderCompleted,
		Actor:   "controller",
		Subject: scoped,
	})
}

// dispatchWisp instantiates a wisp from the order's formula.
func (m *memoryOrderDispatcher) dispatchWisp(ctx context.Context, store beads.Store, a orders.Order, cityPath, trackingID string) {
	scoped := a.ScopedName()

	if err := ctx.Err(); err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-canceled"}}) //nolint:errcheck // best-effort
		return
	}

	// Capture event head before wisp creation for event triggers. Event runs
	// fail closed when the cursor cannot be read.
	var headSeq uint64
	if a.Trigger == "event" && m.ep != nil {
		var err error
		headSeq, err = m.ep.LatestSeq()
		if err != nil {
			errMsg := fmt.Sprintf("reading event cursor: %v", err)
			logDispatchError(m.stderr, "gc: order dispatch: reading event cursor for %s: %v", scoped, err)
			m.rec.Record(events.Event{
				Type:    events.OrderFailed,
				Actor:   "controller",
				Subject: scoped,
				Message: errMsg,
			})
			m.markTrackingFailure(store, trackingID, scoped, a, 0)
			return
		}
	}

	var searchPaths []string
	if a.FormulaLayer != "" {
		searchPaths = []string{a.FormulaLayer}
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, a.Formula, searchPaths, nil)
	if err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		m.markTrackingFailure(store, trackingID, scoped, a, headSeq)
		return
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, molecule.Options{}); err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		m.markTrackingFailure(store, trackingID, scoped, a, headSeq)
		return
	}

	var pool string
	if a.Pool != "" {
		pool, err = qualifyOrderPool(a, m.cfg)
		if err != nil {
			logDispatchError(m.stderr, "gc: order %s: %v", scoped, err)
			m.rec.Record(events.Event{
				Type:    events.OrderFailed,
				Actor:   "controller",
				Subject: scoped,
				Message: err.Error(),
			})
			m.markTrackingFailure(store, trackingID, scoped, a, headSeq)
			return
		}
	}

	// Decorate graph workflow recipes with routing metadata so child step
	// beads get gc.routed_to set before instantiation.
	if a.Pool != "" {
		if err := applyGraphRouting(recipe, nil, pool, nil, "", "", "", "", store, m.cityName, cityPath, m.cfg); err != nil {
			logDispatchError(m.stderr, "gc: order %s: routing decoration failed: %v", scoped, err)
			// Non-fatal — molecule still works, just without step-level routing.
		}
	}

	cookResult, err := molecule.Instantiate(ctx, store, recipe, molecule.Options{})
	if err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		m.markTrackingFailure(store, trackingID, scoped, a, headSeq)
		return
	}
	rootID := cookResult.RootID

	// Stamp the created wisp through the store contract rather than a raw
	// bd subprocess so controller dispatch stays provider-aware.
	update := beads.UpdateOpts{Labels: []string{"order-run:" + scoped}}
	if a.Trigger == "event" && m.ep != nil {
		update.Labels = append(update.Labels,
			fmt.Sprintf("order:%s", scoped),
			fmt.Sprintf("seq:%d", headSeq),
		)
	}
	if a.Pool != "" {
		update.Metadata = map[string]string{"gc.routed_to": pool}
	}
	if err := store.Update(rootID, update); err != nil {
		// Label failure is critical for duplicate-dispatch prevention.
		// Log and emit an event so operators can investigate.
		logDispatchError(m.stderr, "gc: order %s: failed to label wisp %s: %v", scoped, rootID, err)
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: fmt.Sprintf("wisp %s created but label failed: %v", rootID, err),
		})
		m.markTrackingFailure(store, trackingID, scoped, a, headSeq)
		return
	}

	m.rec.Record(events.Event{
		Type:    events.OrderCompleted,
		Actor:   "controller",
		Subject: scoped,
	})

	// Label tracking bead with outcome.
	store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp"}}) //nolint:errcheck // best-effort
}

// orderRigSuspended reports whether the order targets a suspended rig.
// It derives the effective target rig from the qualified pool (after
// rig-prefix resolution) using the canonical ParseQualifiedName parser,
// then checks whether that rig is suspended.
func (m *memoryOrderDispatcher) orderRigSuspended(a orders.Order) bool {
	if m.cfg == nil {
		return false
	}
	qualified, err := qualifyOrderPool(a, m.cfg)
	if err != nil {
		return m.rigSuspendedByName(a.Rig)
	}
	rigName, _ := config.ParseQualifiedName(qualified)
	if rigName == "" {
		rigName = a.Rig
	}
	return m.rigSuspendedByName(rigName)
}

func (m *memoryOrderDispatcher) markTrackingFailure(store beads.Store, trackingID, scoped string, a orders.Order, headSeq uint64) {
	labels := []string{"wisp", "wisp-failed"}
	if a.Trigger == "event" && headSeq > 0 {
		labels = append(labels, eventCursorLabels(scoped, headSeq)...)
	}
	if err := store.Update(trackingID, beads.UpdateOpts{Labels: labels}); err != nil {
		logDispatchError(m.stderr, "gc: order %s: failed to mark tracking bead %s as failed: %v", scoped, trackingID, err)
	}
}

func (m *memoryOrderDispatcher) rigSuspendedByName(rigName string) bool {
	if rigName == "" {
		return false
	}
	for _, r := range m.cfg.Rigs {
		if r.Name == rigName {
			return r.Suspended
		}
	}
	return false
}

// hasOpenWorkStrict reports whether an open tracking bead exists for this
// order — i.e. a dispatchOne goroutine is still in flight. Tracking beads
// carry both "order-run:<scoped>" and labelOrderTracking; dispatchOne closes
// them via defer when dispatch returns.
//
// Wisp root beads also carry "order-run:<scoped>" (so gc order history and
// the orders API feed can attribute the wisp to its order), but molecule
// roots never auto-close when their step beads finish. A leftover open
// root is not in-flight work and must not block re-dispatch — counting it
// caused ga-jra/ga-lo8c, where formula+pool orders stalled indefinitely
// after a city restart because the first auto-fire's wisp root permanently
// tripped this check.
func (m *memoryOrderDispatcher) hasOpenWorkStrict(store beads.Store, scopedName string) (bool, error) {
	results, err := store.List(beads.ListQuery{
		Label: "order-run:" + scopedName,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return false, fmt.Errorf("listing order work beads: %w", err)
	}
	for _, b := range results {
		if b.Status == "closed" {
			continue
		}
		for _, lbl := range b.Labels {
			if lbl == labelOrderTracking {
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *memoryOrderDispatcher) hasOpenWorkInStoresStrict(stores []beads.Store, scopedName string) (bool, error) {
	for _, store := range stores {
		if store == nil {
			continue
		}
		hasOpen, err := m.hasOpenWorkStrict(store, scopedName)
		if err != nil {
			return false, err
		}
		if hasOpen {
			return true, nil
		}
	}
	return false, nil
}

// sweepOrphanedOrderTracking closes any open order-tracking beads left
// behind by a previous controller instance. Returns the count of beads
// closed. This is non-fatal: dispatch proceeds even if the sweep fails.
func sweepOrphanedOrderTracking(store beads.Store) (int, error) {
	// ListByLabel without IncludeClosed returns only open beads.
	all, err := store.ListByLabel(labelOrderTracking, 0)
	if err != nil {
		return 0, fmt.Errorf("listing order-tracking beads: %w", err)
	}
	if len(all) == 0 {
		return 0, nil
	}
	ids := make([]string, len(all))
	for i, b := range all {
		ids[i] = b.ID
	}
	n, err := store.CloseAll(ids, map[string]string{
		"close_reason": orphanedOrderTrackingCloseReason,
	})
	if err != nil {
		return n, fmt.Errorf("closing orphaned order-tracking beads: %w", err)
	}
	return n, nil
}

// sweepStaleOrderTracking closes open order-tracking beads whose creation
// timestamp is older than staleAfter. When onlyOrders is non-empty, it only
// closes tracking beads for those scoped order names.
func sweepStaleOrderTracking(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string) (int, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("stale-after must be positive")
	}
	all, err := store.ListByLabel(labelOrderTracking, 0)
	if err != nil {
		return 0, fmt.Errorf("listing order-tracking beads: %w", err)
	}

	cutoff := now.Add(-staleAfter)
	var ids []string
	for _, b := range all {
		if len(onlyOrders) > 0 {
			name, ok := orderNameFromTrackingBead(b)
			if !ok {
				continue
			}
			if _, ok := onlyOrders[name]; !ok {
				continue
			}
		}
		if b.CreatedAt.IsZero() || b.CreatedAt.After(cutoff) {
			continue
		}
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	metadata := map[string]string{
		"order_tracking_sweep": orderTrackingSweepMetadataReason,
		"close_reason":         staleOrderTrackingCloseReason,
	}
	if initiator != "" {
		metadata["order_tracking_sweep_by"] = initiator
	}
	n, err := store.CloseAll(ids, metadata)
	if err != nil {
		return n, fmt.Errorf("closing stale order-tracking beads: %w", err)
	}
	return n, nil
}

func orderNameFromTrackingBead(b beads.Bead) (string, bool) {
	for _, label := range b.Labels {
		if name, ok := strings.CutPrefix(label, "order-run:"); ok && name != "" {
			return name, true
		}
	}
	if name, ok := strings.CutPrefix(b.Title, "order:"); ok && name != "" {
		return name, true
	}
	return "", false
}

// sweepOrphanedOrderTrackingRetry calls sweepOrphanedOrderTracking with
// bounded retries. On startup the bead store's backing server may not be
// query-ready yet (dolt cold-start race, #753). Errors are retried; the
// total count of beads closed across attempts is returned. Retrying on
// partial closes is safe because beads.Store.CloseAll skips already-closed
// beads (see internal/beads/beads.go). The wrapper sleeps for up to
// attempts*backoff in the worst case.
func sweepOrphanedOrderTrackingRetry(store beads.Store, attempts int, backoff time.Duration) (int, error) { //nolint:unparam // attempts is configurable for testability
	if attempts <= 0 {
		attempts = 1
	}
	total := 0
	var err error
	for i := range attempts {
		var n int
		n, err = sweepOrphanedOrderTracking(store)
		total += n
		if err == nil {
			return total, nil
		}
		if i == attempts-1 {
			return total, fmt.Errorf("sweep failed after %d attempts: %w", attempts, err)
		}
		time.Sleep(backoff)
	}
	return total, err
}

// effectiveTimeout returns the timeout to use for an order dispatch.
// Uses the order's configured timeout (or default), capped by maxTimeout.
func effectiveTimeout(a orders.Order, maxTimeout time.Duration) time.Duration {
	t := a.TimeoutOrDefault()
	if maxTimeout > 0 && t > maxTimeout {
		return maxTimeout
	}
	return t
}

// rigExclusiveLayers returns the suffix of rigLayers that is not in
// cityLayers. Since rig layers are built as [cityLayers..., rigTopoLayers...,
// rigLocalLayer], we strip the city prefix to avoid double-scanning city
// orders.
func rigExclusiveLayers(rigLayers, cityLayers []string) []string {
	if len(rigLayers) <= len(cityLayers) {
		return nil
	}
	return rigLayers[len(cityLayers):]
}

// qualifyPool resolves a raw pool name from an order TOML to the qualified
// form used by Agent.QualifiedName() — the same string the scaler queries
// via gc.routed_to. Three layers of qualification stack:
//
//  1. If pool already contains "/" it is rig-qualified — pass through.
//  2. If pool exactly matches a configured binding-qualified target
//     ("binding.name"), preserve that target and still stack the rig prefix
//     when present.
//  3. If the order came from an imported pack, prefer same-source agents when
//     resolving a bare pool name so pack-local orders stay pack-local even if
//     other scopes also export the same bare agent name.
//  4. Otherwise look up agents in cfg.Agents whose Dir matches rig
//     (city orders use rig=="") and Name matches pool. If exactly one target
//     resolves, swap pool for the binding-qualified form ("binding.name")
//     before any rig prefixing. This handles V2 pack imports where the
//     dispatched wisp must carry "binding.name" so the agent's default
//     scale_check matches its own qualified name.
//
// Ambiguity is a hard failure: silently stamping the bare pool string would
// recreate the exact route/scaler mismatch this helper exists to prevent.
// nil cfg preserves the rig-only behavior so call sites without a loaded
// city remain stable. Dotted values that do not match a configured bound
// target are preserved for backward compatibility.
func qualifyOrderPool(a orders.Order, cfg *config.City) (string, error) {
	return qualifyPool(a.Pool, a.Rig, cfg, orderPoolSourceDirHint(a))
}

func orderPoolSourceDirHint(a orders.Order) string {
	if a.FormulaLayer == "" {
		return ""
	}
	return filepath.Clean(filepath.Dir(a.FormulaLayer))
}

func qualifyPool(pool, rig string, cfg *config.City, sourceDirHint string) (string, error) {
	if strings.Contains(pool, "/") {
		return pool, nil
	}
	if cfg == nil {
		if rig == "" {
			return pool, nil
		}
		return rig + "/" + pool, nil
	}

	qualified := pool
	scope := "city order"
	if rig != "" {
		scope = fmt.Sprintf("rig %q", rig)
	}

	var exactQualified []string
	var sourceScopedMatches []string
	var localBareMatches []string
	var bareMatches []string
	cleanHint := ""
	if sourceDirHint != "" {
		cleanHint = filepath.Clean(sourceDirHint)
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir != rig {
			continue
		}
		switch {
		case strings.Contains(pool, ".") && a.BindingQualifiedName() == pool:
			exactQualified = appendUniquePoolTarget(exactQualified, a.BindingQualifiedName())
		case a.Name == pool:
			bareMatches = appendUniquePoolTarget(bareMatches, a.BindingQualifiedName())
			if a.BindingName == "" {
				localBareMatches = appendUniquePoolTarget(localBareMatches, a.BindingQualifiedName())
			}
			if cleanHint != "" && filepath.Clean(a.SourceDir) == cleanHint {
				sourceScopedMatches = appendUniquePoolTarget(sourceScopedMatches, a.BindingQualifiedName())
			}
		}
	}

	switch {
	case len(exactQualified) == 1:
		qualified = exactQualified[0]
	case len(exactQualified) > 1:
		return "", fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(exactQualified, ", "))
	case len(sourceScopedMatches) == 1:
		qualified = sourceScopedMatches[0]
	case len(sourceScopedMatches) > 1:
		return "", fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(sourceScopedMatches, ", "))
	case len(localBareMatches) == 1:
		qualified = localBareMatches[0]
	case len(localBareMatches) > 1:
		return "", fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(localBareMatches, ", "))
	case len(bareMatches) == 1:
		qualified = bareMatches[0]
	case len(bareMatches) > 1:
		return "", fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(bareMatches, ", "))
	}

	if rig == "" {
		return qualified, nil
	}
	return rig + "/" + qualified, nil
}

func appendUniquePoolTarget(values []string, want string) []string {
	for _, value := range values {
		if value == want {
			return values
		}
	}
	return append(values, want)
}

// convertOverrides converts config.OrderOverride to orders.Override.
func convertOverrides(cfgOvs []config.OrderOverride) []orders.Override {
	out := make([]orders.Override, len(cfgOvs))
	for i, c := range cfgOvs {
		out[i] = orders.Override{
			Name:     c.Name,
			Rig:      c.Rig,
			Enabled:  c.Enabled,
			Trigger:  c.Trigger,
			Interval: c.Interval,
			Schedule: c.Schedule,
			Check:    c.Check,
			On:       c.On,
			Pool:     c.Pool,
			Timeout:  c.Timeout,
		}
	}
	return out
}
