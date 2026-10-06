package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/storebinding"
)

var (
	// ErrSQLiteSourceOpenInProcess reports a fence attempt that could release
	// traditional SQLite POSIX locks when its reservation descriptors close.
	// Migration startup must not retain any source SQLite connection locally.
	ErrSQLiteSourceOpenInProcess = errors.New("SQLite source is open in this process")
	// errSQLiteSourceChanged marks a pinned source namespace or descriptor that
	// no longer matches the inspection census. Callers translate it to their
	// public component-specific ErrFenceTargetMoved error.
	errSQLiteSourceChanged = errors.New("SQLite source changed after source census")
	// ErrSQLiteFenceFilesystemUnqualified reports a filesystem/VFS that cannot
	// prove the OFD locking semantics required for a non-mutating SQLite fence.
	ErrSQLiteFenceFilesystemUnqualified = errors.New("SQLite fence filesystem is unqualified")
)

// FenceFilesystemError reports an OFD-lock qualification failure for one
// source component. It retains the kernel error for callers that need to
// distinguish unsupported locking from an ordinary I/O failure.
type FenceFilesystemError struct {
	Path string
	Err  error
}

// Error returns a stable, path-scoped qualification diagnostic.
func (e *FenceFilesystemError) Error() string {
	if e == nil {
		return ErrSQLiteFenceFilesystemUnqualified.Error()
	}
	return fmt.Sprintf("%s: %s: %v", ErrSQLiteFenceFilesystemUnqualified, e.Path, e.Err)
}

// Unwrap exposes both the typed qualification sentinel and the kernel cause.
func (e *FenceFilesystemError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{ErrSQLiteFenceFilesystemUnqualified, e.Err}
}

const privateSQLiteSnapshotFileMode = 0o600

var (
	removeSQLiteSnapshotFile = os.Remove
	closeSQLiteCensusFile    = func(file *os.File) error { return file.Close() }
	observeSQLiteBoundary    = func(string) {}
)

// sqliteWriterReservation owns the operating-system locks that exclude SQLite
// writers without opening the database through SQLite itself. Implementations
// must release every held file descriptor when Release returns.
type sqliteWriterReservation interface {
	Release() error
	snapshotFiles() sqliteSnapshotFiles
}

// sqliteWriterReservationPlan separates the rollback-lock and WAL-index parts
// of SQLite's lock protocol. Persistent WAL mode without sidecars needs the
// pending byte to prevent a new connection bootstrapping a WAL index, whereas
// ordinary rollback-journal databases do not.
type sqliteWriterReservationPlan struct {
	lockPending  bool
	lockWALIndex bool
}

// sqliteWriterReservationSource pins the database and, for a live WAL, its
// WAL-index inode admitted by the source census. Reservation descriptors must
// prove they opened those exact objects before taking byte locks.
type sqliteWriterReservationSource struct {
	directory     storebinding.PhysicalIdentity
	database      storebinding.PhysicalIdentity
	wal           storebinding.PhysicalIdentity
	shm           storebinding.PhysicalIdentity
	journal       storebinding.PhysicalIdentity
	sequenceFloor storebinding.PhysicalIdentity
}

// sqliteSnapshotFiles holds the exact source descriptors that may be copied
// into a private recovery snapshot. The reservation owns their lifetime.
type sqliteSnapshotFiles struct {
	database      *os.File
	wal           *os.File
	journal       *os.File
	sequenceFloor *os.File
}

func (f sqliteSnapshotFiles) component(name, databaseName string) (*os.File, bool) {
	switch name {
	case databaseName:
		return f.database, f.database != nil
	case databaseName + "-wal":
		return f.wal, f.wal != nil
	case databaseName + "-journal":
		return f.journal, f.journal != nil
	case graphSequenceFloorFilename:
		return f.sequenceFloor, f.sequenceFloor != nil
	default:
		return nil, false
	}
}

// sqliteSnapshotExpectation is the immutable source census for one pinned
// SQLite component. The component descriptor is checked before and after the
// stream, while its hash is checked against the copied output.
type sqliteSnapshotExpectation struct {
	mode     os.FileMode
	size     int64
	modTime  time.Time
	hash     string
	identity storebinding.PhysicalIdentity
}

