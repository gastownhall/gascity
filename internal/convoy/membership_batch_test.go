package convoy

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchReadStore gives a MemStore the batched dependent read the bd store
// answers with one bd sql statement, and counts the reads MembersBatch makes. In bd a bead's parent IS its parent-child edge; MemStore
// also keeps it as the ParentID field, so both count as the edge here.
type batchReadStore struct {
	*beads.MemStore
	dependentBatches int
	edgeBatches      int
	singleDepLists   int
	unsupported      bool
}

func (s *batchReadStore) DependentIDsBatch(ids []string, depType string) ([]string, error) {
	s.dependentBatches++
	if s.unsupported {
		return nil, beads.ErrDepListBatchUnsupported
	}
	seen := make(map[string]bool)
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range ids {
		deps, err := s.MemStore.DepList(id, "up")
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d.Type == depType {
				add(d.IssueID)
			}
		}
		if depType == "parent-child" {
			children, err := s.List(beads.ListQuery{ParentID: id, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				add(c.ID)
			}
		}
	}
	return out, nil
}

func (s *batchReadStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.edgeBatches++
	return s.MemStore.DepListBatch(ids)
}

func (s *batchReadStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.singleDepLists++
	return s.MemStore.DepList(id, direction)
}

// membershipFixture builds convoys covering every way a member is found:
// legacy children, tracked members, closed members of each kind, a member that
// is both, a dangling tracks edge, a dotted id with no parent edge, and a
// convoy with no members.
func membershipFixture(t *testing.T, store *beads.MemStore) []string {
	t.Helper()
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		got, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	dep := func(from, to, kind string) {
		t.Helper()
		if err := store.DepAdd(from, to, kind); err != nil {
			t.Fatal(err)
		}
	}
	closeBead := func(id string) {
		t.Helper()
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}

	legacy := create(beads.Bead{Title: "legacy", Type: "convoy"})
	legacyOpen := create(beads.Bead{Title: "legacy open", ParentID: legacy.ID})
	legacyClosed := create(beads.Bead{Title: "legacy closed", ParentID: legacy.ID})
	closeBead(legacyClosed.ID)
	_ = legacyOpen

	tracking := create(beads.Bead{Title: "tracking", Type: "convoy"})
	open := create(beads.Bead{Title: "tracked open"})
	done := create(beads.Bead{Title: "tracked closed"})
	closeBead(done.ID)
	dep(tracking.ID, open.ID, "tracks")
	dep(tracking.ID, done.ID, "tracks")
	dep(tracking.ID, legacyOpen.ID, "blocks")

	mixed := create(beads.Bead{Title: "mixed", Type: "convoy"})
	both := create(beads.Bead{Title: "child and tracked", ParentID: mixed.ID})
	dep(mixed.ID, both.ID, "tracks")
	dep(mixed.ID, open.ID, "tracks")

	dangling := create(beads.Bead{Title: "dangling", Type: "convoy"})
	dep(dangling.ID, "gc-missing", "tracks")
	dep(dangling.ID, open.ID, "tracks")

	dotted := create(beads.Bead{Title: "dotted", Type: "convoy"})
	create(beads.Bead{ID: dotted.ID + ".1", Title: "dotted id, no parent edge"})

	empty := create(beads.Bead{Title: "empty", Type: "convoy"})

	return []string{legacy.ID, tracking.ID, mixed.ID, dangling.ID, dotted.ID, empty.ID}
}

func memberIDs(members []beads.Bead) []string {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.ID+"="+m.Status)
	}
	return ids
}

func TestMembersBatchAnswersEachConvoyAsMembers(t *testing.T) {
	for _, includeClosed := range []bool{true, false} {
		mem := beads.NewMemStore()
		ids := membershipFixture(t, mem)
		store := &batchReadStore{MemStore: mem}

		got, err := MembersBatch(store, ids, includeClosed)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(ids) {
			t.Fatalf("includeClosed=%v: answered %d convoys, want %d", includeClosed, len(got), len(ids))
		}
		for _, id := range ids {
			want, err := Members(mem, id, includeClosed)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(memberIDs(got[id]), memberIDs(want)) {
				t.Errorf("includeClosed=%v convoy %s: batch %v, Members %v", includeClosed, id, memberIDs(got[id]), memberIDs(want))
			}
		}
	}
}

func TestMembersBatchReadsInAFixedNumberOfBatches(t *testing.T) {
	mem := beads.NewMemStore()
	ids := membershipFixture(t, mem)
	store := &batchReadStore{MemStore: mem}

	if _, err := MembersBatch(store, ids, true); err != nil {
		t.Fatal(err)
	}
	if store.dependentBatches != 1 || store.edgeBatches != 1 {
		t.Errorf("batched reads: dependents %d, edges %d; want 1 and 1", store.dependentBatches, store.edgeBatches)
	}
	// Only the convoy with a dangling tracks edge is read alone.
	if store.singleDepLists != 1 {
		t.Errorf("per-convoy dep lists = %d, want 1 (the dangling convoy)", store.singleDepLists)
	}
}

func TestMembersBatchKeepsTheDanglingEdgeVisible(t *testing.T) {
	mem := beads.NewMemStore()
	ids := membershipFixture(t, mem)
	store := &batchReadStore{MemStore: mem}

	got, err := MembersBatch(store, ids, true)
	if err != nil {
		t.Fatal(err)
	}
	var unknown []string
	for _, m := range got[ids[3]] {
		if IsUnresolvedTrackedItem(m) {
			unknown = append(unknown, m.ID)
		}
	}
	if !reflect.DeepEqual(unknown, []string{"gc-missing"}) {
		t.Errorf("unresolved members of the dangling convoy = %v, want [gc-missing]", unknown)
	}
}

func TestMembersBatchFallsBackPerConvoy(t *testing.T) {
	cases := map[string]func(*beads.MemStore, []string) (beads.Store, []string){
		"store without the batched reads": func(mem *beads.MemStore, ids []string) (beads.Store, []string) {
			return mem, ids
		},
		"batched read unsupported": func(mem *beads.MemStore, ids []string) (beads.Store, []string) {
			return &batchReadStore{MemStore: mem, unsupported: true}, ids
		},
		"one convoy": func(mem *beads.MemStore, ids []string) (beads.Store, []string) {
			return &batchReadStore{MemStore: mem}, ids[1:2]
		},
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mem := beads.NewMemStore()
		store, ids := cases[name](mem, membershipFixture(t, mem))
		got, err := MembersBatch(store, ids, true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, id := range ids {
			want, err := Members(mem, id, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(memberIDs(got[id]), memberIDs(want)) {
				t.Errorf("%s convoy %s: batch %v, Members %v", name, id, memberIDs(got[id]), memberIDs(want))
			}
		}
		if b, ok := store.(*batchReadStore); ok && name == "one convoy" && b.dependentBatches != 0 {
			t.Errorf("one convoy: dependent batches = %d, want 0", b.dependentBatches)
		}
	}
}

func TestMembersBatchReturnsAFailedBatchedRead(t *testing.T) {
	mem := beads.NewMemStore()
	ids := membershipFixture(t, mem)
	boom := errors.New("link dropped")
	store := &failingDependentStore{batchReadStore: batchReadStore{MemStore: mem}, err: boom}

	if _, err := MembersBatch(store, ids, true); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the read's own error", err)
	}
}

type failingDependentStore struct {
	batchReadStore
	err error
}

func (s *failingDependentStore) DependentIDsBatch([]string, string) ([]string, error) {
	return nil, s.err
}
