package beads

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestSQLiteStoreReadOnlyEscapesURIPathsAndPreservesDatabaseAndWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source ? # % spaces")
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore writer: %v", err)
	}
	writer := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = writer.CloseStore() })
	if _, err := writer.Create(Bead{Title: "live WAL source"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	before := snapshotSQLiteReadOnlySource(t, dir)
	readOnly, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore read-only: %v", err)
	}
	reader := readOnly.(*SQLiteStore)
	rows, err := reader.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		_ = reader.CloseStore()
		t.Fatalf("List through read-only store: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "live WAL source" {
		_ = reader.CloseStore()
		t.Fatalf("read-only rows = %#v, want live source", rows)
	}
	if err := reader.CloseStore(); err != nil {
		t.Fatalf("CloseStore read-only: %v", err)
	}
	after := snapshotSQLiteReadOnlySource(t, dir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only SQLite open mutated its database or WAL:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

type sqliteReadOnlySourceSnapshot struct {
	Directory sqliteReadOnlyFileSnapshot
	Entries   []string
	Files     map[string]sqliteReadOnlyFileSnapshot
}

type sqliteReadOnlyFileSnapshot struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
	Hash    string
}

func snapshotSQLiteReadOnlySource(t *testing.T, dir string) sqliteReadOnlySourceSnapshot {
	t.Helper()
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat source directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read source directory: %v", err)
	}
	snapshot := sqliteReadOnlySourceSnapshot{
		Directory: sqliteReadOnlyFileSnapshot{Mode: dirInfo.Mode(), ModTime: dirInfo.ModTime()},
		Files:     make(map[string]sqliteReadOnlyFileSnapshot, 3),
	}
	for _, entry := range entries {
		snapshot.Entries = append(snapshot.Entries, entry.Name())
	}
	sort.Strings(snapshot.Entries)
	for _, suffix := range []string{"", "-wal"} {
		path := filepath.Join(dir, sqliteStoreFilename+suffix)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat live SQLite source %s: %v", suffix, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read live SQLite source %s: %v", suffix, err)
		}
		hash := sha256.Sum256(content)
		snapshot.Files[suffix] = sqliteReadOnlyFileSnapshot{
			Mode:    info.Mode(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Hash:    hex.EncodeToString(hash[:]),
		}
	}
	return snapshot
}
