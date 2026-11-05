package beads_test

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin what a CachingStore over the route-clearing decorator reads
// from its backing (ga-l69b9m, architect Amendment 1 findings A and B): the
// decorator forwards the backing's cache-facing declarations, so a cache over it
// behaves as a cache over the bare engine does.

// declaringStore declares CachedReadExact with a fixed answer.
type declaringStore struct {
	beads.Store
	answer bool
}

func (s declaringStore) CachedReadExact() bool { return s.answer }

func cachedReadExact(store beads.Store) bool {
	d, ok := store.(beads.CachedReadExact)
	return ok && d.CachedReadExact()
}

// The decorator reports exactly what its backing declares.
func TestRouteClearForwardsCachedReadExact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner beads.Store
		want  bool
	}{
		{"SQLite declares true", newSQLiteForConformance(t), true},
		{"MemStore declares nothing", beads.NewMemStore(), false},
		{"a spy declaring true", declaringStore{Store: beads.NewMemStore(), answer: true}, true},
		{"a spy declaring false", declaringStore{Store: beads.NewMemStore(), answer: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cachedReadExact(beads.WithRouteChangeClearing(tc.inner, identityNormalizer)); got != tc.want {
				t.Errorf("CachedReadExact through the decorator = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cache over the decorator serves the same ready set as one over bare SQLite,
// including a row held back by a resident open blocker.
func TestRouteClearCachedReadyMatchesBareSQLite(t *testing.T) {
	build := func(t *testing.T, decorate bool) []string {
		t.Helper()
		engine := newSQLiteForConformance(t)
		blocker, err := engine.Create(beads.Bead{Title: "blocker", Type: "task"})
		if err != nil {
			t.Fatalf("Create blocker: %v", err)
		}
		blocked, err := engine.Create(beads.Bead{Title: "blocked", Type: "task"})
		if err != nil {
			t.Fatalf("Create blocked: %v", err)
		}
		free, err := engine.Create(beads.Bead{Title: "free", Type: "task"})
		if err != nil {
			t.Fatalf("Create free: %v", err)
		}
		if err := engine.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
		var backing beads.Store = engine
		if decorate {
			backing = beads.WithRouteChangeClearing(engine, identityNormalizer)
		}
		cache := newCache(t, backing)
		ready, err := beads.HandlesFor(cache).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("cached Ready: %v", err)
		}
		var titles []string
		for _, b := range ready {
			titles = append(titles, b.Title)
		}
		sort.Strings(titles)
		_ = free
		return titles
	}
	bare, decorated := build(t, false), build(t, true)
	if len(bare) != 2 {
		t.Fatalf("fixture: bare cached ready = %v, want the blocker and the free row", bare)
	}
	if len(decorated) != len(bare) {
		t.Fatalf("cached ready over the decorator = %v, bare SQLite = %v", decorated, bare)
	}
	for i := range bare {
		if bare[i] != decorated[i] {
			t.Fatalf("cached ready over the decorator = %v, bare SQLite = %v", decorated, bare)
		}
	}
}

// depListSpy counts per-bead edge reads.
type depListSpy struct {
	*beads.SQLiteStore
	depLists atomic.Int64
}

func (s *depListSpy) DepList(id, direction string) ([]beads.Dep, error) {
	s.depLists.Add(1)
	return s.SQLiteStore.DepList(id, direction)
}

// Priming a cache over the decorator costs the same edge reads as priming one
// over bare SQLite: the backing's declaration that its list rows carry complete
// edges is forwarded, so the cache does not read each row's edges again.
func TestRouteClearPrimeDepListCountMatchesBareSQLite(t *testing.T) {
	prime := func(t *testing.T, decorate bool) int64 {
		t.Helper()
		spy := &depListSpy{SQLiteStore: newSQLiteForConformance(t)}
		for i := 0; i < 5; i++ {
			if _, err := spy.Create(beads.Bead{Title: "row", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: testOldTarget}}); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}
		var backing beads.Store = spy
		if decorate {
			backing = beads.WithRouteChangeClearing(spy, identityNormalizer)
		}
		spy.depLists.Store(0)
		c := beads.NewCachingStoreForTest(backing, nil)
		if err := c.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return spy.depLists.Load()
	}
	bare, decorated := prime(t, false), prime(t, true)
	if decorated != bare {
		t.Errorf("Prime over the decorator issued %d DepList call(s), bare SQLite %d", decorated, bare)
	}
}
