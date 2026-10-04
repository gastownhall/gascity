package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// claimCapableLeafStore wraps beads.NewMemStore() and adds a minimal
// two-argument Claim so the emitting class store's forwarding can be
// exercised against a real Store without a live NativeDoltStore. It mirrors
// internal/beads' own claimCapableBackingStore test fixture: a conflict
// (assignee already set to someone else) is a value, not an error.
type claimCapableLeafStore struct {
	beads.Store
	claimCalls int
}

func (c *claimCapableLeafStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	c.claimCalls++
	current, err := c.Get(id)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if current.Assignee != "" && current.Assignee != assignee {
		return beads.Bead{}, false, nil
	}
	status := "in_progress"
	if err := c.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		return beads.Bead{}, false, err
	}
	claimed, err := c.Get(id)
	if err != nil {
		return beads.Bead{}, false, err
	}
	return claimed, true, nil
}

// classStoreClaimer is the capability emittingClassStore.Claim satisfies; a
// local mirror of beadsAssignmentClaimer (internal/storebinding/beads_adapter.go)
// so this test package does not need to import an internal-only type.
type classStoreClaimer interface {
	Claim(id, assignee string) (beads.Bead, bool, error)
}

// TestClassStoreEmissionCoversClaim is the emittingClassStore leg of S5b-4's
// forwarding chain: a successful claim through the relocated class front
// door must both forward to the backing store's Claim and append exactly one
// bead.updated row, matching TestClassStoreEmissionCoversConditionalRelease's
// shape for the release half of the same pair.
func TestClassStoreEmissionCoversClaim(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &claimCapableLeafStore{Store: beads.NewMemStore()}
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	bead := seedClassBead(t, leaf, "claim")

	claimer, ok := store.(classStoreClaimer)
	if !ok {
		t.Fatalf("the emitting class store dropped its Claim method (%T)", store)
	}

	claimed, ok2, err := claimer.Claim(bead.ID, "worker-1")
	if err != nil || !ok2 {
		t.Fatalf("claim: claimed=%v err=%v", ok2, err)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead assignee = %q, want worker-1", claimed.Assignee)
	}
	if leaf.claimCalls != 1 {
		t.Fatalf("backing Claim calls = %d, want 1", leaf.claimCalls)
	}
	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadUpdated || got[0].Subject != bead.ID {
		t.Fatalf("got %s, want one bead.updated for %s", eventSummary(got), bead.ID)
	}

	// A conflict claims nothing and must say nothing: the assignee above
	// still holds it, so claiming for someone else is a value, not a write.
	if _, claimedAgain, err := claimer.Claim(bead.ID, "someone-else"); err != nil || claimedAgain {
		t.Fatalf("conflicted claim: claimed=%v err=%v", claimedAgain, err)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 1 {
		t.Fatalf("a claim that did not land appended a row: %s", eventSummary(got))
	}
}

// TestClassStoreEmissionClaimUnsupportedIsErrClaimUnsupported pins the
// specific sentinel a backing store without the capability must produce: not
// the generic ErrConditionalWriteUnsupported CompareAndSetMetadataKey uses,
// but beads.ErrClaimUnsupported, the same sentinel beadPolicyStore.Claim and
// CachingStore.Claim return for the identical reason.
func TestClassStoreEmissionClaimUnsupportedIsErrClaimUnsupported(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore() // no Claim(string,string) method
	store := resolveGraphStore(splitClassRoutes(leaf).withCLIEmission(cityPath), beads.NewMemStore(), nil, cityPath, nil)

	claimer, ok := store.(classStoreClaimer)
	if !ok {
		t.Fatalf("the emitting class store dropped its Claim method (%T)", store)
	}
	if _, claimed, err := claimer.Claim("gc-1", "worker-1"); !errors.Is(err, beads.ErrClaimUnsupported) || claimed {
		t.Fatalf("Claim over an unsupported backing = (claimed=%v, err=%v), want (false, ErrClaimUnsupported)", claimed, err)
	}
}
