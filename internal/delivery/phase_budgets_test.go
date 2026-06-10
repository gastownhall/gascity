package delivery_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

func TestPhaseEnteredAt_fallback(t *testing.T) {
	updatedAt := time.Now().Add(-30 * time.Minute)
	b := beads.Bead{
		UpdatedAt: updatedAt,
	}
	got := delivery.PhaseEnteredAt(b)
	if !got.Equal(updatedAt) {
		t.Fatalf("PhaseEnteredAt with no gc.phase_entered_at = %v, want UpdatedAt %v", got, updatedAt)
	}
}

func TestPhaseEnteredAt_parsesMetadata(t *testing.T) {
	ts := time.Now().Add(-45 * time.Minute).UTC().Truncate(time.Second)
	b := beads.Bead{
		UpdatedAt: time.Now().Add(-10 * time.Minute),
		Metadata:  map[string]string{delivery.MetaKeyPhaseEnteredAt: ts.Format(time.RFC3339)},
	}
	got := delivery.PhaseEnteredAt(b)
	if !got.Equal(ts) {
		t.Fatalf("PhaseEnteredAt with gc.phase_entered_at = %v, want %v", got, ts)
	}
}

func TestPhaseEnteredAt_fallsBackToCreatedAt(t *testing.T) {
	createdAt := time.Now().Add(-60 * time.Minute)
	b := beads.Bead{
		CreatedAt: createdAt,
	}
	got := delivery.PhaseEnteredAt(b)
	if !got.Equal(createdAt) {
		t.Fatalf("PhaseEnteredAt with no UpdatedAt and no metadata = %v, want CreatedAt %v", got, createdAt)
	}
}

// TestPhaseEnteredAt_wardenUnixFormat verifies that PhaseEnteredAt correctly parses
// the Unix-seconds format written by the delivery-warden (strconv.FormatInt(now.Unix(), 10)).
func TestPhaseEnteredAt_wardenUnixFormat(t *testing.T) {
	ts := time.Now().Add(-45 * time.Minute).Truncate(time.Second)
	b := beads.Bead{
		UpdatedAt: time.Now().Add(-10 * time.Minute),
		Metadata:  map[string]string{delivery.MetaKeyPhaseEnteredAt: strconv.FormatInt(ts.Unix(), 10)},
	}
	got := delivery.PhaseEnteredAt(b)
	if !got.Equal(ts) {
		t.Fatalf("PhaseEnteredAt with Unix-seconds format: got %v, want %v", got, ts)
	}
}

func TestPhaseBudgets_hasExpectedPhases(t *testing.T) {
	expectedPhases := []string{
		delivery.PhaseBuilding,
		delivery.PhaseCIPending,
		delivery.PhaseReviewPending,
		delivery.PhaseRework,
		delivery.PhaseDecisionPending,
		delivery.PhaseMergePending,
		delivery.PhaseConflicted,
	}
	for _, phase := range expectedPhases {
		if _, ok := delivery.PhaseBudgets[phase]; !ok {
			t.Errorf("PhaseBudgets missing phase %q", phase)
		}
	}
}
