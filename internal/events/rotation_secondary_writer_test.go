package events

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// openSecondaryRecorder opens a recorder the way the CLI and the controller's
// worker factories do next to the long-lived one: it never rotates and skips
// the startup sweep.
func openSecondaryRecorder(t *testing.T, path string, stderr *bytes.Buffer) *FileRecorder {
	t.Helper()
	rec, err := NewFileRecorder(path, stderr, WithMaxSize(0), WithoutStartupSweep())
	if err != nil {
		t.Fatalf("opening secondary recorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
	return rec
}

func assertNoRotatingLeftovers(t *testing.T, path string, stderr *bytes.Buffer) {
	t.Helper()
	leftover, err := filepath.Glob(path + ".rotating-*")
	if err != nil || len(leftover) != 0 {
		t.Fatalf("archiving left rotation sources behind: %v (glob err %v); stderr: %s", leftover, err, stderr.String())
	}
}

// assertContiguousHistory reads the whole retained history and checks that
// its sequence numbers run 1..n without a gap or repeat, and that every
// subject in want appears exactly once.
func assertContiguousHistory(t *testing.T, path string, want []string) []Event {
	t.Helper()
	all, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	seen := make(map[string]int, len(all))
	for i, e := range all {
		if wantSeq := uint64(i + 1); e.Seq != wantSeq {
			t.Fatalf("history[%d].Seq = %d, want %d (history: %v)", i, e.Seq, wantSeq, all)
		}
		if e.Subject != "" {
			seen[e.Subject]++
		}
	}
	for _, subject := range want {
		if n := seen[subject]; n != 1 {
			t.Errorf("event %q appears %d times in the retained history, want 1", subject, n)
		}
	}
	return all
}

// TestForceRotateWithSecondRecorderAndWatcherOpen covers a long-lived second
// recorder on the same log, such as a CLI emitter or a worker factory's
// recorder, alongside a watcher holding the active log open. The owning
// recorder must still rotate and finish archiving; the second recorder's next
// appends, single and batched, must land in the new active log rather than in
// the renamed file that has already been archived; and the watcher must
// deliver the whole history once, in order.
func TestForceRotateWithSecondRecorderAndWatcherOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	second := openSecondaryRecorder(t, path, &stderr)

	recordN(owner, "owner-pre", 2)                                                 // seq 1..2
	second.Record(Event{Type: BeadCreated, Actor: "human", Subject: "second-pre"}) // seq 3

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, err := second.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	res, err := owner.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate with a second recorder and a watcher open: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("ForceRotate did not rotate: %s", res.Reason)
	}
	<-res.Done
	assertNoRotatingLeftovers(t, path, &stderr)

	second.Record(Event{Type: BeadUpdated, Actor: "human", Subject: "second-post"}) // seq 5 after the anchor
	if err := second.AppendBatch([]Event{{Type: BeadUpdated, Actor: "human", Subject: "second-batch"}}); err != nil {
		t.Fatalf("AppendBatch after rotation: %v", err)
	}
	owner.Record(Event{Type: BeadClosed, Actor: "human", Subject: "owner-post"})

	want := []string{"owner-pre-0", "owner-pre-1", "second-pre", "second-post", "second-batch", "owner-post"}
	all := assertContiguousHistory(t, path, want)
	if len(all) != 7 {
		t.Fatalf("retained history has %d events, want 7 including the anchor: %v", len(all), all)
	}
	active, err := readActiveOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 4 || active[0].Type != EventsRotated {
		t.Fatalf("active log = %v, want the anchor followed by the three post-rotation events", active)
	}

	got := drainWatcher(t, w, len(all))
	for i, e := range got {
		if e.Seq != all[i].Seq || e.Subject != all[i].Subject {
			t.Fatalf("watcher event %d = seq %d %q, want seq %d %q", i, e.Seq, e.Subject, all[i].Seq, all[i].Subject)
		}
	}
}

// TestWatcherOutlivesTheRecorderThatOpenedIt covers consumers that need only a
// watcher and close their recorder right after Watch: the watcher must keep
// delivering events, including across the owning recorder's rotation.
func TestWatcherOutlivesTheRecorderThatOpenedIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup

	recordN(owner, "pre", 2) // seq 1..2

	opener, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	latest, err := opener.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, err := opener.Watch(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	if err := opener.Close(); err != nil {
		t.Fatalf("closing the recorder that opened the watcher: %v", err)
	}

	recordN(owner, "mid", 1) // seq 3
	res, err := owner.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	<-res.Done
	recordN(owner, "post", 1) // anchor seq 4, then seq 5

	got := drainWatcher(t, w, 3)
	if got[0].Subject != "mid-0" || got[1].Type != EventsRotated || got[2].Subject != "post-0" {
		t.Fatalf("watcher delivered %v, want mid-0, the rotation anchor, post-0", got)
	}
}

