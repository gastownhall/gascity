package beads

import (
	"context"
	"math"
)

var _ GuardedBatcher = (*MemStore)(nil)

// ApplyGuarded applies b under the store mutex in two phases: every guard is
// checked against the rows as they stand, then every item is applied. A batch
// names each row at most once, so the rows as they stand are the working copy
// each item would see, and a refusal in the first phase leaves nothing written.
func (m *MemStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	if err := ctx.Err(); err != nil {
		return GuardedResult{}, err
	}
	if err := validateGuardedBatch(b); err != nil {
		return GuardedResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyGuardedLocked(b)
}

// applyGuardedLocked is ApplyGuarded's body. The caller holds m.mu and has
// validated b.
func (m *MemStore) applyGuardedLocked(b GuardedBatch) (GuardedResult, error) {
	m.noteGuardedCreates(b)
	if err := m.checkGuardedLocked(b); err != nil {
		return GuardedResult{}, err
	}
	revisions := make(map[string]int64, len(b.Items))
	for _, it := range b.Items {
		if it.Create != nil {
			stored := m.insertPinnedLocked(*it.Create)
			revisions[stored.ID] = stored.Revision
			continue
		}
		i := m.indexOfLocked(it.Update.ID)
		m.applyUpdateLocked(i, it.Update.Opts)
		revisions[it.Update.ID] = m.beads[i].Revision
	}
	return GuardedResult{Revisions: revisions}, nil
}

// noteGuardedCreates reports each create's seed to the session hooks before any
// guard is checked, as Create reports one before its checks, so a batch the
// store then refuses has reported its creates too. An update reports as it is
// applied (applyUpdateLocked), as Update's does.
func (m *MemStore) noteGuardedCreates(b GuardedBatch) {
	if m.storageOnly {
		return
	}
	for _, it := range b.Items {
		if it.Create != nil {
			noteSessionCreate(*it.Create)
		}
	}
}

// checkGuardedLocked is the first phase: it refuses the batch at the first item
// whose create collides or whose update target is missing or guarded on
// another revision.
func (m *MemStore) checkGuardedLocked(b GuardedBatch) error {
	for idx, it := range b.Items {
		i := m.indexOfLocked(it.id())
		if it.Create != nil {
			if i >= 0 {
				return guardedExistsError(idx, it.Create.ID, nil)
			}
			continue
		}
		if i < 0 {
			return guardedNotFoundError(idx, it.Update.ID, nil)
		}
		if want := it.expectedRevision(); want != nil && m.beads[i].Revision != *want {
			return guardedPreconditionError(idx, it.Update.ID, *want, m.beads[i].Revision, "")
		}
	}
	return nil
}

// insertPinnedLocked stores b under its pinned id, as Create does with
// HonorExplicitIDs, except that the create's status is kept (a batch may write
// a row already closed). The caller holds m.mu and has checked the id is free.
func (m *MemStore) insertPinnedLocked(b Bead) Bead {
	prefix := m.IDPrefix
	if prefix == "" {
		prefix = "gc"
	}
	if n, ok := parseSQLiteAutoIDSuffix(prefix, b.ID); ok && n > 0 && n <= math.MaxInt && n > int64(m.seq) {
		m.seq = int(n)
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	guardedCreateCloseReason(&b)
	b.CreatedAt = m.nextCreatedAtLocked()
	b.UpdatedAt = b.CreatedAt
	b.Revision = 1
	b.ClaimFence = 0
	stored := cloneBead(b)
	m.beads = append(m.beads, stored)
	return cloneBead(stored)
}
