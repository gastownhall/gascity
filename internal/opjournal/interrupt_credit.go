package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// interruptCreditLookback bounds InterruptCredits' read: the newest interrupt
// ops of a conversation. A turn's end is folded soon after the interrupt
// that caused it, so its op is among the newest.
const interruptCreditLookback = 16

// InterruptCredits returns, newest first, the turns gc interrupt ops ran on
// in generation gen of conversation conv, as the refs FoldTurn credits a
// turn's end to (X11 §5.4, D8). It reads the op rows, so the credit survives
// the lane worker that ran the interrupt and a controller restart. One keyed
// read of at most interruptCreditLookback rows, closed ones included.
//
// An op counts once its effect may have reached the runtime: executing or
// settling, or closed succeeded, failed{no_boundary} or
// failed{deadline_in_flight}. Its turn is Result.Turn, else its scope turn;
// an op with neither names no turn and is left out. A row that does not
// decode is left out: the lane's pass quarantines it where an operator sees
// it.
func (s *Store) InterruptCredits(conv string, gen int64) ([]InterruptRef, error) {
	if err := validateConversation(conv); err != nil {
		return nil, err
	}
	rows, err := s.store.List(beads.ListQuery{
		Label:         ConversationLabel(conv),
		Metadata:      map[string]string{keyKind: string(KindInterrupt)},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		Limit:         interruptCreditLookback,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing interrupt ops of conversation %s: %w", conv, err)
	}
	var refs []InterruptRef
	for _, b := range rows {
		op, err := DecodeOp(b)
		if err != nil || op.Conversation != conv || op.Kind != KindInterrupt || op.Scope.Generation != gen {
			continue
		}
		turn := op.Outcome.Result.Turn
		if turn == "" {
			turn = op.Scope.Turn
		}
		if turn == "" || !interruptRan(op) {
			continue
		}
		refs = append(refs, InterruptRef{Op: op.ID, Turn: turn})
	}
	return refs, nil
}

// interruptRan reports whether op's interrupt effect may have reached the
// runtime.
func interruptRan(op Op) bool {
	switch op.Status {
	case StatusExecuting, StatusSettling:
		return true
	case StatusClosed:
		arm, reason := op.Outcome.Effective()
		return arm == ArmSucceeded || (arm == ArmFailed && (reason == ReasonNoBoundary || reason == ReasonDeadlineInFlight))
	default:
		return false
	}
}
