package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// The drain's policy table (CONTRACT v5 D2) and decideRow's arm A19 (void
// and cancel). A20's begin is C6a2's, the signal and its graces C6b1's,
// idle respawn C6d's, and the config-drift begin C7b1's.

// Drain decide reasons. A cancel or void carries the drain reason after the
// colon.
const (
	decideDrainCancel = "drain-cancel:"
	decideDrainVoid   = "drain-void:"
	decideStopResidue = "drain-void:residue"
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
// begins again under the current reason, with its own fences and grace
// (C6a2).
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
	case drainLens(r, req.Reason):
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
