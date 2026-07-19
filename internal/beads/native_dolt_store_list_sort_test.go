package beads

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// A created-order ListQuery sort pushes down to the backing search
// (IssueFilter.SortBy) so the caller's limit survives and the store pages
// instead of materializing + hydrating the whole retained corpus — ~22k closed
// order-tracking wisps for the dispatcher's RecentRunsAll read, 5-8s per call,
// twice a tick (sr-dp9o). The limit only survives for shapes the backing can
// resolve exactly: created-asc (its `id ASC` tie-break matches Gas City's
// canonical order) always keeps it, but created-desc keeps it only for aggregate
// callers that opt in (AllowBackingCreatedLimit), because the backing breaks
// created_at ties by `id ASC` while the canonical desc order breaks them by
// `id DESC` — a bounded desc read would otherwise drop the larger-id boundary
// ties an exact/paginated caller needs.
func TestNativeIssueFilterPushesCreatedSortAndKeepsLimit(t *testing.T) {
	cases := []struct {
		name         string
		sort         SortOrder
		approx       bool
		wantLimit    int
		wantSortBy   string
		wantSortDesc bool
	}{
		{"created asc keeps limit", SortCreatedAsc, false, 2048, "created", true},
		{"created desc without opt-in strips limit", SortCreatedDesc, false, 0, "created", false},
		{"created desc with opt-in keeps limit", SortCreatedDesc, true, 2048, "created", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := nativeIssueFilterFromListQuery(ListQuery{
				Label:                    "order-tracking",
				Limit:                    2048,
				IncludeClosed:            true,
				Sort:                     tc.sort,
				AllowBackingCreatedLimit: tc.approx,
			})
			if filter.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", filter.Limit, tc.wantLimit)
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

// nativeCreatedLimitPushdown is the single source of truth for whether the
// caller's limit is safe to forward to the backing search. Pin every gate: the
// Go-side-only residual filters (SeekAfter, UpdatedBefore, plural Assignees) and
// the wisp tier must strip the limit for every sort, and created-desc must strip
// it unless the caller opted into a bounded newest-by-created_at sample.
func TestNativeCreatedLimitPushdownGates(t *testing.T) {
	seek := &SeekBoundary{CreatedAt: time.Unix(1, 0).UTC(), ID: "gc-x"}
	cases := []struct {
		name string
		q    ListQuery
		want int
	}{
		{"asc pushes", ListQuery{Sort: SortCreatedAsc, Limit: 5}, 5},
		{"desc without opt-in strips", ListQuery{Sort: SortCreatedDesc, Limit: 5}, 0},
		{"desc with opt-in pushes", ListQuery{Sort: SortCreatedDesc, Limit: 5, AllowBackingCreatedLimit: true}, 5},
		{"desc opt-in but seek strips", ListQuery{Sort: SortCreatedDesc, Limit: 5, AllowBackingCreatedLimit: true, SeekAfter: seek}, 0},
		{"asc but seek strips", ListQuery{Sort: SortCreatedAsc, Limit: 5, SeekAfter: seek}, 0},
		{"desc opt-in but updated-before strips", ListQuery{Sort: SortCreatedDesc, Limit: 5, AllowBackingCreatedLimit: true, UpdatedBefore: time.Unix(2, 0).UTC()}, 0},
		{"desc opt-in but plural assignees strips", ListQuery{Sort: SortCreatedDesc, Limit: 5, AllowBackingCreatedLimit: true, Assignees: []string{"a", "b"}}, 0},
		{"wisp tier strips", ListQuery{Sort: SortCreatedDesc, Limit: 5, AllowBackingCreatedLimit: true, TierMode: TierWisps}, 0},
		{"default sort pushes", ListQuery{Sort: SortDefault, Limit: 5}, 5},
		{"zero limit stays zero", ListQuery{Sort: SortCreatedDesc, Limit: 0, AllowBackingCreatedLimit: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeCreatedLimitPushdown(tc.q); got != tc.want {
				t.Errorf("nativeCreatedLimitPushdown = %d, want %d", got, tc.want)
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
// must match the server page. Models the dispatcher's RecentRunsAll aggregate
// read, which opts into the bounded backing limit.
func TestNativeDoltStoreListSortedLimitedPassesFilterToBacking(t *testing.T) {
	var got beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			got = f
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	if _, err := store.List(ListQuery{Label: "order-tracking", Limit: 2048, IncludeClosed: true, Sort: SortCreatedDesc, AllowBackingCreatedLimit: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.Limit != 2048 || got.SortBy != "created" || got.SortDesc {
		t.Fatalf("backing filter = {Limit:%d SortBy:%q SortDesc:%v}, want {2048 created false}", got.Limit, got.SortBy, got.SortDesc)
	}
}

// A bounded created-desc read must return the exact canonical (created_at DESC,
// id DESC) top-N even when the boundary is a created_at tie. The backing breaks
// ties by id ASC, so pushing the limit would return the SMALLER ids (gc-01..03)
// and, after the client re-sort, drop the larger-id ties (gc-04..06) a cursor
// walk then skips. Without the opt-in the store fetches the full set and cuts
// the exact prefix client-side; this test fails against a bare id-ASC pushdown.
func TestNativeDoltStoreListCreatedDescExactTieBreakByDefault(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	var issues []*beadslib.Issue
	for i := 1; i <= 6; i++ {
		issues = append(issues, &beadslib.Issue{
			ID: fmt.Sprintf("gc-%02d", i), Title: "t", Status: beadslib.StatusOpen,
			IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: ts,
		})
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if f.Limit != 0 {
				t.Errorf("backing limit = %d pushed for a default created-desc read; the id-ASC boundary tie would drop canonical rows", f.Limit)
			}
			return backingSortLimitForTest(issues, f), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertBeadIDsForTest(t, got, "gc-06", "gc-05", "gc-04")
}

// An aggregate caller that opts in accepts the backing's bounded page: the fetch
// stays O(limit) and the created_at max is preserved, which is all a
// max-over-the-set reader needs. It does NOT get the canonical id tie-break —
// that is exactly why only aggregate callers set AllowBackingCreatedLimit.
func TestNativeDoltStoreListCreatedDescOptInBoundsTheFetch(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	var issues []*beadslib.Issue
	for i := 1; i <= 6; i++ {
		issues = append(issues, &beadslib.Issue{
			ID: fmt.Sprintf("gc-%02d", i), Title: "t", Status: beadslib.StatusOpen,
			IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: ts,
		})
	}
	var pushed int
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			pushed = f.Limit
			return backingSortLimitForTest(issues, f), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 3, AllowBackingCreatedLimit: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if pushed != 3 {
		t.Fatalf("backing limit = %d, want 3 (aggregate opt-in must bound the fetch)", pushed)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	for _, b := range got {
		if !b.CreatedAt.Equal(ts) {
			t.Fatalf("row %s CreatedAt = %v, want %v (aggregate max must be preserved)", b.ID, b.CreatedAt, ts)
		}
	}
}

// A max(seq) event-cursor reducer (the order dispatcher's Cursor/bdCursor) reads
// created-desc with a bounded Limit, but must NOT opt into the backing limit: it
// reduces over seq, a DIFFERENT column than the created_at sort key. seq is
// forward-only, so the max-seq run is the newest largest-id row — precisely the
// tie member the backing's id-ASC created-desc limit drops first when a
// same-second burst exceeds the bound. Without the opt-in the store fetches the
// full set and cuts the canonical (created_at DESC, id DESC) prefix, keeping the
// max-seq row; with the opt-in it silently regresses the cursor. Regression guard
// for the gastownhall/gascity#4214 attempt-2 finding.
func TestNativeDoltStoreListCreatedDescMaxSeqReducerKeepsHighSeqWithoutOptIn(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	// Six runs in one wall-clock second; seq is forward-only, so the max seq (6)
	// sits on the largest id gc-06. The bound (3) is smaller than the burst (6),
	// and gc-06 is NOT in the backing's id-ASC prefix (gc-01..03).
	var issues []*beadslib.Issue
	for i := 1; i <= 6; i++ {
		issues = append(issues, &beadslib.Issue{
			ID: fmt.Sprintf("gc-%02d", i), Title: "t", Status: beadslib.StatusOpen,
			IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: ts,
			Labels: []string{"order:digest", fmt.Sprintf("seq:%d", i)},
		})
	}
	spyFor := func() *nativeDoltStorageSpy {
		return &nativeDoltStorageSpy{
			searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				return backingSortLimitForTest(issues, f), nil
			},
		}
	}

	// Seq-reducer shape (no opt-in): the max-seq row survives the client-side cut.
	got, err := newNativeDoltStoreForTest(spyFor()).List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 3})
	if err != nil {
		t.Fatalf("List (no opt-in): %v", err)
	}
	assertBeadIDsForTest(t, got, "gc-06", "gc-05", "gc-04")
	if seq := maxSeqLabelForTest(got); seq != 6 {
		t.Fatalf("max seq without opt-in = %d, want 6 (the cursor must not drop the newest run)", seq)
	}

	// The same read WITH the opt-in shows the regression the cursor avoids: the
	// backing keeps its id-ASC prefix (gc-01..03) and drops the max-seq row gc-06;
	// the client re-sort then presents them canonically as gc-03, gc-02, gc-01.
	bugged, err := newNativeDoltStoreForTest(spyFor()).List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 3, AllowBackingCreatedLimit: true})
	if err != nil {
		t.Fatalf("List (opt-in): %v", err)
	}
	assertBeadIDsForTest(t, bugged, "gc-03", "gc-02", "gc-01")
	if seq := maxSeqLabelForTest(bugged); seq != 3 {
		t.Fatalf("max seq with opt-in = %d, want 3 (documents the dropped max-seq run)", seq)
	}
}

// maxSeqLabelForTest extracts the highest seq:<n> label across the beads,
// mirroring the order dispatcher's MaxSeqFromLabels reduction (which this lower
// layer cannot import). It lets a native-store test assert the row a max(seq)
// cursor reducer needs actually survives the read.
func maxSeqLabelForTest(got []Bead) uint64 {
	var maxSeq uint64
	for _, b := range got {
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "seq:") {
				if n, err := strconv.ParseUint(l[len("seq:"):], 10, 64); err == nil && n > maxSeq {
					maxSeq = n
				}
			}
		}
	}
	return maxSeq
}

// A seeked created-desc read must fetch the full candidate set and enforce the
// keyset boundary client-side, even when the caller opted into the aggregate
// bounded limit: a backing limit applied before the Go-side seek filter cuts the
// newest rows and starves the page (the page-2 truncation both reviewers
// flagged). Mirrors the sibling seek gates (exec, doltlite, bdstore).
func TestNativeDoltStoreListSeekAfterFetchesFullSetForCreatedDesc(t *testing.T) {
	t3 := time.Date(2026, 7, 11, 12, 0, 3, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 12, 0, 2, 0, time.UTC)
	t1 := time.Date(2026, 7, 11, 12, 0, 1, 0, time.UTC)
	issues := []*beadslib.Issue{
		{ID: "gc-a", Title: "t", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: t3},
		{ID: "gc-b", Title: "t", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: t2},
		{ID: "gc-c", Title: "t", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: t1},
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if f.Limit != 0 {
				t.Errorf("backing limit = %d pushed for a seeked read; a native limit truncates before the Go-side seek boundary", f.Limit)
			}
			return backingSortLimitForTest(issues, f), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{
		AllowScan:                true,
		Sort:                     SortCreatedDesc,
		Limit:                    1,
		AllowBackingCreatedLimit: true,
		SeekAfter:                &SeekBoundary{CreatedAt: t3, ID: "gc-a"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertBeadIDsForTest(t, got, "gc-b")
}

// backingSortLimitForTest reproduces the upstream backing search: order created
// sorts by (created_at <dir>, id ASC) — sqlbuild.OrderBy hardcodes the id ASC
// tie-break — then apply the row limit as a prefix cut.
func backingSortLimitForTest(all []*beadslib.Issue, f beadslib.IssueFilter) []*beadslib.Issue {
	out := make([]*beadslib.Issue, len(all))
	copy(out, all)
	if f.SortBy == "created" {
		desc := !f.SortDesc // SortDefs["created"] defaults DESC; SortDesc flips it
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if !a.CreatedAt.Equal(b.CreatedAt) {
				if desc {
					return a.CreatedAt.After(b.CreatedAt)
				}
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID < b.ID
		})
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	cloned := make([]*beadslib.Issue, len(out))
	for i, iss := range out {
		cloned[i] = cloneNativeIssueForTest(iss)
	}
	return cloned
}

func assertBeadIDsForTest(t *testing.T, got []Bead, want ...string) {
	t.Helper()
	gotIDs := make([]string, len(got))
	for i, b := range got {
		gotIDs[i] = b.ID
	}
	if len(gotIDs) != len(want) {
		t.Fatalf("got IDs %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("got IDs %v, want %v", gotIDs, want)
		}
	}
}
