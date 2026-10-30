package beads_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestNativeDoltStoreGetExactBatchAnswersEachIDAsGet pins the native exact
// batch read against a real embedded workspace. A CachingStore installs what
// its backing's batch answers as that bead's Get row (readBackingRows), edges
// and all, so every bead the batch finds must be the bead Get returns for its
// id: labels, metadata, a blocks edge, an external: edge, a parent, a closed
// bead and a wisp. An id the store does not hold is unresolved, and the read
// sends no listing request that walks the ledger.
func TestNativeDoltStoreGetExactBatchAnswersEachIDAsGet(t *testing.T) {
	store := splittest.NewNativeWorkStore(t, "gc")
	getter, ok := any(store).(beads.ExactBatchGetter)
	if !ok {
		t.Fatal("*NativeDoltStore has no exact batch read: GetMany and the caching store read its ids one Get at a time or through a whole-ledger listing")
	}
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %q: %v", b.Title, err)
		}
		return created
	}
	parent := create(beads.Bead{Title: "parent", Type: "epic"})
	labeled := create(beads.Bead{
		Title:       "labeled",
		Type:        "task",
		Description: "carries labels and metadata",
		Labels:      []string{"alpha", "beta"},
		Metadata:    map[string]string{"gc.k": "v", "gc.other": "w"},
		Priority:    intPtr(1),
	})
	blocker := create(beads.Bead{Title: "blocker", Type: "task"})
	dependent := create(beads.Bead{Title: "dependent", Type: "task"})
	if err := store.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(dependent.ID, "external:zz-9", "blocks"); err != nil {
		t.Fatal(err)
	}
	child := create(beads.Bead{Title: "child", Type: "task", ParentID: parent.ID})
	closed := create(beads.Bead{Title: "closed", Type: "task"})
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	ids := []string{parent.ID, labeled.ID, blocker.ID, dependent.ID, child.ID, closed.ID}
	if wisp, err := store.Create(beads.Bead{Title: "wisp", Type: "task", Ephemeral: true}); err == nil {
		ids = append(ids, wisp.ID)
	} else {
		t.Logf("the embedded workspace minted no wisp (%v); the ephemeral plane is not covered here", err)
	}

	before, _ := beads.ListRequestStatsOf(store)
	found, unresolved, err := getter.GetExactBatch(append(slices.Clone(ids), "gc-nope"))
	if err != nil {
		t.Fatalf("GetExactBatch: %v", err)
	}
	after, _ := beads.ListRequestStatsOf(store)
	if !slices.Equal(unresolved, []string{"gc-nope"}) {
		t.Errorf("unresolved = %v, want [gc-nope]", unresolved)
	}
	if len(found) != len(ids) {
		t.Errorf("found %d of %d beads the store holds", len(found), len(ids))
	}
	for _, id := range ids {
		want, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got := found[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("GetExactBatch answered %s as\n  %+v\nwant Get's\n  %+v", id, got, want)
		}
	}
	if found[dependent.ID].Dependencies == nil || found[child.ID].ParentID != parent.ID {
		t.Errorf("fixture: the batch carried no edges (dependent deps %v, child parent %q); the comparison proves nothing about them",
			found[dependent.ID].Dependencies, found[child.ID].ParentID)
	}
	if unkeyed := after.Unkeyed - before.Unkeyed; unkeyed != 0 {
		t.Errorf("GetExactBatch sent %d unkeyed listing request(s), want 0", unkeyed)
	}
	if after.Requests == before.Requests {
		t.Error("GetExactBatch's listing requests are not counted; the keyedness budget cannot see them")
	}
}

func intPtr(n int) *int { return &n }
