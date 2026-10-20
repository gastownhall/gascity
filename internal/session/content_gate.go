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

// GateContent is what a lane would start in a session, as far as
// ContentGate tells content apart.
type GateContent int

const (
	// GateContentAny is any content: a prompt, a steer or a notice.
	GateContentAny GateContent = iota
	// GateContentDrainNotice is the drain protocol's notice: the reminder
	// asking a draining session's agent to acknowledge its drain. Its
	// audience is a draining row, so it is the one content "draining" admits
	// (I7's exemption). The caller classifies the op; nothing else may be
	// passed as this.
	GateContentDrainNotice
)

// ContentGate reports why content may not start a delivery into info's
// session now, or "" when the row allows it. running is whether its runtime
// is running, as observed by the caller; content is what would start. It
// reads only the row, so it is pure: a generation mismatch between the row
// and the live runtime, which needs a runtime read, is the caller's to check.
// The first matching reason wins, in the order the constants list them.
//
// I7 has one exemption (ga-iw4nv1): "draining" does not hold
// GateContentDrainNotice, the drain protocol's own notice to the draining
// row. Every other reason still holds it, in the same order, and "draining"
// holds all other content.
//
// It does not yet see a v2 stop request (the drain_intent_* keys at the
// current generation, which the v2 drain writes before it sets state to
// draining): those keys are the v2 reconciler's own and Info carries no
// field for them. In that window content can start a turn in a session v2 is
// about to drain, so I7 holds here only from state=draining on, until the
// session package owns a "stop requested at generation g" predicate this
// gate can call (ga-f35wnp: inert while [ops] authoritative is empty (the
// default), so no producer writes lane ops; the predicate is a hard
// precondition before message joins a default authoritative list or a lane
// producer is switched on).
func ContentGate(info Info, running bool, now time.Time, content GateContent) string {
	state := State(strings.TrimSpace(info.MetadataState))
	switch {
	case IsKillPendingInfo(info, now):
		return ContentGateKillPending
	case HoldVerdictInfo(info, running, now):
		return ContentGateHeld
	case state == StateDraining && content != GateContentDrainNotice:
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
