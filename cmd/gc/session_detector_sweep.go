package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The detector sweep is the read-only observation half of the reconciler
// distillation. It runs beside the legacy god function at all three production
// entry points and classifies every session row into condition families. It
// never mutates the store, the provider, or any domain state, and during the
// WD wave it never enqueues: every family's act constant below is false until
// the WE cutover commit flips it.

// detectorFamily names one condition family from DETECTOR.md §3. The value is
// the stable key used in trace payloads and by the WD.15 parity join.
type detectorFamily string

const (
	detectorFamilyDeadline    detectorFamily = "d-deadline"
	detectorFamilyOrphan      detectorFamily = "d-orphan"
	detectorFamilyStaleCreate detectorFamily = "d-stale-create"
	detectorFamilyDrift       detectorFamily = "d-drift"
	detectorFamilySleep       detectorFamily = "d-sleep"
	detectorFamilyDrain       detectorFamily = "d-drain"
	detectorFamilyWake        detectorFamily = "d-wake"
	detectorFamilyZombie      detectorFamily = "d-zombie"
	detectorFamilyStall       detectorFamily = "d-stall"
	detectorFamilyDup         detectorFamily = "d-dup"
	detectorFamilyStranded    detectorFamily = "d-stranded"
	// detectorFamilyUnknownState is the global unknown-state guard, not a
	// condition family: it never predicts an effect, so it is never destructive
	// and never suppressed by the partial-store guard.
	detectorFamilyUnknownState detectorFamily = "d-unknown-state"
	// detectorFamilyExecutionCompletionScan is an OBSERVED-ONLY patrol member
	// added to main after the WD design was written (WC merge 71d1dad702). It is
	// an unbounded per-tick patrol scan that contradicts A1; the sweep
	// inventories it so the campaign readout sees it, but does not run, absorb,
	// or duplicate it. Absorption-or-retirement is adjudicated at WE.
	//
	// Its sibling, the ready-demand scan, was ABSORBED at WD.10b: Q2 resolved
	// yes-with-promotion, so the scan is now the sweep's own declared
	// routed-work view (readyRoutedWorkView) rather than a foreign scan to
	// inventory. It stopped being an observed-only family when the sweep started
	// owning the read.
	detectorFamilyExecutionCompletionScan detectorFamily = "d-execution-completion-scan"
)

// Per-family act constants. THE SHADOW/ACT RULE IS PER-FAMILY AND EXPLICIT, NOT
// the session-start ownership latch (DETECTOR.md §3, shared shadow shape): the
// latch reads Keyed in the very auto-mode campaign city the evidence run needs,
// while legacy yields only at the start-family seams — there is no legacy yield
// for idle stop, orphan close, drift, stall, dup, or stranded, so a latch-gated
// enqueue would double-act beside a non-yielding legacy.
//
// A constant flips only when that family's keyed handler AND its legacy yield
// have both landed — an acting family beside a non-yielding legacy double-acts
// by construction. D-DEADLINE crossed at WD.2 (handler:
// session_deadline_reconcile.go; yield: withLegacyDeadlineStopExclusion).
// D-ORPHAN has TWO effect arms and therefore TWO constants: its CLOSE arms
// crossed at WD.3 (handler: session_orphan_close_reconcile.go; yield:
// withLegacyOrphanCloseExclusion) and its live-orphan DRAIN arm at WD.4
// (handler: session_orphan_drain_reconcile.go; yield:
// withLegacyOrphanDrainExclusion). One constant governing both would have made
// a family cross wholesale the moment either half landed, which is exactly the
// double-act this rule exists to prevent; the family-spec table folds the two
// back into one Acts bit. D-STALE-CREATE crossed at WD.7 (handler:
// session_stale_create_reconcile.go; yield:
// withLegacyStaleCreateRollbackExclusion). D-DUP crossed at WD.13 (handler:
// session_dup_reconcile.go; yield: withLegacyDuplicateRetireExclusion). D-STALL
// crossed at WD.12 (handler: session_stall_reconcile.go; yield:
// withLegacyProgressStallRecycleExclusion) — one constant for both its arms,
// because the claim-less and claim-holder arms share one condition, one handler
// and one legacy yield, and differ only in which threshold answered.
// D-DRIFT splits like D-ORPHAN but along a different seam: its CONVERGENCE
// arms (silent rebaseline, launch-only relaunch, restart-in-place, drift drain,
// live re-apply) crossed at WD.8 (handler: session_drift_reconcile.go; yield:
// withLegacyConfigDriftConvergeExclusion) and its DEFERRAL arms — attached,
// recently-attached, named-active, pending-interaction, live-assigned-work — at
// WD.9 (same handler; yield: withLegacyConfigDriftDeferExclusion). One constant
// cannot express that split because the DETECTOR cannot see it: attachment is
// provider I/O, so both arms ride one detected condition and the ladder forks
// inside the handler. D-SLEEP crossed at WD.5 (handler:
// session_sleep_reconcile.go; yield: withLegacySleepDrainExclusion) with ONE
// constant for its whole effect arm: unlike D-ORPHAN its idle probe and its
// drain are two rungs of one ladder on one key that landed in one slice, so a
// second constant would gate nothing. D-STRANDED crossed at WD.14 (handler:
// session_stranded_reconcile.go; yield: withLegacyStrandedRepairExclusion),
// gating only the DESTRUCTIVE half of legacy's block: the diagnostic above it
// keeps stamping the confirmation marker, because that marker is the keyed
// family's own entry condition. D-DRAIN crossed at WD.6 (handler:
// session_drain_reconcile.go; yield: withLegacyDrainAdvanceExclusion) with ONE
// constant for a family that straddles two legacy positions — the forward-pass
// acknowledgement block and the end-of-tick advance scan — because both are one
// ladder over one in-memory intent, and a constant per position would gate
// halves of a single decision. D-ZOMBIE crossed at WD.11 (handler:
// session_zombie_reconcile.go; yield: withLegacyZombieMarkExclusion) with ONE
// constant: its single arm is one condition, one handler and one yield.
// D-WAKE splits like D-ORPHAN, along the seam its
// two slices divide on: its NAMED and CONFIGURED-DEPENDENCY demand crossed at
// WD.10a (handler: the existing reconcileExactSessionStartWithOwner admission
// chain, entered under the certified wake leases; yield: the three wake-lease
// arms of sessionStartLegacyExclusionPredicate, which drive the SAME
// withLegacyStartExclusion bridge legacy's start family already honors — no new
// exclusion helper, because the gap ga-ij8mh found was entirely PRE-lease and
// the pre-lease seam closes it). Its POOL-FILL arm crossed at WD.10b with a
// yield of its own: the ALLOCATION-OWNERSHIP seam
// (keyedRoutedWorkAllocations, consulted at legacy's member-creation boundary
// in revalidatePlannedPoolMemberDemand), because the arm that materializes a
// member races the legacy pool builder rather than legacy's start family. One
// constant could not express that split: the arms have different admission
// leases, different sinks, and land a slice apart, which is exactly the
// double-act the per-family rule exists to prevent.
// The rest flip in the WE cutover commit,
// one family at a time, once the WD.15 parity window has cleared their
// must-match bar. They are compile-time constants on purpose: this is not a
// config surface.
const (
	detectorActDeadline                = true
	detectorActOrphanClose             = true
	detectorActOrphanDrain             = true
	detectorActStaleCreate             = true
	detectorActDriftConverge           = true
	detectorActDriftDefer              = true
	detectorActSleep                   = true
	detectorActDrain                   = true
	detectorActWakeNamedDependency     = true
	detectorActWakePoolFill            = true
	detectorActZombie                  = true
	detectorActStall                   = true
	detectorActDup                     = true
	detectorActStranded                = true
	detectorActExecutionCompletionScan = false
)

// detectorShadowEffectOwner is the effect_owner every sweep record carries. It
// is distinct from "legacy" and "keyed" so the WD.15 parity join can separate
// the three record populations on a shared trace cycle.
const detectorShadowEffectOwner = "detector-shadow"

// detectorKeyedEffectOwner is the effect_owner a routed condition carries. The
// key belongs to the keyed population from the moment it is admitted, so the
// WD.15 join must not count it against the shadow arm.
const detectorKeyedEffectOwner = "keyed"

// detectorFamilyRecordBudget bounds how many per-session records one family may
// emit in a single trace cycle; beyond it the sweep emits one summary record
// naming the suppressed count. Eleven acting families at this bound cost at most
// 11*100 + 13 records per cycle, so doubling the cycle's volume beside legacy
// stays inside sessionReconcilerTraceMaxRecordsPerCycle (4000).
const detectorFamilyRecordBudget = 100

// Detector-shadow reason vocabulary. Every code is `detector_`-prefixed and sits
// deliberately outside shouldAutoArmForTrace's reason leg
// (session_reconciler_trace_collector.go), so a shadow record can never trigger
// ensureAutoArm: no arms.json write, no detail-scope mutation, and none of the
// four auto-arm slots consumed. TestDetectorShadowVocabularyNeverAutoArms pins
// this against both the reason and the outcome leg.
const (
	detectorReasonIdleTimeout             TraceReasonCode = "detector_idle_timeout"
	detectorReasonMaxSessionAge           TraceReasonCode = "detector_max_session_age"
	detectorReasonDeadlineDeferred        TraceReasonCode = "detector_deadline_deferred"
	detectorReasonOrphanDead              TraceReasonCode = "detector_orphan_dead"
	detectorReasonOrphanLive              TraceReasonCode = "detector_orphan_live"
	detectorReasonOrphanAssignedWork      TraceReasonCode = "detector_orphan_assigned_work"
	detectorReasonOrphanSuspendDeferred   TraceReasonCode = "detector_orphan_suspend_deferred"
	detectorReasonFailedCreate            TraceReasonCode = "detector_failed_create"
	detectorReasonStalePendingCreate      TraceReasonCode = "detector_stale_pending_create"
	detectorReasonPendingCreatePreserved  TraceReasonCode = "detector_pending_create_preserved"
	detectorReasonConfigDrift             TraceReasonCode = "detector_config_drift"
	detectorReasonLiveDrift               TraceReasonCode = "detector_live_drift"
	detectorReasonNoWake                  TraceReasonCode = "detector_no_wake"
	detectorReasonNoWakeFleetOnly         TraceReasonCode = "detector_no_wake_fleet_only"
	detectorReasonSleepKeepAlive          TraceReasonCode = "detector_sleep_keep_alive"
	detectorReasonIdleProbePending        TraceReasonCode = "detector_idle_probe_pending"
	detectorReasonIdleProbeBudget         TraceReasonCode = "detector_idle_probe_budget"
	detectorReasonDrainInFlight           TraceReasonCode = "detector_drain_in_flight"
	detectorReasonWakeTarget              TraceReasonCode = "detector_wake_target"
	detectorReasonWakeTargetNamed         TraceReasonCode = "detector_wake_target_named"
	detectorReasonWakeTargetDependency    TraceReasonCode = "detector_wake_target_dependency"
	detectorReasonWakePoolFill            TraceReasonCode = "detector_wake_pool_fill"
	detectorReasonWakeBlocked             TraceReasonCode = "detector_wake_blocked"
	detectorReasonZombie                  TraceReasonCode = "detector_zombie"
	detectorReasonProgressStall           TraceReasonCode = "detector_progress_stall"
	detectorReasonProgressStallExempt     TraceReasonCode = "detector_progress_stall_exempt"
	detectorReasonDuplicateNamed          TraceReasonCode = "detector_duplicate_named"
	detectorReasonStrandedPoolSlot        TraceReasonCode = "detector_stranded_pool_slot"
	detectorReasonStrandedConfirmDeferred TraceReasonCode = "detector_stranded_confirm_deferred"
	detectorReasonUnknownStateSkipped     TraceReasonCode = "detector_unknown_state_skipped"
	detectorReasonStoreQueryPartial       TraceReasonCode = "detector_store_query_partial"
	detectorReasonFamilyBudgetExceeded    TraceReasonCode = "detector_family_budget_exceeded"
	detectorReasonObservedPatrolScan      TraceReasonCode = "detector_observed_patrol_scan"
	detectorReasonSweepComplete           TraceReasonCode = "detector_sweep_complete"
	detectorReasonRunningSetUnavailable   TraceReasonCode = "detector_running_set_unavailable"
	detectorReasonProviderLivenessUnknown TraceReasonCode = "detector_provider_liveness_unknown"
)

