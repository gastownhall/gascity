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
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

const (
	labelOrderTracking    = "order-tracking"
	labelTriggerEnvFailed = "trigger-env-failed"

	orderTrackingSweepOrder                = "order-tracking-sweep"
	orderTrackingBeadPolicyName            = "order_tracking"
	defaultOrderTrackingSweepStaleAfter    = 10 * time.Minute
	defaultOrderTrackingDeleteAfterClose   = 7 * 24 * time.Hour
	minClosedOrderTrackingRetained         = 10
	legacyOrderTrackingRetentionBucket     = "\x00legacy-unscoped-order-tracking"
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

	orderTrackingHistoryIndexLimit   = 2048
	defaultMaxOrderDispatchesPerTick = 4
	orderTrackingSweepCloseBudget    = 4

	// orderTrackingRetentionWatchdogInterval is the minimum time between
	// controller-driven closed-bead retention sweeps. 15 minutes balances
	// effective cleanup against per-tick overhead.
	orderTrackingRetentionWatchdogInterval = 15 * time.Minute
	// orderTrackingRetentionWatchdogDeleteBudget bounds the number of
	// closed order-tracking beads deleted per watchdog invocation.
	orderTrackingRetentionWatchdogDeleteBudget = 100
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
	cityPath             string
	cacheMu              sync.Mutex
	lastRunCache         map[string]time.Time

	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc

	inflightMu   sync.Mutex
	inflightN    int
	inflightDone chan struct{} // closed when inflightN returns to 0; nil when idle
}

type orderDispatchTrackingIndex struct {
	// mu guards entries and errs. dispatch shares ONE index across every
	// order's open-work gate, and gateOpenWorkBounded runs each gate in a
	// goroutine it abandons on timeout/ctx-cancel (#2893) — so multiple gate
	// goroutines touch these maps concurrently. The lock is held only around
	// the map reads/writes below, never across the listCanonical* bd calls, so
	// one slow or contended store read cannot stall sibling gates (the property
	// gateOpenWorkBounded exists to preserve).
	mu      sync.Mutex
	entries map[string]map[string]orderTrackingSummary
	errs    map[string]error
}

type orderTrackingSummary struct {
	openTracking bool
	lastRun      time.Time
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
		cityPath:             cityPath,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       dispatchCancel,
	}
}

