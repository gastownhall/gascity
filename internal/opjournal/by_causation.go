package opjournal

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// OpenByCausation returns the open lane ops whose causation is causation, of
// any conversation, in creation order: one keyed read of the op rows (label
// gc:op, the causation and executor=lane metadata). A producer that withdraws
// what it proposed for a cause (a mail notice once its mail is read) cancels
// them. A row that does not decode is left out: the lane's pass quarantines
// it where an operator sees it.
func (s *Store) OpenByCausation(causation string) ([]Op, error) {
	if causation == "" {
		return nil, errors.New("listing ops by causation: causation is required")
	}
	rows, err := s.store.List(beads.ListQuery{
		Label:    LabelOp,
		Metadata: map[string]string{keyCausation: causation, keyExecutor: string(ExecutorLane)},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing ops caused by %s: %w", causation, err)
	}
	var out []Op
	for _, b := range rows {
		op, err := DecodeOp(b)
		if err != nil || op.Status == StatusClosed || op.Causation != causation || op.Executor != ExecutorLane {
			continue
		}
		out = append(out, op)
	}
	return out, nil
}
