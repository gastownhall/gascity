package main

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain's policy table (CONTRACT v5 D2) and decideRow's arms A11 (the
// ack), A19 (void and cancel) and A20 (drain begin). The signal and its
// graces are C6b1's, the ack C6c2's, idle respawn C6d's, and the
// config-drift begin C7b1's.

// Drain decide reasons. A begin, cancel or void carries the drain reason
// after the colon.
const (
	decideDrainBegin     = "drain-begin:"
	decideDrainCancel    = "drain-cancel:"
	decideDrainVoid      = "drain-void:"
	decideStopResidue    = "drain-void:residue"
	decideStopActive     = "stop-requested"
	decideDrainSignal    = "drain-signal:"
	decideAckVoid        = "drain-ack-void"             // C6a ruling 3
	decideFloorAckCancel = "drain-ack-cancel:min-floor" // DRAIN-010, PAR-FLOORACK
	decideWakeGrace      = "undesired-wake-grace"       // INC-003
	decideDrainWorkKept  = "drain-kept:assigned-work"
	decideIdleUnprovable = "drain-kept:idle-unprovable" // SESS-621
	decideIdleStopClear  = "idle-stop-pending-clear"    // SESS-616
)

// Drain reasons and markers the allocation does not name.
const (
	drainIdle        = string(session.SleepReasonIdle)
	drainConfigDrift = "config-drift"
	// drainOperator names the signal of an ack an operator's `gc runtime
	// drain` explains (stopRequest.Operator).
	drainOperator    = "operator-drain"
	reasonIdleSleep  = "idle-sleep"
	sleepIntentIdle  = "idle-stop-pending"
	timerBlockerHold = "user_hold"
)

// drainRank orders the reasons whose authorization is "an equal or
// stronger reason": suspended > orphaned > no-wake-reason > idle.
var drainRank = map[string]int{drainSuspended: 4, drainOrphaned: 3, reasonNoWake: 2, drainIdle: 1}

// drainPolicy is one row of the table. authorized is the "authorization at
// the signal" column, nil for a row a later PR completes, which holds
// meanwhile. The last column is "lens; otherwise void": a lens cancels; a
// drain that lost its authorization holds under Keep where keepHolds, is
// canceled where lostCancels (config drift), and is otherwise voided, so A20
// begins again under the current reason, with its own fences and grace.
type drainPolicy struct {
	keepHolds, lostCancels bool
	authorized             func(r *rowFacts, reason string) bool
}

// drainPolicyOf is reason's row; any other reason is a sleep intent's.
func drainPolicyOf(reason string) drainPolicy {
	switch reason {
	case drainIdle, reasonNoWake:
		return drainPolicy{keepHolds: true, authorized: authorizedByRank}
	case drainOrphaned:
		return drainPolicy{keepHolds: true, authorized: func(r *rowFacts, _ string) bool { return r.entry.Desired == desireDrain }}
	case drainSuspended:
		return drainPolicy{authorized: func(r *rowFacts, _ string) bool { return r.entry.DrainReason == drainSuspended }}
	case drainConfigDrift:
		return drainPolicy{lostCancels: true, authorized: func(r *rowFacts, _ string) bool { return !driftResolvedOrDeferred(r) }}
	case executionStalledDrainReason:
		return drainPolicy{authorized: func(*rowFacts, string) bool { return true }}
	case idleRespawnDrainReason:
		// Eligible (C6d); not cancelable by the wake or assigned-work lenses
		// (the pending lens still cancels it, DRAIN-022), and a request no
		// longer eligible is voided (v5 D2), keeping its attempt.
		return drainPolicy{authorized: func(r *rowFacts, _ string) bool { return respawnEligible(r) }}
	}
	return drainPolicy{authorized: func(r *rowFacts, reason string) bool { return strings.TrimSpace(r.row.Info.SleepIntent) == reason }}
}

// authorizedByRank: the allocation still sleeps or drains the row, for an
// equal or stronger reason.
func authorizedByRank(r *rowFacts, reason string) bool {
	e := r.entry
	return (e.Desired == desireSleep || e.Desired == desireDrain) && drainRank[drainReasonOf(r)] >= drainRank[reason]
}

// drainReasonOf is the reason the allocation drains the row for, as legacy
// selects it (session_reconciler.go:4518-4537, SESS-618): a Drain entry's
// DrainReason; for a Sleep entry the row's sleep intent, then idle, then
// suspended (a configured named row of a suspended city), else
// no-wake-reason. A heartbeat hold (held_until with no intent, SESS-617)
// has none. Legacy's live-claim veto is read at the orphaned signal (L5,
// v5.10a B), not here.
// drainableLiveness reports a runtime A20 may begin a drain over: alive, or
// for a named Drain row a dead runtime, which A21 never closes
// (deadRuntimeCloses excludes named rows; mc-q64l4)
// and A12, ahead of A20, has classified by then. A held pool orphan's dead
// runtime is no drain's: A21's orphan guard keeps its slot (§12.2 #27). Its orphan
// drain then runs as legacy's, whose stop A4 runs on any liveness but gone
// (D2, A4); a closable one is A21's.
func (r *rowFacts) drainableLiveness() bool {
	e := r.entry
	return e.Liveness == livenessAlive || e.Liveness == livenessDead && e.Desired == desireDrain && isNamedSessionInfo(r.row.Info)
}

func drainReasonOf(r *rowFacts) string {
	e, info := r.entry, r.row.Info
	intent := strings.TrimSpace(info.SleepIntent)
	switch {
	case e.Desired == desireDrain:
		return firstNonEmpty(e.DrainReason, drainOrphaned)
	case e.Desired != desireSleep:
		return ""
	case intent == sleepIntentIdle:
		return drainIdle
	case intent != "":
		return intent
	case lifecycleTimerBlockerInfo(info, r.w.Now) == timerBlockerHold:
		return ""
	case e.Reason == reasonIdleSleep || e.Reason == reasonConfigSleep:
		return drainIdle
	case e.DrainReason == drainSuspended:
		return drainSuspended
	}
	return reasonNoWake
}

// drainLens is legacy's cancel lenses for reason, in legacy's order
// (session_wake.go advanceSessionDrainsWithSessionsTraced): a pending
// interaction (DRAIN-045), assigned work (DRAIN-046), any wake reason
// (DRAIN-047), and a heartbeat hold (SESS-617). Each is gated on the
// reason, so a non-cancelable reason never fires one.
func drainLens(r *rowFacts, reason string) bool {
	e := r.entry
	switch {
	case containsWakeReason(e.WakeReasons, WakePending) && pendingDrainReasonCancelable(reason):
		return true
	case e.Reason == reasonAssignedWork && containsWakeReason(e.WakeReasons, WakeWork) && assignedWorkDrainReasonCancelable(reason):
		return true
	case len(e.WakeReasons) > 0 && drainReasonCancelable(reason):
		return true
	}
	return e.Desired == desireSleep && strings.TrimSpace(r.row.Info.SleepIntent) == "" &&
		lifecycleTimerBlockerInfo(r.row.Info, r.w.Now) == timerBlockerHold && drainReasonCancelable(reason)
}

// driftResolvedOrDeferred is legacy's config-drift cancel
// (cancelSessionConfigDriftDrainInfo's callers): the stored config hash no
// longer differs from the resolved one, or the row is attached or recently
// attached-deferred for this drift. An unresolved template proves neither.
func driftResolvedOrDeferred(r *rowFacts) bool {
	if r.w.Env == nil || r.w.Env.Cfg == nil {
		return false
	}
	res, ok := r.w.Templates.lookup(r.row.Info)
	if !ok || res.Err != nil {
		return false
	}
	key := sessionConfigDriftKey(r.row.Info, r.w.Env.Cfg, res.TP)
	return key == "" || r.w.Observed[r.k].Attached ||
		attachedConfigDriftDeferredAt(r.row.Info, r.w.Now, key)
}

// armDrainVoidCancel is A19, on a requested drain: a lens cancels it; one
// that lost its authorization takes the table's last column. A request
// rule 2 ended (a suspended or killed row) is voided, both halves. Acked and
// signaled requests are A11's and A4's.
func armDrainVoidCancel(r *rowFacts) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	req, ok := activeStop(r.row)
	switch {
	case req.Residue:
		return intent{Kind: intentDrainVoid, Reason: decideStopResidue, Basis: basis, Patch: stopVoidResiduePatch()}, true
	case !ok || req.Phase != stopRequested:
		return intent{}, false
	}
	p, kind := drainPolicyOf(req.Reason), intentDrainVoid
	switch {
	case p.authorized == nil || (p.keepHolds && r.entry.Desired == desireKeep):
		return intent{}, false
	case drainLens(r, req.Reason) || (req.Reason == reasonNoWake && workKeepsNoWakeRow(r)):
		// Work assigned to a no-wake-reason row after its begin: legacy's
		// assigned-work cancel at its ack (DRAIN-046), which the begin's
		// keep matches (C6a2 review 1b).
		kind = intentDrainCancel
	case p.authorized(r, req.Reason):
		return intent{}, false
	case p.lostCancels:
		kind = intentDrainCancel
	}
	reason := decideDrainVoid + req.Reason
	if kind == intentDrainCancel {
		reason = decideDrainCancel + req.Reason
	}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: stopCancelPatch()}, true
}

