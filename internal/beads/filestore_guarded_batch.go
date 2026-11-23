package beads

import "context"

// FileStore overrides the ApplyGuarded it would otherwise promote from its
// embedded MemStore: the promoted method writes only to memory, bypassing the
// cross-process lock, the reload before the write and the flush after it.
var _ GuardedBatcher = (*FileStore)(nil)

// ApplyGuarded applies b under the file lock: reload, check every guard and
// apply every item in memory, then persist once. A refused batch saves
// nothing, and a failed save rolls the in-memory state back, so the file holds
// all of the batch or none of it.
func (fs *FileStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	if err := ctx.Err(); err != nil {
		return GuardedResult{}, err
	}
	if err := validateGuardedBatch(b); err != nil {
		return GuardedResult{}, err
	}
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return GuardedResult{}, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return GuardedResult{}, err
	}
	snap := fs.snapshotLocked()
	fs.mu.Lock()
	res, err := fs.applyGuardedLocked(b)
	fs.mu.Unlock()
	if err != nil {
		return GuardedResult{}, err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return GuardedResult{}, err
	}
	return res, nil
}
