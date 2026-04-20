package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
)

const labelOrderTracking = "order-tracking"

// orderDispatcher evaluates order trigger conditions and dispatches due
// orders as wisps or exec scripts. Follows the nil-guard tracker pattern:
// nil means no auto-dispatchable orders exist.
//
// dispatch is fire-and-forget: trigger evaluation is synchronous, but each due
// order's dispatch action runs in its own goroutine. The tracking bead
// is created before the goroutine launches to prevent re-fire on the next tick.
type orderDispatcher interface {
	dispatch(ctx context.Context, cityPath string, now time.Time)
}

// ExecRunner runs a shell command with context, working directory, and
// environment variables. Returns combined stdout or an error.
type ExecRunner func(ctx context.Context, command, dir string, env []string) ([]byte, error)

// shellExecRunner is the production ExecRunner using os/exec.
func shellExecRunner(ctx context.Context, command, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), env...)
	return cmd.CombinedOutput()
}

func logDispatchError(stderr io.Writer, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	if stderr != nil {
		fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
	}
}

type orderStoreFunc func(execStoreTarget) (beads.Store, error)

// memoryOrderDispatcher is the production implementation.
type memoryOrderDispatcher struct {
	aa         []orders.Order
	storeFn    orderStoreFunc
	ep         events.Provider
	execRun    ExecRunner
	rec        events.Recorder
	stderr     io.Writer
	maxTimeout time.Duration
	cfg        *config.City
	cityName   string
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

	return &memoryOrderDispatcher{
		aa: auto,
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			return openStoreAtForCity(target.ScopeRoot, cityPath)
		},
		ep:         ep,
		execRun:    shellExecRunner,
		rec:        rec,
		stderr:     stderr,
		maxTimeout: cfg.Orders.MaxTimeoutDuration(),
		cfg:        cfg,
		cityName:   loadedCityName(cfg, cityPath),
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
		baseLastRunFn := orders.LastRunAcrossStores(storesForGate...)
		var lastRunErr error
		lastRunFn := func(orderName string) (time.Time, error) {
			last, err := baseLastRunFn(orderName)
			if err != nil {
				lastRunErr = err
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
		result := orders.CheckTrigger(a, now, lastRunFn, m.ep, cursorFn)
		if lastRunErr != nil {
			logDispatchError(m.stderr, "gc: order dispatch: reading last run for %s: %v", a.ScopedName(), lastRunErr)
			continue
		}
		if !result.Due {
			continue
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

		// Fire and forget with timeout.
		a := a // capture loop variable
		go m.dispatchOne(ctx, store, target, a, cityPath, trackingBead.ID)
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

// dispatchOne runs a single order dispatch in its own goroutine.
// For exec orders, runs the script directly. For formula orders,
// instantiates a wisp. Emits events and updates the tracking bead.
func (m *memoryOrderDispatcher) dispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string) {
	defer store.Close(trackingID) //nolint:errcheck // best-effort close

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

// dispatchExec runs an exec order's shell command.
func (m *memoryOrderDispatcher) dispatchExec(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string) {
	scoped := a.ScopedName()
	labels := []string{"exec"}

	env := orderExecEnv(cityPath, m.cfg, target, a)
	output, err := m.execRun(ctx, a.Exec, target.ScopeRoot, env)
	if err != nil {
		labels = append(labels, "exec-failed")
		logDispatchError(m.stderr, "gc: order exec %s failed: %v", scoped, err)
		if len(output) > 0 {
			logDispatchError(m.stderr, "gc: order exec %s output: %s", scoped, output)
		}
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
	} else {
		m.rec.Record(events.Event{
			Type:    events.OrderCompleted,
			Actor:   "controller",
			Subject: scoped,
		})
	}

	// Label tracking bead with outcome via store (not CLI).
	store.Update(trackingID, beads.UpdateOpts{Labels: labels}) //nolint:errcheck // best-effort
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

	// Capture event head before wisp creation for event triggers.
	var headSeq uint64
	if a.Trigger == "event" && m.ep != nil {
		headSeq, _ = m.ep.LatestSeq()
	}

	var searchPaths []string
	if a.FormulaLayer != "" {
		searchPaths = []string{a.FormulaLayer}
	}
	recipe, err := formula.Compile(ctx, a.Formula, searchPaths, nil)
	if err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
		return
	}

	// Decorate graph workflow recipes with routing metadata so child step
	// beads get gc.routed_to set before instantiation.
	if a.Pool != "" {
		pool := qualifyPool(a.Pool, a.Rig)
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
		store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
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
		pool := qualifyPool(a.Pool, a.Rig)
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
		store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
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
	qualified := qualifyPool(a.Pool, a.Rig)
	rigName, _ := config.ParseQualifiedName(qualified)
	if rigName == "" {
		rigName = a.Rig
	}
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

// hasOpenWorkStrict reports whether any non-closed work bead exists for this
// order. Tracking beads (title "order:<name>") are excluded, so only actual
// work (wisps, exec results) counts.
func (m *memoryOrderDispatcher) hasOpenWorkStrict(store beads.Store, scopedName string) (bool, error) {
	results, err := store.List(beads.ListQuery{
		Label: "order-run:" + scopedName,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return false, fmt.Errorf("listing order work beads: %w", err)
	}
	trackingTitle := "order:" + scopedName
	for _, b := range results {
		if b.Status != "closed" && b.Title != trackingTitle {
			return true, nil
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
	n, err := store.CloseAll(ids, nil)
	if err != nil {
		return n, fmt.Errorf("closing orphaned order-tracking beads: %w", err)
	}
	return n, nil
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

// qualifyPool prefixes an unqualified pool name with the rig name for
// rig-scoped orders. Already-qualified names (containing "/") are
// returned as-is. City orders (empty rig) are unchanged.
func qualifyPool(pool, rig string) string {
	if rig == "" || strings.Contains(pool, "/") {
		return pool
	}
	return rig + "/" + pool
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
