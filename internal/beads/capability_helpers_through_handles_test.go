package beads

import (
	"context"
	"encoding/json"
	"testing"
)

// countingReadStore is a store with the optional reads ReadyPreferCache,
// ReadyOrdered, DepListBatchFor and ReadyBlockedByAbsentFor look for, and its own live
// reader, counting each one it serves.
type countingReadStore struct {
	*MemStore
	prefer, ordered, batch, absent, live int
}

func (s *countingReadStore) ReadyOrdered(q ReadyQuery, _ ReadyOrder) ([]Bead, bool, error) {
	s.ordered++
	rows, err := s.Ready(q)
	return rows, false, err
}

func (s *countingReadStore) ReadyPreferCache(q ReadyQuery) ([]Bead, error) {
	s.prefer++
	return s.Ready(q)
}

func (s *countingReadStore) DepListBatch([]string) (map[string][]Dep, error) {
	s.batch++
	return map[string][]Dep{}, nil
}

func (s *countingReadStore) ReadyBlockedByAbsent(q ReadyQuery) ([]Bead, error) {
	s.absent++
	return s.MemStore.ReadyBlockedByAbsent(q)
}

func (s *countingReadStore) Handles() StoreHandles {
	return StoreHandles{
		Cached: logicalCachedStoreReader{store: s},
		Live:   countingLiveReader{logicalLiveStoreReader{store: s.MemStore}, &s.live},
		Writer: s,
	}
}

// countingLiveReader is a store's own live reader, counting its reads.
type countingLiveReader struct {
	logicalLiveStoreReader
	n *int
}

func (r countingLiveReader) Ready(query ...ReadyQuery) ([]Bead, error) {
	*r.n++
	return r.logicalLiveStoreReader.Ready(query...)
}

func (r countingLiveReader) List(query ListQuery) ([]Bead, error) {
	*r.n++
	return r.logicalLiveStoreReader.List(query)
}

// policyOverCache stands in for cmd/gc's bead-policy layer over a city or rig
// cache: a wrapper with its own handles, which forwards the cache's ready
// preference, stats and batch read but not IsLive.
type policyOverCache struct {
	Store
	cache *CachingStore
}

func (p policyOverCache) Handles() StoreHandles { return p.cache.Handles() }

func (p policyOverCache) ReadyPreferCache(q ReadyQuery) ([]Bead, error) {
	return p.cache.ReadyPreferCache(q)
}

func (p policyOverCache) Stats() CacheStats { return p.cache.Stats() }

func (p policyOverCache) CachedList(q ListQuery) ([]Bead, bool) { return p.cache.CachedList(q) }

// handleWraps are the shapes a typed caller hands a capability helper: the
// store itself, and a class handle over it of each kind T6 passes.
var handleWraps = map[string]func(Store) Store{
	"the store":       func(s Store) Store { return s },
	"a work handle":   func(s Store) Store { return WorkStore{s} },
	"a graph handle":  func(s Store) Store { return GraphStore{s} },
	"nested handles":  func(s Store) Store { return GraphStore{WorkStore{s}} },
	"a graph target":  func(s Store) Store { return GraphMolecule(GraphStore{s}) },
	"a sessions view": func(s Store) Store { return SessionStore{s} },
}

