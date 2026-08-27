package beads

// SQLiteStore's AtomicConditionalCloser implementation. Without it a routed
// sqlite city takes session.Store.Close's historical ClosePatch-then-Close
// fallback, whose gap is what lets a stale writer strand a closed row in a
// nonterminal lifecycle state (ga-f7v2ft.78.6).

import (
	"context"
	"database/sql"
	"time"
)

var _ AtomicConditionalCloser = (*SQLiteStore)(nil)

// CloseWithMetadataIfMatch merges metadata into id and closes it, but only
// while the stored revision still equals expectedRevision. Both changes are
// applied by a single upsert inside the fence's own transaction, so they commit
// together or neither persists and no writer can land between them. A losing
// fence returns *PreconditionFailedError and leaves the row untouched.
func (s *SQLiteStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, err
	}
	var closed Bead
	err := s.conditionalWrite(id, expectedRevision, func(ctx context.Context, tx *sql.Tx, b Bead) error {
		merged := make(StringMap, len(b.Metadata)+len(metadata))
		for key, value := range b.Metadata {
			merged[key] = value
		}
		for key, value := range metadata {
			merged[key] = value
		}
		b.Metadata = merged
		b.Status = "closed"
		b.UpdatedAt = time.Now()
		if err := s.upsertBeadTx(ctx, tx, b); err != nil {
			return err
		}
		stored, err := s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		closed = stored
		return nil
	})
	if err != nil {
		return Bead{}, err
	}
	return closed, nil
}
