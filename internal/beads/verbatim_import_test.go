package beads_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestFileStoreImportVerbatimKeepsIdentityAndRefusesAHeldID: the restore of a
// purged row keeps its id and created_at across a reopen, and an id the store
// already holds is refused rather than overwritten.
func TestFileStoreImportVerbatimKeepsIdentityAndRefusesAHeldID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 12, 17, 54, 0, 0, time.UTC)
	row := beads.Bead{
		ID: "mc-wisp-jkl", Title: "context cycle", Status: "open", Type: "message", Ephemeral: true,
		Labels: []string{"gc:handoff"}, Metadata: map[string]string{"mail.kind": "handoff"}, CreatedAt: at, UpdatedAt: at,
	}
	if err := store.ImportVerbatim([]beads.Bead{row}); err != nil {
		t.Fatalf("ImportVerbatim: %v", err)
	}
	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(row.ID)
	if err != nil {
		t.Fatalf("the imported row did not survive a reopen: %v", err)
	}
	if !got.CreatedAt.Equal(at) || !got.Ephemeral || got.Metadata["mail.kind"] != "handoff" || len(got.Labels) != 1 {
		t.Errorf("the row came back different: %+v", got)
	}
	if err := reopened.ImportVerbatim([]beads.Bead{row}); err == nil {
		t.Error("importing an id the store holds was accepted")
	}
	var _ beads.VerbatimImporter = (*beads.BdStore)(nil)
}
