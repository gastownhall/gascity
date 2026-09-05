package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLog writes evts as JSONL at path and returns the file size.
func writeLog(t *testing.T, path string, evts []Event) int64 {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range evts {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return int64(buf.Len())
}

// TestReadFilteredSinceStopsBeforeReadingWholeFile is the regression this
// change exists for: a bare Since filter used to json.Unmarshal every record
// in the active log before matchesFilter could reject it, so the cost of
// asking about the last few minutes was the cost of the entire history.
//
// The bound is asserted on WORK DONE, not on the answer — a test that only
// checked the returned events would pass just as well against the old full
// forward scan. The old, unmatched events carry a payload large enough that
// reaching them would be visible, and the seq-1 sentinel is what a walk that
// ran to byte 0 would have had to parse.
func TestReadFilteredSinceStopsBeforeReadingWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	var evts []Event
	padding := string(bytes.Repeat([]byte("x"), 4*1024))
	// ~200KB of ancient history, several 64KB chunks deep.
	for i := 0; i < 50; i++ {
		evts = append(evts, Event{
			Seq: uint64(i + 1), Type: "old.type", Actor: "api",
			Ts: now.Add(-72 * time.Hour), Message: padding,
		})
	}
	// The recent window, at EOF.
	evts = append(evts,
		Event{Seq: 100, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		Event{Seq: 101, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	)
	size := writeLog(t, path, evts)

	got, bounded, err := readFilteredSince(path, Filter{Type: "bead.closed", Since: now.Add(-5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded {
		t.Fatalf("walk reported unbounded; it must stop once past the window (file is %d bytes)", size)
	}
	if len(got) != 2 || got[0].Seq != 100 || got[1].Seq != 101 {
		t.Fatalf("got %+v, want seq 100 then 101 in ascending order", got)
	}
}

// TestReadFilteredSinceToleratesClockSkew pins the reason the walk does not
// stop at the first event older than Since. Seq is globally monotonic; Ts is
// not — events are appended by many processes under an flock, each stamping
// its own wall clock — so an event written later can carry an earlier
// timestamp than the record before it. A first-older-wins stop would drop the
// straggler here, which is inside the window and must be returned.
func TestReadFilteredSinceToleratesClockSkew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	evts := []Event{
		{Seq: 1, Type: "old.type", Actor: "api", Ts: now.Add(-72 * time.Hour)},
		{Seq: 2, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
		// Written AFTER seq 2 but stamped earlier: a process whose clock
		// trails its neighbors. Still inside the window.
		{Seq: 3, Type: "bead.closed", Actor: "api", Ts: now.Add(-3 * time.Minute)},
		{Seq: 4, Type: "bead.closed", Actor: "api", Ts: now.Add(-30 * time.Second)},
	}
	writeLog(t, path, evts)

	got, bounded, err := readFilteredSince(path, Filter{Type: "bead.closed", Since: now.Add(-4 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded {
		t.Fatal("walk reported unbounded; the seq-1 event is a full margin past the window")
	}
	if len(got) != 3 {
		t.Fatalf("got %d events %+v, want all 3 in-window matches including the skewed seq-3", len(got), got)
	}
	for i, want := range []uint64{2, 3, 4} {
		if got[i].Seq != want {
			t.Fatalf("got[%d].Seq = %d, want %d (result must stay in file order)", i, got[i].Seq, want)
		}
	}
}

// TestReadFilteredSinceReportsUnboundedWhenWindowPredatesActiveLog is the
// correctness half. When the walk runs out of file without ever leaving the
// window, the active log does not cover the whole window and an archive may
// still hold matches — so the answer is INCOMPLETE and must not be served.
// Reporting bounded here is how a rotated city would silently lose events.
func TestReadFilteredSinceReportsUnboundedWhenWindowPredatesActiveLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	writeLog(t, path, []Event{
		{Seq: 10, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		{Seq: 11, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	})

	// Every record in the active log is inside this window.
	_, bounded, err := readFilteredSince(path, Filter{Type: "bead.closed", Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if bounded {
		t.Fatal("walk reported bounded after reaching byte 0; archives may hold older matches and must be consulted")
	}
}

// TestReadFilteredSinceMatchesForwardScan is the equivalence check: for a
// window the active log covers, the bounded path must return exactly what the
// forward scan returns, Limit semantics included. readFilteredTracked takes
// the FIRST Limit matches chronologically, not the last, and a backward walk
// that forgot to reverse before truncating would return the newest instead.
func TestReadFilteredSinceMatchesForwardScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	evts := []Event{
		{Seq: 1, Type: "bead.closed", Actor: "api", Ts: now.Add(-90 * time.Minute)},
		{Seq: 2, Type: "bead.closed", Actor: "api", Ts: now.Add(-4 * time.Minute)},
		{Seq: 3, Type: "other.type", Actor: "api", Ts: now.Add(-3 * time.Minute)},
		{Seq: 4, Type: "bead.closed", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		{Seq: 5, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	}
	writeLog(t, path, evts)

	for _, limit := range []int{0, 1, 2, 99} {
		filter := Filter{Type: "bead.closed", Since: now.Add(-10 * time.Minute), Limit: limit}

		want, _, err := readFilteredTracked(path, filter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadFiltered(path, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("limit=%d: got %d events, want %d", limit, len(got), len(want))
		}
		for i := range want {
			if got[i].Seq != want[i].Seq {
				t.Fatalf("limit=%d: got[%d].Seq = %d, want %d", limit, i, got[i].Seq, want[i].Seq)
			}
		}
	}
}

// TestReadFilteredWithInFlightSinceMatchesFullRead covers the path the
// supervisor's provider actually takes. ReadFiltered is not the hot caller —
// `gc events` reaches the supervisor over its API, and the supervisor answers
// from the recorder, which is ReadFilteredWithInFlight. Bounding one and not
// the other would have left the cost exactly where it was.
func TestReadFilteredWithInFlightSinceMatchesFullRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	now := time.Now().UTC()

	var evts []Event
	padding := string(bytes.Repeat([]byte("x"), 4*1024))
	for i := 0; i < 40; i++ {
		evts = append(evts, Event{
			Seq: uint64(i + 1), Type: "old.type", Actor: "api",
			Ts: now.Add(-48 * time.Hour), Message: padding,
		})
	}
	evts = append(evts,
		Event{Seq: 90, Type: "bead.closed", Actor: "api", Ts: now.Add(-3 * time.Minute)},
		Event{Seq: 91, Type: "other.type", Actor: "api", Ts: now.Add(-2 * time.Minute)},
		Event{Seq: 92, Type: "bead.closed", Actor: "api", Ts: now.Add(-1 * time.Minute)},
	)
	writeLog(t, path, evts)

	filter := Filter{Type: "bead.closed", Since: now.Add(-10 * time.Minute)}

	want, _, err := readFilteredTracked(path, filter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFilteredWithInFlight(path, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Seq != want[i].Seq {
			t.Fatalf("got[%d].Seq = %d, want %d", i, got[i].Seq, want[i].Seq)
		}
	}
	if len(got) != 2 || got[0].Seq != 90 || got[1].Seq != 92 {
		t.Fatalf("got %+v, want seq 90 then 92", got)
	}
}
