package main

// This file is the machine-readable transcription of the DETECTOR.md section 3b
// classification table. It exists only for the WD parity campaign and is deleted
// with the rest of the D4-retained perf CLI at WE (DETECTOR.md section 5).
//
// Divergence rules are deliberately conservative: a rule is present only where
// section 3b names the divergence AND the trace codes that express it exist
// today. Where section 3b names a divergence whose codes land with a later WD
// slice, no rule is written — the mismatch then surfaces as UNCLASSIFIED, which
// is exactly the section 3b workflow ("triage it, extend the table with
// evidence, or fix the detector"). Inventing a speculative predicate would
// silently bucket real mismatches, which is the one failure the bar forbids.

import "time"

const (
	parityJoinFamilyStart       = "start"
	parityJoinFamilyDeadline    = "D-DEADLINE"
	parityJoinFamilyOrphan      = "D-ORPHAN"
	parityJoinFamilyStaleCreate = "D-STALE-CREATE"
	parityJoinFamilyDrift       = "D-DRIFT"
	parityJoinFamilySleep       = "D-SLEEP"
	parityJoinFamilyDrain       = "D-DRAIN"
	parityJoinFamilyWake        = "D-WAKE"
	parityJoinFamilyZombie      = "D-ZOMBIE"
	parityJoinFamilyStall       = "D-STALL"
	parityJoinFamilyDup         = "D-DUP"
	parityJoinFamilyStranded    = "D-STRANDED"
)

type parityJoinLevel string

const (
	parityJoinLevelDetection parityJoinLevel = "detection"
	parityJoinLevelDecision  parityJoinLevel = "decision"
	parityJoinLevelAct       parityJoinLevel = "act"
)

type parityJoinSide string

const (
	parityJoinSideBoth         parityJoinSide = "both"
	parityJoinSideLegacyOnly   parityJoinSide = "legacy_only"
	parityJoinSideDetectorOnly parityJoinSide = "detector_only"
)

const (
	parityJoinMatched      = "matched"
	parityJoinMismatched   = "mismatched"
	parityJoinIncomparable = "incomparable"
)

// Classes raised by the join itself rather than by the section 3b table.
const (
	parityJoinClassBeadIDCrossCheck = "bead_id_cross_check_failed"
	parityJoinClassUnclassified     = "UNCLASSIFIED"
	// parityJoinClassYieldFamilyMismatch fires when legacy stood down for one
	// family and a different family acted on the same row in the same tick.
	// Both writers saw the row; they disagree about what it IS.
	parityJoinClassYieldFamilyMismatch = "yield_family_mismatch"
	// parityJoinClassPendingCreateFamilySplit is shared by the two halves of
	// the D-WAKE / D-STALE-CREATE cross-family split so the triage log shows
	// one class, not two coincidences.
	parityJoinClassPendingCreateFamilySplit = "pending_create_in_flight_family_split"
	// parityJoinClassDeadlineCrossedAfterSweepSample names the idle deadline
	// that fell between the sweep's clock sample and legacy's own, inside one
	// tick. See the D-DEADLINE spec below for the evidence.
	parityJoinClassDeadlineCrossedAfterSweepSample = "deadline_crossed_after_sweep_sample"
	// parityJoinClassResetStallAlarmNoDetectorArm names legacy's reset-stall
	// alarm, which sits in D-STALL's site list but in none of its arms. See the
	// D-STALL spec below for the evidence.
	parityJoinClassResetStallAlarmNoDetectorArm = "reset_stall_alarm_no_detector_arm"
	// parityJoinClassPreWakeSupersedeConvergence names legacy's own pre-wake
	// fence firing on a candidate it had already decided. See the D-WAKE spec
	// below for the evidence.
	parityJoinClassPreWakeSupersedeConvergence = "pre_wake_supersede_convergence"
	// parityJoinClassOrphanLiveDetectorLead names the one-tick lead the sweep's
	// live-orphan arm holds over legacy's. See the D-ORPHAN spec below.
	parityJoinClassOrphanLiveDetectorLead = "orphan_live_detector_lead_one_tick"
	// parityJoinClassOrphanRunningSetUnavailable names the sweep's whole-family
	// fail-closed refusal when the provider could not produce the running set,
	// beside legacy's own per-row drain decision. See the D-ORPHAN spec below.
	parityJoinClassOrphanRunningSetUnavailable = "orphan_running_set_unavailable_fail_closed"
	// parityJoinClassDrainAckAdjacentCycleConvergence names legacy's drain
	// acknowledgement standing beside the keyed engine's own ack for the same
	// session, one cycle away. See the D-DRAIN spec below for the evidence and
	// the owner ruling that made it incomparable-with-verified-twin.
	parityJoinClassDrainAckAdjacentCycleConvergence = "drain_ack_adjacent_cycle_convergence"
)

// parityJoinAdjacentCycleWindow bounds how far a keyed acknowledgement may sit
// from the legacy decision it is claimed to be the twin of. It is ONE TICK: the
// campaign corpus's median inter-tick gap is 9.222s over 9010 consecutive tick
// pairs on 2026-08-15 and 11.454s over 8178 on 2026-08-17, so 11.5s is one tick
// at the slower of the two cadences. A window of one tick is the whole claim the
// class makes — the ack and the poll are two observations of the SAME drain
// episode, because no complete tick separates them and the reconciler re-decides
// a session's drain every tick. Widening it past a tick would let a genuinely
// later drain episode vouch for an earlier one.
//
// The bound is not tuned to the specimens: the nearest keyed twin in each of the
// three adjudicated cycles sits at 255ms, 162ms and 190ms, two orders of
// magnitude inside it.
const parityJoinAdjacentCycleWindow = 11500 * time.Millisecond

// legacyReasonNoWakeReason is the code legacy stamps at the drain decision when
// ComputeAwakeSet found no reason to be awake. It is NOT TraceReasonNoWakeReason
// ("no_wake_reason"): the site builds its reason code from its own hyphenated
// drain reason string (session_reconciler.go:4210, :4228), so the corpus carries
// the hyphenated spelling and a rule written against the constant never fires.
const legacyReasonNoWakeReason TraceReasonCode = "no-wake-reason"

