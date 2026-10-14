package beads_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A FileStore batch reaches disk: a second store opened on the same file reads
// every row the batch wrote, at the revision the batch returned.
func TestFileStoreGuardedBatchPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	res, err := s.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbfact", Title: "fact", Status: "closed", NoHistory: true}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed, Metadata: map[string]string{"k": "v"}}}},
	}})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	for id, rev := range res.Revisions {
		got, err := reopened.Get(id)
		if err != nil {
			t.Fatalf("Get(%s) after reopening: %v; the batch never reached disk", id, err)
		}
		if got.Revision != rev || got.Status != "closed" {
			t.Errorf("reopened %s: revision %d status %q, want revision %d status closed", id, got.Revision, got.Status, rev)
		}
	}
	if got, _ := reopened.Get(row.ID); got.Metadata["k"] != "v" {
		t.Errorf("reopened %s metadata = %v, want k=v", row.ID, got.Metadata)
	}
}

// A batch whose save fails persists nothing and rolls the in-memory state
// back: the store and a reopen both read the rows as they were.
func TestFileStoreGuardedBatchFailedSaveRollsBack(t *testing.T) {
	const path = "/city/.gc/beads.json"
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatal(err)
	}
	f.Errors[path+".tmp"] = fmt.Errorf("disk full")
	_, err = s.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbrolled", Title: "rolled back"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
	}})
	if err == nil {
		t.Fatal("ApplyGuarded with a failing save = nil, want the save error")
	}
	delete(f.Errors, path+".tmp")
	reopened, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]beads.Store{"same store": s, "reopened": reopened} {
		if _, err := store.Get("gc-gbrolled"); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("%s: Get(gc-gbrolled) = %v, want ErrNotFound", name, err)
		}
		got, err := store.Get(row.ID)
		if err != nil {
			t.Fatalf("%s: Get(%s): %v", name, row.ID, err)
		}
		if got.Revision != row.Revision || got.Metadata["k"] != "" {
			t.Errorf("%s: %s revision %d metadata %v, want revision %d and no k", name, row.ID, got.Revision, got.Metadata, row.Revision)
		}
	}
}