// TestCapabilityHelpersSeeThroughClassHandles (typed-stores T6a) pins that
// the capability helpers readywork and the API's ready reads hand a leg to
// reach the store's own capability through a class handle, exactly as on the
// raw store: ReadyPreferCache the store's cache-preferring read, else its own
// live reader; ReadyOrdered its ordered read, else its own live reader;
// DepListBatchFor its batch read; ReadyBlockedByAbsentFor its
// absent-blocker listing; LiveFor its live reader. Before T6a a handle hid
// all of them: the helpers asserted the handle, which has none of those methods.
func TestCapabilityHelpersSeeThroughClassHandles(t *testing.T) {
	for name, wrap := range handleWraps {
		store := &countingReadStore{MemStore: NewMemStore()}
		if _, err := store.Create(Bead{Title: "a"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadyPreferCache(wrap(store), ReadyQuery{}); err != nil || store.prefer != 1 {
			t.Errorf("%s: ReadyPreferCache made %d cache-preferring reads (%v), want 1", name, store.prefer, err)
		}
		if _, _, err := ReadyOrdered(wrap(store), ReadyQuery{Limit: 1}, ReadyOrderCanonical); err != nil || store.ordered != 1 {
			t.Errorf("%s: ReadyOrdered made %d ordered reads (%v), want 1", name, store.ordered, err)
		}
		if batch, ok := DepListBatchFor(wrap(store)); !ok {
			t.Errorf("%s: DepListBatchFor found no batch read", name)
		} else if _, err := batch.DepListBatch([]string{"x"}); err != nil || store.batch != 1 {
			t.Errorf("%s: the batch read served %d calls (%v), want 1", name, store.batch, err)
		}
		if reader, ok := ReadyBlockedByAbsentFor(wrap(store)); !ok {
			t.Errorf("%s: ReadyBlockedByAbsentFor found no listing", name)
		} else if _, err := reader.ReadyBlockedByAbsent(ReadyQuery{}); err != nil || store.absent != 1 {
			t.Errorf("%s: the listing served %d calls (%v), want 1", name, store.absent, err)
		}
		if _, err := LiveFor(wrap(store)).List(ListQuery{AllowScan: true}); err != nil || store.live != 1 {
			t.Errorf("%s: LiveFor made %d reads through the store's live reader (%v), want 1", name, store.live, err)
		}

		// A store with no cache preference reads live through its own live
		// reader, not the logical wrapper over the handle.
		plain := &liveOnlyStore{MemStore: NewMemStore()}
		if _, err := ReadyPreferCache(wrap(plain), ReadyQuery{}); err != nil || plain.live != 1 {
			t.Errorf("%s: ReadyPreferCache on a store with no cache made %d live reads (%v), want 1", name, plain.live, err)
		}
		if _, _, err := ReadyOrdered(wrap(plain), ReadyQuery{Limit: 1}, ReadyOrderCanonical); err != nil || plain.live != 2 {
			t.Errorf("%s: ReadyOrdered on a store that cannot cut in order made %d live reads in all (%v), want 2", name, plain.live, err)
		}
	}
	for name, store := range map[string]Store{"nil": nil, "a zero handle": WorkStore{}} {
		if _, ok := DepListBatchFor(store); ok {
			t.Errorf("%s: DepListBatchFor found a batch read", name)
		}
		if _, ok := ReadyBlockedByAbsentFor(store); ok {
			t.Errorf("%s: ReadyBlockedByAbsentFor found a listing", name)
		}
	}
}

// liveOnlyStore is a store with its own live reader and no cache
// preference, counting the live reads it serves.
type liveOnlyStore struct {
	*MemStore
	live int
}

func (s *liveOnlyStore) Handles() StoreHandles {
	return StoreHandles{
		Cached: logicalCachedStoreReader{store: s},
		Live:   countingLiveReader{logicalLiveStoreReader{store: s.MemStore}, &s.live},
		Writer: s,
	}
}

// TestLiveReadsThroughAHandleLeaveTheCacheAlone is the cache half of the
// see-through pin, over a real CachingStore, bare (a class binding) and under
// a policy layer (a city or rig store): a live read through a handle goes to
// the backing store past the cache, as the cache's own live reader does. It
// must not install the row it read into the cache or announce it, which is
// what the logical wrapper over a handle did (CachingStore.List with Live
// set), and ReadyPreferCache must answer what it answers on the raw store.
func TestLiveReadsThroughAHandleLeaveTheCacheAlone(t *testing.T) {
	for layer, over := range map[string]func(*CachingStore) Store{
		"a bare cache":              func(c *CachingStore) Store { return c },
		"a policy layer on a cache": func(c *CachingStore) Store { return policyOverCache{Store: c, cache: c} },
	} {
		for name, wrap := range handleWraps {
			backing := NewMemStore()
			if _, err := backing.Create(Bead{Title: "primed"}); err != nil {
				t.Fatal(err)
			}
			var announced int
			cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) { announced++ })
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			late, err := backing.Create(Bead{Title: "written past the cache"})
			if err != nil {
				t.Fatal(err)
			}
			store := wrap(over(cache))

			rows, err := LiveFor(store).List(ListQuery{AllowScan: true})
			if err != nil {
				t.Fatalf("%s, %s: live list: %v", layer, name, err)
			}
			if !containsBead(rows, late.ID) {
				t.Errorf("%s, %s: the live list missed %s, written to the backing store", layer, name, late.ID)
			}
			if _, err := cache.cachedGetOnly(late.ID); err == nil {
				t.Errorf("%s, %s: the live list installed %s into the cache", layer, name, late.ID)
			}
			if announced != 0 {
				t.Errorf("%s, %s: the live list announced %d changes, want none", layer, name, announced)
			}

			want, wantErr := ReadyPreferCache(over(cache), ReadyQuery{})
			got, gotErr := ReadyPreferCache(store, ReadyQuery{})
			if (gotErr == nil) != (wantErr == nil) || !sameBeadIDs(got, want) {
				t.Errorf("%s, %s: ReadyPreferCache = %v (%v), the raw store's %v (%v)", layer, name, capBeadIDs(got), gotErr, capBeadIDs(want), wantErr)
			}
			if _, err := cache.cachedGetOnly(late.ID); err == nil {
				t.Errorf("%s, %s: ReadyPreferCache installed %s into the cache", layer, name, late.ID)
			}
			if announced != 0 {
				t.Errorf("%s, %s: ReadyPreferCache announced %d changes, want none", layer, name, announced)
			}
		}
	}
}

