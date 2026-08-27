package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSQLiteStoreProvidesTheAtomicTerminalCloser pins the second capability the
// keyed pool drain-ack requires before it will own an admission: the fused
// metadata-and-close write. Without it the admission hands every drain back to
// the legacy path, whatever the fence resolves to.
func TestSQLiteStoreProvidesTheAtomicTerminalCloser(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.(*beads.SQLiteStore).CloseStore() })

	closer, ok := beads.AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatal("the sqlite store advertises no atomic terminal closer; keyed drain-ack hands back to legacy on every admission")
	}

	created, err := store.Create(beads.Bead{Title: "drain me", Type: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"gc.drain": "acked"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("returned bead status = %q, want closed", closed.Status)
	}
	if closed.Metadata["gc.drain"] != "acked" {
		t.Errorf("returned metadata = %v, want gc.drain=acked", closed.Metadata)
	}
	if !beads.RevisionKnown(closed.Revision) || closed.Revision == created.Revision {
		t.Errorf("returned revision = %d, want a fresh revision after the fused close (was %d)", closed.Revision, created.Revision)
	}

	reread, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after atomic close: %v", err)
	}
	if reread.Status != "closed" || reread.Metadata["gc.drain"] != "acked" {
		t.Errorf("stored bead = status %q metadata %v, want the metadata and the close committed together", reread.Status, reread.Metadata)
	}

	// The fence itself: a stale revision must refuse, leaving the row alone.
	stale, err := store.Create(beads.Bead{Title: "still open", Type: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := closer.CloseWithMetadataIfMatch(stale.ID, stale.Revision+1, map[string]string{"gc.drain": "acked"}); err == nil {
		t.Fatal("a stale-revision atomic close succeeded")
	}
	if after, getErr := store.Get(stale.ID); getErr != nil || after.Status == "closed" {
		t.Errorf("a refused atomic close still closed the bead (status=%q, err=%v)", after.Status, getErr)
	}
}
