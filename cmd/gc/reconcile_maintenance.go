package main

// The v2 session reconciler owns every session phase of the tick and of the
// startup step (tickPhase.session); the controller keeps the rest. Nothing
// runs these lists until the v2 runtime is wired behind the
// [daemon].session_reconciler switch.

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
	{name: "sweep_detached_handoff_orphans", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.runDetachedHandoffOrphansDelta(p.recordPhase)
		}
		return false
	}},
	{name: "nudge_dispatch_tick", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.runNudgeDispatchTick(p.ctx, p.recordPhase)
		}
		return false
	}},
}

// bootBeadReconcileMaintenancePhases is the boot pass's share: it emits only
// the marker-gated terminal usage facts, as the legacy boot reconcile does.
var bootBeadReconcileMaintenancePhases = []tickPhase{
	{name: "emit_due_compute_facts", run: func(cr *CityRuntime, p *tickPass) bool {
		if cr.cityBeadStore() != nil {
			cr.emitDueComputeFacts(p.ctx, p.sessionBeads.OpenInfos(), true)
		}
		return false
	}},
}

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
