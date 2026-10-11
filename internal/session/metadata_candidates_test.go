package session

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestExactMetadataSessionCandidatesInfoMatchesRawProjection pins the new
// Info-projecting sibling: for every filter set it returns exactly
// InfoFromPersistedBead of each ExactMetadataSessionCandidates result, in the
// same order — the codec is applied once at this edge and nothing else changes.
// The fixture covers a match, a non-session non-match (dropped by both), and a
// closed row (included only when includeClosed is set), so the order + membership
// equivalence is load-bearing.
func TestExactMetadataSessionCandidatesInfoMatchesRawProjection(t *testing.T) {
	store := beads.NewMemStore()
	openMatch, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":        "sky",
			"alias":               "sky-alias",
			"state":               "active",
			"continuity_eligible": "true",
		},
	})
	if err != nil {
		t.Fatalf("Create(open match): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task", // non-session: excluded by IsSessionBeadOrRepairable
		Metadata: map[string]string{"session_name": "sky"},
	}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	closedMatch, err := store.Create(beads.Bead{
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "sky"},
	})
	if err != nil {
		t.Fatalf("Create(closed match): %v", err)
	}
	if err := store.Close(closedMatch.ID); err != nil {
		t.Fatalf("Close(%s): %v", closedMatch.ID, err)
	}

	for _, includeClosed := range []bool{false, true} {
		filter := map[string]string{"session_name": "sky"}
		raw, err := ExactMetadataSessionCandidates(store, includeClosed, filter)
		if err != nil {
			t.Fatalf("ExactMetadataSessionCandidates(includeClosed=%v): %v", includeClosed, err)
		}
		infos, err := ExactMetadataSessionCandidatesInfo(store, includeClosed, filter)
		if err != nil {
			t.Fatalf("ExactMetadataSessionCandidatesInfo(includeClosed=%v): %v", includeClosed, err)
		}
		if len(infos) != len(raw) {
			t.Fatalf("includeClosed=%v: len(infos)=%d len(raw)=%d", includeClosed, len(infos), len(raw))
		}
		for i := range raw {
			if !reflect.DeepEqual(infos[i], infoFromPersistedBead(raw[i])) {
				t.Errorf("includeClosed=%v: infos[%d] (id %q) != infoFromPersistedBead(raw[%d] id %q)", includeClosed, i, infos[i].ID, i, raw[i].ID)
			}
		}
	}

	// Membership sanity: the open match is always present; the closed match only
	// appears with includeClosed; the task bead never appears.
	openOnly, _ := ExactMetadataSessionCandidatesInfo(store, false, map[string]string{"session_name": "sky"})
	if len(openOnly) != 1 || openOnly[0].ID != openMatch.ID {
		t.Fatalf("includeClosed=false: got %d infos, want only open %s", len(openOnly), openMatch.ID)
	}
	withClosed, _ := ExactMetadataSessionCandidatesInfo(store, true, map[string]string{"session_name": "sky"})
	if len(withClosed) != 2 {
		t.Fatalf("includeClosed=true: got %d infos, want open + closed", len(withClosed))
	}
}

func TestExactMetadataSessionCandidatesDeduplicatesAndFiltersSessions(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
			"alias":        "sky",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			"session_name": "sky",
		},
	}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}

	candidates, err := ExactMetadataSessionCandidates(store, false,
		map[string]string{"session_name": "sky"},
		map[string]string{"alias": "sky"},
		map[string]string{"session_name": ""},
		map[string]string{"": "sky"},
		map[string]string{"session_name": "sky", "alias": "sky"},
	)
	if err != nil {
		t.Fatalf("ExactMetadataSessionCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != sessionBead.ID {
		t.Fatalf("candidates = %#v, want only %s", candidates, sessionBead.ID)
	}
}

func TestExactMetadataSessionCandidatesWithStatusReturnsOnlyStatus(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	closed, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(%s): %v", closed.ID, err)
	}

	candidates, err := ExactMetadataSessionCandidatesWithStatus(store, "closed",
		map[string]string{"session_name": "sky"},
	)
	if err != nil {
		t.Fatalf("ExactMetadataSessionCandidatesWithStatus: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != closed.ID {
		t.Fatalf("candidates = %#v, want closed %s and not open %s", candidates, closed.ID, open.ID)
	}
}
