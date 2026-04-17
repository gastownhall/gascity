package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// CityRuntime holds all running state for a single city's reconciliation
// loop. It encapsulates the per-city lifecycle that was previously spread
// across runController and controllerLoop. A machine-wide supervisor can
// instantiate multiple CityRuntimes — one per registered city.
type CityRuntime struct {
	cityPath  string
	cityName  string
	tomlPath  string
	watchDirs []string
	configRev string

	serviceStateMu          sync.RWMutex
	cfg                     *config.City
	sp                      runtime.Provider
	publication             supervisor.PublicationConfig
	buildFn                 func(*config.City, runtime.Provider, beads.Store) DesiredStateResult
	buildFnWithSessionBeads func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult

	dops  drainOps
	ct    crashTracker
	it    idleTracker
	wg    wispGC
	od    orderDispatcher
	trace *sessionReconcilerTraceManager

	rec events.Recorder
	cs  *controllerState // nil when controller-managed bead stores are unavailable
	svc *workspacesvc.Manager

	poolSessions      map[string]time.Duration
	poolDeathHandlers map[string]poolDeathInfo
	suspendedNames    map[string]bool

	standaloneCityStore beads.Store // non-nil when API disabled; for chat auto-suspend
	standaloneRigStores map[string]beads.Store

	// Bead-driven reconciler state (Phase 2f).
	sessionDrains *drainTracker // in-memory drain tracker; nil when bead reconciler disabled

	convHandler         *convergence.Handler     // nil until bead store available
	convStoreAdapter    *convergenceStoreAdapter // typed reference; avoids type assertions in tick/reconcile
	convergenceReqCh    chan convergenceRequest  // receives CLI commands from controller.sock
	pokeCh              chan struct{}            // non-blocking signal to trigger immediate reconciler tick
	controlDispatcherCh chan struct{}            // non-blocking signal for control-dispatcher-only reconcile
	onStarted           func()
	onStatus            func(string)

	shutdownOnce   sync.Once
	logPrefix      string // "gc start" or "gc supervisor"
	stdout, stderr io.Writer
}

// CityRuntimeParams holds the caller-provided parameters for creating a
// CityRuntime. Internal components (crashTracker, etc.) are built by the
// constructor from these inputs.
type CityRuntimeParams struct {
	CityPath  string
	CityName  string
	TomlPath  string
	WatchDirs []string
	ConfigRev string

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
	PokeCh              chan struct{}           // may be nil; triggers immediate tick
	ControlDispatcherCh chan struct{}           // may be nil; triggers control-dispatcher-only reconcile
	OnStarted           func()                  // called after initial reconciliation succeeds
	OnStatus            func(string)            // called when init status changes

	LogPrefix      string // "gc start" or "gc supervisor"; defaults to "gc start"
	Stdout, Stderr io.Writer
}

