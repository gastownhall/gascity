package beads_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestPinnedCreateMemStoreConformance(t *testing.T) {
	// The default MemStore mints over a supplied id on Create; CreatePinned
	// must honor it anyway, without flipping the shared knob.
	beadstest.RunPinnedCreateConformance(t, func(*testing.T) beads.Store {
		return beads.NewMemStore()
	}, nil)

	store := beads.NewMemStore()
	if _, err := store.CreatePinned(beads.Bead{ID: "gc-pinned", Title: "pinned"}); err != nil {
		t.Fatalf("CreatePinned: %v", err)
	}
	if store.HonorExplicitIDs {
		t.Fatal("CreatePinned flipped HonorExplicitIDs on")
	}
	minted, err := store.Create(beads.Bead{ID: "gc-other", Title: "ordinary"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if minted.ID == "gc-other" {
		t.Fatal("ordinary Create on a default MemStore honored a supplied id; CreatePinned leaked into Create")
	}
}

func TestPinnedCreateFileStoreConformance(t *testing.T) {
	open := func(t *testing.T, path string) *beads.FileStore {
		t.Helper()
		fs, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		return fs
	}
	paths := map[beads.Store]string{}
	beadstest.RunPinnedCreateConformance(t, func(t *testing.T) beads.Store {
		path := filepath.Join(t.TempDir(), "beads.json")
		fs := open(t, path)
		paths[fs] = path
		return fs
	}, func(t *testing.T, s beads.Store) beads.Store {
		return open(t, paths[s])
	})

	t.Run("ReopenSeesThePinnedRowAndRefusesIt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "beads.json")
		first := open(t, path)
		if _, err := first.CreatePinned(beads.Bead{ID: "gc-reopen", Title: "first"}); err != nil {
			t.Fatalf("CreatePinned: %v", err)
		}
		reopened := open(t, path)
		if _, err := reopened.CreatePinned(beads.Bead{ID: "gc-reopen", Title: "second"}); err == nil {
			t.Fatal("a reopened FileStore re-created a pinned id the file already holds")
		}
		got, err := reopened.Get("gc-reopen")
		if err != nil || got.Title != "first" {
			t.Fatalf("Get after reopen = (%q, %v), want first", got.Title, err)
		}
	})
}

func TestPinnedCreateSQLiteConformance(t *testing.T) {
	beadstest.RunPinnedCreateConformance(t, func(t *testing.T) beads.Store {
		return newSQLiteForConformance(t)
	}, nil)
}

func TestPinnedCreateNativeDoltConformance(t *testing.T) {
	beadstest.RunPinnedCreateConformance(t, func(*testing.T) beads.Store {
		return beads.NewNativeDoltStoreForPinnedIDFenceConformance("kitn")
	}, nil)
}

func TestPinnedCreateCachingStoreConformance(t *testing.T) {
	beadstest.RunPinnedCreateConformance(t, func(*testing.T) beads.Store {
		return beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	}, nil)
}

func TestPinnedCreateClassWrapperConformance(t *testing.T) {
	beadstest.RunPinnedCreateConformance(t, func(*testing.T) beads.Store {
		return beads.MailStore{Store: beads.NewCachingStoreForTest(beads.NewMemStore(), nil)}
	}, nil)
	if _, ok := beads.As[beads.PinnedIDCreator](beads.MailStore{Store: beads.NewMemStore()}); !ok {
		t.Fatal("As[PinnedIDCreator] through a class handle = false; As must route through PinnedIDCreatorFor")
	}
}

// capabilityHidingStore embeds a Store and so promotes only the Store
// interface: every optional capability of the wrapped store is hidden.
type capabilityHidingStore struct {
	beads.Store
}

func TestPinnedCreateUnsupportedStoresRefuseBeforeWriting(t *testing.T) {
	t.Run("a wrapper that hides the capability resolves absent", func(t *testing.T) {
		if _, ok := beads.PinnedIDCreatorFor(capabilityHidingStore{beads.NewMemStore()}); ok {
			t.Fatal("PinnedIDCreatorFor guessed through an undeclared wrapper")
		}
	})
	t.Run("a cache over an incapable backing refuses without writing", func(t *testing.T) {
		backing := beads.NewMemStore()
		cache := beads.NewCachingStoreForTest(capabilityHidingStore{backing}, nil)
		_, err := cache.CreatePinned(beads.Bead{ID: "gc-refused", Title: "refused"})
		if !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
			t.Fatalf("CreatePinned over an incapable backing = %v, want ErrPinnedCreateUnsupported", err)
		}
		rows, err := backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("refused pinned create wrote %d rows", len(rows))
		}
	})
	t.Run("a bd without conditional writes refuses before running create", func(t *testing.T) {
		var created bool
		store := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				created = true
			}
			return []byte("Usage:\n  bd x [flags]\n\nFlags:\n  --json   emit JSON\n"), nil
		})
		_, err := store.CreatePinned(beads.Bead{ID: "gc-old", Title: "old bd"})
		if !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
			t.Fatalf("CreatePinned on an incapable bd = %v, want ErrPinnedCreateUnsupported", err)
		}
		if created {
			t.Fatal("CreatePinned ran bd create on a bd that would upsert the pinned id")
		}
	})
}
