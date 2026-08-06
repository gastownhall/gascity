package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveSQLiteWALStore is a real WAL-mode SQLite database. Fence admission has to
// hold against the sidecar layouts SQLite itself produces, so these tests drive
// the engine and copy its bytes instead of assembling a WAL by hand.
type liveSQLiteWALStore struct {
	databasePath string
	db           *sql.DB
}

func newLiveSQLiteWALStoreForTest(t *testing.T) *liveSQLiteWALStore {
	t.Helper()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, graphFilename)
	query := url.Values{}
	query.Set("mode", "rwc")
	query.Add("_pragma", "busy_timeout(0)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: databasePath, RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatalf("opening live SQLite WAL store: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil {
		t.Fatalf("enabling WAL mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	if _, err := db.Exec(`CREATE TABLE beads (id INTEGER PRIMARY KEY, payload TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating live SQLite WAL table: %v", err)
	}
	return &liveSQLiteWALStore{databasePath: databasePath, db: db}
}

func (s *liveSQLiteWALStore) commit(t *testing.T, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if _, err := s.db.Exec(`INSERT INTO beads (payload) VALUES (?)`, fmt.Sprintf("row-%d", index)); err != nil {
			t.Fatalf("committing live SQLite row %d: %v", index, err)
		}
	}
}

func (s *liveSQLiteWALStore) checkpoint(t *testing.T, mode string) {
	t.Helper()
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(`+mode+`)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("checkpointing live SQLite WAL (%s): %v", mode, err)
	}
	if busy != 0 {
		t.Fatalf("checkpoint (%s) reported busy", mode)
	}
}

// snapshot copies the three source components into a fresh directory. Tests
// that need to corrupt or rewind a sidecar work on the copy so the live
// connection never observes bytes it did not write.
func (s *liveSQLiteWALStore) snapshot(t *testing.T) *sqliteWALSource {
	t.Helper()
	directory := t.TempDir()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		content, err := os.ReadFile(s.databasePath + suffix)
		if err != nil {
			t.Fatalf("reading live SQLite component %q: %v", suffix, err)
		}
		if err := os.WriteFile(filepath.Join(directory, graphFilename+suffix), content, 0o600); err != nil {
			t.Fatalf("writing SQLite component copy %q: %v", suffix, err)
		}
	}
	return &sqliteWALSource{databasePath: filepath.Join(directory, graphFilename)}
}

// sqliteWALSource is a database plus its WAL sidecars on disk, addressable by
// frame so a test can state exactly which byte it corrupts.
type sqliteWALSource struct {
	databasePath string
}

func (s *sqliteWALSource) walBytes(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(s.databasePath + "-wal")
	if err != nil {
		t.Fatalf("reading WAL: %v", err)
	}
	return content
}

func (s *sqliteWALSource) writeWAL(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(s.databasePath+"-wal", content, 0o600); err != nil {
		t.Fatalf("writing WAL: %v", err)
	}
}

func (s *sqliteWALSource) shmBytes(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(s.databasePath + "-shm")
	if err != nil {
		t.Fatalf("reading WAL-index: %v", err)
	}
	return content
}

func (s *sqliteWALSource) writeSHM(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(s.databasePath+"-shm", content, 0o600); err != nil {
		t.Fatalf("writing WAL-index: %v", err)
	}
}

func (s *sqliteWALSource) pageSize(t *testing.T) int {
	t.Helper()
	header := make([]byte, sqliteDatabaseHeaderBytes)
	file, err := os.Open(s.databasePath)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.ReadAt(header, 0); err != nil {
		t.Fatalf("reading database header: %v", err)
	}
	encoded := binary.BigEndian.Uint16(header[16:18])
	if encoded == 1 {
		return 65536
	}
	return int(encoded)
}

func (s *sqliteWALSource) frameOffset(t *testing.T, frameNumber int) int {
	t.Helper()
	if frameNumber < 1 {
		t.Fatalf("frame number %d is not a frame", frameNumber)
	}
	return sqliteWALHeaderBytes + (frameNumber-1)*(sqliteWALFrameHeaderBytes+s.pageSize(t))
}

