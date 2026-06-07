package delivery_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

func beadWithPhase(phase string) beads.Bead {
	meta := map[string]string{}
	if phase != "" {
		meta[delivery.MetaKeyPhase] = phase
	}
	return beads.Bead{
		ID:       "test-bead",
		Title:    "test",
		Status:   "open",
		Metadata: meta,
	}
}

func TestValidateClosePrerequisites_NonTerminalReturnsError(t *testing.T) {
	nonTerminal := []string{
		delivery.PhaseBuilding,
		delivery.PhaseCIPending,
		delivery.PhaseReviewPending,
		delivery.PhaseRework,
		delivery.PhaseDecisionPending,
		delivery.PhaseMergePending,
		delivery.PhaseConflicted,
	}

	for _, p := range nonTerminal {
		b := beadWithPhase(p)
		if err := delivery.ValidateClosePrerequisites(b); err == nil {
			t.Errorf("phase %q: expected ErrNotInTerminalPhase, got nil", p)
		} else if !errors.Is(err, delivery.ErrNotInTerminalPhase) {
			t.Errorf("phase %q: expected ErrNotInTerminalPhase, got %v", p, err)
		}
	}
}

func TestValidateClosePrerequisites_TerminalReturnsNil(t *testing.T) {
	terminal := []string{delivery.PhaseMerged, delivery.PhaseAbandoned}

	for _, p := range terminal {
		b := beadWithPhase(p)
		if err := delivery.ValidateClosePrerequisites(b); err != nil {
			t.Errorf("phase %q: expected nil, got %v", p, err)
		}
	}
}

func TestValidateClosePrerequisites_UnsetPhaseReturnsNil(t *testing.T) {
	// A bead with no gc.phase is not a delivery bead — close is allowed.
	b := beadWithPhase("")
	if err := delivery.ValidateClosePrerequisites(b); err != nil {
		t.Errorf("unset phase: expected nil, got %v", err)
	}
}

func TestValidateClosePrerequisites_NilMetadataReturnsNil(t *testing.T) {
	// A bead with no Metadata map is not a delivery bead.
	b := beads.Bead{ID: "test", Title: "test", Status: "open"}
	if err := delivery.ValidateClosePrerequisites(b); err != nil {
		t.Errorf("nil metadata: expected nil, got %v", err)
	}
}
