package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Compile-time interface checks.
var (
	_ Provider = (*FileRecorder)(nil)
	_ Provider = (*Fake)(nil)
)

func TestFileRecorderWritesEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{
		Type:    BeadCreated,
		Actor:   "human",
		Subject: "gc-1",
		Message: "Build Tower of Hanoi",
	})

	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
	if e.Type != BeadCreated {
		t.Errorf("Type = %q, want %q", e.Type, BeadCreated)
	}
	if e.Actor != "human" {
		t.Errorf("Actor = %q, want %q", e.Actor, "human")
	}
	if e.Subject != "gc-1" {
		t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
	}
	if e.Message != "Build Tower of Hanoi" {
		t.Errorf("Message = %q, want %q", e.Message, "Build Tower of Hanoi")
	}
	if e.Ts.IsZero() {
		t.Error("Ts should be auto-filled, got zero")
	}
}

func TestFileRecorderPayloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	payload := json.RawMessage(`{"type":"merge-request","title":"Fix bug","labels":["urgent"]}`)
	rec.Record(Event{
		Type:    BeadCreated,
		Actor:   "polecat",
		Subject: "gc-42",
		Payload: payload,
	})

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Payload == nil {
		t.Fatal("Payload is nil, want JSON")
	}
	if string(events[0].Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", events[0].Payload, payload)
	}
}

func TestFileRecorderPayloadOmittedWhenNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human"})

	// Read raw line and verify no "payload" key.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"payload"`)) {
		t.Errorf("nil Payload should be omitted from JSON, got: %s", data)
	}
}

func TestFileRecorderMonotonicSeq(t *testing.T) {
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

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, e := range events {
		want := uint64(i + 1)
		if e.Seq != want {
			t.Errorf("events[%d].Seq = %d, want %d", i, e.Seq, want)
		}
	}
}

func TestFileRecorderConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	const goroutines = 10
	const eventsPerGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerGoroutine; i++ {
				rec.Record(Event{Type: BeadCreated, Actor: "human"})
			}
		}()
	}
	wg.Wait()

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	total := goroutines * eventsPerGoroutine
	if len(events) != total {
		t.Errorf("got %d events, want %d", len(events), total)
	}

	// All seq values should be unique.
	seen := make(map[uint64]bool, total)
	for _, e := range events {
		if seen[e.Seq] {
			t.Errorf("duplicate seq: %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func TestFileRecorderResumesSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	// First recorder: write 3 events.
	rec1, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec1.Record(Event{Type: BeadCreated, Actor: "human"})
	rec1.Record(Event{Type: BeadCreated, Actor: "human"})
	rec1.Record(Event{Type: BeadCreated, Actor: "human"})
	rec1.Close() //nolint:errcheck // test cleanup

	// Second recorder: should resume from seq 3.
	rec2, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec2.Record(Event{Type: BeadClosed, Actor: "human"})
	rec2.Close() //nolint:errcheck // test cleanup

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	if events[3].Seq != 4 {
		t.Errorf("resumed event Seq = %d, want 4", events[3].Seq)
	}
}

func TestFileRecorderCoordinatesSeqAcrossStaleRecorders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	rec1, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec1.Close() //nolint:errcheck // test cleanup
	rec2, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close() //nolint:errcheck // test cleanup

	rec1.Record(Event{Type: BeadCreated, Actor: "rec1"})
	rec2.Record(Event{Type: BeadUpdated, Actor: "rec2"})

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	for i, event := range events {
		want := uint64(i + 1)
		if event.Seq != want {
			t.Fatalf("events[%d].Seq = %d, want %d; events=%+v", i, event.Seq, want, events)
		}
	}
}

func TestFileRecorderFillsTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	before := time.Now()
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	after := time.Now()

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ts := events[0].Ts
	if ts.Before(before) || ts.After(after) {
		t.Errorf("Ts = %v, want between %v and %v", ts, before, after)
	}
}

func TestFileRecorderPreservesTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	explicit := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	rec.Record(Event{Type: BeadCreated, Actor: "human", Ts: explicit})

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if !events[0].Ts.Equal(explicit) {
		t.Errorf("Ts = %v, want %v", events[0].Ts, explicit)
	}
}

