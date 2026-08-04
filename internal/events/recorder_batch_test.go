package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileRecorderAppendBatchWritesContiguousEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var stderr bytes.Buffer
	recorder, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	recorder.Record(Event{Type: BeadCreated, Actor: "seed"})
	explicit := time.Unix(123, 0).UTC()

	if err := recorder.AppendBatch([]Event{
		{Type: ExecutionWorkAssociated, Actor: "reemit", Subject: "work", RunID: "run"},
		{Type: ExecutionStepDefined, Actor: "reemit", Subject: "step", RunID: "run", StepID: "build", Ts: explicit},
	}); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %#v, want three", got)
	}
	if got[1].Seq != 2 || got[2].Seq != 3 {
		t.Fatalf("batch sequences = %d,%d, want 2,3", got[1].Seq, got[2].Seq)
	}
	if got[1].Ts.IsZero() || !got[2].Ts.Equal(explicit) {
		t.Fatalf("batch timestamps = %s,%s, want generated then %s", got[1].Ts, got[2].Ts, explicit)
	}
}

func TestFileRecorderAppendBatchMarshalsEverythingBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	err = recorder.AppendBatch([]Event{
		{Type: ExecutionWorkAssociated, Actor: "reemit", Subject: "would-partially-land"},
		{Type: ExecutionStepDefined, Actor: "reemit", Payload: json.RawMessage(`{`)},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("AppendBatch error = %v, want marshal error", err)
	}
	got, readErr := ReadAll(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(got) != 0 {
		t.Fatalf("events = %#v, want no partial batch", got)
	}
}

func TestFileRecorderAppendBatchSurfacesClosedAndLockErrors(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		recorder, err := NewFileRecorder(path, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		if err := recorder.AppendBatch([]Event{{Type: ExecutionStepDefined}}); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("AppendBatch error = %v, want closed error", err)
		}
	})

	t.Run("lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		recorder, err := NewFileRecorder(path, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = recorder.Close() })
		sibling := mustOpenSiblingLock(t, path)
		t.Cleanup(func() { _ = sibling.Close() })

		err = recorder.AppendBatch([]Event{{Type: ExecutionStepDefined}})
		if err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("AppendBatch error = %v, want lock error", err)
		}
	})
}

func TestWriteBatchDetectsShortWriteInOneCall(t *testing.T) {
	writer := &shortBatchWriter{}
	err := writeBatch(writer, []byte("complete batch"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeBatch error = %v, want io.ErrShortWrite", err)
	}
	if writer.calls != 1 {
		t.Fatalf("write calls = %d, want one", writer.calls)
	}
}

type shortBatchWriter struct {
	calls int
}

func (w *shortBatchWriter) Write(data []byte) (int, error) {
	w.calls++
	return len(data) - 1, nil
}
