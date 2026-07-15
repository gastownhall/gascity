package beads

import (
	"testing"
	"time"
)

func TestPriorityValueDefaultsMissingPriorityToP2(t *testing.T) {
	if got := PriorityValue(nil); got != DefaultPriority {
		t.Fatalf("PriorityValue(nil) = %d, want P%d", got, DefaultPriority)
	}
	p0 := 0
	if got := PriorityValue(&p0); got != 0 {
		t.Fatalf("PriorityValue(P0) = %d, want 0", got)
	}
}

func TestReadyLessOrdersPriorityThenFIFOThenID(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, p1 := 0, 1

	if !ReadyLess(
		Bead{ID: "new-p0", Priority: &p0, CreatedAt: base.Add(time.Hour)},
		Bead{ID: "old-p1", Priority: &p1, CreatedAt: base},
	) {
		t.Fatal("P0 must sort before P1 regardless of age")
	}
	if !ReadyLess(
		Bead{ID: "old", Priority: &p1, CreatedAt: base},
		Bead{ID: "new", Priority: &p1, CreatedAt: base.Add(time.Hour)},
	) {
		t.Fatal("older work must sort first within one priority band")
	}
	if !ReadyLess(
		Bead{ID: "a", Priority: &p1, CreatedAt: base},
		Bead{ID: "b", Priority: &p1, CreatedAt: base},
	) {
		t.Fatal("ID must provide the deterministic final tie-break")
	}
}
