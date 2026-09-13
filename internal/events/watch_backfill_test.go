package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

// --- Red-team regression coverage ---

// A concurrent Close during a mid-backfill Next must not race the captured fd
// (run with -race). Also asserts Close promptly unblocks.
func TestWatchCloseDuringBackfillNoRace(t *testing.T) {
	for iter := 0; iter < 40; iter++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "events.jsonl")
		var stderr bytes.Buffer
		rec, err := NewFileRecorder(path, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		recordN(rec, "a", 400)
		if _, err := rec.ForceRotate(); err != nil {
			t.Fatal(err)
		}
		rec.WaitForRotations()
		recordN(rec, "b", 400)

		ctx, cancel := context.WithCancel(context.Background())
		w, err := rec.Watch(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Signal once the drain goroutine returns from its first Next so Close
		// races a genuinely in-flight watcher rather than an arbitrary
		// wall-clock delay (lifecycle signal, no time.Sleep).
		started := make(chan struct{})
		go func() {
			first := true
			for {
				_, err := w.Next()
				if first {
					close(started)
					first = false
				}
				if err != nil {
					return
				}
			}
		}()
		<-started
		_ = w.Close()
		cancel()
		rec.Close() //nolint:errcheck // test cleanup
	}
}

// A watcher abandoned mid-archive-backfill must release the open segment reader
// at Close, not orphan the archive fd + gzip.Reader until GC. Regression for the
// iteration-4 major: Close deliberately skipped bfReader, so a cold-resume SSE
// client that read a partial batch and disconnected leaked the .gz descriptor.
func TestWatchCloseReleasesArchiveBackfillReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "m", 5000) // one archive segment spanning several backfill batches
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fw, err := rec.Watch(ctx, 0) // cold resume replays the archive
	if err != nil {
		t.Fatal(err)
	}
	w := fw.(*fileWatcher)

	// One Next reads a bounded batch and keeps the archive segment reader open
	// for the next call — the mid-backfill state a disconnect can strand.
	if _, err := fw.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	sr := w.bfReader
	if sr == nil || sr.f == nil {
		t.Fatalf("precondition: want an open archive segment reader after one Next, got %v", sr)
	}

	// Abandon the watcher without draining the rest of the archive.
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if w.bfReader != nil {
		t.Fatal("Close left the backfill reader open; archive fd leaks until GC")
	}
	if _, err := sr.f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("archive fd still open after Close: Stat err = %v, want os.ErrClosed", err)
	}
}

// A failed rotation catch-up must NOT poll the fresh file at the stale offset and
// advance past the unread window. Here a >1MiB line no longer poisons the scan
// (ReadBytes handles it), so we assert nothing is lost across such a rotation.
func TestWatchRotationLargeLineNotLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "seed", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()       //nolint:errcheck // test cleanup
	drainWatcher(t, w, 1) // consume seed

	big := make([]byte, 2*1024*1024)
	for i := range big {
		big[i] = 'x'
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "big", Message: string(big)})
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "small"})
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "after"})

	// "big" and "small" (pre-rotation tail) must both arrive before "after".
	seen := map[string]bool{}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && !seen["after"] {
		e := drainWatcher(t, w, 1)[0]
		seen[e.Subject] = true
		if e.Subject == "after" && (!seen["big"] || !seen["small"]) {
			t.Fatalf("saw 'after' before big=%v small=%v — large-line rotation lost the tail", seen["big"], seen["small"])
		}
	}
	if !seen["big"] || !seen["small"] {
		t.Fatalf("pre-rotation tail lost: big=%v small=%v", seen["big"], seen["small"])
	}
}

// The ordering-loss race: a .gz for a LATER window coexisting with a rotating
// file for an EARLIER window must still deliver the earlier window (FirstSeq
// order + monotonic guard).
func TestWatchBackfillOrderingAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "w1", 3) // window 1: seq 1..3
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	recordN(rec, "w2", 3) // window 2 (after anchor)
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()
	recordN(rec, "w3", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Everything strictly increasing in seq; window 1 must arrive first.
	got := drainWatcher(t, w, 3)
	if got[0].Subject != "w1-0" {
		t.Fatalf("first backfilled event = %q, want w1-0 (earlier window skipped?)", got[0].Subject)
	}
	var last uint64
	for i, e := range got {
		if e.Seq <= last {
			t.Fatalf("event %d seq %d not increasing (last %d)", i, e.Seq, last)
		}
		last = e.Seq
	}
}

// Resident memory during backfill is bounded to ~backfillBatch, not a whole
// segment: after one Next, the buffer must not hold the entire archive.
func TestWatchBackfillBoundedBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "m", 5000) // one big archive segment
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fw, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close() //nolint:errcheck // test cleanup

	// First Next triggers a backfill batch; the internal buffer must be bounded.
	if _, err := fw.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	w := fw.(*fileWatcher)
	if len(w.buf) > backfillBatch {
		t.Fatalf("backfill buffered %d events after one Next, want <= %d (whole segment pinned?)", len(w.buf), backfillBatch)
	}

	// And it still delivers all 5000 across many Next calls.
	for count := 1; count < 5000; count++ {
		drainWatcher(t, fw, 1)
	}
}

