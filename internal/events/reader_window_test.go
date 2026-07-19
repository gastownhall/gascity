package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadFilteredTailWindowBytes_BoundStopsWalk verifies that WindowBytes
// stops the backward walk before reaching events that precede the window.
// Without the floor guard in readFilteredTailFromFile, the scan continues to
// byte 0 and finds the old event — this test fails in that case.
func TestReadFilteredTailWindowBytes_BoundStopsWalk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Write a target event at the very start of the file.
	target := Event{
		Seq:  1,
		Ts:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Type: "gc.target.event",
	}
	targetJSON, _ := json.Marshal(target)

	// Write enough noise to push the target event well before the window.
	// Each noise line is ~80 bytes; 200 lines ≈ 16 KB.
	noise := Event{
		Seq:  2,
		Ts:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Type: "gc.noise",
	}
	noiseJSON, _ := json.Marshal(noise)
	noiseLine := append(noiseJSON, '\n')

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(targetJSON, '\n')) //nolint:errcheck
	total := int64(len(targetJSON) + 1)
	for total < 20*1024 { // 20 KB of noise after the target
		f.Write(noiseLine) //nolint:errcheck
		total += int64(len(noiseLine))
	}
	f.Close() //nolint:errcheck

	// Window covers only the last 10 KB — the target event is in the first
	// ~80 bytes, well before the window floor.
	const window int64 = 10 * 1024
	got, err := ReadFilteredTail(path, Filter{Type: "gc.target.event", WindowBytes: window}, 1)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no events (target is before window), got %d; WindowBytes bound is not stopping the walk", len(got))
	}
}

// TestReadFilteredTailWindowBytes_EventInWindowIsFound verifies that an event
// within the window IS returned, so the previous test is not vacuously true.
func TestReadFilteredTailWindowBytes_EventInWindowIsFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Write 20 KB of noise, then a target event at the end.
	noiseJSON := []byte(`{"seq":1,"ts":"2026-01-01T00:00:00Z","type":"gc.noise"}` + "\n")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for total < 20*1024 {
		f.Write(noiseJSON) //nolint:errcheck
		total += int64(len(noiseJSON))
	}
	target := Event{
		Seq:  999,
		Ts:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Type: "gc.target.event",
	}
	targetJSON, _ := json.Marshal(target)
	f.Write(append(targetJSON, '\n')) //nolint:errcheck
	f.Close()                         //nolint:errcheck

	// Window covers the last 10 KB — larger than the target event, which is
	// at the very end.
	const window int64 = 10 * 1024
	got, err := ReadFilteredTail(path, Filter{Type: "gc.target.event", WindowBytes: window}, 1)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event in window, got %d", len(got))
	}
	if got[0].Seq != 999 {
		t.Errorf("got seq=%d, want 999", got[0].Seq)
	}
}

// TestReadFilteredTailWindowBytes_ZeroWindowMeansUnbounded verifies that
// WindowBytes=0 leaves the scan unbounded (backward-compatible default).
func TestReadFilteredTailWindowBytes_ZeroWindowMeansUnbounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Write a target event, then 20 KB of noise.
	target := Event{Seq: 1, Ts: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Type: "gc.target.event"}
	targetJSON, _ := json.Marshal(target)
	noiseJSON := []byte(strings.Repeat(`{"seq":2,"ts":"2026-01-02T00:00:00Z","type":"gc.noise"}`+"\n", 350))

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(targetJSON, '\n')) //nolint:errcheck
	f.Write(noiseJSON)                //nolint:errcheck
	f.Close()                         //nolint:errcheck

	// WindowBytes=0: unbounded scan must find the event at byte 0.
	got, err := ReadFilteredTail(path, Filter{Type: "gc.target.event", WindowBytes: 0}, 1)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("WindowBytes=0 should scan the whole file; got %d events, want 1", len(got))
	}
}
