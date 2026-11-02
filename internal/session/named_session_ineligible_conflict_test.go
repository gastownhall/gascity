package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// These tests cover the eligibility asymmetry between canonical detection and
// conflict detection for configured named sessions (ga-iqqsmy).
//
// Every canonical pass gates on NamedSessionContinuityEligible, so a bead in a
// continuity-ineligible state can never be elected to OWN a configured name.
// Conflict detection did not apply that gate, and BeadConflictsWithNamedSession
// flags any bead whose alias equals the identity. A session's own bead in an
// ineligible state therefore BLOCKED the very name it could not own, leaving
// the name permanently unusable: no canonical owner, and a standing conflict.

func ineligibleSpec() NamedSessionSpec {
	return NamedSessionSpec{
		Agent:       &config.Agent{Name: "deployer", Dir: "beads"},
		Identity:    "beads/deployer",
		SessionName: "beads--deployer",
	}
}

// aliasOwnedBead mirrors a real pool-instance session bead that backs a
// configured named session: alias names the configured identity,
// template/agent_name corroborate it, and session_name is instance-shaped so it
// matches neither spec.SessionName nor spec.Identity.
func aliasOwnedBead(spec NamedSessionSpec, sessionName string) beads.Bead {
	return beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        "awake",
			"session_name": sessionName,
			"template":     spec.Identity,
			"agent_name":   spec.Identity,
			"alias":        spec.Identity,
		},
	}
}

// TestLookupNamedSession_LiveAliasOwnedBeadResolvesCanonically guards the case
// already fixed by the canonical alias pass: a live, eligible own bead owns its
// name and is not reported as a conflict.
func TestLookupNamedSession_LiveAliasOwnedBeadResolvesCanonically(t *testing.T) {
	store := beads.NewMemStore()
	spec := ineligibleSpec()
	own, err := store.Create(aliasOwnedBead(spec, "deployer-inst-live"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasCanonical {
		t.Errorf("HasCanonical = false, want the live own bead %q to own %q", own.ID, spec.Identity)
	}
	if lookup.HasConflict {
		t.Errorf("HasConflict = true (%q), want no conflict for the name's own live bead", lookup.Conflict.ID)
	}
}

// TestLookupNamedSession_IneligibleOwnBeadDoesNotBlockItsName is the regression
// test for ga-iqqsmy: a bead that cannot own the name must not block it either.
func TestLookupNamedSession_IneligibleOwnBeadDoesNotBlockItsName(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		continuity string
	}{
		{"archived", "archived", ""},
		{"archived-continuity-false", "archived", "false"},
		{"closing", "closing", ""},
		{"closed-state", "closed", ""},
		{"awake-continuity-false", "awake", "false"},
		{"failed-create", "failed-create", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			spec := ineligibleSpec()
			b := aliasOwnedBead(spec, "deployer-inst")
			b.Metadata["state"] = tc.state
			if tc.continuity != "" {
				b.Metadata["continuity_eligible"] = tc.continuity
			}
			own, err := store.Create(b)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if NamedSessionContinuityEligible(b) {
				t.Fatalf("fixture is continuity-eligible; this case must be ineligible")
			}
			lookup, err := LookupConfiguredNamedSession(store, spec)
			if err != nil {
				t.Fatalf("LookupConfiguredNamedSession: %v", err)
			}
			if lookup.HasConflict && lookup.Conflict.ID == own.ID {
				t.Errorf("name %q permanently unusable: ineligible own bead %q cannot be canonical but still conflicts",
					spec.Identity, own.ID)
			}
		})
	}
}

// TestFindNamedSessionConflict_StillFlagsForeignAliasClaimant guards the
// ga-4of1nc case in the other direction: an unrelated LIVE bead that merely
// claims the reserved alias, with no corroborating template/agent_name, must
// still surface as a conflict.
func TestFindNamedSessionConflict_StillFlagsForeignAliasClaimant(t *testing.T) {
	spec := ineligibleSpec()
	foreign := beads.Bead{
		ID:     "foreign",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":      "awake",
			"alias":      spec.Identity,
			"template":   "some/other",
			"agent_name": "some/other",
		},
	}
	if _, ok := FindNamedSessionConflict([]beads.Bead{foreign}, spec); !ok {
		t.Error("foreign live alias claimant was not reported as a conflict")
	}
}