func (s *sqliteWALSource) physicalFrames(t *testing.T) int {
	t.Helper()
	return (len(s.walBytes(t)) - sqliteWALHeaderBytes) / (sqliteWALFrameHeaderBytes + s.pageSize(t))
}

func (s *sqliteWALSource) walSalt(t *testing.T) [2]uint32 {
	t.Helper()
	wal := s.walBytes(t)
	return [2]uint32{
		binary.BigEndian.Uint32(wal[16:20]),
		binary.BigEndian.Uint32(wal[20:24]),
	}
}

func (s *sqliteWALSource) frameSalt(t *testing.T, frameNumber int) [2]uint32 {
	t.Helper()
	wal := s.walBytes(t)
	offset := s.frameOffset(t, frameNumber)
	return [2]uint32{
		binary.BigEndian.Uint32(wal[offset+8 : offset+12]),
		binary.BigEndian.Uint32(wal[offset+12 : offset+16]),
	}
}

func (s *sqliteWALSource) indexMaxFrame(t *testing.T) uint32 {
	t.Helper()
	return binary.NativeEndian.Uint32(s.shmBytes(t)[16:20])
}

func (s *sqliteWALSource) plan(t *testing.T) (sqliteWriterReservationPlan, error) {
	t.Helper()
	return sqliteWriterReservationPlanFor(
		context.Background(),
		openSQLitePlanComponentForTest(t, s.databasePath),
		openSQLitePlanComponentForTest(t, s.databasePath+"-wal"),
		openSQLitePlanComponentForTest(t, s.databasePath+"-shm"),
		nil,
	)
}

func (s *sqliteWALSource) requireLiveWALPlan(t *testing.T) {
	t.Helper()
	plan, err := s.plan(t)
	if err != nil {
		t.Fatalf("sqliteWriterReservationPlanFor(): %v", err)
	}
	if want := (sqliteWriterReservationPlan{lockWALIndex: true}); plan != want {
		t.Fatalf("sqliteWriterReservationPlanFor() = %#v, want %#v", plan, want)
	}
}

func (s *sqliteWALSource) requirePlanRejection(t *testing.T, details ...string) {
	t.Helper()
	_, err := s.plan(t)
	if err == nil {
		t.Fatal("sqliteWriterReservationPlanFor() unexpectedly succeeded")
	}
	for _, detail := range details {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("sqliteWriterReservationPlanFor() error = %v, want detail %q", err, detail)
		}
	}
}

// TestSQLiteWriterReservationPlanAcceptsRestartedWAL covers the steady state of
// every long-lived WAL store: once a checkpoint has copied the whole log back
// into the database, the next commit restarts the log at frame 1 under fresh
// salts and the frames it does not overwrite keep the previous salts forever.
// SQLite stops reading at that discontinuity, so the fence must too.
func TestSQLiteWriterReservationPlanAcceptsRestartedWAL(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 40)
	store.checkpoint(t, "FULL")
	store.commit(t, 1)
	source := store.snapshot(t)

	// Assert the fixture really is a restarted WAL. Without this the test would
	// keep passing if a future SQLite release truncated the log instead.
	maxFrame := source.indexMaxFrame(t)
	if maxFrame == 0 {
		t.Fatal("restarted WAL fixture advertises no frames")
	}
	physical := source.physicalFrames(t)
	if physical <= int(maxFrame) {
		t.Fatalf("restarted WAL fixture holds %d physical frames, want more than max frame %d", physical, maxFrame)
	}
	if stale, header := source.frameSalt(t, int(maxFrame)+1), source.walSalt(t); stale == header {
		t.Fatalf("frame %d salt %v matches the WAL header, so no restart happened", maxFrame+1, header)
	}

	source.requireLiveWALPlan(t)
}

