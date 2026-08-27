package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// These tests are the WD.15 day-0 lesson written down. The suite next door
// builds records by setting struct fields the collector never sets — a shape
// production cannot produce — which is how a blind reader (the typed
// DetailedTemplateCount) and a blind classifier (owner-absent records dropped
// on the floor) both shipped green. Everything here is production-shaped:
// either byte-copied from the live campaign corpus at
// /data/cities/reconciler-campaign, or written by the real collector and read
// back through the real store, so the JSON round-trip is part of the test.

const parityJoinCorpusFixture = "testdata/wd15_campaign_corpus.jsonl"

// parityJoinRestartGapFixture holds the two cycles that straddle the day-4
// mid-window controller restart: the last good cycle of the outgoing instance
// (05:57:34-05:58:00, three D-DEADLINE pairs plus a D-ORPHAN pair for
// dependent-rc-5kfx6) and the incoming instance's very first tick
// (06:01:16-06:01:25), whose half-armed close_orphan record for that same
// session is the restart artifact the exclusion window exists to drop.
const parityJoinRestartGapFixture = "testdata/wd15_restart_gap_corpus.jsonl"

// parityJoinDrainAckTwinFixture holds the three D-DRAIN ack-timing skews the
// owner adjudicated on day 6 and signed on 2026-08-17/18, each with the keyed
// acknowledgement it has to prove. Every line is byte-copied from
// campaign/trace-archive: for each specimen, legacy's own drain_ack decision
// and the keyed handler's drain_ack operation one cycle over, each beside its
// cycle's rollup so the cycle survives the join's own filters.
const parityJoinDrainAckTwinFixture = "testdata/wd15_drain_ack_twin_corpus.jsonl"

// parityJoinCorpusRecords decodes the byte-copied campaign corpus fixture.
func parityJoinCorpusRecords(t *testing.T) []SessionReconcilerTraceRecord {
	t.Helper()
	return parityJoinCorpusFixtureRecords(t, parityJoinCorpusFixture)
}

// parityJoinCorpusFixtureRecords decodes one byte-copied campaign fixture.
func parityJoinCorpusFixtureRecords(t *testing.T, path string) []SessionReconcilerTraceRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening campaign corpus fixture: %v", err)
	}
	defer f.Close() //nolint:errcheck
	var out []SessionReconcilerTraceRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec SessionReconcilerTraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decoding campaign corpus line: %v", err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading campaign corpus fixture: %v", err)
	}
	return out
}

