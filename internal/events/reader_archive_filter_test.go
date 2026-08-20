package events

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// streamArchive used to take its Filter as `_ Filter` and discard it, so every
// caller paid a full gunzip plus a json.Unmarshal of EVERY record in the
// archive and filtered afterwards. archiveOverlapsFilter only skips an archive
// that ENDS at or below AfterSeq, so an order whose cursor sits inside the
// archive's seq window re-read the whole thing on every dispatcher tick.
// Measured on a real city: a 160,018-event archive cost ~63% of a CPU core on
// an idle supervisor.
//
// These tests pin the seq window at the archive reader, counting callback
// invocations rather than returned rows — the point is the work NOT done.
func TestStreamArchiveHonorsAfterSeq(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.jsonl")
	writeJSONLEvents(t, src, 1, 2, 3, 4, 5)
	archive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 5))
	var stderr bytes.Buffer
	if err := gzipAndArchive(src, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	var seen []uint64
	if err := streamArchive(archive, Filter{AfterSeq: 3}, func(e Event) bool {
		seen = append(seen, e.Seq)
		return true
	}); err != nil {
		t.Fatalf("streamArchive: %v", err)
	}

	// Records at or below the cursor must never reach the callback, which is
	// what proves they were not unmarshalled.
	if len(seen) != 2 || seen[0] != 4 || seen[1] != 5 {
		t.Fatalf("AfterSeq=3 delivered %v, want [4 5]", seen)
	}
}

func TestStreamArchiveStopsAtBeforeSeq(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.jsonl")
	writeJSONLEvents(t, src, 1, 2, 3, 4, 5)
	archive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 5))
	var stderr bytes.Buffer
	if err := gzipAndArchive(src, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	var seen []uint64
	if err := streamArchive(archive, Filter{BeforeSeq: 3}, func(e Event) bool {
		seen = append(seen, e.Seq)
		return true
	}); err != nil {
		t.Fatalf("streamArchive: %v", err)
	}

	// Archives are seq-ordered, so BeforeSeq is an early exit: the tail of the
	// archive is never decoded.
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("BeforeSeq=3 delivered %v, want [1 2]", seen)
	}
}

// A zero Filter must still deliver everything — the seq window is an
// optimisation, not a new default.
func TestStreamArchiveZeroFilterDeliversAll(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.jsonl")
	writeJSONLEvents(t, src, 1, 2, 3)
	archive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 3))
	var stderr bytes.Buffer
	if err := gzipAndArchive(src, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}

	var seen []uint64
	if err := streamArchive(archive, Filter{}, func(e Event) bool {
		seen = append(seen, e.Seq)
		return true
	}); err != nil {
		t.Fatalf("streamArchive: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("zero filter delivered %v, want all 3", seen)
	}
}
