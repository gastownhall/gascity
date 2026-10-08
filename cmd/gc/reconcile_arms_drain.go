package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain's policy table (CONTRACT v5 D2) and decideRow's arms A19 (void
// and cancel) and A20 (drain begin). The signal and its graces are C6b1's,
// idle respawn C6d's, and the config-drift begin C7b1's.

// Drain decide reasons. A begin, cancel or void carries the drain reason
// after the colon.
const (
	decideDrainBegin    = "drain-begin:"
	decideDrainCancel   = "drain-cancel:"
	decideDrainVoid     = "drain-void:"
	decideStopResidue   = "drain-void:residue"
	decideStopActive    = "stop-requested"
	decideWakeGrace     = "undesired-wake-grace" // INC-003
	decideDrainWorkKept = "drain-kept:assigned-work"
	decideNoGeneration  = "drain-kept:no-generation"
)

// Drain reasons the allocation does not name.
const (
	drainIdle        = string(session.SleepReasonIdle)
	drainConfigDrift = "config-drift"
	reasonIdleSleep  = "idle-sleep"
	wakeAssignedWork = "assigned-work"
	sleepIntentIdle  = "idle-stop-pending"
	timerBlockerHold = "user_hold"
)

// drainRank orders the reasons whose authorization is "an equal or
// stronger reason": suspended > orphaned > no-wake-reason > idle.
var drainRank = map[string]int{drainSuspended: 4, drainOrphaned: 3, reasonNoWake: 2, drainIdle: 1}

// drainLost is the table's last column: what A19 does once a requested
// drain loses its authorization.
type drainLost uint8

const (
	lostLensOrKeep drainLost = iota + 1 // hold under Keep, otherwise the lens
	lostLens                            // the lens
	lostVoid                            // void (R16: a resume voids a suspended drain)
	lostCancel                          // cancel; the pending lens also applies (config drift)
	lostNone                            // not cancelable
)

// drainPolicy is one row of the table. authorized is the "authorization at
// the signal" column, which A19 checks on every pass; nil is a row a later
// PR completes, which holds meanwhile.
type drainPolicy struct {
	lost       drainLost
	authorized func(r *rowFacts, reason string) bool
}

// drainPolicyOf is reason's row; any other reason is a sleep intent's.
func drainPolicyOf(reason string) drainPolicy {
	switch reason {
	case drainIdle, reasonNoWake:
		return drainPolicy{lostLensOrKeep, authorizedByRank}
	case drainOrphaned:
		return drainPolicy{lostLensOrKeep, func(r *rowFacts, _ string) bool { return r.entry.Desired == desireDrain }}
	case drainSuspended:
		return drainPolicy{lostVoid, func(r *rowFacts, _ string) bool { return r.entry.DrainReason == drainSuspended }}
	case drainConfigDrift:
		return drainPolicy{lostCancel, func(r *rowFacts, _ string) bool { return !driftResolvedOrDeferred(r) }}
	case executionStalledDrainReason:
		return drainPolicy{lostNone, func(*rowFacts, string) bool { return true }}
	case idleRespawnDrainReason:
		return drainPolicy{lostVoid, nil} // C6d: eligibility and its revalidation
	}
	return drainPolicy{lostLens, func(r *rowFacts, reason string) bool { return strings.TrimSpace(r.row.Info.SleepIntent) == reason }}
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
// (DRAIN-047), and a heartbeat hold (SESS-617).
func drainLens(r *rowFacts, reason string) bool {
	e := r.entry
	switch {
	case containsWakeReason(e.WakeReasons, WakePending) && pendingDrainReasonCancelable(reason):
		return true
	case e.Reason == wakeAssignedWork && containsWakeReason(e.WakeReasons, WakeWork) && assignedWorkDrainReasonCancelable(reason):
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
		recentlyDeferredSessionAttachedConfigDrift(r.row.Info, &clock.Fake{Time: r.w.Now}, key)
}

// armDrainVoidCancel is A19. A requested drain that lost its authorization
// takes the table's last column; a config-drift drain also cancels on a
// pending interaction. A request rule 2 ended (a suspended or killed row)
// is voided, both halves. Acked and signaled requests are A11's and A4's.
func armDrainVoidCancel(r *rowFacts) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	req, ok := activeStop(r.row)
	switch {
	case !ok && stopResidue(r.row):
		return intent{Kind: intentDrainVoid, Reason: decideStopResidue, Basis: basis, Patch: stopVoidResiduePatch()}, true
	case !ok || req.Phase != stopRequested:
		return intent{}, false
	}
	p := drainPolicyOf(req.Reason)
	if p.authorized == nil {
		return intent{}, false
	}
	authorized := p.authorized(r, req.Reason)
	kind := ""
	switch p.lost {
	case lostLensOrKeep:
		if !authorized && r.entry.Desired != desireKeep && drainLens(r, req.Reason) {
			kind = intentDrainCancel
		}
	case lostLens:
		if !authorized && drainLens(r, req.Reason) {
			kind = intentDrainCancel
		}
	case lostVoid:
		if !authorized {
			kind = intentDrainVoid
		}
	case lostCancel:
		if !authorized || (containsWakeReason(r.entry.WakeReasons, WakePending) && pendingDrainReasonCancelable(req.Reason)) {
			kind = intentDrainCancel
		}
	}
	if kind == "" {
		return intent{}, false
	}
	reason := decideDrainCancel + req.Reason
	if kind == intentDrainVoid {
		reason = decideDrainVoid + req.Reason
	}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: stopCancelPatch()}, true
}

// armDrainBegin is A20's begin: an alive row the allocation sleeps or
// drains, that claims its runtime and has no stop request, gets one by CAS
// (v5 D1). An undesired row (orphaned, suspended) with assigned work stays
// open (SESS-074), and one woken within INC-003's grace of now, in either
// direction, waits for its end unless an operator suspended it (v5.4). A
// requested or acked row holds for the signal (C6b1, C6c2).
func armDrainBegin(r *rowFacts) (intent, bool) {
	if req, ok := activeStop(r.row); ok {
		return intent{Reason: decideStopActive}, req.Phase != stopSignaled // a signaled one is A4's
	}
	e := r.entry
	if (e.Desired != desireSleep && e.Desired != desireDrain) || e.Liveness != livenessAlive ||
		!sessionBeadClaimsLiveRuntime(r.row.Info) {
		return intent{}, false
	}
	reason := drainReasonOf(r)
	switch {
	case reason == "":
		return intent{}, false
	case e.Desired == desireDrain && e.AssignedWork != nil:
		return intent{Reason: decideDrainWorkKept}, true
	case e.Desired == desireDrain && r.w.OperatorSuspend[r.k] == "" && wakeGracePreservesUndesiredRow(r.row.Info, r.w.Now):
		woke, _ := parseRFC3339Metadata(r.row.Info.LastWokeAt)
		r.after(woke.Add(wakeUndesiredGrace).Sub(r.w.Now))
		return intent{Reason: decideWakeGrace}, true
	case strings.TrimSpace(r.row.Info.Generation) == "":
		return intent{Reason: decideNoGeneration}, true
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: drainKind(reason, false), Reason: decideDrainBegin + reason, Basis: basis, Patch: stopBeginPatch(r.row, reason, r.w.Now)}, true
}