// parityJoinArmTemplate installs the detail arm the collector requires before it
// promotes a record. Without it the record path stashes and returns
// (trace_collector.go:335-353) and the whole cycle reads empty — the campaign's
// original arming gap, reproduced in miniature.
func parityJoinArmTemplate(t *testing.T, cityDir, template string) {
	t.Helper()
	now := time.Now().UTC()
	arms := newSessionReconcilerTraceArmStore(cityDir)
	if _, err := arms.upsertArm(TraceArm{
		ScopeType:  TraceArmScopeTemplate,
		ScopeValue: template,
		Source:     TraceArmSourceManual,
		Level:      TraceModeDetail,
		ArmedAt:    now,
		ExpiresAt:  now.Add(30 * time.Minute),
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("upsertArm: %v", err)
	}
}

func parityJoinDispositionCount(report parityJoinReport, site TraceSiteCode, disposition string) int {
	total := 0
	for _, entry := range report.Dispositions {
		if entry.Site == site && entry.Disposition == disposition {
			total += entry.Count
		}
	}
	return total
}

// B1. Nothing in the tree stamps effect_owner="legacy": every keyed handler
// stamps keyed, the sweep stamps detector-shadow, and the god function stamps
// nothing. The tool therefore classifies owner-ABSENT records at legacy trace
// sites as legacy by elimination, and the live corpus's legacy population
// appears where day-0 saw only unowned_records.
func TestParityJoinAttributesUnstampedLegacyRecordsOnTheLiveCorpus(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	if report.Cycles.LegacyByElimination != 14 {
		t.Fatalf("legacy_by_elimination = %d, want 14 (2x drain/no-wake-reason, 4x wake_decision/wake, drain_ack/acknowledged, drain_ack/orphaned, drain.timeout/orphaned, rollback_pending_create/recovery, 3x idle_timeout/stop, reset_stalled/failed — %+v)",
			report.Cycles.LegacyByElimination, report.Cycles)
	}

	// The cycle where legacy and the sweep both wrote for the same session at
	// the same site joins, and lands in a real section 3b class.
	sleep := parityJoinFamilyRow(t, report, parityJoinFamilySleep)
	if sleep.Joined != 1 {
		t.Fatalf("D-SLEEP joined = %d, want 1 (%+v)", sleep.Joined, sleep)
	}
	if sleep.Unclassified != 0 {
		t.Fatalf("D-SLEEP unclassified = %d, want 0 (%+v)", sleep.Unclassified, sleep)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilySleep, "fleet_only_no_wake_left_to_legacy"); got != 2 {
		t.Fatalf("fleet-only triage count = %d, want 2 (the joined pair and the legacy-only singleton) (triage=%+v)", got, report.Triage)
	}

	// Legacy's own acknowledgement singleton stays a singleton and reaches the
	// D-DRAIN table as a legacy row, not as an unowned_record. What the table
	// then does with it is parityJoinDrainAckUnprovenSingleton's business.
	if drain := parityJoinFamilyRow(t, report, parityJoinFamilyDrain); drain.LegacyOnly != 2 {
		t.Fatalf("D-DRAIN legacy_only = %d, want 2 (worker-rc-6nq's ack and dependent-rc-1vd's) (%+v)",
			drain.LegacyOnly, drain)
	}
}

// The owner ruling's yield-join: legacy's traced stand-down beside the keyed
// actor's record for the same row in the same tick. Both of these pairs are
// byte-copied from the live campaign corpus, where the D-DEADLINE seam alone
// produces thousands of them per hour — the evidence auto mode actually makes.
func TestParityJoinPairsLegacyYieldsAgainstTheActorOnTheLiveCorpus(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	deadline := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if deadline.YieldJoined != 1 || deadline.Matched != 1 {
		t.Fatalf("D-DEADLINE yield_joined=%d matched=%d, want 1/1 — keyed_deadline_owner beside detector_idle_timeout (%+v)",
			deadline.YieldJoined, deadline.Matched, deadline)
	}
	orphan := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if orphan.YieldJoined != 1 || orphan.Matched != 1 {
		t.Fatalf("D-ORPHAN yield_joined=%d matched=%d, want 1/1 — keyed_orphan_drain_owner beside detector_orphan_live (%+v)",
			orphan.YieldJoined, orphan.Matched, orphan)
	}
	if report.JoinedYields != 2 {
		t.Fatalf("joined_yields = %d, want 2 (%+v)", report.JoinedYields, report.Families)
	}
	if report.JoinedActs == 0 {
		t.Fatal("joined_acts = 0: the act-vs-act join must survive beside the yield-join")
	}

	// The yield vocabulary is reported, not just consumed.
	var deadlineYield *parityJoinYieldEntry
	for i := range report.Yields {
		if report.Yields[i].Reason == "keyed_deadline_owner" {
			deadlineYield = &report.Yields[i]
		}
	}
	if deadlineYield == nil {
		t.Fatalf("yields log has no keyed_deadline_owner entry: %+v", report.Yields)
	}
	if deadlineYield.Arm != parityJoinYieldCandidacy || deadlineYield.Joined != 1 {
		t.Fatalf("keyed_deadline_owner entry = %+v, want candidacy arm with joined=1", *deadlineYield)
	}
}

// A stand-down that only asserts ownership is not evidence. The wake seam fires
// at the top of the row scan before any condition is evaluated, and its two arms
// are indistinguishable in the record, so an unpaired one is counted and
// surfaced rather than scored as a divergence — the same discipline that keeps
// phase markers out of D-DUP and D-STRANDED.
func TestParityJoinRefusesToScoreUnpairedOwnershipYields(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	// Two: the day-1 cycle where legacy stood down to the keyed start owner with
	// nothing else beside it, and the day-2 preserve cycle where the stand-down
	// is the family split's twin. Being a twin does not make a yield PAIRED —
	// the twin sits in another family at another site, so the yield itself is
	// still refused rather than scored.
	if report.Cycles.UnpairedOwnershipYields != 2 {
		t.Fatalf("unpaired_ownership_yields = %d, want 2 (%+v)", report.Cycles.UnpairedOwnershipYields, report.Cycles)
	}
	wake := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if wake.YieldOnly != 0 || wake.Mismatched != 0 {
		t.Fatalf("D-WAKE yield_only=%d mismatched=%d, want 0/0 — an ownership yield is not a divergence (%+v)",
			wake.YieldOnly, wake.Mismatched, wake)
	}
	var entry *parityJoinYieldEntry
	for i := range report.Yields {
		if report.Yields[i].Reason == "keyed_start_owner" {
			entry = &report.Yields[i]
		}
	}
	if entry == nil || entry.Arm != parityJoinYieldOwnership || entry.Unpaired != 2 {
		t.Fatalf("keyed_start_owner yields entry = %+v, want ownership arm with unpaired=2 (%+v)", entry, report.Yields)
	}
}

// The sweep stamps effect_owner=keyed for a condition it ROUTED, so before the
// ruling every routed family's record fell into the keyed column and joined
// nothing. Widening the actor side to the sweep's own records turns three of
// ga-f7v2ft.158's legacy-only shapes into act-pairs.
func TestParityJoinJoinsRoutedSweepRecordsAgainstLegacyActs(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	wake := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if wake.Joined != 3 || wake.Matched != 2 {
		t.Fatalf("D-WAKE joined=%d matched=%d, want 3/2 — legacy wake/start_candidate beside the routed detector_wake_target, plus two admission-refused pairs (%+v)",
			wake.Joined, wake.Matched, wake)
	}
	drain := parityJoinFamilyRow(t, report, parityJoinFamilyDrain)
	if drain.Joined != 1 {
		t.Fatalf("D-DRAIN joined = %d, want 1 — legacy orphaned/stop_pending beside the routed detector_drain_in_flight (%+v)",
			drain.Joined, drain)
	}
}

// Decision-level parity compares the OUTCOME. The two writers' reason
// vocabularies are disjoint by construction, so the old reason-equality clause
// could only ever match a seeded pair — on the live corpus it turned every
// decision-level act-pair into an unclassified WE blocker.
func TestParityJoinDecisionLevelMatchesOnOutcomeNotReason(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	wake := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if wake.Unclassified != 0 {
		t.Fatalf("D-WAKE unclassified = %d, want 0: legacy (wake, start_candidate) and the sweep's (detector_wake_target, start_candidate) decide the same thing (%+v)",
			wake.Unclassified, wake)
	}
	for _, sample := range report.Unclassified {
		if sample.LegacyReason != "" && sample.DetectorReason != "" {
			t.Fatalf("a joined pair was left unclassified on a reason difference alone: %+v", sample)
		}
	}
}

// The cross-family split ga-f7v2ft.158 filed as two separate shapes. The sweep
// claims each row for ONE family; legacy runs an arm per pass. A start-pending
// row with a live create lease is claimed by D-STALE-CREATE's preserve arm while
// legacy's wake pass drives the start already in flight — same row, same tick,
// two families, two sites. The rule fires only because the twin is in the cycle.
func TestParityJoinTriagesThePendingCreateFamilySplitAgainstItsTwin(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	// D-WAKE keeps one: legacy's own wake act, whose preserve twin is the
	// detector's. D-STALE-CREATE has two, one per spelling of the start-family
	// twin (legacy's wake act on day 1, the keyed_start_owner stand-down on
	// day 2 — see TestParityJoinTriagesThePendingCreateSplitUnderKeyedStartOwnership).
	for _, tc := range []struct {
		family string
		want   int
	}{{parityJoinFamilyWake, 1}, {parityJoinFamilyStaleCreate, 2}} {
		if got := parityJoinTriageCount(report, tc.family, parityJoinClassPendingCreateFamilySplit); got != tc.want {
			t.Fatalf("%s %s = %d, want %d (triage=%+v)", tc.family, parityJoinClassPendingCreateFamilySplit, got, tc.want, report.Triage)
		}
		if row := parityJoinFamilyRow(t, report, tc.family); row.Unclassified != 0 {
			t.Fatalf("%s unclassified = %d, want 0 (%+v)", tc.family, row.Unclassified, row)
		}
	}
}

// The rest of ga-f7v2ft.158's survivors: the advance engine's own arms, which
// the sweep detects one site away, and legacy's live-runtime recovery deferral,
// which the sweep excludes from the family by construction.
func TestParityJoinTriagesTheRemainingLegacyOnlySingletons(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	// Two arms of one class: legacy's drain.timeout completion beside the
	// sweep's drain_ack detection, and that detection itself, which has no
	// legacy per-session twin because legacy's advance pass is a phase site.
	if got := parityJoinTriageCount(report, parityJoinFamilyDrain, "advance_arms_journey_proven"); got != 2 {
		t.Fatalf("advance_arms_journey_proven = %d, want 2 (triage=%+v)", got, report.Triage)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyStaleCreate, "live_runtime_recovery_excluded_from_sweep"); got != 1 {
		t.Fatalf("live_runtime_recovery_excluded_from_sweep = %d, want 1 (triage=%+v)", got, report.Triage)
	}
	// The fixture triages clean apart from the one singleton the corpus cannot
	// answer for; see parityJoinDrainAckUnprovenSingleton.
	for _, sample := range report.Unclassified {
		if sample.SessionName != parityJoinDrainAckUnprovenSingleton {
			t.Fatalf("unexpected unclassified mismatch in the campaign corpus fixture: %+v", sample)
		}
	}
}

// parityJoinDrainAckUnprovenSingleton is the campaign fixture's day-0 legacy
// drain acknowledgement (cycle-c10ea5757924016e, tick ...-000030,
// 2026-08-12T03:39:39.085Z, acknowledged/stop_pending).
//
// It is the one row in the fixture that drain_ack_adjacent_cycle_convergence
// cannot decide, and the reason is curation, not divergence: the fixture samples
// 18 individual cycles rather than cycle neighborhoods, and this record predates
// campaign/trace-archive by a day — the archive's first segment is
// 2026-08-13T08:51:27Z. The adjacent cycles that would carry or refute its keyed
// twin exist nowhere on disk any more, so the corpus cannot prove the twin and
// the tool refuses to assume it. That refusal is the class working as signed: an
// unprovable ack is not an excused ack.
//
// Do NOT relax the class to clear this row. The three cycles the owner actually
// adjudicated verify against the live archive
// (TestParityJoinTriagesTheAdjudicatedDrainAckSkewsAgainstTheirKeyedTwins), and
// laundering an unprovable row into `incomparable` here would take the only
// alarm this seam has left off the one shape it exists to catch.
const parityJoinDrainAckUnprovenSingleton = "worker-rc-6nq"

// ga-f7v2ft.158's last survivors: three sibling pool sessions legacy idle-killed
// in one tick with no detector record anywhere in the corpus. The fixture cycle
// is byte-copied from the live campaign window, where its sweep evaluated all
// three rows (rows_evaluated=7, unknown_state_skipped=3, conditions=4 — the 3
// unknown-state skips plus one unrelated drain_ack) and raised nothing, and
// legacy then stopped them 1.05s, 3.88s and 7.25s later in the same tick.
//
// They classify, and they classify as MISMATCHED: the divergence is explained,
// not excused. Laundering a legacy-only destructive stop into `incomparable`
// would take it out of the family's match rate, which is the only number left
// policing this seam.
func TestParityJoinTriagesTheDeadlineCrossingRace(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	deadline := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if deadline.LegacyOnly != 3 {
		t.Fatalf("D-DEADLINE legacy_only = %d, want 3 (%+v)", deadline.LegacyOnly, deadline)
	}
	if deadline.Unclassified != 0 {
		t.Fatalf("D-DEADLINE unclassified = %d, want 0 (%+v)", deadline.Unclassified, deadline)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyDeadline, parityJoinClassDeadlineCrossedAfterSweepSample); got != 3 {
		t.Fatalf("%s = %d, want 3 (triage=%+v)", parityJoinClassDeadlineCrossedAfterSweepSample, got, report.Triage)
	}
	if deadline.Mismatched != 3 {
		t.Fatalf("D-DEADLINE mismatched = %d, want 3: an explained divergence still counts against the bar (%+v)",
			deadline.Mismatched, deadline)
	}
	if deadline.Incomparable != 0 {
		t.Fatalf("D-DEADLINE incomparable = %d, want 0: a legacy-only destructive stop must not leave the match rate (%+v)",
			deadline.Incomparable, deadline)
	}
}

// The rule's blast radius. It fires on legacy's idle-timeout STOP and nothing
// else: a deferral keeps its own section 3b class, and the max-age arm — for
// which this window produced no evidence either way — stays unclassified rather
// than riding a rule written from the idle arm's corpus.
func TestParityJoinDeadlineCrossingRaceDoesNotSwallowNeighbouringArms(t *testing.T) {
	spec := parityJoinSpecFor(t, parityJoinFamilyDeadline)

	deferral := SessionReconcilerTraceRecord{
		SiteCode:    TraceSiteReconcilerIdleTimeout,
		ReasonCode:  TraceReasonCode("idle_timeout"),
		OutcomeCode: TraceOutcomeDeferred,
	}
	_, class := parityJoinClassify(spec, parityJoinSideLegacyOnly, parityJoinRowContext{}, &deferral, nil)
	if class != "legacy_pending_interaction_deferral" {
		t.Fatalf("legacy idle deferral classified as %q, want legacy_pending_interaction_deferral", class)
	}

	maxAge := SessionReconcilerTraceRecord{
		SiteCode:    TraceSiteReconcilerMaxSessionAge,
		ReasonCode:  TraceReasonCode("max_session_age"),
		OutcomeCode: TraceOutcomeStop,
	}
	_, class = parityJoinClassify(spec, parityJoinSideLegacyOnly, parityJoinRowContext{}, &maxAge, nil)
	if class != parityJoinClassUnclassified {
		t.Fatalf("legacy max-age stop classified as %q, want UNCLASSIFIED — the idle arm's evidence does not cover it", class)
	}
}

// The day-2 half of the pending-create family split. The class's first arm was
// written from cycles where legacy's OWN wake pass drove the in-flight start;
// under keyed start ownership legacy does not drive it, it stands down
// (keyed_start_owner, session_reconciler.go:1880-1887 / :4091-4098) — so the
// twin the rule required was never in the cycle and the preserve record fell
// through to UNCLASSIFIED. Same split, same two writers agreeing to leave one
// in-flight start alone; only the identity of the start's driver differs.
//
// Byte-copied from cycle-7edba7f31f6960ea (s-rc-wisp-s08pap), whose baseline
// and result are both state=creating.
func TestParityJoinTriagesThePendingCreateSplitUnderKeyedStartOwnership(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	staleCreate := parityJoinFamilyRow(t, report, parityJoinFamilyStaleCreate)
	if staleCreate.Unclassified != 0 {
		t.Fatalf("D-STALE-CREATE unclassified = %d, want 0 (%+v)", staleCreate.Unclassified, staleCreate)
	}
	// Two arms of one class now: the legacy-act twin and the keyed-ownership
	// stand-down twin.
	if got := parityJoinTriageCount(report, parityJoinFamilyStaleCreate, parityJoinClassPendingCreateFamilySplit); got != 2 {
		t.Fatalf("%s = %d, want 2 (triage=%+v)", parityJoinClassPendingCreateFamilySplit, got, report.Triage)
	}
	// No effect on either side, so it must not enter the family's match rate.
	if staleCreate.Incomparable != 3 {
		t.Fatalf("D-STALE-CREATE incomparable = %d, want 3 (%+v)", staleCreate.Incomparable, staleCreate)
	}
}

// The twin requirement still bites. A preserve record with NO start-family
// record beside it in the cycle is exactly the candidacy gap this alarm exists
// to catch, and neither arm of the rule may absorb it.
func TestParityJoinPendingCreateSplitStillRequiresAStartFamilyTwin(t *testing.T) {
	spec := parityJoinSpecFor(t, parityJoinFamilyStaleCreate)

	preserve := SessionReconcilerTraceRecord{
		SiteCode:    TraceSiteReconcilerPendingCreatePreserved,
		ReasonCode:  detectorReasonPendingCreatePreserved,
		OutcomeCode: TraceOutcomeNoChange,
	}
	_, class := parityJoinClassify(spec, parityJoinSideDetectorOnly, parityJoinRowContext{}, nil, &preserve)
	if class != parityJoinClassUnclassified {
		t.Fatalf("a twinless preserve record classified as %q, want UNCLASSIFIED", class)
	}
	for _, twin := range []TraceReasonCode{TraceReasonWake, "keyed_start_owner"} {
		ctx := parityJoinRowContext{coTwins: map[TraceReasonCode]bool{twin: true}}
		_, class = parityJoinClassify(spec, parityJoinSideDetectorOnly, ctx, nil, &preserve)
		if class != parityJoinClassPendingCreateFamilySplit {
			t.Fatalf("preserve record with twin %q classified as %q, want %s", twin, class, parityJoinClassPendingCreateFamilySplit)
		}
	}
}

// The last day-2 shape: legacy's reset-stall ALARM, which the join table filed
// under D-STALL by site adjacency and which no detector record can ever pair.
//
// Two independent reasons, both re-derived at source:
//  1. recordResetStallIfDue fires only for a NOT-alive row past the startup
//     timeout (session_reconciler.go:223), while detectStall returns unless
//     live.Alive (session_detector_sweep.go:1710). Disjoint populations.
//  2. The site is an alarm, not a decision: the function prints, records a
//     SessionResetStalled event and traces. It mutates nothing, so there is no
//     effect for a keyed handler to own or to double-apply.
//
// Byte-copied from cycle-11e1730d6990ad8d (s-rc-wisp-u71tke, baseline asleep /
// sleep_reason killed), the ONLY reset-stall record in the 13,757-cycle window.
// The same cycle carries the sweep's own D-WAKE condition for the same row, so
// the missing D-STALL record is not the sweep skipping the row.
func TestParityJoinTriagesTheResetStallAlarm(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	stall := parityJoinFamilyRow(t, report, parityJoinFamilyStall)
	if stall.LegacyOnly != 1 {
		t.Fatalf("D-STALL legacy_only = %d, want 1 (%+v)", stall.LegacyOnly, stall)
	}
	if stall.Unclassified != 0 {
		t.Fatalf("D-STALL unclassified = %d, want 0 (%+v)", stall.Unclassified, stall)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyStall, parityJoinClassResetStallAlarmNoDetectorArm); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassResetStallAlarmNoDetectorArm, got, report.Triage)
	}
	if stall.Mismatched != 0 || stall.Incomparable != 1 {
		t.Fatalf("D-STALL mismatched=%d incomparable=%d, want 0/1 — an alarm that mutates nothing has no effect to compare (%+v)",
			stall.Mismatched, stall.Incomparable, stall)
	}
}