// parityJoinDivergenceRule triages one expected divergence. Every non-empty
// predicate must hold for the rule to fire; the first matching rule wins.
type parityJoinDivergenceRule struct {
	Class            string
	Classification   string // defaults to parityJoinMismatched
	Side             parityJoinSide
	Sites            []TraceSiteCode
	LegacyReasons    []TraceReasonCode
	LegacyOutcomes   []TraceOutcomeCode
	DetectorReasons  []TraceReasonCode
	DetectorOutcomes []TraceOutcomeCode
	AnyReasons       []TraceReasonCode
	AnyOutcomes      []TraceOutcomeCode
	// DetectorAdmissionOutcomes constrains the sweep's own routing disposition
	// (fields.admission_outcome). It is what separates "the detector raised a
	// condition and routed it" from "the detector raised it and refused to
	// route it", which are opposite claims about the same record.
	DetectorAdmissionOutcomes []string
	// CoTwinReasons requires a record for the SAME session in the SAME cycle
	// carrying one of these reasons at ANOTHER site. It is how a CROSS-FAMILY
	// split gets triaged without inventing a predicate: the sweep claims each
	// row for exactly one family while legacy runs an arm per pass, so the two
	// writers can agree on a row and still land at two sites a same-site join
	// can never pair. A rule that names its twin fires only when the twin is
	// actually in the corpus — the singleton stays unclassified otherwise.
	CoTwinReasons []TraceReasonCode
	// AdjacentCycleKeyedTwinSites requires a KEYED-STAMPED record for the same
	// session at one of these sites, written in a DIFFERENT cycle within
	// parityJoinAdjacentCycleWindow of this record. It is CoTwinReasons' answer
	// to a skew the same-cycle index structurally cannot see: where a
	// cross-family split lands two writers in one cycle at two sites, an
	// ack-timing skew lands them at ONE site in two cycles, because the keyed
	// handler acks off-tick while legacy polls in-tick.
	//
	// The keyed side is identified by its OWNERSHIP STAMP (effect_owner), not by
	// detector_family: the keyed handler's ack records carry no family label at
	// all, so a detector_family predicate never fires on them.
	//
	// Each skew must prove its OWN twin. The class is incomparable only for the
	// rows that carry one; a twinless skew finds nothing here, falls through the
	// whole table, and stays an unclassified mismatch that blocks WE.
	AdjacentCycleKeyedTwinSites []TraceSiteCode
}

type parityJoinFamilySpec struct {
	Family      string
	Level       parityJoinLevel
	Sites       []TraceSiteCode
	Divergences []parityJoinDivergenceRule
}

// parityJoinGlobalDivergences apply to every family (the section 3b "(global)"
// row): on a partial store view legacy records Closed without closing while the
// detector suppresses the whole destructive family.
var parityJoinGlobalDivergences = []parityJoinDivergenceRule{{
	Class:         "store_query_partial_legacy_only",
	Side:          parityJoinSideLegacyOnly,
	LegacyReasons: []TraceReasonCode{TraceReasonStoreQueryPartial, TraceReasonStorePartial},
}}

