package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// reloadOrderDrainTimeout bounds how long config reload will wait for
// the outgoing order dispatcher's in-flight goroutines before replacing
// it. Reload runs on the tick loop, so a larger budget would stall all
// other subsystems. Dispatchers that do not drain within this budget are
// retained and drained again during controller shutdown; orphan tracking
// beads are still compensated by the next startup sweep if shutdown also
// cannot wait long enough.
const reloadOrderDrainTimeout = 1 * time.Second

// CityRuntime holds all running state for a single city's reconciliation
// loop. It encapsulates the per-city lifecycle that was previously spread
// across runController and controllerLoop. A machine-wide supervisor can
// instantiate multiple CityRuntimes — one per registered city.
type CityRuntime struct {
	cityPath     string
	cityName     string
	configName   string
	tomlPath     string
	watchTargets []config.WatchTarget
	configRev    string
	configDirty  *atomic.Bool
	watchMu      sync.Mutex
	watchCleanup func()

	serviceStateMu          sync.RWMutex
	cfg                     *config.City
	sp                      runtime.Provider
	publication             supervisor.PublicationConfig
	buildFn                 func(*config.City, runtime.Provider, beads.Store) DesiredStateResult
	buildFnWithSessionBeads func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult

	dops                    drainOps
	ct                      crashTracker
	it                      idleTracker
	wg                      wispGC
	od                      orderDispatcher
	retiredOrderDispatchers []orderDispatcher
	trace                   *sessionReconcilerTraceManager

	orderSweepWatchdogLast time.Time

	rec events.Recorder
	cs  *controllerState // nil when controller-managed bead stores are unavailable
	svc *workspacesvc.Manager

	poolSessions      map[string]time.Duration
	poolDeathHandlers map[string]poolDeathInfo
	suspendedNames    map[string]bool

	standaloneCityStore beads.Store // non-nil when API disabled; for chat auto-suspend
	standaloneRigStores map[string]beads.Store

	// Bead-driven reconciler state (Phase 2f).
	sessionDrains     *drainTracker // in-memory drain tracker; nil when bead reconciler disabled
	asyncStartLimiter *asyncStartLimiter
	asyncStarts       asyncStartTracker
	demandSnapshot    *runtimeDemandSnapshot

	convHandler         *convergence.Handler     // nil until bead store available
	convStoreAdapter    *convergenceStoreAdapter // typed reference; avoids type assertions in tick/reconcile
	convergenceReqCh    chan convergenceRequest  // receives CLI commands from controller.sock
	reloadReqCh         chan reloadRequest       // receives structured reload requests from controller.sock
	pokeCh              chan struct{}            // non-blocking signal to trigger immediate reconciler tick
	controlDispatcherCh chan struct{}            // non-blocking signal for control-dispatcher-only reconcile
	activeReload        *reloadRequest
	onStarted           func()
	onStatus            func(string)

	shutdownOnce             sync.Once
	preserveSessionsShutdown atomic.Bool
	logPrefix                string // "gc start" or "gc supervisor"
	stdout, stderr           io.Writer
}

const runtimeDemandSnapshotMaxAge = 30 * time.Second

type runtimeDemandSnapshot struct {
	createdAt          time.Time
	sessionFingerprint string
	result             DesiredStateResult
}

// CityRuntimeParams holds the caller-provided parameters for creating a
// CityRuntime. Internal components (crashTracker, etc.) are built by the
// constructor from these inputs.
type CityRuntimeParams struct {
	CityPath     string
	CityName     string
	TomlPath     string
	WatchTargets []config.WatchTarget
	ConfigRev    string
	ConfigDirty  *atomic.Bool

	Cfg                     *config.City
	SP                      runtime.Provider
	Publication             supervisor.PublicationConfig
	BuildFn                 func(*config.City, runtime.Provider, beads.Store) DesiredStateResult
	BuildFnWithSessionBeads func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult
	Dops                    drainOps

	Rec events.Recorder

	PoolSessions      map[string]time.Duration
	PoolDeathHandlers map[string]poolDeathInfo

	ConvergenceReqCh    chan convergenceRequest // may be nil
	ReloadReqCh         chan reloadRequest      // may be nil; receives structured reload commands
	PokeCh              chan struct{}           // may be nil; triggers immediate tick
	ControlDispatcherCh chan struct{}           // may be nil; triggers control-dispatcher-only reconcile
	OnStarted           func()                  // called after initial reconciliation succeeds
	OnStatus            func(string)            // called when init status changes

	LogPrefix      string // "gc start" or "gc supervisor"; defaults to "gc start"
	Stdout, Stderr io.Writer
}

var (
	cityRuntimeStartBeadsLifecycle       = startBeadsLifecycle
	cityRuntimeReloadLifecycleRetryDelay = time.Second
)

const cityRuntimeReloadLifecycleRetryLimit = 2