// The alarm rule's blast radius. It covers legacy's reset-stall record and
// nothing else in the family: the progress-stall arms both write at
// ProgressStallExempt (DETECTOR.md section 3 delta 2) and keep their own
// classification, and a keyed record ever appearing at the alarm site would
// stay unclassified rather than ride this rule.
func TestParityJoinResetStallAlarmDoesNotSwallowTheProgressStallArms(t *testing.T) {
	spec := parityJoinSpecFor(t, parityJoinFamilyStall)

	exempt := SessionReconcilerTraceRecord{
		SiteCode:    TraceSiteReconcilerProgressStallExempt,
		ReasonCode:  detectorReasonProgressStall,
		OutcomeCode: TraceOutcomeStop,
	}
	if _, class := parityJoinClassify(spec, parityJoinSideDetectorOnly, parityJoinRowContext{}, nil, &exempt); class != parityJoinClassUnclassified {
		t.Fatalf("detector progress-stall singleton classified as %q, want UNCLASSIFIED", class)
	}
	alarmOnDetectorSide := SessionReconcilerTraceRecord{
		SiteCode:    TraceSiteReconcilerResetStalled,
		ReasonCode:  TraceReasonResetStalled,
		OutcomeCode: TraceOutcomeFailed,
	}
	if _, class := parityJoinClassify(spec, parityJoinSideDetectorOnly, parityJoinRowContext{}, nil, &alarmOnDetectorSide); class != parityJoinClassUnclassified {
		t.Fatalf("a detector-side record at the alarm site classified as %q, want UNCLASSIFIED", class)
	}
}

