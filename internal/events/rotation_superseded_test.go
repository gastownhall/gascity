package events

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSupersededSetAsideIsInvisibleToEveryScanner: a set-aside colliding source
// must be invisible to the backfill source listing AND to the startup reaper —
// otherwise it is re-decoded on every AfterSeq=0 read and re-collided on every
// boot, which is the amplification the set-aside exists to end (ga-jctn6).
func TestSupersededSetAsideIsInvisibleToEveryScanner(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 5, 7, 18, 0, 0, 0, time.UTC)
	rotatingBase := "events.jsonl.rotating-" + ts.Format("20060102T150405Z") + "-seq-1-2"
	setAside := filepath.Join(dir, supersededRotatingPrefix+rotatingBase)
	if err := os.WriteFile(setAside, []byte("kept for inspection\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "events.jsonl.rotating-"+ts.Add(time.Hour).Format("20060102T150405Z")+"-seq-3-4")
	if err := os.WriteFile(live, []byte(`{"seq":3,"type":"bead.created","actor":"a","subject":"s"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcs, err := listBackfillSources(dir, 0)
	if err != nil {
		t.Fatalf("listBackfillSources: %v", err)
	}
	for _, s := range srcs {
		if filepath.Base(s.path) == filepath.Base(setAside) {
			t.Fatalf("set-aside file listed as a backfill source: %v", s.path)
		}
	}
	if len(srcs) != 1 {
		t.Fatalf("listed %d source(s), want only the live rotating file", len(srcs))
	}

	var stderr bytes.Buffer
	if err := reapOrphanedRotatingFiles(dir, &stderr); err != nil {
		t.Fatalf("reapOrphanedRotatingFiles: %v", err)
	}
	if _, err := os.Stat(setAside); err != nil {
		t.Fatalf("the reaper touched the set-aside file: %v", err)
	}
}

// TestReadRotationSourcesSkipsRotatingTwinOfListedArchive: a rotating file
// whose seq window an already-read archive covers holds the same events by
// construction (seqs are globally unique), so decoding it is pure waste — and
// the crash window between archive rename and source removal makes such twins
// routine. The twin holds VALID JSONL for its {1,2} window on purpose: a
// rotating source is opened as plain text, so garbage content would be skipped
// line-by-line and yield zero events whether or not the window skip fired,
// leaving the assertion unable to fail. Decodable twin events make it real — if
// the reader.go window skip regresses and the twin is opened, its two events
// come back and len(events)==0 fails, so a pass proves the skip rather than a
// silent parse miss.
func TestReadRotationSourcesSkipsRotatingTwinOfListedArchive(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 5, 7, 18, 0, 0, 0, time.UTC)
	twin := filepath.Join(dir, "events.jsonl.rotating-"+ts.Format("20060102T150405Z")+"-seq-1-2")
	// Valid JSONL whose seqs fill the {1,2} window the listed archive covers, so
	// an un-skipped open would return both events — the genuine falsifier.
	twinData := `{"seq":1,"type":"bead.created","actor":"a","subject":"s"}` + "\n" +
		`{"seq":2,"type":"bead.created","actor":"a","subject":"s"}` + "\n"
	if err := os.WriteFile(twin, []byte(twinData), 0o644); err != nil {
		t.Fatal(err)
	}

	listed := map[eventSeqWindow]struct{}{{first: 1, last: 2}: {}}
	events, err := readRotationSources(active, Filter{}, listed)
	if err != nil {
		t.Fatalf("readRotationSources opened the twin of an already-read archive: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d event(s) from a directory whose only source is a covered twin, want 0", len(events))
	}
}
