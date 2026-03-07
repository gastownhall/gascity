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

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/telemetry"
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

	cfg     *config.City
	sp      runtime.Provider
	buildFn func(*config.City, runtime.Provider) []agent.Agent

	rops reconcileOps
	dops drainOps
	ct   crashTracker
	it   idleTracker
	wg   wispGC
	ad   automationDispatcher

	rec events.Recorder
	cs  *controllerState // nil when API is disabled

	poolSessions      map[string]time.Duration
	poolDeathHandlers map[string]poolDeathInfo
	suspendedNames    map[string]bool

	standaloneCityStore beads.Store // non-nil when API disabled; for chat auto-suspend

	shutdownOnce   sync.Once
	logPrefix      string // "gc start" or "gc supervisor"
	stdout, stderr io.Writer
}

// CityRuntimeParams holds the caller-provided parameters for creating a
// CityRuntime. Internal components (reconcileOps, crashTracker, etc.) are
// built by the constructor from these inputs.
type CityRuntimeParams struct {
	CityPath  string
	CityName  string
	TomlPath  string
	WatchDirs []string

	Cfg     *config.City
	SP      runtime.Provider
	BuildFn func(*config.City, runtime.Provider) []agent.Agent
	Dops    drainOps

	Rec events.Recorder

	PoolSessions      map[string]time.Duration
	PoolDeathHandlers map[string]poolDeathInfo

	LogPrefix      string // "gc start" or "gc supervisor"; defaults to "gc start"
	Stdout, Stderr io.Writer
}

// newCityRuntime creates a CityRuntime, building internal components
// (reconcileOps, crash tracker, idle tracker, wisp GC, automation
// dispatcher) from the provided parameters.
func newCityRuntime(p CityRuntimeParams) *CityRuntime {
	rops := newReconcileOps(p.SP)

	var ct crashTracker
	if maxR := p.Cfg.Daemon.MaxRestartsOrDefault(); maxR > 0 {
		ct = newCrashTracker(maxR, p.Cfg.Daemon.RestartWindowDuration())
	}

	it := buildIdleTracker(p.Cfg, p.CityName, p.SP)

	var wg wispGC
	if p.Cfg.Daemon.WispGCEnabled() {
		wg = newWispGC(p.Cfg.Daemon.WispGCIntervalDuration(),
			p.Cfg.Daemon.WispTTLDuration(), beads.ExecCommandRunner())
	}

	ad := buildAutomationDispatcher(p.CityPath, p.Cfg, beads.ExecCommandRunner(), p.Rec, p.Stderr)

	suspendedNames := computeSuspendedNames(p.Cfg, p.CityName, p.CityPath)

	logPrefix := p.LogPrefix
	if logPrefix == "" {
		logPrefix = "gc start"
	}

	return &CityRuntime{
		cityPath:          p.CityPath,
		cityName:          p.CityName,
		tomlPath:          p.TomlPath,
		watchDirs:         p.WatchDirs,
		cfg:               p.Cfg,
		sp:                p.SP,
		buildFn:           p.BuildFn,
		rops:              rops,
		dops:              p.Dops,
		ct:                ct,
		it:                it,
		wg:                wg,
		ad:                ad,
		rec:               p.Rec,
		poolSessions:      p.PoolSessions,
		poolDeathHandlers: p.PoolDeathHandlers,
		suspendedNames:    suspendedNames,
		logPrefix:         logPrefix,
		stdout:            p.Stdout,
		stderr:            p.Stderr,
	}
}

// setControllerState sets the API state for this city. The controller
// state is managed by the caller (who also owns the API server) and
// passed in after construction.
func (cr *CityRuntime) setControllerState(cs *controllerState) {
	cr.cs = cs
}

// crashTracker returns the crash tracker for API server wiring.
func (cr *CityRuntime) crashTrack() crashTracker {
	return cr.ct
}

