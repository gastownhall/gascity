package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestLifecycleIdentityReleasedInfoEquivalence is the load-bearing oracle for
// the W-flip retire-lane migration: LifecycleIdentityReleasedInfo(info) must
// agree byte-for-byte with LifecycleIdentityReleased(b.Status, b.Metadata) for
// any info == infoFromPersistedBead(b). The retire lane reads it off the typed
// Info feed instead of the raw bead, so a divergence would retire (or spare) the
// wrong named-session identities. The corpus spans the eligible/ineligible ×
// released/holding × open/closed matrix, and the direct-branch assertions below
// make a mutation of either conjunct (the continuity gate or the identifier
// gate) fail.
func TestLifecycleIdentityReleasedInfoEquivalence(t *testing.T) {
	beadsIn := []beads.Bead{
		{
			// Continuity-ineligible + identifiers released → retired.
			ID:     "ga-released",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":                 "archived",
				"continuity_eligible":   "false",
				"alias":                 "",
				"session_name":          "",
				"session_name_explicit": "",
			},
		},
		{
			// Ineligible but still holding an alias → NOT released.
			ID:     "ga-holding-alias",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":               "archived",
				"continuity_eligible": "false",
				"alias":               "worker",
				"session_name":        "",
			},
		},
		{
			// Ineligible but still holding session_name → NOT released.
			ID:     "ga-holding-sn",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":               "asleep",
				"continuity_eligible": "false",
				"alias":               "",
				"session_name":        "s-worker",
			},
		},
		{
			// Ineligible but still holding session_name_explicit → NOT released.
			ID:     "ga-holding-sn-explicit",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":                 "asleep",
				"continuity_eligible":   "false",
				"alias":                 "",
				"session_name":          "",
				"session_name_explicit": "true",
			},
		},
		{
			// Continuity-eligible with released identifiers → NOT released (the
			// continuity gate spares it even though identifiers are blank).
			ID:     "ga-eligible-released",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":               "asleep",
				"continuity_eligible": "true",
				"alias":               "",
				"session_name":        "",
			},
		},
		{
			// Archived + continuity true → still owns identity.
			ID:     "ga-archived-eligible",
			Type:   BeadType,
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":               "archived",
				"continuity_eligible": "true",
				"alias":               "worker",
				"session_name":        "s-worker",
			},
		},
		{
			// Closed bead with released identifiers: closed base state is not
			// continuity-eligible, so the identifier gate decides → released.
			ID:     "ga-closed-released",
			Type:   BeadType,
			Status: "closed",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"state":        "closed",
				"alias":        "",
				"session_name": "",
			},
		},
	}

	// Byte-identical equivalence over the whole corpus.
	for _, b := range beadsIn {
		info := infoFromPersistedBead(b)
		got := LifecycleIdentityReleasedInfo(info)
		want := LifecycleIdentityReleased(b.Status, b.Metadata)
		if got != want {
			t.Errorf("bead %q: LifecycleIdentityReleasedInfo=%v want LifecycleIdentityReleased=%v", b.ID, got, want)
		}
	}

	// Direct-branch assertions so a mutation of either gate fails, independent of
	// the raw twin (guards against both twins drifting together).
	released := LifecycleIdentityReleasedInfo(infoFromPersistedBead(beadsIn[0]))
	if !released {
		t.Fatal("ineligible + released identifiers must be released")
	}
	holding := LifecycleIdentityReleasedInfo(infoFromPersistedBead(beadsIn[1]))
	if holding {
		t.Fatal("ineligible but holding an alias must NOT be released")
	}
	eligible := LifecycleIdentityReleasedInfo(infoFromPersistedBead(beadsIn[4]))
	if eligible {
		t.Fatal("continuity-eligible must NOT be released even with blank identifiers")
	}
}

// TestLifecycleIdentifiersReleasedInfoEquivalence pins the identifier-gate half
// on its own: LifecycleIdentifiersReleasedInfo(info) equals
// LifecycleIdentifiersReleased(b.Metadata) for the three identifier keys (alias,
// session_name, session_name_explicit), so a dropped key would surface here.
func TestLifecycleIdentifiersReleasedInfoEquivalence(t *testing.T) {
	cases := []map[string]string{
		{},
		{"alias": "x"},
		{"session_name": "s"},
		{"session_name_explicit": "true"},
		{"alias": "  ", "session_name": "  ", "session_name_explicit": "  "}, // whitespace trims to released
	}
	for i, meta := range cases {
		b := beads.Bead{ID: "ga", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta}
		info := infoFromPersistedBead(b)
		if got, want := LifecycleIdentifiersReleasedInfo(info), LifecycleIdentifiersReleased(meta); got != want {
			t.Errorf("case %d meta=%v: LifecycleIdentifiersReleasedInfo=%v want %v", i, meta, got, want)
		}
	}
}