// workKeepsNoWakeRow is legacy's assigned-work cancel of a no-wake-reason
// drain (sessionHasAwakeAssignedWorkForReachableStore): the row's awake
// assigned work, claimed or ready, or in-progress work the allocation does
// not count awake because it is blocked, which legacy still counts.
func workKeepsNoWakeRow(r *rowFacts) bool {
	if r.entry.AssignedWork != nil {
		return true
	}
	var cfg *config.City
	if r.w.Env != nil {
		cfg = r.w.Env.Cfg
	}
	ids := sessionAssignmentIdentifiersForConfigInfo(r.row.Info, cfg)
	for _, w := range r.w.Demand.AssignedWork {
		if w.Status == "in_progress" && slices.Contains(ids, strings.TrimSpace(w.Assignee)) {
			return true
		}
	}
	return false
}

// armAck is A11 (v5 D5; C6c2): an ack of the row's generation, the agent's
// or an operator's (activeStop's rule 4), makes the signal due at once, D2's
// ack row with grace 0, whatever the drain's reason; A4's stop verb then
// takes it. v2 reads no runtime env for acks: E3 writes them by CAS under
// the row's own token (I18). The signal rests on the runtime, which its
// effect reads fresh: an ack older than the runtime's creation came from a
// runtime that Manager.ensureRunning has since replaced at the same
// generation (C6a ruling 3, J27), so the effect voids the ack half instead
// (v5.9g), and a drain the controller began goes on as requested. An ack
// whose time is missing or unreadable is void at once (#7635 review
// ruling): never stop a runtime that may have been replaced; the agent
// can ack again.
//
// DRAIN-010 (PAR-FLOORACK, legacy's cancelSelfInitiatedDrainAckAtMinFloor):
// a bare ack, one no request on the row explains, on a member of
// its template's min_active_sessions floor (the allocator's floor set,
// legacy's lowest-bead-id membership) is the agent's own idle ack, and
// stopping the seat only recreates it (sc-j27j0d): the ack is canceled,
// its half voided, and the seat stays warm, whatever the allocation
// wants of it (Keep, Sleep or Drain, as legacy cancels them). Unlike
// legacy, which cancels only on a live runtime, a dead floor seat's ack
// cancels too: A12 recycles the dead pane in place (mc-7is1r), where
// legacy stops the seat and the floor recreates it. A request the row
// carries outranks the floor: the
// controller's own drains, and an operator's `gc runtime drain`
// (stopRequest.Operator, legacy's GC_DRAIN).
func armAck(r *rowFacts) (intent, bool) {
	req, ok := activeStop(r.row)
	if !ok || req.Phase != stopAcked {
		return intent{}, false
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	if stale := r.ackPredatesRuntime(req.AckAt); stale || req.AckAt.IsZero() {
		return intent{Kind: intentSignalAck, Reason: decideAckVoid, Basis: basis, Patch: stopAckVoidPatch()}, true
	}
	// The floor check stays after the stale-ack void: that check records
	// the runtime rest the probing kind needs, so a cancel decided first
	// would defer at admission (rests-missing). A floor seat's ack over a
	// corpse no token attributes is signaled like any other, not canceled:
	// its stop finalizes the row and the floor recreates the seat (CONTRACT
	// v5.10w, §12.2 row 66). Liveness unknown here is that case, since A9
	// holds every other unknown row (ackedOverCorpse). A cancel would void
	// the ack and A9 would then hold the row over the corpse for good, and no
	// restart in place can attribute the corpse.
	if req.Reason == "" && !req.Operator && r.entry.Floor != nil && r.entry.Liveness != livenessUnknown {
		if r.entry.Liveness == livenessDead {
			// A dead floor seat proposes nothing (§12.2 row 66): A18's
			// recycle restarts it in place with the ack still on the row, so
			// its death is a drain's (nothing accrues), and the PreWake
			// clears the stop request. A cancel first would void the ack and
			// account a recent wake's death as a crash (mc-7is1r review).
			return intent{}, false
		}
		return intent{Kind: intentSignalAck, Reason: decideFloorAckCancel, Basis: basis, Patch: stopAckVoidPatch()}, true
	}
	reason := req.Reason
	if reason == "" && req.Operator {
		reason = drainOperator
	}
	return intent{Kind: intentSignalAck, Reason: decideDrainSignal + reason, Basis: basis, Patch: session.DrainAckStopPendingPatch(r.w.Now)}, true
}

// ackPredatesRuntime reports, on the effect's fresh read, a runtime object
// listed under the name that was created after the ack at (tmux
// #{session_created}, whole seconds; empty when nothing is listed). Either
// read counts: a runtime replaced between them is newer still. The pass
// reads no creation, so there it records the rest and reports false; so it
// does on a backend that does not report one, and for an ack without a
// time, which armAck voids itself. A Foreign object, another row's runtime,
// is never this row's same-generation replacement (CONTRACT v5.10k-2): an
// acked drain under a later squatter goes on to its finalize.
func (r *rowFacts) ackPredatesRuntime(at time.Time) bool {
	r.rests |= restRuntime
	if r.fresh == nil || at.IsZero() {
		return false
	}
	if r.fresh.Same && compareIdentity(r.row.Info, r.fresh.Identity) == identityForeign {
		return false // another row's runtime is no replacement of this row's (v5.10k-2)
	}
	for _, l := range []runtime.Liveness{r.fresh.Live, r.fresh.Again} {
		created, err := strconv.ParseInt(l.ObjectCreated, 10, 64)
		if err == nil && time.Unix(created, 0).After(at) {
			return true
		}
	}
	return false
}

// armSignal is A20's signal (v5 D1, D2; C6b1): requested → signaled, by one
// CAS of legacy's stop-pending projection (DrainAckStopPendingPatch), which
// A4's stop verb then takes. A requested drain is due once its policy row's
// grace has passed since the drain began; A19, before this arm, has
// canceled or voided one no longer authorized, and a row it holds under
// Keep, or one whose reason's row a later PR completes, holds here too.
// An idle or no-wake-reason signal reads L3 and L4 fresh (C8.9), so it is
// the fresh kind, and rests on the row's own live runtime. The effect
// decides again on the fresh row, so a drain A19 voids or cancels since
// the pass is never signaled (R16).
func armSignal(r *rowFacts, req stopRequest) (intent, bool) {
	hold := intent{Reason: decideStopActive}
	p := drainPolicyOf(req.Reason)
	if p.authorized == nil || (p.keepHolds && r.entry.Desired == desireKeep) {
		return hold, true
	}
	if wait := signalWait(r.w, req); wait > 0 {
		r.after(wait)
		return hold, true
	}
	kind := drainKind(req.Reason, true)
	if kind == intentSignalFresh && !r.freshOwnAlive() {
		return hold, true
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: kind, Reason: decideDrainSignal + req.Reason, Basis: basis, Patch: session.DrainAckStopPendingPatch(r.w.Now)}, true
}

// signalWait is how long a requested drain waits for its signal: its grace
// less the time since the drain began. A begin stamp that is unparseable
// or ahead of now (a skewed clock wrote it) is due at once, as INC-003 gives
// such a wake no grace: skew must never pin a row, and the decide keeps no
// memory to count a grace from. So the wait is never more than one grace.
func signalWait(w *World, req stopRequest) time.Duration {
	if req.At.IsZero() || req.At.After(w.Now) {
		return 0
	}
	return req.At.Add(drainSignalGrace(w, req.Reason)).Sub(w.Now)
}

// drainSignalGrace is the policy table's signal grace for reason: the
// patrol interval (World.Patrol, gather's), capped by drift_drain_timeout
// for a config drift.
func drainSignalGrace(w *World, reason string) time.Duration {
	grace := cmp.Or(w.Patrol, v2DefaultPatrol)
	if reason == drainConfigDrift && w.Env != nil && w.Env.Cfg != nil {
		grace = min(grace, w.Env.Cfg.Daemon.DriftDrainTimeoutDuration())
	}
	return grace
}

// idleProof is SESS-621's gate on an idle begin: a non-interactive session
// begins at once; an interactive one only with Full sleep capability, and
// then probe asks its effect to prove the agent idle (WaitForIdle, with no
// activity since). A row with no resolved policy never begins, as legacy's
// zero policy. A legacy idle-stop-pending mark is the proof already. An
// idle-respawn begin, whose eligibility requires Full capability, always
// proves the agent idle (legacy's shouldBeginIdleDrainInfo has no mark
// bypass).
func idleProof(w *World, info session.Info, reason string) (allowed, probe bool) {
	if reason == idleRespawnDrainReason {
		return true, true
	}
	if reason != drainIdle || session.DispositionOfInfo(info).IdleStop {
		return true, false
	}
	policy := w.SleepPolicies[info.ID]
	if policy.Class == config.SessionSleepNonInteractive {
		return true, false
	}
	return policy.Capability == runtime.SessionSleepCapabilityFull, true
}

// armDrainBegin is A20's begin: an alive row the allocation sleeps or
// drains, that claims its runtime and has no stop request, gets one by CAS
// (v5 D1). An undesired row (orphaned, suspended) with open or in-progress
// assigned work stays open (SESS-074), and one woken within INC-003's grace
// of now, in either direction, waits for its end unless an operator
// suspended it (v5.4); a last_woke_at a grace or more ahead gets none, as
// legacy's skew guard (mc-qfgfd). An unexpired timer hold or quarantine is
// not operator-dormant (D1 rule 2), so a begin proceeds where legacy
// drains; a user-hold row is, and C6b2's direct stop owns it. An idle begin
// passes SESS-621's gate and writes the idle latch, the row's sleep policy
// fingerprint (D2). A requested row is signaled (armSignal); an acked one
// is A11's, and a signaled one A4's. A woken row's leftover
// idle-stop-pending mark is cleared (SESS-616).
func armDrainBegin(r *rowFacts) (intent, bool) {
	if req, ok := activeStop(r.row); ok {
		if req.Phase != stopRequested {
			return intent{Reason: decideStopActive}, false
		}
		return armSignal(r, req)
	}
	e, info := r.entry, r.row.Info
	// An operator's suspend is the operator's own stop (legacy's user-hold
	// drain, C6a2's operator-suspend skip); an operator's kill in flight is
	// too, and its fence writes the row asleep, which claims no live runtime.
	// No other hold (a wait, a timer: OperatorDormant) stops a live runtime
	// here.
	if !r.drainableLiveness() || !sessionBeadClaimsLiveRuntime(info) || session.DispositionOfInfo(info).KeepsSlot() {
		return intent{}, false
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	if respawnEligible(r) && idleRespawnAttemptAvailable(info, e.AssignedWork.BeadID) {
		return r.idleRespawnBegin(basis)
	}
	if e.Desired == desireWake && session.DispositionOfInfo(info).IdleStop {
		// The idle stop's end is the disposition's IdleStopEnd cell.
		class := session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvIdleStopEnd}, r.w.Now)
		return intent{Kind: intentRowHeal, Reason: decideIdleStopClear, Basis: basis, Patch: session.MetadataPatch{}, Class: class}, true
	}
	reason := drainReasonOf(r)
	allowed, _ := idleProof(r.w, info, reason)
	switch {
	case reason == "":
		return intent{}, false
	case e.Desired == desireDrain && e.OpenWork != nil:
		return intent{Reason: decideDrainWorkKept}, true
	case reason == reasonNoWake && workKeepsNoWakeRow(r):
		// A Sleep row (a live quarantined one) with assigned work, claimed or
		// not: legacy's assigned-work lens cancels its begin in the same tick
		// (DRAIN-046), so a session it keeps running is never drained (ruling
		// at the C6a2 review: match legacy).
		return intent{Reason: decideDrainWorkKept}, true
	case e.Desired == desireDrain && wakeGracePreservesUndesiredRow(info, r.w.Now) && r.w.OperatorSuspend[r.k] == "":
		woke, _ := parseRFC3339Metadata(info.LastWokeAt)
		r.after(requeueUntil(r.w.Now, woke.Add(wakeUndesiredGrace)))
		return intent{Reason: decideWakeGrace}, true
	case !allowed:
		return intent{Reason: decideIdleUnprovable}, true
	}
	// A fresh begin (idle, no-wake-reason) rests on the runtime: its effect
	// reads it fresh and decides again (freshOwnAlive), so the intent records
	// that rest, which admission requires of a fresh kind.
	kind := drainKind(reason, false)
	if kind == intentDrainBeginFresh && !r.freshOwnAlive() {
		return intent{}, false
	}
	patch := stopBeginPatch(r.row, reason, r.w.Now)
	if reason == drainIdle {
		patch["sleep_policy_fingerprint"] = r.w.SleepPolicies[info.ID].Fingerprint
	}
	return intent{Kind: kind, Reason: decideDrainBegin + reason, Basis: basis, Patch: patch}, true
}

