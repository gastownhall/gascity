package events

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