// TestAutoRotateSizesTheActiveLogAfterAnotherRecorderRotated covers two
// recorders that may both rotate, as every recorder opened with the city's
// rotation settings may. After one rotates, the other's handle still names the
// renamed, oversized file; its size check must judge the new active log, not
// that file, or it rotates the fresh log straight away.
func TestAutoRotateSizesTheActiveLogAfterAnotherRecorderRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr, otherStderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr, WithMaxSize(1024), WithRotationCheckRecords(1))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	other := openSecondaryRecorder(t, path, &otherStderr)

	recordN(other, "fill", 20) // well past 1 KiB, written without rotating
	res, err := other.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	<-res.Done

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "after"})
	rec.WaitForRotations()

	archives, err := filepath.Glob(path + ".archive-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want only the one rotation; stderr: %s", archives, stderr.String())
	}
	assertContiguousHistory(t, path, []string{"after"})
}

// pausePoint stops the first goroutine that runs its hook until resume is
// called; later arrivals pass straight through.
type pausePoint struct {
	once    sync.Once
	reached chan struct{}
	resumed chan struct{}
	release sync.Once
}

func newPausePoint() *pausePoint {
	return &pausePoint{reached: make(chan struct{}), resumed: make(chan struct{})}
}

func (p *pausePoint) hook() {
	first := false
	p.once.Do(func() { first = true })
	if !first {
		return
	}
	close(p.reached)
	<-p.resumed
}

func (p *pausePoint) resume() {
	p.release.Do(func() { close(p.resumed) })
}

// rotateWithAnchorPending rotates owner and runs during while the rotation
// holds the lock, with the new active log in place and its anchor not yet
// written.
func rotateWithAnchorPending(t *testing.T, owner *FileRecorder, during func()) {
	t.Helper()
	pause := newPausePoint()
	defer pause.resume()
	owner.hooks.anchorPending = pause.hook
	type result struct {
		res RotationResult
		err error
	}
	rotated := make(chan result, 1)
	go func() {
		res, err := owner.ForceRotate()
		rotated <- result{res, err}
	}()
	select {
	case <-pause.reached:
	case got := <-rotated:
		t.Fatalf("ForceRotate returned before writing an anchor: rotated=%v err=%v", got.res.Rotated, got.err)
	}
	during()
	pause.resume()
	got := <-rotated
	owner.hooks.anchorPending = nil
	if got.err != nil {
		t.Fatalf("ForceRotate: %v", got.err)
	}
	if !got.res.Rotated {
		t.Fatalf("ForceRotate did not rotate: %s", got.res.Reason)
	}
	<-got.res.Done
}

// appendWithLockHeld appends e through rec and runs during while rec holds
// the append lock, before it writes.
func appendWithLockHeld(t *testing.T, rec *FileRecorder, e Event, during func()) {
	t.Helper()
	pause := newPausePoint()
	defer pause.resume()
	rec.hooks.appendLocked = pause.hook
	appended := make(chan error, 1)
	go func() { appended <- rec.RecordAck(e) }()
	select {
	case <-pause.reached:
	case err := <-appended:
		t.Fatalf("RecordAck %q returned before taking the lock: %v", e.Subject, err)
	}
	during()
	pause.resume()
	err := <-appended
	rec.hooks.appendLocked = nil
	if err != nil {
		t.Fatalf("RecordAck %q: %v", e.Subject, err)
	}
}

// assertLockedOut checks that err is the bounded lock wait giving up. The
// other recorder held the lock for the whole call, so the call must fail
// rather than go ahead without it.
func assertLockedOut(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded while another recorder held the lock", what)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("%s: %v, want the bounded lock wait to time out", what, err)
	}
}

// logSiblings lists the rotation artifacts next to path.
func logSiblings(t *testing.T, path string) []string {
	t.Helper()
	siblings, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	return siblings
}

