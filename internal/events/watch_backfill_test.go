package events

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// drainWatcher pulls exactly n events (or fails) with a per-call deadline.
func drainWatcher(t *testing.T, w Watcher, n int) []Event {
	t.Helper()
	out := make([]Event, 0, n)
	type res struct {
		e   Event
		err error
	}
	for i := 0; i < n; i++ {
		ch := make(chan res, 1)
		go func() {
			e, err := w.Next()
			ch <- res{e, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("Next %d/%d: %v", i+1, n, r.err)
			}
			out = append(out, r.e)
		case <-time.After(5 * time.Second):
			t.Fatalf("Next %d/%d timed out (archive-blind watcher?)", i+1, n)
		}
	}
	return out
}

func recordN(rec *FileRecorder, prefix string, n int) {
	for i := 0; i < n; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("%s-%d", prefix, i)})
	}
}

// W1: a watcher attached with afterSeq BELOW the rotation boundary must replay
// the archived events, not silently start at the post-rotation anchor.
func TestWatchResumeAcrossRotationReplaysArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "pre", 5) // seq 1..5
	res, err := rec.ForceRotate()
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate: %v rotated=%v", err, res.Rotated)
	}
	rec.WaitForRotations() // ensure the .gz archive exists
	recordN(rec, "post", 3)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Resume from seq 2: expect seq 3,4,5 (archived) + the rotation anchor + post-0,1,2.
	w, err := rec.Watch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// At least the three archived events (3,4,5) must arrive, in order, before
	// any post-rotation event.
	got := drainWatcher(t, w, 3)
	wantSubjects := []string{"pre-2", "pre-3", "pre-4"}
	for i, e := range got {
		if e.Subject != wantSubjects[i] {
			t.Fatalf("event %d subject = %q, want %q (archived events skipped?)", i, e.Subject, wantSubjects[i])
		}
		if e.Seq <= 2 {
			t.Fatalf("event %d seq = %d, want > 2", i, e.Seq)
		}
	}
}

// The strictly-monotonic guard must never emit a seq at or below afterSeq, and
// must never duplicate, even across the archive/active boundary.
func TestWatchResumeNoDuplicatesAcrossBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "a", 4)
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()
	recordN(rec, "b", 4)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0) // full retained history
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// 4 pre + 1 anchor + 4 post = 9 events, strictly increasing seq, no dupes.
	got := drainWatcher(t, w, 9)
	seen := map[uint64]bool{}
	var last uint64
	for i, e := range got {
		if e.Seq <= last {
			t.Fatalf("event %d seq %d not strictly increasing (last %d)", i, e.Seq, last)
		}
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
		last = e.Seq
	}
}

// A canceled context must unblock a mid-backfill Next promptly.
func TestWatchBackfillHonorsCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "x", 50)
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	ctx, cancel := context.WithCancel(context.Background())
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.Next()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next after cancel returned nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Next did not observe cancellation")
	}
}

// W2: events appended to the OLD active file, then rotation, with the watcher's
// offset behind them, must not be lost when the watcher detects the rotation.
func TestWatchMidRotationTailNotLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "seed", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	drainWatcher(t, w, 2) // consume seed; offset now at EOF of the active file

	// Append a "tail" event, then rotate BEFORE the watcher polls it. The tail
	// event lives only in the rotating/archived file after the rename.
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "tail"})
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "after"})

	// Expect: tail (from archive), the rotation anchor, then after — tail must
	// not be skipped.
	var sawTail bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sawTail {
		e := drainWatcher(t, w, 1)[0]
		if e.Subject == "tail" {
			sawTail = true
		}
		if e.Subject == "after" && !sawTail {
			t.Fatal("saw 'after' before 'tail' — pre-rotation tail was lost")
		}
	}
	if !sawTail {
		t.Fatal("mid-rotation tail event was never delivered")
	}
}

// Concurrent cold resumes must all complete (semaphore must not deadlock).
func TestWatchConcurrentResumes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "c", 6)
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			w, err := rec.Watch(ctx, 0)
			if err != nil {
				t.Errorf("Watch: %v", err)
				return
			}
			defer w.Close() //nolint:errcheck // test cleanup
			drainWatcher(t, w, 6)
		}()
	}
	wg.Wait()
}
