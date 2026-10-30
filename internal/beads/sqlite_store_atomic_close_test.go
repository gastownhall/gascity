package beads

import (
	"errors"
	"testing"
)

// TestSQLiteStoreAtomicCloseRefusedOnLayoutWithoutRevisionColumn pins the
// capability gate on the legacy layout. Without a revision column the store
// cannot fence, so AtomicConditionalCloserFor must answer no (callers keep
// their documented non-atomic fallback) and a direct call must refuse as
// unsupported without writing.
func TestSQLiteStoreAtomicCloseRefusedOnLayoutWithoutRevisionColumn(t *testing.T) {
	dir := t.TempDir()
	createSQLiteSchemaFixture(t, dir, false, false, nil)
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	if store.hasRevisionColumn {
		t.Fatal("fixture unexpectedly carries the revision column")
	}

	if closer, ok := AtomicConditionalCloserFor(store); ok || closer != nil {
		t.Fatalf("AtomicConditionalCloserFor(layout without revision) = (%v, %v), want (nil, false)", closer, ok)
	}
	created, err := store.Create(Bead{Title: "legacy close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"}); !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("CloseWithMetadataIfMatch = %v, want ErrConditionalWriteUnsupported", err)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != "open" || after.Metadata["state"] != "" {
		t.Fatalf("refused close mutated the row: status %q state %q", after.Status, after.Metadata["state"])
	}
}

// TestSQLiteStoreAtomicCloseFencesAcrossConnections proves the fence holds
// between two stores on one database file (two processes, in production):
// the revision is read inside the writing transaction, so a write committed
// through the other connection after this side's observation is refused.
func TestSQLiteStoreAtomicCloseFencesAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	open := func() *SQLiteStore {
		opened, err := OpenSQLiteStore(dir)
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		store := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}
	closerSide, writerSide := open(), open()

	created, err := closerSide.Create(Bead{Title: "cross-connection close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	observed, err := closerSide.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := writerSide.SetMetadata(created.ID, "state", "awake"); err != nil {
		t.Fatalf("SetMetadata from the other connection: %v", err)
	}

	_, err = closerSide.CloseWithMetadataIfMatch(created.ID, observed.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("close fenced on a revision the other connection moved = %v, want *PreconditionFailedError", err)
	}
	after, err := writerSide.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != "open" || after.Metadata["state"] != "awake" {
		t.Fatalf("refused close left status %q state %q, want open/awake", after.Status, after.Metadata["state"])
	}
}
