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
