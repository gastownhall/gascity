package convoy

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// unreadableStore stands in for a named class whose backend is unavailable:
// every Get fails with an I/O-shaped error, which is emphatically NOT
// beads.ErrNotFound. It is the only way to tell the two directions of the
// partial-result rule apart — a store that is silent because nobody asked it,
// and a store that is silent because it broke.
type unreadableStore struct {
	beads.Store
	err   error
	reads int
}

func (s *unreadableStore) Get(string) (beads.Bead, error) {
	s.reads++
	return beads.Bead{}, s.err
}

// prefixStore reports an id prefix, standing in for a class store that mints
// its own id namespace (the recorded-ownership fast path).
type prefixStore struct {
	beads.Store
	prefix string
}

func (p *prefixStore) IDPrefix() string { return p.prefix }

// seedCrossClassConvoy builds the mixed-class shape this whole slice is about:
// a convoy owned by one class store that tracks a member bead physically owned
// by another. The tracks edge lives with the convoy; the member does not.
func seedCrossClassConvoy(t *testing.T) (convoyStore *beads.MemStore, memberStore *beads.MemStore, convoyID, memberID string) {
	t.Helper()
	// Ids are prefix-disjoint across class stores in the real topology, so the
	// member is seeded under a rig work prefix rather than the MemStore default.
	convoyStore = beads.NewMemStore()
	memberStore = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "fe-1", Title: "work item", Type: "task", Status: "open"}}, nil)

	convoy, err := convoyStore.Create(beads.Bead{Title: "drain unit", Type: "convoy"})
	if err != nil {
		t.Fatalf("seed convoy: %v", err)
	}
	if err := convoyStore.DepAdd(convoy.ID, "fe-1", TrackingDepType); err != nil {
		t.Fatalf("seed tracks edge: %v", err)
	}
	return convoyStore, memberStore, convoy.ID, "fe-1"
}

// TestMembersInUnnamedClassContributesEmpty pins direction one of the
// partial-result rule: a class the caller did not name is never read, so the
// member it owns comes back as an unresolved placeholder and the lookup
// SUCCEEDS. The observable effect is read back through the placeholder
// predicate, not through the fixture that seeded the member.
func TestMembersInUnnamedClassContributesEmpty(t *testing.T) {
	convoyStore, memberStore, convoyID, memberID := seedCrossClassConvoy(t)

	members, err := MembersIn(MemberClasses{Convoy: convoyStore}, convoyID, true)
	if err != nil {
		t.Fatalf("MembersIn over the convoy class alone: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d (%+v), want the single tracked edge", len(members), members)
	}
	if members[0].ID != memberID {
		t.Fatalf("member id = %q, want %q", members[0].ID, memberID)
	}
	if !IsUnresolvedTrackedItem(members[0]) {
		t.Fatalf("member = %+v, want an unresolved placeholder: the class owning it was never named", members[0])
	}

	// The member really is materializable — it is only invisible because this
	// lookup did not span its class. Read it back through the owning store,
	// which MembersIn never touched.
	owned, err := memberStore.Get(memberID)
	if err != nil {
		t.Fatalf("member store Get: %v", err)
	}
	if owned.Title != "work item" {
		t.Fatalf("seeded member title = %q, want %q", owned.Title, "work item")
	}
}

// TestMembersInNamedClassMaterializesMember is the control for the test above:
// name the member's class and the same convoy yields the real bead. Without
// this case the placeholder assertion could not tell "not named" from "not
// resolvable at all".
func TestMembersInNamedClassMaterializesMember(t *testing.T) {
	convoyStore, memberStore, convoyID, memberID := seedCrossClassConvoy(t)

	members, err := MembersIn(MemberClasses{
		Convoy: convoyStore,
		Work:   []beads.Store{memberStore},
	}, convoyID, true)
	if err != nil {
		t.Fatalf("MembersIn spanning the work class: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d (%+v), want 1", len(members), members)
	}
	if IsUnresolvedTrackedItem(members[0]) {
		t.Fatalf("member %+v is a placeholder, want the materialized bead", members[0])
	}
	if members[0].ID != memberID || members[0].Title != "work item" {
		t.Fatalf("member = %+v, want the seeded work item %q", members[0], memberID)
	}
}

// TestMembersInNamedClassFailureStaysAnError pins direction two: a class the
// caller DID name is a participant, so its read failure is returned with the
// class as provenance instead of being flattened into a placeholder. Compare
// with TestMembersInUnnamedClassContributesEmpty: identical fixture, identical
// convoy, opposite outcome — which is the whole point of naming.
func TestMembersInNamedClassFailureStaysAnError(t *testing.T) {
	convoyStore, _, convoyID, memberID := seedCrossClassConvoy(t)
	broken := &unreadableStore{err: fmt.Errorf("dial work store: connection refused")}

	members, err := MembersIn(MemberClasses{
		Convoy: convoyStore,
		Work:   []beads.Store{broken},
	}, convoyID, true)
	if err == nil {
		t.Fatalf("MembersIn = %+v, nil; want the participating class's read failure", members)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("MembersIn err = %v, want a hard failure, not a not-found", err)
	}
	if broken.reads == 0 {
		t.Fatal("the named work class was never read; naming a class must make it a participant")
	}
	for _, want := range []string{memberID, "work", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("MembersIn err = %q, want it to name %q", err, want)
		}
	}
}

