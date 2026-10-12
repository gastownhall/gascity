package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeLog writes evts as JSONL at path and returns the file size.
func writeLog(t *testing.T, path string, evts []Event) int64 {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range evts {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return int64(buf.Len())
}

// pastRun returns n "old.type" records stamped 10s apart from start, padded
// so the run crosses walk chunk boundaries. 31 or more of them span
// sinceScanSkew, which closes any Since window they are all older than by
// the margin.
func pastRun(firstSeq uint64, start time.Time, n int) []Event {
	padding := strings.Repeat("x", 4*1024)
	evts := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		evts = append(evts, Event{
			Seq: firstSeq + uint64(i), Type: "old.type", Actor: "api",
			Ts: start.Add(time.Duration(i) * 10 * time.Second), Message: padding,
		})
	}
	return evts
}

// writeSentinelLog writes a log whose answer shows where a backward walk
// stopped, and returns the filter to query it with. Seq 2 is an in-window
// "bead.closed" sentinel appended BEFORE a run of old records that spans
// the margin; seq 100 and 101 are the window at EOF. A walk that stops on
// the run returns [100 101]; a full scan, or a walk that kept going after
// the window closed, also returns the sentinel.
func writeSentinelLog(t *testing.T, path string, now time.Time) Filter {
	t.Helper()
	evts := []Event{
		{Seq: 1, Type: "old.type", Actor: "api", Ts: now.Add(-72 * time.Hour)},
		{Seq: 2, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	}
	evts = append(evts, pastRun(3, now.Add(-72*time.Hour), 40)...)
	evts = append(evts,
		Event{Seq: 100, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		Event{Seq: 101, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	)
	writeLog(t, path, evts)
	return Filter{Type: "bead.closed", Since: now.Add(-5 * time.Minute)}
}

// historyTailReader reads through ReadFilteredHistoryTail with a limit above
// every match these layouts hold, so in either mode matched must count
// exactly the events it returns.
func historyTailReader(count bool) func(string, Filter) ([]Event, error) {
	return func(path string, filter Filter) ([]Event, error) {
		evts, matched, err := ReadFilteredHistoryTail(context.Background(), path, filter, 51, count)
		if err == nil && matched != len(evts) {
			err = fmt.Errorf("matched = %d, want %d: limit 51 holds every match", matched, len(evts))
		}
		return evts, err
	}
}

// TestReadFilteredSinceStopsBeforeReadingWholeFile is the regression this
// change exists for: a bare Since filter used to json.Unmarshal every record
// in the active log before matchesFilter could reject it, so asking about the
// last few minutes cost the entire history. The sentinel proves the walk
// stopped where the window closed instead of parsing on toward byte 0.
func TestReadFilteredSinceStopsBeforeReadingWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	filter := writeSentinelLog(t, path, time.Now().UTC())
	want := []uint64{100, 101}

	got, bounded, err := readFilteredSince(context.Background(), path, filter)
	if err != nil {
		t.Fatal(err)
	}
	if !bounded {
		t.Fatal("walk reported unbounded; the run of old records closes the window")
	}
	if !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("readFilteredSince = %v, want %v: the seq-2 sentinel is behind the closed window", seqsOf(got), want)
	}

	for _, r := range []struct {
		name string
		read func(string, Filter) ([]Event, error)
	}{
		{"ReadFiltered", ReadFiltered},
		{"ReadFilteredWithInFlight", ReadFilteredWithInFlight},
	} {
		got, err := r.read(path, filter)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if !reflect.DeepEqual(seqsOf(got), want) {
			t.Errorf("%s = %v, want %v from the bounded walk", r.name, seqsOf(got), want)
		}
	}
}

// TestReadFilteredSinceKeepsRecordsAroundBackdatedOnes pins the stop rule
// against Ts that is not ordered by append. The recorder keeps a
// caller-supplied Ts, and some producers record after the moment they stamp,
// so one record, or a burst of them, can sit far before the window while
// in-window records were appended before it. Only a contiguous run of old
// records spanning the margin closes the window, and every Since reader must
// agree with the forward scan on these layouts.
func TestReadFilteredSinceKeepsRecordsAroundBackdatedOnes(t *testing.T) {
	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	rec := func(seq uint64, ts time.Time) Event {
		return Event{Seq: seq, Type: "bead.closed", Actor: "api", Ts: ts}
	}
	burst := func(firstSeq uint64, n int, ts func(i int) time.Time) []Event {
		evts := make([]Event, 0, n)
		for i := 0; i < n; i++ {
			evts = append(evts, rec(firstSeq+uint64(i), ts(i)))
		}
		return evts
	}
	join := func(parts ...[]Event) []Event {
		var evts []Event
		for _, p := range parts {
			evts = append(evts, p...)
		}
		return evts
	}
	head := []Event{rec(1, ago(72*time.Hour)), rec(2, ago(40*time.Second)), rec(3, ago(20*time.Second))}

	cases := []struct {
		name    string
		evts    []Event
		bounded bool
		want    []uint64
	}{
		{
			name: "one record backdated past the margin",
			evts: join(head, []Event{rec(4, ago(7*time.Minute)), rec(5, ago(time.Second))}),
			want: []uint64{2, 3, 5},
		},
		{
			name: "burst stamped at one instant",
			evts: join(head, burst(4, 20, func(int) time.Time { return ago(7 * time.Minute) }), []Event{rec(24, ago(time.Second))}),
			want: []uint64{2, 3, 24},
		},
		{
			name: "burst spanning less than the margin",
			evts: join(head, burst(4, 20, func(i int) time.Time {
				return ago(7*time.Minute + time.Duration(i)*10*time.Second)
			}), []Event{rec(24, ago(time.Second))}),
			want: []uint64{2, 3, 24},
		},
		{
			name: "backdated record after a run that closes the window",
			evts: join(pastRun(1, ago(72*time.Hour), 31), []Event{
				rec(40, ago(40*time.Second)), rec(41, ago(20*time.Second)), rec(42, ago(7*time.Minute)), rec(43, ago(time.Second)),
			}),
			bounded: true,
			want:    []uint64{40, 41, 43},
		},
		{
			name: "record stamped before its predecessor inside the window",
			evts: join(pastRun(1, ago(72*time.Hour), 31), []Event{
				rec(40, ago(30*time.Second)), rec(41, ago(50*time.Second)), rec(42, ago(10*time.Second)),
			}),
			bounded: true,
			want:    []uint64{40, 41, 42},
		},
	}
	readers := []struct {
		name string
		read func(path string, filter Filter) ([]Event, error)
	}{
		{"ReadFiltered", ReadFiltered},
		{"ReadFilteredWithInFlight", ReadFilteredWithInFlight},
		{"ReadFilteredContext", func(path string, filter Filter) ([]Event, error) {
			return ReadFilteredContext(context.Background(), path, filter)
		}},
		{"ReadFilteredWithInFlightContext", func(path string, filter Filter) ([]Event, error) {
			return ReadFilteredWithInFlightContext(context.Background(), path, filter)
		}},
		// 51 is more than any layout holds, so only the window can stop
		// the tail walk before byte 0.
		{"ReadFilteredTail", func(path string, filter Filter) ([]Event, error) {
			return ReadFilteredTail(path, filter, 51)
		}},
		{"ReadFilteredHistoryTail", historyTailReader(false)},
		{"ReadFilteredHistoryTail count", historyTailReader(true)},
		// ReadFilteredEach always runs the forward read; on these layouts,
		// which keep the invariant, it must return what the walk returns.
		{"ReadFilteredEach", func(path string, filter Filter) ([]Event, error) {
			var got []Event
			err := ReadFilteredEach(context.Background(), path, filter, func(e Event) error {
				got = append(got, e)
				return nil
			})
			return got, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeLog(t, path, tc.evts)
			filter := Filter{Since: ago(time.Minute)}

			if _, bounded, err := readFilteredSince(context.Background(), path, filter); err != nil || bounded != tc.bounded {
				t.Errorf("readFilteredSince: bounded = %v, err = %v; want bounded = %v", bounded, err, tc.bounded)
			}
			for _, r := range readers {
				got, err := r.read(path, filter)
				if err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
				if !reflect.DeepEqual(seqsOf(got), tc.want) {
					t.Errorf("%s = %v, want %v", r.name, seqsOf(got), tc.want)
				}
			}
		})
	}
}

// TestSinceReadersFoldInRotatingSegmentWhenWindowStaysOpen covers the readers
// that consult the in-flight rotation segment. Right after a rotation the
// previous active log lives only in events.jsonl.rotating-*, so a Since read
// the fresh active log cannot answer must fold that segment in. A record
// backdated past the margin does not answer it, whether the head probe skips
// the walk or the walk reaches byte 0 behind a backdated head.
func TestSinceReadersFoldInRotatingSegmentWhenWindowStaysOpen(t *testing.T) {
	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	rec := func(seq uint64, ts time.Time) Event {
		return Event{Seq: seq, Type: "bead.closed", Actor: "api", Ts: ts}
	}
	readers := []struct {
		name string
		read func(path string, filter Filter) ([]Event, error)
	}{
		{"ReadFilteredWithInFlight", ReadFilteredWithInFlight},
		{"ReadFilteredWithInFlightContext", func(path string, filter Filter) ([]Event, error) {
			return ReadFilteredWithInFlightContext(context.Background(), path, filter)
		}},
		{"ReadFilteredHistoryTail", historyTailReader(false)},
		{"ReadFilteredHistoryTail count", historyTailReader(true)},
	}
	for _, tc := range []struct {
		name   string
		active []Event
		want   []uint64
	}{
		{
			name:   "recent head",
			active: []Event{rec(4, ago(20*time.Second)), rec(5, ago(7*time.Minute)), rec(6, ago(time.Second))},
			want:   []uint64{1, 2, 3, 4, 6},
		},
		{
			name:   "backdated head",
			active: []Event{rec(4, ago(72*time.Hour)), rec(5, ago(20*time.Second)), rec(6, ago(7*time.Minute)), rec(7, ago(time.Second))},
			want:   []uint64{1, 2, 3, 5, 7},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			writeLog(t, filepath.Join(dir, formatRotatingBasename(ago(30*time.Second), 1, 3)),
				[]Event{rec(1, ago(50*time.Second)), rec(2, ago(45*time.Second)), rec(3, ago(40*time.Second))})
			writeLog(t, path, tc.active)
			filter := Filter{Since: ago(time.Minute)}

			for _, r := range readers {
				got, err := r.read(path, filter)
				if err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
				if !reflect.DeepEqual(seqsOf(got), tc.want) {
					t.Errorf("%s = %v, want %v", r.name, seqsOf(got), tc.want)
				}
			}
		})
	}
}

