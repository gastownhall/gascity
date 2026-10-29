package events

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadOnlyFileProviderHoldsNoWriteHandle covers the provider watchers use:
// it reads and watches the log another recorder writes, but never opens or
// creates a file for writing and refuses to record or rotate.
func TestReadOnlyFileProviderHoldsNoWriteHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	reader := NewReadOnlyFileProvider(path, &lockedBuffer{})
	defer reader.Close() //nolint:errcheck // test cleanup
	if err := reader.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "dropped"}); err == nil {
		t.Fatal("RecordAck on a read-only provider succeeded")
	}
	if _, err := reader.ForceRotate(); err == nil {
		t.Fatal("ForceRotate on a read-only provider succeeded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("read-only provider created files: %v (err %v)", entries, err)
	}

	writer, err := NewFileRecorder(path, &lockedBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close() //nolint:errcheck // test cleanup
	writer.Record(Event{Type: BeadCreated, Actor: "test", Subject: "seen"})

	seq, err := reader.LatestSeq()
	if err != nil || seq != 1 {
		t.Fatalf("LatestSeq = %d, %v; want 1", seq, err)
	}
	w, err := reader.Watch(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	e, err := w.Next()
	if err != nil || e.Subject != "seen" {
		t.Fatalf("Next = %+v, %v; want the writer's event", e, err)
	}
}
