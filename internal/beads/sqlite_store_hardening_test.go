package beads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteSQLiteSequenceFloorJoinsTemporaryCloseFailure(t *testing.T) {
	tests := []struct {
		name     string
		chmodErr error
		writeErr error
		syncErr  error
	}{
		{name: "chmod", chmodErr: errors.New("chmod failed")},
		{name: "write", writeErr: errors.New("write failed")},
		{name: "sync", syncErr: errors.New("sync failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closeErr := errors.New("temporary close failed")
			tmp := &sqliteSequenceFloorTestFile{
				name:     filepath.Join(t.TempDir(), ".graph.seqfloor-tmp"),
				chmodErr: tt.chmodErr,
				writeErr: tt.writeErr,
				syncErr:  tt.syncErr,
				closeErr: closeErr,
			}
			originalCreateTemp := createSQLiteSequenceFloorTempFile
			createSQLiteSequenceFloorTempFile = func(string, string) (sqliteSequenceFloorFile, error) {
				return tmp, nil
			}
			t.Cleanup(func() { createSQLiteSequenceFloorTempFile = originalCreateTemp })

			err := writeSQLiteSequenceFloor(filepath.Join(t.TempDir(), "graph.seqfloor"), 23)
			primaryErr := tt.chmodErr
			if primaryErr == nil {
				primaryErr = tt.writeErr
			}
			if primaryErr == nil {
				primaryErr = tt.syncErr
			}
			if !errors.Is(err, primaryErr) {
				t.Fatalf("writeSQLiteSequenceFloor() error = %v, want primary %v", err, primaryErr)
			}
			if !errors.Is(err, closeErr) {
				t.Fatalf("writeSQLiteSequenceFloor() error = %v, want temporary close failure joined", err)
			}
			if tmp.closeCalls != 1 {
				t.Fatalf("temporary Close calls = %d, want 1", tmp.closeCalls)
			}
		})
	}
}

func TestWriteSQLiteSequenceFloorReturnsDirectoryCloseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.seqfloor")
	directoryCloseErr := errors.New("directory close failed")
	var directoryCloseCalls int
	originalOpenDirectory := openSQLiteSequenceFloorDirectory
	openSQLiteSequenceFloorDirectory = func(path string) (sqliteSequenceFloorFile, error) {
		directory, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &sqliteSequenceFloorWrappedFile{
			File:       directory,
			closeErr:   directoryCloseErr,
			closeCalls: &directoryCloseCalls,
		}, nil
	}
	t.Cleanup(func() { openSQLiteSequenceFloorDirectory = originalOpenDirectory })

	err := writeSQLiteSequenceFloor(path, 29)
	if !errors.Is(err, directoryCloseErr) {
		t.Fatalf("writeSQLiteSequenceFloor() error = %v, want directory close failure", err)
	}
	if directoryCloseCalls != 1 {
		t.Fatalf("directory Close calls = %d, want 1", directoryCloseCalls)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading renamed sequence floor: %v", err)
	}
	if string(contents) != "29\n" {
		t.Fatalf("renamed sequence floor = %q, want 29\\n", contents)
	}
}

type sqliteSequenceFloorTestFile struct {
	name       string
	chmodErr   error
	writeErr   error
	syncErr    error
	closeErr   error
	closeCalls int
}

func (f *sqliteSequenceFloorTestFile) Name() string { return f.name }

func (f *sqliteSequenceFloorTestFile) Chmod(os.FileMode) error { return f.chmodErr }

func (f *sqliteSequenceFloorTestFile) WriteString(string) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return 0, nil
}

func (f *sqliteSequenceFloorTestFile) Sync() error { return f.syncErr }

func (f *sqliteSequenceFloorTestFile) Close() error {
	f.closeCalls++
	return f.closeErr
}

type sqliteSequenceFloorWrappedFile struct {
	*os.File
	closeErr   error
	closeCalls *int
}

func (f *sqliteSequenceFloorWrappedFile) Close() error {
	*f.closeCalls++
	if err := f.File.Close(); err != nil {
		return err
	}
	return f.closeErr
}

