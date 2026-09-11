package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
// case: a non-canceled context must not change ReadFiltered's behavior.
func TestReadFilteredContextMatchesReadFilteredWhenNotCanceled(t *testing.T) {
	dir := seedArchives(t)
	path := filepath.Join(dir, "events.jsonl")

	want, wantErr := ReadFiltered(path, Filter{})
	if wantErr != nil {
		t.Fatalf("ReadFiltered: %v", wantErr)
	}
	got, err := ReadFilteredContext(context.Background(), path, Filter{})
	if err != nil {
		t.Fatalf("ReadFilteredContext: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFilteredContext = %v, want %v (ReadFiltered)", got, want)
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
