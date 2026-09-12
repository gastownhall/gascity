package events

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArchiveWithEvents writes a rotated archive holding evs, named with the
// seq window the reader prunes and orders on.
func writeArchiveWithEvents(t *testing.T, dir string, stamp string, firstSeq, lastSeq uint64, evs ...Event) {
	t.Helper()
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"ts":%q,"actor":"test"}`+"\n",
			e.Seq, e.Type, e.Ts.UTC().Format(time.RFC3339Nano))
	}
	base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, firstSeq, lastSeq)
	writeGzipFile(t, filepath.Join(dir, base), b.String())
}

// TestLatestArchivedMatchReturnsNewestNotOldest pins the ordering the fix turns
// on. Archives are walked newest-first, so the newest matching event wins even
// though older archives also match. An oldest-first walk with a limit returns
// the wrong one, which is why a plain Limit could not fix the unbounded read.
func TestLatestArchivedMatchReturnsNewestNotOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	oldest := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
	middle := time.Date(2026, 7, 31, 17, 20, 0, 0, time.UTC)
	newest := time.Date(2026, 8, 4, 21, 46, 0, 0, time.UTC)

	writeArchiveWithEvents(t, dir, "20260730T040012Z", 1, 100,
		Event{Seq: 10, Type: ControllerStarted, Ts: oldest})
	writeArchiveWithEvents(t, dir, "20260731T172025Z", 101, 200,
		Event{Seq: 110, Type: ControllerStarted, Ts: middle})
	writeArchiveWithEvents(t, dir, "20260804T214650Z", 201, 300,
		Event{Seq: 210, Type: ControllerStarted, Ts: newest})

	got, found, err := LatestArchivedMatch(path, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("LatestArchivedMatch: %v", err)
	}
	if !found {
		t.Fatal("LatestArchivedMatch found no match, want the newest archived controller start")
	}
	if !got.Ts.Equal(newest) {
		t.Errorf("LatestArchivedMatch ts = %s, want %s (the newest archive's event, not an older one)",
			got.Ts.UTC(), newest)
	}
}

// TestLatestArchivedMatchTakesLastMatchWithinAnArchive covers ordering inside a
// single archive: events are stored oldest-first, so the scan must keep the
// last match rather than returning on the first one.
func TestLatestArchivedMatchTakesLastMatchWithinAnArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	earlier := time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)
	later := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	writeArchiveWithEvents(t, dir, "20260804T214650Z", 1, 100,
		Event{Seq: 10, Type: ControllerStarted, Ts: earlier},
		Event{Seq: 20, Type: "order.fired", Ts: earlier.Add(time.Hour)},
		Event{Seq: 30, Type: ControllerStarted, Ts: later},
	)

	got, found, err := LatestArchivedMatch(path, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("LatestArchivedMatch: %v", err)
	}
	if !found {
		t.Fatal("LatestArchivedMatch found no match")
	}
	if !got.Ts.Equal(later) {
		t.Errorf("LatestArchivedMatch ts = %s, want %s (last match in the archive)", got.Ts.UTC(), later)
	}
}

// TestLatestArchivedMatchStopsAtTheNewestMatchingArchive is the boundedness
// proof, and it is deliberately mechanical rather than timing-based: the older
// archives are not valid gzip, so any read of them fails loudly. A newest-first
// scan that stops at the first match never opens them and succeeds. A walk that
// visits every archive (what an unbounded read does) surfaces a gunzip error.
func TestLatestArchivedMatchStopsAtTheNewestMatchingArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	for _, stamp := range []struct {
		ts                string
		firstSeq, lastSeq uint64
	}{
		{"20260730T040012Z", 1, 100},
		{"20260731T172025Z", 101, 200},
		{"20260801T090000Z", 201, 300},
	} {
		base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp.ts, stamp.firstSeq, stamp.lastSeq)
		if err := os.WriteFile(filepath.Join(dir, base), []byte("not gzip at all"), 0o600); err != nil {
			t.Fatalf("writing unreadable archive: %v", err)
		}
	}

	newest := time.Date(2026, 8, 4, 21, 46, 0, 0, time.UTC)
	writeArchiveWithEvents(t, dir, "20260804T214650Z", 301, 400,
		Event{Seq: 310, Type: ControllerStarted, Ts: newest})

	got, found, err := LatestArchivedMatch(path, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("LatestArchivedMatch read past the newest matching archive: %v", err)
	}
	if !found || !got.Ts.Equal(newest) {
		t.Errorf("LatestArchivedMatch = (%+v, %v), want the newest archive's event %s", got, found, newest)
	}
}

// TestLatestArchivedMatchReportsNoMatch distinguishes "searched and found
// nothing" from an error, since the caller maps the two to different verdicts.
func TestLatestArchivedMatchReportsNoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeArchiveWithEvents(t, dir, "20260804T214650Z", 1, 100,
		Event{Seq: 10, Type: "order.fired", Ts: time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)})

	got, found, err := LatestArchivedMatch(path, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("LatestArchivedMatch: %v", err)
	}
	if found {
		t.Errorf("LatestArchivedMatch found = true (%+v), want false when no archive matches", got)
	}
}

// TestLatestArchivedMatchOnMissingDirectory keeps a city with no archive
// directory from surfacing as a check error.
func TestLatestArchivedMatchOnMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "events.jsonl")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("precondition: directory should not exist, stat err = %v", err)
	}

	_, found, err := LatestArchivedMatch(path, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("LatestArchivedMatch on missing dir: %v", err)
	}
	if found {
		t.Error("LatestArchivedMatch found = true, want false when there are no archives")
	}
}
