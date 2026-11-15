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
// and is part of the op's request digest. Item builds the write as one
// guarded-batch item; Write is the same write on its own. All three are
// required, so the producer need not know which write the store takes.
// ProposeWith calls Item, then Write on a store without batches, only once
// its read finds no stored op, so a builder that reads and refuses the row
// (session.Store.ContentHoldItem refuses a closed one) never stops a replay.
type Companion struct {
	Tag   string
	Item  func() (beads.GuardedItem, error)
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
// tag, is a *KeyReuseError, and Propose on a key proposed here is one too.
// When the read finds that key nothing is written; on a store without
// batches a conflicting op that lands between the read and the create is
// the same error after the companion is written (at most the companion). A
// proposal whose read finds the same request stored, a replay, neither
// builds nor writes the companion, and on a lane store neither does one that
// loses the create to it, whose batch is refused whole: the stored op was
// proposed with this companion, which landed no later than it, and a later
// write (a resume's clear, a close) must stand. On a store without batches
// the companion is written before the create, so a proposal that loses the
// create to the same request, or retries a MintedKey (which reads nothing
// first), has written it when it returns the stored op as a replay, over
// any clear that landed after the winner's write. So a producer that must
// write the companion again, a Stop after a resume cleared the last Stop's
// hold, needs a fresh key, and one whose lost race must write nothing opens
// its store with NewLaneStore. Refused requests and companions, including an
// Item that returns an error, write nothing. ctx bounds the guarded batch.
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
	item, err := with.Item()
	if err != nil {
		return Op{}, false, fmt.Errorf("building the companion of op %s: %w", op.ID, err)
	}
	if err := validCompanionItem(op.ID, item); err != nil {
		return Op{}, false, err
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
	res, class, applyErr := s.apply(ctx, "opjournal: propose "+string(op.Kind), []beads.GuardedItem{{Create: &b}, item})
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
	switch {
	case with.Tag == "" || strings.TrimSpace(with.Tag) != with.Tag:
		return invalidf("op %s: a companion needs a tag without surrounding space", opID)
	case with.Item == nil:
		return invalidf("op %s: a companion needs its batch item", opID)
	case with.Write == nil:
		return invalidf("op %s: a companion needs its write on its own", opID)
	}
	return nil
}

// validCompanionItem refuses a built companion item a batch cannot carry
// beside the op.
func validCompanionItem(opID string, item beads.GuardedItem) error {
	switch {
	case (item.Create == nil) == (item.Update == nil):
		return invalidf("op %s: a companion is exactly one create or update", opID)
	case item.Create != nil && item.Create.ID == opID, item.Update != nil && item.Update.ID == opID:
		return invalidf("op %s: a companion cannot write the op's own row", opID)
	}
	return nil
}
