package beads

import (
	"context"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// workFilterMatchesStatus mirrors the backing store's status selection for
// GetReadyWork spies: Statuses carries the whole open-class set with OR
// semantics, and singular Status remains honored for callers that still set
// it. Spies that key off filter.Status alone silently match nothing once
// Ready() moved to the single multi-status query.
func workFilterMatchesStatus(filter beadslib.WorkFilter, status beadslib.Status) bool {
	if filter.Status != "" {
		return status == filter.Status
	}
	for _, s := range filter.Statuses {
		if status == s {
			return true
		}
	}
	return false
}

// Ready must fetch all open-class backing statuses in ONE GetReadyWork call
// (WorkFilter.Statuses) instead of one call per status: each backing call
// re-pays the deferred-parents pre-query, the wisp arm, and the transaction
// round trips (sr-5rz: ~30-70ms per call on a live server-mode store, ~7x
// per Ready()).
func TestNativeDoltStoreReadySingleBackingCallWithAllStatuses(t *testing.T) {
	var calls []beadslib.WorkFilter
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			calls = append(calls, filter)
			return []*beadslib.Issue{
				{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-open" {
		t.Fatalf("Ready = %+v, want the one open bead", got)
	}
	if len(calls) != 1 {
		t.Fatalf("backing GetReadyWork calls = %d, want 1 (filters: %+v)", len(calls), calls)
	}
	if calls[0].Status != "" {
		t.Fatalf("singular Status = %q, want empty (Statuses must carry the set)", calls[0].Status)
	}
	if !reflect.DeepEqual(calls[0].Statuses, nativeDoltOpenReadyStatuses) {
		t.Fatalf("Statuses = %v, want %v", calls[0].Statuses, nativeDoltOpenReadyStatuses)
	}
	if !calls[0].IncludeEphemeral {
		t.Fatal("IncludeEphemeral must be set for TierBoth")
	}
}

func TestNativeDoltStoreReadyAssigneeSingleBackingCall(t *testing.T) {
	var calls []beadslib.WorkFilter
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			calls = append(calls, filter)
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(ReadyQuery{TierMode: TierBoth, Assignee: "probe-me", Limit: 3}); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("backing GetReadyWork calls = %d, want 1", len(calls))
	}
	if calls[0].Assignee == nil || *calls[0].Assignee != "probe-me" {
		t.Fatalf("Assignee filter = %v, want probe-me", calls[0].Assignee)
	}
	if calls[0].Limit != 0 {
		t.Fatalf("backing Limit = %d, want 0 (limit applies client-side after the gc post-filter)", calls[0].Limit)
	}
}
