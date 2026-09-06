//go:build unix

package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // Registers the pure-Go driver for the private immutable snapshot only.
)

var (
	errLegacySessionsSnapshotSidecar = errors.New("legacy sessions snapshot has authoritative sidecar content")
	errLegacySessionsSnapshotChanged = errors.New("legacy sessions snapshot source changed while reading")
)

// legacySessionsSnapshotAfterCopy is a test seam for the final source census.
var legacySessionsSnapshotAfterCopy = func() {}

// ReadLegacySessionsSnapshot reads canonical <city>/.gc/store/sessions.db as a
// pinned private snapshot. It never SQLite-opens the deployed database.
func ReadLegacySessionsSnapshot(cityPath string) ([]beads.Bead, error) {
	if cityPath == "" {
		return nil, nil
	}
	rawPath := filepath.Join(cityPath, ".gc", "store", "sessions.db")
	if _, err := os.Lstat(rawPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	path, err := legacySessionsCanonicalPath(rawPath)
	if err != nil {
		return nil, err
	}
	copyPath, cleanup, err := legacySessionsPinnedCopy(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", legacySessionsReadOnlyDSN(copyPath))
	if err != nil {
		return nil, fmt.Errorf("opening private legacy sessions snapshot: %w", err)
	}
	defer db.Close() //nolint:errcheck
	if err := legacySessionsValidateSchema(db); err != nil {
		return nil, fmt.Errorf("validating private legacy sessions snapshot: %w", err)
	}
	rows, err := db.Query(`SELECT id,title,bead_type,status,assignee,description,created_at,updated_at,labels,meta FROM sessions ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing private legacy sessions snapshot: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []beads.Bead
	for rows.Next() {
		var r legacySessionsRow
		if err := rows.Scan(&r.id, &r.title, &r.beadType, &r.status, &r.assignee, &r.description, &r.createdAt, &r.updatedAt, &r.labels, &r.meta); err != nil {
			return nil, fmt.Errorf("scanning legacy sessions row: %w", err)
		}
		b, err := r.bead()
		if err != nil {
			return nil, fmt.Errorf("decoding legacy sessions row %q: %w", r.id, err)
		}
		result = append(result, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing private legacy sessions snapshot: %w", err)
	}
	return result, nil
}

func legacySessionsPinnedCopy(path string) (string, func(), error) {
	before, file, err := legacySessionsOpenPinned(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close() //nolint:errcheck
	if err := before.quiescent(); err != nil {
		return "", nil, err
	}
	dir, err := makeRecoverableSQLiteSnapshotTempDir("", "gc-legacy-sessions-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyPath := filepath.Join(dir, "sessions.db")
	dst, err := os.OpenFile(copyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = dst.Close()
		cleanup()
		return "", nil, err
	}
	_, copyErr := io.Copy(dst, file)
	err = errors.Join(copyErr, dst.Sync(), dst.Close())
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copying pinned legacy sessions source: %w", err)
	}
	copyDigest, err := legacySessionsDigestPath(copyPath)
	if err != nil || copyDigest != before.database.digest {
		cleanup()
		if err != nil {
			return "", nil, err
		}
		return "", nil, errLegacySessionsSnapshotChanged
	}
	legacySessionsSnapshotAfterCopy()
	after, err := legacySessionsCensusPinned(path, file)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if !before.equal(after) {
		cleanup()
		return "", nil, errLegacySessionsSnapshotChanged
	}
	if err := after.quiescent(); err != nil {
		cleanup()
		return "", nil, err
	}
	return copyPath, cleanup, nil
}

func legacySessionsOpenPinned(path string) (legacySessionsSourceState, *os.File, error) {
	pathInfo, err := legacySessionsRegularPathInfo(path)
	if err != nil {
		return legacySessionsSourceState{}, nil, err
	}
	if platformFileHasMultipleLinks(pathInfo) {
		return legacySessionsSourceState{}, nil, fmt.Errorf("%w: legacy sessions source %s", ErrSQLiteSourceHardLinked, path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return legacySessionsSourceState{}, nil, fmt.Errorf("opening pinned legacy sessions source: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	state, err := legacySessionsCensusPinned(path, file)
	if err != nil || !os.SameFile(pathInfo, state.database.info) {
		_ = file.Close()
		if err == nil {
			err = errLegacySessionsSnapshotChanged
		}
		return legacySessionsSourceState{}, nil, err
	}
	return state, file, nil
}

func legacySessionsCensusPinned(path string, file *os.File) (legacySessionsSourceState, error) {
	pathInfo, err := legacySessionsRegularPathInfo(path)
	if err != nil {
		return legacySessionsSourceState{}, err
	}
	fdInfo, err := file.Stat()
	if err != nil {
		return legacySessionsSourceState{}, err
	}
	if !os.SameFile(pathInfo, fdInfo) || !fdInfo.Mode().IsRegular() {
		return legacySessionsSourceState{}, errLegacySessionsSnapshotChanged
	}
	if platformFileHasMultipleLinks(fdInfo) {
		return legacySessionsSourceState{}, fmt.Errorf("%w: legacy sessions source %s", ErrSQLiteSourceHardLinked, path)
	}
	database, err := legacySessionsStateFromFD(file, fdInfo)
	if err != nil {
		return legacySessionsSourceState{}, err
	}
	wal, err := legacySessionsSidecarState(path + "-wal")
	if err != nil {
		return legacySessionsSourceState{}, err
	}
	journal, err := legacySessionsSidecarState(path + "-journal")
	if err != nil {
		return legacySessionsSourceState{}, err
	}
	return legacySessionsSourceState{database: database, wal: wal, journal: journal}, nil
}

func legacySessionsCanonicalPath(path string) (string, error) {
	if _, err := legacySessionsRegularPathInfo(path); err != nil {
		return "", err
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("canonicalizing legacy sessions source: %w", err)
	}
	return canonical, nil
}

func legacySessionsRegularPathInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("legacy sessions source %s must be a non-symlink regular file", path)
	}
	return info, nil
}

func legacySessionsStateFromFD(file *os.File, info os.FileInfo) (legacySessionsFileState, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return legacySessionsFileState{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return legacySessionsFileState{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return legacySessionsFileState{present: true, info: info, mode: info.Mode(), size: info.Size(), mtime: info.ModTime().UnixNano(), digest: digest}, nil
}

func legacySessionsSidecarState(path string) (legacySessionsFileState, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return legacySessionsFileState{}, nil
	}
	if err != nil {
		return legacySessionsFileState{}, fmt.Errorf("opening pinned legacy sessions sidecar %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil {
		return legacySessionsFileState{}, fmt.Errorf("stating pinned legacy sessions sidecar %s: %w", path, err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return legacySessionsFileState{}, fmt.Errorf("stating legacy sessions sidecar path %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return legacySessionsFileState{}, fmt.Errorf("legacy sessions sidecar %s must be a non-symlink regular file", path)
	}
	state, err := legacySessionsStateFromFD(file, info)
	if err != nil {
		return legacySessionsFileState{}, fmt.Errorf("hashing pinned legacy sessions sidecar %s: %w", path, err)
	}
	return state, nil
}

func legacySessionsDigestPath(path string) ([sha256.Size]byte, error) {
	b, err := os.ReadFile(path)
	return sha256.Sum256(b), err
}

func legacySessionsReadOnlyDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func legacySessionsValidateSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("legacy sessions schema user_version = %d, want 1", version)
	}
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck
	want := []legacySessionsColumn{{"id", "TEXT", 0, "", 1}, {"title", "TEXT", 1, "''", 0}, {"bead_type", "TEXT", 1, "''", 0}, {"status", "TEXT", 1, "'open'", 0}, {"assignee", "TEXT", 1, "''", 0}, {"description", "TEXT", 1, "''", 0}, {"created_at", "INTEGER", 1, "", 0}, {"updated_at", "INTEGER", 1, "", 0}, {"labels", "TEXT", 1, "'[]'", 0}, {"meta", "TEXT", 1, "'{}'", 0}, {"state", "TEXT", 1, "''", 0}, {"session_name", "TEXT", 1, "''", 0}, {"configured_named_identity", "TEXT", 1, "''", 0}, {"pool_slot", "TEXT", 1, "''", 0}, {"generation", "TEXT", 1, "''", 0}, {"instance_token", "TEXT", 1, "''", 0}, {"pending_create_claim", "TEXT", 1, "''", 0}, {"pending_create_started_at", "TEXT", 1, "''", 0}, {"last_woke_at", "TEXT", 1, "''", 0}}
	var got []legacySessionsColumn
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return err
		}
		if cid != len(got) {
			return fmt.Errorf("legacy sessions schema column cid = %d, want %d", cid, len(got))
		}
		got = append(got, legacySessionsColumn{name, typ, notnull, def.String, pk})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("legacy sessions schema columns = %v, want %v", got, want)
	}
	return nil
}

type legacySessionsRow struct {
	id, title, beadType, status, assignee, description string
	createdAt, updatedAt                               int64
	labels, meta                                       string
}

func (r legacySessionsRow) bead() (beads.Bead, error) {
	var labels []string
	if err := json.Unmarshal([]byte(r.labels), &labels); err != nil {
		return beads.Bead{}, err
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(r.meta), &meta); err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{ID: r.id, Title: r.title, Type: r.beadType, Status: r.status, Assignee: r.assignee, Description: r.description, CreatedAt: unixNanos(r.createdAt), UpdatedAt: unixNanos(r.updatedAt), Labels: labels, Metadata: meta}, nil
}
func unixNanos(n int64) (t time.Time) { return time.Unix(0, n) }

type (
	legacySessionsSourceState struct{ database, wal, journal legacySessionsFileState }
	legacySessionsColumn      struct {
		name, typ string
		notnull   int
		def       string
		pk        int
	}
)

type legacySessionsFileState struct {
	present     bool
	info        os.FileInfo
	mode        os.FileMode
	size, mtime int64
	digest      [sha256.Size]byte
}

func (s legacySessionsSourceState) quiescent() error {
	for _, x := range []struct {
		name string
		file legacySessionsFileState
	}{{"-wal", s.wal}, {"-journal", s.journal}} {
		if x.file.present && x.file.size > 0 {
			return fmt.Errorf("%w: sessions.db%s holds %d bytes", errLegacySessionsSnapshotSidecar, x.name, x.file.size)
		}
	}
	return nil
}

func (s legacySessionsSourceState) equal(other legacySessionsSourceState) bool {
	return s.database.equal(other.database) && s.wal.equal(other.wal) && s.journal.equal(other.journal)
}

func (s legacySessionsFileState) equal(other legacySessionsFileState) bool {
	if s.present != other.present {
		return false
	}
	if !s.present {
		return true
	}
	if s.info == nil || other.info == nil || !os.SameFile(s.info, other.info) {
		return false
	}
	return s.mode == other.mode && s.size == other.size && s.mtime == other.mtime && s.digest == other.digest
}
