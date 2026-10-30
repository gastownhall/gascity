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
	// ContentGateStopRequested is the v2 controller's stop request at the
	// row's generation (ReadStopRequest): a drain v2 began, which its signal
	// later marks draining and its finalize ends. The text would die with
	// the runtime the drain stops.
	ContentGateStopRequested = "stop_requested"
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
	// audience is a row in drain, so it is the one content "stop_requested"
	// and "draining" admit (I7's exemption, permanent). The caller classifies
	// the op; nothing else may be passed as this.
	GateContentDrainNotice
)

// ContentGate reports why content may not start a delivery into info's
// session now, or "" when the row allows it. stop is the row's stop request
// as ReadStopRequest read it in the same read as info, which carries no
// field for its keys; running is whether its runtime is running, as observed
// by the caller; content is what would start. It reads only the row, so it
// is pure: a generation mismatch between the row and the live runtime, which
// needs a runtime read, is the caller's to check. The first matching reason
// wins, in the order the constants list them.
//
// I7 has one exemption (ga-iw4nv1): "stop_requested" and "draining", a
// drain's two phases, do not hold GateContentDrainNotice, the drain
// protocol's own notice to the row being drained, so whether the notice is
// admitted does not depend on how the stop request was read. The exemption
// is permanent, not a transition allowance: the notice asks the agent being
// drained to acknowledge its drain, so holding it from a stop-requested row
// would hold the very request the drain waits on. Every other reason still
// holds it, in the same order, and both hold all other content. The
// exemption reaches only a caller that asks with GateContentDrainNotice (the
// conversation lane's executors, which report it as Probe.ContentAdmit); a
// caller that asks with GateContentAny alone (the prompt hook's in-band
// pass) holds the notice like any content.
func ContentGate(info Info, stop StopRequest, running bool, now time.Time, content GateContent) string {
	state := State(strings.TrimSpace(info.MetadataState))
	drainNotice := content == GateContentDrainNotice
	switch {
	case IsKillPendingInfo(info, now):
		return ContentGateKillPending
	case HoldVerdictInfo(info, running, now):
		return ContentGateHeld
	case stop.Requested() && !drainNotice:
		return ContentGateStopRequested
	case state == StateDraining && !drainNotice:
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