// B1's guard. Absence classifies as legacy only inside the section 1 legacy
// vocabulary. A phase site's per-cycle marker and a keyed-owned site are each
// counted and surfaced with the reason they were refused — never binned as
// legacy, which would have manufactured phantom legacy-only rows in exactly the
// families (D-DUP, D-STRANDED) whose only site is a phase site.
func TestParityJoinRefusesToAttributeAbsenceOutsideTheLegacyVocabulary(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	for _, family := range []string{parityJoinFamilyDup, parityJoinFamilyStranded, parityJoinFamilyStart} {
		row := parityJoinFamilyRow(t, report, family)
		if row.LegacyOnly != 0 || row.Joined != 0 || row.YieldJoined != 0 {
			t.Fatalf("family %q took a phantom legacy row: legacy_only=%d joined=%d yield_joined=%d (%+v)",
				family, row.LegacyOnly, row.Joined, row.YieldJoined, row)
		}
	}

	for _, want := range []struct {
		site        TraceSiteCode
		disposition string
	}{
		{TraceSiteSessionReconcileHealRetire, parityJoinDispositionPhaseMarker},
		{TraceSiteSessionReconcileWakeSleep, parityJoinDispositionPhaseMarker},
		{TraceSiteLifecycleStartCommit, parityJoinDispositionUnattributable},
	} {
		if got := parityJoinDispositionCount(report, want.site, want.disposition); got != 1 {
			t.Fatalf("owner_absence[%s/%s] = %d, want 1 (absence=%+v)", want.site, want.disposition, got, report.Dispositions)
		}
	}

	// Every refused record is still counted, so the readout never loses one.
	if report.Cycles.UnownedRecords != 4 {
		t.Fatalf("unowned_records = %d, want 4 (2 phase markers, 1 keyed-owned site, 1 sessionless pool-fill): %+v",
			report.Cycles.UnownedRecords, report.Cycles)
	}

	var out strings.Builder
	if err := writeParityJoinReport(&out, report); err != nil {
		t.Fatalf("writeParityJoinReport: %v", err)
	}
	for _, want := range []string{"OWNER-ABSENT", parityJoinDispositionPhaseMarker, "YIELDS", "keyed_deadline_owner"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("human readout hides the owner-absence or yield taxonomy (missing %q):\n%s", want, out.String())
		}
	}
}

// B2. The collector writes every cycle-rollup counter into rec.Fields and never
// sets the typed rec.DetailedTemplateCount, so a reader of the typed field sees
// zero on every real rollup and the campaign's own "did this window run armed?"
// alarm reads all-unarmed. Six of the seven fixture cycles carry a detail arm.
func TestParityJoinReadsRollupCountersWhereTheCollectorWritesThem(t *testing.T) {
	records := parityJoinCorpusRecords(t)
	rollups := 0
	for _, rec := range records {
		if rec.RecordType != TraceRecordCycleResult {
			continue
		}
		rollups++
		if rec.DetailedTemplateCount != 0 {
			t.Fatalf("fixture rollup carries a typed detailed_template_count=%d; production never sets it, so this fixture is no longer production-shaped",
				rec.DetailedTemplateCount)
		}
	}
	if rollups != 18 {
		t.Fatalf("fixture carries %d cycle rollups, want 18", rollups)
	}

	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.Considered != 18 {
		t.Fatalf("considered = %d, want 18 (%+v)", report.Cycles.Considered, report.Cycles)
	}
	if report.Cycles.WithoutDetailArms != 1 {
		t.Fatalf("without_detail_arms = %d, want 1 — sixteen fixture rollups carry fields.detailed_template_count>0 (%+v)",
			report.Cycles.WithoutDetailArms, report.Cycles)
	}
}

// B2, end to end through the real collector and the real store: the mis-arming
// alarm must go quiet for a cycle that actually ran armed. A synthesized rollup
// cannot prove this — only a record the collector wrote and the store read back
// carries the counter where production puts it, as the JSON number it becomes.
func TestParityJoinArmingAlarmIsQuietForACollectorWrittenArmedCycle(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	now := time.Now().UTC()

	tracer := newSessionReconcilerTracer(cityDir, "wd15-arming", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", now, &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerDrainDecision, TraceReasonNoWakeReason, TraceOutcomeDrain,
		"worker", "worker-rc-z1e", map[string]any{"session_id": "gcs-1"})
	cycle.RecordDecision(TraceSiteReconcilerDrainDecision, detectorReasonNoWakeFleetOnly, TraceOutcomeSkipped,
		"worker", "worker-rc-z1e", map[string]any{
			"session_id":     "gcs-1",
			"effect_owner":   detectorShadowEffectOwner,
			"effect_applied": false,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	if report.Cycles.Considered != 1 {
		t.Fatalf("considered = %d, want 1 (%+v)", report.Cycles.Considered, report.Cycles)
	}
	if report.Cycles.WithoutDetailArms != 0 {
		t.Fatalf("without_detail_arms = %d, want 0 for a cycle the collector recorded under a live detail arm (%+v)",
			report.Cycles.WithoutDetailArms, report.Cycles)
	}
	if row := parityJoinFamilyRow(t, report, parityJoinFamilySleep); row.Joined != 1 {
		t.Fatalf("D-SLEEP joined = %d, want 1 through the real collector and store (%+v)", row.Joined, row)
	}
	if report.NoEvidence {
		t.Fatal("no_evidence = true for a corpus with a joined pair")
	}
}

// Two shapes ga-f7v2ft.158 filed as D-WAKE candidacy gaps are neither. The
// sweep's pool-under-min FILL condition is a wake for a session that does not
// exist yet, so it carries no session identity and is not a row in a per-session
// join at all; and a wake target its own admission refused as uncertifiable is a
// row the sweep deliberately left to legacy, whose negative wake arms are
// untraced. Both are refused or classified, never scored as divergences.
func TestParityJoinRefusesSessionlessConditionsAndAdmissionRefusedWakes(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	if got := parityJoinDispositionCount(report, TraceSiteReconcilerWakeDecision, parityJoinDispositionNoSessionKey); got != 1 {
		t.Fatalf("no_session_key refusals at wake_decision = %d, want 1 for the pool-fill condition (%+v)", got, report.Dispositions)
	}
	// Both sides: the detector-only refusal, and the joined pair where legacy
	// declined too because the start was already in flight.
	if got := parityJoinTriageCount(report, parityJoinFamilyWake, "wake_admission_refused_row_stays_legacy"); got != 2 {
		t.Fatalf("wake_admission_refused_row_stays_legacy = %d, want 2 (triage=%+v)", got, report.Triage)
	}
	if row := parityJoinFamilyRow(t, report, parityJoinFamilyWake); row.Unclassified != 0 {
		t.Fatalf("D-WAKE unclassified = %d, want 0 (%+v)", row.Unclassified, row)
	}
}

// The yield-join's agreement condition has two halves: both writers identified
// the row, AND the family legacy stood down FOR is the family that acted. A
// D-DEADLINE stand-down beside a D-ORPHAN act is two writers looking at one row
// and disagreeing about what it is — a divergence to classify, not a match. The
// campaign corpus has no such pair, so this one is written by the real collector
// and read back through the real store.
func TestParityJoinFlagsAYieldStandingDownForADifferentFamilyThanTheActor(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-family-mismatch", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	// Legacy's deadline seam stands down for the keyed deadline owner...
	cycle.RecordDecision(TraceSiteReconcilerIdleTimeout, "keyed_deadline_owner", TraceOutcomeSkipped,
		"worker", "worker-rc-mix", map[string]any{
			"session_id":     "rc-mix",
			"effect_owner":   detectorKeyedEffectOwner,
			"effect_applied": false,
		})
	// ...but the sweep claimed the same row for D-ORPHAN at the same site.
	cycle.RecordDecision(TraceSiteReconcilerIdleTimeout, detectorReasonOrphanLive, TraceOutcomeDrain,
		"worker", "worker-rc-mix", map[string]any{
			"session_id":      "rc-mix",
			"detector_family": string(detectorFamilyOrphan),
			"detector_acts":   true,
			"effect_owner":    detectorKeyedEffectOwner,
			"effect_applied":  false,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if row.YieldJoined != 1 || row.Mismatched != 1 || row.Matched != 0 {
		t.Fatalf("D-DEADLINE yield_joined=%d mismatched=%d matched=%d, want 1/1/0 (%+v)",
			row.YieldJoined, row.Mismatched, row.Matched, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyDeadline, parityJoinClassYieldFamilyMismatch); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassYieldFamilyMismatch, got, report.Triage)
	}
}

// A candidacy-bearing stand-down with nothing beside it is a real divergence:
// legacy reached the family's arm, judged the row actionable, stepped aside —
// and nothing acted that tick. It must surface as an unclassified WE blocker
// with its evidence, never be absorbed by the ownership-arm refusal.
func TestParityJoinReportsACandidacyYieldWithNoActorAsAWEBlocker(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-orphan-yield", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, "keyed_orphan_drain_owner", TraceOutcomeSkipped,
		"worker", "worker-rc-lone", map[string]any{
			"session_id":     "rc-lone",
			"effect_owner":   detectorKeyedEffectOwner,
			"effect_applied": false,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if row.YieldOnly != 1 || row.Unclassified != 1 {
		t.Fatalf("D-ORPHAN yield_only=%d unclassified=%d, want 1/1 (%+v)", row.YieldOnly, row.Unclassified, row)
	}
	if !report.WEBlocker {
		t.Fatal("we_blocker = false for a candidacy stand-down nothing acted on")
	}
	if report.Cycles.UnpairedOwnershipYields != 0 {
		t.Fatalf("unpaired_ownership_yields = %d, want 0: a candidacy arm must not be refused (%+v)",
			report.Cycles.UnpairedOwnershipYields, report.Cycles)
	}
	if len(report.Unclassified) != 1 || report.Unclassified[0].LegacyReason != "keyed_orphan_drain_owner" {
		t.Fatalf("unclassified sample does not carry the stand-down's evidence: %+v", report.Unclassified)
	}
}

// B3. no_evidence was owner-presence-based (owned == 0), so a corpus carrying
// keyed and detector-shadow records but no legacy ones — precisely the corpus
// this campaign produced before B1 — reported no_evidence=false beside joined=0
// in every family. The flag is the campaign's guard against reading an unarmed
// or unjoinable window as a parity result, so it must be join-based.
func TestParityJoinNoEvidenceIsJoinBased(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "wd15-no-evidence", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerIdleTimeout, TraceReasonIdleTimeout, TraceOutcomeStop,
		"worker", "worker-rc-gv8", map[string]any{"effect_owner": detectorKeyedEffectOwner, "effect_applied": false})
	cycle.RecordDecision(TraceSiteReconcilerPendingCreatePreserved, TraceReasonPreserve, TraceOutcomeNoChange,
		"worker", "worker-rc-7iv", map[string]any{"effect_owner": detectorShadowEffectOwner, "effect_applied": false})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995})

	total := 0
	for _, row := range report.Families {
		total += row.Joined
	}
	if total != 0 {
		t.Fatalf("fixture joined %d rows; it must join none for this test to mean anything", total)
	}
	if !report.NoEvidence {
		t.Fatal("no_evidence = false for a corpus with owned records but zero joined rows")
	}
	if report.BarMet {
		t.Fatal("bar_met = true with no joined evidence")
	}
}

// The round-6 pre-wake fence writes a THIRD record on the legacy side, and the
// join had no class for it: day-4 of the campaign reported 4 legacy-only
// start_commit_superseded records as unclassified and drove D-WAKE to 98.30%,
// below the section 3b bar. All four carry a reason of premise_drift:*, which
// session_lifecycle_parallel.go:1278-1280 writes when a wake-relevant premise
// moved between the snapshot the candidate was decided on and its prepare —
// legacy fencing its OWN stale candidate, the ga-l1j53 re-validation working.
// The record is an errPreWakeSuperseded convergence with outcome skipped: no
// effect entered the provider on either side, and the sweep's own record for
// the row refused to route it (admission_outcome refused_uncertifiable). It is
// the same population as wake_admission_refused_row_stays_legacy, one record
// later, so it takes the same incomparable classification.
func TestParityJoinTriagesThePreWakeSupersedeAgainstItsDetectorTwin(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-supersede", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	// The sweep raises the wake target and its own admission declines to route
	// it (cycle-883e3e1d69a4db40, seq 2999118).
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, detectorReasonWakeTarget, TraceOutcomeStartCandidate,
		"worker", "worker-rc-2fan", map[string]any{
			"session_id":        "rc-2fan",
			"detector_family":   string(detectorFamilyWake),
			"detector_acts":     true,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    false,
			"predicted_effect":  "start",
			"admission":         "wake_fill",
			"admission_outcome": string(detectorAdmissionRefusedUncertifiable),
		})
	// Legacy decides the same row is a start candidate...
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, TraceReasonWake, TraceOutcomeStartCandidate,
		"worker", "worker-rc-2fan", map[string]any{"should_wake": true})
	// ...and its own prepare-time re-validation supersedes the commit.
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, parityJoinReasonStartCommitSuperseded, TraceOutcomeSkipped,
		"worker", "worker-rc-2fan", map[string]any{
			"session_id": "rc-2fan",
			"reason":     "premise_drift:state",
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if row.Unclassified != 0 || row.Mismatched != 0 {
		t.Fatalf("D-WAKE unclassified=%d mismatched=%d, want 0/0 — the supersede is convergence, not a divergence (%+v)",
			row.Unclassified, row.Mismatched, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyWake, parityJoinClassPreWakeSupersedeConvergence); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassPreWakeSupersedeConvergence, got, report.Triage)
	}
	if !row.BarMet {
		t.Fatalf("D-WAKE bar_met=false at match_rate=%v; the classified supersede must leave the rate clean (%+v)", row.MatchRate, row)
	}
}

