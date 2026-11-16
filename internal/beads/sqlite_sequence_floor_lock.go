package beads

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// persistSQLiteSequenceFloorAtLeast serializes the final floor re-read and
// atomic replacement across processes. Unix locks the directory holding the
// store. Windows maps that directory identity to a stable lock file in a
// per-user lock root because LockFileEx cannot lock directory handles.
//
// graph.seqfloor is replaced by rename, so it cannot carry its own lock: two
// processes would end up holding locks on different inodes. A sibling lock file
// is also ruled out — the store directory is a censused namespace. Graph
// migration enumerates every entry in it and pins each one's presence, identity
// and bytes as a preservation fact, so an extra file there is not cosmetic
// clutter but a new term in the migration contract.
//
// The database inode is equally unusable, though only one platform says so.
// flock(2) locks belong to the open file description rather than the process,
// so locking the database contends with the store's own live connection instead
// of being re-entrant. Linux never shows it — SQLite serializes there with
// POSIX fcntl byte-range locks and never flocks the database — but macOS builds
// SQLite with SQLITE_ENABLE_LOCKING_STYLE and selects an flock-based VFS, so the
// store's open connection holds an flock on the database and a blocking LOCK_EX
// here deadlocked against it permanently (gas-bsj).
//
// On Unix the directory is left. It always exists, its inode is stable across
// the rename, SQLite never flocks it, and locking it creates nothing for the
// census to see. Windows uses the same canonical directory identity but stores
// the lock file outside the censused namespace. The cost is granularity: this
// is the store directory's single lock, owned by the sequence floor. Anything
// else needing to serialize on this directory must coordinate through here.
func persistSQLiteSequenceFloorAtLeast(floorPath string, requested int64) (persisted int64, returnErr error) {
	returnErr = withSQLiteSequenceFloorLock(floorPath, func() error {
		current, err := readSQLiteSequenceFloor(floorPath)
		if err != nil {
			return err
		}
		requested = sequenceMax(current, requested)
		if err := writeSQLiteSequenceFloor(floorPath, requested); err != nil {
			return err
		}
		persisted = requested
		return nil
	})
	if returnErr != nil {
		return 0, returnErr
	}
	return persisted, nil
}

// withSQLiteSequenceFloorLock runs fn while holding the store directory's
// exclusive lock — the single lock that serializes every read-modify-write of
// a sequence floor sidecar in that directory, across processes. Block
// reservation, SetSequenceFloor and the operator repair all go through here.
func withSQLiteSequenceFloorLock(floorPath string, fn func() error) (returnErr error) {
	lock, lockLabel, err := filelock.OpenDirectoryLock(filepath.Dir(floorPath))
	if err != nil {
		return fmt.Errorf("opening sequence-floor lock directory: %w", err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-lock-open")
	var heldLock *filelock.Lock
	defer func() {
		if heldLock != nil {
			observeSQLiteSequenceFloorBoundary("sequence-floor-lock-release-before")
			if err := heldLock.Release(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("unlocking SQLite sequence floor: %w", err))
			} else {
				observeSQLiteSequenceFloorBoundary("sequence-floor-lock-release-after")
			}
		}
		observeSQLiteSequenceFloorBoundary("sequence-floor-lock-close-before")
		if err := lock.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite sequence-floor lock descriptor: %w", err))
		} else {
			observeSQLiteSequenceFloorBoundary("sequence-floor-lock-close-after")
		}
	}()
	heldLock, err = filelock.Acquire(lock, filelock.Options{Mode: filelock.Exclusive, Label: lockLabel})
	if err != nil {
		return fmt.Errorf("locking SQLite sequence floor: %w", err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-lock-held")
	return fn()
}
