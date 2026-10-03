package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The city runtime's half of the session_reconciler switch. Under v2 the
// tick and the startup step run only their maintenance phases, the startup
// step boots the v2 runtime, and a config reload applies under its barrier;
// the session phases, the control-dispatcher tick and the drain tracker are
// unreachable (reconcile_maintenance.go guards them besides). The v2 code
// itself never sees CityRuntime: it reaches the city through the v2Host built
// here.

// v2TickPhases and v2StartupPhases are what v2 leaves the controller of the
// tick and of the startup step.
var (
	v2TickPhases    = maintenancePhases(legacyTickPhases)
	v2StartupPhases = maintenancePhases(legacyStartupPhases)
)

var errV2NoSessionsStore = errors.New("no sessions store")

// runsV2 reports whether this controller latched the v2 session reconciler.
func (cr *CityRuntime) runsV2() bool {
	return cr.reconcilerDrift.running == reconcilerV2
}

// tickPhases returns the phases this controller's tick runs.
func (cr *CityRuntime) tickPhases() []tickPhase {
	if cr.runsV2() {
		return v2TickPhases
	}
	return legacyTickPhases
}

// controlDispatcherSignal is the channel run's control-dispatcher arm
// selects on: nil under v2, where the control-dispatch key is allocator work
// (MAINT-056) and keys never reach the legacy signals, so the legacy
// control-dispatcher tick cannot run whatever lands there.
func (cr *CityRuntime) controlDispatcherSignal() <-chan struct{} {
	if cr.runsV2() {
		return nil
	}
	return cr.controlDispatcherCh
}

// installV2 binds rt to this city runtime. newCityRuntime calls it once, for
// a controller that latched v2, before it installs the wiring's wake and
// before run.
func (cr *CityRuntime) installV2(rt *v2Runtime) {
	rt.bindHost(cr.newV2Host())
	cr.v2 = rt
}

// newV2Host is the one place the v2 runtime's view of the city is built (F2).
// Every closure reads state a lock publishes (serviceStateMu, the controller
// state's mu) or state fixed before run starts (cityPath, cityName,
// storageRoutes, rec, cs, trace, stderr); none reads a field a reload writes
// unlocked. The exception is inventoryLane: run sets it before the startup
// step, on the goroutine that then boots the runtime, which is the only
// caller of setInventoryHook.
func (cr *CityRuntime) newV2Host() v2Host {
	return v2Host{
		sessions: cr.v2SessionsCensus,
		censusLegs: func() ([]classStoreCandidate, error) {
			cfg := cr.serviceConfigSnapshot()
			rigs := cr.rigBeadStores() // residency:allow — the census frame; censusStoreCandidates plans the legs (storeref.Plan)
			return censusStoreCandidates(cr.cityPath, cfg, cr.v2SessionsStore(), rigs, buildSuspendedRigPathsForCity(cfg, cr.cityPath), censusRefBare)
		},
		snapshotEnv: cr.serviceEnvSnapshot,
		setInventoryHook: func(fn func(prev, next *ObservationSnapshot)) {
			cr.inventoryLane.setPassHook(fn)
		},
		cityStore:   cr.cityBeadStore,
		rigStores:   cr.rigBeadStores, // residency:allow — the reload barrier compares store handles to spot a rebuild; resolves no bead
		retryReload: cr.requestConfigReloadRetry,
		beginTrace:  cr.beginV2Trace,
		safeTick:    cr.safeTick,
		stderr:      cr.stderr,
	}
}

// v2SessionsStore is sessionsBeadStore with the config read under its lock.
func (cr *CityRuntime) v2SessionsStore() beads.Store {
	return resolveSessionStore(cr.storageRoutes, cr.cityBeadStore(), cr.serviceConfigSnapshot(), cr.cityPath, cr.rec)
}

// v2SessionsCensus is the router's sessions census: the open session rows of
// the sessions-class store, through its cache. It uses the package-level
// loader, which returns its error; the CityRuntime method maps an error to an
// empty snapshot, and a failed read must never look like an empty city.
func (cr *CityRuntime) v2SessionsCensus() ([]sessionpkg.Info, error) {
	store := cr.v2SessionsStore()
	if store == nil {
		return nil, errV2NoSessionsStore
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return nil, err
	}
	return snapshot.OpenInfos(), nil
}

// beginV2Trace opens a trace cycle for a v2 decision. It runs off the
// controller goroutine (the worker FS gate), so it is built like
// beginOrdersLaneTrace: the config from the locked snapshot, no revision.
func (cr *CityRuntime) beginV2Trace(trigger string) *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	return cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger: trigger,
		CityPath:    cr.cityPath,
	}, cr.serviceConfigSnapshot(), nil)
}

// bootV2 is the startup step's v2 half, after its maintenance phases. It
// boots the runtime on ctx, the run context, which becomes the runtime's
// lifetime, and returns once every open session row has been reconciled once
// and the allocator has passed (MAINT-010, MAINT-012), or false when ctx
// ends. With no bead store there is nothing to reconcile: the workers stay
// off and readiness proceeds (MAINT-003). boot is idempotent, so a startup
// retry after a panic resumes it (MAINT-005).
func (cr *CityRuntime) bootV2(ctx context.Context) bool {
	if cr.cityBeadStore() == nil {
		fmt.Fprintf(cr.stderr, "%s: session reconciler v2: no bead store; reconcile workers disabled\n", cr.logPrefix) //nolint:errcheck // best-effort stderr
		return true
	}
	if err := cr.v2.boot(ctx); err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(cr.stderr, "%s: session reconciler v2: boot: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		}
		return false
	}
	return true
}

// reloadUnderBarrier applies the tick's config reload with every v2
// reconcile paused (C4.4). A manual reload is answered from inside the
// barrier, after the post-reload hooks and before the reconciles resume
// (MAINT-023); a reload that aborts or defers is answered failed and left
// pending for the next patrol.
func (cr *CityRuntime) reloadUnderBarrier(p *tickPass, source reloadSource) {
	intent := reloadIntent{Source: source, Soft: p.manualReload != nil && p.manualReload.soft}
	apply := func() reloadControlReply {
		return cr.reloadConfigTraced(p.ctx, p.lastProviderName, p.cityRoot, p.trace, source)
	}
	reply := func(r reloadControlReply) {
		p.manualReply = r
		if p.manualReload != nil {
			p.manualReloadCompleted = true
			cr.completeManualReload(p)
		}
	}
	_, _ = cr.v2.barrier.run(p.ctx, intent, apply, reply)
}

// checkReconcilerWiring refuses runtime params whose v2 runtime and wake
// disagree with the latched mode: a v2 runtime must reconcile through the
// wake its controller's socket and API already use, so a v2 controller takes
// both from its wiring (controllerWiring.runtimeParams) and never builds its
// own.
func checkReconcilerWiring(p CityRuntimeParams) error {
	if p.ReconcilerMode != reconcilerV2 {
		switch {
		case p.V2 != nil:
			return errors.New("controller wiring: a v2 runtime handed to a legacy controller")
		case p.Wake != nil && p.Wake.router != nil:
			return errors.New("controller wiring: a legacy controller's wake routes to a v2 router")
		}
		return nil
	}
	switch {
	case p.V2 == nil || p.Wake == nil:
		return errors.New("controller wiring: a v2 controller needs its wiring's wake and v2 runtime")
	case p.Wake.router != p.V2.router:
		return errors.New("controller wiring: the wake does not route to the controller's v2 runtime")
	}
	return nil
}
