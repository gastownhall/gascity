package beads_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// openPinnedFileStore opens a fresh FileStore that honors pinned ids. The
// HonorExplicitIDs knob is what the op journal's create-if-absent writers and
// the pinned-create capability share with plain Create.
func openPinnedFileStore(t *testing.T, path string) *beads.FileStore {
	t.Helper()
	fs, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	fs.HonorExplicitIDs = true
	return fs
}

// TestPinnedCreateFileStorePassesDuplicateConformance runs the shared
// pinned-id suite against the FileStore, the file-backed leg the in-memory
// and database stores already run.
func TestPinnedCreateFileStorePassesDuplicateConformance(t *testing.T) {
	beadstest.RunPinnedIDDuplicateConformance(t, func(t *testing.T) beads.Store {
		return openPinnedFileStore(t, filepath.Join(t.TempDir(), "beads.json"))
	})
}

// TestPinnedCreateFileStoreSeparateHandlesOneInserts pins the cross-handle
// collision: two FileStore handles over one file race a pinned create, and a
// reopened handle refuses the id the other one took instead of overwriting
// or reminting it.
func TestPinnedCreateFileStoreSeparateHandlesOneInserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	first := openPinnedFileStore(t, path)

	created, err := first.CreatePinned(beads.Bead{ID: "gc-77", Title: "first handle"})
	if err != nil {
		t.Fatalf("CreatePinned(gc-77): %v", err)
	}
	if created.ID != "gc-77" {
		t.Fatalf("CreatePinned(gc-77) returned %q, want the exact pinned id", created.ID)
	}

	second := openPinnedFileStore(t, path)
	if _, err := second.CreatePinned(beads.Bead{ID: "gc-77", Title: "second handle"}); err == nil {
		t.Fatal("CreatePinned(gc-77) over the row another handle created succeeded; the loser must learn its slot was taken")
	}
	stored, err := second.Get("gc-77")
	if err != nil {
		t.Fatalf("Get(gc-77) through the second handle: %v", err)
	}
	if stored.Title != "first handle" {
		t.Fatalf("stored row = %q, want the first handle's row unchanged", stored.Title)
	}
}

// pinnedPolicyWrapper models the cmd/gc policy store's shape: a wrapper that
// declares the leaf as its conditional-writes resolution target, so every
// optional capability resolver (PinnedIDCreatorFor included) sees past it.
type pinnedPolicyWrapper struct {
	beads.Store
}

func (w pinnedPolicyWrapper) ConditionalWritesResolveTarget() beads.Store { return w.Store }

// pinnedHidingWrapper models a wrapper that neither implements nor declares
// the pinned-create capability: optional capabilities are not promoted
// through interface embedding, so the capability is genuinely absent.
type pinnedHidingWrapper struct {
	beads.Store
}

// TestPinnedCreateCachingStoreDelegatesAndRefusesWhenBackingCannotPin covers
// the cache leg: the capability resolves through the conditional backing —
// a declared policy wrapper hides nothing — and a backing that cannot pin
// ids refuses ErrPinnedCreateUnsupported before anything is written.
func TestPinnedCreateCachingStoreDelegatesAndRefusesWhenBackingCannotPin(t *testing.T) {
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(pinnedPolicyWrapper{Store: backing}, nil)

	created, err := cache.CreatePinned(beads.Bead{ID: "gc-5", Title: "through the cache"})
	if err != nil {
		t.Fatalf("cache CreatePinned(gc-5): %v", err)
	}
	if created.ID != "gc-5" {
		t.Fatalf("cache CreatePinned(gc-5) returned %q, want the exact pinned id", created.ID)
	}
	if _, err := cache.CreatePinned(beads.Bead{ID: "gc-5", Title: "duplicate"}); err == nil {
		t.Fatal("cache CreatePinned over an existing pinned id succeeded; the occupant must refuse")
	}
	stored, err := cache.Get("gc-5")
	if err != nil {
		t.Fatalf("cache Get(gc-5): %v", err)
	}
	if stored.Title != "through the cache" {
		t.Fatalf("stored row = %q, want the first pinned create unchanged", stored.Title)
	}

	// A backing that cannot pin ids refuses before any mutation: no row is
	// created, and the typed refusal names the absence.
	uncapable := beads.NewCachingStoreForTest(pinnedHidingWrapper{Store: beads.NewMemStore()}, nil)
	_, err = uncapable.CreatePinned(beads.Bead{ID: "gc-6", Title: "never created"})
	if !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
		t.Fatalf("cache CreatePinned over an uncapable backing = %v, want ErrPinnedCreateUnsupported", err)
	}
	if rows, err := uncapable.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("rows after the refused pinned create = %d, want 0 (refusal must precede entry mutation)", len(rows))
	}
}