func (m *memoryOrderDispatcher) dispatch(ctx context.Context, cityPath string, now time.Time) {
	// Skip all order dispatch when the city is suspended. Use the
	// dispatcher's in-scope city path so suspension state resolves
	// against the controlled city rather than the process cwd.
	if m.cfg != nil {
		st, _ := loadSuspensionState(fsys.OSFS{}, m.cityPath)
		if citySuspendedWithState(m.cfg, st) {
			return
		}
	}

	stores := make(map[string]beads.Store)
	// inFlight counts the dispatchOne goroutines launched by THIS tick that
	// still hold handles from `stores`. The per-tick handles must not be closed
	// until those goroutines finish using them: on a native store, CloseStore is
	// a one-way latch (internal/beads/native_dolt_store.go) and the goroutine's
	// post-tick tracking-bead writes would hit a closed handle (gascity#3157).
	// drain() only waits at controller exit/reload, not at end of tick, so the
	// close is handed to a detached closer scoped to this tick's launches. The
	// closer never blocks the tick; when nothing was launched it closes
	// immediately (Wait returns at once on a zero count).
	var inFlight sync.WaitGroup
	defer func() {
		go func() {
			inFlight.Wait()
			for _, st := range stores {
				if err := closeBeadStoreHandle(st); err != nil {
					logDispatchError(m.stderr, "gc: order dispatch: closing store: %v", err)
				}
			}
		}()
	}()
	trackingIndex := newOrderDispatchTrackingIndex()
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
		hasOpenTracking, err := gateOpenWorkBounded(ctx, orderGateTimeout, scoped, func() (bool, error) {
			return trackingIndex.hasOpenTracking(storesForGate, storeKeysForGate, scoped)
		})
		if err != nil {
			if m.gateFailClosed(ctx, a, scoped, err) {
				continue
			}
		}
		if hasOpenTracking {
			continue
		}

		baseLastRunFn := trackingIndex.lastRunFunc(storesForGate, storeKeysForGate, orders.LastRunAcrossStores(storesForGate...))
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
				NoHistory: true,
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
		// Bound the wisp-aware open-work gate (#2921) with our per-order
		// timeout so a slow store can't starve later orders.
		hasOpenWork, err := gateOpenWorkBounded(ctx, orderGateTimeout, scoped, func() (bool, error) {
			return trackingIndex.hasOpenWork(storesForGate, storeKeysForGate, scoped, m.hasOpenWorkInStoresStrict, true)
		})
		if err != nil {
			if m.gateFailClosed(ctx, a, scoped, err) {
				continue
			}
		}
		if hasOpenWork {
			continue
		}

		// Create tracking bead synchronously BEFORE dispatch goroutine.
		// This prevents the cooldown trigger from re-firing on the next tick.
		trackingBead, err := store.Create(beads.Bead{
			Title:     "order:" + scoped,
			Labels:    []string{"order-run:" + scoped, labelOrderTracking},
			NoHistory: true,
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
		inFlight.Add(1)
		m.launchDispatchOne(ctx, store, target, a, cityPath, trackingBead.ID, inFlight.Done)
		if spendDispatchBudget(idx) {
			return
		}
	}
}

// launchDispatchOne spawns dispatchOne with a context that cancels when
// EITHER the caller's tick ctx OR m.dispatchCtx is done — required so
// cancel() reaches goroutines whose tick ctx was context.Background().
// Falls back to the bare caller ctx when m.dispatchCtx is nil (test
// sites that don't initialize the cancel fields). onDone is invoked exactly
// once after dispatchOne returns — i.e. after this goroutine's final store
// call — so the caller can hold per-tick store handles open until the
// goroutine releases them (gascity#3157). A nil onDone is treated as a no-op.
func (m *memoryOrderDispatcher) launchDispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string, onDone func()) {
	if onDone == nil {
		onDone = func() {}
	}
	if m.dispatchCtx == nil {
		go func() {
			defer onDone()
			m.dispatchOne(ctx, store, target, a, cityPath, trackingID)
		}()
		return
	}
	mergedCtx, cancelMerged := context.WithCancel(ctx)
	stopAfter := context.AfterFunc(m.dispatchCtx, cancelMerged)
	go func() {
		// onDone runs last (registered first → LIFO), after dispatchOne has
		// returned, so the per-tick store-close barrier only fires once this
		// goroutine has made its final store call (gascity#3157).
		defer onDone()
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

func newOrderDispatchTrackingIndex() *orderDispatchTrackingIndex {
	return &orderDispatchTrackingIndex{
		entries: make(map[string]map[string]orderTrackingSummary),
		errs:    make(map[string]error),
	}
}

func (idx *orderDispatchTrackingIndex) hasOpenTracking(
	stores []beads.Store,
	storeKeys []string,
	scopedName string,
) (bool, error) {
	if idx == nil {
		return false, nil
	}
	for i, store := range stores {
		if store == nil {
			continue
		}
		entries, err := idx.entriesForStore(store, indexStoreKey(storeKeys, i))
		if err != nil {
			return false, err
		}
		if entries[scopedName].openTracking {
			return true, nil
		}
	}
	return false, nil
}

func (idx *orderDispatchTrackingIndex) hasOpenWork(
	stores []beads.Store,
	storeKeys []string,
	scopedName string,
	fallback func([]beads.Store, string) (bool, error),
	requireStrictFallback bool,
) (bool, error) {
	if idx == nil {
		return fallback(stores, scopedName)
	}
	sawTrackingHistory := false
	for i, store := range stores {
		key := indexStoreKey(storeKeys, i)
		if store == nil {
			continue
		}
		entries, err := idx.entriesForStore(store, key)
		if err != nil {
			return false, err
		}
		if entries[scopedName].openTracking {
			return true, nil
		}
		history, err := idx.historyEntriesForStore(store, key)
		if err != nil {
			return false, err
		}
		if summary, ok := history[scopedName]; ok {
			if summary.openTracking {
				return true, nil
			}
			sawTrackingHistory = true
		}
	}
	if sawTrackingHistory && !requireStrictFallback {
		return false, nil
	}
	return fallback(stores, scopedName)
}

func (idx *orderDispatchTrackingIndex) lastRunFunc(
	stores []beads.Store,
	storeKeys []string,
	fallback orders.LastRunFunc,
) orders.LastRunFunc {
	return func(scopedName string) (time.Time, error) {
		if idx == nil {
			return fallback(scopedName)
		}
		var latest time.Time
		for i, store := range stores {
			last, err := idx.lastRunForStore(store, indexStoreKey(storeKeys, i), scopedName)
			if err != nil {
				return time.Time{}, err
			}
			if last.After(latest) {
				latest = last
			}
		}
		if fallback != nil {
			last, err := fallback(scopedName)
			if err != nil {
				return time.Time{}, err
			}
			if last.After(latest) {
				latest = last
			}
		}
		return latest, nil
	}
}

func (idx *orderDispatchTrackingIndex) lastRunForStore(store beads.Store, storeKey, scopedName string) (time.Time, error) {
	if store == nil {
		return time.Time{}, nil
	}
	entries, err := idx.historyEntriesForStore(store, storeKey)
	if err != nil {
		return time.Time{}, err
	}
	if summary, ok := entries[scopedName]; ok {
		return summary.lastRun, nil
	}
	return time.Time{}, nil
}

func (idx *orderDispatchTrackingIndex) historyEntriesForStore(store beads.Store, storeKey string) (map[string]orderTrackingSummary, error) {
	key := storeKey + "\x00history"
	idx.mu.Lock()
	if err, ok := idx.errs[key]; ok {
		idx.mu.Unlock()
		return nil, err
	}
	if entries, ok := idx.entries[key]; ok {
		idx.mu.Unlock()
		return entries, nil
	}
	idx.mu.Unlock()
	items, err := listCanonicalRecentOrderTrackingHistoryBeads(store)
	if err != nil {
		wrapped := fmt.Errorf("listing order-tracking history: %w", err)
		idx.mu.Lock()
		idx.errs[key] = wrapped
		idx.mu.Unlock()
		return nil, wrapped
	}
	entries := make(map[string]orderTrackingSummary)
	for _, item := range items {
		scopedName, ok := orderNameFromTrackingBead(item)
		if !ok {
			continue
		}
		summary := entries[scopedName]
		if item.CreatedAt.After(summary.lastRun) {
			summary.lastRun = item.CreatedAt
		}
		entries[scopedName] = summary
	}
	// A sibling gate goroutine may have populated this key while we listed;
	// both computed the same result from the same store, so last writer wins.
	idx.mu.Lock()
	idx.entries[key] = entries
	idx.mu.Unlock()
	return entries, nil
}

func (idx *orderDispatchTrackingIndex) entriesForStore(store beads.Store, storeKey string) (map[string]orderTrackingSummary, error) {
	idx.mu.Lock()
	if err, ok := idx.errs[storeKey]; ok {
		idx.mu.Unlock()
		return nil, err
	}
	if entries, ok := idx.entries[storeKey]; ok {
		idx.mu.Unlock()
		return entries, nil
	}
	idx.mu.Unlock()
	items, err := listCanonicalOpenOrderTrackingBeads(store)
	if err != nil {
		wrapped := fmt.Errorf("listing order-tracking beads: %w", err)
		idx.mu.Lock()
		idx.errs[storeKey] = wrapped
		idx.mu.Unlock()
		return nil, wrapped
	}
	entries := make(map[string]orderTrackingSummary)
	for _, item := range items {
		scopedName, ok := orderNameFromTrackingBead(item)
		if !ok {
			continue
		}
		summary := entries[scopedName]
		if item.Status != "closed" {
			summary.openTracking = true
		}
		entries[scopedName] = summary
	}
	// A sibling gate goroutine may have populated this key while we listed;
	// both computed the same result from the same store, so last writer wins.
	idx.mu.Lock()
	idx.entries[storeKey] = entries
	idx.mu.Unlock()
	return entries, nil
}

func indexStoreKey(storeKeys []string, index int) string {
	if index >= 0 && index < len(storeKeys) && storeKeys[index] != "" {
		return storeKeys[index]
	}
	return fmt.Sprintf("store:%d", index)
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

func poolOrderRouteVisibilityWarning(a orders.Order, recipe *formula.Recipe) string {
	if strings.TrimSpace(a.Pool) == "" || formula.RecipeHasReadySurface(recipe) {
		return ""
	}
	return fmt.Sprintf("warning: pool order %q uses formula %q whose root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase=\"vapor\"/root-only or graph.v2 before routing it to a pool.", a.ScopedName(), a.Formula)
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
	if warning := poolOrderRouteVisibilityWarning(a, recipe); warning != "" {
		logDispatchError(m.stderr, "gc: order %s: %s", scoped, warning)
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
	suspState, _ := loadSuspensionState(fsys.OSFS{}, m.cityPath)
	for i := range m.cfg.Rigs {
		if m.cfg.Rigs[i].Name == rigName {
			return suspensionstate.EffectiveRigSuspended(suspState, rigName, m.cfg.Rigs[i].EffectiveSuspendedOnStart())
		}
	}
	return false
}

func listCanonicalRecentOrderTrackingHistoryBeads(store beads.Store) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.List(beads.ListQuery{
		Label:         labelOrderTracking,
		Limit:         orderTrackingHistoryIndexLimit,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
}

func listCanonicalOpenOrderTrackingBeads(store beads.Store) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.List(beads.ListQuery{
		Label:  labelOrderTracking,
		Status: "open",
		Sort:   beads.SortCreatedDesc,
	})
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
		hasOpenDescendants, err := storeHasOpenDescendants(store, b.ID, isTransientNotificationBead)
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

// isTransientNotificationBead reports whether a bead is a short-lived delivery
// chore (a nudge or a mail/message) rather than substantive order work. Such
// beads inherit an order wisp's order-run label via the parent-child graph but
// are reaped on their own TTL, so they must not keep the single-flight open-work
// gate "open" and block the order from re-dispatching (#2893, de-noise).
func isTransientNotificationBead(b beads.Bead) bool {
	if b.Type == "message" {
		return true
	}
	return b.Type == nudgeBeadType && beadLabelsContain(b.Labels, nudgeBeadLabel)
}

// storeHasOpenDescendants reports whether the wisp rooted at rootID still has
// any open descendant bead. It first consults the molecule membership index:
// every descendant created by any growth path (initial pour, convoy Attach,
// fanout fragments, retry attempts) carries gc.root_bead_id == rootID, an
// invariant enforced in internal/molecule. A single metadata-filtered List
// therefore returns the whole membership set in one store round-trip, instead
// of the O(tree) per-node ParentID/DepList walk that spawned a bd subprocess
// per node and blew past the dispatch gate's time bound under Dolt write
// contention (#2893). The membership query's ownership predicate is exactly
// the walk's orderWispGraphDependentOwnedByRoot, so it is strictly at least as
// conservative as the walk — it can only ever report MORE open work, never
// less, and single-flight is never weakened.
//
// When skip is non-nil, an open member for which skip returns true is not
// treated as blocking open work — the gate passes isTransientNotificationBead so
// lingering nudge/mail chores don't wedge it; callers that want the raw
// descendant view (e.g. the stale-wisp sweeper) pass nil. Both the membership
// fast path and the walk fallback honor skip.
//
// When the fast path finds no open member (the membership set is empty,
// all-closed, or only partially stamped — a molecule can carry gc.root_bead_id
// on some steps while sibling ParentID-only steps are un-stamped), it falls
// back to the authoritative tree walk before reporting the root idle, so
// single-flight is never weakened for un-stamped or partial-stamp data.
func storeHasOpenDescendants(store beads.Store, rootID string, skip func(beads.Bead) bool) (bool, error) {
	reader := beads.HandlesFor(store).Live
	members, err := reader.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("listing wisp members of %s: %w", rootID, err)
	}
	for _, b := range members {
		if b.ID == rootID || b.Status == "closed" {
			continue
		}
		if skip != nil && skip(b) {
			continue
		}
		return true, nil
	}
	// No OPEN stamped member found. An empty or all-closed membership set does
	// NOT prove the root is idle, because the index may be incomplete for a
	// partial-stamp molecule (some steps carry gc.root_bead_id, sibling
	// ParentID-only steps do not). Confirm with the authoritative walk before
	// reporting no open work, keeping single-flight safe. The fast path still
	// short-circuits the common in-flight case (any open stamped member) in one
	// query; the walk runs only when no open member is found — i.e. for
	// orphan/just-completed roots.
	return storeHasOpenDescendantsByWalk(store, rootID, skip)
}

// storeHasOpenDescendantsByWalk is the authoritative O(tree) traversal used as
// the fallback for roots whose descendants lack the gc.root_bead_id membership
// metadata. It is the historical storeHasOpenDescendants implementation. It
// includes closed intermediate nodes so nested molecule work remains visible
// after a direct child step has completed. Graph-v2 workflows can link children
// with dependency edges instead of ParentID, so descendants include
// parent-child/tracks/blocks dependents too. When skip is non-nil, an open
// child for which skip returns true is not treated as blocking open work (its
// subtree is still traversed).
func storeHasOpenDescendantsByWalk(store beads.Store, rootID string, skip func(beads.Bead) bool) (bool, error) {
	seen := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	// ParentID queries and closed intermediate traversal require live reads:
	// CachingStore does not retain a complete closed-history parent view.
	reader := beads.HandlesFor(store).Live
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := orderWispParentChildren(reader, parentID)
		if err != nil {
			return false, err
		}
		for _, c := range children {
			if c.ID == "" || c.ID == rootID {
				continue
			}
			if _, ok := seen[c.ID]; ok {
				continue
			}
			seen[c.ID] = struct{}{}
			if c.Status != "closed" && (skip == nil || !skip(c)) {
				return true, nil
			}
			queue = append(queue, c.ID)
		}

		children, err = orderWispGraphDependentChildren(reader, rootID, parentID)
		if err != nil {
			return false, err
		}
		for _, c := range children {
			if c.ID == "" || c.ID == rootID {
				continue
			}
			if _, ok := seen[c.ID]; ok {
				continue
			}
			seen[c.ID] = struct{}{}
			if c.Status != "closed" && (skip == nil || !skip(c)) {
				return true, nil
			}
			queue = append(queue, c.ID)
		}
	}
	return false, nil
}

func orderWispParentChildren(reader beads.LiveReader, parentID string) ([]beads.Bead, error) {
	return reader.List(beads.ListQuery{
		ParentID:      parentID,
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
}

// Order-wisp traversal follows structural ParentID children and graph.v2
// ownership dependents because orders gate and close executable workflow work.
// This is intentionally narrower than generic dependency closure: molecule
// cleanup uses molecule metadata/ParentID, while wisp GC follows its own
// ownership policy for runtime garbage collection.
func orderWispDescendantChildren(reader beads.LiveReader, rootID, parentID string) ([]beads.Bead, error) {
	children, err := orderWispParentChildren(reader, parentID)
	if err != nil {
		return nil, err
	}
	graphChildren, err := orderWispGraphDependentChildren(reader, rootID, parentID)
	if err != nil {
		return nil, err
	}
	return append(children, graphChildren...), nil
}

func orderWispGraphDependentChildren(reader beads.LiveReader, rootID, parentID string) ([]beads.Bead, error) {
	parent, err := reader.Get(parentID)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting graph parent %s: %w", parentID, err)
	}
	if !orderWispMayHaveGraphDependents(parent) {
		return nil, nil
	}

	deps, err := reader.DepList(parentID, "up")
	if err != nil {
		return nil, fmt.Errorf("listing graph dependents for %s: %w", parentID, err)
	}
	children := make([]beads.Bead, 0, len(deps))
	for _, dep := range deps {
		if dep.IssueID == "" {
			continue
		}
		if !isOrderWispDescendantDepType(dep.Type) {
			continue
		}
		child, err := reader.Get(dep.IssueID)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("getting graph dependent %s: %w", dep.IssueID, err)
		}
		if !orderWispGraphDependentOwnedByRoot(child, rootID) {
			continue
		}
		children = append(children, child)
	}
	return children, nil
}

func orderWispGraphDependentOwnedByRoot(child beads.Bead, rootID string) bool {
	if child.ID == rootID {
		return true
	}
	return child.Metadata["gc.root_bead_id"] == rootID
}

func orderWispMayHaveGraphDependents(bead beads.Bead) bool {
	if isOrderWispRootCandidate(bead) {
		return true
	}
	if bead.Metadata["gc.root_bead_id"] != "" {
		return true
	}
	if bead.Metadata["gc.step_ref"] != "" {
		return true
	}
	if bead.Metadata["gc.logical_bead_id"] != "" {
		return true
	}
	return false
}

func isOrderWispDescendantDepType(depType string) bool {
	switch depType {
	case "parent-child", "tracks", "blocks":
		return true
	default:
		return false
	}
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

// orderGateTimeout bounds a single order's open-work gate. The strict gate
// walks an order's wisp subtree by spawning synchronous bd subprocesses
// (storeHasOpenDescendants); under Dolt write contention one heavy order's gate
// can block for minutes, and because dispatch iterates orders synchronously
// that stalls every LATER order (feeders, nudger, route-reclaim) on the same
// tick — the #2893 hang. Bounding the gate lets a slow order be skipped so
// the rest of the sweep proceeds. Package-level var so it is tunable and
// overridable in tests.
var orderGateTimeout = 8 * time.Second

// errGateTimeout marks an open-work gate error caused by the per-order
// bound elapsing (the #2893 contention case), as opposed to ctx cancel or a
// genuine store-read error. Only this case fails open for idempotent orders.
var errGateTimeout = errors.New("open-work gate timed out")

// gateOpenWorkBounded runs the open-work gate under a per-order timeout that
// also honors the dispatch context. On timeout (or cancellation) it returns an
// error; the caller resolves that error through gateFailClosed, which applies
// the per-order policy — non-idempotent orders are skipped (fail-closed) while
// idempotent ones dispatch anyway (fail-open). Either way a heavy gate never
// stalls the LATER orders on the tick (#2893). gate is invoked in a goroutine;
// on timeout that goroutine is left to finish on its own (its result is
// discarded via the buffered channel) rather than blocking the dispatch loop.
func gateOpenWorkBounded(ctx context.Context, timeout time.Duration, scoped string, gate func() (bool, error)) (bool, error) {
	type gateResult struct {
		has bool
		err error
	}
	done := make(chan gateResult, 1)
	go func() {
		has, err := gate()
		done <- gateResult{has: has, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.has, r.err
	case <-timer.C:
		return false, fmt.Errorf("open-work gate for %s timed out after %s; skipping this order so later orders still dispatch (see #2893): %w", scoped, timeout, errGateTimeout)
	case <-ctx.Done():
		return false, fmt.Errorf("open-work gate for %s aborted: %w", scoped, ctx.Err())
	}
}

// gateFailClosed decides whether an open-work gate error must block dispatch of
// this order, and logs the error. The blanket "skip on any gate error" was
// wrong: idempotent sweep orders (feeders, nudger, route-reclaim) are safe to
// double-dispatch, so a gate that times out under store contention must not
// starve them forever (#2893 #2'). Policy:
//   - dispatch context done (shutdown / tick deadline): always block — there is
//     no point dispatching into a canceled context.
//   - a per-order gate TIMEOUT (errGateTimeout): a non-idempotent order fails
//     CLOSED (block, preserving single-flight); an idempotent order fails OPEN
//     (dispatch anyway), since its re-run is a no-op.
//   - any other gate error (e.g. a genuine store-read failure): always block.
//     Only the bounded-gate timeout is the #2893 contention signal that is
//     safe to relax; a real store/gate error is a different signal where the
//     conservative response is to fail CLOSED, matching the pre-#2893 behavior.
//
// Failing open deliberately relaxes single-flight for idempotent orders: it may
// dispatch while a prior run is still in flight. That is safe by the
// idempotent contract (a duplicate run is a no-op) and each dispatch gets its
// own tracking bead, so there is no shared-bead close race. It is also bounded
// in practice — the cooldown trigger's rememberLastRun keeps an order from
// re-firing within its interval, which is far longer than a feeder run — so a
// genuinely concurrent second dispatch is rare, not per-tick. Re-adding an
// open-work check here would reintroduce the #2893 starvation this fixes.
func (m *memoryOrderDispatcher) gateFailClosed(ctx context.Context, a orders.Order, scoped string, err error) bool {
	logDispatchError(m.stderr, "gc: order dispatch: checking open work for %s: %v", scoped, err)
	if ctx.Err() != nil {
		return true
	}
	if a.Idempotent && errors.Is(err, errGateTimeout) {
		logDispatchError(m.stderr, "gc: order dispatch: %s open-work gate failed but order is idempotent; dispatching anyway (#2893)", scoped)
		return false
	}
	return true
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
	trackingClosed  int
	wispClosed      int
	trackingDeleted int
	storesSwept     int
	sweptStoreKeys  map[string]struct{}
}

type orderTrackingRetentionSweepResult struct {
	deleted     int
	storesSwept int
}

type orderTrackingRetentionPolicy struct {
	deleteAfterClose time.Duration
	retainLast       int
}

func orderTrackingRetentionPolicyForConfig(cfg *config.City) orderTrackingRetentionPolicy {
	policy := orderTrackingRetentionPolicy{
		deleteAfterClose: defaultOrderTrackingDeleteAfterClose,
		retainLast:       minClosedOrderTrackingRetained,
	}
	if cfg == nil {
		return policy
	}
	if configured, ok := cfg.Beads.Policies[orderTrackingBeadPolicyName]; ok {
		if duration := configured.DeleteAfterCloseDuration(); duration > 0 {
			policy.deleteAfterClose = duration
		}
	}
	return policy
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

func sweepClosedOrderTrackingRetentionAcrossStores(stores []beads.Store, now time.Time, policy orderTrackingRetentionPolicy, onlyOrders map[string]struct{}) (orderTrackingRetentionSweepResult, error) {
	result := orderTrackingRetentionSweepResult{}
	var errs []error
	for i, store := range stores {
		if store == nil {
			continue
		}
		n, err := sweepClosedOrderTrackingRetention(store, now, policy, onlyOrders)
		result.deleted += n
		if err != nil {
			errs = append(errs, fmt.Errorf("pruning closed order-tracking %s: %w", orderTrackingSweepStoreLabel(store, i), err))
			continue
		}
		result.storesSwept++
	}
	return result, errors.Join(errs...)
}

// sweepClosedOrderTrackingRetentionAcrossStoresBounded is the watchdog variant
// of sweepClosedOrderTrackingRetentionAcrossStores. It stops once the total
// deletion count across all stores reaches limit, returning the partial deleted
// count with a nil error on budget exhaustion. Store errors are returned as
// normal; deletion errors within budget are propagated.
func sweepClosedOrderTrackingRetentionAcrossStoresBounded(stores []beads.Store, now time.Time, policy orderTrackingRetentionPolicy, onlyOrders map[string]struct{}, limit int) (int, error) { //nolint:unparam // onlyOrders is nil at all current call sites; preserved for API parity with the unbounded variant
	if limit <= 0 {
		return 0, nil
	}
	deleted := 0
	var errs []error
	for i, store := range stores {
		if store == nil {
			continue
		}
		remaining := limit - deleted
		if remaining <= 0 {
			break
		}
		// Enforce the global budget by passing the remaining allowance to the
		// per-store bounded sweep, which stops deleting once it is spent.
		n, err := sweepClosedOrderTrackingRetentionBounded(store, now, policy, onlyOrders, remaining)
		deleted += n
		if err != nil {
			errs = append(errs, fmt.Errorf("pruning closed order-tracking %s: %w", orderTrackingSweepStoreLabel(store, i), err))
		}
	}
	return deleted, errors.Join(errs...)
}

func sweepClosedOrderTrackingRetention(store beads.Store, now time.Time, policy orderTrackingRetentionPolicy, onlyOrders map[string]struct{}) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("bead store unavailable")
	}
	if policy.deleteAfterClose <= 0 {
		return 0, nil
	}
	// retainLast is intentionally package-internal and hardcoded; config can
	// shorten the TTL but cannot remove the recent-history floor.
	if policy.retainLast < minClosedOrderTrackingRetained {
		policy.retainLast = minClosedOrderTrackingRetained
	}
	entries, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Status:   "closed",
		Label:    labelOrderTracking,
		Sort:     beads.SortCreatedDesc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return 0, fmt.Errorf("listing closed order-tracking beads: %w", err)
	}

	byOrder := make(map[string][]beads.Bead)
	for _, entry := range entries {
		scopedName, ok := orderTrackingRetentionBucket(entry, onlyOrders)
		if len(onlyOrders) > 0 {
			if !ok {
				continue
			}
		}
		if !ok {
			scopedName = legacyOrderTrackingRetentionBucket
		}
		byOrder[scopedName] = append(byOrder[scopedName], entry)
	}

	cutoff := now.Add(-policy.deleteAfterClose)
	deleted := 0
	var deleteErr error
	for _, entries := range byOrder {
		sort.Slice(entries, func(i, j int) bool {
			left := orderTrackingClosedReferenceTime(entries[i])
			right := orderTrackingClosedReferenceTime(entries[j])
			if left.Equal(right) {
				return entries[i].ID > entries[j].ID
			}
			return left.After(right)
		})
		if len(entries) <= policy.retainLast {
			continue
		}
		for _, entry := range entries[policy.retainLast:] {
			if !orderTrackingClosedReferenceTime(entry).Before(cutoff) {
				continue
			}
			if err := deleteWorkflowBead(store, entry.ID); err != nil {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting closed order-tracking bead %q: %w", entry.ID, err))
				continue
			}
			deleted++
		}
	}
	return deleted, deleteErr
}

// sweepClosedOrderTrackingRetentionBounded is the per-store bounded variant of
// sweepClosedOrderTrackingRetention. It stops deleting once limit deletions have
// occurred within this store call. On budget exhaustion it returns the partial
// count with a nil error; delete errors are still propagated.
func sweepClosedOrderTrackingRetentionBounded(store beads.Store, now time.Time, policy orderTrackingRetentionPolicy, onlyOrders map[string]struct{}, limit int) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("bead store unavailable")
	}
	if policy.deleteAfterClose <= 0 || limit <= 0 {
		return 0, nil
	}
	if policy.retainLast < minClosedOrderTrackingRetained {
		policy.retainLast = minClosedOrderTrackingRetained
	}
	entries, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Status:   "closed",
		Label:    labelOrderTracking,
		Sort:     beads.SortCreatedDesc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return 0, fmt.Errorf("listing closed order-tracking beads: %w", err)
	}

	byOrder := make(map[string][]beads.Bead)
	for _, entry := range entries {
		scopedName, ok := orderTrackingRetentionBucket(entry, onlyOrders)
		if len(onlyOrders) > 0 {
			if !ok {
				continue
			}
		}
		if !ok {
			scopedName = legacyOrderTrackingRetentionBucket
		}
		byOrder[scopedName] = append(byOrder[scopedName], entry)
	}

	cutoff := now.Add(-policy.deleteAfterClose)
	deleted := 0
	var deleteErr error
	for _, entries := range byOrder {
		if deleted >= limit {
			break
		}
		sort.Slice(entries, func(i, j int) bool {
			left := orderTrackingClosedReferenceTime(entries[i])
			right := orderTrackingClosedReferenceTime(entries[j])
			if left.Equal(right) {
				return entries[i].ID > entries[j].ID
			}
			return left.After(right)
		})
		if len(entries) <= policy.retainLast {
			continue
		}
		for _, entry := range entries[policy.retainLast:] {
			if deleted >= limit {
				break
			}
			if !orderTrackingClosedReferenceTime(entry).Before(cutoff) {
				continue
			}
			if err := deleteWorkflowBead(store, entry.ID); err != nil {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting closed order-tracking bead %q: %w", entry.ID, err))
				continue
			}
			deleted++
		}
	}
	return deleted, deleteErr
}

