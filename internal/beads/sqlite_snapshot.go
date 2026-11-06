package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// VacuumSQLiteInto writes an exact copy of the SQLite database at src to dst
// with VACUUM INTO. The caller proves the copy (SQLiteIntegrityCheck).
//
// The copy runs over a mode=ro connection, so it can neither write src nor
// checkpoint its WAL, and it is one read transaction: a writer committing to a
// live WAL database meanwhile is not in the copy, and nothing it commits is
// half in it. dst must not exist (VACUUM INTO refuses a non-empty file). The
// copy is fsynced: VACUUM INTO does not sync its output.
//
// The copy is in rollback-journal mode (journal_mode=delete), whatever src's
// mode, and that is kept on purpose: an archived snapshot is one
// self-contained file, with no -wal or -shm beside it to lose when it is
// copied, and it is only ever read immutable. A restore that installs it as a
// live binding switches it to WAL first (SetSQLiteJournalModeWAL).
func VacuumSQLiteInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", sqliteSnapshotSourceDSN(src))
	if err != nil {
		return fmt.Errorf("opening %s for a snapshot: %w", src, err)
	}
	defer db.Close() //nolint:errcheck // read-only handle
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("VACUUM INTO %s: %w", dst, err)
	}
	if err := syncFile(dst); err != nil {
		return fmt.Errorf("syncing the snapshot %s: %w", dst, err)
	}
	return nil
}

// SQLiteIntegrityCheck runs PRAGMA integrity_check on the database at path,
// opened read-only and immutable (nothing else may have it open), and returns
// an error naming every problem it reports. A database that answers anything
// but the single row "ok" fails.
func SQLiteIntegrityCheck(ctx context.Context, path string) error {
	var problems []string
	err := withImmutableSQLite(path, func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only cursor
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			problems = append(problems, line)
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if len(problems) == 1 && problems[0] == "ok" {
		return nil
	}
	if len(problems) == 0 {
		return fmt.Errorf("integrity check of %s: no answer", path)
	}
	return fmt.Errorf("integrity check of %s failed: %s", path, strings.Join(problems, "; "))
}

// sqliteSnapshotSourceDSN is the read-only DSN a snapshot reads its source
// through, and a restore the live database it checks. It is the store's
// read-only DSN without the mmap window: a copy reads every page once, in
// order, so mapping the file buys nothing and would count up to the whole
// window against the process's resident set.
func sqliteSnapshotSourceDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "mmap_size(0)")
	query.Set("mode", sqliteStoreDSNReadOnlyMode)
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ErrSQLiteCheckpointBusy is CheckpointSQLiteWAL's refusal when another
// connection holds the database: the WAL could not be folded in whole.
var ErrSQLiteCheckpointBusy = errors.New("another connection holds the database")

// CheckpointSQLiteWAL folds the WAL of the database at path into the
// database and truncates it (PRAGMA wal_checkpoint(TRUNCATE)), so the main
// file alone holds every committed row. It writes, so only a caller that
// holds the database's writers off may call it. A checkpoint another
// connection blocks is ErrSQLiteCheckpointBusy.
func CheckpointSQLiteWAL(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", sqliteStoreDSN(path, false))
	if err != nil {
		return fmt.Errorf("checkpointing %s: %w", path, err)
	}
	defer db.Close() //nolint:errcheck // closing the last connection deletes the empty WAL
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpointing %s: %w", path, err)
	}
	if busy != 0 || (logFrames >= 0 && checkpointed < logFrames) {
		return fmt.Errorf("checkpointing %s: %w", path, ErrSQLiteCheckpointBusy)
	}
	return nil
}

// SetSQLiteJournalModeWAL puts the database at path in WAL mode and proves
// it. VACUUM INTO writes its copy in rollback-journal mode, and a store opened
// over an existing database never changes the mode, so a copy installed as a
// live binding without this would make every reader stall its writers.
func SetSQLiteJournalModeWAL(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", sqliteStoreDSN(path, false))
	if err != nil {
		return fmt.Errorf("setting WAL mode on %s: %w", path, err)
	}
	defer db.Close() //nolint:errcheck // nothing written but the header
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("setting WAL mode on %s: %w", path, err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("setting WAL mode on %s: the database answered journal_mode %q", path, mode)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("setting WAL mode on %s: %w", path, err)
	}
	wal, err := SQLiteFileIsWAL(path)
	if err != nil {
		return fmt.Errorf("setting WAL mode on %s: %w", path, err)
	}
	if !wal {
		return fmt.Errorf("setting WAL mode on %s: the header does not record WAL", path)
	}
	return nil
}