// detectorShadowReasons is the closed reason vocabulary of the sweep.
var detectorShadowReasons = []TraceReasonCode{
	detectorReasonIdleTimeout,
	detectorReasonMaxSessionAge,
	detectorReasonDeadlineDeferred,
	detectorReasonOrphanDead,
	detectorReasonOrphanLive,
	detectorReasonOrphanAssignedWork,
	detectorReasonOrphanSuspendDeferred,
	detectorReasonFailedCreate,
	detectorReasonStalePendingCreate,
	detectorReasonPendingCreatePreserved,
	detectorReasonConfigDrift,
	detectorReasonLiveDrift,
	detectorReasonNoWake,
	detectorReasonNoWakeFleetOnly,
	detectorReasonSleepKeepAlive,
	detectorReasonIdleProbePending,
	detectorReasonIdleProbeBudget,
	detectorReasonDrainInFlight,
	detectorReasonWakeTarget,
	detectorReasonWakeTargetNamed,
	detectorReasonWakeTargetDependency,
	detectorReasonWakePoolFill,
	detectorReasonWakeBlocked,
	detectorReasonZombie,
	detectorReasonProgressStall,
	detectorReasonProgressStallExempt,
	detectorReasonDuplicateNamed,
	detectorReasonStrandedPoolSlot,
	detectorReasonStrandedConfirmDeferred,
	detectorReasonUnknownStateSkipped,
	detectorReasonStoreQueryPartial,
	detectorReasonFamilyBudgetExceeded,
	detectorReasonObservedPatrolScan,
	detectorReasonSweepComplete,
	detectorReasonRunningSetUnavailable,
	detectorReasonProviderLivenessUnknown,
}

// detectorShadowOutcomes is the closed outcome vocabulary of the detector-owned
// record populations — the sweep's own shadow records plus the outcomes a landed
// handler records, including the three deferral outcomes D-DRIFT's A6 half
// carries (WD.9). It doubles as the routing frontier's enumeration: every
// outcome a family must NOT enqueue under is drawn from here. It may never
// contain TraceOutcomeFailed, TraceOutcomeProviderError, or
// TraceOutcomeDeadlineExceeded: shouldAutoArmForTrace's OUTCOME leg fires on
// those regardless of reason, which would let a shadow record write arms.
var detectorShadowOutcomes = []TraceOutcomeCode{
	TraceOutcomeStop,
	TraceOutcomeClosed,
	TraceOutcomeDrain,
	TraceOutcomeRollback,
	TraceOutcomeStartCandidate,
	TraceOutcomeSkipped,
	TraceOutcomeNoChange,
	TraceOutcomeDeferredConfirm,
	TraceOutcomeDeferredAttached,
	TraceOutcomeDeferredActive,
	TraceOutcomeDeferredPending,
}

// detectorFamilySpec records the fixed properties of one family: whether its
// keyed effect is destructive (close, stop, drain, rollback, retire — the set
// the storeQueryPartial guard fails closed on), whether it is observed-only,
// whether it may enqueue yet, and whether the D2 stop-capability screen applies
// to it.
type detectorFamilySpec struct {
	Family       detectorFamily
	Destructive  bool
	ObservedOnly bool
	Acts         bool
	// StopCapabilityExempt drops the family out of the D2 routing screen. It is
	// set for exactly one reason: the family's handler does NOT use the
	// token-bound unattended stop the screen exists to guarantee. D-DUP reuses
	// the retire path's own stop-before-mutate
	// (stopRuntimeBeforeSessionBeadMutationInfo: IsRunning → kill → IsRunning),
	// a self-verifying stop every provider supports. Screening it on D2 would
	// make the keyed family strictly less capable than the legacy phase it
	// replaces and strand duplicate named rows forever on non-tmux providers.
	// The partial-store suppression still applies: the effect is destructive.
	StopCapabilityExempt bool
}

var detectorFamilySpecs = []detectorFamilySpec{
	{Family: detectorFamilyDeadline, Destructive: true, Acts: detectorActDeadline},
	{Family: detectorFamilyOrphan, Destructive: true, Acts: detectorActOrphanClose || detectorActOrphanDrain},
	{Family: detectorFamilyStaleCreate, Destructive: true, Acts: detectorActStaleCreate},
	{Family: detectorFamilyDrift, Destructive: true, Acts: detectorActDriftConverge || detectorActDriftDefer},
	{Family: detectorFamilySleep, Destructive: true, Acts: detectorActSleep},
	{Family: detectorFamilyDrain, Destructive: true, Acts: detectorActDrain},
	{Family: detectorFamilyStall, Destructive: true, Acts: detectorActStall},
	{Family: detectorFamilyDup, Destructive: true, Acts: detectorActDup, StopCapabilityExempt: true},
	{Family: detectorFamilyStranded, Destructive: true, Acts: detectorActStranded},
	{Family: detectorFamilyUnknownState, Acts: false},
	{Family: detectorFamilyWake, Acts: detectorActWakeNamedDependency || detectorActWakePoolFill},
	{Family: detectorFamilyZombie, Acts: detectorActZombie},
	{Family: detectorFamilyExecutionCompletionScan, ObservedOnly: true, Acts: detectorActExecutionCompletionScan},
}

func detectorFamilySpecFor(family detectorFamily) detectorFamilySpec {
	for _, spec := range detectorFamilySpecs {
		if spec.Family == family {
			return spec
		}
	}
	return detectorFamilySpec{Family: family}
}

// detectorFamilyDestructive reports whether the family's keyed effect belongs
// to the close/stop/drain/rollback/retire set suppressed on a partial store view.
func detectorFamilyDestructive(family detectorFamily) bool {
	return detectorFamilySpecFor(family).Destructive
}

// detectorFamilyRequiresStopCapability reports whether the routing seam must
// prove the D2 pair (fresh liveness + unattended stop) before it hands this
// family a key. It is the destructive set minus the families whose handler
// stops a runtime some other proven way (see StopCapabilityExempt).
func detectorFamilyRequiresStopCapability(family detectorFamily) bool {
	spec := detectorFamilySpecFor(family)
	return spec.Destructive && !spec.StopCapabilityExempt
}

// detectorFamilyActs reports whether the family may enqueue exact keys yet.
// Every family answers false for the whole WD wave.
func detectorFamilyActs(family detectorFamily) bool {
	return detectorFamilySpecFor(family).Acts
}

// detectorAnyFamilyActs reports whether any family has been flipped to act
// mode. It is the single predicate the sweep consults before it would enqueue.
func detectorAnyFamilyActs() bool {
	for _, spec := range detectorFamilySpecs {
		if spec.Acts {
			return true
		}
	}
	return false
}

// detectorCondition is one detected, un-acted condition. SessionID is the bare
// durable bead ID — the exact key a handler would be admitted under. Identity is
// the canonical session identity resolved through
// canonicalSessionIdentityWithConfigInfo, so detector keying matches
// build_desired_state (a named bead resolves to its stored
// configured_named_identity, not the template's qualified name).
type detectorCondition struct {
	Family      detectorFamily
	SessionID   string
	SessionName string
	Template    string
	Identity    string
	Site        TraceSiteCode
	Reason      TraceReasonCode
	Outcome     TraceOutcomeCode
	Fields      map[string]any

	// Work carries the routed-work key for the one D-WAKE arm that has no
	// session row of its own: the pool-under-min FILL. Its zero value means the
	// condition is keyed on a session, which every other family's is.
	Work readyRoutedWorkEntry

	// AdmissionSource and AdmissionOutcome are filled by routeDetectorConditions
	// for the arms of an ACTING family. They stay zero for every shadow-only
	// family, which is what keeps a shadow record's effect_owner honest.
	AdmissionSource  sessionStartAdmissionSource
	AdmissionOutcome detectorRouteOutcome
}

// detectorRouteOutcome records what the routing seam did with one condition.
type detectorRouteOutcome string

const (
	// detectorAdmissionRefusedProviderIncapable is the traced refusal for a
	// destructive family under a provider that cannot prove fresh liveness or
	// an unattended stop (the D2 capabilities). It is a refusal, not a retry:
	// the condition is re-detected every sweep and refused every sweep, so no
	// re-enqueue treadmill forms.
	detectorAdmissionRefusedProviderIncapable detectorRouteOutcome = "refused_provider_incapable"
	// detectorAdmissionRefusedError is a rejected Admit call. Detector
	// conditions are level-triggered, so the next sweep re-detects.
	detectorAdmissionRefusedError detectorRouteOutcome = "refused_error"
	// detectorAdmissionRefusedUncertifiable is D-WAKE's traced refusal for a
	// call site that has no certified-lease admission entry. A wake key may not
	// ride the bare Admit, so the row stays legacy's and is re-detected next
	// sweep rather than admitted without its ownership proof.
	detectorAdmissionRefusedUncertifiable detectorRouteOutcome = "refused_uncertifiable"
	// detectorAdmissionQueuedPoolAllocation is D-WAKE's pool-under-min FILL
	// disposition: the exact (workID, poolTarget, sourceStore) key was handed to
	// the pool-allocation admission. That arm has no session-start admission
	// source, so this outcome is what marks it keyed-owned.
	detectorAdmissionQueuedPoolAllocation detectorRouteOutcome = "queued_pool_allocation"
	// detectorAdmissionRefusedOverflow is the traced refusal for a bounded
	// admission channel that dropped the key. Recovery is census-owed: the
	// condition is derived from durable state, so the next sweep re-detects it.
	// The seam must not retry, block, or fall back to acting itself.
	detectorAdmissionRefusedOverflow detectorRouteOutcome = "refused_overflow"
)

// detectorSweepResult is the sweep's whole output. During the WD wave it is
// recorded as shadow trace and otherwise discarded; nothing in it is enqueued.
type detectorSweepResult struct {
	Conditions []detectorCondition
	// SuppressedByPartialStore counts conditions that a destructive family
	// would have raised had the store view been complete. The suppression
	// happens before the condition is constructed, so these never become
	// records.
	SuppressedByPartialStore int
	// UnknownStateSkipped counts rows excluded from every detector because
	// their persisted state is unrecognized.
	UnknownStateSkipped int
	// RowsEvaluated counts rows that reached the family predicates.
	RowsEvaluated int
	// RunningSetKnown is false when the single ListRunning probe failed; every
	// absence-dependent family fails closed for the cycle.
	RunningSetKnown bool
	// CircuitHydrated is true when the sweep restored the respawn-breaker
	// singleton from this cycle's snapshot rows. It is false on a city with the
	// breaker disabled, which is the default.
	CircuitHydrated bool
	// CircuitIdentities counts the named identities the hydration pass walked.
	CircuitIdentities int
	// CircuitResetsOwed counts the identities whose durable cluster still says
	// OPEN while the hydrated model has auto-reset to CLOSED. The sweep is
	// zero-write, so it only reports the debt; the wake handler pays it.
	CircuitResetsOwed int
	// Liveness is the sweep's two-bit provider observation, keyed by bead ID.
	// It is the fleet probe the tick already pays (O(awake)), published so a
	// per-key guard whose whole condition is provider I/O can decide whether to
	// pay a probe of its own instead of probing on every admission.
	Liveness map[string]detectorLivenessBits
	// WakeEvaluations is the sweep's wake verdict per bead ID — the same
	// awakeSetToWakeEvals projection the fleet drain scan reads. It is published
	// for the keyed D-DRAIN advance's third cancel arm, whose condition ("the
	// session reacquired ANY wake reason") is a fleet verdict no per-key
	// predicate can re-derive (ga-f7v2ft.179).
	WakeEvaluations map[string]wakeEvaluation
	// FamilyOverflow counts, per family, the conditions dropped past the
	// per-family record budget. recordDetectorShadow turns each entry into one
	// summary record so a truncated family is visible rather than silent.
	FamilyOverflow map[detectorFamily]int
	Duration       time.Duration
}

