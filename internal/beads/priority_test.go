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

func TestPriorityValueTreatsOutOfRangePriorityAsLeastUrgent(t *testing.T) {
	for _, priority := range []int{-1, 5, 99} {
		if got := PriorityValue(&priority); got != InvalidPriorityRank {
			t.Errorf("PriorityValue(%d) = %d, want invalid rank %d", priority, got, InvalidPriorityRank)
		}
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

func TestReadyLessSortsMissingCreatedAtAfterValidTime(t *testing.T) {
	p0 := 0
	valid := Bead{ID: "valid", Priority: &p0, CreatedAt: time.Unix(1, 0)}
	missing := Bead{ID: "missing", Priority: &p0}
	if !ReadyLess(valid, missing) || ReadyLess(missing, valid) {
		t.Fatal("a missing created_at must not outrank a valid timestamp")
	}
}

func TestReadyLessSortsInvalidPriorityAfterP4(t *testing.T) {
	p4, invalid := 4, -1
	created := time.Unix(1, 0)
	valid := Bead{ID: "valid", Priority: &p4, CreatedAt: created}
	malformed := Bead{ID: "malformed", Priority: &invalid, CreatedAt: created.Add(-time.Hour)}
	if !ReadyLess(valid, malformed) || ReadyLess(malformed, valid) {
		t.Fatal("an out-of-range priority must not outrank valid P4 work")
	}
}
