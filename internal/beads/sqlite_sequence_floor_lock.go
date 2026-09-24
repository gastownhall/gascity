package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// persistSQLiteSequenceFloorAtLeast serializes the final floor re-read and
// atomic replacement across processes. The lock target is the directory holding
// the store, which satisfies three constraints at once that no file in it can.
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
// The directory is left. It always exists, its inode is stable across the
// rename, SQLite never flocks it on either platform, and locking it creates
// nothing for the census to see. The cost is granularity: this flock is the
// store directory's single lock, owned by the sequence floor. Anything else
// needing to serialize on this directory must coordinate through here rather
// than take its own flock on the same inode.
func persistSQLiteSequenceFloorAtLeast(floorPath string, requested int64) (persisted int64, returnErr error) {
	lock, err := os.Open(filepath.Dir(floorPath))
	if err != nil {
		return 0, fmt.Errorf("opening sequence-floor lock directory: %w", err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-lock-open")
	locked := false
	defer func() {
		if locked {
			observeSQLiteSequenceFloorBoundary("sequence-floor-lock-release-before")
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
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
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("locking SQLite sequence floor: %w", err)
	}
	locked = true
	observeSQLiteSequenceFloorBoundary("sequence-floor-lock-held")

	current, err := readSQLiteSequenceFloor(floorPath)
	if err != nil {
		return 0, err
	}
	if current > requested {
		requested = current
	}
	if err := writeSQLiteSequenceFloor(floorPath, requested); err != nil {
		return 0, err
	}
	return requested, nil
}