func TestFakeRecordsEvents(t *testing.T) {
	f := NewFake()
	f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	f.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})

	if len(f.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(f.Events))
	}
	if f.Events[0].Type != BeadCreated {
		t.Errorf("Events[0].Type = %q, want %q", f.Events[0].Type, BeadCreated)
	}
	if f.Events[1].Type != BeadClosed {
		t.Errorf("Events[1].Type = %q, want %q", f.Events[1].Type, BeadClosed)
	}
}

func TestFakeList(t *testing.T) {
	f := NewFake()
	f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	f.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})
	f.Record(Event{Type: SessionWoke, Actor: "gc", Subject: "mayor"})

	all, err := f.List(Filter{})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(all) = %d, want 3", len(all))
	}

	byType, err := f.List(Filter{Type: BeadCreated})
	if err != nil {
		t.Fatalf("List(type): %v", err)
	}
	if len(byType) != 1 {
		t.Fatalf("List(type) = %d, want 1", len(byType))
	}
}

func TestFakeListTailFiltersLimitModesAndErrors(t *testing.T) {
	f := NewFake()
	f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "old"})
	f.Record(Event{Type: BeadClosed, Actor: "human", Subject: "ignored"})
	f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "middle"})
	f.Record(Event{Type: BeadCreated, Actor: "gc", Subject: "wrong-actor"})
	f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "new"})

	tail, err := f.ListTail(Filter{Type: BeadCreated, Actor: "human"}, 2)
	if err != nil {
		t.Fatalf("ListTail(limit): %v", err)
	}
	if len(tail) != 2 {
		t.Fatalf("ListTail(limit) got %d events, want 2", len(tail))
	}
	if tail[0].Subject != "middle" || tail[1].Subject != "new" {
		t.Fatalf("tail subjects = [%s %s], want [middle new]", tail[0].Subject, tail[1].Subject)
	}

	all, err := f.ListTail(Filter{Type: BeadCreated, Actor: "human"}, 0)
	if err != nil {
		t.Fatalf("ListTail(limit=0): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListTail(limit=0) got %d events, want 3", len(all))
	}

	if _, err := NewFailFake().ListTail(Filter{}, 1); err == nil {
		t.Fatal("ListTail on broken fake returned nil error")
	}
}

func TestFakeLatestSeq(t *testing.T) {
	f := NewFake()
	seq, err := f.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 0 {
		t.Errorf("LatestSeq(empty) = %d, want 0", seq)
	}

	f.Record(Event{Type: BeadCreated, Actor: "human"})
	f.Record(Event{Type: BeadCreated, Actor: "human"})
	seq, err = f.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 2 {
		t.Errorf("LatestSeq = %d, want 2", seq)
	}
}

func TestFakeWatch(t *testing.T) {
	f := NewFake()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	w, err := f.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Record in a goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	}()

	e, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if e.Subject != "gc-1" {
		t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
	}
}

func TestFailFakeErrors(t *testing.T) {
	f := NewFailFake()

	_, err := f.List(Filter{})
	if err == nil {
		t.Error("List: expected error, got nil")
	}

	_, err = f.LatestSeq()
	if err == nil {
		t.Error("LatestSeq: expected error, got nil")
	}

	_, err = f.Watch(context.Background(), 0)
	if err == nil {
		t.Error("Watch: expected error, got nil")
	}
}

func TestDiscardDoesNothing(_ *testing.T) {
	// Should not panic.
	Discard.Record(Event{Type: BeadCreated, Actor: "human"})
}

func TestReadAllEmpty(t *testing.T) {
	// Missing file → nil, nil.
	events, err := ReadAll("/nonexistent/path/events.jsonl")
	if err != nil {
		t.Fatalf("ReadAll(missing) error: %v", err)
	}
	if events != nil {
		t.Errorf("ReadAll(missing) = %v, want nil", events)
	}

	// Empty file → nil, nil.
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := writeEmpty(path); err != nil {
		t.Fatal(err)
	}
	events, err = ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll(empty) error: %v", err)
	}
	if events != nil {
		t.Errorf("ReadAll(empty) = %v, want nil", events)
	}
}

