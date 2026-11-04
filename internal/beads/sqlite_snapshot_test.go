package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestVacuumSQLiteIntoCopiesALiveWALDatabase snapshots a binding while a
// writer keeps committing to it under WAL. The copy must pass
// integrity_check, open as a store, and hold a consistent prefix of the
// writer's rows: every row it holds has its edge, and it holds at least every
// row committed before the snapshot began.
func TestVacuumSQLiteIntoCopiesALiveWALDatabase(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.(interface{ CloseStore() error }).CloseStore() //nolint:errcheck // test cleanup
	anchor, err := store.Create(Bead{Title: "anchor"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		b, err := store.Create(Bead{Title: "before"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(b.ID, anchor.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, sqliteStoreFilename+"-wal")); err != nil {
		t.Fatalf("the fixture is not a live WAL database: %v", err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var writeErr atomic.Value
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			b, err := store.Create(Bead{Title: "during"})
			if err == nil {
				err = store.DepAdd(b.ID, anchor.ID, "blocks")
			}
			if err != nil {
				writeErr.Store(err)
				return
			}
		}
	}()
	dst := filepath.Join(t.TempDir(), sqliteStoreFilename)
	snapErr := VacuumSQLiteInto(context.Background(), filepath.Join(dir, sqliteStoreFilename), dst)
	stop.Store(true)
	wg.Wait()
	if snapErr != nil {
		t.Fatalf("VacuumSQLiteInto under a live writer: %v", snapErr)
	}
	if err, _ := writeErr.Load().(error); err != nil {
		t.Fatalf("the snapshot blocked or broke the live writer: %v", err)
	}
	if err := SQLiteIntegrityCheck(context.Background(), dst); err != nil {
		t.Fatalf("the copy taken under a live writer: %v", err)
	}

	copied, err := OpenSQLiteStore(filepath.Dir(dst), WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatalf("opening the snapshot as a store: %v", err)
	}
	defer copied.(interface{ CloseStore() error }).CloseStore() //nolint:errcheck // test cleanup
	rows, err := copied.List(ListQuery{IncludeClosed: true, TierMode: TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 51 {
		t.Fatalf("the snapshot holds %d rows, fewer than the 51 committed before it began", len(rows))
	}
	for _, b := range rows {
		if b.ID == anchor.ID {
			continue
		}
		deps, err := copied.DepList(b.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 {
			t.Fatalf("snapshot row %s has %d edges, want its 1: the copy is not one transaction", b.ID, len(deps))
		}
	}
}

// TestSQLiteIntegrityCheckRefusesACorruptCopy damages a page of a good copy;
// the check must fail rather than pass a file that merely opens.
func TestSQLiteIntegrityCheckRefusesACorruptCopy(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := store.Create(Bead{Title: strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.(interface{ CloseStore() error }).CloseStore()
	dst := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := VacuumSQLiteInto(context.Background(), filepath.Join(dir, sqliteStoreFilename), dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the middle of the file, past the header and the schema page.
	for i := len(data) / 2; i < len(data)/2+4096 && i < len(data); i++ {
		data[i] = 0xA5
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SQLiteIntegrityCheck(context.Background(), dst); err == nil {
		t.Fatal("integrity_check passed a copy with a damaged page")
	}
}
