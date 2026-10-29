package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The earlier nonmatching row must not consume the requested row's limit.
// The fixture honors the role's ID predicate before its limit.
func TestNativeListIDsLimitAfterFilter(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsupported=%v", refuse), func(t *testing.T) {
			rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-other", 1), nativeIDListRow("gc-wanted", 2)}
			var sent []issueops.ListRequest
			store := newNativeDoltStoreForTest(nativeIDListFixture(rows, refuse, &sent))
			got, err := store.List(ListQuery{IDs: []string{"gc-wanted"}, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 1})
			if err != nil || len(got) != 1 || got[0].ID != "gc-wanted" {
				t.Fatalf("List = %v, %v; want gc-wanted", got, err)
			}
			wantRequests, wantUnkeyed := int64(1), int64(0)
			if refuse {
				wantRequests, wantUnkeyed = 2, 1
			}
			stats := listRequestStatsForTest(t, store)
			if stats.Requests != wantRequests || stats.Unkeyed != wantUnkeyed {
				t.Fatalf("stats = %+v; want requests=%d unkeyed=%d", stats, wantRequests, wantUnkeyed)
			}
			if sent[0].IDFilter != "gc-wanted" {
				t.Fatalf("IDFilter = %q", sent[0].IDFilter)
			}
		})
	}
}

