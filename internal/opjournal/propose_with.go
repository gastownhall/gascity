package opjournal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Companion is a row write an op must not be durable without: a Stop's
// content hold rides its interrupt (DES-O1 §10.5). Tag names the companion
// and is part of the op's request digest. Item is the write as one
// guarded-batch item; Write is the same write on its own. All three are
// required, so the producer need not know which write the store takes.
type Companion struct {
	Tag   string
	Item  beads.GuardedItem
	Write func() error
}

// ProposeWith is Propose with a companion row write that lands no later than
// the op. On a lane store (NewLaneStore) the op's pinned create and the
// companion go in one guarded batch, all or nothing; on any other store the
// companion is written first and the op created after it, so a failure
// leaves at most the companion, never the op alone.
//
// The companion's Tag is part of the request: the op's digest covers it, so
// a key already proposed without a companion (Propose), or with another
// tag, is a *KeyReuseError and writes nothing, and Propose on a key proposed
// here is one too. A proposal that finds the same request stored, a replay
// or a lost race, writes no companion: the stored op was proposed with this
// companion, which landed no later than it, and a later write (a resume's
// clear) must stand. So a producer that must write the companion again, a
// Stop after a resume cleared the last Stop's hold, needs a fresh key.
// Refused requests and companions write nothing. ctx bounds the guarded
// batch.
func (s *Store) ProposeWith(ctx context.Context, req Request, with Companion) (Op, bool, error) {
	op, err := s.Preview(req)
	if err != nil {
		return Op{}, false, err
	}
	if err := validCompanion(op.ID, with); err != nil {
		return Op{}, false, err
	}
	op.PayloadDigest = companionDigest(op, with.Tag)
	if !req.MintedKey {
		existing, getErr := s.store.Get(op.ID)
		switch {
		case getErr == nil:
			return s.resolveExisting(op, existing)
		case !errors.Is(getErr, beads.ErrNotFound):
			return Op{}, false, fmt.Errorf("reading op %s before creating it: %w", op.ID, getErr)
		}
	}
	if s.batcher == nil {
		if err := with.Write(); err != nil {
			return Op{}, false, fmt.Errorf("writing the companion of op %s: %w", op.ID, err)
		}
		return s.create(op)
	}
	b, err := EncodeOp(op)
	if err != nil {
		return Op{}, false, err
	}
	res, class, applyErr := s.apply(ctx, "opjournal: propose "+string(op.Kind), []beads.GuardedItem{{Create: &b}, with.Item})
	switch class {
	case batchOK:
		op.Revision = res.Revisions[op.ID]
		return op, true, nil
	case batchRefused:
		var item *beads.BatchItemError
		if errors.As(applyErr, &item) && item.Index == 0 && errors.Is(applyErr, beads.ErrAlreadyExists) {
			// A concurrent proposer won; its batch carried its companion.
			return s.proposedElsewhere(op, applyErr)
		}
		return Op{}, false, fmt.Errorf("proposing op %s with its companion: %w", op.ID, applyErr)
	case batchUnknown:
		return s.proposedElsewhere(op, applyErr)
	default:
		return Op{}, false, fmt.Errorf("proposing op %s with its companion: %w", op.ID, applyErr)
	}
}

// proposedElsewhere answers a batch that lost to a stored op or may have
// committed: the op as stored, or batchErr when no op is stored.
func (s *Store) proposedElsewhere(op Op, batchErr error) (Op, bool, error) {
	existing, getErr := s.store.Get(op.ID)
	switch {
	case getErr == nil:
		return s.resolveExisting(op, existing)
	case errors.Is(getErr, beads.ErrNotFound):
		if strings.Contains(batchErr.Error(), "invalid issue type") {
			return Op{}, false, errors.Join(ErrBeadTypeUnregistered, batchErr)
		}
		return Op{}, false, fmt.Errorf("proposing op %s with its companion: %w", op.ID, batchErr)
	default:
		return Op{}, false, errors.Join(fmt.Errorf("proposing op %s with its companion: %w", op.ID, batchErr),
			fmt.Errorf("reading op %s after a failed batch: %w", op.ID, getErr))
	}
}

// validCompanion refuses a companion ProposeWith cannot write both ways.
func validCompanion(opID string, with Companion) error {
	item := with.Item
	switch {
	case with.Tag == "" || strings.TrimSpace(with.Tag) != with.Tag:
		return invalidf("op %s: a companion needs a tag without surrounding space", opID)
	case (item.Create == nil) == (item.Update == nil):
		return invalidf("op %s: a companion is exactly one create or update", opID)
	case with.Write == nil:
		return invalidf("op %s: a companion needs its write on its own", opID)
	case item.Create != nil && item.Create.ID == opID, item.Update != nil && item.Update.ID == opID:
		return invalidf("op %s: a companion cannot write the op's own row", opID)
	}
	return nil
}