// The supersede class is evidence-scoped, not a blanket amnesty for the reason
// code. A start_commit_superseded with NO sweep record for the row in the cycle
// is a supersede with no keyed counterpart at all — legacy fenced a candidate
// nothing else was looking at — and that is a real finding, not a class. It
// must stay unclassified and keep blocking WE.
func TestParityJoinPreWakeSupersedeStillRequiresItsDetectorTwin(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-supersede-lonely", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, parityJoinReasonStartCommitSuperseded, TraceOutcomeSkipped,
		"worker", "worker-rc-lonely", map[string]any{
			"session_id": "rc-lonely",
			"reason":     "premise_drift:state",
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if row.Unclassified != 1 {
		t.Fatalf("D-WAKE unclassified = %d, want 1 for a twinless supersede (%+v)", row.Unclassified, row)
	}
	if !report.WEBlocker {
		t.Fatal("we_blocker = false for a twinless supersede; an unclassified mismatch blocks WE")
	}
}

// D-ORPHAN's live-drain arm runs one tick AHEAD of legacy's. Day 4 reported 3
// detector-only detector_orphan_live/drain records on dependent-rc-* rows as
// unclassified; every one has a legacy orphaned/drain twin for the same session
// at the same site on the very next tick, ~2.24s later (njwi 032633->032634,
// c6xr 038223->038224, h9bb7 038793->038794). The two writers agree on the row
// and on the effect; only the tick differs, and a same-cycle-handle join reports
// an adjacent-cycle pair as two singletons. That is the ack-timing-skew shape
// section 3b already names for D-DRAIN, now in D-ORPHAN. It stays MISMATCHED
// rather than incomparable: the class explains the singleton, it does not prove
// the twin landed, so the rate keeps counting it.
func TestParityJoinTriagesTheOneTickOrphanDetectorLead(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "dependent")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-orphan-lead", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, detectorReasonOrphanLive, TraceOutcomeDrain,
		"dependent", "dependent-rc-njwi", map[string]any{
			"session_id":        "rc-njwi",
			"detector_family":   string(detectorFamilyOrphan),
			"detector_acts":     true,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    false,
			"predicted_effect":  "drain",
			"admission":         "orphan_drain",
			"admission_outcome": "accepted",
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if row.Unclassified != 0 {
		t.Fatalf("D-ORPHAN unclassified = %d, want 0 (%+v)", row.Unclassified, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyOrphan, parityJoinClassOrphanLiveDetectorLead); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassOrphanLiveDetectorLead, got, report.Triage)
	}
	if row.Mismatched != 1 {
		t.Fatalf("D-ORPHAN mismatched = %d, want 1 — the lead class is counted, not excused (%+v)", row.Mismatched, row)
	}
}