// newCityRuntime creates a CityRuntime, building internal components
// (crash tracker, idle tracker, wisp GC, order dispatcher) from the
// provided parameters.
func newCityRuntime(p CityRuntimeParams) *CityRuntime {
	configName := lockedConfigName(p.Cfg, p.CityPath)
	applyRuntimeCityIdentity(p.Cfg, p.CityName)

	var ct crashTracker
	if maxR := p.Cfg.Daemon.MaxRestartsOrDefault(); maxR > 0 {
		ct = newCrashTracker(maxR, p.Cfg.Daemon.RestartWindowDuration())
	}
	configDirty := p.ConfigDirty
	if configDirty == nil {
		configDirty = &atomic.Bool{}
	}

	it := buildIdleTracker(p.Cfg, p.CityName, p.CityPath, p.SP)

	var wg wispGC
	if p.Cfg.Daemon.WispGCEnabled() {
		wg = newWispGC(p.Cfg.Daemon.WispGCIntervalDuration(),
			p.Cfg.Daemon.WispTTLDuration())
	}

	// Sweep orphaned order-tracking beads on startup only (not config reload).
	// A previous controller instance may have left tracking beads open
	// (goroutines killed on restart, or silent Close failures).
	// Retry with backoff as defense-in-depth against transient store
	// errors immediately after ensureBeadsProvider returns (#753).
	if sweepStore, err := openStoreAtForCity(p.CityPath, p.CityPath); err != nil {
		fmt.Fprintf(p.Stderr, "gc start: order tracking sweep: %v\n", err) //nolint:errcheck // best-effort stderr
	} else if n, err := sweepOrphanedOrderTrackingRetry(sweepStore, 3, time.Second); err != nil {
		fmt.Fprintf(p.Stderr, "gc start: order tracking sweep (closed %d): %v\n", n, err) //nolint:errcheck // best-effort stderr
	} else if n > 0 {
		fmt.Fprintf(p.Stderr, "gc start: closed %d orphaned order-tracking beads\n", n) //nolint:errcheck // best-effort stderr
	}

	od := buildOrderDispatcher(p.CityPath, p.Cfg, p.Rec, p.Stderr)

	suspendedNames := computeSuspendedNames(p.Cfg, p.CityName, p.CityPath)

	logPrefix := p.LogPrefix
	if logPrefix == "" {
		logPrefix = "gc start"
	}

	cr := &CityRuntime{
		cityPath:                p.CityPath,
		cityName:                p.CityName,
		configName:              configName,
		tomlPath:                p.TomlPath,
		watchTargets:            p.WatchTargets,
		configRev:               p.ConfigRev,
		configDirty:             configDirty,
		cfg:                     p.Cfg,
		sp:                      p.SP,
		publication:             p.Publication,
		buildFn:                 p.BuildFn,
		buildFnWithSessionBeads: p.BuildFnWithSessionBeads,
		dops:                    p.Dops,
		ct:                      ct,
		it:                      it,
		wg:                      wg,
		od:                      od,
		trace:                   newSessionReconcilerTraceManager(p.CityPath, p.CityName, p.Stderr),
		rec:                     p.Rec,
		poolSessions:            p.PoolSessions,
		poolDeathHandlers:       p.PoolDeathHandlers,
		suspendedNames:          suspendedNames,
		asyncStartLimiter:       newAsyncStartLimiter(maxParallelStartsPerTick(p.Cfg)),
		convergenceReqCh:        p.ConvergenceReqCh,
		reloadReqCh: func() chan reloadRequest {
			if p.ReloadReqCh != nil {
				return p.ReloadReqCh
			}
			return make(chan reloadRequest)
		}(),
		pokeCh: func() chan struct{} {
			if p.PokeCh != nil {
				return p.PokeCh
			}
			return make(chan struct{}, 1)
		}(),
		controlDispatcherCh: func() chan struct{} {
			if p.ControlDispatcherCh != nil {
				return p.ControlDispatcherCh
			}
			return make(chan struct{}, 1)
		}(),
		onStarted: p.OnStarted,
		onStatus:  p.OnStatus,
		logPrefix: logPrefix,
		stdout:    p.Stdout,
		stderr:    p.Stderr,
	}
	cr.svc = workspacesvc.NewManager(&serviceRuntime{cr: cr})
	if err := cr.svc.Reload(); err != nil {
		fmt.Fprintf(cr.stderr, "%s: service init: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	return cr
}

// setControllerState sets the API state for this city. The controller
// state is managed by the caller (who also owns the API server), installed
// before run starts, and never replaced afterward.
func (cr *CityRuntime) setControllerState(cs *controllerState) {
	cr.cs = cs
}

// crashTracker returns the crash tracker for API server wiring.
func (cr *CityRuntime) crashTrack() crashTracker {
	return cr.ct
}

// run executes the reconciliation loop until ctx is canceled. This is
// the per-city main loop — it watches config, reconciles agents, runs
// wisp GC, and dispatches orders.
func (cr *CityRuntime) run(ctx context.Context) {
	defer cr.shutdown()

	dirty := cr.configDirty
	if dirty == nil {
		dirty = &atomic.Bool{}
		cr.configDirty = dirty
	}

	if cr.tomlPath != "" {
		cr.restartConfigWatcher()
		defer cr.stopConfigWatcher()
	}

	// Track effective provider name for hot-reload detection.
	lastProviderName := cr.cfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		lastProviderName = v
	}

	cityRoot := cr.cityPath
	if cityRoot == "" && cr.tomlPath != "" {
		cityRoot = filepath.Dir(cr.tomlPath)
	}

	// Enforce restrictive permissions on .gc/ and its subdirectories.
	enforceGCPermissions(cr.cityPath, cr.stderr)

	// Open standalone city bead store when controllerState is unavailable.
	// When controllerState is present, it manages the cached city store.
	if cr.cs == nil && cityRoot != "" {
		if store, err := openCityStoreAt(cityRoot); err != nil {
			fmt.Fprintf(cr.stderr, "%s: city bead store: %v (auto-suspend disabled)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		} else {
			cr.standaloneCityStore = store
		}
		cr.standaloneRigStores = buildStandaloneRigStores(cr.cfg, cr.cityPath, cr.stderr)
	}

	// Record bead store health metric.
	telemetry.RecordBeadStoreHealth(context.Background(), cr.cityName, cr.cityBeadStore() != nil)

	// Initialize bead-driven drain tracker when bead store is available.
	if cr.cityBeadStore() != nil && cr.tomlPath != "" {
		cr.sessionDrains = newDrainTracker()
	}
	if ctx.Err() != nil {
		return
	}

	retryDelay := cr.cfg.Daemon.PatrolIntervalDuration()
	startupRetryLimit := cr.cfg.Daemon.MaxRestartsOrDefault()
	waitForRetry := func() bool {
		timer := time.NewTimer(retryDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
	retryStartupStep := func(trigger string, complete func() bool, run func()) bool {
		for attempt := 1; !complete(); attempt++ {
			panicked := cr.safeTick(run, trigger)
			if ctx.Err() != nil {
				return false
			}
			if complete() {
				return true
			}
			if !panicked {
				fmt.Fprintf(cr.stderr, "%s: %s did not complete without panic; stopping city runtime\n", //nolint:errcheck
					cr.logPrefix, trigger)
				return false
			}
			if startupRetryLimit > 0 && attempt >= startupRetryLimit {
				fmt.Fprintf(cr.stderr, "%s: %s did not complete after %d attempt(s); stopping city runtime\n", //nolint:errcheck
					cr.logPrefix, trigger, attempt)
				return false
			}
			if !waitForRetry() {
				return false
			}
		}
		return true
	}

	// Adoption barrier: ensure every running session has a bead.
	// Runs on every startup (rerunnable, crash-safe).
	adoptionComplete := false
	if !retryStartupStep("adoption-barrier", func() bool { return adoptionComplete }, func() {
		if cr.onStatus != nil {
			cr.onStatus("adopting_sessions")
		}
		if cr.cityBeadStore() != nil {
			result, passed := runAdoptionBarrier(cr.cityBeadStore(), cr.sp, cr.cfg, cr.cityName, clock.Real{}, cr.stderr, false)
			if result.Adopted > 0 {
				fmt.Fprintf(cr.stdout, "Adopted %d running session(s) into bead store.\n", result.Adopted) //nolint:errcheck
			}
			if !passed {
				// Sessions that fail adoption AND have no matching agent are
				// invisible to the bead reconciler (which only processes beaded
				// sessions). They will be cleaned up when they naturally exit.
				// Sessions with matching agents get beads via syncSessionBeads
				// on the next tick.
				fmt.Fprintf(cr.stderr, "%s: adoption barrier: %d session(s) failed bead creation\n", cr.logPrefix, result.Skipped) //nolint:errcheck
			}
		}
		adoptionComplete = true
	}) {
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Initialize convergence handler (requires bead store).
	cr.initConvergenceHandler()
	if ctx.Err() != nil {
		return
	}

	cr.applyStartupConfigReload(ctx, dirty, &lastProviderName, cityRoot)
	if ctx.Err() != nil {
		return
	}

	// Dispatch due orders before startup session reconciliation. A cold-start
	// reconcile can take minutes when it has stale or config-drifted sessions;
	// due event/condition formulas should not wait behind that maintenance work.
	cr.safeTick(func() {
		cr.dispatchOrders(ctx, cityRoot)
	}, "startup-orders")
	if ctx.Err() != nil {
		return
	}

	// Session bead sync BEFORE reconciliation: ensures beads exist for
	// the reconciler to read/write hashes. Uses ListByLabel (indexed,
	// fast even before CachingStore is primed).
	//
	// Wrapped in safeTick so a panic during startup reconciliation (e.g.
	// a transient bead-store failure triggering a downstream nil deref)
	// does not propagate to the supervisor's panic recovery and cascade
	// into cityRuntime.shutdown(). See issue #663. The trace cycle is
	// ended inside the closure via defer so it's closed out on panic,
	// ctx cancellation, or normal completion alike.
	startupComplete := false
	if !retryStartupStep("startup", func() bool { return startupComplete }, func() {
		sessionBeads := cr.loadSessionBeadSnapshot()
		startupTrace := cr.beginTraceCycle("startup", "initial_reconcile", sessionBeads)
		completion := TraceCompletionAborted
		defer func() {
			if startupTrace != nil {
				startupTrace.end(completion, traceRecordPayload{"phase": "startup"})
			}
		}()

		cleanupDeadRuntimeSessionCorpses(sessionBeads, cr.sessionDrains, cr.sp, cr.stderr)
		// Reap stale session beads from a previous run before building desired
		// state, so desired state does not reference already-closed beads (#742).
		if reapStaleSessionBeads(cr.cityBeadStore(), cr.sp, cr.sessionDrains, clock.Real{}, cr.stderr) > 0 {
			sessionBeads = cr.loadSessionBeadSnapshot()
		}
		result := cr.buildDesiredState(sessionBeads, startupTrace)
		sessionBeads = cr.loadSessionBeadSnapshot()
		result = refreshDesiredStateWithSessionBeads(
			result,
			cr.cityName,
			cr.cityPath,
			cr.cfg,
			cr.sp,
			cr.cityBeadStore(),
			sessionBeads,
			cr.stderr,
		)
		sessionBeads = cr.syncBeadsAndUpdateIndex(result.State, sessionBeads)
		result = refreshDesiredStateWithSessionBeads(
			result,
			cr.cityName,
			cr.cityPath,
			cr.cfg,
			cr.sp,
			cr.cityBeadStore(),
			sessionBeads,
			cr.stderr,
		)
		if ctx.Err() != nil {
			return
		}

		if cr.sessionDrains != nil {
			cr.beadReconcileTick(ctx, result, sessionBeads, startupTrace)
		}
		completion = TraceCompletionCompleted
		startupComplete = true
	}) {
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Convergence startup reconciliation: recover in-progress convergence
	// beads that were interrupted by a controller crash. Runs after "City
	// started" so it doesn't block readiness. List() waits for the full
	// CachingStore prime, then serves from memory.
	//
	// Wrapped in safeTick so a panic during convergence recovery (same
	// class of transient store failure as #663) doesn't cascade to
	// cityRuntime.shutdown(). Startup does not advance until the active
	// convergence index is populated, so later patrols can drain pending
	// convergence beads.
	convergenceStartupDone := convergenceStartupComplete(cr)
	if !retryStartupStep("convergence-startup", func() bool { return convergenceStartupDone }, func() {
		cr.convergenceStartupReconcile(ctx)
		convergenceStartupDone = true
	}) {
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Mark city as started only after all retry-critical startup work has
	// completed. Publishing readiness before bead reconciliation or
	// convergence index population would let API callers observe a started
	// city whose one-shot startup state is still incomplete.
	if cr.onStatus != nil {
		cr.onStatus("starting_agents")
	}
	if cr.onStarted != nil {
		cr.onStarted()
	}
	fmt.Fprintln(cr.stdout, "City started.") //nolint:errcheck // best-effort stdout
	if ctx.Err() != nil {
		return
	}
	// Track pool instance liveness for death detection.
	var prevPoolRunning map[string]bool
	runTick := func(trigger string) {
		cr.safeTick(func() {
			cr.tick(ctx, dirty, &lastProviderName, cityRoot, &prevPoolRunning, trigger)
		}, trigger)
	}
	if dirty.Load() {
		runTick("startup-poke")
		if ctx.Err() != nil {
			return
		}
	}

	interval := cr.cfg.Daemon.PatrolIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTick("patrol")
		case <-cr.pokeCh:
			// Event-driven wake path: sling or API assigned work to a sleeping
			// session. Trigger an immediate tick so the reconciler sees the new
			// work via workSet/poolDesired and wakes the target promptly.
			runTick("poke")
		case <-cr.controlDispatcherCh:
			cr.safeTick(func() {
				cr.controlDispatcherTick(ctx)
			}, "control-dispatcher")
		case req := <-cr.reloadReqCh:
			cr.safeTick(func() {
				cr.handleReloadRequest(&req)
			}, "reload-request")
		case req := <-cr.convergenceReqCh:
			// Low-latency path: process convergence commands between ticks.
			// processConvergenceRequests() in tick() drains any that arrived
			// during tick processing. Both paths are safe — channel receives
			// are atomic, so each request is processed exactly once.
			// Note: ordering relative to convergenceTick is non-deterministic
			// via this path, but handlers are idempotent so interleaving is safe.
			cr.safeTick(func() {
				reply := cr.safeHandleConvergenceRequest(ctx, req)
				req.replyCh <- reply
			}, "convergence-request")
		case <-ctx.Done():
			cr.failActiveReload("Reload canceled because the controller is shutting down.")
			return
		}
	}
}

// safeTick runs fn with panic recovery. A panic inside fn is logged to
// stderr and swallowed so the reconciler loop can continue to the next
// tick. Without this wrapper, a panic in the reconciliation body
// propagates to cmd_supervisor.go's per-city goroutine recovery, which
// escalates to cityRuntime.shutdown() -> gracefulStopAll for every
// session in the city. Transient bead-store failures (e.g. Dolt EOF on
// a single metadata write) must not cascade into a full-city restart:
// the next tick is idempotent and will retry the failed work.
//
// This intentionally swallows ALL panics, including non-transient bugs
// (e.g. nil derefs from broken invariants). That tradeoff is explicit:
// a latent invariant bug that panics every tick will log visibly on
// each patrol interval, surfacing the bug via repetition, which is
// strictly better than the prior behavior of one panic killing every
// session in the city with no log of what triggered it. Operators
// should treat repeated "reconciler tick panicked" lines as a bug
// report, not a steady-state condition. The panic signal is intentionally
// stderr-only because recovery must not depend on event-bus availability
// during store or controller failures.
//
// Trigger identifies which tick site fired so operators can correlate
// the log with the cause.
func (cr *CityRuntime) safeTick(fn func(), trigger string) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			// Include the recovered type and a stack trace so a latent
			// invariant bug (e.g. nil deref) is diagnosable from the log
			// alone — crucial because safeTick intentionally swallows
			// the panic and the bug may only surface via repetition.
			fmt.Fprintf(cr.stderr, "%s: reconciler tick panicked (trigger=%s): %v (type=%T)\n%s\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, trigger, r, r, debug.Stack())
		}
	}()
	fn()
	return false
}

func convergenceStartupComplete(cr *CityRuntime) bool {
	return cr.convHandler == nil ||
		cr.convergenceReqCh == nil ||
		cr.convStoreAdapter == nil ||
		cr.convStoreAdapter.activeIndex != nil
}

// tick performs one reconciliation tick: pool death detection, config
// reload (if dirty), agent reconciliation, wisp GC, and order
// dispatch.
func (cr *CityRuntime) tick(
	ctx context.Context,
	dirty *atomic.Bool,
	lastProviderName *string,
	cityRoot string,
	prevPoolRunning *map[string]bool,
	trigger string,
) {
	sessionBeads := cr.loadSessionBeadSnapshot()
	traceTrigger := trigger
	traceDetail := "controller_tick"
	if cr.activeReload != nil {
		traceDetail = "manual_reload"
	}
	trace := cr.beginTraceCycle(traceTrigger, traceDetail, sessionBeads)
	// End the trace via defer so a panic recovered by safeTick still
	// closes the cycle (aborted). completion flips to Completed at the
	// normal end of the tick body below.
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "tick", "trigger": traceTrigger})
		}
	}()
	// Detect pool instance deaths since last tick.
	if len(cr.poolDeathHandlers) > 0 {
		currentRunning, listErr := cr.sp.ListRunning("")
		if listErr != nil {
			if runtime.IsPartialListError(listErr) {
				fmt.Fprintf(cr.stderr, "%s: pool death check skipped due to partial session listing: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(cr.stderr, "%s: pool death check skipped while listing sessions: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
			}
		} else {
			currentSet := make(map[string]bool, len(currentRunning))
			for _, name := range currentRunning {
				currentSet[name] = true
			}
			if *prevPoolRunning != nil {
				for sn, info := range cr.poolDeathHandlers {
					if (*prevPoolRunning)[sn] && !currentSet[sn] {
						if _, err := shellRunHook(info.Command, info.Dir, info.Env); err != nil {
							fmt.Fprintf(cr.stderr, "on_death %s: %v\n", sn, err) //nolint:errcheck // best-effort stderr
						}
					}
				}
			}
			*prevPoolRunning = make(map[string]bool)
			for sn := range cr.poolDeathHandlers {
				if currentSet[sn] {
					(*prevPoolRunning)[sn] = true
				}
			}
		}
	}

	var manualReload *reloadRequest
	var manualReply reloadControlReply
	manualReloadCompleted := false
	manualReloadReplied := false
	dirtyCleared := false
	tickCompleted := false
	defer func() {
		if dirtyCleared && !tickCompleted && manualReload == nil {
			dirty.Store(true)
		}
		if manualReload == nil || manualReloadReplied {
			return
		}
		reply := reloadControlReply{
			Outcome: reloadOutcomeFailed,
			Error:   fmt.Sprintf("Reload failed because reconciliation tick %q panicked before completion.", trigger),
		}
		if manualReloadCompleted {
			reply = manualReply
		}
		cr.sendReloadReply(manualReload.doneCh, reply)
		cr.activeReload = nil
	}()
	configChanged := dirty.Swap(false)
	if configChanged {
		dirtyCleared = true
		source := reloadSourceWatch
		if cr.activeReload != nil {
			source = reloadSourceManual
			manualReload = cr.activeReload
		}
		manualReply = cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, trace, source)
		if manualReload != nil {
			manualReloadCompleted = true
		}
	}

	// Order dispatch is intentionally before the expensive session reconcile
	// phases so due formulas are not starved by slow startup/config drift work.
	cr.dispatchOrders(ctx, cityRoot)

	// Session bead sync BEFORE reconciliation (one-tick state lag; see run()).
	// Post-reconcile sync was intentionally removed: the daemon's next tick
	// corrects bead state, and the pre-reconcile sync is sufficient for
	// the reconciler to read/write hashes during reconciliation.
	// Reap open session beads whose tmux session is dead before loading demand
	// so stale names cannot block desired-state computation (#742).
	cleanupDeadRuntimeSessionCorpses(sessionBeads, cr.sessionDrains, cr.sp, cr.stderr)
	if reapStaleSessionBeads(cr.cityBeadStore(), cr.sp, cr.sessionDrains, clock.Real{}, cr.stderr) > 0 {
		sessionBeads = cr.loadSessionBeadSnapshot()
	}
	demand := cr.loadDemandSnapshot(sessionBeads, trace, trigger, configChanged)
	result := demand.result
	sessionBeads = cr.loadSessionBeadSnapshot()
	result = refreshDesiredStateWithSessionBeads(
		result,
		cr.cityName,
		cr.cityPath,
		cr.cfg,
		cr.sp,
		cr.cityBeadStore(),
		sessionBeads,
		cr.stderr,
	)
	_ = cr.syncBeadsAndUpdateIndex(result.State, sessionBeads)
	// Reload snapshot after sync so the reconciler sees metadata written
	// by syncBeadsAndUpdateIndex (e.g., configured_named_session/mode
	// stamped on adopted beads). The CachingStore has the updated data
	// from SetMetadataBatch write-through.
	sessionBeads = cr.loadSessionBeadSnapshot()
	result = refreshDesiredStateWithSessionBeads(
		result,
		cr.cityName,
		cr.cityPath,
		cr.cfg,
		cr.sp,
		cr.cityBeadStore(),
		sessionBeads,
		cr.stderr,
	)

	// Bead-driven reconciliation (requires bead store / drain tracker).
	if cr.sessionDrains != nil {
		cr.beadReconcileTick(ctx, result, sessionBeads, trace)
	}

	// Wisp GC: purge expired closed molecules.
	if store := cr.cityBeadStore(); cr.wg != nil && store != nil && cr.wg.shouldRun(time.Now()) {
		purged, gcErr := cr.wg.runGC(store, time.Now())
		if gcErr != nil {
			for _, line := range strings.Split(gcErr.Error(), "\n") {
				if line == "" {
					continue
				}
				fmt.Fprintf(cr.stderr, "%s: wisp gc: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
			}
		}
		if purged > 0 {
			fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
		}
	}

	if cr.svc != nil {
		cr.svc.Tick(ctx, time.Now())
	}

	// Chat session auto-suspend: suspend detached idle sessions.
	if idleTimeout := cr.cfg.ChatSessions.IdleTimeoutDuration(); idleTimeout > 0 {
		autoSuspendChatSessions(cr.cityBeadStore(), cr.sp, idleTimeout, clock.Real{}, cr.stdout, cr.stderr)
	}

	// Drain queued convergence requests (CLI commands) BEFORE tick so
	// user commands (e.g. stop) take precedence over automated progression.
	cr.processConvergenceRequests(ctx)

	// Convergence tick: process active convergence loops.
	cr.convergenceTick(ctx)
	if manualReload != nil {
		cr.sendReloadReply(manualReload.doneCh, manualReply)
		manualReloadReplied = true
		cr.activeReload = nil
	}
	completion = TraceCompletionCompleted
	tickCompleted = true
}

