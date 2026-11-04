package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// boundedScanFakeProvider simulates a production FileRecorder that ALSO
// implements events.BoundedScanProvider, layered on top of the archive/active
// split already modeled by archiveBlindTailProvider in
// handler_events_keyset_test.go: ListTail only sees the "active" band
// (Seq >= activeFloor), and ListNewestBounded walks the rest of the history
// (Seq < activeFloor — the production contract is "never reads the active
// file") newest-to-oldest, stopping after budgetRows matches regardless of
// fetch. That mirrors a per-request byte budget that can land short of a full
// page independent of how many rows the caller asked for, including the
// coincidental case where the budget cutoff lands exactly at the start of
// history (not actually a truncation).
type boundedScanFakeProvider struct {
	*events.Fake
	activeFloor uint64
	budgetRows  int
}

func (p *boundedScanFakeProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
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

func (p *boundedScanFakeProvider) ListNewestBounded(_ context.Context, filter events.Filter, fetch int) ([]events.Event, bool, uint64, error) {
	older := filter
	if older.BeforeSeq == 0 || older.BeforeSeq > p.activeFloor {
		older.BeforeSeq = p.activeFloor
	}
	all, err := p.List(older) // ascending Seq, respects BeforeSeq
	if err != nil {
		return nil, false, 0, err
	}
	n := fetch
	budgetHit := p.budgetRows < n
	if budgetHit {
		n = p.budgetRows
	}
	if n > len(all) {
		n = len(all)
	}
	start := len(all) - n
	page := append([]events.Event(nil), all[start:]...)
	// budgetHit alone isn't enough: if the cutoff happens to land exactly at
	// the start of history, nothing was actually left out.
	truncated := budgetHit && start > 0
	var reachedSeq uint64
	if truncated {
		reachedSeq = page[0].Seq
	}
	return page, truncated, reachedSeq, nil
}

// TestEventListBoundedScanTruncatedSignalsScanTruncatedAndCursorFromReachedSeq
// pins the wire contract for a provider whose bounded archive scan stops at
// its byte budget before finding a full page: the response must carry
// scan_truncated=true and the next cursor must resume from the provider's
// reachedSeq, not from the oldest row actually returned — reusing evts[0].Seq
// here would re-scan the same budget-exhausted window forever.
func TestEventListBoundedScanTruncatedSignalsScanTruncatedAndCursorFromReachedSeq(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 4}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ { // seqs 1..15; 1..12 "archived", 13..15 "active"
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	rec := getList(t, h, cityURL(state, "/events?limit=10"))
	var body struct {
		Items         []WireEvent `json:"items"`
		Total         int         `json:"total"`
		NextCursor    string      `json:"next_cursor"`
		ScanTruncated bool        `json:"scan_truncated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !body.ScanTruncated {
		t.Fatal("scan_truncated = false, want true (budget exhausted before a full page)")
	}
	// tail=[13,14,15] (active band, always trusted) + bounded=[9..12] (budget
	// covers 4 of the 12 archived rows) = 7 events; well short of limit+1=11,
	// so hasMore's overfetch signal alone would have said "no more" — truncated
	// must override that.
	if len(body.Items) != 7 {
		t.Fatalf("items len = %d, want 7", len(body.Items))
	}
	if body.Items[0].Seq != 15 || body.Items[6].Seq != 9 {
		t.Fatalf("items range = [%d..%d], want [15..9]", body.Items[0].Seq, body.Items[6].Seq)
	}
	if body.Total != 15 {
		t.Fatalf("total = %d, want 15 (LatestSeq, filter is empty)", body.Total)
	}
	if !strings.HasPrefix(body.NextCursor, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 token", body.NextCursor)
	}
	c, err := decodeKeysetCursor(body.NextCursor)
	if err != nil {
		t.Fatalf("decodeKeysetCursor: %v", err)
	}
	if c.Kind != cursorKindSeq || c.Seq != 9 {
		t.Fatalf("cursor = %+v, want kind=%s seq=9 (this fake's reachedSeq happens to equal items[6].Seq; the handler still must read it from the provider's reachedSeq return value, not recompute it from evts[0].Seq — a real FileRecorder archive boundary can fall below the oldest matching row when a filter skips leading events in the last-opened archive)", c, cursorKindSeq)
	}
}

// TestEventListBoundedScanNotTruncatedMatchesUnboundedFallback pins that,
// when the bounded scan comfortably covers the requested page within budget,
// the response is identical to what the pre-existing unbounded
// (listWithInFlight) fallback produces for the same archive/active split —
// see TestEventListWalkCrossesArchiveBoundary. The BoundedScanProvider branch
// must be a transparent optimization, not a behavior change, on the
// non-truncated path.
func TestEventListBoundedScanNotTruncatedMatchesUnboundedFallback(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 100}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	rec := getList(t, h, cityURL(state, "/events?limit=10"))
	items, total, next := decodeEventList(t, rec)
	if len(items) != 10 {
		t.Fatalf("items len = %d, want 10", len(items))
	}
	if items[0].Seq != 15 || items[9].Seq != 6 {
		t.Fatalf("items range = [%d..%d], want [15..6]", items[0].Seq, items[9].Seq)
	}
	if total != 15 {
		t.Fatalf("total = %d, want 15", total)
	}
	if !strings.HasPrefix(next, "v1:") {
		t.Fatalf("next_cursor = %q, want a v1 token", next)
	}

	var withScan struct {
		ScanTruncated bool `json:"scan_truncated"`
	}
	rec2 := getList(t, h, cityURL(state, "/events?limit=10"))
	if err := json.NewDecoder(rec2.Body).Decode(&withScan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if withScan.ScanTruncated {
		t.Fatal("scan_truncated = true, want false (budget never exhausted)")
	}
}

// TestEventListBoundedScanFullWalkNoGapNoDup drives a full keyset walk
// against a provider whose bounded scan truncates on every page (a budget
// small enough to never finish an archive walk in one call) and asserts
// every seq is seen exactly once — pinning that the tail+bounded merge and
// the reachedSeq-anchored cursor never skip or duplicate a row across a page
// boundary, including the final page where the budget cutoff coincides with
// the start of history (truncated must correctly report false there so the
// walk terminates).
func TestEventListBoundedScanFullWalkNoGapNoDup(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 4}
	h := newTestCityHandler(t, state)
	const n = 15
	for i := 0; i < n; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	seen := map[uint64]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 6 {
			t.Fatal("walk did not terminate")
		}
		url := cityURL(state, "/events?limit=10")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		items, _, next := decodeEventList(t, rec)
		if len(items) == 0 && next == "" {
			t.Fatal("empty page with no cursor — walk stalled without terminating")
		}
		for _, e := range items {
			seen[e.Seq]++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	for seq := uint64(1); seq <= n; seq++ {
		if seen[seq] != 1 {
			t.Errorf("seq %d seen %d times, want exactly 1", seq, seen[seq])
		}
	}
	for seq, c := range seen {
		if c > 1 {
			t.Errorf("seq %d duplicated (%d times)", seq, c)
		}
	}
}

// rotatedBetweenReadsProvider models a rotation landing between ListTail and
// ListNewestBounded: the tail still sees the active band (Seq >= activeFloor),
// but by the time the bounded scan runs those same rows have been promoted
// into an archive it can read. Its ListNewestBounded honors only the caller's
// BeforeSeq, so without a fence below the tail it re-serves the tail's rows.
type rotatedBetweenReadsProvider struct {
	*events.Fake
	activeFloor uint64
}

func (p *rotatedBetweenReadsProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
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

func (p *rotatedBetweenReadsProvider) ListNewestBounded(_ context.Context, filter events.Filter, fetch int) ([]events.Event, bool, uint64, error) {
	all, err := p.List(filter) // ascending Seq, respects BeforeSeq only
	if err != nil {
		return nil, false, 0, err
	}
	if len(all) > fetch {
		all = all[len(all)-fetch:]
	}
	return all, false, 0, nil
}

// TestEventListBoundedScanFencedBelowTail pins that the bounded scan is
// fenced at the oldest tail row, so a rotation between the two reads cannot
// return any seq twice.
func TestEventListBoundedScanFencedBelowTail(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &rotatedBetweenReadsProvider{Fake: fake, activeFloor: 13}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	seen := map[uint64]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatal("walk did not terminate")
		}
		url := cityURL(state, "/events?limit=10")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		items, _, next := decodeEventList(t, getList(t, h, url))
		for _, e := range items {
			seen[e.Seq]++
		}
		if pages == 0 {
			if len(items) != 10 || items[0].Seq != 15 || items[9].Seq != 6 {
				t.Fatalf("first page = %d items, want 10 spanning [15..6]", len(items))
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for seq := uint64(1); seq <= 15; seq++ {
		if seen[seq] != 1 {
			t.Errorf("seq %d seen %d times, want exactly 1", seq, seen[seq])
		}
	}
}

// boundedScanPage is the part of a GET /events response the tests below
// assert on.
type boundedScanPage struct {
	Items         []WireEvent `json:"items"`
	Total         int         `json:"total"`
	NextCursor    string      `json:"next_cursor"`
	ScanTruncated bool        `json:"scan_truncated"`
}

// decodeBoundedScanPage decodes a 200 GET /events response.
func decodeBoundedScanPage(t *testing.T, rec *httptest.ResponseRecorder) boundedScanPage {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var page boundedScanPage
	if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return page
}

// requireSeqCursor fails the test unless token is an sq-kind keyset cursor at
// wantSeq.
func requireSeqCursor(t *testing.T, token string, wantSeq uint64) {
	t.Helper()
	c, err := decodeKeysetCursor(token)
	if err != nil {
		t.Fatalf("decodeKeysetCursor(%q): %v", token, err)
	}
	if c.Kind != cursorKindSeq || c.Seq != wantSeq {
		t.Fatalf("cursor = %+v, want kind=%s seq=%d", c, cursorKindSeq, wantSeq)
	}
}

// requireSeqRun fails the test unless items are exactly the seqs hi down to lo,
// each once, newest first.
func requireSeqRun(t *testing.T, items []WireEvent, hi, lo uint64) {
	t.Helper()
	for i, it := range items {
		if it.Seq != hi-uint64(i) {
			t.Fatalf("items[%d].Seq = %d, want %d (a descending run [%d..%d], each seq once)", i, it.Seq, hi-uint64(i), hi, lo)
		}
	}
	if got, want := uint64(len(items)), hi-lo+1; got != want {
		t.Fatalf("items = %d rows, want %d spanning [%d..%d]", got, want, hi, lo)
	}
}

// emptyTruncatedProvider models a selective filter whose bounded archive scan
// spends its whole byte budget before it finds a single match: ListNewestBounded
// returns no rows, truncated=true and a resume boundary. The embedded
// provider's tail finds no match for the filter either, so the page is empty and
// the provider's reachedSeq is the only thing that can carry the walk forward.
type emptyTruncatedProvider struct {
	*boundedScanFakeProvider
	reached uint64
}

func (p *emptyTruncatedProvider) ListNewestBounded(_ context.Context, _ events.Filter, _ int) ([]events.Event, bool, uint64, error) {
	return nil, true, p.reached, nil
}

// TestEventListBoundedScanTruncatedEmptyPageMintsReachedSeqCursor pins the one
// truncated shape where the provider's reachedSeq is the ONLY possible resume
// boundary: a selective filter whose scan exhausts its byte budget before it
// finds any match returns an empty page, so there is no oldest row to anchor a
// cursor on. The response must still say scan_truncated and carry a cursor at
// the provider's reachedSeq, or the walk strands every older match.
func TestEventListBoundedScanTruncatedEmptyPageMintsReachedSeqCursor(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &emptyTruncatedProvider{
		boundedScanFakeProvider: &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 4},
		reached:                 5,
	}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	page := decodeBoundedScanPage(t, getList(t, h, cityURL(state, "/events?type=e.rare&limit=10")))
	if len(page.Items) != 0 {
		t.Fatalf("items len = %d, want 0 (no event of type e.rare exists)", len(page.Items))
	}
	if !page.ScanTruncated {
		t.Fatal("scan_truncated = false, want true (the scan stopped at its budget before finding a match)")
	}
	requireSeqCursor(t, page.NextCursor, 5) // the provider's reachedSeq: an empty page has no oldest row to resume from
}

// reachedBelowOldestRowProvider reports the truncation boundary strictly below
// the oldest row the scan returned, the way a FileRecorder does when the last
// archive it opened begins with events the filter skips: the resume point is
// the next unopened archive's last seq + 1, not the oldest MATCHING seq.
type reachedBelowOldestRowProvider struct {
	*boundedScanFakeProvider
	gap uint64
}

func (p *reachedBelowOldestRowProvider) ListNewestBounded(ctx context.Context, filter events.Filter, fetch int) ([]events.Event, bool, uint64, error) {
	page, truncated, reachedSeq, err := p.boundedScanFakeProvider.ListNewestBounded(ctx, filter, fetch)
	if truncated {
		reachedSeq -= p.gap
	}
	return page, truncated, reachedSeq, err
}

// TestEventListBoundedScanTruncatedCursorIsReachedSeqBelowOldestRow pins that
// the truncated cursor is the provider's reachedSeq even when that boundary
// lies below the page's oldest returned row. Seqs 7 and 8 do not match the
// filter, so the oldest matching row the scan returns is 9, while the unopened
// archive below it ends at seq 6 (reachedSeq 7). A handler that anchored the
// cursor on the oldest returned row (9) would hand the next request a boundary
// above the one the scan actually reached.
func TestEventListBoundedScanTruncatedCursorIsReachedSeqBelowOldestRow(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &reachedBelowOldestRowProvider{
		boundedScanFakeProvider: &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 4},
		gap:                     2,
	}
	h := newTestCityHandler(t, state)
	for i := 1; i <= 15; i++ { // seqs 1..15; 7 and 8 are the rows the filter skips
		typ := "e.t"
		if i == 7 || i == 8 {
			typ = "e.skip"
		}
		fake.Record(events.Event{Type: typ, Actor: "a"})
	}

	page := decodeBoundedScanPage(t, getList(t, h, cityURL(state, "/events?type=e.t&limit=10")))
	if !page.ScanTruncated {
		t.Fatal("scan_truncated = false, want true (budget exhausted before a full page)")
	}
	requireSeqRun(t, page.Items, 15, 9) // tail [13..15] + the 4 matching archived rows the budget covered [9..12]
	requireSeqCursor(t, page.NextCursor, 7)
}

// TestEventListBoundedScanSkippedWhenSinceIsSet pins the documented limit of
// the bounded path (fetchEventPageAscending): a query with a lower bound, here
// since=, is answered by the full read, not by the byte-budgeted archive walk.
// The provider that truncates an unfiltered read after four archived rows
// therefore serves the whole page, an exact filtered total and no
// scan_truncated for the same history.
func TestEventListBoundedScanSkippedWhenSinceIsSet(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 4}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	page := decodeBoundedScanPage(t, getList(t, h, cityURL(state, "/events?since=1h&limit=10")))
	if page.ScanTruncated {
		t.Fatal("scan_truncated = true, want false (a since= query is not answered by the byte-budgeted bounded scan)")
	}
	requireSeqRun(t, page.Items, 15, 6)
	if page.Total != 15 {
		t.Fatalf("total = %d, want 15 (the full read's exact match count)", page.Total)
	}
	requireSeqCursor(t, page.NextCursor, 6) // the overfetched row signals more below
}

// TestEventListBoundedScanFencedBelowTailOnCursorPage pins that the fence
// below the tail also holds when the request carries a cursor. The cursor
// (seq 15) lies above the oldest tail row (13), and a rotation between the two
// reads makes the provider's bounded read re-serve the tail's rows unless the
// handler caps it at 13.
func TestEventListBoundedScanFencedBelowTailOnCursorPage(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &rotatedBetweenReadsProvider{Fake: fake, activeFloor: 13}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	cursor := encodeKeysetCursor(keysetCursor{Kind: cursorKindSeq, Seq: 15})
	page := decodeBoundedScanPage(t, getList(t, h, cityURL(state, "/events?limit=10&cursor="+cursor)))
	requireSeqRun(t, page.Items, 14, 5)
	requireSeqCursor(t, page.NextCursor, 5)
}

// TestEventListBoundedScanAsksOnlyForRowsTheTailDidNotSupply pins that the
// bounded read is asked for the rows the tail did not already supply (limit+1
// minus the tail), not for a whole page again. The budget here (9 rows) covers
// the 8 archived rows the page still needs but not the 11 an unreduced request
// would want: asking for 11 would make the provider report truncation and mint
// a cursor at its reachedSeq (4), below rows 4 and 5 that the trimmed page
// never served.
func TestEventListBoundedScanAsksOnlyForRowsTheTailDidNotSupply(t *testing.T) {
	state := newFakeState(t)
	fake := events.NewFake()
	state.eventProv = &boundedScanFakeProvider{Fake: fake, activeFloor: 13, budgetRows: 9}
	h := newTestCityHandler(t, state)
	for i := 0; i < 15; i++ {
		fake.Record(events.Event{Type: "e.t", Actor: "a"})
	}

	page := decodeBoundedScanPage(t, getList(t, h, cityURL(state, "/events?limit=10")))
	if page.ScanTruncated {
		t.Fatal("scan_truncated = true, want false (the 8 archived rows the page needs fit within the 9-row budget)")
	}
	requireSeqRun(t, page.Items, 15, 6)
	requireSeqCursor(t, page.NextCursor, 6) // the overfetched row (seq 5) signals more below
}
