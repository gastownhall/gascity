package beads

import (
	"context"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The plan is what List sends: one request per plan entry, in plan order.
func TestNativeDoltStoreListSendsItsPlan(t *testing.T) {
	queries := []ListQuery{
		{Label: "gc:session"},
		{Assignee: "worker", Status: "open"},
		{Type: "message", Assignees: []string{"worker", "gc-1"}},
		{Metadata: map[string]string{"alias": "worker"}, IncludeClosed: true, TierMode: TierBoth},
		{Status: "in_progress", Assignees: []string{"worker"}},
	}
	for _, query := range queries {
		var sent []issueops.ListRequest
		storage := &nativeDoltReaderSpy{
			list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
				sent = append(sent, req)
				return issueops.IssuePage{}, nil
			},
		}
		store := newNativeDoltStoreForTest(storage)
		if _, err := store.List(query); err != nil {
			t.Fatalf("List(%+v): %v", query, err)
		}
		plan := NativeListPlan(query)
		if len(sent) != len(plan) {
			t.Fatalf("List(%+v) sent %d requests, plan has %d", query, len(sent), len(plan))
		}
		for i := range plan {
			if !nativeListRequestsEqual(sent[i], plan[i]) {
				t.Fatalf("List(%+v) request %d = %+v, plan says %+v", query, i, sent[i], plan[i])
			}
		}
	}
}

func TestNativeListRequestKeyed(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name string
		req  issueops.ListRequest
		want bool
	}{
		{"no predicate", nativeListReadRequest(), false},
		{"parent", issueops.ListRequest{ParentID: "gc-1"}, true},
		{"assignee", issueops.ListRequest{Assignee: "worker"}, true},
		{"label", issueops.ListRequest{Labels: []string{"gc:session"}}, true},
		{"metadata", issueops.ListRequest{MetadataFields: map[string]string{"alias": "worker"}}, true},
		{"created before", issueops.ListRequest{CreatedBefore: &created}, true},
		{"status", issueops.ListRequest{Status: "in_progress"}, true},
		{"type", issueops.ListRequest{IssueType: "session"}, true},
	} {
		if got := NativeListRequestKeyed(tc.req); got != tc.want {
			t.Errorf("%s: NativeListRequestKeyed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ListRequestStatsOf counts what List actually sent, so a budget test can tell a
// keyed read from a whole-ledger walk on a real store.
func TestNativeDoltStoreCountsListRequests(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{Items: []*issueops.IssueWithCounts{nativeIssueRowForTest("gc-1"), nativeIssueRowForTest("gc-2")}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	if _, err := store.List(ListQuery{Label: "gc:session"}); err != nil {
		t.Fatalf("keyed List: %v", err)
	}
	if _, err := store.List(ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("scan List: %v", err)
	}
	got, ok := ListRequestStatsOf(store)
	if !ok {
		t.Fatal("ListRequestStatsOf reports NativeDoltStore keeps no counts")
	}
	want := ListRequestStats{Lists: 2, Requests: 2, Unkeyed: 1, Rows: 4}
	if got != want {
		t.Fatalf("ListRequestStatsOf = %+v, want %+v", got, want)
	}
	if _, ok := ListRequestStatsOf(NewMemStore()); ok {
		t.Fatal("ListRequestStatsOf reports counts for a store that keeps none")
	}
}

func nativeListRequestsEqual(a, b issueops.ListRequest) bool {
	if a.Status != b.Status || a.IssueType != b.IssueType || a.Assignee != b.Assignee ||
		a.ParentID != b.ParentID || a.SortBy != b.SortBy || a.Reverse != b.Reverse ||
		a.AllFlag != b.AllFlag || a.IncludeEphemeral != b.IncludeEphemeral || a.IncludeInfra != b.IncludeInfra ||
		a.IncludeGates != b.IncludeGates || a.IncludeTemplates != b.IncludeTemplates {
		return false
	}
	if (a.Limit == nil) != (b.Limit == nil) || (a.Limit != nil && *a.Limit != *b.Limit) {
		return false
	}
	if len(a.Labels) != len(b.Labels) || len(a.MetadataFields) != len(b.MetadataFields) {
		return false
	}
	for i := range a.Labels {
		if a.Labels[i] != b.Labels[i] {
			return false
		}
	}
	for k, v := range a.MetadataFields {
		if b.MetadataFields[k] != v {
			return false
		}
	}
	return (a.CreatedBefore == nil) == (b.CreatedBefore == nil)
}

func nativeIssueRowForTest(id string) *issueops.IssueWithCounts {
	return &issueops.IssueWithCounts{Issue: &beadslib.Issue{ID: id, Title: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask}}
}

// listRequestStatsForTest reads store's backend listing counts, failing the
// test when the store keeps none.
func listRequestStatsForTest(t *testing.T, store Store) ListRequestStats {
	t.Helper()
	stats, ok := ListRequestStatsOf(store)
	if !ok {
		t.Fatalf("ListRequestStatsOf(%T) reports no counts", store)
	}
	return stats
}
