package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func parityJoinTestRecord(
	traceID, tickID string,
	site TraceSiteCode,
	owner string,
	session string,
	beadID string,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
) SessionReconcilerTraceRecord {
	rec := newTraceRecord(TraceRecordDecision)
	rec.TraceID = traceID
	rec.TickID = tickID
	rec.SiteCode = site
	rec.SessionName = session
	rec.SessionBeadID = beadID
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.Ts = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	rec.Fields["effect_owner"] = owner
	rec.Fields["effect_applied"] = owner == parityJoinOwnerLegacy || owner == parityJoinOwnerKeyed
	return rec
}

// parityJoinTestRollup mirrors the collector's rollup shape
// (session_reconciler_trace_collector.go:970-983): every counter goes into
// rec.Fields, and only the drop-reason map has a typed mirror. Setting the typed
// DetailedTemplateCount instead — a shape nothing in the tree writes — is how
// the mis-arming alarm shipped green while reading zero on every real rollup.
func parityJoinTestRollup(traceID, tickID string, dropReasons map[string]int, detailedTemplates int) SessionReconcilerTraceRecord {
	rec := newTraceRecord(TraceRecordCycleResult)
	rec.TraceID = traceID
	rec.TickID = tickID
	rec.SiteCode = TraceSiteCycleFinish
	rec.Ts = time.Date(2026, 8, 8, 12, 0, 1, 0, time.UTC)
	rec.DropReasonCounts = dropReasons
	rec.Fields["detailed_template_count"] = detailedTemplates
	rec.Fields["drop_reason_counts"] = dropReasons
	for _, count := range dropReasons {
		rec.DroppedRecordCount += count
	}
	return rec
}

func parityJoinFamilyRow(t *testing.T, report parityJoinReport, family string) parityJoinFamilyReport {
	t.Helper()
	for _, row := range report.Families {
		if row.Family == family {
			return row
		}
	}
	t.Fatalf("family %q missing from report families %+v", family, report.Families)
	return parityJoinFamilyReport{}
}

func parityJoinSpecFor(t *testing.T, family string) *parityJoinFamilySpec {
	t.Helper()
	for i := range parityJoinFamilySpecs {
		if parityJoinFamilySpecs[i].Family == family {
			return &parityJoinFamilySpecs[i]
		}
	}
	t.Fatalf("family %q missing from parityJoinFamilySpecs", family)
	return nil
}

func parityJoinTriageCount(report parityJoinReport, family, class string) int {
	for _, entry := range report.Triage {
		if entry.Family == family && entry.Class == class {
			return entry.Count
		}
	}
	return 0
}

// A seeded legacy/detector-shadow pair for the same session and site code in the
// same trace cycle joins into a matched classification with the bead-ID
// cross-check satisfied.
func TestParityJoinMatchesSeededLegacyAndShadowPair(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-1", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-1", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-1", "1", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Joined != 1 || row.Matched != 1 {
		t.Fatalf("joined=%d matched=%d, want 1/1 (%+v)", row.Joined, row.Matched, row)
	}
	if row.Mismatched != 0 || row.Incomparable != 0 || row.LegacyOnly != 0 || row.DetectorOnly != 0 {
		t.Fatalf("unexpected non-matched buckets: %+v", row)
	}
	if !row.BarMet || row.MatchRate != 1 {
		t.Fatalf("bar_met=%v match_rate=%v, want true/1", row.BarMet, row.MatchRate)
	}
	if report.NoEvidence || report.WEBlocker {
		t.Fatalf("no_evidence=%v we_blocker=%v, want false/false", report.NoEvidence, report.WEBlocker)
	}
}