func copyPinnedSQLiteSnapshotFile(ctx context.Context, source *os.File, destination string, expected sqliteSnapshotExpectation) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil {
		return errors.New("copying pinned SQLite source: missing source descriptor")
	}
	if expected.identity == "" || expected.hash == "" {
		return errors.New("copying pinned SQLite source: missing source census")
	}
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stating pinned SQLite source: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode() != expected.mode || info.Size() != expected.size || !info.ModTime().Equal(expected.modTime) || physicalIdentity("", info) != expected.identity {
		return fmt.Errorf("copying pinned SQLite source: %w", errSQLiteSourceChanged)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking pinned SQLite source: %w", err)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateSQLiteSnapshotFileMode)
	if err != nil {
		return fmt.Errorf("creating SQLite snapshot entry: %w", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite snapshot entry: %w", err))
		}
		if returnErr != nil {
			if err := removeSQLiteSnapshotFile(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing incomplete SQLite snapshot entry: %w", err))
			}
		}
	}()
	if err := out.Chmod(privateSQLiteSnapshotFileMode); err != nil {
		return fmt.Errorf("setting SQLite snapshot entry permissions: %w", err)
	}

	hash := sha256.New()
	written, err := copySQLiteSnapshotStream(ctx, out, hash, source)
	if err != nil {
		return fmt.Errorf("copying pinned SQLite source: %w", err)
	}
	if written != expected.size {
		return fmt.Errorf("copying pinned SQLite source: copied %d bytes, want %d: %w", written, expected.size, errSQLiteSourceChanged)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != expected.hash {
		return fmt.Errorf("copying pinned SQLite source: copied hash %s does not match census: %w", got, errSQLiteSourceChanged)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing SQLite snapshot entry: %w", err)
	}
	if err := os.Chtimes(destination, expected.modTime, expected.modTime); err != nil {
		return fmt.Errorf("preserving SQLite snapshot timestamp: %w", err)
	}
	return nil
}

const sqliteSnapshotCopyBufferBytes = 64 * 1024

func hashSQLiteSourceFile(ctx context.Context, file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("hashing SQLite source: missing source descriptor")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seeking SQLite source for hash: %w", err)
	}
	hash := sha256.New()
	if _, err := copySQLiteSnapshotStream(ctx, io.Discard, hash, file); err != nil {
		return "", fmt.Errorf("hashing SQLite source: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copySQLiteSnapshotStream(ctx context.Context, destination io.Writer, hash io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, sqliteSnapshotCopyBufferBytes)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
			if _, err := hash.Write(buffer[:read]); err != nil {
				return written, err
			}
			written += int64(read)
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

var sqliteRollbackJournalMagic = [...]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

const (
	sqliteRollbackJournalHeaderBytes  = 28
	sqliteRollbackJournalMinSector    = 32
	sqliteRollbackJournalMaxSector    = 65536
	sqliteRollbackJournalMinPageSize  = 512
	sqliteRollbackJournalMaxPageSize  = 65536
	sqliteRollbackJournalNoSyncRecord = ^uint32(0)
)

type sqliteRollbackJournalHeader struct {
	recordCount uint32
	checksum    uint32
	sectorSize  uint32
	pageSize    uint32
}

func sqliteWriterReservationPlanFor(ctx context.Context, file, wal, shm, journal *os.File) (sqliteWriterReservationPlan, error) {
	if err := ctx.Err(); err != nil {
		return sqliteWriterReservationPlan{}, err
	}
	header, err := readSQLiteDatabaseHeader(ctx, file)
	if err != nil {
		return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite header: %w", err)
	}
	switch {
	case header.writeFormat == 1 && header.readFormat == 1:
		if wal != nil || shm != nil {
			return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite header: rollback-journal database has WAL sidecars")
		}
		if journal != nil {
			if err := validateSQLiteRollbackJournal(ctx, journal); err != nil {
				return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite rollback journal: %w", err)
			}
		}
		return sqliteWriterReservationPlan{}, nil
	case header.writeFormat == 2 && header.readFormat == 2:
		if journal != nil {
			return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite header: WAL database has rollback journal")
		}
		if err := validateSQLiteWALState(ctx, header, wal, shm); err != nil {
			return sqliteWriterReservationPlan{}, err
		}
		if shm != nil && wal == nil {
			return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite header: WAL-index exists without WAL")
		}
		if wal != nil && shm != nil {
			return sqliteWriterReservationPlan{lockWALIndex: true}, nil
		}
		return sqliteWriterReservationPlan{lockPending: true}, nil
	default:
		return sqliteWriterReservationPlan{}, fmt.Errorf("reading SQLite header: unsupported journal-mode versions %d/%d", header.writeFormat, header.readFormat)
	}
}

// validateSQLiteRollbackJournal admits only a complete, recoverable hot
// journal. SQLite writes a full sector header followed by checksummed page
// records; accepting a bare 28-byte prefix would make a truncated residue look
// safe to copy and recover. See https://www.sqlite.org/fileformat2.html#the_rollback_journal.
func validateSQLiteRollbackJournal(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil {
		return errors.New("missing pinned journal descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stating journal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("journal is not a regular file")
	}
	journalSize := info.Size()
	// SQLite's hot-journal test requires more than a 512-byte header. A
	// complete page record is required here as well so a residual header cannot
	// be mistaken for a recoverable crash journal.
	if journalSize <= sqliteRollbackJournalMinPageSize {
		return errors.New("journal is not hot")
	}

	offset := int64(0)
	var first sqliteRollbackJournalHeader
	for segment := 0; ; segment++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := readSQLiteRollbackJournalHeader(ctx, file, offset, journalSize)
		if err != nil {
			return err
		}
		if segment == 0 {
			first = header
		} else if header.sectorSize != first.sectorSize || header.pageSize != first.pageSize {
			return errors.New("inconsistent journal segment sizes")
		}

		offset += int64(header.sectorSize)
		recordSize := int64(header.pageSize) + 8
		if header.recordCount == sqliteRollbackJournalNoSyncRecord {
			if segment != 0 {
				return errors.New("no-sync record count outside initial journal segment")
			}
			remaining := journalSize - offset
			if remaining <= 0 || remaining%recordSize != 0 {
				return errors.New("truncated no-sync journal records")
			}
			if err := validateSQLiteRollbackJournalRecords(ctx, file, offset, uint64(remaining/recordSize), header); err != nil {
				return err
			}
			return nil
		}
		if header.recordCount == 0 {
			return errors.New("journal is not hot")
		}
		remaining := journalSize - offset
		if uint64(header.recordCount) > uint64(remaining/recordSize) {
			return errors.New("truncated journal records")
		}
		if err := validateSQLiteRollbackJournalRecords(ctx, file, offset, uint64(header.recordCount), header); err != nil {
			return err
		}
		offset += int64(header.recordCount) * recordSize
		if offset == journalSize {
			return nil
		}

		nextHeader, err := sqliteRollbackJournalNextHeaderOffset(offset, header.sectorSize)
		if err != nil {
			return err
		}
		if nextHeader > journalSize {
			// The committed record count is authoritative. SQLite may leave a
			// partial next record before the next sector boundary when a process
			// crashes, and pager playback ignores that tail.
			return nil
		}
		if nextHeader == journalSize {
			return nil
		}
		if journalSize-nextHeader < int64(header.sectorSize) {
			return nil
		}
		var nextMagic [len(sqliteRollbackJournalMagic)]byte
		if err := readSQLiteRollbackJournalAt(ctx, file, nextHeader, nextMagic[:]); err != nil {
			return fmt.Errorf("reading next journal segment: %w", err)
		}
		if !bytes.Equal(nextMagic[:], sqliteRollbackJournalMagic[:]) {
			return nil
		}
		offset = nextHeader
	}
}

func readSQLiteRollbackJournalHeader(ctx context.Context, file *os.File, offset, journalSize int64) (sqliteRollbackJournalHeader, error) {
	if journalSize-offset < sqliteRollbackJournalHeaderBytes {
		return sqliteRollbackJournalHeader{}, errors.New("truncated journal header")
	}
	var raw [sqliteRollbackJournalHeaderBytes]byte
	if err := readSQLiteRollbackJournalAt(ctx, file, offset, raw[:]); err != nil {
		return sqliteRollbackJournalHeader{}, fmt.Errorf("reading journal header: %w", err)
	}
	if !bytes.Equal(raw[:len(sqliteRollbackJournalMagic)], sqliteRollbackJournalMagic[:]) {
		return sqliteRollbackJournalHeader{}, errors.New("invalid journal magic")
	}
	header := sqliteRollbackJournalHeader{
		recordCount: binary.BigEndian.Uint32(raw[8:12]),
		checksum:    binary.BigEndian.Uint32(raw[12:16]),
		sectorSize:  binary.BigEndian.Uint32(raw[20:24]),
		pageSize:    binary.BigEndian.Uint32(raw[24:28]),
	}
	if !validSQLiteJournalSectorSize(header.sectorSize) {
		return sqliteRollbackJournalHeader{}, errors.New("invalid journal sector size")
	}
	if !validSQLiteJournalPageSize(header.pageSize) {
		return sqliteRollbackJournalHeader{}, errors.New("invalid journal page size")
	}
	if journalSize-offset < int64(header.sectorSize) {
		return sqliteRollbackJournalHeader{}, errors.New("truncated journal header sector")
	}
	if err := validateSQLiteRollbackJournalPadding(ctx, file, offset+sqliteRollbackJournalHeaderBytes, int64(header.sectorSize)-sqliteRollbackJournalHeaderBytes); err != nil {
		return sqliteRollbackJournalHeader{}, err
	}
	return header, nil
}

func validSQLiteJournalSectorSize(size uint32) bool {
	return size >= sqliteRollbackJournalMinSector && size <= sqliteRollbackJournalMaxSector && size&(size-1) == 0
}

func validSQLiteJournalPageSize(size uint32) bool {
	return size >= sqliteRollbackJournalMinPageSize && size <= sqliteRollbackJournalMaxPageSize && size&(size-1) == 0
}

func validateSQLiteRollbackJournalRecords(ctx context.Context, file *os.File, offset int64, count uint64, header sqliteRollbackJournalHeader) error {
	record := make([]byte, int(header.pageSize)+8)
	for index := uint64(0); index < count; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := readSQLiteRollbackJournalAt(ctx, file, offset, record); err != nil {
			return fmt.Errorf("reading journal record %d: %w", index, err)
		}
		pageNumber := binary.BigEndian.Uint32(record[:4])
		if pageNumber == 0 || pageNumber == ^uint32(0) {
			return fmt.Errorf("invalid journal record %d page number", index)
		}
		page := record[4 : len(record)-4]
		if got, want := binary.BigEndian.Uint32(record[len(record)-4:]), sqliteRollbackJournalPageChecksum(header.checksum, page); got != want {
			return fmt.Errorf("invalid journal record %d checksum", index)
		}
		offset += int64(len(record))
	}
	return nil
}

func validateSQLiteRollbackJournalPadding(ctx context.Context, file *os.File, offset, length int64) error {
	if length == 0 {
		return nil
	}
	zero, err := sqliteRollbackJournalBytesAreZero(ctx, file, offset, length)
	if err != nil {
		return fmt.Errorf("reading journal padding: %w", err)
	}
	if !zero {
		return errors.New("journal padding is not zeroed")
	}
	return nil
}

func sqliteRollbackJournalBytesAreZero(ctx context.Context, file *os.File, offset, length int64) (bool, error) {
	bytes := make([]byte, int(length))
	if err := readSQLiteRollbackJournalAt(ctx, file, offset, bytes); err != nil {
		return false, err
	}
	for _, value := range bytes {
		if value != 0 {
			return false, nil
		}
	}
	return true, nil
}

func sqliteRollbackJournalNextHeaderOffset(offset int64, sectorSize uint32) (int64, error) {
	sector := int64(sectorSize)
	if offset > int64(^uint64(0)>>1)-sector {
		return 0, errors.New("journal offset overflow")
	}
	return ((offset-1)/sector + 1) * sector, nil
}

func readSQLiteRollbackJournalAt(ctx context.Context, file *os.File, offset int64, destination []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.ReadAt(destination, offset); err != nil {
		return err
	}
	return nil
}

func sqliteRollbackJournalPageChecksum(seed uint32, page []byte) uint32 {
	checksum := seed
	for index := len(page) - 200; index > 0; index -= 200 {
		checksum += uint32(page[index])
	}
	return checksum
}
