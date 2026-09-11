//go:build linux

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/storebinding"
	"golang.org/x/sys/unix"
)

const (
	sqlitePendingLockByte      int64 = 0x40000000
	sqliteReservedLockByte     int64 = sqlitePendingLockByte + 1
	sqliteSharedLockStart      int64 = sqlitePendingLockByte + 2
	sqliteSharedLockCount      int64 = 510
	sqliteWALWriteLockByte     int64 = 120
	sqliteWALLockCount         int64 = 3 // writer, checkpoint, and recovery locks.
	sqliteWALDeadManSwitchByte int64 = 128
)

func sqliteWriterFencingSupported() bool { return true }

// linuxSQLiteWriterReservation uses Linux open-file-description locks rather
// than process-associated POSIX locks. SQLite uses fcntl byte locks, which
// conflict with OFD locks even when the prospective writer is in this process.
// Holding the descriptor prevents unrelated descriptor closes from releasing
// the reservation.
type linuxSQLiteWriterReservation struct {
	directory     *os.File
	database      *os.File
	pending       *os.File
	wal           *os.File
	shm           *os.File
	journal       *os.File
	sequenceFloor *os.File

	mu sync.Mutex
}

var closeSQLiteReservationFile = func(file *os.File) error { return file.Close() }

func acquireSQLiteWriterReservation(ctx context.Context, databasePath string, claim storebinding.MigrationGuardClaim, expected sqliteWriterReservationSource) (sqliteWriterReservation, error) {
	if err := validateSQLiteMigrationGuardClaim(claim); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureNoSQLiteSourceDescriptors(databasePath); err != nil {
		return nil, err
	}

	reservation := &linuxSQLiteWriterReservation{}
	releaseOnError := func(cause error) (sqliteWriterReservation, error) {
		if releaseErr := reservation.Release(); releaseErr != nil {
			cause = errors.Join(cause, fmt.Errorf("releasing incomplete SQLite writer reservation: %w", releaseErr))
			return reservation, cause
		}
		return nil, cause
	}

	directory, err := openSQLiteReservationDirectory(filepath.Dir(databasePath), expected.directory)
	if directory != nil {
		reservation.directory = directory
		observeSQLiteBoundary("reservation-open-directory")
	}
	if err != nil {
		return releaseOnError(err)
	}
	databaseName := filepath.Base(databasePath)

	// The fence never writes source bytes, but Linux requires a writable
	// descriptor for the exact exclusive SQLite writer-byte locks.
	database, err := openSQLiteReservationComponent(directory, databaseName, os.O_RDWR, databasePath, expected.database)
	if database != nil {
		reservation.database = database
		observeSQLiteBoundary("reservation-open-database")
	}
	if err != nil {
		return releaseOnError(err)
	}
	if err := qualifySQLiteFenceFilesystem(database, databasePath); err != nil {
		return releaseOnError(err)
	}

	for _, component := range []struct {
		name     string
		label    string
		flags    int
		boundary string
		expected storebinding.PhysicalIdentity
		assign   func(*os.File)
	}{
		{name: databaseName + "-wal", label: "WAL", flags: os.O_RDONLY, boundary: "reservation-open-wal", expected: expected.wal, assign: func(file *os.File) { reservation.wal = file }},
		{name: databaseName + "-shm", label: "WAL-index", flags: os.O_RDWR, boundary: "reservation-open-shm", expected: expected.shm, assign: func(file *os.File) { reservation.shm = file }},
		{name: databaseName + "-journal", label: "rollback-journal", flags: os.O_RDONLY, boundary: "reservation-open-journal", expected: expected.journal, assign: func(file *os.File) { reservation.journal = file }},
		{name: graphSequenceFloorFilename, label: "sequence-floor", flags: os.O_RDONLY, boundary: "reservation-open-sequence-floor", expected: expected.sequenceFloor, assign: func(file *os.File) { reservation.sequenceFloor = file }},
	} {
		if component.expected == "" {
			continue
		}
		file, err := openSQLiteReservationComponent(directory, component.name, component.flags, filepath.Join(filepath.Dir(databasePath), component.name), component.expected)
		if file != nil {
			component.assign(file)
			observeSQLiteBoundary(component.boundary)
		}
		if err != nil {
			return releaseOnError(fmt.Errorf("opening SQLite %s snapshot source: %w", component.label, err))
		}
	}

	plan, err := sqliteWriterReservationPlanFor(ctx, database, reservation.wal, reservation.shm, reservation.journal)
	if err != nil {
		return releaseOnError(err)
	}
	if err := lockQualifiedSQLiteOFD(database, databasePath, unix.F_WRLCK, sqliteReservedLockByte, 1); err != nil {
		return releaseOnError(fmt.Errorf("acquiring SQLite rollback writer lock: %w", err))
	}
	observeSQLiteBoundary("reservation-lock-reserved")
	if plan.lockPending {
		// WAL without an existing SHM needs the exclusive PENDING byte. A read
		// lock would permit a new SQLite connection to bootstrap a new WAL
		// namespace, so this one exceptional state fails closed when the source
		// cannot be opened writable.
		pending, err := openSQLiteReservationComponent(directory, databaseName, os.O_RDWR, databasePath, expected.database)
		if pending != nil {
			reservation.pending = pending
			observeSQLiteBoundary("reservation-open-pending")
		}
		if err != nil {
			return releaseOnError(fmt.Errorf("opening SQLite pending writer lock: %w", err))
		}
		if err := lockQualifiedSQLiteOFD(pending, databasePath, unix.F_WRLCK, sqlitePendingLockByte, 1); err != nil {
			return releaseOnError(fmt.Errorf("acquiring SQLite pending writer lock: %w", err))
		}
		observeSQLiteBoundary("reservation-lock-pending")
	}
	if err := lockQualifiedSQLiteOFD(database, databasePath, unix.F_RDLCK, sqliteSharedLockStart, sqliteSharedLockCount); err != nil {
		return releaseOnError(fmt.Errorf("acquiring SQLite shared-range close guard: %w", err))
	}
	observeSQLiteBoundary("reservation-lock-shared")
	if !plan.lockWALIndex {
		return reservation, nil
	}

	shm := reservation.shm
	if err := qualifySQLiteFenceFilesystem(shm, databasePath+"-shm"); err != nil {
		return releaseOnError(err)
	}
	if err := lockQualifiedSQLiteOFD(shm, databasePath+"-shm", unix.F_RDLCK, sqliteWALDeadManSwitchByte, 1); err != nil {
		return releaseOnError(fmt.Errorf("acquiring SQLite WAL-index dead-man-switch guard: %w", err))
	}
	observeSQLiteBoundary("reservation-lock-shm-dms")
	if err := lockQualifiedSQLiteOFD(shm, databasePath+"-shm", unix.F_WRLCK, sqliteWALWriteLockByte, sqliteWALLockCount); err != nil {
		return releaseOnError(fmt.Errorf("acquiring SQLite WAL writer locks: %w", err))
	}
	observeSQLiteBoundary("reservation-lock-shm-writers")
	return reservation, nil
}

