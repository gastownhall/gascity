package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// VacuumSQLiteInto writes an exact copy of the SQLite database at src to dst
// with VACUUM INTO. The caller proves the copy (SQLiteIntegrityCheck).
//
// The copy runs over a mode=ro connection, so it can neither write src nor
// checkpoint its WAL, and it is one read transaction: a writer committing to a
// live WAL database meanwhile is not in the copy, and nothing it commits is
// half in it. dst must not exist (VACUUM INTO refuses a non-empty file). The
// copy is fsynced before the check: VACUUM INTO does not sync its output.
func VacuumSQLiteInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", sqliteStoreDSN(src, true))
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
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("integrity check of %s: %w", path, err)
	}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("integrity check of %s: %w", path, err)
	}
	defer db.Close() //nolint:errcheck // read-only handle
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("integrity check of %s: %w", path, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("integrity check of %s: %w", path, err)
		}
		problems = append(problems, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("integrity check of %s: %w", path, err)
	}
	if len(problems) == 1 && problems[0] == "ok" {
		return nil
	}
	if len(problems) == 0 {
		return fmt.Errorf("integrity check of %s: %w", path, errors.New("no answer"))
	}
	return fmt.Errorf("integrity check of %s failed: %s", path, strings.Join(problems, "; "))
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

// CheckpointSQLiteWAL folds the WAL of the database at path into the
// database and truncates it (PRAGMA wal_checkpoint(TRUNCATE)), so the main
// file alone holds every committed row. It writes, so only a caller that
// holds the database's writers off may call it.
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
	if busy != 0 {
		return fmt.Errorf("checkpointing %s: another connection holds the database", path)
	}
	return nil
}