func TestSQLiteStoreTxRollsBackDependencyMetadataAndCloseTogether(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	blocker, err := store.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	seed, err := store.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create seed: %v", err)
	}

	wantErr := errors.New("force graph rollback")
	err = store.Tx("rollback dependency-bearing graph", func(tx Tx) error {
		child, err := tx.Create(Bead{
			Title:    "must disappear",
			Metadata: StringMap{"phase": "transient"},
			Dependencies: []Dep{{
				DependsOnID: blocker.ID,
				Type:        "blocks",
			}},
		})
		if err != nil {
			return err
		}
		if err := tx.SetMetadataBatch(seed.ID, map[string]string{"phase": "also transient"}); err != nil {
			return err
		}
		if err := tx.Close(child.ID); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Tx error = %v, want %v", err, wantErr)
	}

	seed, err = store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get seed: %v", err)
	}
	if _, exists := seed.Metadata["phase"]; exists {
		t.Fatalf("rollback left seed metadata behind: %#v", seed.Metadata)
	}
	var childRows, dependencyRows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM beads WHERE title='must disappear'`).Scan(&childRows); err != nil {
		t.Fatalf("count rollback child: %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM deps`).Scan(&dependencyRows); err != nil {
		t.Fatalf("count rollback dependencies: %v", err)
	}
	if childRows != 0 || dependencyRows != 0 {
		t.Fatalf("rollback left child=%d dependency=%d rows", childRows, dependencyRows)
	}
}

func TestSQLiteStoreDeleteBatchPreservesCloneLocalSidecar(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	created, err := store.Create(Bead{Title: "clone-local state"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetLocalString(created.ID, "last_woke_at", "2026-07-30T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString: %v", err)
	}
	sidecar := filepath.Join(dir, ".beads", "local-strings.json")
	before, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar before DeleteBatch: %v", err)
	}
	if err := store.DeleteBatch([]string{created.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	after, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar after DeleteBatch: %v", err)
	}
	if string(after) != string(before) || !strings.Contains(string(after), created.ID) {
		t.Fatalf("DeleteBatch rewrote clone-local sidecar: before=%q after=%q", before, after)
	}
}

func TestSQLiteStoreGraphSequenceFloorSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	if err := store.SetSequenceFloor(41); err != nil {
		_ = store.CloseStore()
		t.Fatalf("SetSequenceFloor: %v", err)
	}
	first, err := store.Create(Bead{Title: "after floor"})
	if err != nil {
		_ = store.CloseStore()
		t.Fatalf("Create after floor: %v", err)
	}
	if first.ID != "gcg-42" {
		_ = store.CloseStore()
		t.Fatalf("first id after floor = %q, want gcg-42", first.ID)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}

	bytes, err := os.ReadFile(filepath.Join(dir, "graph.seqfloor"))
	if err != nil {
		t.Fatalf("read graph.seqfloor: %v", err)
	}
	if string(bytes) != "41\n" {
		t.Fatalf("graph.seqfloor = %q, want 41\\n", bytes)
	}
	reopened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	secondStore := reopened.(*SQLiteStore)
	t.Cleanup(func() { _ = secondStore.CloseStore() })
	second, err := secondStore.Create(Bead{Title: "after reopen"})
	if err != nil {
		t.Fatalf("Create after reopen: %v", err)
	}
	if second.ID != "gcg-43" {
		t.Fatalf("second id after reopen = %q, want gcg-43", second.ID)
	}
}

func TestSQLiteStoreSetSequenceFloorNeverLowersConcurrentFloor(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening SQLite store: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	firstPersisted := make(chan struct{})
	secondPersisted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	persistCalls := 0
	store.sequenceFloorBeforePersist = func() {
		persistCalls++
		switch persistCalls {
		case 1:
			close(firstPersisted)
			<-releaseFirst
		case 2:
			close(secondPersisted)
			<-releaseSecond
		}
	}

	higherDone := make(chan error, 1)
	go func() { higherDone <- store.SetSequenceFloor(100) }()
	<-firstPersisted
	lowerDone := make(chan error, 1)
	go func() { lowerDone <- store.SetSequenceFloor(50) }()

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-secondPersisted:
		// The unsafe implementation lets the lower request reach its persist
		// point with a stale zero floor. Finish the higher request first so the
		// lower write would prove the regression.
		close(releaseFirst)
		if err := <-higherDone; err != nil {
			t.Fatalf("setting higher floor: %v", err)
		}
		close(releaseSecond)
		if err := <-lowerDone; err != nil {
			t.Fatalf("setting lower floor: %v", err)
		}
	case <-timer.C:
		// A serialized SetSequenceFloor keeps the lower call outside the
		// read/max/write critical section until the higher floor is durable.
		close(releaseFirst)
		if err := <-higherDone; err != nil {
			t.Fatalf("setting higher floor: %v", err)
		}
		<-secondPersisted
		close(releaseSecond)
		if err := <-lowerDone; err != nil {
			t.Fatalf("setting lower floor: %v", err)
		}
	}

	floor, err := store.SequenceFloor()
	if err != nil {
		t.Fatalf("reading persisted sequence floor: %v", err)
	}
	if floor != 100 {
		t.Fatalf("persisted concurrent sequence floor = %d, want never-lowered 100", floor)
	}
}

