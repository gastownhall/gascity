package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// A4, the stop verb's arm (CONTRACT v5 D3; C6b2). It comes after A3's
// rekey, so a StaleSelf runtime is re-keyed before it is stopped, and before
// A5's unknown state, so a stop-pending row (legacy's state=draining) is
// never swallowed as one (DS F1).

// Stop reasons. A signaled request's carries its drain reason after the
// colon.
const (
	decideStopVerb     = "stop:"
	decideDormantStop  = "stop-operator-dormant"
	decideStopFinalize = "stop-finalize:" // the runtime reads gone: confirm and finalize
)

// armStop is A4. A signaled stop request (activeStop, D1) gets the stop
// verb, whatever its runtime's liveness reads: for gone, the verb confirms
// and finalizes without a stop (intent.Finalize). An operator-dormant row
// (suspended, killed, or sleep_intent=user-hold) whose own runtime is still
// alive gets the verb directly (the C6a ruling, v5.5): an operator's suspend
// stops the runtime even while the seat holds work, and its finalize is
// legacy's drain completion through the Sleep cell, which keeps the hold
// (J42 B/C), never a close. Both rest on the runtime, which the effect reads
// fresh. TODO(D8): when resume_pending_at lands, the dormant branch skips a
// row under an unexpired resume fence (D8 rule 2).
func armStop(r *rowFacts) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	if req, ok := activeStop(r.row); ok && req.Phase == stopSignaled {
		it := intent{Kind: intentStop, Reason: decideStopVerb + req.Reason, Basis: basis}
		if r.freshGone() {
			it.Reason, it.Finalize = decideStopFinalize+req.Reason, true
		}
		return it, true
	}
	if !operatorDormantStop(r.row.Info, r.w.Now) || !r.freshOwnAlive() {
		return intent{}, false
	}
	return intent{Kind: intentStop, Reason: decideDormantStop, Basis: basis}, true
}

// operatorDormantStop reports a row an operator made dormant whose runtime
// the controller stops directly: suspended under a hold, killed (a kill
// whose own stop did not finish), or holding the user-hold intent of a
// managed suspend. A soft suspend is no hold (CONTRACT v5.9u): A6's awake
// heal reads its live row awake, as legacy's state heal does.
func operatorDormantStop(info session.Info, now time.Time) bool {
	switch {
	case session.State(strings.TrimSpace(info.MetadataState)) == session.StateSuspended:
		return !softSuspended(info, now)
	case session.SleepReason(strings.TrimSpace(info.SleepReason)) == session.SleepReasonKilled:
		return true
	}
	return session.DispositionOfInfo(info).KeepsSlot()
}

// softSuspended reports state=suspended under no hold: a sweep's soft
// suspend, with no operator suspend, kill, wait or live timer beside it
// (Disposition.OperatorDormant). A city stop's sleep_reason is no hold
// either, as RefusesPreWake reads it. CONTRACT v5.9u: it is no hold, and
// legacy's state heal reads it awake or asleep by its runtime.
func softSuspended(info session.Info, now time.Time) bool {
	d := session.DispositionOfInfo(info)
	if d.Reason == session.SleepReasonCityStop {
		d.Reason = ""
	}
	return session.State(strings.TrimSpace(info.MetadataState)) == session.StateSuspended && !d.OperatorDormant(now)
}