var parityJoinFamilySpecs = []parityJoinFamilySpec{
	{
		// "existing shadow-worker + comparator evidence | per existing comparators"
		Family: parityJoinFamilyStart,
		Level:  parityJoinLevelAct,
		Sites: []TraceSiteCode{
			TraceSiteLifecycleStartPrepare,
			TraceSiteLifecycleStartExecute,
			TraceSiteLifecycleStartCommit,
			TraceSiteLifecycleStartRun,
			TraceSiteLifecycleStartFailed,
			TraceSiteLifecycleStartRollback,
			TraceSiteLifecycleStartSelectionShadow,
		},
	},
	{
		// "legacy pending-interaction deferral (probe-only signal, unpredicted)"
		Family: parityJoinFamilyDeadline,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteReconcilerIdleTimeout, TraceSiteReconcilerMaxSessionAge},
		Divergences: []parityJoinDivergenceRule{
			{
				Class:          "legacy_pending_interaction_deferral",
				LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeDeferred, TraceOutcomeDeferredPending, TraceOutcomeDeferredBusy},
			},
			{
				// The idle deadline crossed INSIDE the tick, after the sweep
				// looked and before legacy did.
				//
				// This is not a candidacy gap, and that is provable from the
				// source rather than argued from the corpus: city_runtime.go
				// hands the SAME *memoryIdleTracker (cr.it) to the sweep
				// (detectorSweepInput.Idle, :2956) and to the god function
				// (:2979) on the same tick, and both then call the same
				// idle_tracker.go:101 checkIdle against the same provider.
				// detectDeadline's only extra gate is live.Alive, which legacy
				// gates on too. The one degree of freedom left is the `now`
				// argument: the sweep captures one clk.Now() at
				// detectSessionConditions entry and shares it across every row,
				// while legacy takes a fresh clk.Now() per row, after the sweep
				// and after each row's provider probes. A deadline landing in
				// that gap is seen by legacy and not by the sweep, and legacy's
				// stop then ends the idle episode before the next sweep, so the
				// twin is never written at all.
				//
				// WD.15 window evidence (2026-08-12 03:39-07:52, 4125 cycles).
				// All 15 legacy idle stops are the same row shape — awake pool
				// worker, no blocker, template idle_timeout=20m — and 11 carry a
				// same-cycle detector_idle_timeout while 4 carry none, so no
				// shape separates them. The split is per-CYCLE, not per-row:
				// sibling sessions minted in one tick cross within seconds of
				// each other, and every observed cycle caught either all of its
				// siblings (5ru+g7d; a1w+8v7+zb9; gv8+xdj; 1xj+7e0+8b1) or none
				// (tca+v32+5rf) — the signature of one clock sample straddling a
				// tight cluster, which a candidacy predicate cannot produce. In
				// the four misses the sweep demonstrably evaluated the rows:
				// cycle-f1b13717f182f0b2 reports rows_evaluated=7 against 10
				// baselines with unknown_state_skipped=3, and its conditions=4
				// are fully accounted for by those 3 skips plus an unrelated
				// drain_ack — so no zombie and no orphan condition fired on the
				// three rows either, which is what proves live.Alive was true.
				// The window's cycles ran long (13.5s and 6.3s against a 3.77s
				// median sweep cadence), which is what widened the gap to the
				// observed 1.05s/1.85s/3.88s/7.25s.
				//
				// Classified MISMATCHED on purpose. The rule explains a
				// legacy-only destructive stop; it does not excuse one, and
				// `incomparable` would drop it out of the family's match rate.
				// Scoped to the idle STOP: the max-age arm shares the mechanism
				// but produced no record either way in this window, so it stays
				// unclassified rather than riding evidence it does not have.
				Class:          parityJoinClassDeadlineCrossedAfterSweepSample,
				Side:           parityJoinSideLegacyOnly,
				Sites:          []TraceSiteCode{TraceSiteReconcilerIdleTimeout},
				LegacyReasons:  []TraceReasonCode{TraceReasonIdleTimeout},
				LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeStop},
			},
		},
	},
	{
		// "deferred-confirm off-by-one (duplicated counters); liveness-error arm incomparable"
		Family: parityJoinFamilyOrphan,
		Level:  parityJoinLevelDecision,
		Sites: []TraceSiteCode{
			TraceSiteReconcilerOrphaned,
			TraceSiteReconcilerCloseOrphan,
			TraceSiteReconcilerCloseFailedCreate,
		},
		Divergences: []parityJoinDivergenceRule{
			{
				Class:          "liveness_error_arm",
				Classification: parityJoinIncomparable,
				AnyOutcomes:    []TraceOutcomeCode{TraceOutcomeSkippedLivenessError},
			},
			{
				Class:       "deferred_confirm_off_by_one",
				AnyOutcomes: []TraceOutcomeCode{TraceOutcomeDeferredConfirm},
			},
			{
				// The live-drain arm runs one tick AHEAD of legacy's. WD.15 day 4
				// reported three of these as unclassified; each has a legacy
				// orphaned/drain twin for the same session at this same site on the
				// very next tick, ~2.24s later and in a different trace cycle:
				// dependent-rc-njwi 032633->032634 (20:21:21.761 -> 20:21:23.997),
				// dependent-rc-c6xr 038223->038224, dependent-rc-h9bb7
				// 038793->038794. Both writers agree on the row AND on the effect;
				// only the tick differs, and the join is a same-cycle-handle
				// equality join by contract, so an adjacent-cycle pair can only
				// ever surface as two singletons. Same shape §3b already names for
				// D-DRAIN as ack-timing skew.
				//
				// MISMATCHED on purpose, like ack_timing_skew and unlike the
				// journey-proven arms: this rule explains the singleton, it cannot
				// prove the twin landed on the neighboring tick, and widening the
				// join to look would admit the cross-tick false pairs the cadence
				// decision deliberately excludes. The class clears the WE blocker;
				// the rate keeps counting the record.
				Class:            parityJoinClassOrphanLiveDetectorLead,
				Side:             parityJoinSideDetectorOnly,
				Sites:            []TraceSiteCode{TraceSiteReconcilerOrphaned},
				DetectorReasons:  []TraceReasonCode{detectorReasonOrphanLive},
				DetectorOutcomes: []TraceOutcomeCode{TraceOutcomeDrain},
			},
			{
				// WD.15 day 6 (cycle-7eb5acef63924183, tick ...-012843, real
				// 2026-08-16T17:16:14Z): the wd15-campaign tmux server dropped
				// mid-tick and the sweep's one-shot names-only ListRunning
				// errored, so the whole family failed closed for the cycle —
				// detector_running_set_unavailable / skipped / predicted_effect
				// none is a refusal to evaluate, not a detection ("proven
				// absence, not assumed absence", detectOrphan). Legacy probes
				// per-row instead and got a fresh non-error Running=true inside
				// the same second, so it re-decided the drain it had stood down
				// from for the previous 15 ticks; its GC_DRAIN_ACK write then
				// failed with the same "no tmux server running" in this cycle
				// and the next, so no effect entered the provider on either
				// side, and the keyed engine rebuilt the fleet within two
				// minutes.
				//
				// Incomparable for the wake_admission_refused_row_stays_legacy
				// reason: the sweep made NO claim on the row, so this compares
				// an act to a non-act — it measures the two engines'
				// degraded-input POLICY (sweep fails closed for the family,
				// legacy trusts its own per-row probe), not detection parity.
				// The zero-write invariant stays independently guarded: a sweep
				// record that acted on unavailable input would trip
				// ShadowEffectViolations regardless of this class. Scoped to
				// the exact fail-closed tuple; any other legacy conclusion
				// beside the refusal stays unclassified and blocks WE.
				Class:            parityJoinClassOrphanRunningSetUnavailable,
				Classification:   parityJoinIncomparable,
				Side:             parityJoinSideBoth,
				Sites:            []TraceSiteCode{TraceSiteReconcilerOrphaned},
				DetectorReasons:  []TraceReasonCode{detectorReasonRunningSetUnavailable},
				DetectorOutcomes: []TraceOutcomeCode{TraceOutcomeSkipped},
				LegacyReasons:    []TraceReasonCode{TraceReasonOrphaned},
				LegacyOutcomes:   []TraceOutcomeCode{TraceOutcomeDrain},
			},
		},
	},
	{
		// "legacy defers rollback #6+ (R6 budget retired)"
		Family: parityJoinFamilyStaleCreate,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteReconcilerPendingCreate, TraceSiteReconcilerPendingCreatePreserved},
		Divergences: []parityJoinDivergenceRule{
			{
				Class:       "legacy_defers_rollback_beyond_budget",
				AnyOutcomes: []TraceOutcomeCode{TraceOutcomeRollbackDeferred},
			},
			{
				// The sweep claims a row for ONE family; legacy runs an arm per
				// pass. A start-pending row whose create lease is still live is
				// claimed here (detectStaleCreate's preserve arm,
				// session_detector_sweep.go:1285-1289, predicted_effect "none")
				// while the start it already began is driven in the D-WAKE
				// family. Both leave the in-flight start alone; they just say so
				// in two families at two sites, so no same-site join can pair
				// them. Requires the twin.
				//
				// The twin has TWO spellings, because the start's driver depends
				// on who holds the key. Legacy drives it itself at
				// session_reconciler.go:4101 (reason "wake") — but under keyed
				// start ownership legacy stands down instead
				// (keyed_start_owner, the row-scan skip at :1880-1887 and the
				// wake-target stand-down at :4091-4098) and the keyed start
				// controller drives it. Either record proves the same two
				// things: legacy scanned the row this cycle, and the start
				// family is handling it — so the preserve singleton is not the
				// candidacy gap the twin requirement exists to catch. WD.15
				// day-2, cycle-7edba7f31f6960ea / ed3284ee51f04cc7 /
				// 5440d2778137242f, all three with baseline and result both
				// state=creating.
				//
				// No D-STALE-CREATE yield can stand in for it: the family's own
				// stand-down (keyed_stale_create_owner, :1835-1846) is gated on
				// pendingCreateLeaseExpiredForRollbackInfo, i.e. the ROLLBACK
				// arm only. A live lease — the preserve arm's whole premise —
				// can never produce one.
				Class:           parityJoinClassPendingCreateFamilySplit,
				Classification:  parityJoinIncomparable,
				Side:            parityJoinSideDetectorOnly,
				Sites:           []TraceSiteCode{TraceSiteReconcilerPendingCreatePreserved},
				DetectorReasons: []TraceReasonCode{detectorReasonPendingCreatePreserved},
				CoTwinReasons:   []TraceReasonCode{TraceReasonWake, "keyed_start_owner"},
			},
			{
				// Legacy defers a pending-create recovery only for a row whose
				// runtime is ALIVE (session_reconciler.go:3117-3125), and
				// detectStaleCreate excludes exactly those rows from the family
				// (session_detector_sweep.go:1272-1274). The legacy-only record
				// is therefore structural, not a candidacy gap.
				Class:          "live_runtime_recovery_excluded_from_sweep",
				Classification: parityJoinIncomparable,
				Side:           parityJoinSideLegacyOnly,
				Sites:          []TraceSiteCode{TraceSiteReconcilerPendingCreate},
				LegacyReasons:  []TraceReasonCode{TraceReasonPendingCreateRecoveryInFlight},
				LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeDeferred},
			},
		},
	},
	{
		// Detection level: the entire 5-arm ladder is handler-side, so reason and
		// outcome are not compared at all. No singleton rule — a drift record on
		// one side only is a real candidacy gap, not an expected divergence.
		Family: parityJoinFamilyDrift,
		Level:  parityJoinLevelDetection,
		Sites:  []TraceSiteCode{TraceSiteReconcilerConfigDrift, TraceSiteReconcilerLiveDrift},
	},
	{
		// "probe/pending arms unpredicted"
		Family: parityJoinFamilySleep,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteReconcilerDrainDecision},
		Divergences: []parityJoinDivergenceRule{
			{
				Class:          "probe_arm_unpredicted",
				Classification: parityJoinIncomparable,
				AnyReasons:     []TraceReasonCode{TraceReasonPending},
			},
			{
				Class:          "pending_arm_unpredicted",
				Classification: parityJoinIncomparable,
				AnyOutcomes:    []TraceOutcomeCode{TraceOutcomeDeferredPending},
			},
			{
				// WD.5 delta 1: legacy's plain "no-wake-reason" rung is a fleet
				// verdict the keyed handler cannot re-derive per key, so the
				// detector records those rows and never enqueues them. Legacy
				// drains where the detector predicts nothing, by design, until
				// D-WAKE gives the fleet demand rungs a keyed home.
				Class:           "fleet_only_no_wake_left_to_legacy",
				Classification:  parityJoinIncomparable,
				DetectorReasons: []TraceReasonCode{detectorReasonNoWakeFleetOnly},
			},
			{
				// The LEGACY-ONLY side of the same WD.5 delta 1 divergence. The
				// rule above keys on the detector's reason, so it cannot fire on a
				// row the sweep never wrote for — and the fleet verdict is exactly
				// the rung where the sweep may write nothing at all, because it
				// re-derives the awake set from its own pre-tick snapshot while
				// legacy drains on the verdict its own end-of-tick pass computed.
				// "Legacy drains where the detector predicts nothing, by design,
				// until D-WAKE gives the fleet demand rungs a keyed home" is the
				// class's own text; this arm lets it fire on that shape.
				Class:          "fleet_only_no_wake_left_to_legacy",
				Classification: parityJoinIncomparable,
				Side:           parityJoinSideLegacyOnly,
				Sites:          []TraceSiteCode{TraceSiteReconcilerDrainDecision},
				LegacyReasons:  []TraceReasonCode{legacyReasonNoWakeReason},
				LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeDrain},
			},
			{
				// WD.5 delta 4: the per-sweep probe budget and the probe already in
				// flight are detector-side scheduling, not a decision about the row.
				Class:           "idle_probe_scheduling",
				Classification:  parityJoinIncomparable,
				DetectorReasons: []TraceReasonCode{detectorReasonIdleProbePending, detectorReasonIdleProbeBudget},
			},
			{
				// WD.5 delta 2: the #3994 keep-alive escape is a detection-side
				// non-enqueue where legacy cancels mid-pass and records nothing.
				Class:           "keep_alive_escape_detector_only",
				Classification:  parityJoinIncomparable,
				Side:            parityJoinSideDetectorOnly,
				DetectorReasons: []TraceReasonCode{detectorReasonSleepKeepAlive},
			},
		},
	},
	{
		// "ack-timing skew (handler-side ack read vs legacy's in-tick poll);
		// advance arms journey-proven". Both are singleton classes: the pair lands
		// in adjacent cycles, which a same-cycle-handle join reports as one
		// legacy-only and one detector-only record. The advance arms are
		// incomparable on the co-twin in the SAME cycle; the ack skew is
		// incomparable on a keyed twin in an ADJACENT one, proved per row.
		Family: parityJoinFamilyDrain,
		Level:  parityJoinLevelDetection,
		Sites: []TraceSiteCode{
			TraceSiteReconcilerDrainAck,
			TraceSiteDrainCancel,
			TraceSiteDrainComplete,
			TraceSiteDrainStale,
			TraceSiteDrainTimeout,
			TraceSiteLifecycleDrainBegin,
			TraceSiteLifecycleDrainAdvance,
			TraceSiteSessionReconcileDrainAdvance,
		},
		Divergences: []parityJoinDivergenceRule{
			{
				// The sweep's DUE-ADVANCE detection (§1 row 28), which the sweep
				// sites at drain_ack. Legacy's advance pass has no per-session
				// decision record at all — session_reconcile.drain_advance is a
				// PHASE site that writes one cycle-level marker with no session
				// identity — so this family's advance parity is candidacy
				// agreement, not a record-to-record join, exactly as §3b's
				// "advance arms journey-proven" says. Placed above the
				// ack-timing-skew rule because both sit at drain_ack and this one
				// names the record precisely.
				Class:           "advance_arms_journey_proven",
				Classification:  parityJoinIncomparable,
				Sites:           []TraceSiteCode{TraceSiteReconcilerDrainAck},
				DetectorReasons: []TraceReasonCode{detectorReasonDrainInFlight},
			},
			{
				// Legacy's own acknowledgement arm, and the one D-DRAIN shape the
				// owner adjudicated by hand (signed 2026-08-17/18, WD.15 day 7).
				//
				// The skew is STRUCTURAL, not a detector defect: the keyed handler
				// reads the ack from inside its own operation while legacy polls
				// the same field in-tick, so the two writes land in different
				// cycles and a same-cycle-handle join reports one legacy-only
				// singleton with the keyed record accounted one cycle away. It is
				// also RARE — roughly 2 a day against ~90 comparable joins a day —
				// so it cannot be characterized statistically the way the volume
				// classes were.
				//
				// The ruling: incomparable, but only per-row and only on proof.
				// Each skew must individually show its keyed twin — same session,
				// same site, an adjacent cycle, inside one tick. That is the
				// co-twin requirement pre_wake_supersede_convergence carries, moved
				// from the same cycle to the adjacent one, and it is why the class
				// cannot blanket the site: a legacy ack with no keyed ack beside it
				// is a legacy write the keyed engine never made, which is a real
				// divergence and stays an unclassified mismatch blocking WE.
				//
				// Specimens: dependent-rc-7mzpx cycle-0860a236ff1b82bd
				// (2026-08-15T04:49:21.053Z, twins in cycle-880fa1b90288d6a4 and
				// cycle-793999c79d218eb5), s-rc-wisp-y73064d cycle-41b467cd627d719e
				// (2026-08-17T00:21:48.259Z) and s-rc-wisp-d30uo8f
				// cycle-03d51f88678dbb50 (2026-08-17T02:05:29.315Z).
				Class:                       parityJoinClassDrainAckAdjacentCycleConvergence,
				Classification:              parityJoinIncomparable,
				Side:                        parityJoinSideLegacyOnly,
				Sites:                       []TraceSiteCode{TraceSiteReconcilerDrainAck},
				AdjacentCycleKeyedTwinSites: []TraceSiteCode{TraceSiteReconcilerDrainAck},
			},
			{
				Class:          "advance_arms_journey_proven",
				Classification: parityJoinIncomparable,
				Sites: []TraceSiteCode{
					TraceSiteDrainComplete,
					TraceSiteLifecycleDrainAdvance,
					TraceSiteSessionReconcileDrainAdvance,
				},
			},
			{
				// The rest of the advance engine's arms. session_wake.go:686-825
				// writes drain.stale / drain.cancel / drain.timeout from inside
				// advanceSessionDrainsExcluding — the same pass §1 row 28 ports as
				// due-advance DETECTION with the interrupt/complete effects keyed.
				// The sweep records that detection at reconciler.session.drain_ack
				// (detector_drain_in_flight, predicted_effect "advance"), one site
				// away, so the pair splits. Requires the twin: an advance arm with
				// no detection beside it is a candidacy gap, not this class.
				Class:          "advance_arms_journey_proven",
				Classification: parityJoinIncomparable,
				Side:           parityJoinSideLegacyOnly,
				Sites: []TraceSiteCode{
					TraceSiteDrainStale,
					TraceSiteDrainCancel,
					TraceSiteDrainTimeout,
				},
				CoTwinReasons: []TraceReasonCode{detectorReasonDrainInFlight},
			},
		},
	},
	{
		// "legacy quarantine skip is UNTRACED (:3702-3705) -> detector-present/
		// legacy-absent, expected"
		Family: parityJoinFamilyWake,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteReconcilerWakeDecision, TraceSiteReconcilerPreserveConfiguredNamed},
		Divergences: []parityJoinDivergenceRule{
			{
				// The round-6 fence writes a THIRD legacy record on a row legacy
				// had already decided, and it is not a stand-down: parityJoinYieldOf
				// routes the keyed_start_owner spelling to the yield vocabulary, so
				// everything reaching here is the errPreWakeSuperseded convergence —
				// premise_drift:* (session_lifecycle_parallel.go:1278-1280),
				// mid_incarnation (:1281-1283) or the lost pre_wake_cas. All three
				// unwrap to one error and the executor treats them as one outcome
				// (:3502-3520): the candidate is dropped BEFORE the provider, so the
				// outcome is skipped and no effect landed on either side.
				//
				// Incomparable, and for the same reason as the refusal rule below:
				// the sweep's record for these rows declines to route them
				// (WD.15 day 4, all four at admission_outcome
				// refused_uncertifiable), so this compares an act to a non-act. It
				// is that population one record later — legacy's two-phase fence
				// catching its own stale snapshot — and the sweep has no prepare
				// phase to produce a counterpart.
				//
				// Requires the twin. A supersede with NO sweep record for the row
				// in the cycle is legacy fencing a candidate nothing else was
				// looking at; that stays unclassified and blocks WE.
				Class:          parityJoinClassPreWakeSupersedeConvergence,
				Classification: parityJoinIncomparable,
				Side:           parityJoinSideLegacyOnly,
				Sites:          []TraceSiteCode{TraceSiteReconcilerWakeDecision},
				LegacyReasons:  []TraceReasonCode{parityJoinReasonStartCommitSuperseded},
				CoTwinReasons:  []TraceReasonCode{detectorReasonWakeTarget},
			},
			{
				// The sweep raised a wake target and its OWN admission refused
				// to route it: detectorAdmissionRefusedUncertifiable is D-WAKE's
				// traced refusal for a call site with no certified-lease entry,
				// and its contract is that "the row stays legacy's and is
				// re-detected next sweep" (session_detector_sweep.go:392-396).
				// The sweep therefore made no claim on the row, and comparing
				// legacy's decision against a condition the sweep declined to
				// route compares an act to a non-act — on EITHER side. Where
				// legacy also wrote nothing, that is its untraced negative wake
				// arm (§1 row 19: the positive arm at :3777-3795 is the only one
				// traced); where it did write, the corpus pairs this refusal
				// with legacy's own start_in_flight decline, one second later in
				// the same tick, and both writers left the in-flight start alone.
				Class:                     "wake_admission_refused_row_stays_legacy",
				Classification:            parityJoinIncomparable,
				Sites:                     []TraceSiteCode{TraceSiteReconcilerWakeDecision},
				DetectorAdmissionOutcomes: []string{string(detectorAdmissionRefusedUncertifiable)},
			},
			{
				Class:           "untraced_legacy_quarantine_skip",
				Side:            parityJoinSideDetectorOnly,
				DetectorReasons: []TraceReasonCode{TraceReasonQuarantine},
			},
			{
				Class:            "untraced_legacy_quarantine_skip",
				Side:             parityJoinSideDetectorOnly,
				DetectorOutcomes: []TraceOutcomeCode{TraceOutcomeDeferredQuarantine},
			},
			{
				// The legacy-only half of the D-STALE-CREATE cross-family split
				// above: legacy's wake pass drives the start already in flight for
				// a start-pending row whose create lease is live, while the sweep
				// has claimed that row for D-STALE-CREATE's preserve arm. Same
				// row, same tick, two families. Requires the twin.
				Class:          parityJoinClassPendingCreateFamilySplit,
				Classification: parityJoinIncomparable,
				Side:           parityJoinSideLegacyOnly,
				Sites:          []TraceSiteCode{TraceSiteReconcilerWakeDecision},
				LegacyReasons:  []TraceReasonCode{TraceReasonWake},
				LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeStartCandidate, TraceOutcomeStartInFlight},
				CoTwinReasons:  []TraceReasonCode{detectorReasonPendingCreatePreserved},
			},
		},
	},
	{
		// Detection level: the classification arm is handler-side and therefore
		// already excluded from the comparison. A candidacy gap stays a mismatch.
		Family: parityJoinFamilyZombie,
		Level:  parityJoinLevelDetection,
		Sites:  []TraceSiteCode{TraceSiteReconcilerTerminalProviderError},
	},
	{
		// "claim-check-error fail-safe arm incomparable" — the fail-safe arm's
		// codes land with WD.13, so no rule yet; it triages as UNCLASSIFIED until
		// the slice that emits it extends this entry with evidence.
		Family: parityJoinFamilyStall,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteReconcilerResetStalled, TraceSiteReconcilerProgressStallExempt},
		Divergences: []parityJoinDivergenceRule{{
			// ResetStalled is in this family's site list by adjacency, not by
			// membership: it is in NONE of D-STALL's arms, and the sweep cannot
			// produce a record to pair with it for two independent reasons.
			//
			// Disjoint populations. recordResetStallIfDue returns unless the row
			// is NOT alive (session_reconciler.go:223, `if alive || ...`) and its
			// committed reset has outlived the startup timeout; detectStall
			// returns unless live.Alive (session_detector_sweep.go:1710) and then
			// gates on a last-activity gap. No row can satisfy both.
			//
			// Nothing to own. The site is an ALARM, not a decision: the function
			// prints to stderr, records a SessionResetStalled event and traces.
			// It mutates no session state, consumes no budget and enqueues
			// nothing, so there is no effect for a keyed handler to take over or
			// to double-apply — which is why this is INCOMPARABLE rather than an
			// explained-but-counted mismatch like the D-DEADLINE race.
			//
			// Corroborating, from the WD.15 window: exactly ONE such record in
			// 13,757 cycles (cycle-11e1730d6990ad8d, s-rc-wisp-u71tke, baseline
			// asleep / sleep_reason killed, elapsed_s 173 vs startup_timeout_s
			// 20), and the sweep raised its own D-WAKE condition for that same
			// row in that same cycle, so the absent D-STALL record is not the
			// sweep skipping the row. The whole family is also off by config in
			// the campaign city (progress_stall_timeout unset -> gate 0 -> the
			// early return above), which is the §6/Q3 test-only-parity case the
			// owner signed off on ga-f7v2ft.122 — zero detector D-STALL records
			// exist in the corpus to pair with anything.
			Class:          parityJoinClassResetStallAlarmNoDetectorArm,
			Classification: parityJoinIncomparable,
			Side:           parityJoinSideLegacyOnly,
			Sites:          []TraceSiteCode{TraceSiteReconcilerResetStalled},
			LegacyReasons:  []TraceReasonCode{TraceReasonResetStalled},
			LegacyOutcomes: []TraceOutcomeCode{TraceOutcomeFailed},
		}},
	},
	{
		// "none expected" — every divergence here is a WE blocker by design.
		Family: parityJoinFamilyDup,
		Level:  parityJoinLevelDecision,
		Sites:  []TraceSiteCode{TraceSiteSessionReconcileHealRetire},
	},
	{
		// "confirmation-window off-by-one (duplicated counters)" — the class
		// name is a misnomer WD.15 owns retiring (WD.14 delta 2): the window is
		// ONE durable marker (stranded_event_emitted_at) read by both paths, so
		// no counters can skew. What the rule actually triages is the detector's
		// in-window DEFER arm, which legacy records nothing for — this family
		// has no legacy decision record at all (WD.14 delta 1), so its detection
		// parity is candidacy agreement, not a record-to-record join.
		Family: parityJoinFamilyStranded,
		Level:  parityJoinLevelDetection,
		Sites:  []TraceSiteCode{TraceSiteSessionReconcileWakeSleep},
		Divergences: []parityJoinDivergenceRule{{
			Class:       "confirmation_window_off_by_one",
			AnyOutcomes: []TraceOutcomeCode{TraceOutcomeDeferredConfirm},
		}},
	},
}

