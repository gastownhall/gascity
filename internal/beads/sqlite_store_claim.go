package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claim atomically claims a bead for assignee via a compare-and-swap on the
// assignee field: it succeeds (status -> in_progress, assignee -> caller) only
// when the bead is open/in_progress and currently unassigned, and is idempotent
// when the same assignee already holds it. It returns ok=false (a conflict, not
// an error) when a different assignee already holds the bead or the bead is
// closed, and ErrNotFound when the bead does not exist.
//
// It is the acquire-dual of [SQLiteStore.ReleaseIfCurrent]. Single-winner under
// concurrency is guaranteed by the store's single write connection
// (MaxOpenConns=1): competing claims serialize through one BeginTx, so exactly
// one observes the bead unassigned.
func (s *SQLiteStore) Claim(id, assignee string) (Bead, bool, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, false, err
	}
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return Bead{}, false, fmt.Errorf("claiming bead %q: empty assignee", id)
	}
	var claimed Bead
	var ok bool
	err := retryOnBusy(func() error {
		ok = false
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite claim: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("claiming bead %q: %w", id, ErrNotFound)
			}
			return err
		}
		if b.Status != "open" && b.Status != "in_progress" {
			// Terminal and otherwise non-claimable states are never resurrected.
			return tx.Commit()
		}
		cur := strings.TrimSpace(b.Assignee)
		if cur != "" && cur != assignee {
			// Held by another worker — conflict, not an error.
			return tx.Commit()
		}
		if cur == assignee && b.Status == "in_progress" {
			// Same-owner reclaims are true no-ops: do not consume either a
			// revision or an ownership fence, and return the stored snapshot.
			claimed = cloneBead(b)
			ok = true
			return tx.Commit()
		}
		before := b
		b.Assignee = assignee
		b.Status = "in_progress"
		b.UpdatedAt = time.Now()
		if err := s.upsertBeadTx(ctx, tx, b); err != nil {
			return err
		}
		if err := s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, before, &b); err != nil {
			return err
		}
		b, err = s.getTx(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("claiming bead %q: re-reading claimed row: %w", id, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite claim: commit: %w", err)
		}
		claimed = cloneBead(b)
		ok = true
		return nil
	})
	if err != nil {
		return Bead{}, false, err
	}
	return claimed, ok, nil
}
