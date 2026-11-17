package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// cachedWispEngineForTest returns a SQLite engine and a primed CachingStore
// over it, the shape every controller-held store has.
func cachedWispEngineForTest(t *testing.T) (*beads.SQLiteStore, *beads.CachingStore) {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	engine := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = engine.CloseStore() })
	cache := beads.NewCachingStore(engine, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return engine, cache
}

// The rootless-orphan reaper asks for a candidate's children across both
// tiers. Through a CachingStore it must see the same children the engine
// does. Kills: Children dropping its tier option, which hides a closed
// wisp-tier child, so the reaper deletes its parent and strands the child
// beyond either GC path (#3780).
func TestWispGC_ReapThroughACacheSeesWispTierChildren(t *testing.T) {
	withReapOrphansEnforced(t, true)
	old := time.Now().Add(-2 * time.Hour)
	for _, via := range []string{"engine", "cache"} {
		t.Run(via, func(t *testing.T) {
			engine, cache := cachedWispEngineForTest(t)
			if _, err := engine.Create(beads.Bead{ID: "gcg-orphan", Title: "orphan", Type: "task", Status: "closed", Ephemeral: true, CreatedAt: old, UpdatedAt: old}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := engine.Create(beads.Bead{ID: "gcg-orphan-child", Title: "child", Type: "task", Status: "closed", Ephemeral: true, ParentID: "gcg-orphan", CreatedAt: old, UpdatedAt: old}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			var store beads.Store = engine
			if via == "cache" {
				store = cache
			}
			reaped, err := reapOrphanedClosedWisps(store, time.Now().Add(time.Hour), 500)
			if err != nil {
				t.Fatalf("reapOrphanedClosedWisps: %v", err)
			}
			if reaped != 0 {
				t.Fatalf("reaped %d through the %s; a rootless wisp that owns a wisp-tier child is not a leaf", reaped, via)
			}
		})
	}
}

// Molecule autoclose closes a root only when its whole subtree is terminal.
// Through a CachingStore the check must match the engine. Kills: Children
// dropping its tier option, which hides an open wisp step and closes a
// molecule that is still running.
func TestSubtreeTerminalThroughACacheSeesWispTierSteps(t *testing.T) {
	engine, cache := cachedWispEngineForTest(t)
	root, err := engine.Create(beads.Bead{Title: "molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := engine.Create(beads.Bead{Title: "open wisp step", Type: "task", Ephemeral: true, ParentID: root.ID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want, _ := subtreeTerminalExcludingRoot(engine, root.ID)
	got, _ := subtreeTerminalExcludingRoot(cache, root.ID)
	if want {
		t.Fatal("the engine calls the subtree terminal; this fixture no longer has an open step")
	}
	if got != want {
		t.Fatalf("subtree terminal through the cache = %v, engine %v; autoclose would close a running molecule", got, want)
	}
}