func orderTrackingRetentionBucket(entry beads.Bead, onlyOrders map[string]struct{}) (string, bool) {
	scopedName, ok := orderNameFromTrackingBead(entry)
	if !ok {
		return "", false
	}
	if len(onlyOrders) > 0 {
		if _, ok := onlyOrders[scopedName]; !ok {
			return "", false
		}
	}
	return scopedName, true
}

func orderTrackingClosedReferenceTime(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
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
			openDescendants, err := storeHasOpenDescendants(store, root.ID, nil)
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
	reader := beads.HandlesFor(store).Live
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := orderWispDescendantChildren(reader, root.ID, parentID)
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
	var exactSourceMatches []string
	var sourceScopedMatches []string
	var localBareMatches []string
	var bareMatches []string
	exactSourceBindings := map[string]bool(nil)
	sourceScopedBindings := map[string]bool(nil)
	if cleanHint != "" {
		exactSourceBindings = sourceBindingsMatchingOrderHint(cfg, dir, cleanHint, agentSourceDirEqualsOrderHint)
		sourceScopedBindings = sourceBindingsMatchingOrderHint(cfg, dir, cleanHint, agentMatchesOrderSourceHint)
	}
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
			if cleanHint != "" {
				if agentSourceDirEqualsOrderHint(*a, cleanHint) || exactSourceBindings[a.BindingName] {
					exactSourceMatches = appendUniquePoolTarget(exactSourceMatches, a.BindingQualifiedName())
				}
				if agentMatchesOrderSourceHint(*a, cleanHint) || sourceScopedBindings[a.BindingName] {
					sourceScopedMatches = appendUniquePoolTarget(sourceScopedMatches, a.BindingQualifiedName())
				}
			}
		}
	}

	// Exact SourceDir matches (and their binding closure) take priority over
	// tail matches: distinct packs can share the same trailing two path
	// components (a city-local fork at packs/<name> vs the builtin pack
	// materialized at .gc/system/packs/<name>), and a hint that names one of
	// them exactly must not go ambiguous because the other tail-matches.
	switch {
	case len(exactQualified) == 1:
		return exactQualified[0], true, nil
	case len(exactQualified) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(exactQualified, ", "))
	case len(exactSourceMatches) == 1:
		return exactSourceMatches[0], true, nil
	case len(exactSourceMatches) > 1:
		return "", false, fmt.Errorf("ambiguous pool %q for %s: matches %s", pool, scope, strings.Join(exactSourceMatches, ", "))
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

