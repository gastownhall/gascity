package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// newCityRuntimeOpenSweepStore opens stores used by order-tracking sweeps.
// Test code can swap this to return in-memory stores and skip spawning
// managed dolt.
var newCityRuntimeOpenSweepStore = openStoreAtForCity

// sweepOrphanedOrderTrackingAtBoot closes order-tracking beads a previous
// controller instance left open (goroutines killed on restart, or silent Close
// failures). It runs on startup only, never on config reload, and is
// best-effort: a store it cannot open is reported and skipped, and the retry
// with backoff is defense-in-depth against transient store errors immediately
// after ensureBeadsProvider returns (#753).
//
// It sweeps the city work store AND, on a split city, the orders binding those
// tracking beads are now born in. Both, not one. A converged city holds
// pre-cutover orphans in the work store and every new one in the binding, and an
// orphan left open is an order that never fires again — the open bead IS the
// single-flight marker, so a controller that cannot see it never clears it. A
// boot sweep that reads only the work store on a split city is therefore not a
// degraded sweep, it is no sweep at all.
//
// The binding handle belongs to routes and is closed with them at runtime
// shutdown; only the work store opened here is closed here.
func sweepOrphanedOrderTrackingAtBoot(routes *storageRoutes, cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer) {
	workStore, err := newCityRuntimeOpenSweepStore(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: order tracking sweep: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	defer closeBeadStoreHandle(workStore) //nolint:errcheck

	stores := []beads.Store{workStore}
	if ordersStore := resolveOrderStore(routes, nil, cfg, cityPath, rec); ordersStore != nil && ordersStore != workStore {
		stores = append(stores, ordersStore)
	}
	for _, store := range stores {
		if n, err := sweepOrphanedOrderTrackingRetryLimit(store, 3, time.Second, orderTrackingSweepCloseBudget); err != nil {
			fmt.Fprintf(stderr, "gc start: order tracking sweep (closed %d): %v\n", n, err) //nolint:errcheck // best-effort stderr
		} else if n > 0 {
			fmt.Fprintf(stderr, "gc start: closed %d orphaned order-tracking beads\n", n) //nolint:errcheck // best-effort stderr
		}
	}
	// The advisory is about the city's backlog, so it counts the stores together.
	// Per-store thresholds would halve the sensitivity on exactly the split cities
	// the sweep was extended for: a converged city holds half its closed tracking
	// beads in the work ledger and half in the binding, and neither half alone
	// reaches the threshold the pair clears.
	warnIfClosedOrderTrackingBacklogLarge(stores, stderr)
}

// reloadOrderDrainTimeout bounds how long config reload will wait for
// the outgoing order dispatcher's in-flight goroutines before replacing
// it. Reload runs on the tick loop, so a larger budget would stall all
// other subsystems. Dispatchers that do not drain within this budget are
// retained and drained again during controller shutdown; orphan tracking
// beads are still compensated by the next startup sweep if shutdown also
// cannot wait long enough.
const reloadOrderDrainTimeout = 1 * time.Second

var orderRescanInterval = time.Minute

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
	mat                     maxSessionAgeTracker
	adt                     assignedWorkDeferTracker
	wg                      wispGC
	od                      orderDispatcher
	retiredOrderDispatchers []orderDispatcher
	orderSet                []orders.Order
	orderSetSignature       string
	orderRescanEnabled      bool
	orderRescanLast         time.Time
	trace                   *sessionReconcilerTraceManager

	// routeRecovery is the route-repair lane: an event-fed delta pass in the
	// tick and a cadenced authoritative scan behind it. Created on first use so
	// a directly-constructed runtime needs no wiring.
	routeRecovery     *routeRecoveryLane
	routeRecoveryOnce sync.Once

	// completions is the graph.v2 completion-fact lane, the same delta/sweep
	// split as routeRecovery.
	completions     *completionsLane
	completionsOnce sync.Once

	// detachedOrphan is the detached-handoff-orphan lane, the same delta/scan
	// split applied to the sweep that was 48.5% of the tick (ga-l7jdg).
	detachedOrphan     *detachedOrphanLane
	detachedOrphanOnce sync.Once

	orderSweepWatchdogLast             time.Time
	orderTrackingRetentionWatchdogLast time.Time
	nudgeMailSweepWatchdogLast         time.Time
	wispIndexMigrationApplied          bool

	rec events.Recorder
	cs  *controllerState // nil when controller-managed bead stores are unavailable
	svc *workspacesvc.Manager

	poolSessions      map[string]time.Duration
	poolDeathHandlers map[string]poolDeathInfo
	suspendedNames    map[string]bool

	standaloneCityStore beads.Store // non-nil when API disabled; for chat auto-suspend
	standaloneRigStores map[string]beads.Store

	// storageRoutes is the opened non-work storage binding this process
	// resolved once at boot, or nil for every city that authors no [storage]
	// section. It is immutable for the life of the process: a reload that would
	// change [storage] is refused by the StorageReloadRequiresRestart check in
	// reloadConfigTraced rather than swapping a live handle.
	storageRoutes *storageRoutes

	// Bead-driven reconciler state (Phase 2f).
	sessionDrains      *drainTracker       // in-memory drain tracker; nil when bead reconciler disabled
	providerHealthGate *providerHealthGate // ADR-0013 A1 M3a; nil until bead reconciler initialized
	asyncStartLimiter  *asyncStartLimiter
	asyncStarts        asyncStartTracker
	asyncStops         asyncStartTracker
	demandSnapshot     *runtimeDemandSnapshot

	// liveSweepMemos carries the live model-usage sweep's per-session memo: the
	// resolved transcript path, whether discovery definitively found nothing, and
	// the sweep-interval floor. The worker factory is rebuilt per tick, so this
	// process-lifetime cache is what keeps a per-tick live lane from repeating
	// bounded discovery and transcript reads for every awake session.
	liveSweepMemos sync.Map // session bead id -> liveSweepMemo

	// transcriptMetaEnabled is set only by the machine-wide supervisor after it
	// has armed the event-correlation sidecar gate. One asynchronous snapshot pass
	// is permitted for this supervisor lifetime; a restart deliberately rebuilds
	// and idempotently replays it. Standalone/one-shot runtimes leave this false.
	transcriptMetaEnabled bool
	transcriptMetaMu      sync.Mutex
	transcriptMetaStarted bool
	transcriptMetaDone    chan struct{}

	fsPressureConsecutiveSkips int
	fsPressureEpisodeLogged    bool

	// reapSkips carries worktree-reaper skip history between ticks so an
	// unchanged skip is reported once instead of on every sweep. Owned by the
	// serial tick, like the other per-tick state above.
	reapSkips *reapSkipTracker

	convScopes          map[string]*convergenceScope // nil until bead store available; keyed by rig name ("" = city/HQ)
	convScopesMu        sync.RWMutex                 // guards convScopes map pointer
	convergenceReqCh    chan convergenceRequest      // receives CLI commands from controller.sock
	reloadReqCh         chan reloadRequest           // receives structured reload requests from controller.sock
	pokeCh              chan struct{}                // non-blocking signal to trigger immediate reconciler tick
	controlDispatcherCh chan struct{}                // non-blocking signal for control-dispatcher-only reconcile
	nudgeWakeCh         chan struct{}                // signal to dispatch queued nudges; fed by wake socket listener
	reloadMu            sync.Mutex                   // guards activeReload
	activeReload        *reloadRequest
	onStarted           func()
	onStatus            func(string)
	managedDoltHealth   func(string) error
	managedDoltOwned    func(string) (bool, error)
	managedDoltPort     func(string) string

	shutdownOnce             sync.Once
	preserveSessionsShutdown atomic.Bool
	// ownedCity is set true once run() begins, i.e. once this runtime has
	// passed every init-failure/discard point and is the process actually
	// driving the city. It gates the shutdown-time server teardown: a
	// discarded half-built runtime (e.g. adoption of an already-running
	// city, or a controller-lock/socket/token failure) calls shutdown()
	// without ever owning the city, and must not tear down the live city's
	// shared server out from under it.
	ownedCity         atomic.Bool
	forceStopShutdown *atomic.Bool
	logPrefix         string // "gc start" or "gc supervisor"
	stdout, stderr    io.Writer
}

// runtimeDemandSnapshotBackstopMaxAge bounds how long an EVENT-BACKED demand
// snapshot may be reused before it is rebuilt regardless of what the ready
// fingerprint says.
//
// It is a convergence backstop, not the freshness mechanism, and it used to be
// neither. At 30s it sat at or below the patrol interval — and far below a slow
// tick — which mattered because the age gate SHORT-CIRCUITS the fingerprint:
// loadDemandSnapshot only computes readyDemandSnapshotFingerprint when the
// snapshot is not already due. On maintainer-city's 373s tick the snapshot was
// therefore always due, the fingerprint was never consulted, and the cache it
// guards was dead code (ga-l7jdg).
//
// Freshness is the fingerprint's job: a claim, close or create anywhere in the
// routed-demand leg set changes it and forces a rebuild in the same tick, and
// the session fingerprint covers the session half. This bound exists for what
// neither can see — a change no read of the ready set reflects — so it is set
// well clear of any plausible tick rather than tuned for responsiveness.
//
// It applies ONLY on the event-backed path. A city with a configured scale_check
// is not event-backed at all (demandSnapshotsEnabled), and keeps the per-tick
// rebuild floored at scaleCheckDemandMinInterval below.
const runtimeDemandSnapshotBackstopMaxAge = 5 * time.Minute

// scaleCheckDemandMinInterval floors how often a patrol tick re-runs an agent
// scale_check probe. scale_check demand cannot ride the event-backed
// demand-snapshot cache because its result is captured neither by the session
// fingerprint nor by the event stream, so demandSnapshotsEnabled reports false
// whenever any agent configures a scale_check. Without a floor, a sub-second
// patrol_interval re-runs the probe subprocess every tick — driving a full,
// metadata-filtered List against the managed Dolt store per tick. Flooring the
// patrol re-eval cadence collapses that storm. It is a no-op for the default
// patrol_interval (30s) and only bites pathologically fast patrol cadences,
// where it bounds scale_check-routed pool-work discovery latency to this floor
// (see demandSnapshotPatrolMaxAge).
const scaleCheckDemandMinInterval = 1 * time.Second

type runtimeDemandSnapshot struct {
	createdAt              time.Time
	sessionFingerprint     string
	readyDemandFingerprint string
	result                 DesiredStateResult
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
	ForceStopShutdown *atomic.Bool

	ConvergenceReqCh    chan convergenceRequest // may be nil
	ReloadReqCh         chan reloadRequest      // may be nil; receives structured reload commands
	PokeCh              chan struct{}           // may be nil; triggers immediate tick
	ControlDispatcherCh chan struct{}           // may be nil; triggers control-dispatcher-only reconcile
	OnStarted           func()                  // called after initial reconciliation succeeds
	OnStatus            func(string)            // called when init status changes
	ManagedDoltHealth   func(string) error
	ManagedDoltOwned    func(string) (bool, error)
	ManagedDoltPort     func(string) string
	// TranscriptMetaEnabled opts this supervisor-owned city runtime into the
	// bounded historical sidecar pass. Standalone `gc start` callers retain the
	// zero value, so one-shot CLI processes cannot activate the reconcile path.
	TranscriptMetaEnabled bool

	LogPrefix      string // "gc start" or "gc supervisor"; defaults to "gc start"
	Stdout, Stderr io.Writer
}

var (
	cityRuntimeStartBeadsLifecycle       = startBeadsLifecycle
	cityRuntimeReloadLifecycleRetryDelay = time.Second
	// reloadActiveTTL bounds how long a single accepted reload may occupy
	// the activeReload slot. If a reconciler tick wedges in something that
	// does not panic (so the tick's defer never runs), activeReload stays
	// set and every subsequent gc reload returns busy. When a fresh
	// request arrives and the existing activeReload has been resident for
	// longer than this TTL, handleReloadRequest force-clears the stuck
	// slot (replying timeout on the old request's doneCh) and accepts the
	// new request. Set well above legitimate reload duration so a slow
	// but progressing reload is not killed in flight. Test-overridable.
	reloadActiveTTL = 10 * time.Minute
)

const cityRuntimeReloadLifecycleRetryLimit = 2

// postCreateProtectionTimeout protects freshly-started pool beads from the
// first steady-state sweep even after wake bookkeeping metadata lands.
// It is intentionally longer than staleCreatingStateTimeout because startup
// plus the first patrol can legitimately exceed one minute.
const postCreateProtectionTimeout = 2 * time.Minute

