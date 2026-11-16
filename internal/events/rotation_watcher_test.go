package events

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInodeStableAcrossAppendChangesOnRotation pins the identity the watcher's
// rotation detector compares: it stays fixed while the active log is appended
// to, changes when a rotation puts a fresh file at the path, and reads the same
// through an open handle as by path.
func TestInodeStableAcrossAppendChangesOnRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, before, err := statInode(path)
	if err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Skip("inode unavailable on this platform")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, appended, err := statInode(path)
	if err != nil {
		t.Fatal(err)
	}
	if appended != before {
		t.Fatalf("identity changed across an append to the same file: %d -> %d", before, appended)
	}

	rotating := path + ".rotating"
	if err := os.Rename(path, rotating); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := statInode(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == before {
		t.Fatal("the fresh active log has the rotated file's identity; the rotation would go undetected")
	}

	held, err := os.Open(rotating)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	_, viaHandle, err := inodeOfFile(held)
	if err != nil {
		t.Fatal(err)
	}
	if viaHandle != before {
		t.Fatalf("identity read through a handle = %d, want the by-path identity %d", viaHandle, before)
	}
}

// TestForceRotateWhileResumeBackfillHoldsActiveLog covers a watcher resuming
// from an old cursor, which keeps the active log open for its backfill. The
// recorder must still rotate and finish archiving while that reader is open,
// and the watcher must deliver every event once, in order, across the rotation.
func TestForceRotateWhileResumeBackfillHoldsActiveLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	recordN(rec, "pre", 3) // seq 1..3

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	if fw, ok := w.(*fileWatcher); !ok || !fw.needsBackfill || fw.activeFile == nil {
		t.Fatalf("precondition: want a resuming watcher holding the active log open, got %#v", w)
	}

	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate while a watcher holds the active log: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("ForceRotate did not rotate: %s", res.Reason)
	}
	if res.Done != nil {
		<-res.Done
	}
	if leftover, err := filepath.Glob(path + ".rotating-*"); err != nil || len(leftover) != 0 {
		t.Fatalf("archiving left rotation sources behind: %v (glob err %v); stderr: %s", leftover, err, stderr.String())
	}

	recordN(rec, "post", 2) // anchor is seq 4, then seq 5..6
	got := drainWatcher(t, w, 6)
	for i, e := range got {
		if want := uint64(i + 1); e.Seq != want {
			t.Fatalf("event %d seq = %d, want %d (all: %v)", i, e.Seq, want, got)
		}
	}
}

// TestWatcherSurvivesRotationWithoutGap exercises designer §8.1: the
// watcher must detect rotation (inode change), reset its byte offset
// to 0, and continue yielding events from the new active log without
// re-emitting any already-yielded event and without skipping the
// events.rotated anchor.
func TestWatcherSurvivesRotationWithoutGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "pre-1"})
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "pre-2"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Drain pre-rotate events first so the watcher's offset is at the
	// end of the active file before rotation.
	pre := []Event{}
	for i := 0; i < 2; i++ {
		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next pre %d: %v", i, err)
		}
		pre = append(pre, e)
	}
	if pre[0].Subject != "pre-1" || pre[1].Subject != "pre-2" {
		t.Errorf("pre-rotate subjects = [%q,%q], want [pre-1,pre-2]", pre[0].Subject, pre[1].Subject)
	}

	// Trigger rotation.
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatal("expected Rotated=true")
	}
	if res.Done != nil {
		<-res.Done
	}

	// Write more events post-rotate.
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-1"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-2"})

	// Watcher should see: anchor (events.rotated, seq 3), post-1 (seq 4), post-2 (seq 5).
	expected := []struct {
		Type    string
		Subject string
	}{
		{EventsRotated, ""},
		{BeadClosed, "post-1"},
		{BeadClosed, "post-2"},
	}
	for i, exp := range expected {
		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next post %d: %v", i, err)
		}
		if e.Type != exp.Type {
			t.Errorf("event %d Type = %q, want %q", i, e.Type, exp.Type)
		}
		if exp.Subject != "" && e.Subject != exp.Subject {
			t.Errorf("event %d Subject = %q, want %q", i, e.Subject, exp.Subject)
		}
	}

	// Verify the anchor's payload identifies the prior archive.
	got, err := readActiveOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 1 || got[0].Type != EventsRotated {
		t.Fatalf("active log doesn't lead with anchor: %+v", got)
	}
	if !strings.Contains(string(got[0].Payload), "prior_archive") {
		t.Errorf("anchor payload missing prior_archive: %s", got[0].Payload)
	}
}

// TestWatcherDoesNotReEmitAfterRotation guards against the watcher
// re-yielding already-seen events when offset resets to 0 on the new
// active file.
func TestWatcherDoesNotReEmitAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 3; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start watching after seq 3 — we should see only the anchor (seq 4)
	// and subsequent events.
	w, err := rec.Watch(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done
	}

	first, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.Seq != 4 {
		t.Errorf("first post-rotate event Seq = %d, want 4 (anchor)", first.Seq)
	}
	if first.Type != EventsRotated {
		t.Errorf("first event Type = %q, want %q", first.Type, EventsRotated)
	}

	rec.Record(Event{Type: BeadClosed, Actor: "human"})
	second, err := w.Next()
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if second.Seq != 5 {
		t.Errorf("second post-rotate event Seq = %d, want 5", second.Seq)
	}
}
