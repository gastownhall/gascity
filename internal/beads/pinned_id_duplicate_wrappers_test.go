package beads_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

func openHonoringFileStore(t *testing.T, path string) *beads.FileStore {
	t.Helper()
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("OpenFileStore(%q): %v", path, err)
	}
	s.HonorExplicitIDs = true
	return s
}

// TestFileStoreHonoringIDsPinnedIDConformance pins the duplicate-pinned-id
// contract for a FileStore that honors explicit ids.
func TestFileStoreHonoringIDsPinnedIDConformance(t *testing.T) {
	beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
		return openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json"))
	})
}

// TestCachingStorePinnedIDConformance pins that a primed CachingStore keeps
// the duplicate-pinned-id contract of each honoring backend it wraps.
func TestCachingStorePinnedIDConformance(t *testing.T) {
	backings := map[string]func(t *testing.T) beads.Store{
		"MemStore": func(*testing.T) beads.Store {
			mem := beads.NewMemStore()
			mem.HonorExplicitIDs = true
			return mem
		},
		"FileStore": func(t *testing.T) beads.Store {
			return openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json"))
		},
		"SQLiteStore": func(t *testing.T) beads.Store { return newSQLiteForConformance(t) },
	}
	for name, backing := range backings {
		t.Run(name, func(t *testing.T) {
			beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
				cs := beads.NewCachingStoreForTest(backing(t), nil)
				if err := cs.Prime(context.Background()); err != nil {
					t.Fatalf("Prime: %v", err)
				}
				return cs
			})
		})
	}
}

// TestFileStoreHandlesRaceForOnePinnedID: two FileStore handles on one file,
// each locking it with its flock, race to create the same pinned id. Exactly
// one create wins, and both handles then read the winner's row.
func TestFileStoreHandlesRaceForOnePinnedID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	handles := []*beads.FileStore{openHonoringFileStore(t, path), openHonoringFileStore(t, path)}
	parent, err := handles[0].Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	for round := range 20 {
		id := parent.ID + "-" + string(rune('a'+round))
		titles := []string{"from handle 0", "from handle 1"}
		errs := make([]error, len(handles))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, h := range handles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = h.Create(beads.Bead{ID: id, Title: titles[i]})
			}()
		}
		close(start)
		wg.Wait()

		winner := -1
		for i, err := range errs {
			if err == nil {
				if winner >= 0 {
					t.Fatalf("round %d: both handles created %q", round, id)
				}
				winner = i
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: neither handle created %q: %v", round, id, errs)
		}
		for i, h := range handles {
			got, err := h.Get(id)
			if err != nil {
				t.Fatalf("round %d: handle %d Get(%q): %v", round, i, id, err)
			}
			if got.Title != titles[winner] {
				t.Fatalf("round %d: handle %d reads %q at %q, want the winner's %q", round, i, got.Title, id, titles[winner])
			}
		}
	}
}
