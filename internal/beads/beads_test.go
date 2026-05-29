package beads

import (
	"testing"
	"time"
)

var (
	_ Tx = (*BdStore)(nil)
	_ Tx = (*CachingStore)(nil)
	_ Tx = (*FileStore)(nil)
	_ Tx = (*MemStore)(nil)
)

func TestIsContainerType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"convoy", true},
		{"epic", false},
		{"task", false},
		{"message", false},
		{"", false},
		{"CONVOY", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsContainerType(tt.typ); got != tt.want {
			t.Errorf("IsContainerType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestIsMoleculeType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"molecule", true},
		{"wisp", true},
		{"task", false},
		{"convoy", false},
		{"step", false},
		{"", false},
		{"MOLECULE", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsMoleculeType(tt.typ); got != tt.want {
			t.Errorf("IsMoleculeType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestIsReadyExcludedType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"merge-request", true},
		{"gate", true},
		{"molecule", true},
		{"step", true},
		{"message", true},
		{"session", true},
		{"agent", true},
		{"role", true},
		{"rig", true},
		{"task", false},
		{"convoy", false},
		{"wisp", false},
		{"", false},
		{"MOLECULE", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsReadyExcludedType(tt.typ); got != tt.want {
			t.Errorf("IsReadyExcludedType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestListQueryCreatedBeforeFiltersBeforeLimit(t *testing.T) {
	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	items := []Bead{
		{ID: "newer-2", Title: "newer 2", Status: "closed", CreatedAt: base.Add(2 * time.Minute), Labels: []string{"order-run:digest"}},
		{ID: "newer-1", Title: "newer 1", Status: "closed", CreatedAt: base.Add(time.Minute), Labels: []string{"order-run:digest"}},
		{ID: "older-2", Title: "older 2", Status: "closed", CreatedAt: base.Add(-2 * time.Minute), Labels: []string{"order-run:digest"}},
		{ID: "older-1", Title: "older 1", Status: "closed", CreatedAt: base.Add(-time.Minute), Labels: []string{"order-run:digest"}},
	}

	got := ApplyListQuery(items, ListQuery{
		Label:         "order-run:digest",
		CreatedBefore: base,
		Limit:         1,
		IncludeClosed: true,
		Sort:          SortCreatedDesc,
	})

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	if got[0].ID != "older-1" {
		t.Fatalf("got[0].ID = %q, want older-1", got[0].ID)
	}
}

func TestListQueryHasFilterIncludesUpdatedBefore(t *testing.T) {
	query := ListQuery{UpdatedBefore: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}

	if !query.HasFilter() {
		t.Fatal("HasFilter() = false, want true for UpdatedBefore")
	}
}

func TestListQueryUpdatedBeforeMatchesReferenceTimestampBoundaries(t *testing.T) {
	cutoff := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{
			name: "updated before cutoff matches",
			bead: Bead{
				ID:        "updated-before",
				Status:    "open",
				CreatedAt: cutoff.Add(-time.Hour),
				UpdatedAt: cutoff.Add(-time.Nanosecond),
			},
			want: true,
		},
		{
			name: "updated equal cutoff is excluded",
			bead: Bead{
				ID:        "updated-equal",
				Status:    "open",
				CreatedAt: cutoff.Add(-time.Hour),
				UpdatedAt: cutoff,
			},
			want: false,
		},
		{
			name: "updated after cutoff is excluded even when created before",
			bead: Bead{
				ID:        "updated-after",
				Status:    "open",
				CreatedAt: cutoff.Add(-time.Hour),
				UpdatedAt: cutoff.Add(time.Nanosecond),
			},
			want: false,
		},
		{
			name: "zero updated falls back to created before cutoff",
			bead: Bead{
				ID:        "created-before",
				Status:    "open",
				CreatedAt: cutoff.Add(-time.Nanosecond),
			},
			want: true,
		},
		{
			name: "zero updated falls back to created equal cutoff",
			bead: Bead{
				ID:        "created-equal",
				Status:    "open",
				CreatedAt: cutoff,
			},
			want: false,
		},
		{
			name: "zero updated falls back to created after cutoff",
			bead: Bead{
				ID:        "created-after",
				Status:    "open",
				CreatedAt: cutoff.Add(time.Nanosecond),
			},
			want: false,
		},
	}

	query := ListQuery{UpdatedBefore: cutoff}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := query.Matches(tt.bead); got != tt.want {
				t.Fatalf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListQueryMatchesIgnoresUpdatedAtWhenUpdatedBeforeZero(t *testing.T) {
	bead := Bead{
		ID:        "future-update",
		Status:    "open",
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC),
	}

	if !(ListQuery{}).Matches(bead) {
		t.Fatal("Matches() = false, want true when UpdatedBefore is zero")
	}
}
