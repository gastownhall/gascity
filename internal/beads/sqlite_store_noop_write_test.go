package beads_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSQLiteStoreSameValueWriteCommitsNothing pins that the store, not a
// cache in front of it, decides a write is a no-op: a write whose target the
// stored row already holds commits nothing, so the revision and updated_at
// stay put (no CAS token a concurrent writer holds goes stale, and no
// bead.updated payload changes). A write that changes the row still moves both.
func TestSQLiteStoreSameValueWriteCommitsNothing(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	intPtr := func(i int) *int { return &i }
	seed := beads.Bead{
		Title: "pinned", Type: "task", Priority: intPtr(2), Description: "body",
		Assignee: "alice", Labels: []string{"a", "b"}, Metadata: map[string]string{"k": "v", "j": "w"},
	}
	noops := []struct {
		name  string
		write func(s beads.Store, id string) error
	}{
		{"SetMetadata", func(s beads.Store, id string) error { return s.SetMetadata(id, "k", "v") }},
		{"SetMetadataBatch", func(s beads.Store, id string) error {
			return s.SetMetadataBatch(id, map[string]string{"k": "v", "j": "w"})
		}},
		{"Update/Title", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Title: strPtr("pinned")}) }},
		{"Update/Status", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Status: strPtr("open")}) }},
		{"Update/Type", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Type: strPtr("task")}) }},
		{"Update/Priority", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Priority: intPtr(2)}) }},
		{"Update/Description", func(s beads.Store, id string) error {
			return s.Update(id, beads.UpdateOpts{Description: strPtr("body")})
		}},
		{"Update/Assignee", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Assignee: strPtr("alice")}) }},
		{"Update/Labels", func(s beads.Store, id string) error { return s.Update(id, beads.UpdateOpts{Labels: []string{"a"}}) }},
		{"Update/RemoveLabels", func(s beads.Store, id string) error {
			return s.Update(id, beads.UpdateOpts{RemoveLabels: []string{"z"}})
		}},
		{"Update/Metadata", func(s beads.Store, id string) error {
			return s.Update(id, beads.UpdateOpts{Metadata: map[string]string{"j": "w"}})
		}},
	}
	for _, tc := range noops {
		t.Run(tc.name, func(t *testing.T) {
			store, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			created, err := store.Create(seed)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			before, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := tc.write(store, created.ID); err != nil {
				t.Fatalf("write: %v", err)
			}
			after, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("same-value write moved the row: revision %d -> %d, updated_at %v -> %v",
					before.Revision, after.Revision, before.UpdatedAt, after.UpdatedAt)
			}

			// The controls: a write that changes the row commits, a label
			// edit included.
			last := after
			for _, control := range []struct {
				opts beads.UpdateOpts
				ok   func(beads.Bead) bool
			}{
				{beads.UpdateOpts{Metadata: map[string]string{"k": "changed"}}, func(b beads.Bead) bool { return b.Metadata["k"] == "changed" }},
				{beads.UpdateOpts{Labels: []string{"c"}}, func(b beads.Bead) bool { return slices.Contains(b.Labels, "c") }},
				{beads.UpdateOpts{RemoveLabels: []string{"a"}}, func(b beads.Bead) bool { return !slices.Contains(b.Labels, "a") }},
			} {
				if err := store.Update(created.ID, control.opts); err != nil {
					t.Fatalf("Update(%+v): %v", control.opts, err)
				}
				changed, err := store.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if changed.Revision == last.Revision || !control.ok(changed) {
					t.Fatalf("changing write %+v did not commit: revision %d -> %d, row %+v", control.opts, last.Revision, changed.Revision, changed)
				}
				last = changed
			}
		})
	}
}

// TestSQLiteStoreReadOnlyRefusesSameValueWrites pins that the in-transaction
// no-op skip does not make a read-only store answer success for a write: a
// same-value write is refused exactly as a changing one is.
func TestSQLiteStoreReadOnlyRefusesSameValueWrites(t *testing.T) {
	dir := t.TempDir()
	writable, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	created, err := writable.Create(beads.Bead{Title: "row", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := writable.(*beads.SQLiteStore).CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	store, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatalf("OpenSQLiteStore read-only: %v", err)
	}
	t.Cleanup(func() { _ = store.(*beads.SQLiteStore).CloseStore() })
	title := "row"
	for name, write := range map[string]func() error{
		"same-value SetMetadata": func() error { return store.SetMetadata(created.ID, "k", "v") },
		"same-value Update":      func() error { return store.Update(created.ID, beads.UpdateOpts{Title: &title}) },
		"changing SetMetadata":   func() error { return store.SetMetadata(created.ID, "k", "changed") },
	} {
		if err := write(); err == nil {
			t.Errorf("%s on a read-only store succeeded, want refused", name)
		}
	}
}