func TestReadFiltered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	past := now.Add(-2 * time.Hour)
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1", Ts: past})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1", Ts: past})
	rec.Record(Event{Type: SessionWoke, Actor: "gc", Subject: "mayor", Ts: now})
	rec.Close() //nolint:errcheck // test cleanup

	t.Run("by_type", func(t *testing.T) {
		got, err := ReadFiltered(path, Filter{Type: BeadCreated})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].Type != BeadCreated {
			t.Errorf("Type = %q, want %q", got[0].Type, BeadCreated)
		}
	})

	t.Run("by_actor", func(t *testing.T) {
		got, err := ReadFiltered(path, Filter{Actor: "gc"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].Actor != "gc" {
			t.Errorf("Actor = %q, want %q", got[0].Actor, "gc")
		}
	})

	t.Run("by_since", func(t *testing.T) {
		since := now.Add(-1 * time.Hour)
		got, err := ReadFiltered(path, Filter{Since: since})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].Type != SessionWoke {
			t.Errorf("Type = %q, want %q", got[0].Type, SessionWoke)
		}
	})

	t.Run("combined", func(t *testing.T) {
		got, err := ReadFiltered(path, Filter{Type: BeadCreated, Actor: "human"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
	})

	t.Run("no_match", func(t *testing.T) {
		got, err := ReadFiltered(path, Filter{Type: MailSent})
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func TestReadFilteredAfterSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-" + string(rune('1'+i))})
	}
	rec.Close() //nolint:errcheck // test cleanup

	got, err := ReadFiltered(path, Filter{AfterSeq: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Seq != 4 {
		t.Errorf("got[0].Seq = %d, want 4", got[0].Seq)
	}
	if got[1].Seq != 5 {
		t.Errorf("got[1].Seq = %d, want 5", got[1].Seq)
	}
}

func TestReadFilteredAfterSeqCombined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 1
	rec.Record(Event{Type: BeadClosed, Actor: "human"})  // seq 2
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 3
	rec.Record(Event{Type: BeadClosed, Actor: "human"})  // seq 4
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 5
	rec.Close()                                          //nolint:errcheck // test cleanup

	// AfterSeq=2 AND Type=bead.created → only seq 3 and 5
	got, err := ReadFiltered(path, Filter{AfterSeq: 2, Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Seq != 3 {
		t.Errorf("got[0].Seq = %d, want 3", got[0].Seq)
	}
	if got[1].Seq != 5 {
		t.Errorf("got[1].Seq = %d, want 5", got[1].Seq)
	}
}

func TestReadFilteredTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 1
	rec.Record(Event{Type: BeadClosed, Actor: "human"})  // seq 2
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 3
	rec.Record(Event{Type: BeadClosed, Actor: "human"})  // seq 4
	rec.Record(Event{Type: BeadCreated, Actor: "human"}) // seq 5
	rec.Close()                                          //nolint:errcheck // test cleanup

	got, err := ReadFilteredTail(path, Filter{Type: BeadCreated}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Seq != 3 || got[1].Seq != 5 {
		t.Fatalf("tail seqs = [%d %d], want [3 5]", got[0].Seq, got[1].Seq)
	}
}

func TestReadFilteredTailScansBackwardsAcrossChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	appendEvent := func(e Event, ending string) {
		t.Helper()
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
		buf.WriteString(ending)
	}

	appendEvent(Event{Seq: 1, Type: SessionWoke, Actor: "api", Subject: "too-low-seq", Ts: base.Add(9 * time.Second)}, "\n")
	appendEvent(Event{
		Seq:     2,
		Type:    SessionWoke,
		Actor:   "api",
		Subject: "cross-chunk",
		Ts:      base.Add(10 * time.Second),
		Message: string(bytes.Repeat([]byte("x"), 70*1024)),
	}, "\n")
	appendEvent(Event{Seq: 3, Type: SessionStopped, Actor: "api", Subject: "wrong-type", Ts: base.Add(11 * time.Second)}, "\n")
	appendEvent(Event{Seq: 4, Type: SessionWoke, Actor: "worker", Subject: "wrong-actor", Ts: base.Add(12 * time.Second)}, "\n")
	appendEvent(Event{Seq: 5, Type: SessionWoke, Actor: "api", Subject: "too-old", Ts: base.Add(-time.Second)}, "\n")
	buf.WriteString("\nnot-json\n")
	appendEvent(Event{Seq: 6, Type: SessionWoke, Actor: "api", Subject: "tail-match", Ts: base.Add(20 * time.Second)}, "\r\n")

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFilteredTail(path, Filter{
		AfterSeq: 1,
		Type:     SessionWoke,
		Actor:    "api",
		Since:    base,
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Subject != "cross-chunk" || got[1].Subject != "tail-match" {
		t.Fatalf("subjects = [%s %s], want [cross-chunk tail-match]", got[0].Subject, got[1].Subject)
	}
}

func TestReadFilteredTailLimitModesAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "created"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "closed"})
	rec.Close() //nolint:errcheck // test cleanup

	got, err := ReadFilteredTail(path, Filter{Actor: "human"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=0 got %d events, want 2", len(got))
	}

	missing, err := ReadFilteredTail(filepath.Join(dir, "missing.jsonl"), Filter{}, 1)
	if err != nil {
		t.Fatalf("missing file error: %v", err)
	}
	if missing != nil {
		t.Fatalf("missing file got %+v, want nil", missing)
	}
}

func TestReadLatestSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	rec.Close() //nolint:errcheck // test cleanup

	seq, err := ReadLatestSeq(path)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Errorf("ReadLatestSeq = %d, want 3", seq)
	}
}

func TestReadLatestSeqEmpty(t *testing.T) {
	// Missing file → (0, nil)
	seq, err := ReadLatestSeq("/nonexistent/path/events.jsonl")
	if err != nil {
		t.Fatalf("ReadLatestSeq(missing) error: %v", err)
	}
	if seq != 0 {
		t.Errorf("ReadLatestSeq(missing) = %d, want 0", seq)
	}

	// Empty file → (0, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := writeEmpty(path); err != nil {
		t.Fatal(err)
	}
	seq, err = ReadLatestSeq(path)
	if err != nil {
		t.Fatalf("ReadLatestSeq(empty) error: %v", err)
	}
	if seq != 0 {
		t.Errorf("ReadLatestSeq(empty) = %d, want 0", seq)
	}
}

func TestReadLatestSeqUsesTailOfAppendOnlyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	hugeMalformed := append(bytes.Repeat([]byte("x"), 2*1024*1024), '\n')
	validTail := []byte(`{"seq":42,"type":"bead.updated","ts":"2026-01-01T00:00:00Z","actor":"test"}` + "\n")
	if err := os.WriteFile(path, append(hugeMalformed, validTail...), 0o644); err != nil {
		t.Fatal(err)
	}

	seq, err := ReadLatestSeq(path)
	if err != nil {
		t.Fatalf("ReadLatestSeq: %v", err)
	}
	if seq != 42 {
		t.Fatalf("ReadLatestSeq = %d, want 42", seq)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	rec.Record(Event{Type: BeadClosed, Actor: "test"})
	rec.Close() //nolint:errcheck // test cleanup

	seq, err = ReadLatestSeq(path)
	if err != nil {
		t.Fatalf("ReadLatestSeq(after record): %v", err)
	}
	if seq != 43 {
		t.Fatalf("ReadLatestSeq(after record) = %d, want 43", seq)
	}
}

func TestReadFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})
	rec.Close() //nolint:errcheck // test cleanup

	// Read from offset 0 → all events
	evts, off, err := ReadFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("ReadFrom(0) got %d events, want 2", len(evts))
	}
	if off <= 0 {
		t.Fatalf("ReadFrom(0) offset = %d, want > 0", off)
	}

	// Write more events
	rec2, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec2.Record(Event{Type: SessionWoke, Actor: "gc", Subject: "mayor"})
	rec2.Close() //nolint:errcheck // test cleanup

	// Read from mid-file offset → only new event
	evts2, off2, err := ReadFrom(path, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts2) != 1 {
		t.Fatalf("ReadFrom(mid) got %d events, want 1", len(evts2))
	}
	if evts2[0].Type != SessionWoke {
		t.Errorf("ReadFrom(mid) Type = %q, want %q", evts2[0].Type, SessionWoke)
	}
	if off2 <= off {
		t.Errorf("ReadFrom(mid) offset = %d, want > %d", off2, off)
	}
}

