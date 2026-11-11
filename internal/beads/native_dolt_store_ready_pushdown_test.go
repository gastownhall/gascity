package beads

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The advisory narrowing predicates map onto the role's own: Unassigned and the
// metadata equality pairs travel in the request, and an unset query sends
// neither, so a plain Ready is byte-identical to what it was.
func TestNativeDoltStoreReadyPushesTheAdvisoryPredicatesDown(t *testing.T) {
	for _, tc := range []struct {
		name         string
		query        ReadyQuery
		wantUnassign bool
		wantMetadata map[string]string
	}{
		{name: "unset query asks for neither", query: ReadyQuery{}},
		{name: "unassigned travels", query: ReadyQuery{Unassigned: true}, wantUnassign: true},
		{
			name:         "metadata pairs travel",
			query:        ReadyQuery{MetadataEquals: map[string]string{"gc.routed_to": "pool/a", "gc.kind": "workflow"}},
			wantMetadata: map[string]string{"gc.routed_to": "pool/a", "gc.kind": "workflow"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured issueops.ReadyRequest
			storage := &nativeDoltReaderSpy{
				ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
					captured = req
					return issueops.IssuePage{}, nil
				},
			}
			store := newNativeDoltStoreForTest(storage)
			if _, err := store.Ready(tc.query); err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if captured.Unassigned != tc.wantUnassign {
				t.Fatalf("Unassigned = %v, want %v", captured.Unassigned, tc.wantUnassign)
			}
			if !reflect.DeepEqual(captured.MetadataFields, tc.wantMetadata) {
				t.Fatalf("MetadataFields = %v, want %v", captured.MetadataFields, tc.wantMetadata)
			}
		})
	}
}

// orderedReadyFixture is a ready role over a fixed population that honors the
// request's sort policy, limit and has-more verdict the way the real role does:
// ORDER BY priority, created_at, id (or created_at, id), then the page.
type orderedReadyFixture struct {
	issues   []*beadslib.Issue
	requests []issueops.ReadyRequest
}

func (f *orderedReadyFixture) ready(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	f.requests = append(f.requests, req)
	rows := slices.Clone(f.issues)
	switch req.Sort {
	case "priority":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Priority != rows[j].Priority {
				return rows[i].Priority < rows[j].Priority
			}
			if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].CreatedAt.Before(rows[j].CreatedAt)
			}
			return rows[i].ID < rows[j].ID
		})
	case "oldest":
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].CreatedAt.Before(rows[j].CreatedAt)
			}
			return rows[i].ID < rows[j].ID
		})
	}
	page := make([]*issueops.IssueWithCounts, 0, len(rows))
	for _, issue := range rows {
		page = append(page, &issueops.IssueWithCounts{Issue: issue})
	}
	if req.Limit != nil && *req.Limit > 0 && len(page) > *req.Limit {
		return issueops.IssuePage{Items: page[:*req.Limit], HasMore: true}, nil
	}
	return issueops.IssuePage{Items: page}, nil
}

// TestNativeDoltStoreReadyOrderedCutsInOrderAfterGcsOwnFilters pins the
// capability's contract: the first Limit rows, in order, of exactly the set an
// unbounded Ready returns. gc filters rows out of the role's page (here, the
// session-bookkeeping rows ready never offers), so a page that leaves the
// answer short while the role reports more behind it is read again, larger.
func TestNativeDoltStoreReadyOrderedCutsInOrderAfterGcsOwnFilters(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var issues []*beadslib.Issue
	for i := range 30 {
		issue := &beadslib.Issue{
			ID:        fmt.Sprintf("gc-%02d", i),
			Title:     "t",
			Status:    beadslib.StatusOpen,
			IssueType: beadslib.TypeTask,
			Priority:  i % 3,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// The first rows in priority order are bookkeeping rows gc drops.
		if i%3 == 0 && i < 24 {
			issue.Labels = []string{"gc:session"}
		}
		issues = append(issues, issue)
	}
	for _, tc := range []struct {
		order    ReadyOrder
		wantSort string
		sortWant func([]Bead)
	}{
		{order: ReadyOrderCanonical, wantSort: "priority", sortWant: SortBeadsReadyOrder},
		{order: ReadyOrderOldest, wantSort: "oldest", sortWant: func(b []Bead) { SortBeads(b, SortCreatedAsc) }},
	} {
		t.Run(tc.wantSort, func(t *testing.T) {
			fixture := &orderedReadyFixture{issues: issues}
			store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{ready: fixture.ready})

			got, ordered, err := store.ReadyOrdered(ReadyQuery{TierMode: TierBoth, Limit: 5}, tc.order)
			if err != nil {
				t.Fatalf("ReadyOrdered: %v", err)
			}
			if !ordered {
				t.Fatal("ordered = false; the native store cuts in order")
			}
			all, err := store.Ready(ReadyQuery{TierMode: TierBoth})
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			tc.sortWant(all)
			want := all[:5]
			if !reflect.DeepEqual(readyRowIDs(got), readyRowIDs(want)) {
				t.Fatalf("ReadyOrdered = %v, want the first five of the unbounded set %v", readyRowIDs(got), readyRowIDs(want))
			}
			bounded := fixture.requests[:len(fixture.requests)-1] // the last request is the unbounded Ready above
			if len(bounded) < 2 {
				t.Fatalf("role asked %d bounded pages, want a refill after gc's filters thinned the first", len(bounded))
			}
			for i, req := range bounded {
				if req.Sort != tc.wantSort {
					t.Fatalf("request %d sort = %q, want %q", i, req.Sort, tc.wantSort)
				}
				if req.Limit == nil || (i > 0 && *req.Limit <= *bounded[i-1].Limit) {
					t.Fatalf("request limits do not grow: %+v", bounded)
				}
			}
		})
	}
}

