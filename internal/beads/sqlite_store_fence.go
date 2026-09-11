package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

func sqliteClaimFenceKey(id string) string {
	return sqliteClaimFenceKVPrefix + id
}

// sqliteOwnershipTransition is deliberately equivalent to the native store's
// ownership-fence rule: an assignee change starts a new owner generation, as
// does reopening terminal work. Closing and content-only updates do not.
func sqliteOwnershipTransition(before, after Bead) bool {
	if before.Assignee != after.Assignee {
		return true
	}
	return before.Status == "closed" && after.Status != "" && after.Status != "closed"
}

func (s *SQLiteStore) bumpClaimFenceIfOwnershipTransitionTx(ctx context.Context, tx *sql.Tx, before Bead, after *Bead) error {
	if !sqliteOwnershipTransition(before, *after) {
		return nil
	}
	next := before.ClaimFence + 1
	if next <= before.ClaimFence {
		return fmt.Errorf("bumping claim fence for %q: counter overflow", before.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kv(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sqliteClaimFenceKey(before.ID), strconv.FormatInt(next, 10)); err != nil {
		return fmt.Errorf("persisting claim fence for %q: %w", before.ID, err)
	}
	after.ClaimFence = next
	return nil
}

func (s *SQLiteStore) clearClaimFenceTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, sqliteClaimFenceKey(id)); err != nil {
		return fmt.Errorf("clearing claim fence for %q: %w", id, err)
	}
	return nil
}
