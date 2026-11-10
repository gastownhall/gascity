package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
			// The row and its edge in one transaction, so a consistent copy
			// holds both or neither.
			_, err := store.Create(Bead{Title: "during", Needs: []string{"blocks:" + anchor.ID}})
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

// TestSQLiteSnapshotHelpers covers the restore's building blocks: a copy
// switched to WAL records it in its header; the no-mmap read-only store reads
// and refuses writes; ids list sorted; a checkpoint under another connection's
// open read is busy; the id floor only rises.
func TestSQLiteSnapshotHelpers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	for range 3 {
		b, err := store.Create(Bead{Title: "row"})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b.ID)
	}
	src := filepath.Join(dir, sqliteStoreFilename)
	if wal, err := SQLiteFileIsWAL(src); err != nil || !wal {
		t.Fatalf("a store's database is not WAL in its header: %v %v", wal, err)
	}

	// A reader holding a read transaction open keeps the checkpoint from
	// truncating the WAL.
	tx, err := store.(*SQLiteStore).readDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM beads").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Bead{Title: "after the reader"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckpointSQLiteWAL(ctx, src); !errors.Is(err, ErrSQLiteCheckpointBusy) {
		t.Fatalf("checkpoint under an open read = %v, want ErrSQLiteCheckpointBusy", err)
	}
	_ = tx.Rollback()
	_ = store.(*SQLiteStore).CloseStore()

	dst := filepath.Join(t.TempDir(), sqliteStoreFilename)
	if err := VacuumSQLiteInto(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	if wal, _ := SQLiteFileIsWAL(dst); wal {
		t.Fatal("the VACUUM INTO copy is WAL; the snapshot documents rollback-journal")
	}
	ids, err := SQLiteBeadIDs(ctx, dst)
	if err != nil || len(ids) != 4 || !sort.StringsAreSorted(ids) {
		t.Fatalf("SQLiteBeadIDs = %v, %v", ids, err)
	}
	hw, err := SQLiteSequenceHighWaterAt(dst, sqliteDefaultPrefix)
	if err != nil || hw != 4 {
		t.Fatalf("high-water = %d, %v; want 4", hw, err)
	}
	copied, err := OpenSQLiteStore(filepath.Dir(dst), WithSQLiteStoreReadOnlyNoMmap())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := copied.Get(created[0]); err != nil || got.ID != created[0] {
		t.Fatalf("no-mmap read = %v, %v", got.ID, err)
	}
	if _, err := copied.Create(Bead{Title: "a write"}); err == nil {
		t.Fatal("the read-only no-mmap store accepted a write")
	}
	_ = copied.(*SQLiteStore).CloseStore()
	if err := SetSQLiteJournalModeWAL(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if wal, _ := SQLiteFileIsWAL(dst); !wal {
		t.Fatal("SetSQLiteJournalModeWAL left the header in rollback-journal mode")
	}

	floorDir := t.TempDir()
	if got, err := RaiseSQLiteSequenceFloorPast(floorDir, "gcg", []int64{10}); err != nil || got != 10 {
		t.Fatalf("raise to 10 = %d, %v", got, err)
	}
	if got, err := RaiseSQLiteSequenceFloorPast(floorDir, "gcg", []int64{4}); err != nil || got != 10 {
		t.Fatalf("raise to 4 = %d, %v; want 10 (never lowered)", got, err)
	}
	if floor, err := SQLiteSequenceFloorAt(floorDir, "gcg"); err != nil || floor != 10 {
		t.Fatalf("floor = %d, %v; want 10", floor, err)
	}
}

// TestSQLiteMetadataValues reads the distinct values of one metadata key from
// a live database whose rows are still in its WAL: both tiers and closed rows
// count, each value once and sorted, and other keys and unstamped rows do not.
func TestSQLiteMetadataValues(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.(*SQLiteStore).CloseStore() //nolint:errcheck // test cleanup
	for _, b := range []Bead{
		{Title: "committed", Metadata: map[string]string{"stamp": "rig:b"}},
		{Title: "ephemeral", Ephemeral: true, Metadata: map[string]string{"stamp": "rig:a"}},
		{Title: "the same stamp again", Metadata: map[string]string{"stamp": "rig:b"}},
		{Title: "another key", Metadata: map[string]string{"other": "rig:c"}},
		{Title: "unstamped"},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := store.Create(Bead{Title: "closed", Metadata: map[string]string{"stamp": "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dir, sqliteStoreFilename)
	if info, err := os.Stat(database + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the fixture's rows are not in a live WAL: %v", err)
	}
	got, err := SQLiteMetadataValues(ctx, database, "stamp")
	if want := []string{"rig:a", "rig:b", "work"}; err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SQLiteMetadataValues = %v, %v; want %v", got, err, want)
	}
	if got, err := SQLiteMetadataValues(ctx, database, "absent"); err != nil || len(got) != 0 {
		t.Fatalf("values of a key no row holds = %v, %v; want none", got, err)
	}
	if _, err := SQLiteMetadataValues(ctx, filepath.Join(t.TempDir(), sqliteStoreFilename), "stamp"); err == nil {
		t.Fatal("reading a database that does not exist succeeded")
	}
}

// TestRaiseSQLiteSequenceFloorPastAWrappedRange is round-3 C: a database an
// older build wrapped holds negative auto ids, which come after every positive
// one in allocation order. With the floor sidecar gone, the raise must land
// on the wrapped high-water, not the positive one, or the next mint refuses
// (ErrSQLiteSequenceWrapped).
func TestRaiseSQLiteSequenceFloorPastAWrappedRange(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*SQLiteStore)
	for range 3 {
		if _, err := store.Create(Bead{Title: "positive"}); err != nil {
			t.Fatal(err)
		}
	}
	const wrapped = int64(-9223372036853998976)
	if _, err := store.CreateWithForeignID(Bead{ID: fmt.Sprintf("gcg-%d", wrapped), Title: "minted by a wrapping build"}); err != nil {
		t.Fatal(err)
	}
	_ = store.CloseStore()
	floors, _ := filepath.Glob(filepath.Join(dir, "*.seqfloor"))
	for _, f := range floors {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(dir, sqliteStoreFilename)
	if hw, err := SQLiteSequenceHighWaterAt(db, "gcg"); err != nil || hw != wrapped {
		t.Fatalf("high-water = %d, %v; want the wrapped %d", hw, err, wrapped)
	}
	// A wrapped floor recorded elsewhere (a snapshot's) counts as later than
	// any positive one, with no database to read it from.
	if got, err := RaiseSQLiteSequenceFloorPast(t.TempDir(), "gcg", []int64{3, wrapped}); err != nil || got != wrapped {
		t.Fatalf("floor from a recorded wrapped floor = %d, %v; want %d", got, err, wrapped)
	}
	floor, err := RaiseSQLiteSequenceFloorPast(dir, "gcg", []int64{3}, db)
	if err != nil || floor != wrapped {
		t.Fatalf("floor = %d, %v; want the wrapped %d", floor, err, wrapped)
	}
	reopened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.(*SQLiteStore).CloseStore() //nolint:errcheck // test cleanup
	minted, err := reopened.Create(Bead{Title: "after the raise"})
	if err != nil {
		t.Fatalf("minting past the raised floor: %v", err)
	}
	if minted.ID != fmt.Sprintf("gcg-%d", wrapped+1) {
		t.Fatalf("minted %s, want the id after the wrapped high-water", minted.ID)
	}
}
