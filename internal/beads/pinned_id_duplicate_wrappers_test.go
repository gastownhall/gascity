package beads_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Create-if-absent callers that pin an id per logical slot (the op journal's
// op ids, the extmsg transcript's entry id per conversation sequence) rely on
// the store refusing a second create at that id. SQLiteStore, NativeDoltStore,
// BdStore and an honoring MemStore run the suite in their own conformance
// tests; these cover the file store and the cache the controller reads through.

func openHonoringFileStore(t *testing.T, path string) *beads.FileStore {
	t.Helper()
	fs, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	fs.HonorExplicitIDs = true
	return fs
}

func primedCacheOver(t *testing.T, backing beads.Store) beads.Store {
	t.Helper()
	c := beads.NewCachingStoreForTest(backing, nil)
	if err := c.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return c
}

func TestFileStoreHonoringIDsPinnedIDDuplicateConformance(t *testing.T) {
	beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
		return openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json"))
	})
}

// TestFileStoreHonoringIDsRefusesAPinnedIDAnotherHandleStored pins the
// cross-process half: two handles on one file (the controller and a CLI) each
// reload under the file lock before a write, so the second handle refuses a
// pinned id the first one stored after the second opened.
func TestFileStoreHonoringIDsRefusesAPinnedIDAnotherHandleStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	first := openHonoringFileStore(t, path)
	second := openHonoringFileStore(t, path)

	if _, err := first.Create(beads.Bead{ID: "gc-slot-1", Title: "first"}); err != nil {
		t.Fatalf("first handle Create: %v", err)
	}
	if got, err := second.Create(beads.Bead{ID: "gc-slot-1", Title: "second"}); err == nil {
		t.Fatalf("second handle stored %q over the pinned id the first handle holds", got.ID)
	}
	stored, err := first.Get("gc-slot-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Title != "first" {
		t.Fatalf("pinned row title = %q, want first", stored.Title)
	}
}

func TestCachingStorePinnedIDDuplicateConformance(t *testing.T) {
	t.Run("OverHonoringMemStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			mem := beads.NewMemStore()
			mem.HonorExplicitIDs = true
			return primedCacheOver(t, mem)
		})
	})
	t.Run("OverHonoringFileStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			return primedCacheOver(t, openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json")))
		})
	})
	t.Run("OverSQLiteStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			return primedCacheOver(t, newSQLiteForConformance(t))
		})
	})
}
