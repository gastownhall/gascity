package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Regression coverage for gascity#8013: a pool template that had real
// demand but was refused by a workspace/rig/agent cap must not be reported
// as "no_demand" in the per-template reconcile trace summary. These tests
// cover the capture mechanism (RecordCapRejectionReason, unconditional on
// detail-arm state) and the summary classification fix in
// recordReconcileTraceInputs, without touching any cap/admission decision:
// capNewDemandCount's return value and the sessions actually requested are
// unchanged by this fix.

// TestRecordNewDemandCapTrace_CapturesCapRejectionReasonRegardlessOfDetailArm
// pins that the new capture is unconditional: a template that is NOT
// detail-armed still gets its cap reason recorded (so the summary fix works
// on every tick, not only when detail tracing happens to be armed), while
// RecordDecision's own existing detail-gated record is unchanged — it still
// stashes rather than appending to trace.records for an unarmed template.
func TestRecordNewDemandCapTrace_CapturesCapRejectionReasonRegardlessOfDetailArm(t *testing.T) {
	wsMax := 1
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Agents:    []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	agent := &cfg.Agents[0]
	template := agent.QualifiedName()

	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()
	// Fill the workspace cap so the next template's demand is refused.
	usage.accept(SessionRequest{Template: "other/already-accepted", Tier: "new"}, limits)

	trace := newPoolDesiredStateTestTrace() // no templates armed for detail
	scaleCount := 3
	newCount := capNewDemandCount(limits, usage, nestedCapFloorReservations{}, agent, scaleCount)
	if newCount >= scaleCount {
		t.Fatalf("newCount = %d, want < scaleCount=%d (expected a cap refusal in this fixture)", newCount, scaleCount)
	}

	recordNewDemandCapTrace(trace, template, agent, limits, usage, scaleCount, newCount)

	reason, ok := trace.CapRejectionReason(template)
	if !ok {
		t.Fatalf("CapRejectionReason(%q) not recorded; expected a workspace-cap rejection", template)
	}
	if reason != TraceReasonWorkspaceCap {
		t.Errorf("CapRejectionReason(%q) = %q, want %q", template, reason, TraceReasonWorkspaceCap)
	}

	// RecordDecision's own detail-gated record must still be unaffected: the
	// template isn't armed, so the decision was stashed, not appended.
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordDecision && rec.Template == template {
			t.Errorf("RecordDecision unexpectedly appended for an unarmed template; detail-gating must be unchanged: %#v", rec)
		}
	}
}

// recordReconcileTraceInputsTestTrace returns a minimal CityRuntime usable
// to call recordReconcileTraceInputs directly, without a full reconcile
// tick.
func recordReconcileTraceInputsTestCityRuntime() *CityRuntime {
	return &CityRuntime{cfg: &config.City{}}
}

func templateTickSummaryRecord(t *testing.T, trace *sessionReconcilerTraceCycle, template string) SessionReconcilerTraceRecord {
	t.Helper()
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordTemplateTickSummary && rec.Template == template {
			return rec
		}
	}
	t.Fatalf("missing template tick summary for %q; records=%#v", template, trace.records)
	return SessionReconcilerTraceRecord{}
}

func noopRecordPhase(TraceSiteCode, string, time.Time, map[string]any) {}

// TestRecordReconcileTraceInputs_CappedPoolReportsCapReasonNotNoDemand is the
// primary gascity#8013 regression: a template with work_requested=true, all
// counts zero, and a cap rejection already recorded this tick must get that
// specific reason, not the false "no_demand".
func TestRecordReconcileTraceInputs_CappedPoolReportsCapReasonNotNoDemand(t *testing.T) {
	cr := recordReconcileTraceInputsTestCityRuntime()
	trace := newPoolDesiredStateTestTrace()
	template := "rig/late-worker"
	trace.RecordCapRejectionReason(template, TraceReasonWorkspaceCap)

	cr.recordReconcileTraceInputs(
		trace,
		nil, // openInfos
		map[string]TemplateParams{},
		map[string]int{template: 0},
		map[string]bool{},
		map[string]bool{template: true}, // work_requested
		map[string]bool{},
		DesiredStateResult{},
		noopRecordPhase,
	)

	rec := templateTickSummaryRecord(t, trace, template)
	if rec.ReasonCode != TraceReasonWorkspaceCap {
		t.Errorf("reason = %q, want %q (must not be no_demand when work_requested=true and a cap reason is known)", rec.ReasonCode, TraceReasonWorkspaceCap)
	}
	if rec.ReasonCode == TraceReasonNoDemand {
		t.Errorf("reason = %q, a capped pool with real demand must never read as no_demand (gascity#8013)", rec.ReasonCode)
	}
}

// TestRecordReconcileTraceInputs_GenuineNoDemandUnchanged is the control: a
// template with no demand at all (work_requested=false) must keep reporting
// plain no_demand, byte-identical to before this fix.
func TestRecordReconcileTraceInputs_GenuineNoDemandUnchanged(t *testing.T) {
	cr := recordReconcileTraceInputsTestCityRuntime()
	trace := newPoolDesiredStateTestTrace()
	template := "rig/idle-worker"

	cr.recordReconcileTraceInputs(
		trace,
		nil,
		map[string]TemplateParams{},
		map[string]int{template: 0},
		map[string]bool{},
		map[string]bool{template: false},
		map[string]bool{},
		DesiredStateResult{},
		noopRecordPhase,
	)

	rec := templateTickSummaryRecord(t, trace, template)
	if rec.ReasonCode != TraceReasonNoDemand {
		t.Errorf("reason = %q, want %q unchanged for a genuinely idle template", rec.ReasonCode, TraceReasonNoDemand)
	}
}

// TestRecordReconcileTraceInputs_WorkRequestedUnknownCapFallsBackToUnknown
// covers the case where work was requested and refused by some mechanism
// this fix's capture doesn't cover (e.g. applyNestedCaps' own separate
// rejection pass, out of scope here): the summary must not fabricate a cap
// reason it was never told, and must not repeat the false no_demand either.
func TestRecordReconcileTraceInputs_WorkRequestedUnknownCapFallsBackToUnknown(t *testing.T) {
	cr := recordReconcileTraceInputsTestCityRuntime()
	trace := newPoolDesiredStateTestTrace()
	template := "rig/mystery-worker"
	// Deliberately do NOT call trace.RecordCapRejectionReason for this template.

	cr.recordReconcileTraceInputs(
		trace,
		nil,
		map[string]TemplateParams{},
		map[string]int{template: 0},
		map[string]bool{},
		map[string]bool{template: true},
		map[string]bool{},
		DesiredStateResult{},
		noopRecordPhase,
	)

	rec := templateTickSummaryRecord(t, trace, template)
	if rec.ReasonCode != TraceReasonUnknown {
		t.Errorf("reason = %q, want %q (honest fallback, not a guessed cap reason)", rec.ReasonCode, TraceReasonUnknown)
	}
	if rec.ReasonCode == TraceReasonNoDemand {
		t.Errorf("reason = %q, must not report no_demand when work_requested=true, even with an unknown specific cause", rec.ReasonCode)
	}
}