// TestCacheCapabilitiesSeeThroughClassHandles pins As for the cache's own
// named reads (T6a): a handle over a cache, bare or under a policy layer
// forwarding them, reaches each one the store has, and IsLive stays hidden
// behind a policy layer that does not forward it.
func TestCacheCapabilitiesSeeThroughClassHandles(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	policy := policyOverCache{Store: cache, cache: cache}
	for name, wrap := range handleWraps {
		if _, ok := As[CacheStatsReporter](wrap(cache)); !ok {
			t.Errorf("%s over a cache: no CacheStatsReporter", name)
		}
		if _, ok := As[CacheLivenessReporter](wrap(cache)); !ok {
			t.Errorf("%s over a cache: no CacheLivenessReporter", name)
		}
		if _, ok := As[CachedLister](wrap(cache)); !ok {
			t.Errorf("%s over a cache: no CachedLister", name)
		}
		if _, ok := As[CacheStatsReporter](wrap(policy)); !ok {
			t.Errorf("%s over a policy layer: no CacheStatsReporter", name)
		}
		if _, ok := As[CacheLivenessReporter](wrap(policy)); ok {
			t.Errorf("%s over a policy layer: a CacheLivenessReporter the layer does not forward", name)
		}
		if _, ok := As[CacheStatsReporter](wrap(NewMemStore())); ok {
			t.Errorf("%s over a plain store: a CacheStatsReporter", name)
		}
	}
	for _, class := range []func() bool{
		passesClassGate[CacheStatsReporter], passesClassGate[CacheLivenessReporter], passesClassGate[CachedLister],
	} {
		if !class() {
			t.Error("a cache read is vetted by the class gate; it is class-neutral")
		}
	}
}

func capBeadIDs(rows []Bead) []string {
	ids := make([]string, len(rows))
	for i, b := range rows {
		ids[i] = b.ID
	}
	return ids
}

func sameBeadIDs(a, b []Bead) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}