// parityJoinSiteAttribution says what an effect_owner-ABSENT record at a site
// means. It is the machine-readable half of the section 1 site-disposition
// table: which of the 28 sites the god function itself writes a per-session
// decision at, and which it does not touch.
//
// The tool needs this because no line of production code stamps
// effect_owner="legacy". Every keyed handler stamps "keyed", the sweep stamps
// "detector-shadow" (or "keyed" for a routed condition), and legacy stamps
// nothing — so legacy is identified by ELIMINATION, not by a stamp it will
// never carry. The alternative, teaching the god function to stamp, is
// scaffolding built into code scheduled for deletion at WE.
type parityJoinSiteAttribution string

const (
	// parityJoinSiteLegacy is a section 1 DECISION site: the god function
	// writes a per-session decision record here, unstamped. Absence of
	// effect_owner classifies the record as legacy.
	parityJoinSiteLegacy parityJoinSiteAttribution = "legacy"
	// parityJoinSitePhase is a section 1 PHASE site. Legacy writes exactly one
	// cycle-level marker per tick here (reason=retained, outcome=complete, no
	// session identity); only the keyed and detector writers write per-session
	// rows, and they stamp. Binning the marker as legacy would manufacture one
	// phantom legacy-only row per cycle in D-DUP and D-STRANDED, whose only
	// sites are phase sites.
	parityJoinSitePhase parityJoinSiteAttribution = "phase"
	// parityJoinSiteNonLegacy is a site with no legacy per-session writer, or
	// one whose writer serves both engines. Absence cannot be attributed by
	// elimination, so the record is counted and surfaced rather than binned.
	parityJoinSiteNonLegacy parityJoinSiteAttribution = "non_legacy"
)