// TestReadFilteredSinceReportsUnboundedWhenWindowPredatesActiveLog is the
// correctness half. When the active log does not reach back past the window,
// an archive may still hold matches, so the answer is INCOMPLETE and must not
// be served. Reporting bounded here is how a rotated city would silently lose
// events.
func TestReadFilteredSinceReportsUnboundedWhenWindowPredatesActiveLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	writeLog(t, path, []Event{
		{Seq: 10, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		{Seq: 11, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	})

	// Every record in the active log is inside this window.
	_, bounded, err := readFilteredSince(context.Background(), path, Filter{Type: "bead.closed", Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if bounded {
		t.Fatal("walk reported bounded after reaching byte 0; archives may hold older matches and must be consulted")
	}
}

// TestReadFilteredSinceSkipsWalkWhenHeadCannotClose pins the head probe that
// keeps a long window from paying for a walk before the full read. Closing
// takes a run spanning sinceScanSkew whose records are all older than Since
// minus the margin, so its oldest record is older than Since minus twice the
// margin. When the log's first record is younger than that, the walk is
// skipped. The run below is backdated behind the head, so a walk would close
// on it; only the probe explains an unbounded answer. A head the probe
// cannot read leaves the decision to the walk.
func TestReadFilteredSinceSkipsWalkWhenHeadCannotClose(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	tail := append(pastRun(2, since.Add(-3*time.Hour), 31),
		Event{Seq: 100, Type: "bead.closed", Actor: "api", Ts: now.Add(-time.Minute)})

	for _, tc := range []struct {
		name    string
		head    string
		bounded bool
	}{
		{"head younger than twice the margin", `{"seq":1,"type":"old.type","ts":"` + since.Add(-9*time.Minute).Format(time.RFC3339Nano) + `"}`, false},
		{"head older than twice the margin", `{"seq":1,"type":"old.type","ts":"` + since.Add(-11*time.Minute).Format(time.RFC3339Nano) + `"}`, true},
		{"head without a Ts", `{"seq":1,"type":"old.type"}`, true},
		{"malformed head", `{"seq":1,"type":`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeLog(t, path, tail)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append([]byte(tc.head+"\n"), data...), 0o644); err != nil {
				t.Fatal(err)
			}

			got, bounded, err := readFilteredSince(context.Background(), path, Filter{Since: since})
			if err != nil {
				t.Fatal(err)
			}
			if bounded != tc.bounded {
				t.Fatalf("bounded = %v, want %v", bounded, tc.bounded)
			}
			if bounded && !reflect.DeepEqual(seqsOf(got), []uint64{100}) {
				t.Fatalf("got %v, want [100]", seqsOf(got))
			}
		})
	}
}

// TestSinceWindowClosesOnlyOnSustainedRun pins the stop rule itself.
// Records arrive newest first, as the backward walk visits them. Only a
// contiguous run of records older than the horizon whose own timestamps span
// sinceScanSkew closes the window: a lone backdated record or a burst stamped
// at one instant never does, a record with no Ts neither extends nor breaks
// the run, and a record at or after the horizon starts it over.
func TestSinceWindowClosesOnlyOnSustainedRun(t *testing.T) {
	since := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	horizon := since.Add(-sinceScanSkew)
	old := horizon.Add(-time.Second)

	cases := []struct {
		name   string
		since  time.Time
		ts     []time.Time
		closes int // index of the record that closes the window, -1 for none
	}{
		{"no Since", time.Time{}, []time.Time{old, old.Add(-time.Hour)}, -1},
		{"one old record", since, []time.Time{old.Add(-72 * time.Hour)}, -1},
		{"burst at one instant", since, slices.Repeat([]time.Time{old}, 100), -1},
		{"run just short of the margin", since, []time.Time{old, old.Add(-sinceScanSkew + time.Nanosecond)}, -1},
		{"run spanning the margin", since, []time.Time{old, old.Add(-time.Minute), old.Add(-sinceScanSkew)}, 2},
		{"run stamped out of order", since, []time.Time{old.Add(-sinceScanSkew), old.Add(-time.Minute), old}, 2},
		{"record without a Ts inside the run", since, []time.Time{old, {}, old.Add(-sinceScanSkew)}, 2},
		{"in-window record restarts the run", since, []time.Time{old, since, old.Add(-sinceScanSkew), old.Add(-2 * sinceScanSkew)}, 3},
		{"record at the horizon restarts the run", since, []time.Time{old, horizon, old.Add(-sinceScanSkew)}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newSinceWindow(tc.since)
			closes := -1
			for i, ts := range tc.ts {
				if w.closedBy(ts) {
					closes = i
					break
				}
			}
			if closes != tc.closes {
				t.Fatalf("closed at record %d, want %d", closes, tc.closes)
			}
		})
	}
}