// TestSQLiteWriterReservationPlanAcceptsRestartedRecoveryWAL covers the same
// restarted log left without its WAL-index, the shape a crashed writer leaves
// behind. Nothing advertises a committed prefix, so the plan falls back to the
// pending byte and the stale tail stays out of the way.
func TestSQLiteWriterReservationPlanAcceptsRestartedRecoveryWAL(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 40)
	store.checkpoint(t, "FULL")
	store.commit(t, 1)
	source := store.snapshot(t)
	if err := os.Remove(source.databasePath + "-shm"); err != nil {
		t.Fatalf("removing WAL-index: %v", err)
	}

	plan, err := sqliteWriterReservationPlanFor(
		context.Background(),
		openSQLitePlanComponentForTest(t, source.databasePath),
		openSQLitePlanComponentForTest(t, source.databasePath+"-wal"),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("sqliteWriterReservationPlanFor(): %v", err)
	}
	if want := (sqliteWriterReservationPlan{lockPending: true}); plan != want {
		t.Fatalf("sqliteWriterReservationPlanFor() = %#v, want %#v", plan, want)
	}
}

// TestSQLiteWriterReservationPlanAcceptsTruncatedWAL covers the other routine
// post-checkpoint shape: wal_checkpoint(TRUNCATE) empties the log file and
// leaves a WAL-index that claims no frames.
func TestSQLiteWriterReservationPlanAcceptsTruncatedWAL(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 10)
	store.checkpoint(t, "TRUNCATE")
	source := store.snapshot(t)

	if size := len(source.walBytes(t)); size != 0 {
		t.Fatalf("truncated WAL fixture is %d bytes, want 0", size)
	}
	if maxFrame := source.indexMaxFrame(t); maxFrame != 0 {
		t.Fatalf("truncated WAL fixture advertises max frame %d, want 0", maxFrame)
	}

	source.requireLiveWALPlan(t)
}

// TestSQLiteWriterReservationPlanRejectsTruncatedWALWithClaimedFrames keeps the
// empty-log admission fail-closed: an index that still claims frames
// contradicts the file it describes.
func TestSQLiteWriterReservationPlanRejectsTruncatedWALWithClaimedFrames(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 10)
	source := store.snapshot(t)
	if maxFrame := source.indexMaxFrame(t); maxFrame == 0 {
		t.Fatal("fixture advertises no frames")
	}
	source.writeWAL(t, nil)

	source.requirePlanRejection(t, "is beyond the valid WAL chain", "truncated to zero frames")
}

// TestSQLiteWriterReservationPlanRejectsCorruptionInsideCommittedPrefix proves
// that reading a chain break as end-of-log does not weaken the committed
// prefix: a frame the WAL-index vouches for must still validate completely.
func TestSQLiteWriterReservationPlanRejectsCorruptionInsideCommittedPrefix(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		corrupt    func(t *testing.T, source *sqliteWALSource, frameOffset int, wal []byte)
		terminator string
	}{
		{
			name: "zero page number",
			corrupt: func(_ *testing.T, _ *sqliteWALSource, frameOffset int, wal []byte) {
				binary.BigEndian.PutUint32(wal[frameOffset:frameOffset+4], 0)
			},
			terminator: "has zero page number",
		},
		{
			name: "frame salt mismatch",
			corrupt: func(_ *testing.T, _ *sqliteWALSource, frameOffset int, wal []byte) {
				wal[frameOffset+8] ^= 0xff
			},
			terminator: "salts do not match WAL header",
		},
		{
			name: "frame checksum break",
			corrupt: func(t *testing.T, source *sqliteWALSource, frameOffset int, wal []byte) {
				wal[frameOffset+sqliteWALFrameHeaderBytes+source.pageSize(t)/2] ^= 0xff
			},
			terminator: "checksum is invalid",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := newLiveSQLiteWALStoreForTest(t)
			store.commit(t, 10)
			source := store.snapshot(t)

			const corruptedFrame = 2
			maxFrame := source.indexMaxFrame(t)
			if maxFrame <= corruptedFrame {
				t.Fatalf("fixture advertises max frame %d, want more than %d", maxFrame, corruptedFrame)
			}
			source.requireLiveWALPlan(t)

			wal := source.walBytes(t)
			scenario.corrupt(t, source, source.frameOffset(t, corruptedFrame), wal)
			source.writeWAL(t, wal)

			source.requirePlanRejection(t,
				fmt.Sprintf("max frame %d is beyond the valid WAL chain", maxFrame),
				fmt.Sprintf("frame %d %s", corruptedFrame, scenario.terminator),
			)
		})
	}
}