func TestNativeListIDsBatchUnionOrderAndLimit(t *testing.T) {
	var ids []string
	var rows []*issueops.IssueWithCounts
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("gc-%03d", i)
		ids = append(ids, id)
		rows = append(rows, nativeIDListRow(id, int64(i)))
	}
	ids = append(ids, ids[0], ids[204])
	var sent []issueops.ListRequest
	store := newNativeDoltStoreForTest(nativeIDListFixture(rows, false, &sent))
	got, err := store.List(ListQuery{IDs: ids, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedDesc, Limit: 2, AllowBackingCreatedLimit: true})
	if err != nil || len(got) != 2 || got[0].ID != "gc-204" || got[1].ID != "gc-203" {
		t.Fatalf("List = %v, %v; want gc-204, gc-203", got, err)
	}
	if len(sent) != 3 {
		t.Fatalf("requests = %d, want 3", len(sent))
	}
	seen := map[string]bool{}
	for _, req := range sent {
		batch := strings.Split(req.IDFilter, ",")
		if len(batch) > 100 || req.Limit == nil || *req.Limit != 0 {
			t.Fatalf("unbounded batch required: %+v", req)
		}
		for _, id := range batch {
			if seen[id] {
				t.Fatalf("duplicate requested ID %q", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 205 {
		t.Fatalf("requested IDs = %d, want 205", len(seen))
	}
	if stats := listRequestStatsForTest(t, store); stats.Unkeyed != 0 || stats.Rows != 205 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestNativeListIDsUnrepresentableStaysExact(t *testing.T) {
	for _, id := range []string{"gc,a", " gc-a", "gc-a ", "", "\tgc-a"} {
		t.Run(fmt.Sprintf("id=%q", id), func(t *testing.T) {
			rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-other", 1), nativeIDListRow(id, 2), nativeIDListRow("gc-a", 3)}
			var sent []issueops.ListRequest
			store := newNativeDoltStoreForTest(nativeIDListFixture(rows, false, &sent))
			got, err := store.List(ListQuery{IDs: []string{id}, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 1})
			if err != nil || len(got) != 1 || got[0].ID != id {
				t.Fatalf("List = %v, %v; want exact %q", got, err, id)
			}
			if len(sent) != 1 || sent[0].IDFilter != "" || sent[0].Limit == nil || *sent[0].Limit != 0 {
				t.Fatalf("unsafe encoding/limit: %+v", sent)
			}
			if stats := listRequestStatsForTest(t, store); stats.Unkeyed != 1 {
				t.Fatalf("residual IDs counted keyed: %+v", stats)
			}
		})
	}
}

func TestNativeListIDsPreservesTiersClosedAndPredicates(t *testing.T) {
	rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-durable", 1), nativeIDListRow("gc-wisp", 2), nativeIDListRow("gc-other-owner", 3)}
	for _, row := range rows {
		row.Status = beadslib.StatusClosed
		row.Assignee = "worker"
		row.Labels = []string{"selected"}
	}
	rows[1].Ephemeral = true
	rows[2].Assignee = "other"
	for _, refuse := range []bool{false, true} {
		for _, tier := range []TierMode{TierBoth, TierIssues, TierWisps} {
			var sent []issueops.ListRequest
			store := newNativeDoltStoreForTest(nativeIDListFixture(rows, refuse, &sent))
			q := ListQuery{IDs: []string{"gc-durable", "gc-wisp", "gc-other-owner"}, Status: "closed", Assignee: "worker", Label: "selected", TierMode: tier, Sort: SortCreatedAsc, Limit: 1}
			got, err := store.List(q)
			want := "gc-durable"
			if tier == TierWisps {
				want = "gc-wisp"
			}
			if err != nil || len(got) != 1 || got[0].ID != want {
				t.Fatalf("refuse=%v tier=%v: %v, %v", refuse, tier, got, err)
			}
			for _, req := range sent {
				if req.Status != "closed" || req.Assignee != "worker" || !slices.Equal(req.Labels, []string{"selected"}) || !req.AllFlag || !req.IncludeInfra || !req.IncludeEphemeral {
					t.Fatalf("lost predicate/default lift: %+v", req)
				}
			}
		}
	}
}

func TestNativeListIDsMixedClosedTiers(t *testing.T) {
	rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-durable", 1), nativeIDListRow("gc-wisp", 2), nativeIDListRow("gc-no-history", 3)}
	for _, row := range rows {
		row.Status = beadslib.StatusClosed
	}
	rows[1].Ephemeral, rows[2].NoHistory = true, true
	ids := []string{"gc-durable", "gc-wisp", "gc-no-history"}
	for _, refuse := range []bool{false, true} {
		for _, tc := range []struct {
			tier TierMode
			want []string
		}{
			{TierBoth, ids}, {TierIssues, []string{"gc-durable", "gc-no-history"}}, {TierWisps, []string{"gc-wisp", "gc-no-history"}},
		} {
			var sent []issueops.ListRequest
			store := newNativeDoltStoreForTest(nativeIDListFixture(rows, refuse, &sent))
			got, err := store.List(ListQuery{IDs: ids, IncludeClosed: true, TierMode: tc.tier, Sort: SortCreatedAsc})
			var actual []string
			for _, bead := range got {
				actual = append(actual, bead.ID)
			}
			if err != nil || !slices.Equal(actual, tc.want) {
				t.Fatalf("refuse=%v tier=%v: %v, %v; want %v", refuse, tc.tier, actual, err, tc.want)
			}
		}
	}
}

func TestNativeListIDsAssigneeFanoutDeduplicatesDefaultOrder(t *testing.T) {
	rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-old", 1), nativeIDListRow("gc-new", 2)}
	var sent []issueops.ListRequest
	// Return both IDs for both assignees to model a row moving between reads.
	spy := nativeIDListFixture(rows, false, &sent)
	list := spy.list
	spy.list = func(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
		copy := req
		copy.Assignee = ""
		return list(ctx, copy)
	}
	store := newNativeDoltStoreForTest(spy)
	rows[0].Assignee, rows[1].Assignee = "worker-a", "worker-b"
	got, err := store.List(ListQuery{IDs: []string{"gc-old", "gc-new"}, Assignees: []string{"worker-a", "worker-b", "worker-a"}, IncludeClosed: true, TierMode: TierBoth, Limit: 2})
	if err != nil || len(got) != 2 || got[0].ID != "gc-new" || got[1].ID != "gc-old" {
		t.Fatalf("List = %v, %v", got, err)
	}
	if len(sent) != 2 {
		t.Fatalf("requests = %d, want 2", len(sent))
	}
}

func TestNativeListIDsUnsupportedBatchesReadFallbackOnce(t *testing.T) {
	var ids []string
	for i := 0; i < 205; i++ {
		ids = append(ids, fmt.Sprintf("gc-%03d", i))
	}
	var sent []issueops.ListRequest
	store := newNativeDoltStoreForTest(nativeIDListFixture([]*issueops.IssueWithCounts{nativeIDListRow("gc-204", 1)}, true, &sent))
	got, err := store.List(ListQuery{IDs: ids, TierMode: TierBoth})
	if err != nil || len(got) != 1 || got[0].ID != "gc-204" {
		t.Fatalf("List = %v, %v", got, err)
	}
	if len(sent) != 2 {
		t.Fatalf("requests = %d, want one refusal and one full fallback", len(sent))
	}
}

func TestNativeListIDsUnsupportedSinglePageKeepsBackendOrder(t *testing.T) {
	var ids []string
	for i := 0; i < 205; i++ {
		ids = append(ids, fmt.Sprintf("gc-%03d", i))
	}
	rows := []*issueops.IssueWithCounts{nativeIDListRow("gc-000", 1), nativeIDListRow("gc-204", 2)}
	rows[0].Priority = 0
	var sent []issueops.ListRequest
	store := newNativeDoltStoreForTest(nativeIDListFixture(rows, true, &sent))
	got, err := store.List(ListQuery{IDs: ids, TierMode: TierBoth, Limit: 1})
	if err != nil || len(got) != 1 || got[0].ID != "gc-204" {
		t.Fatalf("single fallback page reordered: %v, %v", got, err)
	}
}

func TestNativeListIDsDoesNotMaskBackendFailure(t *testing.T) {
	failure := errors.New("reader failure")
	store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
		return issueops.IssuePage{}, failure
	}})
	if _, err := store.List(ListQuery{IDs: []string{"gc-a"}}); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
}

func TestNativeListIDsPlanKeepsOtherPredicates(t *testing.T) {
	created := time.Unix(10, 0).UTC()
	q := ListQuery{IDs: []string{"gc-a", "gc-a", "gc-b"}, ParentID: "gc-parent", Metadata: map[string]string{"key": "value"}, CreatedBefore: created, Status: "closed", Type: "op", Assignee: "worker", Label: "selected", Sort: SortCreatedAsc, Limit: 1, TierMode: TierBoth}
	plan := NativeListPlan(q)
	if len(plan) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	req := plan[0]
	if req.IDFilter != "gc-a,gc-b" || req.ParentID != q.ParentID || req.MetadataFields["key"] != "value" || req.CreatedBefore == nil || !req.CreatedBefore.Equal(created) || req.Status != q.Status || req.IssueType != q.Type || req.Assignee != q.Assignee || !slices.Equal(req.Labels, []string{q.Label}) || req.SortBy != "created" || !req.Reverse || req.Limit == nil || *req.Limit != 0 {
		t.Fatalf("lost query predicate: %+v", req)
	}
}

func TestNativeListIDsTypeRefusalAndFailedFallback(t *testing.T) {
	refusedID := &beadslib.ErrUnsupported{Op: "List", Backend: "http"}
	failure := errors.New("fallback failure")
	for _, fail := range []bool{false, true} {
		calls := 0
		store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			calls++
			if req.IssueType != "" {
				return issueops.IssuePage{}, fmt.Errorf("invalid issue type: op")
			}
			if req.IDFilter != "" {
				return issueops.IssuePage{}, refusedID
			}
			if fail {
				return issueops.IssuePage{}, failure
			}
			row := nativeIDListRow("gc-a", 1)
			row.IssueType = beadslib.IssueType("op")
			return issueops.IssuePage{Items: []*issueops.IssueWithCounts{row}}, nil
		}})
		got, err := store.List(ListQuery{IDs: []string{"gc-a"}, Type: "op"})
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
		if fail {
			var unsupported *beadslib.ErrUnsupported
			if !errors.Is(err, failure) || !errors.As(err, &unsupported) {
				t.Fatalf("lost fallback errors: %v", err)
			}
		} else if err != nil || len(got) != 1 || got[0].ID != "gc-a" {
			t.Fatalf("List = %v, %v", got, err)
		}
	}
}

