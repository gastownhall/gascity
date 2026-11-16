package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	_ GuardedBatcher               = (*SQLiteStore)(nil)
	_ GuardedBatcherHandleProvider = (*SQLiteStore)(nil)
)

// GuardedBatcherHandle reports guarded batches only for a writable layout with
// the revision column: a legacy layout cannot guard an update at all, and a
// read-only open cannot write, so discovery answers no for both rather than
// handing out a batcher that always refuses.
func (s *SQLiteStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	if s == nil || s.readOnly || !s.hasRevisionColumn {
		return nil, false
	}
	return s, true
}

// ApplyGuarded applies b in one write transaction. The write connection begins
// every transaction with BEGIN IMMEDIATE, so the write lock is held from the
// first guard read to the commit, and an item that refuses rolls back every
// item before it. A busy database re-runs the whole transaction from fresh
// reads (retryOnBusy).
func (s *SQLiteStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	if err := s.ensureOpen(); err != nil {
		return GuardedResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return GuardedResult{}, err
	}
	if err := validateGuardedBatch(b); err != nil {
		return GuardedResult{}, err
	}
	if s.readOnly {
		return GuardedResult{}, fmt.Errorf("sqlite store is open read-only: %w", ErrGuardedBatchUnsupported)
	}
	if !s.hasRevisionColumn {
		return GuardedResult{}, fmt.Errorf("sqlite schema lacks the revision column: %w", ErrGuardedBatchUnsupported)
	}
	if err := checkGuardedCreateNamespaces("sqlite guarded batch", b, s.reservedPrefixes); err != nil {
		return GuardedResult{}, err
	}
	for _, it := range b.Items {
		if it.Create != nil {
			noteSessionCreate(*it.Create)
		}
	}
	var res GuardedResult
	err := retryOnBusy(func() error {
		var err error
		res, err = s.applyGuardedOnce(ctx, b)
		return err
	})
	if err != nil {
		return GuardedResult{}, err
	}
	return res, nil
}

// applyGuardedOnce is one transaction attempt.
func (s *SQLiteStore) applyGuardedOnce(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GuardedResult{}, fmt.Errorf("sqlite guarded batch: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for idx, it := range b.Items {
		if it.Create != nil {
			err = s.guardedCreateTx(ctx, tx, idx, *it.Create)
		} else {
			err = s.guardedUpdateTx(ctx, tx, idx, *it.Update)
		}
		if err != nil {
			return GuardedResult{}, err
		}
	}
	revisions := make(map[string]int64, len(b.Items))
	for _, it := range b.Items {
		stored, err := s.getTx(ctx, tx, it.id())
		if err != nil {
			return GuardedResult{}, fmt.Errorf("sqlite guarded batch: reading %q back: %w", it.id(), err)
		}
		revisions[stored.ID] = stored.Revision
	}
	if err := tx.Commit(); err != nil {
		return GuardedResult{}, fmt.Errorf("sqlite guarded batch: commit: %w", err)
	}
	return GuardedResult{Revisions: revisions}, nil
}

func (s *SQLiteStore) guardedCreateTx(ctx context.Context, tx *sql.Tx, idx int, b Bead) error {
	exists, err := s.idExistsTx(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	if exists {
		return guardedExistsError(idx, b.ID, nil)
	}
	stored, err := s.normalizeCreate(b)
	if err != nil {
		return err
	}
	guardedCreateCloseReason(&stored)
	if err := s.clearClaimFenceTx(ctx, tx, stored.ID); err != nil {
		return err
	}
	return s.upsertBeadTx(ctx, tx, stored)
}

func (s *SQLiteStore) guardedUpdateTx(ctx context.Context, tx *sql.Tx, idx int, u GuardedUpdate) error {
	before, err := s.getTx(ctx, tx, u.ID)
	if errors.Is(err, ErrNotFound) {
		return guardedNotFoundError(idx, u.ID, err)
	}
	if err != nil {
		return err
	}
	if u.ExpectedRevision != nil && before.Revision != *u.ExpectedRevision {
		return guardedPreconditionError(idx, u.ID, *u.ExpectedRevision, before.Revision, "")
	}
	next := applySQLiteUpdateOpts(before, u.Opts)
	next.UpdatedAt = time.Now()
	if err := s.upsertBeadTx(ctx, tx, next); err != nil {
		return err
	}
	after, err := s.getTx(ctx, tx, u.ID)
	if err != nil {
		return err
	}
	return s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, before, &after)
}