type parityJoinSiteDisposition struct {
	Attribution parityJoinSiteAttribution
	// Note is the section 1 row (or the writer) this transcribes, so a reader
	// of the readout can check the claim without reading Go.
	Note string
}

// parityJoinSiteDispositions covers every site the section 3b family table
// claims. TestParityJoinSiteDispositionsCoverEveryFamilySite enforces that.
var parityJoinSiteDispositions = map[TraceSiteCode]parityJoinSiteDisposition{
	// start — section 1 row 27 (StartExecution) is KEYED-OWNED ALREADY, and its
	// shared start wave serves both paths, so nothing here attributes. Section
	// 3b routes this family to the existing shadow-worker comparators anyway.
	TraceSiteLifecycleStartPrepare:         {parityJoinSiteNonLegacy, "s1#27 keyed-owned: the keyed start wave fires lifecycle.start.prepare"},
	TraceSiteLifecycleStartExecute:         {parityJoinSiteNonLegacy, "s1#27 keyed-owned: the keyed start wave fires lifecycle.start.execute"},
	TraceSiteLifecycleStartCommit:          {parityJoinSiteNonLegacy, "s1#27 keyed-owned: the keyed start wave fires lifecycle.start.commit"},
	TraceSiteLifecycleStartRun:             {parityJoinSiteNonLegacy, "s1#27 shared start wave (session_lifecycle_parallel.go) serves both paths"},
	TraceSiteLifecycleStartFailed:          {parityJoinSiteNonLegacy, "s1#27 shared start wave (session_lifecycle_parallel.go) serves both paths"},
	TraceSiteLifecycleStartRollback:        {parityJoinSiteNonLegacy, "s1#27 shared start wave (session_lifecycle_parallel.go) serves both paths"},
	TraceSiteLifecycleStartSelectionShadow: {parityJoinSiteNonLegacy, "start-selection shadow comparator, not a reconciler decision"},

	// D-DEADLINE
	TraceSiteReconcilerIdleTimeout:   {parityJoinSiteLegacy, "s1#1 IdleTimeout"},
	TraceSiteReconcilerMaxSessionAge: {parityJoinSiteLegacy, "s1#2 MaxSessionAge"},

	// D-ORPHAN
	TraceSiteReconcilerOrphaned:          {parityJoinSiteLegacy, "s1#3 Orphaned"},
	TraceSiteReconcilerCloseOrphan:       {parityJoinSiteLegacy, "s1#4 CloseOrphan"},
	TraceSiteReconcilerCloseFailedCreate: {parityJoinSiteLegacy, "s1#5 CloseFailedCreate"},

	// D-STALE-CREATE
	TraceSiteReconcilerPendingCreate:          {parityJoinSiteLegacy, "s1#6 PendingCreate"},
	TraceSiteReconcilerPendingCreatePreserved: {parityJoinSiteLegacy, "s1#7 PendingCreatePreserved"},

	// D-DRIFT
	TraceSiteReconcilerConfigDrift: {parityJoinSiteLegacy, "s1#8 ConfigDrift"},
	TraceSiteReconcilerLiveDrift:   {parityJoinSiteLegacy, "s1#9 LiveDrift"},

	// D-SLEEP
	TraceSiteReconcilerDrainDecision: {parityJoinSiteLegacy, "s1#12 DrainDecision"},

	// D-DRAIN. The legacy drain engine (session_wake.go) writes the four
	// reconciler.drain.* sites unstamped; the keyed drain handler
	// (session_drain_reconcile.go) writes the same sites with effect_owner=keyed.
	TraceSiteReconcilerDrainAck:           {parityJoinSiteLegacy, "s1#10 DrainAck"},
	TraceSiteDrainCancel:                  {parityJoinSiteLegacy, "s1#11 DrainCancel"},
	TraceSiteDrainStale:                   {parityJoinSiteLegacy, "legacy drain engine session_wake.go, unstamped"},
	TraceSiteDrainComplete:                {parityJoinSiteLegacy, "legacy drain engine session_wake.go, unstamped"},
	TraceSiteDrainTimeout:                 {parityJoinSiteLegacy, "legacy drain engine session_wake.go, unstamped"},
	TraceSiteLifecycleDrainBegin:          {parityJoinSiteNonLegacy, "no production writer"},
	TraceSiteLifecycleDrainAdvance:        {parityJoinSiteNonLegacy, "keyed drain advance (session_start_reconcile.go)"},
	TraceSiteSessionReconcileDrainAdvance: {parityJoinSitePhase, "s1#28 DrainAdvance (phase)"},

	// D-WAKE
	TraceSiteReconcilerWakeDecision:            {parityJoinSiteLegacy, "s1#19 WakeDecision"},
	TraceSiteReconcilerPreserveConfiguredNamed: {parityJoinSiteLegacy, "s1#13 PreserveConfiguredNamed"},

	// D-ZOMBIE
	TraceSiteReconcilerTerminalProviderError: {parityJoinSiteLegacy, "s1#15 TerminalProviderError"},

	// D-STALL
	TraceSiteReconcilerResetStalled:        {parityJoinSiteLegacy, "legacy stall reset (session_reconciler.go), unstamped"},
	TraceSiteReconcilerProgressStallExempt: {parityJoinSiteLegacy, "s1#14 ProgressStallExempt"},

	// D-DUP / D-STRANDED: phase sites only. Legacy has no per-session decision
	// record in either family (WD.13 / WD.14 delta 1), so their detection parity
	// is candidacy agreement, not a record-to-record join.
	TraceSiteSessionReconcileHealRetire: {parityJoinSitePhase, "s1#22 HealRetire (phase)"},
	TraceSiteSessionReconcileWakeSleep:  {parityJoinSitePhase, "s1#26 WakeSleep (phase)"},
}

