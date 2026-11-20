package opjournal

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// CancelProposal reserves a lane message's SAME pinned request as canceled
// before its first admission. It writes a closed op, never a runnable proposal.
// A concurrent Propose either sees that tombstone or wins first, in which case
// the existing Cancel protocol and actual terminal outcome decide the race.
func (s *Store) CancelProposal(ctx context.Context, req Request, by, reason string) (CancelResult, Op, error) {
	if req.Kind != KindMessage || req.Executor != ExecutorLane {
		return "", Op{}, invalidf("cancel proposal requires a lane message")
	}
	if err := validateText("cancel by", by, maxPrincipalLen); err != nil {
		return "", Op{}, err
	}
	if err := validateOptionalText("cancel reason", reason); err != nil {
		return "", Op{}, err
	}
	if err := errors.Join(validateUpdateText("cancel by", by), validateUpdateText("cancel reason", reason)); err != nil {
		return "", Op{}, err
	}
	want, err := s.Preview(req)
	if err != nil {
		return "", Op{}, err
	}
	want.Cancel = Cancel{By: by, At: utc(s.clk.Now()), Reason: reason}
	want = closed(want, Outcome{Arm: ArmCanceled}, by, s.clk)
	// Read first as Propose does; changed requests fail without writing anything.
	if existing, err := s.store.Get(want.ID); err == nil {
		got, _, err := s.resolveExisting(want, existing)
		if err != nil {
			return "", Op{}, err
		}
		return s.Cancel(got.ID, by, reason)
	} else if !errors.Is(err, beads.ErrNotFound) {
		return "", Op{}, err
	}
	row, err := EncodeOp(want)
	if err != nil {
		return "", Op{}, err
	}
	_, _, writeErr := s.apply(ctx, "reserve canceled public message proposal", []beads.GuardedItem{{Create: &row}})
	// This exact guarded create is the tombstone boundary. Unknown acknowledgments
	// are resolved by the pinned ID; no retry ever substitutes a fresh operation.
	existing, readErr := s.store.Get(want.ID)
	if readErr != nil {
		if writeErr != nil {
			return "", Op{}, errors.Join(fmt.Errorf("reserving canceled proposal: %w", writeErr), readErr)
		}
		return "", Op{}, fmt.Errorf("reserved canceled proposal missing on readback: %w", readErr)
	}
	got, _, err := s.resolveExisting(want, existing)
	if err != nil {
		return "", Op{}, err
	}
	if writeErr == nil {
		// The metadata is already terminal even if physical-row repair fails.
		if err := s.closeBead(got.ID); err != nil {
			return "", got, err
		}
		return CancelClosed, got, nil
	}
	return s.Cancel(got.ID, by, reason)
}
