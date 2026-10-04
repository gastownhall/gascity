package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seqRange(first, last uint64) []uint64 {
	out := make([]uint64, 0, last-first+1)
	for s := first; s <= last; s++ {
		out = append(out, s)
	}
	return out
}

// cancelOnNthErr cancels its context on the Nth Err call, pinning
// cancellation to a point inside a read instead of racing a timer.
type cancelOnNthErr struct {
	context.Context
	cancel context.CancelFunc
	n      int
	calls  int
}

func (c *cancelOnNthErr) Err() error {
	c.calls++
	if c.calls == c.n {
		c.cancel()
	}
	return c.Context.Err()
}

func newCancelOnNthErr(n int) *cancelOnNthErr {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnNthErr{Context: ctx, cancel: cancel, n: n}
}

// historyStamp is the rotation instant of the n-th segment in these fixtures.
func historyStamp(n int) time.Time {
	return time.Date(2026, 9, 27, 9, n, 0, 0, time.UTC)
}

// writeHistoryArchive writes seqs first..last as a canonical .gz archive.
func writeHistoryArchive(t *testing.T, dir string, ts time.Time, first, last uint64) {
	t.Helper()
	src := filepath.Join(dir, "archive-src.jsonl")
	writeJSONLEvents(t, src, seqRange(first, last)...)
	var stderr bytes.Buffer
	if err := gzipAndArchive(src, filepath.Join(dir, formatArchiveBasename(ts, first, last)), &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v (%s)", err, stderr.String())
	}
}

// seedHistoryTailLayout lays out every source kind the history read merges:
// archives 1..40 and 41..80, the not-yet-removed rotating twin of the second,
// an in-flight rotating segment 81..120, and the active log 121..130.
func seedHistoryTailLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeHistoryArchive(t, dir, historyStamp(0), 1, 40)
	writeHistoryArchive(t, dir, historyStamp(5), 41, 80)
	writeJSONLEvents(t, filepath.Join(dir, formatRotatingBasename(historyStamp(5), 41, 80)), seqRange(41, 80)...)
	writeJSONLEvents(t, filepath.Join(dir, formatRotatingBasename(historyStamp(10), 81, 120)), seqRange(81, 120)...)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, path, seqRange(121, 130)...)
	return path
}

func historyTail(ctx context.Context, t *testing.T, path string, f Filter, limit int, count bool) ([]Event, int) {
	t.Helper()
	got, matched, err := ReadFilteredHistoryTail(ctx, path, f, limit, count)
	if err != nil {
		t.Fatalf("ReadFilteredHistoryTail(limit=%d, count=%v): %v", limit, count, err)
	}
	return got, matched
}

// The history read returns the last limit rows of the full in-flight-aware
// scan, for every page boundary, filter and limit, in both modes; with count
// it also returns the full scan's number of matches.
func TestReadFilteredHistoryTailMatchesFullScan(t *testing.T) {
	path := seedHistoryTailLayout(t)
	filters := map[string]Filter{
		"none":         {},
		"rare subject": {Subject: "s7"},
		"after seq":    {AfterSeq: 50},
		"all by type":  {Type: string(BeadCreated)},
		"no match":     {Type: "no.such.type"},
	}
	for name, base := range filters {
		for _, before := range []uint64{0, 131, 125, 121, 120, 100, 81, 80, 60, 41, 40, 8, 2, 1} {
			for _, limit := range []int{1, 5, 10, 11, 39, 40, 41, 51, 200} {
				f := base
				f.BeforeSeq = before
				full, err := ReadFilteredWithInFlight(path, f)
				if err != nil {
					t.Fatalf("%s: reference scan: %v", name, err)
				}
				want := full
				if len(want) > limit {
					want = want[len(want)-limit:]
				}
				for _, count := range []bool{false, true} {
					got, matched := historyTail(context.Background(), t, path, f, limit, count)
					if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
						t.Errorf("%s before=%d limit=%d count=%v: got %v, want %v", name, before, limit, count, seqsOf(got), seqsOf(want))
					}
					wantMatched := len(got)
					if count {
						wantMatched = len(full)
					}
					if matched != wantMatched {
						t.Errorf("%s before=%d limit=%d count=%v: matched %d, want %d", name, before, limit, count, matched, wantMatched)
					}
				}
			}
		}
	}
}

