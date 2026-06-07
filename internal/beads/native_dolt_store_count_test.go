package beads

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreCountStatusOpenMatchesListNormalization(t *testing.T) {
	var gotFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = filter
			return 7, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{Status: "open", AllowScan: true}, "message", "convoy")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 7 {
		t.Fatalf("Count = %d, want 7", got)
	}
	// List maps Status "open" to ExcludeStatus[closed,in_progress] because
	// mapBdStatus normalizes every other upstream status to "open". Count
	// has no post-hydration filter, so it must use the same mapping.
	if gotFilter.Status != nil {
		t.Fatalf("filter.Status = %v, want nil for open", *gotFilter.Status)
	}
	if !slices.Contains(gotFilter.ExcludeStatus, beadslib.StatusClosed) ||
		!slices.Contains(gotFilter.ExcludeStatus, beadslib.StatusInProgress) {
		t.Fatalf("filter.ExcludeStatus = %v, want closed and in_progress", gotFilter.ExcludeStatus)
	}
	if !slices.Contains(gotFilter.ExcludeTypes, beadslib.IssueType("message")) ||
		!slices.Contains(gotFilter.ExcludeTypes, beadslib.IssueType("convoy")) {
		t.Fatalf("filter.ExcludeTypes = %v, want message and convoy", gotFilter.ExcludeTypes)
	}
	if gotFilter.Ephemeral == nil || *gotFilter.Ephemeral {
		t.Fatalf("filter.Ephemeral = %v, want false for default tier", gotFilter.Ephemeral)
	}
	if gotFilter.Limit != 0 {
		t.Fatalf("filter.Limit = %d, want 0", gotFilter.Limit)
	}
}

func TestNativeDoltStoreCountInProgressUsesExactStatus(t *testing.T) {
	var gotFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = filter
			return 3, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{Status: "in_progress", AllowScan: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	if gotFilter.Status == nil || *gotFilter.Status != beadslib.StatusInProgress {
		t.Fatalf("filter.Status = %v, want in_progress", gotFilter.Status)
	}
}

func TestNativeDoltStoreCountTierBothLeavesEphemeralUnset(t *testing.T) {
	var gotFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = filter
			return 0, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Count(context.Background(), ListQuery{Status: "open", AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("Count: %v", err)
	}
	if gotFilter.Ephemeral != nil {
		t.Fatalf("filter.Ephemeral = %v, want nil for TierBoth", *gotFilter.Ephemeral)
	}
}

func TestNativeDoltStoreCountNonContractStatusReturnsZeroWithoutQuery(t *testing.T) {
	called := false
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, _ beadslib.IssueFilter) (int64, error) {
			called = true
			return 99, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	// mapBdStatus normalizes every upstream status to open/in_progress/closed,
	// so List(Status: "ready") always returns nothing for this store. Count
	// must mirror that without issuing a SQL query.
	got, err := store.Count(context.Background(), ListQuery{Status: "ready", AllowScan: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 0 {
		t.Fatalf("Count = %d, want 0 for non-contract status", got)
	}
	if called {
		t.Fatal("CountIssues called for non-contract status, want short-circuit to 0")
	}
}

func TestNativeDoltStoreCountRequiresFilterOrScan(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})

	_, err := store.Count(context.Background(), ListQuery{})
	if !errors.Is(err, ErrQueryRequiresScan) {
		t.Fatalf("Count error = %v, want ErrQueryRequiresScan", err)
	}
}

func TestNativeDoltStoreCountUnsupportedQueryShapes(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, _ beadslib.IssueFilter) (int64, error) {
			t.Fatal("CountIssues called for unsupported query shape")
			return 0, nil
		},
	})

	cases := map[string]ListQuery{
		"limit":         {AllowScan: true, Limit: 5},
		"assignees":     {AllowScan: true, Assignees: []string{"a", "b"}},
		"updatedBefore": {AllowScan: true, UpdatedBefore: time.Now()},
		"metadata":      {AllowScan: true, Metadata: map[string]string{"k": "v"}},
		"tierWisps":     {AllowScan: true, TierMode: TierWisps},
	}
	for name, query := range cases {
		if _, err := store.Count(context.Background(), query); !errors.Is(err, ErrCountUnsupported) {
			t.Errorf("%s: Count error = %v, want ErrCountUnsupported", name, err)
		}
	}
}

func TestNativeDoltStoreCountIncludeClosedCountsEverything(t *testing.T) {
	var gotFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			gotFilter = filter
			return 42, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 42 {
		t.Fatalf("Count = %d, want 42", got)
	}
	if gotFilter.Status != nil || len(gotFilter.ExcludeStatus) != 0 {
		t.Fatalf("filter status constraints = (%v, %v), want none for IncludeClosed", gotFilter.Status, gotFilter.ExcludeStatus)
	}
}

func TestNativeDoltStoreCountPropagatesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &nativeDoltStorageSpy{
		countIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) (int64, error) {
			return 0, ctx.Err()
		},
	}
	store := newNativeDoltStoreForTest(storage)

	_, err := store.Count(ctx, ListQuery{AllowScan: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Count error = %v, want context.Canceled", err)
	}
}
