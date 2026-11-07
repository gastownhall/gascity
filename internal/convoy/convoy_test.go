package convoy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func testConvoyDeps() ConvoyDeps {
	return ConvoyDeps{
		Cfg:      &config.City{},
		Recorder: events.NewFake(),
	}
}

func TestConvoyCreateOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	result, err := ConvoyCreate(deps, MemberClasses{Convoy: beadstest.Work(store)}, ConvoyCreateInput{
		Title: "my convoy",
		Fields: ConvoyFields{
			Owner:  "mayor",
			Target: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Convoy.Title != "my convoy" {
		t.Errorf("title = %q, want %q", result.Convoy.Title, "my convoy")
	}
	if result.Convoy.Type != "convoy" {
		t.Errorf("type = %q, want convoy", result.Convoy.Type)
	}
	// Verify metadata was applied.
	got, err := store.Get(result.Convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want mayor", got.Metadata["convoy.owner"])
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	if len(fake.Events) == 0 {
		t.Error("expected ConvoyCreated event to be emitted")
	}
}

func TestConvoyCreateWithItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	// Create child beads first.
	epic, _ := store.Create(beads.Bead{Title: "epic", Type: "epic"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: epic.ID})
	b2, _ := store.Create(beads.Bead{Title: "task 2"})

	result, err := ConvoyCreate(deps, MemberClasses{Convoy: beadstest.Work(store)}, ConvoyCreateInput{
		Title: "linked convoy",
		Items: []string{b1.ID, b2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LinkedCount != 2 {
		t.Errorf("linked = %d, want 2", result.LinkedCount)
	}

	// Verify children are tracked without evicting their existing parent.
	child1, _ := store.Get(b1.ID)
	if child1.ParentID != epic.ID {
		t.Errorf("child1 parent = %q, want preserved epic parent %q", child1.ParentID, epic.ID)
	}
	requireTracksDep(t, store, result.Convoy.ID, b1.ID)
	requireTracksDep(t, store, result.Convoy.ID, b2.ID)
}

func TestConvoyProgressOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if _, err := store.Create(beads.Bead{Title: "task 2", ParentID: convoy.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 2 {
		t.Errorf("total = %d, want 2", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if progress.Complete {
		t.Error("expected not complete")
	}
}

func TestConvoyProgressCompleteOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Complete {
		t.Error("expected complete")
	}
}

func TestConvoyProgressTracksDepsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1"})
	b2, _ := store.Create(beads.Bead{Title: "task 2"})
	if err := store.DepAdd(convoy.ID, b1.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, b2.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 2 {
		t.Errorf("total = %d, want 2", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if progress.Complete {
		t.Error("expected not complete")
	}
}

func TestConvoyProgressTreatsTombstoneAsCompleteOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1"})
	if err := store.DepAdd(convoy.ID, b1.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	tombstone := "tombstone"
	if err := store.Update(b1.ID, beads.UpdateOpts{Status: &tombstone}); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 1 {
		t.Errorf("total = %d, want 1", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if !progress.Complete {
		t.Error("expected tombstone tracked item to complete convoy")
	}
}

func TestConvoyMembersKeepsDanglingTracksUnknownOps(t *testing.T) {
	store := beads.NewMemStore()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	if err := store.DepAdd(convoy.ID, "gc-missing", "tracks"); err != nil {
		t.Fatal(err)
	}

	members, err := MembersIn(MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if members[0].ID != "gc-missing" {
		t.Errorf("member ID = %q, want gc-missing", members[0].ID)
	}
	if members[0].Status != "unknown" {
		t.Errorf("member status = %q, want unknown", members[0].Status)
	}
}

func TestConvoyMembersKeepsMalformedTrackedItemUnknownOps(t *testing.T) {
	backing := beads.NewMemStore()
	convoy, _ := backing.Create(beads.Bead{Title: "test", Type: "convoy"})
	item, _ := backing.Create(beads.Bead{Title: "task"})
	if err := backing.DepAdd(convoy.ID, item.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	store := metadataParseErrorStore{Store: backing, corruptID: item.ID}

	members, err := MembersIn(MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if members[0].ID != item.ID {
		t.Errorf("member ID = %q, want %s", members[0].ID, item.ID)
	}
	if members[0].Status != "unknown" {
		t.Errorf("member status = %q, want unknown", members[0].Status)
	}
}

// TestMembersInLiveReadsEveryNamedClassPastItsCache: a caching store answers
// MembersIn from its cache, the convoy's tracks edges and each member bead
// alike, so an item tracked, and a member reopened, out of process stay
// invisible to it until the cache absorbs them. MembersInLive reads the
// convoy's edges, and every member in each named class, through that class's
// live handle, so it sees both, a member owned by another class included.
func TestMembersInLiveReadsEveryNamedClassPastItsCache(t *testing.T) {
	convoyBacking := beads.NewMemStore()
	memberBacking := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "fe-1", Title: "reopened item", Type: "task", Status: "open"}}, nil)
	convoy, err := convoyBacking.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoyBacking.DepAdd(convoy.ID, "fe-1", TrackingDepType); err != nil {
		t.Fatal(err)
	}
	convoyCache := beads.NewCachingStoreForTest(convoyBacking, nil)
	memberCache := beads.NewCachingStoreForTest(memberBacking, nil)
	for _, cache := range []*beads.CachingStore{convoyCache, memberCache} {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := memberCache.Close("fe-1"); err != nil {
		t.Fatal(err)
	}
	classes := MemberClasses{Convoy: beadstest.Work(convoyCache), Work: []beads.WorkStore{beadstest.Work(memberCache)}}
	if got := memberStatuses(t, MembersIn, classes, convoy.ID); got != "fe-1=closed" {
		t.Fatalf("cached members = %s, want the closed item the cache holds", got)
	}

	added, err := convoyBacking.Create(beads.Bead{Title: "added item", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoyBacking.DepAdd(convoy.ID, added.ID, TrackingDepType); err != nil {
		t.Fatal(err)
	}
	if err := memberBacking.Reopen("fe-1"); err != nil {
		t.Fatal(err)
	}

	if got := memberStatuses(t, MembersIn, classes, convoy.ID); got != "fe-1=closed" {
		t.Fatalf("cached members = %s; the cache must still miss both writes, or this test proves nothing", got)
	}
	if got, want := memberStatuses(t, MembersInLive, classes, convoy.ID), "fe-1=open "+added.ID+"=open"; got != want {
		t.Fatalf("live members = %s, want %s", got, want)
	}
}

// memberStatuses reads convoyID's members, closed included, with read and
// renders them as "id=status" pairs in id order.
func memberStatuses(t *testing.T, read func(MemberClasses, string, bool) ([]beads.Bead, error), classes MemberClasses, convoyID string) string {
	t.Helper()
	members, err := read(classes, convoyID, true)
	if err != nil {
		t.Fatalf("reading members of %s: %v", convoyID, err)
	}
	pairs := make([]string, 0, len(members))
	for _, m := range members {
		pairs = append(pairs, m.ID+"="+m.Status)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, " ")
}

func TestConvoyAddItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	epic, _ := store.Create(beads.Bead{Title: "epic", Type: "epic"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: epic.ID})

	err := ConvoyAddItems(deps, MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID, []string{b1.ID})
	if err != nil {
		t.Fatal(err)
	}

	child, _ := store.Get(b1.ID)
	if child.ParentID != epic.ID {
		t.Errorf("parent = %q, want preserved epic parent %q", child.ParentID, epic.ID)
	}
	requireTracksDep(t, store, convoy.ID, b1.ID)
}

func TestUntrackItemFailsOnAmbiguousMixedDependencyTypes(t *testing.T) {
	store := beads.NewMemStore()
	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	item, _ := store.Create(beads.Bead{Title: "task"})
	if err := store.DepAdd(convoy.ID, item.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd(parent-child): %v", err)
	}
	if err := store.DepAdd(convoy.ID, item.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}

	if err := UntrackItem(beadstest.Work(store), convoy.ID, item.ID); err == nil {
		t.Fatal("UntrackItem succeeded for mixed dependency types, want error")
	}
	requireTracksDep(t, store, convoy.ID, item.ID)
}

func TestConvoyCloseOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})

	err := ConvoyClose(deps, beadstest.Work(store), convoy.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(convoy.ID)
	if got.Status != "closed" {
		t.Errorf("status = %q, want closed", got.Status)
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	found := false
	for _, e := range fake.Events {
		if e.Type == events.ConvoyClosed {
			found = true
		}
	}
	if !found {
		t.Error("expected ConvoyClosed event to be emitted")
	}
}

func TestConvoyCloseNotFoundOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps()

	err := ConvoyClose(deps, beadstest.Work(store), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent convoy")
	}
}

func requireTracksDep(t *testing.T, store beads.Store, convoyID, itemID string) {
	t.Helper()
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", convoyID, err)
	}
	for _, dep := range deps {
		if dep.IssueID == convoyID && dep.DependsOnID == itemID && dep.Type == "tracks" {
			return
		}
	}
	t.Fatalf("missing tracks dep %s -> %s; deps=%v", convoyID, itemID, deps)
}

type metadataParseErrorStore struct {
	beads.Store
	corruptID string
}

func (s metadataParseErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.corruptID {
		return beads.Bead{}, fmt.Errorf("parsing metadata for bead %q: %w", id, beads.ErrMetadataParse)
	}
	return s.Store.Get(id)
}