// SQLiteFileIsWAL reports whether the database file at path is in WAL mode,
// from its header (the read and write format versions are 2 in WAL mode, 1 in
// rollback-journal mode): the mode a store opened over it will use. It opens
// no connection, so it reads a live database as safely as a closed one.
func SQLiteFileIsWAL(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // read-only
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return false, fmt.Errorf("reading the header of %s: %w", path, err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return false, fmt.Errorf("%s is not a SQLite database", path)
	}
	return header[18] == 2 && header[19] == 2, nil
}

// SQLiteBeadIDs lists every bead id of the database at path, sorted, so a
// reader can page through it by id rather than hold every row at once.
// Nothing else may have the database open (it is read immutable).
func SQLiteBeadIDs(ctx context.Context, path string) ([]string, error) {
	var ids []string
	err := withImmutableSQLite(path, func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, "SELECT id FROM beads ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only cursor
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// SQLiteMetadataValues lists, sorted, the distinct values the beads of the
// database at path hold under metadata key, both tiers and closed rows
// included. It reads them from the metadata index without reading a row, over
// a read-only connection, so a live database is read with its WAL.
func SQLiteMetadataValues(ctx context.Context, path, key string) ([]string, error) {
	db, err := sql.Open("sqlite", sqliteSnapshotSourceDSN(path))
	if err != nil {
		return nil, fmt.Errorf("reading the %s values of %s: %w", key, path, err)
	}
	defer db.Close() //nolint:errcheck // read-only handle
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT meta_value FROM metadata WHERE meta_key = ? ORDER BY meta_value", key)
	if err != nil {
		return nil, fmt.Errorf("reading the %s values of %s: %w", key, path, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("reading the %s values of %s: %w", key, path, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the %s values of %s: %w", key, path, err)
	}
	return values, nil
}

// SQLiteSequenceHighWaterAt is the latest auto id under prefix the database
// at path holds, in allocation order: a wrapped store's negative ids rank
// above every positive one (sequenceMax). It is read immutable.
func SQLiteSequenceHighWaterAt(path, prefix string) (int64, error) {
	var hw sqliteSequenceHighWater
	err := withImmutableSQLite(path, func(db *sql.DB) error {
		var err error
		hw, err = scanSQLiteSequenceHighWater(context.Background(), db, prefix)
		return err
	})
	if err != nil {
		return 0, err
	}
	if hw.hasNegative {
		return sequenceMax(hw.positive, hw.negative), nil
	}
	return hw.positive, nil
}

// SQLiteSequenceFloorAt reads the persisted id floor of prefix in the store
// directory dir (zero when there is none).
func SQLiteSequenceFloorAt(dir, prefix string) (int64, error) {
	return readSQLiteSequenceFloor(filepath.Join(dir, sqliteSequenceFloorFilenameFor(prefix)))
}

// RaiseSQLiteSequenceFloorPast raises the persisted id floor of prefix in dir
// to the latest, in allocation order, of its current value, floors and the
// high-water of each database in databases (one that cannot be read is
// skipped: it may be the damaged database being replaced). It never lowers the
// floor, and returns the floor it persisted. A store opened over dir
// afterwards mints past every id those databases held, a wrapped range
// included.
func RaiseSQLiteSequenceFloorPast(dir, prefix string, floors []int64, databases ...string) (int64, error) {
	path := filepath.Join(dir, sqliteSequenceFloorFilenameFor(prefix))
	floor, err := readSQLiteSequenceFloor(path)
	if err != nil {
		return 0, err
	}
	for _, f := range floors {
		floor = sequenceMax(floor, f)
	}
	for _, db := range databases {
		if hw, err := SQLiteSequenceHighWaterAt(db, prefix); err == nil {
			floor = sequenceMax(floor, hw)
		}
	}
	return persistSQLiteSequenceFloorAtLeast(path, floor)
}

func withImmutableSQLite(path string, fn func(*sql.DB) error) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String())
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer db.Close() //nolint:errcheck // read-only handle
	if err := fn(db); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}