// A deliberately divergent pair classifies as mismatched against the section 3b
// table rather than as an unclassified WE blocker.
func TestParityJoinClassifiesDivergentPairAgainstSection3bTable(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-2", "7", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeDeferredPending),
		parityJoinTestRecord("tr-2", "7", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-2", "7", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Joined != 1 || row.Mismatched != 1 || row.Unclassified != 0 {
		t.Fatalf("joined=%d mismatched=%d unclassified=%d, want 1/1/0 (%+v)", row.Joined, row.Mismatched, row.Unclassified, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyDeadline, "legacy_pending_interaction_deferral"); got != 1 {
		t.Fatalf("triage class count = %d, want 1 (triage=%+v)", got, report.Triage)
	}
	if report.WEBlocker {
		t.Fatalf("we_blocker = true for a classified mismatch")
	}
}

// An unclassified mismatch is reported as a WE blocker with enough evidence to
// extend the table, not silently bucketed.
func TestParityJoinReportsUnclassifiedMismatchAsWEBlocker(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-3", "9", TraceSiteSessionReconcileHealRetire, parityJoinOwnerLegacy, "gc-city-worker-2", "gcs-2", TraceReasonOrphaned, TraceOutcomeHealed),
		parityJoinTestRecord("tr-3", "9", TraceSiteSessionReconcileHealRetire, parityJoinOwnerDetectorShadow, "gc-city-worker-2", "gcs-2", TraceReasonOrphaned, TraceOutcomeNoChange),
		parityJoinTestRollup("tr-3", "9", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDup)
	if row.Mismatched != 1 || row.Unclassified != 1 {
		t.Fatalf("mismatched=%d unclassified=%d, want 1/1 (%+v)", row.Mismatched, row.Unclassified, row)
	}
	if !report.WEBlocker {
		t.Fatalf("we_blocker = false for an unclassified mismatch")
	}
	if len(report.Unclassified) != 1 {
		t.Fatalf("unclassified samples = %d, want 1", len(report.Unclassified))
	}
	sample := report.Unclassified[0]
	if sample.SessionName != "gc-city-worker-2" || sample.LegacyOutcome != string(TraceOutcomeHealed) || sample.DetectorOutcome != string(TraceOutcomeNoChange) {
		t.Fatalf("unclassified sample lacks triage evidence: %+v", sample)
	}
}

// A cycle whose rollup reports record_budget_exceeded drops is excluded from the
// readout rather than counted.
func TestParityJoinExcludesRecordBudgetExceededCycles(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-4", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-4", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-4", "1", map[string]int{"record_budget_exceeded": 3}, 1),
		parityJoinTestRecord("tr-4", "2", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-4", "2", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-4", "2", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.ExcludedRecordBudget != 1 {
		t.Fatalf("excluded_record_budget_exceeded = %d, want 1 (%+v)", report.Cycles.ExcludedRecordBudget, report.Cycles)
	}
	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Joined != 1 {
		t.Fatalf("joined = %d, want 1 (the budget-capped cycle must not be counted)", row.Joined)
	}
}

// An unarmed window records nothing durable, so the readout must be an explicit
// no-evidence result rather than a false all-matched one.
func TestParityJoinUnarmedWindowYieldsNoEvidence(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRollup("tr-5", "1", nil, 0),
		parityJoinTestRollup("tr-5", "2", nil, 0),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if !report.NoEvidence {
		t.Fatalf("no_evidence = false for a window with zero joined cycles")
	}
	if report.Cycles.WithoutDetailArms != 2 {
		t.Fatalf("without_detail_arms = %d, want 2", report.Cycles.WithoutDetailArms)
	}
	for _, row := range report.Families {
		if row.BarMet {
			t.Fatalf("family %q reports bar_met with no evidence: %+v", row.Family, row)
		}
	}

	var out bytes.Buffer
	if err := writeParityJoinReport(&out, report); err != nil {
		t.Fatalf("writeParityJoinReport: %v", err)
	}
	if !strings.Contains(out.String(), "NO EVIDENCE") {
		t.Fatalf("human readout omits the no-evidence verdict:\n%s", out.String())
	}
}

// The bead-ID cross-check guards the normalized-session-name join key: two rows
// sharing a name but not an identity are incomparable, never matched.
func TestParityJoinBeadIDCrossCheckFailureIsIncomparable(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-6", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-6", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-9", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-6", "1", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Matched != 0 || row.Incomparable != 1 {
		t.Fatalf("matched=%d incomparable=%d, want 0/1 (%+v)", row.Matched, row.Incomparable, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyDeadline, parityJoinClassBeadIDCrossCheck); got != 1 {
		t.Fatalf("bead-id cross-check triage count = %d, want 1", got)
	}
}

// Detection-level families predict only (key, condition): a reason/outcome
// divergence is not a mismatch there.
func TestParityJoinDetectionLevelIgnoresReasonAndOutcome(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-7", "1", TraceSiteReconcilerConfigDrift, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonConfigDriftAttached, TraceOutcomeDeferredAttached),
		parityJoinTestRecord("tr-7", "1", TraceSiteReconcilerConfigDrift, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonConfigDrift, TraceOutcomeNoChange),
		parityJoinTestRollup("tr-7", "1", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDrift)
	if row.Level != parityJoinLevelDetection {
		t.Fatalf("D-DRIFT level = %q, want detection", row.Level)
	}
	if row.Matched != 1 || row.Mismatched != 0 {
		t.Fatalf("matched=%d mismatched=%d, want 1/0 (%+v)", row.Matched, row.Mismatched, row)
	}
}

// TestParityJoinCountsKeyedAppliedEffectFromAnUnarmedCity is the census-shaped
// half of ga-f7v2ft.161: the readers key on effect_owner and effect_applied, not
// on the trace tier, so lifting an applied keyed effect to the always-on tier
// has to make it countable with no reader change at all.
//
// The corpus here is not hand-seeded. It is whatever a real keyed handler left
// in a real trace store on a city that armed NOTHING — the only configuration a
// released opt-in reconciler is ever observed in, and the one the soak census
// found empty.
func TestParityJoinCountsKeyedAppliedEffectFromAnUnarmedCity(t *testing.T) {
	records, _ := zombieUnarmedTraceRecords(t, "model_not_found: gpt-5.3-codex-spark")

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.WithoutDetailArms == 0 {
		t.Fatalf("without_detail_arms = 0, want the unarmed shipping shape (%+v)", report.Cycles)
	}
	row := parityJoinFamilyRow(t, report, parityJoinFamilyZombie)
	if row.Keyed != 1 {
		t.Fatalf("D-ZOMBIE keyed = %d, want 1: the census cannot see the opt-in acting (%+v)", row.Keyed, row)
	}
	if report.ShadowEffectViolations != 0 {
		t.Fatalf("shadow_effect_violations = %d, want 0: an always-on keyed effect is not a shadow record", report.ShadowEffectViolations)
	}
}

// Singletons and future keyed-act records are counted per family and kept out of
// the legacy/detector pairing.
func TestParityJoinCountsSingletonsAndKeyedRecords(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-8", "1", TraceSiteReconcilerWakeDecision, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonWake, TraceOutcomeStartCandidate),
		parityJoinTestRecord("tr-8", "1", TraceSiteReconcilerWakeDecision, parityJoinOwnerDetectorShadow, "gc-city-worker-2", "gcs-2", TraceReasonQuarantine, TraceOutcomeDeferredQuarantine),
		parityJoinTestRecord("tr-8", "1", TraceSiteReconcilerWakeDecision, parityJoinOwnerKeyed, "gc-city-worker-3", "gcs-3", TraceReasonWake, TraceOutcomeStartCandidate),
		parityJoinTestRollup("tr-8", "1", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if row.Joined != 0 || row.LegacyOnly != 1 || row.DetectorOnly != 1 || row.Keyed != 1 {
		t.Fatalf("joined=%d legacy_only=%d detector_only=%d keyed=%d, want 0/1/1/1 (%+v)",
			row.Joined, row.LegacyOnly, row.DetectorOnly, row.Keyed, row)
	}
	// The detector-only quarantine skip is expected: legacy never traces it.
	if got := parityJoinTriageCount(report, parityJoinFamilyWake, "untraced_legacy_quarantine_skip"); got != 1 {
		t.Fatalf("quarantine-skip triage count = %d, want 1 (triage=%+v)", got, report.Triage)
	}
}

// D-DRAIN is the one genuinely time-skewed family: the KEYED handler reads the
// ack from inside its own operation while legacy polls the same field in-tick,
// so the two writes land in different cycles and a same-cycle-handle join sees
// one legacy singleton with the keyed record accounted a cycle away.
//
// The join stays a same-cycle join; only the CLASS reaches across, and only to
// the ownership stamp. The three cycles this shape is copied from, and the
// controls proving an unproven skew still blocks WE, are next door in
// cmd_perf_parity_join_corpus_test.go.
func TestParityJoinTriagesDrainAckTimingSkewAcrossAdjacentCycles(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-9", "1", TraceSiteReconcilerDrainAck, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonAcknowledged, TraceOutcomeStopPending),
		parityJoinTestRollup("tr-9", "1", nil, 1),
		parityJoinTestRecord("tr-9", "2", TraceSiteReconcilerDrainAck, parityJoinOwnerKeyed, "gc-city-worker-1", "gcs-1", TraceReasonAcknowledged, TraceOutcomeStopPending),
		parityJoinTestRollup("tr-9", "2", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDrain)
	if row.Joined != 0 || row.LegacyOnly != 1 || row.Keyed != 1 {
		t.Fatalf("joined=%d legacy_only=%d keyed=%d, want 0/1/1 (%+v)", row.Joined, row.LegacyOnly, row.Keyed, row)
	}
	if row.Unclassified != 0 || row.Incomparable != 1 {
		t.Fatalf("unclassified=%d incomparable=%d, want 0/1: the skew proved its keyed twin (%+v)",
			row.Unclassified, row.Incomparable, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyDrain, parityJoinClassDrainAckAdjacentCycleConvergence); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassDrainAckAdjacentCycleConvergence, got, report.Triage)
	}
}

// A detector-shadow record that claims an applied effect breaks the read-only
// invariant the whole campaign rests on; it must be loud, not bucketed.
func TestParityJoinFlagsShadowRecordsClaimingAppliedEffects(t *testing.T) {
	shadow := parityJoinTestRecord("tr-10", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop)
	shadow.Fields["effect_applied"] = true
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-10", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		shadow,
		parityJoinTestRollup("tr-10", "1", nil, 1),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.ShadowEffectViolations != 1 {
		t.Fatalf("shadow_effect_violations = %d, want 1", report.ShadowEffectViolations)
	}
	if !report.WEBlocker {
		t.Fatalf("we_blocker = false despite a shadow record claiming an applied effect")
	}
}

// An unstamped record the elimination rule refuses to attribute is counted,
// never guessed at. lifecycle.start.commit is keyed-owned (section 1 row 27), so
// absence there says nothing about legacy and must not become a join row.
func TestParityJoinCountsUnattributableRecordsInsteadOfGuessing(t *testing.T) {
	unowned := newTraceRecord(TraceRecordDecision)
	unowned.TraceID = "tr-11"
	unowned.TickID = "1"
	unowned.SiteCode = TraceSiteLifecycleStartCommit
	unowned.SessionName = "gc-city-worker-1"
	unowned.SessionBeadID = "gcs-1"
	records := []SessionReconcilerTraceRecord{unowned, parityJoinTestRollup("tr-11", "1", nil, 1)}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.UnownedRecords != 1 || report.Cycles.LegacyByElimination != 0 {
		t.Fatalf("unowned_records=%d legacy_by_elimination=%d, want 1/0 (%+v)",
			report.Cycles.UnownedRecords, report.Cycles.LegacyByElimination, report.Cycles)
	}
	if row := parityJoinFamilyRow(t, report, parityJoinFamilyStart); row.LegacyOnly != 0 {
		t.Fatalf("start legacy_only = %d, want 0 for a keyed-owned site (%+v)", row.LegacyOnly, row)
	}
	if !report.NoEvidence {
		t.Fatalf("no_evidence = false for a trace dir with no joined rows")
	}
}

// A cycle with no rollup record is truncated evidence: excluded, and visibly so.
func TestParityJoinExcludesCyclesWithoutRollup(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-12", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-12", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.ExcludedNoRollup != 1 || report.Cycles.Considered != 0 {
		t.Fatalf("excluded_no_cycle_rollup=%d considered=%d, want 1/0 (%+v)", report.Cycles.ExcludedNoRollup, report.Cycles.Considered, report.Cycles)
	}
	if !report.NoEvidence {
		t.Fatalf("no_evidence = false when every cycle was truncated")
	}
}

// The human readout carries the per-family counts and the triage log the WE
// sign-off needs.
func TestParityJoinHumanReadoutRendersFamilyCountsAndTriage(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-13", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeDeferredPending),
		parityJoinTestRecord("tr-13", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-13", "1", nil, 1),
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	var out bytes.Buffer
	if err := writeParityJoinReport(&out, report); err != nil {
		t.Fatalf("writeParityJoinReport: %v", err)
	}
	text := out.String()
	for _, want := range []string{parityJoinFamilyDeadline, "decision", "legacy_pending_interaction_deferral", "TRIAGE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("human readout missing %q:\n%s", want, text)
		}
	}
}

// End to end through the hidden perf command against a real trace store dir.
func TestParityJoinCommandReadsTraceStoreAndEmitsJSON(t *testing.T) {
	cityPath := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	batch := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-cli", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-cli", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-cli", "1", nil, 1),
	}
	if err := store.AppendBatch(batch, TraceDurabilityDurable); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout bytes.Buffer
	cmd := newPerfParityJoinCmd(&stdout)
	cmd.SetArgs([]string{"--trace-dir", traceCityRuntimeDir(cityPath), "--json"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc perf parity-join: %v", err)
	}

	var report parityJoinReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decoding JSON readout %q: %v", stdout.String(), err)
	}
	if report.SchemaVersion != parityJoinSchemaV1 {
		t.Fatalf("schema_version = %q, want %q", report.SchemaVersion, parityJoinSchemaV1)
	}
	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Joined != 1 || row.Matched != 1 {
		t.Fatalf("joined=%d matched=%d, want 1/1", row.Joined, row.Matched)
	}
}

// An unarmed trace dir must fail the command, not return success with an empty
// all-matched table.
func TestParityJoinCommandFailsOnNoEvidence(t *testing.T) {
	cityPath := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{parityJoinTestRollup("tr-empty", "1", nil, 0)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout bytes.Buffer
	cmd := newPerfParityJoinCmd(&stdout)
	cmd.SetArgs([]string{"--trace-dir", traceCityRuntimeDir(cityPath)})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc perf parity-join returned success for an unarmed window:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "NO EVIDENCE") {
		t.Fatalf("readout omits the no-evidence verdict:\n%s", stdout.String())
	}
}

// The command declares JSON support, and because its result schema is JSONL the
// front door leaves the readout on stdout even though a WE blocker exits
// nonzero — a buffered command would have its report replaced by the shared
// failure envelope, which is the one thing the campaign cannot afford.
func TestParityJoinDeclaresJSONLSupportSoTheReadoutSurvivesANonzeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"perf", "parity-join", "--json-schema"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(perf parity-join --json-schema) = %d, stderr=%q", code, stderr.String())
	}
	var manifest struct {
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if !manifest.JSONSupported || !json.Valid(manifest.Schemas["result"]) {
		t.Fatalf("perf parity-join does not declare JSON support: %s", stdout.String())
	}
	var schema struct {
		JSONL *json.RawMessage `json:"x-gc-jsonl"`
	}
	if err := json.Unmarshal(manifest.Schemas["result"], &schema); err != nil {
		t.Fatalf("result schema is not JSON: %v", err)
	}
	if schema.JSONL == nil {
		t.Fatalf("result schema must declare x-gc-jsonl so a nonzero verdict keeps the readout")
	}
}

func TestParityJoinExcludeWindowFlagParsing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  string
		want  parityJoinWindow
		error string
	}{
		{
			name: "the day-4 restart window",
			spec: "2026-08-15T05:59:50Z/2026-08-15T06:02:40Z",
			want: parityJoinWindow{
				Start: time.Date(2026, 8, 15, 5, 59, 50, 0, time.UTC),
				End:   time.Date(2026, 8, 15, 6, 2, 40, 0, time.UTC),
			},
		},
		{
			name: "an offset window is normalized to UTC",
			spec: "2026-08-15T01:59:50-04:00/2026-08-15T02:02:40-04:00",
			want: parityJoinWindow{
				Start: time.Date(2026, 8, 15, 5, 59, 50, 0, time.UTC),
				End:   time.Date(2026, 8, 15, 6, 2, 40, 0, time.UTC),
			},
		},
		{name: "no separator", spec: "2026-08-15T05:59:50Z", error: "want <RFC3339-start>/<RFC3339-end>"},
		{name: "bad start", spec: "06:00/2026-08-15T06:02:40Z", error: "parsing window start"},
		{name: "bad end", spec: "2026-08-15T05:59:50Z/soon", error: "parsing window end"},
		{name: "empty window", spec: "2026-08-15T06:02:40Z/2026-08-15T06:02:40Z", error: "must be after"},
		{name: "reversed window", spec: "2026-08-15T06:02:40Z/2026-08-15T05:59:50Z", error: "must be after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseParityJoinExcludeWindow(tc.spec)
			if tc.error != "" {
				if err == nil {
					t.Fatalf("parsed %q as %+v, want an error mentioning %q", tc.spec, got, tc.error)
				}
				if !strings.Contains(err.Error(), tc.error) {
					t.Fatalf("error = %q, want it to mention %q", err, tc.error)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.spec, err)
			}
			if !got.Start.Equal(tc.want.Start) || !got.End.Equal(tc.want.End) {
				t.Fatalf("window = %s/%s, want %s/%s", got.Start, got.End, tc.want.Start, tc.want.End)
			}
		})
	}
}