// TestNoAppendLandsBeforeTheRotationAnchor pauses a rotation after the new
// active log is in place and before its anchor is written, and has another
// writer append in that window. However the writer came to hold the log, it
// must be held off rather than write to the new log ahead of the anchor, and
// once the rotation finishes its next append must follow the anchor. Sequence
// numbers must stay unique and contiguous, and a watcher must deliver the
// whole history in order.
func TestNoAppendLandsBeforeTheRotationAnchor(t *testing.T) {
	rotating := []FileRecorderOption{WithMaxSize(1024), WithRotationCheckRecords(1)}
	for _, tc := range []struct {
		name string
		opts []FileRecorderOption
		// openedBefore opens the writer, and appends through it, before the
		// paused rotation; otherwise the writer is opened while it is paused.
		openedBefore bool
		// earlierRotation has the owner finish one rotation first, so the
		// writer's handle is already stale when the paused rotation starts.
		earlierRotation bool
	}{
		{name: "rotating writer opened before", opts: rotating, openedBefore: true},
		{name: "non-rotating writer opened before", openedBefore: true},
		{name: "non-rotating writer left stale by an earlier rotation", openedBefore: true, earlierRotation: true},
		{name: "rotating writer left stale by an earlier rotation", opts: rotating, openedBefore: true, earlierRotation: true},
		{name: "writer opened during the rotation"},
		{name: "rotating writer opened during the rotation", opts: rotating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			var stderr, writerStderr bytes.Buffer
			owner, err := NewFileRecorder(path, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close() //nolint:errcheck // test cleanup
			owner.Record(Event{Type: BeadCreated, Actor: "human", Subject: "seed"})
			want := []string{"seed"}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			w, err := owner.Watch(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close() //nolint:errcheck // test cleanup

			openWriter := func() *FileRecorder {
				opts := append([]FileRecorderOption{WithoutStartupSweep()}, tc.opts...)
				rec, err := NewFileRecorder(path, &writerStderr, opts...)
				if err != nil {
					t.Fatalf("opening writer: %v", err)
				}
				t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
				return rec
			}
			var writer *FileRecorder
			if tc.openedBefore {
				writer = openWriter()
				if err := writer.RecordAck(Event{Type: BeadUpdated, Actor: "human", Subject: "writer-before"}); err != nil {
					t.Fatalf("RecordAck before the rotation: %v", err)
				}
				want = append(want, "writer-before")
			}
			if tc.earlierRotation {
				res, err := owner.ForceRotate()
				if err != nil || !res.Rotated {
					t.Fatalf("earlier ForceRotate: rotated=%v err=%v", res.Rotated, err)
				}
				<-res.Done
			}

			rotateWithAnchorPending(t, owner, func() {
				if writer == nil {
					writer = openWriter()
				}
				err := writer.RecordAck(Event{Type: BeadUpdated, Actor: "human", Subject: "during"})
				assertLockedOut(t, "an append while the rotation anchor is pending", err)
			})
			if err := writer.RecordAck(Event{Type: BeadUpdated, Actor: "human", Subject: "writer-after"}); err != nil {
				t.Fatalf("RecordAck after the rotation: %v", err)
			}
			want = append(want, "writer-after")
			owner.WaitForRotations()
			writer.WaitForRotations()

			active, err := readActiveOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 2 || active[0].Type != EventsRotated || active[1].Subject != "writer-after" {
				t.Fatalf("active log = %v, want the rotation anchor followed by writer-after", active)
			}
			all := assertContiguousHistory(t, path, want)
			for _, e := range all {
				if e.Subject == "during" {
					t.Fatalf("the held-off append was written: %v", all)
				}
			}
			got := drainWatcher(t, w, len(all))
			for i, e := range got {
				if e.Seq != all[i].Seq || e.Subject != all[i].Subject {
					t.Fatalf("watcher event %d = seq %d %q, want seq %d %q", i, e.Seq, e.Subject, all[i].Seq, all[i].Subject)
				}
			}
		})
	}
}

// TestAutoRotateJudgesSizeUnderTheLock covers a recorder that finds a size
// check due while another recorder rotates the log before the first takes
// the lock. Holding the lock, the first must judge the log it now names, the
// fresh one, and leave it alone rather than rotate it a second time.
func TestAutoRotateJudgesSizeUnderTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr, otherStderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	other, err := NewFileRecorder(path, &otherStderr, WithoutStartupSweep(),
		WithMaxSize(1024), WithRotationCheckRecords(1))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // test cleanup

	recordN(owner, "fill", 20) // well past 1 KiB, written without rotating

	pause := newPausePoint()
	defer pause.resume()
	other.hooks.autoRotateDue = pause.hook
	recorded := make(chan error, 1)
	go func() {
		recorded <- other.RecordAck(Event{Type: BeadCreated, Actor: "human", Subject: "after"})
	}()
	select {
	case <-pause.reached:
	case err := <-recorded:
		t.Fatalf("RecordAck returned without a due size check: %v", err)
	}
	res, err := owner.ForceRotate()
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate while a size check is due: rotated=%v err=%v", res.Rotated, err)
	}
	<-res.Done
	pause.resume()
	if err := <-recorded; err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	other.hooks.autoRotateDue = nil
	other.WaitForRotations()

	archives, err := filepath.Glob(path + ".archive-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want only the owner's rotation; stderr: %s", archives, otherStderr.String())
	}
	want := []string{"after"}
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("fill-%d", i))
	}
	assertContiguousHistory(t, path, want)
}