// newCityRuntime creates a CityRuntime, building internal components
// (crash tracker, idle tracker, wisp GC, order dispatcher) from the
// provided parameters.
//
// It returns an error when this city must not start. Today there is exactly one
// such condition and it is deliberate: a city whose [storage.classes] name a
// binding it has not provably converged on would otherwise serve infrastructure
// reads AND writes from a store nothing proved holds its state. The gate that
// decides is storage_boot.go; every caller here does is print the error and
// stop this one city.
func newCityRuntime(p CityRuntimeParams) (*CityRuntime, error) {
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
	mat := buildMaxSessionAgeTracker(p.Cfg, p.CityName, p.SP)
	adt := buildAssignedWorkDeferTracker(p.Cfg, p.CityName, p.SP)

	wg := newWispGCForConfig(p.Cfg)

	managedDoltHealth := p.ManagedDoltHealth
	if managedDoltHealth == nil {
		managedDoltHealth = healthBeadsProvider
	}
	managedDoltOwned := p.ManagedDoltOwned
	if managedDoltOwned == nil {
		managedDoltOwned = managedDoltLifecycleOwned
	}
	managedDoltPort := p.ManagedDoltPort
	if managedDoltPort == nil {
		managedDoltPort = currentResolvableManagedDoltPort
	}

	logPrefix := p.LogPrefix
	if logPrefix == "" {
		logPrefix = "gc start"
	}

	ensureManagedDoltPublishedForRuntime(p.CityPath, p.Stderr, logPrefix, managedDoltHealth, managedDoltOwned, managedDoltPort)

	// Storage-class routing, resolved once and before any store below is opened
	// for use. A city that authors no [storage] short-circuits inside the gate
	// without constructing a registry or a plan; a city whose config and data
	// disagree stops here rather than serving from the wrong side of a cutover.
	routes, err := storageBootGate(p.CityPath, p.Cfg, logPrefix, p.Rec, p.Stderr)
	if err != nil {
		return nil, err
	}
	// NOTE: the routes are NOT registered as this city's residency answer here.
	// Construction happens before the supervisor knows whether it can take the
	// controller lock, and a replacement that loses it would have repointed the
	// live city's release sweeps at a binding it is about to close. The lock
	// holder registers — see registerResidencyRoutes.

	sweepOrphanedOrderTrackingAtBoot(routes, p.CityPath, p.Cfg, p.Rec, p.Stderr)

	od, orderSnapshot := buildOrderDispatcherWithSnapshot(routes, p.CityPath, p.Cfg, p.Rec, p.Stderr, "gc start: order scan")

	suspendedNames := computeSuspendedNames(p.Cfg, p.CityName, p.CityPath)

	cr := &CityRuntime{
		storageRoutes:           routes,
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
		mat:                     mat,
		adt:                     adt,
		wg:                      wg,
		od:                      od,
		orderSet:                orderSnapshot.Orders,
		orderSetSignature:       orderSnapshot.Signature,
		orderRescanEnabled:      true,
		orderRescanLast:         time.Now(),
		trace:                   newSessionReconcilerTraceManager(p.CityPath, p.CityName, p.Stderr),
		rec:                     p.Rec,
		reapSkips:               newReapSkipTracker(),
		poolSessions:            p.PoolSessions,
		poolDeathHandlers:       p.PoolDeathHandlers,
		forceStopShutdown:       p.ForceStopShutdown,
		suspendedNames:          suspendedNames,
		asyncStartLimiter:       newAsyncStartLimiter(maxParallelStartsPerTick(p.Cfg)),
		transcriptMetaEnabled:   p.TranscriptMetaEnabled,
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
		nudgeWakeCh:       make(chan struct{}, 1),
		onStarted:         p.OnStarted,
		onStatus:          p.OnStatus,
		managedDoltHealth: managedDoltHealth,
		managedDoltOwned:  managedDoltOwned,
		managedDoltPort:   managedDoltPort,
		logPrefix:         logPrefix,
		stdout:            p.Stdout,
		stderr:            p.Stderr,
	}
	cr.svc = workspacesvc.NewManager(&serviceRuntime{cr: cr})
	if err := cr.svc.Reload(); err != nil {
		fmt.Fprintf(cr.stderr, "%s: service init: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	return cr, nil
}

// setControllerState sets the API state for this city. The controller
// state is managed by the caller (who also owns the API server), installed
// before run starts, and never replaced afterward.
// The routes deliberately do NOT cross here. They are a construction input to
// newControllerStateWithRoutes, because the class-routed services a
// controllerState owns — the mail provider and the external-messaging services —
// are built during construction and cannot be re-pointed by a later assignment.
// Installing them here also wrote the field without cs.mu while the API's class
// accessors read it under RLock.
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
	// Reaching run() means every init-failure/discard point is behind us:
	// this runtime is the live owner of the city, so its shutdown() is the
	// one allowed to tear the provider's shared server down.
	cr.ownedCity.Store(true)
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

	// Initialize bead-driven drain tracker and provider-health gate when bead store is available.
	if cr.cityBeadStore() != nil && cr.tomlPath != "" {
		cr.sessionDrains = newDrainTracker()
		cr.providerHealthGate = newProviderHealthGate()
	}
	if ctx.Err() != nil {
		return
	}

	// Startup instrumentation: per-phase elapsed timing plus a watchdog
	// that dumps goroutines if onStarted has not fired by half of
	// [daemon].start_ready_timeout. Operators previously got a generic
	// client-side timeout with no breadcrumbs (#gco-4pj); these log lines
	// surface where startup is spending its budget.
	startupBegan := time.Now()
	startupReady := make(chan struct{})
	var readyOnce sync.Once
	markReady := func() { readyOnce.Do(func() { close(startupReady) }) }
	defer markReady()
	if total := cr.cfg.Daemon.StartReadyTimeoutDuration(); total > 0 {
		go cr.startupReadinessWatchdog(ctx, startupReady, total/2, total)
	}
	logPhaseElapsed := func(name string, start time.Time) {
		fmt.Fprintf(cr.stderr, "%s: startup phase=%s elapsed=%s\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, name, time.Since(start).Round(time.Millisecond))
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
		phaseStart := time.Now()
		defer func() { logPhaseElapsed(trigger, phaseStart) }()
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
			result, passed := runAdoptionBarrier(cr.cityPath, sessionFrontDoor(cr.sessionsBeadStore().Store), cr.sp, cr.cfg, cr.cityName, clock.Real{}, cr.stderr, false)
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

	configReloadStart := time.Now()
	cr.applyStartupConfigReload(ctx, dirty, &lastProviderName, cityRoot)
	logPhaseElapsed("config-reload", configReloadStart)
	if ctx.Err() != nil {
		return
	}

	// Dispatch due orders before startup session reconciliation. A cold-start
	// reconcile can take minutes when it has stale or config-drifted sessions;
	// due event/condition formulas should not wait behind that maintenance work.
	startupOrdersStart := time.Now()
	cr.safeTick(func() {
		cr.dispatchOrders(ctx, cityRoot)
	}, "startup-orders")
	logPhaseElapsed("startup-orders", startupOrdersStart)
	if ctx.Err() != nil {
		return
	}

	// Recover ready work whose canonical pool route was lost or never written
	// (gc.run_target set, gc.routed_to empty) before session reconciliation, so a
	// post-restart rig re-enters pool demand without a manual `gc sling`
	// (ga-n2d.4). Placed before the expensive reconcile for the same reason as
	// startup-orders: routed demand should not wait behind cold-start drift work.
	//
	// The journal feed is armed FIRST, then the startup scan runs. The feed
	// watches from the journal head at the moment it starts, so arming it before
	// the scan makes the two overlap: everything older than the head is the
	// scan's to repair and everything newer is the delta lane's. Arming it after
	// would leave the scan's own duration as a hole no lane is watching. The
	// overlap costs nothing — both passes are idempotent.
	cr.startTickDeltaLanes(ctx, cr.routeRecoveryEventProvider())
	// This is the lane's startup backstop, and it is the LAST full scan on the
	// critical path: from here the tick sees only the event-fed delta pass, and
	// the authoritative scan runs on cadence in the background lane.
	startupRouteRecoveryStart := time.Now()
	cr.safeTick(func() {
		cr.recoverUnroutedWorkRoutes()
	}, "startup-route-recovery")
	logPhaseElapsed("startup-route-recovery", startupRouteRecoveryStart)
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
		cr.ensureManagedDoltPublishedForTick()
		sessionBeads := cr.loadSessionBeadSnapshot()
		startupTrace := cr.beginTraceCycle("startup", "initial_reconcile", sessionBeads)
		completion := TraceCompletionAborted
		defer func() {
			if startupTrace != nil {
				startupTrace.end(completion, traceRecordPayload{"phase": "startup"})
			}
		}()

		cleanupDeadRuntimeSessionCorpses(cr.sessionsBeadStore().Store, cr.rigBeadStores(), cr.cfg, sessionBeads, cr.sessionDrains, cr.sp, clock.Real{}, cr.stderr)
		// Reap live runtimes still bound to a closed bead (e.g. a named-session
		// identity re-minted as a pool slot) so the name's current owner can
		// rebind it and attach lands on the right runtime.
		reapRuntimesBoundToClosedBeads(cr.sessionsBeadStore().Store, sessionBeads, cr.sessionDrains, cr.sp, cr.stderr)
		if swept := sweepProcessTableOrphans(cr.sp, sessionBeads, cr.sessionsBeadStore().Store, cr.cityPath, cr.stderr); swept > 0 {
			fmt.Fprintf(cr.stderr, "session reconciler: swept %d process-table orphan runtime(s)\n", swept) //nolint:errcheck
		}
		// Reap stale session beads from a previous run before building desired
		// state, so desired state does not reference already-closed beads (#742).
		if reapStaleSessionBeads(cr.sessionsBeadStore().Store, cr.sp, cr.sessionDrains, clock.Real{}, cr.stderr) > 0 {
			sessionBeads = cr.loadSessionBeadSnapshot()
		}
		result := cr.buildDesiredState(sessionBeads, startupTrace)
		sessionBeads = cr.loadSessionBeadSnapshot()
		result = cr.refreshDesiredState(result, sessionBeads)
		sessionBeads = cr.syncBeadsAndUpdateIndex(result.State, sessionBeads)
		result = cr.refreshDesiredState(result, sessionBeads)
		if ctx.Err() != nil {
			return
		}

		if cr.sessionDrains != nil {
			cr.beadReconcileTick(ctx, result, sessionBeads, startupTrace, true)
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
	markReady()
	fmt.Fprintf(cr.stderr, "%s: startup ready elapsed=%s\n", //nolint:errcheck // best-effort stderr
		cr.logPrefix, time.Since(startupBegan).Round(time.Millisecond))
	fmt.Fprintln(cr.stdout, "City started.") //nolint:errcheck // best-effort stdout
	if ctx.Err() != nil {
		return
	}
	// Track pool instance liveness for death detection.
	var prevPoolRunning map[string]bool
	runTick := func(trigger string) {
		if ctx.Err() != nil {
			return
		}
		// Record the tick reason for any bd subprocess spawned during
		// this tick — TraceBDCall reads it to attribute calls to
		// patrol vs poke. Single-tenant best-effort: restore the
		// previous value on exit so nested ticks don't lose context.
		prev := beads.SetReconcilerTickTrigger(trigger)
		defer beads.RestoreReconcilerTickTrigger(prev)
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

	// Start the supervisor nudge dispatcher when configured. The wake-socket
	// listener feeds nudgeWakeCh on every producer enqueue, giving sub-second
	// dispatch latency. Patrol-tick fallback inside cr.tick() guarantees
	// eventual delivery if the wake is missed (socket race, listener
	// restart). Legacy mode skips the listener entirely; per-session
	// pollers continue to own delivery.
	if nudgeDispatcherIsSupervisor(cr.cfg) && cr.cityPath != "" {
		if _, err := startNudgeWakeListener(ctx, cr.cityPath, cr.nudgeWakeCh, cr.stderr, cr.logPrefix); err != nil {
			fmt.Fprintf(cr.stderr, "%s: nudge dispatcher: %v (falling back to patrol-only delivery)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
	}

	// Reload acceptance runs on its own goroutine so that a slow tick
	// body (e.g., a session-start wave that waits for startup_timeout)
	// does not block reload request acceptance. The accept path
	// (handleReloadRequest) takes cr.reloadMu to stage activeReload and
	// configDirty; the actual reload work still runs on the reconciler
	// goroutine in the next tick. See bead ga-8nbr.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-cr.reloadReqCh:
				cr.safeTick(func() {
					cr.handleReloadRequest(&req)
				}, "reload-accept")
			}
		}
	}()
	defer func() {
		<-acceptDone
		cr.failActiveReload("Reload canceled because the controller is shutting down.")
	}()
	// Debounce event-driven ticks so a burst of pokes / control-dispatcher
	// signals collapses into a single fire. Each channel gets its own
	// debouncer so trace-tag identity ("poke" vs "control-dispatcher") is
	// preserved when the deferred tick eventually runs. With debounce=0
	// (the default), arm() falls back to non-blocking send and the loop
	// behaves identically to the pre-debounce implementation.
	pokeDB := newTickDebouncer()
	ctrlDB := newTickDebouncer()
	defer pokeDB.cancelPending()
	defer ctrlDB.cancelPending()

	for {
		// Re-read on every iteration so a hot reload of city.toml takes
		// effect on the next event without disturbing in-flight timers.
		debounce := cr.cfg.Daemon.TickDebounceDuration()
		select {
		case <-ticker.C:
			// Patrol scans every reconciler state authoritatively, so any
			// pending event-driven fires are redundant — drop them.
			pokeDB.cancelPending()
			ctrlDB.cancelPending()
			runTick("patrol")
		case <-cr.pokeCh:
			// Event-driven wake path: sling or API assigned work to a sleeping
			// session. Arm the debouncer; the deferred fire runs runTick("poke")
			// once the burst settles.
			pokeDB.arm(debounce)
		case <-pokeDB.fired():
			runTick("poke")
		case <-cr.nudgeWakeCh:
			cr.safeTick(func() {
				cr.nudgeDispatchTick(ctx)
			}, "nudge-wake")
		case <-cr.controlDispatcherCh:
			ctrlDB.arm(debounce)
		case <-ctrlDB.fired():
			cr.safeTick(func() {
				cr.controlDispatcherTick(ctx)
			}, "control-dispatcher")
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

// startupReadinessWatchdog emits a warning + goroutine dump to stderr
// if startup has not signaled ready within delay. delay is normally
// half of [daemon].start_ready_timeout, giving operators a snapshot of
// which goroutines are blocked while the client-side probe still has
// budget left. It exits silently when ready is signaled, when ctx is
// canceled, or after firing once. Run as its own goroutine.
func (cr *CityRuntime) startupReadinessWatchdog(ctx context.Context, ready <-chan struct{}, delay, total time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ready:
		return
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	buf := make([]byte, 1<<20)
	n := goruntime.Stack(buf, true)
	fmt.Fprintf(cr.stderr, //nolint:errcheck // best-effort stderr
		"%s: startup watchdog: city %q not ready after %s (half of [daemon].start_ready_timeout=%s); goroutine dump follows:\n%s\n",
		cr.logPrefix, cr.cityName, delay, total, buf[:n])
}

// tickDebouncer coalesces bursty event-driven tick signals into a
// single delayed fire. The first arm() call in a quiet period schedules
// a timer; subsequent arm() calls while the timer is pending are
// dropped (the eventual single fire re-reads authoritative state
// covering all collapsed events). When delay <= 0 it falls back to
// non-blocking send on fired(), preserving the cap=1 channel-level
// coalesce semantics the runtime had before debouncing was added.
//
// Methods are safe to call from multiple goroutines (time.AfterFunc
// callbacks run on their own goroutine).
type tickDebouncer struct {
	mu     sync.Mutex
	timer  *time.Timer
	fireCh chan struct{}
}

// newTickDebouncer allocates a tickDebouncer with a cap=1 fire channel.
// The channel buffer matches the existing pokeCh/controlDispatcherCh
// non-blocking-send pattern so a pending fire collapses with any new
// arm() call that completes before the receiver drains.
func newTickDebouncer() *tickDebouncer {
	return &tickDebouncer{fireCh: make(chan struct{}, 1)}
}

// arm schedules a fire after delay if no fire is already pending. If
// delay <= 0 the fire is enqueued immediately (non-blocking) to keep
// debounce-disabled runtime cost identical to the prior implementation.
func (d *tickDebouncer) arm(delay time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if delay <= 0 {
		select {
		case d.fireCh <- struct{}{}:
		default:
		}
		return
	}
	if d.timer != nil {
		return // already pending — burst collapse
	}
	d.timer = time.AfterFunc(delay, func() {
		d.mu.Lock()
		d.timer = nil
		d.mu.Unlock()
		select {
		case d.fireCh <- struct{}{}:
		default:
		}
	})
}

// cancelPending stops an armed timer and discards a queued fire, if
// any. Used when a higher-priority tick (e.g. the periodic patrol)
// supersedes whatever caused the pending fire.
func (d *tickDebouncer) cancelPending() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	select {
	case <-d.fireCh:
	default:
	}
}

// fired returns the channel that emits when a debounced fire is due.
func (d *tickDebouncer) fired() <-chan struct{} {
	return d.fireCh
}

// convScope returns the convergence scope for the given rig name under a read
// lock so callers outside the run() goroutine can safely read the map.
func (cr *CityRuntime) convScope(rig string) *convergenceScope {
	cr.convScopesMu.RLock()
	defer cr.convScopesMu.RUnlock()
	return cr.convScopes[rig]
}

func convergenceStartupComplete(cr *CityRuntime) bool {
	if cr.convScopes == nil || cr.convergenceReqCh == nil {
		return true
	}
	// Startup is complete once every scope's active index is populated.
	for _, scope := range cr.convScopes {
		if scope.adapter.activeIndex == nil {
			return false
		}
	}
	return true
}

// reconcilePoolDeaths detects pool instances that stopped since the prior
// reconciliation and runs their configured death hooks.
func (cr *CityRuntime) reconcilePoolDeaths(prevPoolRunning *map[string]bool) {
	if len(cr.poolDeathHandlers) == 0 {
		return
	}
	currentRunning, listErr := cr.sp.ListRunning("")
	if listErr != nil {
		if runtime.IsPartialListError(listErr) {
			fmt.Fprintf(cr.stderr, "%s: pool death check skipped due to partial session listing: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(cr.stderr, "%s: pool death check skipped while listing sessions: %v\n", cr.logPrefix, listErr) //nolint:errcheck // best-effort stderr
		}
		return
	}
	currentSet := make(map[string]bool, len(currentRunning))
	for _, name := range currentRunning {
		currentSet[name] = true
	}
	if *prevPoolRunning != nil {
		for sn, info := range cr.poolDeathHandlers {
			if (*prevPoolRunning)[sn] && !currentSet[sn] {
				out, err := shellRunHook(info.Command, info.Dir, info.Env)
				if err != nil {
					fmt.Fprintf(cr.stderr, "on_death %s: %v\n", sn, err) //nolint:errcheck // best-effort stderr
				}
				// Surface only the DEFAULT hook's gc-recovery diagnostic for
				// a bd release it could not complete (the loop exits 0 even
				// when a bd write fails, so this is the only signal). A user
				// on_death override carries no marker and is left alone.
				if strings.Contains(out, config.RecoveryHookMarker) {
					fmt.Fprintf(cr.stderr, "on_death %s: %s\n", sn, strings.TrimSpace(out)) //nolint:errcheck // best-effort stderr
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
	if ctx.Err() != nil {
		return
	}
	traceTrigger := trigger
	traceDetail := "controller_tick"
	cr.reloadMu.Lock()
	hasActive := cr.activeReload != nil
	cr.reloadMu.Unlock()
	if hasActive {
		traceDetail = "manual_reload"
	}
	trace := cr.beginTraceCycle(traceTrigger, traceDetail, nil)
	// End the trace via defer so a panic recovered by safeTick still
	// closes the cycle (aborted). completion flips to Completed at the
	// normal end of the tick body below.
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "tick", "trigger": traceTrigger})
		}
	}()
	cr.reconcilePoolDeaths(prevPoolRunning)

	var manualReload *reloadRequest
	var manualReply reloadControlReply
	manualReloadCompleted := false
	manualReloadReplied := false
	dirtyCleared := false
	tickCompleted := false
	completeManualReload := func() {
		if manualReload == nil || manualReloadReplied {
			return
		}
		cr.sendReloadReply(manualReload.doneCh, manualReply)
		manualReloadReplied = true
		cr.clearActiveReloadIf(manualReload)
	}
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
		cr.clearActiveReloadIf(manualReload)
	}()
	configChanged := dirty.Swap(false)
	if configChanged {
		dirtyCleared = true
		source := reloadSourceWatch
		cr.reloadMu.Lock()
		if cr.activeReload != nil {
			source = reloadSourceManual
			manualReload = cr.activeReload
		}
		cr.reloadMu.Unlock()
		manualReply = cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, trace, source)
		if manualReload != nil {
			manualReloadCompleted = true
			// #3206 defense-in-depth: a manual reload's reply is already final
			// here unless soft-reload acceptance will amend it from
			// post-reconcile state. For every other manual reload, send the
			// reply now — before dispatchOrders and the session-reconcile
			// phases — so reload-reply latency does not scale with order count.
			// AUTO ticks (manualReload == nil) are unaffected and keep the
			// dispatch-before-reply ordering. The end-of-tick
			// completeManualReload() is idempotent, so soft Applied/NoChange
			// reloads still reply after applySoftReloadAcceptance. This
			// condition is the exact negation of the soft-acceptance guard
			// below.
			if !(manualReload.soft && //nolint:staticcheck // QF1001: explicit negation of the soft-acceptance guard below, kept for readability
				(manualReply.Outcome == reloadOutcomeApplied || manualReply.Outcome == reloadOutcomeNoChange)) {
				completeManualReload()
			}
		}
	}
	if ctx.Err() != nil {
		return
	}

	if !configChanged && cr.shouldSkipTickForFSPressure(trace, trigger) {
		cr.processConvergenceRequests(ctx)
		completion = TraceCompletionCompleted
		tickCompleted = true
		return
	}
	if configChanged {
		cr.resetFSPressureEpisode()
	}

	recordPhase := func(site TraceSiteCode, name string, start time.Time, fields map[string]any) {
		if trace != nil {
			trace.RecordControllerOperation(site, TraceReasonRetained, TraceOutcomeComplete, name, time.Since(start), fields)
		}
	}

	phaseStart := time.Now()
	cr.ensureManagedDoltPublishedForTick()
	recordPhase(TraceSiteControllerTickPhase, "managed_dolt_preflight", phaseStart, nil)
	if ctx.Err() != nil {
		return
	}

	// Order dispatch is intentionally before the expensive session reconcile
	// phases so due formulas are not starved by slow startup/config drift work,
	// but after the pressure gate and managed-Dolt preflight so skipped or
	// endpoint-repair ticks do not add tracking writes first.
	phaseStart = time.Now()
	cr.dispatchOrders(ctx, cityRoot)
	recordPhase(TraceSiteOrderDispatch, "dispatch_orders", phaseStart, nil)
	if ctx.Err() != nil {
		return
	}

	// Re-route ready work whose canonical pool route was lost or never written
	// (gc.run_target set, gc.routed_to empty), so the autoscaler — which keys on
	// gc.routed_to — sees it as demand without a manual `gc sling` (ga-n2d.4).
	//
	// The tick runs the DELTA half only: the beads the event feed named since
	// the last pass, and nothing else. A steady tick names nothing and reads no
	// store at all. The authoritative full scan this replaced was 185.3s of a
	// ~360s tick (ga-l7jdg) and now runs off-tick in the backstop lane.
	phaseStart = time.Now()
	routeReport := cr.recoverUnroutedWorkRoutesDelta()
	if trace != nil {
		// The convergence lane runs on a background goroutine, so the tick's
		// record is where its age becomes visible: `gc trace` answers "when did
		// the backstop last converge, and why was it due" without an operator
		// having to find the log line.
		routeFields := routeReport.fields()
		backstopAt, backstopReason, backstopRan := cr.routeRecoveryLaneOf().lastBackstop()
		addBackstopAgeFields(routeFields, backstopAt, backstopReason, backstopRan)
		trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, routeReport.outcome(),
			"recover_unrouted_work_routes", time.Since(phaseStart), routeFields)
	}
	if ctx.Err() != nil {
		return
	}

	phaseStart = time.Now()
	sessionBeads := cr.loadSessionBeadSnapshot()
	recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.initial", phaseStart, traceSessionSnapshotFields(sessionBeads))
	if trace != nil && sessionBeads != nil {
		trace.RecordSessionBaseline("", "", traceRecordPayload{
			"open_count": len(sessionBeads.OpenInfos()),
		})
		_ = trace.flushCurrentBatch(TraceDurabilityDurable)
	}

	// Session bead sync BEFORE reconciliation (one-tick state lag; see run()).
	// Post-reconcile sync was intentionally removed: the daemon's next tick
	// corrects bead state, and the pre-reconcile sync is sufficient for
	// the reconciler to read/write hashes during reconciliation.
	// Reap open session beads whose tmux session is dead before loading demand
	// so stale names cannot block desired-state computation (#742).
	phaseStart = time.Now()
	cleanupDeadRuntimeSessionCorpses(cr.sessionsBeadStore().Store, cr.rigBeadStores(), cr.cfg, sessionBeads, cr.sessionDrains, cr.sp, clock.Real{}, cr.stderr)
	recordPhase(TraceSiteControllerTickPhase, "cleanup_dead_runtime_session_corpses", phaseStart, nil)
	// Reap live runtimes still bound to a closed bead (e.g. a named-session
	// identity re-minted as a pool slot) so the name's current owner can rebind
	// it and attach lands on the right runtime.
	phaseStart = time.Now()
	reapRuntimesBoundToClosedBeads(cr.sessionsBeadStore().Store, sessionBeads, cr.sessionDrains, cr.sp, cr.stderr)
	recordPhase(TraceSiteControllerTickPhase, "reap_runtimes_bound_to_closed_beads", phaseStart, nil)
	phaseStart = time.Now()
	swept := sweepProcessTableOrphans(cr.sp, sessionBeads, cr.sessionsBeadStore().Store, cr.cityPath, cr.stderr)
	if swept > 0 {
		fmt.Fprintf(cr.stderr, "session reconciler: swept %d process-table orphan runtime(s)\n", swept) //nolint:errcheck
	}
	recordPhase(TraceSiteControllerTickPhase, "sweep_process_table_orphans", phaseStart, map[string]any{"reaped": swept})
	phaseStart = time.Now()
	reaped := reapStaleSessionBeads(cr.sessionsBeadStore().Store, cr.sp, cr.sessionDrains, clock.Real{}, cr.stderr)
	recordPhase(TraceSiteControllerTickPhase, "reap_stale_session_beads", phaseStart, map[string]any{"reaped": reaped})
	if reaped > 0 {
		phaseStart = time.Now()
		sessionBeads = cr.loadSessionBeadSnapshot()
		recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.after_reap", phaseStart, traceSessionSnapshotFields(sessionBeads))
	}
	reapEnabled := cr.cfg.Daemon.AutoReapClosedBeadWorktreesEnabled()
	reapDryRun := cr.cfg.Daemon.AutoReapClosedBeadWorktreesDryRunEnabled()
	if reapEnabled || reapDryRun {
		phaseStart = time.Now()
		// Cross-check the liveness gate against the current open-session set in
		// addition to the authoritative /proc cwd scan. Real removal supersedes
		// dry-run when both flags are set.
		liveSessionDirs := liveSessionWorktreeDirs(sessionBeads)
		report := reapClosedBeadWorktrees(cr.cityPath, cr.cfg, cr.rigBeadStores(), liveSessionDirs, !reapEnabled, cr.rec, cr.reapSkips, cr.stderr)
		recordPhase(TraceSiteControllerTickPhase, "reap_closed_bead_worktrees", phaseStart, map[string]any{
			"reaped":    len(report.Reaped),
			"protected": len(report.Protected),
			"dry_run":   !reapEnabled,
		})
		// Agent-home worktree cleanup performs real removals, so it runs only
		// when real reaping is enabled — never under dry-run.
		if reapEnabled {
			phaseStart = time.Now()
			agentHomesReset := cleanupClosedBeadAgentHomeWorktrees(cr.cityPath, cr.cfg, cr.rigBeadStores(), cr.stderr)
			recordPhase(TraceSiteControllerTickPhase, "cleanup_agent_home_worktrees", phaseStart, map[string]any{"reset": agentHomesReset})
		}
	}
	if ctx.Err() != nil {
		return
	}
	phaseStart = time.Now()
	finalizedDrainAckStops := 0
	if sessionBeads != nil {
		finalizedDrainAckStops = finalizeDrainAckStopPendingSessions(
			cr.cityPath,
			cr.cfg,
			cr.sp,
			cr.sessionsBeadStore(),
			cr.rigBeadStores(),
			sessionBeads.OpenInfos(),
			cr.dops,
			cr.sessionDrains,
			&cr.asyncStops,
			clock.Real{},
			cr.rec,
			cr.stderr,
		)
	}
	recordPhase(TraceSiteControllerTickPhase, "finalize_drain_ack_stop_pending", phaseStart, map[string]any{
		"finalized": finalizedDrainAckStops,
	})
	if finalizedDrainAckStops > 0 {
		phaseStart = time.Now()
		sessionBeads = cr.loadSessionBeadSnapshot()
		recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.after_drain_ack_stop_pending", phaseStart, traceSessionSnapshotFields(sessionBeads))
	}
	if ctx.Err() != nil {
		return
	}
	phaseStart = time.Now()
	demand := cr.loadDemandSnapshot(sessionBeads, trace, trigger, configChanged)
	recordPhase(TraceSiteDemandSnapshot, "load_demand_snapshot", phaseStart, map[string]any{
		"config_changed": configChanged,
		"trigger":        trigger,
	})
	result := demand.result
	phaseStart = time.Now()
	sessionBeads = cr.loadSessionBeadSnapshot()
	recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.after_demand", phaseStart, traceSessionSnapshotFields(sessionBeads))
	phaseStart = time.Now()
	result = cr.refreshDesiredState(result, sessionBeads)
	recordPhase(TraceSiteDesiredStateBuild, "refresh_desired_state.before_sync", phaseStart, traceDesiredStateFields(result))
	phaseStart = time.Now()
	_ = cr.syncBeadsAndUpdateIndex(result.State, sessionBeads)
	recordPhase(TraceSiteSessionSync, "sync_beads_and_update_index", phaseStart, traceDesiredStateFields(result))
	// Reload snapshot after sync so the reconciler sees metadata written
	// by syncBeadsAndUpdateIndex (e.g., configured_named_session/mode
	// stamped on adopted beads). The CachingStore has the updated data
	// from SetMetadataBatch write-through.
	phaseStart = time.Now()
	sessionBeads = cr.loadSessionBeadSnapshot()
	recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.after_sync", phaseStart, traceSessionSnapshotFields(sessionBeads))
	// Re-point external-message bindings at respawned sessions (and clear
	// bindings whose session is gone) now that replacement beads are visible.
	phaseStart = time.Now()
	reapStaleExtmsgBindings(ctx, cr.sessionsBeadStore(), time.Now(), cr.stderr)
	recordPhase(TraceSiteControllerTickPhase, "reap_stale_extmsg_bindings", phaseStart, nil)
	// Re-point group participants at respawned sessions and carry their
	// group-owned transcript membership; the participant side has no read-time
	// membership overlay, so this backstop is what converges binding-less
	// participants the binding reaper never sees.
	phaseStart = time.Now()
	reapStaleExtmsgParticipants(ctx, cr.sessionsBeadStore(), cr.stderr)
	recordPhase(TraceSiteControllerTickPhase, "reap_stale_extmsg_participants", phaseStart, nil)
	phaseStart = time.Now()
	result = cr.refreshDesiredState(result, sessionBeads)
	recordPhase(TraceSiteDesiredStateBuild, "refresh_desired_state.after_sync", phaseStart, traceDesiredStateFields(result))

	if manualReload != nil && manualReload.soft && manualReloadCompleted &&
		(manualReply.Outcome == reloadOutcomeApplied || manualReply.Outcome == reloadOutcomeNoChange) {
		phaseStart = time.Now()
		cr.applySoftReloadAcceptance(&manualReply, result.State, sessionBeads)
		recordPhase(TraceSiteConfigReload, "apply_soft_reload_acceptance", phaseStart, nil)
		phaseStart = time.Now()
		sessionBeads = cr.loadSessionBeadSnapshot()
		recordPhase(TraceSiteSessionSnapshot, "load_session_snapshot.after_soft_reload", phaseStart, traceSessionSnapshotFields(sessionBeads))
	}

	// Bead-driven reconciliation (requires bead store / drain tracker).
	if cr.sessionDrains != nil {
		phaseStart = time.Now()
		cr.beadReconcileTick(ctx, result, sessionBeads, trace, false)
		recordPhase(TraceSiteControllerTickPhase, "bead_reconcile_tick", phaseStart, traceDesiredStateFields(result))
	}
	// Graph stores intentionally do not emit bead.closed, so a step closed
	// between the durable write and the best-effort journal append would be a
	// permanent lifecycle gap. The tick repairs only the roots the journal named
	// since the last pass; the whole-corpus convergence sweep runs off-tick in
	// the background lane.
	//
	// The old gate here was `trigger == "patrol"`, which is not a cadence: under
	// overload every surviving ticker fire IS a patrol trigger, so the full pass
	// ran on every tick and cost 72.4s of it (ga-l7jdg).
	if cr.cs != nil {
		phaseStart = time.Now()
		completionsLane := cr.completionsLaneOf()
		namedRoots := completionsLane.takePending()
		emitted := cr.cs.reconcileExecutionCompletionsDelta(namedRoots)
		completionFields := map[string]any{
			"lane":        "delta",
			"named_roots": len(namedRoots),
			"emitted":     emitted,
		}
		sweptAt, sweptReason, swept := completionsLane.lastSweep()
		addBackstopAgeFields(completionFields, sweptAt, sweptReason, swept)
		recordPhase(TraceSiteControllerTickPhase, "reconcile_execution_completions", phaseStart, completionFields)
	}

	// Wisp GC: purge expired closed molecules. The molecule/wisp/workflow purge
	// arm routes through the typed graph-class store; the read-message retention
	// arm through the typed messaging-class store. Both collapse to the city store
	// today, so the GC is byte-identical.
	if graphStore := cr.graphBeadStore(); cr.wg != nil && graphStore.Store != nil && cr.wg.shouldRun(time.Now()) {
		phaseStart = time.Now()
		purged, gcErr := cr.wg.runGC(graphStore, cr.mailBeadStore(), time.Now())
		recordPhase(TraceSiteControllerTickPhase, "wisp_gc", phaseStart, map[string]any{"purged": purged})
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
		phaseStart = time.Now()
		cr.svc.Tick(ctx, time.Now())
		recordPhase(TraceSiteControllerTickPhase, "workspace_service_tick", phaseStart, nil)
	}

	// Chat session auto-suspend: suspend detached idle sessions.
	if idleTimeout := cr.cfg.ChatSessions.IdleTimeoutDuration(); idleTimeout > 0 {
		phaseStart = time.Now()
		autoSuspendChatSessions(cr.sessionsBeadStore().Store, cr.sp, idleTimeout, clock.Real{}, cr.stdout, cr.stderr)
		recordPhase(TraceSiteControllerTickPhase, "auto_suspend_chat_sessions", phaseStart, map[string]any{"idle_timeout_ms": idleTimeout.Milliseconds()})
	}

	// Drain queued convergence requests (CLI commands) BEFORE tick so
	// user commands (e.g. stop) take precedence over automated progression.
	phaseStart = time.Now()
	cr.processConvergenceRequests(ctx)
	recordPhase(TraceSiteControllerTickPhase, "process_convergence_requests", phaseStart, nil)

	// Convergence tick: process active convergence loops.
	phaseStart = time.Now()
	cr.convergenceTick(ctx)
	recordPhase(TraceSiteControllerTickPhase, "convergence_tick", phaseStart, nil)
	completeManualReload()
	completion = TraceCompletionCompleted
	tickCompleted = true
}

func (cr *CityRuntime) dispatchOrders(ctx context.Context, cityRoot string) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	if !cr.wispIndexMigrationApplied {
		cr.wispIndexMigrationApplied = true
		cr.applyWispQueryIndexes(ctx)
	}
	cr.rescanOrderDispatcherIfDue(ctx, cityRoot, now)
	cr.runOrderTrackingSweepWatchdog(now)
	cr.runOrderTrackingRetentionWatchdog(now)
	cr.runNudgeMailSweepWatchdog(now)
	if cr.od != nil {
		cr.od.dispatch(ctx, cityRoot, now)
	}
}

func (cr *CityRuntime) rescanOrderDispatcherIfDue(ctx context.Context, cityRoot string, now time.Time) {
	if !cr.orderRescanEnabled || cr.tomlPath == "" || strings.TrimSpace(cityRoot) == "" {
		return
	}
	if !cr.orderRescanLast.IsZero() && now.Sub(cr.orderRescanLast) < orderRescanInterval {
		return
	}
	if _, _, err := cr.rescanOrderDispatcher(ctx, cityRoot, cr.cfg, "gc patrol: order scan", now); err != nil {
		cr.orderRescanLast = now
		logDispatchError(cr.stderr, "%s: order rescan: %v", cr.logPrefix, err)
	}
}

// replaceOrderDispatcher installs next as the active order dispatcher, carrying
// warm last-run data, active gate-backoff state, and open-work suppression
// streaks from the outgoing dispatcher so a rebuild (reload or rescan) reuses
// them instead of cold-starting (#3201).
// Call after draining the outgoing dispatcher.
func (cr *CityRuntime) replaceOrderDispatcher(next orderDispatcher) {
	if prev, ok := cr.od.(*memoryOrderDispatcher); ok {
		if nextMem, ok := next.(*memoryOrderDispatcher); ok {
			nextMem.carryLastRunCacheFrom(prev)
			nextMem.carryGateBackoffFrom(prev, time.Now())
			nextMem.carryOpenWorkSuppressionFrom(prev)
		}
	}
	cr.od = next
}

func (cr *CityRuntime) rescanOrderDispatcher(ctx context.Context, cityRoot string, cfg *config.City, cmdName string, now time.Time) (bool, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, err := scanOrderSetSnapshotFS(fsys.OSFS{}, cityRoot, cfg, cr.stderr, cmdName)
	if err != nil {
		return false, "", err
	}
	cr.orderRescanLast = now
	if snapshot.Signature == cr.orderSetSignature {
		return false, "unchanged", nil
	}

	summary := orderSetChangeSummary(cr.orderSet, snapshot.Orders)
	if cr.od != nil {
		drainCtx, drainCancel := context.WithTimeout(ctx, reloadOrderDrainTimeout)
		cr.drainOutgoingOrderDispatcher(drainCtx, cr.od)
		drainCancel()
	}
	cr.replaceOrderDispatcher(buildOrderDispatcherFromOrderSet(cr.storageRoutes, cityRoot, cfg, snapshot.Orders, cr.rec, cr.stderr))
	cr.orderSet = snapshot.Orders
	cr.orderSetSignature = snapshot.Signature
	if summary != "unchanged" {
		fmt.Fprintf(cr.stderr, "%s: orders reloaded: %s\n", cr.logPrefix, summary) //nolint:errcheck // best-effort stderr
	}
	return true, summary, nil
}

func orderSetChangeSummary(oldOrders, newOrders []orders.Order) string {
	oldByName := make(map[string]orders.Order, len(oldOrders))
	for _, a := range oldOrders {
		oldByName[a.ScopedName()] = a
	}
	newByName := make(map[string]orders.Order, len(newOrders))
	for _, a := range newOrders {
		newByName[a.ScopedName()] = a
	}

	var added, removed, changed []string
	for name, next := range newByName {
		prev, ok := oldByName[name]
		if !ok {
			added = append(added, name)
			continue
		}
		if orderSetSignature([]orders.Order{prev}) != orderSetSignature([]orders.Order{next}) {
			changed = append(changed, name)
		}
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)

	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(changed) > 0 {
		parts = append(parts, "changed "+strings.Join(changed, ", "))
	}
	if len(parts) == 0 {
		return "unchanged"
	}
	return strings.Join(parts, "; ")
}

func (cr *CityRuntime) runOrderTrackingSweepWatchdog(now time.Time) {
	if !cr.orderSweepWatchdogLast.IsZero() && now.Sub(cr.orderSweepWatchdogLast) < orderTrackingSweepWatchdogInterval {
		return
	}
	cr.orderSweepWatchdogLast = now

	stores, _, closeOpened, storeErr := cr.orderTrackingSweepStores()
	defer closeOpened()
	if len(stores) == 0 {
		if storeErr != nil && cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "%s: order tracking sweep watchdog: %v\n", cr.logPrefix, storeErr) //nolint:errcheck // best-effort stderr
		}
		return
	}
	// Sweep stale tracking beads for ALL orders (nil filter), not just
	// order-tracking-sweep's own. The old narrow scope only swept the sweep
	// order's tracking so that order could bootstrap and clean the rest — a
	// single-point-of-failure: when slow reconciler cycles keep order-tracking-
	// sweep from firing, every order's tracking jams and no order fires (#2168).
	// The staleAfter cutoff still protects in-flight dispatches regardless of
	// which order they belong to, so a direct all-orders sweep is safe and
	// recovers the jam without depending on any single order being scheduled.
	// Closed-history retention is intentionally left to the maintenance exec
	// order or the gc order sweep-tracking CLI; the watchdog only recovers
	// stale open tracking beads.
	result, sweepErr := sweepStaleOrderTrackingAcrossStoresLimit(stores, nil, now, orderTrackingSweepWatchdogStaleAfter, nil, orderTrackingWatchdogMetadataInitiator, false, orderTrackingSweepCloseBudget)
	if err := errors.Join(storeErr, sweepErr); err != nil {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "%s: order tracking sweep watchdog: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
	}
	n := result.trackingClosed
	if n > 0 && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: order tracking sweep watchdog closed %d stale tracking bead(s)\n", cr.logPrefix, n) //nolint:errcheck // best-effort stderr
	}
}

