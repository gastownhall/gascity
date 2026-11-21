package beads

import (
	"context"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

type releaseRoleSpy struct {
	beadslib.Storage
	requests []issueops.ReleaseRequest
	result   issueops.ReleaseResult
	err      error
}

func (s *releaseRoleSpy) Releaser() (issueops.Releaser, error) { return s, nil }

func (s *releaseRoleSpy) Release(_ context.Context, req issueops.ReleaseRequest) (issueops.ReleaseResult, error) {
	s.requests = append(s.requests, req)
	return s.result, s.err
}

func TestReleaseIfCurrentGuardsOnTheNamedHolder(t *testing.T) {
	spy := &releaseRoleSpy{result: issueops.ReleaseResult{Changed: true}}
	store := newNativeDoltStoreForTest(spy)

	released, err := store.ReleaseIfCurrent("gc-1", "worker-1")
	if err != nil || !released {
		t.Fatalf("ReleaseIfCurrent = (%v, %v), want (true, nil)", released, err)
	}
	if len(spy.requests) != 1 {
		t.Fatalf("Release called %d times, want 1", len(spy.requests))
	}
	req := spy.requests[0]
	if req.Actor != "native-test" || req.IssueID != "gc-1" {
		t.Errorf("request = %+v, want the store actor and the caller's id", req)
	}
	if req.ExpectedAssignee == nil || *req.ExpectedAssignee != "worker-1" {
		t.Errorf("ExpectedAssignee = %v, want the named holder — a nil expectation selects the unconditional path, where the FENCE's subject is Actor and gc's actor is not the holder", req.ExpectedAssignee)
	}
	if req.Force {
		t.Error("Force is set; it may not accompany an expectation and would release whoever holds the claim")
	}
}

// The four refusals a conditional release can raise are all (false, nil) here.
// gc's front door reports "somebody else got there first" as a value, not an
// error, and every caller of it acts on the boolean.
func TestReleaseIfCurrentReportsEveryConditionalRefusalAsNotReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a different holder", issueops.ErrAssigneeMismatch},
		{"no claim at all", issueops.ErrNotClaimed},
		{"a status that accepts no release", issueops.ErrNotReleasable},
		{"a bead that is gone", issueops.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &releaseRoleSpy{err: tc.err}
			store := newNativeDoltStoreForTest(spy)
			released, err := store.ReleaseIfCurrent("gc-1", "worker-1")
			if err != nil {
				t.Fatalf("error = %v, want nil: a lost release is a value here", err)
			}
			if released {
				t.Fatal("released = true on a refusal")
			}
		})
	}
}

// Everything else still travels. A transport failure reported as "not
// released" would make a reconciler believe a claim is still held.
func TestReleaseIfCurrentSurfacesATransportFailure(t *testing.T) {
	spy := &releaseRoleSpy{err: errors.New("dial tcp: connection refused")}
	store := newNativeDoltStoreForTest(spy)

	if _, err := store.ReleaseIfCurrent("gc-1", "worker-1"); err == nil {
		t.Fatal("a transport failure was swallowed into (false, nil)")
	}
}

// An empty expectation is not a release at all: the role refuses a non-nil
// pointer to "" as ErrValidation, and a nil one would select the unconditional
// path whose fence subject is the ACTOR — which would let gc release a claim
// it was told to leave alone.
func TestReleaseIfCurrentWithNoHolderDialsNothing(t *testing.T) {
	spy := &releaseRoleSpy{}
	store := newNativeDoltStoreForTest(spy)

	released, err := store.ReleaseIfCurrent("gc-1", "")
	if err != nil || released {
		t.Fatalf("ReleaseIfCurrent with no expected holder = (%v, %v), want (false, nil)", released, err)
	}
	if len(spy.requests) != 0 {
		t.Fatalf("a release was dialed with no holder to guard on: %+v", spy.requests)
	}
}

// divergentRelease seeds one row and names the holder a release expects on it.
type divergentRelease struct {
	name     string
	assignee string
	status   string
	expected string
}

// The two edges on which the store family answers a release differently,
// pinned as NativeDoltStore answers them today so that a further flip fails
// here instead of passing silently. Which answer the family should share is
// still open (the FAMILY DIVERGENCE LEDGER on ConditionalAssignmentReleaser).
// Each row runs against MemStore too, which answers it the other way: that is
// the control proving the seed reaches the edge the row names, rather than a
// shape on which every store agrees.
func TestReleaseIfCurrentPinsTheFamilyDivergenceEdges(t *testing.T) {
	for _, tc := range []struct {
		row        divergentRelease
		wantNative bool
		wantMem    bool
	}{
		// The role releases from open as well as in_progress: an open row that
		// still names a holder is the orphaned claim gc's reconcilers clear.
		{divergentRelease{"an open row that still carries an assignee", "worker-1", "open", "worker-1"}, true, false},
		// Releasing a row nobody holds describes no release, so the native
		// front door answers it without reaching the role.
		{divergentRelease{"an empty expected holder over an unassigned in_progress row", "", "in_progress", ""}, false, true},
	} {
		t.Run(tc.row.name, func(t *testing.T) {
			assertDivergentRelease(t, "NativeDoltStore", newNativeDoltStoreForTest(newNativeDoltMemStorage()), tc.row, tc.wantNative)
			assertDivergentRelease(t, "MemStore", NewMemStore(), tc.row, tc.wantMem)
		})
	}
}

// assertDivergentRelease seeds row in store, releases it against the row's
// expected holder, and checks the verdict and the row it leaves behind: a
// release leaves it open and unassigned, and anything else leaves the seed
// untouched.
func assertDivergentRelease(t *testing.T, label string, store interface {
	Store
	ConditionalAssignmentReleaser
}, row divergentRelease, want bool,
) {
	t.Helper()
	created, err := store.Create(Bead{Title: "claimed work", Assignee: row.assignee})
	if err != nil {
		t.Fatalf("%s: Create: %v", label, err)
	}
	if err := store.Update(created.ID, UpdateOpts{Status: &row.status}); err != nil {
		t.Fatalf("%s: Update status to %q: %v", label, row.status, err)
	}
	seeded, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("%s: Get after seeding: %v", label, err)
	}
	if seeded.Status != row.status || seeded.Assignee != row.assignee {
		t.Fatalf("%s: seeded row = (status %q, assignee %q), want (%q, %q)", label, seeded.Status, seeded.Assignee, row.status, row.assignee)
	}

	released, err := store.ReleaseIfCurrent(created.ID, row.expected)
	if err != nil {
		t.Fatalf("%s: ReleaseIfCurrent: %v", label, err)
	}
	if released != want {
		t.Errorf("%s: ReleaseIfCurrent = %v, want %v", label, released, want)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("%s: Get after release: %v", label, err)
	}
	wantStatus, wantAssignee := row.status, row.assignee
	if want {
		wantStatus, wantAssignee = "open", ""
	}
	if after.Status != wantStatus || after.Assignee != wantAssignee {
		t.Errorf("%s: row after release = (status %q, assignee %q), want (%q, %q)", label, after.Status, after.Assignee, wantStatus, wantAssignee)
	}
}
