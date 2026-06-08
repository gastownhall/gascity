package api

import (
	"context"
	"encoding/json"
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
	if store.maxListLim != limit {
		t.Errorf("max List limit = %d, want %d (page bound pushed into store)", store.maxListLim, limit)
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
