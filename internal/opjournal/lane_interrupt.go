package opjournal

import "strings"

// The interrupt kind's lane vocabulary (OJ-15; DES-O1 §10.1, X11 §5.4).
const (
	// DegradationUnfencedInterrupt marks an interrupt that could not be
	// confined to the turn and incarnation it was meant for: a
	// session-scoped interrupt, a turn-scoped one below the atomic turn
	// fence, or one sent without the incarnation's input fence (X11 D4).
	// The API's stop writes the same token into the same op table as
	// session.DegradationUnfencedInterrupt, which spells it the same.
	DegradationUnfencedInterrupt = "unfenced_interrupt"
	// DegradationRestartedAfterFailedInterrupt marks an interrupt whose turn
	// was not seen to end and whose on_failure restart replaced the runtime
	// in its place (O1's inline restart). An outcome holds one degradation,
	// and this one takes precedence over DegradationUnfencedInterrupt: the
	// restart ended whatever ran, so which turn the interrupt reached no
	// longer decides what was stopped.
	DegradationRestartedAfterFailedInterrupt = "restarted_after_failed_interrupt"
)

// startByReason is the failed reason of an op whose start_by passed while it
// was not executing: deadline_in_flight once an attempt may have reached the
// agent, provider_unavailable for an interrupt whose last attempt was
// released because the runtime was unavailable (NotDelivered.Unavailable,
// X11 §5.4), else expired_before_start, after any other release included.
func startByReason(op Op) string {
	switch {
	case op.MaybeDelivered > 0:
		return ReasonDeadlineInFlight
	case op.Kind == KindInterrupt && (op.LastError == lastErrorUnavailable || strings.HasPrefix(op.LastError, lastErrorUnavailable+":")):
		return ReasonProviderUnavailable
	default:
		return ReasonExpiredBeforeStart
	}
}
