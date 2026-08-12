package beads

// SQLiteStore's ConditionalWriter implementation: without it the graph
// plane's fenced writes — control epochs, drain reservations, attach fences —
// silently degrade to UNFENCED writes on a routed city, because a store that
// does not implement the capability falls back through the resolver to a
// legacy unconditional write.
//
// Each verb is a transactional read-check-write. The fence is evaluated
// inside the same BeginTx that performs the write, and the write handle is
// capped at one connection (OpenSQLiteStore: db.SetMaxOpenConns(1)), so
// in-process racers serialize through the transaction exactly as
// SQLiteStore.Claim documents; cross-process racers hit WAL's conflict and
// retryOnBusy re-runs from a fresh read. Never a lost update.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var _ ConditionalWriter = (*SQLiteStore)(nil)

// UpdateIfMatch applies opts only when the stored revision matches.
func (s *SQLiteStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if updateOptsEmpty(opts) {
		return ErrEmptyConditionalUpdate
	}
	return s.conditionalWrite(id, expectedRevision, func(ctx context.Context, tx *sql.Tx, b Bead) error {
		next := applySQLiteUpdateOpts(b, opts)
		next.UpdatedAt = time.Now()
		return s.upsertBeadTx(ctx, tx, next)
	})
}

// CloseIfMatch closes the bead only when the stored revision matches.
func (s *SQLiteStore) CloseIfMatch(id string, expectedRevision int64) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return s.conditionalWrite(id, expectedRevision, func(ctx context.Context, tx *sql.Tx, b Bead) error {
		b.Status = "closed"
		b.UpdatedAt = time.Now()
		return s.upsertBeadTx(ctx, tx, b)
	})
}

// DeleteIfMatch deletes the bead only when the stored revision matches.
func (s *SQLiteStore) DeleteIfMatch(id string, expectedRevision int64) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return s.conditionalWrite(id, expectedRevision, func(ctx context.Context, tx *sql.Tx, _ Bead) error {
		if _, err := tx.Exec(`DELETE FROM beads WHERE id=?`, id); err != nil {
			return fmt.Errorf("deleting bead %q: %w", id, err)
		}
		if err := s.clearClaimFenceTx(context.Background(), tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM deps WHERE issue_id=? OR depends_on_id=?`, id, id); err != nil {
			return fmt.Errorf("deleting bead %q deps: %w", id, err)
		}
		if err := s.clearGraphEdgeMetadataForBeadsTx(ctx, tx, []string{id}); err != nil {
			return err
		}
		return nil
	})
}

// CompareAndSetMetadataKey swaps metadata[key] iff its current value equals
// expected. A genuine mismatch is (false, nil) — the caller lost the race —
// distinct from an error.
func (s *SQLiteStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if err := s.ensureOpen(); err != nil {
		return false, err
	}
	if !s.hasRevisionColumn {
		return false, ErrConditionalWriteUnsupported
	}
	swapped := false
	err := retryOnBusy(func() error {
		swapped = false
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite compare-and-set: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("compare-and-set metadata on %q: %w", id, ErrNotFound)
			}
			return err
		}
		if b.Metadata[key] != expected {
			return tx.Commit() // genuine mismatch: caller lost, not an error
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, 1)
		}
		b.Metadata[key] = next
		b.UpdatedAt = time.Now()
		if err := s.upsertBeadTx(ctx, tx, b); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		swapped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return swapped, nil
}

// conditionalWrite is the shared fenced read-check-write body: load the bead
// inside the write transaction, compare its revision, apply, commit.
func (s *SQLiteStore) conditionalWrite(id string, expectedRevision int64, apply func(context.Context, *sql.Tx, Bead) error) error {
	if !s.hasRevisionColumn {
		return ErrConditionalWriteUnsupported
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite conditional write: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if b.Revision != expectedRevision {
			return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: b.Revision}
		}
		if err := apply(ctx, tx, b); err != nil {
			return err
		}
		after, err := s.getTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return tx.Commit()
			}
			return err
		}
		if err := s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, b, &after); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// updateOptsEmpty reports whether opts would apply no field at all — the
// ErrEmptyConditionalUpdate contract (a fenced no-op must not consume the
// caller's revision).
func updateOptsEmpty(opts UpdateOpts) bool {
	return opts.Title == nil && opts.Status == nil && opts.Type == nil &&
		opts.Priority == nil && opts.Description == nil && opts.ParentID == nil &&
		opts.Assignee == nil && len(opts.Metadata) == 0 &&
		len(opts.Labels) == 0 && len(opts.RemoveLabels) == 0
}

// sqliteDeleteBatchChunk bounds how many ids ride on one DELETE statement,
// staying well under SQLite's bound-parameter limit.
const sqliteDeleteBatchChunk = 256

var _ BatchDeleter = (*SQLiteStore)(nil)

// DeleteBatch removes exactly the given beads and every dependency row that
// touches them, ORPHANING external dependents rather than rewriting them —
// the BatchDeleter contract. Without it the wisp GC's closure purges fall
// back to per-bead deletes. Idempotent over ids that are already gone,
// chunked so a large closure cannot exceed the parameter limit. Each chunk is
// one transaction; a mid-run failure reports the already-committed ids so a
// caching layer can reconcile.
func (s *SQLiteStore) DeleteBatch(ids []string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	var deleted []string
	for start := 0; start < len(ids); start += sqliteDeleteBatchChunk {
		end := start + sqliteDeleteBatchChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if err := s.deleteBatchChunk(chunk); err != nil {
			if len(deleted) > 0 {
				return &BatchDeleteError{Committed: deleted, Err: err}
			}
			return err
		}
		deleted = append(deleted, chunk...)
	}
	return nil
}

func (s *SQLiteStore) deleteBatchChunk(chunk []string) error {
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite delete batch: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		args := make([]any, 0, len(chunk))
		placeholders := make([]string, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
			placeholders = append(placeholders, "?")
		}
		in := "(" + strings.Join(placeholders, ",") + ")"
		// Deps first, both directions: external dependents are orphaned (their
		// own rows survive, only the edge goes), never text-rewritten.
		depArgs := append(append([]any{}, args...), args...)
		if _, err := tx.ExecContext(ctx, `DELETE FROM deps WHERE issue_id IN `+in+` OR depends_on_id IN `+in, depArgs...); err != nil {
			return fmt.Errorf("deleting batch deps: %w", err)
		}
		if err := s.clearGraphEdgeMetadataForBeadsTx(ctx, tx, chunk); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM beads WHERE id IN `+in, args...); err != nil {
			return fmt.Errorf("deleting batch beads: %w", err)
		}
		fenceArgs := make([]any, 0, len(chunk))
		fencePlaceholders := make([]string, 0, len(chunk))
		for _, id := range chunk {
			fenceArgs = append(fenceArgs, sqliteClaimFenceKey(id))
			fencePlaceholders = append(fencePlaceholders, "?")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key IN (`+strings.Join(fencePlaceholders, ",")+")", fenceArgs...); err != nil {
			return fmt.Errorf("clearing batch claim fences: %w", err)
		}
		return tx.Commit()
	})
}
