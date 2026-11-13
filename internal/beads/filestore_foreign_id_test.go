package beads_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestFileStoreCreateWithForeignIDKeepsTheWholeRow: the file store takes rows
// under their own ids with their status, timestamps and fields, persists
// them, refuses a batch with any taken id whole, and mints past a pinned
// "<prefix>-<n>".
func TestFileStoreCreateWithForeignIDKeepsTheWholeRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path, beads.WithFileStoreIDPrefix("rc"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	priority := 1
	rows := []beads.Bead{
		{ID: "gcg-7", Title: "closed step", Type: "task", Status: "closed", CloseReason: "done", CreatedAt: created, UpdatedAt: created.Add(time.Hour), Priority: &priority, Labels: []string{"a"}, Metadata: beads.StringMap{"k": "v"}, Ref: "step-1", Ephemeral: true},
		{ID: "rc-41", Title: "open", Type: "message", Status: "open", CreatedAt: created},
	}
	if err := store.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatal(err)
	}
	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path, beads.WithFileStoreIDPrefix("rc"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get("gcg-7")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.CloseReason != "done" || !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created.Add(time.Hour)) || *got.Priority != 1 || got.Ref != "step-1" || !got.Ephemeral || got.Metadata["k"] != "v" {
		t.Fatalf("the row did not survive whole: %+v", got)
	}
	if open, _ := reopened.Get("rc-41"); !open.UpdatedAt.Equal(created) {
		t.Fatalf("a zero updated_at did not take the creation time: %v", open.UpdatedAt)
	}
	if err := reopened.CreateWithForeignIDBatch([]beads.Bead{{ID: "gcg-8", Title: "new"}, {ID: "gcg-7", Title: "taken"}}); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("a batch with a taken id = %v", err)
	}
	if _, err := reopened.Get("gcg-8"); err == nil {
		t.Fatal("a refused batch stored part of itself")
	}
	minted, err := reopened.Create(beads.Bead{Title: "minted"})
	if err != nil {
		t.Fatal(err)
	}
	if minted.ID != "rc-42" {
		t.Fatalf("the store minted %s after a pinned rc-41, want rc-42", minted.ID)
	}
	one, err := reopened.CreateWithForeignID(beads.Bead{ID: "gcg-9", Title: "one", Status: "closed"})
	if err != nil || one.Status != "closed" {
		t.Fatalf("CreateWithForeignID = %+v, %v", one, err)
	}
}
