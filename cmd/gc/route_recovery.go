package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// carriedPoolRoute returns the pool route a bead already declares for itself and
// that the controller may safely restore to gc.routed_to, or "" when the bead
// carries no recoverable route. Two bead shapes carry a legacy gc.run_target
// pool route: a plain (kind-less) standalone work bead — this fork's dominant
// work shape — and a pre-ga-eld2x workflow root (recognized by
// legacyWorkflowRunTarget).
//
// Control-dispatcher (retry, ralph, …) and other workflow-topology (scope, spec)
// beads also carry a bare gc.run_target, but there it is a dispatch/structure
// target an agent never claims from a pool; restoring gc.routed_to on one would
// mis-route it into pool demand, so they yield "". The choice is judgment-free
// (ZFC): it copies a route the bead already declares and never invents a target.
// Idempotent: a bead that already carries gc.routed_to yields "".
func carriedPoolRoute(b beads.Bead) string {
	// Legacy pre-ga-eld2x workflow root: gc.run_target is the root's pool route
	// only while gc.routed_to is empty — exactly legacyWorkflowRunTarget's rule.
	if route := legacyWorkflowRunTarget(b); route != "" {
		return route
	}
	// Broaden beyond workflow roots to plain standalone work beads. Any non-empty
	// gc.kind reaching here is a control-dispatcher or workflow-topology construct
	// (legacyWorkflowRunTarget already consumed the lone claimable kind,
	// "workflow"), so its gc.run_target is not a recoverable pool route.
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != "" {
		return ""
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
}

// routeRecoveryLaneOf returns this runtime's lane, creating it on first use so a
// directly-constructed CityRuntime (every test, every one-shot) needs no wiring.
func (cr *CityRuntime) routeRecoveryLaneOf() *routeRecoveryLane {
	cr.routeRecoveryOnce.Do(func() { cr.routeRecovery = newRouteRecoveryLane() })
	return cr.routeRecovery
}

// routeRecoveryEventProvider is the journal the delta feed tails, or nil when
// this runtime has no controller state (every one-shot and standalone runtime).
// A nil provider leaves the lane backstop-only, loudly, rather than pretending a
// delta pass covers the city.
func (cr *CityRuntime) routeRecoveryEventProvider() events.Provider {
	if cr.cs == nil {
		return nil
	}
	return cr.cs.EventProvider()
}

// routeRecoveryPlan resolves the work legs a route-repair pass reads.
//
// The leg set comes from the residency resolver rather than from a per-site
// list: Plan(RoutedWork) is the answer to "which stores hold claimable/routed
// work", which is precisely the surface a lost gc.routed_to makes a bead
// invisible to. Which of those legs a given pass may READ is the plane's
// business, and walkPlaneLegs owns it: the tick reads the infra/class binding
// only, the off-tick convergence lane reads the ledger and the rigs.
//
// The detached-orphan lane resolves the SAME plan (detachedOrphanPlan). A bead
// whose gc.routed_to was lost is invisible to pool demand for the same reason
// whichever lane lost it, so a second derivation of "which stores hold routed
// work" would be the split-store bug class rather than a second opinion.
func (cr *CityRuntime) routeRecoveryPlan() (storeref.ResolvedPlan, error) {
	cfg := cr.serviceConfigSnapshot()
	topo := residencyTopologyForCity(cr.cityPath, cfg, cr.cityBeadStore(), cr.routeRecoveryRigStores(cfg))
	return storeref.Plan(storeref.RoutedWork{}, topo)
}

// serviceConfigSnapshot reads the published config under the lock a reload
// writes it with. The backstop lane runs on its own goroutine, so an unlocked
// read of cr.cfg here would race the config swap in reloadConfigTraced.
func (cr *CityRuntime) serviceConfigSnapshot() *config.City {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	return cr.cfg
}

// routeRecoveryRigStores is the suspension FRAME for this lane: the rig legs it
// may repair through.
//
// Told, not deciding — the same discipline as the census arms. A suspended rig
// is routinely dark, and its store is an unavailableStore whose every read
// errors; planning over it would make the pass Partial and reschedule the
// backstop on the short retry cadence forever.
//
// A rig the store map holds but config no longer declares also stays out — the
// census's rule, and the right one here: repairing an undeclared rig's routes
// feeds pool demand for a rig nothing serves. A runtime with no config at all
// (one-shot, standalone) has nothing declaring suspension, so every open rig is
// serving.
//
// residency:allow — a constructor INPUT, not a residency answer. It filters the
// runtime's own open-store map by the configured suspension set and hands the
// result to residencyTopologyForCity; it consults no binding and no leg order.
func (cr *CityRuntime) routeRecoveryRigStores(cfg *config.City) map[string]beads.Store {
	rigs := cr.rigBeadStores() // residency:allow — constructor input to residencyTopologyForCity, not a residency answer
	if cfg == nil {
		return rigs
	}
	return servingRigStores(cfg, rigs, buildSuspendedRigPathsForCity(cfg, cr.cityPath))
}

// recoverUnroutedWorkRoutes runs the authoritative convergence scan across every
// work leg, restoring gc.routed_to from each bead's own carried route so ready
// work re-enters pool demand after a controller restart without a manual
// `gc sling` (ga-n2d.4).
//
// This is the BACKSTOP lane. It is called directly at startup — ga-n2d.4's
// restart-recovery contract is exactly a startup backstop — and afterwards from
// the background lane on cadence, never from the tick.
func (cr *CityRuntime) recoverUnroutedWorkRoutes() {
	cr.runRouteRecoveryBackstop(backstopReasonStartup)
}

// runRouteRecoveryBackstop executes one authoritative pass and reports it.
func (cr *CityRuntime) runRouteRecoveryBackstop(reason string) routeRecoveryReport {
	lane := cr.routeRecoveryLaneOf()
	if !lane.beginBackstop() {
		// Another pass is already reading the same state. On a large city the
		// startup scan outlives several poller ticks, and stacking scans would
		// multiply the ledger load to converge once.
		return routeRecoveryReport{lane: "backstop", reason: reason}
	}
	defer lane.endBackstop()
	plan, err := cr.routeRecoveryPlan()
	if err != nil {
		// A refused city is the one case Plan declines to answer, and its remedy
		// is in the error. Scanning the work ledger anyway would be the answer
		// that looks like success while reading the store the relocated classes
		// were migrated off.
		fmt.Fprintf(cr.stderr, "%s: route recovery: resolving work legs: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		lane.force(backstopReasonCursorGap)
		return routeRecoveryReport{lane: "backstop", reason: reason, err: err}
	}
	started := time.Now()
	report := lane.backstopPass(plan, reason)
	report.duration = time.Since(started)
	lane.noteBackstopRan(time.Now(), reason, report.partial || report.err != nil)
	cr.logRouteRecovery(report)
	// The convergence lane always says it ran. A clean pass that logs nothing is
	// indistinguishable from a lane that stopped running, and this one runs on a
	// background goroutine where nothing else would notice.
	summary := fmt.Sprintf("pass reason=%s legs=%d reads=%d candidates=%d restored=%d quarantined=%d off_plane_routed=%d partial=%t took=%s",
		reason, report.legs, report.legReads, report.candidates, report.restored, report.quarantined, report.offPlaneRouted, report.partial,
		report.duration.Round(time.Millisecond))
	fmt.Fprintf(cr.stderr, "%s: route recovery (backstop): %s\n", cr.logPrefix, summary) //nolint:errcheck // best-effort stderr
	return report
}

// recoverUnroutedWorkRoutesDelta is the tick's leg: it repairs only the beads
// the event feed named since the last pass. A steady tick names nothing, builds
// no plan, and issues no store read.
func (cr *CityRuntime) recoverUnroutedWorkRoutesDelta() routeRecoveryReport {
	lane := cr.routeRecoveryLaneOf()
	candidates := lane.takePending()
	if len(candidates) == 0 {
		// deltaPass guards this too, and deliberately: that guard is its API
		// contract (it takes a plan and must be cheap for any caller), while this
		// one is what keeps a steady tick from BUILDING the plan at all. They
		// bound different work, so neither is redundant.
		return routeRecoveryReport{lane: "delta"}
	}
	plan, err := cr.routeRecoveryPlan()
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: route recovery: resolving work legs: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		lane.force(backstopReasonCursorGap)
		return routeRecoveryReport{lane: "delta", candidates: len(candidates), err: err}
	}
	report := lane.deltaPass(plan, candidates)
	if report.partial || report.err != nil {
		// A leg the delta pass could not read is a leg whose convergence is now
		// owed to the scan.
		lane.force(backstopReasonLegDegrade)
	}
	cr.logRouteRecovery(report)
	return report
}

// startTickDeltaLanes arms both halves of every delta lane: the single journal
// feed that makes the tick's delta passes possible, and the background sweeps
// that converge what the feed can miss.
//
// One feed, three lanes plus the completion-fact index. A second watcher on the
// same journal would be a second cursor to keep honest, and the gap semantics
// have to be identical anyway: an event the tail missed is a gap for ALL of
// them.
//
// The sweeps are minutes of sequential remote reads on a large city — together
// 88% of a 373s tick before this slice and its predecessor — so they run off the
// tick's critical path, the same shape as the bead-event watcher and the
// store-maintenance loop. A nil provider leaves every lane sweep-only, which the
// feed records by declaring a gap.
func (cr *CityRuntime) startTickDeltaLanes(ctx context.Context, prov events.Provider) {
	route := cr.routeRecoveryLaneOf()
	completions := cr.completionsLaneOf()
	orphans := cr.detachedOrphanLaneOf()
	watchJournalForDeltaLanes(ctx, prov,
		func() {
			route.force(backstopReasonCursorGap)
			completions.force()
			orphans.force(backstopReasonCursorGap)
			cr.invalidateCompletionFacts()
		},
		func(evt events.Event) {
			route.observe(evt)
			completions.observe(evt)
			orphans.observe(evt)
			cr.absorbCompletionFact(evt)
		})
	go cr.runRouteRecoveryBackstopLoop(ctx, route)
	go cr.runCompletionsSweepLoop(ctx, completions)
	go cr.runDetachedOrphanBackstopLoop(ctx, orphans)
}

// runRouteRecoveryBackstopLoop polls the route-recovery cadence off-tick.
func (cr *CityRuntime) runRouteRecoveryBackstopLoop(ctx context.Context, lane *routeRecoveryLane) {
	ticker := time.NewTicker(lane.pollEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if reason, due := lane.backstopDue(time.Now()); due {
			cr.safeTick(func() { cr.runRouteRecoveryBackstop(reason) }, "route-recovery-backstop")
		}
	}
}

// backstopPollInterval is how often a background convergence lane asks whether
// its scan is due. It bounds the latency between "something forced the backstop"
// and the scan starting; it is not any scan's cadence.
const backstopPollInterval = time.Minute

// logRouteRecovery emits the operator-facing lines. The restored line keeps its
// pre-lane wording so an operator's grep still finds it.
func (cr *CityRuntime) logRouteRecovery(report routeRecoveryReport) {
	if cr.stderr == nil {
		return
	}
	if report.err != nil {
		fmt.Fprintf(cr.stderr, "%s: route recovery (%s): %v\n", cr.logPrefix, report.lane, report.err) //nolint:errcheck // best-effort stderr
	}
	if report.restored > 0 {
		fmt.Fprintf(cr.stderr, "%s: route recovery (%s): restored gc.routed_to on %d ready bead(s) from gc.run_target\n", cr.logPrefix, report.lane, report.restored) //nolint:errcheck // best-effort stderr
	}
	if len(report.flapping) > 0 {
		// The loud half of the flap bound: a route that keeps needing restoring
		// is a sibling lane clearing it, and this lane has stopped writing.
		flap := fmt.Sprintf("STOPPED re-stamping %d flapping bead(s) after %d restores each (%s); another lane is clearing gc.routed_to — see `gc doctor` route-recovery-quarantine",
			len(report.flapping), routeRecoveryFlapLimit, strings.Join(report.flapping, " "))
		fmt.Fprintf(cr.stderr, "%s: route recovery (%s): %s\n", cr.logPrefix, report.lane, flap) //nolint:errcheck // best-effort stderr
	}
	if report.offPlaneRouted > 0 {
		// Loud, because the tick's demand read cannot see these and therefore
		// spawns nothing for them. The remedy is a migration, not a wider tick.
		fmt.Fprintf(cr.stderr, "%s: route recovery (%s): %d open routed bead(s) sit on a work leg the runtime plane does not read, so no pool seat is spawned for them; run `gc storage migrate` to move them to the infra binding\n", cr.logPrefix, report.lane, report.offPlaneRouted) //nolint:errcheck // best-effort stderr
	}
	if report.quarantined > 0 {
		fmt.Fprintf(cr.stderr, "%s: route recovery (%s): quarantined %d bead(s) for operator review (`gc doctor` route-recovery-quarantine)\n", cr.logPrefix, report.lane, report.quarantined) //nolint:errcheck // best-effort stderr
	}
}