// A controller restart mid-window leaves a stub of a cycle behind: the outgoing
// instance stops between a legacy decision and its sweep twin, and the incoming
// instance's first tick runs before its detail arms are re-verified. Both halves
// surface as singletons the section 3b table cannot classify, because there is
// no divergence to name — the twin was never written. The WD.15 runbook excludes
// those cycles from the readout; --exclude-window is that rule as an artifact.
//
// This fixture is the day-4 restart itself, byte-copied from the campaign
// archive: the outgoing instance's last good cycle (cycle-96035091d29b893d,
// 05:57:34-05:58:00) and the incoming instance's first tick
// (cycle-6e82321f9d754602, 06:01:16-06:01:25). The unclassified singleton it
// reproduces is the one the archive join reported — D-ORPHAN legacy_only
// orphaned/closed on dependent-rc-5kfx6 — and the control is the SAME session's
// properly joined pair three minutes earlier, which the window must leave alone.
func TestParityJoinExcludesRestartGapCyclesFromTheReadout(t *testing.T) {
	records := parityJoinCorpusFixtureRecords(t, parityJoinRestartGapFixture)

	baseline := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 8})
	if len(baseline.Unclassified) != 1 {
		t.Fatalf("baseline unclassified = %d, want the 1 restart artifact (%+v)", len(baseline.Unclassified), baseline.Unclassified)
	}
	sample := baseline.Unclassified[0]
	if sample.SessionName != "dependent-rc-5kfx6" || sample.TraceID != "cycle-6e82321f9d754602" {
		t.Fatalf("baseline unclassified sample = %+v, want the 5kfx6 first-tick orphan", sample)
	}
	if !baseline.WEBlocker {
		t.Fatalf("baseline we_blocker = false, want true — the restart stub is a blocker until it is excluded")
	}
	if len(baseline.ExcludedWindows) != 0 {
		t.Fatalf("baseline excluded_windows = %+v, want none", baseline.ExcludedWindows)
	}

	window := parityJoinWindow{
		Start: time.Date(2026, 8, 15, 5, 59, 50, 0, time.UTC),
		End:   time.Date(2026, 8, 15, 6, 2, 40, 0, time.UTC),
	}
	report := buildParityJoinReport(records, parityJoinOptions{
		Bar: 0.995, Samples: 8, ExcludedWindows: []parityJoinWindow{window},
	})

	if len(report.Unclassified) != 0 {
		t.Fatalf("unclassified = %+v, want none once the restart gap is excluded", report.Unclassified)
	}
	if report.WEBlocker {
		t.Fatalf("we_blocker = true, want false (%+v)", report)
	}
	if len(report.ExcludedWindows) != 1 {
		t.Fatalf("excluded_windows = %+v, want exactly the one window", report.ExcludedWindows)
	}
	got := report.ExcludedWindows[0]
	if !got.Start.Equal(window.Start) || !got.End.Equal(window.End) {
		t.Fatalf("excluded window = %s/%s, want %s/%s", got.Start, got.End, window.Start, window.End)
	}
	// The first tick's close_orphan decision and its cycle rollup: the whole
	// cycle leaves, so no half-pair from it can leak out as a singleton.
	if got.RecordsExcluded != 2 {
		t.Fatalf("records_excluded = %d, want 2 (the 5kfx6 decision and its cycle rollup)", got.RecordsExcluded)
	}
	if report.Cycles.Scanned != 1 || report.Cycles.Considered != 1 {
		t.Fatalf("cycles = %+v, want 1 scanned / 1 considered", report.Cycles)
	}

	// The control: the outgoing instance's last good cycle is untouched, down to
	// the D-ORPHAN pair for the very session the excluded record names.
	deadline := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if deadline.Joined != 3 || deadline.Matched != 3 || deadline.Mismatched != 0 {
		t.Fatalf("D-DEADLINE = %+v, want the 3 pre-restart pairs still joined and matched", deadline)
	}
	orphan := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if orphan.Joined != 1 || orphan.Matched != 1 {
		t.Fatalf("D-ORPHAN = %+v, want the pre-restart 5kfx6 pair still joined and matched", orphan)
	}
	if orphan.LegacyOnly != 0 || orphan.Mismatched != 0 || orphan.Unclassified != 0 {
		t.Fatalf("D-ORPHAN = %+v, want the restart singleton gone and nothing else with it", orphan)
	}
	if orphan.MatchRate != 1 || !orphan.BarMet {
		t.Fatalf("D-ORPHAN rate = %.4f bar_met = %t, want 1.0 / true", orphan.MatchRate, orphan.BarMet)
	}
}

// A window is half-open: a record exactly on the start instant is dropped, one
// exactly on the end instant is kept. Callers pad the start back a tick by hand
// precisely because the tool never infers a boundary, so the boundary it is
// given has to mean one unambiguous thing.
func TestParityJoinExcludeWindowIsHalfOpen(t *testing.T) {
	records := parityJoinCorpusFixtureRecords(t, parityJoinRestartGapFixture)

	// [decision, rollup) — the decision at 06:01:21.469296086Z is dropped, the
	// rollup at 06:01:24.522622325Z is kept, so the cycle survives with no rows.
	report := buildParityJoinReport(records, parityJoinOptions{
		Bar: 0.995, Samples: 8,
		ExcludedWindows: []parityJoinWindow{{
			Start: time.Date(2026, 8, 15, 6, 1, 21, 469296086, time.UTC),
			End:   time.Date(2026, 8, 15, 6, 1, 24, 522622325, time.UTC),
		}},
	})
	if report.ExcludedWindows[0].RecordsExcluded != 1 {
		t.Fatalf("records_excluded = %d, want 1 — start is inclusive, end exclusive", report.ExcludedWindows[0].RecordsExcluded)
	}
	if report.Cycles.Scanned != 2 {
		t.Fatalf("cycles scanned = %d, want 2 — the kept rollup still makes a cycle", report.Cycles.Scanned)
	}
	if len(report.Unclassified) != 0 {
		t.Fatalf("unclassified = %+v, want none", report.Unclassified)
	}
}

// A family whose joined pairs are ALL incomparable by design has no comparable
// evidence at all, and the human table already says so: it prints "-" for the
// rate and "no-data" in the bar cell. The JSON readout did not — it emitted the
// zero values of match_rate and bar_met, which a reader (or a day-over-day
// diff) cannot tell apart from "every pair disagreed, family below bar".
//
// That ambiguity is not hypothetical. The WD.15 day-4 readout reported D-SLEEP
// as joined=6, match_rate=0.0, bar_met=false, and it was escalated as six real
// disagreements; the corpus in fact held zero comparable pairs, every record
// landing in the named fleet_only_no_wake_left_to_legacy class. The JSON must
// carry the same no-data disclosure the table does.
func TestParityJoinJSONDistinguishesNoComparablePairsFromZeroAgreement(t *testing.T) {
	report := buildParityJoinReport(parityJoinCorpusRecords(t), parityJoinOptions{Bar: 0.995, Samples: 8})

	sleep := parityJoinFamilyRow(t, report, parityJoinFamilySleep)
	if sleep.Joined == 0 {
		t.Fatalf("D-SLEEP joined = 0, want the corpus's joined pair (%+v)", sleep)
	}
	if sleep.Matched+sleep.Mismatched != 0 {
		t.Fatalf("D-SLEEP comparable = %d, want 0 — the fixture's pair is incomparable by design (%+v)",
			sleep.Matched+sleep.Mismatched, sleep)
	}

	encoded, err := json.Marshal(sleep)
	if err != nil {
		t.Fatalf("marshal family row: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal family row: %v", err)
	}

	status, ok := decoded["bar_status"].(string)
	if !ok {
		t.Fatalf("family row JSON has no bar_status string: %s", encoded)
	}
	if status != parityJoinBarNoData {
		t.Fatalf("D-SLEEP bar_status = %q, want %q — joined>0 with no comparable pairs is not a 0%% match rate",
			status, parityJoinBarNoData)
	}

	// The disclosure must stay honest for families that DO have evidence, or it
	// just moves the ambiguity somewhere else.
	deadline := parityJoinFamilyRow(t, report, parityJoinFamilyDeadline)
	if deadline.Matched+deadline.Mismatched == 0 {
		t.Fatalf("D-DEADLINE has no comparable pairs in the fixture; pick another family (%+v)", deadline)
	}
	if deadline.BarStatus == parityJoinBarNoData {
		t.Fatalf("D-DEADLINE bar_status = %q, want a real verdict (%+v)", deadline.BarStatus, deadline)
	}

	// The table cell and the JSON field must be the same answer, always.
	if got := parityJoinBarCell(sleep); got != sleep.BarStatus {
		t.Fatalf("table cell %q != json bar_status %q for D-SLEEP", got, sleep.BarStatus)
	}
	if got := parityJoinBarCell(deadline); got != deadline.BarStatus {
		t.Fatalf("table cell %q != json bar_status %q for D-DEADLINE", got, deadline.BarStatus)
	}
}