// TestPinnedCreateResolvesThroughClassHandleAndDeclaredWrapper pins the
// resolution routes callers actually use: the typed class handle (beads.As
// walks the class layers to the capability's XFor helper) and a wrapper that
// declares the leaf as its resolution target.
func TestPinnedCreateResolvesThroughClassHandleAndDeclaredWrapper(t *testing.T) {
	store := beads.NewMemStore()

	if _, ok := beads.As[beads.PinnedIDCreator](beadstest.Work(store)); !ok {
		t.Fatal("beads.As[beads.PinnedIDCreator] through a WorkStore handle did not resolve; the typed handle must not hide the capability")
	}
	if _, ok := beads.PinnedIDCreatorFor(pinnedPolicyWrapper{Store: store}); !ok {
		t.Fatal("PinnedIDCreatorFor through a resolve-target-declaring wrapper did not resolve; the declaration must be honored")
	}
	if _, ok := beads.PinnedIDCreatorFor(pinnedHidingWrapper{Store: store}); ok {
		t.Fatal("PinnedIDCreatorFor through a hiding wrapper resolved; an undeclared wrapper must hide the capability so callers take their own fallback")
	}
}

// TestPinnedCreateBdStoreOldVersionRefusesWithoutOverwriting pins the gate:
// against a bd release whose `bd create --id` upserts (every release below
// v1.3.0), CreatePinned refuses ErrPinnedCreateUnsupported BEFORE bd runs,
// so the stored row is exactly as it was — not silently reset by the upsert
// the gate kept from ever executing.
func TestPinnedCreateBdStoreOldVersionRefusesWithoutOverwriting(t *testing.T) {
	fake := beadstest.NewFakeBd()
	fake.SetVersion("1.2.2")
	if _, err := fake.Seed(beads.Bead{ID: "gc-9", Title: "occupant"}); err != nil {
		t.Fatalf("Seed(gc-9): %v", err)
	}
	store := fake.NewBdStore()

	_, err := store.CreatePinned(beads.Bead{ID: "gc-9", Title: "attacker"})
	if !errors.Is(err, beads.ErrPinnedCreateUnsupported) {
		t.Fatalf("CreatePinned against bd 1.2.2 = %v, want ErrPinnedCreateUnsupported (the gate must refuse before bd runs)", err)
	}
	stored, err := fake.Bead("gc-9")
	if err != nil {
		t.Fatalf("fake Bead(gc-9): %v", err)
	}
	if stored.Title != "occupant" {
		t.Fatalf("stored row = %q, want the occupant untouched (the old-bd upsert must never run)", stored.Title)
	}
}

// TestPinnedCreateBdStoreCurrentVersionIsInsertOnly pins the capable leg
// through the fake bd: the pinned create reports the exact id, and a
// duplicate is refused without resetting the stored row.
func TestPinnedCreateBdStoreCurrentVersionIsInsertOnly(t *testing.T) {
	fake := beadstest.NewFakeBd()
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
	if stored.Title != "first" {
		t.Fatalf("stored row = %q, want the first create unchanged by the refused duplicate", stored.Title)
	}
}