// TestTrackItemInRefusesCrossClassEdgeBeforeWriting pins the write side: a
// membership edge cannot span stores, so naming a member class that really owns
// the item fails with ErrMemberNotCoResident and writes NOTHING. The absence of
// the write is read back through the convoy store's dependency list, which
// TrackItemIn never writes through on this path.
func TestTrackItemInRefusesCrossClassEdgeBeforeWriting(t *testing.T) {
	convoyStore := beads.NewMemStore()
	memberStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "fe-1", Title: "work item", Type: "task", Status: "open"}}, nil)
	convoy, err := convoyStore.Create(beads.Bead{Title: "graph convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("seed convoy: %v", err)
	}

	err = TrackItemIn(MemberClasses{
		Convoy: convoyStore,
		Work:   []beads.Store{memberStore},
	}, convoy.ID, "fe-1")
	if !errors.Is(err, ErrMemberNotCoResident) {
		t.Fatalf("TrackItemIn err = %v, want ErrMemberNotCoResident", err)
	}

	deps, err := convoyStore.DepList(convoy.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("convoy deps = %+v, want none: a refused cross-class track must write nothing", deps)
	}
}

// TestTrackItemInWritesSameClassEdge is the control: when the named classes
// agree that the convoy's own store owns the member, the edge is written.
func TestTrackItemInWritesSameClassEdge(t *testing.T) {
	store := beads.NewMemStore()
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	member, _ := store.Create(beads.Bead{Title: "item"})

	if err := TrackItemIn(MemberClasses{Convoy: store, Work: []beads.Store{store}}, convoy.ID, member.ID); err != nil {
		t.Fatalf("TrackItemIn: %v", err)
	}
	requireTracksDep(t, store, convoy.ID, member.ID)
}

// TestResolveMemberDuplicateResidenceIsTypedError pins that two residences are
// a typed error rather than a first-match winner. Naming a class is a claim
// about ownership; two claims mean the caller cannot know the owner, and no
// membership decision may be made on a coin flip.
func TestResolveMemberDuplicateResidenceIsTypedError(t *testing.T) {
	first := beads.NewMemStore()
	bead, err := first.Create(beads.Bead{Title: "item"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The same id resident in a second named class — a migration copy left
	// behind, which is exactly the condition a first-match probe would hide.
	second := beads.NewMemStoreFrom(1, []beads.Bead{{ID: bead.ID, Title: "stale copy", Type: "task", Status: "open"}}, nil)
	if _, err := second.Get(bead.ID); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}

	classes := MemberClasses{Convoy: first, Graph: second}
	_, _, err = classes.resolveMember(bead.ID)
	if !errors.Is(err, ErrDuplicateResidence) {
		t.Fatalf("resolveMember err = %v, want ErrDuplicateResidence", err)
	}
	var dup *DuplicateResidenceError
	if !errors.As(err, &dup) {
		t.Fatalf("resolveMember err = %v, want a *DuplicateResidenceError", err)
	}
	if dup.ID != bead.ID {
		t.Fatalf("duplicate id = %q, want %q", dup.ID, bead.ID)
	}
	if len(dup.Classes) != 2 {
		t.Fatalf("duplicate classes = %v, want both claimants", dup.Classes)
	}

	// And the same duplicate refuses the mutation, before it is attempted.
	convoy, _ := first.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err := TrackItemIn(classes, convoy.ID, bead.ID); !errors.Is(err, ErrDuplicateResidence) {
		t.Fatalf("TrackItemIn err = %v, want ErrDuplicateResidence", err)
	}
	deps, _ := first.DepList(convoy.ID, "down")
	if len(deps) != 0 {
		t.Fatalf("convoy deps = %+v, want none", deps)
	}
}

// TestCandidatesNamesEachHandleOnce pins that naming the same physical handle
// for two classes is one candidate, not a duplicate residence. Today every
// class binds to the same store, so this is the case that must stay boring.
func TestCandidatesNamesEachHandleOnce(t *testing.T) {
	store := beads.NewMemStore()
	bead, _ := store.Create(beads.Bead{Title: "item"})

	classes := MemberClasses{Convoy: store, Work: []beads.Store{store}, Graph: store}
	if got := len(classes.candidates()); got != 1 {
		t.Fatalf("candidates = %d, want 1: the same handle named three times is one store", got)
	}
	if _, _, err := classes.resolveMember(bead.ID); err != nil {
		t.Fatalf("resolveMember over one shared handle: %v", err)
	}
}

// TestCandidatesSkipsUnnamedAndTypedNilClasses pins that an unnamed class is
// dropped before it can be probed — including a typed nil handle, which is a
// NON-nil interface value and would panic on first use.
func TestCandidatesSkipsUnnamedAndTypedNilClasses(t *testing.T) {
	store := beads.NewMemStore()
	var typedNil *beads.MemStore

	classes := MemberClasses{Convoy: store, Work: []beads.Store{typedNil, nil}, Graph: typedNil}
	cands := classes.candidates()
	if len(cands) != 1 {
		t.Fatalf("candidates = %d (%+v), want only the named convoy handle", len(cands), cands)
	}
	if _, _, err := classes.resolveMember("gc-absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("resolveMember err = %v, want ErrNotFound", err)
	}
}

// TestResolveMemberPrefixOwnerIsRecordedOwnership pins the recorded-ownership
// fast path: a class store that mints the id's prefix owns it outright, so the
// other named classes are not probed at all.
func TestResolveMemberPrefixOwnerIsRecordedOwnership(t *testing.T) {
	const graphID = "gcg-1"
	graphBacking := beads.NewMemStoreFrom(1, []beads.Bead{{ID: graphID, Title: "graph node", Type: "task", Status: "open"}}, nil)
	graph := &prefixStore{Store: graphBacking, prefix: "gcg"}
	work := &unreadableStore{err: fmt.Errorf("dial work store: connection refused")}

	classes := MemberClasses{Convoy: graph, Work: []beads.Store{work}}
	got, owner, err := classes.resolveMember(graphID)
	if err != nil {
		t.Fatalf("resolveMember: %v", err)
	}
	if got.ID != graphID {
		t.Fatalf("resolved %q, want %q", got.ID, graphID)
	}
	if owner.class != "convoy" {
		t.Fatalf("owner class = %q, want the prefix owner", owner.class)
	}
	if work.reads != 0 {
		t.Fatalf("work class read %d times, want 0: recorded ownership needs no probe", work.reads)
	}
}

// TestConvoyClassHandleIsRequired pins that an operation with no handle for the
// convoy's own class fails with a diagnosis instead of dereferencing a nil (or
// worse, a typed nil, which is a non-nil interface value).
func TestConvoyClassHandleIsRequired(t *testing.T) {
	var typedNil *beads.MemStore
	for name, classes := range map[string]MemberClasses{
		"unnamed":   {Work: []beads.Store{beads.NewMemStore()}},
		"typed nil": {Convoy: typedNil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MembersIn(classes, "gc-1", true); !errors.Is(err, ErrNoConvoyClass) {
				t.Fatalf("MembersIn err = %v, want ErrNoConvoyClass", err)
			}
			if err := TrackItemIn(classes, "gc-1", "gc-2"); !errors.Is(err, ErrNoConvoyClass) {
				t.Fatalf("TrackItemIn err = %v, want ErrNoConvoyClass", err)
			}
		})
	}
}