// TestSQLiteWriterReservationPlanAcceptsRestartHdrWindow covers the crash
// window inside walRestartLog: walRestartHdr publishes the next log's salts in
// the WAL-index and drops max frame to zero before the writer stamps the
// matching WAL header. SQLite handles the surviving mismatch benignly because
// an index claiming no frames sends every reader to the database file.
func TestSQLiteWriterReservationPlanAcceptsRestartHdrWindow(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 10)
	store.checkpoint(t, "FULL")
	source := store.snapshot(t)
	source.requireLiveWALPlan(t)

	source.writeSHM(t, restartedSQLiteWALIndexHeaderForTest(t, source.shmBytes(t), 0))
	if source.walSalt(t) == sqliteWALIndexSaltForTest(source.shmBytes(t)) {
		t.Fatal("restart window fixture did not advance the WAL-index salts")
	}
	if physical := source.physicalFrames(t); physical == 0 {
		t.Fatal("restart window fixture holds no stale frames")
	}

	source.requireLiveWALPlan(t)
}

// TestSQLiteWriterReservationPlanRejectsSaltMismatchWithClaimedFrames keeps the
// restart-window admission narrow: salts may disagree only while the WAL-index
// claims nothing from the WAL file.
func TestSQLiteWriterReservationPlanRejectsSaltMismatchWithClaimedFrames(t *testing.T) {
	store := newLiveSQLiteWALStoreForTest(t)
	store.commit(t, 10)
	source := store.snapshot(t)

	maxFrame := source.indexMaxFrame(t)
	if maxFrame == 0 {
		t.Fatal("fixture advertises no frames")
	}
	source.writeSHM(t, restartedSQLiteWALIndexHeaderForTest(t, source.shmBytes(t), maxFrame))

	source.requirePlanRejection(t, "salts do not match WAL")
}

// restartedSQLiteWALIndexHeaderForTest rewrites a real WAL-index header the way
// walRestartHdr does: salt-1 increments, salt-2 is replaced, max frame is
// overridden, and the header checksum and its duplicate copy are rebuilt.
func restartedSQLiteWALIndexHeaderForTest(t *testing.T, shm []byte, maxFrame uint32) []byte {
	t.Helper()
	if len(shm) < sqliteWALIndexMinimumSize {
		t.Fatalf("WAL-index is %d bytes, want at least %d", len(shm), sqliteWALIndexMinimumSize)
	}
	restarted := append([]byte(nil), shm...)
	header := restarted[:sqliteWALIndexHeaderBytes]
	binary.NativeEndian.PutUint32(header[16:20], maxFrame)
	binary.BigEndian.PutUint32(header[32:36], binary.BigEndian.Uint32(header[32:36])+1)
	binary.BigEndian.PutUint32(header[36:40], 0x5eed5a17)
	checksum := sqliteRollingChecksum(binary.NativeEndian, header[:40], [2]uint32{})
	binary.NativeEndian.PutUint32(header[40:44], checksum[0])
	binary.NativeEndian.PutUint32(header[44:48], checksum[1])
	copy(restarted[sqliteWALIndexHeaderBytes:sqliteWALIndexHeaderBytes*2], header)
	return restarted
}

func sqliteWALIndexSaltForTest(shm []byte) [2]uint32 {
	return [2]uint32{
		binary.BigEndian.Uint32(shm[32:36]),
		binary.BigEndian.Uint32(shm[36:40]),
	}
}
