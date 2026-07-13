package beads

import (
	"context"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// A created-order ListQuery sort must push down to the backing search
// (IssueFilter.SortBy) and KEEP the caller's limit. The old behavior stripped
// the limit for any non-default sort, so a bounded history read (e.g. the
// order dispatcher's RecentRunsAll(2048)) materialized and hydrated the whole
// retained corpus — ~22k closed order-tracking wisps, 5-8s per call, twice a
// tick (sr-dp9o).
func TestNativeIssueFilterPushesCreatedSortAndKeepsLimit(t *testing.T) {
	cases := []struct {
		name         string
		sort         SortOrder
		wantSortBy   string
		wantSortDesc bool
	}{
		{"created desc", SortCreatedDesc, "created", false},
		{"created asc", SortCreatedAsc, "created", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := nativeIssueFilterFromListQuery(ListQuery{
				Label:         "order-tracking",
				Limit:         2048,
				IncludeClosed: true,
				Sort:          tc.sort,
			})
			if filter.Limit != 2048 {
				t.Errorf("Limit = %d, want 2048 (limit must survive a pushed-down sort)", filter.Limit)
			}
			if filter.SortBy != tc.wantSortBy {
				t.Errorf("SortBy = %q, want %q", filter.SortBy, tc.wantSortBy)
			}
			if filter.SortDesc != tc.wantSortDesc {
				t.Errorf("SortDesc = %v, want %v", filter.SortDesc, tc.wantSortDesc)
			}
		})
	}
}

// TierWisps still needs the gc-side post-filter over the full candidate set,
// so its limit strip is preserved.
func TestNativeIssueFilterStillStripsLimitForWispTier(t *testing.T) {
	filter := nativeIssueFilterFromListQuery(ListQuery{Limit: 10, TierMode: TierWisps, AllowScan: true})
	if filter.Limit != 0 {
		t.Errorf("Limit = %d, want 0 for TierWisps", filter.Limit)
	}
}

// Backing search results for a pushed-down sort arrive presorted; the
// client-side ApplyListQuery re-sort must keep them stable and the limit cut
// must match the server page.
func TestNativeDoltStoreListSortedLimitedPassesFilterToBacking(t *testing.T) {
	var got beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			got = f
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	if _, err := store.List(ListQuery{Label: "order-tracking", Limit: 2048, IncludeClosed: true, Sort: SortCreatedDesc}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.Limit != 2048 || got.SortBy != "created" || got.SortDesc {
		t.Fatalf("backing filter = {Limit:%d SortBy:%q SortDesc:%v}, want {2048 created false}", got.Limit, got.SortBy, got.SortDesc)
	}
}
