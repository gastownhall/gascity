package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestFileRecorderMarshalFailureDoesNotAdvanceDurableHead(t *testing.T) {
	recorder, err := NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	recorder.Record(Event{Type: BeadCreated, Subject: "durable"})

	event := Event{Type: BeadCreated, Subject: "invalid", Payload: json.RawMessage(`{`)}
	recorder.mu.Lock()
	err = recorder.writeRecordLocked(&event)
	recorder.mu.Unlock()
	if err == nil {
		t.Fatal("writeRecordLocked invalid payload succeeded")
	}
	if event.Seq != 0 || !event.Ts.IsZero() {
		t.Fatalf("failed event mutated to seq/time %d/%s", event.Seq, event.Ts)
	}
	assertRecorderDurableHead(t, recorder, 1, []string{"durable"})
}

func TestFileRecorderWriteFailureDoesNotAdvanceDurableHead(t *testing.T) {
	tests := []struct {
		name  string
		write func(*FileRecorder, []byte) (int, error)
	}{
		{
			name: "short write",
			write: func(recorder *FileRecorder, data []byte) (int, error) {
				return recorder.file.Write(data[:len(data)/2])
			},
		},
		{
			name: "partial write error",
			write: func(recorder *FileRecorder, data []byte) (int, error) {
				n, _ := recorder.file.Write(data[:len(data)/2])
				return n, errors.New("disk full")
			},
		},
		{
			name: "write error",
			write: func(*FileRecorder, []byte) (int, error) {
				return 0, errors.New("disk unavailable")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			recorder, err := NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recorder.Close() })
			recorder.Record(Event{Type: BeadCreated, Subject: "durable"})

			recorder.writeRecord = func(data []byte) (int, error) { return tc.write(recorder, data) }
			event := Event{Type: BeadCreated, Subject: "failed"}
			recorder.mu.Lock()
			err = recorder.writeRecordLocked(&event)
			recorder.mu.Unlock()
			if err == nil {
				t.Fatal("writeRecordLocked failure succeeded")
			}
			if event.Seq != 0 || !event.Ts.IsZero() {
				t.Fatalf("failed event mutated to seq/time %d/%s", event.Seq, event.Ts)
			}
			assertRecorderDurableHead(t, recorder, 1, []string{"durable"})

			// A later successful append reuses the next durable sequence rather
			// than preserving a phantom gap from the failed candidate.
			recorder.writeRecord = nil
			recorder.Record(Event{Type: BeadCreated, Subject: "recovered"})
			assertRecorderDurableHead(t, recorder, 2, []string{"durable", "recovered"})
		})
	}
}

func TestFileRecorderAppendBatchRollsBackPhysicalShortWrite(t *testing.T) {
	recorder, err := NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	recorder.Record(Event{Type: BeadCreated, Subject: "durable"})
	recorder.writeRecord = func(data []byte) (int, error) {
		return recorder.file.Write(data[:len(data)/2])
	}
	if err := recorder.AppendBatch([]Event{
		{Type: BeadCreated, Subject: "partial-one"},
		{Type: BeadCreated, Subject: "partial-two"},
	}); err == nil {
		t.Fatal("AppendBatch short write succeeded")
	}
	assertRecorderDurableHead(t, recorder, 1, []string{"durable"})

	recorder.writeRecord = nil
	if err := recorder.AppendBatch([]Event{{Type: BeadCreated, Subject: "recovered"}}); err != nil {
		t.Fatalf("AppendBatch after rollback: %v", err)
	}
	assertRecorderDurableHead(t, recorder, 2, []string{"durable", "recovered"})
}

func TestFileRecorderRecordReturnsWhenAutoRotatePoisonsRecorder(t *testing.T) {
	var stderr bytes.Buffer
	recorder, err := NewFileRecorder(
		filepath.Join(t.TempDir(), "events.jsonl"),
		&stderr,
		WithMaxSize(1),
		WithRotationCheckRecords(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	recorder.Record(Event{Type: BeadCreated, Subject: "rotate-me"})
	recorder.writeRecord = func(data []byte) (int, error) {
		n, _ := recorder.file.Write(data[:len(data)/2])
		_ = recorder.file.Close() // force rollback Truncate/Seek to fail and poison
		return n, nil
	}

	// The anchor failure is best-effort and logged, but must not fall through
	// to r.file.Fd() after the rollback path poisoned r.file=nil.
	recorder.Record(Event{Type: BeadCreated, Subject: "must-not-panic"})
	if !recorder.closed || recorder.file != nil {
		t.Fatalf("poisoned recorder state: closed=%v file=%v", recorder.closed, recorder.file)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("rollback failed; recorder closed")) {
		t.Fatalf("stderr = %q, want rollback poison diagnostic", stderr.String())
	}
}

func assertRecorderDurableHead(t *testing.T, recorder *FileRecorder, wantSeq uint64, wantSubjects []string) {
	t.Helper()
	seq, err := recorder.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	if seq != wantSeq {
		t.Fatalf("LatestSeq = %d, want %d", seq, wantSeq)
	}
	events, err := recorder.List(Filter{Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.Subject)
	}
	if len(got) != len(wantSubjects) {
		t.Fatalf("durable subjects = %v, want %v", got, wantSubjects)
	}
	for i := range got {
		if got[i] != wantSubjects[i] {
			t.Fatalf("durable subjects = %v, want %v", got, wantSubjects)
		}
	}
}
