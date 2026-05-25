package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/closeorder"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/processgroup"
)

const (
	labelOrderTracking    = "order-tracking"
	labelTriggerEnvFailed = "trigger-env-failed"

	orderTrackingSweepOrder                = "order-tracking-sweep"
	defaultOrderTrackingSweepStaleAfter    = 10 * time.Minute
	orderTrackingSweepWatchdogInterval     = 30 * time.Second
	orderTrackingSweepWatchdogStaleAfter   = 2 * time.Minute
	orderTrackingSweepMetadataReason       = "stale-order-tracking"
	orderTrackingSweepMetadataInitiator    = "order-tracking-sweep"
	orderTrackingWatchdogMetadataInitiator = "controller-watchdog"
	orderTrackingCloseVerifyAttempts       = 3
	orderTrackingCloseVerifyRetryDelay     = 25 * time.Millisecond

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
	staleOrderWispCloseReason     = "order-tracking sweep: stale order wisp subtree exceeded retention window"

	completedOrderTrackingCloseReason = "order dispatch completed: tracking bead lifecycle finished"

	defaultMaxOrderDispatchesPerTick = 4
	orderTrackingSweepCloseBudget    = 4
)

var (
	// shellExecPostCancelWaitDelay is os/exec's pipe-close wait after
	// Cancel returns; the TERM and KILL waits each use shellExecSignalGrace.
	shellExecPostCancelWaitDelay = 2 * time.Second
	shellExecSignalGrace         = 2 * time.Second
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
// environment variables. Returns combined stdout or an error. When context
// cancellation stops a command, the returned error is ctx.Err(), not an
// *exec.ExitError, and the returned output may be partial.
type ExecRunner func(ctx context.Context, command, dir string, env []string) ([]byte, error)

// shellExecRunner is the production ExecRunner using os/exec.
func shellExecRunner(ctx context.Context, command, dir string, env []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = mergeOrderExecEnv(cmd.Environ(), env)
	processgroup.StartCommandInNewGroup(cmd)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	var cleanupMu sync.Mutex
	var cleanupErr error
	var cleanupOnce sync.Once
	startedPGID := 0
	canceled := false
	cleanupProcess := func() error {
		cleanupOnce.Do(func() {
			cleanupMu.Lock()
			pgid := startedPGID
			cleanupMu.Unlock()
			err := cancelShellExecProcessGroup(cmd, pgid)
			cleanupMu.Lock()
			cleanupErr = err
			cleanupMu.Unlock()
		})
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		return cleanupErr
	}
	cmd.Cancel = func() error {
		cleanupMu.Lock()
		canceled = true
		cleanupMu.Unlock()
		_ = cleanupProcess()
		return nil
	}
	cmd.WaitDelay = shellExecPostCancelWaitDelay

	if err := cmd.Start(); err != nil {
		return output.Bytes(), err
	}
	cleanupMu.Lock()
	startedPGID = cmd.Process.Pid
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		startedPGID = pgid
	}
	cleanupMu.Unlock()

	err := cmd.Wait()
	cleanupMu.Lock()
	wasCanceled := canceled
	cleanupMu.Unlock()
	if errors.Is(err, exec.ErrWaitDelay) || wasCanceled {
		_ = cleanupProcess()
	}

	cleanupMu.Lock()
	wasCanceled = canceled
	errCleanup := cleanupErr
	cleanupMu.Unlock()
	if wasCanceled {
		if err := ctx.Err(); err != nil {
			if errCleanup != nil {
				return output.Bytes(), errors.Join(err, errCleanup)
			}
			return output.Bytes(), err
		}
	}
	if errCleanup != nil {
		if err != nil {
			return output.Bytes(), errors.Join(err, errCleanup)
		}
		return output.Bytes(), errCleanup
	}
	return output.Bytes(), err
}

func cancelShellExecProcessGroup(cmd *exec.Cmd, pgid int) error {
	return processgroup.TerminateCommand(cmd, pgid, shellExecSignalGrace, processgroup.Options{})
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

type orderSetSnapshot struct {
	Orders    []orders.Order
	Signature string
}

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
	aa                   []orders.Order
	storeFn              orderStoreFunc
	ep                   events.Provider
	execRun              ExecRunner
	rec                  events.Recorder
	stderr               io.Writer
	maxTimeout           time.Duration
	maxDispatchesPerTick int
	nextDispatchStart    int
	cfg                  *config.City
	cityName             string
	cacheMu              sync.Mutex
	lastRunCache         map[string]time.Time

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
	od, _ := buildOrderDispatcherWithSnapshot(cityPath, cfg, rec, stderr, "gc start: order scan")
	return od
}

