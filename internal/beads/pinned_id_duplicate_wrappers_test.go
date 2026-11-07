package beads_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// openHonoringFileStore opens a FileStore that honors pinned ids.
func openHonoringFileStore(t *testing.T, path string) *beads.FileStore {
	t.Helper()
	fs, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	fs.HonorExplicitIDs = true
	return fs
}

// TestFileStorePinnedIDDuplicateConformance: the file store that honors
// pinned ids (the transcript backend a local city can run on) must keep the
// pinned-id fence — occupied ids refused without mutation, child-derived ids
// preserved and read exactly.
func TestFileStorePinnedIDDuplicateConformance(t *testing.T) {
	beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
		return openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json"))
	})
}

// TestCachingStorePinnedIDDuplicateConformance: the controller's cached
// handle must not weaken the backing's pinned-id fence. The cache forwards
// Create to the backing and refreshes from it, so a refused create has to
// surface as an error and the stored row has to stay readable exactly.
func TestCachingStorePinnedIDDuplicateConformance(t *testing.T) {
	openOver := func(t *testing.T, backing beads.Store) beads.Store {
		t.Helper()
		c := beads.NewCachingStoreForTest(backing, nil)
		if err := c.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return c
	}
	t.Run("over honoring MemStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			mem := beads.NewMemStore()
			mem.HonorExplicitIDs = true
			return openOver(t, mem)
		})
	})
	t.Run("over honoring FileStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			return openOver(t, openHonoringFileStore(t, filepath.Join(t.TempDir(), "beads.json")))
		})
	})
	t.Run("over SQLiteStore", func(t *testing.T) {
		beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
			return openOver(t, newSQLiteForConformance(t))
		})
	})
}

// TestFileStoreSeparateHandlesRefuseEachOthersPinnedID: two handles on the
// same beads.json are two writers with no shared memory — the shape a second
// gc process takes. A pinned id one handle stored must be refused (never
// overwritten) by the other, and stay exactly readable from both.
func TestFileStoreSeparateHandlesRefuseEachOthersPinnedID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	a := openHonoringFileStore(t, path)
	b := openHonoringFileStore(t, path)

	minted, err := a.Create(beads.Bead{Title: "namespace probe"})
	if err != nil {
		t.Fatalf("Create(minted): %v", err)
	}
	cut := len(minted.ID)
	for cut > 0 && minted.ID[cut-1] != '-' {
		cut--
	}
	if cut <= 0 {
		t.Fatalf("minted id %q has no prefix to pin under", minted.ID)
	}
	pin := minted.ID[:cut-1] + "-pinned"

	first, err := a.Create(beads.Bead{ID: pin, Title: "from handle A"})
	if err != nil {
		t.Fatalf("Create(%q) on handle A: %v", pin, err)
	}
	if first.ID != pin {
		t.Fatalf("Create(%q) minted %q on handle A", pin, first.ID)
	}

	if _, err := b.Create(beads.Bead{ID: pin, Title: "from handle B"}); err == nil {
		t.Fatalf("Create(%q) on handle B over handle A's row succeeded; two writers on one file must not overwrite each other's pinned rows", pin)
	}

	after, err := b.Get(pin)
	if err != nil {
		t.Fatalf("Get(%q) on handle B: %v", pin, err)
	}
	if after.Title != "from handle A" {
		t.Fatalf("row %q reads %q from handle B; the refused create changed it", pin, after.Title)
	}
}
