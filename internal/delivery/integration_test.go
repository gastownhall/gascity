package delivery_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

// TestReviewApprove_BorrowsDecisionPendingNotClosed verifies AC4:
// Given a bead at review-pending, a reviewer APPROVE transitions to
// decision-pending, the bead stays open, and ValidateClosePrerequisites
// returns ErrNotInTerminalPhase.
func TestReviewApprove_BorrowsDecisionPendingNotClosed(t *testing.T) {
	store := beads.NewMemStore()

	// Set up a bead at review-pending.
	b, err := store.Create(beads.Bead{Title: "AC4 test bead", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetMetadata(b.ID, delivery.MetaKeyPhase, delivery.PhaseReviewPending); err != nil {
		t.Fatalf("set phase: %v", err)
	}

	// Reviewer APPROVE: transition to decision-pending.
	if err := delivery.SetPhase(store, b.ID, delivery.PhaseDecisionPending); err != nil {
		t.Fatalf("SetPhase review-pending→decision-pending: %v", err)
	}

	// Verify the bead is now in decision-pending.
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[delivery.MetaKeyPhase] != delivery.PhaseDecisionPending {
		t.Errorf("phase: got %q, want %q", got.Metadata[delivery.MetaKeyPhase], delivery.PhaseDecisionPending)
	}
	// AC4: bead Status must still be "open".
	if got.Status != "open" {
		t.Errorf("status: got %q, want %q", got.Status, "open")
	}
	// AC4: close guard must reject the bead.
	if err := delivery.ValidateClosePrerequisites(got); !errors.Is(err, delivery.ErrNotInTerminalPhase) {
		t.Errorf("ValidateClosePrerequisites: got %v, want ErrNotInTerminalPhase", err)
	}
}

// TestFindDeliveryBeadByPRURL_ReturnsOneBead verifies AC1 — the query
// primitive returns at most one open bead with gc.phase set for a given PR URL.
func TestFindDeliveryBeadByPRURL_ReturnsOneBead(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/gastownhall/gascity/pull/42"

	// Create a delivery bead with gc.pr_url and gc.phase.
	b, err := store.Create(beads.Bead{Title: "delivery bead"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetMetadataBatch(b.ID, map[string]string{
		delivery.MetaKeyPRURL: prURL,
		delivery.MetaKeyPhase: delivery.PhaseReviewPending,
	}); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	// Create a non-delivery bead with the same pr_url but no gc.phase.
	nonDelivery, err := store.Create(beads.Bead{Title: "non-delivery bead"})
	if err != nil {
		t.Fatalf("create non-delivery: %v", err)
	}
	if err := store.SetMetadata(nonDelivery.ID, delivery.MetaKeyPRURL, prURL); err != nil {
		t.Fatalf("set pr_url on non-delivery: %v", err)
	}

	// FindDeliveryBeadByPRURL must return the delivery bead (has gc.phase).
	found, ok, err := delivery.FindDeliveryBeadByPRURL(store, prURL)
	if err != nil {
		t.Fatalf("FindDeliveryBeadByPRURL: %v", err)
	}
	if !ok {
		t.Fatal("FindDeliveryBeadByPRURL: not found, want found")
	}
	if found.ID != b.ID {
		t.Errorf("found bead ID: got %q, want %q", found.ID, b.ID)
	}
	if found.Metadata[delivery.MetaKeyPhase] != delivery.PhaseReviewPending {
		t.Errorf("found bead phase: got %q, want %q", found.Metadata[delivery.MetaKeyPhase], delivery.PhaseReviewPending)
	}
}

// TestFindDeliveryBeadByPRURL_NotFound verifies that a missing bead returns (zero, false, nil).
func TestFindDeliveryBeadByPRURL_NotFound(t *testing.T) {
	store := beads.NewMemStore()

	found, ok, err := delivery.FindDeliveryBeadByPRURL(store, "https://github.com/not/real/pull/1")
	if err != nil {
		t.Fatalf("FindDeliveryBeadByPRURL: %v", err)
	}
	if ok {
		t.Errorf("expected not found, got bead ID %q", found.ID)
	}
}