// parityJoinYieldArm records what a traced stand-down PROVES about its row.
type parityJoinYieldArm string

const (
	// parityJoinYieldCandidacy: the seam sits INSIDE the family's arm, so
	// legacy had already established the row is actionable when it stepped
	// aside. The record is legacy's judgment on the row — the left-hand side
	// of the yield-join.
	parityJoinYieldCandidacy parityJoinYieldArm = "candidacy"
	// parityJoinYieldOwnership: the seam sits at the top of the row scan,
	// before any condition is evaluated — or its arms are indistinguishable in
	// the record. It asserts "the keyed controller holds this key", NOT "this
	// row is actionable". It still joins when an actor is present (the actor
	// carries the candidacy), but an unpaired ownership yield is not evidence
	// of anything and is refused rather than counted as a divergence.
	parityJoinYieldOwnership parityJoinYieldArm = "ownership"
)

// parityJoinYieldSpec is one entry in legacy's stand-down vocabulary.
type parityJoinYieldSpec struct {
	Family string
	Arm    parityJoinYieldArm
	// Note cites the emitting seam so a reader of the readout can check the
	// arm classification without reading Go.
	Note string
}

// parityJoinYieldVocabulary is every traced stand-down the coexistence seams
// emit. Under the owner's yield-join ruling these ARE legacy's decision records:
// a stand-down is "I identified this row and stepped aside", which beside the
// keyed actor's record is a per-row per-tick side-by-side comparison — the
// evidence D4 asks for, and the only one auto mode produces in volume, because
// auto mode is precisely the mode in which legacy declines rather than decides.
//
// A yield is identified by its REASON, never by effect_owner: most seams stamp
// effect_owner=keyed (the effect IS keyed's), which is indistinguishable from
// the actor's own stamp, and two of them (the wake arm at session_reconciler.go
// :1882/:4093 and the drain-ack arms at :2304/:2756) stamp nothing at all.
//
// The untraced stand-downs are deliberately absent: D-DUP's duplicate-retire,
// D-STALL's recycle (:2889), D-STRANDED's repair (:4294) and D-DRAIN's advance
// (:2247, :2634, :4388) suppress their effects without recording anything, so
// they contribute no yield evidence and their families rest on act-vs-act and
// journey parity instead.
var parityJoinYieldVocabulary = map[TraceReasonCode]parityJoinYieldSpec{
	"keyed_start_owner": {
		Family: parityJoinFamilyWake,
		// TWO arms share this reason and payload shape: the row-scan skip at
		// session_reconciler.go:1880-1887, which fires before legacy evaluates
		// any condition, and the wake-target stand-down at :4091-4098, which
		// fires after legacy decided the row is a start candidate. Only the
		// second carries candidacy, and nothing in the record tells them apart
		// (both write session_id and nothing else), so the conservative arm
		// governs: the pair still joins, the unpaired yield is refused.
		Arm:  parityJoinYieldOwnership,
		Note: "s1#19 WakeDecision seam; row-scan :1882 and wake-target :4093 are indistinguishable in the record",
	},
	"keyed_deadline_owner": {
		Family: parityJoinFamilyDeadline,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:3586-3600, gated on an armed idle/max-age tracker and a live row",
	},
	"keyed_orphan_close_owner": {
		Family: parityJoinFamilyOrphan,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:1539-1551, inside both legacy close arms",
	},
	"keyed_orphan_drain_owner": {
		Family: parityJoinFamilyOrphan,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:2428-2441, after legacy determined the row is orphaned",
	},
	"keyed_stale_create_owner": {
		Family: parityJoinFamilyStaleCreate,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:1835-1846, gated on the keyed handler's own rollback predicate",
	},
	"keyed_config_drift_owner": {
		Family: parityJoinFamilyDrift,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:1577-1583, at each convergence effect, drift key re-derived",
	},
	"keyed_config_drift_defer_owner": {
		Family: parityJoinFamilyDrift,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:1594-1605, at each deferral effect, conjunctive with convergence",
	},
	"keyed_drain_ack_owner": {
		Family: parityJoinFamilyDrain,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:2302-2309, :2754-2761, inside the stop-pending arms (ga-f7v2ft.147)",
	},
	"keyed_zombie_mark_owner": {
		Family: parityJoinFamilyZombie,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:2535-2540, gated on running AND not alive",
	},
	"keyed_sleep_owner": {
		Family: parityJoinFamilySleep,
		Arm:    parityJoinYieldCandidacy,
		Note:   "session_reconciler.go:4191-4198, inside the no-wake drain block",
	},
}

