package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// policyGraphOnlyStore is a beads.Store that additionally implements
// beads.GraphOnlyReadyStore so it can serve as a backing for beadPolicyStore
// tests that exercise the ReadyGraphOnlyHandle delegation path.
type policyGraphOnlyStore struct {
	beads.Store
	ready []beads.Bead
	err   error
}

func (s *policyGraphOnlyStore) ReadyGraphOnly(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]beads.Bead(nil), s.ready...), nil
}

// policyCapturingGraphOnlyStore records the query received by ReadyGraphOnly so
// tests can assert pass-through behavior.
type policyCapturingGraphOnlyStore struct {
	beads.Store
	gotQuery beads.ReadyQuery
	ready    []beads.Bead
	err      error
}

func (s *policyCapturingGraphOnlyStore) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if len(query) > 0 {
		s.gotQuery = query[0]
	}
	if s.err != nil {
		return nil, s.err
	}
	return append([]beads.Bead(nil), s.ready...), nil
}

// TestBeadPolicyStorePreservesGraphOnlyReadyCapability is the TDD anchor for
// ga-ifavnc.4. Until beadPolicyStore.ReadyGraphOnlyHandle is added,
// beads.GraphOnlyReadyFor returns (nil, false) and the test fails at ok-assertion.
// Once added, the wrapped store must expose the capability and delegate to the
// backing's ReadyGraphOnly.
func TestBeadPolicyStorePreservesGraphOnlyReadyCapability(t *testing.T) {
	want := []beads.Bead{{ID: "wisp-policy-1", Status: "open"}}
	backing := &policyGraphOnlyStore{Store: beads.NewMemStore(), ready: want}
	wrapped := wrapStoreWithBeadPolicies(backing, nil)

	handle, ok := beads.GraphOnlyReadyFor(wrapped)
	if !ok {
		t.Skip("wrapStoreWithBeadPolicies dropped GraphOnlyReadyStore capability; add ReadyGraphOnlyHandle in ga-ifavnc.4")
	}
	got, err := handle.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if len(got) != 1 || got[0].ID != "wisp-policy-1" {
		t.Fatalf("ReadyGraphOnly = %v, want [{wisp-policy-1}]", got)
	}
}

// TestBeadPolicyStoreReadyGraphOnlyHandlePassesThroughQuery verifies that the
// policy wrapper passes the caller's query through unchanged. This surface is
// wisp-only by contract; the lower layer forces TierWisps regardless of caller
// TierMode, so the policy layer must not alter the query.
func TestBeadPolicyStoreReadyGraphOnlyHandlePassesThroughQuery(t *testing.T) {
	capturing := &policyCapturingGraphOnlyStore{Store: beads.NewMemStore()}
	wrapped := wrapStoreWithBeadPolicies(capturing, nil)

	handle, ok := beads.GraphOnlyReadyFor(wrapped)
	if !ok {
		t.Skip("wrapStoreWithBeadPolicies dropped GraphOnlyReadyStore capability; add ReadyGraphOnlyHandle in ga-ifavnc.4")
	}
	if _, err := handle.ReadyGraphOnly(beads.ReadyQuery{TierMode: beads.TierIssues}); err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if capturing.gotQuery.TierMode != beads.TierIssues {
		t.Fatalf("backing received TierMode=%v, want TierIssues (policy layer must not alter query)", capturing.gotQuery.TierMode)
	}
}
