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
			if len(args) > 0 && args[0] == "version" {
				return []byte("bd version " + beads.BdPinnedCreateMinVersion + "\n"), nil
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

func TestPinnedCreateFileStoreSeparateHandlesOneInserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	open := func() *beads.FileStore {
		t.Helper()
		fs, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		return fs
	}
	first, second := open(), open()

	created, err := first.CreatePinned(beads.Bead{ID: "gc-77", Title: "first handle"})
	if err != nil {
		t.Fatalf("CreatePinned(gc-77): %v", err)
	}
	if created.ID != "gc-77" {
		t.Fatalf("CreatePinned(gc-77) returned %q, want the exact pinned id", created.ID)
	}
	// second was opened before the create, so only a reload under the file
	// lock can tell it the id is taken.
	if _, err := second.CreatePinned(beads.Bead{ID: "gc-77", Title: "second handle"}); err == nil {
		t.Fatal("CreatePinned(gc-77) through a second handle succeeded over the row the first handle created")
	}
	stored, err := open().Get("gc-77")
	if err != nil {
		t.Fatalf("Get(gc-77): %v", err)
	}
	if stored.Title != "first handle" {
		t.Fatalf("stored row = %q, want the first handle's row unchanged", stored.Title)
	}
}

// pinnedResolveTargetWrapper models the cmd/gc policy store's shape: a
// wrapper that declares the leaf as its conditional-writes resolution target.
type pinnedResolveTargetWrapper struct {
	beads.Store
}

func (w pinnedResolveTargetWrapper) ConditionalWritesResolveTarget() beads.Store { return w.Store }

func TestPinnedCreateResolvesThroughClassHandleAndDeclaredWrapper(t *testing.T) {
	store := beads.NewMemStore()
	if _, ok := beads.As[beads.PinnedIDCreator](beadstest.Work(store)); !ok {
		t.Fatal("As[PinnedIDCreator] through a WorkStore handle = false; the typed handle must not hide the capability")
	}
	if _, ok := beads.PinnedIDCreatorFor(pinnedResolveTargetWrapper{Store: store}); !ok {
		t.Fatal("PinnedIDCreatorFor through a wrapper declaring its resolution target = false")
	}
	if _, ok := beads.As[beads.PinnedIDCreator](beads.MailStore{Store: pinnedResolveTargetWrapper{Store: store}}); !ok {
		t.Fatal("As[PinnedIDCreator] through a class handle over a declared wrapper = false")
	}
	if _, ok := beads.PinnedIDCreatorFor(capabilityHidingStore{store}); ok {
		t.Fatal("PinnedIDCreatorFor through an undeclared wrapper = true; it must not guess at unwrapping")
	}
}

func TestPinnedCreateBdStoreOldVersionRefusesWithoutOverwriting(t *testing.T) {
	fake := beadstest.NewFakeBd()
	fake.SetVersion("1.2.2")
	// The old bd advertises conditional writes, so only the version gate
	// stands between the pinned create and its --id upsert.
	fake.AdvertiseConditionalWrites()
	if _, err := fake.Seed(beads.Bead{ID: "gc-9", Title: "occupant"}); err != nil {
		t.Fatalf("Seed(gc-9): %v", err)
	}
	store := fake.NewBdStore()

	_, err := store.CreatePinned(beads.Bead{ID: "gc-9", Title: "overwriter"})
	if !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
		t.Fatalf("CreatePinned against bd 1.2.2 = %v, want ErrPinnedCreateUnsupported", err)
	}
	stored, err := fake.Bead("gc-9")
	if err != nil {
		t.Fatalf("fake Bead(gc-9): %v", err)
	}
	if stored.Title != "occupant" {
		t.Fatalf("stored row = %q, want the occupant untouched (the old-bd upsert must never run)", stored.Title)
	}
	if _, err := store.CreatePinned(beads.Bead{ID: "gc-10", Title: "fresh"}); !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
		t.Fatalf("second CreatePinned against bd 1.2.2 = %v, want the memoized refusal", err)
	}
	if _, err := fake.Bead("gc-10"); err == nil {
		t.Fatal("a refused pinned create wrote gc-10")
	}
}

func TestPinnedCreateBdStoreCurrentVersionIsInsertOnly(t *testing.T) {
	fake := beadstest.NewFakeBd()
	fake.AdvertiseConditionalWrites()
	store := fake.NewBdStore()

	created, err := store.CreatePinned(beads.Bead{ID: "gc-9", Title: "first", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("CreatePinned(gc-9): %v", err)
	}
	if created.ID != "gc-9" {
		t.Fatalf("CreatePinned(gc-9) returned %q, want the exact pinned id", created.ID)
	}
	if _, err := store.CreatePinned(beads.Bead{ID: "gc-9", Title: "second"}); err == nil {
		t.Fatal("CreatePinned over an existing pinned id succeeded; a duplicate must be refused")
	}
	stored, err := fake.Bead("gc-9")
	if err != nil {
		t.Fatalf("fake Bead(gc-9): %v", err)
	}
	if stored.Title != "first" || stored.Metadata["k"] != "v" {
		t.Fatalf("stored row = %q %v, want the first create unchanged by the refused duplicate", stored.Title, stored.Metadata)
	}
}