// bulkDeleteMaxAge returns the maximum backup age allowed for bulk bead
// deletions. Configurable via GC_BACKUP_MAX_AGE_FOR_BULK_DELETE (integer
// seconds); defaults to 86400 s (24 h).
func bulkDeleteMaxAge(_ *config.City) time.Duration {
	if s := os.Getenv("GC_BACKUP_MAX_AGE_FOR_BULK_DELETE"); s != "" {
		var secs int
		if _, err := fmt.Sscanf(s, "%d", &secs); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 24 * time.Hour
}

// runOrderTrackingRetentionWatchdog deletes closed order-tracking beads that
// are past their TTL (defaulting to 7d) and beyond the retain-10 floor, at
// most once every orderTrackingRetentionWatchdogInterval. It deletes at most
// orderTrackingRetentionWatchdogDeleteBudget beads per invocation.
func (cr *CityRuntime) runOrderTrackingRetentionWatchdog(now time.Time) {
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() &&
		now.Sub(cr.orderTrackingRetentionWatchdogLast) < orderTrackingRetentionWatchdogInterval {
		return
	}
	cr.orderTrackingRetentionWatchdogLast = now

	// The cityPath guard is a test affordance: real controllers always set it,
	// so the backup-age check below always runs in production.
	if cr.cityPath != "" {
		if safe, reason := doctor.BulkDeleteSafe(cr.cityPath, cr.cfg, bulkDeleteMaxAge(cr.cfg), now); !safe {
			if cr.stderr != nil {
				fmt.Fprintf(cr.stderr, "%s: order-tracking retention watchdog: skipping bulk delete — %s\n", cr.logPrefix, reason) //nolint:errcheck // best-effort stderr
			}
			return
		}
	}

	stores, _, closeOpened, storeErr := cr.orderTrackingSweepStores()
	defer closeOpened()
	if len(stores) == 0 {
		if storeErr != nil && cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "%s: order-tracking retention watchdog: %v\n", cr.logPrefix, storeErr) //nolint:errcheck // best-effort stderr
		}
		return
	}

	policy := orderTrackingRetentionPolicyForConfig(cr.cfg)
	deleted, sweepErr := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		stores, now, policy, nil, orderTrackingRetentionWatchdogDeleteBudget)
	if err := errors.Join(storeErr, sweepErr); err != nil && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: order-tracking retention watchdog: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	if deleted > 0 && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: order-tracking retention watchdog: pruned %d closed bead(s)\n", cr.logPrefix, deleted) //nolint:errcheck // best-effort stderr
	}
}