func (cr *CityRuntime) dispatchOrders(ctx context.Context, cityRoot string) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	cr.runOrderTrackingSweepWatchdog(now)
	if cr.od != nil {
		cr.od.dispatch(ctx, cityRoot, now)
	}
}

func (cr *CityRuntime) runOrderTrackingSweepWatchdog(now time.Time) {
	if !cr.orderSweepWatchdogLast.IsZero() && now.Sub(cr.orderSweepWatchdogLast) < orderTrackingSweepWatchdogInterval {
		return
	}
	cr.orderSweepWatchdogLast = now

	store := cr.cityBeadStore()
	if store == nil {
		return
	}
	onlyOrders := map[string]struct{}{
		orderTrackingSweepOrder: {},
	}
	n, err := sweepStaleOrderTracking(store, now, orderTrackingSweepWatchdogStaleAfter, onlyOrders, orderTrackingWatchdogMetadataInitiator)
	if err != nil {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "%s: order tracking sweep watchdog: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
		return
	}
	if n > 0 && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: order tracking sweep watchdog closed %d stale tracking bead(s)\n", cr.logPrefix, n) //nolint:errcheck // best-effort stderr
	}
}

func (cr *CityRuntime) handleReloadRequest(req *reloadRequest) {
	if req == nil {
		return
	}
	if cr.activeReload != nil {
		req.acceptedCh <- reloadControlReply{
			Outcome: reloadOutcomeBusy,
			Message: "Reload request could not be accepted because another reload is already in progress.",
		}
		return
	}
	cr.activeReload = req
	if cr.configDirty == nil {
		cr.configDirty = &atomic.Bool{}
	}
	cr.configDirty.Store(true)
	select {
	case cr.pokeCh <- struct{}{}:
	default:
	}
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}
}