func openSQLiteReservationDirectory(path string, expected storebinding.PhysicalIdentity) (*os.File, error) {
	if expected == "" {
		return nil, fmt.Errorf("%w: missing source directory identity", errSQLiteSourceChanged)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, sqliteReservationOpenError("source directory", err)
	}
	directory := os.NewFile(uintptr(fd), path)
	if err := verifySQLiteReservationDirectory(directory, path, expected); err != nil {
		return directory, err
	}
	return directory, nil
}

func openSQLiteReservationComponent(directory *os.File, name string, flags int, path string, expected storebinding.PhysicalIdentity) (*os.File, error) {
	if directory == nil || filepath.Base(name) != name {
		return nil, fmt.Errorf("%w: invalid SQLite source component", errSQLiteSourceChanged)
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, sqliteReservationOpenError(name, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := verifySQLiteReservationFile(file, path, expected); err != nil {
		return file, err
	}
	return file, nil
}

func sqliteReservationOpenError(label string, err error) error {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: opening SQLite %s: %w", errSQLiteSourceChanged, label, err)
	}
	return fmt.Errorf("opening SQLite %s: %w", label, err)
}

func verifySQLiteReservationDirectory(directory *os.File, path string, expected storebinding.PhysicalIdentity) error {
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("stating opened SQLite source directory: %w", err)
	}
	if !info.IsDir() || physicalIdentity(path, info) != expected {
		return fmt.Errorf("%w: SQLite source directory", errSQLiteSourceChanged)
	}
	return nil
}