const (
	// orderTrackingRetentionStartupWarnThreshold is the minimum number of closed
	// order-tracking beads a city holds that triggers a startup advisory.
	// The watchdog prunes automatically; this warning surfaces cities that have
	// accumulated a visible backlog before the first watchdog cycle completes.
	orderTrackingRetentionStartupWarnThreshold = 100
	// orderTrackingRetentionStartupListLimit caps the startup List query so the
	// advisory does not scan unbounded closed-bead history on large stores.
	orderTrackingRetentionStartupListLimit = 1001
)

// warnIfClosedOrderTrackingBacklogLarge writes one advisory line to stderr when
// the city's stores together hold more than
// orderTrackingRetentionStartupWarnThreshold closed order-tracking beads. It is
// best-effort: a nil store or a List error is skipped so startup is never
// blocked.
//
// The count is the total across the stores, not a per-store test, because the
// backlog is one city's. A split city holds its pre-cutover half in the work
// ledger and everything since in the binding; testing each half separately
// stays silent on a city with twice the backlog the threshold is set at.
func warnIfClosedOrderTrackingBacklogLarge(stores []beads.Store, stderr io.Writer) {
	total := 0
	truncated := false
	for _, store := range stores {
		if store == nil {
			continue
		}
		closed, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
			Status:   "closed",
			Label:    labelOrderTracking,
			TierMode: beads.TierBoth,
			Limit:    orderTrackingRetentionStartupListLimit,
		})
		if err != nil {
			continue
		}
		total += len(closed)
		if len(closed) >= orderTrackingRetentionStartupListLimit {
			truncated = true
		}
	}
	if total <= orderTrackingRetentionStartupWarnThreshold {
		return
	}
	countStr := fmt.Sprintf("%d", total)
	if truncated {
		countStr = "≥" + countStr
	}
	fmt.Fprintf(stderr, "gc start: %s closed order-tracking beads detected — retention watchdog will prune automatically (7d TTL default; configure: [beads.policies.order_tracking].delete_after_close). For immediate cleanup: gc order sweep-tracking\n", countStr) //nolint:errcheck // best-effort stderr
}

func (cr *CityRuntime) runNudgeMailSweepWatchdog(now time.Time) {
	if !cr.nudgeMailSweepWatchdogLast.IsZero() && now.Sub(cr.nudgeMailSweepWatchdogLast) < nudgeMailSweepWatchdogInterval {
		return
	}
	cr.nudgeMailSweepWatchdogLast = now

	// The nudge phase routes through the typed nudges accessor; the mail phase
	// through the typed messaging accessor. Both collapse to the city store today,
	// so the sweep is byte-identical.
	nudgeStore := cr.nudgesBeadStore()
	mailStore := cr.mailBeadStore()
	if nudgeStore.Store == nil || mailStore.Store == nil {
		return
	}
	// Load nudge state to protect live nudge IDs. A missing state file is not an
	// error (LoadState returns empty state), so any error here is a real
	// read/parse failure: fail closed and skip this sweep rather than sweeping
	// without live-ID protection, which could close beads for in-flight nudges.
	nudgeState, stateErr := nudgequeue.LoadState(cr.cityPath)
	if stateErr != nil {
		if cr.stderr != nil {
			fmt.Fprintf(cr.stderr, "%s: nudge-mail-sweep watchdog: load nudge state: %v\n", cr.logPrefix, stateErr) //nolint:errcheck // best-effort stderr
		}
		return
	}
	statePtr := &nudgeState

	mailTTL := nudgeMailSweepMailTTLForConfig(cr.cfg, cr.stderr)
	result, sweepErr := sweepStaleNudgeMail(nudgeStore, mailStore, statePtr, now, nudgeMailSweepDefaultNudgeTTL, mailTTL, nudgeMailSweepWatchdogCloseBudget)
	if sweepErr != nil && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: nudge-mail-sweep watchdog: %v\n", cr.logPrefix, sweepErr) //nolint:errcheck // best-effort stderr
	}
	total := result.NudgeClosed + result.MailClosed
	if total > 0 && cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: nudge-mail-sweep watchdog closed %d nudge bead(s), %d mail bead(s)\n", cr.logPrefix, result.NudgeClosed, result.MailClosed) //nolint:errcheck // best-effort stderr
	}
}

func (cr *CityRuntime) orderTrackingSweepStores() ([]beads.Store, []orderTrackingSweepTarget, func(), error) { //nolint:unparam // targets slice returned for callers that need sweep scope metadata; current call sites discard it
	targets := orderTrackingSweepTargetsForConfig(cr.cityPath, cr.cfg)
	rigStores := cr.rigBeadStores()
	var freshlyOpened []beads.Store
	stores, err := orderTrackingSweepStoresFromTargets(targets, func(sweepTarget orderTrackingSweepTarget) (beads.Store, error) {
		var store beads.Store
		switch sweepTarget.target.ScopeKind {
		case "city":
			store = cr.cityBeadStore()
		case "rig":
			store = rigStores[sweepTarget.target.RigName]
		}
		if store == nil {
			fresh, openErr := newCityRuntimeOpenSweepStore(sweepTarget.target.ScopeRoot, cr.cityPath)
			if openErr == nil {
				freshlyOpened = append(freshlyOpened, fresh)
			}
			return fresh, openErr
		}
		return store, nil
	})
	// The tracking beads this controller writes are born in the orders binding,
	// so both watchdogs — stale-close and closed-retention — have to sweep it.
	// Without it a split city's open tracking beads are never recovered and its
	// closed ones are never pruned: the stale-close jam (#2168) comes back with
	// no watchdog able to see it. The binding comes from the routes this process
	// opened at boot, never a second resolution, so nothing here is closed by
	// closeOpened — the runtime owns that handle for its whole life.
	stores = appendOrdersSweepStore(stores, cr.relocatedOrdersStore())
	closeOpened := func() {
		for _, s := range freshlyOpened {
			_ = closeBeadStoreHandle(s) //nolint:errcheck // best-effort
		}
	}
	return stores, targets, closeOpened, err
}

func (cr *CityRuntime) handleReloadRequest(req *reloadRequest) {
	if req == nil {
		return
	}
	req.started = time.Now()
	var stale *reloadRequest
	cr.reloadMu.Lock()
	if existing := cr.activeReload; existing != nil {
		if reloadActiveTTL > 0 && !existing.started.IsZero() &&
			time.Since(existing.started) >= reloadActiveTTL {
			stale = existing
			cr.activeReload = nil
		} else {
			cr.reloadMu.Unlock()
			req.acceptedCh <- reloadControlReply{
				Outcome: reloadOutcomeBusy,
				Message: "Reload request could not be accepted because another reload is already in progress.",
			}
			return
		}
	}
	cr.activeReload = req
	if cr.configDirty == nil {
		cr.configDirty = &atomic.Bool{}
	}
	cr.configDirty.Store(true)
	cr.reloadMu.Unlock()
	if stale != nil {
		cr.sendReloadReply(stale.doneCh, reloadControlReply{
			Outcome: reloadOutcomeTimeout,
			Error: fmt.Sprintf(
				"Previous reload exceeded the %s active-reload TTL without completing; controller force-cleared the stuck reload slot.",
				reloadActiveTTL,
			),
		})
	}
	select {
	case cr.pokeCh <- struct{}{}:
	default:
	}
	req.acceptedCh <- reloadControlReply{
		Outcome: reloadOutcomeAccepted,
		Message: "Reload requested.",
	}
}

// clearActiveReloadIf clears cr.activeReload only when it still points
// to req. Used by the reconciler tick when it finishes (or panics
// through) a reload it was handling: if handleReloadRequest already
// force-cleared the slot via the activeReload TTL and accepted a newer
// request, this tick must not stomp on the newer request's pointer.
func (cr *CityRuntime) clearActiveReloadIf(req *reloadRequest) bool {
	if req == nil {
		return false
	}
	cr.reloadMu.Lock()
	defer cr.reloadMu.Unlock()
	if cr.activeReload != req {
		return false
	}
	cr.activeReload = nil
	return true
}

