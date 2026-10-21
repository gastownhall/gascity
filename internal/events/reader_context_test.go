package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// countingCancelContext cancels its embedded context the Nth time Err is
// called, letting a test pin cancellation to a specific point in a
// multi-archive scan loop instead of racing a background timer.
type countingCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int
	calls    int
}

func (c *countingCancelContext) Err() error {
	c.calls++
	if c.calls == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

// newCancelAfter returns a context that becomes canceled on the Nth call to
// Err(), so a test can prove a scan loop checks ctx.Err() once per
// iteration rather than only once at entry.
func newCancelAfter(n int) *countingCancelContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &countingCancelContext{Context: ctx, cancel: cancel, cancelAt: n}
}

// seedArchives force-rotates a fresh recorder n times, perArchive events per
// rotation, producing n canonical .gz archives. Archive 1 holds exactly
// perArchive events (subjects r0-0..r0-<perArchive-1>); later archives also
// carry the EventsRotated anchor from the prior rotation. Returns the dir.
func seedArchives(t *testing.T) string {
	t.Helper()
	const n, perArchive = 3, 2
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	for r := 0; r < n; r++ {
		for i := 0; i < perArchive; i++ {
			rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("r%d-%d", r, i)})
		}
		res, err := rec.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate r=%d: %v", r, err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	return dir
}

// TestReadFilteredContextMatchesReadFilteredWhenNotCanceled is the control
// case: a non-canceled context must not change what either twin returns. A
// Since window the active log covers is answered by the backward walk, which
// leaves out the sentinel log's seq-2 record, so a twin that scanned forward
// instead would return it.
func TestReadFilteredContextMatchesReadFilteredWhenNotCanceled(t *testing.T) {
	twins := []struct {
		name  string
		plain func(string, Filter) ([]Event, error)
		ctx   func(context.Context, string, Filter) ([]Event, error)
	}{
		{"ReadFilteredContext", ReadFiltered, ReadFilteredContext},
		{"ReadFilteredWithInFlightContext", ReadFilteredWithInFlight, ReadFilteredWithInFlightContext},
	}
	logs := []struct {
		name string
		seed func(t *testing.T) (string, Filter)
	}{
		{"archives without a filter", func(t *testing.T) (string, Filter) {
			return filepath.Join(seedArchives(t), "events.jsonl"), Filter{}
		}},
		{"Since predating the active log", func(t *testing.T) (string, Filter) {
			return filepath.Join(seedArchives(t), "events.jsonl"), Filter{Since: time.Now().Add(-time.Hour)}
		}},
		{"Since inside the active log", func(t *testing.T) (string, Filter) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			return path, writeSentinelLog(t, path, time.Now().UTC())
		}},
	}
	for _, tw := range twins {
		for _, lg := range logs {
			t.Run(tw.name+"/"+lg.name, func(t *testing.T) {
				path, filter := lg.seed(t)
				want, err := tw.plain(path, filter)
				if err != nil {
					t.Fatalf("plain reader: %v", err)
				}
				if len(want) == 0 {
					t.Fatal("plain reader returned no events; the comparison would be vacuous")
				}
				got, err := tw.ctx(context.Background(), path, filter)
				if err != nil {
					t.Fatalf("%s: %v", tw.name, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %v, want %v (plain reader)", tw.name, got, want)
				}
			})
		}
	}
}