func TestSQLiteStoreSequenceFloorRejectsNonCanonicalContents(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening SQLite store: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	for _, contents := range []string{"7", " 7\n", "7 \n", "07\n", "7\n\n"} {
		if err := os.WriteFile(filepath.Join(dir, sqliteGraphSequenceFloorFilename), []byte(contents), 0o600); err != nil {
			t.Fatalf("writing malformed sequence floor %q: %v", contents, err)
		}
		if _, err := store.SequenceFloor(); err == nil {
			t.Fatalf("SequenceFloor() accepted non-canonical %q", contents)
		}
	}
}

func TestSQLiteStoreClaimRejectsEmptyAssigneeAndNonClaimableStatus(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	bead, err := store.Create(Bead{Title: "claim target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok, err := store.Claim(bead.ID, " \t "); err == nil || ok {
		t.Fatalf("Claim(empty assignee) = (_, %v, %v), want rejection", ok, err)
	}

	// Both spellings are on purpose: a status string is opaque data here, and
	// the store must refuse every non-claimable one it is handed.
	for _, status := range []string{"closed", "cancelled", "canceled", "expired", "terminal"} { //nolint:misspell // deliberate spelling variant
		t.Run(status, func(t *testing.T) {
			if err := store.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update(%q): %v", status, err)
			}
			before, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get before Claim: %v", err)
			}
			got, ok, err := store.Claim(bead.ID, "worker")
			if err != nil || ok {
				t.Fatalf("Claim(%q) = (%+v, %v, %v), want conflict", status, got, ok, err)
			}
			after, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get after Claim: %v", err)
			}
			if after.Status != before.Status || after.Assignee != before.Assignee || after.Revision != before.Revision {
				t.Fatalf("Claim resurrected %q: before=%+v after=%+v", status, before, after)
			}
		})
	}
}

func TestSQLiteStoreClaimSameOwnerIsNoopAndReturnsCurrentRevision(t *testing.T) {
	store := newSQLiteGraphApplyStore(t, t.TempDir())
	bead, err := store.Create(Bead{Title: "claim target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	claimed, ok, err := store.Claim(bead.ID, "worker")
	if err != nil || !ok {
		t.Fatalf("first Claim = (%+v, %v, %v), want success", claimed, ok, err)
	}
	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after first Claim: %v", err)
	}
	if claimed.Revision != stored.Revision {
		t.Fatalf("first Claim revision = %d, immediate Get = %d", claimed.Revision, stored.Revision)
	}
	if claimed.ClaimFence != stored.ClaimFence || stored.ClaimFence == 0 {
		t.Fatalf("first Claim fence = %d, stored fence = %d; want persisted nonzero fence", claimed.ClaimFence, stored.ClaimFence)
	}

	reclaimed, ok, err := store.Claim(bead.ID, "worker")
	if err != nil || !ok {
		t.Fatalf("same-owner Claim = (%+v, %v, %v), want success no-op", reclaimed, ok, err)
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after same-owner Claim: %v", err)
	}
	if after.Revision != stored.Revision || after.ClaimFence != stored.ClaimFence {
		t.Fatalf("same-owner Claim rewrote bead: before=%+v after=%+v", stored, after)
	}
	if reclaimed.Revision != after.Revision || reclaimed.ClaimFence != after.ClaimFence {
		t.Fatalf("same-owner Claim return = %+v, immediate Get = %+v", reclaimed, after)
	}
}
