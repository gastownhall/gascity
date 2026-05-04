package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

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