// Lines longer than one read chunk, in the active log (read backward) and in
// a segment (read forward), with a malformed line, a blank line, a CRLF line
// and a partial last line: the same rows as the full scan.
func TestReadFilteredHistoryTailMatchesFullScanWithLongLines(t *testing.T) {
	dir := t.TempDir()
	pad := strings.Repeat("p", 150<<10) // 150 KiB: more than two chunks
	line := func(seq int) string {
		return fmt.Sprintf(`{"seq":%d,"type":%q,"subject":"s%d","message":"%s"}`, seq, string(BeadCreated), seq, pad)
	}
	var seg, active strings.Builder
	for s := 1; s <= 20; s++ {
		seg.WriteString(line(s) + "\n")
	}
	for s := 21; s <= 40; s++ {
		switch s {
		case 25:
			active.WriteString("not json {\n\n")
		case 30:
			active.WriteString(line(s) + "\r\n")
			continue
		}
		active.WriteString(line(s) + "\n")
	}
	active.WriteString(`{"seq":41,"ty`)
	if err := os.WriteFile(filepath.Join(dir, formatRotatingBasename(historyStamp(0), 1, 20)), []byte(seg.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(active.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	full, err := ReadFilteredWithInFlight(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 5, 20, 39, 100} {
		want := full
		if len(want) > limit {
			want = want[len(want)-limit:]
		}
		for _, count := range []bool{false, true} {
			got, _ := historyTail(context.Background(), t, path, Filter{}, limit, count)
			if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
				t.Fatalf("limit=%d count=%v: got %v, want %v", limit, count, seqsOf(got), seqsOf(want))
			}
			for _, e := range got {
				if !strings.HasSuffix(e.Message, pad) {
					t.Fatalf("seq %d: message of %d bytes, want the whole %d", e.Seq, len(e.Message), len(pad))
				}
			}
		}
	}
}

// Without count, the read opens only the segments the page needs. The oldest
// archive is not gzip, so any read that opens it fails.
func TestReadFilteredHistoryTailOpensOnlyNeededSegments(t *testing.T) {
	path := seedHistoryTailLayout(t)
	oldest := filepath.Join(filepath.Dir(path), formatArchiveBasename(historyStamp(0), 1, 40))
	if err := os.WriteFile(oldest, []byte("not a gzip stream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFilteredWithInFlight(path, Filter{}); err == nil {
		t.Fatal("control: the full scan did not reach the unreadable oldest archive")
	}
	got, _ := historyTail(context.Background(), t, path, Filter{}, 90, false)
	if want := seqRange(41, 130); !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("got %v, want %v", seqsOf(got), want)
	}
	if _, _, err := ReadFilteredHistoryTail(context.Background(), path, Filter{}, 91, false); err == nil {
		t.Fatal("control: limit 91 did not reach the oldest archive")
	}
}

// rotateActive does one rotation by hand: the active log gets seqs
// first..last, is renamed to a rotating segment, and a new empty active log
// is opened.
func rotateActive(t *testing.T, path string, ts time.Time, first, last uint64) {
	t.Helper()
	writeJSONLEvents(t, path, seqRange(first, last)...)
	if err := os.Rename(path, filepath.Join(filepath.Dir(path), formatRotatingBasename(ts, first, last))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Fourth-review finding R4-A: a rotation at any point of the read must not
// make the page incomplete. The read returns the log as of the moment it
// opened the active log (archive 1..20 and active 21..30 here), whenever the
// rotations land, for both modes: no event that existed then is missing, and
// none written later is mixed in.
func TestReadFilteredHistoryTailReturnsTheLogAsOfTheOpen(t *testing.T) {
	type step func(t *testing.T, path string)
	rotate := func(ts int, first, last uint64) step {
		return func(t *testing.T, path string) { rotateActive(t, path, historyStamp(ts), first, last) }
	}
	appendNew := func(first, last uint64) step {
		return func(t *testing.T, path string) { writeJSONLEvents(t, path, seqRange(first, last)...) }
	}
	for name, tc := range map[string]struct {
		beforeList, afterList []step
	}{
		"no rotation":                        {},
		"rotation before the listing":        {beforeList: []step{rotate(5, 21, 35)}},
		"rotation after the listing":         {afterList: []step{rotate(5, 21, 35)}},
		"two rotations before the listing":   {beforeList: []step{rotate(5, 21, 35), rotate(6, 36, 40)}},
		"rotation and new events after it":   {afterList: []step{rotate(5, 21, 30), appendNew(31, 45)}},
		"rotations before and after listing": {beforeList: []step{rotate(5, 21, 35)}, afterList: []step{rotate(6, 36, 40)}},
	} {
		for _, count := range []bool{false, true} {
			for _, limit := range []int{25, 100} {
				dir := t.TempDir()
				writeHistoryArchive(t, dir, historyStamp(0), 1, 20)
				path := filepath.Join(dir, "events.jsonl")
				writeJSONLEvents(t, path, seqRange(21, 30)...)
				orig := readRotationDir
				listed := false
				readRotationDir = func(d string) ([]os.DirEntry, error) {
					if listed {
						return orig(d)
					}
					listed = true
					for _, s := range tc.beforeList {
						s(t, path)
					}
					entries, err := orig(d)
					for _, s := range tc.afterList {
						s(t, path)
					}
					return entries, err
				}
				got, matched, err := ReadFilteredHistoryTail(context.Background(), path, Filter{}, limit, count)
				readRotationDir = orig
				if err != nil {
					t.Fatalf("%s count=%v limit=%d: %v", name, count, limit, err)
				}
				want := seqRange(1, 30)
				if len(want) > limit {
					want = want[len(want)-limit:]
				}
				if !reflect.DeepEqual(seqsOf(got), want) {
					t.Errorf("%s count=%v limit=%d: got %v, want %v", name, count, limit, seqsOf(got), want)
				}
				if count && matched != 30 {
					t.Errorf("%s limit=%d: matched %d, want 30", name, limit, matched)
				}
			}
		}
	}
}

// A stray segment whose window overlaps another one but is not identical (for
// example a copy left by hand) adds no duplicate and is not counted twice, as
// in the full scan: without count segments are merged by exact seq, with count
// they are read in seq order and a repeated seq is dropped.
func TestReadFilteredHistoryTailMergesOverlappingSegmentsByExactSeq(t *testing.T) {
	dir := t.TempDir()
	writeHistoryArchive(t, dir, historyStamp(0), 1, 20)
	writeJSONLEvents(t, filepath.Join(dir, formatRotatingBasename(historyStamp(1), 5, 19)), seqRange(5, 19)...)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, path, seqRange(21, 25)...)
	full, err := ReadFilteredWithInFlight(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if want := seqRange(1, 25); !reflect.DeepEqual(seqsOf(full), want) {
		t.Fatalf("control: the full scan gives %v, want %v", seqsOf(full), want)
	}
	for _, count := range []bool{false, true} {
		got, matched := historyTail(context.Background(), t, path, Filter{}, 100, count)
		if !reflect.DeepEqual(seqsOf(got), seqsOf(full)) || matched != len(full) {
			t.Fatalf("count=%v: got %v (matched %d), want %v (matched %d)", count, seqsOf(got), matched, seqsOf(full), len(full))
		}
	}
}

// Cancellation is observed at each point where the read can spend time. A
// read that is not canceled would not return context.Canceled in any case
// (in the first one it would read on to an archive that is not gzip).
func TestReadFilteredHistoryTailStopsWhenCanceled(t *testing.T) {
	cases := map[string]struct {
		setup  func(t *testing.T) string
		filter Filter
		count  bool
		nthErr int
	}{
		// Err calls: 1 at entry, 2 before the one active chunk, 3 before the
		// newest segment; the oldest archive (1..40) is not gzip.
		"between segments": {
			setup: func(t *testing.T) string {
				path := seedHistoryTailLayout(t)
				oldest := filepath.Join(filepath.Dir(path), formatArchiveBasename(historyStamp(0), 1, 40))
				if err := os.WriteFile(oldest, []byte("not a gzip stream\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			nthErr: 3,
		},
		// 1 at entry, 2 before the archive, 3 at line 4096 of it.
		"inside a segment": {
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeHistoryArchive(t, dir, historyStamp(0), 1, 3*historyTailCtxCheckLines)
				return filepath.Join(dir, "events.jsonl")
			},
			filter: Filter{Type: "no.such.type"},
			nthErr: 3,
		},
		// 1 at entry, 2 and 3 before the first two chunks.
		"inside the backward scan of the active log": {
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "events.jsonl")
				writeJSONLEvents(t, path, seqRange(1, 5000)...)
				return path
			},
			filter: Filter{Type: "no.such.type"},
			nthErr: 3,
		},
		// 1 at entry, 2 at line 4096 of the forward read of the active log.
		"inside the forward read of the active log (count)": {
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "events.jsonl")
				writeJSONLEvents(t, path, seqRange(1, 5000)...)
				return path
			},
			filter: Filter{Type: "no.such.type"},
			count:  true,
			nthErr: 2,
		},
		// 1 at entry, 2 before the segment, 3 after the first chunk of a 4 MiB
		// line that has no newline inside it.
		"inside one long line": {
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				body := `{"seq":1,"type":"x"}` + "\n" + strings.Repeat("x", 4<<20) + "\n" + `{"seq":3,"type":"x"}` + "\n"
				if err := os.WriteFile(filepath.Join(dir, formatRotatingBasename(historyStamp(0), 1, 3)), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "events.jsonl")
			},
			nthErr: 3,
		},
	}
	for name, tc := range cases {
		path := tc.setup(t)
		ctx := newCancelOnNthErr(tc.nthErr)
		_, _, err := ReadFilteredHistoryTail(ctx, path, tc.filter, 1000, tc.count)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want context.Canceled", name, err)
		}
	}
}

