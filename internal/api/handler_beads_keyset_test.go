package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the keyset-cursor contract on GET /v0/beads: opaque v1
// tokens that resume at a (created_at, id) boundary, a typed 400 on any
// invalid cursor (including yesterday's base64-offset cursors), and the core
// no-skip/no-dup walk property under concurrent writes that offset cursors
// could not provide.

func getBeads(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBeadListInvalidCursorReturns400(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	for _, tc := range []struct {
		name   string
		cursor string
	}{
		{"garbage", "not-a-cursor"},
		{"legacy offset cursor", "NTA"}, // base64("50") — the pre-keyset format
		{"wrong kind (seq)", encodeKeysetCursor(keysetCursor{Kind: cursorKindSeq, Seq: 9})},
		{"v1 prefix, bad payload", "v1:!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getBeads(t, h, cityURL(state, "/beads?cursor=")+tc.cursor)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid-cursor") {
				t.Fatalf("body lacks the invalid-cursor code: %s", rec.Body.String())
			}
		})
	}
}

func decodeListBody(t *testing.T, rec *httptest.ResponseRecorder) (items []beads.Bead, total int, next string) {
	t.Helper()
	var body struct {
		Items      []beads.Bead `json:"items"`
		Total      int          `json:"total"`
		NextCursor string       `json:"next_cursor"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Items, body.Total, body.NextCursor
}

// TestBeadListKeysetWalkNoSkipNoDup: walking pages while new beads are
// created between requests sees every pre-walk bead exactly once. This is
// the property offset cursors break (a new row shifts every offset).
func TestBeadListKeysetWalkNoSkipNoDup(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	preWalk := map[string]bool{}
	for i := 0; i < 23; i++ {
		b, err := store.Create(beads.Bead{Title: "t", Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		preWalk[b.ID] = true
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		url := cityURL(state, "/beads?limit=7")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getBeads(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d; body = %s", pages, rec.Code, rec.Body.String())
		}
		items, _, next := decodeListBody(t, rec)
		for _, b := range items {
			seen[b.ID]++
		}
		pages++
		if pages > 20 {
			t.Fatal("walk did not terminate")
		}
		if next == "" {
			break
		}
		cursor = next
		// Concurrent write between pages: newer than everything already
		// walked, so it must not shift or duplicate any pre-walk row.
		if _, err := store.Create(beads.Bead{Title: "mid-walk", Status: "open"}); err != nil {
			t.Fatalf("mid-walk create: %v", err)
		}
	}

	for id := range preWalk {
		if seen[id] != 1 {
			t.Errorf("pre-walk bead %s seen %d times, want exactly 1", id, seen[id])
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("bead %s duplicated across pages (%d times)", id, n)
		}
	}
}

// TestBeadListKeysetTruncationMintsCursor: a cursor-less truncated first page
// still carries next_cursor (the #3208 guarantee), now as a v1 keyset token.
func TestBeadListKeysetTruncationMintsCursor(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{Title: "t", Status: "open"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	rec := getBeads(t, h, cityURL(state, "/beads?limit=2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	items, total, next := decodeListBody(t, rec)
	if len(items) != 2 || total != 5 {
		t.Fatalf("page = %d items / total %d, want 2 / 5", len(items), total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 keyset token", next)
	}
	// The token must decode to the boundary of the last row served.
	c, err := decodeKeysetCursor(next)
	if err != nil || c.Kind != cursorKindCreatedID {
		t.Fatalf("next_cursor decode = %+v, %v", c, err)
	}
	if c.ID != items[1].ID {
		t.Fatalf("cursor boundary ID = %s, want last row %s", c.ID, items[1].ID)
	}
}

// TestBeadListKeysetWalkAcrossZeroCreatedAtRows: degraded rows (NULL or
// unparseable created_at from a drifted store) carry zero timestamps and sort
// to the created-DESC tail. When a page cut lands among them, the server
// mints a zero-CreatedAt boundary — and must accept it back on the next
// request. A decoder that rejects its own token wedges the walk in a 400
// loop and makes the tail permanently unreachable.
func TestBeadListKeysetWalkAcrossZeroCreatedAtRows(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	seeded := []beads.Bead{
		{ID: "gc-1", Title: "ok", Status: "open", CreatedAt: ts},
		{ID: "gc-2", Title: "ok", Status: "open", CreatedAt: ts.Add(time.Second)},
		{ID: "gc-3", Title: "ok", Status: "open", CreatedAt: ts.Add(2 * time.Second)},
		// Degraded tail: zero CreatedAt.
		{ID: "gc-z1", Title: "degraded", Status: "open"},
		{ID: "gc-z2", Title: "degraded", Status: "open"},
		{ID: "gc-z3", Title: "degraded", Status: "open"},
		{ID: "gc-z4", Title: "degraded", Status: "open"},
	}
	state := newFakeState(t)
	state.stores["myrig"] = beads.NewMemStoreFrom(100, seeded, nil)
	h := newTestCityHandler(t, state)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		url := cityURL(state, "/beads?limit=5")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getBeads(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d (a 400 here means the server rejected its own minted cursor); body = %s",
				pages, rec.Code, rec.Body.String())
		}
		items, _, next := decodeListBody(t, rec)
		for _, b := range items {
			seen[b.ID]++
		}
		if pages++; pages > 5 {
			t.Fatal("walk did not terminate")
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != len(seeded) {
		t.Fatalf("walk saw %d distinct rows, want %d (tail unreachable?)", len(seen), len(seeded))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s seen %d times, want 1", id, n)
		}
	}
}

// TestBeadListKeysetLastPageOmitsCursor: the final page has no next_cursor.
func TestBeadListKeysetLastPageOmitsCursor(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{Title: "t", Status: "open"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	rec := getBeads(t, h, cityURL(state, "/beads?limit=10"))
	items, total, next := decodeListBody(t, rec)
	if len(items) != 3 || total != 3 || next != "" {
		t.Fatalf("items=%d total=%d next=%q, want 3/3/empty", len(items), total, next)
	}
}
