package opjournal

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RecentClosed returns conversation conv's newest limit closed lane ops,
// newest first: one capped read keyed by the conversation's label and the
// closed status, as InterruptCredits reads its rows. It is a history window: a conversation with more
// than limit later closed ops shows none older. A row that
// does not decode is left out, as OpenByCausation leaves it; the lane's pass
// quarantines an open one where an operator sees it.
func (s *Store) RecentClosed(conv string, limit int) ([]Op, error) {
	if err := validateConversation(conv); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("listing closed ops of conversation %s: limit must be positive", conv)
	}
	rows, err := s.store.List(beads.ListQuery{
		Label:         ConversationLabel(conv),
		Metadata:      map[string]string{keyStatus: string(StatusClosed)},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		Limit:         limit,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing closed ops of conversation %s: %w", conv, err)
	}
	var out []Op
	for _, b := range rows {
		op, err := DecodeOp(b)
		if err != nil || op.Status != StatusClosed || op.Conversation != conv || op.Executor != ExecutorLane {
			continue
		}
		out = append(out, op)
	}
	return out, nil
}

// ClosedAt is when op closed: its last transition to closed, the zero time
// for an op that is open or records none.
func (op Op) ClosedAt() time.Time {
	for i := len(op.Transitions) - 1; i >= 0; i-- {
		if op.Transitions[i].Status == StatusClosed {
			return op.Transitions[i].At
		}
	}
	return time.Time{}
}
