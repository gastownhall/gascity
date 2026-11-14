package beads

import (
	"testing"
	"time"
)

// TestNativeDoltCreatedFormFollowsTheStore pins CreatedForm's normalizations
// without a server: bd's default infra types land in the wisp tables unless
// the row is no-history, the step ref and updated_at are not stored, an
// unset priority reads as the default, and the parent stays the edge's.
func TestNativeDoltCreatedFormFollowsTheStore(t *testing.T) {
	store := &NativeDoltStore{} // no storage: the infra types are bd's default
	two := 2
	for _, tc := range []struct {
		in            Bead
		wantEphemeral bool
	}{
		{Bead{ID: "gcm-1", Title: "m", Type: "message"}, true},
		{Bead{ID: "gcm-2", Title: "m", Type: "message", NoHistory: true}, false},
		{Bead{ID: "gcg-3", Title: "t", Type: "task"}, false},
		{Bead{ID: "gcg-4", Title: "t", Type: "task", Ephemeral: true}, true},
	} {
		got, err := store.CreatedForm(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Ephemeral != tc.wantEphemeral {
			t.Fatalf("%s (%s, no_history=%v): ephemeral = %v, want %v", tc.in.ID, tc.in.Type, tc.in.NoHistory, got.Ephemeral, tc.wantEphemeral)
		}
	}
	created := time.Date(2026, 8, 1, 12, 0, 5, 528_000_000, time.UTC)
	got, err := store.CreatedForm(Bead{ID: "gcg-5", Title: "t", Type: "task", Ref: "step", ParentID: "gcg-0", Priority: &two, CreatedAt: created, DeferUntil: &created})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "" || !got.UpdatedAt.IsZero() || got.Priority != nil || got.ParentID != "gcg-0" {
		t.Fatalf("created form = %+v", got)
	}
	if !got.CreatedAt.Equal(created) || !got.DeferUntil.Equal(created) {
		t.Fatalf("created form times = %v, %v, want %v as given (compared at the second)", got.CreatedAt, got.DeferUntil, created)
	}
}
