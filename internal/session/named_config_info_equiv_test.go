package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestNamedSessionInfoEquivalence is the byte-identical oracle for migrating the
// named-session snapshot consumers onto session.Info. Every Info-form classifier
// and the two Find*Info selectors must agree with their raw-bead originals for
// the same bead once projected through InfoFromPersistedBead.
func TestNamedSessionInfoEquivalence(t *testing.T) {
	spec := NamedSessionSpec{
		Agent:       &config.Agent{Name: "mayor"},
		Identity:    "mayor",
		SessionName: "mayor-session",
		Mode:        "always",
	}

	beadsIn := []beads.Bead{
		{
			ID:     "ga-canonical",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "mayor",
				"session_name":               "mayor-session",
				"state":                      "active",
			},
		},
		{
			// Template-match canonical (no configured flag), matches by backing template.
			ID:     "ga-template-match",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"template":     "mayor",
				"session_name": "mayor-session",
			},
		},
		{
			// Conflict: same runtime session_name but a different (non-matching) template.
			ID:     "ga-conflict-sn",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"template":     "polecat",
				"session_name": "mayor-session",
			},
		},
		{
			// Conflict: alias equals the identity.
			ID:     "ga-conflict-alias",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"alias":        "mayor",
				"session_name": "other-session",
			},
		},
		{
			// Continuity-ineligible: explicit false.
			ID:     "ga-ineligible",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "mayor",
				"session_name":               "mayor-session",
				"continuity_eligible":        "false",
			},
		},
		{
			// Archived + continuity true → eligible only when true.
			ID:     "ga-archived",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "mayor",
				"session_name":               "mayor-session",
				"state":                      "archived",
				"continuity_eligible":        "true",
			},
		},
		{
			// Repairable: empty type but carries the session label.
			ID:     "ga-repairable",
			Type:   "",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "mayor",
				"session_name":               "mayor-session",
			},
		},
		{
			// Not a session bead at all (wrong type, no label).
			ID:   "ga-nonsession",
			Type: "task",
			Metadata: map[string]string{
				"session_name": "mayor-session",
			},
		},
		{
			ID:     "ga-closed",
			Type:   BeadType,
			Status: "closed",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "mayor",
				"session_name":               "mayor-session",
			},
		},
	}

	// Per-bead classifier equivalence.
	for _, b := range beadsIn {
		i := infoFromPersistedBead(b)
		if got, want := IsSessionBeadOrRepairableInfo(i), IsSessionBeadOrRepairable(b); got != want {
			t.Errorf("bead %q: IsSessionBeadOrRepairableInfo=%v want %v", b.ID, got, want)
		}
		if got, want := IsNamedSessionInfo(i), IsNamedSessionBead(b); got != want {
			t.Errorf("bead %q: IsNamedSessionInfo=%v want %v", b.ID, got, want)
		}
		if got, want := NamedSessionIdentityInfo(i), NamedSessionIdentity(b); got != want {
			t.Errorf("bead %q: NamedSessionIdentityInfo=%q want %q", b.ID, got, want)
		}
		if got, want := NamedSessionInfoMatchesSpec(i, spec), NamedSessionBeadMatchesSpec(b, spec); got != want {
			t.Errorf("bead %q: NamedSessionInfoMatchesSpec=%v want %v", b.ID, got, want)
		}
		if got, want := NamedSessionInfoContinuityEligible(i), NamedSessionContinuityEligible(b); got != want {
			t.Errorf("bead %q: NamedSessionInfoContinuityEligible=%v want %v", b.ID, got, want)
		}
		if got, want := InfoConflictsWithNamedSession(i, spec), BeadConflictsWithNamedSession(b, spec); got != want {
			t.Errorf("bead %q: InfoConflictsWithNamedSession=%v want %v", b.ID, got, want)
		}
	}

	// Selector equivalence over several candidate slices (order matters).
	slices := [][]beads.Bead{
		beadsIn,
		{beadsIn[2], beadsIn[3]}, // conflicts only, no canonical
		{beadsIn[1]},             // template-match canonical
		{beadsIn[4], beadsIn[0]}, // ineligible before canonical
		{beadsIn[8]},             // closed only
		{beadsIn[7]},             // non-session only
	}
	for si, candidates := range slices {
		infos := make([]Info, len(candidates))
		for k, b := range candidates {
			infos[k] = infoFromPersistedBead(b)
		}

		wantBead, wantOK := FindCanonicalNamedSessionBead(candidates, spec)
		gotInfo, gotOK := FindCanonicalNamedSessionInfo(infos, spec)
		if wantOK != gotOK || wantBead.ID != gotInfo.ID {
			t.Errorf("slice %d: FindCanonicalNamedSessionInfo=(%q,%v) want (%q,%v)", si, gotInfo.ID, gotOK, wantBead.ID, wantOK)
		}

		wantCBead, wantCOK := FindNamedSessionConflict(candidates, spec)
		gotCInfo, gotCOK := FindNamedSessionConflictInfo(infos, spec)
		if wantCOK != gotCOK || wantCBead.ID != gotCInfo.ID {
			t.Errorf("slice %d: FindNamedSessionConflictInfo=(%q,%v) want (%q,%v)", si, gotCInfo.ID, gotCOK, wantCBead.ID, wantCOK)
		}
	}
}
