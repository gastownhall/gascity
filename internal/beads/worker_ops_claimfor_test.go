package beads

import (
	"errors"
	"testing"
)

// The doubles embed Store so they satisfy it without restating a large
// interface; only the claim shape under test is implemented, and nothing in
// ClaimFor touches the rest.
type assigneeOnlyStore struct {
	Store
	gotID, gotAssignee string
}

func (s *assigneeOnlyStore) Claim(id, assignee string) (Bead, bool, error) {
	s.gotID, s.gotAssignee = id, assignee
	return Bead{ID: id, Assignee: assignee}, true, nil
}

type actorOnlyStore struct {
	Store
	gotID, gotAssignee string
}

func (s *actorOnlyStore) ClaimAs(id, assignee string) (Bead, bool, error) {
	s.gotID, s.gotAssignee = id, assignee
	return Bead{ID: id, Assignee: assignee}, true, nil
}

type neitherShapeStore struct{ Store }

// TestClaimForDispatchesBothClaimShapes is the point of routing the remote
// worker claim through ClaimFor instead of asserting one shape on the store.
//
// Two backends expose the SAME compare-and-swap under different method names:
// the in-process stores as Claim(id, assignee), the bd-backed store as
// ClaimAs(id, assignee). A handler that type-asserts only the first answers a
// typed 501 for the second while the capability is sitting right there. This
// test fails against that direct assert and passes through the dispatch.
func TestClaimForDispatchesBothClaimShapes(t *testing.T) {
	t.Run("assignee shape", func(t *testing.T) {
		s := &assigneeOnlyStore{}
		got, acquired, err := ClaimFor(s, "cr-abc", "seat-1")
		if err != nil || !acquired {
			t.Fatalf("ClaimFor = (%v, %v, %v), want acquired with no error", got, acquired, err)
		}
		if s.gotID != "cr-abc" || s.gotAssignee != "seat-1" {
			t.Errorf("store saw (%q, %q), want (cr-abc, seat-1)", s.gotID, s.gotAssignee)
		}
	})

	t.Run("actor shape — the one a direct assert on Claim would refuse", func(t *testing.T) {
		s := &actorOnlyStore{}
		got, acquired, err := ClaimFor(s, "cr-def", "seat-2")
		if err != nil || !acquired {
			t.Fatalf("ClaimFor = (%v, %v, %v), want acquired with no error", got, acquired, err)
		}
		if s.gotID != "cr-def" || s.gotAssignee != "seat-2" {
			t.Errorf("store saw (%q, %q), want (cr-def, seat-2)", s.gotID, s.gotAssignee)
		}
	})
}

// TestClaimForNamesAnAbsentCapability pins that a backend with neither shape
// gets a named capability error rather than a nil-interface panic — which is
// what lets the handler keep a typed 501 for exactly that case, and only that
// case.
func TestClaimForNamesAnAbsentCapability(t *testing.T) {
	if _, _, err := ClaimFor(&neitherShapeStore{}, "cr-ghi", "seat-3"); !errors.Is(err, ErrClaimUnsupported) {
		t.Errorf("ClaimFor on a store with neither claim shape = %v, want ErrClaimUnsupported", err)
	}
	if _, _, err := ClaimFor(nil, "cr-ghi", "seat-3"); !errors.Is(err, ErrClaimUnsupported) {
		t.Errorf("ClaimFor(nil) = %v, want ErrClaimUnsupported", err)
	}
}
