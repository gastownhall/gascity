package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// eventLogLines renders seqs first..last as FileRecorder-shaped JSONL.
func eventLogLines(first, last uint64) []byte {
	return typedEventLogLines(first, last, func(uint64) string { return "e.t" })
}

// typedEventLogLines is eventLogLines with the event type chosen per seq.
func typedEventLogLines(first, last uint64, typeOf func(seq uint64) string) []byte {
	var b strings.Builder
	for s := first; s <= last; s++ {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"ts":"2026-09-27T09:00:00Z","actor":"a","subject":"s-%d"}`+"\n", s, typeOf(s), s)
	}
	return []byte(b.String())
}

func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeEventLogFile(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func archiveName(stamp string, first, last uint64) string {
	return fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, first, last)
}

// fileBackedEventState serves the city event list from a real FileRecorder
// over dir, the production provider.
func fileBackedEventState(t *testing.T, dir string) *fakeState {
	t.Helper()
	fr, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = fr.Close() })
	state := newFakeState(t)
	state.eventProv = fr
	return state
}

// TestEventListAfterRotationReadsOnlyNeededArchives reproduces the
// post-rotation blow-up: the active log holds fewer events than one page, so
// the list fallback decoded EVERY archive into memory to keep the last
// limit+1 rows. The two oldest archives here are not gzip at all, so a read
// that opens them fails; pages the newest archive can fill must not touch
// them.
func TestEventListAfterRotationReadsOnlyNeededArchives(t *testing.T) {
	dir := t.TempDir()
	notGzip := []byte("not a gzip stream: this archive must never be opened\n")
	writeEventLogFile(t, dir, archiveName("20260927T070000Z", 1, 1000), notGzip)
	writeEventLogFile(t, dir, archiveName("20260927T080000Z", 1001, 2000), notGzip)
	writeEventLogFile(t, dir, archiveName("20260927T090000Z", 2001, 3000), gzipped(t, eventLogLines(2001, 3000)))
	writeEventLogFile(t, dir, "events.jsonl", eventLogLines(3001, 3010))
	state := fileBackedEventState(t, dir)
	h := newTestCityHandler(t, state)

	cursor := ""
	for page, top := range []uint64{3010, 2510} {
		url := cityURL(state, "/events?limit=500")
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", page+1, rec.Code, rec.Body.String())
		}
		items, total, next := decodeEventList(t, rec)
		if len(items) != 500 || items[0].Seq != top || items[499].Seq != top-499 {
			t.Fatalf("page %d: %d items, want 500 from seq %d down", page+1, len(items), top)
		}
		if total != 3010 {
			t.Fatalf("page %d: total = %d, want 3010", page+1, total)
		}
		if next == "" {
			t.Fatalf("page %d: no cursor, want one (older history remains)", page+1)
		}
		cursor = next
	}
}

// TestEventListHistoryReadHonorsCanceledRequest: a request whose client is
// already gone must not be served an archive read. Before the fix the
// handler ignored the request context and ran the full fallback scan.
func TestEventListHistoryReadHonorsCanceledRequest(t *testing.T) {
	dir := t.TempDir()
	writeEventLogFile(t, dir, archiveName("20260927T090000Z", 1, 1000), gzipped(t, eventLogLines(1, 1000)))
	writeEventLogFile(t, dir, "events.jsonl", eventLogLines(1001, 1010))
	state := fileBackedEventState(t, dir)
	h := newTestCityHandler(t, state)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/events?limit=500"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("status 200 (%d bytes): a canceled request was served an archive read", rec.Body.Len())
	}
}

// TestEventListFileRecorderWalkAcrossRotations walks the whole history of a
// real FileRecorder that rotated three times: every seq exactly once, in
// strictly descending order, for an unfiltered and a filtered walk.
func TestEventListFileRecorderWalkAcrossRotations(t *testing.T) {
	dir := t.TempDir()
	state := fileBackedEventState(t, dir)
	fr := state.eventProv.(*events.FileRecorder)
	for r := 0; r < 3; r++ {
		for i := 0; i < 9; i++ {
			typ := "keep.me"
			if i%3 == 0 {
				typ = "drop.me"
			}
			fr.Record(events.Event{Type: typ, Actor: "a"})
		}
		res, err := fr.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate: %v", err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	fr.Record(events.Event{Type: "keep.me", Actor: "a"})
	latest, err := fr.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	all, err := fr.List(events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, state)

	for _, query := range []string{"", "&type=keep.me"} {
		want := map[uint64]bool{}
		for _, e := range all {
			if query == "" || e.Type == "keep.me" {
				want[e.Seq] = true
			}
		}
		seen := map[uint64]int{}
		last := latest + 1
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatalf("query %q: walk did not terminate", query)
			}
			url := cityURL(state, "/events?limit=4"+query)
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			rec := getList(t, h, url)
			if rec.Code != http.StatusOK {
				t.Fatalf("query %q page %d: status %d body %s", query, pages, rec.Code, rec.Body.String())
			}
			items, _, next := decodeEventList(t, rec)
			for _, e := range items {
				if e.Seq >= last {
					t.Fatalf("query %q: seq %d after %d, want strictly descending", query, e.Seq, last)
				}
				last = e.Seq
				seen[e.Seq]++
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if len(seen) != len(want) {
			t.Errorf("query %q: walk saw %d events, want %d", query, len(seen), len(want))
		}
		for seq := range want {
			if seen[seq] != 1 {
				t.Errorf("query %q: seq %d seen %d times, want 1", query, seq, seen[seq])
			}
		}
	}
}

// TestEventListFilteredTotalAfterRotation pins that the filtered Total does
// not change: when the page comes from the archives it is still the count of
// every match below the boundary, as the full scan reported it, also when
// more pages follow. The bounded read counts them without holding them.
func TestEventListFilteredTotalAfterRotation(t *testing.T) {
	dir := t.TempDir()
	rareEvery10 := func(seq uint64) string {
		if seq%10 == 0 {
			return "rare.t"
		}
		return "e.t"
	}
	// 100 rare events, seqs 10..1000, all archived; none in the active file.
	writeEventLogFile(t, dir, archiveName("20260927T090000Z", 1, 1000), gzipped(t, typedEventLogLines(1, 1000, rareEvery10)))
	writeEventLogFile(t, dir, "events.jsonl", eventLogLines(1001, 1010))
	state := fileBackedEventState(t, dir)
	h := newTestCityHandler(t, state)

	for _, tc := range []struct {
		limit, items, total int
		more                bool
	}{
		{limit: 500, items: 100, total: 100, more: false},
		{limit: 40, items: 40, total: 100, more: true},
	} {
		rec := getList(t, h, cityURL(state, fmt.Sprintf("/events?type=rare.t&limit=%d", tc.limit)))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit %d: status %d, body %s", tc.limit, rec.Code, rec.Body.String())
		}
		items, total, next := decodeEventList(t, rec)
		if len(items) != tc.items || total != tc.total || (next != "") != tc.more {
			t.Fatalf("limit %d: items=%d total=%d more=%v, want items=%d total=%d more=%v",
				tc.limit, len(items), total, next != "", tc.items, tc.total, tc.more)
		}
	}
}

// walkEventList follows next_cursor from the head until the list ends,
// calling between(page) after every page that has a next page. It returns how
// often each seq was seen and the seq at the top of the first page, and fails
// if the walk is not strictly descending.
func walkEventList(t *testing.T, h http.Handler, state *fakeState, limit int, between func(page int)) (map[uint64]int, uint64) {
	t.Helper()
	seen := map[uint64]int{}
	var top, last uint64
	cursor := ""
	for page := 1; ; page++ {
		if page > 100 {
			t.Fatal("walk did not terminate")
		}
		url := cityURL(state, fmt.Sprintf("/events?limit=%d", limit))
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := getList(t, h, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", page, rec.Code, rec.Body.String())
		}
		items, _, next := decodeEventList(t, rec)
		for _, e := range items {
			if top == 0 {
				top = e.Seq
			} else if e.Seq >= last {
				t.Fatalf("page %d: seq %d after %d, want strictly descending", page, e.Seq, last)
			}
			last = e.Seq
			seen[e.Seq]++
		}
		if next == "" {
			return seen, top
		}
		cursor = next
		if between != nil {
			between(page)
		}
	}
}

// Third-review finding R1: a client walks the city event list page by page
// with next_cursor while the log grows and rotates between pages (and the
// first page already needs the archive). The walk must return every event
// that existed when its first page was read exactly once, in strictly
// descending order, and no event newer than that page: next_cursor points
// down, so newer events are for the next read from the head. That read then
// returns every event exactly once.
func TestEventListPageWalkAcrossRotations(t *testing.T) {
	state := fileBackedEventState(t, t.TempDir())
	fr := state.eventProv.(*events.FileRecorder)
	record := func(n int) {
		for i := 0; i < n; i++ {
			fr.Record(events.Event{Type: "e.t", Actor: "a"})
		}
	}
	rotate := func() {
		res, err := fr.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate: %v", err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	record(20)
	rotate()
	record(2)
	h := newTestCityHandler(t, state)

	seen, top := walkEventList(t, h, state, 4, func(page int) {
		record(3)
		if page%2 == 1 {
			rotate()
		}
	})
	for seq := uint64(1); seq <= top; seq++ {
		if seen[seq] != 1 {
			t.Errorf("walk saw seq %d %d times, want 1 (it existed when the first page was read)", seq, seen[seq])
		}
	}
	for seq := range seen {
		if seq > top {
			t.Errorf("walk returned seq %d, newer than its first page (top %d)", seq, top)
		}
	}

	latest, err := fr.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	if latest <= top {
		t.Fatalf("control: no events were written during the walk (latest %d, top %d)", latest, top)
	}
	again, top2 := walkEventList(t, h, state, 4, nil)
	if top2 != latest {
		t.Fatalf("new walk starts at %d, want the head %d", top2, latest)
	}
	for seq := uint64(1); seq <= latest; seq++ {
		if again[seq] != 1 {
			t.Errorf("new walk saw seq %d %d times, want 1", seq, again[seq])
		}
	}
}
