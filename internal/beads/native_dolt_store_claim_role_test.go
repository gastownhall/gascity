package beads

import (
	"context"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

type claimRoleSpy struct {
	beadslib.Storage
	requests []issueops.ClaimRequest
	result   issueops.ClaimResult
	err      error

	// getResult/getErr configure IssueReader().Get, which Claim now consults
	// (S5b-7-fixup item 8) to tell a genuinely missing id apart from a wisp id
	// whenever Claim's own err resolves to issueops.ErrNotFound: nil getErr
	// with a non-nil getResult simulates a row the reader finds (a wisp), and
	// a non-nil getErr (normally issueops.ErrNotFound) simulates a row no
	// plane holds. See newWispDisambiguationSpy below the table it is used by.
	getResult *issueops.IssueDetails
	getErr    error
}

func (s *claimRoleSpy) IssueClaimer() (issueops.Claimer, error) { return s, nil }

func (s *claimRoleSpy) Claim(_ context.Context, req issueops.ClaimRequest) (issueops.ClaimResult, error) {
	s.requests = append(s.requests, req)
	return s.result, s.err
}

// IssueReader lets claimRoleSpy double as the reader role Claim's wisp
// disambiguation consults. Only Get is exercised by any test in this file;
// Ready and List are implemented to satisfy issueops.Reader and fail loudly
// if a future test reaches them unconfigured.
func (s *claimRoleSpy) IssueReader() (issueops.Reader, error) { return s, nil }

func (s *claimRoleSpy) Get(_ context.Context, _ issueops.GetRequest) (*issueops.IssueDetails, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.getResult, nil
}

func (s *claimRoleSpy) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("claimRoleSpy.Ready not configured")
}

func (s *claimRoleSpy) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("claimRoleSpy.List not configured")
}

// TestClaimDialsTheRoleWithTheAssigneeAsActor pins the one detail that makes
// Claim different from ReleaseIfCurrent: there is no separate service actor.
// issueops.ClaimRequest.Actor IS the assignee — "I am actor X, I claim issue Y
// for myself" — so the store's own s.actor ("native-test") must never appear
// on the wire here, unlike Release's Actor/ExpectedAssignee split.
func TestClaimDialsTheRoleWithTheAssigneeAsActor(t *testing.T) {
	issue := &beadslib.Issue{ID: "gc-1", Title: "do the thing", Status: beadslib.StatusInProgress, Assignee: "worker-1"}
	spy := &claimRoleSpy{result: issueops.ClaimResult{Issue: issue, Changed: true}}
	store := newNativeDoltStoreForTest(spy)

	bead, claimed, err := store.Claim("gc-1", "worker-1")
	if err != nil || !claimed {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", bead, claimed, err)
	}
	if bead.ID != "gc-1" || bead.Assignee != "worker-1" {
		t.Errorf("bead = %+v, want the claimed row back", bead)
	}
	if len(spy.requests) != 1 {
		t.Fatalf("Claim called %d times, want 1", len(spy.requests))
	}
	req := spy.requests[0]
	if req.Actor != "worker-1" {
		t.Errorf("request.Actor = %q, want the caller's assignee (worker-1), not the store's own actor", req.Actor)
	}
	if req.IssueID != "gc-1" {
		t.Errorf("request.IssueID = %q, want gc-1", req.IssueID)
	}
}

// TestClaimReportsAConflictAsNotClaimedNotAnError pins the (bool, error) idiom
// mutation-list item "map ErrAlreadyClaimed to an error" must catch: a losing
// claim attempt is a value (ok=false, err=nil), matching SQLiteStore.Claim and
// BdStore.Claim, never an error every caller would otherwise have to unwrap.
func TestClaimReportsAConflictAsNotClaimedNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a foreign holder", issueops.ErrAlreadyClaimed},
		{"an ineligible status", issueops.ErrNotClaimable},
		{"a wire-reconstructed conflict wrapping the foreign-holder sentinel", &issueops.ClaimConflictError{Err: issueops.ErrAlreadyClaimed}},
		{"a wire-reconstructed conflict wrapping the ineligible-status sentinel", &issueops.ClaimConflictError{Err: issueops.ErrNotClaimable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &claimRoleSpy{err: tc.err}
			store := newNativeDoltStoreForTest(spy)

			bead, claimed, err := store.Claim("gc-1", "worker-1")
			if err != nil {
				t.Fatalf("error = %v, want nil: a lost claim is a value here", err)
			}
			if claimed {
				t.Fatal("claimed = true on a conflict")
			}
			if bead.ID != "" || bead.Assignee != "" {
				t.Errorf("bead = %+v, want the zero value on a conflict", bead)
			}
		})
	}
}