func (cr *CityRuntime) failActiveReload(message string) {
	if cr.activeReload == nil {
		return
	}
	cr.sendReloadReply(cr.activeReload.doneCh, reloadControlReply{
		Outcome: reloadOutcomeFailed,
		Error:   message,
	})
	cr.activeReload = nil
}

func (cr *CityRuntime) sendReloadReply(ch chan<- reloadControlReply, reply reloadControlReply) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(cr.stderr, "%s: reload reply panicked: %v (type=%T)\n%s\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, r, r, debug.Stack())
		}
	}()
	select {
	case ch <- reply:
	default:
	}
}

// reloadConfig attempts to reload city.toml and update all internal
// components. On error, the old config is kept.
func (cr *CityRuntime) reloadConfig(
	ctx context.Context,
	lastProviderName *string,
	cityRoot string,
) {
	cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, nil, reloadSourceWatch)
}

func (cr *CityRuntime) applyStartupConfigReload(
	ctx context.Context,
	dirty *atomic.Bool,
	lastProviderName *string,
	cityRoot string,
) {
	if cr.tomlPath == "" || cityRoot == "" || cr.configRev == "" || lastProviderName == nil || ctx.Err() != nil {
		return
	}
	if dirty != nil {
		dirty.Swap(false)
	}
	reply := cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, nil, reloadSourceWatch)
	if reply.Outcome == reloadOutcomeFailed && dirty != nil {
		dirty.Store(true)
	}
}

func (cr *CityRuntime) reloadConfigTraced(
	ctx context.Context,
	lastProviderName *string,
	cityRoot string,
	trace *sessionReconcilerTraceCycle,
	source reloadSource,
) reloadControlReply {
	var warnings []string
	appendWarning := func(message string) {
		warnings = append(warnings, message)
		fmt.Fprintf(cr.stderr, "%s: warning: %s\n", cr.logPrefix, message) //nolint:errcheck // best-effort stderr
	}

	configName := cr.configName
	if configName == "" {
		configName = cr.cityName
	}
	result, err := tryReloadConfig(cr.tomlPath, configName, cityRoot)
	if err != nil {
		if result != nil {
			for _, warning := range result.Warnings {
				appendWarning(warning)
			}
		} else {
			for _, warning := range reloadWarningsFromError(err) {
				appendWarning(warning)
			}
		}
		fmt.Fprintf(cr.stderr, "%s: config reload: %v (keeping old config)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, "", string(source), string(reloadOutcomeFailed), len(warnings), err)
		if trace != nil {
			trace.RecordConfigReload("", "", TraceOutcomeFailed, source, nil, nil, false, warnings, err)
		}
		return reloadControlReply{
			Outcome:  reloadOutcomeFailed,
			Error:    err.Error(),
			Warnings: warnings,
		}
	}
	for _, warning := range result.Warnings {
		appendWarning(warning)
	}
	if cr.configRev != "" && result.Revision == cr.configRev {
		if trace != nil {
			trace.RecordConfigReload(cr.configRev, result.Revision, TraceOutcomeNoChange, source, nil, nil, false, warnings, nil)
		}
		telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeNoChange), len(warnings), nil)
		return reloadControlReply{
			Outcome:  reloadOutcomeNoChange,
			Message:  "No config changes detected.",
			Revision: result.Revision,
			Warnings: warnings,
		}
	}

	oldAgentCount := len(cr.cfg.Agents)
	oldRigCount := len(cr.cfg.Rigs)
	oldRevision := cr.configRev
	nextCfg := result.Cfg
	applyRuntimeCityIdentity(nextCfg, cr.cityName)
	nextSp := cr.sp
	nextDops := cr.dops
	providerChanged := false

	// Detect session provider change.
	newProviderName := nextCfg.Session.Provider
	pendingProviderName := *lastProviderName
	if v := os.Getenv("GC_SESSION"); v != "" {
		newProviderName = v
	}
	if newProviderName != *lastProviderName {
		newSp, spErr := newSessionProviderByName(newProviderName, nextCfg.Session, cr.cityName, cr.cityPath)
		if spErr != nil {
			appendWarning(fmt.Sprintf("new session provider %q: %v (keeping old provider)", newProviderName, spErr))
		} else {
			providerChanged = true
			nextSp = newSp
			nextDops = newDrainOps(nextSp)
			pendingProviderName = newProviderName
		}
	}

	if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
		appendWarning(fmt.Sprintf("config reload: %v", err))
	}
	resolveRigPaths(cityRoot, nextCfg.Rigs)
	var lifecycleErr error
	for attempt := 1; attempt <= cityRuntimeReloadLifecycleRetryLimit; attempt++ {
		lifecycleErr = cityRuntimeStartBeadsLifecycle(cityRoot, cr.cityName, nextCfg, cr.stderr)
		if lifecycleErr == nil {
			break
		}
		if attempt == cityRuntimeReloadLifecycleRetryLimit || !isRetryableManagedDoltLifecycleError(lifecycleErr) {
			break
		}
		appendWarning(fmt.Sprintf("config reload: transient bead lifecycle failure: %v; retrying", lifecycleErr))
		if cityRuntimeReloadLifecycleRetryDelay > 0 {
			time.Sleep(cityRuntimeReloadLifecycleRetryDelay)
		}
	}
	if lifecycleErr != nil {
		err := fmt.Errorf("config reload: %w", lifecycleErr)
		fmt.Fprintf(cr.stderr, "%s: %v (keeping old config)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, "", string(source), string(reloadOutcomeFailed), len(warnings), err)
		if trace != nil {
			trace.RecordConfigReload(oldRevision, result.Revision, TraceOutcomeFailed, source, nil, nil, false, warnings, err)
		}
		return reloadControlReply{
			Outcome:  reloadOutcomeFailed,
			Error:    err.Error(),
			Warnings: warnings,
		}
	}
	if len(nextCfg.FormulaLayers.City) > 0 {
		if err := ResolveFormulas(cityRoot, nextCfg.FormulaLayers.City); err != nil {
			appendWarning(fmt.Sprintf("config reload: city formulas: %v", err))
		}
	}
	for _, r := range nextCfg.Rigs {
		layers, ok := nextCfg.FormulaLayers.Rigs[r.Name]
		if !ok || len(layers) == 0 {
			layers = nextCfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			if err := ResolveFormulas(r.Path, layers); err != nil {
				appendWarning(fmt.Sprintf("config reload: rig %q formulas: %v", r.Name, err))
			}
		}
	}

	// Prune legacy top-level scripts/ symlinks from older runtime shims.
	pruneLegacyConfiguredScripts(cityRoot, nextCfg, func(scope string, err error) {
		appendWarning(fmt.Sprintf("config reload: pruning legacy %s scripts: %v", scope, err))
	})

	if providerChanged {
		running, lErr := cr.sp.ListRunning("")
		if lErr != nil {
			err := fmt.Errorf("config reload: listing sessions failed during provider swap: %w", lErr)
			if runtime.IsPartialListError(lErr) {
				err = fmt.Errorf("config reload: listing sessions partially failed during provider swap: %w", lErr)
			}
			fmt.Fprintf(cr.stderr, "%s: %v (keeping old config)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
			telemetry.RecordConfigReload(ctx, "", string(source), string(reloadOutcomeFailed), len(warnings), err)
			if trace != nil {
				trace.RecordConfigReload(oldRevision, result.Revision, TraceOutcomeFailed, source, nil, nil, false, warnings, err)
			}
			return reloadControlReply{
				Outcome:  reloadOutcomeFailed,
				Error:    err.Error(),
				Warnings: warnings,
			}
		}
		if len(running) > 0 {
			fmt.Fprintf(cr.stdout, "Provider changed (%s → %s), stopping %d agent(s)...\n", //nolint:errcheck
				displayProviderName(*lastProviderName), displayProviderName(pendingProviderName), len(running))
			gracefulStopAll(running, cr.sp, nextCfg.Daemon.ShutdownTimeoutDuration(), cr.rec, cr.cfg, cr.cityBeadStore(), cr.stdout, cr.stderr)
		}
		cr.rec.Record(events.Event{
			Type:    events.ProviderSwapped,
			Actor:   "gc",
			Message: fmt.Sprintf("%s → %s", displayProviderName(*lastProviderName), displayProviderName(pendingProviderName)),
		})
		fmt.Fprintf(cr.stdout, "Session provider swapped to %s.\n", displayProviderName(pendingProviderName)) //nolint:errcheck
		*lastProviderName = pendingProviderName
	}

	cr.poolSessions = computePoolSessions(nextCfg, cr.cityName, cr.cityPath, nextSp)
	cr.poolDeathHandlers = computePoolDeathHandlers(nextCfg, cr.cityName, cityRoot, nextSp, cr.stderr)
	cr.suspendedNames = computeSuspendedNames(nextCfg, cr.cityName, cr.cityPath)

	// Rebuild crash tracker if config values changed, otherwise clear all
	// crash history so that a fixed config automatically unquarantines agents.
	newMaxR := nextCfg.Daemon.MaxRestartsOrDefault()
	newWindow := nextCfg.Daemon.RestartWindowDuration()
	switch {
	case newMaxR <= 0:
		cr.ct = nil
	case cr.ct == nil:
		cr.ct = newCrashTracker(newMaxR, newWindow)
	default:
		oldMaxR, oldWindow := cr.ct.limits()
		if newMaxR != oldMaxR || newWindow != oldWindow {
			cr.ct = newCrashTracker(newMaxR, newWindow)
		} else {
			cr.ct.clearAll()
		}
	}
	if cr.cs != nil {
		cr.cs.mu.Lock()
		cr.cs.ct = cr.ct
		cr.cs.mu.Unlock()
	}

	cr.it = buildIdleTracker(nextCfg, cr.cityName, cr.cityPath, nextSp)

	if nextCfg.Daemon.WispGCEnabled() {
		cr.wg = newWispGC(nextCfg.Daemon.WispGCIntervalDuration(),
			nextCfg.Daemon.WispTTLDuration())
	} else {
		cr.wg = nil
	}

	// Drain the outgoing dispatcher before replacing it so in-flight
	// dispatchOne goroutines persist their tracking-bead outcomes against
	// the store they were scheduled against. Reload runs on the same
	// goroutine as tick, so no concurrent dispatch can create a new
	// in-flight signal on this dispatcher while drain observes it. The
	// reload budget is capped at reloadOrderDrainTimeout so a wedged exec
	// order cannot stall the tick loop; timed-out dispatchers are retained
	// and drained again during shutdown.
	// Deriving from ctx (the tick ctx) lets a shutdown racing with reload
	// short-circuit the drain instead of waiting the full 1s.
	if cr.od != nil {
		drainCtx, drainCancel := context.WithTimeout(ctx, reloadOrderDrainTimeout)
		cr.drainOutgoingOrderDispatcher(drainCtx, cr.od)
		drainCancel()
	}
	cr.od = buildOrderDispatcher(cityRoot, nextCfg, cr.rec, cr.stderr)

	cr.serviceStateMu.Lock()
	cr.cfg = nextCfg
	cr.sp = nextSp
	cr.dops = nextDops
	cr.serviceStateMu.Unlock()

	if cr.cs != nil {
		cr.cs.updateFromRuntime(nextCfg, nextSp, result.Revision)
	}
	if cr.svc != nil {
		if err := cr.svc.Reload(); err != nil {
			appendWarning(fmt.Sprintf("service reload: %v", err))
		}
	}

	if cr.cs == nil {
		// Refresh standalone city store for auto-suspend.
		// Also recovers from nil → non-nil when bd becomes available after startup.
		if s, err := openCityStoreAt(cityRoot); err != nil {
			if cr.standaloneCityStore != nil {
				appendWarning(fmt.Sprintf("city bead store reload: %v", err))
			}
		} else {
			cr.standaloneCityStore = s
		}
		cr.standaloneRigStores = buildStandaloneRigStores(nextCfg, cr.cityPath, cr.stderr)
	}

	// Ensure drain tracker is initialized when bead store becomes available.
	if cr.cityBeadStore() != nil && cr.tomlPath != "" && cr.sessionDrains == nil {
		cr.sessionDrains = newDrainTracker()
	}
	cr.configRev = result.Revision
	cr.watchTargets = config.WatchTargets(result.Prov, nextCfg, cityRoot)
	cr.restartConfigWatcher()
	if trace != nil {
		trace.configRevision = result.Revision
		trace.syncArms(time.Now().UTC(), nextCfg)
	}

	message := fmt.Sprintf("Config reloaded: %s (rev %s)",
		configReloadSummary(oldAgentCount, oldRigCount, len(nextCfg.Agents), len(nextCfg.Rigs)),
		shortRev(result.Revision))
	fmt.Fprintln(cr.stdout, message) //nolint:errcheck // best-effort stdout
	telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeApplied), len(warnings), nil)
	if trace != nil {
		trace.RecordConfigReload(oldRevision, result.Revision, TraceOutcomeApplied, source, nil, nil, providerChanged, warnings, nil)
	}
	return reloadControlReply{
		Outcome:  reloadOutcomeApplied,
		Message:  message,
		Revision: result.Revision,
		Warnings: warnings,
	}
}