// ReadFilteredEach yields exactly what ReadFiltered returns, in the same
// order, for every filter, including a Limit.
func TestReadFilteredEachMatchesReadFiltered(t *testing.T) {
	path := seedHistoryTailLayout(t)
	for name, f := range map[string]Filter{
		"none":      {},
		"subject":   {Subject: "s7"},
		"after seq": {AfterSeq: 50},
		"limit":     {Limit: 7},
		"no match":  {Type: "no.such.type"},
	} {
		want, err := ReadFiltered(path, f)
		if err != nil {
			t.Fatal(err)
		}
		var got []Event
		if err := ReadFilteredEach(context.Background(), path, f, func(e Event) error {
			got = append(got, e)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
			t.Errorf("%s: got %v, want %v", name, seqsOf(got), seqsOf(want))
		}
	}
}

// ReadFilteredEach hands each event over as it reads it: an error from fn on
// the first event stops the read before a later, unreadable archive is
// opened, and a done ctx stops it before any archive.
func TestReadFilteredEachStreams(t *testing.T) {
	dir := t.TempDir()
	writeHistoryArchive(t, dir, historyStamp(0), 1, 10)
	if err := os.WriteFile(filepath.Join(dir, formatArchiveBasename(historyStamp(1), 11, 20)), []byte("not a gzip stream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, path, seqRange(21, 25)...)
	if _, err := ReadFiltered(path, Filter{}); err == nil {
		t.Fatal("control: ReadFiltered did not reach the unreadable archive")
	}

	stop := errors.New("stop")
	calls := 0
	err := ReadFilteredEach(context.Background(), path, Filter{}, func(Event) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want fn's error after 1", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ReadFilteredEach(ctx, path, Filter{}, func(Event) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
