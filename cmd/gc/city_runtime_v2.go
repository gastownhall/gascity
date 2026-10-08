package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// The city runtime's half of the session_reconciler switch. Under v2 the
// tick and the startup step run only their maintenance phases, the startup
// step boots the v2 runtime, and a config reload publishes its next env;
// the session phases, the control-dispatcher tick and the drain tracker are
// unreachable (reconcile_maintenance.go guards them besides). The v2 code
// itself never sees CityRuntime: it reaches the city through the plannerHost
// built here.

// v2TickPhases and v2StartupPhases are what v2 leaves the controller of the
// tick and of the startup step.
var (
	v2TickPhases    = maintenancePhases(legacyTickPhases)
	v2StartupPhases = maintenancePhases(legacyStartupPhases)
)

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

// installPlanner binds rt to this city runtime. newCityRuntime calls it
// once, for a controller that latched v2, before it installs the wiring's
// wake and before run.
func (cr *CityRuntime) installPlanner(rt *plannerRuntime) {
	rt.bindHost(cr.newPlannerHost())
	cr.v2 = rt
}

// newPlannerHost is the one place the planner's view of the city is built
// (F2). Every closure reads state a lock publishes or state fixed before run
// starts; run sets inventoryLane before the startup step boots the planner.
// The capacity guard and template resolutions are left out: no trace-only
// arm reads them.
func (cr *CityRuntime) newPlannerHost() plannerHost {
	return plannerHost{
		gather: gatherEnv{
			CityPath: cr.cityPath, CityName: cr.cityName,
			Sessions:  cr.v2SessionsStore,
			RigStores: cr.rigBeadStores, // residency:allow — the census frame; sessionCensusStoreCandidates plans the legs (storeref.Plan)
			Observations: func() *ObservationCache {
				if cr.inventoryLane == nil {
					return nil
				}
				return cr.inventoryLane.cache
			},
			Health: func() *providerHealthSnapshot { return loadProviderHealthSnapshot(cr.cityPath) },
			Episodes: func() (map[string]sessionpkg.StartupHealthEpisode, error) {
				return readStartupHealthEpisodes(cr.v2SessionsStore())
			},
			Suspension: func() suspensionstate.State { return loadSuspensionStateBestEffort(cr.cityPath) },
			Nudges: func() beads.NudgesStore {
				return beads.NudgesStore{Store: resolveNudgesStore(cr.storageRoutes, cr.cityBeadStore(), cr.serviceConfigSnapshot(), cr.cityPath, cr.rec)}
			},
			WorkStore: cr.cityBeadStore,
			LookPath:  exec.LookPath,
		},
		snapshotEnv: cr.serviceEnvSnapshot,
		setInventoryHook: func(fn func(prev, next *ObservationSnapshot)) {
			cr.inventoryLane.setPassHook(fn)
		},
		bootCensus: func() (v2SessionMigration, error) {
			rigs := cr.rigBeadStores() // residency:allow — the census frame; collectOpenSessionInfos plans the legs (storeref.Plan)
			return readV2SessionMigration(cr.cityPath, cr.cityName, cr.serviceConfigSnapshot(), cr.v2SessionsStore(), rigs)
		},
		capabilities: func() error {
			return v2CapabilityRefusal(v2ClassStoreCapabilities(cr.v2SessionsStore(), cr.v2GraphStore(), false))
		},
		beginTrace: cr.beginV2Trace,
		inventoryFields: func() map[string]any {
			if cr.inventoryLane == nil {
				return nil
			}
			return cr.inventoryLane.v2PassFields()
		},
		safeTick: cr.safeTick,
		rec:      cr.rec,
		stderr:   cr.stderr,
	}
}

// v2SessionsStore is sessionsBeadStore with the config read under its lock.
func (cr *CityRuntime) v2SessionsStore() beads.Store {
	return resolveSessionStore(cr.storageRoutes, cr.cityBeadStore(), cr.serviceConfigSnapshot(), cr.cityPath, cr.rec)
}

// v2GraphStore is graphBeadStore with the config read under its lock.
func (cr *CityRuntime) v2GraphStore() beads.Store {
	return resolveGraphStore(cr.storageRoutes, cr.cityBeadStore(), cr.serviceConfigSnapshot(), cr.cityPath, cr.rec)
}

// v2StoreCapability is C0.7's reading of one class store: its stamped
// conditional-writes mode and kind, whether it has a conditional writer and
// an atomic conditional closer, and why it has no writer.
type v2StoreCapability struct {
	Class, Kind    string
	Mode           gate.Mode
	Writer, Closer bool
	Reason         string
}

// v2ClassStoreCapabilities reads C0.7's two capabilities of the sessions
// and graph class stores (CONTRACT v5 R2), writing nothing. Boot resolves
// the writer as the write path does; doctor (inspect) only inspects the
// stamp and the capability memo, so it fires no degraded event.
func v2ClassStoreCapabilities(sessions, graph beads.Store, inspect bool) []v2StoreCapability {
	out := make([]v2StoreCapability, 0, 2)
	for _, c := range []struct {
		class string
		store beads.Store
	}{{"sessions", sessions}, {"graph", graph}} {
		insp := beads.InspectConditionalWrites(c.store)
		got := v2StoreCapability{Class: c.class, Kind: insp.StoreKind, Mode: insp.Mode, Reason: insp.Reason}
		_, got.Closer = beads.AtomicConditionalCloserFor(c.store)
		if inspect {
			_, has := beads.ConditionalWriterForTarget(c.store)
			got.Writer = has && insp.Capable && (insp.Mode == gate.Auto || insp.Mode == gate.Require)
		} else {
			w, diag, err := beads.ResolveConditionalWriter(c.store)
			got.Writer = w != nil && err == nil
			switch {
			case err != nil:
				got.Reason = err.Error()
			case diag != nil && diag.PreflightReason != "":
				got.Reason = diag.PreflightReason
			}
		}
		if !got.Writer && got.Reason == "" {
			got.Reason = "conditional writes are off"
			if got.Mode == gate.Auto || got.Mode == gate.Require {
				got.Reason = "the store does not implement conditional writes"
			}
		}
		out = append(out, got)
	}
	return out
}