func lockedConfigName(cfg *config.City, cityPath string) string {
	return loadedCityName(cfg, cityPath)
}

func (cr *CityRuntime) configWatcherTargets() []config.WatchTarget {
	watchTargets := append([]config.WatchTarget{}, cr.watchTargets...)
	if len(watchTargets) == 0 && cr.tomlPath != "" {
		watchTargets = []config.WatchTarget{{
			Path:                filepath.Dir(cr.tomlPath),
			DiscoverConventions: true,
		}}
	}

	var hasTomlPath bool
	for _, target := range watchTargets {
		if samePath(target.Path, cr.tomlPath) {
			hasTomlPath = true
			break
		}
	}
	if cr.tomlPath != "" && !hasTomlPath {
		watchTargets = append(watchTargets, config.WatchTarget{Path: cr.tomlPath})
	}
	return watchTargets
}

func (cr *CityRuntime) restartConfigWatcher() {
	if cr.tomlPath == "" {
		return
	}
	cr.stopConfigWatcher()

	dirty := cr.configDirty
	if dirty == nil {
		dirty = &atomic.Bool{}
		cr.configDirty = dirty
	}
	cleanup := watchConfigTargets(cr.configWatcherTargets(), dirty, cr.pokeCh, cr.stderr)

	cr.watchMu.Lock()
	cr.watchCleanup = cleanup
	cr.watchMu.Unlock()
}

