package doctor

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// writeOrderFiringArchive writes a rotated, gzipped event archive beside the
// city's active event log.
func writeOrderFiringArchive(t *testing.T, cityPath, stamp string, firstSeq, lastSeq uint64, body string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating event dir: %v", err)
	}
	base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, firstSeq, lastSeq)
	f, err := os.Create(filepath.Join(dir, base))
	if err != nil {
		t.Fatalf("creating archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(body)); err != nil {
		t.Fatalf("writing archive body: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
}

// TestLatestControllerStartedAtFallsBackToArchivesWithoutWalkingThemAll covers
// the condition that made this expensive in production: the controller started
// before the last rotation, so the active log holds no controller.started and
// every invocation took the archived path.
//
// The older archives are intentionally not valid gzip. Reading any of them
// fails loudly, so this asserts both halves at once: the archived controller
// start is still found, and the archives older than it are never opened.
func TestLatestControllerStartedAtFallsBackToArchivesWithoutWalkingThemAll(t *testing.T) {
	cityPath := t.TempDir()

	// Active log: real events, but no controller start.
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Ts: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)},
	)

	dir := filepath.Join(cityPath, ".gc")
	for _, a := range []struct {
		stamp             string
		firstSeq, lastSeq uint64
	}{
		{"20260730T040012Z", 1, 100},
		{"20260731T172025Z", 101, 200},
	} {
		base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", a.stamp, a.firstSeq, a.lastSeq)
		if err := os.WriteFile(filepath.Join(dir, base), []byte("not gzip"), 0o600); err != nil {
			t.Fatalf("writing unreadable archive: %v", err)
		}
	}

	want := time.Date(2026, 8, 4, 21, 46, 0, 0, time.UTC)
	writeOrderFiringArchive(t, cityPath, "20260804T214650Z", 201, 300,
		fmt.Sprintf(`{"seq":210,"type":%q,"ts":%q,"actor":"test"}`+"\n",
			events.ControllerStarted, want.Format(time.RFC3339Nano)))

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt read past the newest matching archive: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("latestControllerStartedAt = %s, want %s", got.UTC(), want)
	}
}

// TestLatestControllerStartedAtPrefersTheActiveLog keeps the cheap path cheap:
// when the active log holds a controller start, the archives are irrelevant. An
// unreadable archive proves none of them is touched.
func TestLatestControllerStartedAtPrefersTheActiveLog(t *testing.T) {
	cityPath := t.TempDir()
	want := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: want},
	)

	dir := filepath.Join(cityPath, ".gc")
	if err := os.WriteFile(
		filepath.Join(dir, "events.jsonl.archive-20260730T040012Z-seq-1-100.gz"),
		[]byte("not gzip"), 0o600); err != nil {
		t.Fatalf("writing unreadable archive: %v", err)
	}

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt touched the archives: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("latestControllerStartedAt = %s, want %s", got.UTC(), want)
	}
}

// TestLatestControllerStartedAtReportsUnknownWhenNothingRecorded pins the
// zero-time contract classifyOrderFiring depends on: absence must stay absence
// rather than becoming a bogus timestamp.
func TestLatestControllerStartedAtReportsUnknownWhenNothingRecorded(t *testing.T) {
	cityPath := t.TempDir()
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Ts: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)},
	)

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("latestControllerStartedAt = %s, want zero time when no controller start was recorded", got.UTC())
	}
}
