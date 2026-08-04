package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// boundedListBody mirrors the bead-list response fields this test inspects.
type boundedListBody struct {
	Items      []beads.Bead `json:"items"`
	Total      int          `json:"total"`
	NextCursor string       `json:"next_cursor"`
	Partial    bool         `json:"partial"`
}

// countingListStore is a Store + Counter fake. It records the largest List
// limit it was asked for so the test can prove the all=true path pushed the
// page bound down, and answers Count exactly from the underlying full list.
type countingListStore struct {
	beads.Store
	mu          sync.Mutex
	maxListLim  int
	countCalled bool
}

func (s *countingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	if q.Limit > s.maxListLim {
		s.maxListLim = q.Limit
	}
	s.mu.Unlock()
	return s.Store.List(q)
}

func (s *countingListStore) Count(_ context.Context, q beads.ListQuery, excludeTypes ...string) (int, error) {
	s.mu.Lock()
	s.countCalled = true
	s.mu.Unlock()
	q.Limit = 0
	rows, err := s.Store.List(q)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range rows {
		if !containsString(excludeTypes, b.Type) {
			n++
		}
	}
	return n, nil
}

func seedMoleculeStore(total int) ([]beads.Bead, *beads.MemStore) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	seed := make([]beads.Bead, 0, total)
	for i := 0; i < total; i++ {
		status := "open"
		if i%3 == 0 {
			status = "closed"
		}
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("gc-mol-%03d", i),
			Type:      "molecule",
			Status:    status,
			Title:     fmt.Sprintf("molecule %d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	return seed, beads.NewMemStoreFrom(total, seed, nil)
}

func fetchBoundedBeads(t *testing.T, fs *fakeState, query string) boundedListBody {
	t.Helper()
	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads")+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body boundedListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

// TestBeadListAllTrueBoundsCounterStore proves the all=true path pushes the
// page bound into a Counter-capable store and sources Total from Count, while
// returning the exact created_at-desc prefix the full scan would (#3253).
func TestBeadListAllTrueBoundsCounterStore(t *testing.T) {
	const total = 30
	const limit = 10
	fs := newFakeState(t)
	seed, mem := seedMoleculeStore(total)
	store := &countingListStore{Store: mem}
	fs.stores["myrig"] = store

	// Expected prefix: the same store's full created_at-desc molecule list.
	full, err := mem.List(beads.ListQuery{Type: "molecule", IncludeClosed: true, Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("seed full list: %v", err)
	}
	if len(full) != total {
		t.Fatalf("full list len = %d, want %d (seed=%d)", len(full), total, len(seed))
	}

	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", limit))

	if body.Total != total {
		t.Errorf("Total = %d, want %d (exact count, not bounded len)", body.Total, total)
	}
	if len(body.Items) != limit {
		t.Fatalf("len(Items) = %d, want %d", len(body.Items), limit)
	}
	for i := 0; i < limit; i++ {
		if body.Items[i].ID != full[i].ID {
			t.Fatalf("Items[%d] = %s, want %s (not a created-desc prefix)", i, body.Items[i].ID, full[i].ID)
		}
	}
	if body.NextCursor == "" {
		t.Errorf("NextCursor empty, want a cursor (Total %d > limit %d)", total, limit)
	}
	if !store.countCalled {
		t.Errorf("Count was not called; bounding did not engage")
	}
	// limit+1: the keyset bounded path overfetches one row as the has-more
	// signal (Counts are un-seeked totals and cannot tell). Still O(limit),
	// not O(history) — the property this test guards.
	if store.maxListLim != limit+1 {
		t.Errorf("max List limit = %d, want %d (page bound pushed into store)", store.maxListLim, limit+1)
	}
}

// TestBeadListAllTrueNoCursorWhenAllFit verifies a page that covers the whole
// result set carries no continuation cursor.
func TestBeadListAllTrueNoCursorWhenAllFit(t *testing.T) {
	const total = 8
	fs := newFakeState(t)
	_, mem := seedMoleculeStore(total)
	fs.stores["myrig"] = &countingListStore{Store: mem}

	body := fetchBoundedBeads(t, fs, "?type=molecule&all=true&limit=50")

	if body.Total != total {
		t.Errorf("Total = %d, want %d", body.Total, total)
	}
	if len(body.Items) != total {
		t.Errorf("len(Items) = %d, want %d", len(body.Items), total)
	}
	if body.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty (all rows fit)", body.NextCursor)
	}
}

// TestBeadListAllTrueFallsBackWithoutCounter verifies a store that cannot Count
// keeps the full-scan path: Total and the prefix stay correct without bounding.
func TestBeadListAllTrueFallsBackWithoutCounter(t *testing.T) {
	const total = 30
	const limit = 10
	fs := newFakeState(t)
	_, mem := seedMoleculeStore(total)
	// Plain MemStore is not a Counter, so the handler must not bound it.
	fs.stores["myrig"] = mem

	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", limit))

	if body.Total != total {
		t.Errorf("Total = %d, want %d (full-scan count preserved)", body.Total, total)
	}
	if len(body.Items) != limit {
		t.Errorf("len(Items) = %d, want %d", len(body.Items), limit)
	}
	if body.NextCursor == "" {
		t.Errorf("NextCursor empty, want a cursor on the fallback path too")
	}
}

// countOKListFailStore is a Store + Counter fake whose Count succeeds — so the
// bounded all=true path bakes its rows into the upfront Total — but whose List
// fails with a non-partial error, so those rows never reach the merged page.
// It reproduces the partial-rig-failure inflation the bounded path must avoid:
// a rig counted upfront yet dropped at List time (gascity#3253 review blocker).
type countOKListFailStore struct {
	beads.Store
	count   int
	listErr error
}

func (s *countOKListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.listErr
}

func (s *countOKListFailStore) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	return s.count, nil
}

// TestBeadListAllTrueBoundedTotalExcludesFailedRigList proves the bounded
// all=true path keeps Total consistent with the rows actually returned when a
// rig is counted upfront but its List then fails with a non-partial error: the
// failed rig's count must be dropped from Total (matching what the full-scan
// path yields under the same partial failure, where total == rows returned),
// and next_cursor must never point past reachable data. Without the fix Total
// is inflated by the failed rig's count and the cursor overshoots the data the
// client can ever fetch.
func TestBeadListAllTrueBoundedTotalExcludesFailedRigList(t *testing.T) {
	const good = 30
	const limit = 10
	fs := newFakeState(t)
	_, mem := seedMoleculeStore(good)
	fs.stores["myrig"] = &countingListStore{Store: mem}
	// "zrig" Counts 10 molecules but its List fails with a non-partial error,
	// so its 10 rows never reach the page. Pre-fix, those 10 stay baked into
	// the bounded Total (40) while only myrig's 30 rows are reachable.
	fs.stores["zrig"] = &countOKListFailStore{
		Store:   beads.NewMemStore(),
		count:   10,
		listErr: errors.New("connection reset after count"),
	}

	full, err := mem.List(beads.ListQuery{Type: "molecule", IncludeClosed: true, Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("seed full list: %v", err)
	}

	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", limit))

	if body.Total != good {
		t.Errorf("Total = %d, want %d (failed rig's count must be dropped, not inflated to %d)", body.Total, good, good+10)
	}
	if !body.Partial {
		t.Errorf("Partial = false, want true (zrig's List failed)")
	}
	if len(body.Items) != limit {
		t.Fatalf("len(Items) = %d, want %d", len(body.Items), limit)
	}
	for i := 0; i < limit; i++ {
		if body.Items[i].ID != full[i].ID {
			t.Fatalf("Items[%d] = %s, want %s (surviving rig's created-desc prefix)", i, body.Items[i].ID, full[i].ID)
		}
	}
	if body.NextCursor == "" {
		t.Errorf("NextCursor empty, want a cursor (reachable Total %d > limit %d)", good, limit)
	}

	// Reachability: walking the keyset cursor chain must visit exactly the 30
	// reachable rows and stop. A Total inflated by the failed rig used to make
	// the offset cursor overshoot; with keyset cursors the equivalent defect
	// would be a dangling next_cursor after the last reachable row.
	seen := map[string]bool{}
	cursor := body.NextCursor
	for _, item := range body.Items {
		seen[item.ID] = true
	}
	pages := 1
	for cursor != "" {
		next := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d&cursor=%s", limit, cursor))
		if next.Total != good {
			t.Errorf("page %d Total = %d, want %d", pages, next.Total, good)
		}
		for _, item := range next.Items {
			if seen[item.ID] {
				t.Errorf("bead %s duplicated across pages", item.ID)
			}
			seen[item.ID] = true
		}
		cursor = next.NextCursor
		pages++
		if pages > 10 {
			t.Fatal("cursor chain did not terminate (dangling next_cursor past reachable data)")
		}
	}
	if len(seen) != good {
		t.Errorf("walk reached %d distinct rows, want %d", len(seen), good)
	}
}

// countingPartialListStore is a Store + Counter fake whose List returns its rows
// AND a PartialResultError (a degraded read that surfaced rows but flagged a
// problem). It Counts exactly like countingListStore. It proves the bounded
// all=true path KEEPS a partial-result rig's count — the returned rows are
// reachable, so dropping the count would under-advertise readable data. This is
// the deliberate counterpart to the hard-List-failure case (gascity#3253).
type countingPartialListStore struct {
	*countingListStore
}

func (s *countingPartialListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.countingListStore.List(q)
	if err != nil {
		return rows, err
	}
	return rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped corrupt bead")}
}

// TestBeadListAllTrueBoundedPartialResultRigKeepsCount proves a rig that returns
// rows alongside a PartialResultError keeps its count in the bounded Total and
// has all its rows reachable — no data loss. This is distinct from a hard List
// failure (which drops the count): a partial read still yields reachable rows,
// so its count must stay (gascity#3253).
func TestBeadListAllTrueBoundedPartialResultRigKeepsCount(t *testing.T) {
	const good = 30
	const partial = 12
	fs := newFakeState(t)
	_, goodMem := seedMoleculeStore(good)
	fs.stores["myrig"] = &countingListStore{Store: goodMem}
	_, partMem := seedMoleculeStore(partial)
	fs.stores["zrig"] = &countingPartialListStore{countingListStore: &countingListStore{Store: partMem}}

	// Limit covers everything so the whole set must be reachable in one page.
	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", good+partial+10))

	if body.Total != good+partial {
		t.Errorf("Total = %d, want %d (partial-result rig's count must be kept, not dropped)", body.Total, good+partial)
	}
	if !body.Partial {
		t.Errorf("Partial = false, want true (zrig flagged a partial read)")
	}
	if len(body.Items) != good+partial {
		t.Errorf("len(Items) = %d, want %d (all rows incl. partial-rig survivors reachable)", len(body.Items), good+partial)
	}
	// A degraded (partial) page always carries a resume cursor: the server
	// cannot know whether the degraded rig withheld rows, so it hands the
	// client a boundary instead of silently ending the walk. Following it
	// here must terminate cleanly on an empty page.
	if body.NextCursor == "" {
		t.Fatalf("NextCursor empty, want a resume cursor on a partial page")
	}
	next := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d&cursor=%s", good+partial+10, body.NextCursor))
	if len(next.Items) != 0 || next.NextCursor != "" {
		t.Errorf("resume page = %d items, cursor %q; want empty page with no cursor (clean termination)", len(next.Items), next.NextCursor)
	}
}
