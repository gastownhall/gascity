package beads_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestMemStoreStampsUpdatedAt(t *testing.T) {
	runUpdatedAtStoreTests(t, func(t *testing.T) beads.Store {
		t.Helper()
		return beads.NewMemStore()
	})
}

func runUpdatedAtStoreTests(t *testing.T, newStore func(*testing.T) beads.Store) {
	t.Helper()

	t.Run("Create", func(t *testing.T) {
		store := newStore(t)
		created, err := store.Create(beads.Bead{Title: "created"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created.CreatedAt.IsZero() {
			t.Fatal("CreatedAt is zero")
		}
		if !created.UpdatedAt.Equal(created.CreatedAt) {
			t.Fatalf("UpdatedAt = %s, want CreatedAt %s", created.UpdatedAt, created.CreatedAt)
		}

		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get created: %v", err)
		}
		if !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Fatalf("stored UpdatedAt = %s, want CreatedAt %s", got.UpdatedAt, got.CreatedAt)
		}
	})

	t.Run("Update", func(t *testing.T) {
		store := newStore(t)
		created := createUpdatedAtTestBead(t, store, "update")
		title := "updated"

		requireUpdatedAtAdvance(t, store, created.ID, func() error {
			return store.Update(created.ID, beads.UpdateOpts{Title: &title})
		})
	})

	t.Run("Close", func(t *testing.T) {
		store := newStore(t)
		created := createUpdatedAtTestBead(t, store, "close")

		requireUpdatedAtAdvance(t, store, created.ID, func() error {
			return store.Close(created.ID)
		})
	})

	t.Run("Reopen", func(t *testing.T) {
		store := newStore(t)
		created := createUpdatedAtTestBead(t, store, "reopen")
		if err := store.Close(created.ID); err != nil {
			t.Fatalf("Close before Reopen: %v", err)
		}

		requireUpdatedAtAdvance(t, store, created.ID, func() error {
			return store.Reopen(created.ID)
		})
	})

	t.Run("CloseAll", func(t *testing.T) {
		store := newStore(t)
		first := createUpdatedAtTestBead(t, store, "close-all-first")
		second := createUpdatedAtTestBead(t, store, "close-all-second")
		firstBefore := getUpdatedAtTestBead(t, store, first.ID)
		secondBefore := getUpdatedAtTestBead(t, store, second.ID)

		waitForClockAdvance()
		closed, err := store.CloseAll([]string{first.ID, second.ID}, map[string]string{"batch": "done"})
		if err != nil {
			t.Fatalf("CloseAll: %v", err)
		}
		if closed != 2 {
			t.Fatalf("CloseAll closed = %d, want 2", closed)
		}

		firstAfter := getUpdatedAtTestBead(t, store, first.ID)
		secondAfter := getUpdatedAtTestBead(t, store, second.ID)
		requireUpdatedAtAfter(t, firstAfter, firstBefore.UpdatedAt)
		requireUpdatedAtAfter(t, secondAfter, secondBefore.UpdatedAt)
	})

	t.Run("SetMetadataBatch", func(t *testing.T) {
		store := newStore(t)
		created := createUpdatedAtTestBead(t, store, "metadata")

		requireUpdatedAtAdvance(t, store, created.ID, func() error {
			return store.SetMetadataBatch(created.ID, map[string]string{"phase": "done"})
		})
	})
}

func createUpdatedAtTestBead(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{Title: title})
	if err != nil {
		t.Fatalf("Create %q: %v", title, err)
	}
	if created.UpdatedAt.IsZero() {
		t.Fatalf("Create %q returned zero UpdatedAt", title)
	}
	return created
}

func requireUpdatedAtAdvance(t *testing.T, store beads.Store, id string, mutate func() error) {
	t.Helper()
	before := getUpdatedAtTestBead(t, store, id)
	waitForClockAdvance()
	if err := mutate(); err != nil {
		t.Fatalf("mutate %s: %v", id, err)
	}
	after := getUpdatedAtTestBead(t, store, id)
	requireUpdatedAtAfter(t, after, before.UpdatedAt)
}

func getUpdatedAtTestBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return got
}

func requireUpdatedAtAfter(t *testing.T, bead beads.Bead, previous time.Time) {
	t.Helper()
	if !bead.UpdatedAt.After(previous) {
		t.Fatalf("%s UpdatedAt = %s, want after %s", bead.ID, bead.UpdatedAt, previous)
	}
	if _, ok := bead.Metadata["updated_at"]; ok {
		t.Fatalf("%s stored updated_at metadata; UpdatedAt must be a field", bead.ID)
	}
}

func waitForClockAdvance() {
	time.Sleep(2 * time.Millisecond)
}
