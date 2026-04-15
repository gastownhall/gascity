package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestNewSessionBeadSnapshot_PoolInstanceSubstitutesTemplate verifies that a
// pool-managed session bead identified only by its pool template is indexed
// under the realized pool_instance name rather than dropped. Without this
// substitution, lookups like `gc status <instance>` cannot map the themed
// instance back to the running session.
func TestNewSessionBeadSnapshot_PoolInstanceSubstitutesTemplate(t *testing.T) {
	bead := beads.Bead{
		ID:     "gc-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/polecat"},
		Metadata: map[string]string{
			"template":             "gascity/polecat",
			"agent_name":           "gascity/polecat",
			"session_name":         "polecat-gc-abc",
			"state":                "active",
			"pool_instance":        "gascity/furiosa",
			poolManagedMetadataKey: "true",
		},
	}

	snap := newSessionBeadSnapshot([]beads.Bead{bead})

	if got := snap.FindSessionNameByTemplate("gascity/furiosa"); got != "polecat-gc-abc" {
		t.Fatalf("FindSessionNameByTemplate(themed instance) = %q, want %q", got, "polecat-gc-abc")
	}
}

// TestNewSessionBeadSnapshot_PoolInstanceSubstitutesTemplateSlotName covers
// the slot-only naming path where pool_instance is of the form "rig/<template>-<slot>".
func TestNewSessionBeadSnapshot_PoolInstanceSubstitutesTemplateSlotName(t *testing.T) {
	bead := beads.Bead{
		ID:     "gc-2",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/dog"},
		Metadata: map[string]string{
			"template":             "gascity/dog",
			"agent_name":           "gascity/dog",
			"session_name":         "dog-gc-def",
			"state":                "active",
			"pool_slot":            "1",
			"pool_instance":        "gascity/dog-1",
			poolManagedMetadataKey: "true",
		},
	}

	snap := newSessionBeadSnapshot([]beads.Bead{bead})

	if got := snap.FindSessionNameByTemplate("gascity/dog-1"); got != "dog-gc-def" {
		t.Fatalf("FindSessionNameByTemplate(slot instance) = %q, want %q", got, "dog-gc-def")
	}
}

// TestNewSessionBeadSnapshot_PoolBeadWithoutInstanceNotMisindexed verifies
// the legacy behavior is preserved: a pool-managed bead that has not yet had
// pool_instance written (pre-fix bead, or bead whose slot has not been claimed
// this cycle) must not be indexed under the bare template name. Doing so would
// make every pool template lookup resolve to a single arbitrary slot's session.
func TestNewSessionBeadSnapshot_PoolBeadWithoutInstanceNotMisindexed(t *testing.T) {
	bead := beads.Bead{
		ID:     "gc-3",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/polecat"},
		Metadata: map[string]string{
			"template":             "gascity/polecat",
			"agent_name":           "gascity/polecat",
			"session_name":         "polecat-gc-xyz",
			"state":                "active",
			poolManagedMetadataKey: "true",
		},
	}

	snap := newSessionBeadSnapshot([]beads.Bead{bead})

	if got := snap.FindSessionNameByTemplate("gascity/polecat"); got != "" {
		t.Fatalf("FindSessionNameByTemplate(bare template) = %q, want empty (pool bead without pool_instance must not be indexed under template)", got)
	}
}

// TestNewSessionBeadSnapshot_NonPoolBeadUnaffectedByPoolInstanceMetadata
// ensures the pool_instance substitution does not change indexing for non-pool
// session beads. Non-pool beads should be indexed under their agent_name and
// (for the template hint) under their template metadata exactly as before.
func TestNewSessionBeadSnapshot_NonPoolBeadUnaffectedByPoolInstanceMetadata(t *testing.T) {
	bead := beads.Bead{
		ID:     "gc-4",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/mayor"},
		Metadata: map[string]string{
			"template":     "gascity/mayor",
			"agent_name":   "gascity/mayor",
			"session_name": "mayor-gc-aaa",
			"state":        "active",
			// Even if pool_instance is incidentally set on a non-pool bead,
			// it must not redirect indexing because the bead is not pool-managed.
			"pool_instance": "should-be-ignored",
		},
	}

	snap := newSessionBeadSnapshot([]beads.Bead{bead})

	if got := snap.FindSessionNameByTemplate("gascity/mayor"); got != "mayor-gc-aaa" {
		t.Fatalf("FindSessionNameByTemplate(non-pool template) = %q, want %q", got, "mayor-gc-aaa")
	}
	if got := snap.FindSessionNameByTemplate("should-be-ignored"); got != "" {
		t.Fatalf("FindSessionNameByTemplate should not index non-pool bead by pool_instance, got %q", got)
	}
}

// TestNewSessionBeadSnapshot_PoolInstanceCoexistsWithCanonicalNamedBead checks
// that when a canonical named bead exists for the same template key, it still
// wins the index over a pool bead that happens to have pool_instance equal to
// the same string. This protects the existing "canonical named beads always
// win" guarantee documented on newSessionBeadSnapshot.
func TestNewSessionBeadSnapshot_PoolInstanceCoexistsWithCanonicalNamedBead(t *testing.T) {
	pool := beads.Bead{
		ID:     "gc-5a",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/polecat"},
		Metadata: map[string]string{
			"template":             "gascity/polecat",
			"agent_name":           "gascity/polecat",
			"session_name":         "polecat-pool",
			"state":                "active",
			"pool_instance":        "gascity/furiosa",
			poolManagedMetadataKey: "true",
		},
	}
	named := beads.Bead{
		ID:     "gc-5b",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/furiosa"},
		Metadata: map[string]string{
			"template":                  "gascity/furiosa",
			"agent_name":                "gascity/furiosa",
			"session_name":              "furiosa-canonical",
			"state":                     "active",
			"configured_named_identity": "gascity/furiosa",
		},
	}

	// Insert pool first so that, without the canonical-wins rule, it would
	// register in the index first.
	snap := newSessionBeadSnapshot([]beads.Bead{pool, named})

	if got := snap.FindSessionNameByTemplate("gascity/furiosa"); got != "furiosa-canonical" {
		t.Fatalf("FindSessionNameByTemplate = %q, want %q (canonical named bead must win over pool substitute)", got, "furiosa-canonical")
	}
}