func (cr *CityRuntime) stopConfigWatcher() {
	cr.watchMu.Lock()
	cleanup := cr.watchCleanup
	cr.watchCleanup = nil
	cr.watchMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

// beadReconcileTick runs one reconciliation tick using the bead-driven
// reconciler. It loads session beads from the store, uses the provided
// desired state, and delegates to reconcileSessionBeads.
func (cr *CityRuntime) beadReconcileTick(ctx context.Context, result DesiredStateResult, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) {
	desiredState := result.State
	store := cr.cityBeadStore()
	if store == nil {
		return
	}

	if sessionBeads == nil {
		var sessionQueryPartial bool
		sessionBeads, sessionQueryPartial = cr.loadSessionBeadSnapshotWithPartial()
		result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	}
	rigStores := cr.rigBeadStores()
	assignedWorkBeads := result.AssignedWorkBeads
	assignedWorkStoreRefs := result.AssignedWorkStoreRefs
	released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, cr.cfg, cr.cityPath, sessionBeads.Open(), result, rigStores)
	if len(released) > 0 {
		for _, r := range released {
			fmt.Fprintf(cr.stderr, "released orphaned pool work: %s\n", r.ID) //nolint:errcheck
		}
		assignedWorkBeads, assignedWorkStoreRefs = filterReleasedAssignedWorkSnapshot(assignedWorkBeads, assignedWorkStoreRefs, released)
	}
	// poolDesired determines how many sessions should be AWAKE. Uses the
	// same scale_check counts that buildDesiredState already computed (no
	// duplicate shell-outs). Resume tier from cross-referenced assigned
	// work beads + new tier from scale_check + min fill.
	poolDesired := result.PoolDesiredCounts
	if poolDesired == nil {
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cr.cfg, cr.cityPath, sessionBeads.Open(), assignedWorkBeads, assignedWorkStoreRefs)
		poolDesired = PoolDesiredCounts(ComputePoolDesiredStatesTraced(
			cr.cfg, poolWorkBeads, sessionBeads.Open(), result.ScaleCheckCounts, trace))
	}
	// Merge named-session assignee demand so on-demand named sessions with
	// direct work (Assignee match, no gc.routed_to) stay config-eligible.
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, result.NamedSessionDemand, cr.cfg)
	for tmpl, count := range poolDesired {
		if count > 0 {
			fmt.Fprintf(cr.stderr, "poolDesired: %s = %d\n", tmpl, count) //nolint:errcheck
		}
	}
	for tmpl, count := range result.ScaleCheckCounts {
		if count > 0 {
			fmt.Fprintf(cr.stderr, "scaleCheck: %s = %d\n", tmpl, count) //nolint:errcheck
		}
	}
	if sweepUndesiredPoolSessionBeads(
		store,
		rigStores,
		sessionBeads,
		desiredState,
		cr.cfg,
		cr.sp,
		result.snapshotQueryPartial(),
	) > 0 {
		var sessionQueryPartial bool
		sessionBeads, sessionQueryPartial = cr.loadSessionBeadSnapshotWithPartial()
		result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	}
	open := sessionBeads.Open()

	// Use cr.cityName consistently — it's the authoritative runtime name.
	cityName := cr.cityName

	cfgNames := configuredSessionNamesWithSnapshot(cr.cfg, cityName, sessionBeads)

	readyWaitSet, err := prepareWaitWakeStateForCityWithSnapshot(cr.cityPath, store, time.Now(), sessionBeads)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: preparing waits: %v\n", cr.logPrefix, err) //nolint:errcheck
		readyWaitSet = nil
	}

	// workSet: defense-in-depth wake signal from work_query. When work_query
	// detects pending work but scale_check hasn't caught up yet, workSet
	// ensures at least one session wakes without waiting for the next tick.
	workSet := result.WorkSet
	if workSet == nil {
		workSet = computeWorkSet(cr.cfg, shellScaleCheck, cityName, cr.cityPath, store, sessionBeads, cr.stderr)
	}
	if trace != nil {
		templateNames := make(map[string]struct{})
		openCounts := make(map[string]int)
		desiredCounts := make(map[string]int)
		for _, bead := range open {
			template := normalizedSessionTemplate(bead, cr.cfg)
			if template == "" {
				continue
			}
			templateNames[template] = struct{}{}
			openCounts[template]++
			trace.RecordSessionBaseline(template, bead.Metadata["session_name"], map[string]any{
				"state":        bead.Metadata["state"],
				"sleep_reason": bead.Metadata["sleep_reason"],
			})
		}
		for _, tp := range desiredState {
			if tp.TemplateName == "" {
				continue
			}
			templateNames[tp.TemplateName] = struct{}{}
			desiredCounts[tp.TemplateName]++
		}
		for template := range poolDesired {
			templateNames[template] = struct{}{}
		}
		for template := range workSet {
			templateNames[template] = struct{}{}
		}
		for _, template := range traceSetStrings(templateNames) {
			status := TraceEvaluationEligible
			reason := TraceReasonRetained
			if desiredCounts[template] == 0 && poolDesired[template] == 0 && openCounts[template] == 0 {
				status = TraceEvaluationSkipped
				reason = TraceReasonNoDemand
			}
			trace.RecordTemplateSummary(template, "", status, reason, map[string]any{
				"desired_count":  desiredCounts[template],
				"open_count":     openCounts[template],
				"pool_desired":   poolDesired[template],
				"work_requested": workSet[template],
			})
		}
		trace.RecordCycleInputSnapshot(map[string]any{
			"desired_session_count":  len(desiredState),
			"open_session_count":     len(open),
			"scale_check_counts":     result.ScaleCheckCounts,
			"pool_desired":           poolDesired,
			"ready_wait_count":       len(readyWaitSet),
			"work_set_count":         len(workSet),
			"store_query_partial":    result.StoreQueryPartial,
			"session_query_partial":  result.SessionQueryPartial,
			"snapshot_query_partial": result.snapshotQueryPartial(),
		})
		for _, agent := range cr.cfg.Agents {
			template := agent.QualifiedName()
			if !trace.detailEnabled(template) {
				continue
			}
			trace.RecordTemplateConfigSnapshot(template, map[string]any{
				"provider":            firstNonEmpty(agent.Provider, cr.cfg.Session.Provider),
				"session":             agent.Session,
				"work_dir":            agent.WorkDir,
				"dir":                 agent.Dir,
				"suspended":           agent.Suspended,
				"start_command":       agent.StartCommand,
				"args":                append([]string(nil), agent.Args...),
				"prompt_mode":         agent.PromptMode,
				"prompt_flag":         agent.PromptFlag,
				"depends_on":          append([]string(nil), agent.DependsOn...),
				"min_active_sessions": agent.MinActiveSessions,
				"max_active_sessions": agent.MaxActiveSessions,
				"scale_check":         agent.ScaleCheck,
				"work_query":          agent.WorkQuery,
				"sling_query":         agent.SlingQuery,
			})
		}
	}

	awakeAssignedWorkBeads := filterAssignedWorkBeadsForSessionWake(cr.cfg, cr.cityPath, open, assignedWorkBeads, assignedWorkStoreRefs)
	reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cr.cityPath, open, desiredState, cfgNames, cr.cfg, cr.sp, store,
		cr.dops,
		awakeAssignedWorkBeads, rigStores, readyWaitSet, cr.sessionDrains, poolDesired,
		result.NamedSessionDemand,
		result.snapshotQueryPartial(),
		workSet, cityName,
		cr.it, clock.Real{}, cr.rec, cr.cfg.Session.StartupTimeoutDuration(),
		cr.cfg.Daemon.DriftDrainTimeoutDuration(),
		cr.stdout, cr.stderr, trace,
		withAsyncStartExecution(),
		withAsyncStartFollowUp(cr.requestAsyncStartFollowUpTick),
		withAsyncStartLimiter(cr.ensureAsyncStartLimiter()),
		withAsyncStartTracker(&cr.asyncStarts),
	)
	cr.requestDeferredDrainFollowUpTick()
	if trace != nil {
		for _, bead := range open {
			template := normalizedSessionTemplate(bead, cr.cfg)
			if template == "" {
				continue
			}
			trace.RecordSessionResult(template, bead.Metadata["session_name"], TraceOutcomeComplete, TraceCompletenessComplete, map[string]any{
				"state":        bead.Metadata["state"],
				"sleep_reason": bead.Metadata["sleep_reason"],
			})
		}
	}
	dispatchSessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: dispatching wait nudges: %v\n", cr.logPrefix, err) //nolint:errcheck
	} else if err := dispatchReadyWaitNudgesWithSnapshot(cr.cityPath, store, time.Now(), dispatchSessionBeads); err != nil {
		fmt.Fprintf(cr.stderr, "%s: dispatching wait nudges: %v\n", cr.logPrefix, err) //nolint:errcheck
	}

	// Idle recovery: detect pool sessions stuck at the prompt after
}

func filterReleasedAssignedWorkBeads(assignedWorkBeads []beads.Bead, released []releasedPoolAssignment) []beads.Bead {
	filtered, _ := filterReleasedAssignedWorkSnapshot(assignedWorkBeads, nil, released)
	return filtered
}

func filterReleasedAssignedWorkSnapshot(assignedWorkBeads []beads.Bead, assignedWorkStoreRefs []string, released []releasedPoolAssignment) ([]beads.Bead, []string) {
	if len(assignedWorkBeads) == 0 || len(released) == 0 {
		return assignedWorkBeads, assignedWorkStoreRefs
	}
	releasedIndexes := make(map[int]struct{}, len(released))
	for _, r := range released {
		if r.Index >= 0 && r.Index < len(assignedWorkBeads) {
			if assignedWorkBeads[r.Index].ID != r.ID {
				log.Printf("filterReleasedAssignedWorkBeads: released index %d points at bead %q, want %q", r.Index, assignedWorkBeads[r.Index].ID, r.ID)
				continue
			}
			releasedIndexes[r.Index] = struct{}{}
		}
	}
	if len(releasedIndexes) == 0 {
		return assignedWorkBeads, assignedWorkStoreRefs
	}
	filtered := make([]beads.Bead, 0, len(assignedWorkBeads)-len(releasedIndexes))
	var filteredStoreRefs []string
	// Preserve AssignedWorkBeads/AssignedWorkStoreRefs index alignment when
	// both slices are complete; otherwise drop refs rather than guess.
	if len(assignedWorkStoreRefs) == len(assignedWorkBeads) {
		filteredStoreRefs = make([]string, 0, len(assignedWorkStoreRefs)-len(releasedIndexes))
	}
	for i, wb := range assignedWorkBeads {
		if _, ok := releasedIndexes[i]; ok {
			continue
		}
		filtered = append(filtered, wb)
		if filteredStoreRefs != nil {
			filteredStoreRefs = append(filteredStoreRefs, assignedWorkStoreRefs[i])
		}
	}
	if filteredStoreRefs == nil {
		filteredStoreRefs = assignedWorkStoreRefs
	}
	return filtered, filteredStoreRefs
}

func (cr *CityRuntime) requestDeferredDrainFollowUpTick() {
	if cr == nil || cr.sessionDrains == nil {
		return
	}
	if !cr.sessionDrains.consumeFollowUpTick() {
		return
	}
	select {
	case cr.pokeCh <- struct{}{}:
	default:
	}
}

func (cr *CityRuntime) ensureAsyncStartLimiter() *asyncStartLimiter {
	capacity := maxParallelStartsPerTick(cr.cfg)
	if cr.asyncStartLimiter == nil {
		cr.asyncStartLimiter = newAsyncStartLimiter(capacity)
	} else {
		cr.asyncStartLimiter.resize(capacity)
	}
	return cr.asyncStartLimiter
}

func (cr *CityRuntime) requestAsyncStartFollowUpTick() {
	if cr == nil {
		return
	}
	// Async completion can commit, rollback, or reject stale work; each case
	// should prompt one cheap reconciliation pass to observe the new reality.
	select {
	case cr.pokeCh <- struct{}{}:
	default:
	}
}

func (cr *CityRuntime) waitForAsyncStarts() {
	if cr == nil {
		return
	}
	timeout := time.Duration(0)
	if cr.cfg != nil {
		timeout = cr.cfg.Daemon.ShutdownTimeoutDuration()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if !cr.asyncStarts.wait(timeout) && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: async session starts still running after %s; continuing shutdown\n", cr.logPrefix, timeout) //nolint:errcheck // best-effort stderr
	}
}

