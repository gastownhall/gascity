package beads_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// primedSQLiteCache opens a SQLite engine the way a relocated binding does (the
// graph mint prefix, fenced to the namespaces it serves) and returns a primed
// cache over it plus the engine itself.
func primedSQLiteCache(t *testing.T, opts ...beads.CachingStoreOption) (*beads.CachingStore, *beads.SQLiteStore) {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix("gcg"),
		beads.WithSQLiteStoreReservedIDPrefixes("gcg", "gcs", "gcn", "gcnq"),
	)
	if err != nil {
		t.Fatalf("opening the SQLite engine: %v", err)
	}
	engine := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = engine.CloseStore() })
	cache := beads.NewCachingStore(engine, nil, opts...)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, engine
}

// A binding engine mints under one prefix but holds rows under every namespace
// its classes reserve. Kills: a cache filtering events on the mint prefix, which
// drops a one-shot CLI's event for a nudge-queue record (gcnq-) and leaves the
// controller serving the old row until the next re-scan.
func TestWithEventIDPrefixesAppliesEveryServedNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		opts  []beads.CachingStoreOption
		apply bool
	}{
		{name: "mint prefix only", apply: false},
		{name: "every served namespace", opts: []beads.CachingStoreOption{beads.WithEventIDPrefixes("gcg", "gcnq")}, apply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache, engine := primedSQLiteCache(t, tc.opts...)
			if got := cache.IDPrefix(); got != "gcg" {
				t.Fatalf("IDPrefix = %q, want the mint prefix gcg", got)
			}
			row, err := engine.CreateWithForeignID(beads.Bead{ID: "gcnq-abc", Title: "queued", Type: "task"})
			if err != nil {
				t.Fatalf("CreateWithForeignID: %v", err)
			}
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("re-Prime: %v", err)
			}
			row.Title = "delivered"
			if err := engine.Update(row.ID, beads.UpdateOpts{Title: &row.Title}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			payload, err := beads.EncodeBeadEventPayload(row)
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			cache.ApplyEvent("bead.updated", payload)
			rows, ok := cache.CachedList(beads.ListQuery{Status: "open"})
			if !ok || len(rows) != 1 {
				t.Fatalf("CachedList = (%v, %v), want the one queue row", rows, ok)
			}
			if applied := rows[0].Title == "delivered"; applied != tc.apply {
				t.Fatalf("event applied = %v, want %v", applied, tc.apply)
			}
		})
	}
}

// The release absorb takes a row's edges from the refreshed row when it
// carries them and keeps the cached ones when it carries none. Kills (M19,
// either direction): an absorb that keeps stale cached edges over the engine's
// current set, and one that wipes the edges of a backing whose rows omit them.
func TestAssignmentWriteAbsorbSourcesEdgesFromTheRowOnlyWhenCarried(t *testing.T) {
	t.Parallel()
	t.Run("release takes the engine's current edges", func(t *testing.T) {
		t.Parallel()
		cache, engine := primedSQLiteCache(t)
		blocker, err := cache.Create(beads.Bead{Title: "blocker", Type: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		status := "in_progress"
		step, err := cache.Create(beads.Bead{Title: "step", Type: "task", Assignee: "worker-1"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := cache.Update(step.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd behind the cache: %v", err)
		}
		if released, err := cache.ReleaseIfCurrent(step.ID, "worker-1"); err != nil || !released {
			t.Fatalf("ReleaseIfCurrent = (%v, %v)", released, err)
		}
		deps, err := beads.HandlesFor(cache).Cached.DepList(step.ID, "down")
		if err != nil || len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
			t.Fatalf("cached edges after release = (%+v, %v), want the edge to %s", deps, err, blocker.ID)
		}
	})
	t.Run("release keeps edges a row does not carry", func(t *testing.T) {
		t.Parallel()
		backing := beads.NewMemStore()
		blocker, _ := backing.Create(beads.Bead{Title: "blocker", Type: "task"})
		step, _ := backing.Create(beads.Bead{Title: "step", Type: "task", Assignee: "worker-1"})
		status := "in_progress"
		if err := backing.Update(step.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := backing.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
		cache := beads.NewCachingStore(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if released, err := cache.ReleaseIfCurrent(step.ID, "worker-1"); err != nil || !released {
			t.Fatalf("ReleaseIfCurrent = (%v, %v)", released, err)
		}
		deps, err := beads.HandlesFor(cache).Cached.DepList(step.ID, "down")
		if err != nil || len(deps) != 1 {
			t.Fatalf("cached edges after release = (%+v, %v), want the edge kept", deps, err)
		}
		ready, err := beads.HandlesFor(cache).Cached.Ready()
		if err != nil {
			t.Fatalf("cached Ready: %v", err)
		}
		for _, b := range ready {
			if b.ID == step.ID {
				t.Fatalf("cached Ready offers %s after a release wiped its edge", step.ID)
			}
		}
	})
}
