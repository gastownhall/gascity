package beads

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBeadUnmarshalJSON_TolerateNumericMetadata reproduces the bd-hook event
// shape seen in ~/.gc/supervisor.log that was spamming parse failures at
// ~100/s: a bead.updated payload whose metadata carries JSON numbers (and
// occasionally booleans/nulls) rather than strings. The custom UnmarshalJSON
// must coerce these to strings so the caching store can apply the event.
func TestBeadUnmarshalJSON_TolerateNumericMetadata(t *testing.T) {
	payload := []byte(`{
	    "id": "at-tmgg5",
	    "title": "gastown.mayor",
	    "status": "open",
	    "issue_type": "session",
	    "created_at": "2026-04-17T06:29:26Z",
	    "metadata": {
	        "agent_name": "gastown.mayor",
	        "continuation_epoch": 10,
	        "generation": 11,
	        "pool_managed": true,
	        "closed_at": null
	    }
	}`)
	var b Bead
	if err := json.Unmarshal(payload, &b); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"agent_name":         "gastown.mayor",
		"continuation_epoch": "10",
		"generation":         "11",
		"pool_managed":       "true",
		"closed_at":          "",
	}
	for k, v := range want {
		if got := b.Metadata[k]; got != v {
			t.Errorf("Metadata[%q] = %q, want %q", k, got, v)
		}
	}
	if b.ID != "at-tmgg5" {
		t.Errorf("ID = %q, want %q", b.ID, "at-tmgg5")
	}
	if b.Type != "session" {
		t.Errorf("Type = %q, want %q", b.Type, "session")
	}
}

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
