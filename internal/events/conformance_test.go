package events_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/events/eventstest"
)

func TestFileRecorderConformance(t *testing.T) {
	factory := func(t *testing.T) (events.Provider, func()) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "events.jsonl")
		var stderr bytes.Buffer
		rec, err := events.NewFileRecorder(path, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		return rec, func() { rec.Close() } //nolint:errcheck // test cleanup
	}
	eventstest.RunProviderTests(t, factory)
	eventstest.RunConcurrencyTests(t, factory)
}

func TestFakeConformance(t *testing.T) {
	factory := func(t *testing.T) (events.Provider, func()) {
		t.Helper()
		return events.NewFake(), func() {}
	}
	eventstest.RunProviderTests(t, factory)
	eventstest.RunConcurrencyTests(t, factory)
}
