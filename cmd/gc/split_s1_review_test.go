package main

import (
	"path/filepath"
	"testing"
)

// Regression tests from the review of #8332 (split-store fail-closed guards).

// A work bead an agent files with an order-run: label inherited from the
// patrol that found it (150 such beads in production single-store cities) is
// work, and gc bd may create it on a split city.
func TestSplitCityCreatesAPatrolFiledBugCarryingAnOrderRunLabel(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	for _, args := range [][]string{
		{"create", "Technical health finding", "--type", "bug", "--labels", "order-run:technical-health-patrol,technical-health"},
		{"create", "[handoff] backlog-review", "--labels", "order-run:backlog-review,product-lens"},
	} {
		if msg, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), args); refused {
			t.Errorf("gc bd %v refused on a split city: %s", args, msg)
		}
	}
}
