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

func TestIsReadyCandidate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{
			name: "open task",
			bead: Bead{Status: "open", Type: "task"},
			want: true,
		},
		{
			name: "closed task",
			bead: Bead{Status: "closed", Type: "task"},
			want: false,
		},
		{
			name: "empty status is not normalized here",
			bead: Bead{Type: "task"},
			want: false,
		},
		{
			name: "ephemeral task",
			bead: Bead{Status: "open", Type: "task", Ephemeral: true},
			want: false,
		},
		{
			name: "no-history task remains durable ready work",
			bead: Bead{Status: "open", Type: "task", NoHistory: true},
			want: true,
		},
		{
			name: "excluded type",
			bead: Bead{Status: "open", Type: "message"},
			want: false,
		},
		{
			name: "nil defer",
			bead: Bead{Status: "open", Type: "task", DeferUntil: nil},
			want: true,
		},
		{
			name: "past defer",
			bead: Bead{Status: "open", Type: "task", DeferUntil: &past},
			want: true,
		},
		{
			name: "future defer",
			bead: Bead{Status: "open", Type: "task", DeferUntil: &future},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReadyCandidate(tt.bead, now); got != tt.want {
				t.Fatalf("IsReadyCandidate(%+v) = %v, want %v", tt.bead, got, tt.want)
			}
		})
	}
}

func TestTierWispsIncludesNoHistoryRows(t *testing.T) {
	items := []Bead{
		{ID: "issue", Title: "issue", Status: "open", Type: "task"},
		{ID: "no-history", Title: "no-history", Status: "open", Type: "task", NoHistory: true},
		{ID: "ephemeral", Title: "ephemeral", Status: "open", Type: "task", Ephemeral: true},
	}

	wisps := ApplyListQuery(items, ListQuery{TierMode: TierWisps, AllowScan: true})
	if got := idsOf(wisps); got != "no-history,ephemeral" {
		t.Fatalf("TierWisps IDs = %s, want no-history,ephemeral", got)
	}

	issues := ApplyListQuery(items, ListQuery{TierMode: TierIssues, AllowScan: true})
	if got := idsOf(issues); got != "issue,no-history" {
		t.Fatalf("TierIssues IDs = %s, want issue,no-history", got)
	}
}

func idsOf(items []Bead) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ","
		}
		out += item.ID
	}
	return out
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

func TestListQueryHasFilterIncludesAssignees(t *testing.T) {
	query := ListQuery{Assignees: []string{"rig/builder", "rig/validator"}}

	if !query.HasFilter() {
		t.Fatal("HasFilter() = false, want true for Assignees")
	}
}

func TestListQueryMatchesAnyAssignee(t *testing.T) {
	query := ListQuery{Assignees: []string{"rig/builder", "rig/validator"}}

	if !query.Matches(Bead{ID: "match", Assignee: "rig/validator"}) {
		t.Fatal("Matches() = false, want true for listed assignee")
	}
	if query.Matches(Bead{ID: "miss", Assignee: "rig/reviewer"}) {
		t.Fatal("Matches() = true, want false for unlisted assignee")
	}
}

func TestListQueryValidateRejectsAssigneeAndAssignees(t *testing.T) {
	query := ListQuery{
		Assignee:  "rig/builder",
		Assignees: []string{"rig/validator"},
	}

	err := query.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	if got, want := err.Error(), "ListQuery: Assignee and Assignees are mutually exclusive"; got != want {
		t.Fatalf("Validate() error = %q, want %q", got, want)
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