// respawnEligible is v5 D2's idle-respawn eligibility (SESS-614; legacy's
// idleRespawnEligible with beginIdleRespawnDrainIfIdle's gates): a
// pool-managed, unnamed row on an interactive Full-capability policy, awake
// only for ready, unclaimed assigned work, detached past its sleep time. The
// begin also requires the row alive (A20's), and the attempt budget is the
// begin's alone (idleRespawnAttemptAvailable), so a begun drain stays
// authorized. A row whose runtime died is A18's start, whose PreWake clears
// the request.
func respawnEligible(r *rowFacts) bool {
	e, info := r.entry, r.row.Info
	policy, ok := r.w.SleepPolicies[info.ID]
	switch {
	case e == nil || e.Desired != desireWake || e.Reason != reasonAssignedWork || len(e.WakeReasons) != 1 || e.WakeReasons[0] != WakeWork:
		return false
	case e.AssignedWork == nil || e.AssignedWork.Claimed || strings.TrimSpace(e.AssignedWork.BeadID) == "":
		return false
	case !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info):
		return false
	case !ok || policy.Duration <= 0 || policy.Class == config.SessionSleepNonInteractive || policy.Capability != runtime.SessionSleepCapabilityFull:
		return false
	}
	detached, err := time.Parse(time.RFC3339, strings.TrimSpace(info.DetachedAt))
	return err == nil && !detached.IsZero() && r.w.Now.Sub(detached) >= policy.Duration
}

// idleRespawnBegin is the idle-respawn drain's begin (C6d): the stop
// request with the attempt for the assigned bead in one CAS, by the fresh
// begin, which proves the agent idle (idleProof). A pending idle stop ends
// in the same write (the IdleStopEnd cell), as legacy's begin does.
func (r *rowFacts) idleRespawnBegin(basis rowBasis) (intent, bool) {
	if !r.freshOwnAlive() {
		return intent{}, false
	}
	info := r.row.Info
	patch := stopBeginPatch(r.row, idleRespawnDrainReason, r.w.Now)
	patch[idleRespawnAttemptsMetadataKey] = "1"
	patch[idleRespawnBeadIDMetadataKey] = strings.TrimSpace(r.entry.AssignedWork.BeadID)
	it := intent{Kind: drainKind(idleRespawnDrainReason, false), Reason: decideDrainBegin + idleRespawnDrainReason, Basis: basis, Patch: patch}
	if session.DispositionOfInfo(info).IdleStop {
		it.Class = session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvIdleStopEnd}, r.w.Now)
	}
	return it, true
}