// TestReadFilteredContextAbortsBetweenArchives proves cancellation is
// honored BETWEEN archives, not just once upfront: archive 1 must be fully
// scanned (proving real progress happened) while archives 2 and 3 must
// never be opened (proving the abort landed before them, not after a full
// scan).
func TestReadFilteredContextAbortsBetweenArchives(t *testing.T) {
	dir := seedArchives(t)
	path := filepath.Join(dir, "events.jsonl")

	// Err() call #1 fires before archive 1 is processed (not yet canceled).
	// Call #2 fires before archive 2 is processed -- that's where this
	// cancels.
	ctx := newCancelAfter(2)
	got, err := ReadFilteredContext(ctx, path, Filter{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want exactly archive 1's 2 events (partial progress before abort): %+v", len(got), got)
	}
	for _, e := range got {
		if !strings.HasPrefix(e.Subject, "r0-") {
			t.Errorf("event %+v leaked from archive 2/3 -- scan should have aborted before opening them", e)
		}
	}
}

// TestReadFilteredContextAbortsUpfrontWhenAlreadyCanceled covers the
// complementary case: a context already canceled before the first archive
// must abort with zero progress, not just fail after a full scan.
func TestReadFilteredContextAbortsUpfrontWhenAlreadyCanceled(t *testing.T) {
	dir := seedArchives(t)
	path := filepath.Join(dir, "events.jsonl")

	ctx := newCancelAfter(1) // cancels on the very first ctx.Err() check.
	got, err := ReadFilteredContext(ctx, path, Filter{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d events, want 0 (canceled before any archive opened): %+v", len(got), got)
	}
}

// TestReadFilteredContextTwinsAbortSinceWalk covers the bounded path both
// twins take for a Since filter. The backward walk of the active log checks
// ctx before each chunk, so a canceled caller gets context.Canceled with the
// matches read so far instead of a walk to the window's close. The sentinel
// log's first chunk holds seqs 100 and 101 but not the whole closing run.
func TestReadFilteredContextTwinsAbortSinceWalk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	filter := writeSentinelLog(t, path, time.Now().UTC())
	twins := map[string]func(context.Context, string, Filter) ([]Event, error){
		"ReadFilteredContext":             ReadFilteredContext,
		"ReadFilteredWithInFlightContext": ReadFilteredWithInFlightContext,
	}
	for name, read := range twins {
		for _, tc := range []struct {
			cancelAt int
			want     []uint64
		}{
			{cancelAt: 1, want: []uint64{}},         // before the first chunk
			{cancelAt: 2, want: []uint64{100, 101}}, // before the second
		} {
			got, err := read(newCancelAfter(tc.cancelAt), path, filter)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s cancelAt=%d: err = %v, want context.Canceled", name, tc.cancelAt, err)
			}
			if !reflect.DeepEqual(seqsOf(got), tc.want) {
				t.Errorf("%s cancelAt=%d: got %v, want %v", name, tc.cancelAt, seqsOf(got), tc.want)
			}
		}
	}
}

// TestReadFilteredWithInFlightContextAbortsBetweenArchives is the
// ReadFilteredWithInFlight analog: the wrapper must propagate the same
// between-archives cancellation behavior from its underlying scan.
func TestReadFilteredWithInFlightContextAbortsBetweenArchives(t *testing.T) {
	dir := seedArchives(t)
	path := filepath.Join(dir, "events.jsonl")

	ctx := newCancelAfter(2)
	got, err := ReadFilteredWithInFlightContext(ctx, path, Filter{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want exactly archive 1's 2 events: %+v", len(got), got)
	}
}

// TestReadFilteredContextTwinsSeekPastTheCursor pins that the ctx twins skip
// the active log's prefix at or below AfterSeq without decoding it, as
// ReadFiltered does (activeScanStart): a line below the cursor that the
// scanner cannot take, longer than its 1 MiB limit, fails only a read that
// starts below it.
func TestReadFilteredContextTwinsSeekPastTheCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var sb strings.Builder
	fmt.Fprintf(&sb, `{"seq":1,"type":"a","ts":"2026-08-25T00:00:00Z","actor":"t","message":%q}`+"\n", strings.Repeat("x", 2<<20))
	for seq := 2; seq <= 5; seq++ {
		fmt.Fprintf(&sb, `{"seq":%d,"type":"a","ts":"2026-08-25T00:00:00Z","actor":"t"}`+"\n", seq)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	for _, read := range []struct {
		name string
		fn   func(context.Context, string, Filter) ([]Event, error)
	}{
		{"ReadFilteredContext", ReadFilteredContext},
		{"ReadFilteredWithInFlightContext", ReadFilteredWithInFlightContext},
	} {
		t.Run(read.name, func(t *testing.T) {
			got, err := read.fn(context.Background(), path, Filter{AfterSeq: 3})
			if err != nil {
				t.Fatalf("read after seq 3: %v", err)
			}
			if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
				t.Fatalf("read after seq 3 = %+v, want seqs 4 and 5", got)
			}
			if _, err := read.fn(context.Background(), path, Filter{}); err == nil {
				t.Fatal("read from the log's start succeeded; want the over-long line's scan error, or this test proves nothing")
			}
		})
	}
}

// TestFileRecorderListInFlightContextStopsWhenCanceled pins that the
// recorder's ctx-aware in-flight read returns ctx's error instead of
// scanning its archives once the caller has gone.
func TestFileRecorderListInFlightContextStopsWhenCanceled(t *testing.T) {
	dir := seedArchives(t)
	rec, err := NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	all, err := rec.ListInFlightContext(context.Background(), Filter{})
	if err != nil || len(all) == 0 {
		t.Fatalf("ListInFlightContext = %d events, %v; want the seeded history", len(all), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rec.ListInFlightContext(ctx, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListInFlightContext(canceled) err = %v, want context.Canceled", err)
	}
}
