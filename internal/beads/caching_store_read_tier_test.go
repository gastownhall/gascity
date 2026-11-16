package beads_test

import (
	"context"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestCachingStoreLogicalReadTierWidensEveryZeroTierRead pins
// WithLogicalReadTier over a real SQLite engine (the split binding's): every
// read entry that leaves its tier at the zero value answers across both tiers,
// cached or live, and an explicit tier is answered as asked. Without the option
// the same cache keeps the narrow answer, which is the control.
func TestCachingStoreLogicalReadTierWidensEveryZeroTierRead(t *testing.T) {
	for _, widened := range []bool{false, true} {
		name := map[bool]string{false: "unwidened", true: "widened"}[widened]
		t.Run(name, func(t *testing.T) {
			engine, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.(interface{ CloseStore() error }).CloseStore() })
			var opts []beads.CachingStoreOption
			if widened {
				opts = append(opts, beads.WithLogicalReadTier())
			}
			cache := beads.NewCachingStoreForTest(engine, nil, opts...)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			root, err := cache.Create(beads.Bead{Title: "root", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			wisp, err := cache.Create(beads.Bead{Title: "wisp", Type: "task", Ephemeral: true, ParentID: root.ID})
			if err != nil {
				t.Fatal(err)
			}
			has := func(rows []beads.Bead) bool {
				return slices.ContainsFunc(rows, func(b beads.Bead) bool { return b.ID == wisp.ID })
			}
			check := func(read string, rows []beads.Bead, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", read, err)
				}
				if has(rows) != widened {
					t.Errorf("%s served the ephemeral row = %v, want %v", read, has(rows), widened)
				}
			}
			rows, err := cache.List(beads.ListQuery{Status: "open"})
			check("List(open)", rows, err)
			rows, err = cache.List(beads.ListQuery{ParentID: root.ID})
			check("List(parent, live)", rows, err)
			rows, err = cache.Children(root.ID)
			check("Children", rows, err)
			rows, err = cache.Ready()
			check("Ready()", rows, err)
			rows, err = cache.Ready(beads.ReadyQuery{Limit: 10})
			check("Ready(limit)", rows, err)
			rows, err = cache.ReadyPreferCache(beads.ReadyQuery{})
			check("ReadyPreferCache", rows, err)
			rows, err = beads.HandlesFor(cache).Live.Ready()
			check("Live.Ready", rows, err)
			if cached, ok := cache.CachedReady(); ok {
				check("CachedReady", cached, nil)
			}
			n, err := cache.Count(context.Background(), beads.ListQuery{Status: "open"})
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{false: 1, true: 2}[widened]; n != want {
				t.Errorf("Count(open) = %d, want %d", n, want)
			}

			// An explicit tier is answered as asked, widened or not.
			issues, err := cache.List(beads.ListQuery{Status: "open", TierMode: beads.TierWisps})
			if err != nil || !has(issues) || slices.ContainsFunc(issues, func(b beads.Bead) bool { return b.ID == root.ID }) {
				t.Errorf("List(TierWisps) = %+v, %v; want only the ephemeral row", issues, err)
			}
		})
	}
}
