package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// interruptCreditLookback bounds InterruptCredits' answer: the newest
// interrupt ops of a conversation that ran in the generation asked. A turn's
// end is folded soon after the interrupt that caused it, so its op is among
// them.
const interruptCreditLookback = 16

// interruptCreditPages bounds InterruptCredits' read: it reads at most this
// many pages of interruptCreditLookback interrupt rows, so interrupt ops that
// never ran or ran in another generation hide an older one that ran only
// from behind that many rows.
const interruptCreditPages = 8

// InterruptCredits returns, newest first, the turns gc interrupt ops ran on
// in generation gen of conversation conv, as the refs FoldTurn credits a
// turn's end to (X11 §5.4, D8): at most interruptCreditLookback. It reads the
// op rows, so the credit survives the lane worker that ran the interrupt and
// a controller restart: keyed reads of the conversation's interrupt rows,
// closed ones included, newest first in pages of interruptCreditLookback
// rows (SeekAfter), until it has interruptCreditLookback refs, the rows run
// out or it has read interruptCreditPages pages.
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
	q := beads.ListQuery{
		Label:         ConversationLabel(conv),
		Metadata:      map[string]string{keyKind: string(KindInterrupt)},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		Limit:         interruptCreditLookback,
		TierMode:      beads.TierBoth,
	}
	var refs []InterruptRef
	for page := 1; ; page++ {
		rows, err := s.store.List(q)
		if err != nil {
			return nil, fmt.Errorf("listing interrupt ops of conversation %s: %w", conv, err)
		}
		for _, b := range rows {
			ref, ok := interruptCreditOf(b, conv, gen)
			if !ok {
				continue
			}
			if refs = append(refs, ref); len(refs) == interruptCreditLookback {
				return refs, nil
			}
		}
		if len(rows) < q.Limit || page == interruptCreditPages {
			return refs, nil
		}
		last := rows[len(rows)-1]
		q.SeekAfter = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// interruptCreditOf is the credit row b gives a turn's end in generation gen
// of conversation conv: ok when it is an interrupt op of that generation that
// ran, and names a turn.
func interruptCreditOf(b beads.Bead, conv string, gen int64) (InterruptRef, bool) {
	op, err := DecodeOp(b)
	if err != nil || op.Conversation != conv || op.Kind != KindInterrupt || op.Scope.Generation != gen {
		return InterruptRef{}, false
	}
	turn := op.Outcome.Result.Turn
	if turn == "" {
		turn = op.Scope.Turn
	}
	if turn == "" || !interruptRan(op) {
		return InterruptRef{}, false
	}
	return InterruptRef{Op: op.ID, Turn: turn}, true
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