// detectorSweepInput is assembled from values the tick has already loaded. The
// sweep performs no store reads of its own: the row feed, desired state,
// demand, ready-wait set, and provider-health snapshot all arrive from the
// caller's existing pipeline (DETECTOR.md §2 inputs, "zero new reads").
type detectorSweepInput struct {
	CityPath    string
	CityName    string
	Cfg         *config.City
	Provider    runtime.Provider
	Rows        []sessionpkg.ReconcileSession
	Snapshot    *sessionBeadSnapshot
	Desired     map[string]TemplateParams
	CfgNames    map[string]bool
	PoolDesired map[string]int

	// RoutedWork is the declared routed-work view's UNALLOCATED entries: one
	// bounded ReadyLive read per store per patrol, promoted from the retired
	// ready-demand fingerprint scan (DETECTOR.md §2, Q2 resolved
	// yes-with-promotion). Only the patrol/boot call site supplies one — it is
	// the site that owns the read and the only site that routes.
	RoutedWork []readyRoutedWorkEntry

	NamedDemand        map[string]bool
	NamedRoutedDemand  map[string]bool
	WorkSet            map[string]bool
	ReadyWaitSet       map[string]bool
	AssignedWorkBeads  []beads.Bead
	ReadyAssignedFlags []bool

	ProviderHealth *providerHealthSnapshot
	Drains         *drainTracker
	Idle           idleTracker
	MaxAge         maxSessionAgeTracker
	Clock          clock.Clock
	StartupTimeout time.Duration

	// SuspendDeferrals carries #3630's named spec-absence confirmation window,
	// moved off the drain tracker into detector state (DETECTOR.md §3,
	// D-ORPHAN). Only the patrol/boot call site supplies one: it is the sole
	// site with a cross-tick identity and the sole site that routes, and a
	// second counting sweep on the same tick would burn the window twice as
	// fast. A nil tracker therefore fails CLOSED — a named live orphan is
	// deferred, never drained on its first spec-absent tick.
	SuspendDeferrals *detectorSuspendDeferralTracker

	// IdleProbes carries D-SLEEP's round-robin probe position across sweeps
	// (DETECTOR.md §2, "probe cursor"). Only the patrol/boot call site supplies
	// one, for the same reason it is the only site that supplies SuspendDeferrals
	// and Admit: it is the sole site with a cross-tick identity, and a second
	// sweep spending the same per-tick probe budget would double the fleet's
	// probe rate. A nil cursor grants no probe slots, which defers rather than
	// drains.
	IdleProbes *detectorIdleProbeCursor

	// StoreQueryPartial marks a degraded view of the work/session stores. Every
	// destructive family fails closed for the cycle.
	StoreQueryPartial bool
	// DeferSessionCloses mirrors the boot tick's withDeferSessionClosesOnBoot:
	// legacy skips per-session orphan/failed-create closes on the boot pass, so
	// the sweep withholds the same candidacy and the cycle stays comparable.
	DeferSessionCloses bool
	// Trigger names the call site ("patrol", "boot", "control-dispatcher",
	// "start") and is carried on every record so the parity join can scope a
	// cycle to its entry point.
	Trigger string
	// Admit hands one exact durable session ID to the existing session-start
	// controller. It is nil wherever no keyed controller owns session start —
	// `gc start`, the control dispatcher, and any legacy-owned city — so those
	// entry points stay read-only no matter which family has flipped to act.
	Admit func(sessionID string, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error)

	// PublishLiveness hands this sweep's two-bit observation back to the
	// runtime so the keyed D-ZOMBIE guard can consult it instead of probing on
	// every admission. Only the patrol/boot call site supplies one (WD.2 delta
	// 6 again): the control dispatcher and `gc start` sweep NARROWED row sets,
	// and publishing one of those would overwrite the fleet view with a partial
	// one and mask a real zombie for a tick. A nil hook publishes nothing,
	// which makes the guard decline — level-triggered, so the next sweep
	// re-detects.
	PublishLiveness func(map[string]detectorLivenessBits)
	// PublishWakeEvaluations hands this sweep's wake verdicts back to the runtime
	// so the keyed D-DRAIN advance's third cancel arm can read the same fleet
	// answer the legacy drain scan reads (ga-f7v2ft.179). It carries
	// PublishLiveness's restriction for PublishLiveness's reason: only the
	// patrol/boot call site supplies one, because a NARROWED sweep would publish
	// wake verdicts for a subset of the fleet and a row absent from that subset
	// reads as "no reason to be awake" — which is precisely the force-stop this
	// arm exists to prevent. A nil hook publishes nothing, which declines the arm
	// and leaves the drain to its other arms; the condition is level-triggered,
	// so the next sweep re-offers it.
	PublishWakeEvaluations func(map[string]wakeEvaluation)
	// AdmitWake is D-WAKE's own admission entry (WD.10a). Every other acting
	// family hands its key to the bare Admit above, because its handler
	// re-derives the condition from the row. A wake cannot: a wake IS a start,
	// and the keyed start path fences a start behind a certified lease, so the
	// key has to arrive already carrying one. This closure certifies the row
	// under the same three wake families the CLI socket certifies and admits the
	// winner. Nil wherever Admit is nil, and additionally nil whenever no keyed
	// controller can mint a certificate, so a sweep that cannot certify simply
	// records and leaves the row to legacy.
	AdmitWake func(sessionID string) (sessionStartAdmissionOutcome, error)

	// EnqueuePoolAllocation is D-WAKE's pool-under-min FILL sink (WD.10b). The
	// arm has no session row to admit — the member does not exist yet — so its
	// exact key is the routed work's (workID, poolTarget, sourceStore) triple
	// and its handler is the existing pool-allocation admission. It reports
	// false when the bounded hint channel drops the key, which the sweep traces
	// and leaves to the next census rather than retrying. Nil wherever no keyed
	// controller owns pool allocation.
	EnqueuePoolAllocation func(readyRoutedWorkEntry) bool
}

// detectSessionConditions classifies every session row in the snapshot into
// condition families. It is READ ONLY: it issues one ListRunning, the existing
// two-bit liveness probe over bead-awake rows, and GetLastActivity only for rows
// whose deadline or stall timer is configured. It performs no store mutation, no
// provider mutation, no domain write, and no enqueue.
func detectSessionConditions(ctx context.Context, in detectorSweepInput) detectorSweepResult {
	started := time.Now()
	result := detectorSweepResult{}
	if in.Cfg == nil {
		result.Duration = time.Since(started)
		return result
	}
	clk := in.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	now := clk.Now()

	rows := detectorOrderedRows(in.Rows)

	// Circuit hydration (DETECTOR.md §3, circuit/health). The sweep is the
	// hydration point for the respawn breaker: it restores the process-wide
	// singleton from the Circuit projection the row feed already carries, so
	// every downstream keyed gate answers from a MODEL that has applied
	// maybeAutoReset rather than from a raw persisted string. Zero store reads,
	// zero store writes — the reset debt it discovers is reported, not paid.
	//
	// It runs over the WHOLE row feed, above the unknown-state guard, because
	// that is where legacy's Phase 0.5 runs: the breaker is keyed on named
	// identity and knows nothing about lifecycle state, so excluding
	// unknown-state rows here would make the two paths derive different progress
	// signatures for the same fleet.
	hydration := detectorHydrateCircuitBreaker(in, rows, now)
	result.CircuitHydrated = hydration.Hydrated
	result.CircuitIdentities = hydration.Identities
	result.CircuitResetsOwed = hydration.ResetsOwed

	// One names-only ListRunning per sweep. It proves ABSENCE but not
	// liveness: a zombie is in this set. When it fails, every family that
	// depends on proven absence fails closed for the cycle.
	runningSet, runningKnown := detectorRunningSet(in.Provider)
	result.RunningSetKnown = runningKnown

	emit := newDetectorConditionSink(in.StoreQueryPartial)

	// Global guard 1: unknown-state rows are excluded from every detector.
	// The throttled diagnostic itself stays legacy-owned this wave — it stamps
	// markers and emits events, and the sweep is zero-write.
	known := make([]sessionpkg.ReconcileSession, 0, len(rows))
	for _, row := range rows {
		if ctx != nil && ctx.Err() != nil {
			result.Duration = time.Since(started)
			return result
		}
		if row.Info.Closed {
			continue
		}
		if !isKnownStateInfo(row.Info) {
			result.UnknownStateSkipped++
			emit.add(detectorCondition{
				Family:      detectorFamilyUnknownState,
				SessionID:   row.Info.ID,
				SessionName: strings.TrimSpace(row.Info.SessionNameMetadata),
				Template:    row.Info.Template,
				Site:        TraceSiteReconcilerUnknownState,
				Reason:      detectorReasonUnknownStateSkipped,
				Outcome:     TraceOutcomeSkipped,
				Fields:      map[string]any{"state": row.Info.MetadataState},
			}, false)
			continue
		}
		known = append(known, row)
	}
	result.RowsEvaluated = len(known)

	// Two-bit liveness over bead-awake rows only (O(awake)). `alive` is what
	// D-ZOMBIE, D-WAKE, and D-SLEEP key on; names-only membership alone would
	// leave zombies permanently stuck.
	liveness := detectorLiveness(in.Provider, in.Cfg, known)
	result.Liveness = liveness

	infoByID := make(map[string]sessionpkg.Info, len(known))
	for _, row := range known {
		infoByID[row.Info.ID] = row.Info
	}

	awake, wakeEvals := detectorAwakeSet(in, known, liveness, now)
	result.WakeEvaluations = wakeEvals

	namedIdentityRows := make(map[string][]sessionpkg.Info)
	var idleProbeCandidates []detectorCondition

	for _, row := range known {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		info := row.Info
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			continue
		}
		template := normalizedSessionTemplateInfo(info, in.Cfg)
		if template == "" {
			template = info.Template
		}
		cfgAgent := findAgentByTemplate(in.Cfg, template)
		_, identity := canonicalSessionIdentityWithConfigInfo(in.Cfg, cfgAgent, info)
		if dupIdentity := detectorDuplicateNamedIdentity(in, info); dupIdentity != "" {
			namedIdentityRows[dupIdentity] = append(namedIdentityRows[dupIdentity], info)
		}
		base := detectorCondition{
			SessionID:   info.ID,
			SessionName: name,
			Template:    template,
			Identity:    identity,
		}
		tp, desired := in.Desired[name]
		live := liveness[info.ID]

		// Family precedence mirrors legacy's forward-pass early-continues: a row
		// that raises drain, orphan, or stale-create candidacy is claimed by that
		// family and evaluated no further this cycle. Without it the sweep would
		// raise a sleep or wake condition on a row legacy had already routed to a
		// close, producing detector-present/legacy-absent mismatches that are an
		// artifact of shape rather than a real divergence.
		if detectDrain(in, emit, base, info) {
			continue
		}
		if detectOrphan(in, emit, base, info, desired, runningSet, runningKnown, now) {
			continue
		}
		if detectStaleCreate(in, emit, base, info, runningSet, runningKnown, clk) {
			continue
		}
		if desired {
			detectDrift(in, emit, base, info, tp)
		}
		detectZombie(emit, base, live)
		detectDeadline(in, emit, base, info, template, live, now)
		detectStall(in, emit, base, infoByID, tp, live, now)
		detectWakeOrSleep(in, emit, base, info, awake, wakeEvals, live, &idleProbeCandidates, clk)
		detectStranded(emit, base, info, desired, live, now)
	}

	detectorGrantIdleProbeSlots(in, emit, idleProbeCandidates)
	detectDuplicateNamed(in, emit, namedIdentityRows)
	detectPoolFill(in, emit, known)
	// Retire every confirmation window this sweep did not count. A row that
	// stopped raising named live-orphan candidacy — its spec came back, it went
	// dead, it picked up work — gets a fresh window next time, which is what
	// makes the counter consecutive rather than cumulative.
	in.SuspendDeferrals.prune()

	result.Conditions = emit.conditions
	result.SuppressedByPartialStore = emit.suppressed
	result.FamilyOverflow = emit.overflow
	result.Duration = time.Since(started)
	return result
}

// detectorOrderedRows pins the sweep's iteration order — stable sort by session
// name, then bead ID — so ComputeAwakeSet's last-write-wins name resolution is
// deterministic without TopoOrder or BuildDeps (DETECTOR.md R3).
func detectorOrderedRows(rows []sessionpkg.ReconcileSession) []sessionpkg.ReconcileSession {
	ordered := make([]sessionpkg.ReconcileSession, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool {
		a := strings.TrimSpace(ordered[i].Info.SessionNameMetadata)
		b := strings.TrimSpace(ordered[j].Info.SessionNameMetadata)
		if a != b {
			return a < b
		}
		return ordered[i].Info.ID < ordered[j].Info.ID
	})
	return ordered
}

func detectorRunningSet(sp runtime.Provider) (map[string]bool, bool) {
	if sp == nil {
		return nil, false
	}
	names, err := sp.ListRunning("")
	if err != nil {
		return nil, false
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[strings.TrimSpace(n)] = true
	}
	return set, true
}

// detectorLivenessBits is the two-bit provider observation for one row.
type detectorLivenessBits struct {
	Probed  bool
	Running bool
	Alive   bool
}

// detectorLiveness runs the existing two-bit observeRuntimeProviderLiveness
// probe over bead-awake rows only. Asleep rows are never probed: their absence
// is already the durable answer, and probing them would make the sweep
// O(fleet) in provider calls.
func detectorLiveness(sp runtime.Provider, cfg *config.City, rows []sessionpkg.ReconcileSession) map[string]detectorLivenessBits {
	out := make(map[string]detectorLivenessBits, len(rows))
	if sp == nil {
		return out
	}
	for _, row := range rows {
		info := row.Info
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" || !detectorBeadAwake(info) {
			continue
		}
		running, alive := observeRuntimeProviderLiveness(sp, name, detectorProcessNames(cfg, info))
		out[info.ID] = detectorLivenessBits{Probed: true, Running: running, Alive: alive}
	}
	return out
}