// The flag reaches the report, and the report says so: a readout that excluded
// part of its own corpus is only citable if the artifact carries the window.
func TestParityJoinCommandRecordsItsExclusionWindowsInTheReport(t *testing.T) {
	cityPath := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dropped := parityJoinTestRecord("tr-gap", "2", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-2", "gcs-2", TraceReasonIdleTimeout, TraceOutcomeStop)
	dropped.Ts = time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)
	batch := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-cli", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-cli", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-cli", "1", nil, 1),
		dropped,
		parityJoinTestRollup("tr-gap", "2", nil, 1),
	}
	if err := store.AppendBatch(batch, TraceDurabilityDurable); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout bytes.Buffer
	cmd := newPerfParityJoinCmd(&stdout)
	cmd.SetArgs([]string{
		"--trace-dir", traceCityRuntimeDir(cityPath), "--json",
		"--exclude-window", "2026-08-08T12:59:50Z/2026-08-08T13:00:10Z",
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc perf parity-join: %v", err)
	}

	var report parityJoinReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decoding JSON readout %q: %v", stdout.String(), err)
	}
	if len(report.ExcludedWindows) != 1 || report.ExcludedWindows[0].RecordsExcluded != 1 {
		t.Fatalf("excluded_windows = %+v, want one window carrying 1 excluded record", report.ExcludedWindows)
	}
	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.Joined != 1 || row.Matched != 1 || row.LegacyOnly != 0 {
		t.Fatalf("D-DEADLINE = %+v, want the out-of-window pair joined and the in-window singleton gone", row)
	}
}