// Promotion window: a rotating file listed at T0 but promoted (renamed to its
// .gz, source removed) before the open must be read via its derived archive
// path, not silently skipped — the .gz did not exist at list time.
func TestBackfillRotatingPromotedBetweenListAndOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "p", 3)
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations() // .gz now exists, rotating file removed

	// Reconstruct the exact race state a watcher would see: a source list that
	// names the (now vanished) rotating file with the archive as fallback.
	archives, err := archiveFilesIn(dir)
	if err != nil || len(archives) != 1 {
		t.Fatalf("archives = %v err = %v, want exactly 1", archives, err)
	}
	info := archives[0]
	src := backfillSource{
		path:         filepath.Join(dir, "events.jsonl.rotating-vanished"), // gone
		fallbackPath: filepath.Join(dir, info.Basename),
		kind:         sourceRotating,
		firstSeq:     info.FirstSeq,
		lastSeq:      info.LastSeq,
	}
	sr, err := openSegmentReader(src)
	if err != nil {
		t.Fatalf("openSegmentReader: %v", err)
	}
	if sr == nil {
		t.Fatal("vanished rotating file with existing .gz was skipped — promotion window lost")
	}
	defer sr.close()

	var maxSeq uint64
	var out []Event
	eof, err := sr.readInto(Filter{}, &maxSeq, &out, 100)
	if err != nil || !eof {
		t.Fatalf("readInto: eof=%v err=%v", eof, err)
	}
	if len(out) != 3 {
		t.Fatalf("fallback archive yielded %d events, want 3", len(out))
	}
}

// TestWatchRepeatedRotationDuringCatchUpNotLost pins the multi-rotation
// catch-up fix: a second rotation that lands while the watcher is still
// draining the first rotation's archive must not truncate or drop the second
// rotation's window. Before the fix the catch-up froze its source list at the
// first rotation, so once that frozen list drained the watcher committed the
// newest inode and skipped the intervening window (or, on inode reuse, hung).
func TestWatchRepeatedRotationDuringCatchUpNotLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "seed", 2) // seq 1..2
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()       //nolint:errcheck // test cleanup
	drainWatcher(t, w, 2) // consume seed; offset at EOF of the active file

	// Fill the active file so draining the first rotation's archive spans
	// several backfill batches, giving the second rotation room to land while
	// the catch-up is provably mid-drain.
	const firstWindow = 2 * backfillBatch
	recordN(rec, "w1", firstWindow)
	if _, err := rec.ForceRotate(); err != nil { // rotation 1
		t.Fatal(err)
	}
	rec.WaitForRotations()

	// Start the catch-up and pull a few events so it is mid-drain of archive-1
	// (bfCatchUp true, first archive not yet exhausted): consumes seq 3..7.
	drainWatcher(t, w, 5)

	// Second rotation lands now, while the first catch-up is still draining.
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "mid"})
	if _, err := rec.ForceRotate(); err != nil { // rotation 2
		t.Fatal(err)
	}
	rec.WaitForRotations()
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "after"})

	latest, err := rec.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	// Seq 1..7 consumed so far. Everything after must arrive contiguously, in
	// strictly-increasing seq with no gap, and include both markers.
	remaining := int(latest) - 7
	got := drainWatcher(t, w, remaining)
	var last uint64 = 7
	sawMid, sawAfter := false, false
	for i, e := range got {
		if e.Seq != last+1 {
			t.Fatalf("event %d seq = %d, want %d (gap → a rotation window was dropped)", i, e.Seq, last+1)
		}
		last = e.Seq
		switch e.Subject {
		case "mid":
			sawMid = true
		case "after":
			sawAfter = true
		}
	}
	if !sawMid {
		t.Fatal("'mid' event (second rotation window) was never delivered")
	}
	if !sawAfter {
		t.Fatal("'after' event (post second rotation) was never delivered")
	}
	if last != latest {
		t.Fatalf("last delivered seq = %d, want %d", last, latest)
	}
}

