package targetscope

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestStampMemberScopeCommitsOnAbsent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	want := Scope{V: 1, Branch: "release", Worktree: "/srv/wt"}

	outcome, err := StampMemberScope(store, member.ID, want)
	if err != nil {
		t.Fatalf("StampMemberScope: %v", err)
	}
	if outcome != OutcomeCommitted {
		t.Fatalf("outcome = %v, want committed", outcome)
	}
	got := declaredScope(t, store, member.ID)
	if !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("stored scope = %v/%+v, want valid %+v", got.State, got.Scope, want)
	}
}

func TestStampMemberScopeIsIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	want := Scope{V: 1, Branch: "release"}
	if _, err := StampMemberScope(store, member.ID, want); err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	outcome, err := StampMemberScope(store, member.ID, want)
	if err != nil {
		t.Fatalf("second stamp: %v", err)
	}
	if outcome != OutcomeNoop {
		t.Fatalf("outcome = %v, want noop on re-stamp of an equal scope", outcome)
	}
}

// The immutability guard is the enforcement the CAS would otherwise carry.
// Dropping the CAS for a lock-serialized stamp must NOT drop it: a
// present-valid-different member rejects, and its existing scope is untouched.
// This is the silent-retarget hazard that removing an enforcement mechanism
// without replacing the invariant would ship.
func TestStampMemberScopeRejectsRetarget(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	if _, err := StampMemberScope(store, member.ID, Scope{V: 1, Branch: "release"}); err != nil {
		t.Fatalf("first stamp: %v", err)
	}

	outcome, err := StampMemberScope(store, member.ID, Scope{V: 1, Branch: "main"})
	if !errors.Is(err, ErrDeclarationConflict) {
		t.Fatalf("err = %v, want ErrDeclarationConflict on retarget", err)
	}
	if outcome != OutcomeNoop {
		t.Fatalf("outcome = %v, want noop (no write) on retarget", outcome)
	}
	got := declaredScope(t, store, member.ID)
	if got.Scope.Branch != "release" {
		t.Fatalf("scope.branch = %q, want the original release preserved — a retarget must not overwrite", got.Scope.Branch)
	}
}

func TestStampMemberScopeRejectsCorruptExistingScope(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":99,"branch":"main"}`,
	})

	_, err := StampMemberScope(store, member.ID, Scope{V: 1, Branch: "release"})
	if err == nil {
		t.Fatal("want an error stamping over a corrupt/unusable existing scope, got nil")
	}
	if store2 := declaredScope(t, store, member.ID); store2.Raw != `{"v":99,"branch":"main"}` {
		t.Fatalf("corrupt scope was modified to %q, want it left intact", store2.Raw)
	}
}
