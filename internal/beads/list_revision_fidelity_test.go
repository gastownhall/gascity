package beads

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
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

// A legacy row lists at 0 and reads live at 0: 0 is its real revision.
func TestCheckListRevisionFidelityAdmitsALegacyRow(t *testing.T) {
	store := NewMemStoreFrom(1, []Bead{{ID: "gc-1", Title: "legacy", Status: "open", Type: "task"}}, nil)
	if err := CheckListRevisionFidelity(store); err != nil {
		t.Fatalf("legacy row: %v", err)
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

// Kills a refusal that does not re-confirm: a cache primed with a legacy row
// at 0 that has not yet seen the row's first write lists it at 0 while it
// reads live at a real revision, but its backing lists the real one, so the
// store is admitted.
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

// backingListCounter counts the List calls that reach it.
type backingListCounter struct {
	Store
	lists atomic.Int32
}

func (s *backingListCounter) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	return s.Store.List(q)
}

// Kills a boot sample the cache cannot serve: over a cache primed only with
// its active rows, as at boot, the sample reaches the backing not at all.
func TestCheckListRevisionFidelitySamplesFromTheCache(t *testing.T) {
	mem := NewMemStore()
	seedWrittenRows(t, mem)
	backing := &backingListCounter{Store: mem}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	backing.lists.Store(0)
	if err := CheckListRevisionFidelity(cache); err != nil {
		t.Fatalf("C0.7: %v", err)
	}
	if n := backing.lists.Load(); n != 0 {
		t.Fatalf("the sample reached the backing %d times, want it served from the cache", n)
	}
}

// The cost predicate doctor reads: native Dolt is a scan unless a cache
// answers the sample, followed through declared resolve targets either way.
func TestListRevisionSampleIsCheap(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStoreForTest(native, nil)
	for name, tc := range map[string]struct {
		store Store
		cheap bool
	}{
		"native":            {native, false},
		"class over native": {SessionStore{Store: native}, false},
		"cache over native": {cache, true},
		"class over cache":  {SessionStore{Store: cache}, true},
		"mem":               {NewMemStore(), true},
		"class over mem":    {SessionStore{Store: NewMemStore()}, true},
	} {
		if got := ListRevisionSampleIsCheap(tc.store); got != tc.cheap {
			t.Errorf("%s: cheap = %v, want %v", name, got, tc.cheap)
		}
	}
}

// A failed sample is not a pass, and not a verdict on the store either.
func TestCheckListRevisionFidelityFailsClosedOnAReadError(t *testing.T) {
	err := CheckListRevisionFidelity(listErrStore{NewMemStore()})
	var blind *ListRevisionsBlindError
	if err == nil || errors.As(err, &blind) {
		t.Fatalf("a failed list read = %v, want a read error that is no refusal", err)
	}
}
