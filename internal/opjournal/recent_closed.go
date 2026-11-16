package opjournal

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RecentClosed returns conversation conv's newest limit closed lane ops of
// kind, newest first: one capped read keyed by the conversation's label, the
// closed status, the kind and the lane executor, as InterruptCredits reads
// its rows. It is a history window over creation: a conversation with more
// than limit later-created closed lane ops of kind shows none older, even
// one that closed after them. A row that does not decode is returned in
// quarantined, as ListOpenTolerant returns an open one, and the rest are
// served.
func (s *Store) RecentClosed(conv string, kind Kind, limit int) ([]Op, []QuarantinedRow, error) {
	if err := validateConversation(conv); err != nil {
		return nil, nil, err
	}
	if _, err := Lookup(kind); err != nil {
		return nil, nil, fmt.Errorf("listing closed ops of conversation %s: %w", conv, err)
	}
	if limit <= 0 {
		return nil, nil, fmt.Errorf("listing closed %s ops of conversation %s: limit must be positive", kind, conv)
	}
	rows, err := s.store.List(beads.ListQuery{
		Label: ConversationLabel(conv),
		Metadata: map[string]string{
			keyStatus: string(StatusClosed), keyKind: string(kind), keyExecutor: string(ExecutorLane),
		},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		Limit:         limit,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing closed %s ops of conversation %s: %w", kind, conv, err)
	}
	var out []Op
	var quarantined []QuarantinedRow
	for _, b := range rows {
		op, err := DecodeOp(b)
		switch {
		case err != nil:
			quarantined = append(quarantined, QuarantinedRow{ID: b.ID, Err: err})
		case op.Status == StatusClosed && op.Conversation == conv && op.Kind == kind && op.Executor == ExecutorLane:
			out = append(out, op)
		}
	}
	return out, quarantined, nil
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