// errReaderAt fails every read.
type errReaderAt struct{ err error }

func (r errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, r.err }

// TestReadFilteredSinceReportsHeadProbeError pins that a failed read of the
// log's first line is an error, neither a reason to skip the walk nor a
// complete answer.
func TestReadFilteredSinceReportsHeadProbeError(t *testing.T) {
	injected := errors.New("injected read failure")
	got, bounded, err := readFilteredSinceFrom(context.Background(), errReaderAt{err: injected}, 100, Filter{Since: time.Now()})
	if !errors.Is(err, injected) || bounded || len(got) != 0 {
		t.Fatalf("got %v, bounded = %v, err = %v; want no events, unbounded and the injected failure", seqsOf(got), bounded, err)
	}
}

// failingReaderAt fails every read that starts after offset 0 and before
// failFrom, so the head probe and the walk's first chunk succeed and the
// walk's next chunk does not.
type failingReaderAt struct {
	r        io.ReaderAt
	failFrom int64
	err      error
}

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off > 0 && off < f.failFrom {
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

// TestReadFilteredSinceReturnsPartialEventsOnReadError pins the error
// contract ReadFiltered documents: a read error mid-walk returns the matches
// parsed before it, oldest first, alongside the error, and an interrupted
// walk is never reported as a complete answer.
func TestReadFilteredSinceReturnsPartialEventsOnReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	now := time.Now().UTC()
	evts := []Event{{Seq: 1, Type: "old.type", Actor: "api", Ts: now.Add(-72 * time.Hour)}}
	padding := strings.Repeat("x", 4*1024)
	for i := 0; i < 60; i++ {
		evts = append(evts, Event{
			Seq: uint64(i + 2), Type: "bead.closed", Actor: "api",
			Ts: now.Add(-time.Minute), Message: padding,
		})
	}
	size := writeLog(t, path, evts)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	injected := errors.New("injected read failure")
	r := failingReaderAt{r: f, failFrom: size - walkChunkSize, err: injected}

	got, bounded, err := readFilteredSinceFrom(context.Background(), r, size, Filter{Type: "bead.closed", Since: now.Add(-5 * time.Minute)})
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want the injected read failure", err)
	}
	if bounded {
		t.Fatal("an interrupted walk reported a complete answer")
	}
	seqs := seqsOf(got)
	if len(seqs) == 0 || seqs[len(seqs)-1] != 61 {
		t.Fatalf("got %v, want the matches read before the failure, ending at seq 61", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("got %v, want consecutive seqs oldest first", seqs)
		}
	}
}