// parityJoinReasonStartCommitSuperseded is the round-6 pre-wake supersede. It
// is a yield only when its own "reason" field names the keyed start owner —
// the other spellings (premise_drift:*, mid_incarnation, pre_wake_cas) are
// convergence against a moved row, not a stand-down for another writer.
const parityJoinReasonStartCommitSuperseded TraceReasonCode = "start_commit_superseded"

// parityJoinSupersedeYieldReason is the value the supersede's reason field
// carries when the pre-wake CAS found the keyed start owner holding the key
// (session_lifecycle_parallel.go:1275-1277, surfaced at :3507-3518).
const parityJoinSupersedeYieldReason = "keyed_start_owner"

// parityJoinYieldOf resolves a record against the stand-down vocabulary.
func parityJoinYieldOf(rec SessionReconcilerTraceRecord) (parityJoinYieldSpec, bool) {
	if rec.ReasonCode == parityJoinReasonStartCommitSuperseded {
		if reason, _ := rec.Fields["reason"].(string); reason == parityJoinSupersedeYieldReason {
			return parityJoinYieldSpec{
				Family: parityJoinFamilyWake,
				Arm:    parityJoinYieldCandidacy,
				Note:   "pre-wake CAS supersede, session_lifecycle_parallel.go:3507-3518, after the candidate was decided",
			}, true
		}
		return parityJoinYieldSpec{}, false
	}
	spec, ok := parityJoinYieldVocabulary[rec.ReasonCode]
	return spec, ok
}

