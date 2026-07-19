package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// S3 of the keyset-cursor track: the city event list speaks one order —
// seq DESC (newest first) on BOTH the cursor-less and cursor paths — with
// sq-kind keyset tokens. The old contract had a window flip: no cursor
// returned the newest-N while any cursor walked oldest-first from the head,
// so walking history coherently was impossible.

func seedEvents(t *testing.T, state *fakeState, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		state.eventProv.Record(events.Event{Type: "e.t", Actor: "a", Subject: fmt.Sprintf("s-%02d", i)})
	}
}

func decodeEventList(t *testing.T, rec *httptest.ResponseRecorder) (items []WireEvent, total int, next string) {
	t.Helper()
	var body struct {
		Items      []WireEvent `json:"items"`
		Total      int         `json:"total"`
		NextCursor string      `json:"next_cursor"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Items, body.Total, body.NextCursor
}

// TestEventListSeqDescBothPaths pins the window-flip fix: page 1 (no cursor)
// is the NEWEST events in seq DESC order, and following the cursor continues
// DESC into strictly older events — one coherent order end to end.
func TestEventListSeqDescBothPaths(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	seedEvents(t, state, 10)

	rec := getList(t, h, cityURL(state, "/events?limit=4"))
	items, _, next := decodeEventList(t, rec)
	if len(items) != 4 {
		t.Fatalf("page1 len = %d, want 4", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i].Seq >= items[i-1].Seq {
			t.Fatalf("page1 not seq DESC: %d then %d", items[i-1].Seq, items[i].Seq)
		}
	}
	if items[0].Seq != 10 {
		t.Fatalf("page1 must start at the newest event (seq 10), got %d", items[0].Seq)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("truncated page1 must mint a v1 cursor, got %q", next)
	}

	rec2 := getList(t, h, cityURL(state, "/events?limit=4&cursor=")+next)
	items2, _, _ := decodeEventList(t, rec2)
	if len(items2) != 4 {
		t.Fatalf("page2 len = %d, want 4", len(items2))
	}
	if items2[0].Seq != items[len(items)-1].Seq-1 {
		t.Fatalf("page2 must continue strictly below the boundary: got seq %d after boundary %d",
			items2[0].Seq, items[len(items)-1].Seq)
	}
	for i := 1; i < len(items2); i++ {
		if items2[i].Seq >= items2[i-1].Seq {
			t.Fatalf("page2 not seq DESC: %d then %d", items2[i-1].Seq, items2[i].Seq)
		}
	}
}

// TestEventListKeysetWalkNoSkipNoDup drives a full walk with concurrent
// appends between pages: every pre-walk event is seen exactly once, and the
// mid-walk appends (newer seqs, above the boundary) never shift the walk.
func TestEventListKeysetWalkNoSkipNoDup(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	const n = 11
	seedEvents(t, state, n)

	seen := map[uint64]int{}
	cursor := ""
	pages := 0
	for {
		url := cityURL(state, "/events?limit=4")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d body %s", pages, rec.Code, rec.Body.String())
		}
		items, _, next := decodeEventList(t, rec)
		for _, e := range items {
			seen[e.Seq]++
		}
		if pages++; pages > 10 {
			t.Fatal("walk did not terminate")
		}
		if next == "" {
			break
		}
		cursor = next
		// Concurrent append mid-walk: newer seq, sorts above the boundary.
		state.eventProv.Record(events.Event{Type: "e.t", Actor: "a", Subject: "mid"})
	}

	for seq := uint64(1); seq <= n; seq++ {
		if seen[seq] != 1 {
			t.Errorf("pre-walk event seq %d seen %d times, want exactly 1", seq, seen[seq])
		}
	}
	for seq, c := range seen {
		if c > 1 {
			t.Errorf("event seq %d duplicated (%d times)", seq, c)
		}
	}
}

func TestEventListInvalidCursorReturns400(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	for _, cursor := range []string{
		"NTA", // legacy offset token
		encodeKeysetCursor(keysetCursor{Kind: cursorKindCreatedID, ID: "x"}), // wrong kind (cb)
		// Crafted sq token with seq 0: the server never mints it (seqs start
		// at 1), and beforeSeq==0 means "first page" internally — accepting it
		// would hand a cursor-following client the first page again, forever.
		encodeKeysetCursor(keysetCursor{Kind: cursorKindSeq, Seq: 0}),
	} {
		rec := getList(t, h, cityURL(state, "/events?cursor=")+cursor)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-cursor") {
			t.Fatalf("cursor %q: status = %d body = %s, want 400 invalid-cursor", cursor, rec.Code, rec.Body.String())
		}
	}
}

// TestEventListFilteredKeysetWalk: type/actor filters compose with the seq
// boundary — the walk sees exactly the matching pre-walk events once each.
func TestEventListFilteredKeysetWalk(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	for i := 0; i < 12; i++ {
		typ := "keep.me"
		if i%3 == 0 {
			typ = "drop.me"
		}
		state.eventProv.Record(events.Event{Type: typ, Actor: "a"})
	}

	seen := map[uint64]int{}
	cursor := ""
	pages := 0
	for {
		url := cityURL(state, "/events?limit=3&type=keep.me")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		items, _, next := decodeEventList(t, rec)
		for _, e := range items {
			if e.Type != "keep.me" {
				t.Fatalf("filter leaked event type %q", e.Type)
			}
			seen[e.Seq]++
		}
		if pages++; pages > 10 {
			t.Fatal("walk did not terminate")
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 8 { // 12 events, every 3rd is drop.me -> 8 keep.me
		t.Fatalf("walk saw %d matching events, want 8", len(seen))
	}
	for seq, c := range seen {
		if c != 1 {
			t.Errorf("seq %d seen %d times", seq, c)
		}
	}
}

// TestEventListLastPageOmitsCursor: exhausting the log ends the walk cleanly.
func TestEventListLastPageOmitsCursor(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	seedEvents(t, state, 3)

	rec := getList(t, h, cityURL(state, "/events?limit=10"))
	items, _, next := decodeEventList(t, rec)
	if len(items) != 3 || next != "" {
		t.Fatalf("items=%d next=%q, want 3/empty", len(items), next)
	}
}

// archiveBlindTailProvider simulates the production FileRecorder split:
// ListTail is a backward scan of the active events.jsonl only (Seq >=
// activeFloor here), while List reads full history (archives + active).
type archiveBlindTailProvider struct {
	*events.Fake
	activeFloor uint64
}

func (p *archiveBlindTailProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
	all, err := p.List(filter)
	if err != nil {
		return nil, err
	}
	var active []events.Event
	for _, e := range all {
		if e.Seq >= p.activeFloor {
			active = append(active, e)
		}
	}
	if limit > 0 && len(active) > limit {
		active = active[len(active)-limit:]
	}
	return active, nil
}

// TestEventListWalkCrossesArchiveBoundary pins the red-team major: the
// ListTail fast path reads only the active log, so its result may be trusted
// only when it fills the whole limit+1 probe. A short active file (the
// normal state right after any rotation) must fall through to the
// archive-aware scan — otherwise the first page under-fills, mints no
// cursor, and the entire archived history is silently unreachable.
func TestEventListWalkCrossesArchiveBoundary(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &archiveBlindTailProvider{Fake: fake, activeFloor: 13}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ { // seqs 1..15; 1..12 "archived", 13..15 "active"
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	rec := getList(t, h, cityURL(state, "/events?limit=10"))
	items, total, next := decodeEventList(t, rec)
	if len(items) != 10 {
		t.Fatalf("page1 len = %d, want 10 (must cross the active/archive boundary, not stop at 3 active rows)", len(items))
	}
	if items[0].Seq != 15 || items[9].Seq != 6 {
		t.Fatalf("page1 range = [%d..%d], want [15..6]", items[0].Seq, items[9].Seq)
	}
	if total != 15 {
		t.Fatalf("total = %d, want 15 (LatestSeq)", total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("truncated page1 must mint a cursor, got %q", next)
	}

	// The rest of the walk drains the archived history exactly once.
	seen := map[uint64]int{}
	for _, e := range items {
		seen[e.Seq]++
	}
	cursor := next
	for pages := 0; cursor != ""; pages++ {
		if pages > 5 {
			t.Fatal("walk did not terminate")
		}
		rec := getList(t, h, cityURL(state, "/events?limit=10&cursor=")+cursor)
		items, _, nxt := decodeEventList(t, rec)
		for _, e := range items {
			seen[e.Seq]++
		}
		cursor = nxt
	}
	for seq := uint64(1); seq <= 15; seq++ {
		if seen[seq] != 1 {
			t.Errorf("seq %d seen %d times, want exactly 1", seq, seen[seq])
		}
	}
}

// rotationBlindProvider models the production FileRecorder during a rotation's
// asynchronous compression window. Three seq bands live on disk at once:
// ListTail scans only the ACTIVE file (Seq >= activeFloor); plain List reads
// canonical .gz archives + active but CANNOT see the in-flight .rotating-*
// segment [rotatingLow, activeFloor) (that is exactly what ReadFiltered
// misses); ListInFlight folds that segment back in (ReadFilteredWithInFlight).
// A descending keyset walk that fell through to List would serve rows above the
// segment, then jump below it, silently skipping the whole band — the fast path
// can't see it either — so the handler must use the in-flight-aware read.
type rotationBlindProvider struct {
	*events.Fake
	activeFloor uint64 // Seq >= activeFloor lives in the active file
	rotatingLow uint64 // [rotatingLow, activeFloor) lives ONLY in the in-flight file
}

// List models ReadFiltered: canonical archives + active, MISSING the in-flight
// rotating segment.
func (p *rotationBlindProvider) List(filter events.Filter) ([]events.Event, error) {
	all, err := p.Fake.List(filter)
	if err != nil {
		return nil, err
	}
	var visible []events.Event
	for _, e := range all {
		if e.Seq >= p.rotatingLow && e.Seq < p.activeFloor {
			continue // stranded in the .rotating-* file ReadFiltered can't read
		}
		visible = append(visible, e)
	}
	return visible, nil
}

// ListInFlight models ReadFilteredWithInFlight: the complete history including
// the in-flight rotating segment.
func (p *rotationBlindProvider) ListInFlight(filter events.Filter) ([]events.Event, error) {
	return p.Fake.List(filter)
}

// ListTail models the active-file-only backward scan.
func (p *rotationBlindProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
	all, err := p.Fake.List(filter)
	if err != nil {
		return nil, err
	}
	var active []events.Event
	for _, e := range all {
		if e.Seq >= p.activeFloor {
			active = append(active, e)
		}
	}
	if limit > 0 && len(active) > limit {
		active = active[len(active)-limit:]
	}
	return active, nil
}

// TestEventListWalkCrossesInFlightRotation pins the in-flight rotation gap that
// the archive-boundary test misses: during a rotation's compression window the
// just-rotated segment lives ONLY in the .rotating-* file, which neither the
// active-file tail fast path nor the plain archive-aware scan can see. A keyset
// walk that fell through to the plain scan would jump straight from the active
// band to the archived band, silently skipping the in-flight segment. The
// handler must route the fallback through the in-flight-aware read so the walk
// covers every seq exactly once.
func TestEventListWalkCrossesInFlightRotation(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	// seqs 1..15: 1..6 archived (.gz), 7..12 in-flight (.rotating-*), 13..15 active.
	state.eventProv = &rotationBlindProvider{Fake: fake, activeFloor: 13, rotatingLow: 7}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	seen := map[uint64]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("walk did not terminate")
		}
		url := cityURL(state, "/events?limit=10")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d body %s", pages, rec.Code, rec.Body.String())
		}
		items, _, next := decodeEventList(t, rec)
		for _, e := range items {
			seen[e.Seq]++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	for seq := uint64(1); seq <= 15; seq++ {
		if seen[seq] != 1 {
			t.Errorf("seq %d seen %d times, want exactly 1 (in-flight rotation band 7..12 must not be skipped)", seq, seen[seq])
		}
	}
}
