package main

import "fmt"

// The v2 session reconciler owns every session phase of the tick and of the
// startup step (tickPhase.session); the controller keeps the rest. A
// controller that latched v2 runs these lists (v2TickPhases, v2StartupPhases
// in city_runtime_v2.go).

// beadReconcileMaintenancePhases are the steps of beadReconcileTick that are
// maintenance, not session reconciliation: they outlive it under v2.
var beadReconcileMaintenancePhases = []tickPhase{
	{name: "emit_due_compute_facts", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.emitDueComputeFacts(p.ctx, p.sessionBeads.OpenInfos(), false)
		}
		return false
	}},
	{name: "start_historical_transcript_meta_reconcile", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.startHistoricalTranscriptMetaReconcile(p.ctx)
		}
		return false
	}},
	sweepDetachedHandoffOrphansPhase,
	nudgeDispatchTickPhase,
}

// bootBeadReconcileMaintenancePhases is the boot pass's share, as the legacy
// boot reconcile runs it: only the marker-gated terminal usage facts, no
// historical transcript pass, then the detached-orphan delta and the nudge
// fallback.
var bootBeadReconcileMaintenancePhases = []tickPhase{
	{name: "emit_due_compute_facts", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.emitDueComputeFacts(p.ctx, p.sessionBeads.OpenInfos(), true)
		}
		return false
	}},
	sweepDetachedHandoffOrphansPhase,
	nudgeDispatchTickPhase,
}

var sweepDetachedHandoffOrphansPhase = tickPhase{name: "sweep_detached_handoff_orphans", run: func(cr *CityRuntime, p *tickPass) bool {
	if cr.cityBeadStore() != nil {
		cr.runDetachedHandoffOrphansDelta(p.recordPhase)
	}
	return false
}}

var nudgeDispatchTickPhase = tickPhase{name: "nudge_dispatch_tick", run: func(cr *CityRuntime, p *tickPass) bool {
	if cr.cityBeadStore() != nil {
		cr.runNudgeDispatchTick(p.ctx, p.recordPhase)
	}
	return false
}}

// maintenancePhases is phases without its session phases, each replaced by
// its maintenance steps, in order: what the v2 reconciler leaves the
// controller of a legacy tick or startup step.
func maintenancePhases(phases []tickPhase) []tickPhase {
	var kept []tickPhase
	for _, phase := range phases {
		if !phase.session {
			kept = append(kept, phase)
			continue
		}
		kept = append(kept, phase.maintenance...)
	}
	return kept
}

// legacySessionEntry guards each way into the legacy session reconciler:
// the session phases of the tick and the startup step, beadReconcileTick and
// controlDispatcherTick. Under v2 none of them may run, so it refuses the
// entry, counts it, reports each site once on stderr and returns true. Under
// legacy it returns false.
func (cr *CityRuntime) legacySessionEntry(site string) bool {
	if !cr.runsV2() {
		return false
	}
	cr.legacySessionEntries.Add(1)
	if _, logged := cr.legacySessionEntryLogged.LoadOrStore(site, true); !logged {
		fmt.Fprintf(cr.stderr, "%s: refused legacy session reconciler entry %q: this controller runs session_reconciler=v2\n", cr.logPrefix, site) //nolint:errcheck // best-effort stderr
	}
	return true
}