// detectorCircuitHydration is what one sweep's breaker pass observed. It is
// reported, never acted on: the sweep is zero-write, so a reset the cooldown
// produced is a DEBT the wake handler settles on its next admission for that
// key (exactSessionCircuitOpen).
type detectorCircuitHydration struct {
	Hydrated   bool
	Identities int
	ResetsOwed int
}

// detectorHydrateCircuitBreaker restores the respawn-breaker singleton from the
// snapshot rows this tick already loaded, observes each named identity's
// progress signature, and prunes idle entries — the whole of legacy's Phase 0.5
// minus its two persistence call sites (DETECTOR.md §3, circuit/health).
//
// Three properties are load-bearing.
//
// It costs NO store read. The persisted cluster rides the row feed as
// ReconcileSession.Circuit, the same projection legacy's restore phase reads at
// session_reconciler.go's Phase 0.5, so hydrating here is byte-identical to
// hydrating there.
//
// It costs no store WRITE either, which is what makes the sweep safe to run
// beside legacy on the same tick. Legacy's Phase 0.5 keeps both of its persists;
// this pass only reports the debt (ResetsOwed) so the cycle rollup shows it.
//
// And it must not SWALLOW anything legacy still owns. restoreFromMetadata and
// ObserveProgressSignature are both consume-once edges on a shared singleton,
// and the sweep runs FIRST on every tick — so before this pass could exist,
// legacy's two `if edge { persist }` gates had to become level-triggered
// (sessionCircuitBreakerResetOwed / sessionCircuitBreakerProgressPersistOwed).
// Those two arms are READ-SHARED with this pass, not effect-competing: they
// converge the durable row onto the model with an idempotent, provider-free
// write, so no yield is required and the order of hydration does not matter.
func detectorHydrateCircuitBreaker(in detectorSweepInput, rows []sessionpkg.ReconcileSession, now time.Time) detectorCircuitHydration {
	cbCfg, enabled := sessionCircuitBreakerConfigFromCity(in.Cfg)
	if !enabled {
		return detectorCircuitHydration{}
	}
	cb := defaultSessionCircuitBreaker()
	cb.configure(cbCfg)

	out := detectorCircuitHydration{Hydrated: true}
	infos := make([]sessionpkg.Info, 0, len(rows))
	// The stale-snapshot floor is observed for every identity BEFORE any
	// restore, exactly as legacy orders its two loops: a reset generation
	// observed after a restore could not reject the snapshot it just installed.
	for i := range rows {
		identity := namedSessionIdentityInfo(rows[i].Info)
		if identity == "" {
			continue
		}
		infos = append(infos, rows[i].Info)
		if err := cb.observeResetGenerationFromMetadata(identity, rows[i].Circuit); err != nil {
			continue
		}
	}
	for i := range rows {
		identity := namedSessionIdentityInfo(rows[i].Info)
		if identity == "" {
			continue
		}
		out.Identities++
		if _, err := cb.restoreFromMetadata(identity, rows[i].Circuit, now); err != nil {
			continue
		}
		if sessionCircuitBreakerResetOwed(cb, identity, rows[i].Circuit, now) {
			out.ResetsOwed++
		}
	}
	for identity, sig := range computeNamedSessionProgressSignatures(infos, in.AssignedWorkBeads) {
		cb.ObserveProgressSignature(identity, sig, now)
	}
	cb.pruneIdle(now)
	return out
}

// detectorBeadAwake reports whether the durable row claims to be awake. It is
// the probe-selection predicate, not a liveness verdict.
func detectorBeadAwake(info sessionpkg.Info) bool {
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateCreating, sessionpkg.StateStartPending:
		return true
	default:
		return false
	}
}

func detectorProcessNames(cfg *config.City, info sessionpkg.Info) []string {
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	if agent := findAgentByTemplate(cfg, template); agent != nil {
		return agent.ProcessNames
	}
	return nil
}

// detectorAwakeSet reuses ComputeAwakeSet as a pure library over the pinned
// order. Attachment and pending-interaction probes are deliberately absent:
// they are provider I/O and move handler-side, so their arms are unpredicted
// (DETECTOR.md §3b, D-SLEEP "probe/pending arms unpredicted").
//
// AttachedSessions and PendingSessions therefore stay EMPTY here, and every
// consumer of this projection owes the matching handler-side re-pay for its own
// key: D-SLEEP's and D-ORPHAN's drain arms through
// exactSessionActiveUseDeferralReason, D-DRAIN's cancel arms through
// pendingInteractionKeepsAwakeInfo (arm 1) and exactSessionUserAttached (arm 3).
// A consumer that reads "attached" or "pending" off this view alone reads a
// verdict it can never see. The pairing is guarded by named exemptions in
// TestAwakeInputBuilderTwinsPopulateTheSameFields.
//
// The wakeEvaluation map it returns beside the decisions is the same bridge
// projection legacy feeds its sleep-policy pass (awakeSetToWakeEvals), so the
// detector's ConfigSuppressed pass and legacy's answer the demand-override rung
// from one derivation rather than two.
func detectorAwakeSet(in detectorSweepInput, rows []sessionpkg.ReconcileSession, liveness map[string]detectorLivenessBits, now time.Time) (map[string]AwakeDecision, map[string]wakeEvaluation) {
	input := AwakeInput{
		ScaleCheckCounts:         in.PoolDesired,
		NamedSessionDemand:       cloneBoolMap(in.NamedDemand),
		NamedSessionRoutedDemand: cloneBoolMap(in.NamedRoutedDemand),
		WorkSet:                  in.WorkSet,
		ReadyWaitSet:             in.ReadyWaitSet,
		RunningSessions:          make(map[string]bool),
		AttachedSessions:         map[string]bool{},
		PendingSessions:          map[string]bool{},
		ChatIdleTimeout:          in.Cfg.ChatSessions.IdleTimeoutDuration(),
		ManualGracePeriod:        in.Cfg.ChatSessions.GracePeriodDuration(),
		Now:                      now,
	}
	infos := make([]sessionpkg.Info, 0, len(rows))
	for _, row := range rows {
		infos = append(infos, row.Info)
		if liveness[row.Info.ID].Alive {
			if name := strings.TrimSpace(row.Info.SessionNameMetadata); name != "" {
				input.RunningSessions[name] = true
			}
		}
	}
	detectorFillAwakeConfig(&input, in.Cfg, in.CityPath)
	detectorFillAwakeWork(&input, in.AssignedWorkBeads, in.ReadyAssignedFlags)
	detectorFillAwakeSessions(&input, in.Cfg, infos, now)
	// The one derivation the legacy bridge also calls, once its three inputs —
	// NamedSessions, WorkSet, SessionBeads — are all populated (ga-f7v2ft.180).
	fillAwakeNamedSessionWorkQueue(&input)
	decisions := ComputeAwakeSet(input)
	return decisions, awakeSetToWakeEvals(decisions, input.SessionBeads)
}

func detectorFillAwakeConfig(input *AwakeInput, cfg *config.City, cityPath string) {
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		agent := AwakeAgent{
			QualifiedName:     a.QualifiedName(),
			Suspended:         isAgentEffectivelySuspendedWith(cfg, cityPath, a, suspState),
			SleepAfterIdle:    parseSleepDuration(a.SleepAfterIdle),
			MinActiveSessions: a.EffectiveMinActiveSessions(),
		}
		if len(a.DependsOn) > 0 {
			agent.DependsOn = a.DependsOn
		}
		input.Agents = append(input.Agents, agent)
	}
	cityName := config.EffectiveCityName(cfg, "")
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		identity := ns.QualifiedName()
		input.NamedSessions = append(input.NamedSessions, AwakeNamedSession{
			Identity:    identity,
			Template:    ns.TemplateQualifiedName(),
			Mode:        ns.Mode,
			RuntimeName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		})
	}
}

func detectorFillAwakeWork(input *AwakeInput, work []beads.Bead, readyFlags []bool) {
	for i := range work {
		wb := work[i]
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || (wb.Status != "open" && wb.Status != "in_progress") {
			continue
		}
		ready := i < len(readyFlags) && readyFlags[i]
		blocked := wb.Status == "in_progress" && wb.IsBlocked != nil && *wb.IsBlocked
		input.WorkBeads = append(input.WorkBeads, AwakeWorkBead{
			ID: wb.ID, Assignee: assignee, Status: wb.Status, Ready: ready, Blocked: blocked,
		})
	}
}

func detectorFillAwakeSessions(input *AwakeInput, cfg *config.City, infos []sessionpkg.Info, now time.Time) {
	for i := range infos {
		info := infos[i]
		if info.Closed {
			continue
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			continue
		}
		lcInput := sessionpkg.LifecycleInputFromInfo(info)
		lcInput.Now = now
		lifecycle := sessionpkg.ProjectLifecycle(lcInput)
		bead := AwakeSessionBead{
			ID:                     info.ID,
			SessionName:            name,
			Template:               normalizeAgentTemplateIdentity(cfg, info.Template),
			State:                  string(lifecycle.CompatState),
			SleepReason:            info.SleepReason,
			ManualSession:          isManualSessionInfo(info),
			PendingCreate:          lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate),
			ExplicitWake:           lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit),
			DependencyOnly:         info.DependencyOnly,
			NamedIdentity:          lifecycle.NamedIdentity,
			ConfiguredNamedSession: isNamedSessionInfo(info),
			Pinned:                 lifecycle.HasWakeCause(sessionpkg.WakeCausePinned),
			Drained:                lifecycle.BaseState == sessionpkg.BaseStateDrained,
			WaitHold:               info.WaitHold == "true",
			RestartRequested:       strings.TrimSpace(info.RestartRequested) == "true",
			ContinuationResetPending: strings.TrimSpace(info.ContinuationResetPending) == "true" &&
				strings.TrimSpace(info.ResetCommittedAt) != "",
			CurrentlyProcessingBeadID: strings.TrimSpace(info.CurrentlyProcessingBeadID),
			HeldUntil:                 lifecycle.HeldUntil,
			QuarantinedUntil:          lifecycle.QuarantinedUntil,
			CreatedAt:                 info.CreatedAt,
		}
		if t, err := time.Parse(time.RFC3339, info.DetachedAt); err == nil && !t.IsZero() {
			bead.IdleSince = t
		}
		input.SessionBeads = append(input.SessionBeads, bead)
	}
}

// detectorConditionSink collects conditions and applies the storeQueryPartial
// guard BEFORE a condition exists. That ordering is the point: legacy records
// Outcome=Closed and then declines to close on a partial view; suppressing
// ahead of the record makes that trace lie impossible by construction.
type detectorConditionSink struct {
	partial    bool
	conditions []detectorCondition
	suppressed int
	perFamily  map[detectorFamily]int
	overflow   map[detectorFamily]int
}

func newDetectorConditionSink(partial bool) *detectorConditionSink {
	return &detectorConditionSink{
		partial:   partial,
		perFamily: make(map[detectorFamily]int),
		overflow:  make(map[detectorFamily]int),
	}
}

// add records one condition. destructive names the family's effect class for
// the partial-store guard; the caller passes it explicitly so a family whose
// arms differ (D-ORPHAN's kept-open arm vs its close arm) can classify per arm.
func (s *detectorConditionSink) add(cond detectorCondition, destructive bool) {
	if destructive && s.partial {
		s.suppressed++
		return
	}
	if s.perFamily[cond.Family] >= detectorFamilyRecordBudget {
		s.overflow[cond.Family]++
		return
	}
	s.perFamily[cond.Family]++
	s.conditions = append(s.conditions, cond)
}

func detectorOverflowSummaries(overflow map[detectorFamily]int) []detectorCondition {
	families := make([]detectorFamily, 0, len(overflow))
	for family := range overflow {
		families = append(families, family)
	}
	sort.Slice(families, func(i, j int) bool { return families[i] < families[j] })
	out := make([]detectorCondition, 0, len(families))
	for _, family := range families {
		out = append(out, detectorCondition{
			Family:  family,
			Site:    TraceSiteControllerTickPhase,
			Reason:  detectorReasonFamilyBudgetExceeded,
			Outcome: TraceOutcomeSkipped,
			Fields: map[string]any{
				"detector_family":  string(family),
				"budget":           detectorFamilyRecordBudget,
				"suppressed_count": overflow[family],
			},
		})
	}
	return out
}

