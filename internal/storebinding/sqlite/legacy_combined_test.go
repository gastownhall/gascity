package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestOpenLegacyCombinedSourceUsesOnlyHistoricalInfraPath(t *testing.T) {
	city := t.TempDir()
	other, err := beads.OpenSQLiteStore(filepath.Join(city, ".gc"), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening non-historical source: %v", err)
	}
	if err := other.(*beads.SQLiteStore).CloseStore(); err != nil {
		t.Fatalf("closing non-historical source: %v", err)
	}

	_, err = openTestLegacyCombinedSource(t, city, 1, nil)
	if !errors.Is(err, ErrLegacyCombinedSourceNotFound) {
		t.Fatalf("OpenLegacyCombinedSource error = %v, want ErrLegacyCombinedSourceNotFound", err)
	}
}

func TestLegacyCombinedSourceClassifiesAndPreservesRecordsWithoutMutatingSource(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	city := t.TempDir()
	source := openLegacyCombinedWriter(t, city)
	work, err := source.Create(beads.Bead{ID: "gcg-1", Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("creating work bead: %v", err)
	}
	graph, err := source.Create(beads.Bead{
		ID:       "gcg-2",
		Title:    "graph",
		Type:     "step",
		Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "gcg-root"},
	})
	if err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := source.DepAdd(graph.ID, work.ID, "blocks"); err != nil {
		t.Fatalf("adding graph dependency: %v", err)
	}
	if _, err := source.Create(beads.Bead{ID: "gcg-3", Title: "session", Type: "session"}); err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	if _, err := source.Create(beads.Bead{ID: "gcg-4", Title: "mail", Type: "message"}); err != nil {
		t.Fatalf("creating messaging bead: %v", err)
	}
	if _, err := source.Create(beads.Bead{ID: "gcg-5", Title: "order", Type: "task", Labels: []string{"order-tracking"}}); err != nil {
		t.Fatalf("creating order bead: %v", err)
	}
	if _, err := source.Create(beads.Bead{ID: "gcg-6", Title: "nudge", Type: "chore", Labels: []string{"gc:nudge"}}); err != nil {
		t.Fatalf("creating nudge bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing legacy source writer: %v", err)
	}

	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	info, err := os.Stat(sourceDir)
	if err != nil {
		t.Fatalf("stat legacy source directory: %v", err)
	}
	if err := os.Chmod(sourceDir, 0o500); err != nil {
		t.Fatalf("making legacy source directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, info.Mode()) })
	before := snapshotLegacyCombinedSource(t, sourceDir)
	reader, err := openTestLegacyCombinedSource(t, city, 2, nil)
	if err != nil {
		t.Fatalf("OpenLegacyCombinedSource: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	gotClasses := make(map[string]coordclass.Class, len(records))
	for _, record := range records {
		gotClasses[record.Bead.ID] = record.Class
	}
	wantClasses := map[string]coordclass.Class{
		"gcg-1": coordclass.ClassWork,
		"gcg-2": coordclass.ClassGraph,
		"gcg-3": coordclass.ClassSessions,
		"gcg-4": coordclass.ClassMessaging,
		"gcg-5": coordclass.ClassOrders,
		"gcg-6": coordclass.ClassNudges,
	}
	if !reflect.DeepEqual(gotClasses, wantClasses) {
		t.Fatalf("classified records = %#v, want %#v", gotClasses, wantClasses)
	}
	graphRecords, err := reader.ReadClass(coordclass.ClassGraph)
	if err != nil {
		t.Fatalf("ReadClass(graph): %v", err)
	}
	if len(graphRecords) != 1 || graphRecords[0].ID != graph.ID || len(graphRecords[0].Dependencies) != 1 || graphRecords[0].Dependencies[0].DependsOnID != work.ID {
		t.Fatalf("graph records = %#v, want graph dependency preserved", graphRecords)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("closing legacy source reader: %v", err)
	}
	after := snapshotLegacyCombinedSource(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy reader mutated its source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestLegacyCombinedSourceReopensPrivateSnapshotReadOnlyAfterRecovery(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	city := t.TempDir()
	source := openLegacyCombinedWriter(t, city)
	if _, err := source.Create(beads.Bead{ID: "gcg-read-only-snapshot", Title: "private recovery", Type: "task"}); err != nil {
		t.Fatalf("creating legacy source bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing legacy source writer: %v", err)
	}

	reader, err := openTestLegacyCombinedSource(t, city, 3, nil)
	if err != nil {
		t.Fatalf("opening legacy combined source: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if _, err := reader.store.Create(beads.Bead{ID: "gcg-private-write", Title: "must not write", Type: "task"}); err == nil {
		t.Fatal("legacy combined source retained a writable private recovery handle")
	}
}

func TestLegacyCombinedSourceCloseRetriesStoreCleanupBeforeRemovingSnapshot(t *testing.T) {
	snapshotDir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(snapshotDir, beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening private snapshot store: %v", err)
	}
	store, ok := opened.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("private snapshot store type = %T, want *beads.SQLiteStore", opened)
	}
	closeFailure := errors.New("transient private snapshot close failure")
	closeCalls := 0
	source := &LegacyCombinedSource{
		store:       store,
		snapshotDir: snapshotDir,
		closeStore: func(candidate *beads.SQLiteStore) error {
			closeCalls++
			if closeCalls == 1 {
				return closeFailure
			}
			return candidate.CloseStore()
		},
	}
	t.Cleanup(func() { _ = source.Close() })

	if err := source.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("first Close() error = %v, want transient store close failure", err)
	}
	if _, err := os.Stat(snapshotDir); err != nil {
		t.Fatalf("snapshot removed after failed store close: %v", err)
	}
	if source.store == nil || !source.cleanupPending || source.closed {
		t.Fatalf("source state after failed close = store:%p cleanupPending:%v closed:%v, want retained pending cleanup", source.store, source.cleanupPending, source.closed)
	}
	if _, err := source.ReadAll(); !errors.Is(err, ErrLegacyCombinedSourceClosed) {
		t.Fatalf("ReadAll during cleanup-pending state error = %v, want ErrLegacyCombinedSourceClosed", err)
	}

	if err := source.Close(); err != nil {
		t.Fatalf("retrying Close(): %v", err)
	}
	if closeCalls != 2 {
		t.Fatalf("snapshot store close calls = %d, want 2", closeCalls)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot remained after successful close retry: %v", err)
	}
	if source.store != nil || source.cleanupPending || !source.closed {
		t.Fatalf("source state after successful close = store:%p cleanupPending:%v closed:%v, want closed", source.store, source.cleanupPending, source.closed)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("cached successful Close(): %v", err)
	}
	if closeCalls != 2 {
		t.Fatalf("cached successful close called store again: %d calls", closeCalls)
	}
}

func TestLegacyCombinedFenceSnapshotCancellationCleansPrivateSnapshotRoot(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	city := t.TempDir()
	writer := openLegacyCombinedWriter(t, city)
	if _, err := writer.Create(beads.Bead{ID: "gcg-cancel", Title: "cancellable snapshot", Type: "task"}); err != nil {
		t.Fatalf("creating legacy source bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing legacy source writer: %v", err)
	}

	cityGCDir := filepath.Join(city, ".gc")
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, 45)
	if err != nil {
		t.Fatalf("acquiring migration guard: %v", err)
	}
	request, err := newLegacyCombinedFenceRequest(context.Background(), city, 45)
	if err != nil {
		t.Fatalf("creating legacy fence request: %v", err)
	}
	acquired, err := storebinding.AcquireWriterFence(context.Background(), guard, legacyCombinedFenceAcquirer{}, request)
	if err != nil {
		t.Fatalf("acquiring legacy fence: %v", err)
	}
	t.Cleanup(func() { _ = acquired.Release(context.Background()) })

	temporaryParent := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	originalMakeTemp := makeLegacyCombinedSnapshotTempDir
	makeLegacyCombinedSnapshotTempDir = func(_ string, pattern string) (string, error) {
		root, err := os.MkdirTemp(temporaryParent, pattern)
		if err == nil {
			cancel()
		}
		return root, err
	}
	t.Cleanup(func() { makeLegacyCombinedSnapshotTempDir = originalMakeTemp })
	cleanupFailure := errors.New("injected legacy snapshot cleanup failure")
	originalRemove := removeLegacyCombinedSnapshotRoot
	removeLegacyCombinedSnapshotRoot = func(path string) error {
		if err := originalRemove(path); err != nil {
			return err
		}
		return cleanupFailure
	}
	t.Cleanup(func() { removeLegacyCombinedSnapshotRoot = originalRemove })

	_, err = openLegacyCombinedSnapshot(ctx, acquired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("opening canceled legacy snapshot error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("opening canceled legacy snapshot error = %v, want cleanup failure joined", err)
	}
	entries, err := os.ReadDir(temporaryParent)
	if err != nil {
		t.Fatalf("reading temporary snapshot parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled legacy snapshot left private entries: %#v", entries)
	}
}

func TestLegacyCombinedSourceRejectsAChangedCopyWindow(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	city := t.TempDir()
	source := openLegacyCombinedWriter(t, city)
	t.Cleanup(func() { _ = source.CloseStore() })
	if _, err := source.Create(beads.Bead{ID: "gcg-8", Title: "copy window", Type: "task"}); err != nil {
		t.Fatalf("creating source bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing legacy source writer: %v", err)
	}
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	_, err = openTestLegacyCombinedSource(t, city, 4, func() {
		if err := os.WriteFile(filepath.Join(sourceDir, "graph.seqfloor"), []byte("8\n"), 0o644); err != nil {
			t.Fatalf("changing legacy source during copy: %v", err)
		}
	})
	if !errors.Is(err, ErrLegacyCombinedSourceChanged) {
		t.Fatalf("openLegacyCombinedSource error = %v, want ErrLegacyCombinedSourceChanged", err)
	}
}

func TestCopyLegacyCombinedSnapshotOmitsDerivedWALIndex(t *testing.T) {
	city := t.TempDir()
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("creating legacy source directory: %v", err)
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(2, 2), 0o600); err != nil {
		t.Fatalf("writing legacy database: %v", err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("authoritative WAL"), 0o600); err != nil {
		t.Fatalf("writing legacy WAL: %v", err)
	}
	if err := os.WriteFile(databasePath+"-shm", sqliteSHMHeaderForTest(), 0o600); err != nil {
		t.Fatalf("writing legacy WAL index: %v", err)
	}
	state, err := captureLegacyCombinedSource(sourceDir)
	if err != nil {
		t.Fatalf("capturing legacy source: %v", err)
	}

	destinationDir := filepath.Join(city, "snapshot", ".beads")
	if err := copyLegacyCombinedSnapshot(context.Background(), destinationDir, state, openSQLiteSnapshotFilesForTest(t, sourceDir, legacyCombinedDatabaseFilename)); err != nil {
		t.Fatalf("copying legacy snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationDir, legacyCombinedDatabaseFilename+"-shm")); !os.IsNotExist(err) {
		t.Fatalf("legacy snapshot copied derived WAL index, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationDir, legacyCombinedDatabaseFilename+"-wal")); err != nil {
		t.Fatalf("legacy snapshot omitted authoritative WAL: %v", err)
	}
}

func TestLegacyCombinedFenceComparisonAllowsOnlyWALIndexReaderMarkChurn(t *testing.T) {
	city := t.TempDir()
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("creating legacy source directory: %v", err)
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(2, 2), 0o600); err != nil {
		t.Fatalf("writing legacy database: %v", err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("authoritative WAL"), 0o600); err != nil {
		t.Fatalf("writing legacy WAL: %v", err)
	}
	shmPath := databasePath + "-shm"
	if err := os.WriteFile(shmPath, sqliteSHMHeaderForTest(), 0o600); err != nil {
		t.Fatalf("writing legacy WAL index: %v", err)
	}
	before, err := captureLegacyCombinedSource(sourceDir)
	if err != nil {
		t.Fatalf("capturing legacy source before reader-mark change: %v", err)
	}
	shm, err := os.OpenFile(shmPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening legacy WAL index: %v", err)
	}
	defer shm.Close() //nolint:errcheck
	if _, err := shm.WriteAt([]byte{1}, 100); err != nil {
		t.Fatalf("changing legacy WAL-index reader mark: %v", err)
	}
	// The reader-mark region is deliberately excluded from the WAL-index hash,
	// so the STRICT comparison's sensitivity to this write rests on ModTime
	// alone. A same-tick write is invisible on filesystems with coarse
	// timestamp granularity (CI runners), which is a property of the kernel,
	// not of the comparator. Move the mtime explicitly so the test asserts the
	// comparator's field sensitivity deterministically everywhere.
	markTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(shmPath, markTime, markTime); err != nil {
		t.Fatalf("bumping legacy WAL-index mtime: %v", err)
	}
	afterReadMark, err := captureLegacyCombinedSource(sourceDir)
	if err != nil {
		t.Fatalf("capturing legacy source after reader-mark change: %v", err)
	}
	if before.equal(afterReadMark) {
		t.Fatal("strict legacy source comparison ignored WAL-index churn")
	}
	if !before.equalForFence(afterReadMark) {
		t.Fatal("fenced legacy source comparison rejected WAL-index reader-mark churn")
	}
	if _, err := shm.WriteAt([]byte{1}, 120); err != nil {
		t.Fatalf("changing stable legacy WAL-index payload: %v", err)
	}
	afterStableChange, err := captureLegacyCombinedSource(sourceDir)
	if err != nil {
		t.Fatalf("capturing legacy source after stable WAL-index change: %v", err)
	}
	if afterReadMark.equalForFence(afterStableChange) {
		t.Fatal("fenced legacy source comparison ignored stable WAL-index payload change")
	}
}

func TestCopyLegacyCombinedSnapshotRejectsReplacementAfterSourceCensus(t *testing.T) {
	city := t.TempDir()
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("creating legacy source directory: %v", err)
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing legacy database: %v", err)
	}
	state, err := captureLegacyCombinedSource(sourceDir)
	if err != nil {
		t.Fatalf("capturing legacy source: %v", err)
	}

	replacement := filepath.Join(city, "replacement.sqlite")
	replacementBytes := sqliteHeaderForTest(1, 1)
	replacementBytes[20] = 1
	if err := os.WriteFile(replacement, replacementBytes, 0o600); err != nil {
		t.Fatalf("writing replacement legacy database: %v", err)
	}
	if err := os.Rename(replacement, databasePath); err != nil {
		t.Fatalf("replacing legacy database after census: %v", err)
	}

	err = copyLegacyCombinedSnapshot(context.Background(), filepath.Join(city, "snapshot", ".beads"), state, openSQLiteSnapshotFilesForTest(t, sourceDir, legacyCombinedDatabaseFilename))
	if err == nil || !strings.Contains(err.Error(), "changed after source census") {
		t.Fatalf("copyLegacyCombinedSnapshot() error = %v, want replacement rejection", err)
	}
}

func openLegacyCombinedWriter(t *testing.T, city string) *beads.SQLiteStore {
	t.Helper()
	dir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	opened, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening legacy combined writer: %v", err)
	}
	store, ok := opened.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("legacy combined writer type = %T, want *beads.SQLiteStore", opened)
	}
	return store
}

func openTestLegacyCombinedSource(t *testing.T, city string, generation storebinding.Generation, beforeCopy func()) (*LegacyCombinedSource, error) {
	t.Helper()
	cityGCDir := filepath.Join(city, ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		return nil, err
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, generation)
	if err != nil {
		return nil, err
	}
	reader, openErr := openLegacyCombinedSource(context.Background(), city, guard, generation, beforeCopy)
	releaseErr := guard.Release()
	if openErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(openErr, releaseErr)
		}
		return nil, openErr
	}
	if releaseErr != nil {
		_ = reader.Close()
		return nil, releaseErr
	}
	return reader, nil
}

type legacyCombinedSnapshot struct {
	Directory legacyCombinedFile
	Entries   []string
	Files     map[string]legacyCombinedFile
}

type legacyCombinedFile struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
	Hash    string
}

func snapshotLegacyCombinedSource(t *testing.T, directory string) legacyCombinedSnapshot {
	t.Helper()
	dirInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat legacy source directory: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read legacy source directory: %v", err)
	}
	snapshot := legacyCombinedSnapshot{
		Directory: legacyCombinedFile{Mode: dirInfo.Mode(), ModTime: dirInfo.ModTime()},
		Files:     make(map[string]legacyCombinedFile, len(entries)),
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat legacy source entry %q: %v", entry.Name(), err)
		}
		file := legacyCombinedFile{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		if info.Mode().IsRegular() {
			contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatalf("read legacy source entry %q: %v", entry.Name(), err)
			}
			sum := sha256.Sum256(contents)
			file.Hash = hex.EncodeToString(sum[:])
		}
		snapshot.Entries = append(snapshot.Entries, entry.Name())
		snapshot.Files[entry.Name()] = file
	}
	sort.Strings(snapshot.Entries)
	return snapshot
}
