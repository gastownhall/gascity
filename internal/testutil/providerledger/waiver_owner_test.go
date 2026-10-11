package providerledger

import (
	"regexp"
	"testing"
)

// waiverOwnerPattern is the shape of a bead id: a prefix, a suffix, and an
// optional child index. It is deliberately not a liveness check — resolving a
// bead needs a bd shell-out, which would itself need a waiver from this ledger.
var waiverOwnerPattern = regexp.MustCompile(`^[a-z]{2,4}-[a-z0-9]+(\.[0-9]+)*$`)

// TestRuntimeWaiverOwnerIsPinnedAndWellFormed pins the owner of every runtime
// waiver to one literal.
//
// The catalog check only asserts that each waiver matches the constant, so
// editing the constant silently re-owns all of them at once. Pinning the value
// here makes that reassignment a deliberate edit with a diff a reviewer sees:
// these waivers are waiting on ga-80po0c.3 ("H5: contract production runtime
// compositions once"), and pointing them at a bead that does not resolve leaves
// the fleet with eight dated waivers nobody is on the hook for.
func TestRuntimeWaiverOwnerIsPinnedAndWellFormed(t *testing.T) {
	const want = "ga-80po0c.3"
	if runtimeContractWaiverOwner != want {
		t.Errorf("runtimeContractWaiverOwner = %q, want %q; re-owning the runtime waivers needs the new owner recorded here too", runtimeContractWaiverOwner, want)
	}
	if !waiverOwnerPattern.MatchString(runtimeContractWaiverOwner) {
		t.Errorf("runtimeContractWaiverOwner = %q, want a bead id like ga-80po0c.3", runtimeContractWaiverOwner)
	}
}

// TestEveryWaiverNamesAnOwner checks that no catalog waiver is left unowned.
//
// Validate reports an empty owner as a structural problem, but only when the
// waiver is otherwise well formed enough to be reached; this asserts it
// directly against the shipped catalog so an unowned waiver cannot ride in
// behind an unrelated failure.
func TestEveryWaiverNamesAnOwner(t *testing.T) {
	for _, entry := range Catalog() {
		for _, claim := range entry.Claims {
			if claim.Waiver == nil {
				continue
			}
			if !waiverOwnerPattern.MatchString(claim.Waiver.Owner) {
				t.Errorf("%s %s: waiver owner = %q, want a bead id", entry.ID, claim.Contract, claim.Waiver.Owner)
			}
		}
	}
}
