package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
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
// The endpoint capacity guard is built here, before run, so the planner and
// its effects share the controller's guard without racing its lazy build.
// A generation's template resolver builds its params once, as legacy's
// buildDesiredState does each tick, over the sessions store gather reads.
func (cr *CityRuntime) newPlannerHost() plannerHost {
	guard := cr.ensureEndpointCapacityGuard()
	return plannerHost{
		preserveSessions: cr.preserveSessionsShutdown.Load,
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
			Capacity:  func() *endpointCapacityGuard { return guard },
			Templates: func(env *reconcileEnv, now time.Time) templateResolver {
				return newTemplateResolver(newAgentBuildParams(cr.cityName, cr.cityPath, env.Cfg, env.SP, now.UTC(), cr.v2SessionsStore(), cr.stderr))
			},
			LookPath: exec.LookPath,
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
			return v2CapabilityVerdict(v2ClassStoreCapabilities(cr.v2SessionsStore(), cr.v2GraphStore(), false))
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
		stranded: cr.strandedClaimAlerter(),
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
// conditional-writes mode and kind, whether it has a conditional writer (and
// why not) and an atomic conditional closer, and whether its list reads carry
// the revision its live reads do (and why not), or else the read that failed
// or why that was not decided.
type v2StoreCapability struct {
	Class, Kind    string
	Mode           gate.Mode
	Writer, Closer bool
	Reason         string
	ListRevisions  bool
	ListReason     string
	ListErr        error
	ListSkipped    string
}

// v2ClassStoreCapabilities reads C0.7's capabilities of the sessions and
// graph class stores (CONTRACT v5 R2), writing nothing. Boot resolves the
// writer as the write path does; doctor (inspect) only inspects the stamp
// and the capability memo, so it fires no degraded event.
//
// Both then read revision fidelity the same way
// (beads.CheckListRevisionFidelity): a writer fenced on revisions its listings
// report as 0 fences nothing (mc-3c8h5). The read is skipped for a store that
// already lacks a writer or a closer, which refuses regardless, and is made
// once when both classes resolve to the same store. Doctor skips it over
// native Dolt, cached or not, where the read's Live listing walks the
// non-closed ledger (seconds at 80k rows), and boot makes the read that
// decides there. A sample with no row able to decide (no open or in-progress
// row, or legacy rows alone) is not decided: boot proceeds without a verdict
// and records nothing, and only doctor names the outcome, for a store it
// reads. Boot proceeds because a city with no rows must boot, a write v2
// fences on a revision 0 its store listed is refused rather than written
// blind, and the next boot reads the store again.
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
		switch {
		case !got.Writer || !got.Closer:
		case len(out) == 1 && sameClassStore(sessions, graph) && out[0].Writer && out[0].Closer:
			got.ListRevisions, got.ListReason, got.ListErr, got.ListSkipped = out[0].ListRevisions, out[0].ListReason, out[0].ListErr, out[0].ListSkipped
		case inspect && !listRevisionCheckIsCheap(c.store):
			got.ListSkipped = "not read over native Dolt, whose listing walks the ledger; boot reads it"
		default:
			var blind *beads.ListRevisionsBlindError
			var undecided *beads.ListRevisionsUndecidedError
			switch err := beads.CheckListRevisionFidelity(c.store); {
			case err == nil:
				got.ListRevisions = true
			case errors.As(err, &blind):
				got.ListReason = err.Error()
			case errors.As(err, &undecided):
				got.ListSkipped = "not decided (" + err.Error() + ")"
			default:
				got.ListErr = err
			}
		}
		out = append(out, got)
	}
	return out
}

// listRevisionCheckIsCheap is beads.ListRevisionCheckIsCheap, a var so a test
// can stand in a native store's answer.
var listRevisionCheckIsCheap = beads.ListRevisionCheckIsCheap