func detectDeadline(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info, template string, live detectorLivenessBits, now time.Time) {
	if !live.Alive {
		return
	}
	// The durable blocker rung of DecideIdleTimeout / DecideMaxSessionAge is the
	// only rung the sweep can evaluate: it reads held_until, quarantined_until
	// and pin_awake off the row it already has. The pending-interaction and
	// assigned-work rungs
	// are a provider probe and a reachable-store scan, so they stay handler-side
	// with the rest of the ladder (§3b: "legacy pending-interaction deferral —
	// probe-only signal, unpredicted"). A blocked row records its deferral for
	// the parity join and is never enqueued, so the handler is never entered.
	deadline := func(site TraceSiteCode, reason TraceReasonCode, maxAge bool, fields map[string]any) {
		cond := base
		cond.Family = detectorFamilyDeadline
		cond.Site = site
		// Per ARM, not per row. The two timers take different blocker sets
		// (deadlineTimerBlockerInfo), and capturing one verdict for the closure
		// gives the age arm the idle ladder's pin exemption — which records
		// deadline_deferred where a stop belongs, and detectorAdmissionSourceFor
		// enqueues only on TraceOutcomeStop, so the age restart is never routed.
		if blocker := deadlineTimerBlockerInfo(info, now, maxAge); blocker != "" {
			cond.Reason = detectorReasonDeadlineDeferred
			cond.Outcome = TraceOutcomeNoChange
			cond.Fields = map[string]any{"predicted_effect": "none", "blocker": blocker, "deadline": string(reason)}
			emit.add(cond, false)
			return
		}
		cond.Reason = reason
		cond.Outcome = TraceOutcomeStop
		cond.Fields = fields
		emit.add(cond, true)
	}
	if in.Idle != nil && in.Idle.checkIdle(base.SessionName, template, in.Provider, now) {
		deadline(TraceSiteReconcilerIdleTimeout, detectorReasonIdleTimeout, false, map[string]any{
			"predicted_effect": "stop",
			"sleep_reason":     string(sessionpkg.SleepReasonIdleTimeout),
		})
	}
	if in.MaxAge == nil {
		return
	}
	completeAt, err := time.Parse(time.RFC3339, strings.TrimSpace(info.CreationCompleteAt))
	if err != nil || completeAt.IsZero() {
		return
	}
	if in.MaxAge.shouldRestart(base.SessionName, template, completeAt, now) {
		deadline(TraceSiteReconcilerMaxSessionAge, detectorReasonMaxSessionAge, true, map[string]any{
			"predicted_effect": "stop",
			"sleep_reason":     string(sessionpkg.SleepReasonMaxSessionAge),
			"age_seconds":      int64(now.Sub(completeAt).Seconds()),
		})
	}
}

// detectorSuspendDeferralTracker is the detector's own copy of #3630's named
// spec-absence confirmation window (legacy's drainTracker.suspendDeferrals,
// session_reconciler.go:2264). It counts CONSECUTIVE sweeps in which a named
// row raised live-orphan candidacy: a namedSessionSpecs enumeration collapse
// that drops a spec for a single tick must not drain a named session and lose
// its in-session context.
//
// It is deliberately a SECOND counter rather than a shared one. Legacy keeps
// counting on its own tracker for the WD.15 parity join, and §3b already
// classifies the resulting off-by-one as expected divergence; sharing would
// have coupled the two paths' windows and made the join unable to tell them
// apart.
//
// Bounded by construction: prune() at the end of every sweep retains only the
// rows that sweep counted, so the map never outgrows the live fleet and a row
// that stops being a candidate restarts its window.
type detectorSuspendDeferralTracker struct {
	mu    sync.Mutex
	ticks map[string]int
	seen  map[string]bool
}

func newDetectorSuspendDeferralTracker() *detectorSuspendDeferralTracker {
	return &detectorSuspendDeferralTracker{
		ticks: make(map[string]int),
		seen:  make(map[string]bool),
	}
}

// bump advances one row's confirmation window and returns the new consecutive
// count.
func (t *detectorSuspendDeferralTracker) bump(sessionID string) int {
	if t == nil || sessionID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ticks == nil {
		t.ticks = make(map[string]int)
	}
	if t.seen == nil {
		t.seen = make(map[string]bool)
	}
	t.ticks[sessionID]++
	t.seen[sessionID] = true
	return t.ticks[sessionID]
}

// prune drops every window the sweep just ending did not count, which is what
// makes the counter consecutive and bounds it to the live fleet. A sweep cut
// short by context cancellation prunes the rows it never reached, restarting
// their windows — fail-closed, in the direction of not draining.
func (t *detectorSuspendDeferralTracker) prune() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.ticks {
		if !t.seen[id] {
			delete(t.ticks, id)
		}
	}
	t.seen = make(map[string]bool)
}

// detectorNamedSuspendConfirmed advances the confirmation window for one named
// live-orphan row and reports whether it has elapsed. A nil tracker fails
// closed: a sweep with no cross-tick state cannot prove a spec has been absent
// for namedSuspendConfirmTicks consecutive ticks, and an unproven window must
// not drain a named session.
func detectorNamedSuspendConfirmed(tr *detectorSuspendDeferralTracker, sessionID string) (int, bool) {
	if tr == nil {
		return 0, false
	}
	ticks := tr.bump(sessionID)
	return ticks, ticks >= namedSuspendConfirmTicks
}

// detectOrphan reports whether the row was claimed by the orphan family.
func detectOrphan(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info, desired bool, runningSet map[string]bool, runningKnown bool, now time.Time) bool {
	if desired || in.DeferSessionCloses {
		return false
	}
	// WD.10a sweep rule, detection side: a wake-current canonical singleton is
	// D-WAKE's target, so this family neither claims nor enqueues it. The
	// handler re-derives the same rule from the same predicate, which is what
	// keeps the two sides from disagreeing about the row.
	if wakeCurrentSingletonPreservesUndesiredRow(info, in.Cfg, now) {
		return false
	}
	// Legacy's kept-open suppressor (session_reconciler.go:2193-2204), evaluated
	// against the assigned-work snapshot the tick already loaded — no new read.
	// A row still holding work is claimed by the family and recorded, but never
	// enqueued: the handler's own live re-query inside
	// closeSessionBeadIfReachableStoreUnassigned stays the authority.
	if sessionBeadHasAssignedWorkInfo(in.AssignedWorkBeads, info) {
		cond := base
		cond.Family = detectorFamilyOrphan
		cond.Site = TraceSiteReconcilerOrphaned
		cond.Reason = detectorReasonOrphanAssignedWork
		cond.Outcome = TraceOutcomeNoChange
		cond.Fields = map[string]any{"predicted_effect": "none", "live_assigned_work": true}
		emit.add(cond, false)
		return true
	}
	if strings.TrimSpace(info.MetadataState) == string(sessionpkg.StateFailedCreate) {
		cond := base
		cond.Family = detectorFamilyOrphan
		cond.Site = TraceSiteReconcilerCloseFailedCreate
		cond.Reason = detectorReasonFailedCreate
		cond.Outcome = TraceOutcomeClosed
		cond.Fields = map[string]any{"predicted_effect": "close"}
		emit.add(cond, true)
		return true
	}
	// Proven absence, not assumed absence: without a running set the close arm
	// fails closed for the cycle, matching legacy's liveness-error refusal.
	if !runningKnown {
		cond := base
		cond.Family = detectorFamilyOrphan
		cond.Site = TraceSiteReconcilerOrphaned
		cond.Reason = detectorReasonRunningSetUnavailable
		cond.Outcome = TraceOutcomeSkipped
		cond.Fields = map[string]any{"predicted_effect": "none"}
		emit.add(cond, false)
		return true
	}
	cond := base
	cond.Family = detectorFamilyOrphan
	if runningSet[base.SessionName] {
		cond.Site = TraceSiteReconcilerOrphaned
		// #3630, moved off the drain tracker into detector state: a LIVE named
		// row reaches the drain arm only because its configured spec is absent
		// this sweep, and a boot-time namedSessionSpecs enumeration collapse can
		// drop a spec for one tick and restore it on the next. Suspend-class
		// drains are revertible, so the window must confirm before the key is
		// ever enqueued. Scoped to live rows exactly as legacy scopes it: a dead
		// named row still releases its alias immediately through the close arm
		// (ga-ue1r).
		if isNamedSessionInfo(info) {
			if ticks, confirmed := detectorNamedSuspendConfirmed(in.SuspendDeferrals, info.ID); !confirmed {
				cond.Reason = detectorReasonOrphanSuspendDeferred
				cond.Outcome = TraceOutcomeDeferredConfirm
				cond.Fields = map[string]any{
					"predicted_effect": "none",
					"confirm_ticks":    ticks,
					"confirm_required": namedSuspendConfirmTicks,
				}
				emit.add(cond, false)
				return true
			}
		}
		cond.Reason = detectorReasonOrphanLive
		cond.Outcome = TraceOutcomeDrain
		cond.Fields = map[string]any{"predicted_effect": "drain"}
	} else {
		cond.Site = TraceSiteReconcilerCloseOrphan
		cond.Reason = detectorReasonOrphanDead
		cond.Outcome = TraceOutcomeClosed
		cond.Fields = map[string]any{"predicted_effect": "close"}
	}
	emit.add(cond, true)
	return true
}

// detectStaleCreate reports whether the row was claimed by the stale-create
// family.
func detectStaleCreate(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info, runningSet map[string]bool, runningKnown bool, clk clock.Clock) bool {
	if !info.PendingCreateClaim {
		return false
	}
	if runningKnown && runningSet[base.SessionName] {
		return false
	}
	cond := base
	cond.Family = detectorFamilyStaleCreate
	if pendingCreateLeaseExpiredForRollbackInfo(info, clk, in.StartupTimeout) {
		cond.Site = TraceSiteReconcilerPendingCreate
		cond.Reason = detectorReasonStalePendingCreate
		cond.Outcome = TraceOutcomeRollback
		cond.Fields = map[string]any{"predicted_effect": "rollback"}
		emit.add(cond, true)
		return true
	}
	cond.Site = TraceSiteReconcilerPendingCreatePreserved
	cond.Reason = detectorReasonPendingCreatePreserved
	cond.Outcome = TraceOutcomeNoChange
	cond.Fields = map[string]any{"predicted_effect": "none"}
	emit.add(cond, false)
	return true
}

// detectDrift raises the family's ONE effect arm for one row: the CORE
// fingerprint moved, or — when it did not — the LIVE fingerprint did. Both are
// pure hash compares over the snapshot plus config, with zero provider I/O, and
// both predict the same thing at detection level: this session's running config
// no longer matches its declared config, so the handler will converge it
// (DETECTOR.md §3b, D-DRIFT parity level = detection).
//
// The two arms differ in SITE and REASON, never in outcome, so the family keeps
// a single routing outcome and therefore a single admission source. Legacy's
// ConfigDrift and LiveDrift sites are two spellings of one convergence ladder
// behind one yield ("site 9 merges in"), and two outcomes would have implied two
// legacy yields that do not exist.
//
// Which RUNG of the ladder runs — silent rebaseline, launch-only relaunch,
// restart-in-place, drift drain, live re-apply, or a WD.9 deferral — is decided
// handler-side: the rungs below the hash compare read attachment and pending
// interaction, which are provider probes the sweep may not pay fleet-wide.
func detectDrift(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info, tp TemplateParams) {
	// Legacy's restart-requested block (session_reconciler.go:2806) runs above
	// the drift block and `continue`s the row past it, and on the one path that
	// falls through it has already cleared started_config_hash — so a drifted row
	// carrying the durable marker is legacy-ABSENT at this site. Raising it is
	// the detector-present/legacy-absent mismatch the family precedence above
	// exists to prevent, and it is not free: the enqueued config_drift admission
	// claims the key for a family that will decline it (ga-f7v2ft.138), spending
	// a sweep slot on a row whose reset the arm below the seam owns. The seam
	// guard in resolveExactSessionConfigDrift makes the same call from the row;
	// this keeps the sweep from spending the key at all.
	if strings.TrimSpace(info.RestartRequested) == "true" {
		return
	}
	cond := base
	cond.Family = detectorFamilyDrift
	cond.Outcome = TraceOutcomeDrain
	cond.Site = TraceSiteReconcilerConfigDrift
	cond.Reason = detectorReasonConfigDrift
	stored, current, drifted := sessionConfigDriftHashes(info, in.Cfg, tp)
	if !drifted {
		cond.Site = TraceSiteReconcilerLiveDrift
		cond.Reason = detectorReasonLiveDrift
		stored, current, drifted = sessionLiveDriftHashes(info, in.Cfg, tp)
	}
	if !drifted {
		return
	}
	cond.Fields = map[string]any{
		"predicted_effect": "converge",
		"stored_hash":      stored,
		"current_hash":     current,
	}
	emit.add(cond, true)
}

// detectDrain reports whether the row was claimed by the drain family.
func detectDrain(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info) bool {
	if in.Drains == nil {
		return false
	}
	state := in.Drains.get(info.ID)
	if state == nil {
		return false
	}
	// Tracker state only. The sweep performs NO GetMeta(GC_DRAIN_ACK) read: ack
	// discovery is handler-side (DETECTOR.md §3, D-DRAIN architect decision).
	cond := base
	cond.Family = detectorFamilyDrain
	cond.Site = TraceSiteReconcilerDrainAck
	cond.Reason = detectorReasonDrainInFlight
	cond.Outcome = TraceOutcomeDrain
	cond.Fields = map[string]any{
		"predicted_effect": "advance",
		"drain_reason":     state.reason,
	}
	emit.add(cond, true)
	return true
}