func sweepUndesiredPoolSessionBeads(
	store beads.Store,
	rigStores map[string]beads.Store,
	sessionBeads *sessionBeadSnapshot,
	desiredState map[string]TemplateParams,
	cfg *config.City,
	sp runtime.Provider,
	storeQueryPartial bool,
) int {
	if store == nil || sessionBeads == nil || cfg == nil || storeQueryPartial {
		return 0
	}
	var candidates []beads.Bead
	for _, bead := range sessionBeads.Open() {
		if bead.Status == "closed" {
			continue
		}
		if _, desired := desiredState[bead.Metadata["session_name"]]; desired {
			continue
		}
		if isManualSessionBead(bead) || isNamedSessionBead(bead) {
			continue
		}
		if running, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, bead.ID); err == nil && running {
			continue
		}
		// Don't sweep beads that the reconciler still considers "start
		// requested" — their work assignment window hasn't opened. Mirrors
		// sessionStartRequested (session_reconcile.go) exactly so the two
		// loops agree about ownership:
		//   - pending_create_claim=true: in-flight create claim, protected
		//     regardless of age until the lifecycle clears it.
		//   - state=creating: protected until staleCreatingState would
		//     return true (i.e., until staleCreatingStateTimeout has
		//     elapsed; zero CreatedAt is treated as stale, matching
		//     staleCreatingState in session_reconcile.go).
		// Without this, a pool's freshly-created session bead gets swept
		// on the same tick it's created (no work assigned →
		// GCSweepSessionBeads closes it), spinning the pool in a rapid
		// create→sweep→recreate loop.
		if strings.TrimSpace(bead.Metadata["pending_create_claim"]) == "true" {
			continue
		}
		if strings.TrimSpace(bead.Metadata["state"]) == "creating" && !isStaleCreating(bead) {
			continue
		}
		// Age grace period for the post-creating, pre-wake window. After
		// session_lifecycle_parallel flips state from "creating" to
		// "active" + state_reason=creation_complete, there's still a gap
		// before the wake pipeline records last_woke_at. Sweeping that
		// window produces the same spin as sweeping during creation —
		// we observed pool sessions with state=active, last_woke=empty
		// getting closed before wake ever landed.
		//
		// The guard matches both "active" and "awake" because the
		// reconciler's healStatePatch (session_reconcile.go) rewrites a
		// live bead from "active" to "awake" whenever the runtime is
		// alive, and the reconciler treats both values as equivalent
		// live states. Limiting the guard to "active" alone would leave
		// the same spin-loop open on the "awake" alias path.
		//
		// The guard must only match the post-create window, not crash/
		// churn/start-failure paths that ALSO clear last_woke_at
		// (checkStability, checkChurn, and the start-failure branch in
		// session_lifecycle_parallel.go all clear last_woke_at on beads
		// that may already be state=active). We distinguish by the
		// per-start marker creation_complete_at, written atomically with
		// the state transition by CommitStartedPatch / ConfirmStartedPatch
		// and restamped by recoverRunningPendingCreate on heal. A bead
		// is protected while creation_complete_at is recent (within
		// staleCreatingStateTimeout) AND last_woke_at is still empty —
		// crash/churn paths do not touch creation_complete_at, so a
		// post-crash bead whose last successful start was longer than
		// the timeout ago is sweepable even when wake_attempts or
		// churn_count are non-zero. The age bound mirrors
		// staleCreatingState: a missing or zero creation_complete_at is
		// treated as stale (sweepable) so beads without the per-start
		// marker (older builds, manually repaired) stay recoverable.
		//
		// Upgrade contract: older binaries did not write
		// creation_complete_at, so any bead persisted before upgrade
		// fails the age check and becomes sweepable. That matches the
		// semantics a crashed bead would get under the current binary
		// and is the intended behavior — a bead that survived a binary
		// restart without completing its wake is not in the protected
		// "mid-start" window. The atomicity requirement therefore only
		// binds within a single binary (writers and sweep are the same
		// process); the rollout needs no cross-version coordination.
		if state := strings.TrimSpace(bead.Metadata["state"]); (state == "active" || state == "awake") &&
			strings.TrimSpace(bead.Metadata["last_woke_at"]) == "" &&
			strings.TrimSpace(bead.Metadata["state_reason"]) == "creation_complete" {
			if creationCompleteAt, ok := parseRFC3339Metadata(bead.Metadata["creation_complete_at"]); ok &&
				time.Since(creationCompleteAt) < staleCreatingStateTimeout {
				continue
			}
		}
		template := normalizedSessionTemplate(bead, cfg)
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !isEphemeralSessionBead(bead) {
			continue
		}
		candidates = append(candidates, bead)
	}
	return len(GCSweepSessionBeads(store, rigStores, candidates))
}

// isStaleCreating mirrors staleCreatingState in session_reconcile.go without
// requiring a clock.Clock dependency. It prefers the per-attempt
// pending_create_started_at marker and falls back to CreatedAt for older beads
// so the sweep and reconciler agree about which in-flight create beads are
// still alive.
func isStaleCreating(bead beads.Bead) bool {
	now := time.Now()
	if started, ok := parseRFC3339Metadata(bead.Metadata["pending_create_started_at"]); ok {
		return !now.Before(started.Add(staleCreatingStateTimeout))
	}
	if bead.CreatedAt.IsZero() {
		return true
	}
	return !now.Before(bead.CreatedAt.Add(staleCreatingStateTimeout))
}

// parseRFC3339Metadata parses an RFC3339 timestamp metadata value. A missing,
// zero, or unparseable value returns ok=false; the caller treats that as "no
// per-start marker present" so older beads (pre-creation_complete_at rollout)
// fall through to the default sweepable path rather than being protected
// indefinitely.
func parseRFC3339Metadata(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	if t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

func (cr *CityRuntime) controlDispatcherTick(ctx context.Context) {
	store := cr.cityBeadStore()
	if store == nil || cr.sessionDrains == nil {
		return
	}

	filteredCfg := controlDispatcherOnlyConfig(cr.cfg)
	if filteredCfg == nil {
		return
	}

	sessionBeads := cr.loadSessionBeadSnapshot()
	wfcResult := buildDesiredStateWithSessionBeads(
		cr.cityName,
		cr.cityPath,
		time.Now(),
		filteredCfg,
		cr.sp,
		store,
		cr.rigBeadStores(),
		sessionBeads,
		nil,
		cr.stderr,
	)
	desiredState := wfcResult.State
	cfgNames := configuredSessionNamesWithSnapshot(filteredCfg, cr.cityName, sessionBeads)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(
		cr.cityPath,
		store,
		cr.rigBeadStores(),
		desiredState,
		cr.sp,
		cfgNames,
		filteredCfg,
		clock.Real{},
		cr.stderr,
		true,
		sessionBeads,
	)
	open := filterSessionBeadsByName(updated, cfgNames)
	poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(filteredCfg, cr.cityPath, open, wfcResult.AssignedWorkBeads, wfcResult.AssignedWorkStoreRefs)
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(
		filteredCfg, poolWorkBeads, open, wfcResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, wfcResult.NamedSessionDemand, filteredCfg)
	reconcileSessionBeadsAtPathWithNamedDemand(
		ctx,
		cr.cityPath,
		open,
		desiredState,
		cfgNames,
		filteredCfg,
		cr.sp,
		store,
		cr.dops,
		nil,
		cr.rigBeadStores(),
		nil, // control-dispatcher ticks only need ownership continuity, not main-tick assigned/ready snapshots
		cr.sessionDrains,
		poolDesired,
		wfcResult.NamedSessionDemand,
		false, // storeQueryPartial: config-change path doesn't query work beads
		nil,   // workSet: not computed for config-change reconcile
		cr.cityName,
		cr.it,
		clock.Real{},
		cr.rec,
		cr.cfg.Session.StartupTimeoutDuration(),
		cr.cfg.Daemon.DriftDrainTimeoutDuration(),
		cr.stdout,
		cr.stderr,
	)
	cr.requestDeferredDrainFollowUpTick()
}

// syncBeadsAndUpdateIndex runs syncSessionBeads.
func (cr *CityRuntime) syncBeadsAndUpdateIndex(desiredState map[string]TemplateParams, sessionBeads *sessionBeadSnapshot) *sessionBeadSnapshot {
	store := cr.cityBeadStore()
	cfgNames := configuredSessionNamesWithSnapshot(cr.cfg, cr.cityName, sessionBeads)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(
		cr.cityPath, store, cr.rigBeadStores(), desiredState, cr.sp, cfgNames, cr.cfg, clock.Real{}, cr.stderr, cr.sessionDrains != nil, sessionBeads,
	)
	return updated
}

// cityBeadStore returns the bead store for this city, preferring the
// controllerState store over the standalone store.
func (cr *CityRuntime) cityBeadStore() beads.Store {
	if cr.cs != nil {
		return cr.cs.CityBeadStore()
	}
	return cr.standaloneCityStore
}

func (cr *CityRuntime) rigBeadStores() map[string]beads.Store {
	if cr.cs != nil {
		stores := cr.cs.BeadStores()
		delete(stores, cr.cityName)
		return stores
	}
	return cr.standaloneRigStores
}

func (cr *CityRuntime) loadSessionBeadSnapshot() *sessionBeadSnapshot {
	sessionBeads, _ := cr.loadSessionBeadSnapshotWithPartial()
	return sessionBeads
}

func (cr *CityRuntime) loadSessionBeadSnapshotWithPartial() (*sessionBeadSnapshot, bool) {
	store := cr.cityBeadStore()
	if store == nil {
		return nil, false
	}
	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: loading session beads: %v\n", cr.logPrefix, err) //nolint:errcheck
		return nil, true
	}
	return sessionBeads, false
}

func filterSessionBeadsByName(snapshot *sessionBeadSnapshot, names map[string]bool) []beads.Bead {
	if snapshot == nil || len(names) == 0 {
		return nil
	}
	var filtered []beads.Bead
	for _, bead := range snapshot.Open() {
		if names[bead.Metadata["session_name"]] {
			filtered = append(filtered, bead)
		}
	}
	return filtered
}