// TestMembersInParticipantFailureOutranksAFoundOwnerInEitherOrder pins the
// rule that makes the partial-result semantics safe under multi-scope Work: a
// named class that cannot be read is an error EVEN WHEN another named class
// already produced the bead, and the answer does not depend on probe order.
//
// The reason is uniqueness. Two residences are a typed error precisely because
// nobody may pick a winner between them, and an unreadable participant means
// the candidate set cannot be proven to hold exactly one owner. Returning the
// bead the readable class happened to hold would be first-match ownership with
// extra steps — and it would flip behavior depending on which rig scope
// happened to be listed first, which is the least debuggable failure mode this
// design has.
func TestMembersInParticipantFailureOutranksAFoundOwnerInEitherOrder(t *testing.T) {
	orders := []struct {
		name  string
		build func(owner, broken beads.Store) []beads.Store
	}{
		{
			name:  "broken scope probed first",
			build: func(owner, broken beads.Store) []beads.Store { return []beads.Store{broken, owner} },
		},
		{
			name:  "owning scope probed first",
			build: func(owner, broken beads.Store) []beads.Store { return []beads.Store{owner, broken} },
		},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			convoyStore, memberStore, convoyID, memberID := seedCrossClassConvoy(t)
			broken := &unreadableStore{err: fmt.Errorf("dial rig work store: connection refused")}

			members, err := MembersIn(MemberClasses{
				Convoy: convoyStore,
				Work:   order.build(memberStore, broken),
			}, convoyID, true)
			if err == nil {
				t.Fatalf("MembersIn = %+v, nil; a readable class answered while a named class was unreadable", members)
			}
			if errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("MembersIn err = %v, want a hard failure, not a not-found", err)
			}
			if broken.reads == 0 {
				t.Fatal("the broken work scope was never read; naming a class must make it a participant")
			}
			if !strings.Contains(err.Error(), "connection refused") {
				t.Fatalf("MembersIn err = %q, want it to carry the unreadable class's own failure", err)
			}
			// The member really is resolvable through the readable scope, so
			// the failure above is the rule and not a broken fixture.
			resolvable, getErr := memberStore.Get(memberID)
			if getErr != nil || resolvable.ID != memberID {
				t.Fatalf("the fixture member is not readable through the owning scope (%v); this test would prove nothing", getErr)
			}
		})
	}
}
