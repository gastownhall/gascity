package session

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TurnReplacement is what deliver-now (interrupt_now) does to a session's
// running turn before it delivers the new message
// (engdocs/design/x11-turn-identity-coverage.md D11). The conversation lane
// reads it to know when replacing a turn needs a restart, or cannot be done.
type TurnReplacement string

// The turn replacements.
const (
	// TurnReplacementNone means interrupt_now is refused: the running turn
	// cannot be replaced.
	TurnReplacementNone TurnReplacement = "none"
	// TurnReplacementInterrupt interrupts the turn, waits for it to settle,
	// then delivers.
	TurnReplacementInterrupt TurnReplacement = "interrupt"
	// TurnReplacementInterruptReset also discards the interrupted turn
	// before it delivers.
	TurnReplacementInterruptReset TurnReplacement = "interrupt_reset"
	// TurnReplacementRestart stops the agent and restarts it with the
	// message, discarding the pending turn.
	TurnReplacementRestart TurnReplacement = "restart"
)

// turnReplacementFor reports what interrupt_now does to the running turn of
// the session info describes, when the session's runtime cancels turns in
// mode and resets reports that its routed backend discards an interrupted
// turn ([runtime.InterruptedTurnResetsFor]). It decides with the predicates
// submit and interruptAndSubmitLocked run on, over the persisted metadata
// they read (info's raw provider and transport mirrors), so the answer is
// interrupt_now's own.
func turnReplacementFor(info Info, mode runtime.TurnInterruptMode, resets bool) TurnReplacement {
	b := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": info.BuiltinAncestor,
		"provider_kind":    info.ProviderKind,
		"provider":         info.Provider,
		"transport":        info.TransportMetadata,
	}}
	switch {
	case !supportsInterruptNowForMetadata(b.Metadata), refusesTurnInterrupt(b, mode):
		return TurnReplacementNone
	case requiresHardRestartInterrupt(b) && mode != runtime.TurnInterruptProtocol:
		return TurnReplacementRestart
	case requiresInterruptedTurnReset(b) && resets:
		return TurnReplacementInterruptReset
	default:
		return TurnReplacementInterrupt
	}
}