// run executes the reconciliation loop until ctx is canceled. This is
// the per-city main loop — it watches config, reconciles agents, runs
// wisp GC, and dispatches automations.
func (cr *CityRuntime) run(ctx context.Context) {
	dirty := &atomic.Bool{}
	if cr.tomlPath != "" {
		dirs := cr.watchDirs
		if len(dirs) == 0 {
			dirs = []string{filepath.Dir(cr.tomlPath)}
		}
		cleanup := watchConfigDirs(dirs, dirty, cr.stderr)
		defer cleanup()
	}

	// Track effective provider name for hot-reload detection.
	lastProviderName := cr.cfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		lastProviderName = v
	}

	observePaths := observeSearchPaths(cr.cfg.Daemon.ObservePaths)

	cityRoot := filepath.Dir(cr.tomlPath)

	// Open standalone city bead store when API is disabled.
	// When API is enabled, controllerState manages the store.
	if cr.cs == nil {
		if store, err := openCityStoreAt(cityRoot); err != nil {
			fmt.Fprintf(cr.stderr, "%s: city bead store: %v (auto-suspend disabled)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		} else {
			cr.standaloneCityStore = store
		}
	}

	// Upgrade to bead-driven reconcile ops when a bead store is available.
	cr.upgradeToBeadReconcileOps()

	// Session bead sync BEFORE reconciliation: ensures beads exist for
	// beadReconcileOps to read/write hashes. Returns session_name → bead_id
	// index for the reconciler.
	agents := cr.buildFn(cr.cfg, cr.sp)
	cr.syncBeadsAndUpdateIndex(agents)

	// Initial reconciliation.
	doReconcileAgents(agents, cr.sp, cr.rops, cr.dops, cr.ct, cr.it, cr.rec,
		cr.poolSessions, cr.suspendedNames,
		cr.cfg.Daemon.DriftDrainTimeoutDuration(), cr.cfg.Session.StartupTimeoutDuration(),
		cr.stdout, cr.stderr, ctx)
	ensureObservers(agents, observePaths)

	fmt.Fprintln(cr.stdout, "City started.") //nolint:errcheck // best-effort stdout

	interval := cr.cfg.Daemon.PatrolIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Track pool instance liveness for death detection.
	var prevPoolRunning map[string]bool

	for {
		select {
		case <-ticker.C:
			cr.tick(ctx, dirty, &lastProviderName, &observePaths, cityRoot, &prevPoolRunning)
		case <-ctx.Done():
			return
		}
	}
}