func detectWakeOrSleep(
	in detectorSweepInput,
	emit *detectorConditionSink,
	base detectorCondition,
	info sessionpkg.Info,
	awake map[string]AwakeDecision,
	evals map[string]wakeEvaluation,
	live detectorLivenessBits,
	probes *[]detectorCondition,
	clk clock.Clock,
) {
	decision, ok := awake[base.SessionName]
	if !ok {
		return
	}
	if decision.ShouldWake && !live.Alive {
		cond := base
		cond.Family = detectorFamilyWake
		cond.Site = TraceSiteReconcilerWakeDecision
		// The traced refusal that replaces legacy's UNTRACED quarantine skip
		// (§3 D-WAKE, delta-8 arm 3). A row inside a live quarantine or hold
		// window is a wake target legacy silently drops, which is a trace lie:
		// the campaign join sees a wake that never happened and no record
		// saying why. Refusing here records the blocker and enqueues nothing.
		// It cannot double-act: legacy already skips these rows, and the keyed
		// admission chain blocks them again at the handler, so the refusal is a
		// non-action on both sides.
		//
		// The TIMED blockers only (wakeBlockerInfo). Those are the two
		// ComputeAwakeSet itself suppresses, which is what made this refusal a
		// pure trace repair. A pin is the opposite — it is why ShouldWake is
		// true on an asleep row — so taking it here would refuse the pin-revive
		// wake instead of recording a skip legacy also performs.
		if blocker := wakeBlockerInfo(info, clk.Now()); blocker != "" {
			cond.Reason = detectorReasonWakeBlocked
			cond.Outcome = TraceOutcomeSkipped
			cond.Fields = map[string]any{"predicted_effect": "none", "wake_reason": decision.Reason, "blocker": blocker}
			emit.add(cond, false)
			return
		}
		cond.Reason = detectorWakeTargetReason(info, in.Cfg)
		cond.Outcome = TraceOutcomeStartCandidate
		cond.Fields = map[string]any{"predicted_effect": "start", "wake_reason": decision.Reason}
		emit.add(cond, false)
		return
	}
	if !live.Alive {
		// Asleep and not wanted. Nothing to drain, nothing to start.
		return
	}
	detectSleep(in, emit, base, info, decision, evals[info.ID], probes, clk)
}

// detectorWakeTargetReason splits D-WAKE's target arm by ROW SHAPE, which is the
// same seam WD.10a and WD.10b divide on and the only one detection can see.
//
// A named row and a canonical singleton row (pool-managed, NO slot) are the two
// shapes the certified named-wake and configured-dependency leases own, so they
// carry the routing reasons. Everything else — slotized pool members, and the
// pool-under-min fill that has no row of its own yet — keeps the original
// shadow-only reason and waits for WD.10b. The partition is on SLOT markers, not
// on pool_managed, for the reason ga-ij8mh recorded: sync stamps pool_managed on
// every configured single-session agent's row, so pool_managed alone would put
// the singleton shape back on the family that cannot own it.
func detectorWakeTargetReason(info sessionpkg.Info, cfg *config.City) TraceReasonCode {
	if isNamedSessionInfo(info) {
		return detectorReasonWakeTargetNamed
	}
	if cfg == nil {
		return detectorReasonWakeTarget
	}
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if agent == nil || len(agent.DependsOn) == 0 {
		return detectorReasonWakeTarget
	}
	if isPoolManagedSessionInfo(info) && !configuredDependencyWakeShapeMatches(info, agent) {
		return detectorReasonWakeTarget
	}
	return detectorReasonWakeTargetDependency
}

// detectSleep raises D-SLEEP for one ALIVE row. It runs the sleep-policy pass
// legacy runs between the awake scan and the drain arm
// (session_reconciler.go:3666-3704), because a row ComputeAwakeSet still wants
// awake is exactly the row a workspace `session_sleep` window puts to sleep —
// that pass IS the idle-sleep production path, and without it the family would
// key only the arms nobody runs.
//
// Two of legacy's rungs are deliberately absent, and both are provider I/O the
// sweep may not pay fleet-wide (DETECTOR.md §2): the pending-interaction probe
// and the attachment probe. Their arms are unpredicted by §3b, and the handler
// pays both per key before it drains anything, so the detector's suppression is
// the wider of the two views and the handler is the authority.
func detectSleep(
	in detectorSweepInput,
	emit *detectorConditionSink,
	base detectorCondition,
	info sessionpkg.Info,
	decision AwakeDecision,
	eval wakeEvaluation,
	probes *[]detectorCondition,
	clk clock.Clock,
) {
	policy := resolveSessionSleepPolicyInfo(info, in.Cfg, in.Provider)
	intent := strings.TrimSpace(info.SleepIntent)
	// The suppression verdict is computed for EVERY live row, not just the ones
	// the awake set still wants, because it is also what makes the no-wake
	// verdict re-derivable per key. A row ComputeAwakeSet already put down for
	// its own idle-sleep reason is the family's only if this per-key pass agrees;
	// otherwise the reason is fleet-only and the handler could not check it.
	suppressed := strings.TrimSpace(info.PinAwake) != "true" &&
		configWakeSuppressedInfo(info, policy, in.Provider, clk)
	if decision.ShouldWake {
		if !suppressed || wakeDemandOverridesSleepSuppression(decision, eval, policy, in.PoolDesired, base.Template, intent != "") {
			// Still wanted awake: no sleep condition at all.
			return
		}
	}

	cond := base
	cond.Family = detectorFamilySleep
	cond.Site = TraceSiteReconcilerDrainDecision
	cond.Reason = detectorReasonNoWake
	cond.Outcome = TraceOutcomeDrain
	cond.Fields = map[string]any{
		"predicted_effect":  "drain",
		"no_wake_reason":    decision.Reason,
		"config_suppressed": suppressed,
		"sleep_intent":      intent,
	}

	// The #3994 keep-alive escape becomes a detection-side NON-ENQUEUE rather
	// than a mid-pass branch: a live session held only by a future held_until
	// with no sleep_intent is running `gc runtime heartbeat` through a long,
	// silent operation, and draining it would force-stop the very session the
	// heartbeat protects.
	if intent == "" && lifecycleTimerBlockerInfo(info, clk.Now()) == "user_hold" {
		cond.Reason = detectorReasonSleepKeepAlive
		cond.Outcome = TraceOutcomeSkipped
		cond.Fields["predicted_effect"] = "none"
		emit.add(cond, true)
		return
	}

	// Legacy's last reason rung — plain "no-wake-reason" — is a FLEET verdict
	// over pool counts, named and routed demand and the ready-wait set, and no
	// per-key predicate can re-derive it. The seam's rule is that the handler
	// answers from the row, so rather than hand it a reason it cannot check, the
	// family records those rows and leaves them to legacy this wave.
	if !suppressed && intent == "" {
		cond.Reason = detectorReasonNoWakeFleetOnly
		cond.Outcome = TraceOutcomeSkipped
		cond.Fields["predicted_effect"] = "none"
		emit.add(cond, true)
		return
	}

	if !exactSessionSleepPolicyProbeGated(policy) || intent == sessionSleepIntentIdleStopPending {
		// No idle confirmation stands between this row and its drain.
		emit.add(cond, true)
		return
	}
	if probe, has := in.Drains.idleProbe(info.ID); has {
		if !probe.ready {
			cond.Reason = detectorReasonIdleProbePending
			cond.Outcome = TraceOutcomeDeferredConfirm
			cond.Fields["predicted_effect"] = "none"
			emit.add(cond, true)
			return
		}
		// The confirmation is in. The handler consumes it and drains.
		emit.add(cond, true)
		return
	}
	// The row needs a probe it does not have. Its key competes for one of the
	// sweep's per-tick probe slots, which is the fleet-shaped rate limit that
	// stays detector-side.
	cond.Fields["predicted_effect"] = "idle_probe"
	*probes = append(*probes, cond)
}

// detectorGrantIdleProbeSlots applies maxIdleSleepProbesPerTick to the sweep's
// probe candidates and enqueues only the winners, exactly as legacy's
// selectIdleProbeTargets does: the per-tick ceiling is reduced by the probes
// already in flight, and a round-robin cursor over the sweep's pinned candidate
// order means no session is starved across sweeps and none is probed twice in a
// cycle. What legacy launches inline, this hands to the handler behind the key.
//
// Only the patrol/boot call site supplies a cursor. A nil cursor grants nothing
// — fail-closed, in the direction of not draining — because the narrowed sweeps
// have no cross-tick identity and a second sweep spending the same budget on the
// same tick would double the fleet's probe rate.
// detectPoolFill raises D-WAKE's pool-under-min FILL arm (WD.10b): a template
// whose open pool member count is under its desired count, plus routed work with
// no allocation. Both halves come from data the tick already carries — the
// desired counts ComputePoolDesiredStates precomputed in loadDemandSnapshot, and
// the declared routed-work view's unallocated entries — so detection itself
// still pays no read of its own.
//
// It is the family's only arm without a session row: there is nothing asleep to
// wake, the member does not exist yet. That is why its key is the routed work's
// (workID, poolTarget, sourceStore) triple and its sink is the pool-allocation
// admission rather than the session-start controller. It is also what makes the
// arm census-owed re-detection: a hint the 256-slot channel dropped, an
// allocation that failed, or an event nobody fired all leave the same durable
// condition — unallocated routed work under an under-filled pool — which this
// arm re-raises on the next sweep.
//
// Demand already spent is never re-raised: a member the planner counts as
// serving new demand (pending create, creating, start-pending, wait-held) is an
// open member here too, which is the same accounting
// poolSessionConsumesNewDemandInfo gives the legacy planner.
func detectPoolFill(in detectorSweepInput, emit *detectorConditionSink, rows []sessionpkg.ReconcileSession) {
	if len(in.RoutedWork) == 0 || len(in.PoolDesired) == 0 {
		return
	}
	open := detectorOpenPoolMembersByTemplate(in.Cfg, rows)
	filled := make(map[string]int, len(in.PoolDesired))
	for _, entry := range in.RoutedWork {
		target := strings.TrimSpace(entry.PoolTarget)
		if target == "" || entry.Assigned {
			continue
		}
		desired := in.PoolDesired[target]
		if desired <= 0 {
			continue
		}
		if open[target]+filled[target] >= desired {
			continue
		}
		filled[target]++
		emit.add(detectorCondition{
			Family:   detectorFamilyWake,
			Template: target,
			Site:     TraceSiteReconcilerWakeDecision,
			Reason:   detectorReasonWakePoolFill,
			Outcome:  TraceOutcomeStartCandidate,
			Work:     entry,
			Fields: map[string]any{
				"predicted_effect": "start",
				"work_id":          entry.WorkID,
				"pool_target":      target,
				"source_store":     entry.SourceStore,
				"pool_open":        open[target],
				"pool_desired":     desired,
			},
		}, false)
	}
}

// detectorOpenPoolMembersByTemplate counts the open pool-managed rows per
// template the way the pool planner counts them: a row that already represents
// spent demand counts, so the sweep never asks for a member the planner has
// already committed to.
func detectorOpenPoolMembersByTemplate(cfg *config.City, rows []sessionpkg.ReconcileSession) map[string]int {
	open := make(map[string]int)
	for _, row := range rows {
		info := row.Info
		if info.Closed || !isPoolManagedSessionInfo(info) {
			continue
		}
		template := normalizedSessionTemplateInfo(info, cfg)
		if template == "" {
			template = strings.TrimSpace(info.Template)
		}
		if template == "" {
			continue
		}
		open[template]++
	}
	return open
}

func detectorGrantIdleProbeSlots(in detectorSweepInput, emit *detectorConditionSink, candidates []detectorCondition) {
	if len(candidates) == 0 {
		return
	}
	limit := maxIdleSleepProbesPerTick - in.Drains.activeIdleProbes()
	granted := in.IdleProbes.grant(len(candidates), limit)
	for i := range candidates {
		cond := candidates[i]
		if !granted[i] {
			cond.Reason = detectorReasonIdleProbeBudget
			cond.Outcome = TraceOutcomeDeferredConfirm
			cond.Fields["predicted_effect"] = "none"
		}
		emit.add(cond, true)
	}
}

