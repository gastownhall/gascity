package main

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain's policy table (CONTRACT v5 D2) and decideRow's arms A19 (void
// and cancel) and A20 (drain begin). The signal and its graces are C6b1's,
// idle respawn C6d's, and the config-drift begin C7b1's.

// Drain decide reasons. A begin, cancel or void carries the drain reason
// after the colon.
const (
	decideDrainBegin     = "drain-begin:"
	decideDrainCancel    = "drain-cancel:"
	decideDrainVoid      = "drain-void:"
	decideStopResidue    = "drain-void:residue"
	decideStopActive     = "stop-requested"
	decideDrainSignal    = "drain-signal:"
	decideWakeGrace      = "undesired-wake-grace" // INC-003
	decideDrainWorkKept  = "drain-kept:assigned-work"
	decideIdleUnprovable = "drain-kept:idle-unprovable" // SESS-621
	decideIdleStopClear  = "idle-stop-pending-clear"    // SESS-616
)

// Drain reasons and markers the allocation does not name.
const (
	drainIdle        = string(session.SleepReasonIdle)
	drainConfigDrift = "config-drift"
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
		return drainPolicy{} // C6d: eligibility and its revalidation
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
// has none. Legacy's live-claim veto is not ported (PAR-LIVECLAIM).
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

// armSignal is A20's signal (v5 D1, D2; C6b1): requested or acked →
// signaled, by one CAS of legacy's stop-pending projection
// (DrainAckStopPendingPatch), which A4's stop verb then takes. An ack is
// due at once. A requested drain is due once its policy row's grace has
// passed since the drain began; A19, before this arm, has canceled or
// voided one no longer authorized, and a row it holds under Keep, or one
// whose reason's row a later PR completes, holds here too.
// An idle or no-wake-reason signal reads L3 and L4 fresh (C8.9), so it is
// the fresh kind, and rests on the row's own live runtime. The effect
// decides again on the fresh row, so a drain A19 voids or cancels since
// the pass is never signaled (R16).
func armSignal(r *rowFacts, req stopRequest) (intent, bool) {
	hold := intent{Reason: decideStopActive}
	if req.Phase == stopRequested {
		p := drainPolicyOf(req.Reason)
		if p.authorized == nil || (p.keepHolds && r.entry.Desired == desireKeep) {
			return hold, true
		}
		if wait := signalWait(r.w, req); wait > 0 {
			r.after(wait)
			return hold, true
		}
	}
	kind := intentSignal
	if req.Phase == stopRequested {
		kind = drainKind(req.Reason, true)
	}
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
// zero policy. A legacy idle-stop-pending mark is the proof already.
func idleProof(w *World, info session.Info, reason string) (allowed, probe bool) {
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
// fingerprint (D2). A requested or acked row is signaled (armSignal). A
// woken row's leftover idle-stop-pending mark is cleared (SESS-616).
func armDrainBegin(r *rowFacts) (intent, bool) {
	if req, ok := activeStop(r.row); ok {
		if req.Phase == stopSignaled {
			return intent{Reason: decideStopActive}, false // A4's
		}
		// TODO(C7, A12-A16): the ack's fast path sits at A20's position, not
		// the contract's A11; when A12-A16 register, check none pre-empts an
		// acked stop (C6c2 moves it to A11).
		return armSignal(r, req)
	}
	e, info := r.entry, r.row.Info
	// An operator's suspend is the operator's own stop (legacy's user-hold
	// drain, C6a2's operator-suspend skip); an operator's kill in flight is
	// too, and its fence writes the row asleep, which claims no live runtime.
	// No other hold (a wait, a timer: OperatorDormant) stops a live runtime
	// here.
	if e.Liveness != livenessAlive || !sessionBeadClaimsLiveRuntime(info) || session.DispositionOfInfo(info).KeepsSlot() {
		return intent{}, false
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
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