// Day 6 of the WD.15 window (cycle-7eb5acef63924183, tick ...-012843, real
// 2026-08-16T17:16:14Z): the wd15-campaign tmux server dropped between the
// sweep's one-shot names-only ListRunning and legacy's own per-row probe. The
// sweep failed closed for the whole family — detector_running_set_unavailable /
// skipped / predicted_effect none is a refusal to evaluate, not a detection
// (session_detector_sweep.go's detectOrphan: "proven absence, not assumed
// absence") — while legacy's fresh per-row probe returned Running=true inside
// the same second and it re-decided the drain it had been standing down from
// for the previous 15 ticks (keyed_orphan_drain_owner, 012828-012842). Legacy's
// own GC_DRAIN_ACK write then failed with "no tmux server running" in this
// cycle and the next, so no effect entered the provider on either side, and the
// keyed engine rebuilt the fleet within two minutes. Comparing legacy's act
// against a record that DECLINED to evaluate the family measures degraded-input
// policy, not detection parity — the wake_admission_refused_row_stays_legacy
// rationale one family over.
func TestParityJoinTriagesTheFailClosedOrphanSweepAgainstLegacyDrain(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "dependent")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-runningset", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	// The sweep's fail-closed record (cycle-7eb5acef63924183, seq 4976959).
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, detectorReasonRunningSetUnavailable, TraceOutcomeSkipped,
		"dependent", "dependent-rc-6f2nb", map[string]any{
			"session_id":       "rc-6f2nb",
			"detector_family":  string(detectorFamilyOrphan),
			"detector_acts":    true,
			"effect_owner":     detectorShadowEffectOwner,
			"effect_applied":   false,
			"predicted_effect": "none",
		})
	// Legacy re-decides the drain on its own probe result (seq 4976973).
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, TraceReasonOrphaned, TraceOutcomeDrain,
		"dependent", "dependent-rc-6f2nb", map[string]any{
			"provider_alive":      true,
			"store_query_partial": false,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if row.Unclassified != 0 || row.Mismatched != 0 {
		t.Fatalf("D-ORPHAN unclassified=%d mismatched=%d, want 0/0 — the fail-closed sweep made no claim on the row (%+v)",
			row.Unclassified, row.Mismatched, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyOrphan, parityJoinClassOrphanRunningSetUnavailable); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassOrphanRunningSetUnavailable, got, report.Triage)
	}
	if report.WEBlocker {
		t.Fatal("we_blocker = true for the classified fail-closed pair")
	}
}

// The fail-closed class is scoped to the exact tuple the corpus carries —
// legacy orphaned/drain beside the sweep's running-set refusal. Any OTHER
// legacy conclusion beside that refusal (here a close) is a shape the campaign
// has never seen and must stay unclassified and keep blocking WE, not ride the
// class. This is the control that must keep failing differently.
func TestParityJoinFailClosedOrphanSweepDoesNotBlanketTheSite(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "dependent")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-runningset-control", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, detectorReasonRunningSetUnavailable, TraceOutcomeSkipped,
		"dependent", "dependent-rc-ctrl", map[string]any{
			"session_id":       "rc-ctrl",
			"detector_family":  string(detectorFamilyOrphan),
			"detector_acts":    true,
			"effect_owner":     detectorShadowEffectOwner,
			"effect_applied":   false,
			"predicted_effect": "none",
		})
	cycle.RecordDecision(TraceSiteReconcilerOrphaned, TraceReasonOrphaned, TraceOutcomeClosed,
		"dependent", "dependent-rc-ctrl", map[string]any{
			"provider_alive":      false,
			"store_query_partial": false,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyOrphan)
	if row.Unclassified != 1 {
		t.Fatalf("D-ORPHAN unclassified = %d, want 1 — a close beside the refusal is not the classified shape (%+v)",
			row.Unclassified, row)
	}
	if !report.WEBlocker {
		t.Fatal("we_blocker = false for an unclassified degraded-input divergence")
	}
}

// Day 6, four ticks after the running-set outage (cycle-88f708218a46489c, tick
// ...-012847): the same pre-wake supersede convergence the day-4 triage
// classified, in its OTHER spelling. The wisp died with the tmux server
// (baseline sleep_reason runtime-missing), both engines raised its restart, and
// legacy's pre-wake CAS found the KEYED start owner holding the key — so the
// supersede's own reason field says keyed_start_owner, not premise_drift:*,
// and parityJoinYieldOf routes it to the yield vocabulary as a candidacy
// stand-down. The sweep's detector_wake_target co-twin is in the SAME cycle
// (seq 4977424, admission refused_uncertifiable, effect_applied false, emitted
// ~2s before), exactly as the class requires — but the act-join consumes it
// against legacy's own wake record, and the unpaired candidacy yield was
// classified with a nil co-twin index, so the adjudicated class could never
// fire on this spelling. Same evidence, same convergence, same classification
// as the premise_drift spelling.
func TestParityJoinTriagesTheSupersedeYieldSpellingAgainstItsDetectorTwin(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-supersede-yield", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	// The sweep raises the wake target and its own admission declines to route
	// it (cycle-88f708218a46489c, seq 4977424).
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, detectorReasonWakeTarget, TraceOutcomeStartCandidate,
		"worker", "s-rc-wisp-mx105dm", map[string]any{
			"session_id":        "rc-wisp-mx105dm",
			"detector_family":   string(detectorFamilyWake),
			"detector_acts":     true,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    false,
			"predicted_effect":  "start",
			"admission":         "wake_fill",
			"admission_outcome": string(detectorAdmissionRefusedUncertifiable),
			"wake_reason":       "manual",
		})
	// Legacy decides the same row is a start candidate (seq 4977446)...
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, TraceReasonWake, TraceOutcomeStartCandidate,
		"worker", "s-rc-wisp-mx105dm", map[string]any{"should_wake": true})
	// ...and its pre-wake CAS finds the keyed start owner holding the key
	// (seq 4977448) — the supersede spelling that IS a yield.
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, parityJoinReasonStartCommitSuperseded, TraceOutcomeSkipped,
		"worker", "s-rc-wisp-mx105dm", map[string]any{
			"session_id": "rc-wisp-mx105dm",
			"reason":     parityJoinSupersedeYieldReason,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if row.Unclassified != 0 || row.Mismatched != 0 {
		t.Fatalf("D-WAKE unclassified=%d mismatched=%d, want 0/0 — the supersede-yield has its same-cycle co-twin (%+v)",
			row.Unclassified, row.Mismatched, row)
	}
	if got := parityJoinTriageCount(report, parityJoinFamilyWake, parityJoinClassPreWakeSupersedeConvergence); got != 1 {
		t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassPreWakeSupersedeConvergence, got, report.Triage)
	}
	if row.Matched != 1 {
		t.Fatalf("D-WAKE matched = %d, want 1 — the wake/wake-target act pair must still join (%+v)", row.Matched, row)
	}
	if report.WEBlocker {
		t.Fatal("we_blocker = true for the classified supersede-yield")
	}
}

// The yield-path fix must not relax the twin requirement: a supersede-yield
// with NO sweep record for the row in its cycle is still legacy fencing a
// candidate nothing else was looking at — the genuinely twinless case the
// class exists to block on. It stays unclassified and keeps blocking WE.
func TestParityJoinSupersedeYieldStillRequiresItsDetectorTwin(t *testing.T) {
	cityDir := t.TempDir()
	parityJoinArmTemplate(t, cityDir, "worker")
	tracer := newSessionReconcilerTracer(cityDir, "wd15-supersede-yield-lonely", io.Discard)
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(TraceSiteReconcilerWakeDecision, parityJoinReasonStartCommitSuperseded, TraceOutcomeSkipped,
		"worker", "s-rc-wisp-lonely", map[string]any{
			"session_id": "rc-wisp-lonely",
			"reason":     parityJoinSupersedeYieldReason,
		})
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})

	row := parityJoinFamilyRow(t, report, parityJoinFamilyWake)
	if row.Unclassified != 1 {
		t.Fatalf("D-WAKE unclassified = %d, want 1 for a twinless supersede-yield (%+v)", row.Unclassified, row)
	}
	if !report.WEBlocker {
		t.Fatal("we_blocker = false for a twinless supersede-yield; an unclassified mismatch blocks WE")
	}
}

