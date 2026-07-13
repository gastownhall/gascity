package runproj

import (
	"testing"
)

// allRunNodeStatuses enumerates every RunNodeStatus value from
// shared/src/run-detail.ts. It is the guard for the terminality taxonomy: the
// test below asserts each value here is classified into exactly one of the
// terminal/non-terminal partitions, so an unclassified status fails here instead
// of silently changing run terminality. NOTE: this list is a hand-maintained
// mirror of the TS RunNodeStatus union — the guard only fires for a new status
// if it is added to BOTH the TS type AND this list, so keep the two in lockstep.
var allRunNodeStatuses = []string{
	"pending",
	"ready",
	"running",
	"active",
	"done",
	"completed",
	"failed",
	"blocked",
	"skipped",
}

// TestRunNodeStatusTaxonomyIsExhaustive proves every RunNodeStatus is classified
// exactly once across the terminal and non-terminal partitions. A newly-added
// status that is not placed in either list (or is placed in both) fails.
func TestRunNodeStatusTaxonomyIsExhaustive(t *testing.T) {
	classification := map[string]int{}
	for _, s := range terminalRunNodeStatuses {
		classification[s]++
	}
	for _, s := range nonTerminalRunNodeStatuses {
		classification[s]++
	}

	for _, status := range allRunNodeStatuses {
		switch classification[status] {
		case 0:
			t.Errorf("status %q is unclassified: add it to terminalRunNodeStatuses or nonTerminalRunNodeStatuses", status)
		case 1:
			// classified exactly once — good
		default:
			t.Errorf("status %q is classified %d times: it must appear in exactly one taxonomy list", status, classification[status])
		}
	}

	// No taxonomy list may reference a status that is not a known RunNodeStatus.
	known := map[string]bool{}
	for _, s := range allRunNodeStatuses {
		known[s] = true
	}
	for _, s := range append(append([]string{}, terminalRunNodeStatuses...), nonTerminalRunNodeStatuses...) {
		if !known[s] {
			t.Errorf("taxonomy references unknown status %q — remove it or add it to allRunNodeStatuses", s)
		}
	}
}

// TestDeriveRunTerminal proves the Go derivation matches the retired client
// isTerminalProgress fold across representative cases.
func TestDeriveRunTerminal(t *testing.T) {
	tests := []struct {
		name         string
		statuses     map[string]int
		visibleCount int
		want         bool
	}{
		{
			name:         "empty run is not terminal",
			statuses:     nil,
			visibleCount: 0,
			want:         false,
		},
		{
			name:         "zero visible nodes is not terminal even with terminal counts",
			statuses:     map[string]int{"completed": 3},
			visibleCount: 0,
			want:         false,
		},
		{
			name:         "all terminal covering every visible node is terminal",
			statuses:     map[string]int{"completed": 2, "done": 1, "failed": 1, "skipped": 1},
			visibleCount: 5,
			want:         true,
		},
		{
			name:         "one running node is not terminal",
			statuses:     map[string]int{"completed": 4, "running": 1},
			visibleCount: 5,
			want:         false,
		},
		{
			name:         "one active node is not terminal (golden dt-adopt1 shape)",
			statuses:     map[string]int{"ready": 1, "completed": 3, "active": 1},
			visibleCount: 5,
			want:         false,
		},
		{
			name:         "terminal tally short of visible count is not terminal",
			statuses:     map[string]int{"completed": 2},
			visibleCount: 5,
			want:         false,
		},
		{
			name:         "single completed node is terminal",
			statuses:     map[string]int{"completed": 1},
			visibleCount: 1,
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := statusCountsFrom(tt.statuses)
			if got := deriveRunTerminal(counts, tt.visibleCount); got != tt.want {
				t.Errorf("deriveRunTerminal(%v, %d) = %v, want %v", tt.statuses, tt.visibleCount, got, tt.want)
			}
		})
	}
}

// TestDeriveRunTerminalMatchesClientFoldPerStatus feeds a single-node run of each
// status and asserts terminality equals the status's taxonomy class — the exact
// equivalence the retired client fold provided.
func TestDeriveRunTerminalMatchesClientFoldPerStatus(t *testing.T) {
	terminal := map[string]bool{}
	for _, s := range terminalRunNodeStatuses {
		terminal[s] = true
	}
	for _, status := range allRunNodeStatuses {
		counts := statusCountsFrom(map[string]int{status: 1})
		got := deriveRunTerminal(counts, 1)
		if want := terminal[status]; got != want {
			t.Errorf("single %q node: deriveRunTerminal = %v, want %v", status, got, want)
		}
	}
}

// statusCountsFrom builds a nodeStatusCounts from a plain map, using the exported
// inc path so the internal representation matches production.
func statusCountsFrom(m map[string]int) nodeStatusCounts {
	var counts nodeStatusCounts
	// Iterate allRunNodeStatuses for a deterministic key order; any status not
	// in the canonical set is appended afterwards so callers can still test
	// unexpected values.
	for _, status := range allRunNodeStatuses {
		for i := 0; i < m[status]; i++ {
			counts.inc(status)
		}
	}
	for status, n := range m {
		if _, known := counts.counts[status]; known {
			continue
		}
		for i := 0; i < n; i++ {
			counts.inc(status)
		}
	}
	return counts
}