// TestClaimMapsAMissingIDToErrNotFound pins the deliberate divergence from
// ReleaseIfCurrent: a missing bead is a real, wrapped error here, not folded
// into the (false, nil) conflict idiom — matching SQLiteStore.Claim and
// BdStore.Claim, both of which already surface a missing id as an error.
//
// A genuinely missing id has no row for the reader to find either, so
// Claim's wisp-disambiguation Get (S5b-7-fixup item 8: see the Claim doc
// comment) also misses, and the original issueops.ErrNotFound passes through
// unchanged rather than being reclassified as a wisp refusal.
func TestClaimMapsAMissingIDToErrNotFound(t *testing.T) {
	spy := &claimRoleSpy{err: issueops.ErrNotFound, getErr: issueops.ErrNotFound}
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gc-missing", "worker-1")
	if claimed {
		t.Fatal("claimed = true on a not-found row")
	}
	if err == nil {
		t.Fatal("error = nil, want a wrapped ErrNotFound: a missing bead is not a conflict")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap beads.ErrNotFound", err)
	}
	if errors.Is(err, ErrWispNotClaimable) {
		t.Errorf("error = %v, want it NOT to claim this missing id is a wisp", err)
	}
}

// TestClaimMapsAWispIDToErrWispNotClaimableNotErrNotFound pins
// S5b-7-fixup item 8: issueops.Claimer's own contract (claimer.go) refuses a
// wisp id as the identical issueops.ErrNotFound sentinel a plain missing id
// produces, BEFORE any pre-image read. Before this fix, this front door
// forwarded that sentinel unchanged, leaving a caller unable to tell "this id
// is a wisp" from "this id does not exist". Now Claim asks the reader role,
// which (unlike the claimer) DOES resolve the wisp table — a row the reader
// finds for an id the claimer could not is the signature of a wisp — and
// reports the named beads.ErrWispNotClaimable instead.
func TestClaimMapsAWispIDToErrWispNotClaimableNotErrNotFound(t *testing.T) {
	spy := &claimRoleSpy{
		err:       issueops.ErrNotFound,
		getResult: &issueops.IssueDetails{Issue: beadslib.Issue{ID: "gc-wisp-1", Ephemeral: true}},
	}
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gc-wisp-1", "worker-1")
	if claimed {
		t.Fatal("claimed = true on a wisp id")
	}
	if err == nil {
		t.Fatal("error = nil, want a wrapped ErrWispNotClaimable")
	}
	if !errors.Is(err, ErrWispNotClaimable) {
		t.Errorf("error = %v, want it to wrap beads.ErrWispNotClaimable", err)
	}
}

// TestClaimSurfacesATransportFailure pins that anything other than the two
// named conflict sentinels (or a not-found) travels as a real error, exactly
// like ReleaseIfCurrent's equivalent guard — a transport failure folded into
// "not claimed" would make a caller believe the row was simply contested
// rather than unreachable.
func TestClaimSurfacesATransportFailure(t *testing.T) {
	spy := &claimRoleSpy{err: errors.New("dial tcp: connection refused")}
	store := newNativeDoltStoreForTest(spy)

	if _, claimed, err := store.Claim("gc-1", "worker-1"); err == nil || claimed {
		t.Fatalf("Claim = (claimed=%v, err=%v), want a surfaced transport error", claimed, err)
	}
}

// TestClaimRejectsAnEmptyAssignee pins that Claim never dials the role with a
// blank Actor — issueops.ClaimRequest.Actor becomes the issue's assignee, and
// an empty one would claim a bead for nobody.
func TestClaimRejectsAnEmptyAssignee(t *testing.T) {
	spy := &claimRoleSpy{}
	store := newNativeDoltStoreForTest(spy)

	if _, claimed, err := store.Claim("gc-1", "   "); err == nil || claimed {
		t.Fatalf("Claim with a blank assignee = (claimed=%v, err=%v), want a refusal", claimed, err)
	}
	if len(spy.requests) != 0 {
		t.Fatalf("a claim was dialed with no assignee: %+v", spy.requests)
	}
}
