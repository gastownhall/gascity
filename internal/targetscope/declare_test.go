package targetscope

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func newMember(t *testing.T, store *beads.MemStore, id string, metadata map[string]string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{ID: id, Title: "member " + id, Metadata: metadata})
	if err != nil {
		t.Fatalf("creating member %s: %v", id, err)
	}
	return bead
}

func declaredScope(t *testing.T, store *beads.MemStore, id string) Resolution {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading member %s: %v", id, err)
	}
	return Parse(bead.Metadata[beadmeta.TargetScopeMetadataKey])
}

func TestDeclareCommitsOnAbsent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	want := Scope{V: 1, Branch: "release", Worktree: "/srv/wt"}

	outcome, err := Declare(store, member.ID, want)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if outcome != OutcomeCommitted {
		t.Fatalf("outcome = %v, want committed", outcome)
	}
	got := declaredScope(t, store, member.ID)
	if !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("member carries %+v (%v), want %+v", got.Scope, got.State, want)
	}
}

// Re-declaring the same scope must be a no-op. This is what makes a retry after
// a partially-declared launch safe rather than a conflict.
func TestDeclareIsIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	want := Scope{V: 1, Branch: "main"}

	if _, err := Declare(store, member.ID, want); err != nil {
		t.Fatalf("first Declare: %v", err)
	}
	outcome, err := Declare(store, member.ID, want)
	if err != nil {
		t.Fatalf("second Declare: %v", err)
	}
	if outcome != OutcomeNoop {
		t.Fatalf("outcome = %v, want noop", outcome)
	}
}

// Member scope is immutable: a deliberate retarget is rejected, not applied.
func TestDeclareRejectsRetarget(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	first := Scope{V: 1, Branch: "main"}
	if _, err := Declare(store, member.ID, first); err != nil {
		t.Fatalf("first Declare: %v", err)
	}

	_, err := Declare(store, member.ID, Scope{V: 1, Branch: "release"})
	if !errors.Is(err, ErrDeclarationConflict) {
		t.Fatalf("err = %v, want ErrDeclarationConflict", err)
	}
	// The original declaration must survive the rejected attempt.
	got := declaredScope(t, store, member.ID)
	if !got.Scope.Equal(first) {
		t.Fatalf("member now carries %+v, want the original %+v", got.Scope, first)
	}
}

// A corrupt object is never overwritten: we cannot tell what intent we would
// be destroying, so the launch fails instead.
func TestDeclareRejectsCorruptExistingScope(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":99,"branch":"main"}`,
	})

	_, err := Declare(store, member.ID, Scope{V: 1, Branch: "main"})
	if err == nil {
		t.Fatal("Declare accepted a member carrying an unusable scope")
	}
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("err = %v, want it to wrap ErrInvalidScope", err)
	}
	got, _ := store.Get(member.ID)
	if got.Metadata[beadmeta.TargetScopeMetadataKey] != `{"v":99,"branch":"main"}` {
		t.Fatal("the corrupt value was overwritten; it must be preserved for administrative recovery")
	}
}

// Poisoned flat keys are NOT a scope. A member carrying only claim-derived
// gc.work_* must read as absent so the declaration proceeds from trusted
// sources — never laundering the poison into the new object.
func TestDeclareTreatsPoisonedFlatKeysAsAbsent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", map[string]string{
		beadmeta.WorkBranchMetadataKey: "parked-branch",
		beadmeta.WorkDirMetadataKey:    "/shared/poison",
		beadmeta.WorkCommitMetadataKey: "deadbeef",
	})
	want := Scope{V: 1, Branch: "release", Worktree: "/srv/clean"}

	outcome, err := Declare(store, member.ID, want)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if outcome != OutcomeCommitted {
		t.Fatalf("outcome = %v, want committed", outcome)
	}
	got := declaredScope(t, store, member.ID)
	if got.Scope.Branch != "release" || got.Scope.Worktree != "/srv/clean" {
		t.Fatalf("declared scope %+v must come from the trusted resolution, not the flat keys", got.Scope)
	}
}

// A field-empty {v:1} is a legitimate declaration and must persist as VALID.
// Declaring nothing at all would leave the member object-absent, which is what
// re-enables the cwd stamp on the next claim.
func TestDeclareUnknownPersistsAsValidNotAbsent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)

	if _, err := Declare(store, member.ID, Unknown()); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	got := declaredScope(t, store, member.ID)
	if got.State != StateValid {
		t.Fatalf("state = %v, want valid", got.State)
	}
	if !got.Scope.IsUnknown() {
		t.Fatalf("scope = %+v, want field-empty", got.Scope)
	}
}

// rendezvous returns a barrier that blocks until n callers have reached it.
//
// Both declarers therefore complete their READ before either performs its
// write, which is the only interleaving under which an unsafe unconditional
// write produces two winners. Without this, the scheduler serializes the two
// goroutines and the test silently degrades into "the second declarer's read
// saw the first one's value" — which passes even when the write is unsafe.
func rendezvous(n int) func() {
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(gate)
		}
		mu.Unlock()
		<-gate
	}
}