// TestReadFilteredSinceMatchesForwardScan is the equivalence check: for a
// window the active log covers, the bounded path must return exactly what the
// forward scan returns, Limit semantics included. readFilteredTracked takes
// the FIRST Limit matches chronologically, not the last, and a backward walk
// that forgot to reverse before truncating would return the newest instead.
func TestReadFilteredSinceMatchesForwardScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	evts := []Event{
		// Ten minutes of old records: a run that closes the window.
		{Seq: 1, Type: "bead.closed", Actor: "api", Ts: now.Add(-90 * time.Minute)},
		{Seq: 2, Type: "bead.closed", Actor: "api", Ts: now.Add(-80 * time.Minute)},
		{Seq: 3, Type: "bead.closed", Actor: "api", Ts: now.Add(-4 * time.Minute)},
		{Seq: 4, Type: "other.type", Actor: "api", Ts: now.Add(-3 * time.Minute)},
		{Seq: 5, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		{Seq: 6, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	}
	writeLog(t, path, evts)

	for _, limit := range []int{0, 1, 2, 99} {
		filter := Filter{Type: "bead.closed", Since: now.Add(-10 * time.Minute), Limit: limit}

		if _, bounded, err := readFilteredSince(context.Background(), path, filter); err != nil || !bounded {
			t.Fatalf("limit=%d: readFilteredSince bounded = %v, err = %v; want the bounded path", limit, bounded, err)
		}
		want, _, err := readFilteredTracked(path, filter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadFiltered(path, filter)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
			t.Fatalf("limit=%d: ReadFiltered = %v, want %v (forward scan)", limit, seqsOf(got), seqsOf(want))
		}
	}
}

// TestReadFilteredWithInFlightSinceMatchesFullRead pins that
// ReadFilteredWithInFlight, behind FileRecorder.ListInFlight, agrees with the
// full read when the active log alone closes the window.
// TestReadFilteredSinceStopsBeforeReadingWholeFile pins that it takes the
// bounded walk.
func TestReadFilteredWithInFlightSinceMatchesFullRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	var evts []Event
	padding := string(bytes.Repeat([]byte("x"), 4*1024))
	for i := 0; i < 40; i++ {
		evts = append(evts, Event{
			Seq: uint64(i + 1), Type: "old.type", Actor: "api",
			Ts: now.Add(-48*time.Hour + time.Duration(i)*time.Minute), Message: padding,
		})
	}
	evts = append(evts,
		Event{Seq: 90, Type: "bead.closed", Actor: "api", Ts: now.Add(-3 * time.Minute)},
		Event{Seq: 91, Type: "other.type", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		Event{Seq: 92, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	)
	writeLog(t, path, evts)

	filter := Filter{Type: "bead.closed", Since: now.Add(-10 * time.Minute)}

	if _, bounded, err := readFilteredSince(context.Background(), path, filter); err != nil || !bounded {
		t.Fatalf("readFilteredSince bounded = %v, err = %v; want the bounded path", bounded, err)
	}
	want, _, err := readFilteredTracked(path, filter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFilteredWithInFlight(path, filter)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(got), seqsOf(want)) {
		t.Fatalf("ReadFilteredWithInFlight = %v, want %v (full read)", seqsOf(got), seqsOf(want))
	}
	if !reflect.DeepEqual(seqsOf(got), []uint64{90, 92}) {
		t.Fatalf("got %v, want [90 92]", seqsOf(got))
	}
}

// TestReadFilteredTailSinceStopsAtHorizon covers the walk the supervisor's
// event API actually runs: humaHandleEventList -> fetchEventPageAscending ->
// FileRecorder.ListTail -> ReadFilteredTail. A selective Type filter combined
// with Since used to walk to byte 0 chasing a limit it could never reach. The
// limit here is far more than the window holds, so only the closed window can
// stop the walk short of the sentinel.
func TestReadFilteredTailSinceStopsAtHorizon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	filter := writeSentinelLog(t, path, time.Now().UTC())

	got, err := ReadFilteredTail(path, filter, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{100, 101}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ReadFilteredTail = %v, want %v: the seq-2 sentinel is behind the closed window", seqsOf(got), want)
	}
}

// TestReadFilteredHistoryTailSinceStopsAtHorizon covers the read behind a
// short event API page: fetchEventPageAscending falls back from ListTail to
// ListHistoryTail, counting every match of a filtered page. Both modes must
// stop where the window closes instead of reading the whole history, and the
// count must cover every match in the window when limit holds fewer.
func TestReadFilteredHistoryTailSinceStopsAtHorizon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	filter := writeSentinelLog(t, path, time.Now().UTC())

	for _, tc := range []struct {
		limit       int
		count       bool
		want        []uint64
		wantMatched int
	}{
		{limit: 50, count: false, want: []uint64{100, 101}, wantMatched: 2},
		{limit: 50, count: true, want: []uint64{100, 101}, wantMatched: 2},
		{limit: 1, count: false, want: []uint64{101}, wantMatched: 1},
		{limit: 1, count: true, want: []uint64{101}, wantMatched: 2},
	} {
		got, matched, err := ReadFilteredHistoryTail(context.Background(), path, filter, tc.limit, tc.count)
		if err != nil {
			t.Fatalf("limit=%d count=%v: %v", tc.limit, tc.count, err)
		}
		if !reflect.DeepEqual(seqsOf(got), tc.want) || matched != tc.wantMatched {
			t.Errorf("limit=%d count=%v: got %v (matched %d), want %v (matched %d): the seq-2 sentinel is behind the closed window",
				tc.limit, tc.count, seqsOf(got), matched, tc.want, tc.wantMatched)
		}
	}
}

// TestReadFilteredTailWithoutSinceIsUnchanged pins that the horizon is opt-in.
// A tail read with no Since must still walk as far as it needs to fill its
// limit, which is what every existing caller depends on.
func TestReadFilteredTailWithoutSinceIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	var evts []Event
	padding := string(bytes.Repeat([]byte("x"), 4*1024))
	evts = append(evts, Event{Seq: 1, Type: "target.type", Actor: "api", Ts: base})
	for i := 0; i < 40; i++ {
		evts = append(evts, Event{
			Seq: uint64(i + 2), Type: "other.type", Actor: "api",
			Ts: base.Add(time.Duration(i) * time.Second), Message: padding,
		})
	}
	writeLog(t, path, evts)

	got, err := ReadFilteredTail(path, Filter{Type: "target.type"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("got %+v, want the seq-1 match ~160KB back", got)
	}
}