// detectorIdleProbeCursor is the sweep's round-robin position over its idle-probe
// candidates — the "probe cursor" §2 names as bounded in-memory detector state.
//
// It is a SECOND cursor rather than legacy's (drainTracker.idleProbeCursor), for
// the reason WD.4 recorded when it gave the named suspend window its own
// counter: legacy keeps advancing its cursor over its OWN candidate list on the
// same tick, so sharing one position would have made two writers interleave
// their round-robins and starve rows neither meant to skip. A cursor is an int,
// so the duplication costs nothing and keeps the two fairness schedules apart.
type detectorIdleProbeCursor struct {
	mu     sync.Mutex
	cursor int
}

func newDetectorIdleProbeCursor() *detectorIdleProbeCursor {
	return &detectorIdleProbeCursor{}
}

// grant returns which of the count candidates, in the sweep's pinned order, win
// a probe slot this sweep, and advances the cursor past them.
func (c *detectorIdleProbeCursor) grant(count, limit int) map[int]bool {
	granted := make(map[int]bool)
	if c == nil || count <= 0 || limit <= 0 {
		return granted
	}
	if limit > count {
		limit = count
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := c.cursor % count
	for i := 0; i < limit; i++ {
		granted[(start+i)%count] = true
	}
	c.cursor = (start + limit) % count
	return granted
}

func detectZombie(emit *detectorConditionSink, base detectorCondition, live detectorLivenessBits) {
	if !live.Probed || !live.Running || live.Alive {
		return
	}
	cond := base
	cond.Family = detectorFamilyZombie
	cond.Site = TraceSiteReconcilerTerminalProviderError
	cond.Reason = detectorReasonZombie
	cond.Outcome = TraceOutcomeNoChange
	cond.Fields = map[string]any{"predicted_effect": "mark_unhealthy"}
	emit.add(cond, false)
}

func detectStall(in detectorSweepInput, emit *detectorConditionSink, base detectorCondition, infoByID map[string]sessionpkg.Info, tp TemplateParams, live detectorLivenessBits, now time.Time) {
	claimless := in.Cfg.Session.ProgressStallTimeoutDuration()
	claimHolder := in.Cfg.Session.ClaimHolderStallTimeoutDuration()
	gate := minPositiveDuration(claimless, claimHolder)
	if gate <= 0 || !live.Alive || !sessionActivityReportable(in.Provider, base.SessionName) {
		return
	}
	lastActivity, err := in.Provider.GetLastActivity(base.SessionName)
	if err != nil || lastActivity.IsZero() || now.Sub(lastActivity) <= gate {
		return
	}
	// The min-floor exemption suppresses the CLAIM-LESS arm only. When
	// claim_holder_stall_timeout is set, a floor worker is still a candidate:
	// the handler runs the per-session claim lookup (DETECTOR.md §3, D-STALL).
	if cfgAgent := findAgentByTemplate(in.Cfg, tp.TemplateName); cfgAgent != nil {
		minFloor := cfgAgent.EffectiveMinActiveSessions()
		if minFloor > 0 && isMinFloorIdleWorker(minFloor, openPoolSessionCountForTemplate(infoByID, in.Cfg, tp.TemplateName)) && claimHolder <= 0 {
			cond := base
			cond.Family = detectorFamilyStall
			cond.Site = TraceSiteReconcilerProgressStallExempt
			cond.Reason = detectorReasonProgressStallExempt
			cond.Outcome = TraceOutcomeNoChange
			cond.Fields = map[string]any{"predicted_effect": "none", "pool_min": minFloor}
			emit.add(cond, false)
			return
		}
	}
	cond := base
	cond.Family = detectorFamilyStall
	cond.Site = TraceSiteReconcilerProgressStallExempt
	cond.Reason = detectorReasonProgressStall
	cond.Outcome = TraceOutcomeStop
	cond.Fields = map[string]any{
		"predicted_effect":       "recycle",
		"idle_gap_seconds":       int64(now.Sub(lastActivity).Seconds()),
		"gate_threshold_seconds": int64(gate.Seconds()),
	}
	emit.add(cond, true)
}

// detectStranded raises D-STRANDED for one row. The family's entry condition is
// the stranded diagnostic's own confirmation marker: a pool member the fleet has
// already seen not-alive while it still held assigned work. The marker tracks
// CONTINUOUS non-liveness (clearStrandedEventMarker drops it on any alive
// observation), so an alive row is never this family's.
//
// The arm SPLIT is what makes the family safe to act on. Only a row that
// satisfies every durable rung the handler re-derives — pool-managed, in a
// freeable terminal sleep state, and past strandedRepairConfirmGrace — carries
// the effect outcome and may enqueue. Every other marker-bearing row still
// records, under the deferred outcome, so the WD.15 join keeps the whole
// population WD.1 gave it; it just never becomes a key the handler would refuse
// on every patrol.
func detectStranded(emit *detectorConditionSink, base detectorCondition, info sessionpkg.Info, desired bool, live detectorLivenessBits, now time.Time) {
	marker := strings.TrimSpace(info.StrandedEventEmittedAt)
	if marker == "" || live.Alive {
		return
	}
	emittedAt, err := time.Parse(time.RFC3339, marker)
	if err != nil {
		return
	}
	confirmed := !emittedAt.IsZero() &&
		now.Sub(emittedAt) >= strandedRepairConfirmGrace &&
		isPoolManagedSessionInfo(info) &&
		isPoolSessionSlotFreeableInfo(info)
	cond := base
	cond.Family = detectorFamilyStranded
	cond.Site = TraceSiteSessionReconcileWakeSleep
	cond.Reason = detectorReasonStrandedPoolSlot
	cond.Outcome = TraceOutcomeClosed
	predicted := "repair_and_close"
	if !confirmed {
		cond.Reason = detectorReasonStrandedConfirmDeferred
		cond.Outcome = TraceOutcomeDeferredConfirm
		predicted = "defer"
	}
	cond.Fields = map[string]any{
		"predicted_effect":        predicted,
		"stranded_for_seconds":    int64(now.Sub(emittedAt).Seconds()),
		"desired":                 desired,
		"confirmation_window_sec": int64(strandedRepairConfirmGrace.Seconds()),
	}
	emit.add(cond, true)
}

// detectorDuplicateNamedIdentity answers D-DUP's grouping key for one row with
// the SAME predicate the retire logic applies
// (retireDuplicateConfiguredNamedSessionRows, session_beads.go): an open
// configured-named row, continuity-eligible, whose stored identity still
// resolves to a named-session spec. Two things about it are load-bearing.
//
// First, D-DUP cannot key on the canonical identity every other family uses:
// pool rows that have no slot stamp yet all resolve to the template's qualified
// name, so identity-grouping would read ordinary pool siblings as duplicates of
// one another (the WD.1 delta this preserves).
//
// Second, detection and the handler's re-derivation must answer from the same
// predicate. A detector that grouped more loosely than the handler would enqueue
// keys the handler refuses on every patrol — a 30-second treadmill against a
// condition that is real but not this family's.
func detectorDuplicateNamedIdentity(in detectorSweepInput, info sessionpkg.Info) string {
	if info.Closed || !isNamedSessionInfo(info) || !sessionpkg.NamedSessionInfoContinuityEligible(info) {
		return ""
	}
	identity := namedSessionIdentityInfo(info)
	if identity == "" {
		return ""
	}
	if _, ok := findNamedSessionSpec(in.Cfg, in.CityName, identity); !ok {
		return ""
	}
	return identity
}

// detectDuplicateNamed raises one condition per LOSER row when more than one
// open continuity-eligible row shares a named identity. The family has exactly
// this one arm: every condition it raises predicts the retire of its own key.
func detectDuplicateNamed(in detectorSweepInput, emit *detectorConditionSink, byIdentity map[string][]sessionpkg.Info) {
	identities := make([]string, 0, len(byIdentity))
	for identity, rows := range byIdentity {
		if len(rows) > 1 {
			identities = append(identities, identity)
		}
	}
	sort.Strings(identities)
	for _, identity := range identities {
		rows := byIdentity[identity]
		spec, _ := findNamedSessionSpec(in.Cfg, in.CityName, identity)
		winner := detectorDuplicateWinner(rows, spec.SessionName)
		for _, info := range rows {
			if info.ID == winner.ID {
				continue
			}
			emit.add(detectorCondition{
				Family:      detectorFamilyDup,
				SessionID:   info.ID,
				SessionName: strings.TrimSpace(info.SessionNameMetadata),
				Template:    normalizedSessionTemplateInfo(info, in.Cfg),
				Identity:    identity,
				Site:        TraceSiteSessionReconcileHealRetire,
				Reason:      detectorReasonDuplicateNamed,
				Outcome:     TraceOutcomeNoChange,
				Fields: map[string]any{
					"predicted_effect": "retire",
					"winner_id":        winner.ID,
					"duplicate_count":  len(rows),
				},
			}, true)
		}
	}
}

// detectorDuplicateWinner names the surviving row with the retire path's OWN
// rule — generation, then canonical session name, then created-at, then bead ID
// (namedSessionWinsCanonicalRepairInfo). Reusing that predicate rather than
// restating it is the point: a detector-side tiebreak that drifted from the
// handler's would schedule the retire of a row the handler then keeps.
//
// rows arrive in the sweep's pinned iteration order (session name, then bead ID
// — detectorOrderedRows), which is what makes the incumbent seat deterministic;
// the rule itself is a total order over distinct IDs, so pinned order and rule
// agree by construction rather than by luck. Both the sweep and the handler feed
// this the same pinned order.
func detectorDuplicateWinner(rows []sessionpkg.Info, canonicalSessionName string) sessionpkg.Info {
	if len(rows) == 0 {
		return sessionpkg.Info{}
	}
	winner := rows[0]
	for _, candidate := range rows[1:] {
		if namedSessionWinsCanonicalRepairInfo(candidate, winner, canonicalSessionName) {
			winner = candidate
		}
	}
	return winner
}

// detectorAdmissionSourceFor is THE detector half of the handler-dispatch seam.
// It answers one question for one detected condition: may this arm hand its
// exact key to the session-start controller, and under which admission source?
//
// Each later WD slice adds EXACTLY ONE case. The case names the family, its act
// constant, and the arm(s) that predict a real effect — a family's non-effect
// arms (deferrals, preserved rows, traced refusals) record for the parity join
// and never enqueue. No new controller, queue, or framework: the value returned
// here is fed straight into the existing Admit(id, source) entry.
func detectorAdmissionSourceFor(cond detectorCondition) (sessionStartAdmissionSource, bool) {
	switch cond.Family {
	case detectorFamilyDeadline:
		return sessionStartAdmissionDeadline, detectorActDeadline && cond.Outcome == TraceOutcomeStop
	case detectorFamilyOrphan:
		// The family's two effect arms split by OUTCOME and carry SEPARATE
		// admission sources: a provably dead undesired row is closed (WD.3), a
		// live one is drained (WD.4). Every other arm — kept-open,
		// deferred-confirm, running-set-unavailable — predicts no effect, so it
		// records for the parity join and never enqueues.
		switch cond.Outcome {
		case TraceOutcomeClosed:
			return sessionStartAdmissionOrphanClose, detectorActOrphanClose
		case TraceOutcomeDrain:
			return sessionStartAdmissionOrphanDrain, detectorActOrphanDrain
		}
		return "", false
	case detectorFamilyStaleCreate:
		// Only the expired-lease ROLLBACK arm predicts an effect; the
		// preserved-row arm records no-change for the parity join (WD.7).
		return sessionStartAdmissionStaleCreate, detectorActStaleCreate && cond.Outcome == TraceOutcomeRollback
	case detectorFamilyDrift:
		// D-DRIFT's two SITES (ConfigDrift, LiveDrift) are one effect arm under
		// one outcome, so they take one admission source and one legacy yield.
		// The split this family needs is the one detection cannot make —
		// converge versus defer — so it lives in the handler, and EITHER half
		// having landed is reason enough to enqueue the key: the same admission
		// carries a row to a rebaseline, a relaunch, or an attached-user
		// deferral, and only the handler knows which.
		return sessionStartAdmissionConfigDrift,
			(detectorActDriftConverge || detectorActDriftDefer) && cond.Outcome == TraceOutcomeDrain
	case detectorFamilyDup:
		// D-DUP has a single arm: detectDuplicateNamed raises one condition per
		// LOSER row and nothing else, so every condition here predicts the retire
		// of its own key. That arm carries TraceOutcomeNoChange — the retire is
		// the predicted EFFECT, while the sweep itself applies nothing — and the
		// gate names that outcome explicitly rather than routing on the act
		// constant alone. Naming it is behavior-identical today and is what
		// actually delivers the guarantee this family wants: a future second
		// D-DUP arm carrying some other outcome cannot ride in unnoticed on a
		// family-wide gate, it has to come here and declare itself.
		return sessionStartAdmissionDuplicateNamed, detectorActDup && cond.Outcome == TraceOutcomeNoChange
	case detectorFamilySleep:
		// One effect arm, one source. The family raises several arms — the #3994
		// keep-alive escape, a probe still in flight, a budget-deferred probe slot,
		// and the fleet-only no-wake verdicts this slice's handler cannot
		// re-derive per key — and every one of them predicts NO effect, so they
		// record for the parity join and never enqueue. Only the arm that predicts
		// a real drain (or the probe launch that gates it) carries
		// TraceOutcomeDrain, and only that arm routes.
		return sessionStartAdmissionSleepDrain, detectorActSleep && cond.Outcome == TraceOutcomeDrain
	case detectorFamilyStall:
		// Only the RECYCLE arm predicts an effect. The min-floor exempt arm
		// carries TraceOutcomeNoChange and records for the parity join without
		// enqueueing — that suppression is the whole bounded exemption, and it
		// applies to the claim-less family only: detectStall falls through to
		// this arm for a floor worker whenever claim_holder_stall_timeout is
		// positive, so the handler can pay the per-session claim lookup legacy
		// pays at session_reconciler.go:2696.
		return sessionStartAdmissionProgressStall, detectorActStall && cond.Outcome == TraceOutcomeStop
	case detectorFamilyDrain:
		// One arm, one source. detectDrain raises exactly one condition per row
		// carrying drain intent, and every one of them predicts an advance of that
		// drain — the arm is named by its outcome anyway so a future second arm has
		// to come here and declare itself rather than ride in on the family gate.
		return sessionStartAdmissionDrainAdvance, detectorActDrain && cond.Outcome == TraceOutcomeDrain
	case detectorFamilyWake:
		// D-WAKE's arms split by REASON rather than by outcome: every arm predicts
		// the same effect (a start) on the same outcome, and what differs is which
		// certified lease can own the row. The named and configured-dependency
		// arms route at WD.10a; the pool-fill arm keeps recording for the parity
		// join until WD.10b lands its handler and its yield.
		switch cond.Reason {
		case detectorReasonWakeTargetNamed, detectorReasonWakeTargetDependency:
			return sessionStartAdmissionWakeFill,
				detectorActWakeNamedDependency && cond.Outcome == TraceOutcomeStartCandidate
		case detectorReasonWakeTarget:
			// The slotized pool member's re-wake: an existing asleep row under a
			// strict-default pool, owned by AdmitStrictDefaultPoolWake through
			// the same certified-lease entry its siblings use.
			return sessionStartAdmissionWakeFill,
				detectorActWakePoolFill && cond.Outcome == TraceOutcomeStartCandidate
		case detectorReasonWakePoolFill:
			// The one arm with no session key. It routes through
			// EnqueuePoolAllocation instead of an admission source, so it
			// reports no source here — routeDetectorConditions dispatches it on
			// the reason.
			return "", false
		}
		return "", false
	case detectorFamilyStranded:
		// Only the CONFIRMED arm predicts an effect: a marker-bearing row that
		// is still inside its confirmation window, or that no longer satisfies
		// the durable pool-freeable rungs, records the deferred outcome for the
		// parity join and never enqueues (WD.14).
		return sessionStartAdmissionStrandedRepair, detectorActStranded && cond.Outcome == TraceOutcomeClosed
	case detectorFamilyZombie:
		// One arm: detectZombie raises a condition only for `running ∧ !alive`
		// and nothing else, so every condition here predicts the mark of its own
		// key. It carries TraceOutcomeNoChange — the mark is the predicted
		// EFFECT while the sweep applies nothing — and the gate names that
		// outcome explicitly rather than routing on the act constant alone, for
		// the reason D-DUP's arm records: a future second arm under some other
		// outcome has to come here and declare itself.
		return sessionStartAdmissionZombieMark, detectorActZombie && cond.Outcome == TraceOutcomeNoChange
	}
	return "", false
}

// detectorProviderStopCapable is the detection-side D2 screen: a family whose
// handler must stop or kill a runtime is never enqueued under a provider that
// cannot prove fresh liveness and an unattended, token-bound stop. Screening
// here rather than in the handler is what keeps a D2-incapable city off the
// 30-second re-enqueue treadmill (DETECTOR.md §2).
func detectorProviderStopCapable(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	if _, ok := sp.(runtime.FreshLivenessObserver); !ok {
		return false
	}
	_, ok := sp.(runtime.UnattendedSessionStopper)
	return ok
}

// routeDetectorConditions hands every acting family's effect arms to the
// existing session-start controller by exact key. It is the only place the
// sweep is allowed to leave read-only mode, and it still writes nothing itself:
// the handler behind the key owns every effect.
func routeDetectorConditions(in detectorSweepInput, result *detectorSweepResult) {
	// The seam runs when SOME sink exists. Admit is the session-keyed families'
	// sink; D-WAKE's pool-under-min FILL arm has its own, because its key is a
	// routed work item rather than a session, so a city with only the allocation
	// admission still routes that arm.
	if result == nil || !detectorAnyFamilyActs() || (in.Admit == nil && in.EnqueuePoolAllocation == nil) {
		return
	}
	for i := range result.Conditions {
		cond := &result.Conditions[i]
		if routeDetectorPoolFill(in, cond) {
			continue
		}
		if in.Admit == nil {
			continue
		}
		source, routable := detectorAdmissionSourceFor(*cond)
		if !routable || cond.SessionID == "" {
			continue
		}
		if detectorFamilyRequiresStopCapability(cond.Family) && !detectorProviderStopCapable(in.Provider) {
			cond.AdmissionOutcome = detectorAdmissionRefusedProviderIncapable
			continue
		}
		cond.AdmissionSource = source
		// D-WAKE is the one family that may not ride the bare Admit: a wake is a
		// start, and a start is fenced by a certified lease, so its key has to
		// arrive already carrying one. Without a certifying entry — or when no
		// family can own the row — it records a refusal and stays legacy's.
		outcome, err := sessionStartAdmissionOutcome(""), error(nil)
		if source == sessionStartAdmissionWakeFill {
			if in.AdmitWake == nil {
				cond.AdmissionOutcome = detectorAdmissionRefusedUncertifiable
				continue
			}
			outcome, err = in.AdmitWake(cond.SessionID)
		} else {
			outcome, err = in.Admit(cond.SessionID, source)
		}
		if err != nil {
			cond.AdmissionOutcome = detectorAdmissionRefusedError
			continue
		}
		if outcome == "" {
			cond.AdmissionOutcome = detectorAdmissionRefusedUncertifiable
			continue
		}
		cond.AdmissionOutcome = detectorRouteOutcome(outcome)
	}
}

// routeDetectorPoolFill hands D-WAKE's pool-under-min FILL arm to the existing
// pool-allocation admission by exact (workID, poolTarget, sourceStore) key. It
// reports whether the condition belongs to that arm, so the session-keyed
// routing below never sees a condition with no session.
//
// A dropped key is a traced refusal and nothing else: the condition is derived
// from durable state, so the next sweep re-detects it. That is the whole of
// Q2's census-owed recovery — no retry, no block, no legacy poke.
func routeDetectorPoolFill(in detectorSweepInput, cond *detectorCondition) bool {
	if cond.Family != detectorFamilyWake || cond.Reason != detectorReasonWakePoolFill {
		return false
	}
	if !detectorActWakePoolFill || cond.Outcome != TraceOutcomeStartCandidate {
		return true
	}
	if in.EnqueuePoolAllocation == nil || cond.Work.WorkID == "" || cond.Work.PoolTarget == "" {
		cond.AdmissionOutcome = detectorAdmissionRefusedUncertifiable
		return true
	}
	if !in.EnqueuePoolAllocation(cond.Work) {
		cond.AdmissionOutcome = detectorAdmissionRefusedOverflow
		return true
	}
	cond.AdmissionOutcome = detectorAdmissionQueuedPoolAllocation
	return true
}

// routedToKeyed reports whether the routing seam handed this condition to the
// keyed population. Every session-keyed family answers on its admission source;
// D-WAKE's pool-fill arm has no source because its sink is the pool-allocation
// admission rather than the session-start controller, so it answers on the
// outcome the seam recorded.
func (c detectorCondition) routedToKeyed() bool {
	return c.AdmissionSource != "" || c.AdmissionOutcome == detectorAdmissionQueuedPoolAllocation
}

// recordDetectorShadow writes the sweep's conditions to the trace cycle as
// detector-shadow records: the LEGACY site codes, effect_applied=false,
// effect_owner=detector-shadow, and a detector_-prefixed reason. A condition
// that WAS routed carries effect_owner=keyed instead, because the keyed
// population now owns it — but still effect_applied=false, because the sweep
// enqueued a key and applied nothing. The effect_applied=true record at the
// same legacy site is the handler's, written when the effect actually lands.
// The sweep writes nothing else — no store, no provider. A nil cycle (the `gc
// start` one-shot has no tracer) makes every call a no-op.
func recordDetectorShadow(cycle *sessionReconcilerTraceCycle, in detectorSweepInput, result detectorSweepResult) {
	if cycle == nil {
		return
	}
	conditions := result.Conditions
	if len(result.FamilyOverflow) > 0 {
		conditions = append(append([]detectorCondition(nil), conditions...), detectorOverflowSummaries(result.FamilyOverflow)...)
	}
	for _, cond := range conditions {
		owner := detectorShadowEffectOwner
		if cond.routedToKeyed() {
			owner = detectorKeyedEffectOwner
		}
		fields := traceRecordPayload{
			"effect_applied":   false,
			"effect_owner":     owner,
			"detector_family":  string(cond.Family),
			"detector_acts":    detectorFamilyActs(cond.Family),
			"session_id":       cond.SessionID,
			"session_identity": cond.Identity,
			"sweep_trigger":    in.Trigger,
		}
		if cond.AdmissionOutcome != "" {
			fields["admission_outcome"] = string(cond.AdmissionOutcome)
		}
		if cond.AdmissionSource != "" {
			fields["admission"] = string(cond.AdmissionSource)
		}
		for k, v := range cond.Fields {
			fields[k] = v
		}
		cycle.RecordDecision(cond.Site, cond.Reason, cond.Outcome, cond.Template, cond.SessionName, fields)
	}
	for _, spec := range detectorFamilySpecs {
		if !spec.ObservedOnly {
			continue
		}
		cycle.RecordControllerOperation(TraceSiteControllerTickPhase, detectorReasonObservedPatrolScan, TraceOutcomeNoChange,
			"detector_sweep.observed_patrol_scan", 0, map[string]any{
				"effect_applied":  false,
				"effect_owner":    detectorShadowEffectOwner,
				"detector_family": string(spec.Family),
				"observed_only":   true,
				"sweep_trigger":   in.Trigger,
			})
	}
	cycle.RecordControllerOperation(TraceSiteControllerTickPhase, detectorReasonSweepComplete, TraceOutcomeNoChange,
		"detector_sweep.complete", result.Duration, map[string]any{
			"effect_applied":              false,
			"effect_owner":                detectorShadowEffectOwner,
			"sweep_trigger":               in.Trigger,
			"rows_evaluated":              result.RowsEvaluated,
			"conditions":                  len(result.Conditions),
			"unknown_state_skipped":       result.UnknownStateSkipped,
			"suppressed_by_partial_store": result.SuppressedByPartialStore,
			"running_set_known":           result.RunningSetKnown,
			"circuit_hydrated":            result.CircuitHydrated,
			"circuit_identities":          result.CircuitIdentities,
			"circuit_resets_owed":         result.CircuitResetsOwed,
			"store_query_partial":         in.StoreQueryPartial,
			"any_family_acts":             detectorAnyFamilyActs(),
			"duration_ms":                 result.Duration.Milliseconds(),
		})
}

// detectorSweepTriggerFor names the patrol-family entry point the sweep ran on.
func detectorSweepTriggerFor(bootReconcile bool) string {
	if bootReconcile {
		return "boot"
	}
	return "patrol"
}

// runDetectorSweep is the single production entry point: detect (read-only),
// route the acting families' exact keys into the existing session-start
// controller, then record. Shadow-only families and every entry point without
// an Admit hook come out of this exactly as they went in: read-only. Callers
// deliberately discard the result — the trace records ARE the output. Tests
// that need the conditions call detectSessionConditions directly.
func runDetectorSweep(ctx context.Context, cycle *sessionReconcilerTraceCycle, in detectorSweepInput) {
	result := detectSessionConditions(ctx, in)
	// Publish BEFORE routing: the keys this sweep is about to enqueue are
	// handled against the observation this sweep made, not the previous one.
	if in.PublishLiveness != nil {
		in.PublishLiveness(result.Liveness)
	}
	if in.PublishWakeEvaluations != nil {
		in.PublishWakeEvaluations(result.WakeEvaluations)
	}
	routeDetectorConditions(in, &result)
	recordDetectorShadow(cycle, in, result)
}
