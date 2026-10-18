package beads

import (
	"context"
	"errors"
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

// A failed sample is not a pass.
func TestCheckListRevisionFidelityFailsClosedOnAReadError(t *testing.T) {
	if err := CheckListRevisionFidelity(listErrStore{NewMemStore()}); err == nil {
		t.Fatal("a failed list read passed the check")
	}
}