func TestReadFromMissingFile(t *testing.T) {
	evts, off, err := ReadFrom("/nonexistent/path/events.jsonl", 0)
	if err != nil {
		t.Fatalf("ReadFrom(missing) error: %v", err)
	}
	if evts != nil {
		t.Errorf("ReadFrom(missing) events = %v, want nil", evts)
	}
	if off != 0 {
		t.Errorf("ReadFrom(missing) offset = %d, want 0", off)
	}
}

func TestReadFromNoNewData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	rec.Close() //nolint:errcheck // test cleanup

	// Read all to get EOF offset
	_, off, err := ReadFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Read from EOF → no new data
	evts, off2, err := ReadFrom(path, off)
	if err != nil {
		t.Fatal(err)
	}
	if evts != nil {
		t.Errorf("ReadFrom(eof) events = %v, want nil", evts)
	}
	if off2 != off {
		t.Errorf("ReadFrom(eof) offset = %d, want %d", off2, off)
	}
}

// --- Provider methods on FileRecorder ---

func TestFileRecorderList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})
	rec.Record(Event{Type: SessionWoke, Actor: "gc", Subject: "mayor"})

	// List all
	all, err := rec.List(Filter{})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(all) = %d events, want 3", len(all))
	}

	// List filtered by type
	created, err := rec.List(Filter{Type: BeadCreated})
	if err != nil {
		t.Fatalf("List(type): %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("List(type=bead.created) = %d, want 1", len(created))
	}
	if created[0].Subject != "gc-1" {
		t.Errorf("Subject = %q, want %q", created[0].Subject, "gc-1")
	}
}

func TestFileRecorderListTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "old"})
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "ignored"})
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "new"})

	got, err := rec.ListTail(Filter{Type: BeadCreated}, 1)
	if err != nil {
		t.Fatalf("ListTail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTail got %d events, want 1", len(got))
	}
	if got[0].Subject != "new" {
		t.Fatalf("subject = %q, want new", got[0].Subject)
	}
}

func TestFileRecorderLatestSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	rec.Record(Event{Type: BeadCreated, Actor: "human"})

	seq, err := rec.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 2 {
		t.Errorf("LatestSeq = %d, want 2", seq)
	}
}

func TestFileRecorderWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	// Write an initial event.
	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Should return the existing event (seq 1 > afterSeq 0).
	e, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
	if e.Subject != "gc-1" {
		t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
	}

	// Write another event in a goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})
	}()

	// Should eventually get the new event.
	e, err = w.Next()
	if err != nil {
		t.Fatalf("Next(2): %v", err)
	}
	if e.Seq != 2 {
		t.Errorf("Seq = %d, want 2", e.Seq)
	}
	if e.Type != BeadClosed {
		t.Errorf("Type = %q, want %q", e.Type, BeadClosed)
	}
}

func TestFileRecorderWatchAfterLatestStartsAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "gc-1"})
	rec.Record(Event{Type: BeadUpdated, Actor: "human", Subject: "gc-1"})
	seq, err := rec.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w, err := rec.Watch(ctx, seq)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	fw, ok := w.(*fileWatcher)
	if !ok {
		t.Fatalf("Watch returned %T, want *fileWatcher", w)
	}
	if fw.offset != info.Size() {
		t.Fatalf("watch offset = %d, want EOF %d", fw.offset, info.Size())
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "gc-1"})
	}()
	e, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if e.Seq != seq+1 {
		t.Fatalf("Seq = %d, want %d", e.Seq, seq+1)
	}
	if e.Type != BeadClosed {
		t.Fatalf("Type = %q, want %q", e.Type, BeadClosed)
	}
}

func TestFileRecorderWatchContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())

	w, err := rec.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup

	// Cancel immediately — Next should return context.Canceled.
	cancel()
	_, err = w.Next()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Next after cancel = %v, want context.Canceled", err)
	}
}

// writeEmpty creates an empty file at path.
func writeEmpty(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}