// tick performs one reconciliation tick: pool death detection, config
// reload (if dirty), agent reconciliation, wisp GC, and automation
// dispatch.
func (cr *CityRuntime) tick(
	ctx context.Context,
	dirty *atomic.Bool,
	lastProviderName *string,
	observePaths *[]string,
	cityRoot string,
	prevPoolRunning *map[string]bool,
) {
	// Detect pool instance deaths since last tick.
	if len(cr.poolDeathHandlers) > 0 {
		currentRunning, _ := cr.rops.listRunning("")
		currentSet := make(map[string]bool, len(currentRunning))
		for _, name := range currentRunning {
			currentSet[name] = true
		}
		if *prevPoolRunning != nil {
			for sn, info := range cr.poolDeathHandlers {
				if (*prevPoolRunning)[sn] && !currentSet[sn] {
					if _, err := shellScaleCheck(info.Command, info.Dir); err != nil {
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
		cr.reloadConfig(ctx, lastProviderName, observePaths, cityRoot)
	}

	// Session bead sync BEFORE reconciliation.
	agents := cr.buildFn(cr.cfg, cr.sp)
	cr.syncBeadsAndUpdateIndex(agents)

	doReconcileAgents(agents, cr.sp, cr.rops, cr.dops, cr.ct, cr.it, cr.rec,
		cr.poolSessions, cr.suspendedNames,
		cr.cfg.Daemon.DriftDrainTimeoutDuration(), cr.cfg.Session.StartupTimeoutDuration(),
		cr.stdout, cr.stderr, ctx)
	ensureObservers(agents, *observePaths)

	// Wisp GC: purge expired closed molecules.
	if cr.wg != nil && cr.wg.shouldRun(time.Now()) {
		purged, gcErr := cr.wg.runGC(cityRoot, time.Now())
		if gcErr != nil {
			fmt.Fprintf(cr.stderr, "%s: wisp gc: %v\n", cr.logPrefix, gcErr) //nolint:errcheck // best-effort stderr
		} else if purged > 0 {
			fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
		}
	}

	// Automation dispatch.
	if cr.ad != nil {
		cr.ad.dispatch(ctx, cityRoot, time.Now())
	}

	// Chat session auto-suspend: suspend detached idle sessions.
	if idleTimeout := cr.cfg.ChatSessions.IdleTimeoutDuration(); idleTimeout > 0 {
		autoSuspendChatSessions(cr.cityBeadStore(), cr.sp, idleTimeout, cr.stdout, cr.stderr)
	}
}

// reloadConfig attempts to reload city.toml and update all internal
// components. On error, the old config is kept.
func (cr *CityRuntime) reloadConfig(
	ctx context.Context,
	lastProviderName *string,
	observePaths *[]string,
	cityRoot string,
) {
	result, err := tryReloadConfig(cr.tomlPath, cr.cityName, cityRoot, cr.stderr)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v (keeping old config)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, "", err)
		return
	}

	oldAgentCount := len(cr.cfg.Agents)
	oldRigCount := len(cr.cfg.Rigs)
	cr.cfg = result.Cfg

	// Detect session provider change.
	newProviderName := cr.cfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		newProviderName = v
	}
	if newProviderName != *lastProviderName {
		if running, lErr := cr.rops.listRunning(""); lErr == nil && len(running) > 0 {
			fmt.Fprintf(cr.stdout, "Provider changed (%s → %s), stopping %d agent(s)...\n", //nolint:errcheck
				displayProviderName(*lastProviderName), displayProviderName(newProviderName), len(running))
			gracefulStopAll(running, cr.sp, cr.cfg.Daemon.ShutdownTimeoutDuration(), cr.rec, cr.stdout, cr.stderr)
		}
		newSp, spErr := newSessionProviderByName(newProviderName, cr.cfg.Session, cr.cityName)
		if spErr != nil {
			fmt.Fprintf(cr.stderr, "%s: new session provider %q: %v (keeping old provider)\n", //nolint:errcheck
				cr.logPrefix, newProviderName, spErr)
		} else {
			cr.sp = newSp
			provOps := newReconcileOps(cr.sp)
			// Preserve bead-driven reconciliation if a store is available.
			if store := cr.cityBeadStore(); store != nil {
				cr.rops = newBeadReconcileOps(provOps, store)
			} else {
				cr.rops = provOps
			}
			cr.dops = newDrainOps(cr.sp)
			cr.rec.Record(events.Event{
				Type:    events.ProviderSwapped,
				Actor:   "gc",
				Message: fmt.Sprintf("%s → %s", displayProviderName(*lastProviderName), displayProviderName(newProviderName)),
			})
			fmt.Fprintf(cr.stdout, "Session provider swapped to %s.\n", displayProviderName(newProviderName)) //nolint:errcheck
			*lastProviderName = newProviderName
		}
	}

	// Re-materialize and prepend system formulas.
	sysDir, _ := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityRoot)
	if sysDir != "" {
		cr.cfg.FormulaLayers.City = append([]string{sysDir}, cr.cfg.FormulaLayers.City...)
		for rigName, layers := range cr.cfg.FormulaLayers.Rigs {
			cr.cfg.FormulaLayers.Rigs[rigName] = append([]string{sysDir}, layers...)
		}
	}
	if err := config.ValidateRigs(cr.cfg.Rigs, cr.cityName); err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
	resolveRigPaths(cityRoot, cr.cfg.Rigs)
	if err := startBeadsLifecycle(cityRoot, cr.cityName, cr.cfg, cr.stderr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: config reload: %v\n", cr.logPrefix, err) //nolint:errcheck
	}
	if len(cr.cfg.FormulaLayers.City) > 0 {
		if err := ResolveFormulas(cityRoot, cr.cfg.FormulaLayers.City); err != nil {
			fmt.Fprintf(cr.stderr, "%s: config reload: city formulas: %v\n", cr.logPrefix, err) //nolint:errcheck
		}
	}
	for _, r := range cr.cfg.Rigs {
		if layers, ok := cr.cfg.FormulaLayers.Rigs[r.Name]; ok && len(layers) > 0 {
			if err := ResolveFormulas(r.Path, layers); err != nil {
				fmt.Fprintf(cr.stderr, "%s: config reload: rig %q formulas: %v\n", cr.logPrefix, r.Name, err) //nolint:errcheck
			}
		}
	}

	cr.poolSessions = computePoolSessions(cr.cfg, cr.cityName, cr.sp)
	cr.poolDeathHandlers = computePoolDeathHandlers(cr.cfg, cr.cityName, cityRoot, cr.sp)
	cr.suspendedNames = computeSuspendedNames(cr.cfg, cr.cityName, cr.cityPath)

	// Rebuild crash tracker if config values changed.
	newMaxR := cr.cfg.Daemon.MaxRestartsOrDefault()
	newWindow := cr.cfg.Daemon.RestartWindowDuration()
	switch {
	case newMaxR <= 0:
		cr.ct = nil
	case cr.ct == nil:
		cr.ct = newCrashTracker(newMaxR, newWindow)
	default:
		oldMaxR, oldWindow := cr.ct.limits()
		if newMaxR != oldMaxR || newWindow != oldWindow {
			cr.ct = newCrashTracker(newMaxR, newWindow)
		}
	}
	if cr.cs != nil {
		cr.cs.mu.Lock()
		cr.cs.ct = cr.ct
		cr.cs.mu.Unlock()
	}

	cr.it = buildIdleTracker(cr.cfg, cr.cityName, cr.sp)

	if cr.cfg.Daemon.WispGCEnabled() {
		cr.wg = newWispGC(cr.cfg.Daemon.WispGCIntervalDuration(),
			cr.cfg.Daemon.WispTTLDuration(), beads.ExecCommandRunner())
	} else {
		cr.wg = nil
	}

	cr.ad = buildAutomationDispatcher(cityRoot, cr.cfg, beads.ExecCommandRunner(), cr.rec, cr.stderr)
	*observePaths = observeSearchPaths(cr.cfg.Daemon.ObservePaths)

	if cr.cs != nil {
		cr.cs.update(cr.cfg, cr.sp)
	} else if cr.standaloneCityStore != nil {
		// Refresh standalone city store for auto-suspend.
		if s, err := openCityStoreAt(cityRoot); err != nil {
			fmt.Fprintf(cr.stderr, "%s: city bead store reload: %v\n", cr.logPrefix, err) //nolint:errcheck
		} else {
			cr.standaloneCityStore = s
		}
	}

	fmt.Fprintf(cr.stdout, "Config reloaded: %s (rev %s)\n", //nolint:errcheck
		configReloadSummary(oldAgentCount, oldRigCount, len(cr.cfg.Agents), len(cr.cfg.Rigs)),
		shortRev(result.Revision))
	telemetry.RecordConfigReload(ctx, result.Revision, nil)
}

