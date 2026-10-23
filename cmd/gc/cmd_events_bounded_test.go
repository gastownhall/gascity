package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// The local --since fallback prints each event as it reads it instead of
// collecting the whole window first, so the window's size does not bound the
// CLI's memory. Proof: when printing the first event fails, the read stops
// there, before a later archive that cannot be read. A read that collected
// first would fail on that archive instead.
func TestDoEventsLocalSinceWindowIsStreamed(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	rec := newTestProvider(t, gcDir)
	for i := 0; i < 5; i++ {
		rec.Record(events.Event{Type: events.SessionStopped, Actor: "gc", Subject: "worker"})
	}
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	rec.WaitForRotations()
	rec.Record(events.Event{Type: events.SessionStopped, Actor: "gc", Subject: "worker"})
	// A newer archive (read after the real one) that is not gzip.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	unreadable := filepath.Join(gcDir, "events.jsonl.archive-"+stamp+"-seq-100-200.gz")
	if err := os.WriteFile(unreadable, []byte("not a gzip stream\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doEvents(eventsAPIScope{localOnly: true, cityName: "mc-city", cityPath: cityDir}, "", "1h", nil, failingWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("doEvents = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "marshal: write failed") || strings.Contains(stderr.String(), "gzip") {
		t.Fatalf("stderr = %q, want the write error from the first event, not the unreadable archive", stderr.String())
	}
}