func verifySQLiteReservationFile(file *os.File, path string, expected storebinding.PhysicalIdentity) error {
	if expected == "" {
		return fmt.Errorf("verifying SQLite writer reservation: missing expected source identity")
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stating opened SQLite writer reservation: %w", err)
	}
	if !info.Mode().IsRegular() || physicalIdentity(path, info) != expected {
		return fmt.Errorf("%w: SQLite source component", errSQLiteSourceChanged)
	}
	return nil
}

var lockSQLiteOFD = func(file *os.File, lockType int, start, length int64) error {
	return unix.FcntlFlock(file.Fd(), unix.F_OFD_SETLK, &unix.Flock_t{
		Type:   int16(lockType),
		Whence: int16(unix.SEEK_SET),
		Start:  start,
		Len:    length,
	})
}

var probeSQLiteOFD = func(file *os.File) error {
	return unix.FcntlFlock(file.Fd(), unix.F_OFD_GETLK, &unix.Flock_t{
		Type:   int16(unix.F_RDLCK),
		Whence: int16(unix.SEEK_SET),
	})
}

func qualifySQLiteFenceFilesystem(file *os.File, path string) error {
	if err := probeSQLiteOFD(file); err != nil {
		if sqliteOFDUnsupported(err) {
			return &FenceFilesystemError{Path: path, Err: err}
		}
		return fmt.Errorf("probing SQLite OFD lock support for %s: %w", path, err)
	}
	return nil
}

// qualifySQLiteStaticWriterFencing proves that the exact static source path
// supports OFD locks before inspection advertises writer fencing. It opens
// only read-only descriptors pinned to the preceding census; callers must
// recensus after this probe before publishing their descriptor.
func qualifySQLiteStaticWriterFencing(ctx context.Context, databasePath string, directoryIdentity, databaseIdentity storebinding.PhysicalIdentity) (qualified bool, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	directory, err := openSQLiteReservationDirectory(filepath.Dir(databasePath), directoryIdentity)
	if directory != nil {
		defer func() {
			if err := directory.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite static source directory: %w", err))
			}
		}()
	}
	if err != nil {
		return false, err
	}
	database, err := openSQLiteReservationComponent(directory, filepath.Base(databasePath), os.O_RDONLY, databasePath, databaseIdentity)
	if database != nil {
		defer func() {
			if err := database.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite static source descriptor: %w", err))
			}
		}()
	}
	if err != nil {
		return false, err
	}
	if err := qualifySQLiteFenceFilesystem(database, databasePath); err != nil {
		if errors.Is(err, ErrSQLiteFenceFilesystemUnqualified) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func lockQualifiedSQLiteOFD(file *os.File, path string, lockType int, start, length int64) error {
	if err := lockSQLiteOFD(file, lockType, start, length); err != nil {
		if sqliteOFDUnsupported(err) {
			return &FenceFilesystemError{Path: path, Err: err}
		}
		return err
	}
	return nil
}

func sqliteOFDUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}

func (r *linuxSQLiteWriterReservation) Release() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	r.closeFile(&r.shm, "shm", "SQLite WAL-index writer lock", &errs)
	r.closeFile(&r.pending, "pending", "SQLite pending writer lock", &errs)
	r.closeFile(&r.sequenceFloor, "sequence-floor", "SQLite sequence-floor snapshot source", &errs)
	r.closeFile(&r.journal, "journal", "SQLite rollback-journal snapshot source", &errs)
	r.closeFile(&r.wal, "wal", "SQLite WAL snapshot source", &errs)
	r.closeFile(&r.database, "database", "SQLite rollback writer lock", &errs)
	r.closeFile(&r.directory, "directory", "SQLite source directory pin", &errs)
	return errors.Join(errs...)
}

func (r *linuxSQLiteWriterReservation) closeFile(file **os.File, boundary, label string, errs *[]error) {
	if *file == nil {
		return
	}
	observeSQLiteBoundary("reservation-release-" + boundary + "-before")
	if err := closeSQLiteReservationFile(*file); err != nil {
		*errs = append(*errs, fmt.Errorf("closing %s: %w", label, err))
		return
	}
	*file = nil
	observeSQLiteBoundary("reservation-release-" + boundary + "-after")
}

func (r *linuxSQLiteWriterReservation) snapshotFiles() sqliteSnapshotFiles {
	if r == nil {
		return sqliteSnapshotFiles{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return sqliteSnapshotFiles{database: r.database, wal: r.wal, journal: r.journal, sequenceFloor: r.sequenceFloor}
}
