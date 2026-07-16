package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// S2 of the keyset-cursor track: convoys, mail, and sessions speak the same
// contract the bead list shipped in S1 — opaque v1 tokens, typed 400 on
// invalid cursors, one (created_at DESC, id DESC) total order, and a
// truncated response ALWAYS carrying next_cursor (cursor-less requests
// previously truncated silently, making the remainder unfetchable).

func getList(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeGenericList(t *testing.T, rec *httptest.ResponseRecorder) (items []json.RawMessage, total int, next string) {
	t.Helper()
	var body struct {
		Items      []json.RawMessage `json:"items"`
		Total      int               `json:"total"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Items, body.Total, body.NextCursor
}

func itemID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal item id: %v", err)
	}
	return v.ID
}

// walkKeysetList drives a full cursor walk over a list endpoint and returns
// how many times each row id was seen plus the page count.
func walkKeysetList(t *testing.T, h http.Handler, base string, sep string, wantTotal int) map[string]int {
	t.Helper()
	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		url := base
		if cursor != "" {
			url += sep + "cursor=" + cursor
		}
		rec := getList(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d; body = %s", pages, rec.Code, rec.Body.String())
		}
		items, total, next := decodeGenericList(t, rec)
		if total != wantTotal {
			t.Fatalf("page %d: total = %d, want %d (full-set meaning, constant across a walk)", pages, total, wantTotal)
		}
		for _, it := range items {
			seen[itemID(t, it)]++
		}
		if pages++; pages > 20 {
			t.Fatal("walk did not terminate")
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return seen
}

func assertExactlyOnce(t *testing.T, seen map[string]int, want int) {
	t.Helper()
	if len(seen) != want {
		t.Fatalf("walk saw %d distinct rows, want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s seen %d times, want 1", id, n)
		}
	}
}

// --- Convoys ---

func TestConvoyListKeysetWalkNoSkipNoDup(t *testing.T) {
	state := newFakeMutatorState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)

	const n = 11
	for i := 0; i < n; i++ {
		if _, err := store.Create(beads.Bead{Title: "c", Type: "convoy"}); err != nil {
			t.Fatalf("create convoy: %v", err)
		}
	}
	seen := walkKeysetList(t, h, cityURL(state, "/convoys?limit=4"), "&", n)
	assertExactlyOnce(t, seen, n)
}

// TestConvoyListTruncationMintsCursor pins the audit fix: a cursor-less
// truncated response carries next_cursor instead of silently cutting.
func TestConvoyListTruncationMintsCursor(t *testing.T) {
	state := newFakeMutatorState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{Title: "c", Type: "convoy"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	rec := getList(t, h, cityURL(state, "/convoys?limit=2"))
	items, total, next := decodeGenericList(t, rec)
	if len(items) != 2 || total != 5 {
		t.Fatalf("items=%d total=%d, want 2/5", len(items), total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 keyset token on a truncated cursor-less page", next)
	}
}

func TestConvoyListInvalidCursorReturns400(t *testing.T) {
	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)
	rec := getList(t, h, cityURL(state, "/convoys?cursor=NTA")) // legacy offset token
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-cursor") {
		t.Fatalf("status = %d body = %s, want 400 invalid-cursor", rec.Code, rec.Body.String())
	}
}

// --- Mail ---

func TestMailListKeysetWalkNoSkipNoDup(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	h := newTestCityHandler(t, state)

	const n = 9
	for i := 0; i < n; i++ {
		if _, err := mp.Send("alice", "worker", "s", "b"); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	seen := walkKeysetList(t, h, cityURL(state, "/mail?limit=4"), "&", n)
	assertExactlyOnce(t, seen, n)
}

func TestMailListTruncationMintsCursor(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	h := newTestCityHandler(t, state)
	for i := 0; i < 5; i++ {
		if _, err := mp.Send("alice", "worker", "s", "b"); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	rec := getList(t, h, cityURL(state, "/mail?limit=2"))
	items, total, next := decodeGenericList(t, rec)
	if len(items) != 2 || total != 5 {
		t.Fatalf("items=%d total=%d, want 2/5", len(items), total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 keyset token on a truncated cursor-less page", next)
	}
}

func TestMailListInvalidCursorReturns400(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	rec := getList(t, h, cityURL(state, "/mail?cursor=garbage"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-cursor") {
		t.Fatalf("status = %d body = %s, want 400 invalid-cursor", rec.Code, rec.Body.String())
	}
}

// --- Sessions ---

func TestSessionListTruncationMintsCursor(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	createTestSession(t, fs.cityBeadStore, fs.sp, "S1")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S2")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S3")

	rec := getList(t, h, cityURL(fs, "/sessions?limit=2"))
	items, total, next := decodeGenericList(t, rec)
	if len(items) != 2 || total != 3 {
		t.Fatalf("items=%d total=%d, want 2/3", len(items), total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 keyset token on a truncated cursor-less page", next)
	}
	// Following it must complete the walk without skips or dups.
	rec2 := getList(t, h, cityURL(fs, "/sessions?limit=2&cursor=")+next)
	items2, total2, next2 := decodeGenericList(t, rec2)
	if len(items2) != 1 || total2 != 3 || next2 != "" {
		t.Fatalf("page2 items=%d total=%d next=%q, want 1/3/empty", len(items2), total2, next2)
	}
	first := map[string]bool{}
	for _, it := range items {
		first[itemID(t, it)] = true
	}
	if first[itemID(t, items2[0])] {
		t.Fatal("page 2 repeated a page 1 row")
	}
}

func TestSessionListInvalidCursorReturns400(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	rec := getList(t, h, cityURL(fs, "/sessions?cursor=NTA"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-cursor") {
		t.Fatalf("status = %d body = %s, want 400 invalid-cursor", rec.Code, rec.Body.String())
	}
}
