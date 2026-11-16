package beads

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// cachedHandleCountStore counts the backing reads a cached-handle call
// causes: any List or Get on it means the handle went to the backing store,
// which on the tick is a prime stalling the build.
type cachedHandleCountStore struct {
	Store
	lists atomic.Int64
	gets  atomic.Int64
	// failFullScan makes the cache's full-scan list fail, so a Prime fails.
	failFullScan atomic.Bool
}

func (s *cachedHandleCountStore) List(query ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	if s.failFullScan.Load() && !query.Live && query.AllowScan && query.SkipLabels {
		return nil, errors.New("full scan unavailable")
	}
	return s.Store.List(query)
}

func (s *cachedHandleCountStore) Get(id string) (Bead, error) {
	s.gets.Add(1)
	return s.Store.Get(id)
}

func (s *cachedHandleCountStore) reads() int64 { return s.lists.Load() + s.gets.Load() }

func newCachedHandleFixture(t *testing.T) (*CachingStore, *cachedHandleCountStore, Bead) {
	t.Helper()
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cachedHandleCountStore{Store: mem}
	return NewCachingStoreForTest(backing, nil), backing, bead
}

// The tick-flavored cached handle serves only a fully primed cache. On any
// other state it answers ErrCacheUnavailable at once, with no backing read
// and no prime started, so the caller falls back to a live read instead of
// the tick blocking on a prime.
func TestCachedHandleDeclinesUnlessFullyPrimedWithoutReadingBacking(t *testing.T) {
	states := []struct {
		name  string
		setup func(t *testing.T, cache *CachingStore)
	}{
		{"uninitialized", func(*testing.T, *CachingStore) {}},
		{"partial", func(t *testing.T, cache *CachingStore) {
			t.Helper()
			if err := cache.PrimeActive(); err != nil {
				t.Fatalf("PrimeActive: %v", err)
			}
			cache.mu.RLock()
			state := cache.state
			cache.mu.RUnlock()
			if state != cachePartial {
				t.Fatalf("state after PrimeActive = %v, want cachePartial", state)
			}
		}},
		{"degraded", func(t *testing.T, cache *CachingStore) {
			t.Helper()
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			cache.mu.Lock()
			cache.state = cacheDegraded
			cache.mu.Unlock()
		}},
		{"live with a partial prime error", func(t *testing.T, cache *CachingStore) {
			t.Helper()
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			cache.mu.Lock()
			cache.primePartialErr = errors.New("skipped 1 corrupt bead")
			cache.mu.Unlock()
		}},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache, backing, bead := newCachedHandleFixture(t)
			tc.setup(t, cache)
			before := backing.reads()
			h := cache.Handles().Cached

			calls := map[string]func() error{
				"Get":     func() error { _, err := h.Get(bead.ID); return err },
				"List":    func() error { _, err := h.List(ListQuery{Status: "open"}); return err },
				"Ready":   func() error { _, err := h.Ready(); return err },
				"DepList": func() error { _, err := h.DepList(bead.ID, "down"); return err },
				// The lookup under Get has no state check of its own: on an
				// unprimed cache it would answer ErrNotFound for a bead that
				// exists, and callers fall back only on ErrCacheUnavailable.
				"cachedGetOnly": func() error { _, err := cache.cachedGetOnly(bead.ID); return err },
			}
			for name, call := range calls {
				err := call()
				if !errors.Is(err, ErrCacheUnavailable) {
					t.Errorf("%s on a %s cache: error = %v, want ErrCacheUnavailable", name, tc.name, err)
				}
				if errors.Is(err, ErrNotFound) {
					t.Errorf("%s on a %s cache answered ErrNotFound for bead %s that exists", name, tc.name, bead.ID)
				}
			}
			if got := backing.reads() - before; got != 0 {
				t.Errorf("backing reads caused by cached-handle calls on a %s cache = %d, want 0 (no inline prime)", tc.name, got)
			}
			cache.primeMu.Lock()
			running := cache.primeRunning
			cache.primeMu.Unlock()
			if running {
				t.Errorf("a prime is running after cached-handle calls on a %s cache", tc.name)
			}
		})
	}
}

// A fully primed cache is served as before, from memory.
func TestCachedHandleServesFullyPrimedCacheFromMemory(t *testing.T) {
	t.Parallel()
	cache, backing, bead := newCachedHandleFixture(t)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before := backing.reads()
	h := cache.Handles().Cached

	got, err := h.Get(bead.ID)
	if err != nil || got.ID != bead.ID {
		t.Fatalf("Get = %+v, %v; want bead %s", got, err, bead.ID)
	}
	rows, err := h.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertHasBeadIDs(t, rows, bead.ID)
	ready, err := h.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	assertHasBeadIDs(t, ready, bead.ID)
	if _, err := h.DepList(bead.ID, "down"); err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if got := backing.reads() - before; got != 0 {
		t.Fatalf("backing reads for a fully primed cache = %d, want 0", got)
	}
}

// Recovery happens off the tick: a cache whose Prime failed becomes servable
// after one clean reconcile pass, with no reader calling a prime.
func TestCachedHandleBecomesServableAfterOneCleanReconcile(t *testing.T) {
	t.Parallel()
	cache, backing, bead := newCachedHandleFixture(t)
	cache.primeRetryDelay = func(int) time.Duration { return 0 }
	backing.failFullScan.Store(true)
	if err := cache.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded against a failing full scan")
	}
	if _, err := cache.Handles().Cached.Get(bead.ID); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Get after failed Prime = %v, want ErrCacheUnavailable", err)
	}

	backing.failFullScan.Store(false)
	cache.runReconciliation()

	if !cache.cacheFullyPrimed() {
		t.Fatal("cache is not fully primed after one clean reconcile pass")
	}
	before := backing.reads()
	got, err := cache.Handles().Cached.Get(bead.ID)
	if err != nil || got.ID != bead.ID {
		t.Fatalf("Get after reconcile = %+v, %v; want bead %s", got, err, bead.ID)
	}
	if delta := backing.reads() - before; delta != 0 {
		t.Fatalf("Get after reconcile read the backing store %d times, want 0", delta)
	}
}
