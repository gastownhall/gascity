package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
	"github.com/gastownhall/gascity/internal/doctor"
)

func mustCreateBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%s): %v", b.Title, err)
	}
	if len(b.Metadata) > 0 {
		if err := store.SetMetadataBatch(created.ID, b.Metadata); err != nil {
			t.Fatalf("SetMetadataBatch(%s): %v", b.Title, err)
		}
	}
	return created
}

func deliveryBead(phase string, enteredAgo time.Duration) beads.Bead {
	return beads.Bead{
		Title:  "delivery bead",
		Status: "open",
		Metadata: map[string]string{
			delivery.MetaKeyPhase:          phase,
			delivery.MetaKeyPhaseEnteredAt: time.Now().Add(-enteredAgo).UTC().Format(time.RFC3339),
		},
	}
}

func TestPRDeliveryCheck_emptyStore(t *testing.T) {
	store := beads.NewMemStore()
	check := &prDeliveryDoctorCheck{
		cityPath: "/city",
		newStore: func(string) (beads.Store, error) { return store, nil },
	}
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK; message: %q", result.Status, result.Message)
	}
	if result.Message != "no delivery beads in flight" {
		t.Fatalf("Message = %q, want %q", result.Message, "no delivery beads in flight")
	}
}

func TestPRDeliveryCheck_budgetBreach(t *testing.T) {
	store := beads.NewMemStore()
	// review-pending budget is 60m; bead is 65m old → stuck
	mustCreateBead(t, store, deliveryBead(delivery.PhaseReviewPending, 65*time.Minute))

	check := &prDeliveryDoctorCheck{
		cityPath: "/city",
		newStore: func(string) (beads.Store, error) { return store, nil },
	}
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning; message: %q", result.Status, result.Message)
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", result.Severity)
	}
	if !strings.Contains(result.Message, "1 stuck") {
		t.Fatalf("Message = %q, want to contain %q", result.Message, "1 stuck")
	}
}

func TestPRDeliveryCheck_renderExtras(t *testing.T) {
	store := beads.NewMemStore()
	stuckBead := mustCreateBead(t, store, deliveryBead(delivery.PhaseReviewPending, 65*time.Minute))
	escalatedB := deliveryBead(delivery.PhaseCIPending, 20*time.Minute)
	escalatedB.Metadata[delivery.MetaKeyWardenEscalated] = delivery.PhaseCIPending
	escalatedBead := mustCreateBead(t, store, escalatedB)

	check := &prDeliveryDoctorCheck{
		cityPath: "/city",
		newStore: func(string) (beads.Store, error) { return store, nil },
	}
	check.Run(&doctor.CheckContext{})

	var buf strings.Builder
	check.RenderExtras(&doctor.CheckContext{}, &buf)
	out := buf.String()

	if !strings.Contains(out, stuckBead.ID) {
		t.Errorf("RenderExtras output missing stuck bead ID %q\ngot:\n%s", stuckBead.ID, out)
	}
	if !strings.Contains(out, "STUCK") {
		t.Errorf("RenderExtras output missing STUCK flag\ngot:\n%s", out)
	}
	if !strings.Contains(out, escalatedBead.ID) {
		t.Errorf("RenderExtras output missing escalated bead ID %q\ngot:\n%s", escalatedBead.ID, out)
	}
	escalatedIdx := strings.Index(out, "Escalated:")
	if escalatedIdx < 0 {
		t.Fatalf("RenderExtras output missing \"Escalated:\" section\ngot:\n%s", out)
	}
	if !strings.Contains(out[escalatedIdx:], escalatedBead.ID) {
		t.Errorf("Escalated section missing bead %q\ngot:\n%s", escalatedBead.ID, out[escalatedIdx:])
	}
}

func TestPRDeliveryCheck_escalationDedupe(t *testing.T) {
	store := beads.NewMemStore()
	b := deliveryBead(delivery.PhaseReviewPending, 20*time.Minute)
	b.Metadata[delivery.MetaKeyWardenEscalated] = delivery.PhaseReviewPending
	escalatedBead := mustCreateBead(t, store, b)

	check := &prDeliveryDoctorCheck{
		cityPath: "/city",
		newStore: func(string) (beads.Store, error) { return store, nil },
	}
	check.Run(&doctor.CheckContext{})

	var buf strings.Builder
	check.RenderExtras(&doctor.CheckContext{}, &buf)
	out := buf.String()

	escalatedIdx := strings.Index(out, "Escalated:")
	if escalatedIdx < 0 {
		t.Fatalf("RenderExtras output missing \"Escalated:\" section\ngot:\n%s", out)
	}
	section := out[escalatedIdx:]
	if count := strings.Count(section, escalatedBead.ID); count != 1 {
		t.Errorf("Escalated section contains bead %q %d times, want exactly 1\ngot:\n%s", escalatedBead.ID, count, section)
	}
}