// The three D-DRAIN ack-timing skews the owner adjudicated on WD.15 day 6 and
// signed on 2026-08-17/18. Each is one legacy drain_ack decision with no sweep
// record beside it in its own cycle, and the keyed handler's own drain_ack
// operation for the same session one cycle away:
//
//	dependent-rc-7mzpx  cycle-0860a236ff1b82bd  2026-08-15T04:49:21.053Z
//	    keyed twins cycle-880fa1b90288d6a4 (-3.413s), cycle-793999c79d218eb5 (+255ms)
//	s-rc-wisp-y73064d   cycle-41b467cd627d719e  2026-08-17T00:21:48.259Z
//	    keyed twin  cycle-10efb4c154f087cd (+162ms)
//	s-rc-wisp-d30uo8f   cycle-03d51f88678dbb50  2026-08-17T02:05:29.315Z
//	    keyed twin  cycle-9b0154e4270d3a4c (+190ms)
//
// The skew is structural — the keyed handler acks from inside its own operation
// while legacy polls the same field in-tick — and far too rare to characterize
// statistically: roughly 2 a day against ~90 comparable joins a day. So the
// owner ruled it incomparable PER ROW ON PROOF rather than by rule, and each
// specimen runs in ISOLATION here: one specimen's twin cannot vouch for another.
func TestParityJoinTriagesTheAdjudicatedDrainAckSkewsAgainstTheirKeyedTwins(t *testing.T) {
	records := parityJoinCorpusFixtureRecords(t, parityJoinDrainAckTwinFixture)

	for _, session := range parityJoinDrainAckSpecimens {
		t.Run(session, func(t *testing.T) {
			report := buildParityJoinReport(
				parityJoinDrainAckSpecimen(records, session, true),
				parityJoinOptions{Bar: 0.995, Samples: 4},
			)

			row := parityJoinFamilyRow(t, report, parityJoinFamilyDrain)
			if row.LegacyOnly != 1 {
				t.Fatalf("legacy_only = %d, want 1 — the skew is a singleton by construction (%+v)", row.LegacyOnly, row)
			}
			if row.Mismatched != 0 || row.Unclassified != 0 {
				t.Fatalf("mismatched=%d unclassified=%d, want 0/0 — the twin is in the corpus (%+v)",
					row.Mismatched, row.Unclassified, row)
			}
			if row.Incomparable != 1 {
				t.Fatalf("incomparable = %d, want 1 (%+v)", row.Incomparable, row)
			}
			if got := parityJoinTriageCount(report, parityJoinFamilyDrain, parityJoinClassDrainAckAdjacentCycleConvergence); got != 1 {
				t.Fatalf("%s = %d, want 1 (triage=%+v)", parityJoinClassDrainAckAdjacentCycleConvergence, got, report.Triage)
			}
			if report.WEBlocker {
				t.Fatalf("we_blocker = true for a skew that proved its twin (%+v)", report.Unclassified)
			}
		})
	}
}

// The control, and the reason the class is worth having at all. Strip the keyed
// RECORD out of each specimen — its cycle stays in the corpus, so the corpus can
// still answer the question and answers "no keyed ack" — and the skew must fall
// all the way through the section 3b table to UNCLASSIFIED and block WE.
//
// A legacy acknowledgement with no keyed acknowledgement beside it is not a
// timing artifact; it is legacy writing a drain ack the keyed engine never
// wrote, which is the exact divergence D-DRAIN exists to catch. If this test
// ever passes for the same reason the one above does, the class has become a
// blanket over the site and the family's only remaining alarm is gone.
func TestParityJoinDrainAckSkewWithoutItsKeyedTwinStaysAnUnclassifiedMismatch(t *testing.T) {
	records := parityJoinCorpusFixtureRecords(t, parityJoinDrainAckTwinFixture)

	for _, session := range parityJoinDrainAckSpecimens {
		t.Run(session, func(t *testing.T) {
			report := buildParityJoinReport(
				parityJoinDrainAckSpecimen(records, session, false),
				parityJoinOptions{Bar: 0.995, Samples: 4},
			)

			row := parityJoinFamilyRow(t, report, parityJoinFamilyDrain)
			if row.Mismatched != 1 || row.Unclassified != 1 {
				t.Fatalf("mismatched=%d unclassified=%d, want 1/1 — a twinless skew is not explained (%+v)",
					row.Mismatched, row.Unclassified, row)
			}
			if row.Incomparable != 0 {
				t.Fatalf("incomparable = %d, want 0 — the class must not fire without its twin (%+v)", row.Incomparable, row)
			}
			if got := parityJoinTriageCount(report, parityJoinFamilyDrain, parityJoinClassDrainAckAdjacentCycleConvergence); got != 0 {
				t.Fatalf("%s = %d, want 0 (triage=%+v)", parityJoinClassDrainAckAdjacentCycleConvergence, got, report.Triage)
			}
			if !report.WEBlocker {
				t.Fatal("we_blocker = false for a twinless drain ack; an unclassified mismatch blocks WE")
			}
		})
	}
}

// The two edges of "adjacent cycle", probed against the specimen with the
// tightest real skew (s-rc-wisp-d30uo8f, +190ms).
//
// A keyed ack in the SAME cycle is evidence the two writers were IN STEP, which
// is the opposite of the claim this class makes, so it must not verify. A keyed
// ack more than one tick away is a different drain episode — the reconciler
// re-decides a session's drain every tick — so it must not verify either. Only
// the window between them is an ack-timing skew.
func TestParityJoinDrainAckTwinMustBeAdjacentAndInsideOneTick(t *testing.T) {
	const session = "s-rc-wisp-d30uo8f"
	fixture := parityJoinCorpusFixtureRecords(t, parityJoinDrainAckTwinFixture)
	legacy := parityJoinDrainAckRecordOf(t, fixture, session, false)

	for _, tc := range []struct {
		name     string
		shift    time.Duration
		intoLeft *SessionReconcilerTraceRecord
		verified bool
	}{
		{name: "as_recorded", verified: true},
		{name: "just_inside_one_tick", shift: parityJoinAdjacentCycleWindow - time.Second, verified: true},
		{name: "just_past_one_tick", shift: parityJoinAdjacentCycleWindow + time.Second},
		{name: "same_cycle", intoLeft: &legacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := parityJoinDrainAckSpecimen(fixture, session, true)
			for i := range records {
				owner, _ := records[i].Fields["effect_owner"].(string)
				if owner != parityJoinOwnerKeyed {
					continue
				}
				records[i].Ts = records[i].Ts.Add(tc.shift)
				if tc.intoLeft != nil {
					records[i].TraceID, records[i].TickID = tc.intoLeft.TraceID, tc.intoLeft.TickID
				}
			}

			report := buildParityJoinReport(records, parityJoinOptions{Bar: 0.995, Samples: 4})
			row := parityJoinFamilyRow(t, report, parityJoinFamilyDrain)
			if tc.verified && row.Incomparable != 1 {
				t.Fatalf("incomparable = %d, want 1 — this twin is inside the window (%+v)", row.Incomparable, row)
			}
			if !tc.verified && row.Unclassified != 1 {
				t.Fatalf("unclassified = %d, want 1 — this twin is out of bounds and vouches for nothing (%+v)",
					row.Unclassified, row)
			}
		})
	}
}

// parityJoinDrainAckSpecimens are the sessions of the three adjudicated skews,
// in the order the archive wrote them.
var parityJoinDrainAckSpecimens = []string{
	"dependent-rc-7mzpx",
	"s-rc-wisp-y73064d",
	"s-rc-wisp-d30uo8f",
}

// parityJoinDrainAckSpecimen narrows the fixture to one specimen: that session's
// records plus the rollup of every cycle they touch. With keyedTwins false the
// keyed RECORDS are dropped and their cycles are kept, so the corpus still
// covers the adjacent cycles and the absence of an ack is a proven absence.
func parityJoinDrainAckSpecimen(records []SessionReconcilerTraceRecord, session string, keyedTwins bool) []SessionReconcilerTraceRecord {
	cycles := make(map[parityJoinCycleKey]bool)
	picked := make([]SessionReconcilerTraceRecord, 0, len(records))
	for _, rec := range records {
		if rec.SessionName != session {
			continue
		}
		cycles[parityJoinCycleKey{TraceID: rec.TraceID, TickID: rec.TickID}] = true
		owner, _ := rec.Fields["effect_owner"].(string)
		if !keyedTwins && owner == parityJoinOwnerKeyed {
			continue
		}
		picked = append(picked, rec)
	}
	for _, rec := range records {
		if rec.RecordType == TraceRecordCycleResult && cycles[parityJoinCycleKey{TraceID: rec.TraceID, TickID: rec.TickID}] {
			picked = append(picked, rec)
		}
	}
	return picked
}

// parityJoinDrainAckRecordOf returns the specimen's legacy decision (keyed
// false) or its first keyed acknowledgement (keyed true).
func parityJoinDrainAckRecordOf(t *testing.T, records []SessionReconcilerTraceRecord, session string, keyed bool) SessionReconcilerTraceRecord {
	t.Helper()
	for _, rec := range records {
		if rec.SessionName != session || rec.SiteCode != TraceSiteReconcilerDrainAck {
			continue
		}
		owner, _ := rec.Fields["effect_owner"].(string)
		if (owner == parityJoinOwnerKeyed) == keyed {
			return rec
		}
	}
	t.Fatalf("no drain_ack record for %s (keyed=%v) in the fixture", session, keyed)
	return SessionReconcilerTraceRecord{}
}