// off reports whether c's conditional writes are off or never stamped.
func (c v2StoreCapability) off() bool { return c.Mode == gate.ModeUnset || c.Mode == gate.Off }

// String is c as the boot refusal and doctor name it.
func (c v2StoreCapability) String() string {
	mode := string(c.Mode)
	if c.Mode == gate.ModeUnset {
		mode = "unset"
	}
	writer := fmt.Sprintf("conditional writer %v", c.Writer)
	if !c.Writer {
		writer += " (" + c.Reason + ")"
	}
	return fmt.Sprintf("%s store (%s, conditional_writes=%s): %s, atomic conditional closer %v", c.Class, c.Kind, mode, writer, c.Closer)
}

// v2CapabilityRefusal is C0.7's boot refusal: no capability, no effect. It
// names each store lacking one, with both its capabilities, and the fix:
// conditional_writes = "auto" when a store has them off, else a store that
// fences.
func v2CapabilityRefusal(caps []v2StoreCapability) error {
	var parts []string
	off := false
	for _, c := range caps {
		if !c.Writer || !c.Closer {
			parts = append(parts, c.String())
			off = off || (!c.Writer && c.off())
		}
	}
	if len(parts) == 0 {
		return nil
	}
	fix := "put the class on a store that fences (SQLite or native Dolt)"
	if off {
		fix = `set [beads] conditional_writes = "auto"`
	}
	return fmt.Errorf("v2 refuses to boot without conditional writes (C0.7): %s; v2 never writes blind: %s, or remove session_reconciler to run legacy", strings.Join(parts, "; "), fix)
}

// recordV2Pass records the planner's reconcile_pass operation in a
// maintenance tick's trace, however the tick ends.
func (cr *CityRuntime) recordV2Pass(trace *sessionReconcilerTraceCycle) {
	fields := cr.v2.passRecord(time.Now(), cr.legacySessionEntries.Load())
	trace.RecordControllerOperation(TraceSiteReconcilePass, TraceReasonRetained, TraceOutcomeComplete, "reconcile_pass", 0, fields)
}

// beginV2Trace opens a trace cycle for a v2 decision. It runs off the
// controller goroutine (the planner's), so it is built like
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

// bootV2 is the startup step's v2 half, after its maintenance phases: it
// boots the planner on ctx, the run context, which becomes its lifetime
// (plannerRuntime.boot). With no bead store the planner latches no-store and
// stays off, and readiness proceeds (MAINT-003).
func (cr *CityRuntime) bootV2(ctx context.Context) bool {
	if cr.cityBeadStore() == nil {
		cr.v2.noStore.Store(true)
		fmt.Fprintf(cr.stderr, "%s: session reconciler v2: no bead store; the planner is disabled\n", cr.logPrefix) //nolint:errcheck // best-effort stderr
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

// reloadV2 applies the tick's config reload as legacy does, publishes the
// planner's next env (CONTRACT v5 P7), then amends and sends the reply
// (MAINT-023). Nothing pauses: a pass reads one env however long it runs.
func (cr *CityRuntime) reloadV2(p *tickPass, source reloadSource) {
	defer cr.v2.publishEnv() // a panic in apply after it published still publishes
	r := cr.reloadConfigTraced(p.ctx, p.lastProviderName, p.cityRoot, p.trace, source)
	cr.v2.publishEnv()
	if r.Outcome != reloadOutcomeFailed {
		cr.v2.softReload(reloadIntent{Source: source, Soft: p.manualReload != nil && p.manualReload.soft}, &r)
	}
	cr.v2.planner.markDirty("reload")
	p.manualReply = r
	if p.manualReload != nil {
		p.manualReloadCompleted = true
		cr.completeManualReload(p)
	}
}

// beforeProviderSwap holds a provider swap (CONTRACT v5 P7): it pauses the
// planner's starts and creates (swap-pause) and closes the executor to starts
// admitted before the pause, then waits for every in-flight
// v2 start (effectExecutor.waitStarts), so the swap's listing cannot miss a
// runtime a start is still creating; an error aborts the reload. The caller
// resumes once the swap applied or aborted. Legacy waits on nothing.
func (cr *CityRuntime) beforeProviderSwap(cfg *config.City) (resume func(), err error) {
	if cr.v2 == nil {
		return func() {}, nil
	}
	startup := cfg.Session.StartupTimeoutDuration()
	if startup <= 0 {
		startup = 60 * time.Second // as admit's start deadline (CONTRACT v5.4 P3)
	}
	cr.v2.planner.pauseStarts()
	cr.v2.exec.closeStarts()
	resume = func() {
		cr.v2.exec.openStarts()
		cr.v2.planner.resumeStarts()
	}
	return resume, cr.v2.exec.waitStarts(startup + startDeadlineSlack)
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
		case p.Wake != nil && p.Wake.planner != nil:
			return errors.New("controller wiring: a legacy controller's wake marks a v2 planner")
		}
		return nil
	}
	switch {
	case p.V2 == nil || p.Wake == nil:
		return errors.New("controller wiring: a v2 controller needs its wiring's wake and v2 runtime")
	case p.Wake.planner != p.V2.planner:
		return errors.New("controller wiring: the wake does not route to the controller's v2 runtime")
	}
	return nil
}
