package main

// The live executor's deliver-now replacement step (OJ-17; DES-O1 §10.3, X11
// D11): a unit's interrupt goes as OJ-15's interrupt with ForReplacement,
// so the worker runs the session's replacement preparation once the turn
// settles (Gemini's reset, the input clear), and the step then reads the
// session's turns once to tell the lane whether the unit's message may go
// now (opjournal.TurnAfter). The message itself goes through Deliver, typed
// at once (Delivery.Replacement). Nothing here types.

import (
	"context"
	"errors"
	"slices"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// replacementResult is a replacement step's result, from out, the
// interrupt's result under OJ-15's mapping, and err, the worker's answer: a
// settled step carries the session's turns as it left them
// (replacementAfter), and a turn that settled but whose session could not be
// prepared for the message (session.ErrReplacementNotPrepared) is
// failed{replacement_not_prepared}, naming the turn: the interrupt ran, the
// replacement did not complete. Every other answer maps as for a
// standalone interrupt, under which one whose read or dispatch may have run
// (session.ErrInterruptOutcomeUnknown) is Unknown: a step that may have run
// is never sent again.
func (e *conversationLaneExec) replacementResult(ctx context.Context, out opjournal.ControlResult, err error, turn string) opjournal.ControlResult {
	switch {
	case err == nil:
		if s, ok := out.(opjournal.Settled); ok {
			s.After = e.replacementAfter(ctx, turn)
			return s
		}
	case errors.Is(err, session.ErrReplacementNotPrepared):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonReplacementNotPrepared, Turn: turn}
	}
	return out
}

// replacementAfter reads where a settled replacement step left the
// session's turns: one bounded read (interruptRead) of a source that shows
// a turn end (ends at least inferred). A turn running other than replaced
// says another turn runs (Boundary no); none running, or a read lagging on
// the replaced turn, which the worker just saw end, says the boundary is
// reached (Boundary yes, no turn live). With replaced "" (a session-scoped
// stop the executor did not aim) any running turn counts as another: a
// protocol stop returns before its turn ends, and the message must never be
// typed into a running turn. With no source to read, or a read that failed,
// a keystroke runtime's boundary is the session layer's settle waits, as
// interrupt_now trusts them (Boundary yes, liveness unknown), and a protocol
// runtime's is unknown. The replaced turn is known on every path, read or
// not, so the message's link never names it. The executor's own look moves
// to what the step read, so Deliver's link evidence starts from the same
// evidence the lane records.
func (e *conversationLaneExec) replacementAfter(ctx context.Context, replaced string) opjournal.TurnAfter {
	unread := opjournal.TurnAfter{}
	if !e.protocol {
		unread = opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriUnknown}
	}
	if replaced != "" {
		unread.Known = []string{replaced}
	}
	after := unread
	if e.turnHandle != nil && e.traits.readable && e.turnCaps.End.AtLeast(runtime.TurnEndInferred) {
		if hist, err := e.interruptRead(ctx); err == nil {
			look := laneTurnLookOf(hist, nil)
			known := look.known
			if replaced != "" && !slices.Contains(known, replaced) {
				known = append(known, replaced)
			}
			after = opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriNo, Known: known}
			if cur := hist.Current; cur.Active() && (replaced == "" || cur.TurnID != replaced) {
				after = opjournal.TurnAfter{Boundary: opjournal.TriNo, Live: opjournal.TriYes, Turn: cur.TurnID, Known: known}
			}
		}
	}
	known := slices.Clone(e.lastTurn.known)
	for _, t := range after.Known {
		if !slices.Contains(known, t) {
			known = append(known, t)
		}
	}
	e.lastTurn = laneTurnLook{live: after.Live, turn: after.Turn, known: known}
	return after
}
