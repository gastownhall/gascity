package events

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedRecorderWithRotation creates a fresh recorder, writes recordsBefore
// events, force-rotates, then writes recordsAfter events. Returns the
// directory holding the active log + archives; the recorder is
// closed via t.Cleanup so callers can read disk state directly.
func seedRecorderWithRotation(t *testing.T, recordsBefore, recordsAfter int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rec.Close()
	})
	for i := 0; i < recordsBefore; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("pre-%d", i)})
	}
	if recordsBefore > 0 {
		res, err := rec.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate: %v", err)
		}
		if res.Done != nil {
			<-res.Done
		}
	}
	for i := 0; i < recordsAfter; i++ {
		rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: fmt.Sprintf("post-%d", i)})
	}
	return dir
}

func TestReadAllSpansArchivesAndActive(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	// 4 pre-rotate + 1 anchor + 3 post-rotate = 8 events
	if len(got) != 8 {
		t.Fatalf("ReadAll returned %d events, want 8", len(got))
	}

	// Seqs are monotonically increasing.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}

	// First 4 should be pre-rotate events; index 4 the anchor; rest post-rotate.
	for i := 0; i < 4; i++ {
		if got[i].Type != BeadCreated || !strings.HasPrefix(got[i].Subject, "pre-") {
			t.Errorf("got[%d] = %+v, want pre-rotate BeadCreated", i, got[i])
		}
	}
	if got[4].Type != EventsRotated {
		t.Errorf("got[4].Type = %q, want %q (anchor)", got[4].Type, EventsRotated)
	}
	for i := 5; i < 8; i++ {
		if got[i].Type != BeadClosed || !strings.HasPrefix(got[i].Subject, "post-") {
			t.Errorf("got[%d] = %+v, want post-rotate BeadClosed", i, got[i])
		}
	}
}

func TestReadFilteredSkipsNonOverlappingArchives(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	// AfterSeq=4 → archive (seqs 1..4) is fully excluded; only anchor + post events.
	got, err := ReadFiltered(path, Filter{AfterSeq: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("ReadFiltered(AfterSeq=4) returned %d events, want 4 (anchor + 3 post)", len(got))
	}
	for _, e := range got {
		if e.Seq <= 4 {
			t.Errorf("event seq %d should be > 4", e.Seq)
		}
	}
}

func TestReadFilteredAcrossArchivesAppliesPredicates(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadFiltered(path, Filter{Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	// 4 pre-rotate BeadCreated; 0 anchor (different type); 0 post-rotate (BeadClosed)
	if len(got) != 4 {
		t.Fatalf("ReadFiltered(Type=BeadCreated) returned %d events, want 4", len(got))
	}
	for _, e := range got {
		if e.Type != BeadCreated {
			t.Errorf("Type = %q, want %q", e.Type, BeadCreated)
		}
	}
}

func TestReadFilteredAcrossArchivesAppliesLimit(t *testing.T) {
	dir := seedRecorderWithRotation(t, 4, 3)
	path := filepath.Join(dir, "events.jsonl")

	got, err := ReadFiltered(path, Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadFiltered(Limit=2) returned %d events, want 2", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("got seqs = [%d,%d], want [1,2]", got[0].Seq, got[1].Seq)
	}
}

func TestReadLatestSeqSpansArchiveOnlyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z-seq-100-102")
	const body = `{"seq":100,"type":"x"}
{"seq":101,"type":"y"}
{"seq":102,"type":"z"}
`
	if err := os.WriteFile(rotating, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 100, 102))
	var stderr bytes.Buffer
	if err := gzipAndArchive(rotating, dest, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	seq, err := ReadLatestSeq(path)
	if err != nil {
		t.Fatalf("ReadLatestSeq: %v", err)
	}
	if seq != 102 {
		t.Fatalf("ReadLatestSeq archive-only = %d, want 102", seq)
	}
}

func TestReadAllSurvivesMultipleRotations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	const rotations = 3
	const perBatch = 3
	for r := 0; r < rotations; r++ {
		for i := 0; i < perBatch; i++ {
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
	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "tail"})

	// Expect 3 archives (3 rotations) — each with 3 batch events; each
	// rotation also writes 1 anchor to the next active file.
	// Total visible to ReadAll:
	//   3 rotations × 3 events = 9 batch events
	//   3 anchors (from rotations 1, 2, 3)
	//   1 tail = 13 events
	got, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 13 {
		t.Fatalf("ReadAll across %d rotations returned %d events, want 13", rotations, len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	// Last event is the tail.
	if got[len(got)-1].Subject != "tail" {
		t.Errorf("last event subject = %q, want %q", got[len(got)-1].Subject, "tail")
	}
}

func TestReadFilteredHandlesMissingArchiveDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir", "events.jsonl")

	got, err := ReadFiltered(missing, Filter{})
	if err != nil {
		t.Errorf("ReadFiltered missing dir: %v", err)
	}
	if got != nil {
		t.Errorf("got %d events, want nil", len(got))
	}
}

func TestReadFilteredIgnoresUnrelatedFiles(t *testing.T) {
	dir := seedRecorderWithRotation(t, 3, 2)
	path := filepath.Join(dir, "events.jsonl")
	// Drop a foreign file in the dir to confirm the archive walker
	// doesn't trip on it.
	if err := os.WriteFile(filepath.Join(dir, "scratch.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.archive-bogus.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("ReadFiltered returned %d events, want 6 (3 pre + 1 anchor + 2 post)", len(got))
	}
}
