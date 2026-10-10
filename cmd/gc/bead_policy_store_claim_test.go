package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadPolicyStoreClaimForwardsToACapableBackingStore pins beadPolicyStore
// as a pure pass-through in the claim forwarding chain, the same shape as its
// ReleaseIfCurrent: it holds no id, no assignee and no policy of its own, so a
// successful claim on the wrapped store must come back unchanged.
func TestBeadPolicyStoreClaimForwardsToACapableBackingStore(t *testing.T) {
	leaf := &claimCapableLeafStore{Store: beads.NewMemStore()}
	bead, err := leaf.Create(beads.Bead{Title: "claimable", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	policy := wrapStoreWithBeadPolicies(leaf, nil)
	claimer, ok := policy.(classStoreClaimer)
	if !ok {
		t.Fatalf("wrapStoreWithBeadPolicies dropped the Claim capability (%T)", policy)
	}

	claimed, ok2, err := claimer.Claim(bead.ID, "worker-1")
	if err != nil || !ok2 {
		t.Fatalf("Claim = (claimed=%v, ok=%v, err=%v), want a landed claim", claimed, ok2, err)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead assignee = %q, want worker-1", claimed.Assignee)
	}
	if leaf.claimCalls != 1 {
		t.Fatalf("backing Claim calls = %d, want 1", leaf.claimCalls)
	}
}

// TestBeadPolicyStoreClaimUnsupportedIsErrClaimUnsupported pins the refusal a
// backing store without the two-argument Claim capability must produce
// through the policy layer.
func TestBeadPolicyStoreClaimUnsupportedIsErrClaimUnsupported(t *testing.T) {
	policy := wrapStoreWithBeadPolicies(beads.NewMemStore(), nil)
	claimer, ok := policy.(classStoreClaimer)
	if !ok {
		t.Fatalf("wrapStoreWithBeadPolicies dropped the Claim method (%T)", policy)
	}
	if _, claimed, err := claimer.Claim("gc-1", "worker-1"); !errors.Is(err, beads.ErrClaimUnsupported) || claimed {
		t.Fatalf("Claim over an unsupported backing = (claimed=%v, err=%v), want (false, ErrClaimUnsupported)", claimed, err)
	}
}
