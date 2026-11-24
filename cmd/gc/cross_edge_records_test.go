package main

import (
	"strings"
	"testing"
)

// TestMergeCrossEdgeRecordsIsIdempotent: a writer that runs again records each
// edge once, beside what the bead already carries, and a record that does not
// parse is refused rather than overwritten.
func TestMergeCrossEdgeRecordsIsIdempotent(t *testing.T) {
	earlier := `[{"dir":"in","type":"tracks","other":"rg-1","action":"dropped"}]`
	add := []rigCrossEdgeRecord{{Dir: "out", Type: "discovered-from", Other: "gcg-1", OtherStatus: "closed", Action: rigEdgeDropped}}
	first, changed, err := mergeCrossEdgeRecords(earlier, add)
	if err != nil || !changed || !strings.HasPrefix(first, strings.TrimSuffix(earlier, "]")+",") {
		t.Fatalf("first merge = %q changed=%v err=%v, want %s with the new record appended", first, changed, err, earlier)
	}
	second, changed, err := mergeCrossEdgeRecords(first, add)
	if err != nil || changed || second != first {
		t.Fatalf("second merge = %q changed=%v err=%v, want %q unchanged", second, changed, err, first)
	}
	if _, _, err := mergeCrossEdgeRecords("{not json", add); err == nil {
		t.Fatal("a record that does not parse was overwritten, losing whatever it held")
	}
}