func sourceBindingsMatchingOrderHint(cfg *config.City, dir, cleanHint string, matches func(config.Agent, string) bool) map[string]bool {
	bindings := make(map[string]bool)
	for i := range cfg.Agents {
		a := cfg.Agents[i]
		if a.Dir != dir {
			continue
		}
		if matches(a, cleanHint) {
			bindings[a.BindingName] = true
		}
	}
	return bindings
}

func agentSourceDirEqualsOrderHint(a config.Agent, cleanHint string) bool {
	return a.SourceDir != "" && filepath.Clean(a.SourceDir) == cleanHint
}

func agentMatchesOrderSourceHint(a config.Agent, cleanHint string) bool {
	if agentSourceDirEqualsOrderHint(a, cleanHint) {
		return true
	}
	if a.SourceDir == "" {
		return false
	}
	return packSourceTailMatches(filepath.Clean(a.SourceDir), cleanHint)
}

func packSourceTailMatches(source, hint string) bool {
	sourceTail := lastPathComponents(source, 2)
	hintTail := lastPathComponents(hint, 2)
	if len(sourceTail) != 2 || len(hintTail) != 2 {
		return false
	}
	for i := range sourceTail {
		if sourceTail[i] != hintTail[i] {
			return false
		}
	}
	return true
}

func lastPathComponents(path string, n int) []string {
	clean := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(clean, "/")
	compact := parts[:0]
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		compact = append(compact, part)
	}
	if len(compact) < n {
		return nil
	}
	return compact[len(compact)-n:]
}

func appendUniquePoolTarget(values []string, want string) []string {
	for _, value := range values {
		if value == want {
			return values
		}
	}
	return append(values, want)
}