func (cr *CityRuntime) failActiveReload(message string) {
	cr.reloadMu.Lock()
	req := cr.activeReload
	cr.activeReload = nil
	cr.reloadMu.Unlock()
	if req == nil {
		return
	}
	cr.sendReloadReply(req.doneCh, reloadControlReply{
		Outcome: reloadOutcomeFailed,
		Error:   message,
	})
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
		ordersChanged, orderSummary, orderErr := cr.rescanOrderDispatcher(ctx, cityRoot, result.Cfg, "gc reload: order scan", time.Now())
		if orderErr != nil {
			err := fmt.Errorf("order reload: %w", orderErr)
			fmt.Fprintf(cr.stderr, "%s: %v (keeping old orders)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
			telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeFailed), len(warnings), err)
			if trace != nil {
				trace.RecordConfigReload(cr.configRev, result.Revision, TraceOutcomeFailed, source, nil, nil, false, warnings, err)
			}
			return reloadControlReply{
				Outcome:  reloadOutcomeFailed,
				Error:    err.Error(),
				Revision: result.Revision,
				Warnings: warnings,
			}
		}
		if cr.cs != nil && cr.cs.storeMetadataChanged(result.Cfg) {
			cr.cs.update(result.Cfg, cr.sp)
			message := fmt.Sprintf("Config reloaded: bead store metadata changed (rev %s)", shortRev(result.Revision))
			if ordersChanged {
				message = fmt.Sprintf("Config reloaded: bead store metadata changed; orders reloaded: %s (rev %s)", orderSummary, shortRev(result.Revision))
			}
			fmt.Fprintln(cr.stdout, message) //nolint:errcheck // best-effort stdout
			if trace != nil {
				trace.RecordConfigReload(cr.configRev, result.Revision, TraceOutcomeApplied, source, nil, nil, false, warnings, nil)
			}
			telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeApplied), len(warnings), nil)
			return reloadControlReply{
				Outcome:  reloadOutcomeApplied,
				Message:  message,
				Revision: result.Revision,
				Warnings: warnings,
			}
		}
		if ordersChanged {
			message := fmt.Sprintf("Orders reloaded: %s (config rev %s)", orderSummary, shortRev(result.Revision))
			fmt.Fprintln(cr.stdout, message) //nolint:errcheck // best-effort stdout
			if trace != nil {
				trace.RecordConfigReload(cr.configRev, result.Revision, TraceOutcomeApplied, source, nil, nil, false, warnings, nil)
			}
			telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeApplied), len(warnings), nil)
			return reloadControlReply{
				Outcome:  reloadOutcomeApplied,
				Message:  message,
				Revision: result.Revision,
				Warnings: warnings,
			}
		}
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

	// [storage] is decided once, at boot, and the engine it selected is open for
	// the life of the process (storage_boot.go). Applying a config that names a
	// different arrangement would leave every class resolver pointing at a
	// binding this process never opened, so the whole reload is refused here —
	// before a provider is rebuilt or a bead lifecycle is started — and the
	// booted configuration stays in force until an operator restarts the city.
	if config.StorageReloadRequiresRestart(cr.cfg, nextCfg) {
		err := fmt.Errorf("config reload: [storage] changed, and live storage handles cannot be swapped; restart this city to apply it (keeping old config)")
		fmt.Fprintf(cr.stderr, "%s: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, result.Revision, string(source), string(reloadOutcomeFailed), len(warnings), err)
		if trace != nil {
			trace.RecordConfigReload(oldRevision, result.Revision, TraceOutcomeFailed, source, nil, nil, false, warnings, err)
		}
		return reloadControlReply{
			Outcome:  reloadOutcomeFailed,
			Error:    err.Error(),
			Revision: result.Revision,
			Warnings: warnings,
		}
	}
	nextSp := cr.sp
	nextDops := cr.dops
	providerChanged := false

	// Detect session provider change. A pack-declared runtime binds its
	// command into the provider at construction time, so a changed (or
	// added/removed) declaration behind an unchanged selection name also
	// requires a rebuild — otherwise session ops keep forking the old
	// executable until a controller restart.
	newProviderName := nextCfg.Session.Provider
	pendingProviderName := *lastProviderName
	if v := os.Getenv("GC_SESSION"); v != "" {
		newProviderName = v
	}
	if newProviderName != *lastProviderName || packRuntimeDeclarationChanged(cr.cfg, nextCfg, newProviderName) {
		newSp, spErr := newSessionProviderForCityByName(nextCfg, newProviderName, nextCfg.Session, cr.cityName, cr.cityPath)
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
	for _, w := range config.ReservedPrefixWarnings(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)) {
		appendWarning(fmt.Sprintf("config reload: %s", w))
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
		providerSwapSummary := fmt.Sprintf("%s → %s", displayProviderName(*lastProviderName), displayProviderName(pendingProviderName))
		if pendingProviderName == *lastProviderName {
			providerSwapSummary = fmt.Sprintf("%s runtime declaration changed", displayProviderName(pendingProviderName))
		}
		if len(running) > 0 {
			fmt.Fprintf(cr.stdout, "Provider changed (%s), stopping %d agent(s)...\n", //nolint:errcheck
				providerSwapSummary, len(running))
			gracefulStopAll(running, cr.sp, nextCfg.Daemon.ShutdownTimeoutDuration(), cr.rec, cr.cfg, cr.sessionsBeadStore(), cr.stdout, cr.stderr)
		}
		cr.rec.Record(events.Event{
			Type:    events.ProviderSwapped,
			Actor:   "gc",
			Message: providerSwapSummary,
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
	cr.mat = buildMaxSessionAgeTracker(nextCfg, cr.cityName, nextSp)
	cr.adt = buildAssignedWorkDeferTracker(nextCfg, cr.cityName, nextSp)

	cr.wg = newWispGCForConfig(nextCfg)

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
	nextOD, orderSnapshot := buildOrderDispatcherWithSnapshot(cr.storageRoutes, cityRoot, nextCfg, cr.rec, cr.stderr, "gc reload: order scan")
	orderSummary := orderSetChangeSummary(cr.orderSet, orderSnapshot.Orders)
	cr.replaceOrderDispatcher(nextOD)
	cr.orderSet = orderSnapshot.Orders
	cr.orderSetSignature = orderSnapshot.Signature
	cr.orderRescanLast = time.Now()
	if orderSummary != "unchanged" {
		fmt.Fprintf(cr.stderr, "%s: orders reloaded: %s\n", cr.logPrefix, orderSummary) //nolint:errcheck // best-effort stderr
	}

	cr.serviceStateMu.Lock()
	cr.cfg = nextCfg
	cr.sp = nextSp
	cr.dops = nextDops
	cr.serviceStateMu.Unlock()
	cr.demandSnapshot = nil

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

	// Rebuild convergence scopes against the reloaded config so rigs added,
	// removed, or rebound by this reload are honored live (#2403). New
	// scopes repopulate their active index via the needsStartupReconcile
	// path on the next tick.
	cr.rebuildConvergenceHandler()

	// Ensure drain tracker and provider-health gate are initialized when bead store becomes available.
	if cr.cityBeadStore() != nil && cr.tomlPath != "" && cr.sessionDrains == nil {
		cr.sessionDrains = newDrainTracker()
		cr.providerHealthGate = newProviderHealthGate()
	}
	cr.configRev = result.Revision
	cr.watchTargets = config.WatchTargets(result.Prov, nextCfg, cityRoot)
	cr.restartConfigWatcher()
	if trace != nil {
		trace.configRevision = result.Revision
		trace.syncArms(time.Now().UTC(), nextCfg)
	}

	// Stage-1 skill materialization must also run here, not just at city
	// start: its only other call site (prepareCityForSupervisor) is reached
	// exclusively for cities not yet running (reconcileCities' toStart
	// filter), so a skill added/removed via a live config reload was
	// advertised in the catalog and prompt appendix but never materialized
	// into (or pruned from) the vendor sink until a full supervisor
	// restart (#3459). Idempotent — a converged pass creates nothing new;
	// per-agent errors are logged to stderr internally and never abort
	// the reload.
	//
	// Match the start/supervisor invariant: validate skill collisions
	// before materializing so a colliding live-reload config can't write
	// half-written/conflicting sinks. The config is already applied and
	// passed agent validation; on collision we keep the previously
	// materialized sink in place (supervisor semantics) and surface a
	// warning rather than aborting the reload.
	if err := checkSkillCollisions(nextCfg, cr.cityPath); err != nil {
		appendWarning(fmt.Sprintf("skill collision; skipping materialization: %v", err))
	} else {
		_ = runStage1SkillMaterialization(cr.cityPath, nextCfg, cr.stderr)
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

func (cr *CityRuntime) applySoftReloadAcceptance(
	reply *reloadControlReply,
	desired map[string]TemplateParams,
	sessionBeads *sessionBeadSnapshot,
) {
	if reply == nil {
		return
	}
	result := acceptConfigDriftAcrossSessions(sessionFrontDoor(cr.sessionsBeadStore().Store), desired, sessionBeads, cr.sp, cr.sessionDrains, cr.stderr)
	accepted := result.Updated
	reply.AcceptedDriftCount = &accepted
	for _, warning := range result.warnings() {
		reply.Warnings = append(reply.Warnings, warning)
		fmt.Fprintf(cr.stderr, "%s: warning: %s\n", cr.logPrefix, warning) //nolint:errcheck // best-effort stderr
	}
	fmt.Fprintf(cr.stdout, "%s: soft reload: accepted config drift on %d session(s)\n", cr.logPrefix, result.Updated) //nolint:errcheck // best-effort stdout
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

// beadReconcileTick runs one bead-driven reconciliation pass. bootReconcile is
// true only for the synchronous pass on the startup path: that pass must flip
// readiness quickly, so it skips the undesired-pool-session sweep (a heavy
// candidate × store × status × identifier bd-read fan-out that, serialized on
// the readiness path, can exceed the startup watchdog on a heavy-session city —
// gastownhall/gascity#3288) and the usage lane's live transcript sweep (bounded
// per-session file discovery and reads across every awake session at once). The
// first steady-state tick performs both.
func (cr *CityRuntime) beadReconcileTick(ctx context.Context, result DesiredStateResult, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle, bootReconcile bool) {
	desiredState := result.State
	store := cr.cityBeadStore()
	if store == nil {
		return
	}
	// Session-class ops (pool-session sweep, wait-wake state, reconcile, and the
	// orphan-release liveness read) route through the typed session store
	// (gastownhall/gascity#3773). On a city whose [storage.classes] relocates
	// sessions this is a different store than the work store, so the two must
	// not be used interchangeably (ga-g3pf0).
	sessStore := cr.sessionsBeadStore()
	recordPhase := func(site TraceSiteCode, name string, start time.Time, fields map[string]any) {
		if trace != nil {
			trace.RecordControllerOperation(site, TraceReasonRetained, TraceOutcomeComplete, name, time.Since(start), fields)
		}
	}

	if sessionBeads == nil {
		var sessionQueryPartial bool
		phaseStart := time.Now()
		sessionBeads, sessionQueryPartial = cr.loadSessionBeadSnapshotWithPartial()
		recordPhase(TraceSiteSessionSnapshot, "bead_reconcile.load_session_snapshot", phaseStart, traceSessionSnapshotFields(sessionBeads))
		result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	}
	// Emit any due compute usage facts by reusing the open-session snapshot this
	// tick already loaded, rather than issuing a second redundant store scan. The
	// boot pass covers the whole fleet at once on the readiness path, so it takes
	// only the marker-gated terminal lane and leaves the fleet-proportional live
	// lane to the first steady-state tick.
	cr.emitDueComputeFacts(ctx, sessionBeads.OpenInfos(), bootReconcile)
	// Historical sidecar reconciliation is supervisor-owned and asynchronous.
	// Keep it off the synchronous boot/readiness pass; the first steady-state
	// patrol starts one bounded-batch background pass without delaying city
	// availability or later controller ticks.
	if !bootReconcile {
		cr.startHistoricalTranscriptMetaReconcile(ctx)
	}
	rigStores := cr.rigBeadStores()
	assignedWorkBeads := result.AssignedWorkBeads
	assignedWorkStoreRefs := result.AssignedWorkStoreRefs
	phaseStart := time.Now()
	released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, sessStore, cr.cfg, cr.cityPath, sessionBeads.OpenInfos(), result, rigStores)
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.release_orphaned_pool_assignments", phaseStart, map[string]any{
		"released_count": len(released),
	})
	if len(released) > 0 {
		for _, r := range released {
			fmt.Fprintf(cr.stderr, "released orphaned pool work: %s\n", r.ID) //nolint:errcheck
		}
		// Turn the otherwise-silent reopen into an observable signal. The reopen
		// (clear dead assignee, reset in_progress→open) already ran above and is
		// gated on confirmed non-liveness; emit the event BEFORE the snapshot
		// filter so the dead assignee and route can still be read off the beads.
		emitDeadAssigneeReopenedEvents(cr.rec, assignedWorkBeads, released, time.Now())
		assignedWorkBeads, assignedWorkStoreRefs = filterReleasedAssignedWorkSnapshot(assignedWorkBeads, assignedWorkStoreRefs, released)
	}
	// Detached handoff orphans: the tick repairs only the beads the journal named
	// since the last pass. The whole-corpus scan this replaced was a live
	// open-corpus read of the city ledger and every rig, serially, on every tick
	// — 180.8s of a 373s tick with restored_count=0 on every one (ga-l7jdg). It
	// now runs off-tick on the convergence lane's cadence.
	phaseStart = time.Now()
	detachedReport := cr.sweepDetachedHandoffOrphansDelta()
	detachedFields := detachedReport.fields()
	sweptAt, sweptReason, swept := cr.detachedOrphanLaneOf().lastBackstop()
	addBackstopAgeFields(detachedFields, sweptAt, sweptReason, swept)
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.sweep_detached_handoff_orphans", phaseStart, detachedFields)
	// Squatter guard (gastownhall/gascity#2930): a foreign Dolt that has bound
	// this city's managed port returns zero demand, indistinguishable from a
	// genuinely-idle fleet — and would drain every running pool. This runs on
	// the steady-state tick (not just startup), before the sweep and the
	// singleton reconcile below. The ctx-bounded @@datadir probe is paid only
	// when the sweep would actually close a running pool session this tick (a
	// scale-down event), so a steady warm fleet pays nothing; a confirmed
	// data-dir mismatch marks the tick partial so the existing hold suppresses
	// the drain. Fail-open in every other case.
	if storeIdentityHold(ctx, cr.cityPath, poolSweepWouldDrain(sessionBeads, result.State, cr.cfg), cr.stderr) {
		fmt.Fprintf(cr.stderr, "%s: managed dolt serves an unexpected data-dir (squatter on the managed port?); holding pools this tick — see gastownhall/gascity#2930\n", cr.logPrefix) //nolint:errcheck // best-effort stderr
		result.StoreQueryPartial = true
	}
	// poolDesired determines how many sessions should be AWAKE. Uses the
	// same scale_check counts that buildDesiredState already computed (no
	// duplicate shell-outs). Resume tier from cross-referenced assigned
	// work beads + new tier from scale_check + min fill.
	poolDesired := result.PoolDesiredCounts
	if poolDesired == nil {
		phaseStart = time.Now()
		openInfos := sessionBeads.OpenInfos()
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cr.cfg, cr.cityPath, store, openInfos, assignedWorkBeads, assignedWorkStoreRefs)
		poolDecisionTime := time.Now()
		poolDesired = retainScaleCheckPartialPoolDesired(
			cr.cfg,
			PoolDesiredCounts(ComputePoolDesiredStatesTracedAt(
				cr.cfg, poolWorkBeads, openInfos, result.ScaleCheckCounts, poolDecisionTime, trace)),
			sessionBeads,
			effectivePoolPartialRetentionTemplates(result),
		)
		recordPhase(TraceSitePoolDemandCompute, "bead_reconcile.compute_pool_desired", phaseStart, map[string]any{
			"pool_work_bead_count": len(poolWorkBeads),
			"pool_desired_count":   len(poolDesired),
		})
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
	// #3288: defer the undesired-pool-session sweep on the boot tick. The sweep
	// probes each sweepable candidate against the city store + N rig stores × 2
	// statuses × identifiers, each a `bd` read (listWispsTier fires two
	// subprocesses); serialized on the synchronous readiness path that fan-out
	// can exceed the startup watchdog on a heavy-session city and hang boot.
	// Skipping it on boot lets readiness flip without waiting on those reads; the
	// first steady-state tick (fired moments later by the startup poke / patrol
	// ticker) performs the identical sweep. Safe to defer: the sweep only closes
	// not-running, unassigned ephemeral pool-session beads and fails closed on
	// query error, so a few seconds of staleness cannot wrongly close live work.
	if bootReconcile {
		recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.sweep_undesired_pool_sessions.deferred_on_boot", time.Now(), traceSessionSnapshotFields(sessionBeads))
	} else {
		phaseStart = time.Now()
		if sweepUndesiredPoolSessionBeads(
			cr.cityPath,
			sessStore,
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
		recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.sweep_undesired_pool_sessions", phaseStart, traceSessionSnapshotFields(sessionBeads))
	}
	openInfos := sessionBeads.OpenInfos()

	// Use cr.cityName consistently — it's the authoritative runtime name.
	cityName := cr.cityName

	phaseStart = time.Now()
	cfgNames := configuredSessionNamesWithSnapshot(cr.cfg, cityName, sessionBeads)

	readyWaitSet, err := prepareWaitWakeStateWithSnapshot(sessionpkg.NewStore(sessStore), newWaitDependencyStoreSet(store, rigStores, cr.graphBeadStore()), cr.nudgesBeadStore(), time.Now(), sessionBeads)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: preparing waits: %v\n", cr.logPrefix, err) //nolint:errcheck
		readyWaitSet = nil
	}
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.prepare_wait_wake_state", phaseStart, map[string]any{
		"configured_name_count": len(cfgNames),
		"ready_wait_count":      len(readyWaitSet),
	})

	// Controller wake demand comes from assigned-work scans and scale_check.
	// work_query remains the agent-side gc hook claim path; running every
	// work_query here can block assigned-work resumes behind unrelated probes.
	workSet := make(map[string]bool)
	traceWorkRequested := traceWorkRequestedByTemplate(result.ScaleCheckCounts, result.NamedSessionDemand, workSet, cr.cfg)
	cr.recordReconcileTraceInputs(trace, openInfos, desiredState, poolDesired, workSet, traceWorkRequested, readyWaitSet, result, recordPhase)

	phaseStart = time.Now()
	awakeAssignedWorkBeads, awakeAssignedStoreRefs := filterAssignedWorkBeadsForSessionWake(cr.cfg, cr.cityPath, store, openInfos, assignedWorkBeads, assignedWorkStoreRefs)
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.filter_assigned_work_for_wake", phaseStart, map[string]any{
		"assigned_work_bead_count":       len(assignedWorkBeads),
		"awake_assigned_work_bead_count": len(awakeAssignedWorkBeads),
	})
	phaseStart = time.Now()
	reconcileStartOptions := []startExecutionOption{
		withAsyncStartExecution(),
		withAsyncStartFollowUp(cr.requestAsyncStartFollowUpTick),
		withAsyncStartLimiter(cr.ensureAsyncStartLimiter()),
		withAsyncStartTracker(&cr.asyncStarts),
		withAsyncDrainAckStopTracker(&cr.asyncStops),
		withMaxSessionAgeTracker(cr.mat),
		withAssignedWorkDeferTracker(cr.adt),
		withReadyAssignedFlags(readyAssignedFlagsForBeads(result.ReadyAssigned, awakeAssignedWorkBeads, awakeAssignedStoreRefs)),
		// Warm-bind claim nudge: deliver a pool slot's claim instruction to an
		// already-running, idle slot that had on-demand work bound to it after it
		// last Started (bindPoolSessionTriggerBead), which cold Start's nudge cannot
		// cover. The probe resolves the bound trigger from its owning store (via the
		// cached rig stores) to confirm it is still unclaimed; the delivery + the
		// once-per-binding marker live in startPreparedStartCandidate's warm-reuse
		// branch. Provider-agnostic (not CanReportActivity-gated): on herdr this is
		// the only closer of the warm-bind gap; on tmux it is the fast primary nudge
		// ahead of the idle-timeout relaunch backstop, deduped by the marker + the
		// unclaimed-trigger gate.
		withWarmClaimProbe(buildWarmClaimTriggerProbe(store, rigStores)),
	}
	if bootReconcile {
		// #3288: skip the per-session orphan/failed-create session-bead closes on
		// the boot tick so readiness does not wait on their wisp-tier work-probe
		// fan-out; the first steady-state tick performs them.
		reconcileStartOptions = append(reconcileStartOptions, withDeferSessionClosesOnBoot())
	}
	reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cr.cityPath, sessionBeads.OpenForReconcile(), sessionBeads, desiredState, cfgNames, cr.cfg, cr.sp, sessStore,
		cr.dops,
		awakeAssignedWorkBeads, rigStores, readyWaitSet, cr.sessionDrains, cr.providerHealthGate,
		poolDesired,
		result.NamedSessionDemand,
		result.NamedSessionRoutedDemand,
		result.snapshotQueryPartial(),
		workSet, cityName,
		cr.it, clock.Real{}, cr.rec, cr.cfg.Session.StartupTimeoutDuration(),
		cr.cfg.Daemon.DriftDrainTimeoutDuration(),
		cr.stdout, cr.stderr, trace,
		reconcileStartOptions...,
	)
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.reconcile_sessions", phaseStart, map[string]any{
		"open_session_count":             len(openInfos),
		"desired_session_count":          len(desiredState),
		"awake_assigned_work_bead_count": len(awakeAssignedWorkBeads),
	})
	cr.requestDeferredDrainFollowUpTick()
	// The RESULTS trace reads the post-tick carrier: the reconciler's
	// WriteBackReconcileInfos folded its post-tick Info snapshot onto sessionBeads,
	// so sessionBeads.OpenInfos() now carries the tick's in-memory heals/retires/
	// closes. This restores the post-tick observation the old in-place raw-bead
	// mutation provided (a dedup-retired loser under its retired session_name, a
	// healed-then-closed session under its post-heal/closed state) — MORE accurate
	// than a store reload, which excludes closed history.
	cr.recordReconcileTraceResults(trace, sessionBeads.OpenInfos(), recordPhase)
	// Post-reconcile session snapshot feeds the wait-nudge dispatch; read it from
	// the typed session store (controller-parity fix, identity today).
	dispatchSessionBeads, err := loadSessionBeadSnapshot(sessStore.Store)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: loading post-reconcile session snapshot: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
	phaseStart = time.Now()
	if err == nil {
		if nudgeErr := dispatchReadyWaitNudgesWithSnapshot(cr.cityPath, cr.cfg, sessionpkg.NewStore(sessStore), cr.nudgesBeadStore(), time.Now(), dispatchSessionBeads); nudgeErr != nil {
			fmt.Fprintf(cr.stderr, "%s: dispatching wait nudges: %v\n", cr.logPrefix, nudgeErr) //nolint:errcheck
		}
	}
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.dispatch_wait_nudges", phaseStart, traceSessionSnapshotFields(dispatchSessionBeads))
	// Patrol-tick fallback for the supervisor nudge dispatcher: ensures
	// queued items get delivered even if the wake socket missed the
	// enqueue (process race during supervisor restart, listener crash).
	phaseStart = time.Now()
	cr.nudgeDispatchTick(ctx)
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.nudge_dispatch_tick", phaseStart, nil)

	// Idle recovery: re-nudge pool slots that are running but never claimed
	// either their assigned/ready-routed trigger bead or the one ready graph-v2
	// successor preassigned after a completed step. Runs for every runtime, not
	// just herdr.
	// tmux's relaunch/respawn path only heals a session that DIED; it does
	// nothing for a session that is alive but idle at its prompt on a trigger
	// bead it never began (a warm slot resumed onto work whose submit-CR was
	// swallowed, or that survived a `gc restart` and was never re-Started).
	// Activity reporting lets the controller SEE such a slot as alive but never
	// delivers the claim nudge, so tmux has no demand-driven wake for it. The
	// backstop is churn-free by construction for either runtime: it keys on the
	// trigger bead still being open (the instant a pool slot claims, the bead
	// flips to in_progress and stops matching), persists its bounded
	// observe→nudge→backoff state on the session bead, and never spams a tick.
	// See nudgeStalledPoolClaims for the full invariant.
	phaseStart = time.Now()
	// The idle-claim nudge lane reads idle-claim marker keys that session.Info
	// does not project, so it needs raw beads. Now that the snapshot no longer
	// holds a raw half, this lane does its own loadSessionBeads edge read every
	// tick (main passes its in-memory raw snapshot slice here; this tree pays a
	// store list instead — kept unconditional for exact parity with #1129's
	// per-tick marker-clear semantics).
	if stalledPoolBeads, err := loadSessionBeads(sessStore.Store); err != nil {
		fmt.Fprintf(cr.stderr, "%s: loading sessions for idle-claim nudge: %v\n", cr.logPrefix, err) //nolint:errcheck
	} else {
		claimWork := make([]beads.Bead, 0, len(assignedWorkBeads)+len(result.ReadyUnassignedRoutedWorkBeads))
		claimWork = append(claimWork, assignedWorkBeads...)
		claimWork = append(claimWork, result.ReadyUnassignedRoutedWorkBeads...)
		claimWorkStoreRefs := make([]string, len(claimWork))
		copy(claimWorkStoreRefs, assignedWorkStoreRefs)
		copy(claimWorkStoreRefs[len(assignedWorkBeads):], result.ReadyUnassignedRoutedWorkStoreRefs)
		nudgeStalledPoolClaims(cr.sp, cr.cfg, sessStore, stalledPoolBeads, claimWork, claimWorkStoreRefs, time.Now(), cr.stdout)
		nudgeStalledPoolContinuations(
			cr.sp,
			cr.cfg,
			sessStore,
			stalledPoolBeads,
			result.ContinuationClaimCandidates,
			result.StoreQueryPartial ||
				result.SessionQueryPartial ||
				result.ContinuationClaimQueryPartial,
			time.Now(),
			cr.stdout,
		)
		// The claim-without-execution lane. Both backstops above end at the
		// claim; this one starts there. It reads the UNFILTERED assigned-work
		// triple, which is the only index-aligned (bead, store, ref) view in the
		// tick — the release filter above rewrites beads and refs but not stores
		// — and re-checks ownership against each session's own identities anyway,
		// so a released bead (whose assignee resolves to no live session by
		// construction) cannot match a running one.
		nudgeStalledPoolExecution(
			cr.sp,
			cr.cfg,
			sessStore,
			stalledPoolBeads,
			result.AssignedWorkBeads,
			result.AssignedWorkStores,
			result.AssignedWorkStoreRefs,
			result.StoreQueryPartial || result.SessionQueryPartial,
			time.Now(),
			cr.rec,
			cr.requestExecutionStalledDrain,
			cr.stdout,
		)
	}
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.nudge_stalled_pool_claims", phaseStart, nil)
}

