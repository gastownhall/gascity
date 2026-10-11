//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"
)

// The doltlite seek-safety gates: SQL cannot express the compound
// (created_at, id) boundary, so every SQL-side row cut must be disabled for
// seeked queries and the exact filter applied Go-side before the limit.

func TestDoltliteBoundedTopNDisqualifiesSeek(t *testing.T) {
	sets := []doltliteTableSet{doltliteIssueTables, doltliteWispTables}
	base := ListQuery{Type: "task", Sort: SortCreatedDesc}
	if !doltliteCanSelectBoundedTopN(base, sets, "", 10, "") {
		t.Fatal("baseline bounded query should qualify for SQL top-N (test setup wrong)")
	}
	seeked := base
	seeked.SeekAfter = &SeekBoundary{CreatedAt: time.Now(), ID: "gc-1"}
	if doltliteCanSelectBoundedTopN(seeked, sets, "", 10, "") {
		t.Fatal("seeked query must not take the SQL top-N path — the SQL LIMIT would cut before the boundary filter")
	}
}

func TestDoltliteCountUnsupportedForSeek(t *testing.T) {
	base := ListQuery{Type: "task"}
	if !doltliteCountSupported(base) {
		t.Fatal("baseline query should be countable (test setup wrong)")
	}
	seeked := base
	seeked.SeekAfter = &SeekBoundary{CreatedAt: time.Now(), ID: "gc-1"}
	if doltliteCountSupported(seeked) {
		t.Fatal("seeked query must be count-unsupported — Count cannot reproduce the Go-side boundary")
	}
}

func TestDoltliteFilterBeforeTimesAppliesSeek(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	rows := []Bead{
		{ID: "gc-3", CreatedAt: ts.Add(2 * time.Second)}, // newer than boundary — drop
		{ID: "gc-2", CreatedAt: ts},                      // boundary row — drop
		{ID: "gc-1", CreatedAt: ts},                      // tie, smaller id — keep (after in DESC)
		{ID: "gc-0", CreatedAt: ts.Add(-time.Second)},    // older — keep
	}
	q := ListQuery{
		Sort:      SortCreatedDesc,
		SeekAfter: &SeekBoundary{CreatedAt: ts, ID: "gc-2"},
	}
	out := filterDoltliteBeforeTimes(rows, q)
	if len(out) != 2 || out[0].ID != "gc-1" || out[1].ID != "gc-0" {
		ids := make([]string, len(out))
		for i, b := range out {
			ids[i] = b.ID
		}
		t.Fatalf("filtered = %v, want [gc-1 gc-0]", ids)
	}
}