func buildOrderDispatcherWithSnapshot(cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer, cmdName string) (orderDispatcher, orderSetSnapshot) {
	snapshot, err := scanOrderSetSnapshotFS(fsys.OSFS{}, cityPath, cfg, stderr, cmdName)
	if err != nil {
		logDispatchError(stderr, "%s: %v", cmdName, err)
		return nil, orderSetSnapshot{}
	}
	return buildOrderDispatcherFromOrderSet(cityPath, cfg, snapshot.Orders, rec, stderr), snapshot
}

func scanOrderSetSnapshotFS(fs fsys.FS, cityPath string, cfg *config.City, stderr io.Writer, cmdName string) (orderSetSnapshot, error) {
	if cfg == nil {
		cfg = &config.City{}
	}
	allAA, err := orderdiscovery.ScanAll(cityPath, cfg, orderdiscovery.ScanOptions{
		FS: fs,
		OnRigScanError: func(rigName string, err error) error {
			fmt.Fprintf(stderr, "%s: rig %s: %v\n", cmdName, rigName, err) //nolint:errcheck // best-effort stderr
			return nil
		},
		OnOverrideError: func(err error) error {
			logDispatchError(stderr, "%s: order overrides: %v", cmdName, err)
			return nil
		},
		OnValidateError: func(orderName string, err error) error {
			logDispatchError(stderr, "%s: order %s: %v", cmdName, orderName, err)
			return nil
		},
		ValidateOrder: validateOrderExecEnvOverrides,
	})
	if err != nil {
		return orderSetSnapshot{}, err
	}
	return orderSetSnapshot{
		Orders:    append([]orders.Order(nil), allAA...),
		Signature: orderSetSignature(allAA),
	}, nil
}

