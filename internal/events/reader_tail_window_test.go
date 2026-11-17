package events

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tailWindowLog writes n events 1 minute apart from base, every third one an
// "order.failed" and the rest padded "bead.updated" noise, and returns the
// path and the file size.
func tailWindowLog(t *testing.T, base time.Time, n int) (string, int64) {
	t.Helper()
	pad := strings.Repeat("x", 300)
	evts := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		typ := "bead.updated"
		if i%3 == 0 {
			typ = OrderFailed
		}
		evts = append(evts, Event{
			Seq: uint64(i + 1), Type: typ, Actor: "t", Subject: "s",
			Ts: base.Add(time.Duration(i) * time.Minute), Message: pad,
		})
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	return path, writeLog(t, path, evts)
}

func writeTailWindowArchive(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "events.jsonl.archive-20260730T040012Z-seq-1-100.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(`{"seq":1,"type":"controller.started","ts":"2026-07-30T04:00:00Z","actor":"t"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadTailWindowReportsCompleteWhenTheWholeActiveLogFits pins the only
// case a caller may treat the window as the whole history: the walk reached
// byte 0 and nothing older lives in an archive.
func TestReadTailWindowReportsCompleteWhenTheWholeActiveLogFits(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	path, size := tailWindowLog(t, base, 30)

	for _, bound := range []int64{0, size, size + 4096} {
		w, err := ReadTailWindow(context.Background(), path, Filter{Types: []string{OrderFailed}, MaxScanBytes: bound})
		if err != nil {
			t.Fatalf("ReadTailWindow(bound=%d): %v", bound, err)
		}
		if !w.Complete {
			t.Errorf("bound=%d: Complete = false, want true for a log the walk fully covered", bound)
		}
		if len(w.Events) != 10 {
			t.Errorf("bound=%d: %d matching events, want 10", bound, len(w.Events))
		}
		if !w.Oldest.Equal(base) {
			t.Errorf("bound=%d: Oldest = %s, want %s (the first event, matching or not)", bound, w.Oldest, base)
		}
	}
}

// TestReadTailWindowStopsAtTheByteBoundAndReportsTheHorizon is the property the
// order-outcome bounded mode rests on: Oldest is the timestamp of the oldest
// event the walk visited, of ANY type, so a caller can tell which part of
// history it has not seen.
func TestReadTailWindowStopsAtTheByteBoundAndReportsTheHorizon(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	path, size := tailWindowLog(t, base, 400)

	w, err := ReadTailWindow(context.Background(), path, Filter{Types: []string{OrderFailed}, MaxScanBytes: size / 10})
	if err != nil {
		t.Fatalf("ReadTailWindow: %v", err)
	}
	if w.Complete {
		t.Fatal("Complete = true after a walk that stopped at a tenth of the file")
	}
	if w.Oldest.IsZero() || !w.Oldest.After(base.Add(300*time.Minute)) {
		t.Fatalf("Oldest = %s, want a time inside the newest tenth of the log", w.Oldest)
	}
	if len(w.Events) == 0 || len(w.Events) > 60 {
		t.Fatalf("%d events returned, want a few dozen from a tenth of the log", len(w.Events))
	}
	for _, e := range w.Events {
		if e.Type != OrderFailed {
			t.Fatalf("event %d has type %q, want only the filtered type", e.Seq, e.Type)
		}
		if e.Ts.Before(w.Oldest) {
			t.Fatalf("event %d at %s is older than the reported horizon %s", e.Seq, e.Ts, w.Oldest)
		}
	}
	for i := 1; i < len(w.Events); i++ {
		if w.Events[i].Seq <= w.Events[i-1].Seq {
			t.Fatalf("events not chronological: seq %d after %d", w.Events[i].Seq, w.Events[i-1].Seq)
		}
	}
}

// TestReadTailWindowIsNotCompleteWhileAnArchiveExists: an active log read to
// byte 0 is still not the history when older events live in an archive.
func TestReadTailWindowIsNotCompleteWhileAnArchiveExists(t *testing.T) {
	path, _ := tailWindowLog(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 9)
	writeTailWindowArchive(t, filepath.Dir(path))

	w, err := ReadTailWindow(context.Background(), path, Filter{Types: []string{OrderFailed}})
	if err != nil {
		t.Fatalf("ReadTailWindow: %v", err)
	}
	if w.Complete {
		t.Fatal("Complete = true although an archive holds older events")
	}
	if len(w.Events) != 3 {
		t.Fatalf("%d events, want the 3 active-log matches (archives are never opened)", len(w.Events))
	}
}

// TestReadTailWindowMissingLog pins that a city with no event log yet is a
// complete, empty history, and with only archives is an incomplete one.
func TestReadTailWindowMissingLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	w, err := ReadTailWindow(context.Background(), path, Filter{})
	if err != nil {
		t.Fatalf("ReadTailWindow on a missing log: %v", err)
	}
	if !w.Complete || len(w.Events) != 0 || !w.Oldest.IsZero() {
		t.Fatalf("missing log = %+v, want complete, empty, zero horizon", w)
	}

	writeTailWindowArchive(t, dir)
	w, err = ReadTailWindow(context.Background(), path, Filter{})
	if err != nil {
		t.Fatalf("ReadTailWindow with only an archive: %v", err)
	}
	if w.Complete {
		t.Fatal("Complete = true with no active log but an archive on disk")
	}
}

func TestReadTailWindowStopsWhenCanceled(t *testing.T) {
	path, _ := tailWindowLog(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ReadTailWindow(ctx, path, Filter{}); err == nil {
		t.Fatal("ReadTailWindow on a canceled context returned nil error")
	} else if !strings.Contains(fmt.Sprint(err), "context canceled") {
		t.Fatalf("error = %v, want it to wrap the cancellation", err)
	}
}
