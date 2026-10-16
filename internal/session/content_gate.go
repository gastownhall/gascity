package session

import (
	"strings"
	"time"
)

// Content gate reasons: why a conversation's row says content may not start
// a delivery now (DES-O1 I7). Each is a token a lane records as its wait
// detail.
const (
	// ContentGateKillPending is a `gc session kill` mid-teardown
	// (IsKillPendingInfo): the text would die with the runtime.
	ContentGateKillPending = "kill_pending"
	// ContentGateHeld is a hold that blocks a consume (HoldVerdictInfo).
	ContentGateHeld = "held"
	// ContentGateDraining is a row in drain.
	ContentGateDraining = "draining"
	// ContentGateStopPending is a pending stop of the runtime (the
	// disposition's idle stop, DispositionOfInfo).
	ContentGateStopPending = "stop_pending"
	// ContentGateRestartPending is a restart or a continuation reset
	// recorded but not yet carried out: the live runtime is the outgoing
	// incarnation.
	ContentGateRestartPending = "restart_pending"
	// ContentGateStarting is a runtime still being created or started.
	ContentGateStarting = "starting"
)

// ContentGate reports why content may not start a delivery into info's
// session now, or "" when the row allows it. running is whether its runtime
// is running, as observed by the caller. It reads only the row, so it is
// pure: a generation mismatch between the row and the live runtime, which
// needs a runtime read, is the caller's to check. The first matching reason
// wins, in the order the constants list them.
//
// It does not yet see a v2 stop request (the drain_intent_* keys at the
// current generation, which the v2 drain writes before it sets state to
// draining): those keys are the v2 reconciler's own and Info carries no
// field for them. In that window content can start a turn in a session v2 is
// about to drain, so I7 holds here only from state=draining on, until the
// session package owns a "stop requested at generation g" predicate this
// gate can call (ga-f35wnp, due before a lane producer lands).
func ContentGate(info Info, running bool, now time.Time) string {
	state := State(strings.TrimSpace(info.MetadataState))
	switch {
	case IsKillPendingInfo(info, now):
		return ContentGateKillPending
	case HoldVerdictInfo(info, running, now):
		return ContentGateHeld
	case state == StateDraining:
		return ContentGateDraining
	case DispositionOfInfo(info).IdleStop:
		return ContentGateStopPending
	case strings.TrimSpace(info.RestartRequested) != "" || strings.TrimSpace(info.ContinuationResetPending) != "":
		return ContentGateRestartPending
	case state == StateCreating || state == StateStartPending || info.PendingCreateClaim:
		return ContentGateStarting
	}
	return ""
}
