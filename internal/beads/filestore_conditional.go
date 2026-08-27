package beads

import "fmt"

// FileStore embeds *MemStore, which implements ConditionalWriter — but the
// promoted methods would write straight to the in-memory MemStore, bypassing
// FileStore's flush-on-write, cross-process flock, and reload-before-write. So
// FileStore overrides all four, plus the atomic terminal close, with the same
// reload → snapshot → delegate → save → rollback wrapper its other write
// methods use. Revisions survive the reload/save cycle via the out-of-band
// Revisions map in fileData (Bead.Revision is json:"-").
var (
	_ ConditionalWriter       = (*FileStore)(nil)
	_ AtomicConditionalCloser = (*FileStore)(nil)
)

// UpdateIfMatch applies opts only when the bead's persisted revision matches,
// then flushes to disk. A precondition failure or not-found leaves the store
// unchanged (no save). A failed flush rolls back the in-memory mutation.
func (fs *FileStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := validateConditionalUpdateOpts(opts); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if fs.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		return err // precondition failed / not found: nothing mutated, nothing to save
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// CloseIfMatch closes the bead only when its persisted revision matches, then
// flushes to disk.
func (fs *FileStore) CloseIfMatch(id string, expectedRevision int64) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if fs.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.CloseIfMatch(id, expectedRevision); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// DeleteIfMatch removes the bead only when its persisted revision matches, then
// flushes to disk.
func (fs *FileStore) DeleteIfMatch(id string, expectedRevision int64) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if fs.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.DeleteIfMatch(id, expectedRevision); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// CloseWithMetadataIfMatch merges metadata and closes the bead in one step,
// only while its persisted revision matches, then flushes to disk. It is the
// AtomicConditionalCloser capability for file-backed cities: without it
// session.Store.Close falls back to the ClosePatch-then-Close split, whose gap
// lets a stale writer strand a closed row in a nonterminal lifecycle state
// (ga-f7v2ft.78.6). The merge and the close share one MemStore lock inside one
// flock-held reload/save cycle, so no writer can land between them. A
// precondition failure leaves both memory and file untouched — which is what
// Store.Close's bounded reread-and-retry arm needs — and a failed flush rolls
// the in-memory mutation back.
func (fs *FileStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if fs.DisableConditionalWrites {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	if err := fs.locker.Lock(); err != nil {
		return Bead{}, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return Bead{}, err
	}
	snap := fs.snapshotLocked()
	// The delegate is the embedded MemStore's shared atomic-close body; the
	// four siblings above spell out fs.MemStore only because their names would
	// otherwise recurse.
	closed, err := fs.closeWithMetadataIfMatch(id, expectedRevision, metadata)
	if err != nil {
		return Bead{}, err // precondition failed / not found: nothing mutated, nothing to save
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return Bead{}, err
	}
	return closed, nil
}

// CompareAndSetMetadataKey performs a value-CAS on one metadata key, then
// flushes to disk. A genuine value mismatch returns (false, nil) without a save;
// a swap persists and returns (true, nil).
func (fs *FileStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if fs.DisableConditionalWrites {
		return false, ErrConditionalWriteUnsupported
	}
	if err := fs.locker.Lock(); err != nil {
		return false, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return false, err
	}
	snap := fs.snapshotLocked()
	ok, err := fs.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
	if err != nil || !ok {
		return ok, err // error, or (false, nil) genuine mismatch: nothing to persist
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return false, err
	}
	return true, nil
}