// parityJoinDetectorFamilies maps the sweep's own family label onto the section
// 3b family names, so the yield-join can check that the family legacy stood
// down FOR is the family that acted. A D-DEADLINE yield beside a D-ORPHAN act is
// a divergence to classify, not a match.
var parityJoinDetectorFamilies = map[string]string{
	string(detectorFamilyDeadline):    parityJoinFamilyDeadline,
	string(detectorFamilyOrphan):      parityJoinFamilyOrphan,
	string(detectorFamilyStaleCreate): parityJoinFamilyStaleCreate,
	string(detectorFamilyDrift):       parityJoinFamilyDrift,
	string(detectorFamilySleep):       parityJoinFamilySleep,
	string(detectorFamilyDrain):       parityJoinFamilyDrain,
	string(detectorFamilyWake):        parityJoinFamilyWake,
	string(detectorFamilyZombie):      parityJoinFamilyZombie,
	string(detectorFamilyStall):       parityJoinFamilyStall,
	string(detectorFamilyDup):         parityJoinFamilyDup,
	string(detectorFamilyStranded):    parityJoinFamilyStranded,
}

// parityJoinSiteFamily indexes every section 3b site to its family.
var parityJoinSiteFamily = func() map[TraceSiteCode]*parityJoinFamilySpec {
	index := make(map[TraceSiteCode]*parityJoinFamilySpec)
	for i := range parityJoinFamilySpecs {
		spec := &parityJoinFamilySpecs[i]
		for _, site := range spec.Sites {
			index[site] = spec
		}
	}
	return index
}()