func (cr *CityRuntime) buildDesiredState(sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
	store := cr.cityBeadStore()
	rigStores := cr.rigBeadStores()
	if cr.buildFnWithSessionBeads != nil {
		return cr.buildFnWithSessionBeads(cr.cfg, cr.sp, store, rigStores, sessionBeads, trace)
	}
	return cr.buildFn(cr.cfg, cr.sp, store)
}

func (cr *CityRuntime) loadDemandSnapshot(
	sessionBeads *sessionBeadSnapshot,
	trace *sessionReconcilerTraceCycle,
	trigger string,
	configChanged bool,
) runtimeDemandSnapshot {
	sessionFingerprint := sessionBeadSnapshotFingerprint(sessionBeads)
	if cr.shouldRefreshDemandSnapshot(trigger, configChanged, sessionFingerprint) {
		result := cr.buildDesiredState(sessionBeads, trace)
		var openSessionBeads []beads.Bead
		if sessionBeads != nil {
			openSessionBeads = sessionBeads.Open()
		}
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cr.cfg, cr.cityPath, openSessionBeads, result.AssignedWorkBeads, result.AssignedWorkStoreRefs)
		result.PoolDesiredCounts = PoolDesiredCounts(ComputePoolDesiredStatesTraced(
			cr.cfg, poolWorkBeads, openSessionBeads, result.ScaleCheckCounts, trace))
		if result.PoolDesiredCounts == nil {
			result.PoolDesiredCounts = make(map[string]int)
		}
		mergeNamedSessionDemand(result.PoolDesiredCounts, result.NamedSessionDemand, cr.cfg)
		result.WorkSet = computeWorkSet(cr.cfg, shellScaleCheck, cr.cityName, cr.cityPath, cr.cityBeadStore(), sessionBeads, cr.stderr)
		cr.demandSnapshot = &runtimeDemandSnapshot{
			createdAt:          time.Now(),
			sessionFingerprint: sessionFingerprint,
			result:             result,
		}
	}
	if cr.demandSnapshot == nil {
		return runtimeDemandSnapshot{}
	}
	snapshot := *cr.demandSnapshot
	cr.installDemandSnapshotSideEffects(snapshot.result)
	return snapshot
}

func (cr *CityRuntime) shouldRefreshDemandSnapshot(
	trigger string,
	configChanged bool,
	sessionFingerprint string,
) bool {
	if !cr.demandSnapshotsEnabled() {
		return true
	}
	if configChanged || trigger != "patrol" {
		return true
	}
	if cr.demandSnapshot == nil {
		return true
	}
	if cr.demandSnapshot.sessionFingerprint != sessionFingerprint {
		return true
	}
	return time.Since(cr.demandSnapshot.createdAt) >= runtimeDemandSnapshotMaxAge
}

func (cr *CityRuntime) demandSnapshotsEnabled() bool {
	return cr.cs != nil && cr.cs.EventProvider() != nil && demandSnapshotDemandSourcesEventBacked(cr.cfg)
}

func demandSnapshotDemandSourcesEventBacked(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Agents {
		if strings.TrimSpace(cfg.Agents[i].ScaleCheck) != "" || strings.TrimSpace(cfg.Agents[i].WorkQuery) != "" {
			return false
		}
	}
	return true
}

func (cr *CityRuntime) installDemandSnapshotSideEffects(result DesiredStateResult) {
	autoSP, ok := cr.sp.(*sessionauto.Provider)
	if !ok {
		return
	}
	for _, tp := range result.State {
		if !tp.IsACP || strings.TrimSpace(tp.SessionName) == "" {
			continue
		}
		autoSP.RouteACP(tp.SessionName)
	}
}

func sessionBeadSnapshotFingerprint(snapshot *sessionBeadSnapshot) string {
	if snapshot == nil {
		return ""
	}
	open := snapshot.Open()
	sort.Slice(open, func(i, j int) bool {
		return open[i].ID < open[j].ID
	})
	h := fnv.New64a()
	for _, bead := range open {
		_, _ = io.WriteString(h, bead.ID)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Status)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Assignee)
		_, _ = io.WriteString(h, "\x00")
		keys := make([]string, 0, len(bead.Metadata))
		for key := range bead.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = io.WriteString(h, key)
			_, _ = io.WriteString(h, "\x00")
			_, _ = io.WriteString(h, bead.Metadata[key])
			_, _ = io.WriteString(h, "\x00")
		}
	}
	return fmt.Sprintf("%x", h.Sum64())
}

func buildStandaloneRigStores(cfg *config.City, cityPath string, stderr io.Writer) map[string]beads.Store {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil
	}
	stores := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		// Unbound rigs (declared in city.toml but missing a
		// .gc/site.toml binding) have an empty rig.Path;
		// openStoreAtForCity would silently fall back to the city
		// scope, aliasing the rig store to the city store. Skip them
		// so supervisor-mode store maps match api_state.buildStores.
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc supervisor: rig bead store %q: %v\n", rig.Name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		stores[rig.Name] = store
	}
	if len(stores) == 0 {
		return nil
	}
	return stores
}

func (cr *CityRuntime) beginTraceCycle(trigger, detail string, sessionBeads *sessionBeadSnapshot) *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	info := sessionReconcilerTraceCycleInfo{
		TickTrigger:    trigger,
		TriggerDetail:  detail,
		CityPath:       cr.cityPath,
		ConfigRevision: cr.configRev,
	}
	return cr.trace.beginCycle(info, cr.cfg, sessionBeads)
}

func (cr *CityRuntime) drainOutgoingOrderDispatcher(ctx context.Context, od orderDispatcher) {
	if od == nil {
		return
	}
	if od.drain(ctx) {
		return
	}
	cr.retiredOrderDispatchers = append(cr.retiredOrderDispatchers, od)
}

func (cr *CityRuntime) drainOrderDispatchers(ctx context.Context) {
	var retained []orderDispatcher
	if cr.od != nil && !cr.od.drain(ctx) {
		retained = append(retained, cr.od)
	}
	for _, od := range cr.retiredOrderDispatchers {
		if od == nil {
			continue
		}
		if !od.drain(ctx) {
			retained = append(retained, od)
		}
	}
	cr.retiredOrderDispatchers = retained
}

func orderShutdownDrainTimeout(total time.Duration) time.Duration {
	if total <= 0 {
		return 0
	}
	if total < reloadOrderDrainTimeout {
		return total
	}
	return reloadOrderDrainTimeout
}

func (cr *CityRuntime) recordPreservedShutdownTrace() {
	trace := cr.beginTraceCycle("shutdown", "preserve_sessions", nil)
	if trace == nil {
		return
	}
	trace.recordOperation("lifecycle.shutdown.preserve_sessions", "", "", "", "retained", string(TraceOutcomeApplied), traceRecordPayload{
		"city_path": cr.cityPath,
		"city_name": cr.cityName,
		"reason":    "supervisor_shutdown_preserve_mode",
	}, "")
	trace.end(TraceCompletionCompleted, traceRecordPayload{
		"phase":     "shutdown",
		"mode":      "preserve_sessions",
		"city_name": cr.cityName,
		"reason":    "supervisor_shutdown_preserve_mode",
	})
}

// shutdown performs graceful two-pass agent shutdown for this city.
// Safe to call multiple times (e.g., from both panic recovery and
// normal shutdown) — only the first call takes effect.
func (cr *CityRuntime) shutdown() {
	cr.shutdownOnce.Do(func() {
		cr.waitForAsyncStarts()
		preserveSessions := cr.preserveSessionsShutdown.Load()
		if preserveSessions {
			cr.recordPreservedShutdownTrace()
		}
		if cr.trace != nil {
			_ = cr.trace.Close()
		}
		if cr.svc != nil {
			// Workspace-service proxies are process-group-bound, not preserved
			// agent sessions. Close them so the next supervisor can reacquire
			// their sockets and ports during re-adoption.
			if err := cr.svc.Close(); err != nil {
				fmt.Fprintf(cr.stderr, "%s: service shutdown: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
			}
		}
		if preserveSessions {
			fmt.Fprintf(cr.stdout, "Preserving agent sessions for supervisor re-adoption.\n") //nolint:errcheck // best-effort stdout
			return
		}
		// Drain order dispatchers with a small cap before stopping sessions.
		// Use a fresh context because the tick ctx is already canceled at this
		// point, which would make drain a no-op. shutdown_timeout remains the
		// graceful session-stop budget; order drain does not silently halve it.
		// Orphaned tracking beads (if drain times out) are closed by
		// sweepOrphanedOrderTrackingRetry on next start.
		total := cr.cfg.Daemon.ShutdownTimeoutDuration()
		gracefulTimeout := total
		if cr.od != nil || len(cr.retiredOrderDispatchers) > 0 {
			drainTimeout := orderShutdownDrainTimeout(total)
			drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
			cr.drainOrderDispatchers(drainCtx)
			drainCancel()
		}
		running, listErr := cr.sp.ListRunning("")
		if listErr != nil {
			if runtime.IsPartialListError(listErr) {
				fmt.Fprintf(cr.stderr, "%s: shutdown session listing partially failed; stopping %d visible agent(s): %v\n", cr.logPrefix, len(running), listErr) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(cr.stderr, "%s: shutdown session listing failed: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
			}
		}
		store := cr.cityBeadStore()
		markCityStopSessionSleepReason(store, cr.stderr)
		gracefulStopAll(running, cr.sp, gracefulTimeout, cr.rec, cr.cfg, store, cr.stdout, cr.stderr)
	})
}

func (cr *CityRuntime) preserveSessionsOnShutdown() {
	cr.preserveSessionsShutdown.Store(true)
}