// sameClassStore reports whether two class stores are one store. == on two
// interface values panics when their dynamic type is not comparable, or holds
// (in a field) an interface whose dynamic type is not, which no type check
// rules out up front; such stores are not the same store here.
func sameClassStore(a, b beads.Store) (same bool) {
	if a == nil || b == nil || reflect.TypeOf(a) != reflect.TypeOf(b) || !reflect.TypeOf(a).Comparable() {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
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
	out := fmt.Sprintf("%s store (%s, conditional_writes=%s): %s, atomic conditional closer %v", c.Class, c.Kind, mode, writer, c.Closer)
	switch {
	case c.ListRevisions:
		out += ", list revisions true"
	case c.ListReason != "":
		out += ", list revisions false (" + c.ListReason + ")"
	case c.ListErr != nil:
		out += ", list revisions not read (" + c.ListErr.Error() + ")"
	case c.ListSkipped != "":
		out += ", list revisions " + c.ListSkipped
	}
	return out
}

// v2CapabilityRefusal is C0.7's boot refusal: no capability, no effect. It
// names each store lacking one, with all its capabilities, and the fix:
// conditional_writes = "auto" when a store has them off, else a store that
// fences, or whose list reads carry revisions, as the refused stores lack.
func v2CapabilityRefusal(caps []v2StoreCapability) error {
	var parts []string
	off, unfenced, blind := false, false, false
	for _, c := range caps {
		if !c.Writer || !c.Closer || c.ListReason != "" {
			parts = append(parts, c.String())
			off = off || (!c.Writer && c.off())
			unfenced = unfenced || !c.Writer || !c.Closer
			blind = blind || c.ListReason != ""
		}
	}
	if len(parts) == 0 {
		return nil
	}
	fix := "put the class on a store that fences (SQLite or native Dolt)"
	switch {
	case off:
		fix = `set [beads] conditional_writes = "auto"`
	case unfenced && blind:
		fix = "put the class on a store that fences and whose list reads carry revisions (SQLite or native Dolt)"
	case blind:
		fix = "put the class on a store whose list reads carry revisions (SQLite or native Dolt)"
	}
	return fmt.Errorf("v2 refuses to boot without conditional writes (C0.7): %s; v2 never writes blind: %s, or set session_reconciler = \"legacy\"", strings.Join(parts, "; "), fix)
}

// v2CapabilityUnreadError is C0.7 failing to read a class store's list
// revisions: no verdict on the store, so boot retries rather than refusing.
type v2CapabilityUnreadError struct {
	Class string
	Err   error
}

func (e *v2CapabilityUnreadError) Error() string {
	return fmt.Sprintf("C0.7 could not read the %s store to check its list revisions: %v", e.Class, e.Err)
}

func (e *v2CapabilityUnreadError) Unwrap() error { return e.Err }

// v2CapabilityVerdict is boot's C0.7 answer: the refusal when a store lacks a
// capability, else a *v2CapabilityUnreadError when a store could not be read,
// else nil.
func v2CapabilityVerdict(caps []v2StoreCapability) error {
	if refusal := v2CapabilityRefusal(caps); refusal != nil {
		return refusal
	}
	for _, c := range caps {
		if c.ListErr != nil {
			return &v2CapabilityUnreadError{Class: c.Class, Err: c.ListErr}
		}
	}
	return nil
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
		cr.v2.softReload(p.ctx, reloadIntent{Source: source, Soft: p.manualReload != nil && p.manualReload.soft}, &r)
	}
	cr.v2.planner.markDirty("reload")
	cr.v2.wakeReads()
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
// resumes once the swap applied or aborted. Legacy waits the same bound for
// its launched async starts. A ctx done during the wait (a controller stop)
// aborts the swap too.
func (cr *CityRuntime) beforeProviderSwap(ctx context.Context, cfg *config.City) (resume func(), err error) {
	startup := cfg.Session.StartupTimeoutDuration()
	if startup <= 0 {
		startup = 60 * time.Second // as admit's start deadline (CONTRACT v5.4 P3)
	}
	if cr.v2 == nil {
		return func() {}, cr.asyncStarts.waitLaunchedStarts(ctx, startup+startDeadlineSlack)
	}
	if r, refused := v2ProviderSwapRefusal(cfg, reconcilerModeLookupEnv); refused {
		return func() {}, fmt.Errorf("v2 refuses the provider swap: %s", r)
	}
	cr.v2.planner.pauseStarts()
	cr.v2.exec.closeStarts()
	resume = func() {
		cr.v2.exec.openStarts()
		cr.v2.planner.resumeStarts()
	}
	if err := cr.v2.exec.waitStarts(startup + startDeadlineSlack + startFailureSlack); err != nil {
		return resume, err
	}
	return resume, ctx.Err()
}

// v2ProviderSwapRefusal is the latch's refusal of the session provider cfg
// runs under lookupEnv: at boot (v2LatchRefusals), and on a reload's provider
// swap, so a v2 city never swaps onto a runtime it would not start on.
func v2ProviderSwapRefusal(cfg *config.City, lookupEnv func(string) (string, bool)) (latchRefusal, bool) {
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil { // a pack runtime collision; config load already rejects it
		reg = runtimeRegistry
	}
	return v2SessionRuntimeRefusal(cfg, reg, v2SessionRuntimeName(cfg, lookupEnv))
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
