package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyPinnedSQLiteSnapshotFileRejectsSameSizeContentChangedAfterCensus(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, graphFilename)
	original := []byte("source")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("stating source: %v", err)
	}
	sum := sha256.Sum256(original)
	expected := sqliteSnapshotExpectation{
		mode:     info.Mode(),
		size:     info.Size(),
		modTime:  info.ModTime(),
		hash:     hex.EncodeToString(sum[:]),
		identity: physicalIdentity(sourcePath, info),
	}

	source, err := os.OpenFile(sourcePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening pinned source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if _, err := source.WriteAt([]byte("mutate"), 0); err != nil {
		t.Fatalf("changing source after census: %v", err)
	}
	if err := source.Sync(); err != nil {
		t.Fatalf("syncing changed source: %v", err)
	}
	if err := os.Chtimes(sourcePath, expected.modTime, expected.modTime); err != nil {
		t.Fatalf("restoring source timestamp: %v", err)
	}
	if after, err := source.Stat(); err != nil {
		t.Fatalf("stating changed pinned source: %v", err)
	} else if after.Mode() != expected.mode || after.Size() != expected.size || !after.ModTime().Equal(expected.modTime) || physicalIdentity(sourcePath, after) != expected.identity {
		t.Fatalf("test fixture did not preserve non-content census facts: %#v", after)
	}

	err = copyPinnedSQLiteSnapshotFile(context.Background(), source, filepath.Join(directory, "snapshot.sqlite"), expected)
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("copyPinnedSQLiteSnapshotFile() error = %v, want censused hash rejection", err)
	}
}

func TestCopyGraphSnapshotMakesReadOnlySourceComponentsOwnerWritable(t *testing.T) {
	for _, sourceMode := range []os.FileMode{0o400, 0o444} {
		t.Run(sourceMode.String(), func(t *testing.T) {
			sourceDir := filepath.Join(t.TempDir(), graphDirectoryName)
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				t.Fatalf("creating source directory: %v", err)
			}
			databasePath := filepath.Join(sourceDir, graphFilename)
			if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
				t.Fatalf("writing source database: %v", err)
			}
			if err := os.Chmod(databasePath, sourceMode); err != nil {
				t.Fatalf("making source database read-only: %v", err)
			}
			if err := os.Chmod(sourceDir, 0o500); err != nil {
				t.Fatalf("making source directory non-writable: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o700) })

			state, err := captureGraphSource(databasePath)
			if err != nil {
				t.Fatalf("capturing read-only source: %v", err)
			}
			pinned := openSQLiteSnapshotFilesForTest(t, sourceDir, graphFilename)
			destinationDir := filepath.Join(t.TempDir(), graphDirectoryName)
			if err := copyGraphSnapshot(context.Background(), sourceDir, destinationDir, state, pinned); err != nil {
				t.Fatalf("copying read-only source snapshot: %v", err)
			}
			info, err := os.Stat(filepath.Join(destinationDir, graphFilename))
			if err != nil {
				t.Fatalf("stating private snapshot database: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("private snapshot mode = %04o, want 0600", got)
			}
			if private, err := os.OpenFile(filepath.Join(destinationDir, graphFilename), os.O_RDWR, 0); err != nil {
				t.Fatalf("opening private snapshot read-write: %v", err)
			} else if err := private.Close(); err != nil {
				t.Fatalf("closing private snapshot: %v", err)
			}
		})
	}
}

// openSQLiteSnapshotFilesForTest pins the component descriptors a snapshot copy
// consumes. graphFilename and legacyCombinedDatabaseFilename are independent
// constants that happen to share a value today, so callers keep naming the
// component they are pinning.
//
//nolint:unparam // databaseName distinguishes two independent component constants.
func openSQLiteSnapshotFilesForTest(t *testing.T, directory, databaseName string) sqliteSnapshotFiles {
	t.Helper()
	files := sqliteSnapshotFiles{}
	for _, component := range []struct {
		name   string
		assign func(*os.File)
	}{
		{name: databaseName, assign: func(file *os.File) { files.database = file }},
		{name: databaseName + "-wal", assign: func(file *os.File) { files.wal = file }},
		{name: databaseName + "-journal", assign: func(file *os.File) { files.journal = file }},
		{name: graphSequenceFloorFilename, assign: func(file *os.File) { files.sequenceFloor = file }},
	} {
		path := filepath.Join(directory, component.name)
		file, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("opening pinned SQLite test source %q: %v", path, err)
		}
		component.assign(file)
		t.Cleanup(func() { _ = file.Close() })
	}
	return files
}
