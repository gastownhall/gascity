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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/closeorder"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

const (
	// labelOrderTracking is the label applied to order-dispatch tracking beads.
	// coordclass mirrors this string privately (as labelOrderTracking) for store
	// routing; the two must stay in sync.
	labelOrderTracking    = "order-tracking"
	labelTriggerEnvFailed = "trigger-env-failed"

	orderTrackingSweepOrder                = "order-tracking-sweep"
	orderTrackingBeadPolicyName            = "order_tracking"
	defaultOrderTrackingSweepStaleAfter    = 10 * time.Minute
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

// defaultOrderTrackingDeleteAfterClose is derived from the canonical config
// constant so both load-time defaults and the runtime fallback stay in sync.
var defaultOrderTrackingDeleteAfterClose = config.BeadPolicyConfig{
	DeleteAfterClose: config.DefaultOrderTrackingDeleteAfterClose,
}.DeleteAfterCloseDuration()

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
	gateBackoffUntil     map[string]time.Time

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
	// the map reads/writes below, never across the RecentRunsAll/OpenRuns bd
	// calls, so one slow or contended store read cannot stall sibling gates (the
	// property gateOpenWorkBounded exists to preserve).
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

	// Filter out manual- and webhook-trigger orders — they are never
	// auto-dispatched (webhook orders fire only via the supervisor receiver).
	var auto []orders.Order
	for _, a := range allAA {
		if a.Trigger != "manual" && a.Trigger != "webhook" {
			auto = append(auto, a)
		}
	}
	if len(auto) == 0 {
		return nil
	}

	return newMemoryOrderDispatcher(auto, cityPath, cfg, rec, stderr)
}