func orderSetSignature(aa []orders.Order) string {
	normalized := append([]orders.Order(nil), aa...)
	sort.Slice(normalized, func(i, j int) bool {
		left, right := normalized[i].ScopedName(), normalized[j].ScopedName()
		if left != right {
			return left < right
		}
		return normalized[i].Source < normalized[j].Source
	})
	data, err := json.Marshal(normalized)
	if err != nil {
		data = []byte(fmt.Sprintf("%#v", normalized))
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func buildOrderDispatcherFromOrderSet(cityPath string, cfg *config.City, allAA []orders.Order, rec events.Recorder, stderr io.Writer) orderDispatcher {
	if cfg == nil {
		cfg = &config.City{}
	}
	allAA = orders.FilterEnabled(allAA)

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
		ep:                   ep,
		execRun:              shellExecRunner,
		rec:                  rec,
		stderr:               lockedStderr(stderr),
		maxTimeout:           cfg.Orders.MaxTimeoutDuration(),
		maxDispatchesPerTick: defaultMaxOrderDispatchesPerTick,
		cfg:                  cfg,
		cityName:             loadedCityName(cfg, cityPath),
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       dispatchCancel,
	}
}

func (m *memoryOrderDispatcher) dispatch(ctx context.Context, cityPath string, now time.Time) {
	// Skip all order dispatch when the city is suspended.
	if m.cfg != nil && citySuspended(m.cfg) {
		return
	}

	stores := make(map[string]beads.Store)
	budgetSpent := 0

	total := len(m.aa)
	if total == 0 {
		return
	}
	start := 0
	if m.maxDispatchesPerTick > 0 {
		start = m.nextDispatchStart % total
	}
	spendDispatchBudget := func(idx int) bool {
		budgetSpent++
		if m.maxDispatchesPerTick > 0 {
			m.nextDispatchStart = (idx + 1) % total
		}
		return m.maxDispatchesPerTick > 0 && budgetSpent >= m.maxDispatchesPerTick
	}

	for offset := 0; offset < total; offset++ {
		idx := (start + offset) % total
		a := m.aa[idx]
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
		scoped := a.ScopedName()
		hasOpenWork, err := m.hasOpenWorkInStoresStrict(storesForGate, scoped)
		if err != nil {
			logDispatchError(m.stderr, "gc: order dispatch: checking open work for %s: %v", scoped, err)
			continue
		}
		if hasOpenWork {
			continue
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
		triggerOpts, err := orderTriggerOptionsForTarget(cityPath, m.cfg, target, a)
		if err != nil {
			redacted := redactOrderEnvError(err, os.Environ())
			msg := fmt.Sprintf("building trigger env: %s", redacted)
			logDispatchError(m.stderr, "gc: order dispatch: building trigger env for %s: %s", a.ScopedName(), redacted)
			// Leave this open so the existing open-work gate suppresses repeat
			// ticks until the normal stale tracking sweep gives the order another try.
			trackingBead, createErr := store.Create(beads.Bead{
				Title:     "order:" + scoped,
				Labels:    []string{"order-run:" + scoped, labelOrderTracking, labelTriggerEnvFailed},
				Ephemeral: true,
			})
			if createErr != nil {
				logDispatchError(m.stderr, "gc: order dispatch: creating trigger env failure tracking bead for %s: %v", scoped, createErr)
			} else {
				m.rememberLastRun(scoped, storeKeysForGate, trackingBead.CreatedAt)
			}
			m.rec.Record(events.Event{
				Type:    events.OrderFailed,
				Actor:   "controller",
				Subject: a.ScopedName(),
				Message: msg,
			})
			if spendDispatchBudget(idx) {
				return
			}
			continue
		}
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
		hasOpenWork, err = m.hasOpenWorkInStoresStrict(storesForGate, scoped)
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
			Title:     "order:" + scoped,
			Labels:    []string{"order-run:" + scoped, labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			logDispatchError(m.stderr, "gc: order dispatch: creating tracking bead for %s: %v", scoped, err)
			continue
		}
		m.rememberLastRun(scoped, storeKeysForGate, trackingBead.CreatedAt)

		// Fire with timeout; inflight tracks the spawned goroutine so
		// drain can wait for tracking-bead outcome persistence before
		// controller exit or config reload.
		m.addInflight()
		m.launchDispatchOne(ctx, store, target, a, cityPath, trackingBead.ID)
		if spendDispatchBudget(idx) {
			return
		}
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
// goroutine completion; dispatchOne's deferred cleanup writes the
// tracking-bead outcome before doneInflight signals drain.
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
	defer func() {
		if err := closeOrderTrackingBead(ctx, store, trackingID); err != nil {
			logDispatchError(m.stderr, "gc: order %s: closing tracking bead %s: %v", a.ScopedName(), trackingID, err)
		}
	}()

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

func closeOrderTrackingBead(ctx context.Context, store beads.Store, trackingID string) error {
	_, err := closeAndVerifyOrderTrackingBeads(ctx, store, []string{trackingID}, map[string]string{
		"close_reason": completedOrderTrackingCloseReason,
	})
	return err
}

func closeAndVerifyOrderTrackingBeads(ctx context.Context, store beads.Store, ids []string, metadata map[string]string) (int, error) {
	ids = uniqueNonEmptyOrderTrackingIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil {
		return 0, fmt.Errorf("order-tracking close: nil store")
	}

	closed := 0
	var lastErr error
	for attempt := 1; attempt <= orderTrackingCloseVerifyAttempts; attempt++ {
		n, err := store.CloseAll(ids, metadata)
		closed += n
		if closed > len(ids) {
			closed = len(ids)
		}
		if err != nil {
			lastErr = fmt.Errorf("closing order-tracking beads %s: %w", strings.Join(ids, ", "), err)
			if attempt < orderTrackingCloseVerifyAttempts {
				if waitErr := waitOrderTrackingCloseRetry(ctx); waitErr != nil {
					return closed, errors.Join(lastErr, waitErr)
				}
			}
			continue
		}
		openIDs, err := openOrderTrackingIDs(store, ids)
		if err != nil {
			lastErr = fmt.Errorf("verifying order-tracking close for %s: %w", strings.Join(ids, ", "), err)
			if attempt < orderTrackingCloseVerifyAttempts {
				if waitErr := waitOrderTrackingCloseRetry(ctx); waitErr != nil {
					return closed, errors.Join(lastErr, waitErr)
				}
			}
			continue
		}
		if len(openIDs) == 0 {
			return closed, nil
		}
		lastErr = fmt.Errorf("verifying order-tracking close: still open: %s", strings.Join(openIDs, ", "))
		if attempt < orderTrackingCloseVerifyAttempts {
			if waitErr := waitOrderTrackingCloseRetry(ctx); waitErr != nil {
				return closed, errors.Join(lastErr, waitErr)
			}
		}
	}
	return closed, lastErr
}

func waitOrderTrackingCloseRetry(ctx context.Context) error {
	timer := time.NewTimer(orderTrackingCloseVerifyRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func uniqueNonEmptyOrderTrackingIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func openOrderTrackingIDs(store beads.Store, ids []string) ([]string, error) {
	var openIDs []string
	for _, id := range ids {
		b, err := store.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return openIDs, err
		}
		if b.Status != "closed" {
			openIDs = append(openIDs, id)
		}
	}
	return openIDs, nil
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

	env, err := orderExecEnvWithError(cityPath, m.cfg, target, a)
	var output []byte
	var execErrMsg string
	if err != nil {
		redactionEnv := append(os.Environ(), env...)
		redacted := redactOrderEnvError(err, redactionEnv)
		execErrMsg = "exec env failed: " + redacted
		labels = []string{"exec-env-failed"}
		logDispatchError(m.stderr, "gc: order exec %s env failed: %s", scoped, redacted)
	} else {
		output, err = m.execRun(ctx, a.Exec, target.ScopeRoot, env)
		if err != nil {
			redactionEnv := append(os.Environ(), env...)
			execErrMsg = execenv.RedactText(err.Error(), redactionEnv)
			labels = []string{"exec-failed"}
			logDispatchError(m.stderr, "gc: order exec %s failed: %s", scoped, execErrMsg)
			if len(output) > 0 {
				logDispatchError(m.stderr, "gc: order exec %s output: %s", scoped, execenv.RedactText(string(output), redactionEnv))
			}
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

func prepareOrderWispRecipe(ctx context.Context, store beads.Store, a orders.Order, searchPaths []string) (*formula.Recipe, error) {
	inv, err := graphv2.PrepareInvocation(ctx, store, a.Formula, searchPaths, "", nil)
	if err != nil {
		return nil, err
	}
	return formula.CompileWithoutRuntimeVarValidation(ctx, a.Formula, searchPaths, inv.Vars)
}

func redactOrderEnvError(err error, env []string) string {
	if err == nil {
		return ""
	}
	return execenv.RedactText(err.Error(), env)
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
	recipe, err := prepareOrderWispRecipe(ctx, store, a, searchPaths)
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
		if err := applyGraphRouting(recipe, nil, pool, nil, "", "", "", store, m.cityName, cityPath, m.cfg); err != nil {
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
		// Same metadata-pair the CLI path (cmd_order.go:doOrderRunWithJSON)
		// writes — gc.routed_to so the worker's Tier-3 work_query and bd
		// CLI tooling see the routing, plus poolDemandMetadataPair() so
		// the supervisor's defaultScaleCheckCounts can count the wisp as
		// scale_check demand. Without the second pair, supervisor-cron
		// dispatched orders silently never spawn a pool worker because
		// the molecule wisp is filtered out of Ready() by
		// readyExcludeTypes (per PR #1154). See cmd/gc/pool_demand.go.
		update.Metadata = map[string]string{"gc.routed_to": pool}
		for k, v := range poolDemandMetadataPair() {
			update.Metadata[k] = v
		}
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

// hasOpenWorkStrict reports whether any in-flight work exists for this
// order — either a dispatchOne goroutine still running, or a wisp whose
// step beads have not all been completed by the pool agent.
//
// Tracking beads carry both "order-run:<scoped>" and labelOrderTracking;
// dispatchOne closes them via defer when dispatch returns. An open
// tracking bead means a dispatchOne goroutine is in flight.
//
// Wisp root beads also carry "order-run:<scoped>" (so gc order history
// and the orders API feed can attribute the wisp to its order) but never
// carry labelOrderTracking. Molecule roots never auto-close when their
// step beads finish, so a leftover open root with all-closed children is
// orphan state — counting it would permanently block re-dispatch
// (ga-jra/ga-lo8c, where formula+pool orders stalled after a city restart
// because the first auto-fire's wisp root tripped this check). But a
// wisp root whose child step beads are still open IS in-flight work: the
// pool agent has not yet executed the wisp. Counting those prevents the
// cooldown gate from pouring duplicate wisps when the pool stalls
// (tr-kds01, where 24h-interval digest wisps accumulated because the
// pool never picked them up).
func (m *memoryOrderDispatcher) hasOpenWorkStrict(store beads.Store, scopedName string) (bool, error) {
	results, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Label: "order-run:" + scopedName,
		Sort:  beads.SortCreatedDesc,
		// Tracking beads are ephemeral while wisp roots are issue-tier, so
		// the authoritative single-flight gate must union both tiers.
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("listing order work beads: %w", err)
	}
	for _, b := range results {
		if b.Status == "closed" {
			continue
		}
		if beadLabelsContain(b.Labels, labelOrderTracking) {
			return true, nil
		}
		if !isOrderWispRootCandidate(b) {
			continue
		}
		if isOrderRootOnlyWispCandidate(b) {
			return true, nil
		}
		hasOpenDescendants, err := storeHasOpenDescendants(store, b.ID)
		if err != nil {
			return false, fmt.Errorf("checking open descendants of wisp %s: %w", b.ID, err)
		}
		if hasOpenDescendants {
			return true, nil
		}
	}
	return false, nil
}

func isOrderWispRootCandidate(b beads.Bead) bool {
	if beads.IsMoleculeType(b.Type) {
		return true
	}
	return b.Metadata["gc.kind"] == "workflow" || b.Metadata["gc.kind"] == "wisp"
}

func isOrderRootOnlyWispCandidate(b beads.Bead) bool {
	return b.Metadata["gc.kind"] == "wisp" && !beads.IsMoleculeType(b.Type)
}

// storeHasOpenDescendants reports whether any transitive child of parentID is
// non-closed. It includes closed intermediate nodes so nested molecule work
// remains visible after a direct child step has completed.
func storeHasOpenDescendants(store beads.Store, parentID string) (bool, error) {
	seen := map[string]struct{}{parentID: {}}
	queue := []string{parentID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := store.List(beads.ListQuery{
			ParentID:      parentID,
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return false, err
		}
		for _, c := range children {
			if c.ID == "" {
				continue
			}
			if _, ok := seen[c.ID]; ok {
				continue
			}
			seen[c.ID] = struct{}{}
			if c.Status != "closed" {
				return true, nil
			}
			queue = append(queue, c.ID)
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
	return sweepOrphanedOrderTrackingLimit(store, 0)
}

func sweepOrphanedOrderTrackingLimit(store beads.Store, limit int) (int, error) {
	// ListByLabel without IncludeClosed returns only open beads.
	// New tracking beads live in the wisps tier, but legacy issues-tier
	// tracking beads may still exist after upgrade; sweep both.
	all, err := store.ListByLabel(labelOrderTracking, 0, beads.WithBothTiers)
	if err != nil {
		return 0, fmt.Errorf("listing order-tracking beads: %w", err)
	}
	if len(all) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(all))
	for _, b := range all {
		if beadLabelsContain(b.Labels, labelTriggerEnvFailed) {
			continue
		}
		ids = append(ids, b.ID)
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := closeAndVerifyOrderTrackingBeads(context.Background(), store, ids, map[string]string{
		"close_reason": orphanedOrderTrackingCloseReason,
	})
	if err != nil {
		return n, fmt.Errorf("closing orphaned order-tracking beads: %w", err)
	}
	return n, nil
}

func beadLabelsContain(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

type orderTrackingSweepResult struct {
	trackingClosed int
	wispClosed     int
	storesSwept    int
	sweptStoreKeys map[string]struct{}
}

// sweepStaleOrderTracking closes open order-tracking beads whose creation
// timestamp is older than staleAfter. When onlyOrders is non-empty, it only
// closes tracking beads for those scoped order names.
func sweepStaleOrderTracking(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string) (int, error) {
	result, err := sweepStaleOrderTrackingWithOptionsLimit(store, now, staleAfter, onlyOrders, initiator, false, 0)
	return result.trackingClosed, err
}

func sweepStaleOrderTrackingLimit(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, limit int) (int, error) {
	result, err := sweepStaleOrderTrackingWithOptionsLimit(store, now, staleAfter, onlyOrders, initiator, false, limit)
	return result.trackingClosed, err
}

func sweepStaleOrderTrackingAcrossStores(stores []beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, includeWispSubtrees bool) (orderTrackingSweepResult, error) {
	return sweepStaleOrderTrackingAcrossStoresLimit(stores, now, staleAfter, onlyOrders, orderTrackingSweepMetadataInitiator, includeWispSubtrees, 0)
}

// sweepStaleOrderTrackingAcrossStoresLimit applies limit only to
// order-tracking bead closes. Wisp subtree recovery is operator-scoped by
// order name and closes complete stale subtrees when explicitly requested.
func sweepStaleOrderTrackingAcrossStoresLimit(stores []beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, includeWispSubtrees bool, limit int) (orderTrackingSweepResult, error) {
	if staleAfter <= 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("stale-after must be positive")
	}
	if includeWispSubtrees && len(onlyOrders) == 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("include-wisps requires at least one order name")
	}
	result := orderTrackingSweepResult{}
	var errs []error
	for i, store := range stores {
		if store == nil {
			continue
		}
		remainingLimit := 0
		if limit > 0 {
			remainingLimit = limit - result.trackingClosed
			if remainingLimit <= 0 {
				break
			}
		}
		partial, err := sweepStaleOrderTrackingWithOptionsLimit(store, now, staleAfter, onlyOrders, initiator, includeWispSubtrees, remainingLimit)
		result.trackingClosed += partial.trackingClosed
		result.wispClosed += partial.wispClosed
		if err != nil {
			errs = append(errs, fmt.Errorf("sweeping order-tracking %s: %w", orderTrackingSweepStoreLabel(store, i), err))
			continue
		}
		result.storesSwept++
		if key := orderTrackingSweepStoreKey(store); key != "" {
			if result.sweptStoreKeys == nil {
				result.sweptStoreKeys = make(map[string]struct{})
			}
			result.sweptStoreKeys[key] = struct{}{}
		}
	}
	return result, errors.Join(errs...)
}

func orderTrackingSweepStoreKey(store beads.Store) string {
	type keyed interface {
		orderTrackingSweepKey() string
	}
	if keyedStore, ok := store.(keyed); ok {
		return strings.TrimSpace(keyedStore.orderTrackingSweepKey())
	}
	return ""
}

func orderTrackingSweepStoreLabel(store beads.Store, index int) string {
	type labeled interface {
		orderTrackingSweepLabel() string
	}
	if labeledStore, ok := store.(labeled); ok {
		if label := strings.TrimSpace(labeledStore.orderTrackingSweepLabel()); label != "" {
			return label
		}
	}
	return fmt.Sprintf("store %d", index+1)
}

// sweepStaleOrderTrackingWithOptionsLimit applies limit only to
// order-tracking bead closes. Wisp subtree recovery is order-scoped and closes
// complete stale subtrees when includeWispSubtrees is set.
func sweepStaleOrderTrackingWithOptionsLimit(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, includeWispSubtrees bool, limit int) (orderTrackingSweepResult, error) {
	if staleAfter <= 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("stale-after must be positive")
	}
	if includeWispSubtrees && len(onlyOrders) == 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("include-wisps requires at least one order name")
	}
	all, err := store.ListByLabel(labelOrderTracking, 0, beads.WithBothTiers)
	if err != nil {
		return orderTrackingSweepResult{}, fmt.Errorf("listing order-tracking beads: %w", err)
	}

	cutoff := now.Add(-staleAfter)
	result := orderTrackingSweepResult{}
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
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	if len(ids) == 0 {
		if !includeWispSubtrees {
			return result, nil
		}
	} else {
		metadata := map[string]string{
			"order_tracking_sweep": orderTrackingSweepMetadataReason,
			"close_reason":         staleOrderTrackingCloseReason,
		}
		if initiator != "" {
			metadata["order_tracking_sweep_by"] = initiator
		}
		n, err := closeAndVerifyOrderTrackingBeads(context.Background(), store, ids, metadata)
		result.trackingClosed = n
		if err != nil {
			return result, fmt.Errorf("closing stale order-tracking beads: %w", err)
		}
	}

	if includeWispSubtrees {
		n, err := sweepStaleOrderWispSubtrees(store, cutoff, onlyOrders, initiator)
		result.wispClosed = n
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func sweepStaleOrderWispSubtrees(store beads.Store, cutoff time.Time, onlyOrders map[string]struct{}, initiator string) (int, error) {
	roots, err := staleOrderWispRoots(store, cutoff, onlyOrders)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root.ID == "" || root.Status == "closed" {
			continue
		}
		if beadLabelsContain(root.Labels, labelOrderTracking) {
			continue
		}
		if !isOrderWispRootCandidate(root) {
			continue
		}
		if !isOrderRootOnlyWispCandidate(root) {
			openDescendants, err := storeHasOpenDescendants(store, root.ID)
			if err != nil {
				return 0, fmt.Errorf("checking stale wisp descendants of %s: %w", root.ID, err)
			}
			if !openDescendants {
				continue
			}
		}
		subtree, err := collectOrderWispSubtree(store, root)
		if err != nil {
			return 0, fmt.Errorf("collecting stale wisp subtree %s: %w", root.ID, err)
		}
		if !openSubtreeOlderThan(subtree, cutoff) {
			continue
		}
		for _, id := range staleOrderWispSubtreeCloseIDs(subtree) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ordered, err := closeorder.Order(store, ids)
	if err != nil {
		return 0, fmt.Errorf("ordering stale order wisp closes: %w", err)
	}
	metadata := map[string]string{
		"order_tracking_sweep": orderTrackingSweepMetadataReason,
		"order_wisp_sweep":     "stale-order-wisp",
		"close_reason":         staleOrderWispCloseReason,
	}
	if initiator != "" {
		metadata["order_tracking_sweep_by"] = initiator
	}
	n, err := store.CloseAll(ordered, metadata)
	if err != nil {
		return n, fmt.Errorf("closing stale order wisp subtrees: %w", err)
	}
	return n, nil
}

func staleOrderWispRoots(store beads.Store, cutoff time.Time, onlyOrders map[string]struct{}) ([]beads.Bead, error) {
	if len(onlyOrders) == 0 {
		return nil, fmt.Errorf("include-wisps requires at least one order name")
	}
	var roots []beads.Bead
	for orderName := range onlyOrders {
		matches, err := store.List(beads.ListQuery{
			Label:         "order-run:" + orderName,
			CreatedBefore: cutoff,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return nil, fmt.Errorf("listing stale order wisps for %s: %w", orderName, err)
		}
		roots = append(roots, matches...)
	}
	return roots, nil
}

func collectOrderWispSubtree(store beads.Store, root beads.Bead) ([]beads.Bead, error) {
	if root.ID == "" {
		return nil, nil
	}
	seen := map[string]struct{}{root.ID: {}}
	out := []beads.Bead{root}
	queue := []string{root.ID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := store.List(beads.ListQuery{
			ParentID:      parentID,
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.ID == "" {
				continue
			}
			if _, ok := seen[child.ID]; ok {
				continue
			}
			seen[child.ID] = struct{}{}
			out = append(out, child)
			queue = append(queue, child.ID)
		}
	}
	return out, nil
}

func staleOrderWispSubtreeCloseIDs(subtree []beads.Bead) []string {
	if len(subtree) == 0 {
		return nil
	}
	byID := make(map[string]beads.Bead, len(subtree))
	for _, bead := range subtree {
		if bead.ID != "" {
			byID[bead.ID] = bead
		}
	}
	depthMemo := make(map[string]int, len(subtree))
	const visitingDepth = -1
	var depth func(string) int
	depth = func(id string) int {
		if d, ok := depthMemo[id]; ok {
			if d == visitingDepth {
				return 0
			}
			return d
		}
		bead, ok := byID[id]
		if !ok {
			return 0
		}
		parentID := strings.TrimSpace(bead.ParentID)
		if parentID == "" || parentID == id {
			depthMemo[id] = 0
			return 0
		}
		parent, ok := byID[parentID]
		if !ok || parent.ID == "" {
			depthMemo[id] = 0
			return 0
		}
		depthMemo[id] = visitingDepth
		d := depth(parentID) + 1
		depthMemo[id] = d
		return d
	}

	ordered := append([]beads.Bead(nil), subtree...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if da, db := depth(ordered[i].ID), depth(ordered[j].ID); da != db {
			return da > db
		}
		return ordered[i].ID < ordered[j].ID
	})

	ids := make([]string, 0, len(ordered))
	for _, bead := range ordered {
		if bead.ID == "" || bead.Status == "closed" {
			continue
		}
		ids = append(ids, bead.ID)
	}
	return ids
}

func openSubtreeOlderThan(subtree []beads.Bead, cutoff time.Time) bool {
	for _, b := range subtree {
		if b.Status == "closed" {
			continue
		}
		if b.CreatedAt.IsZero() || !b.CreatedAt.Before(cutoff) {
			return false
		}
	}
	return true
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
	return sweepOrphanedOrderTrackingRetryLimit(store, attempts, backoff, 0)
}

func sweepOrphanedOrderTrackingRetryLimit(store beads.Store, attempts int, backoff time.Duration, limit int) (int, error) { //nolint:unparam // attempts is configurable for testability
	if attempts <= 0 {
		attempts = 1
	}
	total := 0
	var err error
	for i := range attempts {
		remainingLimit := limit
		if limit > 0 {
			remainingLimit = limit - total
			if remainingLimit <= 0 {
				if err != nil {
					return total, fmt.Errorf("sweep reached close budget after partial close: %w", err)
				}
				return total, nil
			}
		}
		var n int
		n, err = sweepOrphanedOrderTrackingLimit(store, remainingLimit)
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
	return orderdiscovery.RigExclusiveLayers(rigLayers, cityLayers)
}

// qualifyPool resolves a raw pool name from an order TOML to the qualified
// form used by Agent.QualifiedName() — the same string the scaler queries
// via gc.routed_to. Qualification resolves configured agents before falling
// back to legacy synthesized routes:
//
//  1. If pool already contains "/" it is rig-qualified — pass through.
//  2. If pool exactly matches a configured binding-qualified target
//     ("binding.name"), preserve that target and still stack the rig prefix
//     when present.
//  3. If the order came from an imported pack, prefer same-source agents when
//     resolving a bare pool name so pack-local orders stay pack-local even if
//     other scopes also export the same bare agent name.
//  4. Otherwise look up agents in cfg.Agents whose Dir matches the order
//     scope and Name matches pool. If a rig order has no rig-scoped match,
//     fall back to configured city-scoped agents before synthesizing a legacy
//     rig/pool target. This lets rig orders from city packs target city-scoped
//     utility pools instead of routing to non-existent rig-local pools.
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

	cleanHint := ""
	if sourceDirHint != "" {
		cleanHint = filepath.Clean(sourceDirHint)
	}

	if rig != "" {
		qualified, matched, err := qualifyPoolInDir(pool, rig, fmt.Sprintf("rig %q", rig), cfg, cleanHint)
		if err != nil {
			return "", err
		}
		if matched {
			return rig + "/" + qualified, nil
		}
		qualified, matched, err = qualifyPoolInDir(pool, "", "city order", cfg, cleanHint)
		if err != nil {
			return "", err
		}
		if matched {
			return qualified, nil
		}
		return rig + "/" + pool, nil
	}

	qualified, matched, err := qualifyPoolInDir(pool, "", "city order", cfg, cleanHint)
	if err != nil {
		return "", err
	}
	if matched {
		return qualified, nil
	}
	return pool, nil
}

func qualifyPoolInDir(pool, dir, scope string, cfg *config.City, cleanHint string) (string, bool, error) {
	var exactQualified []string
	var sourceScopedMatches []string
	var localBareMatches []string
	var bareMatches []string
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir != dir {
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
		return exactQualified[0], true, nil
	case len(exactQualified) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(exactQualified, ", "))
	case len(sourceScopedMatches) == 1:
		return sourceScopedMatches[0], true, nil
	case len(sourceScopedMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(sourceScopedMatches, ", "))
	case len(localBareMatches) == 1:
		return localBareMatches[0], true, nil
	case len(localBareMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(localBareMatches, ", "))
	case len(bareMatches) == 1:
		return bareMatches[0], true, nil
	case len(bareMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(bareMatches, ", "))
	}
	return pool, false, nil
}

func appendUniquePoolTarget(values []string, want string) []string {
	for _, value := range values {
		if value == want {
			return values
		}
	}
	return append(values, want)
}
