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

// TestRecordAutoRotatesOnSizeThreshold drives the size-gated rotation
// path on the Record() hot path. With a small MaxSize and an
// aggressive check cadence, repeated writes must trigger at least one
// rotation, the active log must shrink afterward, and the seq stream
// must remain monotonic across the boundary.
func TestRecordAutoRotatesOnSizeThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(
		path, &stderr,
		WithMaxSize(2048),
		WithRotationCheckRecords(8),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	// Each event is ~100 bytes; ~25 events should cross 2 KiB and
	// trigger rotation on the next 8-record check window.
	for i := 0; i < 200; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "auto-rotate-fixture"})
	}

	// Wait for any auto-rotation gzips to finish so ReadAll observes
	// the canonical archives instead of in-flight rotating-* files.
	rec.WaitForRotations()

	var archives []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), archivePrefix) && strings.HasSuffix(e.Name(), ".gz") {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) == 0 {
		t.Fatalf("expected at least one archive after %d records, got none in %v",
			200, dir)
	}

	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, all[i].Seq, all[i-1].Seq)
		}
	}
	// Every event we recorded should be visible across active+archives,
	// plus at least one events.rotated anchor for each archive.
	got := 0
	rotations := 0
	for _, e := range all {
		switch e.Type {
		case BeadCreated:
			got++
		case EventsRotated:
			rotations++
		}
	}
	if got != 200 {
		t.Errorf("recorded BeadCreated count = %d, want 200", got)
	}
	if rotations < 1 {
		t.Errorf("expected at least one events.rotated anchor, got %d", rotations)
	}
}

// TestRecordAutoRotateDisabledByZeroMaxSize ensures the size-gated
// rotation path stays dormant when MaxSize is zero or negative,
// preserving backwards compatibility for callers that only set the
// size threshold via env override.
func TestRecordAutoRotateDisabledByZeroMaxSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(
		path, &stderr,
		WithMaxSize(0),
		WithRotationCheckRecords(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 100; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human"})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), archivePrefix) {
			t.Errorf("unexpected archive %q with MaxSize=0", e.Name())
		}
		if strings.HasPrefix(e.Name(), "events.jsonl.rotating-") {
			t.Errorf("unexpected rotating file %q with MaxSize=0", e.Name())
		}
	}
}

func TestArchiveRetainAgePrunesOldArchivesAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	oldArchive := filepath.Join(dir, formatArchiveBasename(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 1, 1))
	if err := os.WriteFile(oldArchive, []byte("stale archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr, WithArchiveRetainAge(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done != nil {
		<-res.Done
	}

	if _, err := os.Stat(oldArchive); !os.IsNotExist(err) {
		t.Fatalf("old archive should be pruned by retain age, stat err = %v", err)
	}
	if _, err := os.Stat(res.ArchivePath); err != nil {
		t.Fatalf("fresh archive should remain after retention pruning: %v", err)
	}
}

// TestNewFileRecorderReapsOrphansOnOpen exercises the constructor's
// one-shot orphan sweep: a rotating-* file left behind by a crashed
// rotation must be gzipped into its canonical archive name on the
// next NewFileRecorder open.
func TestNewFileRecorderReapsOrphansOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z")
	const body = `{"seq":1,"type":"x"}
{"seq":2,"type":"y"}
{"seq":3,"type":"z"}
`
	if err := os.WriteFile(rotating, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpOrphan := filepath.Join(dir, "events.jsonl.archive-20260101T000000Z-seq-1-2.gz.tmp")
	if err := os.WriteFile(tmpOrphan, []byte("incomplete"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	if _, err := os.Stat(rotating); !os.IsNotExist(err) {
		t.Errorf("rotating file should be reaped on open: %v", err)
	}
	if _, err := os.Stat(tmpOrphan); !os.IsNotExist(err) {
		t.Errorf(".gz.tmp orphan should be removed on open: %v", err)
	}

	// The reaped archive should be visible to ReadAll.
	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ReadAll after reap returned %d events, want 3", len(all))
	}
}

// TestNewFileRecorderSeedsSeqFromArchives verifies that on open, the
// recorder seeds its seq counter from the max archive LastSeq when
// the active log is absent or trails behind. Without this, a process
// that crashed during rotation could leave an archive with seqs
// [100..200] while the active log has no events, and the next Record
// would emit seq=1, silently duplicating seqs already on disk.
func TestNewFileRecorderSeedsSeqFromArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	rotating := filepath.Join(dir, "events.jsonl.rotating-20260507T120000Z-seq-100-200")
	var buf bytes.Buffer
	for i := uint64(100); i <= 200; i++ {
		fmt.Fprintf(&buf, "{\"seq\":%d,\"type\":\"x\",\"actor\":\"events\"}\n", i)
	}
	if err := os.WriteFile(rotating, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	// The reaper should have promoted the rotating file to a canonical
	// archive by the time NewFileRecorder returns.
	if _, err := os.Stat(rotating); !os.IsNotExist(err) {
		t.Errorf("rotating file should be reaped on open: %v", err)
	}

	rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: "post-recovery"})

	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 102 {
		t.Fatalf("ReadAll after recovery returned %d events, want 102", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Errorf("seq not monotonic at index %d: %d <= %d", i, all[i].Seq, all[i-1].Seq)
		}
	}
	last := all[len(all)-1]
	if last.Seq != 201 {
		t.Errorf("post-recovery Record Seq = %d, want 201", last.Seq)
	}
	if last.Subject != "post-recovery" {
		t.Errorf("last event Subject = %q, want %q", last.Subject, "post-recovery")
	}
}

// BenchmarkRecord measures the steady-state hot-path overhead of
// Record. The architect's NFR-01 bounds this at ≤1 ms per call. With
// auto-rotation off (MaxSize=0) we measure the baseline; with
// auto-rotation on at a high threshold, the size check is amortized
// and should not push past the bound.
func BenchmarkRecord(b *testing.B) {
	cases := []struct {
		name    string
		options []FileRecorderOption
	}{
		{"NoRotation", []FileRecorderOption{WithMaxSize(0)}},
		{"RotationGateAmortised", []FileRecorderOption{
			WithMaxSize(1 << 30),
			WithRotationCheckRecords(1024),
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			var stderr bytes.Buffer
			rec, err := NewFileRecorder(path, &stderr, tc.options...)
			if err != nil {
				b.Fatal(err)
			}
			defer rec.Close() //nolint:errcheck // bench cleanup

			ev := Event{Type: BeadCreated, Actor: "human", Subject: "bench"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rec.Record(ev)
			}
		})
	}
}