// TestForceRotateGivesUpWhileAnotherRecorderHoldsTheLock covers a rotation
// that meets another recorder mid-append. It must give up once the bounded
// lock wait runs out, leaving the log as it was; after the append finishes, a
// rotation archives it.
func TestForceRotateGivesUpWhileAnotherRecorderHoldsTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr, secondStderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	second := openSecondaryRecorder(t, path, &secondStderr)
	owner.Record(Event{Type: BeadCreated, Actor: "human", Subject: "seed"})

	appendWithLockHeld(t, second, Event{Type: BeadUpdated, Actor: "human", Subject: "held"}, func() {
		before := logSiblings(t, path)
		start := time.Now()
		_, err := owner.ForceRotate()
		elapsed := time.Since(start)
		assertLockedOut(t, "ForceRotate", err)
		if elapsed < recordFlockTimeout {
			t.Errorf("ForceRotate gave up after %v, want the full %v lock wait", elapsed, recordFlockTimeout)
		}
		if after := logSiblings(t, path); !slices.Equal(after, before) {
			t.Fatalf("a rotation that never got the lock changed the log's siblings: %v, was %v", after, before)
		}
	})

	res, err := owner.ForceRotate()
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate after the append: rotated=%v err=%v", res.Rotated, err)
	}
	<-res.Done
	if res.FirstSeq != 1 || res.LastSeq != 2 {
		t.Fatalf("archived seq window = %d..%d, want 1..2 including the held append", res.FirstSeq, res.LastSeq)
	}
	assertContiguousHistory(t, path, []string{"seed", "held"})
}

// TestSecondRecorderAppendsAcrossRotations drives a non-rotating second
// recorder through each way its appends meet the owner's rotations: held off
// during a rotation's critical section, through the handle a finished
// rotation left stale, and holding the lock while a rotation is held off.
// Each meeting is set up with a pause point instead of a race, so every run
// takes the same schedule. Every acknowledged append must be retained exactly
// once, with sequence numbers that never repeat or skip.
func TestSecondRecorderAppendsAcrossRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr, secondStderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	second := openSecondaryRecorder(t, path, &secondStderr)

	owner.Record(Event{Type: BeadCreated, Actor: "human", Subject: "seed"})
	want := []string{"seed"}
	appendOK := func(subject string) {
		t.Helper()
		if err := second.RecordAck(Event{Type: BeadUpdated, Actor: "human", Subject: subject}); err != nil {
			t.Fatalf("RecordAck %s: %v", subject, err)
		}
		want = append(want, subject)
	}

	const rotations = 3
	for i := 0; i < rotations; i++ {
		rotateWithAnchorPending(t, owner, func() {
			err := second.RecordAck(Event{Type: BeadUpdated, Actor: "human", Subject: fmt.Sprintf("held-off-%d", i)})
			assertLockedOut(t, "an append during the rotation", err)
		})
		appendOK(fmt.Sprintf("stale-handle-%d", i))
		appendOK(fmt.Sprintf("current-handle-%d", i))

		holder := fmt.Sprintf("lock-holder-%d", i)
		appendWithLockHeld(t, second, Event{Type: BeadUpdated, Actor: "human", Subject: holder}, func() {
			before := logSiblings(t, path)
			_, err := owner.ForceRotate()
			assertLockedOut(t, "a rotation while the second recorder appends", err)
			if after := logSiblings(t, path); !slices.Equal(after, before) {
				t.Fatalf("a rotation that never got the lock changed the log's siblings: %v, was %v", after, before)
			}
		})
		want = append(want, holder)
	}
	owner.WaitForRotations()

	assertNoRotatingLeftovers(t, path, &stderr)
	all := assertContiguousHistory(t, path, want)
	if len(all) != len(want)+rotations {
		t.Fatalf("retained history has %d events, want %d appends and %d anchors: %v", len(all), len(want), rotations, all)
	}
}