// An unbounded query is not a bounded read: it is answered by Ready, in Ready's
// own order, and reported unordered.
func TestNativeDoltStoreReadyOrderedWithoutALimitIsAPlainRead(t *testing.T) {
	fixture := &orderedReadyFixture{}
	store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{ready: fixture.ready})
	if _, ordered, err := store.ReadyOrdered(ReadyQuery{TierMode: TierBoth}, ReadyOrderCanonical); err != nil || ordered {
		t.Fatalf("ReadyOrdered(no limit) = ordered %v, err %v; want an unordered plain read", ordered, err)
	}
	if len(fixture.requests) != 1 || fixture.requests[0].Sort != "hybrid" {
		t.Fatalf("role asked %+v, want Ready's own hybrid read", fixture.requests)
	}
}

// readyRecordingStore records the ready queries a non-ordered store is asked.
type readyRecordingStore struct {
	*MemStore
	queries []ReadyQuery
}

func (s *readyRecordingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	s.queries = append(s.queries, q)
	return s.MemStore.Ready(q)
}

// A store that cannot cut in order must never be handed the bound: its own
// Limit cuts in ITS order (insertion order, for MemStore), and a bound cut in
// the wrong order is a wrong answer. The helper reads it whole instead.
func TestReadyOrderedReadsAStoreThatCannotCutInOrderWhole(t *testing.T) {
	store := &readyRecordingStore{MemStore: NewMemStore()}
	for i := range 4 {
		if _, err := store.Create(Bead{Title: fmt.Sprintf("b%d", i)}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	rows, ordered, err := ReadyOrdered(store, ReadyQuery{Limit: 2}, ReadyOrderCanonical)
	if err != nil {
		t.Fatalf("ReadyOrdered: %v", err)
	}
	if ordered || len(rows) != 4 {
		t.Fatalf("ReadyOrdered = %d rows, ordered %v; want the whole set, unordered", len(rows), ordered)
	}
	if len(store.queries) != 1 || store.queries[0].Limit != 0 {
		t.Fatalf("store asked %+v; the bound must not reach a store that cannot cut in order", store.queries)
	}
}

// orderedBacking is a backing store that cuts bounded reads in order.
type orderedBacking struct {
	*MemStore
	ordered int
}

func (s *orderedBacking) ReadyOrdered(query ReadyQuery, _ ReadyOrder) ([]Bead, bool, error) {
	s.ordered++
	limit := query.Limit
	query.Limit = 0
	rows, err := s.Ready(query)
	if err != nil {
		return nil, false, err
	}
	SortBeadsReadyOrder(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, true, nil
}

// A caching store answers a bounded read from its BACKING store, live, even
// when its own ready projection is warm: a bounded read is an authoritative
// read.
func TestCachingStoreReadyOrderedReadsTheBackingStoreLive(t *testing.T) {
	backing := &orderedBacking{MemStore: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "primed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// A row the warm cache has not seen: only a live read returns it.
	if _, err := backing.Create(Bead{Title: "written behind the cache"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows, ordered, err := ReadyOrdered(cache, ReadyQuery{Limit: 5}, ReadyOrderCanonical)
	if err != nil {
		t.Fatalf("ReadyOrdered: %v", err)
	}
	if !ordered || backing.ordered != 1 || len(rows) != 2 {
		t.Fatalf("ReadyOrdered = %d rows, ordered %v, backing asked %d times; want the backing's live ordered read of both rows", len(rows), ordered, backing.ordered)
	}
}

func readyRowIDs(rows []Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}
