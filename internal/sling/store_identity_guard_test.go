package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// reviewedStoreIdentitySites are this package's store-identity comparisons
// and store-keyed maps (typed-stores T6 plan, OD-9, R3), keyed "file func
// rule". The table only shrinks: a new comparison uses beads.SameStore.
var reviewedStoreIdentitySites = map[string]beadstest.IdentitySite{
	"sling_attachment.go sameStoreValue eq": {Count: 1, Reason: "the panic-safe raw == helper attachmentScope.stores dedupes with; T6d replaces it with beads.SameStore"},
}

// TestStoreIdentityGuard holds internal/sling's store comparisons to the
// reviewed table.
func TestStoreIdentityGuard(t *testing.T) {
	beadstest.CheckStoreIdentity(t, ".", reviewedStoreIdentitySites)
}
