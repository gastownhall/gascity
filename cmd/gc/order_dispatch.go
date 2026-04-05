package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderDispatcher evaluates order gate conditions and dispatches due
// orders as wisps or exec scripts. Follows the nil-guard tracker pattern:
// nil means no auto-dispatchable orders exist.
//
// dispatch is fire-and-forget: gate evaluation is synchronous, but each due
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

// memoryOrderDispatcher is the production implementation.
type memoryOrderDispatcher struct {
	aa         []orders.Order
	store      beads.Store
	ep         events.Provider
	runner     beads.CommandRunner
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
func buildOrderDispatcher(cityPath string, cfg *config.City, runner beads.CommandRunner, rec events.Recorder, stderr io.Writer) orderDispatcher {
	allAA, err := scanAllOrders(cityPath, cfg, stderr, "gc start: order scan")
	if err != nil {
		fmt.Fprintf(stderr, "gc start: order scan: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	if len(cfg.Orders.Overrides) > 0 {
		if err := orders.ApplyOverrides(allAA, convertOverrides(cfg.Orders.Overrides)); err != nil {
			fmt.Fprintf(stderr, "gc start: order overrides: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	// Filter out manual-gate orders — they are never auto-dispatched.
	var auto []orders.Order
	for _, a := range allAA {
		if a.Gate != "manual" {
			auto = append(auto, a)
		}
	}
	if len(auto) == 0 {
		return nil
	}

	store := beads.NewBdStore(cityPath, runner)

	// Extract events.Provider from recorder if available.
	// FileRecorder implements Provider; Discard does not.
	var ep events.Provider
	if p, ok := rec.(events.Provider); ok {
		ep = p
	}

	return &memoryOrderDispatcher{
		aa:         auto,
		store:      store,
		ep:         ep,
		runner:     runner,
		execRun:    shellExecRunner,
		rec:        rec,
		stderr:     stderr,
		maxTimeout: cfg.Orders.MaxTimeoutDuration(),
		cfg:        cfg,
		cityName:   cfg.Workspace.Name,
	}
}

func (m *memoryOrderDispatcher) dispatch(ctx context.Context, cityPath string, now time.Time) {
	lastRunFn := orderLastRunFn(m.store)
	cursorFn := bdCursorFunc(m.store)

	for _, a := range m.aa {
		result := orders.CheckGate(a, now, lastRunFn, m.ep, cursorFn)
		if !result.Due {
			continue
		}

		// Skip dispatch if previous work hasn't been processed yet.
		scoped := a.ScopedName()
		if m.hasOpenWork(scoped) {
			continue
		}

		// Create tracking bead synchronously BEFORE dispatch goroutine.
		// This prevents the cooldown gate from re-firing on the next tick.
		trackingBead, err := m.store.Create(beads.Bead{
			Title:  "order:" + scoped,
			Labels: []string{"order-run:" + scoped, "order-tracking"},
		})
		if err != nil {
			fmt.Fprintf(m.stderr, "gc: order dispatch: creating tracking bead for %s: %v\n", scoped, err) //nolint:errcheck
			continue
		}

		// Fire and forget with timeout.
		a := a // capture loop variable
		go m.dispatchOne(ctx, a, cityPath, trackingBead.ID)
	}
}

// dispatchOne runs a single order dispatch in its own goroutine.
// For exec orders, runs the script directly. For formula orders,
// instantiates a wisp. Emits events and updates the tracking bead.
func (m *memoryOrderDispatcher) dispatchOne(ctx context.Context, a orders.Order, cityPath, trackingID string) {
	defer m.store.Close(trackingID) //nolint:errcheck // best-effort close

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
		m.dispatchExec(childCtx, a, cityPath, trackingID)
	} else {
		m.dispatchWisp(childCtx, a, cityPath, trackingID)
	}
}

// dispatchExec runs an exec order's shell command.
func (m *memoryOrderDispatcher) dispatchExec(ctx context.Context, a orders.Order, cityPath, trackingID string) {
	scoped := a.ScopedName()

	// Build env with ORDER_DIR and PACK_DIR.
	env := orderExecEnv(cityPath, a)
	if a.Source != "" {
		env = append(env, "ORDER_DIR="+filepath.Dir(a.Source))
	}

	output, err := m.execRun(ctx, a.Exec, cityPath, env)

	// Update tracking bead with outcome labels.
	labels := []string{"exec"}
	if err != nil {
		labels = append(labels, "exec-failed")
		fmt.Fprintf(m.stderr, "gc: order exec %s failed: %v\n", scoped, err) //nolint:errcheck
		if len(output) > 0 {
			fmt.Fprintf(m.stderr, "gc: order exec %s output: %s\n", scoped, output) //nolint:errcheck
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
	m.store.Update(trackingID, beads.UpdateOpts{Labels: labels}) //nolint:errcheck // best-effort
}

func orderExecEnv(cityPath string, a orders.Order) []string {
	env := citylayout.CityRuntimeEnv(cityPath)
	if a.FormulaLayer == "" {
		return env
	}

	packDir := filepath.Dir(a.FormulaLayer)
	env = append(env, "PACK_DIR="+packDir)
	env = append(env, "GC_PACK_DIR="+packDir)

	packName := filepath.Base(packDir)
	if packName != "." && packName != string(filepath.Separator) {
		env = append(env, "GC_PACK_NAME="+packName)
		env = append(env, "GC_PACK_STATE_DIR="+citylayout.PackStateDir(cityPath, packName))
	}
	return env
}

// dispatchWisp instantiates a wisp from the order's formula.
func (m *memoryOrderDispatcher) dispatchWisp(ctx context.Context, a orders.Order, cityPath, trackingID string) {
	scoped := a.ScopedName()

	if err := ctx.Err(); err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		m.store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-canceled"}}) //nolint:errcheck // best-effort
		return
	}

	// Capture event head before wisp creation for event gates.
	var headSeq uint64
	if a.Gate == "event" && m.ep != nil {
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
		m.store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
		return
	}

	// Decorate graph workflow recipes with routing metadata so child step
	// beads get gc.routed_to set. Without this, only the root bead gets the
	// pool label and agents cannot discover their step work.
	if a.Pool != "" {
		pool := qualifyPool(a.Pool, a.Rig)
		if err := applyGraphRouting(recipe, nil, pool, nil, "", "", "", "", m.store, m.cityName, m.cfg); err != nil {
			fmt.Fprintf(m.stderr, "gc: order %s: routing decoration failed: %v\n", scoped, err) //nolint:errcheck
			// Non-fatal — molecule still works, just without step-level routing.
		}
	}

	cookResult, err := molecule.Instantiate(ctx, m.store, recipe, molecule.Options{})
	if err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		m.store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
		return
	}
	rootID := cookResult.RootID

	// Label wisp with order-run:<scopedName> for tracking.
	args := []string{"update", rootID, "--add-label=order-run:" + scoped}
	if a.Gate == "event" && m.ep != nil {
		args = append(args, fmt.Sprintf("--add-label=order:%s", scoped))
		args = append(args, fmt.Sprintf("--add-label=seq:%d", headSeq))
	}
	if a.Pool != "" {
		pool := qualifyPool(a.Pool, a.Rig)
		args = append(args, fmt.Sprintf("--add-label=pool:%s", pool))
	}
	if _, err := m.runner(cityPath, "bd", args...); err != nil {
		// Label failure is critical for duplicate-dispatch prevention.
		// Log and emit an event so operators can investigate.
		fmt.Fprintf(m.stderr, "gc: order %s: failed to label wisp %s: %v\n", scoped, rootID, err) //nolint:errcheck
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: fmt.Sprintf("wisp %s created but label failed: %v", rootID, err),
		})
		m.store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp", "wisp-failed"}}) //nolint:errcheck // best-effort
		return
	}

	m.rec.Record(events.Event{
		Type:    events.OrderCompleted,
		Actor:   "controller",
		Subject: scoped,
	})

	// Label tracking bead with outcome.
	m.store.Update(trackingID, beads.UpdateOpts{Labels: []string{"wisp"}}) //nolint:errcheck // best-effort
}

// hasOpenWork reports whether any non-closed work bead exists for this
// order. Tracking beads (title "order:<name>") are excluded —
// only actual work (wisps, exec results) counts. Returns false on error
// (fail open: allow dispatch rather than block).
func (m *memoryOrderDispatcher) hasOpenWork(scopedName string) bool {
	results, err := m.store.List(beads.ListQuery{
		Label: "order-run:" + scopedName,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return false
	}
	trackingTitle := "order:" + scopedName
	for _, b := range results {
		if b.Status != "closed" && b.Title != trackingTitle {
			return true
		}
	}
	return false
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
			Gate:     c.Gate,
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