// newCityRuntime creates a CityRuntime, building internal components
// (crash tracker, idle tracker, wisp GC, order dispatcher) from the
// provided parameters.
func newCityRuntime(p CityRuntimeParams) *CityRuntime {
	var ct crashTracker
	if maxR := p.Cfg.Daemon.MaxRestartsOrDefault(); maxR > 0 {
		ct = newCrashTracker(maxR, p.Cfg.Daemon.RestartWindowDuration())
	}

	it := buildIdleTracker(p.Cfg, p.CityName, p.CityPath, p.SP)

	var wg wispGC
	if p.Cfg.Daemon.WispGCEnabled() {
		wg = newWispGC(p.Cfg.Daemon.WispGCIntervalDuration(),
			p.Cfg.Daemon.WispTTLDuration(), bdCommandRunnerForCity(p.CityPath))
	}

	// Sweep orphaned order-tracking beads on startup only (not config reload).
	// A previous controller instance may have left tracking beads open
	// (goroutines killed on restart, or silent Close failures).
	sweepStore := beads.NewBdStore(p.CityPath, bdCommandRunnerForCity(p.CityPath))
	if n, err := sweepOrphanedOrderTracking(sweepStore); err != nil {
		fmt.Fprintf(p.Stderr, "gc start: order tracking sweep (closed %d): %v\n", n, err) //nolint:errcheck // best-effort stderr
	} else if n > 0 {
		fmt.Fprintf(p.Stderr, "gc start: closed %d orphaned order-tracking beads\n", n) //nolint:errcheck // best-effort stderr
	}

	od := buildOrderDispatcher(p.CityPath, p.Cfg, bdCommandRunnerForCity(p.CityPath), p.Rec, p.Stderr)

	suspendedNames := computeSuspendedNames(p.Cfg, p.CityName, p.CityPath)

	logPrefix := p.LogPrefix
	if logPrefix == "" {
		logPrefix = "gc start"
	}

	cr := &CityRuntime{
		cityPath:                p.CityPath,
		cityName:                p.CityName,
		tomlPath:                p.TomlPath,
		watchDirs:               p.WatchDirs,
		configRev:               p.ConfigRev,
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
		convergenceReqCh:        p.ConvergenceReqCh,
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

	dirty := &atomic.Bool{}
	if cr.tomlPath != "" {
		watchPaths := append([]string{}, cr.watchDirs...)
		if len(watchPaths) == 0 {
			watchPaths = []string{filepath.Dir(cr.tomlPath)}
		}
		var hasTomlPath bool
		for _, path := range watchPaths {
			if samePath(path, cr.tomlPath) {
				hasTomlPath = true
				break
			}
		}
		if !hasTomlPath {
			watchPaths = append(watchPaths, cr.tomlPath)
		}
		cleanup := watchConfigDirs(watchPaths, dirty, cr.pokeCh, cr.stderr)
		defer cleanup()
	}

	// Track effective provider name for hot-reload detection.
	lastProviderName := cr.cfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		lastProviderName = v
	}

	cityRoot := filepath.Dir(cr.tomlPath)

	// Enforce restrictive permissions on .gc/ and its subdirectories.
	enforceGCPermissions(cr.cityPath, cr.stderr)

	// Open standalone city bead store when controllerState is unavailable.
	// When controllerState is present, it manages the cached city store.
	if cr.cs == nil {
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

	// Adoption barrier: ensure every running session has a bead.
	// Runs on every startup (rerunnable, crash-safe).
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
	if ctx.Err() != nil {
		return
	}

	// Initialize convergence handler (requires bead store).
	cr.initConvergenceHandler()
	if ctx.Err() != nil {
		return
	}

	// Session bead sync BEFORE reconciliation: ensures beads exist for
	// the reconciler to read/write hashes. Uses ListByLabel (indexed,
	// fast even before CachingStore is primed).
	sessionBeads := cr.loadSessionBeadSnapshot()
	startupTrace := cr.beginTraceCycle("startup", "initial_reconcile", sessionBeads)
	result := cr.buildDesiredState(sessionBeads, startupTrace)
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
		if startupTrace != nil {
			startupTrace.end(TraceCompletionAborted, traceRecordPayload{"phase": "startup"})
		}
		return
	}

	// Mark city as started. Convergence startup reconciliation runs on
	// the first tick (it calls List() which waits for the full async prime).
	if cr.onStatus != nil {
		cr.onStatus("starting_agents")
	}
	if cr.onStarted != nil {
		cr.onStarted()
	}
	fmt.Fprintln(cr.stdout, "City started.") //nolint:errcheck // best-effort stdout

	if cr.sessionDrains != nil {
		cr.beadReconcileTick(ctx, result, sessionBeads, startupTrace)
	}
	if startupTrace != nil {
		startupTrace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "startup"})
	}
	if ctx.Err() != nil {
		return
	}

	// Convergence startup reconciliation: recover in-progress convergence
	// beads that were interrupted by a controller crash. Runs after "City
	// started" so it doesn't block readiness. List() waits for the full
	// CachingStore prime, then serves from memory.
	cr.convergenceStartupReconcile(ctx)
	if ctx.Err() != nil {
		return
	}

	interval := cr.cfg.Daemon.PatrolIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Track pool instance liveness for death detection.
	var prevPoolRunning map[string]bool

	for {
		select {
		case <-ticker.C:
			cr.tick(ctx, dirty, &lastProviderName, cityRoot, &prevPoolRunning, "patrol")
		case <-cr.pokeCh:
			// Event-driven wake path: sling or API assigned work to a sleeping
			// session. Trigger an immediate tick so the reconciler sees the new
			// work via workSet/poolDesired and wakes the target promptly.
			cr.tick(ctx, dirty, &lastProviderName, cityRoot, &prevPoolRunning, "poke")
		case <-cr.controlDispatcherCh:
			cr.controlDispatcherTick(ctx)
		case req := <-cr.convergenceReqCh:
			// Low-latency path: process convergence commands between ticks.
			// processConvergenceRequests() in tick() drains any that arrived
			// during tick processing. Both paths are safe — channel receives
			// are atomic, so each request is processed exactly once.
			// Note: ordering relative to convergenceTick is non-deterministic
			// via this path, but handlers are idempotent so interleaving is safe.
			reply := cr.safeHandleConvergenceRequest(ctx, req)
			req.replyCh <- reply
		case <-ctx.Done():
			return
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
	sessionBeads := cr.loadSessionBeadSnapshot()
	trace := cr.beginTraceCycle(trigger, "controller_tick", sessionBeads)
	// Detect pool instance deaths since last tick.
	if len(cr.poolDeathHandlers) > 0 {
		currentRunning, _ := cr.sp.ListRunning("")
		currentSet := make(map[string]bool, len(currentRunning))
		for _, name := range currentRunning {
			currentSet[name] = true
		}
		if *prevPoolRunning != nil {
			for sn, info := range cr.poolDeathHandlers {
				if (*prevPoolRunning)[sn] && !currentSet[sn] {
					if _, err := shellRunHook(info.Command, info.Dir); err != nil {
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

	if dirty.Swap(false) {
		cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, trace)
	}
	if ctx.Err() != nil {
		return
	}

	// Session bead sync BEFORE reconciliation (one-tick state lag; see run()).
	// Post-reconcile sync was intentionally removed: the daemon's next tick
	// corrects bead state, and the pre-reconcile sync is sufficient for
	// the reconciler to read/write hashes during reconciliation.
	result := cr.buildDesiredState(sessionBeads, trace)
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
	if ctx.Err() != nil {
		return
	}

	// Wisp GC: purge expired closed molecules.
	if cr.wg != nil && cr.wg.shouldRun(time.Now()) {
		purged, gcErr := cr.wg.runGC(cityRoot, time.Now())
		if gcErr != nil {
			fmt.Fprintf(cr.stderr, "%s: wisp gc: %v\n", cr.logPrefix, gcErr) //nolint:errcheck // best-effort stderr
		} else if purged > 0 {
			fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
		}
	}

	if ctx.Err() != nil {
		return
	}

	// Order dispatch.
	if cr.od != nil {
		cr.od.dispatch(ctx, cityRoot, time.Now())
	}
	if ctx.Err() != nil {
		return
	}

	if cr.svc != nil {
		cr.svc.Tick(ctx, time.Now())
	}

	// Chat session auto-suspend: suspend detached idle sessions.
	if idleTimeout := cr.cfg.ChatSessions.IdleTimeoutDuration(); idleTimeout > 0 {
		autoSuspendChatSessions(cr.cityBeadStore(), cr.sp, idleTimeout, clock.Real{}, cr.stdout, cr.stderr)
	}
	if ctx.Err() != nil {
		return
	}

	// Drain queued convergence requests (CLI commands) BEFORE tick so
	// user commands (e.g. stop) take precedence over automated progression.
	cr.processConvergenceRequests(ctx)

	// Convergence tick: process active convergence loops.
	cr.convergenceTick(ctx)
	if trace != nil {
		trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "tick", "trigger": trigger})
	}
}

// reloadConfig attempts to reload city.toml and update all internal
// components. On error, the old config is kept.
func (cr *CityRuntime) reloadConfig(
	ctx context.Context,
	lastProviderName *string,
	cityRoot string,
) {
	cr.reloadConfigTraced(ctx, lastProviderName, cityRoot, nil)
}

func (cr *CityRuntime) reloadConfigTraced(
	ctx context.Context,
	lastProviderName *string,
	cityRoot string,
	trace *sessionReconcilerTraceCycle,
) {
	result, err := tryReloadConfig(cr.tomlPath, cr.cityName, cityRoot, cr.stderr)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v (keeping old config)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, "", err)
		if trace != nil {
			trace.RecordConfigReload("", "", TraceOutcomeFailed, nil, nil, false, err)
		}
		return
	}
	if cr.configRev != "" && result.Revision == cr.configRev {
		if trace != nil {
			trace.RecordConfigReload(cr.configRev, result.Revision, TraceOutcomeNoChange, nil, nil, false, nil)
		}
		return
	}

	oldAgentCount := len(cr.cfg.Agents)
	oldRigCount := len(cr.cfg.Rigs)
	oldRevision := cr.configRev
	nextCfg := result.Cfg
	nextSp := cr.sp
	nextDops := cr.dops
	providerChanged := false

	// Detect session provider change.
	newProviderName := nextCfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		newProviderName = v
	}
	if newProviderName != *lastProviderName {
		providerChanged = true
		if running, lErr := cr.sp.ListRunning(""); lErr == nil && len(running) > 0 {
			fmt.Fprintf(cr.stdout, "Provider changed (%s → %s), stopping %d agent(s)...\n", //nolint:errcheck
				displayProviderName(*lastProviderName), displayProviderName(newProviderName), len(running))
			gracefulStopAll(running, cr.sp, nextCfg.Daemon.ShutdownTimeoutDuration(), cr.rec, cr.cfg, cr.cityBeadStore(), cr.stdout, cr.stderr)
		}
		newSp, spErr := newSessionProviderByName(newProviderName, nextCfg.Session, cr.cityName, cr.cityPath)
		if spErr != nil {
			fmt.Fprintf(cr.stderr, "%s: new session provider %q: %v (keeping old provider)\n", //nolint:errcheck
				cr.logPrefix, newProviderName, spErr)
		} else {
			nextSp = newSp
			nextDops = newDrainOps(nextSp)
			cr.rec.Record(events.Event{
				Type:    events.ProviderSwapped,
				Actor:   "gc",
				Message: fmt.Sprintf("%s → %s", displayProviderName(*lastProviderName), displayProviderName(newProviderName)),
			})
			fmt.Fprintf(cr.stdout, "Session provider swapped to %s.\n", displayProviderName(newProviderName)) //nolint:errcheck
			*lastProviderName = newProviderName
		}
	}

	// Re-materialize system formulas into the city formulas/ directory.
	MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityRoot) //nolint:errcheck // best-effort
	if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
	resolveRigPaths(cityRoot, nextCfg.Rigs)
	if err := startBeadsLifecycle(cityRoot, cr.cityName, nextCfg, cr.stderr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
	if len(nextCfg.FormulaLayers.City) > 0 {
		if err := ResolveFormulas(cityRoot, nextCfg.FormulaLayers.City); err != nil {
			fmt.Fprintf(cr.stderr, "%s: config reload: city formulas: %v\n", cr.logPrefix, err) //nolint:errcheck
		}
	}
	for _, r := range nextCfg.Rigs {
		layers, ok := nextCfg.FormulaLayers.Rigs[r.Name]
		if !ok || len(layers) == 0 {
			layers = nextCfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			if err := ResolveFormulas(r.Path, layers); err != nil {
				fmt.Fprintf(cr.stderr, "%s: config reload: rig %q formulas: %v\n", cr.logPrefix, r.Name, err) //nolint:errcheck
			}
		}
	}

	// Resolve script symlinks for newly activated packs.
	if len(nextCfg.ScriptLayers.City) > 0 {
		if err := ResolveScripts(cityRoot, nextCfg.ScriptLayers.City); err != nil {
			fmt.Fprintf(cr.stderr, "%s: config reload: city scripts: %v\n", cr.logPrefix, err) //nolint:errcheck
		}
	}
	for _, r := range nextCfg.Rigs {
		if layers, ok := nextCfg.ScriptLayers.Rigs[r.Name]; ok && len(layers) > 0 {
			if err := ResolveScripts(r.Path, layers); err != nil {
				fmt.Fprintf(cr.stderr, "%s: config reload: rig %q scripts: %v\n", cr.logPrefix, r.Name, err) //nolint:errcheck
			}
		}
	}

	cr.poolSessions = computePoolSessions(nextCfg, cr.cityName, cr.cityPath, nextSp)
	cr.poolDeathHandlers = computePoolDeathHandlers(nextCfg, cr.cityName, cityRoot, nextSp)
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
			nextCfg.Daemon.WispTTLDuration(), bdCommandRunnerForCity(cityRoot))
	} else {
		cr.wg = nil
	}

	cr.od = buildOrderDispatcher(cityRoot, nextCfg, bdCommandRunnerForCity(cityRoot), cr.rec, cr.stderr)

	cr.serviceStateMu.Lock()
	cr.cfg = nextCfg
	cr.sp = nextSp
	cr.dops = nextDops
	cr.serviceStateMu.Unlock()

	if cr.cs != nil {
		cr.cs.update(nextCfg, nextSp)
	}
	if cr.svc != nil {
		if err := cr.svc.Reload(); err != nil {
			fmt.Fprintf(cr.stderr, "%s: service reload: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
	}

	if cr.cs == nil {
		// Refresh standalone city store for auto-suspend.
		// Also recovers from nil → non-nil when bd becomes available after startup.
		if s, err := openCityStoreAt(cityRoot); err != nil {
			if cr.standaloneCityStore != nil {
				fmt.Fprintf(cr.stderr, "%s: city bead store reload: %v\n", cr.logPrefix, err) //nolint:errcheck
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
	if trace != nil {
		trace.configRevision = result.Revision
		trace.syncArms(time.Now().UTC(), nextCfg)
	}

	fmt.Fprintf(cr.stdout, "Config reloaded: %s (rev %s)\n", //nolint:errcheck
		configReloadSummary(oldAgentCount, oldRigCount, len(nextCfg.Agents), len(nextCfg.Rigs)),
		shortRev(result.Revision))
	telemetry.RecordConfigReload(ctx, result.Revision, nil)
	if trace != nil {
		trace.RecordConfigReload(oldRevision, result.Revision, TraceOutcomeApplied, nil, nil, providerChanged, nil)
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
		sessionBeads = cr.loadSessionBeadSnapshot()
	}
	assignedWorkBeads := result.AssignedWorkBeads
	if released := releaseOrphanedPoolAssignments(store, cr.cfg, sessionBeads.Open(), assignedWorkBeads); len(released) > 0 {
		releasedSet := make(map[string]struct{}, len(released))
		for _, id := range released {
			releasedSet[id] = struct{}{}
			fmt.Fprintf(cr.stderr, "released orphaned pool work: %s\n", id) //nolint:errcheck
		}
		filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
		for _, wb := range assignedWorkBeads {
			if _, ok := releasedSet[wb.ID]; ok {
				continue
			}
			filtered = append(filtered, wb)
		}
		assignedWorkBeads = filtered
	}
	// poolDesired determines how many sessions should be AWAKE. Uses the
	// same scale_check counts that buildDesiredState already computed (no
	// duplicate shell-outs). Resume tier from cross-referenced assigned
	// work beads + new tier from scale_check + min fill.
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStatesTraced(
		cr.cfg, assignedWorkBeads, sessionBeads.Open(), result.ScaleCheckCounts, trace))
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
		sessionBeads,
		desiredState,
		assignedWorkBeads,
		cr.cfg,
		cr.sp,
		result.StoreQueryPartial,
	) > 0 {
		sessionBeads = cr.loadSessionBeadSnapshot()
	}
	open := sessionBeads.Open()

	// Use cr.cityName consistently — it's the authoritative runtime name.
	cityName := cr.cityName

	cfgNames := configuredSessionNamesWithSnapshot(cr.cfg, cityName, sessionBeads)

	readyWaitSet, err := prepareWaitWakeState(store, cr.rigBeadStores(), time.Now())
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: preparing waits: %v\n", cr.logPrefix, err) //nolint:errcheck
		readyWaitSet = nil
	}

	// workSet: defense-in-depth wake signal from work_query. When work_query
	// detects pending work but scale_check hasn't caught up yet, workSet
	// ensures at least one session wakes without waiting for the next tick.
	workSet := computeWorkSet(cr.cfg, shellScaleCheck, cityName, cr.cityPath, store, sessionBeads)
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
			"desired_session_count": len(desiredState),
			"open_session_count":    len(open),
			"scale_check_counts":    result.ScaleCheckCounts,
			"pool_desired":          poolDesired,
			"ready_wait_count":      len(readyWaitSet),
			"work_set_count":        len(workSet),
			"store_query_partial":   result.StoreQueryPartial,
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

	reconcileSessionBeadsTraced(
		ctx, cr.cityPath, open, desiredState, cfgNames, cr.cfg, cr.sp, store,
		cr.dops,
		assignedWorkBeads, readyWaitSet, cr.sessionDrains, poolDesired,
		result.StoreQueryPartial,
		workSet, cityName,
		cr.it, clock.Real{}, cr.rec, cr.cfg.Session.StartupTimeoutDuration(),
		cr.cfg.Daemon.DriftDrainTimeoutDuration(),
		cr.stdout, cr.stderr, trace,
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
	if err := dispatchReadyWaitNudges(cr.cityPath, store, cr.sp, time.Now()); err != nil {
		fmt.Fprintf(cr.stderr, "%s: dispatching wait nudges: %v\n", cr.logPrefix, err) //nolint:errcheck
	}

	// Idle recovery: detect pool sessions stuck at the prompt after
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

func sweepUndesiredPoolSessionBeads(
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	desiredState map[string]TemplateParams,
	assignedWorkBeads []beads.Bead,
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
		if bead.Metadata["manual_session"] == boolMetadata(true) || isNamedSessionBead(bead) {
			continue
		}
		if sp != nil && sp.IsRunning(bead.Metadata["session_name"]) {
			continue
		}
		template := normalizedSessionTemplate(bead, cfg)
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !isMultiSessionCfgAgent(agentCfg) {
			continue
		}
		candidates = append(candidates, bead)
	}
	return len(GCSweepSessionBeads(store, candidates, assignedWorkBeads))
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
	_, updated := syncSessionBeadsWithSnapshot(
		cr.cityPath,
		store,
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
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(
		filteredCfg, wfcResult.AssignedWorkBeads, open, wfcResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, wfcResult.NamedSessionDemand, filteredCfg)
	reconcileSessionBeadsAtPath(
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
		nil,
		cr.sessionDrains,
		poolDesired,
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
	_, updated := syncSessionBeadsWithSnapshot(
		cr.cityPath, store, desiredState, cr.sp, cfgNames, cr.cfg, clock.Real{}, cr.stderr, cr.sessionDrains != nil, sessionBeads,
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
	store := cr.cityBeadStore()
	if store == nil {
		return nil
	}
	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: loading session beads: %v\n", cr.logPrefix, err) //nolint:errcheck
		return nil
	}
	return sessionBeads
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

func buildStandaloneRigStores(cfg *config.City, cityPath string, stderr io.Writer) map[string]beads.Store {
	return buildRigStores(cfg, cityPath, "gc supervisor", stderr)
}

// buildRigStores opens bead stores for all rigs attached to the city.
// Errors on individual rigs are logged with logPrefix and skipped.
func buildRigStores(cfg *config.City, cityPath, logPrefix string, stderr io.Writer) map[string]beads.Store {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil
	}
	stores := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		store, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "%s: rig bead store %q: %v\n", logPrefix, rig.Name, err) //nolint:errcheck // best-effort stderr
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

// shutdown performs graceful two-pass agent shutdown for this city.
// Safe to call multiple times (e.g., from both panic recovery and
// normal shutdown) — only the first call takes effect.
func (cr *CityRuntime) shutdown() {
	cr.shutdownOnce.Do(func() {
		if cr.trace != nil {
			_ = cr.trace.Close()
		}
		if cr.svc != nil {
			if err := cr.svc.Close(); err != nil {
				fmt.Fprintf(cr.stderr, "%s: service shutdown: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
			}
		}
		timeout := cr.cfg.Daemon.ShutdownTimeoutDuration()
		running, _ := cr.sp.ListRunning("")
		gracefulStopAll(running, cr.sp, timeout, cr.rec, cr.cfg, cr.cityBeadStore(), cr.stdout, cr.stderr)
	})
}
