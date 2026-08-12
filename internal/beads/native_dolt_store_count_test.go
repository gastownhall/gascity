package beads

import (
	"context"
	"errors"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// NativeDoltStore implements Counter through the pinned beads library's
// CountIssues, which merges the wisps tier with the same semantics as
// SearchIssues (GH#4387) — the wisps undercount that previously pinned this
// store as Counter-less is fixed upstream. The supported-shape gate keeps
// the parity contract: only queries whose ListQuery→IssueFilter translation
// is exact are answered; everything else reports ErrCountUnsupported so
// callers fall back to the hydrating List.

func TestNativeDoltStoreImplementsCounter(t *testing.T) {
	var store any = &NativeDoltStore{}
	if _, ok := store.(Counter); !ok {
		t.Fatal("NativeDoltStore does not implement Counter; the closed-inclusive store-health count needs the hydration-free CountIssues path")
	}
}

func TestNativeDoltStoreCountDelegatesToCountIssues(t *testing.T) {
	var gotFilter *beadslib.IssueFilter
	searchCalls := 0
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, query string, filter beadslib.IssueFilter) (int64, error) {
			if query != "" {
				t.Fatalf("CountIssues search text = %q, want empty (same as List)", query)
			}
			gotFilter = &filter
			return 19342, nil
		},
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searchCalls++
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 19342 {
		t.Fatalf("Count = %d, want 19342", got)
	}
	if searchCalls != 0 {
		t.Fatalf("SearchIssues called %d times, want 0 (Count must not hydrate)", searchCalls)
	}
	if gotFilter == nil {
		t.Fatal("CountIssues was never called")
	}
	// The filter must be the same translation List uses: closed rows
	// included (no status exclusion) and the default tier's ephemeral
	// exclusion applied backend-side.
	if len(gotFilter.ExcludeStatus) != 0 {
		t.Fatalf("filter.ExcludeStatus = %v, want none for IncludeClosed", gotFilter.ExcludeStatus)
	}
	if gotFilter.Ephemeral == nil || *gotFilter.Ephemeral {
		t.Fatalf("filter.Ephemeral = %v, want &false for the default tier", gotFilter.Ephemeral)
	}
	if gotFilter.Limit != 0 {
		t.Fatalf("filter.Limit = %d, want 0", gotFilter.Limit)
	}
}

func TestNativeDoltStoreCountTranslatesExactFilterShapes(t *testing.T) {
	var gotFilter *beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = &filter
			return 7, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{
		Status:        "closed",
		Type:          "task",
		Assignee:      "gascity/builder",
		Label:         "sweep",
		AllowScan:     true,
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 7 {
		t.Fatalf("Count = %d, want 7", got)
	}
	if gotFilter == nil {
		t.Fatal("CountIssues was never called")
	}
	if gotFilter.Status == nil || *gotFilter.Status != beadslib.StatusClosed {
		t.Fatalf("filter.Status = %v, want closed", gotFilter.Status)
	}
	if gotFilter.IssueType == nil || *gotFilter.IssueType != beadslib.IssueType("task") {
		t.Fatalf("filter.IssueType = %v, want task", gotFilter.IssueType)
	}
	if gotFilter.Assignee == nil || *gotFilter.Assignee != "gascity/builder" {
		t.Fatalf("filter.Assignee = %v, want gascity/builder", gotFilter.Assignee)
	}
	if len(gotFilter.Labels) != 1 || gotFilter.Labels[0] != "sweep" {
		t.Fatalf("filter.Labels = %v, want [sweep]", gotFilter.Labels)
	}
}

// TestNativeDoltStoreCountBothTiersSupported asserts the TierBoth shape —
// what beadPolicyStore expands every TierIssues read into — is served by
// CountIssues with no ephemeral predicate. TierBoth has no narrowing on
// either side (no SQL tier filter, matchesTier always true), so the merged
// backend count is exact; gating it out silently cost policy-wrapped cities
// the store-health fast path.
func TestNativeDoltStoreCountBothTiersSupported(t *testing.T) {
	var gotFilter beadslib.IssueFilter
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = filter
			return 20641, nil
		},
	})
	n, err := store.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("Count(TierBoth) err = %v, want nil", err)
	}
	if n != 20641 {
		t.Fatalf("Count(TierBoth) = %d, want 20641", n)
	}
	if gotFilter.Ephemeral != nil {
		t.Fatalf("filter.Ephemeral = %v, want nil (no tier predicate for TierBoth)", *gotFilter.Ephemeral)
	}
}

// TestNativeDoltStoreCountUnsupportedShapes asserts Count reports
// ErrCountUnsupported for every shape List narrows Go-side, so callers fall
// back to the hydrating path instead of receiving a superset count.
func TestNativeDoltStoreCountUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name         string
		query        ListQuery
		excludeTypes []string
	}{
		{name: "excludeTypes", query: ListQuery{AllowScan: true, IncludeClosed: true}, excludeTypes: []string{"message"}},
		{name: "status open exclude-list translation", query: ListQuery{Status: "open", AllowScan: true}},
		{name: "wisps tier filtered Go-side", query: ListQuery{AllowScan: true, TierMode: TierWisps}},
		{name: "metadata re-filtered Go-side", query: ListQuery{AllowScan: true, Metadata: map[string]string{"gc.rig": "fc"}}},
		{name: "assignees not translated", query: ListQuery{AllowScan: true, Assignees: []string{"a", "b"}}},
		{name: "parentID Go-side projection", query: ListQuery{AllowScan: true, ParentID: "ga-parent"}},
		{name: "parentIDs not translated", query: ListQuery{AllowScan: true, ParentIDs: []string{"ga-parent"}}},
		{name: "createdBefore precision", query: ListQuery{AllowScan: true, CreatedBefore: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "updatedBefore not translated", query: ListQuery{AllowScan: true, UpdatedBefore: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "limit cap is List-side", query: ListQuery{AllowScan: true, Limit: 5}},
		{name: "seekAfter resolved Go-side", query: ListQuery{AllowScan: true, Sort: SortCreatedAsc, SeekAfter: &SeekBoundary{ID: "ga-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
				countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
					t.Error("CountIssues called for an unsupported shape")
					return 0, nil
				},
			})
			_, err := store.Count(context.Background(), tc.query, tc.excludeTypes...)
			if !errors.Is(err, ErrCountUnsupported) {
				t.Fatalf("Count err = %v, want ErrCountUnsupported", err)
			}
		})
	}
}

func TestNativeDoltStoreCountRequiresFilterOrScan(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	if _, err := store.Count(context.Background(), ListQuery{}); !errors.Is(err, ErrQueryRequiresScan) {
		t.Fatalf("Count err = %v, want ErrQueryRequiresScan", err)
	}
}

func TestNativeDoltStoreCountHonorsCallerCancellation(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			t.Error("CountIssues called after the caller's context was canceled")
			return 0, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Count(ctx, ListQuery{AllowScan: true, IncludeClosed: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Count err = %v, want context.Canceled", err)
	}
}

func TestNativeDoltStoreCountPropagatesBackendError(t *testing.T) {
	wantErr := errors.New("count blew up")
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			return 0, wantErr
		},
	})
	got, err := store.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if got != 0 {
		t.Errorf("Count = %d, want zero value on error", got)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Count err = %v, want %v", err, wantErr)
	}
}
