package beads

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listRevisionStripper lists every row at revision 0, as a store whose list
// reads publish no revision does (BdStore over `bd list --json`, mc-3c8h5),
// while its Get keeps the real one.
type listRevisionStripper struct{ Store }

func (s listRevisionStripper) List(q ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(q)
	for i := range rows {
		rows[i].Revision = 0
	}
	return rows, err
}

type listErrStore struct{ Store }

func (listErrStore) List(ListQuery) ([]Bead, error) { return nil, errors.New("backing down") }

// liveListErrStore fails only Live listings: the sample reads, the verdict's
// listing does not.
type liveListErrStore struct{ Store }

func (s liveListErrStore) List(q ListQuery) ([]Bead, error) {
	if q.Live {
		return nil, errors.New("backing down")
	}
	return s.Store.List(q)
}

// getErrStripper lists every row at 0 and fails every point read.
type getErrStripper struct{ listRevisionStripper }

func (getErrStripper) Get(string) (Bead, error) { return Bead{}, errors.New("backing down") }

// seedWrittenRows creates two rows on s and writes one, so every row is at a
// real revision.
func seedWrittenRows(t *testing.T, s Store) {
	t.Helper()
	a, err := s.Create(Bead{Title: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(Bead{Title: "b"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	assignee := "worker"
	if err := s.Update(a.ID, UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// seedInProgressRows creates two rows on s and moves both to in_progress, so
// no row is open and every row is at a real revision.
func seedInProgressRows(t *testing.T, s Store) {
	t.Helper()
	inProgress := "in_progress"
	for _, title := range []string{"a", "b"} {
		b, err := s.Create(Bead{Title: title})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Update(b.ID, UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
}

// activeCacheForFidelityTest primes a cache over backing with its active rows
// only, as at boot.
func activeCacheForFidelityTest(t *testing.T, backing Store) *CachingStore {
	t.Helper()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	return cache
}

func primedCacheForFidelityTest(t *testing.T, backing Store) *CachingStore {
	t.Helper()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache
}

// Kills over-refusal: the in-process stores whose list reads carry the row's
// revision pass, bare, under a primed cache, and under a class wrapper over
// that cache. The native case also kills beadFromNativeIssueRow clearing the
// revision.
func TestCheckListRevisionFidelityAdmitsRevisionListers(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T) Store
	}{
		{"mem", func(*testing.T) Store { return NewMemStore() }},
		{"sqlite", func(t *testing.T) Store {
			s, err := OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			t.Cleanup(func() { _ = s.(*SQLiteStore).CloseStore() })
			return s
		}},
		{"native-dolt", func(*testing.T) Store { return newNativeDoltStoreForTest(newNativeDoltMemStorage()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := tc.open(t)
			seedWrittenRows(t, backing)
			cache := primedCacheForFidelityTest(t, backing)
			for name, s := range map[string]Store{"bare": backing, "cached": cache, "class over cache": SessionStore{Store: cache}} {
				if err := CheckListRevisionFidelity(s); err != nil {
					t.Errorf("%s: %v", name, err)
				}
			}
		})
	}
}

// A legacy row lists at 0 and reads live at 0: 0 is its real revision, so the
// row neither convicts the store nor vouches for its list path, and a sample
// of nothing else is undecided. Kills a vacuous pass, and a refusal of a
// legacy row.
func TestCheckListRevisionFidelityLeavesALegacyOnlySampleUndecided(t *testing.T) {
	store := NewMemStoreFrom(1, []Bead{{ID: "gc-1", Title: "legacy", Status: "open", Type: "task"}}, nil)
	var undecided *ListRevisionsUndecidedError
	if err := CheckListRevisionFidelity(store); !errors.As(err, &undecided) || undecided.Sampled != 1 {
		t.Fatalf("legacy row: %v, want undecided over 1 sampled row", err)
	}
}

// A store with no open or in-progress row has nothing to sample: undecided,
// neither admitted nor refused, bare and cached. Kills a vacuous pass.
func TestCheckListRevisionFidelityLeavesAnEmptySampleUndecided(t *testing.T) {
	mem := NewMemStore()
	done, err := mem.Create(Bead{Title: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Store{"bare": mem, "cached": activeCacheForFidelityTest(t, mem)} {
		var undecided *ListRevisionsUndecidedError
		if err := CheckListRevisionFidelity(s); !errors.As(err, &undecided) || undecided.Sampled != 0 {
			t.Errorf("%s: %v, want undecided over an empty sample", name, err)
		}
	}
}

// Kills a sample of open rows alone: the rows a release or an autoclose fences
// are in progress, and a city may have no open row at all. In-progress rows
// decide the store both ways, bare and under a cache primed with its active
// rows, as at boot.
func TestCheckListRevisionFidelitySamplesInProgressRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrap  func(Store) Store
		blind bool
	}{
		{"faithful", func(s Store) Store { return s }, false},
		{"blind", func(s Store) Store { return listRevisionStripper{s} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemStore()
			seedInProgressRows(t, mem)
			backing := tc.wrap(mem)
			for name, s := range map[string]Store{"bare": backing, "cached": activeCacheForFidelityTest(t, backing)} {
				err := CheckListRevisionFidelity(s)
				var blind *ListRevisionsBlindError
				switch {
				case tc.blind && !errors.As(err, &blind):
					t.Errorf("%s: %v, want a ListRevisionsBlindError", name, err)
				case !tc.blind && err != nil:
					t.Errorf("%s: %v, want admitted", name, err)
				}
			}
		})
	}
}

// Kills a C0.7 blind to revision fidelity: a store that lists rows at 0 while
// they read live at a real revision is refused, bare, as the backing of a
// primed cache, and behind a class wrapper over that cache.
func TestCheckListRevisionFidelityRefusesARevisionZeroLister(t *testing.T) {
	mem := NewMemStore()
	seedWrittenRows(t, mem)
	blind := listRevisionStripper{mem}
	cache := primedCacheForFidelityTest(t, blind)
	for name, s := range map[string]Store{"bare": blind, "cached": cache, "class over cache": SessionStore{Store: cache}} {
		var blindErr *ListRevisionsBlindError
		if err := CheckListRevisionFidelity(s); !errors.As(err, &blindErr) || blindErr.Live == 0 {
			t.Errorf("%s: %v, want a ListRevisionsBlindError naming the live revision", name, err)
		}
	}
}

// Kills a verdict the cache answers: a cache primed with a legacy row at 0
// that has not yet seen the row's first write lists it at 0 while it reads
// live at a real revision, but its backing lists the real one, so the store is
// admitted.
func TestCheckListRevisionFidelityReconfirmsALaggingLegacyRow(t *testing.T) {
	mem := NewMemStoreFrom(1, []Bead{{ID: "gc-1", Title: "legacy", Status: "open", Type: "task"}}, nil)
	cache := primedCacheForFidelityTest(t, mem)
	assignee := "worker"
	if err := mem.Update("gc-1", UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	rows, err := cache.List(ListQuery{Status: "open"})
	if err != nil || len(rows) != 1 || rows[0].Revision != 0 {
		t.Fatalf("cached List = (%+v, %v), want the lagging row at 0", rows, err)
	}
	for name, s := range map[string]Store{"cached": cache, "class over cache": SessionStore{Store: cache}} {
		if err := CheckListRevisionFidelity(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// writeAfterFirstLiveList writes id once, right after the first Live listing
// returns: a write landing between the check's listing and its live read.
type writeAfterFirstLiveList struct {
	Store
	id      string
	written bool
}

func (s *writeAfterFirstLiveList) List(q ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(q)
	if q.Live && !s.written {
		s.written = true
		assignee := "worker"
		if err := s.Update(s.id, UpdateOpts{Assignee: &assignee}); err != nil {
			return nil, err
		}
	}
	return rows, err
}

// Kills a refusal that does not re-confirm: a legacy row written after the
// sample's Live listing reads live off 0, and a Live listing of that row alone
// carries the new revision, so the store is admitted.
func TestCheckListRevisionFidelityReconfirmsARowWrittenBetweenTheReads(t *testing.T) {
	mem := NewMemStoreFrom(1, []Bead{{ID: "gc-1", Title: "legacy", Status: "open", Type: "task"}}, nil)
	if err := CheckListRevisionFidelity(&writeAfterFirstLiveList{Store: mem, id: "gc-1"}); err != nil {
		t.Fatalf("a legacy row written between the reads: %v", err)
	}
	got, err := mem.Get("gc-1")
	if err != nil || got.Revision == 0 {
		t.Fatalf("Get = (%d, %v), want the write to have landed; the fixture no longer races the reads", got.Revision, err)
	}
}

// oneRowStripper lists one row at revision 0 and every other at its own.
type oneRowStripper struct {
	Store
	id string
}

func (s oneRowStripper) List(q ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(q)
	for i := range rows {
		if rows[i].ID == s.id {
			rows[i].Revision = 0
		}
	}
	return rows, err
}

// One row listed at 0 beside rows that carry their revisions is that row's
// anomaly, not a read path that drops revisions, and is no refusal; the same
// row alone is.
func TestCheckListRevisionFidelityRefusesOnlyASystemicDrop(t *testing.T) {
	mem := NewMemStore()
	seedWrittenRows(t, mem)
	if err := CheckListRevisionFidelity(oneRowStripper{Store: mem, id: "gc-1"}); err != nil {
		t.Fatalf("one row at 0 among rows with revisions: %v", err)
	}
	if err := mem.Close("gc-2"); err != nil {
		t.Fatal(err)
	}
	var blind *ListRevisionsBlindError
	if err := CheckListRevisionFidelity(oneRowStripper{Store: mem, id: "gc-1"}); !errors.As(err, &blind) {
		t.Fatalf("the only sampled row at 0: %v, want a refusal", err)
	}
}

// Kills deciding a systemic drop from the cached sample: a cache over a store
// whose list path drops every revision still holds real revisions on the rows
// it wrote itself (a create, an update), which came from point reads. They do
// not vouch for the list path, so the store is still refused.
func TestCheckListRevisionFidelityRefusesBehindPointReadRows(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, cache *CachingStore, newest string){
		"local create": func(t *testing.T, cache *CachingStore, _ string) {
			if _, err := cache.Create(Bead{Title: "created through the cache"}); err != nil {
				t.Fatal(err)
			}
		},
		"local update": func(t *testing.T, cache *CachingStore, newest string) {
			title := "touched"
			if err := cache.Update(newest, UpdateOpts{Title: &title}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mem := NewMemStore()
			var newest string
			for i := 0; i < 3; i++ {
				b, err := mem.Create(Bead{Title: "old"})
				if err != nil {
					t.Fatal(err)
				}
				newest = b.ID
			}
			cache := NewCachingStoreForTest(listRevisionStripper{mem}, nil)
			if err := cache.PrimeActive(); err != nil {
				t.Fatal(err)
			}
			write(t, cache, newest)
			var blind *ListRevisionsBlindError
			if err := CheckListRevisionFidelity(cache); !errors.As(err, &blind) {
				t.Fatalf("C0.7 over a list path that drops every revision = %v, want a refusal", err)
			}
		})
	}
}

// Kills a sample, or a verdict listing, of one tier: a store whose active rows
// are all wisps (a graph's ephemeral steps) is still decided.
func TestCheckListRevisionFidelitySamplesBothTiers(t *testing.T) {
	mem := NewMemStore()
	inProgress := "in_progress"
	for _, title := range []string{"a", "b"} {
		b, err := mem.Create(Bead{Title: title, Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Update(b.ID, UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
	}
	blind := listRevisionStripper{mem}
	for name, s := range map[string]Store{"bare": blind, "cached": activeCacheForFidelityTest(t, blind)} {
		var blindErr *ListRevisionsBlindError
		if err := CheckListRevisionFidelity(s); !errors.As(err, &blindErr) {
			t.Errorf("%s: %v, want a ListRevisionsBlindError", name, err)
		}
	}
}

// Kills a pass on a sample whose rows all carry revisions: a cache whose own
// writes touched every sampled row holds revisions from point reads only, so a
// store whose list path drops every revision is still refused.
func TestCheckListRevisionFidelityRefusesASampleOfPointReadRevisions(t *testing.T) {
	for name, wrap := range map[string]func(*CachingStore) Store{
		"cached":           func(c *CachingStore) Store { return c },
		"class over cache": func(c *CachingStore) Store { return SessionStore{Store: c} },
	} {
		t.Run(name, func(t *testing.T) {
			mem := NewMemStore()
			seedWrittenRows(t, mem)
			seedInProgressRows(t, mem)
			cache := activeCacheForFidelityTest(t, listRevisionStripper{mem})
			title := "touched"
			for _, status := range []string{"open", "in_progress"} {
				rows, err := cache.List(ListQuery{Status: status, TierMode: TierBoth})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, status := range []string{"open", "in_progress"} {
				rows, err := cache.List(ListQuery{Status: status, TierMode: TierBoth})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.Revision == 0 {
						t.Fatalf("cached %s at 0; the fixture no longer point-reads every sampled row", row.ID)
					}
				}
			}
			var blind *ListRevisionsBlindError
			if err := CheckListRevisionFidelity(wrap(cache)); !errors.As(err, &blind) {
				t.Fatalf("C0.7 over point-read rows whose list path drops every revision = %v, want a refusal", err)
			}
		})
	}
}

// The sample-wide listing goes past the cache even when the full prime has
// finished, as it may have by v2 boot: a fully primed cache answers every
// List from rows its own writes installed from point reads. Kills that
// listing reading the cache (dropping its Live).
func TestCheckListRevisionFidelityRefusesBehindPointReadRowsInALiveCache(t *testing.T) {
	mem := NewMemStore()
	for i := 0; i < 3; i++ {
		if _, err := mem.Create(Bead{Title: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewCachingStoreForTest(listRevisionStripper{mem}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Create(Bead{Title: "created through the cache"}); err != nil {
		t.Fatal(err)
	}
	var blind *ListRevisionsBlindError
	if err := CheckListRevisionFidelity(cache); !errors.As(err, &blind) {
		t.Fatalf("C0.7 over a fully primed cache whose backing drops list revisions = %v, want a refusal", err)
	}
}

// Kills a pass on a sample the cache's own creates fill: once the full prime
// has finished, the rows created through the cache since (a formula pour) can
// be the whole sample, each at a revision from a point read, and a store whose
// list path drops every revision is still refused.
func TestCheckListRevisionFidelityRefusesASampleOfRowsCreatedThroughTheCache(t *testing.T) {
	for name, wrap := range map[string]func(*CachingStore) Store{
		"cached":           func(c *CachingStore) Store { return c },
		"class over cache": func(c *CachingStore) Store { return SessionStore{Store: c} },
	} {
		t.Run(name, func(t *testing.T) {
			cache := primedCacheForFidelityTest(t, listRevisionStripper{NewMemStore()})
			for i := 0; i < 3; i++ {
				if _, err := cache.Create(Bead{Title: "created through the cache"}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := cache.List(ListQuery{Status: "open", TierMode: TierBoth})
			if err != nil || len(rows) != 3 {
				t.Fatalf("cached open rows = (%d rows, %v), want the 3 created", len(rows), err)
			}
			for _, row := range rows {
				if row.Revision == 0 {
					t.Fatalf("cached %s at 0; the fixture no longer point-reads every sampled row", row.ID)
				}
			}
			var blind *ListRevisionsBlindError
			if err := CheckListRevisionFidelity(wrap(cache)); !errors.As(err, &blind) {
				t.Fatalf("C0.7 over rows created through a fully primed cache whose backing drops list revisions = %v, want a refusal", err)
			}
		})
	}
}

// cacheWriteDuringLiveList lists every row at revision 0 and, once handed the
// cache over it, writes id through that cache inside its first Live listing: a
// writer landing on a sampled row while the verdict's listing is in flight.
type cacheWriteDuringLiveList struct {
	listRevisionStripper
	cache   *CachingStore
	id      string
	written bool
}

func (s *cacheWriteDuringLiveList) List(q ListQuery) ([]Bead, error) {
	rows, err := s.listRevisionStripper.List(q)
	if q.Live && s.cache != nil && !s.written {
		s.written = true
		title := "written during the listing"
		if err := s.cache.Update(s.id, UpdateOpts{Title: &title}); err != nil {
			return nil, err
		}
	}
	return rows, err
}

// Kills a verdict listing the cache answers: a cache answers a Live listing of
// a row written through it since the listing began with its own copy, whose
// revision came from a point read. On the cache's backing that row lists at 0,
// so the store is still refused.
func TestCheckListRevisionFidelityRefusesAroundAWriteDuringTheListing(t *testing.T) {
	for name, wrap := range map[string]func(*CachingStore) Store{
		"cached":           func(c *CachingStore) Store { return c },
		"class over cache": func(c *CachingStore) Store { return SessionStore{Store: c} },
	} {
		t.Run(name, func(t *testing.T) {
			mem := NewMemStore()
			seedWrittenRows(t, mem)
			backing := &cacheWriteDuringLiveList{listRevisionStripper: listRevisionStripper{mem}, id: "gc-1"}
			cache := activeCacheForFidelityTest(t, backing)
			backing.cache = cache
			var blind *ListRevisionsBlindError
			if err := CheckListRevisionFidelity(wrap(cache)); !errors.As(err, &blind) {
				t.Errorf("C0.7 around a write through the cache during its listing = %v, want a refusal", err)
			}
			if !backing.written {
				t.Fatal("no write landed during a Live listing; the fixture no longer races the verdict")
			}
		})
	}
}

// Kills a verdict listing the cache answers: within its recent-local window, a
// cache answers a Live listing of a row it wrote with its own copy, whose
// revision came from a point read, when the listed row differs (here another
// writer changed it since). On the cache's backing that row lists at 0, so the
// store is still refused.
func TestCheckListRevisionFidelityRefusesBehindARecentLocalWrite(t *testing.T) {
	for name, wrap := range map[string]func(*CachingStore) Store{
		"cached":           func(c *CachingStore) Store { return c },
		"class over cache": func(c *CachingStore) Store { return SessionStore{Store: c} },
	} {
		t.Run(name, func(t *testing.T) {
			mem := NewMemStore()
			seedWrittenRows(t, mem)
			now := time.Now()
			cache := NewCachingStoreForTest(listRevisionStripper{mem}, nil, WithClock(func() time.Time { return now }))
			if err := cache.PrimeActive(); err != nil {
				t.Fatalf("PrimeActive: %v", err)
			}
			assignee := "local"
			if err := cache.Update("gc-1", UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
			title := "written by another process"
			if err := mem.Update("gc-1", UpdateOpts{Title: &title}); err != nil {
				t.Fatal(err)
			}
			rows, err := cache.List(ListQuery{IDs: []string{"gc-1"}, TierMode: TierBoth, Live: true})
			if err != nil || len(rows) != 1 || rows[0].Revision == 0 {
				t.Fatalf("cached Live List = (%+v, %v), want the cache's own copy off 0; the fixture no longer conflicts", rows, err)
			}
			var blind *ListRevisionsBlindError
			if err := CheckListRevisionFidelity(wrap(cache)); !errors.As(err, &blind) {
				t.Fatalf("C0.7 behind a recent local write = %v, want a refusal", err)
			}
		})
	}
}

// backingListCounter counts the List calls that reach it and keeps the
// queries.
type backingListCounter struct {
	Store
	lists   atomic.Int32
	mu      sync.Mutex
	queries []ListQuery
}

func (s *backingListCounter) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	s.mu.Lock()
	s.queries = append(s.queries, q)
	s.mu.Unlock()
	return s.Store.List(q)
}

// reset forgets the calls so far.
func (s *backingListCounter) reset() {
	s.lists.Store(0)
	s.mu.Lock()
	s.queries = nil
	s.mu.Unlock()
}

// Kills a boot sample the cache cannot serve: over a cache primed only with
// its active rows, as at boot, the sample is served from memory, and the
// backing sees one read, the Live listing of the sampled rows.
func TestCheckListRevisionFidelitySamplesFromTheCache(t *testing.T) {
	mem := NewMemStore()
	seedWrittenRows(t, mem)
	seedInProgressRows(t, mem)
	backing := &backingListCounter{Store: mem}
	cache := activeCacheForFidelityTest(t, backing)
	backing.reset()
	if err := CheckListRevisionFidelity(cache); err != nil {
		t.Fatalf("C0.7: %v", err)
	}
	if n := backing.lists.Load(); n != 1 {
		t.Fatalf("the check reached the backing %d times, want once", n)
	}
	backing.mu.Lock()
	q := backing.queries[0]
	backing.mu.Unlock()
	if !q.Live || len(q.IDs) != 4 || q.Status != "" || q.IncludeClosed {
		t.Fatalf("the backing read %+v, want a Live listing of the 4 sampled ids", q)
	}
}

// The cost predicate doctor reads: the verdict's Live listing walks native
// Dolt's non-closed rows whether or not a cache is in front, followed through
// declared resolve targets either way.
func TestListRevisionCheckIsCheap(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStoreForTest(native, nil)
	memCache := NewCachingStoreForTest(NewMemStore(), nil)
	for name, tc := range map[string]struct {
		store Store
		cheap bool
	}{
		"native":                  {native, false},
		"class over native":       {SessionStore{Store: native}, false},
		"cache over native":       {cache, false},
		"class over cache":        {SessionStore{Store: cache}, false},
		"cache over mem":          {memCache, true},
		"class over cache of mem": {SessionStore{Store: memCache}, true},
		"mem":                     {NewMemStore(), true},
		"class over mem":          {SessionStore{Store: NewMemStore()}, true},
	} {
		if got := ListRevisionCheckIsCheap(tc.store); got != tc.cheap {
			t.Errorf("%s: cheap = %v, want %v", name, got, tc.cheap)
		}
	}
}

// A failed read is not a pass, and not a verdict on the store either: neither
// a refusal nor undecided.
func TestCheckListRevisionFidelityFailsClosedOnAReadError(t *testing.T) {
	seeded := func() Store {
		mem := NewMemStore()
		seedWrittenRows(t, mem)
		return mem
	}
	for name, s := range map[string]Store{
		"sample":       listErrStore{seeded()},
		"live listing": liveListErrStore{seeded()},
		"live read":    getErrStripper{listRevisionStripper{seeded()}},
	} {
		err := CheckListRevisionFidelity(s)
		var blind *ListRevisionsBlindError
		var undecided *ListRevisionsUndecidedError
		if err == nil || errors.As(err, &blind) || errors.As(err, &undecided) {
			t.Errorf("%s: a failed read = %v, want a read error that is no verdict", name, err)
		}
	}
}
