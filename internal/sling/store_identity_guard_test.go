package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// reviewedStoreIdentitySites are this package's store-identity comparisons
// and store-keyed maps (typed-stores T6 plan, OD-9, R3), keyed "file func
// rule". The table only shrinks: a new comparison uses beads.SameStore.
var reviewedStoreIdentitySites = map[string]beadstest.IdentitySite{}

// TestStoreIdentityGuard holds internal/sling's store comparisons to the
// reviewed table.
func TestStoreIdentityGuard(t *testing.T) {
	beadstest.CheckStoreIdentity(t, ".", reviewedStoreIdentitySites)
}
