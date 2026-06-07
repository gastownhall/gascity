package delivery_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/delivery"
)

func TestValidatePhaseTransition(t *testing.T) {
	legalCases := []struct {
		from, to string
	}{
		// bootstrap
		{"", PhaseBuilding},
		// building edges
		{PhaseBuilding, PhaseCIPending},
		{PhaseBuilding, PhaseAbandoned},
		// ci-pending edges
		{PhaseCIPending, PhaseReviewPending},
		{PhaseCIPending, PhaseRework},
		{PhaseCIPending, PhaseConflicted},
		{PhaseCIPending, PhaseAbandoned},
		// review-pending edges
		{PhaseReviewPending, PhaseRework},
		{PhaseReviewPending, PhaseDecisionPending},
		{PhaseReviewPending, PhaseAbandoned},
		// rework edges
		{PhaseRework, PhaseBuilding},
		{PhaseRework, PhaseCIPending},
		{PhaseRework, PhaseAbandoned},
		// decision-pending edges
		{PhaseDecisionPending, PhaseMergePending},
		{PhaseDecisionPending, PhaseRework},
		{PhaseDecisionPending, PhaseAbandoned},
		// merge-pending edges
		{PhaseMergePending, PhaseMerged},
		{PhaseMergePending, PhaseRework},
		{PhaseMergePending, PhaseConflicted},
		// conflicted edges
		{PhaseConflicted, PhaseRework},
		{PhaseConflicted, PhaseAbandoned},
	}

	for _, c := range legalCases {
		if err := delivery.ValidatePhaseTransition(c.from, c.to); err != nil {
			t.Errorf("ValidatePhaseTransition(%q, %q): unexpected error: %v", c.from, c.to, err)
		}
	}

	illegalCases := []struct {
		from, to string
	}{
		// ci-pending cannot go back to building
		{PhaseCIPending, PhaseBuilding},
		// terminal phases have no outgoing edges
		{PhaseMerged, PhaseBuilding},
		{PhaseAbandoned, PhaseRework},
		// unknown from-phase
		{"unknown-phase", PhaseBuilding},
		// review-pending cannot skip to merge-pending
		{PhaseReviewPending, PhaseMergePending},
	}

	for _, c := range illegalCases {
		if err := delivery.ValidatePhaseTransition(c.from, c.to); err == nil {
			t.Errorf("ValidatePhaseTransition(%q, %q): expected error, got nil", c.from, c.to)
		}
	}
}

// Verify the error from an illegal transition is not a plain nil-sentinel but an actual error value.
func TestValidatePhaseTransition_ErrorIsNonNil(t *testing.T) {
	err := delivery.ValidatePhaseTransition(PhaseMerged, PhaseBuilding)
	if err == nil {
		t.Fatal("expected error for merged→building, got nil")
	}
	// The error must not satisfy errors.Is(err, nil).
	if errors.Is(err, nil) {
		t.Fatal("error should not satisfy errors.Is(nil)")
	}
}

// Constant aliases for use within this test file.
const (
	PhaseBuilding        = delivery.PhaseBuilding
	PhaseCIPending       = delivery.PhaseCIPending
	PhaseReviewPending   = delivery.PhaseReviewPending
	PhaseRework          = delivery.PhaseRework
	PhaseDecisionPending = delivery.PhaseDecisionPending
	PhaseMergePending    = delivery.PhaseMergePending
	PhaseConflicted      = delivery.PhaseConflicted
	PhaseMerged          = delivery.PhaseMerged
	PhaseAbandoned       = delivery.PhaseAbandoned
)
