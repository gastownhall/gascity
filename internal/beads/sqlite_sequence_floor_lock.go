package beads

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// persistSQLiteSequenceFloorAtLeast serializes the final floor re-read and
// atomic replacement across processes. The existing database inode is the
// stable lock target: graph.seqfloor itself is replaced by rename and therefore
// cannot safely carry its own lock, while a sibling lock/status file would add
// another persistent namespace object.
func persistSQLiteSequenceFloorAtLeast(databasePath, floorPath string, requested int64) (persisted int64, returnErr error) {
	database, err := os.Open(databasePath)
	if err != nil {
		return 0, fmt.Errorf("opening SQLite database for sequence-floor lock: %w", err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-lock-open")
	locked := false
	defer func() {
		if locked {
			observeSQLiteSequenceFloorBoundary("sequence-floor-lock-release-before")
			if err := syscall.Flock(int(database.Fd()), syscall.LOCK_UN); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("unlocking SQLite sequence floor: %w", err))
			} else {
				observeSQLiteSequenceFloorBoundary("sequence-floor-lock-release-after")
			}
		}
		observeSQLiteSequenceFloorBoundary("sequence-floor-lock-close-before")
		if err := database.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite sequence-floor lock descriptor: %w", err))
		} else {
			observeSQLiteSequenceFloorBoundary("sequence-floor-lock-close-after")
		}
	}()
	if err := syscall.Flock(int(database.Fd()), syscall.LOCK_EX); err != nil {
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