// upgradeToBeadReconcileOps upgrades rops from providerReconcileOps to
// beadReconcileOps when a bead store is available. Called once during run()
// after the bead store is opened. No-op if no store is available.
func (cr *CityRuntime) upgradeToBeadReconcileOps() {
	store := cr.cityBeadStore()
	if store == nil || cr.rops == nil {
		return
	}
	cr.rops = newBeadReconcileOps(cr.rops, store)
}

// syncBeadsAndUpdateIndex runs syncSessionBeads and, if rops is a
// beadReconcileOps, updates its session_name → bead_id index.
func (cr *CityRuntime) syncBeadsAndUpdateIndex(agents []agent.Agent) {
	store := cr.cityBeadStore()
	cfgNames := configuredSessionNames(cr.cfg, cr.cityName)
	idx := syncSessionBeads(store, agents, cfgNames, clock.Real{}, cr.stderr)
	if bro, ok := cr.rops.(*beadReconcileOps); ok && idx != nil {
		bro.updateIndex(idx)
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

// shutdown performs graceful two-pass agent shutdown for this city.
// Safe to call multiple times (e.g., from both panic recovery and
// normal shutdown) — only the first call takes effect.
func (cr *CityRuntime) shutdown() {
	cr.shutdownOnce.Do(func() {
		timeout := cr.cfg.Daemon.ShutdownTimeoutDuration()
		if cr.rops != nil {
			running, _ := cr.rops.listRunning("")
			gracefulStopAll(running, cr.sp, timeout, cr.rec, cr.stdout, cr.stderr)
		} else {
			var names []string
			for _, a := range cr.buildFn(cr.cfg, cr.sp) {
				if a.IsRunning() {
					names = append(names, a.SessionName())
				}
			}
			gracefulStopAll(names, cr.sp, timeout, cr.rec, cr.stdout, cr.stderr)
		}
	})
}