// recordReconcileTraceInputs records the per-template baseline, the cycle input
// snapshot, and per-template config snapshots for one reconcile tick. It is a
// no-op when trace is nil. It is split out of beadReconcileTick so that the hot
// reconcile path is not dominated by trace bookkeeping.
func (cr *CityRuntime) recordReconcileTraceInputs(
	trace *sessionReconcilerTraceCycle,
	openInfos []sessionpkg.Info,
	desiredState map[string]TemplateParams,
	poolDesired map[string]int,
	workSet map[string]bool,
	traceWorkRequested map[string]bool,
	readyWaitSet map[string]bool,
	result DesiredStateResult,
	recordPhase func(TraceSiteCode, string, time.Time, map[string]any),
) {
	if trace == nil {
		return
	}
	phaseStart := time.Now()
	templateNames := make(map[string]struct{})
	openCounts := make(map[string]int)
	desiredCounts := make(map[string]int)
	// Pre-tick baseline: openInfos is the tick's input row feed projected to Info,
	// captured before the reconciler runs, so these reads are the pre-tick values
	// (byte-equivalent to the raw open-bead read they replace).
	for _, info := range openInfos {
		template := normalizedSessionTemplateInfo(info, cr.cfg)
		if template == "" {
			continue
		}
		templateNames[template] = struct{}{}
		openCounts[template]++
		trace.RecordSessionBaseline(template, info.SessionNameMetadata, map[string]any{
			"state":        info.MetadataState,
			"sleep_reason": info.SleepReason,
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
	for template := range traceWorkRequested {
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
			"work_requested": traceWorkRequested[template],
		})
	}
	trace.RecordCycleInputSnapshot(map[string]any{
		"desired_session_count":               len(desiredState),
		"open_session_count":                  len(openInfos),
		"scale_check_counts":                  result.ScaleCheckCounts,
		"pool_desired":                        poolDesired,
		"ready_wait_count":                    len(readyWaitSet),
		"work_set_count":                      len(workSet),
		"store_query_partial":                 result.StoreQueryPartial,
		"scale_check_query_partial":           len(result.ScaleCheckPartialTemplates) > 0,
		"scale_check_partial_templates":       sortedBoolMapKeys(result.ScaleCheckPartialTemplates),
		"pool_scale_check_partial_templates":  sortedBoolMapKeys(result.PoolScaleCheckPartialTemplates),
		"named_scale_check_partial_templates": sortedBoolMapKeys(result.NamedScaleCheckPartialTemplates),
		"session_query_partial":               result.SessionQueryPartial,
		"snapshot_query_partial":              result.snapshotQueryPartial(),
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
			"lifecycle":           agent.Lifecycle,
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
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.record_trace_input_summary", phaseStart, map[string]any{
		"template_count": len(templateNames),
		"open_count":     len(openInfos),
	})
}

// recordReconcileTraceResults records the per-session terminal result for one
// reconcile tick. No-op when trace is nil.
//
// postTickInfos is the tick's carrier snapshot projected to Info AFTER the
// reconciler's WriteBackReconcileInfos fold — so the template/session_name/state/
// sleep_reason reads are the tick's post-tick in-memory values (a dedup-retired
// loser under its retired session_name="", a healed-then-closed session under its
// post-heal/closed Info). Before W-tick the reconciler mutated the raw open beads
// in place and this recorder read them post-tick; the row reshape moved the working
// set off those beads, and the writeback restores the post-tick observation. It is
// deliberately MORE accurate than a store reload (which excludes closed history).
func (cr *CityRuntime) recordReconcileTraceResults(
	trace *sessionReconcilerTraceCycle,
	postTickInfos []sessionpkg.Info,
	recordPhase func(TraceSiteCode, string, time.Time, map[string]any),
) {
	if trace == nil {
		return
	}
	phaseStart := time.Now()
	for _, info := range postTickInfos {
		template := normalizedSessionTemplateInfo(info, cr.cfg)
		if template == "" {
			continue
		}
		trace.RecordSessionResult(template, info.SessionNameMetadata, TraceOutcomeComplete, TraceCompletenessComplete, map[string]any{
			"state":        info.MetadataState,
			"sleep_reason": info.SleepReason,
		})
	}
	recordPhase(TraceSiteControllerTickPhase, "bead_reconcile.record_trace_session_results", phaseStart, map[string]any{
		"open_count": len(postTickInfos),
	})
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

func traceWorkRequestedByTemplate(scaleCheckCounts map[string]int, namedDemand map[string]bool, workSet map[string]bool, cfg *config.City) map[string]bool {
	result := make(map[string]bool)
	for template, requested := range workSet {
		if requested {
			result[template] = true
		}
	}
	for template, count := range scaleCheckCounts {
		if count > 0 {
			result[template] = true
		}
	}
	for identity, requested := range namedDemand {
		if !requested {
			continue
		}
		spec, ok := findNamedSessionSpec(cfg, "", identity)
		if !ok {
			continue
		}
		template := spec.Agent.QualifiedName()
		if template != "" {
			result[template] = true
		}
	}
	return result
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

func (cr *CityRuntime) waitForAsyncStarts() bool {
	if cr == nil {
		return true
	}
	timeout := time.Duration(0)
	if cr.cfg != nil {
		timeout = cr.cfg.Daemon.ShutdownTimeoutDuration()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// A force stop may leave provider Start calls to finish after shutdown has
	// stopped waiting. That favors bounded shutdown; the next controller start
	// re-lists live runtime sessions after the first stop pass so late-created
	// sessions do not survive the shutdown that abandoned the async wait.
	if !cr.asyncStarts.waitUntil(timeout, cr.forceStopRequested) {
		if cr.stderr != nil && !cr.forceStopRequested() {
			fmt.Fprintf(cr.stderr, "%s: async session starts still running after %s; continuing shutdown\n", cr.logPrefix, timeout) //nolint:errcheck // best-effort stderr
		}
		return false
	}
	return true
}

func (cr *CityRuntime) waitForAsyncStops() bool {
	if cr == nil {
		return true
	}
	timeout := time.Duration(0)
	if cr.cfg != nil {
		timeout = cr.cfg.Daemon.ShutdownTimeoutDuration()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if !cr.asyncStops.waitUntil(timeout, cr.forceStopRequested) {
		if cr.stderr != nil && !cr.forceStopRequested() {
			fmt.Fprintf(cr.stderr, "%s: async drain-ack stops still running after %s; continuing shutdown\n", cr.logPrefix, timeout) //nolint:errcheck // best-effort stderr
		}
		return false
	}
	return true
}

// poolSweepWouldDrain reports whether sweepUndesiredPoolSessionBeads would
// close at least one running pool session this tick — i.e. an open pool session
// bead is not present in desiredState. It mirrors the sweep's core candidate
// filter (open, not desired, not a manual/named session); it intentionally
// omits the sweep's transient create/post-create grace checks because an
// over-inclusive answer only costs an extra identity probe, never a wrong hold.
// Used to gate the managed-Dolt squatter probe to actual scale-down events.
func poolSweepWouldDrain(sessionBeads *sessionBeadSnapshot, desiredState map[string]TemplateParams, cfg *config.City) bool {
	if sessionBeads == nil || cfg == nil {
		return false
	}
	for _, info := range sessionBeads.OpenInfos() {
		if info.Closed {
			continue
		}
		if _, desired := desiredState[info.SessionNameMetadata]; desired {
			continue
		}
		if isManualSessionInfo(info) || isNamedSessionInfo(info) {
			continue
		}
		return true
	}
	return false
}

func sweepUndesiredPoolSessionBeads(
	cityPath string,
	store beads.SessionStore,
	rigStores map[string]beads.Store,
	sessionBeads *sessionBeadSnapshot,
	desiredState map[string]TemplateParams,
	cfg *config.City,
	sp runtime.Provider,
	storeQueryPartial bool,
) int {
	if store.Store == nil || sessionBeads == nil || cfg == nil || storeQueryPartial {
		return 0
	}
	startupTimeout := cfg.Session.StartupTimeoutDuration()
	sweepTime := time.Now()
	var candidates []sessionpkg.Info
	for _, info := range sessionBeads.OpenInfos() {
		if info.Closed {
			continue
		}
		if _, desired := desiredState[info.SessionNameMetadata]; desired {
			continue
		}
		if isManualSessionInfo(info) || isNamedSessionInfo(info) {
			continue
		}
		// Don't sweep beads that the reconciler still considers "start
		// requested" — their work assignment window hasn't opened. The
		// pending_create_claim lease mirrors the reconciler's recovery model:
		// fresh start-in-flight and never-started queue entries are protected,
		// but once that lease expires the crashed creator must not strand the
		// pool slot forever.
		//   - state=creating: protected until staleCreatingState would
		//     return true (i.e., until staleCreatingStateTimeout has
		//     elapsed; zero CreatedAt is treated as stale, matching
		//     staleCreatingState in session_reconcile.go).
		// Without this, a pool's freshly-created session bead gets swept
		// on the same tick it's created (no work assigned →
		// GCSweepSessionBeads closes it), spinning the pool in a rapid
		// create→sweep→recreate loop.
		if pendingCreateClaimStillLeasedForSweepInfo(info, startupTimeout) {
			continue
		}
		if strings.TrimSpace(info.MetadataState) == "creating" && !isStaleCreatingInfo(info) {
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
		// postCreateProtectionTimeout). The guard intentionally ignores
		// last_woke_at: wake bookkeeping can land before the first steady-state
		// patrol, and a just-started bead must not become sweepable simply
		// because that field was already populated. crash/churn paths do not
		// touch creation_complete_at, so a post-crash bead whose last
		// successful start was longer than the timeout ago is sweepable even
		// when wake_attempts or churn_count are non-zero. The age bound mirrors
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
		if poolSessionWithinPostCreateProtection(info, sweepTime) {
			continue
		}
		template := normalizedSessionTemplateInfo(info, cfg)
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !isEphemeralSessionInfo(info) {
			continue
		}
		processNames := config.AgentProcessNames(cfg, *agentCfg, exec.LookPath)
		if running, err := poolSessionBeadRuntimeRunningInfo(info, sp, processNames); err == nil && running {
			continue
		}
		// The candidate is a session-class close op; GCSweepSessionBeads takes the
		// typed session.Info directly and routes the close through the session
		// front door.
		candidates = append(candidates, info)
	}
	return len(GCSweepSessionBeads(cityPath, store.Store, rigStores, candidates))
}

func poolSessionBeadRuntimeRunning(bead beads.Bead, sp runtime.Provider, processNames []string) (bool, error) {
	if sp == nil {
		return false, fmt.Errorf("pool session runtime check: %w", runtime.ErrSessionNotFound)
	}
	name := strings.TrimSpace(bead.Metadata["session_name"])
	if name == "" {
		return false, fmt.Errorf("pool session runtime check missing session name: %w", runtime.ErrSessionNotFound)
	}
	// The sweep only needs provider-runtime/process presence, not attachment or
	// activity details. Process-name hints preserve the same false-negative
	// recovery used by worker observation without the heavier handle path.
	return runtime.ObserveLiveness(sp, name, processNames).Running, nil
}

// poolSessionBeadRuntimeRunningInfo is the session.Info sibling of
// poolSessionBeadRuntimeRunning; it reads the RAW session_name
// (Info.SessionNameMetadata) the same way the bead form does. Equivalence
// is structural: identical body over the same raw field.
func poolSessionBeadRuntimeRunningInfo(info sessionpkg.Info, sp runtime.Provider, processNames []string) (bool, error) {
	if sp == nil {
		return false, fmt.Errorf("pool session runtime check: %w", runtime.ErrSessionNotFound)
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return false, fmt.Errorf("pool session runtime check missing session name: %w", runtime.ErrSessionNotFound)
	}
	return runtime.ObserveLiveness(sp, name, processNames).Running, nil
}

// pendingCreateClaimStillLeasedForSweepInfo keeps pending_create_claim
// protection aligned with the reconciler: start-in-flight claims stay protected
// for the provider-start lease, never-started creates get the longer queue
// lease, and stale claims stop blocking pool-slot recovery. It reads the typed
// session.Info via pendingCreateLeaseActiveInfo.
func pendingCreateClaimStillLeasedForSweepInfo(info sessionpkg.Info, startupTimeout time.Duration) bool {
	return pendingCreateLeaseActiveInfo(info, nil, startupTimeout)
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

// isStaleCreatingInfo is the session.Info mirror of isStaleCreating.
func isStaleCreatingInfo(i sessionpkg.Info) bool {
	now := time.Now()
	if started, ok := parseRFC3339Metadata(i.PendingCreateStartedAt); ok {
		return !now.Before(started.Add(staleCreatingStateTimeout))
	}
	if i.CreatedAt.IsZero() {
		return true
	}
	return !now.Before(i.CreatedAt.Add(staleCreatingStateTimeout))
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

// nudgeDispatchTick runs one supervisor-side nudge dispatch pass. Called
// from the main run loop on wake-socket signal and (belt-and-suspenders)
// at the end of each patrol tick so a missed wake doesn't strand a queue
// item past the patrol interval.
func (cr *CityRuntime) nudgeDispatchTick(_ context.Context) {
	if !nudgeDispatcherIsSupervisor(cr.cfg) {
		return
	}
	// Nudge ops route through the nudges accessor; the session snapshot it pairs
	// them with is loaded via the sessions accessor inside loadSessionBeadSnapshot.
	// Both collapse to the city store today.
	store := cr.nudgesBeadStore()
	if store.Store == nil {
		return
	}
	sessionBeads := cr.loadSessionBeadSnapshot()
	if sessionBeads == nil {
		return
	}
	if _, err := dispatchAllQueuedNudges(cr.cityPath, cr.cfg, store.Store, cr.sessionsBeadStore().Store, cr.sp, sessionBeads, cr.stderr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: nudge dispatcher: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
}

func (cr *CityRuntime) controlDispatcherTick(ctx context.Context) {
	// The control-dispatcher tick threads one city store as two roles at once:
	// the session-bead store the desired-state build creates and updates session
	// beads through (sessions — the build-fn's leading store param flows into
	// agentBuildParams.beadStore and the collectAllOpenSessionInfos "city" arm)
	// and the per-rig work tail (work). The session-sync and reconcile arms below
	// take the same sessions store. Split into the class accessors so a future
	// per-class backend routes each role independently; both collapse to the same
	// store today, so the tick is byte-identical.
	sessionsStore := cr.sessionsBeadStore()
	if sessionsStore.Store == nil || cr.sessionDrains == nil {
		return
	}

	filteredCfg := controlDispatcherOnlyConfig(cr.cfg)
	if filteredCfg == nil {
		return
	}

	cr.ensureManagedDoltPublishedForTick()

	sessionBeads := cr.loadSessionBeadSnapshot()
	tickTime := time.Now()
	wfcResult := buildDesiredStateWithSessionBeadsAt(
		cr.cityName,
		cr.cityPath,
		tickTime,
		tickTime,
		filteredCfg,
		cr.sp,
		sessionsStore.Store,
		unwrapWorkStores(cr.workBeadStores()),
		sessionBeads,
		nil,
		cr.stderr,
	)
	desiredState := wfcResult.State
	cfgNames := configuredSessionNamesWithSnapshot(filteredCfg, cr.cityName, sessionBeads)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(
		cr.cityPath,
		cr.sessionsBeadStore(),
		unwrapWorkStores(cr.workBeadStores()),
		desiredState,
		cr.sp,
		cfgNames,
		filteredCfg,
		clock.Real{},
		cr.stderr,
		true,
		sessionBeads,
	)
	// This targeted tick must include dynamically named pool sessions it just
	// materialized. configuredSessionNamesWithSnapshot intentionally contains
	// only named-session identities, so filtering by cfgNames alone leaves a new
	// dispatcher in start-pending forever. desiredState is already restricted to
	// control-dispatcher configs and is therefore the exact safe row domain.
	reconcileNames := make(map[string]bool, len(desiredState))
	for sessionName := range desiredState {
		reconcileNames[sessionName] = true
	}
	// Feed the reconciler a typed row feed filtered to that exact domain; its
	// carrier is the same rows-built snapshot shape the main tick uses.
	filteredRows := filterReconcileRowsByName(updated, reconcileNames)
	filteredSnap := newSessionBeadSnapshotFromReconcileRows(filteredRows)
	openInfos := filterSessionInfosByName(updated, reconcileNames)
	poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(filteredCfg, cr.cityPath, cr.cityBeadStore(), openInfos, wfcResult.AssignedWorkBeads, wfcResult.AssignedWorkStoreRefs)
	poolDecisionTime := time.Now()
	poolDesired := retainScaleCheckPartialPoolDesired(
		filteredCfg,
		PoolDesiredCounts(ComputePoolDesiredStatesAt(
			filteredCfg, poolWorkBeads, openInfos, wfcResult.ScaleCheckCounts, poolDecisionTime)),
		filteredSnap,
		effectivePoolPartialRetentionTemplates(wfcResult),
	)
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, wfcResult.NamedSessionDemand, filteredCfg)
	reconcileSessionBeadsAtPathWithNamedDemand(
		ctx,
		cr.cityPath,
		filteredRows,
		filteredSnap,
		desiredState,
		reconcileNames,
		filteredCfg,
		cr.sp,
		cr.sessionsBeadStore().Store,
		cr.dops,
		nil,
		unwrapWorkStores(cr.workBeadStores()),
		nil, // control-dispatcher ticks only need ownership continuity, not main-tick assigned/ready snapshots
		cr.sessionDrains,
		cr.providerHealthGate,
		poolDesired,
		wfcResult.NamedSessionDemand,
		wfcResult.NamedSessionRoutedDemand,
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

func (cr *CityRuntime) ensureManagedDoltPublishedForTick() {
	healthFn := cr.managedDoltHealth
	if healthFn == nil {
		healthFn = healthBeadsProvider
	}
	ownedFn := cr.managedDoltOwned
	if ownedFn == nil {
		ownedFn = managedDoltLifecycleOwned
	}
	portFn := cr.managedDoltPort
	if portFn == nil {
		portFn = currentResolvableManagedDoltPort
	}
	ensureManagedDoltPublishedForRuntime(cr.cityPath, cr.stderr, cr.logPrefix, healthFn, ownedFn, portFn)
}

func ensureManagedDoltPublishedForRuntime(
	cityPath string,
	stderr io.Writer,
	logPrefix string,
	healthFn func(string) error,
	ownedFn func(string) (bool, error),
	portFn func(string) string,
) {
	if !cityUsesBdStoreContract(cityPath) {
		return
	}
	owned, err := ownedFn(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: managed dolt ownership preflight: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	if !owned {
		return
	}
	if portFn(cityPath) != "" {
		return
	}
	if err := healthFn(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: managed dolt health preflight: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
	}
}

// syncBeadsAndUpdateIndex runs syncSessionBeads.
func (cr *CityRuntime) syncBeadsAndUpdateIndex(desiredState map[string]TemplateParams, sessionBeads *sessionBeadSnapshot) *sessionBeadSnapshot {
	store := cr.sessionsBeadStore()
	cfgNames := configuredSessionNamesWithSnapshot(cr.cfg, cr.cityName, sessionBeads)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(
		cr.cityPath, store, cr.rigBeadStores(), desiredState, cr.sp, cfgNames, cr.cfg, clock.Real{}, cr.stderr, cr.sessionDrains != nil, sessionBeads,
	)
	return updated
}

func traceSessionSnapshotFields(sessionBeads *sessionBeadSnapshot) map[string]any {
	if sessionBeads == nil {
		return nil
	}
	return map[string]any{
		"open_session_count": len(sessionBeads.OpenInfos()),
	}
}

func traceDesiredStateFields(result DesiredStateResult) map[string]any {
	return map[string]any{
		"desired_session_count":     len(result.State),
		"assigned_work_bead_count":  len(result.AssignedWorkBeads),
		"assigned_work_store_count": len(result.AssignedWorkStoreRefs),
		"store_query_partial":       result.StoreQueryPartial,
		"session_query_partial":     result.SessionQueryPartial,
	}
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
	// The session-bead snapshot is a sessions-class read, so route it through the
	// sessions accessor (identity to the city store today).
	store := cr.sessionsBeadStore()
	if store.Store == nil {
		return nil, false
	}
	sessionBeads, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: loading session beads: %v\n", cr.logPrefix, err) //nolint:errcheck
		return nil, true
	}
	return sessionBeads, false
}

// filterSessionInfosByName selects the open sessions matched on the RAW
// session_name metadata (SessionNameMetadata), for the pool-demand path that reads
// typed Info fields. filterReconcileRowsByName is the ReconcileSession sibling the
// reconciler tick feed uses.
func filterSessionInfosByName(snapshot *sessionBeadSnapshot, names map[string]bool) []sessionpkg.Info {
	if snapshot == nil || len(names) == 0 {
		return nil
	}
	var filtered []sessionpkg.Info
	for _, info := range snapshot.OpenInfos() {
		if names[info.SessionNameMetadata] {
			filtered = append(filtered, info)
		}
	}
	return filtered
}

// filterReconcileRowsByName is the ReconcileSession mirror of
// filterSessionBeadsByName / filterSessionInfosByName: it selects the same open
// sessions (matched on the RAW session_name metadata) in the same order, as the
// typed row feed the config-change tick passes to the reconciler.
func filterReconcileRowsByName(snapshot *sessionBeadSnapshot, names map[string]bool) []sessionpkg.ReconcileSession {
	if snapshot == nil || len(names) == 0 {
		return nil
	}
	var filtered []sessionpkg.ReconcileSession
	for _, row := range snapshot.OpenForReconcile() {
		if names[row.Info.SessionNameMetadata] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (cr *CityRuntime) buildDesiredState(sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
	// The desired-state build threads two store roles: the session-bead store the
	// build-fn's leading store param flows into (sessions — it becomes
	// agentBuildParams.beadStore, which creates and updates session beads, and the
	// collectAllOpenSessionInfos "city" arm) and the per-rig work tail. Split the
	// single city store into the class accessors so a future per-class backend
	// routes each role independently; both collapse to the same store today.
	sessionsStore := cr.sessionsBeadStore()
	if cr.buildFnWithSessionBeads != nil {
		return cr.buildFnWithSessionBeads(cr.cfg, cr.sp, sessionsStore.Store, unwrapWorkStores(cr.workBeadStores()), sessionBeads, trace)
	}
	return cr.buildFn(cr.cfg, cr.sp, sessionsStore.Store)
}

// refreshDesiredState re-applies the session-bead overlay to an already-built
// desired state against a freshly loaded snapshot, so sessions that appeared
// during the build are not missed.
//
// The store param has the same role as buildDesiredState's: it becomes
// agentBuildParams.beadStore, which creates and updates session beads — the
// overlay realizes a dependency floor by minting a `type=session` bead through
// it. So it is the SESSIONS store, not the work store; on a relocated city the
// work store would take a session-class create the class binding never sees and
// the boot containment re-check names.
func (cr *CityRuntime) refreshDesiredState(result DesiredStateResult, sessionBeads *sessionBeadSnapshot) DesiredStateResult {
	return refreshDesiredStateWithSessionBeads(
		result,
		cr.cityName,
		cr.cityPath,
		cr.cfg,
		cr.sp,
		cr.sessionsBeadStore().Store,
		sessionBeads,
		cr.stderr,
	)
}

func (cr *CityRuntime) loadDemandSnapshot(
	sessionBeads *sessionBeadSnapshot,
	trace *sessionReconcilerTraceCycle,
	trigger string,
	configChanged bool,
) runtimeDemandSnapshot {
	sessionFingerprint := sessionBeadSnapshotFingerprint(sessionBeads)
	readyDemandFingerprint := ""
	refresh := cr.shouldRefreshDemandSnapshot(trigger, configChanged, sessionFingerprint)
	if !refresh && trigger == "patrol" && cr.demandSnapshotsEnabled() {
		readyDemandFingerprint = cr.readyDemandSnapshotFingerprint()
		refresh = cr.demandSnapshot.readyDemandFingerprint != readyDemandFingerprint
	}
	if refresh {
		if trigger == "patrol" && cr.demandSnapshotsEnabled() && readyDemandFingerprint == "" {
			readyDemandFingerprint = cr.readyDemandSnapshotFingerprint()
		} else if cr.demandSnapshot != nil {
			readyDemandFingerprint = cr.demandSnapshot.readyDemandFingerprint
		}
		result := cr.buildDesiredState(sessionBeads, trace)
		var openSessionInfos []sessionpkg.Info
		if sessionBeads != nil {
			openSessionInfos = sessionBeads.OpenInfos()
		}
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cr.cfg, cr.cityPath, cr.cityBeadStore(), openSessionInfos, result.AssignedWorkBeads, result.AssignedWorkStoreRefs)
		poolDecisionTime := time.Now()
		result.PoolDesiredCounts = retainScaleCheckPartialPoolDesired(
			cr.cfg,
			PoolDesiredCounts(ComputePoolDesiredStatesTracedAt(
				cr.cfg, poolWorkBeads, openSessionInfos, result.ScaleCheckCounts, poolDecisionTime, trace)),
			sessionBeads,
			effectivePoolPartialRetentionTemplates(result),
		)
		if result.PoolDesiredCounts == nil {
			result.PoolDesiredCounts = make(map[string]int)
		}
		mergeNamedSessionDemand(result.PoolDesiredCounts, result.NamedSessionDemand, cr.cfg)
		result.WorkSet = make(map[string]bool)
		cr.demandSnapshot = &runtimeDemandSnapshot{
			createdAt:              time.Now(),
			sessionFingerprint:     sessionFingerprint,
			readyDemandFingerprint: readyDemandFingerprint,
			result:                 result,
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
	// Non-patrol triggers (config reloads and controller pokes — e.g. from
	// sling-dispatched work) always rebuild immediately; only patrol-cadence
	// re-evaluation is throttled below.
	if configChanged || trigger != "patrol" {
		return true
	}
	if cr.demandSnapshot == nil {
		return true
	}
	if cr.demandSnapshot.sessionFingerprint != sessionFingerprint {
		return true
	}
	maxAge := cr.demandSnapshotPatrolMaxAge()
	if maxAge <= 0 {
		return true
	}
	return time.Since(cr.demandSnapshot.createdAt) >= maxAge
}

// demandSnapshotPatrolMaxAge reports how long a cached demand snapshot may be
// reused across consecutive patrol ticks, or 0 when patrol must rebuild every
// tick. Non-patrol triggers bypass this entirely (see shouldRefreshDemandSnapshot).
func (cr *CityRuntime) demandSnapshotPatrolMaxAge() time.Duration {
	if cr.demandSnapshotsEnabled() {
		return runtimeDemandSnapshotBackstopMaxAge
	}
	// Snapshots are not event-backed. Without an event provider the cache
	// cannot be invalidated by routed-work events, so patrol must rebuild every
	// tick to stay responsive.
	if cr.cs == nil || cr.cs.EventProvider() == nil {
		return 0
	}
	// An event provider exists but a configured scale_check makes demand
	// non-event-backed, so it cannot ride the 30s cache. The control-dispatcher
	// does not poke the controller for scale_check-routed pool work (only sling
	// does), so that work is discovered by the next patrol scale_check rather
	// than by an immediate poke. Flooring the patrol re-eval cadence therefore
	// bounds discovery latency to scaleCheckDemandMinInterval and is a no-op
	// whenever patrol_interval >= that floor (e.g. the 30s default); it only
	// bites sub-second patrol_intervals, where it stops the probe subprocess
	// from running on every tick.
	return scaleCheckDemandMinInterval
}

// readyDemandSnapshotFingerprint hashes the ready sets of every store the
// demand probe reads, so a write anywhere in that set invalidates the cached
// demand snapshot instead of letting it be reused for up to its max age.
//
// "Every store the probe reads" is the load-bearing part, and it is now answered
// by the resolver: the legs are Plan(RoutedWork) narrowed to the RUNTIME plane —
// the identical leg set the routed-demand read itself consumes
// (routedWorkStoreCandidates). A fingerprint over a WIDER set than the read
// invalidates the cache for changes the read cannot see; a fingerprint over a
// NARROWER set licenses reuse of a snapshot that is already wrong. The old
// hand-rolled list was the second kind: it hashed the work store and the rigs
// and missed the graph binding where every routed step lives, so a step claimed
// there changed nothing it could see and the controller kept asserting demand
// for work already taken.
//
// The plane is what makes the check cheap. It used to issue one ReadyLive per
// store — remote work ledger, binding, every rig — so asking "may I reuse the
// cached demand?" cost several remote round trips on every patrol tick, to avoid
// recomputing something cheaper (ga-l7jdg).
//
// Read through the resolver's Walk executor rather than an enumeration: a leg
// read failure is HASHED rather than raised (a stable error must license reuse
// exactly as a stable read does, or a dark store rebuilds the snapshot every
// tick forever), so the visit never returns an error and the plan's per-leg
// policy has nothing to escalate.
func (cr *CityRuntime) readyDemandSnapshotFingerprint() string {
	h := fnv.New64a()
	cfg := cr.serviceConfigSnapshot()
	// The rig map is a constructor INPUT to the topology, not a residency answer
	// — it is filtered by the configured suspension frame and handed straight to
	// residencyTopology, which is what decides the legs. It stays on the
	// residency-boundary census (baselined) because the census counts every
	// consumer of a base enumerator, not only the wrong ones.
	rigs := cr.rigBeadStores()
	topo := cr.residencyTopology(servingRigStores(cfg, rigs, buildSuspendedRigPathsForCity(cfg, cr.cityPath)))
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err == nil {
		plan, err = storeref.Narrow(plan, storeref.PlaneRuntime)
	}
	if err != nil {
		// A refused city has no plan. Hash the refusal: it is stable while the
		// refusal stands, and every arm downstream already reports it loudly.
		_, _ = io.WriteString(h, "refused:")
		_, _ = io.WriteString(h, err.Error())
		return fmt.Sprintf("%x", h.Sum64())
	}
	_, _ = storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		ref := demandFingerprintRef(cr.cityName, leg.Ref)
		_, _ = io.WriteString(h, ref)
		_, _ = io.WriteString(h, "\x00")
		if leg.Store == nil {
			_, _ = io.WriteString(h, "<nil>")
			_, _ = io.WriteString(h, "\x00")
			return false, nil
		}
		ready, err := beads.ReadyLive(leg.Store, beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			log.Printf("readyDemandSnapshotFingerprint: store %s: %v", ref, err)
			_, _ = io.WriteString(h, "error:")
			_, _ = io.WriteString(h, err.Error())
			_, _ = io.WriteString(h, "\x00")
			return false, nil
		}
		sort.Slice(ready, func(i, j int) bool {
			return ready[i].ID < ready[j].ID
		})
		for _, bead := range ready {
			writeReadyDemandFingerprintBead(h, bead)
		}
		return false, nil
	})
	return fmt.Sprintf("%x", h.Sum64())
}

// demandFingerprintRef spells a plan leg the way this fingerprint's log line
// always has.
func demandFingerprintRef(cityName string, ref storeref.StoreRef) string {
	switch {
	case ref == storeref.WorkRef:
		return cityName
	case storeref.IsClassRef(string(ref)):
		return string(ref)
	default:
		rig, _ := storeref.ScopeRigContext(string(ref))
		return rig
	}
}

func writeReadyDemandFingerprintBead(w io.Writer, bead beads.Bead) {
	_, _ = io.WriteString(w, bead.ID)
	_, _ = io.WriteString(w, "\x00")
	_, _ = io.WriteString(w, bead.Status)
	_, _ = io.WriteString(w, "\x00")
	_, _ = io.WriteString(w, bead.Type)
	_, _ = io.WriteString(w, "\x00")
	_, _ = io.WriteString(w, bead.Assignee)
	_, _ = io.WriteString(w, "\x00")
	_, _ = io.WriteString(w, bead.UpdatedAt.Format(time.RFC3339Nano))
	_, _ = io.WriteString(w, "\x00")
	for _, key := range []string{
		beadmeta.RoutedToMetadataKey,
		beadmeta.RunTargetMetadataKey,
		beadmeta.KindMetadataKey,
		beadmeta.FormulaContractMetadataKey,
	} {
		_, _ = io.WriteString(w, key)
		_, _ = io.WriteString(w, "\x00")
		_, _ = io.WriteString(w, bead.Metadata[key])
		_, _ = io.WriteString(w, "\x00")
	}
}

func (cr *CityRuntime) demandSnapshotsEnabled() bool {
	return cr.cs != nil && cr.cs.EventProvider() != nil && demandSnapshotDemandSourcesEventBacked(cr.cfg)
}

func demandSnapshotDemandSourcesEventBacked(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Agents {
		if strings.TrimSpace(cfg.Agents[i].ScaleCheck) != "" {
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

// sessionBeadSnapshotFingerprint returns the snapshot's config-change cache key,
// computed from the raw beads at the store edge (session.SetFingerprint) and
// carried on the snapshot as a field. It hashes every open bead's ID + Status +
// Assignee + ALL metadata keys — a shape session.Info deliberately drops, which is
// why it must be computed at construction, not reconstructed here. An empty string is
// returned for a nil snapshot or one built without raw beads (which never reaches this
// getter — only store-loaded snapshots feed loadDemandSnapshot).
func sessionBeadSnapshotFingerprint(snapshot *sessionBeadSnapshot) string {
	if snapshot == nil {
		return ""
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return snapshot.fingerprint
}

// rigStoreOpenFailure names one rig whose bead store could not be opened.
//
// The rig name is carried apart from the error rather than pre-formatted into
// it, because the callers report a dead rig differently on purpose — see
// openStandaloneRigStores — and a caller that had to strip another surface's
// prefix back off would be reporting under the wrong command's name.
type rigStoreOpenFailure struct {
	rig string
	err error
}

// openStandaloneRigStores opens the work store of every BOUND rig in cfg,
// returning them keyed by rig name plus the rigs that failed to open, in cfg
// order.
//
// It deliberately does NOT decide what a failure means, because the two callers
// disagree and both are right. The controller (buildStandaloneRigStores) warns
// and keeps supervising the rigs it could open: halting a whole city because one
// rig is unmounted is worse than running degraded, and the controller has an
// operator-visible log to say so. `gc ready` (readyRigLegStores) fails the whole
// query: its entire output is a JSON array with nowhere to say it is short.
func openStandaloneRigStores(cfg *config.City, cityPath string) (map[string]beads.Store, []rigStoreOpenFailure) {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil, nil
	}
	stores := make(map[string]beads.Store, len(cfg.Rigs))
	var failures []rigStoreOpenFailure
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
			failures = append(failures, rigStoreOpenFailure{rig: rig.Name, err: err})
			continue
		}
		stores[rig.Name] = store
	}
	if len(stores) == 0 {
		return nil, failures
	}
	return stores, failures
}

func buildStandaloneRigStores(cfg *config.City, cityPath string, stderr io.Writer) map[string]beads.Store {
	stores, failures := openStandaloneRigStores(cfg, cityPath)
	for _, f := range failures {
		fmt.Fprintf(stderr, "gc supervisor: rig bead store %q: %v\n", f.rig, f.err) //nolint:errcheck // best-effort stderr
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
	trace.RecordOperation(TraceSiteLifecycleShutdownPreserveSessions, TraceReasonRetained, TraceOutcomeApplied, "", "", "", 0, traceRecordPayload{
		"city_path": cr.cityPath,
		"city_name": cr.cityName,
		"reason":    "supervisor_shutdown_preserve_mode",
	})
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
		// The storage binding's engine is opened once per process and closed
		// once, at the very end of this one — deferred first so it runs last.
		//
		// Everything below still writes: the order drain, the city-stop sleep
		// reason, and the two-pass session stop all go through the session and
		// order classes, which on a split city are served from this binding.
		// Closing it any earlier turns those writes into errors against a closed
		// store, and the sleep reason an operator reads after `gc stop` is the
		// one that never lands. The defer is also what makes the close reach
		// BOTH exits: a shutdown that hands its sessions to the next supervisor
		// returns early, and it still has to hand over the database file — a
		// process that exits holding it leaves the successor's open racing this
		// one's.
		defer func() {
			// Stop naming these routes before closing them: a sweep that
			// resolved its bindings from a closed handle would answer every leg
			// with an error instead of falling back to the one-shot funnel.
			// Passing our OWN routes is what keeps this from dropping a
			// registration a live replacement already installed.
			unregisterResidencyRoutes(cr.cityPath, cr.storageRoutes)
			if err := cr.storageRoutes.close(); err != nil {
				fmt.Fprintf(cr.stderr, "%s: closing the storage binding: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
			}
		}()
		asyncStartsDrained := cr.waitForAsyncStarts()
		cr.waitForAsyncStops()
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
		if cr.forceStopRequested() {
			gracefulTimeout = 0
		}
		if cr.od != nil || len(cr.retiredOrderDispatchers) > 0 {
			drainTimeout := orderShutdownDrainTimeout(total)
			if cr.forceStopRequested() {
				drainTimeout = 0
			}
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
		// sweepEnumerated tracks whether every stop pass this shutdown ran
		// actually listed the fleet. A failed ListRunning yields an empty
		// slice, so gracefulStopAll stops nothing — tearing the server down
		// after that would turn a transient listing error into an ungraceful
		// mass kill of sessions we never enumerated. Partial failures count
		// as not-enumerated too: we did not see the whole fleet, so we must
		// not kill-server on the strength of a subset.
		sweepEnumerated := listErr == nil
		store := cr.sessionsBeadStore()
		markCityStopSessionSleepReason(sessionFrontDoor(store.Store), cr.stderr)
		gracefulStopAllWithForceSignal(running, cr.sp, gracefulTimeout, cr.rec, cr.cfg, store, cr.stdout, cr.stderr, cr.forceStopRequested)
		if !asyncStartsDrained && cr.forceStopRequested() {
			lateRunning, lateListErr := cr.sp.ListRunning("")
			if lateListErr != nil {
				sweepEnumerated = false
				if runtime.IsPartialListError(lateListErr) {
					fmt.Fprintf(cr.stderr, "%s: force shutdown late async-start listing partially failed; stopping %d visible agent(s): %v\n", cr.logPrefix, len(lateRunning), lateListErr) //nolint:errcheck // best-effort stderr
				} else {
					fmt.Fprintf(cr.stderr, "%s: force shutdown late async-start listing failed: %v\n", cr.logPrefix, lateListErr) //nolint:errcheck // best-effort stderr
				}
			}
			if len(lateRunning) > 0 {
				markCityStopSessionSleepReason(sessionFrontDoor(store.Store), cr.stderr)
				gracefulStopAllWithForceSignal(lateRunning, cr.sp, 0, cr.rec, cr.cfg, store, cr.stdout, cr.stderr, cr.forceStopRequested)
			}
		}
		// With every enumerated session stopped, tear down the provider's
		// shared server, exactly like the standalone stop path (cmdStopBody
		// -> teardownServerForStop). Without this, a supervisor-managed stop
		// leaks the city's tmux server until someone kills it by hand
		// (#5175). Two guards keep the kill-server narrow:
		//   - ownedCity: only a runtime that reached run() owns this city.
		//     A discarded half-built runtime (adoption of an already-running
		//     city; a controller-lock/socket/token failure) calls shutdown()
		//     too, and must never kill the live owner's server.
		//   - sweepEnumerated: only tear down after a stop that actually
		//     listed the fleet, never on the empty slice a failed list leaves.
		// The preserve-sessions shutdown returned above, before the session
		// stops — preserved sessions live inside this server, so keeping it
		// up there is by design, not an omission.
		if cr.ownedCity.Load() && sweepEnumerated {
			teardownServerForStop(cr.sp, cr.stderr, fmt.Sprintf("%s: city '%s'", cr.logPrefix, cr.cityName))
		}
	})
}

func (cr *CityRuntime) preserveSessionsOnShutdown() {
	cr.preserveSessionsShutdown.Store(true)
}

func (cr *CityRuntime) forceStopRequested() bool {
	return cr != nil && cr.forceStopShutdown != nil && cr.forceStopShutdown.Load()
}