// TestStepRotationDrivesCatchUpDespiteReusedInode pins the same-inode guard for
// the repeated-rotation fix. Genuine inode reuse — a later rotation's fresh
// active file reusing the freed inode the watcher is anchored to — is
// filesystem-dependent and cannot be forced deterministically from a black-box
// test, so this drives the watcher state machine directly: with a catch-up in
// progress and the active inode equal to the watcher's anchor inode, stepRotation
// must keep draining the catch-up rather than take the same-inode fast path
// (which abandoned the drain and polled the fresh file at a stale offset before
// the fix, hanging the stream).
func TestStepRotationDrivesCatchUpDespiteReusedInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "arch", 4) // seq 1..4 → archived
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations() // archive-1 (seq 1..4) exists; active holds the anchor

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	activeInode := inodeOf(info)
	if activeInode == 0 {
		t.Skip("inode unavailable on this platform")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	fw, ok := w.(*fileWatcher)
	if !ok {
		t.Fatalf("Watch returned %T, want *fileWatcher", w)
	}

	// Simulate a tailing watcher that detected a rotation and began a catch-up,
	// then had its anchor inode reused by a second rotation's fresh active file.
	fw.needsBackfill = false
	fw.bfActive = false
	fw.closeFd() // release the resume fd we are not exercising here
	fw.bfCatchUp = true
	fw.bfListed = false
	fw.maxSeq = 0
	fw.inode = activeInode // == current active inode (as if reused)

	cont, err := fw.stepRotation()
	if err != nil {
		t.Fatalf("stepRotation: %v", err)
	}
	if !cont {
		t.Fatal("stepRotation took the same-inode fast path during a catch-up; the archived window would be skipped")
	}
	if len(fw.buf) == 0 {
		t.Fatal("catch-up produced no events; the archived window was not replayed")
	}
	if fw.buf[0].Seq != 1 {
		t.Fatalf("first catch-up event seq = %d, want 1 (archive replay from genesis)", fw.buf[0].Seq)
	}
}

// TestWatchConclusionWindowRotationNotLost pins the fix for the catch-up
// *conclusion* window race. When stepCatchUp's final re-list is empty it
// concludes catch-up and stepRotation commits the active inode with offset 0,
// then stepTail reads the active file. A rotation that lands in that seam — after
// the empty re-list, before the tail read — archives the committed active file
// and points the path at a fresh active file; a by-path tail would read the
// fresh file, advance maxSeq past the just-archived window, and lose it forever.
// The seam is sub-poll and filesystem-timing dependent, so this drives the
// watcher state machine directly and injects the rotation exactly at it.
func TestWatchConclusionWindowRotationNotLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "arch", 4) // seq 1..4 → archive-1
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations() // archive-1 (seq 1..4); active holds the anchor (seq 5)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	activeInode := inodeOf(info)
	if activeInode == 0 {
		t.Skip("inode unavailable on this platform")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	fw, ok := w.(*fileWatcher)
	if !ok {
		t.Fatalf("Watch returned %T, want *fileWatcher", w)
	}

	// Simulate a live tail mid-catch-up whose archived window (seq 1..4) is
	// already drained: resume done, cursor past the archive, and the current
	// active file (anchor seq 5) the next contiguous source with nothing archived
	// beyond the cursor. The next stepRotation therefore concludes catch-up and
	// commits the active inode.
	fw.needsBackfill = false
	fw.bfActive = false
	fw.closeFd()
	fw.bfCatchUp = true
	fw.bfListed = false
	fw.maxSeq = 4
	fw.inode = activeInode
	fw.offset = 0

	cont, err := fw.stepRotation()
	if err != nil {
		t.Fatalf("stepRotation (conclude catch-up): %v", err)
	}
	if cont {
		t.Fatal("catch-up did not conclude; expected the active file to be the next contiguous source")
	}
	if fw.maxSeq != 4 {
		t.Fatalf("maxSeq after catch-up conclusion = %d, want 4", fw.maxSeq)
	}

	// Conclusion-window rotation: append seq 6, then rotate. The just-committed
	// active file (seq 5..6) is archived and the path now names a fresh active
	// file whose anchor is seq 7. Do NOT settle rotations yet, so the committed
	// inode cannot be freed and reused for the fresh active file (which would
	// defeat the identity check for reasons orthogonal to this finding).
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "seq6"}) // seq 6
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}

	// The by-path tail would read the fresh active file (anchor seq 7) and jump
	// maxSeq to 7, skipping [5..6]. The identity-checked tail must refuse to
	// advance from the unverified fresh file.
	if err := fw.stepTail(); err != nil {
		t.Fatalf("stepTail (conclusion-window rotation): %v", err)
	}
	if fw.maxSeq != 4 {
		t.Fatalf("maxSeq after conclusion-window rotation = %d, want 4 (tail read the fresh active file and skipped the just-archived [5..6] window)", fw.maxSeq)
	}
	if len(fw.buf) != 0 {
		t.Fatalf("stepTail buffered %d events from the unverified fresh active file; want 0", len(fw.buf))
	}

	// Rotation catch-up on the next poll must recover the just-archived [5..6]
	// window before the fresh active file is tailed.
	rec.WaitForRotations()
	cont, err = fw.stepRotation()
	if err != nil {
		t.Fatalf("stepRotation (recover conclusion window): %v", err)
	}
	if !cont {
		t.Fatal("stepRotation did not re-enter catch-up for the conclusion-window rotation; the [5..6] window would be lost")
	}
	if len(fw.buf) < 2 {
		t.Fatalf("catch-up recovered %d events; want the [5..6] window (2 events)", len(fw.buf))
	}
	if fw.buf[0].Seq != 5 || fw.buf[1].Seq != 6 {
		t.Fatalf("recovered window seqs = [%d, %d], want [5, 6]", fw.buf[0].Seq, fw.buf[1].Seq)
	}
}