// An unpassed --exclude-window changes nothing about the readout, in either
// rendering. Every campaign artifact filed before the flag existed has to stay
// comparable to one filed after it.
func TestParityJoinReadoutIsUnchangedWithoutAnExclusionWindow(t *testing.T) {
	records := []SessionReconcilerTraceRecord{
		parityJoinTestRecord("tr-same", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerLegacy, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRecord("tr-same", "1", TraceSiteReconcilerIdleTimeout, parityJoinOwnerDetectorShadow, "gc-city-worker-1", "gcs-1", TraceReasonIdleTimeout, TraceOutcomeStop),
		parityJoinTestRollup("tr-same", "1", nil, 1),
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshaling report: %v", err)
	}
	if strings.Contains(string(encoded), "excluded_windows") {
		t.Fatalf("JSON readout carries excluded_windows with no window passed:\n%s", encoded)
	}

	var table bytes.Buffer
	if err := writeParityJoinReport(&table, report); err != nil {
		t.Fatalf("writeParityJoinReport: %v", err)
	}
	if strings.Contains(table.String(), "excluded before the join") {
		t.Fatalf("human readout carries an exclusion line with no window passed:\n%s", table.String())
	}

	// An explicitly empty window list is the same corpus and must render the
	// same bytes as no list at all.
	var empty bytes.Buffer
	if err := writeParityJoinReport(&empty, buildParityJoinReport(records, parityJoinOptions{
		Bar: 0.995, Samples: 4, ExcludedWindows: []parityJoinWindow{},
	})); err != nil {
		t.Fatalf("writeParityJoinReport: %v", err)
	}
	if empty.String() != table.String() {
		t.Fatalf("empty window list changed the readout:\n%s\nwant:\n%s", empty.String(), table.String())
	}
}

// Every site the section 3b table claims must resolve to exactly one family, and
// every family must declare a parity level.
func TestParityJoinFamilyTableIsWellFormed(t *testing.T) {
	seen := map[TraceSiteCode]string{}
	for _, spec := range parityJoinFamilySpecs {
		if spec.Level != parityJoinLevelDetection && spec.Level != parityJoinLevelDecision && spec.Level != parityJoinLevelAct {
			t.Fatalf("family %q has level %q", spec.Family, spec.Level)
		}
		if len(spec.Sites) == 0 {
			t.Fatalf("family %q claims no trace sites", spec.Family)
		}
		for _, site := range spec.Sites {
			if other, ok := seen[site]; ok {
				t.Fatalf("site %q claimed by both %q and %q", site, other, spec.Family)
			}
			seen[site] = spec.Family
		}
	}
}

// A yield names the family legacy stood down FOR, and the join compares that
// name against the family the actor claims. Both vocabularies must therefore
// speak the section 3b family names: a typo would turn every pair in that family
// into a yield_family_mismatch, which is precisely the alarm the check exists to
// keep meaningful. Every entry must also cite the seam it transcribes.
func TestParityJoinYieldAndDetectorVocabulariesNameRealFamilies(t *testing.T) {
	families := map[string]bool{}
	for _, spec := range parityJoinFamilySpecs {
		families[spec.Family] = true
	}
	for reason, spec := range parityJoinYieldVocabulary {
		if !families[spec.Family] {
			t.Fatalf("yield %q claims family %q, which no section 3b family spec declares", reason, spec.Family)
		}
		switch spec.Arm {
		case parityJoinYieldCandidacy, parityJoinYieldOwnership:
		default:
			t.Fatalf("yield %q has arm %q", reason, spec.Arm)
		}
		if strings.TrimSpace(spec.Note) == "" {
			t.Fatalf("yield %q cites no emitting seam", reason)
		}
	}
	for label, family := range parityJoinDetectorFamilies {
		if !families[family] {
			t.Fatalf("detector family label %q maps to %q, which no section 3b family spec declares", label, family)
		}
	}
}

// The elimination rule is only as good as its guard, so every site the section
// 3b table joins on must carry an explicit section 1 disposition. A site added
// to a family without one would silently default to "not legacy" and drop that
// family's whole legacy population on the floor.
func TestParityJoinEverySection3bSiteHasASection1Disposition(t *testing.T) {
	for _, spec := range parityJoinFamilySpecs {
		for _, site := range spec.Sites {
			disposition, ok := parityJoinSiteDispositions[site]
			if !ok {
				t.Fatalf("site %q (family %q) has no section 1 disposition", site, spec.Family)
			}
			switch disposition.Attribution {
			case parityJoinSiteLegacy, parityJoinSitePhase, parityJoinSiteNonLegacy:
			default:
				t.Fatalf("site %q has attribution %q", site, disposition.Attribution)
			}
			if strings.TrimSpace(disposition.Note) == "" {
				t.Fatalf("site %q states no section 1 evidence for its disposition", site)
			}
		}
	}
	for site := range parityJoinSiteDispositions {
		if _, ok := parityJoinSiteFamily[site]; !ok {
			t.Fatalf("site %q carries a disposition but no section 3b family claims it", site)
		}
	}
}