func nativeIDListRow(id string, created int64) *issueops.IssueWithCounts {
	row := nativeIssueRowForTest(id)
	row.CreatedAt = time.Unix(created, 0).UTC()
	row.Priority = 2
	return row
}

func nativeIDListFixture(rows []*issueops.IssueWithCounts, refuse bool, sent *[]issueops.ListRequest) *nativeDoltReaderSpy {
	return &nativeDoltReaderSpy{list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
		*sent = append(*sent, req)
		if refuse && req.IDFilter != "" {
			return issueops.IssuePage{}, &beadslib.ErrUnsupported{Op: "List", Backend: "http"}
		}
		var out []*issueops.IssueWithCounts
		var ids []string
		if req.IDFilter != "" {
			for _, id := range strings.Split(req.IDFilter, ",") {
				ids = append(ids, strings.TrimSpace(id))
			}
		}
		for _, row := range rows {
			if req.IDFilter != "" && !slices.Contains(ids, row.ID) {
				continue
			}
			if req.Assignee != "" && row.Assignee != req.Assignee {
				continue
			}
			if req.Status != "" && string(row.Status) != req.Status {
				continue
			}
			if len(req.Labels) > 0 && !slices.Contains(row.Labels, req.Labels[0]) {
				continue
			}
			out = append(out, row)
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].ID < out[j].ID
			}
			if req.SortBy == "created" && req.Reverse {
				return out[i].CreatedAt.Before(out[j].CreatedAt)
			}
			return out[i].CreatedAt.After(out[j].CreatedAt)
		})
		if req.Limit != nil && *req.Limit > 0 && len(out) > *req.Limit {
			out = out[:*req.Limit]
		}
		return issueops.IssuePage{Items: out}, nil
	}}
}