// newMemoryOrderDispatcher builds a memoryOrderDispatcher over a resolved order
// set. aa is the tick loop's auto-dispatchable set; it may be nil for callers
// that only fire pre-resolved orders through the orderdispatch.Dispatcher seam
// (the webhook receiver), where the tick dispatch() path is never invoked.
func newMemoryOrderDispatcher(aa []orders.Order, cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer) *memoryOrderDispatcher {
	if cfg == nil {
		cfg = &config.City{}
	}
	// Extract events.Provider from recorder if available.
	// FileRecorder implements Provider; Discard does not.
	var ep events.Provider
	if p, ok := rec.(events.Provider); ok {
		ep = p
	}

	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	return &memoryOrderDispatcher{
		aa: aa,
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
		if m.gateBackoffActive(scoped, now) {
			continue
		}
		hasOpenTracking, err := gateOpenWorkBounded(ctx, orderGateTimeout, scoped, func() (bool, error) {
			return trackingIndex.hasOpenTracking(storesForGate, storeKeysForGate, scoped)
		})
		if err != nil {
			if m.gateFailClosed(ctx, a, scoped, err) {
				if errors.Is(err, errGateTimeout) {
					// Anchor to actual wall clock after the gate consumed orderGateTimeout;
					// using the tick-start 'now' would set a deadline that has already passed.
					m.setGateBackoff(scoped, time.Now().Add(orderGateBackoffDuration))
				}
				continue
			}
		}
		if hasOpenTracking {
			continue
		}

		baseLastRunFn := trackingIndex.lastRunFunc(storesForGate, storeKeysForGate, orders.LastRunAcross(orderFrontDoorsForStores(storesForGate)))
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
		cursorFn := orders.CursorAcross(orderFrontDoorsForStores(storesForGate))
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
			trackingBead, createErr := orders.NewStore(beads.OrdersStore{Store: store}).CreateRun(scoped, orders.RunOpts{Outcome: orders.RunOutcomeTriggerEnvFailed})
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
				if errors.Is(err, errGateTimeout) {
					// Anchor to actual wall clock after the gate consumed orderGateTimeout;
					// using the tick-start 'now' would set a deadline that has already passed.
					m.setGateBackoff(scoped, time.Now().Add(orderGateBackoffDuration))
				}
				continue
			}
		}
		if hasOpenWork {
			continue
		}

		// Create the tracking bead (which suppresses re-fire on the next tick)
		// and launch the shared dispatch core. The webhook receiver fires the
		// same launchResolvedDispatch → dispatchOne path through the exported
		// seam, so a tick dispatch and a webhook dispatch run the identical core,
		// not two implementations. inFlight (this tick's WaitGroup) is reserved
		// before the launch and released via onDone; on a create failure nothing
		// launched, so it is released immediately to balance the reservation.
		//
		// Auto-triggered orders carry no args channel: vars/execEnv are nil.
		inFlight.Add(1)
		trackingBead, err := m.launchResolvedDispatch(ctx, store, target, a, cityPath, nil, nil, inFlight.Done)
		if err != nil {
			inFlight.Done()
			logDispatchError(m.stderr, "gc: order dispatch: creating tracking bead for %s: %v", scoped, err)
			continue
		}
		m.rememberLastRun(scoped, storeKeysForGate, trackingBead.CreatedAt)
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
func (m *memoryOrderDispatcher) launchDispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string, vars, execEnv map[string]string, onDone func()) {
	if onDone == nil {
		onDone = func() {}
	}
	if m.dispatchCtx == nil {
		go func() {
			defer onDone()
			m.runDispatchGuarded(ctx, store, target, a, cityPath, trackingID, vars, execEnv)
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
		m.runDispatchGuarded(mergedCtx, store, target, a, cityPath, trackingID, vars, execEnv)
	}()
}

// runDispatchGuarded runs dispatchOne with a panic boundary. A dispatch goroutine
// is detached from the request/tick that launched it — the webhook fast-ACK path
// in particular returns its HTTP response (past any recovery middleware) before
// this goroutine runs — so a panic here (e.g. while processing untrusted
// webhook-derived args) would otherwise crash the whole supervisor. dispatchOne's
// own defers close the tracking bead as the stack unwinds before recovery here;
// this boundary logs the panic and contains it to the single dispatch.
func (m *memoryOrderDispatcher) runDispatchGuarded(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string, vars, execEnv map[string]string) {
	defer func() {
		if p := recover(); p != nil {
			logDispatchError(m.stderr, "gc: order %s: dispatch goroutine panic (tracking %s): %v", a.ScopedName(), trackingID, p)
		}
	}()
	m.dispatchOne(ctx, store, target, a, cityPath, trackingID, vars, execEnv)
}

// launchResolvedDispatch is the single fire path shared by the controller tick
// loop and the webhook dispatch seam (memoryOrderDispatcher.Dispatch). It writes
// the order-tracking bead that suppresses re-fire, registers the in-flight
// goroutine so drain can await outcome persistence, and launches dispatchOne.
// vars drive required-param validation and the formula ExpandVars channel;
// execEnv is the exec-env overlay (nil ⇒ vars) that untrusted callers
// pre-namespace for R4. onDone runs after the dispatch goroutine returns (the
// tick loop passes its per-tick store barrier; the seam passes its store-close).
// A caller tracking its own WaitGroup must register it before calling and
// release it in onDone (and, on a returned error, itself — nothing launched).
func (m *memoryOrderDispatcher) launchResolvedDispatch(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath string, vars, execEnv map[string]string, onDone func()) (orders.OrderRun, error) {
	trackingRun, err := orders.NewStore(beads.OrdersStore{Store: store}).CreateRun(a.ScopedName(), orders.RunOpts{})
	if err != nil {
		return orders.OrderRun{}, err
	}
	m.addInflight()
	m.launchDispatchOne(ctx, store, target, a, cityPath, trackingRun.ID, vars, execEnv, onDone)
	return trackingRun, nil
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
		// The in-memory history index (limit orderTrackingHistoryIndexLimit,
		// newest-first) is authoritative for any recently-run order: a non-zero
		// entry is that order's true last run. Only a genuine index miss pays
		// the per-order fallback query. Without this gate, every cooldown/cron
		// order runs the fallback on each cold-cache (post-reload) dispatch —
		// N serial bd-queries that hang gc reload/gc doctor (#3201; residual of
		// #3191, which #3197's per-query cap bounded but did not eliminate).
		if latest.IsZero() && fallback != nil {
			return fallback(scopedName)
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
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).RecentRunsAll(orderTrackingHistoryIndexLimit)
	if err != nil {
		wrapped := fmt.Errorf("listing order-tracking history: %w", err)
		idx.mu.Lock()
		idx.errs[key] = wrapped
		idx.mu.Unlock()
		return nil, wrapped
	}
	entries := make(map[string]orderTrackingSummary)
	for _, run := range runs {
		summary := entries[run.Scoped]
		if run.CreatedAt.After(summary.lastRun) {
			summary.lastRun = run.CreatedAt
		}
		entries[run.Scoped] = summary
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
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).OpenRuns()
	if err != nil {
		wrapped := fmt.Errorf("listing order-tracking beads: %w", err)
		idx.mu.Lock()
		idx.errs[storeKey] = wrapped
		idx.mu.Unlock()
		return nil, wrapped
	}
	entries := make(map[string]orderTrackingSummary)
	for _, run := range runs {
		summary := entries[run.Scoped]
		// OpenRuns filters Status=="open" in the query, so every returned run
		// is open tracking work.
		summary.openTracking = true
		entries[run.Scoped] = summary
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

// carryLastRunCacheFrom copies warm last-run entries from a previous
// dispatcher into this one, so a reload/rescan-triggered rebuild reuses them
// instead of cold-starting and re-querying every order (#3201). last-run times
// are historical truth unaffected by a config reload; entries only ever move
// forward. Callers invoke this after draining the previous dispatcher, when no
// goroutine still writes its cache.
func (m *memoryOrderDispatcher) carryLastRunCacheFrom(prev *memoryOrderDispatcher) {
	if m == nil || prev == nil {
		return
	}
	prev.cacheMu.Lock()
	defer prev.cacheMu.Unlock()
	if len(prev.lastRunCache) == 0 {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.lastRunCache == nil {
		m.lastRunCache = make(map[string]time.Time, len(prev.lastRunCache))
	}
	for key, last := range prev.lastRunCache {
		if existing, ok := m.lastRunCache[key]; !ok || last.After(existing) {
			m.lastRunCache[key] = last
		}
	}
}

// gateBackoffActive reports whether the named order is in a gate-timeout
// backoff window. Call before either open-work gate to suppress re-entry after
// a prior timeout; the backoff expires naturally so the order resumes once the
// store recovers.
func (m *memoryOrderDispatcher) gateBackoffActive(key string, now time.Time) bool {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.gateBackoffUntil == nil {
		return false
	}
	until, ok := m.gateBackoffUntil[key]
	return ok && now.Before(until)
}

// setGateBackoff records a gate-timeout backoff for the named order. The
// backoff suppresses both open-work gates on every subsequent tick until the
// deadline passes (see gateBackoffActive). Only the latest/furthest deadline
// is retained.
func (m *memoryOrderDispatcher) setGateBackoff(key string, until time.Time) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.gateBackoffUntil == nil {
		m.gateBackoffUntil = make(map[string]time.Time)
	}
	if existing, ok := m.gateBackoffUntil[key]; !ok || until.After(existing) {
		m.gateBackoffUntil[key] = until
	}
}

// carryGateBackoffFrom copies non-expired gate-backoff entries from a previous
// dispatcher so a reload/rescan-triggered rebuild preserves active backoffs.
// now is used to filter out already-expired entries; only call after draining
// the previous dispatcher.
func (m *memoryOrderDispatcher) carryGateBackoffFrom(prev *memoryOrderDispatcher, now time.Time) {
	if m == nil || prev == nil {
		return
	}
	prev.cacheMu.Lock()
	defer prev.cacheMu.Unlock()
	if len(prev.gateBackoffUntil) == 0 {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.gateBackoffUntil == nil {
		m.gateBackoffUntil = make(map[string]time.Time, len(prev.gateBackoffUntil))
	}
	for key, until := range prev.gateBackoffUntil {
		if until.After(now) {
			if existing, ok := m.gateBackoffUntil[key]; !ok || until.After(existing) {
				m.gateBackoffUntil[key] = until
			}
		}
	}
}

func orderHistoryCacheKey(orderName string, storeKeys []string) string {
	return orderName + "\x00" + strings.Join(storeKeys, "\x00")
}

func orderTriggerUsesLastRun(a orders.Order) bool {
	return a.Trigger == "cooldown" || a.Trigger == "cron"
}

// dispatchOne runs a single order dispatch in its own goroutine.
// For exec orders, runs the script directly. For formula orders,
// instantiates a wisp. Emits events and updates the tracking bead.
//
// vars are the raw, param-named dispatch args: they drive required-param
// validation and the formula ExpandVars channel. execEnv is the exec-env
// overlay for exec orders (nil ⇒ fall back to vars). The webhook sink passes a
// namespaced execEnv (GC_WEBHOOK_ARG_*) so an untrusted payload can never shadow
// a controller-owned or static [order.env] key (R4); the tick loop and CLI pass
// nil (raw overlay), preserving existing semantics.
func (m *memoryOrderDispatcher) dispatchOne(ctx context.Context, store beads.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string, vars, execEnv map[string]string) {
	// Defer order matters: doneInflight runs last, after Close makes the
	// tracking bead outcome observable to a waiting drain.
	defer m.doneInflight()
	defer func() {
		if err := closeOrderTrackingBead(ctx, store, trackingID); err != nil {
			logDispatchError(m.stderr, "gc: order %s: closing tracking bead %s: %v", a.ScopedName(), trackingID, err)
		}
	}()

	scoped := a.ScopedName()

	// Refuse to fire when a declared-required param is absent from the dispatch
	// vars. The tracking bead was already created by the caller and is closed by
	// the deferred close above.
	if err := orders.ValidateRequiredParams(a, vars); err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		return
	}

	timeout := effectiveTimeout(a, m.maxTimeout)
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m.rec.Record(events.Event{
		Type:    events.OrderFired,
		Actor:   "controller",
		Subject: scoped,
	})

	if a.IsExec() {
		// dispatchExec is order-only: hand it the typed order front door so the
		// goroutine leaf has no raw beads.Store to misuse. Constructed here (the
		// per-order coordination point that still holds the raw store for the
		// closeOrderTrackingBead defer) from the same store, so the bead writes
		// stay byte-identical.
		front := orders.NewStore(beads.OrdersStore{Store: store})
		// The exec-env overlay is namespaced by an untrusted caller (webhook);
		// nil means use the raw vars (tick/CLI), preserving prior behavior.
		execOverlay := execEnv
		if execOverlay == nil {
			execOverlay = vars
		}
		m.dispatchExec(childCtx, front, target, a, cityPath, trackingID, execOverlay)
	} else {
		m.dispatchWisp(childCtx, store, a, cityPath, trackingID, vars)
	}
}

func closeOrderTrackingBead(ctx context.Context, store beads.Store, trackingID string) error {
	_, err := orders.NewStore(beads.OrdersStore{Store: store}).CloseRuns(ctx, []string{trackingID}, completedOrderTrackingCloseReason)
	return err
}

// closeAndVerifyOrderTrackingBeads survives the WI-3 orders migration ONLY for
// the stale sweep, which stamps richer sweep-vocabulary metadata (order_tracking_sweep
// + initiator) that orders.Store.CloseRuns's close_reason-only signature does not
// carry. The close_reason-only sites moved onto CloseRuns.
//
// DRIFT GUARD: this retry loop is a deliberate twin of orders.Store.CloseRuns
// (orderTrackingCloseVerifyAttempts/orderTrackingCloseVerifyRetryDelay mirror
// closeVerifyAttempts/closeVerifyRetryDelay). Any change to the retry policy MUST
// land in both.
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
func (m *memoryOrderDispatcher) dispatchExec(ctx context.Context, front *orders.Store, target execStoreTarget, a orders.Order, cityPath, trackingID string, vars map[string]string) {
	scoped := a.ScopedName()
	outcome := orders.RunOutcomeExec
	var headSeq uint64
	var hasEventCursor bool
	if a.Trigger == "event" && m.ep != nil {
		var err error
		headSeq, err = m.ep.LatestSeq()
		if err != nil {
			errMsg := fmt.Sprintf("reading event cursor: %v", err)
			outcome = orders.RunOutcomeExecFailed
			logDispatchError(m.stderr, "gc: order dispatch: reading event cursor for %s: %v", scoped, err)
			if updateErr := front.SetOutcome(trackingID, outcome); updateErr != nil {
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
		if err := front.SetCursor(trackingID, scoped, orders.EventCursor(headSeq)); err != nil {
			logDispatchError(m.stderr, "gc: order %s: failed to label exec event cursor on tracking bead %s: %v", scoped, trackingID, err)
			outcome = orders.RunOutcomeExecFailed
			if updateErr := front.SetOutcome(trackingID, outcome); updateErr != nil {
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

	env, err := orderExecEnvWithError(cityPath, m.cfg, target, a, vars)
	var output []byte
	var execErrMsg string
	if err != nil {
		redactionEnv := append(os.Environ(), env...)
		redacted := redactOrderEnvError(err, redactionEnv)
		execErrMsg = "exec env failed: " + redacted
		outcome = orders.RunOutcomeExecEnvFailed
		logDispatchError(m.stderr, "gc: order exec %s env failed: %s", scoped, redacted)
	} else {
		output, err = m.execRun(ctx, a.Exec, target.ScopeRoot, env)
		if err != nil {
			redactionEnv := append(os.Environ(), env...)
			execErrMsg = execenv.RedactText(err.Error(), redactionEnv)
			outcome = orders.RunOutcomeExecFailed
			logDispatchError(m.stderr, "gc: order exec %s failed: %s", scoped, execErrMsg)
			if len(output) > 0 {
				logDispatchError(m.stderr, "gc: order exec %s output: %s", scoped, execenv.RedactText(string(output), redactionEnv))
			}
		}
	}

	// Label tracking bead with outcome via store (not CLI). For event execs,
	// cursor labels were already persisted before the command ran.
	if err := front.SetOutcome(trackingID, outcome); err != nil {
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

func prepareOrderWispRecipe(ctx context.Context, store beads.Store, a orders.Order, searchPaths []string, vars map[string]string) (*formula.Recipe, error) {
	inv, err := graphv2.PrepareInvocation(ctx, store, a.Formula, searchPaths, "", vars)
	if err != nil {
		return nil, err
	}
	return formula.CompileWithoutRuntimeVarValidation(ctx, a.Formula, searchPaths, inv.Vars)
}

func poolOrderRouteVisibilityWarning(a orders.Order, recipe *formula.Recipe) string {
	if strings.TrimSpace(a.Pool) == "" || formula.RecipeHasReadySurface(recipe) {
		return ""
	}
	return fmt.Sprintf("warning: pool order %q uses formula %q whose root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase=\"vapor\"/root-only or formulas v2 before routing it to a pool.", a.ScopedName(), a.Formula)
}

func redactOrderEnvError(err error, env []string) string {
	if err == nil {
		return ""
	}
	return execenv.RedactText(err.Error(), env)
}

// dispatchWisp instantiates a wisp from the order's formula.
func (m *memoryOrderDispatcher) dispatchWisp(ctx context.Context, store beads.Store, a orders.Order, cityPath, trackingID string, vars map[string]string) {
	scoped := a.ScopedName()

	if err := ctx.Err(); err != nil {
		m.rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: scoped,
			Message: err.Error(),
		})
		orders.NewStore(beads.OrdersStore{Store: store}).SetOutcome(trackingID, orders.RunOutcomeWispCanceled) //nolint:errcheck // best-effort
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
	recipe, err := prepareOrderWispRecipe(ctx, store, a, searchPaths, vars)
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
		update.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: pool}
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
	orders.NewStore(beads.OrdersStore{Store: store}).SetOutcome(trackingID, orders.RunOutcomeWisp) //nolint:errcheck // best-effort
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
	var cursor *orders.EventCursor
	if a.Trigger == "event" && headSeq > 0 {
		c := orders.EventCursor(headSeq)
		cursor = &c
	}
	front := orders.NewStore(beads.OrdersStore{Store: store})
	if err := front.MarkFailed(trackingID, scoped, orders.RunOutcomeWispFailed, cursor); err != nil {
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
	// The order-run:<scoped> single-flight list is a MIXED orders+graph read:
	// the label rides both order-tracking beads (orders class) and wisp/molecule
	// roots (graph class). Route it through the two-class edge so a graph-store
	// split still unions both classes; on a single-store city the two legs wrap
	// the same store and the union deduplicates to one read (byte-identical). The
	// wisp-root subtree verdict stays graph-owned via the injected predicate.
	front := orders.NewStoreWithGraph(
		beads.OrdersStore{Store: store},
		beads.GraphStore{Store: store},
	)
	return front.HasOpenWork(scopedName, m.wispRootHasOpenWork)
}

// wispRootHasOpenWork is the graph-owned half of the single-flight gate: given an
// open order-run:<scoped> bead that is NOT an order-tracking bead, it decides
// whether the wisp/molecule root still has open work. A root-only wisp counts as
// in-flight; a molecule root counts only if its subtree still has open
// descendants. It stays in the controller because the subtree walk is graph
// residual (molecule membership + graph traversal).
func (m *memoryOrderDispatcher) wispRootHasOpenWork(store beads.Store, b beads.Bead) (bool, error) {
	if !isOrderWispRootCandidate(b) {
		return false, nil
	}
	if isOrderRootOnlyWispCandidate(b) {
		return true, nil
	}
	hasOpenDescendants, err := storeHasOpenDescendants(store, b.ID, isTransientNotificationBead)
	if err != nil {
		return false, fmt.Errorf("checking open descendants of wisp %s: %w", b.ID, err)
	}
	return hasOpenDescendants, nil
}

func isOrderWispRootCandidate(b beads.Bead) bool {
	if beads.IsMoleculeType(b.Type) {
		return true
	}
	return b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow || b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp
}

func isOrderRootOnlyWispCandidate(b beads.Bead) bool {
	return b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp && !beads.IsMoleculeType(b.Type)
}

// isTransientNotificationBead reports whether a bead is a short-lived delivery
// chore (a nudge or a mail/message) rather than substantive order work. Such
// beads inherit an order wisp's order-run label via the parent-child graph but
// are reaped on their own TTL, so they must not keep the single-flight open-work
// gate "open" and block the order from re-dispatching (#2893, de-noise).
func isTransientNotificationBead(b beads.Bead) bool {
	if beadmail.IsMessageBead(b) {
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
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
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

func orderWispMetadataDescendants(reader beads.LiveReader, rootID string, includeClosed bool) ([]beads.Bead, error) {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return nil, nil
	}
	query := beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: includeClosed,
	}
	descendants, err := reader.List(query)
	if err != nil {
		return nil, fmt.Errorf("listing graph metadata descendants for %s: %w", rootID, err)
	}
	return descendants, nil
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
	return child.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID
}

func orderWispMayHaveGraphDependents(bead beads.Bead) bool {
	if isOrderWispRootCandidate(bead) {
		return true
	}
	if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != "" {
		return true
	}
	if bead.Metadata[beadmeta.StepRefMetadataKey] != "" {
		return true
	}
	if bead.Metadata[beadmeta.LogicalBeadIDMetadataKey] != "" {
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

// orderGateBackoffDuration is the suppression window set after a gate timeout,
// anchored to the actual wall clock at the moment the timeout fires (not the
// tick-start timestamp). It is intentionally larger than orderGateTimeout so
// the expensive gate query is genuinely skipped for a bounded span; an equal
// window would be consumed by the gate itself, yielding no real suppression.
var orderGateBackoffDuration = 24 * time.Second

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
	// OrphanedOpenRuns lists the OPEN tracking beads across both tiers (new wisp
	// + legacy issues) and excludes the trigger-env-failure markers the open-work
	// gate intentionally keeps open.
	front := orders.NewStore(beads.OrdersStore{Store: store})
	runs, err := front.OrphanedOpenRuns()
	if err != nil {
		return 0, fmt.Errorf("listing order-tracking beads: %w", err)
	}
	if len(runs) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := front.CloseRuns(context.Background(), ids, orphanedOrderTrackingCloseReason)
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
	return sweepStaleOrderTrackingAcrossStoresLimitMode(stores, now, staleAfter, onlyOrders, initiator, includeWispSubtrees, limit, false)
}

func sweepStaleOrderTrackingAcrossStoresDryRun(stores []beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, includeWispSubtrees bool) (orderTrackingSweepResult, error) {
	return sweepStaleOrderTrackingAcrossStoresLimitMode(stores, now, staleAfter, onlyOrders, orderTrackingSweepMetadataInitiator, includeWispSubtrees, 0, true)
}

func sweepStaleOrderTrackingAcrossStoresLimitMode(stores []beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, includeWispSubtrees bool, limit int, dryRun bool) (orderTrackingSweepResult, error) {
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
		partial, err := sweepStaleOrderTrackingWithOptionsLimitMode(store, now, staleAfter, onlyOrders, initiator, includeWispSubtrees, remainingLimit, dryRun)
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
	return sweepStaleOrderTrackingWithOptionsLimitMode(store, now, staleAfter, onlyOrders, initiator, includeWispSubtrees, limit, false)
}

func sweepStaleOrderTrackingWithOptionsLimitDryRun(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, includeWispSubtrees bool, limit int) (orderTrackingSweepResult, error) {
	return sweepStaleOrderTrackingWithOptionsLimitMode(store, now, staleAfter, onlyOrders, initiator, includeWispSubtrees, limit, true)
}

func sweepStaleOrderTrackingWithOptionsLimitMode(store beads.Store, now time.Time, staleAfter time.Duration, onlyOrders map[string]struct{}, initiator string, includeWispSubtrees bool, limit int, dryRun bool) (orderTrackingSweepResult, error) {
	if staleAfter <= 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("stale-after must be positive")
	}
	if includeWispSubtrees && len(onlyOrders) == 0 {
		return orderTrackingSweepResult{}, fmt.Errorf("include-wisps requires at least one order name")
	}
	cutoff := now.Add(-staleAfter)
	// StaleOpenRuns is the typed read half: OPEN tracking runs at or before the
	// cutoff, across both tiers, with best-effort names. The sweep-vocabulary
	// close (below) stays raw because it stamps sweep audit metadata that the
	// domain object deliberately omits, and the wisp-subtree recovery is graph
	// residual.
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).StaleOpenRuns(cutoff)
	if err != nil {
		return orderTrackingSweepResult{}, fmt.Errorf("listing order-tracking beads: %w", err)
	}

	result := orderTrackingSweepResult{}
	var ids []string
	for _, run := range runs {
		if len(onlyOrders) > 0 {
			if run.Scoped == "" {
				continue
			}
			if _, ok := onlyOrders[run.Scoped]; !ok {
				continue
			}
		}
		ids = append(ids, run.ID)
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	if len(ids) == 0 {
		if !includeWispSubtrees {
			return result, nil
		}
	} else {
		if dryRun {
			result.trackingClosed = len(ids)
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
	}

	if includeWispSubtrees {
		n, err := sweepStaleOrderWispSubtreesMode(store, cutoff, onlyOrders, initiator, dryRun)
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
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
	if err != nil {
		return 0, fmt.Errorf("listing closed order-tracking beads: %w", err)
	}

	byOrder := bucketClosedRetentionRuns(runs, onlyOrders)

	cutoff := now.Add(-policy.deleteAfterClose)
	deleted := 0
	var deleteErr error
	for _, runs := range byOrder {
		sort.Slice(runs, func(i, j int) bool {
			left := orderTrackingClosedReferenceTime(runs[i])
			right := orderTrackingClosedReferenceTime(runs[j])
			if left.Equal(right) {
				return runs[i].ID > runs[j].ID
			}
			return left.After(right)
		})
		if len(runs) <= policy.retainLast {
			continue
		}
		for _, run := range runs[policy.retainLast:] {
			if !orderTrackingClosedReferenceTime(run).Before(cutoff) {
				continue
			}
			// deleteWorkflowBead is the graph-aware delete (dep unwind) the
			// retention prune uses; it stays raw graph residual.
			if err := deleteWorkflowBead(store, run.ID); err != nil {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting closed order-tracking bead %q: %w", run.ID, err))
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
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
	if err != nil {
		return 0, fmt.Errorf("listing closed order-tracking beads: %w", err)
	}

	byOrder := bucketClosedRetentionRuns(runs, onlyOrders)

	cutoff := now.Add(-policy.deleteAfterClose)
	deleted := 0
	var deleteErr error
	for _, runs := range byOrder {
		if deleted >= limit {
			break
		}
		sort.Slice(runs, func(i, j int) bool {
			left := orderTrackingClosedReferenceTime(runs[i])
			right := orderTrackingClosedReferenceTime(runs[j])
			if left.Equal(right) {
				return runs[i].ID > runs[j].ID
			}
			return left.After(right)
		})
		if len(runs) <= policy.retainLast {
			continue
		}
		for _, run := range runs[policy.retainLast:] {
			if deleted >= limit {
				break
			}
			if !orderTrackingClosedReferenceTime(run).Before(cutoff) {
				continue
			}
			if err := deleteWorkflowBead(store, run.ID); err != nil {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting closed order-tracking bead %q: %w", run.ID, err))
				continue
			}
			deleted++
		}
	}
	return deleted, deleteErr
}

func orderTrackingRetentionBucket(run orders.OrderRun, onlyOrders map[string]struct{}) (string, bool) {
	if run.Scoped == "" {
		return "", false
	}
	if len(onlyOrders) > 0 {
		if _, ok := onlyOrders[run.Scoped]; !ok {
			return "", false
		}
	}
	return run.Scoped, true
}

func orderTrackingClosedReferenceTime(run orders.OrderRun) time.Time {
	if !run.UpdatedAt.IsZero() {
		return run.UpdatedAt
	}
	return run.CreatedAt
}

// bucketClosedRetentionRuns groups closed retention runs by order name, routing
// unresolvable-name runs to the legacy bucket (only when no order filter is set,
// matching the raw sweep).
func bucketClosedRetentionRuns(runs []orders.OrderRun, onlyOrders map[string]struct{}) map[string][]orders.OrderRun {
	byOrder := make(map[string][]orders.OrderRun)
	for _, run := range runs {
		scopedName, ok := orderTrackingRetentionBucket(run, onlyOrders)
		if len(onlyOrders) > 0 {
			if !ok {
				continue
			}
		}
		if !ok {
			scopedName = legacyOrderTrackingRetentionBucket
		}
		byOrder[scopedName] = append(byOrder[scopedName], run)
	}
	return byOrder
}

func sweepStaleOrderWispSubtrees(store beads.Store, cutoff time.Time, onlyOrders map[string]struct{}) (int, error) {
	return sweepStaleOrderWispSubtreesMode(store, cutoff, onlyOrders, orderTrackingSweepMetadataInitiator, false)
}

func sweepStaleOrderWispSubtreesMode(store beads.Store, cutoff time.Time, onlyOrders map[string]struct{}, initiator string, dryRun bool) (int, error) {
	batchIDs, handled, err := staleOrderWispSubtreeBatchCloseIDs(store, cutoff, onlyOrders)
	if err != nil {
		return 0, err
	}
	if handled {
		if dryRun || len(batchIDs) == 0 {
			return len(batchIDs), nil
		}
		return closeStaleOrderWispIDs(store, batchIDs, initiator)
	}
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
	if dryRun {
		return len(ids), nil
	}
	ordered, err := closeorder.Order(store, ids)
	if err != nil {
		return 0, fmt.Errorf("ordering stale order wisp closes: %w", err)
	}
	return closeStaleOrderWispIDs(store, ordered, initiator)
}

// closeStaleOrderWispIDs closes ids via Store.CloseAll with the sweep's audit
// metadata. The legacy path pre-orders ids with closeorder.Order; the batch
// path passes them unordered and is safe only because BdStore.CloseAll issues
// `bd close --force`, which closes blocked beads regardless of in-batch order
// (a non-forced wrong-order batch silently skips blocked beads while exiting
// 0). That flag, pinned by TestBdCloseArgsAlwaysForce, is what keeps the
// unordered batch correct on stores that enforce blocks dependencies.
func closeStaleOrderWispIDs(store beads.Store, ids []string, initiator string) (int, error) {
	metadata := map[string]string{
		"order_tracking_sweep": orderTrackingSweepMetadataReason,
		"order_wisp_sweep":     "stale-order-wisp",
		"close_reason":         staleOrderWispCloseReason,
	}
	if initiator != "" {
		metadata["order_tracking_sweep_by"] = initiator
	}
	n, err := store.CloseAll(ids, metadata)
	if err != nil {
		return n, fmt.Errorf("closing stale order wisp subtrees: %w", err)
	}
	return n, nil
}

func staleOrderWispSubtreeBatchCloseIDs(store beads.Store, cutoff time.Time, onlyOrders map[string]struct{}) ([]string, bool, error) {
	if len(onlyOrders) == 0 {
		return nil, false, fmt.Errorf("include-wisps requires at least one order name")
	}
	all, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		// The batch sweep deliberately scans every bead once and groups
		// candidate roots with their descendants in memory, instead of issuing
		// the per-root queries the walk path needs. Closed beads are included
		// so the ParentID closure can traverse closed intermediates down to
		// the open descendants beneath them, matching the walk path's
		// IncludeClosed per-node queries.
		AllowScan:     true,
		IncludeClosed: true,
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing wisp candidates for order-wisp batch sweep: %w", err)
	}
	if len(all) == 0 {
		return nil, false, nil
	}

	descendantsByRoot := make(map[string][]beads.Bead)
	openStampedByRoot := make(map[string]struct{})
	childrenByParent := make(map[string][]beads.Bead)
	for _, bead := range all {
		if bead.ID == "" {
			continue
		}
		if parentID := strings.TrimSpace(bead.ParentID); parentID != "" && parentID != bead.ID {
			childrenByParent[parentID] = append(childrenByParent[parentID], bead)
		}
		rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			continue
		}
		descendantsByRoot[rootID] = append(descendantsByRoot[rootID], bead)
		if bead.Status != "closed" {
			openStampedByRoot[rootID] = struct{}{}
		}
	}

	handled := false
	ids := make([]string, 0)
	seenIDs := make(map[string]struct{})
	for _, root := range all {
		if root.ID == "" || root.Status == "closed" {
			continue
		}
		if beadLabelsContain(root.Labels, labelOrderTracking) {
			continue
		}
		// Force-close roots are matched on the order-run:<name> label only,
		// matching the legacy path's staleOrderWispRoots selection. The
		// order:<title> fallback in orders.NameFromTrackingBead exists for legacy
		// tracking beads; honoring it here would make a workflow root that was
		// never order-poured force-closable just because its title collides
		// with a swept order name.
		name, ok := orders.NameFromOrderRunLabel(root)
		if !ok {
			continue
		}
		if _, ok := onlyOrders[name]; !ok {
			continue
		}
		if root.CreatedAt.IsZero() || !root.CreatedAt.Before(cutoff) {
			continue
		}
		if !isOrderWispRootCandidate(root) {
			continue
		}
		descendants := descendantsByRoot[root.ID]
		if _, hasOpenStamped := openStampedByRoot[root.ID]; !hasOpenStamped && !isOrderRootOnlyWispCandidate(root) {
			// Legacy graph-v2 wisps may only expose descendants through deps.
			return nil, false, nil
		}
		handled = true
		subtree := make([]beads.Bead, 0, 1+len(descendants))
		subtree = append(subtree, root)
		subtree = append(subtree, descendants...)
		subtree = appendParentChainDescendants(subtree, childrenByParent)
		if !openSubtreeOlderThan(subtree, cutoff) {
			continue
		}
		for _, id := range staleOrderWispSubtreeCloseIDs(subtree) {
			if _, ok := seenIDs[id]; ok {
				continue
			}
			seenIDs[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids, handled, nil
}

// appendParentChainDescendants unions a metadata-stamped subtree with every
// bead reachable from it over ParentID edges. Partially-stamped molecules
// carry open ParentID-only children that the gc.root_bead_id grouping cannot
// see; without this union a fresh un-stamped child would no longer veto the
// root's close and a stale one would be stranded open under a closed root —
// the leak class the sweep drains. Closed children are appended and traversed
// too: an open un-stamped descendant can hide behind a closed intermediate (a
// lingering nudge/mail chore parented under an already-closed step is the
// production shape), and only a closed-inclusive index reaches it — the walk
// path resolves the identical shape through its IncludeClosed per-node
// queries. Appending closed beads is safe because openSubtreeOlderThan and
// staleOrderWispSubtreeCloseIDs both filter on status. The closure walks the
// in-memory index built from the batch List, so it adds no store round-trips.
func appendParentChainDescendants(subtree []beads.Bead, childrenByParent map[string][]beads.Bead) []beads.Bead {
	seen := make(map[string]struct{}, len(subtree))
	queue := make([]string, 0, len(subtree))
	for _, b := range subtree {
		if b.ID == "" {
			continue
		}
		seen[b.ID] = struct{}{}
		queue = append(queue, b.ID)
	}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		for _, child := range childrenByParent[parentID] {
			if _, ok := seen[child.ID]; ok {
				continue
			}
			seen[child.ID] = struct{}{}
			subtree = append(subtree, child)
			queue = append(queue, child.ID)
		}
	}
	return subtree
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

// collectOrderWispSubtree returns the root plus every descendant the sweep
// reasons about, as the union of two views: the gc.root_bead_id membership
// set (stamped members regardless of edge linkage) and the authoritative
// ParentID/graph walk seeded with those members. Neither view alone is
// complete — the membership query cannot see the un-stamped ParentID-only
// children of a partially-stamped molecule (the same gap documented on
// storeHasOpenDescendants), and the walk cannot see stamped members linked by
// neither ParentID nor an owned dependency edge. Trusting the metadata set
// alone would let a fresh un-stamped child slip past the freshness veto and a
// stale one be stranded open under a closed root, the leak class this sweep
// drains, so the walk always runs.
func collectOrderWispSubtree(store beads.Store, root beads.Bead) ([]beads.Bead, error) {
	if root.ID == "" {
		return nil, nil
	}
	reader := beads.HandlesFor(store).Live
	seen := map[string]struct{}{root.ID: {}}
	out := []beads.Bead{root}
	queue := []string{root.ID}
	if orderWispMayHaveGraphDependents(root) {
		metadataDescendants, err := orderWispMetadataDescendants(reader, root.ID, true)
		if err != nil {
			return nil, err
		}
		for _, descendant := range metadataDescendants {
			if descendant.ID == "" {
				continue
			}
			if _, ok := seen[descendant.ID]; ok {
				continue
			}
			seen[descendant.ID] = struct{}{}
			out = append(out, descendant)
			queue = append(queue, descendant.ID)
		}
	}
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

// Order-name resolution from a bead's labels/title lives in the orders edge as
// orders.NameFromOrderRunLabel (label-only, for destructive selection) and
// orders.NameFromTrackingBead (label with legacy order:<title> fallback).

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
	// served from the user-global cache), and a hint that names one of
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
