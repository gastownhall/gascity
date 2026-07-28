package api

import (
	"context"
	"encoding/json"
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