// THE RACE. Two concurrent launches resolving DIFFERENT valid scopes for one
// member must never both proceed: exactly one commits, the other is rejected
// before any root is materialized.
//
// GUARD: this test is verified to FAIL if the compare-and-set is replaced by an
// unconditional SetMetadata. See TestDeclareRaceBarrierActuallyInterleaves for
// the assertion that keeps the barrier honest.
func TestDeclareRaceHasExactlyOneWinner(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		store := beads.NewMemStore()
		member := newMember(t, store, "", nil)
		scopeX := Scope{V: 1, Branch: "branch-x"}
		scopeY := Scope{V: 1, Branch: "branch-y"}

		raceBarrier = rendezvous(2)

		var wg sync.WaitGroup
		results := make([]error, 2)
		outcomes := make([]Outcome, 2)
		start := make(chan struct{})
		for i, scope := range []Scope{scopeX, scopeY} {
			wg.Add(1)
			go func(i int, scope Scope) {
				defer wg.Done()
				<-start
				outcomes[i], results[i] = Declare(store, member.ID, scope)
			}(i, scope)
		}
		close(start)
		wg.Wait()
		raceBarrier = nil

		winners := 0
		for i := range results {
			if results[i] == nil && outcomes[i] == OutcomeCommitted {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("attempt %d: %d winners, want exactly 1 (errs: %v, %v)", attempt, winners, results[0], results[1])
		}
		// The loser must have been rejected, and the member must carry exactly
		// one of the two scopes — never a blend, never the loser's value.
		final := declaredScope(t, store, member.ID)
		if !final.Valid() {
			t.Fatalf("attempt %d: member ended %v, want a single valid scope", attempt, final.State)
		}
		if !final.Scope.Equal(scopeX) && !final.Scope.Equal(scopeY) {
			t.Fatalf("attempt %d: member carries %+v, want exactly one of the two declared scopes", attempt, final.Scope)
		}
		// And the committed value must be the winner's.
		for i, scope := range []Scope{scopeX, scopeY} {
			if results[i] == nil && outcomes[i] == OutcomeCommitted && !final.Scope.Equal(scope) {
				t.Fatalf("attempt %d: winner declared %+v but member carries %+v", attempt, scope, final.Scope)
			}
		}
	}
}

// Two concurrent launches resolving the SAME scope both succeed — convergence,
// not a spurious rejection.
func TestDeclareRaceWithEqualScopesConverges(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		store := beads.NewMemStore()
		member := newMember(t, store, "", nil)
		scope := Scope{V: 1, Branch: "same", Worktree: "/srv/wt"}

		raceBarrier = rendezvous(4)

		var wg sync.WaitGroup
		results := make([]error, 4)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, results[i] = Declare(store, member.ID, scope)
			}(i)
		}
		wg.Wait()
		raceBarrier = nil

		for i, err := range results {
			if err != nil {
				t.Fatalf("attempt %d: declarer %d rejected an equal scope: %v", attempt, i, err)
			}
		}
	}
}

// Keeps the race test honest.
//
// The barrier is only reached by a declarer whose FIRST read found the scope
// absent — decideAgainst returns early otherwise. So observing the barrier
// entered once per declarer proves both reached the write window believing
// they were the first, which is precisely the interleaving under which an
// unconditional write would produce two winners. If this count ever drops to 1,
// the race test above has silently stopped testing anything.
func TestDeclareRaceBarrierActuallyInterleaves(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)

	var mu sync.Mutex
	entered := 0
	inner := rendezvous(2)
	raceBarrier = func() {
		mu.Lock()
		entered++
		mu.Unlock()
		inner()
	}
	defer func() { raceBarrier = nil }()

	var wg sync.WaitGroup
	for _, scope := range []Scope{{V: 1, Branch: "x"}, {V: 1, Branch: "y"}} {
		wg.Add(1)
		go func(scope Scope) {
			defer wg.Done()
			_, _ = Declare(store, member.ID, scope)
		}(scope)
	}
	wg.Wait()

	if entered != 2 {
		t.Fatalf("write window entered %d times, want 2: both declarers must reach the "+
			"compare-and-set having read absent, or the race test proves nothing", entered)
	}
}

// A store with no compare-and-set capability must be REJECTED, never served by
// an unserialized write. Fail-closed is the whole guarantee.
func TestDeclareRejectsStoreWithoutCAS(t *testing.T) {
	_, err := Declare(noCASStore{}, "member-1", Scope{V: 1, Branch: "main"})
	if !errors.Is(err, ErrDeclarationUnsupported) {
		t.Fatalf("err = %v, want ErrDeclarationUnsupported", err)
	}
}

// DeclareAll stops at the first rejection. Members already declared stay
// pinned; the caller must abandon the launch before materializing a root.
func TestDeclareAllStopsAtFirstRejection(t *testing.T) {
	store := beads.NewMemStore()
	good := newMember(t, store, "", nil)
	conflicted := newMember(t, store, "", nil)
	if _, err := Declare(store, conflicted.ID, Scope{V: 1, Branch: "already"}); err != nil {
		t.Fatalf("seeding conflict: %v", err)
	}

	err := DeclareAll([]Member{
		{ID: good.ID, Store: store, Scope: Scope{V: 1, Branch: "wanted"}},
		{ID: conflicted.ID, Store: store, Scope: Scope{V: 1, Branch: "wanted"}},
	})
	if !errors.Is(err, ErrDeclarationConflict) {
		t.Fatalf("err = %v, want ErrDeclarationConflict", err)
	}
	// The already-committed pin is deliberately NOT rolled back.
	if got := declaredScope(t, store, good.ID); !got.Valid() || got.Scope.Branch != "wanted" {
		t.Fatalf("first member lost its pin (%+v); declarations are not rolled back", got.Scope)
	}
}

// noCASStore implements just enough of beads.Store to be handed to Declare
// while offering no metadata compare-and-set.
type noCASStore struct{ beads.Store }

func (noCASStore) Get(id string) (beads.Bead, error) {
	return beads.Bead{ID: id}, nil
}

var _ DeclareStore = noCASStore{}

func TestDescribeNamesUnknownFields(t *testing.T) {
	got := describe(Scope{V: 1, Branch: "main"})
	want := fmt.Sprintf("{branch:%s worktree:%s}", "main", "<unknown>")
	if got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
}
